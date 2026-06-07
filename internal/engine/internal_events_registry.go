package engine

// Registry of every internal event the engine and drip may emit. This is
// the authoritative source for:
//
//   - GET /api/v1/internal-events/catalog (HTTP)
//   - nanocli internal-events catalog
//   - validating SetInternalEventsGroup arguments
//
// The list mirrors the "Event catalog" tables in docs/INTERNAL_EVENTS.md.
// Adding a new emit site means adding the corresponding row here first,
// otherwise the catalog endpoint and the runtime group toggle will not
// know about the new event.
//
// One row per event name. Group is the toggle unit. Defaults for the
// group enable/disable state live in internalEventsGroupDefaults below
// — also mirrored from the "Group taxonomy" tables in the spec.

// internalEventDef describes a single internal event the engine or drip
// may emit. Used at runtime to answer the catalog endpoint and at config
// load to validate group names.
type internalEventDef struct {
	Name        string   // dotted event name, e.g. "nanotdb.partition.sealed"
	Group       string   // group key for enable/disable toggling
	ValueType   byte     // EventValueNone | Int32Sample | Float32Sample
	ValueUnits  string   // human-readable units for the typed value (e.g. "ms", "bytes"); empty for none
	PayloadKeys []string // JSON keys present in the payload
	Description string   // one-line description for catalog consumers
}

