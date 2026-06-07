package engine

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"time"
)

// ErrEventsDisabled is returned from Engine.AddEvent / Engine.QueryEvents
// when the target database does not have the events layer opted in via
// its manifest ([events].enabled = true). The default is false, matching
// docs/EVENTS.md's "opt-in per DB — no surprise files on upgrade" rule.
var ErrEventsDisabled = errors.New("events layer is not enabled for this database")

// AddEvent ingests one event occurrence into the named database. The
// database must have [events].enabled = true in its manifest. Mirrors
// AddSample's ingest discipline: writeMu serializes writers, the catalog
// is consulted to pin (or verify) the event's value type, the events
// WAL is appended before the in-memory page is mutated, and per-event
// monotonic-ts is enforced.
//
// value must be nil (for a none-typed event), an int32, or a float32.
// payload is opaque bytes; pass nil for no payload. ts == 0 means "use
// the current time" (mirroring AddSample's behaviour via the ingest
// HTTP layer).
//
// Returns ErrEventsDisabled if the database has not opted in,
// ErrEventTypeMismatch on a value-type/catalog mismatch, ErrTooManyEvents
// when the per-database catalog cap is reached, ErrEventPayloadTooLarge
// when the payload exceeds the documented hard cap.
func (e *Engine) AddEvent(database, name string, ts Timestamp, value any, payload []byte) error {
	database = strings.TrimSpace(database)
	if err := ValidateDatabaseName(database); err != nil {
		return err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return ErrEventNameEmpty
	}
	if len(name) > MaxEventNameLen {
		return ErrEventNameTooLong
	}

	var valueType byte
	var valueRaw uint32
	switch v := value.(type) {
	case nil:
		valueType = EventValueNone
	case int32:
		valueType = Int32Sample
		valueRaw = uint32(v)
	case float32:
		valueType = Float32Sample
		valueRaw = math.Float32bits(v)
	default:
		return fmt.Errorf("unsupported event value type %T (want int32, float32, or nil)", value)
	}

	e.writeMu.Lock()
	defer e.writeMu.Unlock()

	db, rt, err := e.getOrCreateDB(database)
	if err != nil {
		return err
	}
	if !rt.info.EventsEnabled || db.eventCatalog == nil || db.eventsWAL == nil {
		return ErrEventsDisabled
	}
	if len(payload) > rt.info.EventsMaxPayloadBytes {
		if e.internalEventsActive("nanotdb.ingest.reject") {
			e.emitInternalEvent("nanotdb.ingest.reject", "nanotdb.ingest.rejected.payload_too_large", int32(len(payload)), map[string]any{
				"db":   database,
				"name": name,
			}, database)
		}
		return ErrEventPayloadTooLarge
	}

	if ts == 0 {
		ts = Timestamp(time.Now().UnixNano())
	}

	// Resolve/assign the EventID. existsBefore lets us emit a newEvent
	// WAL record on first occurrence (carrying the inline name +
	// value_type so replay can rebuild the catalog).
	_, existsBefore := db.eventCatalog.GetEventEntry(name)
	eventID, err := db.eventCatalog.GetOrAssignEventID(name, valueType)
	if err != nil {
		e.emitCatalogFullIfApplicable(database, "events", err)
		return err
	}
	if !existsBefore {
		e.emitCatalogEventAdded(database, name, eventID, eventValueTypeName(valueType))
	}

	// Per-event-name monotonic ordering. Mirrors the metric stale-sample
	// rejection in addParsedSample.
	if lastTS, ok := db.eventCatalog.LastTS(eventID); ok && ts < lastTS {
		e.emitInternalEventBatched("nanotdb.ingest.reject", "nanotdb.ingest.rejected.stale", database, map[string]any{
			"db":   database,
			"name": name,
		}, defaultInternalEventsBatchEvery, database)
		return fmt.Errorf("stale event rejected for %s/%s: ts=%d < last=%d", database, name, ts, lastTS)
	}

	day := partitionKey(rt, ts)

	// WAL append first (crash-safety rule 1: catalog entry must be
	// re-derivable from WAL if we crash before events.json is rewritten).
	var rawValue [4]byte
	binary.LittleEndian.PutUint32(rawValue[:], valueRaw)
	var walSegment uint16
	if !existsBefore {
		walSegment, err = AppendEventWithName(db.eventsWAL, eventID, name, ts, valueType, rawValue, payload)
	} else {
		walSegment, err = AppendEvent(db.eventsWAL, eventID, ts, valueType, rawValue, payload)
	}
	if err != nil {
		return err
	}
	_ = walSegment

	// In-memory page. Lazy-create the page for this partition; events
	// follow the same partition cadence as metrics.
	page := rt.openEventsDays[day]
	if page == nil {
		page = NewEventsPageWithLimits(ts, rt.info.EventsPageMaxRecords, rt.info.EventsPageMaxBytes, rt.eventsPageMaxAge, rt.info.EventsMaxInMemoryBytes)
		rt.openEventsDays[day] = page
	}
	if err := page.AddEvent(eventID, ts, valueType, valueRaw, payload); err != nil {
		return err
	}
	if walSegment != 0 {
		page.SetWALSegmentID(walSegment)
	}
	if err := db.eventCatalog.UpdateLastByEventID(eventID, ts); err != nil {
		return err
	}
	// Flush oversized/aged pages immediately so events pages follow the same
	// bounded-memory discipline as metric pages.
	if page.MustForceFlush() || page.IsFull() {
		if page.MustForceFlush() && e.internalEventsActive("nanotdb.ingest.spike") {
			e.emitInternalEvent("nanotdb.ingest.spike", "nanotdb.ingest.spike.force_flush", int32(page.SizeBytes()), map[string]any{
				"db":    database,
				"layer": "event",
			}, database)
		}
		if _, err := AppendEventsPageFrame(db.RootDataDir, day, page, e.SyncDataFile); err != nil {
			return err
		}
		delete(rt.openEventsDays, day)
		if err := e.maybeResetEventsWAL(db, rt, database); err != nil {
			return err
		}
	}
	return nil
}

