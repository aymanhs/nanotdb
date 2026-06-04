package engine

import (
	"bytes"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// newPageTestCatalog returns a fresh catalog pre-populated with the
// three canonical fixtures used across these tests.
func newPageTestCatalog(t *testing.T) (*EventCatalog, map[string]EventID) {
	t.Helper()
	cat, err := LoadEventCatalog(filepath.Join(t.TempDir(), "events.json"))
	if err != nil {
		t.Fatalf("LoadEventCatalog: %v", err)
	}
	ids := make(map[string]EventID)
	for _, spec := range []struct {
		name  string
		vtype byte
	}{
		{"disc.write.slow", Int32Sample},
		{"temp.over", Float32Sample},
		{"heartbeat", EventValueNone},
	} {
		id, err := cat.GetOrAssignEventID(spec.name, spec.vtype)
		if err != nil {
			t.Fatalf("assign %q: %v", spec.name, err)
		}
		ids[spec.name] = id
	}
	return cat, ids
}

func float32Bits(f float32) uint32 { return math.Float32bits(f) }

// TestEventsPage_RoundTrip exercises the full encode → decode cycle
// using mixed value types (int32, float32, none) and varying payload
// sizes, including an empty payload and a multi-byte uvarint payload
// length.
func TestEventsPage_RoundTrip(t *testing.T) {
	cat, ids := newPageTestCatalog(t)
	defer cat.Close()

	page := NewEventsPage(0)

	type rec struct {
		name    string
		ts      Timestamp
		vraw    uint32
		payload []byte
	}
	recs := []rec{
		{"disc.write.slow", 100, uint32(int32(542)), []byte(`{"path":"/tmp"}`)},
		{"temp.over", 110, float32Bits(31.2), nil},
		{"heartbeat", 120, 0, nil},
		{"disc.write.slow", 130, uint32(int32(870)), bytes.Repeat([]byte("a"), 300)},
		{"heartbeat", 130, 0, []byte("note")},
	}

	for _, r := range recs {
		_, e, _ := cat.GetEventByID(ids[r.name])
		if err := page.AddEvent(ids[r.name], r.ts, e.ValueType, r.vraw, r.payload); err != nil {
			t.Fatalf("AddEvent %q: %v", r.name, err)
		}
	}

	var buf bytes.Buffer
	if err := page.EncodeInto(&buf); err != nil {
		t.Fatalf("EncodeInto: %v", err)
	}

	hdr, err := decodeEventsFrameHeader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("decodeEventsFrameHeader: %v", err)
	}
	if int(hdr.RecordCount) != len(recs) {
		t.Fatalf("RecordCount = %d, want %d", hdr.RecordCount, len(recs))
	}
	for _, name := range []string{"disc.write.slow", "temp.over", "heartbeat"} {
		if !hdr.HasEventID(ids[name]) {
			t.Errorf("bitmap missing %q (id=%d)", name, ids[name])
		}
	}

	body := buf.Bytes()[EventsFrameHeaderBytes : EventsFrameHeaderBytes+int(hdr.CompressedLen)]
	crc := binary.LittleEndian.Uint32(buf.Bytes()[EventsFrameHeaderBytes+int(hdr.CompressedLen):])

	out := NewEventsPage(0)
	if err := out.DecodeFromFrame(hdr, body, crc, cat); err != nil {
		t.Fatalf("DecodeFromFrame: %v", err)
	}
	if out.Count() != len(recs) {
		t.Fatalf("decoded count = %d, want %d", out.Count(), len(recs))
	}
	for i, r := range recs {
		if out.EventIDs[i] != ids[r.name] {
			t.Errorf("rec[%d].EventID = %d, want %d", i, out.EventIDs[i], ids[r.name])
		}
		if out.Times[i] != r.ts {
			t.Errorf("rec[%d].TS = %d, want %d", i, out.Times[i], r.ts)
		}
		if !bytes.Equal(out.Payloads[i], r.payload) {
			t.Errorf("rec[%d] payload mismatch", i)
		}
	}
}

