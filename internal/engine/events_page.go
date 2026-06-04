package engine

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"sync"
	"time"

	"github.com/klauspost/compress/s2"
)

const (
	// EventsPageMaxRecords is the default soft cap on records per in-memory
	// page. The events workload is sparse, so the limit is much lower than
	// PageMaxRecords for metrics.
	EventsPageMaxRecords = 1000

	// EventsPageMaxBytes is the default soft cap on uncompressed payload
	// bytes per in-memory page.
	EventsPageMaxBytes = 64 * 1024

	// EventsPageMaxAge is the default wall-clock age limit before a page
	// is force-flushed. Longer than the metric page's 10-second default
	// because events are sparse — a 1-hour idle window with no events is
	// perfectly normal and doesn't merit an early flush.
	EventsPageMaxAge = time.Hour

	// EventsPageEventIDBitmapBytes is the fixed size of the per-page
	// event-id presence bitmap on disk: ceil(MaxEventsPerDatabase/8) =
	// ceil(1023/8) = 128 bytes. EventID 0 is the invalid sentinel; bit 0
	// of byte 0 is always zero.
	EventsPageEventIDBitmapBytes = 128

	// EventsFrameHeaderBytes is the fixed on-disk size of EventsPageHeader:
	// start_ts(8) + end_ts(8) + record_count(4) + event_id_bitmap(128) +
	// compressed_len(4) = 152 bytes.
	EventsFrameHeaderBytes = 8 + 8 + 4 + EventsPageEventIDBitmapBytes + 4
)

var eventsPageEncodeBufferPool = sync.Pool{
	New: func() any {
		return bytes.NewBuffer(make([]byte, 0, EventsPageMaxBytes))
	},
}

// EventsPageHeader is the per-frame header that lives at the start of
// each frame in events-<partition>.dat. The 128-byte bitmap lets
// name-filtered queries skip whole frames without decompressing — when
// a query resolves "name = disc.*" to {EventID 1, 7, 12}, the scanner
// ANDs that set against the per-frame bitmap before considering the
// frame. See docs/EVENTS.md for the rationale.
type EventsPageHeader struct {
	StartTime     Timestamp
	EndTime       Timestamp
	RecordCount   uint32
	EventIDBitmap [EventsPageEventIDBitmapBytes]byte
	CompressedLen uint32
}

// SetEventID flips the bit corresponding to id in the bitmap.
func (h *EventsPageHeader) SetEventID(id EventID) {
	if id == 0 || uint16(id) > MaxEventsPerDatabase {
		return
	}
	h.EventIDBitmap[id/8] |= 1 << (id % 8)
}

// HasEventID reports whether id's bit is set in the bitmap.
func (h *EventsPageHeader) HasEventID(id EventID) bool {
	if id == 0 || uint16(id) > MaxEventsPerDatabase {
		return false
	}
	return h.EventIDBitmap[id/8]&(1<<(id%8)) != 0
}

// IntersectsAny reports whether any id in the supplied set is set in
// the bitmap. Used by queries to skip non-matching frames.
func (h *EventsPageHeader) IntersectsAny(ids []EventID) bool {
	if len(ids) == 0 {
		return false
	}
	for _, id := range ids {
		if h.HasEventID(id) {
			return true
		}
	}
	return false
}

// encode writes the header bytes in fixed order. Total = 152 bytes.
func (h *EventsPageHeader) encode(w *bytes.Buffer) error {
	var hdr [EventsFrameHeaderBytes]byte
	binary.LittleEndian.PutUint64(hdr[0:8], uint64(h.StartTime))
	binary.LittleEndian.PutUint64(hdr[8:16], uint64(h.EndTime))
	binary.LittleEndian.PutUint32(hdr[16:20], h.RecordCount)
	copy(hdr[20:20+EventsPageEventIDBitmapBytes], h.EventIDBitmap[:])
	binary.LittleEndian.PutUint32(hdr[20+EventsPageEventIDBitmapBytes:], h.CompressedLen)
	_, err := w.Write(hdr[:])
	return err
}

// decodeEventsFrameHeader reads exactly EventsFrameHeaderBytes from r.
func decodeEventsFrameHeader(r io.Reader) (EventsPageHeader, error) {
	var raw [EventsFrameHeaderBytes]byte
	if _, err := io.ReadFull(r, raw[:]); err != nil {
		return EventsPageHeader{}, err
	}
	h := EventsPageHeader{
		StartTime:     Timestamp(binary.LittleEndian.Uint64(raw[0:8])),
		EndTime:       Timestamp(binary.LittleEndian.Uint64(raw[8:16])),
		RecordCount:   binary.LittleEndian.Uint32(raw[16:20]),
		CompressedLen: binary.LittleEndian.Uint32(raw[20+EventsPageEventIDBitmapBytes:]),
	}
	copy(h.EventIDBitmap[:], raw[20:20+EventsPageEventIDBitmapBytes])
	return h, nil
}

