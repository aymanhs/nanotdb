package engine

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func writeTestFrame(t *testing.T, path string, start, end Timestamp, compressed []byte) {
	t.Helper()

	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create frame file: %v", err)
	}
	defer f.Close()

	var header [HeaderSize]byte
	binary.LittleEndian.PutUint64(header[0:8], uint64(start))
	binary.LittleEndian.PutUint64(header[8:16], uint64(end))
	binary.LittleEndian.PutUint16(header[16:18], 1)
	if _, err := f.Write(header[:]); err != nil {
		t.Fatalf("write header: %v", err)
	}

	var lenBuf [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(lenBuf[:], uint64(len(compressed)))
	if _, err := f.Write(lenBuf[:n]); err != nil {
		t.Fatalf("write length: %v", err)
	}

	if _, err := f.Write(compressed); err != nil {
		t.Fatalf("write compressed payload: %v", err)
	}

	// CRC bytes (ignored unless payload is decoded).
	if _, err := f.Write([]byte{0, 0, 0, 0}); err != nil {
		t.Fatalf("write crc: %v", err)
	}
}

func TestCollectMetricFromFile_SkipsOutOfRangeFrameBeforeDecode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data-2026-05-12.dat")
	// Deliberately invalid compressed payload.
	writeTestFrame(t, path, Timestamp(100), Timestamp(200), []byte{0xFF, 0x00, 0x01})

	entry := MetricEntry{MetricID: 1, ValueType: Int32Sample}
	count := 0
	err := collectMetricFromFile("db", "metric", entry, path, Timestamp(1000), Timestamp(2000), 1, &count, func(Sample) error {
		count++
		return nil
	})
	if err != nil {
		t.Fatalf("expected out-of-range frame skip without decode error, got: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 samples, got %d", count)
	}
}

func TestCollectMetricFromFile_InRangeFrameAttemptsDecode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data-2026-05-12.dat")
	// Deliberately invalid compressed payload.
	writeTestFrame(t, path, Timestamp(100), Timestamp(200), []byte{0xFF, 0x00, 0x01})

	entry := MetricEntry{MetricID: 1, ValueType: Int32Sample}
	count := 0
	err := collectMetricFromFile("db", "metric", entry, path, Timestamp(50), Timestamp(250), 1, &count, func(Sample) error {
		count++
		return nil
	})
	if err == nil {
		t.Fatal("expected decode error for in-range invalid frame")
	}
}