// TestEventsPage_NoneTypedHasNoValueBytes is the size-economy check
// that the documented hot-path math depends on. A page of N none-typed
// events with empty payloads should be ~N × (2+8+1) bytes uncompressed.
// We don't measure exact uncompressed bytes here (S2 compresses it),
// but we can verify by comparing to a same-count int32 page.
func TestEventsPage_NoneTypedHasNoValueBytes(t *testing.T) {
	cat, ids := newPageTestCatalog(t)
	defer cat.Close()

	makePage := func(name string, count int) []byte {
		_, e, _ := cat.GetEventByID(ids[name])
		p := NewEventsPage(0)
		for i := range count {
			if err := p.AddEvent(ids[name], Timestamp(i), e.ValueType, uint32(i), nil); err != nil {
				t.Fatalf("add: %v", err)
			}
		}
		var buf bytes.Buffer
		if err := p.EncodeInto(&buf); err != nil {
			t.Fatalf("encode: %v", err)
		}
		return buf.Bytes()
	}

	intBytes := makePage("disc.write.slow", 100)
	noneBytes := makePage("heartbeat", 100)
	if !(len(noneBytes) < len(intBytes)) {
		t.Errorf("expected none-typed page (%d bytes) smaller than int32 page (%d bytes)", len(noneBytes), len(intBytes))
	}
}

// TestEventsPage_AcceptsArrivalOrderRecords verifies the intentional
// page semantics: unlike the metric Page, the events page accepts
// records in arrival order. Different event names may interleave
// with reordered timestamps. The per-event-name monotonic-ts rule is
// enforced one level up by EventCatalog.LastTS. p.Start / p.End
// track min/max ts (not first/last record's ts) so the on-disk frame
// bitmap + time range stays a valid skip box.
func TestEventsPage_AcceptsArrivalOrderRecords(t *testing.T) {
	cat, ids := newPageTestCatalog(t)
	defer cat.Close()

	page := NewEventsPage(0)
	if err := page.AddEvent(ids["heartbeat"], 200, EventValueNone, 0, nil); err != nil {
		t.Fatal(err)
	}
	if err := page.AddEvent(ids["disc.write.slow"], 100, Int32Sample, 0, nil); err != nil {
		t.Fatalf("interleaved earlier-ts on a different event should be accepted: %v", err)
	}
	if err := page.AddEvent(ids["temp.over"], 150, Float32Sample, float32Bits(31.5), nil); err != nil {
		t.Fatal(err)
	}
	if page.Start != 100 {
		t.Errorf("Start = %d, want 100 (min ts observed)", page.Start)
	}
	if page.End != 200 {
		t.Errorf("End = %d, want 200 (max ts observed)", page.End)
	}
	if page.Count() != 3 {
		t.Fatalf("Count = %d, want 3", page.Count())
	}
}

// TestEventsPage_IsFullThresholds covers the three soft thresholds.
func TestEventsPage_IsFullThresholds(t *testing.T) {
	cat, ids := newPageTestCatalog(t)
	defer cat.Close()

	t.Run("max_records", func(t *testing.T) {
		page := NewEventsPageWithLimits(0, 3, 1<<20, time.Hour, 0)
		for i := range 3 {
			if err := page.AddEvent(ids["heartbeat"], Timestamp(i), EventValueNone, 0, nil); err != nil {
				t.Fatal(err)
			}
		}
		if !page.IsFull() {
			t.Fatal("expected IsFull after max_records reached")
		}
	})

	t.Run("max_bytes", func(t *testing.T) {
		page := NewEventsPageWithLimits(0, 1000, 256, time.Hour, 0)
		big := bytes.Repeat([]byte("x"), 200)
		// Adding two 200-byte payloads should cross the byte threshold.
		if err := page.AddEvent(ids["heartbeat"], 1, EventValueNone, 0, big); err != nil {
			t.Fatal(err)
		}
		if err := page.AddEvent(ids["heartbeat"], 2, EventValueNone, 0, big); err != nil {
			t.Fatal(err)
		}
		if !page.IsFull() {
			t.Fatalf("expected IsFull after max_bytes; SizeBytes=%d", page.SizeBytes())
		}
	})

	t.Run("max_age", func(t *testing.T) {
		page := NewEventsPageWithLimits(0, 1000, 1<<20, 10*time.Millisecond, 0)
		if err := page.AddEvent(ids["heartbeat"], 1, EventValueNone, 0, nil); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
		if !page.IsFull() {
			t.Fatal("expected IsFull after max_age elapsed")
		}
	})
}

