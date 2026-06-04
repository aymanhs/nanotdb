package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/aymanhs/nanotdb/internal/engine"
)

type inspectEventsCatalogItem struct {
	Name      string `json:"name"`
	ID        uint16 `json:"id"`
	ValueType string `json:"value_type"`
}

type inspectEventsCatalogReport struct {
	RootDir     string                     `json:"root_dir"`
	Database    string                     `json:"database"`
	DatabaseDir string                     `json:"database_dir"`
	CatalogPath string                     `json:"catalog_path"`
	Count       int                        `json:"count"`
	Events      []inspectEventsCatalogItem `json:"events"`
	GeneratedAt string                     `json:"generated_at"`
}

type inspectEventsFileReport struct {
	Path            string `json:"path"`
	Bytes           int64  `json:"bytes"`
	Frames          int    `json:"frames"`
	Records         int64  `json:"records"`
	CompressedBytes int64  `json:"compressed_bytes"`
	MinTimestamp    int64  `json:"min_timestamp_ns,omitempty"`
	MaxTimestamp    int64  `json:"max_timestamp_ns,omitempty"`
	MinUTC          string `json:"min_utc,omitempty"`
	MaxUTC          string `json:"max_utc,omitempty"`
	ScanError       string `json:"scan_error,omitempty"`
}

type inspectEventsReport struct {
	RootDir      string                    `json:"root_dir"`
	Database     string                    `json:"database"`
	DatabaseDir  string                    `json:"database_dir"`
	Files        []inspectEventsFileReport `json:"files"`
	FileCount    int                       `json:"file_count"`
	TotalBytes   int64                     `json:"total_bytes"`
	TotalFrames  int64                     `json:"total_frames"`
	TotalRecords int64                     `json:"total_records"`
	HasErrors    bool                      `json:"has_errors"`
	GeneratedAt  string                    `json:"generated_at"`
}

type inspectEventsWALReport struct {
	RootDir      string `json:"root_dir"`
	Database     string `json:"database"`
	DatabaseDir  string `json:"database_dir"`
	Path         string `json:"path"`
	Exists       bool   `json:"exists"`
	Bytes        int64  `json:"bytes"`
	Records      int    `json:"records"`
	MinTimestamp int64  `json:"min_timestamp_ns,omitempty"`
	MaxTimestamp int64  `json:"max_timestamp_ns,omitempty"`
	MinUTC       string `json:"min_utc,omitempty"`
	MaxUTC       string `json:"max_utc,omitempty"`
	ScanError    string `json:"scan_error,omitempty"`
	GeneratedAt  string `json:"generated_at"`
}

func runInspectEventsCatalog(args []string) error {
	fs := flag.NewFlagSet("inspect events-catalog", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	rootDir := fs.String("root", "", "root data directory that contains engine.toml")
	dbName := fs.String("db", "", "database name")
	jsonOut := fs.Bool("json", false, "emit JSON output")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("usage: nanocli inspect events-catalog --root <root-dir> --db <database> [--json]")
	}

	ctx, err := resolveDBContext(*rootDir, *dbName)
	if err != nil {
		return err
	}
	report, err := buildInspectEventsCatalogReport(ctx)
	if err != nil {
		return err
	}

	ow := outputWriter{w: os.Stdout, asJSON: *jsonOut}
	return ow.emit(report, func(w io.Writer) {
		fmt.Fprintf(w, "Events catalog for %s\n", report.Database)
		fmt.Fprintf(w, "Catalog: %s\n", report.CatalogPath)
		fmt.Fprintf(w, "events=%d\n", report.Count)
		if report.Count == 0 {
			fmt.Fprintln(w, "no events registered")
			return
		}
		rows := make([][]string, 0, len(report.Events))
		for _, e := range report.Events {
			rows = append(rows, []string{e.Name, fmt.Sprintf("%d", e.ID), e.ValueType})
		}
		printAlignedTable(w, []string{"name", "id", "type"}, rows, map[int]bool{1: true})
	})
}

