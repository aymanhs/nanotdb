# Getting Started with NanoTDB

A guided installation and first-use walkthrough. For the absolute shortest
copy/paste path, see [HELLO_WORLD.md](HELLO_WORLD.md). For the full reference
material, see the topic docs linked at the bottom.

---

## Installation

### Option A: Prebuilt binaries (fastest)

Download from
[GitHub Releases](https://github.com/aymanhs/nanotdb/releases/latest):

- Raspberry Pi 0/1: `nanotdb-linux-armv6-rpi0-rpi1`, `nanocli-linux-armv6-rpi0-rpi1`, `drip-linux-armv6-rpi0-rpi1`
- Raspberry Pi 2/3/4, 32-bit: `nanotdb-linux-armv7-rpi3-rpi4`, `nanocli-linux-armv7-rpi3-rpi4`, `drip-linux-armv7-rpi3-rpi4`
- Raspberry Pi 64-bit: `nanotdb-linux-arm64`, `nanocli-linux-arm64`, `drip-linux-arm64`
- Linux x86_64: `nanotdb-linux-amd64`, `nanocli-linux-amd64`, `drip-linux-amd64`
- macOS Intel: `nanotdb-darwin-amd64`, `nanocli-darwin-amd64`
- macOS Apple Silicon: `nanotdb-darwin-arm64`, `nanocli-darwin-arm64`
- Windows x64: `nanotdb-windows-amd64.exe`, `nanocli-windows-amd64.exe`
- Windows ARM64: `nanotdb-windows-arm64.exe`, `nanocli-windows-arm64.exe`

Make Linux/macOS binaries executable:

```bash
chmod +x nanotdb-* nanocli-* drip-*
```

`drip` is optional — only needed if you want NanoTDB to collect and push host
metrics automatically. Prebuilt `drip` is published for Linux targets only.

### Option B: Build from source

Requirements: Go 1.20+, Git, Linux/macOS/Windows (WSL2 on Windows).

```bash
git clone <repository-url>
cd nanotdb

go build -o nanotdb ./cmd/nanotdb
go build -o nanocli ./cmd/nanocli
go build -o drip    ./cmd/drip   # optional collector
```

---

## First run

### 1. Create a data directory

```bash
mkdir -p ~/nanotdb-data
```

### 2. Initialize the config

```bash
./nanotdb --init --config ~/nanotdb-data/engine.toml
```

This writes a default `engine.toml` and a starter `dashboard.json`.

### 3. Start the server

```bash
./nanotdb --config ~/nanotdb-data/engine.toml
```

Defaults emit sparse `info` logs to stderr. To write logs to a file too,
add a `[[logging.logger]]` entry. See [CONFIGURATION.md](CONFIGURATION.md).

The server is now WAL-protecting recent samples and will replay the WAL on
restart after a crash. See [RECOVERY.md](RECOVERY.md) for the recovery
model and tuning knobs.

### 4. Open the UI

- <http://localhost:8428/> — the dashboard
- <http://localhost:8428/explore> — ad-hoc metric explorer
- <http://localhost:8428/dashboard/edit> — the in-browser editor
- <http://localhost:8428/engine> — engine view

See [DASHBOARD.md](DASHBOARD.md) for what each surface does.

---

## Push your first samples

Line protocol shape:

```text
database/metric.name value [timestamp_ns]
```

The name before the slash is the **database**, the name after is the
**metric**, and one written value is a **sample**. See [GLOSSARY.md](GLOSSARY.md)
for canonical definitions.

```bash
curl -X POST "http://localhost:8428/api/v1/import" \
  -d "mydb/temperature 23.5"

curl -X POST "http://localhost:8428/api/v1/import" \
  -d $'weather/temp.outdoor 22.1 1715000000000000000
weather/temp.indoor 21.5 1715000000000000000
weather/pressure 1013 1715000000000000000'
```

If you don't provide a timestamp, the current time is used.

For the full ingest, query, and discovery API, see [HTTP_API.md](HTTP_API.md).

---

## A small periodic shell script

```bash
#!/bin/bash
NANOTDB_URL="http://localhost:8428"
INTERVAL=60

while true; do
  TS=$(($(date +%s) * 1000000000))
  TEMP=$(echo "20 + $RANDOM % 10" | bc)
  HUM=$(echo  "50 + $RANDOM % 20" | bc)

  curl -X POST "$NANOTDB_URL/api/v1/import" \
    -d "sensors/room.temp $TEMP $TS
sensors/room.humidity $HUM $TS" 2>/dev/null

  echo "$(date): pushed temp=$TEMP, humidity=$HUM"
  sleep $INTERVAL
done
```

---

## Query the data

```bash
# Latest point
curl "http://localhost:8428/api/v1/query?query=sensors/room.temp"

# Range query
curl "http://localhost:8428/api/v1/query_range?query=sensors/room.temp&start=2026-05-01T00:00:00Z&end=2026-05-02T00:00:00Z&step=60s"

# Windowed aggregate
curl "http://localhost:8428/api/v1/query_range?query=sensors/room.temp&start=2026-05-01T00:00:00Z&end=2026-05-01T06:00:00Z&aggregate=avg&window=5m"
```

Full HTTP API reference: [HTTP_API.md](HTTP_API.md).

---

## Push from Python

```python
import requests, time

NANOTDB_URL = "http://localhost:8428"

def push(database, metric, value, ts_ns=None):
    ts_ns = ts_ns or int(time.time() * 1e9)
    line = f"{database}/{metric} {value} {ts_ns}"
    r = requests.post(f"{NANOTDB_URL}/api/v1/import", data=line)
    r.raise_for_status()

push("home", "kitchen.temperature", 22.5)
push("home", "kitchen.humidity",    55)
```

A range query:

```python
import requests
from datetime import datetime, timedelta, timezone

def query_recent(database, metric, hours=1):
    end   = datetime.now(timezone.utc)
    start = end - timedelta(hours=hours)
    r = requests.get(
        "http://localhost:8428/api/v1/query_range",
        params={
            "query": f"{database}/{metric}",
            "start": start.isoformat(),
            "end":   end.isoformat(),
        },
    )
    r.raise_for_status()
    return r.json()
```

---

## Inspect data offline with `nanocli`

While or after the server runs, you can look at the local files directly:

```bash
./nanocli inspect db      --root ~/nanotdb-data --db sensors --verbose
./nanocli inspect wal     --root ~/nanotdb-data --db sensors --verbose
./nanocli inspect dat     --root ~/nanotdb-data --db sensors --verbose
./nanocli inspect catalog --root ~/nanotdb-data --db sensors
```

Export to line protocol, import a backup, or run aggregate queries directly
against the data directory:

```bash
./nanocli export --root ~/nanotdb-data --db sensors --out backup.lp
./nanocli import --root ~/nanotdb-data --in   backup.lp

./nanocli query --root ~/nanotdb-data --db sensors --start 2m --format table
```

Full CLI reference: [NANOCLI.md](NANOCLI.md).

---

## Rollup backfill

Adding or changing rollups? Rebuild derived databases from existing source
data either offline or through the running server:

```bash
# Offline
./nanocli rollup --root ~/nanotdb-data
./nanocli rollup --root ~/nanotdb-data --db weather

# Online, against a running server
curl -X POST "http://localhost:8428/api/v1/rollup/backfill" \
  -H 'Content-Type: application/json' -d '{"source_db":"weather"}'
```

Full rollup reference: [ROLLUPS.md](ROLLUPS.md).

---

## Common issues

**Connection refused.** Make sure the server is running and listening on
the port you're hitting (default `8428`).

**404 Not Found on import.** Endpoint is `POST /api/v1/import`. Body shape
is `database/metric.name value [timestamp_ns]`.

**Python `requests` not installed.**

```bash
pip3 install requests
```

**Command not found: `nanotdb` / `nanocli`.** Either run them from where you
built them, or move them onto your `$PATH`:

```bash
sudo mv nanotdb nanocli /usr/local/bin/
```

---

## Next steps

- [DASHBOARD.md](DASHBOARD.md) — dashboard, editor, `dashboard.json` reference.
- [DRIP.md](DRIP.md) — the optional host metrics collector.
- [RUN_AS_A_SERVICE.md](RUN_AS_A_SERVICE.md) — systemd setup for a Pi or Linux box.
- [CONFIGURATION.md](CONFIGURATION.md) — `engine.toml` and `manifest.toml`.
- [HTTP_API.md](HTTP_API.md) and [NANOCLI.md](NANOCLI.md) — full API and CLI reference.
- [ROLLUPS.md](ROLLUPS.md) and [METRIC_FILES.md](METRIC_FILES.md) — downsampling and query-optimized storage.
- [RECOVERY.md](RECOVERY.md) — WAL behavior and durability tuning.
- [CONCEPTS.md](CONCEPTS.md) — friendly walkthrough of databases, metrics, partitions, WAL, and `data-*.dat` vs `metric-*.dat`.
- [ARCHITECTURE.md](ARCHITECTURE.md), [GLOSSARY.md](GLOSSARY.md), [DESIGN.md](DESIGN.md), [LAWS.md](LAWS.md) — deeper reference.
- [EMBEDDING.md](EMBEDDING.md) — embed the engine in your own Go program.