// TestEventsPage_MustForceFlush verifies the spike-protection ceiling.
// The ceiling MUST report true independently of the soft thresholds.
func TestEventsPage_MustForceFlush(t *testing.T) {
	cat, ids := newPageTestCatalog(t)
	defer cat.Close()

	page := NewEventsPageWithLimits(0, 10_000, 1<<20, time.Hour, 512 /* tight ceiling */)
	big := bytes.Repeat([]byte("y"), 300)
	if err := page.AddEvent(ids["heartbeat"], 1, EventValueNone, 0, big); err != nil {
		t.Fatal(err)
	}
	if err := page.AddEvent(ids["heartbeat"], 2, EventValueNone, 0, big); err != nil {
		t.Fatal(err)
	}
	if !page.MustForceFlush() {
		t.Fatalf("expected MustForceFlush, SizeBytes=%d ceiling=%d", page.SizeBytes(), page.MaxInMemoryBytes)
	}
	// Soft IsFull should not be true at these limits.
	if page.IsFull() {
		t.Fatal("IsFull true but soft thresholds were intentionally generous")
	}

	// Disabled ceiling never trips.
	loose := NewEventsPageWithLimits(0, 10_000, 1<<20, time.Hour, 0)
	if err := loose.AddEvent(ids["heartbeat"], 1, EventValueNone, 0, bytes.Repeat([]byte("z"), 10_000)); err != nil {
		t.Fatal(err)
	}
	if loose.MustForceFlush() {
		t.Fatal("disabled ceiling should never trip")
	}
}

// TestEventsPage_BitmapHelpers covers the EventID bitmap math in both
// EventsPageHeader and EventsFrameHeader (they share the encoding so
// we exercise both code paths via the same bit positions).
func TestEventsPage_BitmapHelpers(t *testing.T) {
	var h EventsPageHeader
	h.SetEventID(1)
	h.SetEventID(8)
	h.SetEventID(1023)
	for _, id := range []EventID{1, 8, 1023} {
		if !h.HasEventID(id) {
			t.Errorf("HasEventID(%d) = false, want true", id)
		}
	}
	if h.HasEventID(2) || h.HasEventID(7) || h.HasEventID(1022) {
		t.Error("phantom bit set")
	}
	if !h.IntersectsAny([]EventID{1022, 1023}) {
		t.Error("IntersectsAny missed 1023")
	}
	if h.IntersectsAny([]EventID{2, 3, 4}) {
		t.Error("IntersectsAny false positive")
	}

	// Invalid ids are silently ignored on Set/Has, returning false.
	h.SetEventID(0)
	h.SetEventID(EventID(MaxEventsPerDatabase + 1))
	if h.HasEventID(0) || h.HasEventID(EventID(MaxEventsPerDatabase+1)) {
		t.Error("invalid id reported as present")
	}

	// FrameHeader bitmap shares the same byte layout.
	var fh EventsFrameHeader
	fh.EventIDBitmap = h.EventIDBitmap
	if !fh.HasEventID(1023) || !fh.IntersectsAny([]EventID{1023}) {
		t.Error("FrameHeader bitmap walks do not match PageHeader")
	}
}

