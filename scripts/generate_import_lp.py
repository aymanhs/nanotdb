#!/usr/bin/env python3
import argparse
import math
import random
from pathlib import Path


def build_metric_names(count: int) -> list[str]:
    names = []
    for i in range(count):
        names.append(f"sensors.room{i % 5 + 1}.metric{i + 1:02d}")
    return names


def main() -> int:
    p = argparse.ArgumentParser(
        description=(
            "Generate large nanotdb LP import files in format: DB/metric value ts\n"
            "with 2+ databases, dotted metrics, 10s spacing plus jitter."
        )
    )
    p.add_argument("--output", default="devdata/import-large.lp", help="Output LP file path")
    p.add_argument("--db-count", type=int, default=2, help="Number of databases")
    p.add_argument("--metrics", type=int, default=10, help="Metrics per database")
    p.add_argument(
        "--points-per-metric",
        type=int,
        default=50000,
        help="Points per metric (total lines = dbs * metrics * points)",
    )
    p.add_argument("--interval-sec", type=float, default=10.0, help="Base sample interval seconds")
    p.add_argument("--jitter-sec", type=float, default=1.5, help="Uniform jitter range (+/- sec)")
    p.add_argument(
        "--start-ts-ns",
        type=int,
        default=1_700_000_000_000_000_000,
        help="Starting timestamp in unix nanos",
    )
    p.add_argument("--seed", type=int, default=42, help="Random seed")
    p.add_argument(
        "--float-ratio",
        type=float,
        default=0.0,
        help="Fraction of metrics emitted as floats in [0,1]. Default 0 for byte-identical import/export checks.",
    )
    p.add_argument(
        "--db-prefix",
        default="prod",
        help="Database prefix. Generated names are <prefix>, <prefix>_1, <prefix>_2...",
    )
    args = p.parse_args()

    if args.db_count < 1:
        raise SystemExit("--db-count must be >= 1")
    if args.metrics < 1:
        raise SystemExit("--metrics must be >= 1")
    if args.points_per_metric < 1:
        raise SystemExit("--points-per-metric must be >= 1")
    if args.interval_sec <= 0:
        raise SystemExit("--interval-sec must be > 0")
    if args.float_ratio < 0 or args.float_ratio > 1:
        raise SystemExit("--float-ratio must be in [0,1]")

    random.seed(args.seed)

    out_path = Path(args.output)
    out_path.parent.mkdir(parents=True, exist_ok=True)

    db_names: list[str] = []
    for i in range(args.db_count):
        if i == 0:
            db_names.append(args.db_prefix)
        else:
            db_names.append(f"{args.db_prefix}_{i}")

    metrics = build_metric_names(args.metrics)

    total_lines = args.db_count * args.metrics * args.points_per_metric
    interval_ns = int(args.interval_sec * 1e9)
    jitter_ns = int(args.jitter_sec * 1e9)

    with out_path.open("w", encoding="utf-8", newline="\n") as f:
        # Write each database independently; inside each DB, timestamps are globally non-decreasing.
        for db_idx, db in enumerate(db_names):
            ts = args.start_ts_ns + db_idx * int(5e8)

            # Per-metric baseline values: half int-like, half float-like.
            baselines = [
                2000.0 + (i * 13.0) + random.uniform(-5.0, 5.0)
                for i in range(args.metrics)
            ]

            for step in range(args.points_per_metric):
                jitter = random.randint(-jitter_ns, jitter_ns)
                ts += max(1, interval_ns + jitter)

                # Write all metrics at this timestamp.
                for m_idx, metric in enumerate(metrics):
                    # Smooth trend + random noise to mimic telemetry.
                    drift = 0.003 * step
                    wave = 7.5 * math.sin(step / 60.0 + (m_idx * 0.3))
                    noise = random.uniform(-1.2, 1.2)
                    value = baselines[m_idx] + drift + wave + noise

                    # Float emission can be enabled, but int-only is default for exact round-trip checks.
                    if (m_idx / max(1, args.metrics - 1)) >= args.float_ratio:
                        value_text = str(int(round(value)))
                    else:
                        value_text = f"{value:.4f}"

                    f.write(f"{db}/{metric} {value_text} {ts}\n")

    print(f"wrote: {out_path}")
    print(f"databases: {args.db_count}")
    print(f"metrics per db: {args.metrics}")
    print(f"points per metric: {args.points_per_metric}")
    print(f"total lines: {total_lines}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
