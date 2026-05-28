# Getting Started with NanoTDB

A beginner-friendly guide to installing, running, and using NanoTDB. For technical details, see [README.md](README.md).

If you want the shortest copy/paste path first, start with [docs/HELLO_WORLD.md](docs/HELLO_WORLD.md).

---

## Installation

### Option A: Download Prebuilt Binaries (Fastest)

If you do not want to compile from source, download the latest binaries from
[GitHub Releases](https://github.com/aymanhs/nanotdb/releases/latest).

Pick the files that match your device:

- Raspberry Pi 0/1 (older boards): `nanotdb-linux-armv6-rpi0-rpi1`, `nanocli-linux-armv6-rpi0-rpi1`, `drip-linux-armv6-rpi0-rpi1`
- Raspberry Pi 2/3/4 with 32-bit OS: `nanotdb-linux-armv7-rpi3-rpi4`, `nanocli-linux-armv7-rpi3-rpi4`, `drip-linux-armv7-rpi3-rpi4`
- Raspberry Pi with 64-bit OS: `nanotdb-linux-arm64`, `nanocli-linux-arm64`, `drip-linux-arm64`
- Linux PC/server (x86_64): `nanotdb-linux-amd64`, `nanocli-linux-amd64`, `drip-linux-amd64`
- macOS Intel: `nanotdb-darwin-amd64`, `nanocli-darwin-amd64`
- macOS Apple Silicon: `nanotdb-darwin-arm64`, `nanocli-darwin-arm64`
- Windows x64: `nanotdb-windows-amd64.exe`, `nanocli-windows-amd64.exe`
- Windows ARM64: `nanotdb-windows-arm64.exe`, `nanocli-windows-arm64.exe`

Make binaries executable on Linux/macOS:

```bash
chmod +x nanotdb-* nanocli-* drip-*
```

Then run them directly from the download directory or move them into your `$PATH`.

`drip` is optional. You only need it if you want NanoTDB to collect and push host metrics automatically. Prebuilt `drip` release assets are currently published for Linux targets only.

---

### Option B: Build from Source

### Prerequisites

- **Go** (version 1.20 or later) — [Download here](https://golang.org/dl/)
- **Git** — [Download here](https://git-scm.com/)
- Linux, macOS, or Windows (with WSL2)

### 1. Clone the repository

```bash
git clone <repository-url>
cd nanotdb
```

### 2. Build the server and CLI tool

```bash
go build -o nanotdb ./cmd/nanotdb
go build -o nanocli ./cmd/nanocli
```

Both binaries will be created in the current directory. You can now use them.

If you also want the optional host metrics collector:

```bash
go build -o drip ./cmd/drip
```

`drip` is intended for Linux-style host metric surfaces such as `/proc` and `/sys`. It can then POST host metrics into your running `nanotdb` instance using line protocol.

---

## Quick Start

This guide is the longer walkthrough. If you want the shortest path possible, use [docs/HELLO_WORLD.md](docs/HELLO_WORLD.md).

### 1. Create a data directory

```bash
mkdir -p ~/nanotdb-data
```

### 2. Initialize the database

```bash
./nanotdb --init --config ~/nanotdb-data/engine.toml
```

This creates a configuration file. You can edit it later if needed, but the defaults work fine for getting started.

### 3. Start the server

```bash
./nanotdb --config ~/nanotdb-data/engine.toml
```

With the default config, startup emits sparse `info` logs to stderr, including the listen address.

The server is now running and ready to receive data. Keep this terminal open, or run it in the background.

If you want file-backed logs, edit `~/nanotdb-data/engine.toml` and add logger entries like:

```toml
[logging]

[[logging.logger]]
output = "console"
level = "info"

[[logging.logger]]
output = "/tmp/nanotdb-debug.log"
level = "debug"
```

At this point NanoTDB is already WAL-protecting recent samples and will replay
that WAL on restart after a crash. For the recovery model and tuning knobs, see
[README.md](README.md) and [docs/GLOSSARY.md](docs/GLOSSARY.md).

---

## Pushing Data

Before the examples: the name before the slash is the database, the name after
the slash is the metric, and one written value is a sample. For the canonical
definitions, see [README.md](README.md) and [docs/GLOSSARY.md](docs/GLOSSARY.md).

NanoTDB accepts data in **line protocol** format:
```
database/metric_name value [timestamp]
```

Example:

```text
weather/outdoor.temp 22.1 1715000000000000000
```

That means database `weather`, metric `outdoor.temp`, value `22.1`, timestamp
`1715000000000000000`.

### Example 1: Push data from the shell

In a new terminal, use `curl` to send data:

```bash
curl -X POST "http://localhost:8428/api/v1/import" \
  -d "mydb/temperature 23.5"

curl -X POST "http://localhost:8428/api/v1/import" \
  -d "mydb/humidity 65"
```

Each line becomes one sample. If you don't provide a timestamp, the current time is used.

### Example 2: Push data with timestamps

```bash
curl -X POST "http://localhost:8428/api/v1/import" \
  -d "weather/temp.outdoor 22.1 1715000000000000000
weather/temp.indoor 21.5 1715000000000000000
weather/pressure 1013 1715000000000000000"
```

### Example 3: Simple shell script to push periodic data

Create a file `push_data.sh`:

```bash
#!/bin/bash

# Configuration
NANOTDB_URL="http://localhost:8428"
INTERVAL=60  # seconds

while true; do
  # Get current timestamp in nanoseconds
  TIMESTAMP_NS=$(($(date +%s) * 1000000000))
  
  # Generate some sample data (replace with real sensor data)
  TEMP=$(echo "20 + $RANDOM % 10" | bc)
  HUMIDITY=$(echo "50 + $RANDOM % 20" | bc)
  
  # Push data
  curl -X POST "$NANOTDB_URL/api/v1/import" \
    -d "sensors/room.temp $TEMP $TIMESTAMP_NS
sensors/room.humidity $HUMIDITY $TIMESTAMP_NS" \
    2>/dev/null
  
  echo "$(date): pushed temp=$TEMP, humidity=$HUMIDITY"
  sleep $INTERVAL
done
```

Make it executable:
```bash
chmod +x push_data.sh
./push_data.sh
```

---

## Querying Data

### 1. Query in the shell

```bash
curl "http://localhost:8428/api/v1/query?query=sensors/room.temp"
```

Returns data in JSON format.

### 2. Query with time range

```bash
curl "http://localhost:8428/api/v1/query_range?query=sensors/room.temp&start=2024-05-01T00:00:00Z&end=2024-05-02T00:00:00Z&step=60s"
```

### 3. Discover supported aggregates

```bash
curl "http://localhost:8428/api/v1/aggregates"
```

### 4. Query aggregate windows

```bash
curl "http://localhost:8428/api/v1/query_range?query=sensors/room.temp&start=2024-05-01T00:00:00Z&end=2024-05-01T06:00:00Z&step=5m&aggregate=avg"
```

### 5. Discover databases and metrics

List user databases:

```bash
curl "http://localhost:8428/api/v1/databases"
```

Example response:

```json
{
    "status": "success",
    "data": {
        "resultType": "databases",
        "result": ["home_sensors", "weather"]
    }
}
```

Include the internal database too:

```bash
curl "http://localhost:8428/api/v1/databases?include_internal=true"
```

Example response:

```json
{
    "status": "success",
    "data": {
        "resultType": "databases",
        "result": ["home_sensors", "internal", "weather"]
    }
}
```

List metrics in one database:

```bash
curl "http://localhost:8428/api/v1/metrics?db=sensors"
```

---

## Rollup Backfill

If you add or change rollup config and want to rebuild derived databases from existing source data, use one of these paths.

### Offline rebuild with `nanocli`

When the server is not running, recompute all discovered rollup sources:

```bash
./nanocli rollup --root ~/nanotdb-data
```

Or limit the rebuild to one source DB:

```bash
./nanocli rollup --root ~/nanotdb-data --db weather
```

### Online rebuild through the server API

When `nanotdb` is already running, use the engine-owned HTTP endpoint instead of editing files under a live server:

```bash
curl -X POST "http://localhost:8428/api/v1/rollup/backfill" \
    -H 'Content-Type: application/json' \
    -d '{"source_db":"weather"}'
```

To rebuild every discovered rollup source:

```bash
curl -X POST "http://localhost:8428/api/v1/rollup/backfill" \
    -H 'Content-Type: application/json' \
    -d '{}'
```

This endpoint clears rebuildable rollup destination state, reruns the rollups in the engine, and persists rebuilt destination files before returning.

Example response:

```json
{
    "status": "success",
    "data": {
        "resultType": "metrics",
        "db": "sensors",
        "result": ["room.humidity", "room.temp"]
    }
}
```

List metrics with metadata (metric id and type):

```bash
curl "http://localhost:8428/api/v1/metrics?db=sensors&details=true"
```

Example response:

```json
{
    "status": "success",
    "data": {
        "resultType": "metrics",
        "db": "sensors",
        "result": [
            {"name": "room.humidity", "id": 1, "type": "float32"},
            {"name": "room.temp", "id": 2, "type": "float32"}
        ]
    }
}
```

---

## Python Examples

### Example 1: Push data with Python

```python
#!/usr/bin/env python3
import requests
import time

NANOTDB_URL = "http://localhost:8428"

def push_data(database, metric, value, timestamp_ns=None):
    """Push a single data point to NanoDB."""
    if timestamp_ns is None:
        timestamp_ns = int(time.time() * 1e9)
    
    line = f"{database}/{metric} {value} {timestamp_ns}"
    
    response = requests.post(
        f"{NANOTDB_URL}/api/v1/import",
        data=line
    )
    
    if response.status_code == 200:
        print(f"✓ Pushed: {line}")
    else:
        print(f"✗ Error: {response.status_code} - {response.text}")

# Push some example data
push_data("home", "kitchen.temperature", 22.5)
push_data("home", "kitchen.humidity", 55)
push_data("home", "living_room.temperature", 21.8)
```

Save this as `push_example.py` and run:
```bash
pip install requests  # if not already installed
python3 push_example.py
```

### Example 2: Continuous data collection

```python
#!/usr/bin/env python3
import requests
import time
import random
from datetime import datetime

NANOTDB_URL = "http://localhost:8428"

def push_sensor_reading(db_name, metric_name, value):
    """Push a sensor reading to NanoDB."""
    timestamp_ns = int(time.time() * 1e9)
    line = f"{db_name}/{metric_name} {value} {timestamp_ns}"
    
    try:
        response = requests.post(
            f"{NANOTDB_URL}/api/v1/import",
            data=line,
            timeout=5
        )
        if response.status_code == 200:
            print(f"[{datetime.now().strftime('%H:%M:%S')}] ✓ {metric_name}: {value}")
            return True
        else:
            print(f"[{datetime.now().strftime('%H:%M:%S')}] ✗ Error: {response.status_code}")
            return False
    except Exception as e:
        print(f"[{datetime.now().strftime('%H:%M:%S')}] ✗ Connection error: {e}")
        return False

def simulate_sensors():
    """Simulate sensors and push data continuously."""
    print("Starting sensor simulation... (Ctrl+C to stop)")
    
    try:
        while True:
            # Simulate temperature reading (vary between 18-28°C)
            temp = 20 + 5 * random.gauss(0, 1)
            push_sensor_reading("home_sensors", "living_room.temperature", round(temp, 1))
            
            # Simulate humidity reading (vary between 40-70%)
            humidity = 55 + 10 * random.gauss(0, 1)
            push_sensor_reading("home_sensors", "living_room.humidity", round(humidity, 1))
            
            # Simulate pressure (around 1013 hPa)
            pressure = 1013 + 2 * random.gauss(0, 1)
            push_sensor_reading("weather", "barometric_pressure", round(pressure, 1))
            
            time.sleep(10)  # Push new data every 10 seconds
    
    except KeyboardInterrupt:
        print("\nStopped.")

if __name__ == "__main__":
    simulate_sensors()
```

Save this as `continuous_sensors.py` and run:
```bash
python3 continuous_sensors.py
```

### Example 3: Query data with Python

```python
#!/usr/bin/env python3
import requests
from datetime import datetime, timedelta

NANOTDB_URL = "http://localhost:8428"

def query_recent(database, metric, hours=1):
    """Query recent data points."""
    end_time = datetime.utcnow()
    start_time = end_time - timedelta(hours=hours)
    
    params = {
        "query": f"{database}/{metric}",
        "start": start_time.isoformat() + "Z",
        "end": end_time.isoformat() + "Z",
    }
    
    response = requests.get(
        f"{NANOTDB_URL}/api/v1/query_range",
        params=params
    )
    
    if response.status_code == 200:
        data = response.json()
        
        # Print results
        if "data" in data and "result" in data["data"]:
            results = data["data"]["result"]
            for series in results:
                metric_name = series.get("metric", {})
                values = series.get("values", [])
                print(f"\n{metric_name}:")
                for timestamp, value in values:
                    dt = datetime.fromtimestamp(int(timestamp))
                    print(f"  {dt.strftime('%Y-%m-%d %H:%M:%S')} = {value}")
        else:
            print("No data found")
    else:
        print(f"Error: {response.status_code}")

# Query last hour of living room temperature
query_recent("home_sensors", "living_room.temperature", hours=1)
```

---

## Using the Command-Line Tool (nanocli)

The `nanocli` tool lets you inspect and manage data without running the server.

`nanocli` stays quiet by default and only writes diagnostics if you opt in with `--log-file`. If you pass `--log-file` without `--log-level`, it defaults to `debug`. `--log-level` on its own is rejected.

### Inspect database

```bash
./nanocli inspect db --root ~/nanotdb-data --db home_sensors --verbose
```

Shows catalog, manifest, data/WAL summaries, and verbose DAT/WAL tables for one database.

Inspect only data files or WAL files:

```bash
./nanocli inspect dat --root ~/nanotdb-data --db home_sensors --verbose
./nanocli inspect wal --root ~/nanotdb-data --db home_sensors --verbose
./nanocli inspect wal --root ~/nanotdb-data --db home_sensors --log-file /tmp/nanocli.log --log-level trace
```

Verbose terminal output is rendered as aligned tables. DAT output shows per-file and per-page size statistics; WAL output shows per-file size/decode/tail diagnostics. Human-readable output uses `start` plus `duration`; `--json` keeps raw timestamps.

Inspect query-optimized metric files:

```bash
./nanocli inspect metric --root ~/nanotdb-data --db home_sensors --verbose
```

Build metric files for all discovered partitions with the configured codec and verify the result:

```bash
./nanocli build metric --root ~/nanotdb-data --db home_sensors --verify
```

Override the codec for one build without editing `engine.toml`:

```bash
./nanocli build metric --root ~/nanotdb-data --db home_sensors --codec zstd_default --verify
```

The default builder writes the current `v2` metric-file format. Use `--format v1`
only if you want a direct comparison against the legacy layout.

### Export data to a file

```bash
./nanocli export --root ~/nanotdb-data --db home_sensors --out backup.lp
```

Exports all data from a database to a line-protocol file.

### Import data from a file

```bash
./nanocli import --root ~/nanotdb-data --in data.lp
```

Bulk-import data from a line-protocol file.

### Query with nanocli

```bash
./nanocli query --root ~/nanotdb-data --db home_sensors --metric "living_room.*" --format table
```

Aggregate queries are windowed range queries over exactly one matched metric:

```bash
./nanocli query \
    --root ~/nanotdb-data \
    --db home_sensors \
    --metric '^living_room\.temp$' \
    --start 2026-05-24T12:00:00Z \
    --end 2026-05-24T13:00:00Z \
    --aggregate min,max,sum,avg,count \
    --window 5m \
    --format table
```

Rules worth remembering:

- supported aggregates are `min`, `max`, `sum`, `avg`, and `count`
- aggregate queries require both `--aggregate` and `--window`
- aggregate queries require `--start`; `--end` is optional
- output timestamps are bucket ends, with edge buckets clipped to your requested range

For diagnostics or benchmarking, force raw or metric-backed query routing regardless of config:

```bash
./nanocli query --root ~/nanotdb-data --db home_sensors --metric "living_room.*" --metric-files off --format json > /dev/null
./nanocli query --root ~/nanotdb-data --db home_sensors --metric "living_room.*" --metric-files on --format json > /dev/null
```

The HTTP API exposes the same aggregate behavior on `query_range` using
`aggregate=<csv>` and `window=<duration>`. Example:

```bash
curl "http://localhost:8428/api/v1/query_range?query=home_sensors/living_room.temp&start=2026-05-24T12:00:00Z&end=2026-05-24T13:00:00Z&aggregate=min,max,sum,avg,count&window=5m"
```

For aggregate HTTP queries, omit `step`; `query_range` rejects `step` when
`aggregate` and `window` are used together.

### Standalone metric-file benchmarks

You do not need Go installed to benchmark metric files on your own data. Use a released `nanocli` binary plus the shell script in [scripts/benchmark_metric_files.sh](/home/ayman/code/nanotdb/scripts/benchmark_metric_files.sh):

```bash
./scripts/benchmark_metric_files.sh \
    --nanocli ./nanocli \
    --root ~/nanotdb-data \
    --db home_sensors \
    --metric 'living_room.*' \
    --repeats 7
```

The script copies your data into temporary work directories, builds `metric-*.dat` files with each codec, verifies them, and prints a table with:
- raw bytes vs metric bytes
- build time per codec
- raw query average vs metric-file query average
- relative speedup so you can choose the best tradeoff for your own dataset

See [docs/METRIC_FILES.md](/home/ayman/code/nanotdb/docs/METRIC_FILES.md) for the detailed workflow and config guidance.

---

## Common Issues

### "Connection refused" when running scripts

- Make sure the server is running: `./nanotdb --config ~/nanotdb-data/engine.toml`
- Check that the port 8428 is correct in your script

### "404 Not Found" when pushing data

- Verify the endpoint: should be `http://localhost:8428/api/v1/import`
- Make sure the data format is correct: `database/metric_name value [timestamp]`

### Python scripts fail with "requests not installed"

```bash
pip3 install requests
```

### Command not found: nanotdb or nanocli

- Make sure you're in the directory where you ran `go build`, or move the binaries to a directory in your `$PATH`:
```bash
mv nanotdb nanocli ~/bin/  # if ~/bin exists
# or
sudo mv nanotdb nanocli /usr/local/bin/
```

---

## Next Steps

- Read the [README.md](README.md) for technical details and advanced configuration
- Check [DESIGN.md](docs/DESIGN.md) for architecture deep-dive
- Look at [scripts/](scripts/) for more real-world examples

---

## Support

For issues or questions:
1. Check existing GitHub issues
2. Review [docs/](docs/) for detailed documentation
3. Create a new issue with details about what you're trying to do
