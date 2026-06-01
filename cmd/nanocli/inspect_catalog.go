package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/aymanhs/nanotdb/internal/engine"
)

type inspectCatalogMetricReport struct {
	Name      string `json:"name"`
	MetricID  uint16 `json:"id"`
	ValueType string `json:"type"`
}

type inspectCatalogListReport struct {
	RootDir     string                       `json:"root_dir"`
	Database    string                       `json:"database"`
	DatabaseDir string                       `json:"database_dir"`
	CatalogPath string                       `json:"catalog_path"`
	Count       int                          `json:"count"`
	Metrics     []inspectCatalogMetricReport `json:"metrics"`
	GeneratedAt string                       `json:"generated_at"`
}

func runInspectCatalog(args []string) error {
	fs := flag.NewFlagSet("inspect catalog", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	rootDir := fs.String("root", "", "root data directory that contains engine.toml")
	dbName := fs.String("db", "", "database name")
	jsonOut := fs.Bool("json", false, "emit JSON output")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("usage: nanocli inspect catalog --root <root-dir> --db <database> [--json]")
	}

	ctx, err := resolveDBContext(*rootDir, *dbName)
	if err != nil {
		return err
	}
	report, err := buildInspectCatalogReport(ctx)
	if err != nil {
		return err
	}

	ow := outputWriter{w: os.Stdout, asJSON: *jsonOut}
	return ow.emit(report, func(w io.Writer) {
		fmt.Fprintf(w, "Catalog for %s\n", report.Database)
		fmt.Fprintf(w, "Root: %s\n", report.RootDir)
		fmt.Fprintf(w, "DB dir: %s\n", report.DatabaseDir)
		fmt.Fprintf(w, "Catalog: %s\n", report.CatalogPath)
		fmt.Fprintf(w, "metrics=%d\n", report.Count)
		if report.Count == 0 {
			fmt.Fprintln(w, "no metrics registered")
			return
		}

		rows := make([][]string, 0, len(report.Metrics))
		for _, metric := range report.Metrics {
			rows = append(rows, []string{metric.Name, fmt.Sprintf("%d", metric.MetricID), metric.ValueType})
		}
		printAlignedTable(w,
			[]string{"name", "id", "type"},
			rows,
			map[int]bool{1: true},
		)
	})
}

func buildInspectCatalogReport(ctx dbContext) (inspectCatalogListReport, error) {
	report := inspectCatalogListReport{
		RootDir:     ctx.RootDir,
		Database:    ctx.Database,
		DatabaseDir: ctx.DatabaseDir,
		CatalogPath: ctx.CatalogPath,
		Metrics:     []inspectCatalogMetricReport{},
		GeneratedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}

	raw, err := os.ReadFile(ctx.CatalogPath)
	if err != nil {
		return inspectCatalogListReport{}, fmt.Errorf("read catalog %s: %w", ctx.CatalogPath, err)
	}
	var disk catalogDisk
	if err := json.Unmarshal(raw, &disk); err != nil {
		return inspectCatalogListReport{}, fmt.Errorf("parse catalog %s: %w", ctx.CatalogPath, err)
	}

	report.Metrics = make([]inspectCatalogMetricReport, 0, len(disk.Metrics))
	for _, metric := range disk.Metrics {
		report.Metrics = append(report.Metrics, inspectCatalogMetricReport{
			Name:      metric.Name,
			MetricID:  uint16(metric.MetricID),
			ValueType: inspectCatalogValueType(metric.ValueType),
		})
	}
	sort.Slice(report.Metrics, func(i, j int) bool {
		return report.Metrics[i].Name < report.Metrics[j].Name
	})
	report.Count = len(report.Metrics)
	return report, nil
}

// inspectCatalogValueType is kept as a one-line shim; engine.ValueTypeName is
// the canonical implementation used by every other consumer.
func inspectCatalogValueType(valueType byte) string { return engine.ValueTypeName(valueType) }
