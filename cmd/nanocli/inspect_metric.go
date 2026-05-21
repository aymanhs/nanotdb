package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/aymanhs/nanotdb/internal/engine"
)

type inspectMetricFrameReport struct {
	Index           int    `json:"index"`
	MetricID        uint16 `json:"metric_id"`
	ValueType       byte   `json:"value_type"`
	PointCount      uint32 `json:"point_count"`
	PayloadLen      uint32 `json:"payload_len"`
	UncompressedLen uint32 `json:"uncompressed_len"`
	StartTS         int64  `json:"start_timestamp_ns"`
	EndTS           int64  `json:"end_timestamp_ns"`
	StartUTC        string `json:"start_utc"`
	EndUTC          string `json:"end_utc"`
}

type inspectMetricFileReport struct {
	Path            string                     `json:"path"`
	Bytes           int64                      `json:"bytes"`
	Frames          int                        `json:"frames"`
	DistinctMetrics int                        `json:"distinct_metrics"`
	Points          int64                      `json:"points"`
	AvgPayloadBytes int64                      `json:"avg_payload_bytes,omitempty"`
	MinTimestamp    int64                      `json:"min_timestamp_ns,omitempty"`
	MaxTimestamp    int64                      `json:"max_timestamp_ns,omitempty"`
	MinUTC          string                     `json:"min_utc,omitempty"`
	MaxUTC          string                     `json:"max_utc,omitempty"`
	FramesDetail    []inspectMetricFrameReport `json:"frames_detail,omitempty"`
	ScanError       string                     `json:"scan_error,omitempty"`
}

type inspectMetricReport struct {
	RootDir      string                    `json:"root_dir"`
	Database     string                    `json:"database"`
	DatabaseDir  string                    `json:"database_dir"`
	Files        []inspectMetricFileReport `json:"files"`
	FileCount    int                       `json:"file_count"`
	TotalBytes   int64                     `json:"total_bytes"`
	TotalFrames  int64                     `json:"total_frames"`
	TotalMetrics int64                     `json:"total_metrics"`
	TotalPoints  int64                     `json:"total_points"`
	HasErrors    bool                      `json:"has_errors"`
	GeneratedAt  string                    `json:"generated_at"`
}

func runInspectMetric(args []string) error {
	fs := flag.NewFlagSet("inspect metric", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	rootDir := fs.String("root", "", "root data directory that contains engine.toml")
	dbName := fs.String("db", "", "database name")
	verbose := fs.Bool("verbose", false, "include per-frame stats")
	jsonOut := fs.Bool("json", false, "emit JSON output")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("usage: nanocli inspect metric --root <root-dir> --db <database> [--verbose] [--json]")
	}

	ctx, err := resolveDBContext(*rootDir, *dbName)
	if err != nil {
		return err
	}
	if _, err := os.Stat(ctx.DatabaseDir); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("database directory not found: %s", ctx.DatabaseDir)
		}
		return err
	}

	report, err := buildInspectMetricReport(ctx, *verbose)
	if err != nil {
		return err
	}

	ow := outputWriter{w: os.Stdout, asJSON: *jsonOut}
	return ow.emit(report, func(w io.Writer) {
		fmt.Fprintf(w, "Metric files for %s\n", report.Database)
		fmt.Fprintf(w, "Root: %s\n", report.RootDir)
		fmt.Fprintf(w, "DB dir: %s\n", report.DatabaseDir)
		fmt.Fprintf(w, "summary: files=%d bytes=%d frames=%d distinct_metrics=%d points=%d\n", report.FileCount, report.TotalBytes, report.TotalFrames, report.TotalMetrics, report.TotalPoints)
		if report.FileCount == 0 {
			fmt.Fprintf(w, "no metric files discovered\n")
			return
		}

		rows := make([][]string, 0, len(report.Files))
		for _, f := range report.Files {
			displayPath := shortenTablePath(report.DatabaseDir, f.Path)
			if f.ScanError != "" {
				rows = append(rows, []string{displayPath, "ERR", "ERR", "ERR", "ERR", "-", f.ScanError})
				continue
			}
			start := "-"
			duration := "-"
			if f.Frames > 0 {
				start = f.MinUTC
				duration = formatDurationNS(f.MinTimestamp, f.MaxTimestamp)
			}
			rows = append(rows, []string{
				displayPath,
				fmt.Sprintf("%d", f.Bytes),
				fmt.Sprintf("%d", f.Frames),
				fmt.Sprintf("%d", f.DistinctMetrics),
				fmt.Sprintf("%d", f.Points),
				fmt.Sprintf("%d", f.AvgPayloadBytes),
				start,
				duration,
			})
		}
		printAlignedTable(w,
			[]string{"file", "bytes", "frames", "metrics", "points", "avg_payload", "start", "duration"},
			rows,
			map[int]bool{1: true, 2: true, 3: true, 4: true, 5: true},
		)

		if !*verbose {
			return
		}

		for _, f := range report.Files {
			if f.ScanError != "" || len(f.FramesDetail) == 0 {
				continue
			}
			fmt.Fprintf(w, "\nFrames: %s\n", shortenTablePath(report.DatabaseDir, f.Path))
			rows := make([][]string, 0, len(f.FramesDetail))
			for _, frame := range f.FramesDetail {
				rows = append(rows, []string{
					fmt.Sprintf("%d", frame.Index),
					fmt.Sprintf("%d", frame.MetricID),
					fmt.Sprintf("%d", frame.ValueType),
					fmt.Sprintf("%d", frame.PointCount),
					fmt.Sprintf("%d", frame.PayloadLen),
					fmt.Sprintf("%d", frame.UncompressedLen),
					frame.StartUTC,
					formatDurationNS(frame.StartTS, frame.EndTS),
				})
			}
			printAlignedTable(w,
				[]string{"idx", "metric", "type", "points", "payload", "decoded", "start", "duration"},
				rows,
				map[int]bool{0: true, 1: true, 2: true, 3: true, 4: true, 5: true},
			)
		}
	})
}

