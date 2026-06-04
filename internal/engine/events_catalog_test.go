package engine

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEventCatalog_AssignAndRetrieve covers the basic register-and-lookup
// flow: GetOrAssignEventID assigns IDs sequentially starting at 1, the
// catalog goes dirty, and subsequent lookups round-trip through both name
// and id.
func TestEventCatalog_AssignAndRetrieve(t *testing.T) {
	dir := t.TempDir()
	c, err := LoadEventCatalog(filepath.Join(dir, "events.json"))
	if err != nil {
		t.Fatalf("LoadEventCatalog failed: %v", err)
	}
	defer c.Close()

	id, err := c.GetOrAssignEventID("disc.write.slow", Int32Sample)
	if err != nil {
		t.Fatalf("GetOrAssignEventID failed: %v", err)
	}
	if id != 1 {
		t.Fatalf("expected first id 1, got %d", id)
	}
	if !c.IsDirty() {
		t.Fatal("expected catalog dirty after first assignment")
	}

	id2, err := c.GetOrAssignEventID("temp.office.overheat", Float32Sample)
	if err != nil {
		t.Fatalf("second assignment failed: %v", err)
	}
	if id2 != 2 {
		t.Fatalf("expected second id 2, got %d", id2)
	}

	// Re-lookup returns the same id without changing state.
	idAgain, err := c.GetOrAssignEventID("disc.write.slow", Int32Sample)
	if err != nil {
		t.Fatalf("re-lookup failed: %v", err)
	}
	if idAgain != 1 {
		t.Fatalf("re-lookup returned %d, want 1", idAgain)
	}

	name, entry, ok := c.GetEventByID(1)
	if !ok {
		t.Fatal("GetEventByID(1) not found")
	}
	if name != "disc.write.slow" || entry.ValueType != Int32Sample {
		t.Fatalf("unexpected GetEventByID(1) result: name=%q vt=%d", name, entry.ValueType)
	}
}

// TestEventCatalog_TypeMismatch verifies that registering an existing
// event name with a different value type returns ErrEventTypeMismatch
// rather than silently accepting it or assigning a new id.
func TestEventCatalog_TypeMismatch(t *testing.T) {
	c, err := LoadEventCatalog(filepath.Join(t.TempDir(), "events.json"))
	if err != nil {
		t.Fatalf("LoadEventCatalog: %v", err)
	}
	defer c.Close()

	if _, err := c.GetOrAssignEventID("ev.a", Int32Sample); err != nil {
		t.Fatalf("first assign: %v", err)
	}
	if _, err := c.GetOrAssignEventID("ev.a", Float32Sample); !errors.Is(err, ErrEventTypeMismatch) {
		t.Fatalf("expected ErrEventTypeMismatch, got %v", err)
	}
	if _, err := c.GetOrAssignEventID("ev.a", EventValueNone); !errors.Is(err, ErrEventTypeMismatch) {
		t.Fatalf("expected ErrEventTypeMismatch for none-on-int32, got %v", err)
	}
}

