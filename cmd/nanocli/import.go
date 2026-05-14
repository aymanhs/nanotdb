package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"aymanhs/nanotdb/internal/engine"
)

type importReport struct {
	RootDir       string   `json:"root_dir"`
	InputFile     string   `json:"input_file"`
	TotalLines    int      `json:"total_lines"`
	ImportedLines int      `json:"imported_lines"`
	SkippedLines  int      `json:"skipped_lines"`
	DatabasesSeen []string `json:"databases_seen"`
	DurationMS    int64    `json:"duration_ms"`
}

func runImport(args []string) error {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	rootDir := fs.String("root", "", "root data directory that contains engine.toml")
	inputPath := fs.String("in", "", "line protocol input file")
	jsonOut := fs.Bool("json", false, "emit JSON output")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("usage: nanocli import --root <root-dir> --in <line-protocol-file> [--json]")
	}
	if *inputPath == "" {
		return fmt.Errorf("--in is required")
	}

	rootAbs, _, err := resolveRootDir(*rootDir)
	if err != nil {
		return err
	}

	in, err := os.Open(*inputPath)
	if err != nil {
		return err
	}
	defer in.Close()

	started := time.Now()
	eng, err := engine.OpenEngine(rootAbs, 0)
	if err != nil {
		return err
	}
	defer eng.Close()

	total, imported, skipped, dbsSeen, err := importLineProtocol(eng, in)
	if err != nil {
		return err
	}
	report := importReport{
		RootDir:       rootAbs,
		InputFile:     *inputPath,
		TotalLines:    total,
		ImportedLines: imported,
		SkippedLines:  skipped,
		DatabasesSeen: dbsSeen,
		DurationMS:    time.Since(started).Milliseconds(),
	}

	ow := outputWriter{w: os.Stdout, asJSON: *jsonOut}
	return ow.emit(report, func(w io.Writer) {
		fmt.Fprintf(w, "Imported %d/%d lines from %s (%d skipped, %dms)\n", report.ImportedLines, report.TotalLines, report.InputFile, report.SkippedLines, report.DurationMS)
		if len(report.DatabasesSeen) > 0 {
			fmt.Fprintf(w, "Databases seen: %s\n", strings.Join(report.DatabasesSeen, ", "))
		}
	})
}

func importLineProtocol(eng *engine.Engine, r io.Reader) (total int, imported int, skipped int, databases []string, err error) {
	s := bufio.NewScanner(r)
	lineNo := 0
	dbSet := make(map[string]struct{})
	for s.Scan() {
		lineNo++
		total++
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			skipped++
			continue
		}
		dbName, err := extractDatabaseName(line)
		if err != nil {
			return total, imported, skipped, nil, fmt.Errorf("line %d: %w", lineNo, err)
		}
		if err := eng.AddLine(line); err != nil {
			return total, imported, skipped, nil, fmt.Errorf("line %d: %w", lineNo, err)
		}
		dbSet[dbName] = struct{}{}
		imported++
	}
	if err := s.Err(); err != nil {
		return total, imported, skipped, nil, err
	}
	databases = make([]string, 0, len(dbSet))
	for name := range dbSet {
		databases = append(databases, name)
	}
	sort.Strings(databases)
	return total, imported, skipped, databases, nil
}

func extractDatabaseName(line string) (string, error) {
	parts := strings.Fields(line)
	if len(parts) < 2 {
		return "", fmt.Errorf("invalid line protocol")
	}
	key := parts[0]
	idx := strings.Index(key, "/")
	if idx <= 0 || idx == len(key)-1 {
		return "", fmt.Errorf("expected metric key in form db/metric")
	}
	lineDB := strings.TrimSpace(key[:idx])
	if lineDB == "" {
		return "", fmt.Errorf("database name cannot be empty")
	}
	return lineDB, nil
}
