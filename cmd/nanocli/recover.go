package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/aymanhs/nanotdb/internal/engine"
)

func runRecover(args []string) error {
	fs := flag.NewFlagSet("recover", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	rootDir := fs.String("root", "", "root data directory that contains engine.toml")
	dbName := fs.String("db", "", "database name")
	part := fs.String("part", "", "sealed data partition to recover, such as 2026-05-19")
	outPath := fs.String("out", "", "output path for the recovered data file")
	jsonOut := fs.Bool("json", false, "emit JSON output")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("usage: nanocli recover --root <root-dir> --db <database> --part <partition> --out <path> [--json]")
	}

	ctx, err := resolveDBContext(*rootDir, *dbName)
	if err != nil {
		return err
	}
	partName := strings.TrimSpace(*part)
	if partName == "" {
		return fmt.Errorf("--part is required")
	}
	output := strings.TrimSpace(*outPath)
	if output == "" {
		return fmt.Errorf("--out is required")
	}
	if !filepath.IsAbs(output) {
		output = filepath.Join(ctx.RootDir, output)
	}

	eng, err := engine.OpenEngine(ctx.RootDir, 0)
	if err != nil {
		return err
	}
	defer eng.Close()

	report, err := eng.RecoverDataFile(ctx.Database, partName, output)
	if err != nil {
		return err
	}

	ow := outputWriter{w: os.Stdout, asJSON: *jsonOut}
	return ow.emit(report, func(w io.Writer) {
		fmt.Fprintf(w, "Recovered %s/data-%s.dat to %s (%d frames, %d records, %d skipped bytes)\n", report.Database, report.Part, report.OutputPath, report.RecoveredFrames, report.RecoveredRecords, report.SkippedBytes)
	})
}