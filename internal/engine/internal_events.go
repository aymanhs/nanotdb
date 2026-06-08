package engine

// Internal events emitter. See docs/INTERNAL_EVENTS.md for the spec.
//
// Lifecycle: newInternalEventsEmitter is called from OpenEngineWithConfig
// with the validated config. If Enabled, startInternalEventsDrain spins
// up the drain goroutine. Close calls stopInternalEventsDrain before
// taking writeMu, mirroring stopMQTT.
//
// The emit fast-path is one atomic pointer load + one map lookup —
// disabled groups and a disabled master switch both no-op without
// allocations. The drain goroutine is the only thing that ever calls
// e.AddEvent; the emit call site never blocks.
//
// Self-recursion guard: the drain goroutine writes into the configured
// destination DB (default "internal"). emitInternalEvent skips the
// send when the call site is itself operating on the destination DB —
// the source-db argument is what gates this. Mirrors the existing
// stats short-circuit at engine.go:2308 and engine.go:2366.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BurntSushi/toml"
)

// internalEventRecord is one buffered event in flight from the emit
// site to the drain goroutine.
type internalEventRecord struct {
	name    string
	ts      Timestamp
	value   any
	payload []byte
}

// batchKey identifies one batched-event aggregation slot. (name) is the
// event being aggregated; (key) lets call sites bucket by db, by
// metric, etc.
type batchKey struct {
	name string
	key  string
}

// batchState accumulates a per-(name, key) count and the most recent
// payload between batched-event emit windows. The timer goroutine
// drains state into a real emit on its next tick.
type batchState struct {
	count    int32
	payload  map[string]any
	every    time.Duration
	lastEmit time.Time
}

// internalEventsEmitter is the engine's per-instance internal-events
// state. The Engine struct holds one *internalEventsEmitter.
type internalEventsEmitter struct {
	cfg EngineConfigInternalEvents

	// activeGroups maps group name → enabled. Replaced atomically on
	// runtime toggle so the emit-site fast-path is lock-free.
	activeGroups atomic.Pointer[map[string]bool]

	// groupSource records why each group is in its current state.
	// Updated under sourceMu by the runtime-toggle path. Read by the
	// catalog/groups HTTP handlers.
	sourceMu    sync.RWMutex
	groupSource map[string]string

	ch       chan internalEventRecord
	stopOnce sync.Once
	stopped  atomic.Bool
	done     chan struct{}

	// Dropped records, exposed through engineStatStore as
	// nanotdb/internal_events_dropped.
	dropCounter atomic.Uint64

	// Batched-event state.
	batchMu    sync.Mutex
	batches    map[batchKey]*batchState
	batchStop  chan struct{}
	batchDone  chan struct{}
	batchStart sync.Once

	// logOnce per event name — used by the drain goroutine to log a
	// failed AddEvent at most once per name.
	loggedMu    sync.Mutex
	loggedNames map[string]struct{}
}

// newInternalEventsEmitter builds the emitter from validated config.
// Does NOT start the drain goroutine — that happens in
// startInternalEventsDrain after the engine is otherwise ready.
func newInternalEventsEmitter(cfg EngineConfigInternalEvents) *internalEventsEmitter {
	em := &internalEventsEmitter{
		cfg:         cfg,
		groupSource: map[string]string{},
		done:        make(chan struct{}),
		batches:     map[batchKey]*batchState{},
		batchStop:   make(chan struct{}),
		batchDone:   make(chan struct{}),
		loggedNames: map[string]struct{}{},
	}
	groups := buildActiveGroupsMap(cfg)
	em.activeGroups.Store(&groups)
	for g := range groups {
		em.groupSource[g] = configGroupSource(cfg.Groups, g)
	}
	if cfg.QueueDepth <= 0 {
		cfg.QueueDepth = defaultInternalEventsQueueDepth
		em.cfg = cfg
	}
	em.ch = make(chan internalEventRecord, em.cfg.QueueDepth)
	return em
}

