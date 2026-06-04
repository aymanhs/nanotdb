package engine

import (
	"bytes"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// helper: build a (catalog, wal, paths) trio in a fresh temp dir.
func newEventsWALTestRig(t *testing.T) (*EventCatalog, *EventsWAL, string, string) {
	t.Helper()
	dir := t.TempDir()
	catPath := filepath.Join(dir, "events.json")
	walPath := filepath.Join(dir, "test.events.wal")
	cat, err := LoadEventCatalog(catPath)
	if err != nil {
		t.Fatalf("LoadEventCatalog: %v", err)
	}
	wal, err := NewEventsWAL(walPath, 1<<20, WALFsyncPolicySegment)
	if err != nil {
		t.Fatalf("NewEventsWAL: %v", err)
	}
	return cat, wal, catPath, walPath
}

func encInt32(v int32) [4]byte {
	var b [4]byte
	b[0] = byte(uint32(v))
	b[1] = byte(uint32(v) >> 8)
	b[2] = byte(uint32(v) >> 16)
	b[3] = byte(uint32(v) >> 24)
	return b
}

func encFloat32(v float32) [4]byte {
	bits := math.Float32bits(v)
	var b [4]byte
	b[0] = byte(bits)
	b[1] = byte(bits >> 8)
	b[2] = byte(bits >> 16)
	b[3] = byte(bits >> 24)
	return b
}

// TestEventsWAL_NewEventThenKnownEvent_RoundTrip exercises the core
// design contract: first record carries inline name+vtype, subsequent
// records for the same event omit them, and replay reconstructs both
// the catalog and the record stream verbatim.
func TestEventsWAL_NewEventThenKnownEvent_RoundTrip(t *testing.T) {
	cat, wal, _, _ := newEventsWALTestRig(t)
	defer wal.Close()
	defer cat.Close()

	id, err := cat.GetOrAssignEventID("disc.write.slow", Int32Sample)
	if err != nil {
		t.Fatalf("assign: %v", err)
	}

	// First write is a newEvent record (carries inline name + vtype).
	if _, err := AppendEventWithName(wal, id, "disc.write.slow", 100, Int32Sample, encInt32(542), []byte(`{"path":"/tmp"}`)); err != nil {
		t.Fatalf("first append: %v", err)
	}

	// Subsequent occurrences are known-event records.
	if _, err := AppendEvent(wal, id, 200, Int32Sample, encInt32(870), nil); err != nil {
		t.Fatalf("second append: %v", err)
	}
	if _, err := AppendEvent(wal, id, 300, Int32Sample, encInt32(1100), []byte("opaque-bytes-here")); err != nil {
		t.Fatalf("third append: %v", err)
	}

	if err := wal.Fsync(); err != nil {
		t.Fatalf("fsync: %v", err)
	}

	// Fresh catalog to prove the WAL alone can reconstruct it.
	fresh, err := LoadEventCatalog(filepath.Join(t.TempDir(), "events.json"))
	if err != nil {
		t.Fatalf("fresh LoadEventCatalog: %v", err)
	}
	defer fresh.Close()

	recs, err := wal.RecordsWithCatalog(fresh)
	if err != nil {
		t.Fatalf("RecordsWithCatalog: %v", err)
	}
	if len(recs) != 3 {
		t.Fatalf("expected 3 records, got %d", len(recs))
	}
	// Replay should have populated the fresh catalog.
	if _, e, ok := fresh.GetEventByID(id); !ok || e.ValueType != Int32Sample {
		t.Fatalf("fresh catalog missing newEvent entry: ok=%v entry=%+v", ok, e)
	}

	want := []struct {
		ts   Timestamp
		val  int32
		pl   string
		name string
	}{
		{100, 542, `{"path":"/tmp"}`, "disc.write.slow"},
		{200, 870, "", "disc.write.slow"},
		{300, 1100, "opaque-bytes-here", "disc.write.slow"},
	}
	for i, w := range want {
		r := recs[i]
		if r.TS != w.ts {
			t.Errorf("rec[%d].TS = %d, want %d", i, r.TS, w.ts)
		}
		if r.Int32Value != w.val {
			t.Errorf("rec[%d].Int32Value = %d, want %d", i, r.Int32Value, w.val)
		}
		if string(r.Payload) != w.pl {
			t.Errorf("rec[%d].Payload = %q, want %q", i, r.Payload, w.pl)
		}
		// Name is populated on every record by RecordsWithCatalog (it
		// looks up by id for known-event records).
		if r.EventName != w.name {
			t.Errorf("rec[%d].EventName = %q, want %q", i, r.EventName, w.name)
		}
	}
}

// TestEventsWAL_NoneValueType_NoValueBytes ensures a none-typed event
// writes zero value bytes — the hot-path size assumption documented in
// EVENTS.md depends on this.
func TestEventsWAL_NoneValueType_NoValueBytes(t *testing.T) {
	cat, wal, _, walPath := newEventsWALTestRig(t)
	defer wal.Close()
	defer cat.Close()

	id, _ := cat.GetOrAssignEventID("heartbeat", EventValueNone)

	if _, err := AppendEventWithName(wal, id, "heartbeat", 1234567890, EventValueNone, [4]byte{}, nil); err != nil {
		t.Fatalf("append newEvent: %v", err)
	}
	if _, err := AppendEvent(wal, id, 1234567900, EventValueNone, [4]byte{}, nil); err != nil {
		t.Fatalf("append known: %v", err)
	}
	_ = wal.Fsync()

	recs, err := wal.RecordsWithCatalog(cat)
	if err != nil {
		t.Fatalf("RecordsWithCatalog: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("want 2 recs, got %d", len(recs))
	}
	for i, r := range recs {
		if r.ValueType != EventValueNone {
			t.Errorf("rec[%d].ValueType = %d, want EventValueNone", i, r.ValueType)
		}
		if r.Int32Value != 0 || r.Float32Value != 0 {
			t.Errorf("rec[%d] has unexpected value bytes: i32=%d f32=%v", i, r.Int32Value, r.Float32Value)
		}
		if len(r.Payload) != 0 {
			t.Errorf("rec[%d] has unexpected payload: %q", i, r.Payload)
		}
	}

	// Sanity: the on-disk record should be small. Two records, each:
	// 2 (id) + 8 (ts) + 1 (flags) + [newEvent: 1+9+1 for first only] + 0 (no value) + 1 (PayloadLen=0)
	// First record body = 12 + 1 + 11 = ~22 bytes; second = 12 + 0 = 12 bytes.
	// Plus uvarint length prefixes (1 byte each). So total < 40 bytes.
	st, err := os.Stat(walPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if st.Size() > 64 {
		t.Errorf("none-typed two-record WAL larger than expected: %d bytes", st.Size())
	}
}

// TestEventsWAL_Float32Roundtrip exercises the float32 decode branch.
func TestEventsWAL_Float32Roundtrip(t *testing.T) {
	cat, wal, _, _ := newEventsWALTestRig(t)
	defer wal.Close()
	defer cat.Close()

	id, _ := cat.GetOrAssignEventID("temp.over", Float32Sample)
	if _, err := AppendEventWithName(wal, id, "temp.over", 42, Float32Sample, encFloat32(31.5), nil); err != nil {
		t.Fatalf("append: %v", err)
	}
	if _, err := AppendEvent(wal, id, 43, Float32Sample, encFloat32(-0.25), []byte("p")); err != nil {
		t.Fatalf("append: %v", err)
	}
	recs, err := wal.RecordsWithCatalog(cat)
	if err != nil {
		t.Fatalf("RecordsWithCatalog: %v", err)
	}
	if len(recs) != 2 || recs[0].Float32Value != 31.5 || recs[1].Float32Value != -0.25 {
		t.Fatalf("float decode wrong: %+v", recs)
	}
	if !bytes.Equal(recs[1].Payload, []byte("p")) {
		t.Fatalf("payload roundtrip: %q", recs[1].Payload)
	}
}

// TestEventsWAL_UnknownEventIDIsHardError verifies that a non-newEvent
// record whose EventID has no catalog entry surfaces as a hard error
// (no silent drop) — crash-safety contract rule 3 in EVENTS.md.
func TestEventsWAL_UnknownEventIDIsHardError(t *testing.T) {
	cat, wal, _, _ := newEventsWALTestRig(t)
	defer wal.Close()
	defer cat.Close()

	// Append a known-event record for an id we never registered. This
	// would only happen in real life via WAL corruption or a wrong-file
	// mixup — the engine layer never emits a known-event record without
	// a prior newEvent record. We bypass that here to exercise the
	// failure mode.
	if _, err := AppendEvent(wal, 99, 1, Int32Sample, encInt32(7), nil); err != nil {
		t.Fatalf("force-append: %v", err)
	}
	_, err := wal.RecordsWithCatalog(cat)
	if !errors.Is(err, ErrEventsWALUnknownEventID) {
		t.Fatalf("expected ErrEventsWALUnknownEventID, got %v", err)
	}
}

// TestEventsWAL_NewEventCatalogConflictIsHardError verifies that if the
// catalog already has the event registered with a different type, a
// newEvent WAL record with a conflicting inline type is rejected at
// replay (crash-safety rule 4).
func TestEventsWAL_NewEventCatalogConflictIsHardError(t *testing.T) {
	cat, wal, _, _ := newEventsWALTestRig(t)
	defer wal.Close()
	defer cat.Close()

	id, _ := cat.GetOrAssignEventID("ev.x", Int32Sample)
	// Inline ValueType disagrees with catalog: Float32Sample inline vs Int32Sample in catalog.
	if _, err := AppendEventWithName(wal, id, "ev.x", 1, Float32Sample, encFloat32(1.0), nil); err != nil {
		t.Fatalf("force-append conflicting newEvent: %v", err)
	}
	if _, err := wal.RecordsWithCatalog(cat); err == nil {
		t.Fatal("expected catalog/inline type conflict to be a hard error")
	}
}

// TestEventsWAL_NewEventMaterializesIntoFreshCatalog is the headline
// recovery test: crash after WAL append but before catalog write →
// restart → replay against an empty catalog → catalog is rebuilt
// from the inline data. Mirrors crash-safety rule 2.
func TestEventsWAL_NewEventMaterializesIntoFreshCatalog(t *testing.T) {
	_, wal, _, _ := newEventsWALTestRig(t)
	defer wal.Close()

	// Write three newEvent records (different names) without persisting
	// any events catalog. Then replay into a fresh catalog and verify
	// every entry came through.
	if _, err := AppendEventWithName(wal, 1, "ev.alpha", 10, Int32Sample, encInt32(1), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := AppendEventWithName(wal, 2, "ev.beta", 20, Float32Sample, encFloat32(2), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := AppendEventWithName(wal, 3, "ev.gamma", 30, EventValueNone, [4]byte{}, []byte("ctx")); err != nil {
		t.Fatal(err)
	}

	fresh, err := LoadEventCatalog(filepath.Join(t.TempDir(), "events.json"))
	if err != nil {
		t.Fatalf("LoadEventCatalog: %v", err)
	}
	defer fresh.Close()

	recs, err := wal.RecordsWithCatalog(fresh)
	if err != nil {
		t.Fatalf("RecordsWithCatalog: %v", err)
	}
	if len(recs) != 3 {
		t.Fatalf("want 3 recs, got %d", len(recs))
	}

	want := map[string]byte{
		"ev.alpha": Int32Sample,
		"ev.beta":  Float32Sample,
		"ev.gamma": EventValueNone,
	}
	for name, vt := range want {
		entry, ok := fresh.GetEventEntry(name)
		if !ok {
			t.Errorf("fresh catalog missing %q after replay", name)
			continue
		}
		if entry.ValueType != vt {
			t.Errorf("%q: catalog type = %d, want %d", name, entry.ValueType, vt)
		}
	}
}

// TestEventsWAL_CrashTailTruncation simulates a truncated record at the
// tail of the WAL (mid-append crash). Replay must stop at the truncated
// record and not return an error, mirroring metric WAL behavior.
func TestEventsWAL_CrashTailTruncation(t *testing.T) {
	cat, wal, _, walPath := newEventsWALTestRig(t)
	id, _ := cat.GetOrAssignEventID("ev.t", Int32Sample)
	if _, err := AppendEventWithName(wal, id, "ev.t", 1, Int32Sample, encInt32(1), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := AppendEvent(wal, id, 2, Int32Sample, encInt32(2), nil); err != nil {
		t.Fatal(err)
	}
	if err := wal.Fsync(); err != nil {
		t.Fatal(err)
	}
	wal.Close()
	cat.Close()

	// Truncate the file mid-second-record. The first record must
	// survive; the second is gone.
	st, err := os.Stat(walPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(walPath, st.Size()-3); err != nil {
		t.Fatal(err)
	}

	// Reopen and decode against a fresh catalog (records the newEvent
	// will register).
	wal2, err := OpenAndRecoverEventsWAL(walPath, WALFsyncPolicySegment)
	if err != nil {
		t.Fatalf("OpenAndRecoverEventsWAL: %v", err)
	}
	defer wal2.Close()
	cat2, err := LoadEventCatalog(filepath.Join(t.TempDir(), "events.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer cat2.Close()

	recs, err := wal2.RecordsWithCatalog(cat2)
	if err != nil {
		t.Fatalf("RecordsWithCatalog after truncation: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("expected 1 surviving record, got %d", len(recs))
	}
	if recs[0].TS != 1 || recs[0].Int32Value != 1 {
		t.Errorf("surviving record wrong: %+v", recs[0])
	}
}

// TestEventsWAL_FsyncAlwaysPolicy verifies that with the always policy,
// every append fsyncs (FsyncCount tracks AppendCount). Mirrors the
// metric WAL's policy semantics.
func TestEventsWAL_FsyncAlwaysPolicy(t *testing.T) {
	dir := t.TempDir()
	walPath := filepath.Join(dir, "x.events.wal")
	wal, err := NewEventsWAL(walPath, 1<<20, WALFsyncPolicyAlways)
	if err != nil {
		t.Fatal(err)
	}
	defer wal.Close()

	for i := 0; i < 5; i++ {
		if _, err := AppendEventWithName(wal, EventID(i+1), "ev", Timestamp(i+1), Int32Sample, encInt32(int32(i)), nil); err != nil {
			t.Fatal(err)
		}
	}
	stats := wal.Stats()
	if stats.AppendCount != 5 || stats.FsyncCount < 5 {
		t.Fatalf("always-policy expected >= 5 fsyncs for 5 appends; got append=%d fsync=%d", stats.AppendCount, stats.FsyncCount)
	}
}

// TestEventsWAL_SegmentPolicyOnlyFsyncsAtSegmentEnd verifies the segment
// policy does NOT fsync on every append. With a large segment cap, the
// only fsync should come from an explicit call.
func TestEventsWAL_SegmentPolicyOnlyFsyncsAtSegmentEnd(t *testing.T) {
	dir := t.TempDir()
	walPath := filepath.Join(dir, "x.events.wal")
	wal, err := NewEventsWAL(walPath, 1<<20, WALFsyncPolicySegment)
	if err != nil {
		t.Fatal(err)
	}
	defer wal.Close()

	for i := 0; i < 5; i++ {
		if _, err := AppendEventWithName(wal, EventID(i+1), "ev", Timestamp(i+1), EventValueNone, [4]byte{}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if got := wal.Stats().FsyncCount; got != 0 {
		t.Fatalf("segment-policy below segment cap: want 0 fsyncs, got %d", got)
	}
}

// TestEventsWAL_ResetTruncatesAndRecordsFlush verifies Reset clears the
// file and updates the stats (FlushCount, FlushedBytes).
func TestEventsWAL_ResetTruncatesAndRecordsFlush(t *testing.T) {
	cat, wal, _, walPath := newEventsWALTestRig(t)
	defer wal.Close()
	defer cat.Close()

	id, _ := cat.GetOrAssignEventID("ev.r", Int32Sample)
	if _, err := AppendEventWithName(wal, id, "ev.r", 1, Int32Sample, encInt32(1), nil); err != nil {
		t.Fatal(err)
	}
	pre := wal.Stats()
	if pre.BufferBytes == 0 {
		t.Fatal("expected non-zero buffer bytes before reset")
	}
	if err := wal.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	post := wal.Stats()
	if post.BufferBytes != 0 {
		t.Errorf("BufferBytes after reset = %d, want 0", post.BufferBytes)
	}
	if post.FlushCount != pre.FlushCount+1 {
		t.Errorf("FlushCount = %d, want %d", post.FlushCount, pre.FlushCount+1)
	}
	if post.FlushedBytes < pre.BufferBytes {
		t.Errorf("FlushedBytes = %d, want >= %d", post.FlushedBytes, pre.BufferBytes)
	}
	if st, err := os.Stat(walPath); err != nil || st.Size() != 0 {
		t.Errorf("WAL file size after reset = %d, want 0 (err=%v)", st.Size(), err)
	}
}

// TestEventsWAL_AppendValidation rejects malformed args at the API
// boundary rather than corrupting the WAL.
func TestEventsWAL_AppendValidation(t *testing.T) {
	_, wal, _, _ := newEventsWALTestRig(t)
	defer wal.Close()

	cases := []struct {
		name string
		fn   func() error
		want string
	}{
		{
			"id_zero",
			func() error {
				_, err := AppendEvent(wal, 0, 1, Int32Sample, encInt32(1), nil)
				return err
			},
			"out of range",
		},
		{
			"id_over_cap",
			func() error {
				_, err := AppendEvent(wal, EventID(MaxEventsPerDatabase+1), 1, Int32Sample, encInt32(1), nil)
				return err
			},
			"out of range",
		},
		{
			"bad_vtype",
			func() error {
				_, err := AppendEvent(wal, 1, 1, 99, encInt32(1), nil)
				return err
			},
			"invalid value_type",
		},
		{
			"newevent_empty_name",
			func() error {
				_, err := AppendEventWithName(wal, 1, "", 1, Int32Sample, encInt32(1), nil)
				return err
			},
			"empty name",
		},
		{
			"newevent_name_too_long",
			func() error {
				_, err := AppendEventWithName(wal, 1, string(make([]byte, MaxEventNameLen+1)), 1, Int32Sample, encInt32(1), nil)
				return err
			},
			"exceeds",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.fn()
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

// TestEventsWAL_ReservedFlagBitRejected ensures the v1 reserved-bits
// guard fires when a record carries an unknown flag. Forward-compat
// check.
func TestEventsWAL_ReservedFlagBitRejected(t *testing.T) {
	cat, wal, _, walPath := newEventsWALTestRig(t)
	id, _ := cat.GetOrAssignEventID("ev.f", Int32Sample)
	if _, err := AppendEventWithName(wal, id, "ev.f", 1, Int32Sample, encInt32(1), nil); err != nil {
		t.Fatal(err)
	}
	if err := wal.Fsync(); err != nil {
		t.Fatal(err)
	}
	wal.Close()
	cat.Close()

	// Mutate the flag byte to set a reserved bit. The record layout is:
	//   [uvarint payload_len:1B][EventID:2][TS:8][Flags:1]...
	// Open ahead of the flag byte and OR in bit 0.
	f, err := os.OpenFile(walPath, os.O_RDWR, 0644)
	if err != nil {
		t.Fatal(err)
	}
	// uvarint encoded length is 1 byte for any payload < 128 bytes,
	// which the newEvent record here easily is.
	flagOffset := int64(1 + 2 + 8)
	var single [1]byte
	if _, err := f.ReadAt(single[:], flagOffset); err != nil {
		t.Fatal(err)
	}
	single[0] |= 0x01 // reserved bit
	if _, err := f.WriteAt(single[:], flagOffset); err != nil {
		t.Fatal(err)
	}
	f.Close()

	wal2, err := OpenAndRecoverEventsWAL(walPath, WALFsyncPolicySegment)
	if err != nil {
		t.Fatal(err)
	}
	defer wal2.Close()
	cat2, err := LoadEventCatalog(filepath.Join(t.TempDir(), "events.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer cat2.Close()

	// Reserved-bit set inside an otherwise-intact record is corruption
	// (the outer length prefix matched), so RecordsWithCatalog must
	// surface it as a hard error rather than swallow the record.
	_, err = wal2.RecordsWithCatalog(cat2)
	if !errors.Is(err, ErrEventsWALReservedFlagSet) {
		t.Fatalf("expected ErrEventsWALReservedFlagSet, got %v", err)
	}
}

// TestEventsWAL_PayloadLargeRoundtrip exercises a payload that needs
// multi-byte uvarint encoding.
func TestEventsWAL_PayloadLargeRoundtrip(t *testing.T) {
	cat, wal, _, _ := newEventsWALTestRig(t)
	defer wal.Close()
	defer cat.Close()

	id, _ := cat.GetOrAssignEventID("ev.big", EventValueNone)
	big := bytes.Repeat([]byte("abc123"), 200) // 1200 bytes
	if _, err := AppendEventWithName(wal, id, "ev.big", 1, EventValueNone, [4]byte{}, big); err != nil {
		t.Fatal(err)
	}
	recs, err := wal.RecordsWithCatalog(cat)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("want 1, got %d", len(recs))
	}
	if !bytes.Equal(recs[0].Payload, big) {
		t.Errorf("payload mismatch: got %d bytes, want %d", len(recs[0].Payload), len(big))
	}
}

// TestEventsWAL_OpenAndRecoverStats verifies that reopening an existing
// WAL file populates AppendCount and AppendBytes from a one-shot scan.
func TestEventsWAL_OpenAndRecoverStats(t *testing.T) {
	cat, wal, _, walPath := newEventsWALTestRig(t)
	id, _ := cat.GetOrAssignEventID("ev.o", Int32Sample)
	for i := 0; i < 4; i++ {
		if _, err := AppendEventWithName(wal, id, "ev.o", Timestamp(i+1), Int32Sample, encInt32(int32(i)), nil); err != nil {
			t.Fatal(err)
		}
	}
	_ = wal.Fsync()
	wal.Close()
	cat.Close()

	w2, err := OpenAndRecoverEventsWAL(walPath, WALFsyncPolicySegment)
	if err != nil {
		t.Fatal(err)
	}
	defer w2.Close()
	stats := w2.Stats()
	if stats.AppendCount != 4 {
		t.Errorf("AppendCount = %d, want 4", stats.AppendCount)
	}
	if stats.AppendBytes == 0 {
		t.Error("AppendBytes should be > 0")
	}
	if stats.BufferBytes != stats.AppendBytes {
		t.Errorf("BufferBytes (%d) != AppendBytes (%d)", stats.BufferBytes, stats.AppendBytes)
	}
}

// TestEventsWAL_InvalidFsyncPolicyRejected covers the constructor's
// fsync-policy validation symmetrically with NewWAL.
func TestEventsWAL_InvalidFsyncPolicyRejected(t *testing.T) {
	_, err := NewEventsWAL(filepath.Join(t.TempDir(), "x.events.wal"), 1<<20, "nope")
	if err == nil {
		t.Fatal("expected invalid policy to be rejected")
	}
}

// contains is a tiny strings.Contains shim used by the validation table
// test (avoiding a `strings` import bloat in one test file).
func contains(s, sub string) bool {
	if sub == "" {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
