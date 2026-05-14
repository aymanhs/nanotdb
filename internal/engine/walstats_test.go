package engine

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScanWALFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.wal")
	w, err := NewWAL(path, 0, WALFsyncPolicySegment)
	if err != nil {
		t.Fatalf("NewWAL failed: %v", err)
	}
	defer w.Close()

	if _, err := AppendSample[int32](w, 11, Timestamp(1001), int32(7)); err != nil {
		t.Fatalf("AppendSample 1 failed: %v", err)
	}
	if _, err := AppendSample[float32](w, 12, Timestamp(1002), float32(3.25)); err != nil {
		t.Fatalf("AppendSample 2 failed: %v", err)
	}

	stats, recs, err := ScanWALFile(path)
	if err != nil {
		t.Fatalf("ScanWALFile failed: %v", err)
	}
	if stats.Records != 2 || len(recs) != 2 {
		t.Fatalf("records mismatch stats=%d len=%d", stats.Records, len(recs))
	}
	if recs[0].MetricID != 11 || recs[1].MetricID != 12 {
		t.Fatalf("metric ids mismatch: got=%d,%d", recs[0].MetricID, recs[1].MetricID)
	}
	if recs[0].Offset != 0 {
		t.Fatalf("first offset mismatch: got=%d want=0", recs[0].Offset)
	}
	if recs[0].RecordBytes <= 0 || recs[1].RecordBytes <= 0 {
		t.Fatalf("invalid record bytes: %d, %d", recs[0].RecordBytes, recs[1].RecordBytes)
	}
	if stats.HasTail {
		t.Fatalf("unexpected tail detection")
	}
}

func TestScanWALFileTruncatedTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.wal")
	w, err := NewWAL(path, 0, WALFsyncPolicySegment)
	if err != nil {
		t.Fatalf("NewWAL failed: %v", err)
	}
	if _, err := AppendSample[int32](w, 1, Timestamp(10), int32(99)); err != nil {
		t.Fatalf("AppendSample failed: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		t.Fatalf("OpenFile append failed: %v", err)
	}
	if _, err := f.Write([]byte{0x81, 0x81}); err != nil {
		f.Close()
		t.Fatalf("append tail failed: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close append file failed: %v", err)
	}

	stats, recs, err := ScanWALFile(path)
	if err != nil {
		t.Fatalf("ScanWALFile failed: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("expected one good record before tail, got=%d", len(recs))
	}
	if !stats.HasTail {
		t.Fatalf("expected tail detection")
	}
	if stats.TailBytes != 2 {
		t.Fatalf("tail bytes mismatch: got=%d want=2", stats.TailBytes)
	}
}
