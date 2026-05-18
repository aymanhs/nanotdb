package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/aymanhs/nanotdb/internal/engine"
)

func runRollup(args []string) error {
	fs := flag.NewFlagSet("rollup", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	rootDir := fs.String("root", "", "root data directory that contains engine.toml")
	sourceDB := fs.String("db", "", "optional rollup source database to backfill")
	jsonOut := fs.Bool("json", false, "emit JSON output")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("usage: nanocli rollup --root <root-dir> [--db <source-database>] [--json]")
	}

	rootAbs, _, err := resolveRootDir(*rootDir)
	if err != nil {
		return err
	}

	eng, err := engine.OpenEngine(rootAbs, 0)
	if err != nil {
		return err
	}
	defer eng.Close()

	requested := make([]string, 0, 1)
	if db := strings.TrimSpace(*sourceDB); db != "" {
		db, err = normalizeDatabaseName(db)
		if err != nil {
			return err
		}
		requested = append(requested, db)
	}

	report, err := eng.BackfillRollups(requested)
	if err != nil {
		return err
	}

	ow := outputWriter{w: os.Stdout, asJSON: *jsonOut}
	return ow.emit(report, func(w io.Writer) {
		if len(report.SourceDatabases) == 0 {
			fmt.Fprintln(w, "No rollup sources found.")
			return
		}
		fmt.Fprintf(w, "Backfilled %d rollup source DBs into %d destination DBs in %d passes.\n", len(report.SourceDatabases), len(report.DestinationDatabases), report.ReplayPasses)
		fmt.Fprintf(w, "Cleared %d checkpoint files, %d data files, %d WAL files, %d catalog files.\n", len(report.ClearedCheckpointFiles), len(report.ClearedDataFiles), len(report.ClearedWALFiles), len(report.ClearedCatalogFiles))
	})
}
