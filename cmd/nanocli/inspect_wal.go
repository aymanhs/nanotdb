package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/aymanhs/nanotdb/internal/engine"
)

type inspectWALFileReport struct {
	Path         string `json:"path"`
	Bytes        int64  `json:"bytes"`
	Records      int    `json:"records"`
	DecodedBytes int64  `json:"decoded_bytes"`
	MinTimestamp int64  `json:"min_timestamp_ns,omitempty"`
	MaxTimestamp int64  `json:"max_timestamp_ns,omitempty"`
	MinUTC       string `json:"min_utc,omitempty"`
	MaxUTC       string `json:"max_utc,omitempty"`
	HasTail      bool   `json:"has_tail"`
	TailBytes    int64  `json:"tail_bytes,omitempty"`
	StopOffset   int64  `json:"stop_offset,omitempty"`
	StopReason   string `json:"stop_reason,omitempty"`
	ScanError    string `json:"scan_error,omitempty"`
}

type inspectWALReportDetailed struct {
	RootDir      string                 `json:"root_dir"`
	Database     string                 `json:"database"`
	DatabaseDir  string                 `json:"database_dir"`
	Files        []inspectWALFileReport `json:"files"`
	FileCount    int                    `json:"file_count"`
	TotalBytes   int64                  `json:"total_bytes"`
	TotalRecords int64                  `json:"total_records"`
	HasTail      bool                   `json:"has_tail"`
	HasErrors    bool                   `json:"has_errors"`
	GeneratedAt  string                 `json:"generated_at"`
}

func runInspectWAL(args []string) error {
	fs := flag.NewFlagSet("inspect wal", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	rootDir := fs.String("root", "", "root data directory that contains engine.toml")
	dbName := fs.String("db", "", "database name")
	verbose := fs.Bool("verbose", false, "include tail diagnostics columns")
	jsonOut := fs.Bool("json", false, "emit JSON output")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("usage: nanocli inspect wal --root <root-dir> --db <database> [--verbose] [--json]")
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

	report, err := buildInspectWALReportDetailed(ctx)
	if err != nil {
		return err
	}

	ow := outputWriter{w: os.Stdout, asJSON: *jsonOut}
	return ow.emit(report, func(w io.Writer) {
		fmt.Fprintf(w, "WAL files for %s\n", report.Database)
		fmt.Fprintf(w, "Root: %s\n", report.RootDir)
		fmt.Fprintf(w, "DB dir: %s\n", report.DatabaseDir)
		fmt.Fprintf(w, "summary: files=%d bytes=%d records=%d has_tail=%t\n", report.FileCount, report.TotalBytes, report.TotalRecords, report.HasTail)
		if report.FileCount == 0 {
			fmt.Fprintf(w, "no wal files discovered\n")
			return
		}

		headers := []string{"file", "bytes", "records", "decoded", "start", "duration", "tail"}
		right := map[int]bool{1: true, 2: true, 3: true}
		if *verbose {
			headers = append(headers, "tail_bytes", "stop_off", "reason")
			right[7] = true
			right[8] = true
		}

		rows := make([][]string, 0, len(report.Files))
		for _, f := range report.Files {
			displayPath := shortenTablePath(report.DatabaseDir, f.Path)
			if f.ScanError != "" {
				base := []string{displayPath, "ERR", "ERR", "ERR", "-", "-", "-"}
				if *verbose {
					base = append(base, "-", "-", f.ScanError)
				}
				rows = append(rows, base)
				continue
			}
			start := "-"
			duration := "-"
			if f.Records > 0 {
				start = f.MinUTC
				duration = formatDurationNS(f.MinTimestamp, f.MaxTimestamp)
			}
			row := []string{
				displayPath,
				fmt.Sprintf("%d", f.Bytes),
				fmt.Sprintf("%d", f.Records),
				fmt.Sprintf("%d", f.DecodedBytes),
				start,
				duration,
				fmt.Sprintf("%t", f.HasTail),
			}
			if *verbose {
				reason := "-"
				if f.StopReason != "" {
					reason = f.StopReason
				}
				row = append(row,
					fmt.Sprintf("%d", f.TailBytes),
					fmt.Sprintf("%d", f.StopOffset),
					reason,
				)
			}
			rows = append(rows, row)
		}
		printAlignedTable(w, headers, rows, right)
	})
}

func buildInspectWALReportDetailed(ctx dbContext) (inspectWALReportDetailed, error) {
	report := inspectWALReportDetailed{
		RootDir:     ctx.RootDir,
		Database:    ctx.Database,
		DatabaseDir: ctx.DatabaseDir,
		Files:       make([]inspectWALFileReport, 0, len(ctx.WALFilePaths)),
		GeneratedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}

	for _, path := range ctx.WALFilePaths {
		fileReport := inspectWALFileReport{Path: path}
		stats, err := engine.WalkWALFile(path, nil)
		if err != nil {
			fileReport.ScanError = err.Error()
			report.HasErrors = true
			report.Files = append(report.Files, fileReport)
			continue
		}

		fileReport.Bytes = stats.FileBytes
		fileReport.Records = stats.Records
		fileReport.DecodedBytes = stats.DecodedBytes
		if stats.Records > 0 {
			fileReport.MinTimestamp = int64(stats.MinTS)
			fileReport.MaxTimestamp = int64(stats.MaxTS)
			fileReport.MinUTC = engine.FormatTimestamp(stats.MinTS)
			fileReport.MaxUTC = engine.FormatTimestamp(stats.MaxTS)
		}
		fileReport.HasTail = stats.HasTail
		fileReport.TailBytes = stats.TailBytes
		fileReport.StopOffset = stats.StopOffset
		fileReport.StopReason = stats.StopReason

		report.TotalBytes += fileReport.Bytes
		report.TotalRecords += int64(fileReport.Records)
		if fileReport.HasTail {
			report.HasTail = true
		}
		report.Files = append(report.Files, fileReport)
	}
	report.FileCount = len(report.Files)

	return report, nil
}
