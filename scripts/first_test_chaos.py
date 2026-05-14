#!/usr/bin/env python3
"""Crash-safety first test for nanotdb server.

This script repeatedly:
1) starts the server,
2) ingests points for multiple metrics,
3) kills the server at random times,
4) restarts and validates query responses are not corrupted.

Missing most-recent samples after crash are acceptable; corrupted samples are not.
"""

from __future__ import annotations

import argparse
import json
import random
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass
from pathlib import Path
from typing import Dict, List, Optional, Tuple


@dataclass
class MetricState:
    base_value: int
    sent_count: int = 0


class FirstTestError(RuntimeError):
    pass


def http_get_json(url: str, timeout: float = 3.0) -> dict:
    req = urllib.request.Request(url, method="GET")
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        return json.loads(resp.read().decode("utf-8"))


def http_post_text(url: str, body: str, timeout: float = 5.0) -> dict:
    req = urllib.request.Request(
        url,
        data=body.encode("utf-8"),
        headers={"Content-Type": "text/plain"},
        method="POST",
    )
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        return json.loads(resp.read().decode("utf-8"))


def wait_for_health(base_url: str, timeout_sec: float, proc: subprocess.Popen) -> None:
    deadline = time.time() + timeout_sec
    last_err: Optional[Exception] = None
    while time.time() < deadline:
        if proc.poll() is not None:
            output = ""
            try:
                output, _ = proc.communicate(timeout=1)
            except Exception:  # noqa: BLE001
                output = "<unavailable>"
            raise FirstTestError(
                f"server exited before health check (code={proc.returncode}); output={output.strip()}"
            )
        try:
            payload = http_get_json(base_url + "/health", timeout=1.0)
            if payload.get("status") == "ok":
                return
        except Exception as exc:  # noqa: BLE001
            last_err = exc
        time.sleep(0.05)
    raise FirstTestError(f"server health check failed: {last_err}")


def build_server_binary(repo_root: Path) -> Path:
    bin_dir = repo_root / ".tmp"
    bin_dir.mkdir(parents=True, exist_ok=True)
    binary_path = bin_dir / "nanotdb-first-test"
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
        raise FirstTestError(f"go build failed: {proc.stdout.strip()}")
    return binary_path


def write_engine_config(config_path: Path, listen: str, wal_max_seg_size: int) -> None:
    content = (
        "[engine]\n"
        f"listen = \"{listen}\"\n\n"
        "[wal]\n"
        f"max_segment_size = {wal_max_seg_size}\n"
    )
    config_path.write_text(content, encoding="utf-8")


def start_server(binary_path: Path, config_path: Path) -> subprocess.Popen:
    cmd = [
        str(binary_path),
        "--config",
        str(config_path),
    ]
    proc = subprocess.Popen(  # noqa: S603
        cmd,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
    )
    return proc


def stop_server(proc: Optional[subprocess.Popen], force_kill: bool, timeout_sec: float = 5.0) -> bool:
    if proc is None or proc.poll() is not None:
        return True
    try:
        if force_kill:
            proc.kill()
            proc.wait(timeout=timeout_sec)
            return True
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


def assert_data_files_exist(data_dir: Path, db: str) -> None:
    db_dir = data_dir / db
    if not db_dir.exists():
        raise FirstTestError(f"database directory missing after checkpoint: {db_dir}")
    data_files = [p for p in db_dir.glob("data-*.dat") if p.is_file() and p.stat().st_size > 0]
    if not data_files:
        wal_path = db_dir / f"{db}.wal"
        wal_bytes = wal_path.stat().st_size if wal_path.exists() else 0
        raise FirstTestError(
            f"no persisted .dat files found in {db_dir}; wal_size={wal_bytes}. "
            "checkpoint shutdown did not flush pages as expected"
        )

    wal_path = db_dir / f"{db}.wal"
    if wal_path.exists() and wal_path.stat().st_size != 0:
        raise FirstTestError(
            f"WAL should be empty after graceful checkpoint, but {wal_path} has {wal_path.stat().st_size} bytes"
        )


def warmup_checkpoint(
    binary_path: Path,
    config_path: Path,
    data_dir: Path,
    base_url: str,
    db: str,
    metrics: Dict[str, MetricState],
    start_timeout_sec: float,
) -> None:
    proc = start_server(binary_path, config_path)
    try:
        wait_for_health(base_url, timeout_sec=start_timeout_sec, proc=proc)
        lines: List[str] = []
        for metric_name, st in metrics.items():
            val = st.base_value + st.sent_count
            lines.append(f"{db}/{metric_name} {val} {time.time_ns()}")
            st.sent_count += 1

        res = http_post_text(base_url + "/api/v1/import", "\n".join(lines) + "\n")
        if res.get("status") != "success":
            raise FirstTestError(f"warmup import failed: {res}")

        graceful = stop_server(proc, force_kill=False)
        if not graceful:
            raise FirstTestError("warmup graceful shutdown failed")
        proc = None
    finally:
        stop_server(proc, force_kill=True)

    assert_data_files_exist(data_dir, db)