const (
	defaultInternalEventsQueueDepth = 4096
	defaultInternalEventsDB         = "internal"
	defaultInternalEventsBatchEvery = time.Minute
	defaultInternalEventsDropMetric = "internal_events_dropped"
)

// buildActiveGroupsMap merges per-group defaults with the config-supplied
// overrides. Unknown group keys must already have been rejected in
// normalizeEngineConfig — this function only sees validated input.
func buildActiveGroupsMap(cfg EngineConfigInternalEvents) map[string]bool {
	out := make(map[string]bool, len(internalEventsGroupDefaults))
	for g, def := range internalEventsGroupDefaults {
		out[g] = def
	}
	for g, v := range cfg.Groups {
		switch v {
		case "on", "true", "yes":
			out[g] = true
		case "off", "false", "no":
			out[g] = false
		}
	}
	return out
}

// configGroupSource reports whether a group's enabled state came from
// config (engine.toml) or from defaults.
func configGroupSource(cfgGroups map[string]string, group string) string {
	if _, ok := cfgGroups[group]; ok {
		return "engine.toml"
	}
	return "default"
}

// startInternalEventsDrain spins up the drain goroutine. Safe to call
// when cfg.Enabled is false — it returns immediately, so the emitter
// is dormant.
func (e *Engine) startInternalEventsDrain() {
	if e.internalEvents == nil || !e.internalEvents.cfg.Enabled {
		return
	}
	go e.internalEventsDrainLoop()
	e.internalEvents.batchStart.Do(func() {
		go e.internalEventsBatchLoop()
	})
}

// stopInternalEventsDrain stops the drain goroutine and the batch
// timer. Blocks until both have drained. Safe to call multiple times.
func (e *Engine) stopInternalEventsDrain() {
	if e.internalEvents == nil || !e.internalEvents.cfg.Enabled {
		return
	}
	em := e.internalEvents
	em.stopOnce.Do(func() {
		// Stop the batch timer first so it can emit any pending
		// batched records through the still-open channel.
		close(em.batchStop)
		<-em.batchDone
		// Mark stopped before closing the channel so emitInternalEvent
		// sees the flag and refuses to send. Without this, late
		// post-Close emits from page-flush/wal-reset paths (which run
		// AFTER stopInternalEventsDrain in Engine.Close) would panic
		// with "send on closed channel".
		em.stopped.Store(true)
		close(em.ch)
		<-em.done
	})
}

// internalEventsActive reports whether emitting into the given group
// would produce a record. Encouraged for use as a guard around
// payload construction at call sites:
//
//	if e.internalEventsActive("nanotdb.partition") {
//	    payload := map[string]any{...}
//	    e.emitInternalEvent("nanotdb.partition", "...", val, payload, db)
//	}
func (e *Engine) internalEventsActive(group string) bool {
	if e.internalEvents == nil || !e.internalEvents.cfg.Enabled {
		return false
	}
	groups := e.internalEvents.activeGroups.Load()
	if groups == nil {
		return false
	}
	return (*groups)[group]
}

