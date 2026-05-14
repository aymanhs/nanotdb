#!/usr/bin/env python3
import argparse
import csv
import json
from datetime import datetime, timedelta, timezone
from pathlib import Path


class Agg:
    def __init__(self) -> None:
        self.count = 0
        self.sum = 0
        self.min = None
        self.max = None

    def add(self, v: int) -> None:
        self.count += 1
        self.sum += v
        if self.min is None or v < self.min:
            self.min = v
        if self.max is None or v > self.max:
            self.max = v


def metric_name(idx: int) -> str:
    return f"temp.synthetic{idx:02d}"


def gap_metric_name(idx: int) -> str:
    return f"temp.gap_probe{idx:02d}"


def fmt_ts(dt: datetime) -> str:
    return dt.strftime("%Y-%m-%d %H:%M:%S") + ".000000000"


def write_lines(path: Path, lines: list[str]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text("\n".join(lines) + "\n", encoding="utf-8")


def main() -> int:
    p = argparse.ArgumentParser(description="Generate deterministic LP and expected rollups for full-cycle checks")
    p.add_argument("--out-dir", default="test-data/full-cycle-check/work", help="Output directory for generated files")
    p.add_argument("--duration-hours", type=int, default=30, help="Duration of generated data in hours")
    p.add_argument("--metrics", type=int, default=10, help="Number of rollup metrics to generate")
    p.add_argument("--gap-metrics", type=int, default=2, help="Number of non-rollup metrics with deterministic known gaps")
    p.add_argument("--cadence-seconds", type=int, default=10, help="Sample cadence in seconds")
    p.add_argument("--start-date", default="2026-01-01", help="Start date in YYYY-MM-DD (UTC)")
    p.add_argument("--source-db", default="source", help="Source database name")
    p.add_argument("--rollup-1h-db", default="source_rollup_1h", help="1h rollup database name")
    p.add_argument("--rollup-1d-db", default="source_rollup_1d", help="1d rollup database name")
    args = p.parse_args()

    if args.duration_hours <= 0:
        raise SystemExit("--duration-hours must be > 0")
    if args.metrics < 10:
        raise SystemExit("--metrics must be >= 10")
    if args.gap_metrics < 0:
        raise SystemExit("--gap-metrics must be >= 0")
    if args.cadence_seconds <= 0:
        raise SystemExit("--cadence-seconds must be > 0")

    out_dir = Path(args.out_dir)
    out_dir.mkdir(parents=True, exist_ok=True)
    start_dt = datetime.strptime(args.start_date, "%Y-%m-%d").replace(tzinfo=timezone.utc)

    rollup_metric_names = [metric_name(i) for i in range(args.metrics)]
    gap_metric_names = [gap_metric_name(i) for i in range(args.gap_metrics)]
    all_metric_names = rollup_metric_names + gap_metric_names
    duration_seconds = args.duration_hours * 3600
    ticks = duration_seconds // args.cadence_seconds

    # Deterministic, per-metric missing windows (in seconds from start).
    # These windows are written to known_gaps.csv for later targeted checks.
    gap_windows: dict[int, list[tuple[int, int]]] = {}
    for idx in range(args.gap_metrics):
        gap_windows[idx] = [
            (1800 + idx * 11, 90 + (idx % 5) * 10),
            (18 * 3600 + idx * 7, 120 + (idx % 3) * 15),
        ]

    def in_gap(gap_metric_idx: int, second_offset: int) -> bool:
        for start, length in gap_windows[gap_metric_idx]:
            if second_offset >= start and second_offset < start + length:
                return True
        return False

    source_lines: list[str] = []
    rollup_1h_lines: list[str] = []
    rollup_1d_lines: list[str] = []
    per_metric_written = [0 for _ in range(len(all_metric_names))]
    per_metric_dropped = [0 for _ in range(len(all_metric_names))]

    hour_agg: dict[tuple[int, int], Agg] = {}
    day_from_hour_agg: dict[tuple[int, int], Agg] = {}

    for tick in range(ticks):
        sec_offset = tick * args.cadence_seconds
        ts = start_dt + timedelta(seconds=sec_offset)
        ts_text = fmt_ts(ts)
        hour_bucket = sec_offset // 3600

        for metric_idx, metric in enumerate(rollup_metric_names):
            # Easy-to-reason value formula for rollup metrics:
            # value = metric_idx * 1000 + (second_offset % 1000)
            # Keeping values in a lower range avoids float32 sum rounding noise
            # in exported rollup lines while still providing deterministic variety.
            val = metric_idx * 1000 + (sec_offset % 1000)
            source_lines.append(f"{args.source_db}/{metric} {val} {ts_text}")
            per_metric_written[metric_idx] += 1

            key = (metric_idx, hour_bucket)
            agg = hour_agg.get(key)
            if agg is None:
                agg = Agg()
                hour_agg[key] = agg
            agg.add(val)

        for gap_idx, metric in enumerate(gap_metric_names):
            full_idx = len(rollup_metric_names) + gap_idx
            if in_gap(gap_idx, sec_offset):
                per_metric_dropped[full_idx] += 1
                continue
            # Gap probe metrics are present in raw data but not rolled up.
            val = 100_000 + gap_idx * 10_000 + sec_offset
            source_lines.append(f"{args.source_db}/{metric} {val} {ts_text}")
            per_metric_written[full_idx] += 1

    max_source_sec = -1
    if ticks > 0:
        max_source_sec = (ticks - 1) * args.cadence_seconds

    full_hour_buckets = set()
    for _, hour_bucket in hour_agg.keys():
        # Engine rollups only finalize fully closed source intervals.
        if (hour_bucket + 1) * 3600 <= max_source_sec:
            full_hour_buckets.add(hour_bucket)

    for (metric_idx, hour_bucket) in sorted(hour_agg.keys()):
        if hour_bucket not in full_hour_buckets:
            continue
        agg = hour_agg[(metric_idx, hour_bucket)]
        if agg.count == 0:
            continue
        hour_start = start_dt + timedelta(hours=hour_bucket)
        ts_hour = fmt_ts(hour_start)
        prefix = f"{args.rollup_1h_db}/{rollup_metric_names[metric_idx]}"
        rollup_1h_lines.append(f"{prefix}.min {agg.min} {ts_hour}")
        rollup_1h_lines.append(f"{prefix}.max {agg.max} {ts_hour}")
        rollup_1h_lines.append(f"{prefix}.sum {agg.sum} {ts_hour}")
        rollup_1h_lines.append(f"{prefix}.count {agg.count} {ts_hour}")

        day_bucket = (hour_bucket * 3600) // 86400
        day_key = (metric_idx, day_bucket)
        day_agg = day_from_hour_agg.get(day_key)
        if day_agg is None:
            day_agg = Agg()
            day_from_hour_agg[day_key] = day_agg
        day_agg.add(agg.sum)

    last_rollup_1h_ts_sec = -1
    if full_hour_buckets:
        last_rollup_1h_ts_sec = max(full_hour_buckets) * 3600

    for (metric_idx, day_bucket) in sorted(day_from_hour_agg.keys()):
        # 1d rollup source is the 1h.sum metric. It is also finalized only
        # when a complete 1d interval is closed relative to 1h source last TS.
        if (day_bucket + 1) * 86400 > last_rollup_1h_ts_sec:
            continue
        agg = day_from_hour_agg[(metric_idx, day_bucket)]
        if agg.count == 0:
            continue
        day_start = start_dt + timedelta(days=day_bucket)
        ts_day = fmt_ts(day_start)
        prefix = f"{args.rollup_1d_db}/{rollup_metric_names[metric_idx]}"
        rollup_1d_lines.append(f"{prefix}.min {agg.min} {ts_day}")
        rollup_1d_lines.append(f"{prefix}.max {agg.max} {ts_day}")
        rollup_1d_lines.append(f"{prefix}.sum {agg.sum} {ts_day}")
        rollup_1d_lines.append(f"{prefix}.count {agg.count} {ts_day}")

    write_lines(out_dir / "input.lp", source_lines)
    write_lines(out_dir / "expected_source.lp", source_lines)
    write_lines(out_dir / "expected_rollup_1h.lp", rollup_1h_lines)
    write_lines(out_dir / "expected_rollup_1d.lp", rollup_1d_lines)

    with (out_dir / "known_gaps.csv").open("w", encoding="utf-8", newline="") as f:
        w = csv.writer(f)
        w.writerow(["metric", "gap_start_utc", "gap_end_utc", "gap_seconds"])
        for idx, metric in enumerate(gap_metric_names):
            for gap_start, gap_len in gap_windows[idx]:
                if gap_start >= duration_seconds:
                    continue
                effective_len = min(gap_len, duration_seconds - gap_start)
                gap_start_dt = start_dt + timedelta(seconds=gap_start)
                gap_end_dt = gap_start_dt + timedelta(seconds=effective_len)
                w.writerow([metric, fmt_ts(gap_start_dt), fmt_ts(gap_end_dt), effective_len])

    theoretical_total = ticks * len(all_metric_names)
    actual_total = len(source_lines)
    duration_minutes = duration_seconds / 60.0
    summary = {
        "dataset": {
            "start_utc": fmt_ts(start_dt),
            "duration_hours": args.duration_hours,
            "duration_seconds": duration_seconds,
            "cadence_seconds": args.cadence_seconds,
            "rollup_metrics": args.metrics,
            "gap_probe_metrics": args.gap_metrics,
            "total_metrics": len(all_metric_names),
        },
        "counts": {
            "theoretical_source_points": theoretical_total,
            "actual_source_points": actual_total,
            "dropped_by_known_gaps": theoretical_total - actual_total,
            "expected_rollup_1h_rows": len(rollup_1h_lines),
            "expected_rollup_1d_rows": len(rollup_1d_lines),
        },
        "rates": {
            "records_per_second": (actual_total / duration_seconds) if duration_seconds > 0 else 0,
            "records_per_minute": (actual_total / duration_minutes) if duration_minutes > 0 else 0,
        },
        "files": {
            "input": str(out_dir / "input.lp"),
            "expected_source": str(out_dir / "expected_source.lp"),
            "expected_rollup_1h": str(out_dir / "expected_rollup_1h.lp"),
            "expected_rollup_1d": str(out_dir / "expected_rollup_1d.lp"),
            "known_gaps": str(out_dir / "known_gaps.csv"),
        },
        "per_metric": [
            {
                "metric": all_metric_names[idx],
                "role": "rollup" if idx < len(rollup_metric_names) else "gap_probe",
                "written_points": per_metric_written[idx],
                "dropped_points": per_metric_dropped[idx],
            }
            for idx in range(len(all_metric_names))
        ],
    }

    (out_dir / "scenario_summary.json").write_text(json.dumps(summary, indent=2) + "\n", encoding="utf-8")
    (out_dir / "SCENARIO.md").write_text(
        "\n".join(
            [
                "# Full-Cycle Dataset Scenario",
                "",
                f"- Start UTC: {summary['dataset']['start_utc']}",
                f"- Duration: {summary['dataset']['duration_hours']}h ({summary['dataset']['duration_seconds']}s)",
                f"- Cadence: every {summary['dataset']['cadence_seconds']}s",
                f"- Rollup metrics: {summary['dataset']['rollup_metrics']}",
                f"- Gap probe metrics: {summary['dataset']['gap_probe_metrics']}",
                f"- Total metrics: {summary['dataset']['total_metrics']}",
                f"- Actual source points: {summary['counts']['actual_source_points']}",
                f"- Gap-dropped points: {summary['counts']['dropped_by_known_gaps']}",
                f"- Records/minute: {summary['rates']['records_per_minute']:.3f}",
                "",
                "Artifacts:",
                f"- input.lp",
                f"- expected_source.lp",
                f"- expected_rollup_1h.lp",
                f"- expected_rollup_1d.lp",
                f"- known_gaps.csv",
                f"- scenario_summary.json",
            ]
        )
        + "\n",
        encoding="utf-8",
    )

    print(f"wrote {len(source_lines)} source lines to {out_dir / 'input.lp'}")
    print(f"wrote {len(rollup_1h_lines)} expected 1h rollup lines")
    print(f"wrote {len(rollup_1d_lines)} expected 1d rollup lines")
    print(f"wrote scenario summary to {out_dir / 'scenario_summary.json'}")
    print(f"wrote known gaps to {out_dir / 'known_gaps.csv'}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
