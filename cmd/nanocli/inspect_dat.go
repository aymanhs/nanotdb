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
	Path         string `json:"path"`
	Bytes        int64  `json:"bytes"`
	Frames       int    `json:"frames"`
	Records      int64  `json:"records"`
	MinTimestamp int64  `json:"min_timestamp_ns,omitempty"`
	MaxTimestamp int64  `json:"max_timestamp_ns,omitempty"`
	MinUTC       string `json:"min_utc,omitempty"`
	MaxUTC       string `json:"max_utc,omitempty"`
	ScanError    string `json:"scan_error,omitempty"`
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
	jsonOut := fs.Bool("json", false, "emit JSON output")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("usage: nanocli inspect dat --root <root-dir> --db <database> [--json]")
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

	report, err := buildInspectDatReport(ctx)
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
		for _, f := range report.Files {
			if f.ScanError != "" {
				fmt.Fprintf(w, "- %s error=%s\n", f.Path, f.ScanError)
				continue
			}
			if f.Frames > 0 {
				fmt.Fprintf(w, "- %s bytes=%d frames=%d records=%d range=%s .. %s\n", f.Path, f.Bytes, f.Frames, f.Records, f.MinUTC, f.MaxUTC)
			} else {
				fmt.Fprintf(w, "- %s bytes=%d frames=0 records=0\n", f.Path, f.Bytes)
			}
		}
	})
}

func buildInspectDatReport(ctx dbContext) (inspectDatReport, error) {
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

		stats, err := engine.WalkDataFileHeaders(path, nil)
		if err != nil {
			fileReport.ScanError = err.Error()
			report.HasErrors = true
			report.Files = append(report.Files, fileReport)
			continue
		}

		fileReport.Frames = stats.Frames
		fileReport.Records = stats.TotalRecords
		if stats.Frames > 0 {
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
