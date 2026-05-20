package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/aymanhs/nanotdb/internal/engine"
)

func runCompact(args []string) error {
	fs := flag.NewFlagSet("compact", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	rootDir := fs.String("root", "", "root data directory that contains engine.toml")
	dbName := fs.String("db", "", "database name")
	part := fs.String("part", "", "sealed data partition to rewrite, such as 2026-05-20")
	jsonOut := fs.Bool("json", false, "emit JSON output")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("usage: nanocli compact --root <root-dir> --db <database> --part <partition> [--json]")
	}

	ctx, err := resolveDBContext(*rootDir, *dbName)
	if err != nil {
		return err
	}
	partName := strings.TrimSpace(*part)
	if partName == "" {
		return fmt.Errorf("--part is required")
	}

	eng, err := engine.OpenEngine(ctx.RootDir, 0)
	if err != nil {
		return err
	}
	defer eng.Close()

	report, err := eng.RecompactDataFile(ctx.Database, partName)
	if err != nil {
		return err
	}

	ow := outputWriter{w: os.Stdout, asJSON: *jsonOut}
	return ow.emit(report, func(w io.Writer) {
		fmt.Fprintf(w, "Recompacted %s/data-%s.dat (%d -> %d frames, %d -> %d bytes)\n", report.Database, report.Part, report.OldFrames, report.NewFrames, report.OldBytes, report.NewBytes)
	})
}
