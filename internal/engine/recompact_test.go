package engine

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRecompactDataFileRewritesSealedPartition(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "prod"), 0755); err != nil {
		t.Fatalf("MkdirAll prod failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "prod", "manifest.toml"), []byte("[page]\nmax_records = 1\nmax_bytes = 64\nmax_age = \"10m\"\n"), 0644); err != nil {
		t.Fatalf("WriteFile manifest failed: %v", err)
	}

	e, err := OpenEngine(root, 0)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}

	base := time.Date(2024, time.January, 2, 12, 0, 0, 0, time.UTC)
	ts1 := base.UnixNano()
	ts2 := base.Add(5 * time.Minute).UnixNano()
	part := dayKey(Timestamp(ts1))
	path := filepath.Join(root, "prod", "data-"+part+".dat")

	if err := e.AddLine("prod/temp.out_dry 11 " + itoa64(ts1)); err != nil {
		t.Fatalf("AddLine first failed: %v", err)
	}
	if err := e.AddLine("prod/temp.out_dry 13 " + itoa64(ts2)); err != nil {
		t.Fatalf("AddLine second failed: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	oldStats, err := ScanDataFileStats(path)
	if err != nil {
		t.Fatalf("ScanDataFileStats old failed: %v", err)
	}
	if oldStats.Frames < 2 {
		t.Fatalf("expected at least 2 frames before recompact, got=%d", oldStats.Frames)
	}

	if err := os.WriteFile(filepath.Join(root, "prod", "manifest.toml"), []byte("[page]\nmax_records = 128\nmax_bytes = 4096\nmax_age = \"10m\"\n"), 0644); err != nil {
		t.Fatalf("WriteFile manifest update failed: %v", err)
	}

	e, err = OpenEngine(root, 0)
	if err != nil {
		t.Fatalf("OpenEngine reopen failed: %v", err)
	}
	defer e.Close()

	report, err := e.RecompactDataFile("prod", part)
	if err != nil {
		t.Fatalf("RecompactDataFile failed: %v", err)
	}
	if report.OldFrames != oldStats.Frames {
		t.Fatalf("old frame count mismatch: got=%d want=%d", report.OldFrames, oldStats.Frames)
	}
	if report.NewFrames >= report.OldFrames {
		t.Fatalf("expected fewer frames after recompact, got old=%d new=%d", report.OldFrames, report.NewFrames)
	}
	if report.NewRecords != report.OldRecords || report.NewRecords != 2 {
		t.Fatalf("record count mismatch: old=%d new=%d", report.OldRecords, report.NewRecords)
	}
	if _, err := os.Stat(path + ".recompact.tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected temporary file to be removed, stat err=%v", err)
	}

	var values []float32
	err = e.QueryRange("prod", "temp.out_dry", Timestamp(ts1), Timestamp(ts2), 1, func(s Sample) error {
		values = append(values, s.Float32)
		return nil
	})
	if err != nil {
		t.Fatalf("QueryRange failed: %v", err)
	}
	if len(values) != 2 || values[0] != 11 || values[1] != 13 {
		t.Fatalf("unexpected values after recompact: %+v", values)
	}

	newStats, err := ScanDataFileStats(path)
	if err != nil {
		t.Fatalf("ScanDataFileStats new failed: %v", err)
	}
	if newStats.Frames != report.NewFrames {
		t.Fatalf("new frame count mismatch: report=%d scan=%d", report.NewFrames, newStats.Frames)
	}
	if newStats.TotalRecords != 2 {
		t.Fatalf("new record count mismatch: got=%d want=2", newStats.TotalRecords)
	}
}

func TestRecompactDataFileRejectsActivePartition(t *testing.T) {
	e, err := OpenEngine(t.TempDir(), 0)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer e.Close()

	ts := Timestamp(time.Now().UTC().UnixNano())
	part := dayKey(ts)
	if err := e.AddLine("prod/temp.out_dry 11 " + itoa64(int64(ts))); err != nil {
		t.Fatalf("AddLine failed: %v", err)
	}

	_, err = e.RecompactDataFile("prod", part)
	if !errors.Is(err, ErrDataFileActive) {
		t.Fatalf("expected ErrDataFileActive, got=%v", err)
	}
}

