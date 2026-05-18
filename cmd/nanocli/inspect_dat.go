package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/aymanhs/nanotdb/internal/engine"
)

type inspectDatFileReport struct {
	Path          string                 `json:"path"`
	Bytes         int64                  `json:"bytes"`
	Frames        int                    `json:"frames"`
	Records       int64                  `json:"records"`
	AvgFrameBytes int64                  `json:"avg_frame_bytes,omitempty"`
	MinFrameBytes int64                  `json:"min_frame_bytes,omitempty"`
	MaxFrameBytes int64                  `json:"max_frame_bytes,omitempty"`
	Pages         []inspectDatPageReport `json:"pages,omitempty"`
	MinTimestamp  int64                  `json:"min_timestamp_ns,omitempty"`
	MaxTimestamp  int64                  `json:"max_timestamp_ns,omitempty"`
	MinUTC        string                 `json:"min_utc,omitempty"`
	MaxUTC        string                 `json:"max_utc,omitempty"`
	ScanError     string                 `json:"scan_error,omitempty"`
}

type inspectDatPageReport struct {
	Index         int     `json:"index"`
	Offset        int64   `json:"offset"`
	FrameBytes    int64   `json:"frame_bytes"`
	CompressedLen uint64  `json:"compressed_len"`
	Records       uint16  `json:"records"`
	BytesPerRec   float64 `json:"bytes_per_record,omitempty"`
	StartTS       int64   `json:"start_timestamp_ns"`
	EndTS         int64   `json:"end_timestamp_ns"`
	StartUTC      string  `json:"start_utc"`
	EndUTC        string  `json:"end_utc"`
}

type inspectDatReport struct {
	RootDir      string                 `json:"root_dir"`
	Database     string                 `json:"database"`
	DatabaseDir  string                 `json:"database_dir"`
	Files        []inspectDatFileReport `json:"files"`
	FileCount    int                    `json:"file_count"`
	TotalBytes   int64                  `json:"total_bytes"`
	TotalFrames  int64                  `json:"total_frames"`
	TotalRecords int64                  `json:"total_records"`
	HasErrors    bool                   `json:"has_errors"`
	GeneratedAt  string                 `json:"generated_at"`
}

