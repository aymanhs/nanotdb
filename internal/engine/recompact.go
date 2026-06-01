package engine

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var ErrDataFileActive = errors.New("data file partition is currently open")

type DataFileRecompactReport struct {
	Database   string `json:"database"`
	Part       string `json:"part"`
	Path       string `json:"path"`
	OldFrames  int    `json:"old_frames"`
	NewFrames  int    `json:"new_frames"`
	OldRecords int64  `json:"old_records"`
	NewRecords int64  `json:"new_records"`
	OldBytes   int64  `json:"old_bytes"`
	NewBytes   int64  `json:"new_bytes"`
	DurationMS int64  `json:"duration_ms"`
}

type DataFileMetricCompactReport struct {
	Database    string `json:"database"`
	Part        string `json:"part"`
	DataPath    string `json:"data_path"`
	MetricPath  string `json:"metric_path"`
	DataBytes   int64  `json:"data_bytes"`
	MetricBytes int64  `json:"metric_bytes"`
	SavedBytes  int64  `json:"saved_bytes"`
	DurationMS  int64  `json:"duration_ms"`
}

func (e *Engine) RecompactDataFile(database, part string) (DataFileRecompactReport, error) {
	report := DataFileRecompactReport{
		Database: strings.TrimSpace(database),
		Part:     strings.TrimSpace(part),
	}
	if err := ValidateDatabaseName(report.Database); err != nil {
		return report, err
	}
	if report.Part == "" {
		return report, fmt.Errorf("part is required")
	}

	e.writeMu.Lock()
	defer e.writeMu.Unlock()

	started := time.Now()
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

	report.Path = filepath.Join(db.RootDataDir, "data-"+report.Part+".dat")
	oldStats, err := ScanDataFileStats(report.Path)
	if err != nil {
		return report, err
	}
	report.OldFrames = oldStats.Frames
	report.OldRecords = oldStats.TotalRecords
	report.OldBytes = oldStats.FileBytes

	blob, err := os.ReadFile(report.Path)
	if err != nil {
		return report, err
	}

	encoded, newFrames, err := recompactDataFileBlob(blob, rt.info.PageMaxRecords, rt.info.PageMaxBytes)
	if err != nil {
		return report, fmt.Errorf("recompact %s: %w", report.Path, err)
	}

	if err := writeAtomicDataFile(report.Path, encoded); err != nil {
		return report, err
	}

	newStats, err := ScanDataFileStats(report.Path)
	if err != nil {
		return report, err
	}
	report.NewFrames = newFrames
	report.NewRecords = newStats.TotalRecords
	report.NewBytes = newStats.FileBytes
	report.DurationMS = time.Since(started).Milliseconds()
	return report, nil
}

func (e *Engine) CompactDataFileToMetricV2(database, part string) (DataFileMetricCompactReport, error) {
	report := DataFileMetricCompactReport{
		Database: strings.TrimSpace(database),
		Part:     strings.TrimSpace(part),
	}
	if err := ValidateDatabaseName(report.Database); err != nil {
		return report, err
	}
	if report.Part == "" {
		return report, fmt.Errorf("part is required")
	}

	e.writeMu.Lock()
	defer e.writeMu.Unlock()

	started := time.Now()
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
	partitionKind, err := partitionModeToMetricPartitionKind(rt.info.Partition)
	if err != nil {
		return report, err
	}
	report.DataPath, err = resolveMetricRawPartitionPath(db.RootDataDir, report.Part)
	if err != nil {
		return report, err
	}
	dataStat, err := os.Stat(report.DataPath)
	if err != nil {
		return report, err
	}
	report.DataBytes = dataStat.Size()
	report.MetricPath, err = e.buildMetricFileForPartitionV2(db, partitionKind, report.Part)
	if err != nil {
		return report, err
	}
	metricStat, err := os.Stat(report.MetricPath)
	if err != nil {
		return report, err
	}
	report.MetricBytes = metricStat.Size()
	report.SavedBytes = report.DataBytes - report.MetricBytes
	report.DurationMS = time.Since(started).Milliseconds()
	return report, nil
}

func validatePartitionKeyForRuntime(rt *dbRuntime, part string) error {
	part = strings.TrimSpace(part)
	if part == "" {
		return fmt.Errorf("part is required")
	}
	if strings.Contains(part, "/") || strings.Contains(part, `\\`) {
		return fmt.Errorf("invalid partition key: %s", part)
	}
	if part == "forever" {
		if partitionKey(rt, 0) != "forever" {
			return fmt.Errorf("partition %q does not match database partitioning", part)
		}
		return nil
	}
	start, err := parsePartitionStart(part)
	if err != nil {
		return err
	}
	if want := partitionKey(rt, Timestamp(start.UnixNano())); want != part {
		return fmt.Errorf("partition %q does not match database partitioning (want %s)", part, want)
	}
	return nil
}

