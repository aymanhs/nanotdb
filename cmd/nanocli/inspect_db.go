package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/aymanhs/nanotdb/internal/engine"
)

type catalogDisk struct {
	Metrics []struct {
		Name      string          `json:"name"`
		MetricID  engine.MetricID `json:"id"`
		ValueType byte            `json:"type"`
	} `json:"metrics"`
}

type inspectDBReport struct {
	RootDir      string                    `json:"root_dir"`
	Database     string                    `json:"database"`
	DatabaseDir  string                    `json:"database_dir"`
	EngineConfig string                    `json:"engine_config"`
	Catalog      inspectCatalogReport      `json:"catalog"`
	Manifest     inspectManifestReport     `json:"manifest"`
	Data         inspectDataReport         `json:"data"`
	WAL          inspectWALReport          `json:"wal"`
	Verbose      bool                      `json:"verbose"`
	DataDetail   *inspectDatReport         `json:"data_detail,omitempty"`
	WALDetail    *inspectWALReportDetailed `json:"wal_detail,omitempty"`
	GeneratedAt  string                    `json:"generated_at"`
}

type inspectCatalogReport struct {
	Path         string `json:"path"`
	Exists       bool   `json:"exists"`
	ParseError   string `json:"parse_error,omitempty"`
	MetricsCount int    `json:"metrics_count"`
	Int32Count   int    `json:"int32_count"`
	Float32Count int    `json:"float32_count"`
}

type inspectManifestReport struct {
	Path       string                 `json:"path"`
	Exists     bool                   `json:"exists"`
	ParseError string                 `json:"parse_error,omitempty"`
	Config     *engine.DBManifestTOML `json:"config,omitempty"`
}

type inspectDataReport struct {
	Files        int    `json:"files"`
	TotalBytes   int64  `json:"total_bytes"`
	TotalFrames  int64  `json:"total_frames"`
	TotalRecords int64  `json:"total_records"`
	MinTimestamp int64  `json:"min_timestamp_ns,omitempty"`
	MaxTimestamp int64  `json:"max_timestamp_ns,omitempty"`
	MinUTC       string `json:"min_utc,omitempty"`
	MaxUTC       string `json:"max_utc,omitempty"`
}

type inspectWALReport struct {
	Files        int   `json:"files"`
	TotalBytes   int64 `json:"total_bytes"`
	TotalRecords int64 `json:"total_records"`
	HasTail      bool  `json:"has_tail"`
}

func runInspect(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("inspect requires a subcommand (db)")
	}
	sub := args[0]
	subArgs := args[1:]

	switch sub {
	case "db":
		return runInspectDB(subArgs)
	case "catalog":
		return runInspectCatalog(subArgs)
	case "dat":
		return runInspectDat(subArgs)
	case "metric":
		return runInspectMetric(subArgs)
	case "wal":
		return runInspectWAL(subArgs)
	case "events":
		return runInspectEvents(subArgs)
	case "events-catalog":
		return runInspectEventsCatalog(subArgs)
	case "events-wal":
		return runInspectEventsWAL(subArgs)
	default:
		return fmt.Errorf("unknown inspect subcommand: %s", sub)
	}
}

