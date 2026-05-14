#!/bin/sh
# push_sensors.sh — read 1-wire + CPU temp and push to nanotdb.
#
# Usage: ./push_sensors.sh [--url http://host:8428] [--interval 60] [-test] [-debug]
#
# Default interval: 60s (pass 0 to run once and exit).

NANOTDB_URL="http://localhost:8428"
INTERVAL=60
TEST_MODE=0
DEBUG_MODE=0

while [ $# -gt 0 ]; do
  case "$1" in
    --url)      NANOTDB_URL="$2"; shift 2 ;;
    --interval) INTERVAL="$2";    shift 2 ;;
    --test)     TEST_MODE=1;        shift 1 ;;
    --debug)    DEBUG_MODE=1;       shift 1 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done

# Map 1-wire device IDs to human-readable metric names.
sensor_name() {
  case "$1" in
    28-0120481ddd35) echo "temp.office_wet" ;;
    28-01214506d428) echo "temp.out_dry" ;;
    28-0121450f31ef) echo "temp.office_dry" ;;
    *)               echo "temp.$1" ;;   # fallback: safe, uses the raw ID
  esac
}

# Read one 1-wire sensor; prints "millidegrees" value or nothing on error.
read_w1_millidegrees() {
  slave="$1"
  # The file ends with: t=<millidegrees>
  line=$(grep -m1 "t=" "$slave" 2>/dev/null) || return 1
  # Make sure the sensor reported a valid CRC
  grep -q "YES" "$slave" 2>/dev/null || return 1
  echo "${line##*t=}"
}

# Read CPU temperature from the thermal zone (millidegrees Celsius).
read_cpu_millidegrees() {
  for f in /sys/class/thermal/thermal_zone*/temp; do
    [ -r "$f" ] || continue
    cat "$f" 2>/dev/null && return
  done
  return 1
}

collect_and_push() {
  # Capture nanosecond timestamp once for the whole batch.
  ts_ns=$(date +%s%N)
  lines=""

  # 1-wire sensors
  for slave in /sys/bus/w1/devices/28-*/w1_slave; do
    [ -e "$slave" ] || continue
    dev=$(basename "$(dirname "$slave")")
    milli=$(read_w1_millidegrees "$slave") || continue
    # Send raw millidegrees as int32 line protocol (suffix "i").
    val="${milli}i"
    name=$(sensor_name "$dev")
    line="sensors/$name $val $ts_ns"
    lines="${lines}${line}
"
  done

  # CPU temperature
  cpu_milli=$(read_cpu_millidegrees)
  if [ -n "$cpu_milli" ]; then
    cpu_val="${cpu_milli}i"
    lines="${lines}sensors/temp.cpu $cpu_val $ts_ns
"
  fi

  # Nothing to push
  [ -z "$lines" ] && return

  if [ "$TEST_MODE" -eq 1 ]; then
    # Test mode: print the sampled line protocol and quit without POST.
    printf '%s' "$lines"
    return
  fi

  if [ "$DEBUG_MODE" -eq 1 ]; then
    # Debug mode: show the exact payload that will be sent to curl.
    echo "--- curl payload begin ---"
    printf '%s' "$lines"
    echo "--- curl payload end ---"
  fi

  # POST all lines in one request
  http_code=$(printf '%s' "$lines" | curl -sS -o /dev/null -w "%{http_code}" \
    -X POST \
    -H "Content-Type: text/plain" \
    --data-binary @- \
    "$NANOTDB_URL/api/v1/import")

  if [ "$http_code" != "200" ]; then
    echo "$(date -Iseconds) push failed: HTTP $http_code" >&2
    return 1
  fi
}

if [ "$INTERVAL" -le 0 ] 2>/dev/null; then
  collect_and_push
elif [ "$TEST_MODE" -eq 1 ]; then
  collect_and_push
else
  while true; do
    collect_and_push || true
    sleep "$INTERVAL"
  done
fi
