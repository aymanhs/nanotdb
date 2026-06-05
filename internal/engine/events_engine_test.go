package engine

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeEventsEnabledManifest seeds a manifest.toml in the engine root
// that opts the named database into the events layer. Must be called
// before the first AddEvent for that DB so getOrCreateDB picks up the
// flag.
func writeEventsEnabledManifest(t *testing.T, root, dbName string) {
	t.Helper()
	dir := filepath.Join(root, dbName)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	body := []byte("" +
		"[retention]\n" +
		"retention_action = \"keep\"\n\n" +
		"[events]\n" +
		"enabled = true\n")
	if err := os.WriteFile(filepath.Join(dir, "manifest.toml"), body, 0644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

func writeEventsManifest(t *testing.T, root, dbName string, eventsBlock string) {
	t.Helper()
	dir := filepath.Join(root, dbName)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	body := []byte("" +
		"[retention]\n" +
		"retention_action = \"keep\"\n\n" +
		eventsBlock)
	if err := os.WriteFile(filepath.Join(dir, "manifest.toml"), body, 0644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

// collectQueryEvents drains QueryEvents into a slice — handy for the
// table-driven assertions below.
func collectQueryEvents(t *testing.T, e *Engine, db, name string, fromTS, toTS Timestamp) []EventQueryResult {
	t.Helper()
	var out []EventQueryResult
	if err := e.QueryEvents(db, name, fromTS, toTS, func(r EventQueryResult) error {
		out = append(out, r)
		return nil
	}); err != nil {
		t.Fatalf("QueryEvents(%q,%q,%d,%d): %v", db, name, fromTS, toTS, err)
	}
	return out
}

// TestEngine_AddEvent_RoundTrip is the headline test: enable events,
// add a mix of typed and untyped events with payloads, query them
// back, verify everything round-trips. This exercises the catalog,
// the WAL, and the in-memory page in one shot.
func TestEngine_AddEvent_RoundTrip(t *testing.T) {
	root := t.TempDir()
	writeEventsEnabledManifest(t, root, "sensors")

	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })

	type input struct {
		name    string
		ts      Timestamp
		value   any
		payload []byte
	}
	cases := []input{
		{"disc.write.slow", 1_000_000, int32(542), []byte(`{"path":"/tmp"}`)},
		{"temp.over", 2_000_000, float32(31.2), nil},
		{"heartbeat", 3_000_000, nil, []byte("note")},
		{"disc.write.slow", 4_000_000, int32(870), nil},
		{"heartbeat", 5_000_000, nil, nil},
	}
	for _, c := range cases {
		if err := e.AddEvent("sensors", c.name, c.ts, c.value, c.payload); err != nil {
			t.Fatalf("AddEvent %q@%d: %v", c.name, c.ts, err)
		}
	}

	all := collectQueryEvents(t, e, "sensors", "", 0, 6_000_000)
	if len(all) != len(cases) {
		t.Fatalf("QueryEvents all: got %d, want %d", len(all), len(cases))
	}
	for i, got := range all {
		want := cases[i]
		if got.Name != want.name || got.TS != want.ts {
			t.Errorf("rec[%d]: name=%q ts=%d, want %q@%d", i, got.Name, got.TS, want.name, want.ts)
		}
		switch want.value.(type) {
		case int32:
			if got.ValueType != Int32Sample || got.Int32 != want.value.(int32) {
				t.Errorf("rec[%d] int32 mismatch: vt=%d val=%d, want %d", i, got.ValueType, got.Int32, want.value)
			}
		case float32:
			if got.ValueType != Float32Sample || got.Float32 != want.value.(float32) {
				t.Errorf("rec[%d] float32 mismatch: vt=%d val=%v, want %v", i, got.ValueType, got.Float32, want.value)
			}
		case nil:
			if got.ValueType != EventValueNone {
				t.Errorf("rec[%d] should be none-typed, got vt=%d", i, got.ValueType)
			}
		}
		if !bytes.Equal(got.Payload, want.payload) && !(len(got.Payload) == 0 && len(want.payload) == 0) {
			t.Errorf("rec[%d] payload mismatch: got %q want %q", i, got.Payload, want.payload)
		}
	}

	// Name filter restricts results.
	slow := collectQueryEvents(t, e, "sensors", "disc.write.slow", 0, 6_000_000)
	if len(slow) != 2 {
		t.Errorf("disc.write.slow filter: got %d, want 2", len(slow))
	}
	for _, r := range slow {
		if r.Name != "disc.write.slow" {
			t.Errorf("filter leaked: name=%q", r.Name)
		}
	}

	// Time-range narrowing.
	mid := collectQueryEvents(t, e, "sensors", "", 2_000_000, 4_000_000)
	if len(mid) != 3 {
		t.Errorf("time-range filter: got %d, want 3", len(mid))
	}

	// Unknown name returns empty (no error).
	none := collectQueryEvents(t, e, "sensors", "nope.does.not.exist", 0, 6_000_000)
	if len(none) != 0 {
		t.Errorf("unknown-name query returned %d records, want 0", len(none))
	}
}

// TestEngine_AddEvent_DisabledDB verifies the opt-in invariant. When
// the manifest doesn't say events.enabled = true, AddEvent must return
// ErrEventsDisabled. Same for QueryEvents.
func TestEngine_AddEvent_DisabledDB(t *testing.T) {
	root := t.TempDir()
	// Note: no manifest fixture — defaults apply, EventsEnabled = false.
	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })

	// Bootstrap the DB via a metric write first so getOrCreateDB has run.
	if err := e.AddSample("metrics", "x", 1, int32(0)); err != nil {
		t.Fatalf("seed metric: %v", err)
	}

	if err := e.AddEvent("metrics", "ev.x", 1, nil, nil); !errors.Is(err, ErrEventsDisabled) {
		t.Fatalf("AddEvent on disabled DB: got %v, want ErrEventsDisabled", err)
	}
	if err := e.QueryEvents("metrics", "", 0, 100, func(EventQueryResult) error { return nil }); !errors.Is(err, ErrEventsDisabled) {
		t.Fatalf("QueryEvents on disabled DB: got %v, want ErrEventsDisabled", err)
	}
}

