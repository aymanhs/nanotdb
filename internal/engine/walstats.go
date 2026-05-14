package engine

import (
	"encoding/binary"
	"fmt"
	"os"
)

// WALWalkRecord is one decoded WAL record plus its raw encoded bytes.
type WALWalkRecord struct {
	Index       int
	Offset      int64
	RecordBytes int64
	MetricID    MetricID
	Timestamp   Timestamp
	ValueType   byte
	Value       interface{}
	Raw         []byte
}

// WALFileStats summarizes a WAL file walk.
type WALFileStats struct {
	Path         string
	FileBytes    int64
	Records      int
	DecodedBytes int64
	MinTS        Timestamp
	MaxTS        Timestamp
	HasTail      bool
	TailBytes    int64
	StopOffset   int64
	StopReason   string
}

type WALRecordCallback func(WALWalkRecord) error

// WalkWALFile walks WAL records and invokes fn for each successfully decoded record.
// It stops at first invalid/truncated tail and returns tail metadata in stats.
func WalkWALFile(path string, fn WALRecordCallback) (WALFileStats, error) {
	st, err := os.Stat(path)
	if err != nil {
		return WALFileStats{}, err
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		return WALFileStats{}, err
	}

	stats := WALFileStats{Path: path, FileBytes: st.Size()}

	var baselineTS Timestamp
	for pos := 0; pos < len(blob); {
		start := pos
		payloadLen, n := binary.Uvarint(blob[pos:])
		if n <= 0 {
			stats.HasTail = true
			stats.TailBytes = int64(len(blob) - start)
			stats.StopOffset = int64(start)
			stats.StopReason = "invalid length varint"
			break
		}
		pos += n
		if payloadLen > uint64(len(blob)-pos) {
			stats.HasTail = true
			stats.TailBytes = int64(len(blob) - start)
			stats.StopOffset = int64(start)
			stats.StopReason = "truncated payload"
			break
		}

		payload := blob[pos : pos+int(payloadLen)]
		pos += int(payloadLen)

		rec, err := decodeWALPayloadCompactWithBaseline(payload, baselineTS)
		if err != nil {
			stats.HasTail = true
			stats.TailBytes = int64(len(blob) - start)
			stats.StopOffset = int64(start)
			stats.StopReason = fmt.Sprintf("decode error: %v", err)
			break
		}
		if len(payload) > 5 && (payload[5]&walCompactNewBaseline) != 0 && len(payload) >= 14 {
			baselineTS = Timestamp(binary.LittleEndian.Uint64(payload[6:14]))
		}

		raw := make([]byte, pos-start)
		copy(raw, blob[start:pos])
		walkRec := WALWalkRecord{
			Index:       stats.Records,
			Offset:      int64(start),
			RecordBytes: int64(pos - start),
			MetricID:    rec.MetricID,
			Timestamp:   rec.Timestamp,
			ValueType:   rec.ValueType,
			Value:       rec.Value,
			Raw:         raw,
		}
		if fn != nil {
			if err := fn(walkRec); err != nil {
				return stats, err
			}
		}

		stats.Records++
		stats.DecodedBytes += int64(pos - start)
		if stats.Records == 1 || rec.Timestamp < stats.MinTS {
			stats.MinTS = rec.Timestamp
		}
		if stats.Records == 1 || rec.Timestamp > stats.MaxTS {
			stats.MaxTS = rec.Timestamp
		}
	}

	return stats, nil
}

// ScanWALFile walks WAL records from the beginning and stops at the first invalid or truncated tail.
// It returns all successfully decoded records before the stop point.
func ScanWALFile(path string) (WALFileStats, []WALWalkRecord, error) {
	records := make([]WALWalkRecord, 0, 64)
	stats, err := WalkWALFile(path, func(rec WALWalkRecord) error {
		records = append(records, rec)
		return nil
	})
	if err != nil {
		return WALFileStats{}, nil, err
	}
	return stats, records, nil
}