// emitInternalEvent is the hot path. Drops the record on:
//   - master switch off
//   - group disabled
//   - sourceDB == the configured destination DB (recursion guard)
//   - send-channel full (also bumps dropCounter)
//
// sourceDB is the database the *caller* is operating on (or empty for
// engine-wide events). The recursion guard fires when that equals the
// destination, NOT when the destination's writes call into the emitter
// — both directions are covered by the same one check, because the
// drain goroutine writes to dest and any event whose sourceDB is dest
// is exactly the self-write case.
func (e *Engine) emitInternalEvent(group, name string, value any, payload map[string]any, sourceDB string) {
	if e == nil || e.internalEvents == nil || !e.internalEvents.cfg.Enabled {
		return
	}
	em := e.internalEvents
	if em.stopped.Load() {
		// Close path already drained the channel; quietly drop.
		return
	}
	if sourceDB != "" && sourceDB == em.destDB() {
		return
	}
	groups := em.activeGroups.Load()
	if groups == nil || !(*groups)[group] {
		return
	}
	rec := internalEventRecord{
		name:  name,
		ts:    Timestamp(time.Now().UnixNano()),
		value: value,
	}
	if len(payload) > 0 {
		// Stable key order so the encoded bytes are deterministic —
		// matters for tests and for any payload-equality checks.
		keys := make([]string, 0, len(payload))
		for k := range payload {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		ordered := make(map[string]any, len(payload))
		for _, k := range keys {
			ordered[k] = payload[k]
		}
		buf, err := json.Marshal(ordered)
		if err == nil {
			rec.payload = buf
		}
	}
	select {
	case em.ch <- rec:
	default:
		em.dropCounter.Add(1)
	}
}

// emitInternalEventBatched accumulates per-(name, key) counts and
// flushes one event per non-zero bucket every `every` interval. Used
// for floods like ingest.rejected.stale, auth.failure,
// drip.buffer.dropped.
func (e *Engine) emitInternalEventBatched(group, name, key string, payload map[string]any, every time.Duration, sourceDB string) {
	if e == nil || e.internalEvents == nil || !e.internalEvents.cfg.Enabled {
		return
	}
	em := e.internalEvents
	if em.stopped.Load() {
		return
	}
	if sourceDB != "" && sourceDB == em.destDB() {
		return
	}
	groups := em.activeGroups.Load()
	if groups == nil || !(*groups)[group] {
		return
	}
	if every <= 0 {
		every = defaultInternalEventsBatchEvery
	}
	bk := batchKey{name: name, key: key}
	em.batchMu.Lock()
	st := em.batches[bk]
	if st == nil {
		st = &batchState{every: every, lastEmit: time.Now()}
		em.batches[bk] = st
	}
	st.count++
	st.payload = payload
	em.batchMu.Unlock()
}

// destDB returns the configured destination DB name, falling back to
// the documented default.
func (em *internalEventsEmitter) destDB() string {
	if em.cfg.DB == "" {
		return defaultInternalEventsDB
	}
	return em.cfg.DB
}

// internalEventsDrainLoop ranges the channel and writes each record
// via AddEvent. Per-name AddEvent failures are logged once and the
// record is dropped — the goroutine never exits because of a per-record
// error.
func (e *Engine) internalEventsDrainLoop() {
	em := e.internalEvents
	defer close(em.done)
	for rec := range em.ch {
		if err := e.AddEvent(em.destDB(), rec.name, rec.ts, rec.value, rec.payload); err != nil {
			em.logFailureOnce(e, rec.name, err)
		}
	}
}

// internalEventsBatchLoop ticks every batchTickInterval, drains the
// accumulated batches, and emits one event per non-zero bucket whose
// `every` window has elapsed.
func (e *Engine) internalEventsBatchLoop() {
	em := e.internalEvents
	defer close(em.batchDone)
	ticker := time.NewTicker(internalEventsBatchTickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-em.batchStop:
			e.flushBatches(time.Now(), true)
			return
		case <-ticker.C:
			e.flushBatches(time.Now(), false)
		}
	}
}

// internalEventsBatchTickInterval is how often the batch loop checks
// for ready-to-emit slots. Smaller than the typical batch window
// (1 minute) so we don't miss flush windows by much.
const internalEventsBatchTickInterval = 5 * time.Second

// flushBatches emits accumulated counters whose windows have elapsed.
// When force is true (called from the batch loop exit path), emits
// every non-zero bucket regardless of window.
func (e *Engine) flushBatches(now time.Time, force bool) {
	em := e.internalEvents
	em.batchMu.Lock()
	type out struct {
		name    string
		count   int32
		payload map[string]any
	}
	var ready []out
	for bk, st := range em.batches {
		if st.count == 0 {
			continue
		}
		if !force && now.Sub(st.lastEmit) < st.every {
			continue
		}
		ready = append(ready, out{name: bk.name, count: st.count, payload: st.payload})
		st.count = 0
		st.lastEmit = now
	}
	em.batchMu.Unlock()

	for _, r := range ready {
		// The batched event's typed value is the count this window.
		// The payload key (db, addr, ...) was set at the most recent
		// emit; we don't merge across the window — the most recent
		// context wins.
		def, ok := internalEventDefByName(r.name)
		group := ""
		if ok {
			group = def.Group
		}
		// Skip recursion guard for batch-flush because the per-call
		// emit was already gated; just use emitInternalEvent's normal
		// path with empty sourceDB.
		e.emitInternalEvent(group, r.name, r.count, r.payload, "")
	}
}

