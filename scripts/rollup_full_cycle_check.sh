#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="${1:-test-data/full-cycle-check}"
DURATION_HOURS="${2:-30}"
METRICS="${3:-10}"
CADENCE_SECONDS="${4:-10}"
GAP_METRICS="${5:-2}"

cd "$(dirname "$0")/.."

if [[ ! -f "go.mod" ]]; then
  echo "error: run this script inside the nanotdb repository" >&2
  exit 1
fi

if ! command -v python3 >/dev/null 2>&1; then
  echo "error: python3 is required" >&2
  exit 1
fi

WORK_DIR="$ROOT_DIR/work"
SOURCE_DB="source"
ROLLUP_1H_DB="source_rollup_1h"
ROLLUP_1D_DB="source_rollup_1d"

if [[ "$DURATION_HOURS" -le 0 ]]; then
  echo "error: duration hours must be > 0" >&2
  exit 1
fi
if [[ "$METRICS" -lt 10 ]]; then
  echo "error: metrics must be >= 10" >&2
  exit 1
fi
if [[ "$CADENCE_SECONDS" -le 0 ]]; then
  echo "error: cadence seconds must be > 0" >&2
  exit 1
fi
if [[ "$GAP_METRICS" -lt 0 ]]; then
  echo "error: gap metrics must be >= 0" >&2
  exit 1
fi

metric_name() {
  printf "temp.synthetic%02d" "$1"
}

echo "[1/7] Resetting full-cycle test root at $ROOT_DIR"
rm -rf "$ROOT_DIR"
mkdir -p "$ROOT_DIR/$SOURCE_DB" "$ROOT_DIR/$ROLLUP_1H_DB" "$ROOT_DIR/$ROLLUP_1D_DB" "$WORK_DIR"

cat > "$ROOT_DIR/engine.toml" <<'EOF'
[engine]
listen = ":8428"

[wal]
max_segment_size = 67108864
fsync_policy = "segment"

[durability]
profile = "strict"

[stats]
enabled = true
interval = "30s"

[defaults]
databases = ["source_rollup_1h", "source_rollup_1d"]

[manifest_defaults.retention]
grace = "5m"
retention_days = 30
max_active_days = 2
partition = "day"

[manifest_defaults.wal]
enabled = true
skip_before = "1h"

[manifest_defaults.page]
max_records = 16000
max_bytes = 127000
max_age = "60s"

[manifest_defaults.rollups]
enabled = false
checkpoint_file = "rollup.checkpoints.log"
default_grace = ""
EOF

cat > "$ROOT_DIR/$SOURCE_DB/manifest.toml" <<'EOF'
[retention]
  grace = "5m"
  retention_days = 30
  max_active_days = 2
  partition = "day"

[wal]
  enabled = true
  skip_before = "1h"

[page]
  max_records = 16000
  max_bytes = 127000
  max_age = "60s"

[rollups]
  enabled = true
  checkpoint_file = "rollup.checkpoints.log"
  default_grace = "5m"
EOF

for ((i=0; i<METRICS; i++)); do
  m="$(metric_name "$i")"
  cat >> "$ROOT_DIR/$SOURCE_DB/manifest.toml" <<EOF

[[rollups.jobs]]
  id = "${m}_1h"
  source_metric = "${m}"
  interval = "1h"
  aggregates = ["min", "max", "sum", "count"]
  destination_db = "source_rollup_1h"
  destination_metric_prefix = "${m}"
EOF
done

cat > "$ROOT_DIR/$ROLLUP_1H_DB/manifest.toml" <<'EOF'
[retention]
  grace = "5m"
  retention_days = 30
  max_active_days = 30
  partition = "month"

[wal]
  enabled = true
  skip_before = "1h"

[page]
  max_records = 16000
  max_bytes = 127000
  max_age = "60s"

[rollups]
  enabled = true
  checkpoint_file = "rollup.checkpoints.log"
  default_grace = "5m"
EOF

for ((i=0; i<METRICS; i++)); do
  m="$(metric_name "$i")"
  cat >> "$ROOT_DIR/$ROLLUP_1H_DB/manifest.toml" <<EOF

[[rollups.jobs]]
  id = "${m}_1d_from_1h_sum"
  source_metric = "${m}.sum"
  interval = "24h"
  aggregates = ["min", "max", "sum", "count"]
  destination_db = "source_rollup_1d"
  destination_metric_prefix = "${m}"
EOF
done

cat > "$ROOT_DIR/$ROLLUP_1D_DB/manifest.toml" <<'EOF'
[retention]
  grace = "5m"
  retention_days = 30
  max_active_days = 30
  partition = "year"

[wal]
  enabled = true
  skip_before = "1h"

[page]
  max_records = 16000
  max_bytes = 127000
  max_age = "60s"

[rollups]
  enabled = false
  checkpoint_file = "rollup.checkpoints.log"
  default_grace = ""
EOF

echo "[2/7] Generating deterministic LP and expected rollups"
python3 ./scripts/generate_full_cycle_lp.py \
  --out-dir "$WORK_DIR" \
  --duration-hours "$DURATION_HOURS" \
  --metrics "$METRICS" \
  --gap-metrics "$GAP_METRICS" \
  --cadence-seconds "$CADENCE_SECONDS" \
  --source-db "$SOURCE_DB" \
  --rollup-1h-db "$ROLLUP_1H_DB" \
  --rollup-1d-db "$ROLLUP_1D_DB"

echo "[3/7] Importing generated LP"
go run ./cmd/nanocli import --root "$ROOT_DIR" --in "$WORK_DIR/input.lp"

echo "[4/7] Exporting source and rollup databases"
go run ./cmd/nanocli export --root "$ROOT_DIR" --db "$SOURCE_DB" --out "$WORK_DIR/export_source.lp"
go run ./cmd/nanocli export --root "$ROOT_DIR" --db "$ROLLUP_1H_DB" --out "$WORK_DIR/export_rollup_1h.lp"
go run ./cmd/nanocli export --root "$ROOT_DIR" --db "$ROLLUP_1D_DB" --out "$WORK_DIR/export_rollup_1d.lp"

echo "[5/7] Comparing source export to original input"
python3 ./scripts/lp_multiset_diff.py \
  --label "source round-trip" \
  --expected "$WORK_DIR/expected_source.lp" \
  --actual "$WORK_DIR/export_source.lp"

echo "[6/7] Comparing 1h rollup export to expected rollups"
python3 ./scripts/lp_multiset_diff.py \
  --label "1h rollup" \
  --expected "$WORK_DIR/expected_rollup_1h.lp" \
  --actual "$WORK_DIR/export_rollup_1h.lp"

echo "[7/7] Comparing 1d rollup export to expected rollups"
python3 ./scripts/lp_multiset_diff.py \
  --label "1d rollup" \
  --expected "$WORK_DIR/expected_rollup_1d.lp" \
  --actual "$WORK_DIR/export_rollup_1d.lp"

echo "success: full-cycle checks passed"
echo "  root: $ROOT_DIR"
echo "  generated input: $WORK_DIR/input.lp"
echo "  exported source: $WORK_DIR/export_source.lp"
echo "  exported 1h: $WORK_DIR/export_rollup_1h.lp"
echo "  exported 1d: $WORK_DIR/export_rollup_1d.lp"
echo "  scenario summary: $WORK_DIR/scenario_summary.json"
echo "  known gaps: $WORK_DIR/known_gaps.csv"
