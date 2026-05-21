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

// WALRecord represents a single entry in the WAL
// Used for crash recovery (LAW 9 — deterministic replay)
type WALRecord struct {
	SegmentID  uint16 // ID of the segment this record came from (LAW 6 origin, wal-NNNN.log format)
	MetricID   MetricID
	MetricName string
	Timestamp  Timestamp
	ValueType  byte        // Int32Sample or Float32Sample
	Value      interface{} // int32 or float32
}

var ErrWALMissingBaseline = errors.New("wal compact record missing initial baseline")

const (
	// Compact WAL format flags (1 byte: CompactTL)
	walCompactNewBaseline byte = 0x80 // bit 7: new baseline TS follows (8 bytes)
	walCompactNewMetric   byte = 0x40 // bit 6: new metric with metric name + value type

	walFlushHistoryLimit = 256
)

const (
	WALFsyncPolicySegment = "segment"
	WALFsyncPolicyAlways  = "always"
)

type WALFlushEvent struct {
	At    time.Time
	Bytes int64
}

type WALStats struct {
	AppendCount        int64
	AppendBytes        int64
	BufferBytes        int64
	FsyncCount         int64
	FsyncDurationTotal time.Duration
	MinFsyncDuration   time.Duration
	MaxFsyncDuration   time.Duration
	FlushCount         int64
	FlushedBytes       int64
	MinFlushBytes      int64
	MaxFlushBytes      int64
	ResetDurationTotal time.Duration
	MinResetDuration   time.Duration
	MaxResetDuration   time.Duration
	LastAppendAt       time.Time
	LastFsyncAt        time.Time
	LastFlushAt        time.Time
	FlushIntervalCount int64
	FlushIntervalTotal time.Duration
	MinFlushInterval   time.Duration
	MaxFlushInterval   time.Duration
	RecentFlushes      []WALFlushEvent
}

var walAppendBufferPool = sync.Pool{
	New: func() any {
		return bytes.NewBuffer(make([]byte, 0, 24))
	},
}

// WAL manages a single reusable write-ahead log file.
type WAL struct {
	path        string
	currentFile *os.File
	maxSegSize  int64
	fsyncPolicy string
	bufferSize  int64
	baselineTS  Timestamp // baseline timestamp for delta encoding
	hasBaseline bool      // whether baseline has been written
	stats       WALStats
}