// logFailureOnce records a per-event-name log on the first failed
// AddEvent and silently drops subsequent failures with the same name.
func (em *internalEventsEmitter) logFailureOnce(e *Engine, name string, err error) {
	em.loggedMu.Lock()
	defer em.loggedMu.Unlock()
	if _, seen := em.loggedNames[name]; seen {
		return
	}
	em.loggedNames[name] = struct{}{}
	if e.logger != nil {
		e.logInfo("internal-events emit failed", "event", name, "err", err.Error())
	}
}

// SetInternalEventsGroup flips a group on or off at runtime. The
// change is in-memory only — restart reverts to the engine.toml /
// drip.toml value. Emits an audit event nanotdb.internal_events.group.toggled.
func (e *Engine) SetInternalEventsGroup(group string, on bool) error {
	if e.internalEvents == nil {
		return fmt.Errorf("internal events emitter is not configured")
	}
	if !internalEventGroupKnown(group) {
		return fmt.Errorf("unknown internal-events group: %q", group)
	}
	em := e.internalEvents
	current := em.activeGroups.Load()
	updated := make(map[string]bool, len(*current))
	for k, v := range *current {
		updated[k] = v
	}
	from := updated[group]
	updated[group] = on
	em.activeGroups.Store(&updated)

	em.sourceMu.Lock()
	em.groupSource[group] = "runtime"
	em.sourceMu.Unlock()

	e.emitInternalEvent("nanotdb.lifecycle", "nanotdb.internal_events.group.toggled", nil, map[string]any{
		"group":  group,
		"from":   boolToOnOff(from),
		"to":     boolToOnOff(on),
		"source": "runtime",
	}, "")
	return nil
}

func boolToOnOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// InternalEventsGroupView is one row in the GET /groups response.
type InternalEventsGroupView struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	Default bool   `json:"default"`
	Source  string `json:"source"`
}

// InternalEventsCatalogEntry is one event in the catalog response.
type InternalEventsCatalogEntry struct {
	Name        string   `json:"name"`
	ValueType   string   `json:"value_type"`
	ValueUnits  string   `json:"value_units,omitempty"`
	PayloadKeys []string `json:"payload_keys,omitempty"`
	Description string   `json:"description"`
}

// InternalEventsCatalogGroup is one group section in the catalog response.
type InternalEventsCatalogGroup struct {
	Name    string                       `json:"name"`
	Enabled bool                         `json:"enabled"`
	Default bool                         `json:"default"`
	Source  string                       `json:"source"`
	Events  []InternalEventsCatalogEntry `json:"events"`
}

// InternalEventsCatalogSnapshot is the GET /catalog response body.
type InternalEventsCatalogSnapshot struct {
	MasterEnabled bool                         `json:"master_enabled"`
	DestinationDB string                       `json:"destination_db"`
	Groups        []InternalEventsCatalogGroup `json:"groups"`
}

// InternalEventsGroups returns the current group state for the GET
// /groups handler.
func (e *Engine) InternalEventsGroups() []InternalEventsGroupView {
	if e.internalEvents == nil {
		return nil
	}
	em := e.internalEvents
	groups := em.activeGroups.Load()
	if groups == nil {
		return nil
	}
	em.sourceMu.RLock()
	defer em.sourceMu.RUnlock()
	out := make([]InternalEventsGroupView, 0, len(*groups))
	for _, g := range internalEventsGroupsSorted() {
		enabled, ok := (*groups)[g]
		if !ok {
			continue
		}
		def := internalEventsGroupDefaults[g]
		src := em.groupSource[g]
		if src == "" {
			src = "default"
		}
		out = append(out, InternalEventsGroupView{
			Name:    g,
			Enabled: enabled,
			Default: def,
			Source:  src,
		})
	}
	return out
}