// EventsPage is the in-memory accumulator for a single (database,
// partition) events stream. Records are kept in parallel slices to
// match the metric Page pattern. The catalog is consulted at encode
// time to determine value-width per record (0 or 4 bytes); the page
// itself caches the per-record ValueType to avoid catalog lookups
// during encode.
//
// EventsPage is not safe for concurrent use; the engine layer wraps
// access in the same write-path lock that already protects the
// per-database catalog and metric page.
type EventsPage struct {
	Start, End  Timestamp
	EventIDs    []EventID
	Times       []Timestamp
	ValueTypes  []byte // EventValueNone / Int32Sample / Float32Sample per record
	ValuesRaw   []uint32 // raw 32-bit value bits (0 for none-typed records)
	Payloads    [][]byte // opaque payload bytes; nil/empty allowed
	EventIDSet  [EventsPageEventIDBitmapBytes]byte

	payloadBytes int // running sum of len(Payloads[i])

	MaxRecords       int
	MaxBytes         int
	MaxAge           time.Duration
	MaxInMemoryBytes int // separate spike-protection ceiling (0 = disabled)

	WALSegmentID uint16
	createdAt    time.Time
}

// NewEventsPage builds an empty page anchored at firstTS, using the
// default flush thresholds. firstTS may be zero for a not-yet-anchored
// page; the first AddEvent sets the real Start.
func NewEventsPage(firstTS Timestamp) *EventsPage {
	return NewEventsPageWithLimits(firstTS, EventsPageMaxRecords, EventsPageMaxBytes, EventsPageMaxAge, 0)
}

// NewEventsPageWithLimits builds a page with custom thresholds. Zero
// values for maxRecords / maxBytes / maxAge fall back to the package
// defaults. maxInMemoryBytes is the spike-protection force-flush
// ceiling enforced by the engine layer; zero disables the check.
func NewEventsPageWithLimits(firstTS Timestamp, maxRecords, maxBytes int, maxAge time.Duration, maxInMemoryBytes int) *EventsPage {
	if maxRecords <= 0 {
		maxRecords = EventsPageMaxRecords
	}
	if maxBytes <= 0 {
		maxBytes = EventsPageMaxBytes
	}
	if maxAge <= 0 {
		maxAge = EventsPageMaxAge
	}
	return &EventsPage{
		Start:            firstTS,
		End:              firstTS,
		EventIDs:         make([]EventID, 0, 64),
		Times:            make([]Timestamp, 0, 64),
		ValueTypes:       make([]byte, 0, 64),
		ValuesRaw:        make([]uint32, 0, 64),
		Payloads:         make([][]byte, 0, 64),
		MaxRecords:       maxRecords,
		MaxBytes:         maxBytes,
		MaxAge:           maxAge,
		MaxInMemoryBytes: maxInMemoryBytes,
		createdAt:        time.Now(),
	}
}

// AddEvent appends one event record in arrival order. Unlike the
// metric Page (which enforces page-wide monotonic ts), the events page
// is intentionally lax: a busy DB may receive interleaved events from
// different threads/sources with reordered timestamps within the page.
//
// The per-event-name monotonic-ts rule (crash-safety contract rule 5)
// is enforced at the engine layer via EventCatalog.LastTS — by the time
// AddEvent reaches this method, ts has already been gated for THIS
// event name. Different event names are free to interleave.
//
// p.Start and p.End track the min/max ts observed, not the first/last
// record's ts. That's enough to bound the frame for time-range
// skip-without-decompress at query time.
func (p *EventsPage) AddEvent(id EventID, ts Timestamp, valueType byte, valueRaw uint32, payload []byte) error {
	if id == 0 || uint16(id) > MaxEventsPerDatabase {
		return fmt.Errorf("events page: event id %d out of range", id)
	}
	if !IsValidEventValueType(valueType) {
		return fmt.Errorf("events page: invalid value_type %d", valueType)
	}
	if len(p.Times) == 0 {
		p.Start = ts
		p.End = ts
	} else {
		if ts < p.Start {
			p.Start = ts
		}
		if ts > p.End {
			p.End = ts
		}
	}
	p.EventIDs = append(p.EventIDs, id)
	p.Times = append(p.Times, ts)
	p.ValueTypes = append(p.ValueTypes, valueType)
	p.ValuesRaw = append(p.ValuesRaw, valueRaw)
	if len(payload) > 0 {
		// Copy: callers may reuse their payload buffer after AddEvent
		// returns. Mirrors how WAL records are copied out by RecordsWith*.
		cp := make([]byte, len(payload))
		copy(cp, payload)
		p.Payloads = append(p.Payloads, cp)
		p.payloadBytes += len(cp)
	} else {
		p.Payloads = append(p.Payloads, nil)
	}
	// Mark presence in the per-page bitmap. id is already validated.
	p.EventIDSet[id/8] |= 1 << (id % 8)
	return nil
}

