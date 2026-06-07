package engine

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"sync"
	"time"
)

// EventsWAL manages a single reusable write-ahead log file for one
// database's events stream. It is structurally analogous to WAL
// (metric WAL) but uses an independent file (<db>.events.wal) and a
// different record format. The two WALs never share bytes on disk.
//
// On-disk record format (v1):
//
//	[uvarint: payload_len]
//	  EventID     uint16 LE
//	  TS          int64  LE
//	  Flags       uint8         bit 7 = newEvent (name + value_type follow)
//	                            bits 0..6 reserved (must be zero in v1)
//	  [if Flags & newEvent]
//	    NameLen   uint8         (1..MaxEventNameLen)
//	    Name      NameLen bytes
//	    ValueType uint8         (0=none, 1=int32, 2=float32)
//	  [if event ValueType != EventValueNone, per catalog (or inline newEvent)]
//	    Value     4 bytes LE
//	  PayloadLen  uvarint
//	  Payload     PayloadLen bytes
//
// Hot-path size (known event, int32 value, no payload):
//
//	1 (varint length) + 2 + 8 + 1 + 4 + 1 = 17 bytes
//
// Decoding non-newEvent records requires the events catalog because
// presence of the Value field depends on the event's pinned ValueType.
// This is the same coupling the metric WAL accepts (sentinel ValueType
// resolved via catalog at replay).
type EventsWAL struct {
	path        string
	currentFile *os.File
	maxSegSize  int64
	fsyncPolicy string
	bufferSize  int64
	statsMu     sync.RWMutex
	stats       WALStats
	hook        walLifecycleHook
}

// SetLifecycleHook installs an engine-side observer for events WAL
// events. Mirrors WAL.SetLifecycleHook.
func (w *EventsWAL) SetLifecycleHook(h walLifecycleHook) {
	if w == nil {
		return
	}
	w.hook = h
}

// Events WAL flag-byte definitions. Independent of the metric WAL's
// CompactTL bit assignments because the file format is independent.
const (
	walEventNewEvent      byte = 0x80 // bit 7: name + value_type follow inline
	walEventReservedBits  byte = 0x7F // bits 0..6 reserved; must be zero in v1
)

// EventWALRecord is a single decoded record from an EventsWAL file.
type EventWALRecord struct {
	SegmentID    uint16
	EventID      EventID
	EventName    string // populated on newEvent records; empty otherwise
	TS           Timestamp
	ValueType    byte // EventValueNone, Int32Sample, or Float32Sample
	Int32Value   int32
	Float32Value float32
	Payload      []byte
}

// ErrEventsWALUnknownEventID is returned when RecordsWithCatalog
// encounters a non-newEvent record whose EventID has no catalog entry.
// Mirrors the metric WAL's "wal record omits value_type; catalog has no
// entry" failure mode.
var ErrEventsWALUnknownEventID = errors.New("events wal record references unknown event id")

// ErrEventsWALReservedFlagSet is returned when a record's Flags byte has
// any bit other than newEvent set. v1 reserves bits 0..6; an unknown bit
// is treated as a hard parse error rather than silently ignored, so we
// surface a forward-incompatible WAL early.
var ErrEventsWALReservedFlagSet = errors.New("events wal record has reserved flag bits set")

var eventsWALAppendBufferPool = sync.Pool{
	New: func() any {
		return bytes.NewBuffer(make([]byte, 0, 32))
	},
}

// NewEventsWAL opens (creating if missing) the events WAL file at the
// given path. Mirrors NewWAL — same fsync-policy validation, same
// append-mode O_RDWR|O_CREATE|O_APPEND open flags.
func NewEventsWAL(path string, maxSegmentSize int64, fsyncPolicy string) (*EventsWAL, error) {
	if path == "" {
		return nil, fmt.Errorf("events wal path cannot be empty")
	}
	if fsyncPolicy == "" {
		fsyncPolicy = WALFsyncPolicySegment
	}
	if fsyncPolicy != WALFsyncPolicySegment && fsyncPolicy != WALFsyncPolicyAlways {
		return nil, fmt.Errorf("invalid events wal fsync policy %q", fsyncPolicy)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0644)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &EventsWAL{
		path:        path,
		currentFile: f,
		maxSegSize:  maxSegmentSize,
		fsyncPolicy: fsyncPolicy,
		bufferSize:  st.Size(),
		stats: WALStats{
			BufferBytes: st.Size(),
		},
	}, nil
}