// TestEventsPage_DecodeRejectsCRCMismatch exercises the integrity
// check: a single-bit flip in the compressed payload must surface as
// an error, not silently decode bogus records.
func TestEventsPage_DecodeRejectsCRCMismatch(t *testing.T) {
	cat, ids := newPageTestCatalog(t)
	defer cat.Close()

	page := NewEventsPage(0)
	for i := range 5 {
		_ = page.AddEvent(ids["disc.write.slow"], Timestamp(i), Int32Sample, uint32(i), nil)
	}
	var buf bytes.Buffer
	if err := page.EncodeInto(&buf); err != nil {
		t.Fatal(err)
	}
	hdr, _ := decodeEventsFrameHeader(bytes.NewReader(buf.Bytes()))
	body := buf.Bytes()[EventsFrameHeaderBytes : EventsFrameHeaderBytes+int(hdr.CompressedLen)]
	crc := binary.LittleEndian.Uint32(buf.Bytes()[EventsFrameHeaderBytes+int(hdr.CompressedLen):])

	// Flip a byte mid-payload.
	tampered := append([]byte(nil), body...)
	tampered[len(tampered)/2] ^= 0xff

	out := NewEventsPage(0)
	if err := out.DecodeFromFrame(hdr, tampered, crc, cat); err == nil {
		t.Fatal("expected CRC mismatch error from tampered payload")
	}
}

