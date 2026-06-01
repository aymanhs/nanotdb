package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/aymanhs/nanotdb/internal/engine"
)

type importPartsReport struct {
	InputFile   string                             `json:"input_file"`
	OutputDir   string                             `json:"output_dir"`
	CatalogPath string                             `json:"catalog_path"`
	CatalogMode string                             `json:"catalog_mode"`
	TotalLines  int                                `json:"total_lines"`
	Imported    int                                `json:"imported_lines"`
	Skipped     int                                `json:"skipped_lines"`
	Partitions  []engine.OfflineLPImportPartReport `json:"partitions"`
	DurationMS  int64                              `json:"duration_ms"`
}

func runImportParts(args []string) error {
	fs := flag.NewFlagSet("import parts", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	inputPath := fs.String("in", "", "line protocol input file")
	partitionMode := fs.String("partition-mode", "", "partition mode: day, month, or year")
	catalogPath := fs.String("catalog", "", "optional existing catalog snapshot to validate against")
	outDir := fs.String("out-dir", ".", "output folder for catalog.json and metric-*.dat files")
	timestampUnit := fs.String("timestamp-unit", "ns", "unit for bare numeric timestamps: ns, us, ms, s")
	jsonOut := fs.Bool("json", false, "emit JSON output")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("usage: nanocli import parts --in <line-protocol-file> --partition-mode <day|month|year> [--catalog <catalog.json>] [--out-dir <dir>] [--timestamp-unit ns|us|ms|s] [--json]")
	}
	if *inputPath == "" {
		return fmt.Errorf("--in is required")
	}
	if *partitionMode == "" {
		return fmt.Errorf("--partition-mode is required")
	}

	in, err := os.Open(*inputPath)
	if err != nil {
		return err
	}
	defer in.Close()

	start := time.Now()
	rootOut := *outDir
	if rootOut == "" {
		rootOut = "."
	}
	rootOut, err = filepath.Abs(rootOut)
	if err != nil {
		return err
	}

	report, err := engine.ImportOfflineLPToMetricParts(in, engine.OfflineLPImportOptions{
		CatalogPath:   *catalogPath,
		OutDir:        rootOut,
		PartitionMode: *partitionMode,
		TimestampUnit: *timestampUnit,
	})
	if err != nil {
		return err
	}

	out := importPartsReport{
		InputFile:   *inputPath,
		OutputDir:   rootOut,
		CatalogPath: report.CatalogPath,
		CatalogMode: report.CatalogMode,
		TotalLines:  report.TotalLines,
		Imported:    report.ImportedLines,
		Skipped:     report.SkippedLines,
		Partitions:  report.Partitions,
		DurationMS:  time.Since(start).Milliseconds(),
	}

	ow := outputWriter{w: os.Stdout, asJSON: *jsonOut}
	return ow.emit(out, func(w io.Writer) {
		fmt.Fprintf(w, "Imported %d/%d lines from %s (%d skipped, %dms)\n", out.Imported, out.TotalLines, out.InputFile, out.Skipped, out.DurationMS)
		fmt.Fprintf(w, "Catalog: %s (%s)\n", out.CatalogPath, out.CatalogMode)
		for _, part := range out.Partitions {
			fmt.Fprintf(w, "%s metrics=%d points=%d\n", part.MetricFilePath, part.Metrics, part.Points)
		}
	})
}
