#!/usr/bin/env python3
"""Crash-safety chaos test for the events storage layer.

Sibling to first_test_chaos.py — same loop shape (start → ingest → SIGKILL →
restart → verify), but exercises the per-database events layer rather than
metrics. Validates the five crash-safety properties documented in
docs/EVENTS.md:

  1. Events catalog (events.json) is persisted before the events WAL is reset
     on graceful shutdown — final assert_events_post_checkpoint() requires the
     WAL to be empty and at least one events-*.dat to exist.
  2. WAL replay reconstructs catalog entries introduced by newEvent records.
     We verify every distinct event name we wrote is queryable after every
     SIGKILL+restart, even when the catalog file wasn't rewritten between
     first occurrence and the crash.
  3. Per-event-name monotonic timestamps preserved across the loop —
     queried timestamps for each name must be non-decreasing.
  4. No corrupted/spurious events appear (everything received was sent).
  5. Mixed value types round-trip — int32, float32, and none-typed events
     all replay with their typed values intact.

Acceptable: missing the most recent samples that hadn't fsync'd before the
SIGKILL. Not acceptable: corrupted bytes, wrong types, broken JSON,
wrong-order timestamps, events the test didn't send.

Run:

    python3 scripts/events_chaos.py --iterations 100
"""

from __future__ import annotations

import argparse
import json
import math
import random
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass, field
from pathlib import Path
from typing import Dict, List, Optional, Tuple


# Three event names exercise all three value types so a single iteration
# stresses every decode path on the engine side.
EVENT_INT32 = "evchaos.disc_write_slow"
EVENT_FLOAT32 = "evchaos.temp_overheat"
EVENT_NONE = "evchaos.heartbeat"
EVENT_NAMES = (EVENT_INT32, EVENT_FLOAT32, EVENT_NONE)


@dataclass
class EventState:
    """Per-event running counters for what we sent and the highest ts we used.

    received_count is reused across iterations; we never decrement it (every
    iteration's restart-and-query happens against the cumulative population
    of writes, not a per-iteration slice).
    """

    base_int: int = 0
    sent_count: int = 0
    max_ts_ns: int = 0
    last_seen_ts_ns: int = 0  # most recent ts we've ever observed in a query
    # Track every (ts, value, payload_marker) we sent so we can spot phantom
    # events on the receive side. We don't dedupe by ts because equal ts is
    # legal (per-name monotonic-NON-decreasing rule); the (ts, value) pair is
    # what must match exactly.
    sent: List[Tuple[int, str, str]] = field(default_factory=list)


class EventsChaosError(RuntimeError):
    pass


def http_get_json(url: str, timeout: float = 3.0) -> dict:
    req = urllib.request.Request(url, method="GET")
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        return json.loads(resp.read().decode("utf-8"))