func buildInspectMetricReport(ctx dbContext, verbose bool) (inspectMetricReport, error) {
	report := inspectMetricReport{
		RootDir:     ctx.RootDir,
		Database:    ctx.Database,
		DatabaseDir: ctx.DatabaseDir,
		Files:       make([]inspectMetricFileReport, 0, len(ctx.MetricFilePaths)),
		GeneratedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}

	for _, path := range ctx.MetricFilePaths {
		fileReport := inspectMetricFileReport{Path: path}
		st, err := os.Stat(path)
		if err != nil {
			fileReport.ScanError = err.Error()
			report.HasErrors = true
			report.Files = append(report.Files, fileReport)
			continue
		}
		fileReport.Bytes = st.Size()

		pages, err := engine.ReadMetricFileV1(path)
		if err != nil {
			fileReport.ScanError = err.Error()
			report.HasErrors = true
			report.Files = append(report.Files, fileReport)
			continue
		}

		fileReport.Frames = len(pages)
		seenMetrics := make(map[engine.MetricID]struct{}, len(pages))
		var totalPayload int64
		if verbose {
			fileReport.FramesDetail = make([]inspectMetricFrameReport, 0, len(pages))
		}
		for idx, page := range pages {
			seenMetrics[page.MetricID] = struct{}{}
			fileReport.Points += int64(page.PointCount)
			totalPayload += int64(page.PayloadLen)
			if fileReport.MinTimestamp == 0 || int64(page.MetricMinTS) < fileReport.MinTimestamp {
				fileReport.MinTimestamp = int64(page.MetricMinTS)
			}
			if fileReport.MaxTimestamp == 0 || int64(page.MetricMaxTS) > fileReport.MaxTimestamp {
				fileReport.MaxTimestamp = int64(page.MetricMaxTS)
			}
			if verbose {
				fileReport.FramesDetail = append(fileReport.FramesDetail, inspectMetricFrameReport{
					Index:           idx,
					MetricID:        uint16(page.MetricID),
					ValueType:       page.ValueType,
					PointCount:      page.PointCount,
					PayloadLen:      page.PayloadLen,
					UncompressedLen: page.UncompressedLen,
					StartTS:         int64(page.MetricMinTS),
					EndTS:           int64(page.MetricMaxTS),
					StartUTC:        engine.FormatTimestamp(page.MetricMinTS),
					EndUTC:          engine.FormatTimestamp(page.MetricMaxTS),
				})
			}
		}
		fileReport.DistinctMetrics = len(seenMetrics)
		if fileReport.Frames > 0 {
			fileReport.AvgPayloadBytes = totalPayload / int64(fileReport.Frames)
			fileReport.MinUTC = engine.FormatTimestamp(engine.Timestamp(fileReport.MinTimestamp))
			fileReport.MaxUTC = engine.FormatTimestamp(engine.Timestamp(fileReport.MaxTimestamp))
		}

		report.TotalBytes += fileReport.Bytes
		report.TotalFrames += int64(fileReport.Frames)
		report.TotalMetrics += int64(fileReport.DistinctMetrics)
		report.TotalPoints += fileReport.Points
		report.Files = append(report.Files, fileReport)
	}
	report.FileCount = len(report.Files)

	return report, nil
}