// internalEventsRegistry enumerates every internal event the engine and
// drip may emit. Sorted by group, then by name within group.
var internalEventsRegistry = []internalEventDef{
	// ---- nanotdb.lifecycle ----
	{
		Name: "nanotdb.engine.started", Group: "nanotdb.lifecycle", ValueType: Int32Sample, ValueUnits: "ms_to_ready",
		PayloadKeys: []string{"version", "root_dir", "db_count"},
		Description: "Engine startup completed.",
	},
	{
		Name: "nanotdb.engine.shutdown.clean", Group: "nanotdb.lifecycle", ValueType: Int32Sample, ValueUnits: "ms_to_drain",
		PayloadKeys: []string{"db_count"},
		Description: "Engine shut down cleanly via Close().",
	},
	{
		Name: "nanotdb.engine.shutdown.dirty", Group: "nanotdb.lifecycle", ValueType: EventValueNone,
		PayloadKeys: []string{"detected_at_startup", "prev_wal_bytes"},
		Description: "Previous shutdown left WAL non-empty; emitted by the next startup.",
	},
	{
		Name: "nanotdb.engine.flush.completed", Group: "nanotdb.lifecycle", ValueType: Int32Sample, ValueUnits: "bytes_written",
		PayloadKeys: []string{"dbs_flushed", "ms"},
		Description: "Engine-wide flush completed.",
	},
	{
		Name: "nanotdb.internal_events.group.toggled", Group: "nanotdb.lifecycle", ValueType: EventValueNone,
		PayloadKeys: []string{"group", "from", "to", "source"},
		Description: "An internal-events group was toggled (config or runtime).",
	},

	// ---- nanotdb.db ----
	{
		Name: "nanotdb.db.created", Group: "nanotdb.db", ValueType: EventValueNone,
		PayloadKeys: []string{"db", "partition_mode", "retention_action"},
		Description: "A new database was created.",
	},
	{
		Name: "nanotdb.db.deleted", Group: "nanotdb.db", ValueType: EventValueNone,
		PayloadKeys: []string{"db"},
		Description: "A database was deleted.",
	},
	{
		Name: "nanotdb.db.opened", Group: "nanotdb.db", ValueType: Int32Sample, ValueUnits: "ms_to_open",
		PayloadKeys: []string{"db", "metric_count", "event_count", "partition_count"},
		Description: "A database was opened at engine startup or first access.",
	},

	// ---- nanotdb.partition ----
	{
		Name: "nanotdb.partition.sealed", Group: "nanotdb.partition", ValueType: Int32Sample, ValueUnits: "record_count",
		PayloadKeys: []string{"db", "file", "partition_key", "bytes"},
		Description: "A partition window closed and its page was sealed to disk.",
	},
	{
		Name: "nanotdb.partition.deleted", Group: "nanotdb.partition", ValueType: Int32Sample, ValueUnits: "bytes_freed",
		PayloadKeys: []string{"db", "partition_key", "files_removed", "retention_reason"},
		Description: "A partition was deleted by retention.",
	},
	{
		Name: "nanotdb.partition.archived", Group: "nanotdb.partition", ValueType: Int32Sample, ValueUnits: "bytes",
		PayloadKeys: []string{"db", "partition_key", "tar_path"},
		Description: "A partition was archived by retention.",
	},
	{
		Name: "nanotdb.partition.optimized", Group: "nanotdb.partition", ValueType: Int32Sample, ValueUnits: "bytes_saved",
		PayloadKeys: []string{"db", "partition_key", "file", "source_bytes", "dest_bytes"},
		Description: "A query-optimized metric file was written from a raw partition. The value is signed source_bytes - dest_bytes; a negative value means the optimized file is larger (small partitions where framing overhead exceeds the compression win).",
	},

	// ---- nanotdb.partition.slow ----
	{
		Name: "nanotdb.partition.flush.slow", Group: "nanotdb.partition.slow", ValueType: Float32Sample, ValueUnits: "ms",
		PayloadKeys: []string{"db", "file", "partition_key"},
		Description: "A single page flush exceeded the slow-flush threshold.",
	},

	// ---- nanotdb.wal ----
	{
		Name: "nanotdb.wal.replayed", Group: "nanotdb.wal", ValueType: Int32Sample, ValueUnits: "records_replayed",
		PayloadKeys: []string{"db", "file", "bytes_scanned"},
		Description: "A WAL file was replayed at startup.",
	},
	{
		Name: "nanotdb.wal.tail_truncated", Group: "nanotdb.wal", ValueType: Int32Sample, ValueUnits: "bytes",
		PayloadKeys: []string{"db", "file", "reason"},
		Description: "WAL crash-tail was truncated during replay.",
	},
	{
		Name: "nanotdb.wal.segment.rotated", Group: "nanotdb.wal", ValueType: Int32Sample, ValueUnits: "segment_size_bytes",
		PayloadKeys: []string{"db", "file"},
		Description: "A WAL segment was rotated.",
	},
	{
		Name: "nanotdb.wal.reset", Group: "nanotdb.wal", ValueType: Int32Sample, ValueUnits: "bytes_reclaimed",
		PayloadKeys: []string{"db", "file"},
		Description: "A WAL was reset (truncated to empty) after a clean flush.",
	},

	// ---- nanotdb.wal.fsync ----
	{
		Name: "nanotdb.wal.fsync.slow", Group: "nanotdb.wal.fsync", ValueType: Float32Sample, ValueUnits: "ms",
		PayloadKeys: []string{"db", "file"},
		Description: "A WAL fsync exceeded the slow-fsync threshold.",
	},
	{
		Name: "nanotdb.wal.fsync.error", Group: "nanotdb.wal.fsync", ValueType: EventValueNone,
		PayloadKeys: []string{"db", "file", "err"},
		Description: "A WAL fsync returned an error.",
	},

	// ---- nanotdb.catalog ----
	{
		Name: "nanotdb.catalog.metric.added", Group: "nanotdb.catalog", ValueType: Int32Sample, ValueUnits: "metric_id",
		PayloadKeys: []string{"db", "name", "value_type"},
		Description: "A new metric name was registered in the catalog.",
	},
	{
		Name: "nanotdb.catalog.event.added", Group: "nanotdb.catalog", ValueType: Int32Sample, ValueUnits: "event_id",
		PayloadKeys: []string{"db", "name", "value_type"},
		Description: "A new event name was registered in the events catalog.",
	},
	{
		Name: "nanotdb.catalog.full", Group: "nanotdb.catalog", ValueType: Int32Sample, ValueUnits: "cap",
		PayloadKeys: []string{"db", "kind"},
		Description: "The metric or event catalog hit its hard cap.",
	},
	{
		Name: "nanotdb.catalog.write.failed", Group: "nanotdb.catalog", ValueType: EventValueNone,
		PayloadKeys: []string{"db", "file", "err"},
		Description: "Writing the catalog to disk failed.",
	},

	// ---- nanotdb.ingest.reject ----
	{
		Name: "nanotdb.ingest.rejected.stale", Group: "nanotdb.ingest.reject", ValueType: Int32Sample, ValueUnits: "count_this_window",
		PayloadKeys: []string{"db", "top_offenders"},
		Description: "Stale samples or events rejected during the last batching window.",
	},
	{
		Name: "nanotdb.ingest.rejected.payload_too_large", Group: "nanotdb.ingest.reject", ValueType: Int32Sample, ValueUnits: "bytes",
		PayloadKeys: []string{"db", "name"},
		Description: "An event payload exceeded the configured max_payload_bytes.",
	},

	// ---- nanotdb.ingest.spike ----
	{
		Name: "nanotdb.ingest.spike.force_flush", Group: "nanotdb.ingest.spike", ValueType: Int32Sample, ValueUnits: "page_bytes",
		PayloadKeys: []string{"db", "layer"},
		Description: "The in-memory ceiling tripped and a page was force-flushed under back-pressure.",
	},

	// ---- nanotdb.disk ----
	{
		Name: "nanotdb.disk.low", Group: "nanotdb.disk", ValueType: Int32Sample, ValueUnits: "bytes_free",
		PayloadKeys: []string{"mount", "threshold_bytes"},
		Description: "Free disk space crossed below the configured threshold.",
	},
	{
		Name: "nanotdb.disk.write.error", Group: "nanotdb.disk", ValueType: EventValueNone,
		PayloadKeys: []string{"file", "err"},
		Description: "A disk write returned an error.",
	},

	// ---- nanotdb.rollup ----
	{
		Name: "nanotdb.rollup.window.emitted", Group: "nanotdb.rollup", ValueType: Int32Sample, ValueUnits: "records_written",
		PayloadKeys: []string{"src_metric", "dst_metric", "window"},
		Description: "A rollup window produced a destination metric sample.",
	},
	{
		Name: "nanotdb.rollup.catchup.started", Group: "nanotdb.rollup", ValueType: Int32Sample, ValueUnits: "windows_pending",
		PayloadKeys: []string{"src_metric", "dst_metric"},
		Description: "A rollup backfill started catching up.",
	},
	{
		Name: "nanotdb.rollup.catchup.completed", Group: "nanotdb.rollup", ValueType: Int32Sample, ValueUnits: "windows_caught_up",
		PayloadKeys: []string{"src_metric", "dst_metric", "ms"},
		Description: "A rollup backfill finished catching up.",
	},

	// ---- nanotdb.http ----
	{
		Name: "nanotdb.http.listener.started", Group: "nanotdb.http", ValueType: EventValueNone,
		PayloadKeys: []string{"addr"},
		Description: "The HTTP listener started.",
	},
	{
		Name: "nanotdb.http.listener.stopped", Group: "nanotdb.http", ValueType: EventValueNone,
		PayloadKeys: []string{"addr", "reason"},
		Description: "The HTTP listener stopped.",
	},

	// ---- nanotdb.mqtt ----
	{
		Name: "nanotdb.mqtt.connected", Group: "nanotdb.mqtt", ValueType: EventValueNone,
		PayloadKeys: []string{"broker"},
		Description: "MQTT worker connected to its broker.",
	},
	{
		Name: "nanotdb.mqtt.disconnected", Group: "nanotdb.mqtt", ValueType: EventValueNone,
		PayloadKeys: []string{"broker", "reason"},
		Description: "MQTT worker disconnected from its broker.",
	},
	{
		Name: "nanotdb.mqtt.subscription.dropped", Group: "nanotdb.mqtt", ValueType: Int32Sample, ValueUnits: "count",
		PayloadKeys: []string{"broker", "topic"},
		Description: "MQTT messages were dropped on a subscription.",
	},

	// ---- nanotdb.auth ----
	{
		Name: "nanotdb.auth.failure", Group: "nanotdb.auth", ValueType: Int32Sample, ValueUnits: "count_this_window",
		PayloadKeys: []string{"addr", "route", "top_users"},
		Description: "Auth failures aggregated over the last batching window.",
	},

	// ---- nanotdb.retention ----
	{
		Name: "nanotdb.retention.sweep.started", Group: "nanotdb.retention", ValueType: Int32Sample, ValueUnits: "candidate_partitions",
		Description: "A retention sweep started.",
	},
	{
		Name: "nanotdb.retention.sweep.completed", Group: "nanotdb.retention", ValueType: Int32Sample, ValueUnits: "partitions_actioned",
		PayloadKeys: []string{"deleted", "archived", "kept", "ms"},
		Description: "A retention sweep finished.",
	},

	// ---- drip.lifecycle ----
	{
		Name: "drip.started", Group: "drip.lifecycle", ValueType: Int32Sample, ValueUnits: "collector_count",
		PayloadKeys: []string{"version", "target_db", "target_url"},
		Description: "drip startup completed.",
	},
	{
		Name: "drip.stopped.clean", Group: "drip.lifecycle", ValueType: Int32Sample, ValueUnits: "ms_to_drain",
		Description: "drip shut down cleanly.",
	},
	{
		Name: "drip.config.reloaded", Group: "drip.lifecycle", ValueType: EventValueNone,
		Description: "drip reloaded its configuration.",
	},

	// ---- drip.target ----
	{
		Name: "drip.target.disconnected", Group: "drip.target", ValueType: EventValueNone,
		PayloadKeys: []string{"url", "err"},
		Description: "drip's target nanotdb stopped accepting writes.",
	},
	{
		Name: "drip.target.reconnected", Group: "drip.target", ValueType: Int32Sample, ValueUnits: "outage_ms",
		PayloadKeys: []string{"url"},
		Description: "drip's target nanotdb resumed accepting writes.",
	},

	// ---- drip.buffer ----
	{
		Name: "drip.buffer.flush.failed", Group: "drip.buffer", ValueType: Int32Sample, ValueUnits: "queued_samples",
		PayloadKeys: []string{"err"},
		Description: "drip's flush to the target failed.",
	},
	{
		Name: "drip.buffer.high_water", Group: "drip.buffer", ValueType: Int32Sample, ValueUnits: "queued_samples",
		PayloadKeys: []string{"capacity"},
		Description: "drip's buffer crossed the high-water mark.",
	},
	{
		Name: "drip.buffer.dropped", Group: "drip.buffer", ValueType: Int32Sample, ValueUnits: "count_this_window",
		Description: "drip dropped buffered samples over the last batching window.",
	},

	// ---- drip.collector ----
	{
		Name: "drip.collector.started", Group: "drip.collector", ValueType: EventValueNone,
		PayloadKeys: []string{"name", "interval_seconds"},
		Description: "A drip collector started running.",
	},
	{
		Name: "drip.collector.failed", Group: "drip.collector", ValueType: Int32Sample, ValueUnits: "consecutive_failures",
		PayloadKeys: []string{"name", "err"},
		Description: "A drip collector failed N times in a row (rate-limited).",
	},
	{
		Name: "drip.collector.recovered", Group: "drip.collector", ValueType: Int32Sample, ValueUnits: "outage_ms",
		PayloadKeys: []string{"name"},
		Description: "A drip collector recovered after a streak of failures.",
	},

	// ---- drip.host ----
	{
		Name: "drip.host.boot", Group: "drip.host", ValueType: EventValueNone,
		PayloadKeys: []string{"kernel", "uptime_seconds"},
		Description: "Host reported a boot event.",
	},
	{
		Name: "drip.host.disk.low", Group: "drip.host", ValueType: Int32Sample, ValueUnits: "bytes_free",
		PayloadKeys: []string{"mount"},
		Description: "Host disk free space crossed below threshold.",
	},
	{
		Name: "drip.host.temp.crossed", Group: "drip.host", ValueType: Float32Sample, ValueUnits: "celsius",
		PayloadKeys: []string{"sensor", "threshold_celsius"},
		Description: "Host temperature crossed configured threshold.",
	},

	// ---- drip.threshold ----
	//
	// Per-collector threshold events live in this group, one event name
	// per (collector, metric, threshold) tuple. The registry entry below
	// is the umbrella entry: collectors register concrete names that
	// match the dotted pattern at first emit. The catalog endpoint
	// reports both the umbrella entry and any concrete instances seen
	// at runtime.
	{
		Name: "drip.threshold.disk.sd_write_probe.slow", Group: "drip.threshold", ValueType: Float32Sample, ValueUnits: "ms",
		PayloadKeys: []string{"collector", "metric", "threshold", "observed"},
		Description: "drip SD-write-probe latency crossed configured threshold.",
	},
}