// TestEngine_AddEvent_TypePinningEnforced verifies that an event's
// value type is pinned at first write and subsequent writes with a
// different value type are rejected.
func TestEngine_AddEvent_TypePinningEnforced(t *testing.T) {
	root := t.TempDir()
	writeEventsEnabledManifest(t, root, "sensors")
	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })

	if err := e.AddEvent("sensors", "ev.x", 1, int32(7), nil); err != nil {
		t.Fatalf("first AddEvent: %v", err)
	}
	if err := e.AddEvent("sensors", "ev.x", 2, float32(7.0), nil); !errors.Is(err, ErrEventTypeMismatch) {
		t.Fatalf("second AddEvent with wrong type: got %v, want ErrEventTypeMismatch", err)
	}
	if err := e.AddEvent("sensors", "ev.x", 3, nil, nil); !errors.Is(err, ErrEventTypeMismatch) {
		t.Fatalf("third AddEvent with none: got %v, want ErrEventTypeMismatch", err)
	}
}

// TestEngine_AddEvent_MonotonicOrderingPerName ensures the per-event-
// name monotonic-ts rule is enforced (crash-safety contract rule 5).
// Different event names with interleaved timestamps are allowed.
func TestEngine_AddEvent_MonotonicOrderingPerName(t *testing.T) {
	root := t.TempDir()
	writeEventsEnabledManifest(t, root, "sensors")
	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })

	if err := e.AddEvent("sensors", "ev.a", 100, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.AddEvent("sensors", "ev.b", 50, nil, nil); err != nil {
		t.Fatalf("different-name interleave should be allowed: %v", err)
	}
	if err := e.AddEvent("sensors", "ev.a", 99, nil, nil); err == nil {
		t.Fatal("expected stale-ts rejection for ev.a@99 after ev.a@100")
	}
	// Equal ts is allowed.
	if err := e.AddEvent("sensors", "ev.a", 100, nil, nil); err != nil {
		t.Fatalf("equal-ts should be allowed: %v", err)
	}
}

// TestEngine_AddEvent_PayloadCap verifies the documented 4096-byte
// hard cap is enforced at ingress.
func TestEngine_AddEvent_PayloadCap(t *testing.T) {
	root := t.TempDir()
	writeEventsEnabledManifest(t, root, "sensors")
	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })

	ok := bytes.Repeat([]byte("a"), 4096)
	if err := e.AddEvent("sensors", "ev.ok", 1, nil, ok); err != nil {
		t.Fatalf("4096-byte payload should be accepted: %v", err)
	}
	tooBig := bytes.Repeat([]byte("b"), 4097)
	if err := e.AddEvent("sensors", "ev.big", 2, nil, tooBig); !errors.Is(err, ErrEventPayloadTooLarge) {
		t.Fatalf("4097-byte payload: got %v, want ErrEventPayloadTooLarge", err)
	}
}