// InternalEventsCatalog returns the full catalog snapshot for the
// GET /catalog handler.
func (e *Engine) InternalEventsCatalog() InternalEventsCatalogSnapshot {
	snap := InternalEventsCatalogSnapshot{}
	if e.internalEvents == nil {
		return snap
	}
	em := e.internalEvents
	snap.MasterEnabled = em.cfg.Enabled
	snap.DestinationDB = em.destDB()

	groups := em.activeGroups.Load()
	if groups == nil {
		return snap
	}
	em.sourceMu.RLock()
	defer em.sourceMu.RUnlock()

	byGroup := map[string][]InternalEventsCatalogEntry{}
	for _, d := range internalEventsRegistry {
		byGroup[d.Group] = append(byGroup[d.Group], InternalEventsCatalogEntry{
			Name:        d.Name,
			ValueType:   eventValueTypeName(d.ValueType),
			ValueUnits:  d.ValueUnits,
			PayloadKeys: d.PayloadKeys,
			Description: d.Description,
		})
	}
	for _, g := range internalEventsGroupsSorted() {
		enabled, ok := (*groups)[g]
		if !ok {
			continue
		}
		src := em.groupSource[g]
		if src == "" {
			src = "default"
		}
		entries := byGroup[g]
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
		snap.Groups = append(snap.Groups, InternalEventsCatalogGroup{
			Name:    g,
			Enabled: enabled,
			Default: internalEventsGroupDefaults[g],
			Source:  src,
			Events:  entries,
		})
	}
	return snap
}

// EmitInternalEvent is the exported wrapper around emitInternalEvent
// for callers in other packages (the HTTP layer, the MQTT worker,
// drip's admin loop). Same semantics as the internal call.
func (e *Engine) EmitInternalEvent(group, name string, value any, payload map[string]any, sourceDB string) {
	e.emitInternalEvent(group, name, value, payload, sourceDB)
}

// InternalEventsActive is the exported wrapper around
// internalEventsActive for the same out-of-package callers.
func (e *Engine) InternalEventsActive(group string) bool {
	return e.internalEventsActive(group)
}

// InternalEventsDropped returns the lifetime drop counter — exposed so
// the stats writer can fold it into the nanotdb/* metrics family.
func (e *Engine) InternalEventsDropped() uint64 {
	if e.internalEvents == nil {
		return 0
	}
	return e.internalEvents.dropCounter.Load()
}

// eventValueTypeName mirrors EVENTS.md naming for the response.
func eventValueTypeName(v byte) string {
	switch v {
	case EventValueNone:
		return "none"
	case Int32Sample:
		return "int32"
	case Float32Sample:
		return "float32"
	default:
		return "unknown"
	}
}

// Defensive context import for future cancellation hooks. Unused
// today but listed so callers reaching into the emitter don't need to
// add the import later.
var _ = context.Background

// emitCatalogMetricAdded emits the nanotdb.catalog.metric.added
// event on a newly-registered metric. Caller has already confirmed
// the metric was not previously registered.
func (e *Engine) emitCatalogMetricAdded(dbName, metric string, id MetricID, valueType string) {
	if !e.internalEventsActive("nanotdb.catalog") {
		return
	}
	e.emitInternalEvent("nanotdb.catalog", "nanotdb.catalog.metric.added", int32(id), map[string]any{
		"db":         dbName,
		"name":       metric,
		"value_type": valueType,
	}, dbName)
}

// emitCatalogEventAdded emits the nanotdb.catalog.event.added event
// on a newly-registered event.
func (e *Engine) emitCatalogEventAdded(dbName, eventName string, id EventID, valueType string) {
	if !e.internalEventsActive("nanotdb.catalog") {
		return
	}
	e.emitInternalEvent("nanotdb.catalog", "nanotdb.catalog.event.added", int32(id), map[string]any{
		"db":         dbName,
		"name":       eventName,
		"value_type": valueType,
	}, dbName)
}