// EventQueryResult is one decoded event returned by QueryEvents. Mirrors
// Sample for the events layer; the HTTP layer and nanocli serialize this
// shape directly into their responses.
type EventQueryResult struct {
	Database  string
	Name      string
	EventID   EventID
	TS        Timestamp
	ValueType byte
	Int32     int32
	Float32   float32
	Payload   []byte
}

// EventQueryCallback is invoked once per matching event, in
// non-decreasing-ts order. Returning a non-nil error stops the scan.
type EventQueryCallback func(EventQueryResult) error

// QueryEvents scans events in [fromTS, toTS] for the named database,
// optionally filtered by an exact event name. cb is invoked once per
// matching event in non-decreasing-ts order. The scan walks the sealed
// events-*.dat partition files first (using the per-frame bitmap to
// skip frames that don't carry the requested event id), then the
// in-memory open events page for each partition.
//
// When name is empty, all events in the time range match. Returns
// ErrEventsDisabled if the database hasn't opted into the events layer.
func (e *Engine) QueryEvents(database, name string, fromTS, toTS Timestamp, cb EventQueryCallback) error {
	database = strings.TrimSpace(database)
	if err := ValidateDatabaseName(database); err != nil {
		return err
	}
	if cb == nil {
		return fmt.Errorf("callback is required")
	}
	if toTS < fromTS {
		return fmt.Errorf("invalid time range: toTS=%d < fromTS=%d", toTS, fromTS)
	}

	db, rt, err := e.getOrCreateDB(database)
	if err != nil {
		return err
	}
	if !rt.info.EventsEnabled || db.eventCatalog == nil {
		return ErrEventsDisabled
	}

	// Resolve the optional name filter to an EventID set.
	name = strings.TrimSpace(name)
	var nameFilter []EventID
	if name != "" {
		entry, ok := db.eventCatalog.GetEventEntry(name)
		if !ok {
			// Unknown name — no matches possible.
			return nil
		}
		nameFilter = []EventID{entry.EventID}
	}

	// Iterate partitions in time order. Phase 1 step 5: walk the
	// directory once and filter by overlap; the metric-side parallel
	// (collectPersistedPartitions) is rich enough that mirroring it
	// exactly is more code than needed for a first cut.
	parts, err := listEventsPartitions(db.RootDataDir)
	if err != nil {
		return err
	}
	for _, part := range parts {
		path := EventsFilePath(db.RootDataDir, part)
		// Walk frame headers to find candidates that intersect both
		// the time range and the name filter (when present).
		var candidateOffsets []int64
		_, walkErr := WalkEventsFileHeaders(path, func(h EventsFrameHeader) error {
			if h.EndTime < fromTS || h.StartTime > toTS {
				return nil
			}
			if nameFilter != nil && !h.IntersectsAny(nameFilter) {
				return nil
			}
			candidateOffsets = append(candidateOffsets, h.Offset)
			return nil
		})
		if walkErr != nil {
			return fmt.Errorf("walk events file %q: %w", path, walkErr)
		}
		for _, off := range candidateOffsets {
			page, err := CollectEventsFrame(path, off, db.eventCatalog)
			if err != nil {
				return err
			}
			if err := emitMatchingEvents(database, db.eventCatalog, page, fromTS, toTS, nameFilter, cb); err != nil {
				return err
			}
		}
	}

	// In-memory open pages — one per partition. Sort partitions so emit
	// order remains time-ordered across the boundary between sealed
	// frames and the open page.
	openParts := make([]string, 0, len(rt.openEventsDays))
	for part := range rt.openEventsDays {
		openParts = append(openParts, part)
	}
	sortStringsAscending(openParts)
	for _, part := range openParts {
		page := rt.openEventsDays[part]
		if page == nil || page.Count() == 0 {
			continue
		}
		if err := emitMatchingEvents(database, db.eventCatalog, page, fromTS, toTS, nameFilter, cb); err != nil {
			return err
		}
	}

	return nil
}

