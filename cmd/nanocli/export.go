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

type exportReport struct {
	RootDir    string `json:"root_dir"`
	Database   string `json:"database"`
	OutputFile string `json:"output_file"`
	Bytes      int64  `json:"bytes"`
	DurationMS int64  `json:"duration_ms"`
}

func runExport(args []string) error {
	if len(args) > 0 && args[0] == "parts" {
		return runExportParts(args[1:])
	}

	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	rootDir := fs.String("root", "", "root data directory that contains engine.toml")
	dbName := fs.String("db", "", "database name")
	outputPath := fs.String("out", "", "line protocol output file")
	jsonOut := fs.Bool("json", false, "emit JSON output")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("usage: nanocli export --root <root-dir> --db <database> [--out <line-protocol-file>] [--json]")
	}

	ctx, err := resolveDBContext(*rootDir, *dbName)
	if err != nil {
		return err
	}

	started := time.Now()
	eng, err := engine.OpenEngine(ctx.RootDir, 0)
	if err != nil {
		return err
	}
	defer eng.Close()

	if *outputPath == "" {
		if *jsonOut {
			return fmt.Errorf("--json requires --out when exporting to stdout")
		}
		return eng.ExportToWriter(ctx.Database, os.Stdout)
	}

	if err := os.MkdirAll(filepath.Dir(*outputPath), 0755); err != nil {
		return err
	}

	if err := eng.ExportFile(ctx.Database, *outputPath); err != nil {
		return err
	}
	st, err := os.Stat(*outputPath)
	if err != nil {
		return err
	}
	report := exportReport{
		RootDir:    ctx.RootDir,
		Database:   ctx.Database,
		OutputFile: *outputPath,
		Bytes:      st.Size(),
		DurationMS: time.Since(started).Milliseconds(),
	}

	ow := outputWriter{w: os.Stdout, asJSON: *jsonOut}
	return ow.emit(report, func(w io.Writer) {
		fmt.Fprintf(w, "Exported %s to %s (%d bytes, %dms)\n", report.Database, report.OutputFile, report.Bytes, report.DurationMS)
	})
}