// OpenAndRecoverEventsWAL opens an events WAL file and seeds runtime
// stats (count + bytes) from a one-shot scan of the existing on-disk
// content. Mirrors OpenAndRecoverWAL. Replay-into-page is the caller's
// responsibility (via RecordsWithCatalog) — opening alone does not
// mutate any in-memory page state.
func OpenAndRecoverEventsWAL(path, fsyncPolicy string) (*EventsWAL, error) {
	w, err := NewEventsWAL(path, 0, fsyncPolicy)
	if err != nil {
		return nil, err
	}

	count, bytesConsumed, err := scanEventsWALAppendStats(path)
	if err != nil {
		_ = w.Close()
		return nil, err
	}
	w.withStatsLock(func(stats *WALStats) {
		stats.AppendCount = count
		stats.AppendBytes = bytesConsumed
	})
	return w, nil
}

// AppendEvent writes a known-event occurrence (no inline name). The
// caller-supplied valueType must match the catalog's pinned type for
// this event. When valueType == EventValueNone, valueRaw is ignored
// and no value bytes are written. payload may be nil or empty.
//
// Returns the segment id (always 1 in v1; preserved for API symmetry
// with AppendSample).
func AppendEvent(w *EventsWAL, eventID EventID, ts Timestamp, valueType byte, valueRaw [4]byte, payload []byte) (uint16, error) {
	return appendEventRecord(w, eventID, "", ts, valueType, valueRaw, payload)
}

// AppendEventWithName writes a newEvent record that carries the event
// name + value type inline. Used on the first occurrence of a name so
// catalog state can be rebuilt from WAL alone after a crash that
// occurred before the events catalog was rewritten.
func AppendEventWithName(w *EventsWAL, eventID EventID, name string, ts Timestamp, valueType byte, valueRaw [4]byte, payload []byte) (uint16, error) {
	if name == "" {
		return 0, fmt.Errorf("events wal: AppendEventWithName called with empty name (use AppendEvent for known events)")
	}
	return appendEventRecord(w, eventID, name, ts, valueType, valueRaw, payload)
}

