package main

import (
	"strings"
	"testing"

	"github.com/aymanhs/nanotdb/internal/engine"
)

func TestParseGlobalLoggingArgsRejectsLevelWithoutFile(t *testing.T) {
	_, _, err := parseGlobalLoggingArgs([]string{"inspect", "--log-level", "debug"})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestParseGlobalLoggingArgsDefaultsFileToDebug(t *testing.T) {
	args, cfg, err := parseGlobalLoggingArgs([]string{"inspect", "--log-file", "/tmp/nanocli.log", "--root", "/data", "--db", "metrics"})
	if err != nil {
		t.Fatalf("parseGlobalLoggingArgs failed: %v", err)
	}
	if cfg.File != "/tmp/nanocli.log" {
		t.Fatalf("log file mismatch: got=%q", cfg.File)
	}
	if cfg.Level != engine.LogLevelDebug {
		t.Fatalf("log level mismatch: got=%q want=%q", cfg.Level, engine.LogLevelDebug)
	}
	if len(args) != 5 {
		t.Fatalf("remaining args mismatch: got=%v", args)
	}
}

func TestParseGlobalLoggingArgsStripsExplicitLevel(t *testing.T) {
	args, cfg, err := parseGlobalLoggingArgs([]string{"--log-level=trace", "query", "--log-file=/tmp/nanocli.log", "--root", "/data"})
	if err != nil {
		t.Fatalf("parseGlobalLoggingArgs failed: %v", err)
	}
	if cfg.File != "/tmp/nanocli.log" {
		t.Fatalf("log file mismatch: got=%q", cfg.File)
	}
	if cfg.Level != engine.LogLevelTrace {
		t.Fatalf("log level mismatch: got=%q want=%q", cfg.Level, engine.LogLevelTrace)
	}
	if len(args) != 3 || args[0] != "query" {
		t.Fatalf("remaining args mismatch: got=%v", args)
	}
}

func TestRunBuildRejectsUnknownSubcommand(t *testing.T) {
	err := runBuild([]string{"mystery"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "unknown build command") {
		t.Fatalf("unexpected error: %v", err)
	}
}