// internalEventsGroupDefaults is the per-group default enabled/disabled
// state, mirroring the "Group taxonomy" tables in
// docs/INTERNAL_EVENTS.md. A group not listed in engine.toml or drip.toml
// uses the value here; a group not listed here either is unknown and
// will cause a hard error at config load.
var internalEventsGroupDefaults = map[string]bool{
	// nanotdb groups
	"nanotdb.lifecycle":      true,
	"nanotdb.db":             true,
	"nanotdb.partition":      true,
	"nanotdb.partition.slow": true,
	"nanotdb.wal":            false,
	"nanotdb.wal.fsync":      false,
	"nanotdb.catalog":        true,
	"nanotdb.ingest.reject":  false,
	"nanotdb.ingest.spike":   true,
	"nanotdb.disk":           true,
	"nanotdb.rollup":         true,
	"nanotdb.http":           true,
	"nanotdb.mqtt":           true,
	"nanotdb.auth":           true,
	"nanotdb.retention":      true,

	// drip groups
	"drip.lifecycle": true,
	"drip.target":    true,
	"drip.buffer":    true,
	"drip.collector": true,
	"drip.host":      false,
	"drip.threshold": true,
}

// internalEventGroupKnown reports whether the named group exists in the
// registry. Used to validate config and runtime toggle calls.
func internalEventGroupKnown(group string) bool {
	_, ok := internalEventsGroupDefaults[group]
	return ok
}

