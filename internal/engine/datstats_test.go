package engine

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestScanDataFileHeaders(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data-2026-05-11.dat")

	p1 := NewPage(100)
	if err := p1.AddSample(1, 100, encodeInt32(10)); err != nil {
		t.Fatalf("AddSample p1 failed: %v", err)
	}
	if err := p1.AddSample(1, 101, encodeInt32(11)); err != nil {
		t.Fatalf("AddSample p1 failed: %v", err)
	}

	p2 := NewPage(200)
	if err := p2.AddSample(2, 200, encodeInt32(20)); err != nil {
		t.Fatalf("AddSample p2 failed: %v", err)
	}

	var bb bytes.Buffer
	if err := p1.EncodeInto(&bb); err != nil {
		t.Fatalf("EncodeInto p1 failed: %v", err)
	}
	if err := p2.EncodeInto(&bb); err != nil {
		t.Fatalf("EncodeInto p2 failed: %v", err)
	}
	if err := os.WriteFile(path, bb.Bytes(), 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	stats, frames, err := ScanDataFileHeaders(path)
	if err != nil {
		t.Fatalf("ScanDataFileHeaders failed: %v", err)
	}
	if stats.Frames != 2 {
		t.Fatalf("frames mismatch: got=%d want=2", stats.Frames)
	}
	if stats.TotalRecords != 3 {
		t.Fatalf("records mismatch: got=%d want=3", stats.TotalRecords)
	}
	if stats.MinStart != 100 || stats.MaxEnd != 200 {
		t.Fatalf("range mismatch: got=(%d,%d) want=(100,200)", stats.MinStart, stats.MaxEnd)
	}
	if len(frames) != 2 {
		t.Fatalf("frame header count mismatch: got=%d want=2", len(frames))
	}
	if frames[0].Offset != 0 {
		t.Fatalf("first frame offset mismatch: got=%d want=0", frames[0].Offset)
	}
	if frames[0].NumRecords != 2 {
		t.Fatalf("first frame records mismatch: got=%d want=2", frames[0].NumRecords)
	}
	if frames[1].NumRecords != 1 {
		t.Fatalf("second frame records mismatch: got=%d want=1", frames[1].NumRecords)
	}
}

func TestScanDataFileHeadersTruncated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data-2026-05-11.dat")
	if err := os.WriteFile(path, []byte{1, 2, 3, 4, 5}, 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	_, _, err := ScanDataFileHeaders(path)
	if err == nil {
		t.Fatalf("expected truncated header error")
	}
}