// emitMatchingEvents walks the records in page (the in-memory open
// page or a freshly-decoded sealed frame), filters by time range and
// optional name set, resolves the event name from the catalog, and
// invokes cb for each match.
func emitMatchingEvents(database string, cat *EventCatalog, page *EventsPage, fromTS, toTS Timestamp, nameFilter []EventID, cb EventQueryCallback) error {
	for i := 0; i < page.Count(); i++ {
		ts := page.Times[i]
		if ts < fromTS || ts > toTS {
			continue
		}
		id := page.EventIDs[i]
		if nameFilter != nil && !containsEventID(nameFilter, id) {
			continue
		}
		name, _, ok := cat.GetEventByID(id)
		if !ok {
			return fmt.Errorf("query events: catalog missing entry for id %d", id)
		}
		result := EventQueryResult{
			Database:  database,
			Name:      name,
			EventID:   id,
			TS:        ts,
			ValueType: page.ValueTypes[i],
			Payload:   append([]byte(nil), page.Payloads[i]...),
		}
		switch page.ValueTypes[i] {
		case Int32Sample:
			result.Int32 = int32(page.ValuesRaw[i])
		case Float32Sample:
			result.Float32 = math.Float32frombits(page.ValuesRaw[i])
		}
		if err := cb(result); err != nil {
			return err
		}
	}
	return nil
}

func containsEventID(set []EventID, id EventID) bool {
	for _, x := range set {
		if x == id {
			return true
		}
	}
	return false
}

// sortStringsAscending is a tiny insertion-sort to avoid pulling in
// the sort package just for the small openEventsDays partition list.
// The metric side uses sort.Strings; we mirror that semantics without
// the extra import in this file.
func sortStringsAscending(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// listEventsPartitions returns the sorted partition strings for which
// an events-*.dat file exists in dbRoot.
func listEventsPartitions(dbRoot string) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(dbRoot, "events-*.dat"))
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		base := filepath.Base(m)
		part := strings.TrimSuffix(strings.TrimPrefix(base, "events-"), ".dat")
		if part == "" {
			continue
		}
		out = append(out, part)
	}
	sortStringsAscending(out)
	return out, nil
}

// ListEvents returns a stable, name-sorted snapshot of the events
// catalog for the named database. Returns ErrEventsDisabled when the
// database has not opted into the events layer. Mirrors ListMetrics.
func (e *Engine) ListEvents(database string) ([]EventInfo, error) {
	database = strings.TrimSpace(database)
	if database == "" {
		return nil, fmt.Errorf("database cannot be empty")
	}
	db, rt, err := e.getOrCreateDB(database)
	if err != nil {
		return nil, err
	}
	if !rt.info.EventsEnabled || db.eventCatalog == nil {
		return nil, ErrEventsDisabled
	}
	return db.eventCatalog.ListEvents(), nil
}

