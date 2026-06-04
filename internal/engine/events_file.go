package engine

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// EventsFrameHeader summarizes one frame parsed from an events-*.dat file
// without decompressing its payload. Used by inspect tooling and by the
// query planner to skip frames whose time range or event-id bitmap
// doesn't intersect the query.
type EventsFrameHeader struct {
	Index         int
	Offset        int64
	StartTime     Timestamp
	EndTime       Timestamp
	RecordCount   uint32
	CompressedLen uint32
	FrameBytes    int64
	EventIDBitmap [EventsPageEventIDBitmapBytes]byte
}

// HasEventID reports whether the given EventID's bit is set in the
// frame's bitmap. Mirrors EventsPageHeader.HasEventID.
func (h *EventsFrameHeader) HasEventID(id EventID) bool {
	if id == 0 || uint16(id) > MaxEventsPerDatabase {
		return false
	}
	return h.EventIDBitmap[id/8]&(1<<(id%8)) != 0
}

// IntersectsAny reports whether any id in the supplied set is present.
func (h *EventsFrameHeader) IntersectsAny(ids []EventID) bool {
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

// EventsFileStats summarizes one events-<partition>.dat file. Mirrors
// DataFileStats for symmetry with the metric inspect path.
type EventsFileStats struct {
	Path            string
	FileBytes       int64
	Frames          int
	TotalRecords    int64
	TotalCompressed int64
	TotalFrameBytes int64
	MinStart        Timestamp
	MaxEnd          Timestamp
}

// EventsFrameCallback is invoked once per parsed frame header during a
// walk. Returning a non-nil error terminates the walk.
type EventsFrameCallback func(EventsFrameHeader) error

// WalkEventsFileHeaders walks frame headers (no payload decompression)
// in an events-*.dat file and invokes fn for each one. Mirrors
// WalkDataFileHeaders. Useful for query planners that want to skip
// non-matching frames using only the bitmap + time range.
func WalkEventsFileHeaders(path string, fn EventsFrameCallback) (EventsFileStats, error) {
	st, err := os.Stat(path)
	if err != nil {
		return EventsFileStats{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		return EventsFileStats{}, err
	}
	defer f.Close()

	stats := EventsFileStats{Path: path, FileBytes: st.Size()}
	r := bufio.NewReaderSize(f, 64*1024)

	var offset int64
	for {
		frameOffset := offset

		// Read fixed header.
		var hdrBuf [EventsFrameHeaderBytes]byte
		nRead, err := io.ReadFull(r, hdrBuf[:])
		if err == io.EOF && nRead == 0 {
			break
		}
		if err == io.ErrUnexpectedEOF {
			return stats, fmt.Errorf("events file: truncated frame header at offset %d", frameOffset)
		}
		if err != nil {
			return stats, fmt.Errorf("events file: read frame header at offset %d: %w", frameOffset, err)
		}
		offset += EventsFrameHeaderBytes

		startTS := Timestamp(binary.LittleEndian.Uint64(hdrBuf[0:8]))
		endTS := Timestamp(binary.LittleEndian.Uint64(hdrBuf[8:16]))
		recordCount := binary.LittleEndian.Uint32(hdrBuf[16:20])
		compressedLen := binary.LittleEndian.Uint32(hdrBuf[20+EventsPageEventIDBitmapBytes:])

		if compressedLen > MaxOnDiskFramePayloadBytes {
			return stats, fmt.Errorf("events file: compressed payload %d exceeds cap %d at offset %d", compressedLen, MaxOnDiskFramePayloadBytes, frameOffset)
		}

		// Skip the compressed payload and CRC trailer. We do not
		// validate the CRC here — inspect-summary cost would balloon.
		// Verbose / decode paths call CollectEventsFrame instead.
		if _, err := r.Discard(int(compressedLen) + 4); err != nil {
			return stats, fmt.Errorf("events file: skip payload at offset %d: %w", frameOffset, err)
		}
		offset += int64(compressedLen) + 4

		hdr := EventsFrameHeader{
			Index:         stats.Frames,
			Offset:        frameOffset,
			StartTime:     startTS,
			EndTime:       endTS,
			RecordCount:   recordCount,
			CompressedLen: compressedLen,
			FrameBytes:    EventsFrameHeaderBytes + int64(compressedLen) + 4,
		}
		copy(hdr.EventIDBitmap[:], hdrBuf[20:20+EventsPageEventIDBitmapBytes])

		if fn != nil {
			if err := fn(hdr); err != nil {
				return stats, err
			}
		}

		stats.Frames++
		stats.TotalRecords += int64(recordCount)
		stats.TotalCompressed += int64(compressedLen)
		stats.TotalFrameBytes += hdr.FrameBytes
		if stats.Frames == 1 || startTS < stats.MinStart {
			stats.MinStart = startTS
		}
		if stats.Frames == 1 || endTS > stats.MaxEnd {
			stats.MaxEnd = endTS
		}
	}

	return stats, nil
}

// ScanEventsFileStats returns just the summary numbers for an events
// file. Mirrors ScanDataFileStats.
func ScanEventsFileStats(path string) (EventsFileStats, error) {
	return WalkEventsFileHeaders(path, nil)
}

// CollectEventsFrame reads, validates, and fully decodes a single frame
// at the given offset, returning a populated EventsPage. The catalog is
// required (the on-disk records omit value-type and rely on catalog
// lookup). Used by the query path and by verbose inspect.
func CollectEventsFrame(path string, offset int64, cat *EventCatalog) (*EventsPage, error) {
	if cat == nil {
		return nil, fmt.Errorf("events file: catalog required to decode frame")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}

	hdr, err := decodeEventsFrameHeader(f)
	if err != nil {
		return nil, fmt.Errorf("events file: read frame header at offset %d: %w", offset, err)
	}
	if hdr.CompressedLen > MaxOnDiskFramePayloadBytes {
		return nil, fmt.Errorf("events file: compressed payload %d at offset %d exceeds cap %d", hdr.CompressedLen, offset, MaxOnDiskFramePayloadBytes)
	}

	compressed := make([]byte, hdr.CompressedLen)
	if _, err := io.ReadFull(f, compressed); err != nil {
		return nil, fmt.Errorf("events file: read payload at offset %d: %w", offset, err)
	}
	var crcBuf [4]byte
	if _, err := io.ReadFull(f, crcBuf[:]); err != nil {
		return nil, fmt.Errorf("events file: read crc at offset %d: %w", offset, err)
	}
	expectedCRC := binary.LittleEndian.Uint32(crcBuf[:])

	page := NewEventsPage(0)
	if err := page.DecodeFromFrame(hdr, compressed, expectedCRC, cat); err != nil {
		return nil, fmt.Errorf("events file: decode frame at offset %d: %w", offset, err)
	}
	return page, nil
}

// EventsPageFlushStats describes what was written to disk for one
// frame append. Mirrors pageFlushStats but is exported because the
// events-side caller chain is shallower.
type EventsPageFlushStats struct {
	FrameBytes      int64
	CompressedBytes int64
	SyncDuration    time.Duration
}

var eventsFileWriteBufferPool = sync.Pool{
	New: func() any {
		return bytes.NewBuffer(make([]byte, 0, 32*1024))
	},
}

// AppendEventsPageFrame seals one EventsPage into a frame and appends
// it to events-<partition>.dat at the given root. The file is opened
// with O_APPEND so concurrent-safe append is OS-level. syncDataFile,
// when true, fsyncs the file before returning (controlled by the
// engine's durability profile).
//
// The page must not be modified after this call returns; the engine
// layer is responsible for swapping in a new page first.
func AppendEventsPageFrame(rootDir, partition string, page *EventsPage, syncDataFile bool) (EventsPageFlushStats, error) {
	if page == nil || page.Count() == 0 {
		return EventsPageFlushStats{}, fmt.Errorf("events file: cannot append empty page")
	}
	if rootDir == "" {
		return EventsPageFlushStats{}, fmt.Errorf("events file: rootDir cannot be empty")
	}
	if partition == "" {
		return EventsPageFlushStats{}, fmt.Errorf("events file: partition cannot be empty")
	}

	bb := eventsFileWriteBufferPool.Get().(*bytes.Buffer)
	bb.Reset()
	defer eventsFileWriteBufferPool.Put(bb)

	if err := page.EncodeInto(bb); err != nil {
		return EventsPageFlushStats{}, err
	}
	encoded := bb.Bytes()

	compressedLen := binary.LittleEndian.Uint32(encoded[20+EventsPageEventIDBitmapBytes : 20+EventsPageEventIDBitmapBytes+4])
	st := EventsPageFlushStats{
		FrameBytes:      int64(len(encoded)),
		CompressedBytes: int64(compressedLen),
	}

	path := filepath.Join(rootDir, "events-"+partition+".dat")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return EventsPageFlushStats{}, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0644)
	if err != nil {
		return EventsPageFlushStats{}, err
	}
	defer f.Close()

	if _, err := f.Write(encoded); err != nil {
		return EventsPageFlushStats{}, err
	}
	if syncDataFile {
		syncStart := time.Now()
		if err := f.Sync(); err != nil {
			return EventsPageFlushStats{}, err
		}
		st.SyncDuration = time.Since(syncStart)
	}
	return st, nil
}

// EventsFilePath returns the canonical path for an events file given
// the database root and the partition string. Centralized so callers
// don't recompute the convention.
func EventsFilePath(rootDir, partition string) string {
	return filepath.Join(rootDir, "events-"+partition+".dat")
}

// VerifyEventsFrame reads, CRC-checks, and fully decodes the frame at
// the given offset, then discards the decoded page. Used by verbose
// inspect to validate file integrity. Returns the decoded record count
// on success.
func VerifyEventsFrame(path string, offset int64, cat *EventCatalog) (uint32, error) {
	page, err := CollectEventsFrame(path, offset, cat)
	if err != nil {
		return 0, err
	}
	return uint32(page.Count()), nil
}