func TestEngine_AddEvent_UsesManifestEventsSettings(t *testing.T) {
	root := t.TempDir()
	writeEventsManifest(t, root, "sensors", ""+
		"[events]\n"+
		"enabled = true\n"+
		"max_payload_bytes = 8\n"+
		"max_in_memory_bytes = 32\n\n"+
		"[events.page]\n"+
		"max_records = 2\n"+
		"max_bytes = 64\n"+
		"max_age = \"2h\"\n\n"+
		"[events.wal]\n"+
		"max_segment_size = 12345\n"+
		"fsync_policy = \"always\"\n")

	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })

	if err := e.AddEvent("sensors", "ev.cfg", 1, nil, []byte("123456789")); !errors.Is(err, ErrEventPayloadTooLarge) {
		t.Fatalf("payload above configured max should fail: %v", err)
	}
	if err := e.AddEvent("sensors", "ev.cfg", 2, nil, []byte("12345678")); err != nil {
		t.Fatalf("payload at configured max should pass: %v", err)
	}

	db, rt, err := e.getOrCreateDB("sensors")
	if err != nil {
		t.Fatal(err)
	}
	if db.eventsWAL == nil {
		t.Fatal("expected events WAL to be open")
	}
	if db.eventsWAL.maxSegSize != 12345 {
		t.Fatalf("events wal max segment size = %d, want 12345", db.eventsWAL.maxSegSize)
	}
	if db.eventsWAL.fsyncPolicy != WALFsyncPolicyAlways {
		t.Fatalf("events wal fsync policy = %q, want %q", db.eventsWAL.fsyncPolicy, WALFsyncPolicyAlways)
	}

	day := partitionKey(rt, 2)
	page := rt.openEventsDays[day]
	if page == nil {
		t.Fatal("expected open events page")
	}
	if page.MaxRecords != 2 {
		t.Fatalf("events page max_records = %d, want 2", page.MaxRecords)
	}
	if page.MaxBytes != 64 {
		t.Fatalf("events page max_bytes = %d, want 64", page.MaxBytes)
	}
	if page.MaxAge != 2*time.Hour {
		t.Fatalf("events page max_age = %s, want 2h", page.MaxAge)
	}
	if page.MaxInMemoryBytes != 32 {
		t.Fatalf("events page max_in_memory_bytes = %d, want 32", page.MaxInMemoryBytes)
	}
}

// TestEngine_AddEvent_CrashRecovery is the headline crash-safety test:
// write events, close the engine, reopen, query — all events must be
// present with identical fields. Mirrors crash-safety contract rules
// 1 and 2 (catalog before WAL reset; replay reconstructs in-memory
// catalog).
func TestEngine_AddEvent_CrashRecovery(t *testing.T) {
	root := t.TempDir()
	writeEventsEnabledManifest(t, root, "sensors")

	type input struct {
		name  string
		ts    Timestamp
		value any
		pl    []byte
	}
	cases := []input{
		{"disc.write.slow", 100, int32(542), []byte(`{"x":1}`)},
		{"heartbeat", 200, nil, nil},
		{"temp.over", 300, float32(31.5), nil},
		{"disc.write.slow", 400, int32(870), nil},
	}

	// First incarnation: write events and close cleanly.
	{
		e, err := OpenEngine(root, 1024*1024)
		if err != nil {
			t.Fatalf("OpenEngine #1: %v", err)
		}
		for _, c := range cases {
			if err := e.AddEvent("sensors", c.name, c.ts, c.value, c.pl); err != nil {
				t.Fatalf("AddEvent %q: %v", c.name, err)
			}
		}
		if err := e.Close(); err != nil {
			t.Fatalf("Close #1: %v", err)
		}
	}

	// Catalog file must exist after close.
	catPath := filepath.Join(root, "sensors", "events.json")
	if _, err := os.Stat(catPath); err != nil {
		t.Fatalf("events.json missing after Close: %v", err)
	}

	// Second incarnation: reopen and query.
	e2, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine #2: %v", err)
	}
	t.Cleanup(func() { _ = e2.Close() })

	got := collectQueryEvents(t, e2, "sensors", "", 0, 500)
	if len(got) != len(cases) {
		t.Fatalf("post-restart Query: got %d, want %d", len(got), len(cases))
	}
	for i, r := range got {
		w := cases[i]
		if r.Name != w.name || r.TS != w.ts {
			t.Errorf("rec[%d]: name=%q ts=%d, want %q@%d", i, r.Name, r.TS, w.name, w.ts)
		}
		if !bytes.Equal(r.Payload, w.pl) && !(len(r.Payload) == 0 && len(w.pl) == 0) {
			t.Errorf("rec[%d] payload mismatch", i)
		}
	}
}