func buildInspectEventsCatalogReport(ctx dbContext) (inspectEventsCatalogReport, error) {
	report := inspectEventsCatalogReport{
		RootDir:     ctx.RootDir,
		Database:    ctx.Database,
		DatabaseDir: ctx.DatabaseDir,
		CatalogPath: filepath.Join(ctx.DatabaseDir, "events.json"),
		GeneratedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Events:      []inspectEventsCatalogItem{},
	}
	cat, err := engine.LoadEventCatalog(report.CatalogPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return report, nil
		}
		return inspectEventsCatalogReport{}, err
	}
	defer cat.Close()

	list := cat.ListEvents()
	report.Events = make([]inspectEventsCatalogItem, 0, len(list))
	for _, e := range list {
		report.Events = append(report.Events, inspectEventsCatalogItem{
			Name:      e.Name,
			ID:        uint16(e.EventID),
			ValueType: engine.EventValueTypeName(e.ValueType),
		})
	}
	report.Count = len(report.Events)
	return report, nil
}

func runInspectEvents(args []string) error {
	fs := flag.NewFlagSet("inspect events", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	rootDir := fs.String("root", "", "root data directory that contains engine.toml")
	dbName := fs.String("db", "", "database name")
	verbose := fs.Bool("verbose", false, "include per-file timestamp span details")
	jsonOut := fs.Bool("json", false, "emit JSON output")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("usage: nanocli inspect events --root <root-dir> --db <database> [--verbose] [--json]")
	}

	ctx, err := resolveDBContext(*rootDir, *dbName)
	if err != nil {
		return err
	}
	report, err := buildInspectEventsReport(ctx)
	if err != nil {
		return err
	}

	ow := outputWriter{w: os.Stdout, asJSON: *jsonOut}
	return ow.emit(report, func(w io.Writer) {
		fmt.Fprintf(w, "Events files for %s\n", report.Database)
		fmt.Fprintf(w, "summary: files=%d bytes=%d frames=%d records=%d\n", report.FileCount, report.TotalBytes, report.TotalFrames, report.TotalRecords)
		if report.FileCount == 0 {
			fmt.Fprintln(w, "no events files discovered")
			return
		}
		headers := []string{"file", "bytes", "frames", "records", "compressed"}
		right := map[int]bool{1: true, 2: true, 3: true, 4: true}
		if *verbose {
			headers = append(headers, "start", "duration")
		}
		rows := make([][]string, 0, len(report.Files))
		for _, f := range report.Files {
			displayPath := shortenTablePath(report.DatabaseDir, f.Path)
			if f.ScanError != "" {
				base := []string{displayPath, "ERR", "ERR", "ERR", "ERR"}
				if *verbose {
					base = append(base, "-", f.ScanError)
				}
				rows = append(rows, base)
				continue
			}
			row := []string{
				displayPath,
				fmt.Sprintf("%d", f.Bytes),
				fmt.Sprintf("%d", f.Frames),
				fmt.Sprintf("%d", f.Records),
				fmt.Sprintf("%d", f.CompressedBytes),
			}
			if *verbose {
				start := "-"
				duration := "-"
				if f.Records > 0 {
					start = f.MinUTC
					duration = formatDurationNS(f.MinTimestamp, f.MaxTimestamp)
				}
				row = append(row, start, duration)
			}
			rows = append(rows, row)
		}
		printAlignedTable(w, headers, rows, right)
	})
}