func runInspectDat(args []string) error {
	fs := flag.NewFlagSet("inspect dat", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	rootDir := fs.String("root", "", "root data directory that contains engine.toml")
	dbName := fs.String("db", "", "database name")
	verbose := fs.Bool("verbose", false, "include per-page stats")
	jsonOut := fs.Bool("json", false, "emit JSON output")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("usage: nanocli inspect dat --root <root-dir> --db <database> [--verbose] [--json]")
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

	report, err := buildInspectDatReport(ctx, *verbose)
	if err != nil {
		return err
	}

	ow := outputWriter{w: os.Stdout, asJSON: *jsonOut}
	return ow.emit(report, func(w io.Writer) {
		fmt.Fprintf(w, "Data files for %s\n", report.Database)
		fmt.Fprintf(w, "Root: %s\n", report.RootDir)
		fmt.Fprintf(w, "DB dir: %s\n", report.DatabaseDir)
		fmt.Fprintf(w, "summary: files=%d bytes=%d frames=%d records=%d\n", report.FileCount, report.TotalBytes, report.TotalFrames, report.TotalRecords)
		if report.FileCount == 0 {
			fmt.Fprintf(w, "no data files discovered\n")
			return
		}

		fileRows := make([][]string, 0, len(report.Files))
		for _, f := range report.Files {
			displayPath := shortenTablePath(report.DatabaseDir, f.Path)
			if f.ScanError != "" {
				fileRows = append(fileRows, []string{displayPath, "ERR", "ERR", "ERR", "ERR", "ERR", "ERR", "-", f.ScanError})
				continue
			}
			start := "-"
			duration := "-"
			if f.Frames > 0 {
				start = f.MinUTC
				duration = formatDurationNS(f.MinTimestamp, f.MaxTimestamp)
			}
			fileRows = append(fileRows, []string{
				displayPath,
				fmt.Sprintf("%d", f.Bytes),
				fmt.Sprintf("%d", f.Frames),
				fmt.Sprintf("%d", f.Records),
				fmt.Sprintf("%d", f.AvgFrameBytes),
				fmt.Sprintf("%d", f.MinFrameBytes),
				fmt.Sprintf("%d", f.MaxFrameBytes),
				start,
				duration,
			})
		}
		printAlignedTable(w,
			[]string{"file", "bytes", "frames", "records", "avg_page", "min_page", "max_page", "start", "duration"},
			fileRows,
			map[int]bool{1: true, 2: true, 3: true, 4: true, 5: true, 6: true},
		)

		if !*verbose {
			return
		}

		for _, f := range report.Files {
			if f.ScanError != "" || len(f.Pages) == 0 {
				continue
			}
			fmt.Fprintf(w, "\nPages: %s\n", shortenTablePath(report.DatabaseDir, f.Path))
			rows := make([][]string, 0, len(f.Pages))
			for _, p := range f.Pages {
				rows = append(rows, []string{
					fmt.Sprintf("%d", p.Index),
					fmt.Sprintf("%d", p.Offset),
					fmt.Sprintf("%d", p.FrameBytes),
					fmt.Sprintf("%d", p.CompressedLen),
					fmt.Sprintf("%d", p.Records),
					fmt.Sprintf("%.2f", p.BytesPerRec),
					p.StartUTC,
					formatDurationNS(p.StartTS, p.EndTS),
				})
			}
			printAlignedTable(w,
				[]string{"idx", "offset", "bytes", "comp", "recs", "bytes/rec", "start", "duration"},
				rows,
				map[int]bool{0: true, 1: true, 2: true, 3: true, 4: true, 5: true},
			)
		}
	})
}

func buildInspectDatReport(ctx dbContext, verbose bool) (inspectDatReport, error) {
	report := inspectDatReport{
		RootDir:     ctx.RootDir,
		Database:    ctx.Database,
		DatabaseDir: ctx.DatabaseDir,
		Files:       make([]inspectDatFileReport, 0, len(ctx.DataFilePaths)),
		GeneratedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}

	for _, path := range ctx.DataFilePaths {
		fileReport := inspectDatFileReport{Path: path}
		st, err := os.Stat(path)
		if err != nil {
			fileReport.ScanError = err.Error()
			report.HasErrors = true
			report.Files = append(report.Files, fileReport)
			continue
		}
		fileReport.Bytes = st.Size()

		minFrameBytes := int64(0)
		maxFrameBytes := int64(0)
		if verbose {
			fileReport.Pages = make([]inspectDatPageReport, 0, 64)
		}
		stats, err := engine.WalkDataFileHeaders(path, func(frame engine.DataFrameHeader) error {
			if minFrameBytes == 0 || frame.FrameBytes < minFrameBytes {
				minFrameBytes = frame.FrameBytes
			}
			if frame.FrameBytes > maxFrameBytes {
				maxFrameBytes = frame.FrameBytes
			}
			if verbose {
				page := inspectDatPageReport{
					Index:         frame.Index,
					Offset:        frame.Offset,
					FrameBytes:    frame.FrameBytes,
					CompressedLen: frame.CompressedLen,
					Records:       frame.NumRecords,
					StartTS:       int64(frame.StartTime),
					EndTS:         int64(frame.EndTime),
					StartUTC:      engine.FormatTimestamp(frame.StartTime),
					EndUTC:        engine.FormatTimestamp(frame.EndTime),
				}
				if frame.NumRecords > 0 {
					page.BytesPerRec = float64(frame.FrameBytes) / float64(frame.NumRecords)
				}
				fileReport.Pages = append(fileReport.Pages, page)
			}
			return nil
		})
		if err != nil {
			fileReport.ScanError = err.Error()
			report.HasErrors = true
			report.Files = append(report.Files, fileReport)
			continue
		}

		fileReport.Frames = stats.Frames
		fileReport.Records = stats.TotalRecords
		if stats.Frames > 0 {
			fileReport.AvgFrameBytes = stats.TotalFrameBytes / int64(stats.Frames)
			fileReport.MinFrameBytes = minFrameBytes
			fileReport.MaxFrameBytes = maxFrameBytes
			fileReport.MinTimestamp = int64(stats.MinStart)
			fileReport.MaxTimestamp = int64(stats.MaxEnd)
			fileReport.MinUTC = engine.FormatTimestamp(stats.MinStart)
			fileReport.MaxUTC = engine.FormatTimestamp(stats.MaxEnd)
		}

		report.TotalBytes += fileReport.Bytes
		report.TotalFrames += int64(fileReport.Frames)
		report.TotalRecords += fileReport.Records
		report.Files = append(report.Files, fileReport)
	}
	report.FileCount = len(report.Files)

	return report, nil
}