// InternalEventNamesForGroup returns every event name registered
// under the given group. Returns nil for an unknown group. Exported
// for offline consumers (e.g. nanocli) that need to map a group name
// to its events without going through the HTTP catalog endpoint.
//
// Reads a static registry — does not depend on a running engine and
// does not allocate per-event.
func InternalEventNamesForGroup(group string) []string {
	out := make([]string, 0, 8)
	for _, d := range internalEventsRegistry {
		if d.Group == group {
			out = append(out, d.Name)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// internalEventDefByName looks up an event definition by exact name.
// Used by the emitter to validate registered events and by the catalog
// endpoint.
func internalEventDefByName(name string) (internalEventDef, bool) {
	for _, d := range internalEventsRegistry {
		if d.Name == name {
			return d, true
		}
	}
	return internalEventDef{}, false
}

// internalEventsGroupsSorted returns the set of all known groups in a
// stable order — used by HTTP responses and tests that need
// reproducible output.
func internalEventsGroupsSorted() []string {
	seen := make(map[string]struct{}, len(internalEventsGroupDefaults))
	out := make([]string, 0, len(internalEventsGroupDefaults))
	for _, d := range internalEventsRegistry {
		if _, ok := seen[d.Group]; ok {
			continue
		}
		seen[d.Group] = struct{}{}
		out = append(out, d.Group)
	}
	// Include groups present only in defaults (none today, but stays
	// honest if the registry and defaults ever drift).
	for g := range internalEventsGroupDefaults {
		if _, ok := seen[g]; ok {
			continue
		}
		seen[g] = struct{}{}
		out = append(out, g)
	}
	// Sort for stable output.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
