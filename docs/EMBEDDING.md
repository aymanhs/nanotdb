# Embedding the engine in Go

NanoTDB's engine package can be embedded directly in a Go program when you
don't want to run a separate server. The same engine is what backs the
`nanotdb` binary.

This is useful when your application already owns the process lifecycle —
for example, a Go service that needs local metric history alongside its own
state — and you'd rather not run two binaries.

---

## Minimal example

```go
package main

import (
    "fmt"
    "strconv"
    "time"

    "github.com/aymanhs/nanotdb/internal/engine"
)

func main() {
    // 0 = default WAL segment size
    e, err := engine.OpenEngine("/data", 0)
    if err != nil {
        panic(err)
    }
    defer e.Close()

    // Ingest one sample.
    err = e.AddLine("sensors/temp 22.1 " + strconv.FormatInt(time.Now().UnixNano(), 10))
    if err != nil {
        panic(err)
    }

    // Range query.
    fromTS := time.Now().Add(-1 * time.Hour).UnixNano()
    toTS   := time.Now().UnixNano()
    err = e.QueryRange("sensors", "temp", fromTS, toTS, 1, func(s engine.Sample) error {
        fmt.Println(s.TS, s.Float32)
        return nil
    })
    if err != nil {
        panic(err)
    }

    // Last value (from in-memory catalog cache).
    sample, ok, err := e.QueryLast("sensors", "temp")
    _ = sample
    _ = ok
    _ = err

    // Bulk import / export.
    _ = e.ImportFile("backup.lp")
    _ = e.ExportFile("sensors", "backup.lp")
}
```

---

## Key types

| Type        | Description                                                         |
|-------------|---------------------------------------------------------------------|
| `Engine`    | Top-level coordinator. Safe for concurrent use.                     |
| `Database`  | One named DB with its own WAL, catalog, and data files.             |
| `Catalog`   | Metric name ↔ ID registry; persisted as JSON.                       |
| `Page`      | In-memory buffer of interleaved samples; flushed when full.         |
| `WAL`       | Single-file write-ahead log with compact v2 encoding.               |
| `Sample`    | Decoded data point from a query.                                    |
| `Timestamp` | `int64` Unix nanoseconds.                                           |
| `MetricID`  | `uint16` per-database metric address.                               |

---

## Lifecycle

- `OpenEngine(root, walSegmentSize)` reads `engine.toml` if present, replays
  WALs for each known database, and returns a ready engine.
- `AddLine` is the ingest path. It parses line protocol, opens or reuses the
  target database, appends to the WAL, and writes into the in-memory page.
- `QueryRange` iterates UTC partition windows for the requested time range.
  Each window scans the corresponding `data-*.dat` (or `metric-*.dat` if
  present), then the in-memory page for the current partition.
- `Close` flushes open pages and shuts the WAL down cleanly. Recent samples
  end up in durable `.dat` pages so a subsequent open does not need WAL
  replay.

---

## Ordering rules

Same rules as the server: timestamps must be monotonically non-decreasing
per metric across the write stream. Out-of-order or stale samples are
rejected at ingest time.

For backfill of older data, set the per-database `wal.skip_before` window
in the database manifest to skip WAL for samples below that threshold —
this avoids gigantic WAL churn during bulk loads.

---

## See also

- [ARCHITECTURE.md](ARCHITECTURE.md) — the storage and query walkthrough the
  engine API is built on.
- [GLOSSARY.md](GLOSSARY.md) — canonical meanings of `Engine`, `Database`,
  `Catalog`, `Page`, `WAL`, `Sample`, etc.
- [CONFIGURATION.md](CONFIGURATION.md) — `engine.toml` and `manifest.toml`
  reference (which the embedded engine reads the same way as the server).