// TestEventCatalog_NoneValueType ensures EventValueNone is a legal pinned
// type and persists across a write+reload cycle.
func TestEventCatalog_NoneValueType(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.json")
	c, err := LoadEventCatalog(path)
	if err != nil {
		t.Fatalf("LoadEventCatalog: %v", err)
	}

	if _, err := c.GetOrAssignEventID("heartbeat", EventValueNone); err != nil {
		t.Fatalf("none-typed assign failed: %v", err)
	}
	if err := c.WriteCatalog(); err != nil {
		t.Fatalf("WriteCatalog: %v", err)
	}
	c.Close()

	c2, err := LoadEventCatalog(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	defer c2.Close()
	e, ok := c2.GetEventEntry("heartbeat")
	if !ok {
		t.Fatal("heartbeat missing after reload")
	}
	if e.ValueType != EventValueNone {
		t.Fatalf("reload value type: got %d want %d", e.ValueType, EventValueNone)
	}
}

// TestEventCatalog_CapEnforced is the regression test for the hard
// architectural cap. Filling to MaxEventsPerDatabase succeeds; the next
// new name returns ErrTooManyEvents. Re-registering an existing name
// after the cap is reached must still succeed (no new id required).
func TestEventCatalog_CapEnforced(t *testing.T) {
	c, err := LoadEventCatalog(filepath.Join(t.TempDir(), "events.json"))
	if err != nil {
		t.Fatalf("LoadEventCatalog: %v", err)
	}
	defer c.Close()

	for i := 0; i < MaxEventsPerDatabase; i++ {
		name := "ev." + itoaTest(i)
		if _, err := c.GetOrAssignEventID(name, Int32Sample); err != nil {
			t.Fatalf("assign #%d failed: %v", i, err)
		}
	}

	// Next new name must be rejected.
	_, err = c.GetOrAssignEventID("ev.overflow", Int32Sample)
	if !errors.Is(err, ErrTooManyEvents) {
		t.Fatalf("expected ErrTooManyEvents, got %v", err)
	}

	// An existing name must still resolve even when the cap is full.
	if _, err := c.GetOrAssignEventID("ev.0", Int32Sample); err != nil {
		t.Fatalf("re-register of existing name failed at cap: %v", err)
	}
}

// TestEventCatalog_NameValidation rejects empty and over-length names at
// the in-memory API boundary, mirroring the metric-side hardening.
func TestEventCatalog_NameValidation(t *testing.T) {
	c, _ := LoadEventCatalog(filepath.Join(t.TempDir(), "events.json"))
	defer c.Close()

	if _, err := c.GetOrAssignEventID("", Int32Sample); !errors.Is(err, ErrEventNameEmpty) {
		t.Fatalf("expected ErrEventNameEmpty, got %v", err)
	}
	longName := strings.Repeat("x", MaxEventNameLen+1)
	if _, err := c.GetOrAssignEventID(longName, Int32Sample); !errors.Is(err, ErrEventNameTooLong) {
		t.Fatalf("expected ErrEventNameTooLong, got %v", err)
	}
}

// TestEventCatalog_LastTSMonotonic verifies LastTS / UpdateLastByEventID
// implements the per-event-name monotonic ordering rule that AddEvent
// will rely on. Equal timestamps are allowed; strictly older are tracked
// no-op (the catalog never regresses LastTS).
func TestEventCatalog_LastTSMonotonic(t *testing.T) {
	c, _ := LoadEventCatalog(filepath.Join(t.TempDir(), "events.json"))
	defer c.Close()

	id, _ := c.GetOrAssignEventID("ev.x", Int32Sample)

	if _, ok := c.LastTS(id); ok {
		t.Fatal("expected no LastTS before any update")
	}
	if err := c.UpdateLastByEventID(id, 100); err != nil {
		t.Fatalf("update 100: %v", err)
	}
	if ts, ok := c.LastTS(id); !ok || ts != 100 {
		t.Fatalf("LastTS after 100: got (%d,%v)", ts, ok)
	}
	// Equal ts: allowed, no regression.
	if err := c.UpdateLastByEventID(id, 100); err != nil {
		t.Fatalf("update 100 again: %v", err)
	}
	if ts, _ := c.LastTS(id); ts != 100 {
		t.Fatalf("LastTS after equal: %d", ts)
	}
	// Older ts: must not move backwards.
	if err := c.UpdateLastByEventID(id, 50); err != nil {
		t.Fatalf("update 50: %v", err)
	}
	if ts, _ := c.LastTS(id); ts != 100 {
		t.Fatalf("LastTS must not regress: got %d, want 100", ts)
	}
	// Newer ts: advances.
	if err := c.UpdateLastByEventID(id, 200); err != nil {
		t.Fatalf("update 200: %v", err)
	}
	if ts, _ := c.LastTS(id); ts != 200 {
		t.Fatalf("LastTS after 200: %d", ts)
	}
}

// TestEventCatalog_Persistence_RoundTrip writes, closes, reloads, and
// verifies every catalog field survives the JSON round trip.
func TestEventCatalog_Persistence_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.json")

	c, err := LoadEventCatalog(path)
	if err != nil {
		t.Fatalf("LoadEventCatalog: %v", err)
	}
	if _, err := c.GetOrAssignEventID("disc.write.slow", Int32Sample); err != nil {
		t.Fatalf("assign a: %v", err)
	}
	if _, err := c.GetOrAssignEventID("temp.over", Float32Sample); err != nil {
		t.Fatalf("assign b: %v", err)
	}
	if _, err := c.GetOrAssignEventID("heartbeat", EventValueNone); err != nil {
		t.Fatalf("assign c: %v", err)
	}
	if err := c.WriteCatalog(); err != nil {
		t.Fatalf("WriteCatalog: %v", err)
	}
	c.Close()

	c2, err := LoadEventCatalog(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	defer c2.Close()

	want := []EventInfo{
		{"disc.write.slow", 1, Int32Sample},
		{"heartbeat", 3, EventValueNone},
		{"temp.over", 2, Float32Sample},
	}
	got := c2.ListEvents()
	if len(got) != len(want) {
		t.Fatalf("ListEvents len=%d, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("entry %d: got %+v, want %+v", i, got[i], w)
		}
	}
}

// TestEventCatalog_LoadRejectsBadFile exercises the validation paths in
// LoadEventCatalog. Each malformed file must be rejected with a clear
// error rather than silently accepted.
func TestEventCatalog_LoadRejectsBadFile(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantSub string
	}{
		{"empty_name", `{"events":[{"name":"","id":1,"value_type":"int32"}]}`, "empty name"},
		{"id_zero", `{"events":[{"name":"ev.a","id":0,"value_type":"int32"}]}`, "invalid event id 0"},
		{"bad_value_type", `{"events":[{"name":"ev.a","id":1,"value_type":"string"}]}`, "invalid event value_type"},
		{"id_overflow", `{"events":[{"name":"ev.a","id":1024,"value_type":"int32"}]}`, "exceeds MaxEventsPerDatabase"},
		{"dup_name", `{"events":[{"name":"a","id":1,"value_type":"int32"},{"name":"a","id":2,"value_type":"int32"}]}`, "duplicate event name"},
		{"dup_id", `{"events":[{"name":"a","id":1,"value_type":"int32"},{"name":"b","id":1,"value_type":"int32"}]}`, "duplicate event id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "events.json")
			if err := os.WriteFile(path, []byte(tc.body), 0644); err != nil {
				t.Fatalf("write fixture: %v", err)
			}
			_, err := LoadEventCatalog(path)
			if err == nil {
				t.Fatalf("expected error for %s, got nil", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantSub)
			}
		})
	}
}