func recompactDataFileBlob(blob []byte, maxRecords, maxBytes int) ([]byte, int, error) {
	var output bytes.Buffer
	var page *Page
	frames := 0

	flushPage := func() error {
		if page == nil || len(page.Times) == 0 {
			return nil
		}
		if err := page.EncodeInto(&output); err != nil {
			return err
		}
		frames++
		page = nil
		return nil
	}
	appendSample := func(metricID MetricID, ts Timestamp, raw []byte) error {
		if page == nil {
			page = NewPageWithLimits(ts, maxRecords, maxBytes, time.Hour)
		}
		if err := page.AddSample(metricID, ts, raw); err != nil {
			if err != ErrOutOfOrderTimestamp {
				return err
			}
			if err := flushPage(); err != nil {
				return err
			}
			page = NewPageWithLimits(ts, maxRecords, maxBytes, time.Hour)
			if err := page.AddSample(metricID, ts, raw); err != nil {
				return err
			}
		}
		if len(page.Times) >= page.MaxRecords || len(page.Metrics)*2+len(page.Times)*8+page.Values.Len() >= page.MaxBytes {
			return flushPage()
		}
		return nil
	}

	for pos := 0; pos < len(blob); {
		reader := bytes.NewReader(blob[pos:])
		startLen := reader.Len()
		var decoded Page
		if err := decoded.DecodeFrom(reader); err != nil {
			return nil, 0, fmt.Errorf("decode page at offset %d: %w", pos, err)
		}
		if err := validateRecompactSourcePage(&decoded); err != nil {
			return nil, 0, fmt.Errorf("invalid source page at offset %d: %w", pos, err)
		}
		consumed := startLen - reader.Len()
		if consumed <= 0 {
			return nil, 0, fmt.Errorf("invalid page decoding at offset %d", pos)
		}
		pos += consumed

		if len(decoded.Metrics) != len(decoded.Times) {
			return nil, 0, fmt.Errorf("page corruption: metrics/times length mismatch")
		}
		values := decoded.Values.Bytes()
		if len(values) < len(decoded.Metrics)*4 {
			return nil, 0, fmt.Errorf("page corruption: values blob too short")
		}
		for i := 0; i < len(decoded.Metrics); i++ {
			off := i * 4
			if err := appendSample(decoded.Metrics[i], decoded.Times[i], values[off:off+4]); err != nil {
				return nil, 0, err
			}
		}
	}

	if err := flushPage(); err != nil {
		return nil, 0, err
	}
	return output.Bytes(), frames, nil
}

func validateRecompactSourcePage(p *Page) error {
	if p == nil {
		return fmt.Errorf("nil page")
	}
	if len(p.Metrics) != len(p.Times) {
		return fmt.Errorf("metrics/times length mismatch")
	}
	if len(p.Values.Bytes()) != len(p.Metrics)*4 {
		return fmt.Errorf("values length mismatch: got=%d want=%d", len(p.Values.Bytes()), len(p.Metrics)*4)
	}
	if len(p.Times) == 0 {
		return nil
	}
	if p.Start != p.Times[0] {
		return fmt.Errorf("page start/header mismatch: header=%d first=%d", p.Start, p.Times[0])
	}
	if p.End != p.Times[len(p.Times)-1] {
		return fmt.Errorf("page end/header mismatch: header=%d last=%d", p.End, p.Times[len(p.Times)-1])
	}
	for i := 1; i < len(p.Times); i++ {
		if p.Times[i] < p.Times[i-1] {
			return fmt.Errorf("timestamp rollback inside page at index %d", i)
		}
	}
	return nil
}

func writeAtomicDataFile(path string, payload []byte) error {
	return writeFileAtomicWithSuffix(path, payload, ".recompact.tmp")
}

// writeFileAtomic is the canonical atomic-replace primitive: write bytes to a
// temp file, fsync, close, rename over the target, fsync the parent directory.
// Used by every code path that updates a config/manifest/catalog file. The
// directory fsync is essential — without it, on ext3, ext4 with
// `data=writeback`, or XFS edge cases, a crash immediately after rename can
// leave the directory entry update unjournaled and the new file unreachable.
func writeFileAtomic(path string, payload []byte) error {
	return writeFileAtomicWithSuffix(path, payload, ".tmp")
}

func writeFileAtomicWithSuffix(path string, payload []byte, suffix string) error {
	tmpPath := path + suffix
	f, err := os.OpenFile(tmpPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	if _, err := f.Write(payload); err != nil {
		f.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return syncParentDir(path)
}

func syncParentDir(path string) error {
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
