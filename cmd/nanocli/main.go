package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/aymanhs/nanotdb/internal/engine"
)

func main() {
	args, logCfg, err := parseGlobalLoggingArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(2)
	}
	logger, closeLogger, err := newCLILogger(logCfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer func() {
		if err := closeLogger(); err != nil {
			fmt.Fprintf(os.Stderr, "error: close logger failed: %v\n", err)
		}
	}()
	slog.SetDefault(logger)

	if len(args) < 1 {
		printUsage(os.Stderr)
		os.Exit(2)
	}

	cmd := args[0]
	args = args[1:]

	var runErr error
	switch cmd {
	case "inspect":
		runErr = runInspect(args)
	case "import":
		runErr = runImport(args)
	case "csv2nlp":
		runErr = runCSV2NLP(args)
	case "rollup":
		runErr = runRollup(args)
	case "export":
		runErr = runExport(args)
	case "compact":
		runErr = runCompact(args)
	case "recover":
		runErr = runRecover(args)
	case "query":
		runErr = runQuery(args)
	case "build":
		runErr = runBuild(args)
	case "-h", "--help", "help":
		printUsage(os.Stdout)
		return
	default:
		printUsage(os.Stderr)
		fmt.Fprintf(os.Stderr, "\nunknown command: %s\n", cmd)
		os.Exit(2)
	}

	if runErr == nil {
		return
	}
	logger.Debug("nanocli command failed", "command", cmd, "error", runErr)
	fmt.Fprintf(os.Stderr, "error: %v\n", runErr)
	os.Exit(1)
}

func printUsage(w *os.File) {
	fmt.Fprintln(w, "nanocli - high-level NanoTDB operations")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  nanocli inspect db --root <root-dir> --db <database> [--verbose] [--json]")
	fmt.Fprintln(w, "  nanocli inspect catalog --root <root-dir> --db <database> [--json]")
	fmt.Fprintln(w, "  nanocli inspect dat --root <root-dir> --db <database> [--verbose] [--json]")
	fmt.Fprintln(w, "  nanocli inspect metric --root <root-dir> --db <database> [--verbose] [--json]")
	fmt.Fprintln(w, "  nanocli inspect wal --root <root-dir> --db <database> [--verbose] [--json]")
	fmt.Fprintln(w, "  nanocli import --root <root-dir> --in <line-protocol-file> [--db <database>] [--json]")
	fmt.Fprintln(w, "  nanocli csv2nlp --in-dir <csv-dir> --out <line-protocol-file> --db <database> [--meta <json>] [--json]")
	fmt.Fprintln(w, "  nanocli rollup --root <root-dir> [--db <source-database>] [--json]")
	fmt.Fprintln(w, "  nanocli export --root <root-dir> --db <database> [--out <line-protocol-file>] [--json]")
	fmt.Fprintln(w, "  nanocli compact --root <root-dir> --db <database> --part <partition> [--json]")
	fmt.Fprintln(w, "  nanocli recover --root <root-dir> --db <database> --part <partition> --out <path> [--json]")
	fmt.Fprintln(w, "  nanocli build metric --root <root-dir> --db <database> [--part <partition>] [--codec <name>] [--raw-ingest-action <keep|rename|delete>] [--verify] [--json]")
	fmt.Fprintln(w, "  nanocli query --root <root-dir> --db <database> [--metric <regex>] [--start <time|duration>] [--end <time>] [--metric-files <config|on|off>] [--format table|json]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Global logging flags:")
	fmt.Fprintln(w, "  --log-file <path>   append diagnostics to a file")
	fmt.Fprintln(w, "  --log-level <level> set diagnostics level: debug or trace (requires --log-file)")
}

type cliLogConfig struct {
	File  string
	Level string
}

func parseGlobalLoggingArgs(args []string) ([]string, cliLogConfig, error) {
	remaining := make([]string, 0, len(args))
	var cfg cliLogConfig

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--log-file":
			i++
			if i >= len(args) {
				return nil, cliLogConfig{}, fmt.Errorf("--log-file requires a value")
			}
			cfg.File = args[i]
		case strings.HasPrefix(arg, "--log-file="):
			cfg.File = strings.TrimPrefix(arg, "--log-file=")
		case arg == "--log-level":
			i++
			if i >= len(args) {
				return nil, cliLogConfig{}, fmt.Errorf("--log-level requires a value")
			}
			cfg.Level = args[i]
		case strings.HasPrefix(arg, "--log-level="):
			cfg.Level = strings.TrimPrefix(arg, "--log-level=")
		default:
			remaining = append(remaining, arg)
		}
	}

	if cfg.File == "" && cfg.Level != "" {
		return nil, cliLogConfig{}, fmt.Errorf("--log-level requires --log-file")
	}
	if cfg.File != "" && cfg.Level == "" {
		cfg.Level = engine.LogLevelDebug
	}
	return remaining, cfg, nil
}

func newCLILogger(cfg cliLogConfig) (*slog.Logger, func() error, error) {
	if cfg.File == "" {
		return slog.New(slog.NewTextHandler(io.Discard, nil)), func() error { return nil }, nil
	}
	logger, closeFn, err := engine.NewLogger(engine.EngineConfigLogging{Loggers: []engine.EngineConfigLogger{{Output: cfg.File, Level: cfg.Level}}})
	if err != nil {
		return nil, nil, err
	}
	return logger, closeFn, nil
}
