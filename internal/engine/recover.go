package engine

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type DataFileRecoverReport struct {
	Database             string `json:"database"`
	Part                 string `json:"part"`
	SourcePath           string `json:"source_path"`
	OutputPath           string `json:"output_path"`
	SourceBytes          int64  `json:"source_bytes"`
	OutputBytes          int64  `json:"output_bytes"`
	RecoveredFrames      int    `json:"recovered_frames"`
	RecoveredRecords     int64  `json:"recovered_records"`
	SkippedBytes         int64  `json:"skipped_bytes"`
	RejectedFrames       int    `json:"rejected_frames"`
	FirstAcceptedOffset  int64  `json:"first_accepted_offset"`
	LastAcceptedOffset   int64  `json:"last_accepted_offset"`
	DurationMS           int64  `json:"duration_ms"`
}

type dataFileRecoverBlobReport struct {
	RecoveredFrames      int
	RecoveredRecords     int64
	SkippedBytes         int64
	RejectedFrames       int
	FirstAcceptedOffset  int64
	LastAcceptedOffset   int64
	lastAcceptedEnd      Timestamp
	hasAccepted          bool
}

func (e *Engine) RecoverDataFile(database, part, outputPath string) (DataFileRecoverReport, error) {
	report := DataFileRecoverReport{
		Database: strings.TrimSpace(database),
		Part:     strings.TrimSpace(part),
	}
	if report.Database == "" {
		return report, fmt.Errorf("database is required")
	}
	if report.Part == "" {
		return report, fmt.Errorf("part is required")
	}
	outputPath = strings.TrimSpace(outputPath)
	if outputPath == "" {
		return report, fmt.Errorf("output path is required")
	}

	started := time.Now()

	e.writeMu.Lock()
	defer e.writeMu.Unlock()

	db, rt, err := e.getOrCreateDB(report.Database)
	if err != nil {
		return report, err
	}
	if err := validatePartitionKeyForRuntime(rt, report.Part); err != nil {
		return report, err
	}
	if p := rt.openDays[report.Part]; p != nil {
		return report, fmt.Errorf("%w: %s/%s", ErrDataFileActive, report.Database, report.Part)
	}

	report.SourcePath = filepath.Join(db.RootDataDir, "data-"+report.Part+".dat")
	report.OutputPath, err = filepath.Abs(outputPath)
	if err != nil {
		return report, err
	}
	if report.OutputPath == report.SourcePath {
		return report, fmt.Errorf("output path must differ from source path")
	}

	blob, err := os.ReadFile(report.SourcePath)
	if err != nil {
		return report, err
	}
	report.SourceBytes = int64(len(blob))

	encoded, blobReport, err := recoverDataFileBlob(blob, func(p *Page, current dataFileRecoverBlobReport) bool {
		if p == nil || len(p.Times) == 0 {
			return false
		}
		if partitionKey(rt, p.Start) != report.Part || partitionKey(rt, p.End) != report.Part {
			return false
		}
		if current.hasAccepted && p.End <= current.lastAcceptedEnd {
			return false
		}
		return true
	})
	if err != nil {
		return report, err
	}
	if blobReport.RecoveredFrames == 0 {
		return report, fmt.Errorf("no valid pages recovered from %s", report.SourcePath)
	}

	if err := os.MkdirAll(filepath.Dir(report.OutputPath), 0755); err != nil {
		return report, err
	}
	if err := writeAtomicDataFile(report.OutputPath, encoded); err != nil {
		return report, err
	}

	st, err := os.Stat(report.OutputPath)
	if err != nil {
		return report, err
	}
	report.OutputBytes = st.Size()
	report.RecoveredFrames = blobReport.RecoveredFrames
	report.RecoveredRecords = blobReport.RecoveredRecords
	report.SkippedBytes = blobReport.SkippedBytes
	report.RejectedFrames = blobReport.RejectedFrames
	report.FirstAcceptedOffset = blobReport.FirstAcceptedOffset
	report.LastAcceptedOffset = blobReport.LastAcceptedOffset
	report.DurationMS = time.Since(started).Milliseconds()
	return report, nil
}

func recoverDataFileBlob(blob []byte, accept func(*Page, dataFileRecoverBlobReport) bool) ([]byte, dataFileRecoverBlobReport, error) {
	var output bytes.Buffer
	report := dataFileRecoverBlobReport{FirstAcceptedOffset: -1, LastAcceptedOffset: -1}

	for pos := 0; pos < len(blob); {
		reader := bytes.NewReader(blob[pos:])
		startLen := reader.Len()
		var decoded Page
		if err := decoded.DecodeFrom(reader); err != nil {
			report.SkippedBytes++
			pos++
			continue
		}
		if err := validateRecompactSourcePage(&decoded); err != nil {
			report.SkippedBytes++
			pos++
			continue
		}
		consumed := startLen - reader.Len()
		if consumed <= 0 {
			return nil, report, fmt.Errorf("invalid page decoding at offset %d", pos)
		}
		if accept != nil && !accept(&decoded, report) {
			report.RejectedFrames++
			pos += consumed
			continue
		}
		if _, err := output.Write(blob[pos : pos+consumed]); err != nil {
			return nil, report, err
		}
		report.RecoveredFrames++
		report.RecoveredRecords += int64(len(decoded.Times))
		if !report.hasAccepted {
			report.FirstAcceptedOffset = int64(pos)
		}
		report.LastAcceptedOffset = int64(pos)
		report.lastAcceptedEnd = decoded.End
		report.hasAccepted = true
		pos += consumed
	}

	return output.Bytes(), report, nil
}