package engine

import (
	"bytes"
	"testing"
	"time"
)

func TestRecoverDataFileBlobRecoversAcrossCorruption(t *testing.T) {
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

	corrupt := NewPageWithLimits(Timestamp(250), 8, 4096, time.Hour)
	if err := corrupt.AddSample(1, Timestamp(250), []byte{3, 0, 0, 0}); err != nil {
		t.Fatalf("corrupt.AddSample failed: %v", err)
	}
	if err := corrupt.EncodeInto(&source); err != nil {
		t.Fatalf("corrupt.EncodeInto failed: %v", err)
	}

	second := NewPageWithLimits(Timestamp(300), 8, 4096, time.Hour)
	if err := second.AddSample(1, Timestamp(300), []byte{4, 0, 0, 0}); err != nil {
		t.Fatalf("second.AddSample failed: %v", err)
	}
	if err := second.AddSample(1, Timestamp(400), []byte{5, 0, 0, 0}); err != nil {
		t.Fatalf("second.AddSample second failed: %v", err)
	}
	if err := second.EncodeInto(&source); err != nil {
		t.Fatalf("second.EncodeInto failed: %v", err)
	}

	blob := append([]byte(nil), source.Bytes()...)

	var firstEncoded bytes.Buffer
	if err := first.EncodeInto(&firstEncoded); err != nil {
		t.Fatalf("first EncodeInto failed: %v", err)
	}
	var corruptEncoded bytes.Buffer
	if err := corrupt.EncodeInto(&corruptEncoded); err != nil {
		t.Fatalf("corrupt EncodeInto failed: %v", err)
	}
	corruptStart := firstEncoded.Len()
	blob[corruptStart+corruptEncoded.Len()-1] ^= 0xff

	encoded, report, err := recoverDataFileBlob(blob, func(p *Page, current dataFileRecoverBlobReport) bool {
		if current.hasAccepted && p.End <= current.lastAcceptedEnd {
			return false
		}
		return true
	})
	if err != nil {
		t.Fatalf("recoverDataFileBlob failed: %v", err)
	}
	if report.RecoveredFrames != 2 {
		t.Fatalf("expected 2 recovered frames, got=%d", report.RecoveredFrames)
	}
	if report.RecoveredRecords != 4 {
		t.Fatalf("expected 4 recovered records, got=%d", report.RecoveredRecords)
	}
	if report.SkippedBytes == 0 {
		t.Fatal("expected skipped bytes while resynchronizing")
	}

	pos := 0
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
		starts = append(starts, page.Start)
	}
	if len(starts) != 2 || starts[0] != 100 || starts[1] != 300 {
		t.Fatalf("unexpected recovered page starts: %+v", starts)
	}
}

func TestRecoverDataFileBlobSkipsDuplicateEarlierPage(t *testing.T) {
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
	if err := first.EncodeInto(&source); err != nil {
		t.Fatalf("first duplicate EncodeInto failed: %v", err)
	}

	second := NewPageWithLimits(Timestamp(300), 8, 4096, time.Hour)
	if err := second.AddSample(1, Timestamp(300), []byte{3, 0, 0, 0}); err != nil {
		t.Fatalf("second.AddSample failed: %v", err)
	}
	if err := second.AddSample(1, Timestamp(400), []byte{4, 0, 0, 0}); err != nil {
		t.Fatalf("second.AddSample second failed: %v", err)
	}
	if err := second.EncodeInto(&source); err != nil {
		t.Fatalf("second.EncodeInto failed: %v", err)
	}

	_, report, err := recoverDataFileBlob(source.Bytes(), func(p *Page, current dataFileRecoverBlobReport) bool {
		if current.hasAccepted && p.End <= current.lastAcceptedEnd {
			return false
		}
		return true
	})
	if err != nil {
		t.Fatalf("recoverDataFileBlob failed: %v", err)
	}
	if report.RecoveredFrames != 2 {
		t.Fatalf("expected 2 recovered frames, got=%d", report.RecoveredFrames)
	}
	if report.RejectedFrames != 1 {
		t.Fatalf("expected 1 rejected duplicate frame, got=%d", report.RejectedFrames)
	}
}