// TestEventCatalog_WriteCatalogToSafetyRails covers the same two safety
// rails the metric WriteCatalogTo has: empty path rejected, canonical
// path rejected.
func TestEventCatalog_WriteCatalogToSafetyRails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.json")
	c, err := LoadEventCatalog(path)
	if err != nil {
		t.Fatalf("LoadEventCatalog: %v", err)
	}
	defer c.Close()

	if err := c.WriteCatalogTo(""); err == nil {
		t.Fatal("expected WriteCatalogTo(\"\") to fail")
	}
	if err := c.WriteCatalogTo(path); err == nil || !strings.Contains(err.Error(), "refusing to overwrite canonical") {
		t.Fatalf("expected canonical-path refusal, got %v", err)
	}
}

// TestEventCatalog_EnsureEventEntry covers the WAL-replay materialization
// path. Mirrors what events WAL replay will call when it sees a newEvent
// record.
func TestEventCatalog_EnsureEventEntry(t *testing.T) {
	c, _ := LoadEventCatalog(filepath.Join(t.TempDir(), "events.json"))
	defer c.Close()

	// First call materializes a new entry and sets dirty.
	if err := c.EnsureEventEntry("ev.a", 5, Int32Sample); err != nil {
		t.Fatalf("first EnsureEventEntry: %v", err)
	}
	if !c.IsDirty() {
		t.Fatal("expected dirty after EnsureEventEntry")
	}
	name, _, ok := c.GetEventByID(5)
	if !ok || name != "ev.a" {
		t.Fatalf("GetEventByID(5)=%q,%v", name, ok)
	}

	// Same (name,id,type) idempotent.
	if err := c.EnsureEventEntry("ev.a", 5, Int32Sample); err != nil {
		t.Fatalf("idempotent EnsureEventEntry: %v", err)
	}

	// Conflicting id for existing name: hard error.
	if err := c.EnsureEventEntry("ev.a", 6, Int32Sample); err == nil {
		t.Fatal("expected id-mismatch error")
	}

	// Conflicting type for existing entry: hard error.
	if err := c.EnsureEventEntry("ev.a", 5, Float32Sample); err == nil {
		t.Fatal("expected type-mismatch error")
	}

	// Conflicting name for already-used id: hard error.
	if err := c.EnsureEventEntry("ev.b", 5, Int32Sample); err == nil {
		t.Fatal("expected id-already-assigned error")
	}

	// Invalid id ranges.
	if err := c.EnsureEventEntry("ev.c", 0, Int32Sample); err == nil {
		t.Fatal("expected id=0 to be rejected")
	}
	if err := c.EnsureEventEntry("ev.c", MaxEventsPerDatabase+1, Int32Sample); err == nil {
		t.Fatal("expected id>cap to be rejected")
	}
}

// itoaTest avoids a strconv import bloat in TestEventCatalog_CapEnforced
// and stays cheap enough to call 1023 times.
func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [12]byte
	i := len(buf)
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
