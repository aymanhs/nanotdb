#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage:
  benchmark_metric_files.sh --root <root-dir> --db <database> [options]

Options:
  --nanocli <path>        path to nanocli binary (default: ./nanocli or $NANOCLI_BIN)
  --metric <regex>        query regex used for perf runs (default: .*)
  --start <time>          optional query start time passed to nanocli query
  --end <time>            optional query end time passed to nanocli query
  --repeats <count>       query repetitions per mode/codec (default: 5)
  --codecs <list>         comma-separated codec list (default: s2,s2_better,zstd_fastest,zstd_default)
  --keep-work             keep temporary benchmark copies

This script uses only the nanocli binary plus standard shell tools. It does not require Go.
EOF
}

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
repo_root=$(cd -- "$script_dir/.." && pwd)
nanocli_bin=${NANOCLI_BIN:-"$repo_root/nanocli"}
root=""
db=""
metric_regex=".*"
start_text=""
end_text=""
repeats=5
codecs="s2,s2_better,zstd_fastest,zstd_default"
keep_work=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --nanocli)
      nanocli_bin=$2
      shift 2
      ;;
    --root)
      root=$2
      shift 2
      ;;
    --db)
      db=$2
      shift 2
      ;;
    --metric)
      metric_regex=$2
      shift 2
      ;;
    --start)
      start_text=$2
      shift 2
      ;;
    --end)
      end_text=$2
      shift 2
      ;;
    --repeats)
      repeats=$2
      shift 2
      ;;
    --codecs)
      codecs=$2
      shift 2
      ;;
    --keep-work)
      keep_work=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "unknown argument: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

if [[ -z "$root" || -z "$db" ]]; then
  usage >&2
  exit 2
fi

if [[ ! -x "$nanocli_bin" ]]; then
  echo "nanocli binary not found or not executable: $nanocli_bin" >&2
  exit 1
fi

sum_metric_bytes() {
  local db_dir=$1
  find "$db_dir" -maxdepth 1 -type f -name 'metric-*.dat' -exec wc -c {} + 2>/dev/null | awk '$2 != "total" {sum += $1} END {print sum + 0}'
}

sum_raw_bytes() {
  local db_dir=$1
  find "$db_dir" -maxdepth 1 -type f \( -name 'data-*.dat' -o -name 'raw-*.dat' \) -exec wc -c {} + 2>/dev/null | awk '$2 != "total" {sum += $1} END {print sum + 0}'
}

extract_duration_ms() {
  awk -F': ' '/"duration_ms"/ {gsub(",", "", $2); print $2; exit}'
}

run_query_total_ms() {
  local query_root=$1
  local mode=$2
  local total_ms=0
  local i elapsed_ms
  local cmd=("$nanocli_bin" query --root "$query_root" --db "$db" --metric "$metric_regex" --metric-files "$mode" --format json)
  if [[ -n "$start_text" ]]; then
    cmd+=(--start "$start_text")
  fi
  if [[ -n "$end_text" ]]; then
    cmd+=(--end "$end_text")
  fi
  for ((i = 0; i < repeats; i++)); do
    elapsed_ms=$("${cmd[@]}" | extract_duration_ms)
    [[ -z "$elapsed_ms" ]] && elapsed_ms=0
    total_ms=$((total_ms + elapsed_ms))
  done
  printf '%s\n' "$total_ms"
}

run_build_ms() {
  local build_root=$1
  local codec=$2
  local duration_ms
  duration_ms=$("$nanocli_bin" metric build --root "$build_root" --db "$db" --codec "$codec" --raw-ingest-action keep --verify --json | extract_duration_ms)
  [[ -z "$duration_ms" ]] && duration_ms=0
  printf '%s\n' "$duration_ms"
}

root=$(cd -- "$root" && pwd)
db_dir="$root/$db"
if [[ ! -d "$db_dir" ]]; then
  echo "database directory not found: $db_dir" >&2
  exit 1
fi

raw_bytes=$(sum_raw_bytes "$db_dir")
raw_query_total_ms=$(run_query_total_ms "$root" off)
raw_query_avg_ms=$(awk -v total="$raw_query_total_ms" -v reps="$repeats" 'BEGIN { printf "%.2f", total / reps }')
resolution_note=0
if [[ "$raw_query_total_ms" -eq 0 ]]; then
  resolution_note=1
fi

echo "Metric file benchmark"
echo "root=$root"
echo "db=$db"
echo "metric_regex=$metric_regex"
echo "repeats=$repeats"
echo "raw_bytes=$raw_bytes"
echo "raw_query_avg_ms=$raw_query_avg_ms"
echo
printf '%-14s %12s %12s %10s %12s %12s %9s\n' \
  codec metric_bytes size_pct build_ms raw_avg_ms metric_avg_ms speedup

IFS=',' read -r -a codec_list <<<"$codecs"
for codec in "${codec_list[@]}"; do
  codec=$(echo "$codec" | xargs)
  [[ -z "$codec" ]] && continue

  tmp_root=$(mktemp -d "${TMPDIR:-/tmp}/nanotdb-metric-bench-XXXXXX")
  if [[ "$keep_work" -eq 0 ]]; then
    trap 'rm -rf "$tmp_root"' EXIT
  fi
  cp -a "$root/." "$tmp_root/"

  build_ms=$(run_build_ms "$tmp_root" "$codec")

  metric_bytes=$(sum_metric_bytes "$tmp_root/$db")
  metric_query_total_ms=$(run_query_total_ms "$tmp_root" on)
  metric_query_avg_ms=$(awk -v total="$metric_query_total_ms" -v reps="$repeats" 'BEGIN { printf "%.2f", total / reps }')
  if [[ "$metric_query_total_ms" -eq 0 ]]; then
    resolution_note=1
  fi

  size_pct="-"
  if [[ "$raw_bytes" -gt 0 ]]; then
    size_pct=$(awk -v metric="$metric_bytes" -v raw="$raw_bytes" 'BEGIN { printf "%.2f%%", (metric * 100.0) / raw }')
  fi

  speedup="-"
  if [[ "$metric_query_total_ms" -gt 0 ]]; then
    speedup=$(awk -v raw="$raw_query_total_ms" -v metric="$metric_query_total_ms" 'BEGIN { printf "%.2fx", raw / metric }')
  fi

  printf '%-14s %12d %12s %10d %12s %12s %9s\n' \
    "$codec" "$metric_bytes" "$size_pct" "$build_ms" "$raw_query_avg_ms" "$metric_query_avg_ms" "$speedup"

  if [[ "$keep_work" -eq 0 ]]; then
    rm -rf "$tmp_root"
    trap - EXIT
  else
    echo "kept benchmark copy: $tmp_root" >&2
  fi
done

if [[ "$resolution_note" -eq 1 ]]; then
  echo
  echo "Note: one or more query timings were below 1 ms resolution. Increase --repeats or benchmark a larger metric/time range for clearer perf comparisons."
fi