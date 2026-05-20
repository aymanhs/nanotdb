package engine

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestWALAppendAndReadRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.wal")
	w, err := NewWAL(path, 0, WALFsyncPolicySegment)
	if err != nil {
		t.Fatalf("NewWAL failed: %v", err)
	}
	defer w.Close()

	// For compact format, use AppendSampleWithMetricName to include ValueType in WAL.
	// Known metrics (no name) omit ValueType and require catalog lookup for decoding.
	if _, err := AppendSampleWithMetricName(w, 7, "test.int", Timestamp(1001), int32(-42)); err != nil {
		t.Fatalf("AppendSampleWithMetricName int failed: %v", err)
	}
	if _, err := AppendSampleWithMetricName(w, 8, "test.float", Timestamp(1002), float32(3.5)); err != nil {
		t.Fatalf("AppendSampleWithMetricName float failed: %v", err)
	}

	recs, err := w.Records()
	if err != nil {
		t.Fatalf("Records failed: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("expected 2 records, got=%d", len(recs))
	}
	if recs[0].MetricID != 7 || recs[0].Timestamp != 1001 || recs[0].ValueType != Int32Sample {
		t.Fatalf("unexpected first record header: %+v", recs[0])
	}
	if recs[0].MetricName != "test.int" {
		t.Fatalf("expected metric name for new sample, got=%q", recs[0].MetricName)
	}
	v0, ok := recs[0].Value.(int32)
	if !ok || v0 != -42 {
		t.Fatalf("unexpected first record value: %#v", recs[0].Value)
	}
	if recs[1].MetricID != 8 || recs[1].Timestamp != 1002 || recs[1].ValueType != Float32Sample {
		t.Fatalf("unexpected second record header: %+v", recs[1])
	}
	v1, ok := recs[1].Value.(float32)
	if !ok || math.Abs(float64(v1-3.5)) > 0.0001 {
		t.Fatalf("unexpected second record value: %#v", recs[1].Value)
	}
}

func TestWALAppendSampleWithMetricNameRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.wal")
	w, err := NewWAL(path, 0, WALFsyncPolicySegment)
	if err != nil {
		t.Fatalf("NewWAL failed: %v", err)
	}
	defer w.Close()

	if _, err := AppendSampleWithMetricName(w, 7, "temp.office", Timestamp(1001), int32(25000)); err != nil {
		t.Fatalf("AppendSampleWithMetricName failed: %v", err)
	}

	recs, err := w.Records()
	if err != nil {
		t.Fatalf("Records failed: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("expected 1 record, got=%d", len(recs))
	}
	if recs[0].MetricName != "temp.office" {
		t.Fatalf("metric name mismatch: got=%q want=temp.office", recs[0].MetricName)
	}
}

func TestWALResetTruncatesAndReuses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.wal")
	w, err := NewWAL(path, 0, WALFsyncPolicySegment)
	if err != nil {
		t.Fatalf("NewWAL failed: %v", err)
	}
	defer w.Close()

	if _, err := AppendSample(w, 1, Timestamp(10), int32(11)); err != nil {
		t.Fatalf("AppendSample failed: %v", err)
	}
	recs, err := w.Records()
	if err != nil {
		t.Fatalf("Records failed: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("expected one record before reset, got=%d", len(recs))
	}

	if err := w.Reset(); err != nil {
		t.Fatalf("Reset failed: %v", err)
	}
	recs, err = w.Records()
	if err != nil {
		t.Fatalf("Records after reset failed: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("expected zero records after reset, got=%d", len(recs))
	}

	if _, err := AppendSample(w, 2, Timestamp(20), int32(22)); err != nil {
		t.Fatalf("AppendSample after reset failed: %v", err)
	}
	recs, err = w.Records()
	if err != nil {
		t.Fatalf("Records after reuse failed: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("expected one record after reuse, got=%d", len(recs))
	}
	if recs[0].MetricID != 2 {
		t.Fatalf("unexpected metric id after reuse: got=%d", recs[0].MetricID)
	}
}

func TestWALRecordsSkipPrefixWithoutBaseline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.wal")
	prefix := []byte{1, 0, 0, 0, 0, 0, 7, 0, 0, 0}
	var lenBuf [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(lenBuf[:], uint64(len(prefix)))
	if err := os.WriteFile(path, append(append([]byte{}, lenBuf[:n]...), prefix...), 0644); err != nil {
		t.Fatalf("write bad prefix failed: %v", err)
	}

	w, err := NewWAL(path, 0, WALFsyncPolicySegment)
	if err != nil {
		t.Fatalf("NewWAL failed: %v", err)
	}
	defer w.Close()

	if _, err := AppendSampleWithMetricName(w, 7, "temp.office", Timestamp(1001), int32(25000)); err != nil {
		t.Fatalf("AppendSampleWithMetricName failed: %v", err)
	}

	recs, err := w.Records()
	if err != nil {
		t.Fatalf("Records failed: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("expected only the valid record after skipping bad prefix, got=%d", len(recs))
	}
	if recs[0].Timestamp != 1001 {
		t.Fatalf("timestamp mismatch: got=%d want=1001", recs[0].Timestamp)
	}
}

func TestWALStatsTrackFlushMetrics(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.wal")
	w, err := NewWAL(path, 0, WALFsyncPolicySegment)
	if err != nil {
		t.Fatalf("NewWAL failed: %v", err)
	}
	defer w.Close()

	if _, err := AppendSample(w, 1, Timestamp(10), int32(11)); err != nil {
		t.Fatalf("AppendSample failed: %v", err)
	}
	before := w.Stats()
	if before.AppendCount != 1 {
		t.Fatalf("append count mismatch before reset: got=%d want=1", before.AppendCount)
	}
	if before.AppendBytes <= 0 {
		t.Fatalf("expected positive append bytes")
	}

	if err := w.Reset(); err != nil {
		t.Fatalf("Reset failed: %v", err)
	}
	after := w.Stats()
	if after.FlushCount != 1 {
		t.Fatalf("flush count mismatch: got=%d want=1", after.FlushCount)
	}
	if after.FlushedBytes <= 0 {
		t.Fatalf("expected positive flushed bytes")
	}
	if after.BufferBytes != 0 {
		t.Fatalf("expected buffer bytes to be zero after reset, got=%d", after.BufferBytes)
	}
	if after.MinFlushBytes != after.MaxFlushBytes {
		t.Fatalf("expected min/max flush bytes equal for single flush: min=%d max=%d", after.MinFlushBytes, after.MaxFlushBytes)
	}
	if after.ResetDurationTotal <= 0 {
		t.Fatalf("expected positive reset duration")
	}
	if after.FsyncDurationTotal <= 0 {
		t.Fatalf("expected positive fsync duration")
	}
	if len(after.RecentFlushes) != 1 {
		t.Fatalf("expected one recent flush event, got=%d", len(after.RecentFlushes))
	}
}

func TestWALFsyncPolicyAlwaysFsyncsEachAppend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.wal")
	w, err := NewWAL(path, 1024*1024, WALFsyncPolicyAlways)
	if err != nil {
		t.Fatalf("NewWAL failed: %v", err)
	}
	defer w.Close()

	if _, err := AppendSample(w, 1, Timestamp(10), int32(11)); err != nil {
		t.Fatalf("AppendSample failed: %v", err)
	}
	if _, err := AppendSample(w, 1, Timestamp(11), int32(12)); err != nil {
		t.Fatalf("AppendSample failed: %v", err)
	}

	stats := w.Stats()
	if stats.FsyncCount != 2 {
		t.Fatalf("fsync count mismatch: got=%d want=2", stats.FsyncCount)
	}
}