// replayEventsWALIntoRuntime is the events-side mirror of
// replayWALIntoRuntime. Called from getOrCreateDBWithDefaults when the
// events layer is enabled. Reconstructs the in-memory events catalog
// entries (from newEvent WAL records) and refills the per-partition
// in-memory open pages so they pick up exactly where the previous
// process left off.
func (e *Engine) replayEventsWALIntoRuntime(db *Database, rt *dbRuntime, dbName string) error {
	if db == nil || rt == nil || db.eventsWAL == nil || db.eventCatalog == nil {
		return nil
	}
	recs, err := db.eventsWAL.RecordsWithCatalog(db.eventCatalog)
	if err != nil {
		return err
	}
	if len(recs) == 0 {
		return nil
	}
	for _, rec := range recs {
		day := partitionKey(rt, rec.TS)

		// Per-partition durable watermark: if events-<part>.dat exists
		// and covers this ts, skip — the data is already durable and a
		// fresh AddEvent during this replay would double-write.
		path := EventsFilePath(db.RootDataDir, day)
		st, statErr := ScanEventsFileStats(path)
		if statErr == nil && st.Frames > 0 && rec.TS <= st.MaxEnd {
			continue
		}

		page := rt.openEventsDays[day]
		if page == nil {
			page = NewEventsPageWithLimits(rec.TS, rt.info.EventsPageMaxRecords, rt.info.EventsPageMaxBytes, rt.eventsPageMaxAge, rt.info.EventsMaxInMemoryBytes)
			rt.openEventsDays[day] = page
		}
		var valueRaw uint32
		switch rec.ValueType {
		case Int32Sample:
			valueRaw = uint32(rec.Int32Value)
		case Float32Sample:
			valueRaw = math.Float32bits(rec.Float32Value)
		}
		if err := page.AddEvent(rec.EventID, rec.TS, rec.ValueType, valueRaw, rec.Payload); err != nil {
			return fmt.Errorf("events replay add: %w", err)
		}
		if err := db.eventCatalog.UpdateLastByEventID(rec.EventID, rec.TS); err != nil {
			return err
		}
	}
	e.logInfo("events wal replayed", "database", dbName, "records", len(recs))
	return nil
}

// flushOpenEventsPages writes every non-empty in-memory events page
// for db to its events-<partition>.dat file and clears the map. Used
// by the engine close/flush paths.
func (e *Engine) flushOpenEventsPages(db *Database, rt *dbRuntime, dbName string) error {
	if db == nil || rt == nil || rt.openEventsDays == nil {
		return nil
	}
	for part, page := range rt.openEventsDays {
		if page == nil || page.Count() == 0 {
			delete(rt.openEventsDays, part)
			continue
		}
		frameStats, err := AppendEventsPageFrame(db.RootDataDir, part, page, e.SyncDataFile)
		if err != nil {
			return fmt.Errorf("flush events page partition %q: %w", part, err)
		}
		records := page.Count()
		delete(rt.openEventsDays, part)
		if e.internalEventsActive("nanotdb.partition") {
			e.emitInternalEvent("nanotdb.partition", "nanotdb.partition.sealed", int32(records), map[string]any{
				"db":            dbName,
				"file":          "events",
				"partition_key": part,
				"bytes":         frameStats.FrameBytes,
			}, dbName)
		}
	}
	return nil
}

// writeEventCatalogIfDirty persists the events catalog when it has
// pending writes. Safe to call when the events layer is disabled
// (db.eventCatalog is nil — no-op). Mirrors the metric-side check
// `db.catalog.IsDirty()` at the four engine checkpoint sites.
func writeEventCatalogIfDirty(db *Database) error {
	if db == nil || db.eventCatalog == nil {
		return nil
	}
	if !db.eventCatalog.IsDirty() {
		return nil
	}
	return db.eventCatalog.WriteCatalog()
}

// maybeResetEventsWAL flushes any open events pages and resets the
// events WAL, but only when every open page has been written to disk
// — same eligibility rule as maybeResetWAL for the metric WAL. Order
// of operations is strict: events catalog must be persisted before
// the WAL is allowed to reset (crash-safety contract rule 1).
func (e *Engine) maybeResetEventsWAL(db *Database, rt *dbRuntime, dbName string) error {
	if db == nil || rt == nil || db.eventsWAL == nil {
		return nil
	}
	if !rt.info.EventsEnabled {
		return nil
	}
	// If any page still has unflushed records, the WAL must stay.
	for _, p := range rt.openEventsDays {
		if p != nil && p.Count() > 0 {
			return nil
		}
	}
	if err := writeEventCatalogIfDirty(db); err != nil {
		return fmt.Errorf("write events catalog for %q before WAL reset: %w", dbName, err)
	}
	return db.eventsWAL.Reset()
}
