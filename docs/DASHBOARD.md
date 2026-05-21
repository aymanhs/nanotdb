# Dashboard

NanoTDB includes a lightweight browser UI served by the `internal/web` package.
The UI stays thin on the server side and does its rendering and refresh behavior
in the browser.

## Pages

- `/` and `/dashboard` serve the configurable dashboard.
- `/explore` serves the manual database and metric explorer.
- `/engine` serves the operational engine view for database, file, runtime, and active settings inspection.

## Getting Started

Initialize a data directory:

```bash
./nanotdb --init --config ~/nanotdb-data/engine.toml
```

That creates:

- `engine.toml` for server configuration.
- `dashboard.json` for the default dashboard layout.

Start the server:

```bash
./nanotdb --config ~/nanotdb-data/engine.toml
```

Then open:

- `http://localhost:8428/`
- `http://localhost:8428/dashboard`
- `http://localhost:8428/explore`
- `http://localhost:8428/engine`

## Dashboard Config

`dashboard.json` is file-backed and editable. The embedded default sample is tuned
to the metric names emitted by `drip` in [cmd/drip/drip.toml](../cmd/drip/drip.toml).

Example:

```json
{
  "title": "NanoTDB Dashboard",
  "default_db": "metrics",
  "groups": [
    {
      "id": "overview",
      "label": "Overview",
      "widgets": ["system_snapshot", "load_history", "storage_snapshot"]
    },
    {
      "id": "temperatures",
      "label": "Temperatures",
      "widgets": ["temperature_snapshot", "temperature_history"]
    }
  ],
  "widgets": {
    "system_snapshot": {
      "type": "numbers",
      "title": "System Snapshot",
      "refresh_sec": 10,
      "series": [
        { "label": "Load 1m", "metric": "sys.load1", "transform": { "decimals": 2 } },
        { "label": "Load 5m", "metric": "sys.load5", "transform": { "decimals": 2 } },
        { "label": "Mem Avail", "metric": "mem.available", "transform": { "factor": 0.000001, "unit": " GB", "decimals": 1 }, "thresholds": { "direction": "below", "warning": 1.0, "critical": 0.5 } },
        { "label": "CPU Clock", "metric": "cpu.freq_khz", "transform": { "factor": 0.000001, "unit": " GHz", "decimals": 2 } },
        { "label": "CPU Temp", "metric": "temp.cpu", "transform": { "factor": 0.001, "unit": " C", "decimals": 1 }, "thresholds": { "direction": "above", "warning": 70, "critical": 80 } }
      ]
    },
    "load_history": {
      "type": "line_chart",
      "title": "Load Average",
      "refresh_sec": 15,
      "lookback": "6h",
      "interval": "1m",
      "series": [
        { "label": "Load 1m", "metric": "sys.load1" },
        { "label": "Load 5m", "metric": "sys.load5" },
        { "label": "Load 15m", "metric": "sys.load15" }
      ]
    },
    "storage_snapshot": {
      "type": "numbers",
      "title": "Storage Snapshot",
      "refresh_sec": 15,
      "series": [
        { "label": "Root Used %", "metric": "diskfs.root.used_pct", "transform": { "unit": "%", "decimals": 1 }, "thresholds": { "direction": "above", "warning": 80, "critical": 90 } },
        { "label": "Root Free", "metric": "diskfs.root.bytes_avail", "transform": { "factor": 0.0000000009313225746154785, "unit": " GiB", "decimals": 1 }, "thresholds": { "direction": "below", "warning": 8, "critical": 4 } },
        { "label": "SD Write ms", "metric": "disk.sd_write_probe_ms", "transform": { "unit": " ms", "decimals": 1 }, "thresholds": { "direction": "above", "warning": 25, "critical": 50 } }
      ]
    },
    "temperature_snapshot": {
      "type": "numbers",
      "title": "Temperatures",
      "refresh_sec": 15,
      "series": [
        { "label": "CPU", "metric": "temp.cpu", "transform": { "factor": 0.001, "unit": " C", "decimals": 1 }, "thresholds": { "direction": "above", "warning": 70, "critical": 80 } },
        { "label": "Office Dry", "metric": "temp.office_dry.mdeg", "transform": { "factor": 0.001, "unit": " C", "decimals": 1 } },
        { "label": "Office Wet", "metric": "temp.office_wet.mdeg", "transform": { "factor": 0.001, "unit": " C", "decimals": 1 } },
        { "label": "Outdoor", "metric": "temp.out_dry.mdeg", "transform": { "factor": 0.001, "unit": " C", "decimals": 1 } }
      ]
    },
    "temperature_history": {
      "type": "line_chart",
      "title": "Temperature History",
      "refresh_sec": 30,
      "lookback": "12h",
      "interval": "2m",
      "series": [
        { "label": "CPU", "metric": "temp.cpu", "transform": { "factor": 0.001, "unit": " C", "decimals": 1 } },
        { "label": "Office Dry", "metric": "temp.office_dry.mdeg", "transform": { "factor": 0.001, "unit": " C", "decimals": 1 } },
        { "label": "Office Wet", "metric": "temp.office_wet.mdeg", "transform": { "factor": 0.001, "unit": " C", "decimals": 1 } },
        { "label": "Outdoor", "metric": "temp.out_dry.mdeg", "transform": { "factor": 0.001, "unit": " C", "decimals": 1 } }
      ]
    }
  }
}
```