// TestEngine_AddEvent_ReplayFromWALOnly is the harder recovery test:
// crash before any catalog write. The events catalog must be rebuilt
// from newEvent records in the WAL alone.
//
// We can't actually SIGKILL inside a Go unit test — a graceful Close()
// correctly flushes the in-memory page, writes events.json, AND resets
// the WAL (the bug scripts/events_chaos.py caught earlier). So we
// snapshot the WAL bytes *before* Close, then after Close restore those
// bytes while deleting both events.json and the now-flushed
// events-*.dat. That leaves the on-disk state any real crash between
// WAL append and catalog write would produce: a populated WAL,
// no catalog file, no sealed page file.
func TestEngine_AddEvent_ReplayFromWALOnly(t *testing.T) {
	root := t.TempDir()
	writeEventsEnabledManifest(t, root, "sensors")

	dbDir := filepath.Join(root, "sensors")
	walPath := filepath.Join(dbDir, "sensors.events.wal")
	catPath := filepath.Join(dbDir, "events.json")

	// First incarnation: write the two events, snapshot the WAL bytes
	// while they're still on disk and before Close has a chance to
	// reset the WAL.
	var walSnapshot []byte
	{
		e, err := OpenEngine(root, 1024*1024)
		if err != nil {
			t.Fatal(err)
		}
		if err := e.AddEvent("sensors", "ev.alpha", 1, int32(1), nil); err != nil {
			t.Fatal(err)
		}
		if err := e.AddEvent("sensors", "ev.beta", 2, float32(2), []byte("p")); err != nil {
			t.Fatal(err)
		}
		// Snapshot the WAL bytes BEFORE graceful close (which would
		// reset them).
		walSnapshot, err = os.ReadFile(walPath)
		if err != nil {
			t.Fatalf("read events wal: %v", err)
		}
		if len(walSnapshot) == 0 {
			t.Fatal("expected non-empty events WAL after two AddEvent calls")
		}
		if err := e.Close(); err != nil {
			t.Fatal(err)
		}
	}

	// Simulate the crash state: restore the WAL bytes, delete the
	// flushed page file(s), and remove the catalog. Now the engine has
	// nothing but the WAL to recover from.
	if err := os.WriteFile(walPath, walSnapshot, 0644); err != nil {
		t.Fatalf("restore wal: %v", err)
	}
	if err := os.Remove(catPath); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove events.json: %v", err)
	}
	matches, _ := filepath.Glob(filepath.Join(dbDir, "events-*.dat"))
	for _, m := range matches {
		if err := os.Remove(m); err != nil {
			t.Fatalf("remove %s: %v", m, err)
		}
	}

	// Second incarnation: empty catalog + populated WAL. Replay must
	// rebuild the catalog from the WAL's newEvent records, and the
	// in-memory page must be populated.
	e2, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine #2: %v", err)
	}
	t.Cleanup(func() { _ = e2.Close() })

	got := collectQueryEvents(t, e2, "sensors", "", 0, 100)
	if len(got) != 2 {
		t.Fatalf("WAL-only replay: got %d events, want 2", len(got))
	}
	if got[0].Name != "ev.alpha" || got[0].Int32 != 1 {
		t.Errorf("rec[0] = %+v, want ev.alpha=1", got[0])
	}
	if got[1].Name != "ev.beta" || got[1].Float32 != 2.0 {
		t.Errorf("rec[1] = %+v, want ev.beta=2.0", got[1])
	}
}

// TestEngine_AddEvent_NoEventsForMetricsOnlyDB confirms the metric
// and events layers stay independent. A DB with only metrics doesn't
// suddenly get an events catalog or WAL on disk.
func TestEngine_AddEvent_NoEventsForMetricsOnlyDB(t *testing.T) {
	root := t.TempDir()
	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.AddSample("metrics", "x", 1, int32(1)); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	dbDir := filepath.Join(root, "metrics")
	if _, err := os.Stat(filepath.Join(dbDir, "events.json")); !os.IsNotExist(err) {
		t.Errorf("events.json should not exist for metrics-only DB; stat err = %v", err)
	}
	walPattern := filepath.Join(dbDir, "*.events.wal")
	if matches, _ := filepath.Glob(walPattern); len(matches) > 0 {
		t.Errorf("events.wal should not exist for metrics-only DB; found %v", matches)
	}
}