func runInspectDB(args []string) error {
	fs := flag.NewFlagSet("inspect db", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	rootDir := fs.String("root", "", "root data directory that contains engine.toml")
	dbName := fs.String("db", "", "database name")
	verbose := fs.Bool("verbose", false, "include detailed per-file dat/wal sections")
	jsonOut := fs.Bool("json", false, "emit JSON output")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("usage: nanocli inspect db --root <root-dir> --db <database> [--verbose] [--json]")
	}

	ctx, err := resolveDBContext(*rootDir, *dbName)
	if err != nil {
		return err
	}

	report, err := buildInspectReport(ctx, *verbose)
	if err != nil {
		return err
	}

	ow := outputWriter{w: os.Stdout, asJSON: *jsonOut}
	return ow.emit(report, func(w io.Writer) {
		fmt.Fprintf(w, "Database: %s\n", report.Database)
		fmt.Fprintf(w, "Root: %s\n", report.RootDir)
		fmt.Fprintf(w, "DB dir: %s\n", report.DatabaseDir)
		fmt.Fprintf(w, "Engine config: %s\n", report.EngineConfig)
		fmt.Fprintf(w, "\nCatalog\n")
		fmt.Fprintf(w, "  path: %s\n", report.Catalog.Path)
		fmt.Fprintf(w, "  exists: %t\n", report.Catalog.Exists)
		if report.Catalog.ParseError != "" {
			fmt.Fprintf(w, "  parse_error: %s\n", report.Catalog.ParseError)
		}
		if report.Catalog.Exists {
			fmt.Fprintf(w, "  metrics: total=%d int32=%d float32=%d\n", report.Catalog.MetricsCount, report.Catalog.Int32Count, report.Catalog.Float32Count)
		}
		fmt.Fprintf(w, "\nManifest\n")
		fmt.Fprintf(w, "  path: %s\n", report.Manifest.Path)
		fmt.Fprintf(w, "  exists: %t\n", report.Manifest.Exists)
		if report.Manifest.ParseError != "" {
			fmt.Fprintf(w, "  parse_error: %s\n", report.Manifest.ParseError)
		}
		if report.Manifest.Config != nil {
			cfg := report.Manifest.Config
			fmt.Fprintf(w, "  retention: grace=%s retention_days=%d retention_action=%s max_active_days=%d\n", cfg.Retention.Grace, cfg.Retention.RetentionDays, cfg.Retention.RetentionAction, cfg.Retention.MaxActiveDays)
			fmt.Fprintf(w, "  wal: enabled=%t skip_before=%s\n", cfg.WAL.Enabled, cfg.WAL.SkipBefore)
			fmt.Fprintf(w, "  page: max_records=%d max_bytes=%d max_age=%s\n", cfg.Page.MaxRecords, cfg.Page.MaxBytes, cfg.Page.MaxAge)
		}
		fmt.Fprintf(w, "\nData\n")
		fmt.Fprintf(w, "  files=%d bytes=%d frames=%d records=%d\n", report.Data.Files, report.Data.TotalBytes, report.Data.TotalFrames, report.Data.TotalRecords)
		if report.Data.MinTimestamp > 0 {
			fmt.Fprintf(w, "  start=%s duration=%s\n", report.Data.MinUTC, formatDurationNS(report.Data.MinTimestamp, report.Data.MaxTimestamp))
		}
		fmt.Fprintf(w, "\nWAL\n")
		fmt.Fprintf(w, "  files=%d bytes=%d records=%d tail_present=%t\n", report.WAL.Files, report.WAL.TotalBytes, report.WAL.TotalRecords, report.WAL.HasTail)

		if report.Verbose && report.DataDetail != nil {
			fmt.Fprintf(w, "\nData Detail\n")
			if report.DataDetail.FileCount == 0 {
				fmt.Fprintf(w, "  no data files discovered\n")
			} else {
				rows := make([][]string, 0, len(report.DataDetail.Files))
				for _, f := range report.DataDetail.Files {
					displayPath := shortenTablePath(report.DataDetail.DatabaseDir, f.Path)
					if f.ScanError != "" {
						rows = append(rows, []string{displayPath, "ERR", "ERR", "ERR", "ERR", "ERR", "ERR", "-", f.ScanError})
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
					rows,
					map[int]bool{1: true, 2: true, 3: true, 4: true, 5: true, 6: true},
				)
			}
		}

		if report.Verbose && report.WALDetail != nil {
			fmt.Fprintf(w, "\nWAL Detail\n")
			if report.WALDetail.FileCount == 0 {
				fmt.Fprintf(w, "  no wal files discovered\n")
			} else {
				rows := make([][]string, 0, len(report.WALDetail.Files))
				for _, f := range report.WALDetail.Files {
					displayPath := shortenTablePath(report.WALDetail.DatabaseDir, f.Path)
					if f.ScanError != "" {
						rows = append(rows, []string{displayPath, "ERR", "ERR", "ERR", "-", "-", "-", "-", "-", f.ScanError})
						continue
					}
					start := "-"
					duration := "-"
					if f.Records > 0 {
						start = f.MinUTC
						duration = formatDurationNS(f.MinTimestamp, f.MaxTimestamp)
					}
					reason := "-"
					if f.StopReason != "" {
						reason = f.StopReason
					}
					rows = append(rows, []string{
						displayPath,
						fmt.Sprintf("%d", f.Bytes),
						fmt.Sprintf("%d", f.Records),
						fmt.Sprintf("%d", f.DecodedBytes),
						start,
						duration,
						fmt.Sprintf("%t", f.HasTail),
						fmt.Sprintf("%d", f.TailBytes),
						fmt.Sprintf("%d", f.StopOffset),
						reason,
					})
				}
				printAlignedTable(w,
					[]string{"file", "bytes", "records", "decoded", "start", "duration", "tail", "tail_bytes", "stop_off", "reason"},
					rows,
					map[int]bool{1: true, 2: true, 3: true, 7: true, 8: true},
				)
			}
		}
	})
}

func buildInspectReport(ctx dbContext, verbose bool) (inspectDBReport, error) {
	report := inspectDBReport{
		RootDir:      ctx.RootDir,
		Database:     ctx.Database,
		DatabaseDir:  ctx.DatabaseDir,
		EngineConfig: ctx.EngineConfig,
		Verbose:      verbose,
		Catalog: inspectCatalogReport{
			Path: ctx.CatalogPath,
		},
		Manifest: inspectManifestReport{
			Path: ctx.ManifestPath,
		},
		GeneratedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}

	if st, err := os.Stat(ctx.CatalogPath); err == nil && !st.IsDir() {
		report.Catalog.Exists = true
		catRaw, err := os.ReadFile(ctx.CatalogPath)
		if err != nil {
			return inspectDBReport{}, err
		}
		var cat catalogDisk
		if err := json.Unmarshal(catRaw, &cat); err != nil {
			report.Catalog.ParseError = err.Error()
		} else {
			report.Catalog.MetricsCount = len(cat.Metrics)
			for _, m := range cat.Metrics {
				switch m.ValueType {
				case engine.Int32Sample:
					report.Catalog.Int32Count++
				case engine.Float32Sample:
					report.Catalog.Float32Count++
				}
			}
		}
	} else if err != nil && !os.IsNotExist(err) {
		return inspectDBReport{}, err
	}

	if st, err := os.Stat(ctx.ManifestPath); err == nil && !st.IsDir() {
		report.Manifest.Exists = true
		var cfg engine.DBManifestTOML
		if _, err := toml.DecodeFile(ctx.ManifestPath, &cfg); err != nil {
			report.Manifest.ParseError = err.Error()
		} else {
			report.Manifest.Config = &cfg
		}
	} else if err != nil && !os.IsNotExist(err) {
		return inspectDBReport{}, err
	}

	report.Data.Files = len(ctx.DataFilePaths)
	for _, path := range ctx.DataFilePaths {
		st, err := os.Stat(path)
		if err != nil {
			return inspectDBReport{}, err
		}
		report.Data.TotalBytes += st.Size()
		stats, err := engine.WalkDataFileHeaders(path, nil)
		if err != nil {
			return inspectDBReport{}, fmt.Errorf("scan data file %s: %w", path, err)
		}
		report.Data.TotalFrames += int64(stats.Frames)
		report.Data.TotalRecords += stats.TotalRecords
		if stats.Frames > 0 {
			if report.Data.MinTimestamp == 0 || int64(stats.MinStart) < report.Data.MinTimestamp {
				report.Data.MinTimestamp = int64(stats.MinStart)
			}
			if report.Data.MaxTimestamp == 0 || int64(stats.MaxEnd) > report.Data.MaxTimestamp {
				report.Data.MaxTimestamp = int64(stats.MaxEnd)
			}
		}
	}
	if report.Data.MinTimestamp > 0 {
		report.Data.MinUTC = engine.FormatTimestamp(engine.Timestamp(report.Data.MinTimestamp))
		report.Data.MaxUTC = engine.FormatTimestamp(engine.Timestamp(report.Data.MaxTimestamp))
	}

	report.WAL.Files = len(ctx.WALFilePaths)
	for _, path := range ctx.WALFilePaths {
		stats, err := engine.WalkWALFile(path, nil)
		if err != nil {
			return inspectDBReport{}, fmt.Errorf("scan WAL file %s: %w", path, err)
		}
		report.WAL.TotalBytes += stats.FileBytes
		report.WAL.TotalRecords += int64(stats.Records)
		if stats.HasTail {
			report.WAL.HasTail = true
		}
	}

	if _, err := os.Stat(ctx.DatabaseDir); err != nil {
		if os.IsNotExist(err) {
			return inspectDBReport{}, fmt.Errorf("database directory not found: %s", ctx.DatabaseDir)
		}
		return inspectDBReport{}, err
	}

	if verbose {
		datDetail, err := buildInspectDatReport(ctx, true)
		if err != nil {
			return inspectDBReport{}, err
		}
		report.DataDetail = &datDetail

		walDetail, err := buildInspectWALReportDetailed(ctx)
		if err != nil {
			return inspectDBReport{}, err
		}
		report.WALDetail = &walDetail
	}

	return report, nil
}