func appendEventRecord(w *EventsWAL, eventID EventID, name string, ts Timestamp, valueType byte, valueRaw [4]byte, payload []byte) (uint16, error) {
	if w == nil || w.currentFile == nil {
		return 0, fmt.Errorf("events wal is nil")
	}
	if eventID == 0 || uint16(eventID) > MaxEventsPerDatabase {
		return 0, fmt.Errorf("events wal: event id %d out of range [1..%d]", eventID, MaxEventsPerDatabase)
	}
	if !IsValidEventValueType(valueType) {
		return 0, fmt.Errorf("events wal: invalid value_type byte %d", valueType)
	}
	if name != "" && len(name) > MaxEventNameLen {
		// Belt-and-braces: ingest validation already rejects names >255 bytes,
		// but the on-disk NameLen field is uint8, so any oversize name here
		// would silently truncate the prefix. Surface as a write error
		// instead. Mirrors the equivalent guard in AppendSampleWithMetricName.
		return 0, fmt.Errorf("events wal: event name %q exceeds %d-byte limit", name, MaxEventNameLen)
	}

	buf := eventsWALAppendBufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer eventsWALAppendBufferPool.Put(buf)

	// EventID (2 bytes LE)
	var idBuf [2]byte
	binary.LittleEndian.PutUint16(idBuf[:], uint16(eventID))
	buf.Write(idBuf[:])

	// TS (8 bytes LE). v1 uses the full timestamp (no delta encoding) —
	// events are sparse, so the metric WAL's baseline/delta scheme would
	// pay its own overhead more often than it saves bytes.
	var tsBuf [8]byte
	binary.LittleEndian.PutUint64(tsBuf[:], uint64(ts))
	buf.Write(tsBuf[:])

	// Flags
	flags := byte(0)
	if name != "" {
		flags |= walEventNewEvent
	}
	buf.WriteByte(flags)

	// Inline name + ValueType when newEvent.
	if name != "" {
		buf.WriteByte(byte(len(name)))
		buf.WriteString(name)
		buf.WriteByte(valueType)
	}

	// Value (4 bytes) when the event carries a numeric value.
	if valueType != EventValueNone {
		buf.Write(valueRaw[:])
	}

	// Payload length + payload bytes. PayloadLen is a uvarint, so 0-byte
	// payloads cost exactly one byte on disk.
	var payloadLenBuf [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(payloadLenBuf[:], uint64(len(payload)))
	buf.Write(payloadLenBuf[:n])
	if len(payload) > 0 {
		buf.Write(payload)
	}

	recordPayload := buf.Bytes()

	// Outer length prefix.
	var lenBuf [binary.MaxVarintLen64]byte
	nLen := binary.PutUvarint(lenBuf[:], uint64(len(recordPayload)))

	if wrote, err := w.currentFile.Write(lenBuf[:nLen]); err != nil {
		return 0, err
	} else if wrote != nLen {
		return 0, fmt.Errorf("short events wal write (length): wrote=%d want=%d", wrote, nLen)
	}

	if wrote, err := w.currentFile.Write(recordPayload); err != nil {
		return 0, err
	} else if wrote != len(recordPayload) {
		return 0, fmt.Errorf("short events wal write (payload): wrote=%d want=%d", wrote, len(recordPayload))
	}

	totalBytes := int64(nLen) + int64(len(recordPayload))
	w.bufferSize += totalBytes
	now := time.Now()
	w.withStatsLock(func(stats *WALStats) {
		stats.AppendCount++
		stats.AppendBytes += totalBytes
		stats.BufferBytes = w.bufferSize
		stats.LastAppendAt = now
	})

	if w.ShouldFsyncAfterAppend() {
		if err := w.Fsync(); err != nil {
			return 0, err
		}
	}

	return 1, nil
}

// ShouldFsyncAfterAppend mirrors WAL.ShouldFsyncAfterAppend.
func (w *EventsWAL) ShouldFsyncAfterAppend() bool {
	if w == nil {
		return false
	}
	switch w.fsyncPolicy {
	case WALFsyncPolicyAlways:
		return true
	case "", WALFsyncPolicySegment:
		return w.SegmentFull()
	default:
		return false
	}
}

// SegmentFull mirrors WAL.SegmentFull.
func (w *EventsWAL) SegmentFull() bool {
	if w == nil || w.maxSegSize <= 0 {
		return false
	}
	return w.bufferSize >= w.maxSegSize
}

// Fsync calls fsync on the events WAL file. Mirrors WAL.Fsync; the only
// difference is the stats target is this EventsWAL's WALStats.
func (w *EventsWAL) Fsync() error {
	if w == nil || w.currentFile == nil {
		return fmt.Errorf("events wal is nil")
	}
	start := time.Now()
	if err := w.currentFile.Sync(); err != nil {
		if w.hook.onFsyncError != nil {
			w.hook.onFsyncError(w.hook.dbName, w.hook.file, err)
		}
		return err
	}
	dur := time.Since(start)
	now := time.Now()
	w.withStatsLock(func(stats *WALStats) {
		stats.FsyncCount++
		stats.FsyncDurationTotal += dur
		if stats.MinFsyncDuration == 0 || dur < stats.MinFsyncDuration {
			stats.MinFsyncDuration = dur
		}
		if dur > stats.MaxFsyncDuration {
			stats.MaxFsyncDuration = dur
		}
		stats.LastFsyncAt = now
	})
	if w.hook.onFsyncSlow != nil && dur >= walSlowFsyncThreshold {
		w.hook.onFsyncSlow(w.hook.dbName, w.hook.file, float64(dur.Microseconds())/1000.0)
	}
	return nil
}

// Flush is retained for parity with WAL.Flush; events WAL writes go
// straight to the OS page cache like the metric WAL, so this is a no-op.
func (w *EventsWAL) Flush() error { return nil }

// Reset truncates the events WAL file to zero and reuses it. The caller
// MUST persist the events catalog (and have flushed the in-memory events
// page) before invoking Reset — see crash-safety contract rule 1 in
// docs/EVENTS.md.
func (w *EventsWAL) Reset() error {
	if w == nil || w.currentFile == nil {
		return fmt.Errorf("events wal is nil")
	}
	if w.bufferSize == 0 {
		return nil
	}
	resetStart := time.Now()
	flushed := w.bufferSize
	if err := w.Fsync(); err != nil {
		return err
	}
	if err := w.currentFile.Truncate(0); err != nil {
		return err
	}
	if _, err := w.currentFile.Seek(0, 0); err != nil {
		return err
	}
	w.bufferSize = 0
	w.recordFlush(flushed)
	resetDur := time.Since(resetStart)
	w.withStatsLock(func(stats *WALStats) {
		stats.BufferBytes = 0
		stats.ResetDurationTotal += resetDur
		if stats.MinResetDuration == 0 || resetDur < stats.MinResetDuration {
			stats.MinResetDuration = resetDur
		}
		if resetDur > stats.MaxResetDuration {
			stats.MaxResetDuration = resetDur
		}
	})
	if w.hook.onReset != nil {
		w.hook.onReset(w.hook.dbName, w.hook.file, flushed)
	}
	return nil
}

// Close closes the underlying file.
func (w *EventsWAL) Close() error {
	if w == nil {
		return nil
	}
	if w.currentFile != nil {
		return w.currentFile.Close()
	}
	return nil
}

// Stats returns a snapshot of the events WAL stats. Mirrors WAL.Stats.
func (w *EventsWAL) Stats() WALStats {
	if w == nil {
		return WALStats{}
	}
	w.statsMu.RLock()
	defer w.statsMu.RUnlock()

	out := w.stats
	out.RecentFlushes = append([]WALFlushEvent(nil), w.stats.RecentFlushes...)
	return out
}

// Path returns the on-disk file path. Useful for inspect tooling.
func (w *EventsWAL) Path() string {
	if w == nil {
		return ""
	}
	return w.path
}

// RecordsWithCatalog scans the events WAL and returns every decodable
// record, materializing in the supplied catalog any newEvent entries it
// encounters. Catalog is REQUIRED — non-newEvent records cannot be
// decoded without the pinned ValueType lookup.
//
// Crash-tail handling is intentionally narrow: the only legitimate
// crash-tail signal is the *outer* uvarint length prefix being short
// or pointing past EOF. By the time a payload reaches the decoder, the
// writer has already declared a length that matches the available
// bytes, so any decoder-level error means the writer and reader
// disagree about layout — corruption, not a benign mid-write crash.
// All decoder errors are therefore propagated as hard failures.
func (w *EventsWAL) RecordsWithCatalog(cat *EventCatalog) ([]EventWALRecord, error) {
	if w == nil {
		return nil, fmt.Errorf("events wal is nil")
	}
	if cat == nil {
		return nil, fmt.Errorf("events wal: catalog required for full decode")
	}
	blob, err := os.ReadFile(w.path)
	if err != nil {
		return nil, err
	}

	out := make([]EventWALRecord, 0, 64)
	for pos := 0; pos < len(blob); {
		payloadLen, n := binary.Uvarint(blob[pos:])
		if n <= 0 {
			// Truncated outer length prefix — crash tail.
			break
		}
		if payloadLen > uint64(len(blob)-pos-n) {
			// Outer length declares more bytes than the file has
			// after it — crash tail mid-payload-write.
			break
		}
		pos += n
		payload := blob[pos : pos+int(payloadLen)]
		pos += int(payloadLen)

		rec, err := decodeEventsWALPayload(payload, cat)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, nil
}

// decodeEventsWALPayload decodes one record's payload bytes, using the
// catalog to resolve the value type for non-newEvent records (and to
// materialize the catalog entry for newEvent records that aren't there
// yet — the WAL is the source of truth for "this event exists" during
// the window between in-memory assignment and catalog write).
func decodeEventsWALPayload(payload []byte, cat *EventCatalog) (EventWALRecord, error) {
	if len(payload) < 2+8+1+1 { // EventID + TS + Flags + min PayloadLen byte
		return EventWALRecord{}, fmt.Errorf("events wal payload too short: %d bytes", len(payload))
	}

	pos := 0
	eventID := EventID(binary.LittleEndian.Uint16(payload[pos : pos+2]))
	pos += 2

	ts := Timestamp(binary.LittleEndian.Uint64(payload[pos : pos+8]))
	pos += 8

	flags := payload[pos]
	pos++

	if flags&walEventReservedBits != 0 {
		return EventWALRecord{}, ErrEventsWALReservedFlagSet
	}

	rec := EventWALRecord{
		SegmentID: 1,
		EventID:   eventID,
		TS:        ts,
	}

	var valueType byte
	if flags&walEventNewEvent != 0 {
		// Inline: NameLen + Name + ValueType.
		if pos+1 > len(payload) {
			return EventWALRecord{}, fmt.Errorf("truncated event name length")
		}
		nameLen := int(payload[pos])
		pos++
		if pos+nameLen+1 > len(payload) {
			return EventWALRecord{}, fmt.Errorf("truncated event name")
		}
		rec.EventName = string(payload[pos : pos+nameLen])
		pos += nameLen
		valueType = payload[pos]
		pos++
		if !IsValidEventValueType(valueType) {
			return EventWALRecord{}, fmt.Errorf("invalid inline event value_type %d", valueType)
		}
		// Materialize / cross-check catalog. If catalog has no entry,
		// register from the inline data (this is the recovery path for
		// "crashed after WAL append, before catalog write"). If it
		// disagrees, that is a hard error per crash-safety rule 4.
		if err := cat.EnsureEventEntry(rec.EventName, eventID, valueType); err != nil {
			return EventWALRecord{}, fmt.Errorf("events wal newEvent: %w", err)
		}
	} else {
		// Known event: resolve ValueType from catalog.
		name, entry, ok := cat.GetEventByID(eventID)
		if !ok {
			return EventWALRecord{}, ErrEventsWALUnknownEventID
		}
		rec.EventName = name
		valueType = entry.ValueType
	}
	rec.ValueType = valueType

	// Optional Value field.
	if valueType != EventValueNone {
		if pos+4 > len(payload) {
			return EventWALRecord{}, fmt.Errorf("truncated event value")
		}
		raw := binary.LittleEndian.Uint32(payload[pos : pos+4])
		pos += 4
		switch valueType {
		case Int32Sample:
			rec.Int32Value = int32(raw)
		case Float32Sample:
			rec.Float32Value = math.Float32frombits(raw)
		}
	}

	// Payload length + bytes.
	payloadLen, n := binary.Uvarint(payload[pos:])
	if n <= 0 {
		return EventWALRecord{}, fmt.Errorf("malformed event payload length")
	}
	pos += n
	if pos+int(payloadLen) > len(payload) {
		return EventWALRecord{}, fmt.Errorf("truncated event payload")
	}
	if payloadLen > 0 {
		rec.Payload = append([]byte(nil), payload[pos:pos+int(payloadLen)]...)
	}
	pos += int(payloadLen)

	// Trailing bytes inside a single record are a hard error (corruption
	// surface). The outer caller has already trimmed `payload` to the
	// length prefix, so a mismatch means our decoder skipped bytes the
	// encoder wrote — surface it loudly rather than silently dropping
	// them.
	if pos != len(payload) {
		return EventWALRecord{}, fmt.Errorf("events wal record has %d trailing bytes after decode", len(payload)-pos)
	}

	return rec, nil
}

// scanEventsWALAppendStats counts valid records and consumed bytes in
// an events WAL file. Mirrors scanWALAppendStats. Stops on the first
// truncated/corrupt record (treated as crash-tail). Does not require
// the catalog because it uses the outer uvarint length prefix to skip
// records without decoding their payloads.
func scanEventsWALAppendStats(path string) (int64, int64, error) {
	blob, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, nil
		}
		return 0, 0, err
	}

	var count int64
	var consumed int64
	for pos := 0; pos < len(blob); {
		payloadLen, n := binary.Uvarint(blob[pos:])
		if n <= 0 {
			break
		}
		if payloadLen > uint64(len(blob)-pos-n) {
			break
		}
		// We do not validate inner payload bytes here — the goal is
		// fast stats reconstruction at open. RecordsWithCatalog is the
		// real validator.
		count++
		pos += n + int(payloadLen)
		consumed = int64(pos)
	}
	return count, consumed, nil
}