// SetWALSegmentID records the WAL segment that this page's data came
// from. Used to gate WAL reset until all pages with dependencies on the
// current WAL segment have been flushed. Same semantics as
// Page.SetWalSegmentID.
func (p *EventsPage) SetWALSegmentID(segmentID uint16) {
	if p.WALSegmentID == 0 {
		p.WALSegmentID = segmentID
	}
}

// SizeBytes returns a conservative estimate of the in-memory bytes
// consumed by accumulated records. Used by the engine layer to decide
// whether the spike-protection ceiling has been crossed.
func (p *EventsPage) SizeBytes() int {
	// Per record: EventID(2) + TS(8) + ValueType(1) + ValueRaw(4) + payload ptr/len overhead.
	// We don't count the slice header overhead — it's a constant factor
	// that doesn't materially change the spike-detection threshold.
	const perRecordOverhead = 2 + 8 + 1 + 4
	return len(p.EventIDs)*perRecordOverhead + p.payloadBytes
}

// Count returns the number of accumulated records.
func (p *EventsPage) Count() int { return len(p.EventIDs) }

// IsFull returns true when any soft flush threshold is crossed
// (max_records / max_bytes / max_age). Polled by the engine layer
// after each AddEvent.
func (p *EventsPage) IsFull() bool {
	maxRecords := p.MaxRecords
	if maxRecords <= 0 {
		maxRecords = EventsPageMaxRecords
	}
	maxBytes := p.MaxBytes
	if maxBytes <= 0 {
		maxBytes = EventsPageMaxBytes
	}
	maxAge := p.MaxAge
	if maxAge <= 0 {
		maxAge = EventsPageMaxAge
	}
	if len(p.EventIDs) >= maxRecords {
		return true
	}
	if p.SizeBytes() >= maxBytes {
		return true
	}
	if !p.createdAt.IsZero() && time.Since(p.createdAt) >= maxAge {
		return true
	}
	return false
}

// MustForceFlush returns true when the spike-protection ceiling
// (max_in_memory_bytes) has been crossed. The engine layer treats this
// as a hard back-pressure signal: flush before accepting any further
// AddEvent for this page. Separate from IsFull because the ceiling is
// about preventing OOM, not about producing reasonably-sized pages.
func (p *EventsPage) MustForceFlush() bool {
	if p.MaxInMemoryBytes <= 0 {
		return false
	}
	return p.SizeBytes() >= p.MaxInMemoryBytes
}

// EncodeInto serializes the page as one frame: fixed header + uvarint
// compressed length is NOT used here (the header carries CompressedLen
// as uint32 directly, per the design); S2-compressed payload; CRC32
// trailer. The format mirrors data-*.dat's per-frame discipline but
// uses a dedicated header layout to carry the event-id bitmap.
func (p *EventsPage) EncodeInto(bb *bytes.Buffer) error {
	n := len(p.EventIDs)
	if n == 0 {
		return fmt.Errorf("events page: cannot encode empty page")
	}

	payload := eventsPageEncodeBufferPool.Get().(*bytes.Buffer)
	payload.Reset()
	defer eventsPageEncodeBufferPool.Put(payload)

	for i := range n {
		// EventID (2 bytes LE)
		var idBuf [2]byte
		binary.LittleEndian.PutUint16(idBuf[:], uint16(p.EventIDs[i]))
		payload.Write(idBuf[:])

		// TS (8 bytes LE)
		var tsBuf [8]byte
		binary.LittleEndian.PutUint64(tsBuf[:], uint64(p.Times[i]))
		payload.Write(tsBuf[:])

		// Value (4 bytes when not none)
		if p.ValueTypes[i] != EventValueNone {
			var vBuf [4]byte
			binary.LittleEndian.PutUint32(vBuf[:], p.ValuesRaw[i])
			payload.Write(vBuf[:])
		}

		// PayloadLen (uvarint) + Payload bytes
		var lenBuf [binary.MaxVarintLen64]byte
		nLen := binary.PutUvarint(lenBuf[:], uint64(len(p.Payloads[i])))
		payload.Write(lenBuf[:nLen])
		if len(p.Payloads[i]) > 0 {
			payload.Write(p.Payloads[i])
		}
	}

	compressed := s2.Encode(nil, payload.Bytes())
	if len(compressed) > MaxOnDiskFramePayloadBytes {
		return fmt.Errorf("events page: encoded frame %d bytes exceeds cap %d", len(compressed), MaxOnDiskFramePayloadBytes)
	}

	h := EventsPageHeader{
		StartTime:     p.Start,
		EndTime:       p.End,
		RecordCount:   uint32(n),
		EventIDBitmap: p.EventIDSet,
		CompressedLen: uint32(len(compressed)),
	}
	if err := h.encode(bb); err != nil {
		return err
	}
	if _, err := bb.Write(compressed); err != nil {
		return err
	}

	var crcBuf [4]byte
	binary.LittleEndian.PutUint32(crcBuf[:], crc32.ChecksumIEEE(compressed))
	if _, err := bb.Write(crcBuf[:]); err != nil {
		return err
	}
	return nil
}

