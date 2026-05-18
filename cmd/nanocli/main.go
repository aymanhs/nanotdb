package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		printUsage(os.Stderr)
		os.Exit(2)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	var err error
	switch cmd {
	case "inspect":
		err = runInspect(args)
	case "import":
		err = runImport(args)
	case "export":
		err = runExport(args)
	case "query":
		err = runQuery(args)
	case "-h", "--help", "help":
		printUsage(os.Stdout)
		return
	default:
		printUsage(os.Stderr)
		fmt.Fprintf(os.Stderr, "\nunknown command: %s\n", cmd)
		os.Exit(2)
	}

	if err == nil {
		return
	}
	fmt.Fprintf(os.Stderr, "error: %v\n", err)
	os.Exit(1)
}

func printUsage(w *os.File) {
	fmt.Fprintln(w, "nanocli - high-level NanoTDB operations")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  nanocli inspect db --root <root-dir> --db <database> [--verbose] [--json]")
	fmt.Fprintln(w, "  nanocli inspect dat --root <root-dir> --db <database> [--verbose] [--json]")
	fmt.Fprintln(w, "  nanocli inspect wal --root <root-dir> --db <database> [--verbose] [--json]")
	fmt.Fprintln(w, "  nanocli import --root <root-dir> --in <line-protocol-file> [--json]")
	fmt.Fprintln(w, "  nanocli export --root <root-dir> --db <database> [--out <line-protocol-file>] [--json]")
	fmt.Fprintln(w, "  nanocli query --root <root-dir> --db <database> --metric <regex> [--start <time>] [--end <time>] [--format table|json]")
}
