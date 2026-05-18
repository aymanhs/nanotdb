# Getting Started with NanoDB

A beginner-friendly guide to installing, running, and using NanoDB. For technical details, see [README.md](README.md).

---

## Installation

### Option A: Download Prebuilt Binaries (Fastest)

If you do not want to compile from source, download the latest binaries from
[GitHub Releases](https://github.com/aymanhs/nanotdb/releases/latest).

Pick the files that match your device:

- Raspberry Pi 0/1 (older boards): `nanotdb-linux-armv6-rpi0-rpi1`, `nanocli-linux-armv6-rpi0-rpi1`
- Raspberry Pi 2/3/4 with 32-bit OS: `nanotdb-linux-armv7-rpi3-rpi4`, `nanocli-linux-armv7-rpi3-rpi4`
- Raspberry Pi with 64-bit OS: `nanotdb-linux-arm64`, `nanocli-linux-arm64`
- Linux PC/server (x86_64): `nanotdb-linux-amd64`, `nanocli-linux-amd64`
- macOS Intel: `nanotdb-darwin-amd64`, `nanocli-darwin-amd64`
- macOS Apple Silicon: `nanotdb-darwin-arm64`, `nanocli-darwin-arm64`
- Windows x64: `nanotdb-windows-amd64.exe`, `nanocli-windows-amd64.exe`

Make binaries executable on Linux/macOS:

```bash
chmod +x nanotdb-* nanocli-*
```

Then run them directly from the download directory or move them into your `$PATH`.

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

---

## Quick Start

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

You should see output like:
```
Server listening on :8428
```

The server is now running and ready to receive data. Keep this terminal open, or run it in the background.

---

## Pushing Data

NanoDB accepts data in **line protocol** format:
```
database/metric_name value [timestamp]
```

### Example 1: Push data from the shell

In a new terminal, use `curl` to send data:

```bash
curl -X POST "http://localhost:8428/api/v1/import" \
  -d "mydb/temperature 23.5"

curl -X POST "http://localhost:8428/api/v1/import" \
  -d "mydb/humidity 65"
```

Each line becomes one data point. If you don't provide a timestamp, the current time is used.

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

### 3. Discover databases and metrics

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

### Inspect database

```bash
./nanocli inspect db --root ~/nanotdb-data --db home_sensors --verbose
```

Shows catalog, manifest, data/WAL summaries, and verbose DAT/WAL tables for one database.

Inspect only data files or WAL files:

```bash
./nanocli inspect dat --root ~/nanotdb-data --db home_sensors --verbose
./nanocli inspect wal --root ~/nanotdb-data --db home_sensors --verbose
```

Verbose terminal output is rendered as aligned tables. DAT output shows per-file and per-page size statistics; WAL output shows per-file size/decode/tail diagnostics. Human-readable output uses `start` plus `duration`; `--json` keeps raw timestamps.

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