def fetch_query_range(base_url: str, db: str, metric: str, start_sec: int, end_sec: int) -> List[Tuple[float, str]]:
    params = urllib.parse.urlencode(
        {
            "db": db,
            "query": metric,
            "start": str(start_sec),
            "end": str(end_sec),
        }
    )
    try:
        payload = http_get_json(f"{base_url}/api/v1/query_range?{params}")
    except urllib.error.HTTPError as exc:
        body = ""
        try:
            body = exc.read().decode("utf-8", errors="replace")
        except Exception:  # noqa: BLE001
            body = "<unavailable>"
        raise FirstTestError(
            f"query_range HTTP error for {db}/{metric}: HTTP {exc.code} {exc.reason}; body={body}"
        ) from exc
    if payload.get("status") != "success":
        raise FirstTestError(f"query_range failed for {db}/{metric}: {payload}")

    data = payload.get("data", {})
    result = data.get("result", [])
    if not result:
        return []

    values = result[0].get("values", [])
    out: List[Tuple[float, str]] = []
    for pair in values:
        if not isinstance(pair, list) or len(pair) != 2:
            raise FirstTestError(f"invalid value pair in query_range for {db}/{metric}: {pair}")
        sec = float(pair[0])
        val = str(pair[1])
        out.append((sec, val))
    return out


def fetch_query_last(base_url: str, db: str, metric: str) -> Optional[int]:
    params = urllib.parse.urlencode({"db": db, "query": metric})
    try:
        payload = http_get_json(f"{base_url}/api/v1/query?{params}")
    except urllib.error.HTTPError as exc:
        body = ""
        try:
            body = exc.read().decode("utf-8", errors="replace")
        except Exception:  # noqa: BLE001
            body = "<unavailable>"
        raise FirstTestError(
            f"query HTTP error for {db}/{metric}: HTTP {exc.code} {exc.reason}; body={body}"
        ) from exc
    if payload.get("status") != "success":
        raise FirstTestError(f"query failed for {db}/{metric}: {payload}")
    result = payload.get("data", {}).get("result", [])
    if not result:
        return None
    value_pair = result[0].get("value", [])
    if len(value_pair) != 2:
        raise FirstTestError(f"invalid query value pair for {db}/{metric}: {value_pair}")
    try:
        return int(str(value_pair[1]))
    except ValueError as exc:
        raise FirstTestError(f"invalid numeric query value for {db}/{metric}: {value_pair[1]}") from exc


def validate_metric_series(metric: str, points: List[Tuple[float, str]], st: MetricState) -> None:
    prev_ts: Optional[float] = None
    prev_val: Optional[int] = None
    min_allowed = st.base_value
    max_allowed = st.base_value + st.sent_count - 1

    for ts, raw_val in points:
        try:
            val = int(raw_val)
        except ValueError as exc:
            raise FirstTestError(f"non-int sample in {metric}: {raw_val}") from exc

        if prev_ts is not None and ts < prev_ts:
            raise FirstTestError(f"timestamps moved backwards in {metric}: {ts} < {prev_ts}")
        if prev_val is not None and val <= prev_val:
            raise FirstTestError(f"values not strictly increasing in {metric}: {val} <= {prev_val}")
        if val < min_allowed or (st.sent_count > 0 and val > max_allowed):
            raise FirstTestError(
                f"value out of expected range in {metric}: {val}, expected [{min_allowed}, {max_allowed}]"
            )

        prev_ts = ts
        prev_val = val


