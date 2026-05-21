package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aymanhs/nanotdb/internal/engine"
)

type metricBuildPartitionReport struct {
	Partition  string `json:"partition"`
	SourcePath string `json:"source_path"`
	MetricPath string `json:"metric_path"`
	SourceBytes int64 `json:"source_bytes"`
	MetricBytes int64 `json:"metric_bytes"`
	Verified   bool   `json:"verified"`
}

type metricBuildReport struct {
	RootDir         string                       `json:"root_dir"`
	Database        string                       `json:"database"`
	Codec           string                       `json:"codec"`
	RawIngestAction string                       `json:"raw_ingest_action"`
	Verified        bool                         `json:"verified"`
	Partitions      []metricBuildPartitionReport `json:"partitions"`
	PartitionCount  int                          `json:"partition_count"`
	SourceBytes     int64                        `json:"source_bytes"`
	MetricBytes     int64                        `json:"metric_bytes"`
	DurationMS      int64                        `json:"duration_ms"`
}

func runMetric(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: nanocli metric build --root <root-dir> --db <database> [--part <partition>] [--codec <name>] [--raw-ingest-action <keep|rename|delete>] [--verify] [--json]")
	}
	cmd := args[0]
	args = args[1:]
	switch cmd {
	case "build":
		return runMetricBuild(args)
	default:
		return fmt.Errorf("unknown metric command: %s", cmd)
	}
}

func runMetricBuild(args []string) error {
	fs := flag.NewFlagSet("metric build", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	rootDir := fs.String("root", "", "root data directory that contains engine.toml")
	dbName := fs.String("db", "", "database name")
	partition := fs.String("part", "", "optional partition key to build (for example 2026-05-03 or 2026-05)")
	codecName := fs.String("codec", "", "optional metric file compression override")
	rawAction := fs.String("raw-ingest-action", "", "optional raw ingest action override: keep, rename, or delete")
	verify := fs.Bool("verify", false, "compare source raw and metric files after build")
	jsonOut := fs.Bool("json", false, "emit JSON output")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("usage: nanocli metric build --root <root-dir> --db <database> [--part <partition>] [--codec <name>] [--raw-ingest-action <keep|rename|delete>] [--verify] [--json]")
	}

	ctx, err := resolveDBContext(*rootDir, *dbName)
	if err != nil {
		return err
	}

	partitions, err := metricBuildPartitions(ctx.DatabaseDir, *partition)
	if err != nil {
		return err
	}
	if len(partitions) == 0 {
		return fmt.Errorf("no data-*.dat or raw-*.dat partitions found under %s", ctx.DatabaseDir)
	}

	eng, err := engine.OpenEngine(ctx.RootDir, 0)
	if err != nil {
		return err
	}
	defer eng.Close()

	if strings.TrimSpace(*codecName) != "" {
		codec, err := engine.BlockCompressionCodecByName(*codecName)
		if err != nil {
			return err
		}
		eng.MetricFileCompression = codec.Name()
	}
	if strings.TrimSpace(*rawAction) != "" {
		action := strings.ToLower(strings.TrimSpace(*rawAction))
		switch action {
		case engine.MetricRawIngestActionKeep, engine.MetricRawIngestActionRename, engine.MetricRawIngestActionDelete:
			eng.MetricRawIngestAction = action
		default:
			return fmt.Errorf("invalid --raw-ingest-action %q (expected keep, rename, or delete)", *rawAction)
		}
	}

	started := time.Now()
	report := metricBuildReport{
		RootDir:         ctx.RootDir,
		Database:        ctx.Database,
		Codec:           eng.MetricFileCompression,
		RawIngestAction: eng.MetricRawIngestAction,
		Verified:        *verify,
		Partitions:      make([]metricBuildPartitionReport, 0, len(partitions)),
	}

	for _, part := range partitions {
		sourcePath := metricPartitionSourcePath(ctx.DatabaseDir, part)
		sourceStat, err := os.Stat(sourcePath)
		if err != nil {
			return err
		}
		metricPath, err := eng.BuildMetricFileV1(ctx.Database, part)
		if err != nil {
			return fmt.Errorf("build metric partition %s: %w", part, err)
		}
		metricStat, err := os.Stat(metricPath)
		if err != nil {
			return err
		}
		if *verify {
			if err := eng.CompareDataAndMetricPartitionV1(ctx.Database, part); err != nil {
				return fmt.Errorf("verify metric partition %s: %w", part, err)
			}
		}
		report.Partitions = append(report.Partitions, metricBuildPartitionReport{
			Partition:   part,
			SourcePath:  sourcePath,
			MetricPath:  metricPath,
			SourceBytes: sourceStat.Size(),
			MetricBytes: metricStat.Size(),
			Verified:    *verify,
		})
		report.SourceBytes += sourceStat.Size()
		report.MetricBytes += metricStat.Size()
	}
	report.PartitionCount = len(report.Partitions)
	report.DurationMS = time.Since(started).Milliseconds()

	ow := outputWriter{w: os.Stdout, asJSON: *jsonOut}
	return ow.emit(report, func(w io.Writer) {
		fmt.Fprintf(w, "Metric build for %s\n", report.Database)
		fmt.Fprintf(w, "Root: %s\n", report.RootDir)
		fmt.Fprintf(w, "codec=%s raw_ingest_action=%s partitions=%d duration_ms=%d\n", report.Codec, report.RawIngestAction, report.PartitionCount, report.DurationMS)
		rows := make([][]string, 0, len(report.Partitions))
		for _, part := range report.Partitions {
			ratio := "-"
			if part.SourceBytes > 0 {
				ratio = fmt.Sprintf("%.2f%%", float64(part.MetricBytes)*100/float64(part.SourceBytes))
			}
			rows = append(rows, []string{
				part.Partition,
				fmt.Sprintf("%d", part.SourceBytes),
				fmt.Sprintf("%d", part.MetricBytes),
				ratio,
				fmt.Sprintf("%t", part.Verified),
			})
		}
		printAlignedTable(w,
			[]string{"partition", "source_bytes", "metric_bytes", "metric_vs_source", "verified"},
			rows,
			map[int]bool{1: true, 2: true},
		)
		fmt.Fprintf(w, "totals: source_bytes=%d metric_bytes=%d\n", report.SourceBytes, report.MetricBytes)
	})
}

func metricBuildPartitions(dbDir, partition string) ([]string, error) {
	if strings.TrimSpace(partition) != "" {
		return []string{strings.TrimSpace(partition)}, nil
	}
	patterns := []string{"data-*.dat", "raw-*.dat"}
	seen := map[string]struct{}{}
	parts := make([]string, 0, 8)
	for _, pattern := range patterns {
		matches, err := filepath.Glob(filepath.Join(dbDir, pattern))
		if err != nil {
			return nil, err
		}
		for _, match := range matches {
			base := filepath.Base(match)
			prefix := "data-"
			if strings.HasPrefix(base, "raw-") {
				prefix = "raw-"
			}
			part := strings.TrimSuffix(strings.TrimPrefix(base, prefix), ".dat")
			if part == "" {
				continue
			}
			if _, ok := seen[part]; ok {
				continue
			}
			seen[part] = struct{}{}
			parts = append(parts, part)
		}
	}
	sort.Strings(parts)
	return parts, nil
}

func metricPartitionSourcePath(dbDir, partition string) string {
	dataPath := filepath.Join(dbDir, "data-"+partition+".dat")
	if _, err := os.Stat(dataPath); err == nil {
		return dataPath
	}
	return filepath.Join(dbDir, "raw-"+partition+".dat")
}