// emitCatalogFullIfApplicable looks at an error returned from a
// catalog assignment path and emits nanotdb.catalog.full if it was
// the cap that bit. Other errors are no-ops here — they surface
// through their own channels.
func (e *Engine) emitCatalogFullIfApplicable(dbName, kind string, err error) {
	if err == nil {
		return
	}
	if !e.internalEventsActive("nanotdb.catalog") {
		return
	}
	cap := 0
	switch {
	case errors.Is(err, ErrTooManyMetrics):
		cap = MaxMetricsPerDatabase
	case errors.Is(err, ErrTooManyEvents):
		cap = MaxEventsPerDatabase
	default:
		return
	}
	e.emitInternalEvent("nanotdb.catalog", "nanotdb.catalog.full", int32(cap), map[string]any{
		"db":   dbName,
		"kind": kind,
	}, dbName)
}

// emitCatalogWriteFailed emits the nanotdb.catalog.write.failed
// event when a catalog persistence call fails. file is "catalog" or
// "events_catalog" for grep-friendly differentiation.
func (e *Engine) emitCatalogWriteFailed(dbName, file string, err error) {
	if !e.internalEventsActive("nanotdb.catalog") {
		return
	}
	e.emitInternalEvent("nanotdb.catalog", "nanotdb.catalog.write.failed", nil, map[string]any{
		"db":   dbName,
		"file": file,
		"err":  err.Error(),
	}, dbName)
}

// installWALLifecycleHooks wires the per-db WAL and events WAL to
// forward their fsync/reset events to the internal-events emitter.
// Called from getOrCreateDBWithDefaults right after the WAL handles
// are constructed. No-op when the emitter is disabled.
func (e *Engine) installWALLifecycleHooks(db *Database, dbName string) {
	if e.internalEvents == nil || !e.internalEvents.cfg.Enabled {
		return
	}
	if db == nil {
		return
	}
	if db.wal != nil {
		db.wal.SetLifecycleHook(walLifecycleHook{
			dbName: dbName,
			file:   "wal",
			onFsyncSlow: func(name, file string, ms float64) {
				if !e.internalEventsActive("nanotdb.wal.fsync") {
					return
				}
				e.emitInternalEvent("nanotdb.wal.fsync", "nanotdb.wal.fsync.slow", float32(ms), map[string]any{
					"db":   name,
					"file": file,
				}, name)
			},
			onFsyncError: func(name, file string, err error) {
				if !e.internalEventsActive("nanotdb.wal.fsync") {
					return
				}
				e.emitInternalEvent("nanotdb.wal.fsync", "nanotdb.wal.fsync.error", nil, map[string]any{
					"db":   name,
					"file": file,
					"err":  err.Error(),
				}, name)
			},
			onReset: func(name, file string, bytesReclaimed int64) {
				if !e.internalEventsActive("nanotdb.wal") {
					return
				}
				e.emitInternalEvent("nanotdb.wal", "nanotdb.wal.reset", int32(bytesReclaimed), map[string]any{
					"db":   name,
					"file": file,
				}, name)
			},
		})
	}
	if db.eventsWAL != nil {
		db.eventsWAL.SetLifecycleHook(walLifecycleHook{
			dbName: dbName,
			file:   "events.wal",
			onFsyncSlow: func(name, file string, ms float64) {
				if !e.internalEventsActive("nanotdb.wal.fsync") {
					return
				}
				e.emitInternalEvent("nanotdb.wal.fsync", "nanotdb.wal.fsync.slow", float32(ms), map[string]any{
					"db":   name,
					"file": file,
				}, name)
			},
			onFsyncError: func(name, file string, err error) {
				if !e.internalEventsActive("nanotdb.wal.fsync") {
					return
				}
				e.emitInternalEvent("nanotdb.wal.fsync", "nanotdb.wal.fsync.error", nil, map[string]any{
					"db":   name,
					"file": file,
					"err":  err.Error(),
				}, name)
			},
			onReset: func(name, file string, bytesReclaimed int64) {
				if !e.internalEventsActive("nanotdb.wal") {
					return
				}
				e.emitInternalEvent("nanotdb.wal", "nanotdb.wal.reset", int32(bytesReclaimed), map[string]any{
					"db":   name,
					"file": file,
				}, name)
			},
		})
	}
}

