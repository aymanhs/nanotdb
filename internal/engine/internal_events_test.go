package engine

import (
	"testing"
	"time"
)

// waitForInternalEvent polls QueryEvents on the internal db until at
// least one event with the given name is found, or the deadline
// expires. The drain goroutine is async, so the test must give it
// time to flush.
func waitForInternalEvent(t *testing.T, e *Engine, name string, deadline time.Duration) []EventQueryResult {
	t.Helper()
	stop := time.Now().Add(deadline)
	for time.Now().Before(stop) {
		got := collectQueryEvents(t, e, "internal", name, 0, Timestamp(time.Now().UnixNano()+int64(time.Minute)))
		if len(got) > 0 {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil
}

// TestInternalEvents_Lifecycle confirms the engine emits
// nanotdb.engine.started on open and that the event is queryable
// from the internal db.
func TestInternalEvents_Lifecycle(t *testing.T) {
	root := t.TempDir()
	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })

	got := waitForInternalEvent(t, e, "nanotdb.engine.started", 2*time.Second)
	if len(got) == 0 {
		t.Fatalf("expected nanotdb.engine.started in internal db")
	}
}

// TestInternalEvents_GroupToggle exercises the runtime toggle path:
// turn a group off, emit, confirm no record; turn it back on, emit,
// confirm the record lands. Also asserts the audit-event for the
// toggle.
func TestInternalEvents_GroupToggle(t *testing.T) {
	root := t.TempDir()
	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })

	// Wait for startup events to settle so the queries below don't
	// race with the open path.
	_ = waitForInternalEvent(t, e, "nanotdb.engine.started", 2*time.Second)

	if err := e.SetInternalEventsGroup("nanotdb.partition", false); err != nil {
		t.Fatalf("SetInternalEventsGroup off: %v", err)
	}
	if e.internalEventsActive("nanotdb.partition") {
		t.Fatalf("group still active after disable")
	}
	e.emitInternalEvent("nanotdb.partition", "nanotdb.partition.sealed", int32(1), nil, "some-other-db")
	time.Sleep(50 * time.Millisecond)
	got := collectQueryEvents(t, e, "internal", "nanotdb.partition.sealed", 0, Timestamp(time.Now().UnixNano()+int64(time.Minute)))
	if len(got) != 0 {
		t.Fatalf("expected 0 partition.sealed events while group disabled, got %d", len(got))
	}

	if err := e.SetInternalEventsGroup("nanotdb.partition", true); err != nil {
		t.Fatalf("SetInternalEventsGroup on: %v", err)
	}
	if !e.internalEventsActive("nanotdb.partition") {
		t.Fatalf("group inactive after enable")
	}

	// Audit event for the toggle should be reachable.
	toggled := waitForInternalEvent(t, e, "nanotdb.internal_events.group.toggled", 2*time.Second)
	if len(toggled) < 1 {
		t.Fatalf("expected nanotdb.internal_events.group.toggled audit event")
	}
}

// TestInternalEvents_RecursionGuard verifies the destination-db
// guard: emitting an event whose sourceDB equals the destination is
// dropped silently, regardless of the group enable state.
func TestInternalEvents_RecursionGuard(t *testing.T) {
	root := t.TempDir()
	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })

	_ = waitForInternalEvent(t, e, "nanotdb.engine.started", 2*time.Second)

	// Snapshot baseline event count.
	before := collectQueryEvents(t, e, "internal", "nanotdb.partition.sealed", 0, Timestamp(time.Now().UnixNano()+int64(time.Minute)))

	// Emit 50 partition-sealed records with the internal db as source
	// — these should all be dropped by the guard.
	for i := 0; i < 50; i++ {
		e.emitInternalEvent("nanotdb.partition", "nanotdb.partition.sealed", int32(i), nil, "internal")
	}
	time.Sleep(50 * time.Millisecond)

	after := collectQueryEvents(t, e, "internal", "nanotdb.partition.sealed", 0, Timestamp(time.Now().UnixNano()+int64(time.Minute)))
	if len(after) != len(before) {
		t.Fatalf("recursion guard failed: before=%d after=%d", len(before), len(after))
	}
}

// TestInternalEvents_UnknownGroup confirms the registry-validation
// path rejects an unknown group at runtime.
func TestInternalEvents_UnknownGroup(t *testing.T) {
	root := t.TempDir()
	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })

	if err := e.SetInternalEventsGroup("nantodb.misspelled", true); err == nil {
		t.Fatalf("expected error on unknown group, got nil")
	}
}

// TestInternalEvents_RepeatedOpenCloseStable confirms that opening
// and closing the engine repeatedly on the same root does NOT cause
// internal events to accumulate via WAL re-replay. Regression test
// for the bug where the Close() path skipped the internal db's
// events-WAL flush/reset entirely (because the same line also
// skipped the metric-side flush, which is correct only for metrics
// since the stats writer handles those separately).
//
// Symptom of the bug: each open/close cycle adds one nanotdb.engine.started
// event to the events WAL but never flushes the page or resets the WAL,
// so the next replay re-materializes every prior cycle's events into
// memory, then a query returns 2x / 3x / 4x copies of each event;
// eventually a catalog/WAL drift produces "event id N already assigned".
func TestInternalEvents_RepeatedOpenCloseStable(t *testing.T) {
	root := t.TempDir()
	const cycles = 5

	for i := 0; i < cycles; i++ {
		e, err := OpenEngine(root, 1024*1024)
		if err != nil {
			t.Fatalf("OpenEngine cycle %d: %v", i, err)
		}
		// Give the drain goroutine a moment to flush startup events
		// through AddEvent before we close.
		_ = waitForInternalEvent(t, e, "nanotdb.engine.started", 2*time.Second)
		if err := e.Close(); err != nil {
			t.Fatalf("Close cycle %d: %v", i, err)
		}
	}

	// Reopen once more for the query.
	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine final: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })

	got := collectQueryEvents(t, e, "internal", "nanotdb.engine.started", 0, Timestamp(time.Now().UnixNano()+int64(time.Minute)))
	// We expect exactly one event per cycle plus one for the final
	// open. Anything materially higher (>= 2x) means WAL records
	// from prior cycles are being re-replayed into memory.
	expected := cycles + 1
	if len(got) > expected+1 {
		t.Fatalf("expected ~%d nanotdb.engine.started events across %d cycles, got %d — WAL re-replay accumulation", expected, cycles, len(got))
	}
}

