# HTTP API

NanoTDB exposes a small HTTP API on the address from `engine.listen`
(default `:8428`). The shape is compatible with VictoriaMetrics's instant
and range query wire format.

All examples below assume `http://localhost:8428`.

---

## Health

```
GET /health
```

Returns `OK` when the server is up.

---

## Ingest

### `POST /api/v1/import`

Accepts line protocol in the request body. One sample per line:

```text
database/metric.name value [timestamp_ns]
```

Examples:

```bash
curl -X POST "http://localhost:8428/api/v1/import" \
  -d "mydb/temperature 23.5"

curl -X POST "http://localhost:8428/api/v1/import" \
  -d $'weather/temp.outdoor 22.1 1715000000000000000
weather/temp.indoor 21.5 1715000000000000000
weather/pressure 1013 1715000000000000000'
```

If `timestamp_ns` is omitted, the current time is used.

Value rules:

- An integer literal (`42`, `-7`) creates an `int32` metric.
- A float literal (`3.14`, `1e-3`) creates a `float32` metric.
- Use an `i` suffix (`256i`) to force integer interpretation for values that
  look like floats.
- The value type is fixed on first write. Mixing types for the same metric
  is rejected.

### `POST /api/v1/import/prometheus`

Prometheus-compatible import endpoint for collectors that already emit
Prometheus text format.

---

## Query

### `GET /api/v1/query` — instant query

Returns the latest point for a metric.

```bash
curl "http://localhost:8428/api/v1/query?query=sensors/room.temp"
```

### `GET /api/v1/query_range` — range query

```bash
curl "http://localhost:8428/api/v1/query_range?query=sensors/room.temp&start=2026-05-01T00:00:00Z&end=2026-05-02T00:00:00Z&step=60s"
```

Parameters:

- `query` — `database/metric.name`
- `start` — RFC3339, `YYYY-MM-DD [HH[:MM[:SS[.nnnnnnnnn]]]]`, or Unix seconds/ns
- `end`   — same formats as `start` (optional for aggregate queries; defaults to now)
- `step`  — sampling stride for raw queries (`60s`, `5m`, etc.)

### `GET /api/v1/query_range` with aggregates

For windowed aggregation, pass `aggregate=<csv>` and `window=<duration>`
instead of `step`:

```bash
curl "http://localhost:8428/api/v1/query_range?query=home_sensors/living_room.temp&start=2026-05-24T12:00:00Z&end=2026-05-24T13:00:00Z&aggregate=min,max,sum,avg,count&window=5m"
```

Rules:

- `aggregate` and `window` are required together.
- `step` is rejected when `aggregate` and `window` are used together.
- Aggregate queries must match exactly one metric.
- Output rows are emitted at bucket-end timestamps; the first and last bucket
  are clipped to the requested range.

### `GET /api/v1/aggregates`

Returns the aggregate names the server supports:

```bash
curl "http://localhost:8428/api/v1/aggregates"
```

Current set: `avg`, `count`, `max`, `median`, `min`, `p50`, `p95`, `p99`,
`sum`, `trimmed_avg`, `trimmed_average`.

---

## Discovery

### `GET /api/v1/databases`

List user databases.

```bash
curl "http://localhost:8428/api/v1/databases"
```

Response:

```json
{
  "status": "success",
  "data": {
    "resultType": "databases",
    "result": ["home_sensors", "weather"]
  }
}
```

Include the internal stats database:

```bash
curl "http://localhost:8428/api/v1/databases?include_internal=true"
```

### `GET /api/v1/metrics?db=<name>`

List metrics in one database.

```bash
curl "http://localhost:8428/api/v1/metrics?db=sensors"
```

Add `&details=true` for `id` and `type`:

```bash
curl "http://localhost:8428/api/v1/metrics?db=sensors&details=true"
```

```json
{
  "status": "success",
  "data": {
    "resultType": "metrics",
    "db": "sensors",
    "result": [
      {"name": "room.humidity", "id": 1, "type": "float32"},
      {"name": "room.temp",     "id": 2, "type": "float32"}
    ]
  }
}
```

---

## Rollup backfill

### `POST /api/v1/rollup/backfill`

Rebuild rollup destination databases through the live engine. See
[ROLLUPS.md](ROLLUPS.md) for the full discussion.

One source DB:

```bash
curl -X POST "http://localhost:8428/api/v1/rollup/backfill" \
  -H 'Content-Type: application/json' \
  -d '{"source_db":"weather"}'
```

A list:

```bash
curl -X POST "http://localhost:8428/api/v1/rollup/backfill" \
  -H 'Content-Type: application/json' \
  -d '{"source_dbs":["weather","sensors"]}'
```

All discovered sources:

```bash
curl -X POST "http://localhost:8428/api/v1/rollup/backfill" \
  -H 'Content-Type: application/json' \
  -d '{}'
```

The endpoint clears rebuildable destination state, runs the rollups, and
persists rebuilt destination `.dat` pages and `catalog.json` before
returning.

---

## Dashboard config API

Used by the in-browser editor. See [DASHBOARD.md](DASHBOARD.md) for the full
walkthrough.

- `GET  /api/dashboard-config` — current `dashboard.json`
- `POST /api/dashboard-config/validate` — validate a draft without writing
- `PUT  /api/dashboard-config` — persist a draft (server writes a backup of the
  previous file and returns its path)

---

## Engine view API

Powers the operational engine view at `/engine`. Generally read-only.

- `GET /api/engine/overview`
- `GET /api/engine/database?db=<name>`
- `GET /api/engine/files?db=<name>`
- `GET /api/engine/runtime`