// ensureInternalEventsDB opens (and if missing, creates) the
// destination database for internal events. Forces
// [events].enabled = true on the manifest so the events layer is
// always available for the drain goroutine's AddEvent calls.
//
// Idempotent: safe to call on an already-open engine whose internal
// db already has events enabled.
func (e *Engine) ensureInternalEventsDB() error {
	if e.internalEvents == nil {
		return nil
	}
	dbName := e.internalEvents.destDB()

	dbDir := filepath.Join(e.RootDataDir, dbName)
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dbDir, err)
	}
	manifestPath := filepath.Join(dbDir, manifestFileName)
	if err := ensureInternalEventsManifest(manifestPath, e.dbDefaults); err != nil {
		return err
	}
	// getOrCreateDB respects the manifest we just wrote, so the
	// returned runtime has EventsEnabled = true.
	if _, _, err := e.getOrCreateDB(dbName); err != nil {
		return err
	}
	return nil
}

// ensureInternalEventsManifest writes a manifest.toml at the given
// path with [events].enabled = true. If a manifest already exists,
// the file is rewritten with the events block flipped on while every
// other field is preserved (we go through DBInfo so unrelated fields
// retain their existing values).
func ensureInternalEventsManifest(path string, defaults DBInfo) error {
	var info DBInfo
	if raw, err := os.ReadFile(path); err == nil {
		manifest := DBManifestTOML{}
		md, err := toml.Decode(string(raw), &manifest)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		warnUnknownTOMLKeys(path, md)
		// retention_action defaults to the engine default for fresh
		// installs of the internal db — we'd rather start with the
		// engine's normal retention than refuse to open.
		if manifest.Retention.RetentionAction == "" {
			manifest.Retention.RetentionAction = defaults.RetentionAction
		}
		info = dbInfoFromManifest(manifest)
	} else if !os.IsNotExist(err) {
		return err
	} else {
		info = defaults
	}
	if info.RetentionAction == "" {
		info.RetentionAction = defaults.RetentionAction
	}
	if info.Partition == "" {
		info.Partition = defaults.Partition
	}
	if !info.EventsEnabled {
		info.EventsEnabled = true
		if info.EventsMaxPayloadBytes <= 0 {
			info.EventsMaxPayloadBytes = defaults.EventsMaxPayloadBytes
		}
		if info.EventsMaxInMemoryBytes <= 0 {
			info.EventsMaxInMemoryBytes = defaults.EventsMaxInMemoryBytes
		}
		if info.EventsPageMaxRecords <= 0 {
			info.EventsPageMaxRecords = defaults.EventsPageMaxRecords
		}
		if info.EventsPageMaxBytes <= 0 {
			info.EventsPageMaxBytes = defaults.EventsPageMaxBytes
		}
		if info.EventsPageMaxAge == "" {
			info.EventsPageMaxAge = defaults.EventsPageMaxAge
		}
		if info.EventsWALMaxSegmentSize <= 0 {
			info.EventsWALMaxSegmentSize = defaults.EventsWALMaxSegmentSize
		}
		if info.EventsWALFsyncPolicy == "" {
			info.EventsWALFsyncPolicy = defaults.EventsWALFsyncPolicy
		}
	}
	normalized, err := normalizeDBInfo(info, defaults)
	if err != nil {
		return fmt.Errorf("normalize internal events manifest: %w", err)
	}
	normalized.EventsEnabled = true
	buf := bytes.NewBuffer(nil)
	if err := toml.NewEncoder(buf).Encode(dbManifestFromInfo(normalized)); err != nil {
		return fmt.Errorf("encode internal events manifest: %w", err)
	}
	return writeFileAtomic(path, buf.Bytes())
}