UI-only display conversion is configured per series using `transform`, for example
`{"factor": 0.001, "unit": " C", "decimals": 1}` to convert millidegrees to
degrees in the browser.

## Web Config

The dashboard-related settings live under `[web]` in `engine.toml`:

- `enabled` enables or disables the web handlers.
- `base_path` sets the dashboard route prefix, default `/dashboard`.
- `explore_path` sets the manual explorer route prefix, default `/explore`.
- `engine_path` sets the engine explorer route prefix, default `/engine`.
- `title` sets the browser page title.
- `refresh_seconds` sets the default UI refresh cadence.
- `dashboard_config` points at the dashboard JSON file.
- `web_root` points at a filesystem directory that overrides the embedded UI bundle.
- `api_base_url` sets the absolute API base the browser should call when the UI is hosted separately.

Example:

```toml
[web]
enabled = true
base_path = "/dashboard"
explore_path = "/explore"
engine_path = "/engine"
title = "NanoTDB Dashboard"
refresh_seconds = 10
dashboard_config = "dashboard.json"
web_root = "ui"
api_base_url = ""
```

## Editable UI Assets

To export the embedded UI bundle for editing:

```bash
./nanotdb --export-web-assets ./ui
```

Then set `[web].web_root` to that directory. NanoTDB will serve these files from
disk instead of the embedded bundle:

- `dashboard.html`
- `index.html`
- `engine.html`
- `dashboard_assets/`
- `assets/`
- `engine_assets/`
- `common_assets/`

This lets you edit HTML, CSS, and JavaScript without rebuilding the Go binary.
If you host the exported UI separately from the NanoTDB process, set `[web].api_base_url`
so the browser pages call the NanoTDB API at the correct origin.

## API Endpoints Used By The UI

- `GET /api/dashboard-config`
- `GET /api/v1/databases`
- `GET /api/v1/metrics?db=<name>`
- `GET /api/v1/query`
- `GET /api/v1/query_range`
- `GET /api/engine/overview`
- `GET /api/engine/database?db=<name>`
- `GET /api/engine/files?db=<name>`
- `GET /api/engine/runtime?db=<name>`

## Sample Rollup Fixture

To run NanoTDB against the rollup-enabled fixture and keep appending fresh points
every 10 seconds:

```bash
./scripts/run_sample_rollup_server.sh
```

Defaults:

- root dir: `test-data/full-cycle-check`
- config: `test-data/full-cycle-check/engine.toml`
- dashboard config: `test-data/full-cycle-check/dashboard.json`
- ingest interval: `10` seconds
- base URL: `http://127.0.0.1:8428`
- metrics per tick: `10` (`temp.synthetic00` .. `temp.synthetic09`)
- source DB: `source`

Optional arguments:

```bash
./scripts/run_sample_rollup_server.sh <root-dir> <config-path> <interval-seconds> <base-url> <metric-count> <source-db>
```

This keeps the server in the foreground and prints an ingest tick log. Stop with
`Ctrl+C`.