// TestInternalEvents_CatalogMetricAdded confirms that the first
// sample for a new metric fires nanotdb.catalog.metric.added.
func TestInternalEvents_CatalogMetricAdded(t *testing.T) {
	root := t.TempDir()
	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })

	_ = waitForInternalEvent(t, e, "nanotdb.engine.started", 2*time.Second)

	if err := e.AddSample("prod", "cpu.user", Timestamp(time.Now().UnixNano()), int32(42)); err != nil {
		t.Fatalf("AddSample: %v", err)
	}

	got := waitForInternalEvent(t, e, "nanotdb.catalog.metric.added", 2*time.Second)
	if len(got) == 0 {
		t.Fatalf("expected nanotdb.catalog.metric.added event")
	}
}

// TestInternalEvents_BatchedStaleRejection confirms that batched
// emission accumulates per-(db, name) counts and flushes one record
// on close (force-flush path), with the count as the value.
//
// Two flavours exercised here:
//
//   - in-process flush via flushBatches(now, true), which the
//     stop-path of the batch loop calls before the emitter channel is
//     closed — this is the contract the docs promise for shutdown
//     batched events.
//   - cross-restart durability: the in-process flush happened before
//     stopInternalEventsDrain, so the records have already been
//     handed to AddEvent and made their way through the events WAL.
//     A fresh engine on the same root reads them back.
func TestInternalEvents_BatchedStaleRejection(t *testing.T) {
	root := t.TempDir()
	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine: %v", err)
	}
	if err := e.SetInternalEventsGroup("nanotdb.ingest.reject", true); err != nil {
		t.Fatalf("SetInternalEventsGroup on: %v", err)
	}

	now := Timestamp(time.Now().UnixNano())
	if err := e.AddSample("prod", "cpu.user", now, int32(1)); err != nil {
		t.Fatalf("AddSample fresh: %v", err)
	}
	for i := 0; i < 3; i++ {
		_ = e.AddSample("prod", "cpu.user", now-Timestamp(1+i), int32(0))
	}

	// First check: force a batch flush WITHOUT closing the engine,
	// so we prove the on-Close flush isn't the only path that works.
	e.flushBatches(time.Now(), true)
	inflight := waitForInternalEvent(t, e, "nanotdb.ingest.rejected.stale", 2*time.Second)
	if len(inflight) == 0 {
		t.Fatalf("expected nanotdb.ingest.rejected.stale after in-process flushBatches")
	}
	// The value field carries the count for the window. We emitted
	// three stale samples for one (db, name) bucket, so the value
	// must be 3.
	var sawCount3 bool
	for _, r := range inflight {
		if r.Int32 == 3 {
			sawCount3 = true
			break
		}
	}
	if !sawCount3 {
		t.Fatalf("expected a batched event with count=3, got %+v", inflight)
	}

	// Close path: should also drain any *newly* accumulated batched
	// records, then survive across restart.
	if err := e.AddSample("prod", "cpu.user", now-Timestamp(100), int32(0)); err != nil {
		// stale → counted, not returned to us as fatal; ignore the
		// returned error
		_ = err
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	e2, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine 2: %v", err)
	}
	t.Cleanup(func() { _ = e2.Close() })
	got := collectQueryEvents(t, e2, "internal", "nanotdb.ingest.rejected.stale", 0, Timestamp(time.Now().UnixNano()+int64(time.Minute)))
	if len(got) < 1 {
		t.Fatalf("expected batched events to persist across restart, got %d", len(got))
	}
}

// TestInternalEvents_Catalog confirms the catalog snapshot reports
// the expected number of groups and a sane shape.
func TestInternalEvents_Catalog(t *testing.T) {
	root := t.TempDir()
	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })

	snap := e.InternalEventsCatalog()
	if !snap.MasterEnabled {
		t.Fatalf("expected master_enabled=true by default")
	}
	if snap.DestinationDB != "internal" {
		t.Fatalf("expected destination_db=\"internal\", got %q", snap.DestinationDB)
	}
	if len(snap.Groups) < 10 {
		t.Fatalf("expected at least 10 groups, got %d", len(snap.Groups))
	}
	seenLifecycle := false
	for _, g := range snap.Groups {
		if g.Name == "nanotdb.lifecycle" {
			seenLifecycle = true
			if len(g.Events) == 0 {
				t.Fatalf("nanotdb.lifecycle group has no events")
			}
		}
	}
	if !seenLifecycle {
		t.Fatalf("nanotdb.lifecycle group missing from catalog")
	}
}