// TestEventsPage_DecodeRejectsUnknownEventID exercises the catalog-
// required property at the page-decode level.
func TestEventsPage_DecodeRejectsUnknownEventID(t *testing.T) {
	cat, ids := newPageTestCatalog(t)
	defer cat.Close()

	page := NewEventsPage(0)
	_ = page.AddEvent(ids["heartbeat"], 1, EventValueNone, 0, nil)
	var buf bytes.Buffer
	if err := page.EncodeInto(&buf); err != nil {
		t.Fatal(err)
	}
	hdr, _ := decodeEventsFrameHeader(bytes.NewReader(buf.Bytes()))
	body := buf.Bytes()[EventsFrameHeaderBytes : EventsFrameHeaderBytes+int(hdr.CompressedLen)]
	crc := binary.LittleEndian.Uint32(buf.Bytes()[EventsFrameHeaderBytes+int(hdr.CompressedLen):])

	freshCat, err := LoadEventCatalog(filepath.Join(t.TempDir(), "events.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer freshCat.Close()

	out := NewEventsPage(0)
	if err := out.DecodeFromFrame(hdr, body, crc, freshCat); err == nil {
		t.Fatal("expected unknown-event-id error with empty catalog")
	}

	// Nil catalog must also fail (page decode requires it).
	if err := out.DecodeFromFrame(hdr, body, crc, nil); err == nil {
		t.Fatal("expected error from nil catalog")
	}
}

// TestEventsPage_EncodeEmptyRejected ensures we don't write a
// zero-record frame that would confuse readers.
func TestEventsPage_EncodeEmptyRejected(t *testing.T) {
	page := NewEventsPage(0)
	var buf bytes.Buffer
	if err := page.EncodeInto(&buf); err == nil {
		t.Fatal("expected encoding an empty page to fail")
	}
}

// TestEventsFile_AppendAndWalk writes three pages to one events-*.dat
// file and verifies WalkEventsFileHeaders sees all three, with
// summary numbers matching the inputs.
func TestEventsFile_AppendAndWalk(t *testing.T) {
	cat, ids := newPageTestCatalog(t)
	defer cat.Close()
	dir := t.TempDir()

	mkPage := func(start Timestamp, count int, idKey string) *EventsPage {
		_, e, _ := cat.GetEventByID(ids[idKey])
		p := NewEventsPage(0)
		for i := range count {
			if err := p.AddEvent(ids[idKey], start+Timestamp(i), e.ValueType, uint32(i), nil); err != nil {
				t.Fatalf("add: %v", err)
			}
		}
		return p
	}

	pages := []struct {
		page    *EventsPage
		recs    int
		idName  string
		startTS Timestamp
	}{
		{mkPage(100, 4, "disc.write.slow"), 4, "disc.write.slow", 100},
		{mkPage(200, 7, "temp.over"), 7, "temp.over", 200},
		{mkPage(300, 2, "heartbeat"), 2, "heartbeat", 300},
	}
	for _, p := range pages {
		st, err := AppendEventsPageFrame(dir, "2026-06-01", p.page, false)
		if err != nil {
			t.Fatalf("AppendEventsPageFrame: %v", err)
		}
		if st.FrameBytes == 0 || st.CompressedBytes == 0 {
			t.Fatalf("flush stats look wrong: %+v", st)
		}
	}

	path := EventsFilePath(dir, "2026-06-01")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected %s to exist: %v", path, err)
	}

	var seen []EventsFrameHeader
	stats, err := WalkEventsFileHeaders(path, func(h EventsFrameHeader) error {
		seen = append(seen, h)
		return nil
	})
	if err != nil {
		t.Fatalf("WalkEventsFileHeaders: %v", err)
	}
	if stats.Frames != 3 {
		t.Errorf("Frames = %d, want 3", stats.Frames)
	}
	if stats.TotalRecords != 13 {
		t.Errorf("TotalRecords = %d, want 13", stats.TotalRecords)
	}
	if stats.MinStart != 100 || stats.MaxEnd != Timestamp(300+1) {
		t.Errorf("Min/Max = %d/%d, want 100/301", stats.MinStart, stats.MaxEnd)
	}
	for i, h := range seen {
		want := pages[i]
		if int(h.RecordCount) != want.recs {
			t.Errorf("frame[%d].RecordCount = %d, want %d", i, h.RecordCount, want.recs)
		}
		if h.StartTime != want.startTS {
			t.Errorf("frame[%d].StartTime = %d, want %d", i, h.StartTime, want.startTS)
		}
		if !h.HasEventID(ids[want.idName]) {
			t.Errorf("frame[%d] bitmap missing %s", i, want.idName)
		}
	}
}

// TestEventsFile_BitmapEnablesSkip verifies the design's headline
// query-time skip optimization: a frame whose bitmap doesn't intersect
// the query's id set can be excluded purely from the header.
func TestEventsFile_BitmapEnablesSkip(t *testing.T) {
	cat, ids := newPageTestCatalog(t)
	defer cat.Close()
	dir := t.TempDir()

	// Two frames: one with only "heartbeat", one with only "disc.write.slow".
	makeFrame := func(name string, count int, start Timestamp) {
		_, e, _ := cat.GetEventByID(ids[name])
		p := NewEventsPage(0)
		for i := range count {
			_ = p.AddEvent(ids[name], start+Timestamp(i), e.ValueType, uint32(i), nil)
		}
		if _, err := AppendEventsPageFrame(dir, "p", p, false); err != nil {
			t.Fatal(err)
		}
	}
	makeFrame("heartbeat", 3, 100)
	makeFrame("disc.write.slow", 4, 200)

	query := []EventID{ids["disc.write.slow"]}
	var matched int
	_, err := WalkEventsFileHeaders(EventsFilePath(dir, "p"), func(h EventsFrameHeader) error {
		if h.IntersectsAny(query) {
			matched++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if matched != 1 {
		t.Fatalf("expected exactly one frame to match disc.*, got %d", matched)
	}
}

// TestEventsFile_CollectFrameRoundtrip verifies the offset-based
// single-frame decode path used by query/inspect callers.
func TestEventsFile_CollectFrameRoundtrip(t *testing.T) {
	cat, ids := newPageTestCatalog(t)
	defer cat.Close()
	dir := t.TempDir()

	in := NewEventsPage(0)
	for i, name := range []string{"disc.write.slow", "temp.over", "heartbeat", "disc.write.slow"} {
		_, e, _ := cat.GetEventByID(ids[name])
		var raw uint32
		switch e.ValueType {
		case Int32Sample:
			raw = uint32(int32(i + 10))
		case Float32Sample:
			raw = float32Bits(float32(i) + 0.5)
		}
		if err := in.AddEvent(ids[name], Timestamp(i+1), e.ValueType, raw, []byte("p")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := AppendEventsPageFrame(dir, "p", in, false); err != nil {
		t.Fatal(err)
	}

	path := EventsFilePath(dir, "p")
	var firstOffset int64 = -1
	_, err := WalkEventsFileHeaders(path, func(h EventsFrameHeader) error {
		if firstOffset < 0 {
			firstOffset = h.Offset
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if firstOffset != 0 {
		t.Fatalf("first frame offset = %d, want 0", firstOffset)
	}

	out, err := CollectEventsFrame(path, firstOffset, cat)
	if err != nil {
		t.Fatalf("CollectEventsFrame: %v", err)
	}
	if out.Count() != in.Count() {
		t.Fatalf("decoded count %d, want %d", out.Count(), in.Count())
	}
	for i := range in.Count() {
		if in.EventIDs[i] != out.EventIDs[i] || in.Times[i] != out.Times[i] {
			t.Errorf("rec[%d] header mismatch", i)
		}
		if in.ValueTypes[i] != EventValueNone && in.ValuesRaw[i] != out.ValuesRaw[i] {
			t.Errorf("rec[%d] value mismatch", i)
		}
		if !bytes.Equal(in.Payloads[i], out.Payloads[i]) {
			t.Errorf("rec[%d] payload mismatch", i)
		}
	}

	// VerifyEventsFrame returns the same record count.
	if got, err := VerifyEventsFrame(path, firstOffset, cat); err != nil || int(got) != in.Count() {
		t.Fatalf("VerifyEventsFrame: got %d err %v, want %d", got, err, in.Count())
	}
}

// TestEventsFile_TruncatedFrameHeader simulates a power-loss mid frame
// header. WalkEventsFileHeaders must surface the truncation as an error.
func TestEventsFile_TruncatedFrameHeader(t *testing.T) {
	cat, ids := newPageTestCatalog(t)
	defer cat.Close()
	dir := t.TempDir()

	p := NewEventsPage(0)
	for i := range 3 {
		_ = p.AddEvent(ids["heartbeat"], Timestamp(i), EventValueNone, 0, nil)
	}
	if _, err := AppendEventsPageFrame(dir, "p", p, false); err != nil {
		t.Fatal(err)
	}
	path := EventsFilePath(dir, "p")
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Truncate to inside the header.
	if err := os.Truncate(path, st.Size()-int64(EventsFrameHeaderBytes/2)); err != nil {
		t.Fatal(err)
	}
	// Append a partial new frame header by truncating to exactly inside
	// the second frame's header. Replace existing file with content + 10
	// bytes of garbage to simulate a partial second header.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(make([]byte, 10)); err != nil {
		t.Fatal(err)
	}
	f.Close()

	if _, err := WalkEventsFileHeaders(path, nil); err == nil {
		t.Fatal("expected truncated header to surface as error")
	}
}

// TestEventsFile_AppendValidatesArgs covers the input-validation
// rails on the append helper.
func TestEventsFile_AppendValidatesArgs(t *testing.T) {
	dir := t.TempDir()
	page := NewEventsPage(0) // empty
	if _, err := AppendEventsPageFrame(dir, "p", page, false); err == nil {
		t.Error("expected error appending empty page")
	}
	if _, err := AppendEventsPageFrame("", "p", page, false); err == nil {
		t.Error("expected error with empty rootDir")
	}
	if _, err := AppendEventsPageFrame(dir, "", page, false); err == nil {
		t.Error("expected error with empty partition")
	}
}