func (w *EventsWAL) recordFlush(bytesFlushed int64) {
	now := time.Now()
	w.withStatsLock(func(stats *WALStats) {
		stats.FlushCount++
		stats.FlushedBytes += bytesFlushed
		if stats.MinFlushBytes == 0 || bytesFlushed < stats.MinFlushBytes {
			stats.MinFlushBytes = bytesFlushed
		}
		if bytesFlushed > stats.MaxFlushBytes {
			stats.MaxFlushBytes = bytesFlushed
		}
		if !stats.LastFlushAt.IsZero() {
			iv := now.Sub(stats.LastFlushAt)
			stats.FlushIntervalCount++
			stats.FlushIntervalTotal += iv
			if stats.MinFlushInterval == 0 || iv < stats.MinFlushInterval {
				stats.MinFlushInterval = iv
			}
			if iv > stats.MaxFlushInterval {
				stats.MaxFlushInterval = iv
			}
		}
		stats.LastFlushAt = now
		stats.RecentFlushes = append(stats.RecentFlushes, WALFlushEvent{At: now, Bytes: bytesFlushed})
		if len(stats.RecentFlushes) > walFlushHistoryLimit {
			stats.RecentFlushes = append([]WALFlushEvent(nil), stats.RecentFlushes[len(stats.RecentFlushes)-walFlushHistoryLimit:]...)
		}
	})
}

func (w *EventsWAL) withStatsLock(fn func(*WALStats)) {
	if w == nil || fn == nil {
		return
	}
	w.statsMu.Lock()
	defer w.statsMu.Unlock()
	fn(&w.stats)
}