def http_post_json(url: str, body: list | dict, timeout: float = 5.0) -> dict:
    req = urllib.request.Request(
        url,
        data=json.dumps(body).encode("utf-8"),
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        return json.loads(resp.read().decode("utf-8"))


def wait_for_health(base_url: str, timeout_sec: float, proc: subprocess.Popen) -> None:
    deadline = time.time() + timeout_sec
    last_err: Optional[Exception] = None
    while time.time() < deadline:
        if proc.poll() is not None:
            try:
                output, _ = proc.communicate(timeout=1)
            except Exception:  # noqa: BLE001
                output = "<unavailable>"
            raise EventsChaosError(
                f"server exited before health check (code={proc.returncode}); output={output.strip()}"
            )
        try:
            payload = http_get_json(base_url + "/health", timeout=1.0)
            if payload.get("status") == "ok":
                return
        except Exception as exc:  # noqa: BLE001
            last_err = exc
        time.sleep(0.05)
    raise EventsChaosError(f"server health check failed: {last_err}")


def build_server_binary(repo_root: Path) -> Path:
    bin_dir = repo_root / ".tmp"
    bin_dir.mkdir(parents=True, exist_ok=True)
    binary_path = bin_dir / "nanotdb-events-chaos"
    cmd = ["go", "build", "-o", str(binary_path), "./cmd/nanotdb"]
    proc = subprocess.run(  # noqa: S603
        cmd,
        cwd=str(repo_root),
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
        check=False,
    )
    if proc.returncode != 0:
        raise EventsChaosError(f"go build failed: {proc.stdout.strip()}")
    return binary_path


def write_engine_config(config_path: Path, listen: str, wal_max_seg_size: int) -> None:
    """Write an engine.toml that enables the events layer for every new DB.

    Putting [manifest_defaults.events] enabled = true means the test DB,
    which is created on first ingest, gets events automatically without
    needing a manual manifest edit. The metric WAL fsync_policy stays at
    'segment' to match the first-test default.
    """
    content = (
        "[engine]\n"
        f"listen = \"{listen}\"\n\n"
        "[wal]\n"
        f"max_segment_size = {wal_max_seg_size}\n\n"
        "[manifest_defaults.events]\n"
        "enabled = true\n"
        "max_payload_bytes = 4096\n"
        "max_in_memory_bytes = 1048576\n"
    )
    config_path.write_text(content, encoding="utf-8")


def start_server(binary_path: Path, config_path: Path) -> subprocess.Popen:
    return subprocess.Popen(  # noqa: S603
        [str(binary_path), "--config", str(config_path)],
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
    )


def stop_server(proc: Optional[subprocess.Popen], force_kill: bool, timeout_sec: float = 5.0) -> bool:
    if proc is None or proc.poll() is not None:
        return True
    try:
        if force_kill:
            proc.kill()
            proc.wait(timeout=timeout_sec)
        else:
            proc.terminate()
            proc.wait(timeout=timeout_sec)
        return True
    except Exception:  # noqa: BLE001
        if not force_kill:
            try:
                proc.kill()
                proc.wait(timeout=timeout_sec)
            except Exception:  # noqa: BLE001
                pass
        return False


def assert_events_post_checkpoint(data_dir: Path, db: str) -> None:
    """Crash-safety property 1: after a *graceful* shutdown, the events WAL
    must be empty and at least one events-*.dat must exist on disk.

    Empty WAL means the engine flushed the open events page and only then
    reset the WAL. A non-empty WAL after graceful shutdown would mean either
    the catalog wasn't written before reset, or the page didn't flush — both
    are correctness bugs.
    """
    db_dir = data_dir / db
    if not db_dir.exists():
        raise EventsChaosError(f"database directory missing after checkpoint: {db_dir}")

    events_files = [p for p in db_dir.glob("events-*.dat") if p.is_file() and p.stat().st_size > 0]
    if not events_files:
        wal_path = db_dir / f"{db}.events.wal"
        wal_bytes = wal_path.stat().st_size if wal_path.exists() else 0
        raise EventsChaosError(
            f"no persisted events-*.dat in {db_dir}; events_wal_size={wal_bytes}. "
            "checkpoint shutdown did not flush the events page"
        )

    events_wal = db_dir / f"{db}.events.wal"
    if events_wal.exists() and events_wal.stat().st_size != 0:
        raise EventsChaosError(
            f"events WAL should be empty after graceful checkpoint, but {events_wal} has "
            f"{events_wal.stat().st_size} bytes (crash-safety rule 1: catalog must be written before WAL reset)"
        )

    events_json = db_dir / "events.json"
    if not events_json.exists() or events_json.stat().st_size == 0:
        raise EventsChaosError(
            f"events.json missing/empty in {db_dir}; catalog persistence path is broken"
        )


def query_events(
    base_url: str,
    db: str,
    name: str,
    start_ns: int,
    end_ns: int,
    limit: int = 1000,
) -> List[dict]:
    """Range-query the events endpoint for one event name.

    limit defaults to the server's hard cap so a single call returns
    everything in the window for our small test populations.
    """
    params = urllib.parse.urlencode(
        {
            "db": db,
            "name": name,
            "start": str(start_ns),
            "end": str(end_ns),
            "limit": str(limit),
            "timestamp_unit": "ns",
        }
    )
    try:
        payload = http_get_json(f"{base_url}/api/v1/events?{params}")
    except urllib.error.HTTPError as exc:
        body = ""
        try:
            body = exc.read().decode("utf-8", errors="replace")
        except Exception:  # noqa: BLE001
            body = "<unavailable>"
        raise EventsChaosError(
            f"events query HTTP error for {db}/{name}: HTTP {exc.code} {exc.reason}; body={body}"
        ) from exc

    if payload.get("status") != "success":
        raise EventsChaosError(f"events query failed for {db}/{name}: {payload}")

    data = payload.get("data", {})
    rt = data.get("resultType")
    if rt != "events":
        raise EventsChaosError(f"unexpected resultType {rt!r} (want 'events')")
    return data.get("result", [])


def get_events_catalog(base_url: str, db: str) -> List[dict]:
    params = urllib.parse.urlencode({"db": db})
    try:
        payload = http_get_json(f"{base_url}/api/v1/events/catalog?{params}")
    except urllib.error.HTTPError as exc:
        body = ""
        try:
            body = exc.read().decode("utf-8", errors="replace")
        except Exception:  # noqa: BLE001
            body = "<unavailable>"
        raise EventsChaosError(
            f"events catalog HTTP error for {db}: HTTP {exc.code} {exc.reason}; body={body}"
        ) from exc
    if payload.get("status") != "success":
        raise EventsChaosError(f"events catalog failed for {db}: {payload}")
    return payload.get("data", {}).get("result", [])


def import_event_batch(base_url: str, batch: list) -> None:
    try:
        res = http_post_json(base_url + "/api/v1/events", batch)
    except urllib.error.HTTPError as exc:
        body = ""
        try:
            body = exc.read().decode("utf-8", errors="replace")
        except Exception:  # noqa: BLE001
            body = "<unavailable>"
        raise EventsChaosError(
            f"events import failed: HTTP {exc.code} {exc.reason}; body={body}"
        ) from exc
    if res.get("status") != "success":
        raise EventsChaosError(f"events import returned non-success: {res}")
    imported = res.get("data", {}).get("imported", 0)
    if imported != len(batch):
        raise EventsChaosError(
            f"events import counted {imported}, sent {len(batch)} — engine partially applied?"
        )


def build_event_payload(name: str, st: EventState, now_ns: int) -> Tuple[dict, Tuple[int, str, str]]:
    """Build one JSON event request + the tracking tuple we'll later match
    against query results.

    The tracking tuple is (ts, value_str, payload_marker). value_str uses
    repr for floats so we don't fight rounding when comparing across the
    JSON round trip — the chaos test cares about "no corruption", not about
    bit-exact reproduction of every float (which the test couldn't promise
    anyway because Python repr ≠ Go strconv).
    """
    # Monotonic-non-decreasing per name. Add jitter inside this iteration so
    # multiple in-iteration ts values aren't all identical, but never go
    # below the prior max_ts (the engine would reject those).
    ts = max(st.max_ts_ns + 1, now_ns)
    st.max_ts_ns = ts
    rec: dict = {"db": "evchaos", "name": name, "ts": ts}
    payload_marker = ""

    if name == EVENT_INT32:
        val = st.base_int + st.sent_count
        rec["value"] = val
        rec["payload"] = {"i": st.sent_count, "kind": "disc"}
        payload_marker = f"i={st.sent_count}"
        return rec, (ts, str(val), payload_marker)
    if name == EVENT_FLOAT32:
        # Pick a value Python and Go agree on through json encoding; whole +
        # tenths is enough for the regression we care about.
        val = round(20.0 + (st.sent_count % 100) / 10.0, 1)
        rec["value"] = val
        return rec, (ts, repr(val), "")
    if name == EVENT_NONE:
        if st.sent_count % 3 == 0:
            rec["payload"] = {"tick": st.sent_count}
            payload_marker = f"tick={st.sent_count}"
        return rec, (ts, "", payload_marker)
    raise EventsChaosError(f"unknown event name {name}")


def validate_event_series(name: str, events: List[dict], st: EventState) -> int:
    """Validate the query result for one event name.

    Checks crash-safety properties 3, 4, 5: monotonic ts, no corruption, no
    phantom events, correct types. Returns the count of received events.
    """
    prev_ts = -1
    sent_tuple_set = set(st.sent)
    seen_count = 0
    for ev in events:
        ts = int(ev.get("ts", -1))
        if ts < prev_ts:
            raise EventsChaosError(
                f"timestamps moved backwards for {name}: {ts} < {prev_ts}"
            )
        prev_ts = ts

        vt = ev.get("value_type")
        if name == EVENT_INT32:
            if vt != "int32":
                raise EventsChaosError(f"{name}: expected value_type=int32, got {vt}")
            v_val = ev.get("int32")
            if v_val is None:
                raise EventsChaosError(f"{name}: missing int32 value: {ev}")
            tup_value = str(int(v_val))
        elif name == EVENT_FLOAT32:
            if vt != "float32":
                raise EventsChaosError(f"{name}: expected value_type=float32, got {vt}")
            v_val = ev.get("float32")
            if v_val is None:
                raise EventsChaosError(f"{name}: missing float32 value: {ev}")
            tup_value = repr(round(float(v_val), 1))
        elif name == EVENT_NONE:
            if vt != "none":
                raise EventsChaosError(f"{name}: expected value_type=none, got {vt}")
            if "int32" in ev or "float32" in ev:
                raise EventsChaosError(f"{name}: none-typed event carried a value: {ev}")
            tup_value = ""

        payload = ev.get("payload")
        payload_marker = ""
        if payload is not None:
            if name == EVENT_INT32:
                if not isinstance(payload, dict) or "i" not in payload:
                    raise EventsChaosError(f"{name}: payload shape unexpected: {payload}")
                payload_marker = f"i={payload['i']}"
            elif name == EVENT_NONE:
                if not isinstance(payload, dict) or "tick" not in payload:
                    raise EventsChaosError(f"{name}: payload shape unexpected: {payload}")
                payload_marker = f"tick={payload['tick']}"

        tup = (ts, tup_value, payload_marker)
        if tup not in sent_tuple_set:
            raise EventsChaosError(
                f"received an event that was never sent for {name}: {tup}"
            )
        seen_count += 1

    return seen_count


def run_test(args: argparse.Namespace) -> None:
    random.seed(args.seed)

    repo_root = Path(__file__).resolve().parents[1]
    data_dir = Path(args.data_dir).resolve()
    data_dir.mkdir(parents=True, exist_ok=True)
    config_path = data_dir / "engine.toml"
    write_engine_config(config_path, args.listen, args.wal_max_segment_size)
    binary_path = build_server_binary(repo_root)

    base_url = f"http://{args.listen}"
    db = "evchaos"
    states: Dict[str, EventState] = {
        EVENT_INT32: EventState(base_int=1_000_000),
        EVENT_FLOAT32: EventState(),
        EVENT_NONE: EventState(),
    }

    proc: Optional[subprocess.Popen] = None
    started_at_ns = time.time_ns()

    try:
        for i in range(args.iterations):
            # ---------- ingest phase ----------
            proc = start_server(binary_path, config_path)
            wait_for_health(base_url, timeout_sec=args.start_timeout_sec, proc=proc)

            batch_size = random.randint(args.batch_min, args.batch_max)
            batch: list = []
            now_ns = time.time_ns()
            for _ in range(batch_size):
                # Random subset of event names each iteration so newEvent
                # records vs known-event records are interleaved.
                name = random.choice(EVENT_NAMES)
                st = states[name]
                rec, tup = build_event_payload(name, st, now_ns)
                batch.append(rec)
                st.sent_count += 1
                st.sent.append(tup)
                now_ns += 1  # keep monotonic across the batch

            import_event_batch(base_url, batch)

            # ---------- chaos: SIGKILL after random jitter ----------
            time.sleep(random.uniform(args.sleep_min_sec, args.sleep_max_sec))
            stop_server(proc, force_kill=True)
            proc = None

            # ---------- replay + verify phase ----------
            proc = start_server(binary_path, config_path)
            wait_for_health(base_url, timeout_sec=args.start_timeout_sec, proc=proc)

            # Property 2: every event name we've ever sent must appear in
            # the catalog after restart, regardless of when its first
            # occurrence was relative to checkpoint windows.
            catalog = get_events_catalog(base_url, db)
            cat_names = {e["name"] for e in catalog}
            for ev_name, st in states.items():
                if st.sent_count > 0 and ev_name not in cat_names:
                    raise EventsChaosError(
                        f"events catalog missing {ev_name!r} after restart "
                        f"(sent={st.sent_count}); replay did not reconstruct catalog"
                    )

            start_ns = started_at_ns - 1
            end_ns = time.time_ns() + 1_000_000
            total_received = 0
            for ev_name, st in states.items():
                events = query_events(base_url, db, ev_name, start_ns, end_ns, limit=1000)
                received = validate_event_series(ev_name, events, st)
                total_received += received
                lost = st.sent_count - received
                if lost < 0:
                    raise EventsChaosError(
                        f"{ev_name}: received {received} > sent {st.sent_count} — phantom events"
                    )
                if lost > 0:
                    # Acceptable up to the most-recent batch (un-fsynced).
                    if lost > args.batch_max:
                        raise EventsChaosError(
                            f"{ev_name}: lost {lost} events; window after kill should be <= batch_max ({args.batch_max})"
                        )

            # ---------- graceful checkpoint ----------
            graceful = stop_server(proc, force_kill=False)
            if not graceful:
                raise EventsChaosError("graceful shutdown failed (SIGTERM did not exit the process)")
            proc = None
            assert_events_post_checkpoint(data_dir, db)

            if (i + 1) % 10 == 0 or i == args.iterations - 1:
                tot = sum(s.sent_count for s in states.values())
                print(f"[progress] iteration {i + 1}/{args.iterations} passed; total sent across all events={tot}")

    finally:
        stop_server(proc, force_kill=True)

    print("[ok] events chaos test completed without detected corruption")

    # Final per-event summary using a brief boot.
    proc = start_server(binary_path, config_path)
    try:
        wait_for_health(base_url, timeout_sec=args.start_timeout_sec, proc=proc)
        start_ns = started_at_ns - 1
        end_ns = time.time_ns() + 1_000_000
        for ev_name, st in states.items():
            events = query_events(base_url, db, ev_name, start_ns, end_ns, limit=1000)
            print(f"  {db}/{ev_name}: sent={st.sent_count}, received={len(events)}, lost={st.sent_count - len(events)}")
    finally:
        stop_server(proc, force_kill=True)


def parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(description="NanoTDB events chaos test")
    p.add_argument("--listen", default="127.0.0.1:18429", help="server listen address")
    p.add_argument("--data-dir", default="./devdata-events-chaos", help="data dir for test runs")
    p.add_argument("--iterations", type=int, default=50, help="number of crash/restart iterations")
    p.add_argument("--batch-min", type=int, default=1, help="minimum events per iteration")
    p.add_argument("--batch-max", type=int, default=8, help="maximum events per iteration")
    p.add_argument("--sleep-min-sec", type=float, default=0.0, help="minimum random sleep before kill")
    p.add_argument("--sleep-max-sec", type=float, default=0.02, help="maximum random sleep before kill")
    p.add_argument("--start-timeout-sec", type=float, default=10.0, help="server startup timeout")
    p.add_argument("--wal-max-segment-size", type=int, default=64 * 1024 * 1024, help="metric wal max segment size")
    p.add_argument("--seed", type=int, default=42, help="random seed")
    return p.parse_args()


def main() -> int:
    args = parse_args()
    if args.batch_min <= 0 or args.batch_max < args.batch_min:
        print("invalid batch min/max", file=sys.stderr)
        return 2
    if args.iterations <= 0:
        print("iterations must be > 0", file=sys.stderr)
        return 2

    try:
        run_test(args)
    except EventsChaosError as exc:
        print(f"[fail] {exc}", file=sys.stderr)
        return 1
    except KeyboardInterrupt:
        print("[abort] interrupted", file=sys.stderr)
        return 130
    except Exception as exc:  # noqa: BLE001
        print(f"[fail] unexpected error: {exc}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
