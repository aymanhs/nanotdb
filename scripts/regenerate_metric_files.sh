#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
repo_root=$(cd -- "$script_dir/.." && pwd)

nanocli_bin=${NANOCLI_BIN:-"$repo_root/nanocli"}
root=${1:-"$repo_root/test-data/metric-poc-big"}
db=${2:-"sensors"}

if [[ ! -x "$nanocli_bin" ]]; then
  echo "nanocli binary not found or not executable: $nanocli_bin" >&2
  echo "Set NANOCLI_BIN=/path/to/nanocli or place nanocli at $repo_root/nanocli" >&2
  exit 1
fi

"$nanocli_bin" metric build --root "$root" --db "$db" --verify