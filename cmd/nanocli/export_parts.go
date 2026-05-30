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

type exportPartsReport struct {
	InputPath   string                             `json:"input_path"`
	OutputFile  string                             `json:"output_file"`
	CatalogPath string                             `json:"catalog_path"`
	Files       []engine.OfflineLPExportFileReport `json:"files"`
	TotalLines  int64                              `json:"total_lines"`
	DurationMS  int64                              `json:"duration_ms"`
}

func runExportParts(args []string) error {
	fs := flag.NewFlagSet("export parts", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	inputPath := fs.String("in", "", "input file or directory containing sealed partition files")
	catalogPath := fs.String("catalog", "", "catalog snapshot used to resolve metric ids to names")
	outputPath := fs.String("out", "", "line protocol output file")
	inputKind := fs.String("input-kind", "auto", "input kind: metric, dat, or auto")
	withDB := fs.String("with-db", "", "optional database name prefix for output keys")
	jsonOut := fs.Bool("json", false, "emit JSON output")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("usage: nanocli export parts --in <file-or-dir> --catalog <catalog.json> --out <line-protocol-file> [--input-kind <metric|dat|auto>] [--with-db <name>] [--json]")
	}
	if *inputPath == "" {
		return fmt.Errorf("--in is required")
	}
	if *catalogPath == "" {
		return fmt.Errorf("--catalog is required")
	}
	if *outputPath == "" {
		return fmt.Errorf("--out is required")
	}

	if err := os.MkdirAll(filepath.Dir(*outputPath), 0755); err != nil {
		return err
	}
	outFile, err := os.Create(*outputPath)
	if err != nil {
		return err
	}
	defer outFile.Close()

	start := time.Now()
	report, err := engine.ExportOfflinePartsToLP(outFile, engine.OfflineLPExportOptions{
		InputPath:   *inputPath,
		CatalogPath: *catalogPath,
		InputKind:   *inputKind,
		WithDB:      *withDB,
	})
	if err != nil {
		return err
	}

	out := exportPartsReport{
		InputPath:   *inputPath,
		OutputFile:  *outputPath,
		CatalogPath: report.CatalogPath,
		Files:       report.Files,
		TotalLines:  report.TotalLines,
		DurationMS:  time.Since(start).Milliseconds(),
	}

	ow := outputWriter{w: os.Stdout, asJSON: *jsonOut}
	return ow.emit(out, func(w io.Writer) {
		fmt.Fprintf(w, "Exported %d lines to %s (%dms)\n", out.TotalLines, out.OutputFile, out.DurationMS)
		for _, fileReport := range out.Files {
			fmt.Fprintf(w, "%s kind=%s lines=%d\n", fileReport.Path, fileReport.Kind, fileReport.Lines)
		}
	})
}