// TestEngine_AddEvent_MultiplePartitions verifies that events spanning
// partition boundaries flush+query correctly. We use 'day' partitioning
// (the default) and timestamps separated by >1 day.
func TestEngine_AddEvent_MultiplePartitions(t *testing.T) {
	root := t.TempDir()
	writeEventsEnabledManifest(t, root, "sensors")
	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })

	// Two events ~2 days apart.
	const day = int64(24 * 3600 * 1_000_000_000)
	tsDay1 := Timestamp(day * 365 * 56) // arbitrary far-future
	tsDay2 := tsDay1 + Timestamp(2*day)

	if err := e.AddEvent("sensors", "ev.x", tsDay1, nil, []byte("day1")); err != nil {
		t.Fatal(err)
	}
	if err := e.AddEvent("sensors", "ev.x", tsDay2, nil, []byte("day2")); err != nil {
		t.Fatal(err)
	}

	got := collectQueryEvents(t, e, "sensors", "ev.x", tsDay1-1, tsDay2+1)
	if len(got) != 2 {
		t.Fatalf("got %d, want 2 (across two partitions)", len(got))
	}
	if string(got[0].Payload) != "day1" || string(got[1].Payload) != "day2" {
		t.Errorf("partition-order payloads wrong: %q / %q", got[0].Payload, got[1].Payload)
	}
}

// TestEngine_AddEvent_InvalidArgs covers the API-boundary validations
// that catch bad inputs before they reach the catalog or WAL.
func TestEngine_AddEvent_InvalidArgs(t *testing.T) {
	root := t.TempDir()
	writeEventsEnabledManifest(t, root, "sensors")
	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })

	cases := []struct {
		desc  string
		db    string
		name  string
		value any
		want  string
	}{
		{"empty name", "sensors", "", nil, "empty"},
		{"name too long", "sensors", strings.Repeat("x", MaxEventNameLen+1), nil, "too long"},
		{"unsupported value type", "sensors", "ev.x", int64(7), "unsupported"},
		{"invalid db name", "/etc/passwd", "ev.x", nil, "invalid"},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			err := e.AddEvent(c.db, c.name, 1, c.value, nil)
			if err == nil {
				t.Fatalf("expected error containing %q", c.want)
			}
			if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(c.want)) {
				t.Fatalf("error %q does not contain %q", err.Error(), c.want)
			}
		})
	}
}

// TestEngine_QueryEvents_InvalidArgs covers the QueryEvents validation.
func TestEngine_QueryEvents_InvalidArgs(t *testing.T) {
	root := t.TempDir()
	writeEventsEnabledManifest(t, root, "sensors")
	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })

	// Nil callback.
	if err := e.QueryEvents("sensors", "", 0, 100, nil); err == nil {
		t.Error("expected error for nil callback")
	}
	// Inverted range.
	if err := e.QueryEvents("sensors", "", 100, 50, func(EventQueryResult) error { return nil }); err == nil {
		t.Error("expected error for toTS < fromTS")
	}
}

// TestEngine_AddEvent_PerDBIsolation confirms two databases with
// events enabled have independent catalogs (different EventIDs for
// the same name is fine).
func TestEngine_AddEvent_PerDBIsolation(t *testing.T) {
	root := t.TempDir()
	writeEventsEnabledManifest(t, root, "alpha")
	writeEventsEnabledManifest(t, root, "beta")
	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })

	if err := e.AddEvent("alpha", "ev.x", 1, int32(1), nil); err != nil {
		t.Fatal(err)
	}
	if err := e.AddEvent("beta", "ev.x", 1, float32(2.0), nil); err != nil {
		t.Fatalf("same name should be free to take a different type in a different DB: %v", err)
	}
	// Each side sees only its own.
	a := collectQueryEvents(t, e, "alpha", "", 0, 100)
	b := collectQueryEvents(t, e, "beta", "", 0, 100)
	if len(a) != 1 || a[0].Int32 != 1 {
		t.Errorf("alpha events wrong: %+v", a)
	}
	if len(b) != 1 || b[0].Float32 != 2.0 {
		t.Errorf("beta events wrong: %+v", b)
	}
}

// TestEngine_QueryEvents_CallbackErrorPropagates checks the callback
// short-circuit path.
func TestEngine_QueryEvents_CallbackErrorPropagates(t *testing.T) {
	root := t.TempDir()
	writeEventsEnabledManifest(t, root, "sensors")
	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })

	for i := range 5 {
		if err := e.AddEvent("sensors", "ev.x", Timestamp(i+1), nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	stopAt := fmt.Errorf("stop")
	var seen int
	err = e.QueryEvents("sensors", "", 0, 100, func(EventQueryResult) error {
		seen++
		if seen == 2 {
			return stopAt
		}
		return nil
	})
	if !errors.Is(err, stopAt) {
		t.Errorf("callback error not propagated: got %v", err)
	}
	if seen != 2 {
		t.Errorf("seen = %d, want 2 (early stop)", seen)
	}
}
