#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="${1:-test-data/full-cycle-check}"
CONFIG_PATH="${2:-$ROOT_DIR/engine.toml}"
INTERVAL_SECONDS="${3:-10}"
BASE_URL="${4:-http://127.0.0.1:8428}"
METRIC_COUNT="${5:-10}"
SOURCE_DB="${6:-source}"

cd "$(dirname "$0")/.."

if [[ ! -f "go.mod" ]]; then
  echo "error: run this script inside the nanotdb repository" >&2
  exit 1
fi

if ! command -v go >/dev/null 2>&1; then
  echo "error: go is required" >&2
  exit 1
fi

if ! command -v curl >/dev/null 2>&1; then
  echo "error: curl is required" >&2
  exit 1
fi

if [[ ! -d "$ROOT_DIR" ]]; then
  echo "error: missing root dir: $ROOT_DIR" >&2
  exit 1
fi

if [[ ! -f "$CONFIG_PATH" ]]; then
  echo "error: missing config file: $CONFIG_PATH" >&2
  exit 1
fi

if [[ "$INTERVAL_SECONDS" -le 0 ]]; then
  echo "error: interval seconds must be > 0" >&2
  exit 1
fi

if [[ "$METRIC_COUNT" -le 0 ]]; then
  echo "error: metric count must be > 0" >&2
  exit 1
fi

log_dir="$ROOT_DIR/work"
mkdir -p "$log_dir"
server_log="$log_dir/live_server.log"

server_pid=""
cleanup() {
  if [[ -n "$server_pid" ]] && kill -0 "$server_pid" >/dev/null 2>&1; then
    kill "$server_pid" >/dev/null 2>&1 || true
    wait "$server_pid" 2>/dev/null || true
  fi
}
trap cleanup EXIT INT TERM

echo "starting nanotdb server"
echo "  config: $CONFIG_PATH"
echo "  root:   $ROOT_DIR"
echo "  log:    $server_log"

go run ./cmd/nanotdb -config "$CONFIG_PATH" >"$server_log" 2>&1 &
server_pid="$!"

echo "waiting for health endpoint at $BASE_URL/health"
ready=0
for _ in $(seq 1 60); do
  if curl -fsS "$BASE_URL/health" >/dev/null 2>&1; then
    ready=1
    break
  fi
  sleep 0.5
done

if [[ "$ready" -ne 1 ]]; then
  echo "error: server did not become healthy (see $server_log)" >&2
  exit 1
fi

echo "server ready"
echo "  dashboard: $BASE_URL/"
echo "  dashboard: $BASE_URL/dashboard"
echo "  explore:   $BASE_URL/explore"
echo "  api:       $BASE_URL/api/v1/query_range"
echo ""
echo "streaming synthetic points into $SOURCE_DB (Ctrl+C to stop)"

tick=0
while true; do
  ts_ns="$(date +%s%N)"
  payload=""

  for metric_idx in $(seq 0 $((METRIC_COUNT - 1))); do
    metric_name="$(printf 'temp.synthetic%02d' "$metric_idx")"
    value="$((1000 + metric_idx * 11 + (tick % 97)))"
    payload+="$SOURCE_DB/$metric_name $value $ts_ns\n"
  done

  if ! printf "%b" "$payload" | curl -fsS \
    -H "Content-Type: text/plain" \
    --data-binary @- \
    "$BASE_URL/api/v1/import" >/dev/null; then
    echo "$(date -u +%Y-%m-%dT%H:%M:%SZ) import failed" >&2
  else
    echo "$(date -u +%Y-%m-%dT%H:%M:%SZ) tick=$tick imported metrics=$METRIC_COUNT"
  fi

  tick=$((tick + 1))
  sleep "$INTERVAL_SECONDS"
done