def run_test(args: argparse.Namespace) -> None:
    random.seed(args.seed)

    repo_root = Path(__file__).resolve().parents[1]
    data_dir = Path(args.data_dir).resolve()
    data_dir.mkdir(parents=True, exist_ok=True)
    config_path = data_dir / "engine.toml"
    write_engine_config(config_path, args.listen, args.wal_max_segment_size)
    binary_path = build_server_binary(repo_root)

    base_url = f"http://{args.listen}"
    db = args.db
    metrics: Dict[str, MetricState] = {
        "first.a": MetricState(base_value=1_000_000),
        "first.b": MetricState(base_value=2_000_000),
        "first.c": MetricState(base_value=3_000_000),
    }

    proc: Optional[subprocess.Popen] = None
    started_at_ns = time.time_ns()

    if args.warmup:
        warmup_checkpoint(
            binary_path=binary_path,
            config_path=config_path,
            data_dir=data_dir,
            base_url=base_url,
            db=db,
            metrics=metrics,
            start_timeout_sec=args.start_timeout_sec,
        )

    try:
        for i in range(args.iterations):
            proc = start_server(binary_path, config_path)
            wait_for_health(base_url, timeout_sec=args.start_timeout_sec, proc=proc)

            batch_size = random.randint(args.batch_min, args.batch_max)
            lines: List[str] = []
            for _ in range(batch_size):
                for metric_name, st in metrics.items():
                    val = st.base_value + st.sent_count
                    ts = time.time_ns()
                    lines.append(f"{db}/{metric_name} {val} {ts}")
                    st.sent_count += 1

            payload = "\n".join(lines) + "\n"
            try:
                res = http_post_text(base_url + "/api/v1/import", payload)
            except urllib.error.HTTPError as exc:
                body = ""
                try:
                    body = exc.read().decode("utf-8", errors="replace")
                except Exception:  # noqa: BLE001
                    body = "<unavailable>"
                raise FirstTestError(
                    f"import request failed on iteration {i}: HTTP {exc.code} {exc.reason}; body={body}"
                ) from exc
            except urllib.error.URLError as exc:
                raise FirstTestError(f"import request failed on iteration {i}: {exc}") from exc

            if res.get("status") != "success":
                raise FirstTestError(f"import failed on iteration {i}: {res}")

            time.sleep(random.uniform(args.sleep_min_sec, args.sleep_max_sec))

            stop_server(proc, force_kill=True)
            proc = None

            proc = start_server(binary_path, config_path)
            wait_for_health(base_url, timeout_sec=args.start_timeout_sec, proc=proc)

            start_sec = int(started_at_ns / 1_000_000_000) - 1
            end_sec = int(time.time()) + 1

            for metric_name, st in metrics.items():
                points = fetch_query_range(base_url, db, metric_name, start_sec, end_sec)
                received_count = len(points)
                validate_metric_series(metric_name, points, st)

                last = fetch_query_last(base_url, db, metric_name)
                if last is not None:
                    min_allowed = st.base_value
                    max_allowed = st.base_value + st.sent_count - 1
                    if last < min_allowed or last > max_allowed:
                        raise FirstTestError(
                            f"last value out of expected range in {metric_name}: {last}, expected [{min_allowed}, {max_allowed}]"
                        )
                
                # Report sent vs received counts
                lost_count = st.sent_count - received_count
                if lost_count > 0:
                    print(f"  {metric_name}: sent={st.sent_count}, received={received_count}, lost={lost_count}")
                else:
                    print(f"  {metric_name}: sent={st.sent_count}, received={received_count}")

            graceful = stop_server(proc, force_kill=False)
            if not graceful:
                raise FirstTestError("graceful checkpoint shutdown failed (process did not exit on SIGTERM)")
            proc = None
            assert_data_files_exist(data_dir, db)

            if (i + 1) % 10 == 0 or i == args.iterations - 1:
                print(f"[progress] iteration {i + 1}/{args.iterations} passed")

    finally:
        stop_server(proc, force_kill=True)

    print("[ok] first test completed without detected corruption")
    
    # Start server briefly for final query
    proc = start_server(binary_path, config_path)
    try:
        wait_for_health(base_url, timeout_sec=args.start_timeout_sec, proc=proc)
        start_sec = int(started_at_ns / 1_000_000_000) - 1
        end_sec = int(time.time()) + 1
        
        for metric_name, st in metrics.items():
            points = fetch_query_range(base_url, db, metric_name, start_sec, end_sec)
            received_count = len(points)
            lost_count = st.sent_count - received_count
            print(f"  {db}/{metric_name}: sent={st.sent_count}, received={received_count}, lost={lost_count}")
    finally:
        stop_server(proc, force_kill=True)


def parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(description="NanoTDB crash-safety first test")
    p.add_argument("--listen", default="127.0.0.1:18428", help="server listen address")
    p.add_argument("--data-dir", default="./devdata-first-test", help="data dir for test runs")
    p.add_argument("--db", default="first", help="database name used for inserts")
    p.add_argument("--iterations", type=int, default=100, help="number of crash/restart iterations")
    p.add_argument("--batch-min", type=int, default=1, help="minimum batches per iteration")
    p.add_argument("--batch-max", type=int, default=5, help="maximum batches per iteration")
    p.add_argument("--sleep-min-sec", type=float, default=0.0, help="minimum random sleep before kill")
    p.add_argument("--sleep-max-sec", type=float, default=0.02, help="maximum random sleep before kill")
    p.add_argument("--start-timeout-sec", type=float, default=10.0, help="server startup timeout")
    p.add_argument("--wal-max-segment-size", type=int, default=64 * 1024 * 1024, help="wal max segment size")
    p.add_argument("--seed", type=int, default=42, help="random seed")
    p.add_argument("--warmup", action=argparse.BooleanOptionalAction, default=True, help="seed metric mappings and checkpoint before chaos loop")
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
    except FirstTestError as exc:
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
