#!/usr/bin/env python3
import argparse
from collections import Counter
from pathlib import Path


def load_lines(path: Path) -> Counter[str]:
    lines = []
    for raw in path.read_text(encoding="utf-8").splitlines():
        line = raw.strip()
        if not line or line.startswith("#"):
            continue
        lines.append(line)
    return Counter(lines)


def main() -> int:
    p = argparse.ArgumentParser(description="Compare two LP files as multisets of lines")
    p.add_argument("--expected", required=True, help="Expected LP file")
    p.add_argument("--actual", required=True, help="Actual LP file")
    p.add_argument("--label", default="lp-compare", help="Comparison label")
    p.add_argument("--show", type=int, default=10, help="How many mismatched lines to print")
    args = p.parse_args()

    expected = Path(args.expected)
    actual = Path(args.actual)

    exp = load_lines(expected)
    got = load_lines(actual)

    if exp == got:
        print(f"ok: {args.label} ({sum(exp.values())} lines)")
        return 0

    missing = exp - got
    unexpected = got - exp

    print(f"error: {args.label} mismatch")
    print(f"  expected lines: {sum(exp.values())}")
    print(f"  actual lines:   {sum(got.values())}")
    print(f"  missing lines:  {sum(missing.values())}")
    print(f"  extra lines:    {sum(unexpected.values())}")

    if missing:
        print("  sample missing:")
        for i, (line, count) in enumerate(missing.items()):
            if i >= args.show:
                break
            print(f"    x{count} {line}")
    if unexpected:
        print("  sample unexpected:")
        for i, (line, count) in enumerate(unexpected.items()):
            if i >= args.show:
                break
            print(f"    x{count} {line}")

    return 1


if __name__ == "__main__":
    raise SystemExit(main())