func buildInspectEventsReport(ctx dbContext) (inspectEventsReport, error) {
	report := inspectEventsReport{
		RootDir:     ctx.RootDir,
		Database:    ctx.Database,
		DatabaseDir: ctx.DatabaseDir,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	paths, err := filepath.Glob(filepath.Join(ctx.DatabaseDir, "events-*.dat"))
	if err != nil {
		return inspectEventsReport{}, err
	}
	sort.Strings(paths)
	report.Files = make([]inspectEventsFileReport, 0, len(paths))
	for _, p := range paths {
		fr := inspectEventsFileReport{Path: p}
		stats, err := engine.ScanEventsFileStats(p)
		if err != nil {
			fr.ScanError = err.Error()
			report.HasErrors = true
			report.Files = append(report.Files, fr)
			continue
		}
		fr.Bytes = stats.FileBytes
		fr.Frames = stats.Frames
		fr.Records = stats.TotalRecords
		fr.CompressedBytes = stats.TotalCompressed
		if stats.TotalRecords > 0 {
			fr.MinTimestamp = int64(stats.MinStart)
			fr.MaxTimestamp = int64(stats.MaxEnd)
			fr.MinUTC = engine.FormatTimestamp(stats.MinStart)
			fr.MaxUTC = engine.FormatTimestamp(stats.MaxEnd)
		}
		report.TotalBytes += fr.Bytes
		report.TotalFrames += int64(fr.Frames)
		report.TotalRecords += fr.Records
		report.Files = append(report.Files, fr)
	}
	report.FileCount = len(report.Files)
	return report, nil
}

func runInspectEventsWAL(args []string) error {
	fs := flag.NewFlagSet("inspect events-wal", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	rootDir := fs.String("root", "", "root data directory that contains engine.toml")
	dbName := fs.String("db", "", "database name")
	_ = fs.Bool("verbose", false, "reserved for future diagnostics")
	jsonOut := fs.Bool("json", false, "emit JSON output")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("usage: nanocli inspect events-wal --root <root-dir> --db <database> [--verbose] [--json]")
	}

	ctx, err := resolveDBContext(*rootDir, *dbName)
	if err != nil {
		return err
	}
	report := inspectEventsWALReport{
		RootDir:     ctx.RootDir,
		Database:    ctx.Database,
		DatabaseDir: ctx.DatabaseDir,
		Path:        filepath.Join(ctx.DatabaseDir, ctx.Database+".events.wal"),
		GeneratedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if st, err := os.Stat(report.Path); err == nil && !st.IsDir() {
		report.Exists = true
		report.Bytes = st.Size()
	}
	if report.Exists {
		cat, err := engine.LoadEventCatalog(filepath.Join(ctx.DatabaseDir, "events.json"))
		if err != nil {
			report.ScanError = fmt.Sprintf("load events catalog: %v", err)
		} else {
			defer cat.Close()
			wal, err := engine.OpenAndRecoverEventsWAL(report.Path, engine.WALFsyncPolicySegment)
			if err != nil {
				report.ScanError = fmt.Sprintf("open events wal: %v", err)
			} else {
				defer wal.Close()
				recs, err := wal.RecordsWithCatalog(cat)
				if err != nil {
					report.ScanError = fmt.Sprintf("scan events wal: %v", err)
				} else {
					report.Records = len(recs)
					if len(recs) > 0 {
						minTS := recs[0].TS
						maxTS := recs[0].TS
						for i := 1; i < len(recs); i++ {
							if recs[i].TS < minTS {
								minTS = recs[i].TS
							}
							if recs[i].TS > maxTS {
								maxTS = recs[i].TS
							}
						}
						report.MinTimestamp = int64(minTS)
						report.MaxTimestamp = int64(maxTS)
						report.MinUTC = engine.FormatTimestamp(minTS)
						report.MaxUTC = engine.FormatTimestamp(maxTS)
					}
				}
			}
		}
	}

	ow := outputWriter{w: os.Stdout, asJSON: *jsonOut}
	return ow.emit(report, func(w io.Writer) {
		fmt.Fprintf(w, "Events WAL for %s\n", report.Database)
		fmt.Fprintf(w, "path: %s\n", report.Path)
		fmt.Fprintf(w, "exists: %t\n", report.Exists)
		if !report.Exists {
			return
		}
		fmt.Fprintf(w, "bytes=%d records=%d\n", report.Bytes, report.Records)
		if report.Records > 0 {
			fmt.Fprintf(w, "start=%s duration=%s\n", report.MinUTC, formatDurationNS(report.MinTimestamp, report.MaxTimestamp))
		}
		if report.ScanError != "" {
			fmt.Fprintf(w, "scan_error: %s\n", report.ScanError)
		}
	})
}