// DecodeFromFrame populates p from the bytes following an already-read
// EventsPageHeader. The catalog is required to interpret each record's
// value width — same coupling the events WAL has. records past the
// header.RecordCount are a hard error (corruption surface).
func (p *EventsPage) DecodeFromFrame(h EventsPageHeader, compressed []byte, expectedCRC uint32, cat *EventCatalog) error {
	if cat == nil {
		return fmt.Errorf("events page decode: catalog required")
	}
	if h.CompressedLen != uint32(len(compressed)) {
		return fmt.Errorf("events page decode: header.CompressedLen=%d does not match compressed slice len=%d", h.CompressedLen, len(compressed))
	}
	if actual := crc32.ChecksumIEEE(compressed); actual != expectedCRC {
		return fmt.Errorf("events page decode: crc mismatch: expected=%08x actual=%08x", expectedCRC, actual)
	}

	payload, err := s2.Decode(nil, compressed)
	if err != nil {
		return err
	}

	n := int(h.RecordCount)
	p.Start = h.StartTime
	p.End = h.EndTime
	p.EventIDSet = h.EventIDBitmap
	p.EventIDs = p.EventIDs[:0]
	p.Times = p.Times[:0]
	p.ValueTypes = p.ValueTypes[:0]
	p.ValuesRaw = p.ValuesRaw[:0]
	p.Payloads = p.Payloads[:0]
	p.payloadBytes = 0

	pos := 0
	for i := range n {
		if pos+2+8 > len(payload) {
			return fmt.Errorf("events page record[%d]: truncated header bytes", i)
		}
		id := EventID(binary.LittleEndian.Uint16(payload[pos:]))
		pos += 2
		ts := Timestamp(binary.LittleEndian.Uint64(payload[pos:]))
		pos += 8

		// Look up value type from catalog (the on-disk record omits it).
		_, entry, ok := cat.GetEventByID(id)
		if !ok {
			return fmt.Errorf("events page record[%d]: unknown event id %d", i, id)
		}
		valueType := entry.ValueType
		var valueRaw uint32
		if valueType != EventValueNone {
			if pos+4 > len(payload) {
				return fmt.Errorf("events page record[%d]: truncated value", i)
			}
			valueRaw = binary.LittleEndian.Uint32(payload[pos:])
			pos += 4
		}

		payloadLen, varLen := binary.Uvarint(payload[pos:])
		if varLen <= 0 {
			return fmt.Errorf("events page record[%d]: malformed payload length", i)
		}
		pos += varLen
		if pos+int(payloadLen) > len(payload) {
			return fmt.Errorf("events page record[%d]: truncated payload bytes", i)
		}
		var pbytes []byte
		if payloadLen > 0 {
			pbytes = append([]byte(nil), payload[pos:pos+int(payloadLen)]...)
			p.payloadBytes += len(pbytes)
		}
		pos += int(payloadLen)

		p.EventIDs = append(p.EventIDs, id)
		p.Times = append(p.Times, ts)
		p.ValueTypes = append(p.ValueTypes, valueType)
		p.ValuesRaw = append(p.ValuesRaw, valueRaw)
		p.Payloads = append(p.Payloads, pbytes)
	}

	// Trailing bytes inside a single frame's decompressed payload are a
	// hard error (writer/reader layout disagreement).
	if pos != len(payload) {
		return fmt.Errorf("events page: %d trailing bytes after decode", len(payload)-pos)
	}
	return nil
}
