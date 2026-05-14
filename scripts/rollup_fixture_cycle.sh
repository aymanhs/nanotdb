#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="${1:-test-data/rollup}"
INPUT_LP="${2:-test-data/nano.nlp}"

cd "$(dirname "$0")/.."

if [[ ! -f "go.mod" ]]; then
  echo "error: run this script inside the nanotdb repository" >&2
  exit 1
fi

if [[ ! -f "$ROOT_DIR/engine.toml" ]]; then
  echo "error: missing engine.toml at $ROOT_DIR/engine.toml" >&2
  exit 1
fi

if [[ ! -f "$INPUT_LP" ]]; then
  echo "error: missing input line protocol file: $INPUT_LP" >&2
  exit 1
fi

echo "[1/4] Cleaning fixture databases under $ROOT_DIR"
for db in sensors sensors_rollup_1h sensors_rollup_1d internal; do
  db_dir="$ROOT_DIR/$db"
  if [[ -d "$db_dir" ]]; then
    find "$db_dir" -mindepth 1 -maxdepth 1 -type f ! -name "manifest.toml" -delete
  fi
done

echo "[2/4] Importing $INPUT_LP"
go run ./cmd/nanocli import --root "$ROOT_DIR" --in "$INPUT_LP"

echo "[3/4] Verifying file partition shapes"
shopt -s nullglob
h1_month=("$ROOT_DIR/sensors_rollup_1h"/data-[0-9][0-9][0-9][0-9]-[0-9][0-9].dat)
h1_day=("$ROOT_DIR/sensors_rollup_1h"/data-[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9].dat)
d1_year=("$ROOT_DIR/sensors_rollup_1d"/data-[0-9][0-9][0-9][0-9].dat)
d1_day=("$ROOT_DIR/sensors_rollup_1d"/data-[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9].dat)
d1_month=("$ROOT_DIR/sensors_rollup_1d"/data-[0-9][0-9][0-9][0-9]-[0-9][0-9].dat)

if [[ ${#h1_month[@]} -eq 0 ]]; then
  echo "error: expected at least one monthly 1h shard (data-YYYY-MM.dat)" >&2
  exit 1
fi
if [[ ${#h1_day[@]} -ne 0 ]]; then
  echo "error: found daily shards in 1h DB; expected monthly partition only" >&2
  exit 1
fi
if [[ ${#d1_year[@]} -eq 0 ]]; then
  echo "error: expected at least one yearly 1d shard (data-YYYY.dat)" >&2
  exit 1
fi
if [[ ${#d1_day[@]} -ne 0 || ${#d1_month[@]} -ne 0 ]]; then
  echo "error: found non-yearly shards in 1d DB" >&2
  exit 1
fi

echo "[4/4] Verifying rollup row counts"
get_row_count() {
  local db="$1"
  local metric_regex="$2"
  local end_ns
  end_ns="$(date +%s)000000000"
  go run ./cmd/nanocli query --root "$ROOT_DIR" --db "$db" --metric "$metric_regex" --start 0 --end "$end_ns" --format json \
    | python3 -c 'import json,sys; print(json.load(sys.stdin).get("row_count", 0))'
}

rows_h1=$(get_row_count "sensors_rollup_1h" "^temp\\.out_dry\\.sum$")
rows_d1=$(get_row_count "sensors_rollup_1d" "^temp\\.out_dry\\.avg$")

if [[ "$rows_h1" -le 0 ]]; then
  echo "error: expected 1h rollup rows for temp.out_dry.sum, got $rows_h1" >&2
  exit 1
fi
if [[ "$rows_d1" -le 0 ]]; then
  echo "error: expected 1d rollup rows for temp.out_dry.avg, got $rows_d1" >&2
  exit 1
fi

echo "success: fixture rebuilt and verified"
echo "  1h monthly shards: ${#h1_month[@]}"
echo "  1d yearly shards:  ${#d1_year[@]}"
echo "  rows 1h out_dry.sum: $rows_h1"
echo "  rows 1d out_dry.avg: $rows_d1"