// NewWAL creates a new WAL manager
func NewWAL(path string, maxSegmentSize int64, fsyncPolicy string) (*WAL, error) {
	if path == "" {
		return nil, fmt.Errorf("wal path cannot be empty")
	}
	if fsyncPolicy == "" {
		fsyncPolicy = WALFsyncPolicySegment
	}
	if fsyncPolicy != WALFsyncPolicySegment && fsyncPolicy != WALFsyncPolicyAlways {
		return nil, fmt.Errorf("invalid wal fsync policy %q", fsyncPolicy)
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
	return &WAL{
		path:        path,
		currentFile: f,
		maxSegSize:  maxSegmentSize,
		fsyncPolicy: fsyncPolicy,
		bufferSize:  st.Size(),
		baselineTS:  0,
		hasBaseline: false,
		stats: WALStats{
			BufferBytes: st.Size(),
		},
	}, nil
}

// AppendSample writes a typed sample to the current WAL segment
// Satisfies LAW 1 — sample written to WAL before memory mutation
// Returns current segment ID for tracking page origin (LAW 6)
func AppendSample[T SampleType](w *WAL, metricID MetricID, ts Timestamp, value T) (uint16, error) {
	return appendSampleRecord(w, metricID, "", ts, value)
}

// AppendSampleWithMetricName writes a typed sample and embeds the metric name.
// This is used on first-seen metrics so catalog state can be rebuilt from WAL.
func AppendSampleWithMetricName[T SampleType](w *WAL, metricID MetricID, metricName string, ts Timestamp, value T) (uint16, error) {
	return appendSampleRecord(w, metricID, metricName, ts, value)
}

func appendSampleRecord[T SampleType](w *WAL, metricID MetricID, metricName string, ts Timestamp, value T) (uint16, error) {
	if w == nil || w.currentFile == nil {
		return 0, fmt.Errorf("wal is nil")
	}

	var vtype byte
	var raw [4]byte
	switch v := any(value).(type) {
	case int32:
		vtype = Int32Sample
		binary.LittleEndian.PutUint32(raw[:], uint32(v))
	case float32:
		vtype = Float32Sample
		binary.LittleEndian.PutUint32(raw[:], math.Float32bits(v))
	default:
		return 0, fmt.Errorf("unsupported sample type")
	}

	// Reuse encode buffers to reduce per-append allocations.
	buf := walAppendBufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer walAppendBufferPool.Put(buf)

	// Write new baseline TS if needed (first record or large gap)
	var newBaseline bool
	if !w.hasBaseline || ts < w.baselineTS || (ts-w.baselineTS) > (1<<24)-1 {
		newBaseline = true
		w.baselineTS = ts
		w.hasBaseline = true
	}

	// Compact WAL format (v2):
	// MetricID (2) + TS delta (3) + CompactTL (1) + [metric name TLV if new] + value (4)
	var midBuf [2]byte
	binary.LittleEndian.PutUint16(midBuf[:], uint16(metricID))
	buf.Write(midBuf[:])

	// Timestamp delta from baseline (3 bytes LE)
	tsDelta := ts - w.baselineTS
	if tsDelta < 0 || tsDelta >= (1<<24) {
		return 0, fmt.Errorf("timestamp delta out of range: %d", tsDelta)
	}
	var tsDeltaBuf [3]byte
	tsDeltaBuf[0] = byte(tsDelta)
	tsDeltaBuf[1] = byte(tsDelta >> 8)
	tsDeltaBuf[2] = byte(tsDelta >> 16)
	buf.Write(tsDeltaBuf[:])

	// CompactTL flags
	compactTL := byte(0)
	if newBaseline {
		compactTL |= walCompactNewBaseline
	}
	if metricName != "" {
		compactTL |= walCompactNewMetric
	}
	buf.WriteByte(compactTL)

	// If new baseline, write the full 8-byte baseline TS
	if newBaseline {
		var baselineBuf [8]byte
		binary.LittleEndian.PutUint64(baselineBuf[:], uint64(w.baselineTS))
		buf.Write(baselineBuf[:])
	}

	// If new metric, write metric name and value type
	if metricName != "" {
		buf.WriteByte(byte(len(metricName)))
		buf.WriteString(metricName)
		buf.WriteByte(vtype)
	}

	// Value (4 bytes, always present)
	buf.Write(raw[:])

	payload := buf.Bytes()

	// Build record with length prefix
	var lenBuf [binary.MaxVarintLen64]byte
	nLen := binary.PutUvarint(lenBuf[:], uint64(len(payload)))

	// Write in two parts to avoid additional allocation
	n, err := w.currentFile.Write(lenBuf[:nLen])
	if err != nil {
		return 0, err
	}
	if n != nLen {
		return 0, fmt.Errorf("short wal write (length): wrote=%d want=%d", n, nLen)
	}

	n, err = w.currentFile.Write(payload)
	if err != nil {
		return 0, err
	}
	if n != len(payload) {
		return 0, fmt.Errorf("short wal write (payload): wrote=%d want=%d", n, len(payload))
	}
	totalBytes := int64(nLen) + int64(n)
	w.bufferSize += totalBytes
	w.stats.AppendCount++
	w.stats.AppendBytes += totalBytes
	w.stats.BufferBytes = w.bufferSize
	w.stats.LastAppendAt = time.Now()

	if w.ShouldFsyncAfterAppend() {
		if err := w.Fsync(); err != nil {
			return 0, err
		}
	}

	return 1, nil
}

func (w *WAL) ShouldFsyncAfterAppend() bool {
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

// SegmentFull checks if current segment should rotate
// Returns true if bufferSize >= maxSegSize
func (w *WAL) SegmentFull() bool {
	if w == nil || w.maxSegSize <= 0 {
		return false
	}
	return w.bufferSize >= w.maxSegSize
}

// RotateSegment is retained for API compatibility.
// WAL v1 uses a single file; rotate maps to reset-and-reuse.
func (w *WAL) RotateSegment() (uint16, error) {
	if w == nil {
		return 0, fmt.Errorf("wal is nil")
	}
	if err := w.Reset(); err != nil {
		return 0, err
	}
	return 1, nil
}

// Flush buffers buffered records to disk without rotating
// Does not fsync (records remain in OS page cache)
func (w *WAL) Flush() error {
	return nil
}

// Fsync calls fsync on the current segment
// Called on page flush and segment rotation (Durability & fsync Policy)
func (w *WAL) Fsync() error {
	if w == nil || w.currentFile == nil {
		return fmt.Errorf("wal is nil")
	}
	start := time.Now()
	if err := w.currentFile.Sync(); err != nil {
		return err
	}
	dur := time.Since(start)
	w.stats.FsyncCount++
	w.stats.FsyncDurationTotal += dur
	if w.stats.MinFsyncDuration == 0 || dur < w.stats.MinFsyncDuration {
		w.stats.MinFsyncDuration = dur
	}
	if dur > w.stats.MaxFsyncDuration {
		w.stats.MaxFsyncDuration = dur
	}
	w.stats.LastFsyncAt = time.Now()
	return nil
}

// Records returns all WAL records currently present in the WAL file.
func (w *WAL) Records() ([]WALRecord, error) {
	return w.RecordsWithCatalog(nil)
}

// RecordsWithCatalog returns all WAL records with optional catalog for ValueType lookups.
func (w *WAL) RecordsWithCatalog(cat *Catalog) ([]WALRecord, error) {
	if w == nil {
		return nil, fmt.Errorf("wal is nil")
	}
	blob, err := os.ReadFile(w.path)
	if err != nil {
		return nil, err
	}

	out := make([]WALRecord, 0, 64)
	var baselineTS Timestamp
	hasBaseline := false
	for pos := 0; pos < len(blob); {
		payloadLen, n := binary.Uvarint(blob[pos:])
		if n <= 0 {
			break
		}
		pos += n
		if payloadLen > uint64(len(blob)-pos) {
			break
		}

		payload := blob[pos : pos+int(payloadLen)]
		pos += int(payloadLen)

		rec, err := decodeWALPayloadCompactWithBaseline(payload, baselineTS, hasBaseline)
		if err != nil {
			if errors.Is(err, ErrWALMissingBaseline) && !hasBaseline {
				continue
			}
			break
		}

		// Update baseline if this record has a new one
		// Payload layout: MetricID(2) + TSDelta(3) + CompactTL(1) + [optional data]
		// CompactTL is at byte 5
		if len(payload) > 5 && (payload[5]&walCompactNewBaseline) != 0 && len(payload) >= 14 {
			baselineTS = Timestamp(binary.LittleEndian.Uint64(payload[6:14]))
			hasBaseline = true
		}

		// If catalog provided and ValueType is sentinel (0), look it up
		if cat != nil && rec.ValueType == 0 && rec.MetricName == "" {
			_, entry, ok := cat.GetMetricByID(rec.MetricID)
			if ok {
				rec.ValueType = entry.ValueType
				// Re-interpret the value bytes with the correct type
				var raw [4]byte
				if v, ok := rec.Value.(int32); ok {
					binary.LittleEndian.PutUint32(raw[:], uint32(v))
				} else if v, ok := rec.Value.(float32); ok {
					binary.LittleEndian.PutUint32(raw[:], math.Float32bits(v))
				}
				if entry.ValueType == Int32Sample {
					rec.Value = int32(binary.LittleEndian.Uint32(raw[:]))
				} else if entry.ValueType == Float32Sample {
					rec.Value = math.Float32frombits(binary.LittleEndian.Uint32(raw[:]))
				}
			}
		}

		out = append(out, rec)
	}

	return out, nil
}

// Close closes the current segment and cleans up
func (w *WAL) Close() error {
	if w.currentFile != nil {
		return w.currentFile.Close()
	}
	return nil
}

// DeleteSegment is retained for API compatibility.
func (w *WAL) DeleteSegment(segmentID uint16) error {
	if w == nil {
		return fmt.Errorf("wal is nil")
	}
	_ = segmentID
	return nil
}

// Reset truncates the WAL to zero and reuses it from the beginning.
func (w *WAL) Reset() error {
	if w == nil || w.currentFile == nil {
		return fmt.Errorf("wal is nil")
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
	w.baselineTS = 0
	w.hasBaseline = false
	w.stats.BufferBytes = 0
	w.recordFlush(flushed)
	resetDur := time.Since(resetStart)
	w.stats.ResetDurationTotal += resetDur
	if w.stats.MinResetDuration == 0 || resetDur < w.stats.MinResetDuration {
		w.stats.MinResetDuration = resetDur
	}
	if resetDur > w.stats.MaxResetDuration {
		w.stats.MaxResetDuration = resetDur
	}
	return nil
}

func (w *WAL) Stats() WALStats {
	if w == nil {
		return WALStats{}
	}
	out := w.stats
	out.RecentFlushes = append([]WALFlushEvent(nil), w.stats.RecentFlushes...)
	return out
}

func (w *WAL) recordFlush(bytes int64) {
	now := time.Now()
	w.stats.FlushCount++
	w.stats.FlushedBytes += bytes
	if w.stats.MinFlushBytes == 0 || bytes < w.stats.MinFlushBytes {
		w.stats.MinFlushBytes = bytes
	}
	if bytes > w.stats.MaxFlushBytes {
		w.stats.MaxFlushBytes = bytes
	}
	if !w.stats.LastFlushAt.IsZero() {
		iv := now.Sub(w.stats.LastFlushAt)
		w.stats.FlushIntervalCount++
		w.stats.FlushIntervalTotal += iv
		if w.stats.MinFlushInterval == 0 || iv < w.stats.MinFlushInterval {
			w.stats.MinFlushInterval = iv
		}
		if iv > w.stats.MaxFlushInterval {
			w.stats.MaxFlushInterval = iv
		}
	}
	w.stats.LastFlushAt = now
	w.stats.RecentFlushes = append(w.stats.RecentFlushes, WALFlushEvent{At: now, Bytes: bytes})
	if len(w.stats.RecentFlushes) > walFlushHistoryLimit {
		w.stats.RecentFlushes = append([]WALFlushEvent(nil), w.stats.RecentFlushes[len(w.stats.RecentFlushes)-walFlushHistoryLimit:]...)
	}
}

func OpenAndRecoverWAL(name string, fsyncPolicy string) (*WAL, error) {
	w, err := NewWAL(name, 0, fsyncPolicy)
	if err != nil {
		return nil, err
	}

	count, bytes, err := scanWALAppendStats(name)
	if err != nil {
		_ = w.Close()
		return nil, err
	}
	w.stats.AppendCount = count
	w.stats.AppendBytes = bytes
	return w, nil
}

// scanWALAppendStats counts valid records and consumed bytes in a WAL file.
// It stops at the first truncated/corrupt record, matching Records() behavior.
func scanWALAppendStats(path string) (int64, int64, error) {
	blob, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, nil
		}
		return 0, 0, err
	}

	var count int64
	var consumed int64
	var baselineTS Timestamp
	hasBaseline := false
	for pos := 0; pos < len(blob); {
		payloadLen, n := binary.Uvarint(blob[pos:])
		if n <= 0 {
			break
		}
		if payloadLen > uint64(len(blob)-pos-n) {
			break
		}

		payloadStart := pos + n
		payloadEnd := payloadStart + int(payloadLen)
		payload := blob[payloadStart:payloadEnd]
		if _, err := decodeWALPayloadCompactWithBaseline(payload, baselineTS, hasBaseline); err != nil {
			if errors.Is(err, ErrWALMissingBaseline) && !hasBaseline {
				pos = payloadEnd
				consumed = int64(pos)
				continue
			}
			break
		}
		if len(payload) > 5 && (payload[5]&walCompactNewBaseline) != 0 && len(payload) >= 14 {
			baselineTS = Timestamp(binary.LittleEndian.Uint64(payload[6:14]))
			hasBaseline = true
		}

		count++
		pos = payloadEnd
		consumed = int64(pos)
	}

	return count, consumed, nil
}

func decodeWALPayloadCompactWithBaseline(payload []byte, baselineTS Timestamp, hasBaseline bool) (WALRecord, error) {
	if len(payload) < 2+3+1+4 {
		return WALRecord{}, fmt.Errorf("wal compact payload too short")
	}

	pos := 0
	metricID := MetricID(binary.LittleEndian.Uint16(payload[pos : pos+2]))
	pos += 2

	// 3-byte TS delta
	tsDelta := Timestamp(payload[pos]) | Timestamp(payload[pos+1])<<8 | Timestamp(payload[pos+2])<<16
	pos += 3

	compactTL := payload[pos]
	pos++

	var ts Timestamp
	var vtype byte
	metricName := ""

	if (compactTL & walCompactNewBaseline) != 0 {
		if len(payload) < pos+8 {
			return WALRecord{}, fmt.Errorf("truncated baseline ts")
		}
		ts = Timestamp(binary.LittleEndian.Uint64(payload[pos : pos+8]))
		pos += 8
	} else {
		if !hasBaseline {
			return WALRecord{}, ErrWALMissingBaseline
		}
		// Reconstruct from baseline + delta
		ts = baselineTS + tsDelta
	}

	if (compactTL & walCompactNewMetric) != 0 {
		if len(payload) < pos+1 {
			return WALRecord{}, fmt.Errorf("truncated metric name length")
		}
		nameLen := payload[pos]
		pos++
		if len(payload) < pos+int(nameLen)+1 {
			return WALRecord{}, fmt.Errorf("truncated metric name")
		}
		metricName = string(payload[pos : pos+int(nameLen)])
		pos += int(nameLen)
		vtype = payload[pos]
		pos++
	} else {
		// ValueType omitted for known metrics; must be looked up from catalog.
		// For now, use a sentinel; caller will populate from catalog.
		vtype = 0 // sentinel: caller must look up
	}

	if len(payload) < pos+4 {
		return WALRecord{}, fmt.Errorf("truncated value")
	}
	var valueRaw [4]byte
	copy(valueRaw[:], payload[pos:pos+4])

	if vtype == 0 {
		// Sentinel: will be resolved during replay by looking up catalog
		vtype = Int32Sample // default; caller overrides
	}

	rec := WALRecord{SegmentID: 1, MetricID: metricID, MetricName: metricName, Timestamp: ts, ValueType: vtype}
	switch vtype {
	case Int32Sample:
		rec.Value = int32(binary.LittleEndian.Uint32(valueRaw[:]))
	case Float32Sample:
		rec.Value = math.Float32frombits(binary.LittleEndian.Uint32(valueRaw[:]))
	default:
		return WALRecord{}, fmt.Errorf("unsupported wal value type: %d", vtype)
	}
	return rec, nil
}