func TestRecompactDataFileBlobSplitsOnTimestampRollback(t *testing.T) {
	var source bytes.Buffer

	first := NewPageWithLimits(Timestamp(100), 8, 4096, time.Hour)
	if err := first.AddSample(1, Timestamp(100), []byte{1, 0, 0, 0}); err != nil {
		t.Fatalf("first.AddSample failed: %v", err)
	}
	if err := first.AddSample(1, Timestamp(200), []byte{2, 0, 0, 0}); err != nil {
		t.Fatalf("first.AddSample second failed: %v", err)
	}
	if err := first.EncodeInto(&source); err != nil {
		t.Fatalf("first.EncodeInto failed: %v", err)
	}

	second := NewPageWithLimits(Timestamp(150), 8, 4096, time.Hour)
	if err := second.AddSample(1, Timestamp(150), []byte{3, 0, 0, 0}); err != nil {
		t.Fatalf("second.AddSample failed: %v", err)
	}
	if err := second.AddSample(1, Timestamp(250), []byte{4, 0, 0, 0}); err != nil {
		t.Fatalf("second.AddSample second failed: %v", err)
	}
	if err := second.EncodeInto(&source); err != nil {
		t.Fatalf("second.EncodeInto failed: %v", err)
	}

	encoded, frames, err := recompactDataFileBlob(source.Bytes(), 64, 4096)
	if err != nil {
		t.Fatalf("recompactDataFileBlob failed: %v", err)
	}
	if frames != 2 {
		t.Fatalf("expected 2 frames after rollback split, got=%d", frames)
	}

	pos := 0
	records := 0
	starts := make([]Timestamp, 0, 2)
	for pos < len(encoded) {
		reader := bytes.NewReader(encoded[pos:])
		startLen := reader.Len()
		var page Page
		if err := page.DecodeFrom(reader); err != nil {
			t.Fatalf("DecodeFrom failed at offset %d: %v", pos, err)
		}
		consumed := startLen - reader.Len()
		if consumed <= 0 {
			t.Fatalf("invalid consumed size at offset %d", pos)
		}
		pos += consumed
		records += len(page.Times)
		starts = append(starts, page.Start)
	}
	if records != 4 {
		t.Fatalf("record count mismatch: got=%d want=4", records)
	}
	if len(starts) != 2 || starts[0] != 100 || starts[1] != 150 {
		t.Fatalf("unexpected page starts after recompact: %+v", starts)
	}
}

func TestRecompactDataFileBlobRejectsCorruptSourceHeader(t *testing.T) {
	var source bytes.Buffer
	page := NewPageWithLimits(Timestamp(100), 8, 4096, time.Hour)
	if err := page.AddSample(1, Timestamp(100), []byte{1, 0, 0, 0}); err != nil {
		t.Fatalf("AddSample first failed: %v", err)
	}
	if err := page.AddSample(1, Timestamp(200), []byte{2, 0, 0, 0}); err != nil {
		t.Fatalf("AddSample second failed: %v", err)
	}
	if err := page.EncodeInto(&source); err != nil {
		t.Fatalf("EncodeInto failed: %v", err)
	}

	blob := append([]byte(nil), source.Bytes()...)
	for i := 0; i < 8; i++ {
		blob[i] = 0
	}

	_, _, err := recompactDataFileBlob(blob, 64, 4096)
	if err == nil {
		t.Fatal("expected invalid source page error")
	}
	if !strings.Contains(err.Error(), "invalid source page") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRecompactDataFileBlobRejectsChecksumMismatch(t *testing.T) {
	var source bytes.Buffer
	page := NewPageWithLimits(Timestamp(100), 8, 4096, time.Hour)
	if err := page.AddSample(1, Timestamp(100), []byte{1, 0, 0, 0}); err != nil {
		t.Fatalf("AddSample first failed: %v", err)
	}
	if err := page.AddSample(1, Timestamp(200), []byte{2, 0, 0, 0}); err != nil {
		t.Fatalf("AddSample second failed: %v", err)
	}
	if err := page.EncodeInto(&source); err != nil {
		t.Fatalf("EncodeInto failed: %v", err)
	}

	blob := append([]byte(nil), source.Bytes()...)
	blob[len(blob)-1] ^= 0xff

	_, _, err := recompactDataFileBlob(blob, 64, 4096)
	if err == nil {
		t.Fatal("expected checksum mismatch error")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("unexpected error: %v", err)
	}
}
