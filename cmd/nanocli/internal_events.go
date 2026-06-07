package main

// `nanocli internal-events tail` — OFFLINE reader for internal events
// stored in a database's events files.
//
// Why no `catalog`, `groups`, or `set` subcommands here: those used to
// talk to a running nanotdb server over HTTP. nanocli is for offline
// disk inspection only; live state (which groups are on, etc.)
// belongs to the running engine and is exposed via the Engine UI's
// Internal Events tab and the /api/v1/internal-events/{catalog,groups}
// HTTP endpoints. If you need to flip a group at runtime, hit the
// HTTP endpoint directly (curl, or via the engine UI).
//
// The `tail` subcommand is a thin wrapper around the offline events
// reader (cmd/nanocli/events.go): it forces --db internal and, when
// --group is set, expands the group to the static set of event names
// it contains. The group → names mapping comes from
// engine.InternalEventNamesForGroup, which reads a static registry
// and does NOT depend on a running engine.

import (
	"fmt"
	"strings"

	"github.com/aymanhs/nanotdb/internal/engine"
)

func runInternalEvents(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: nanocli internal-events tail --root <root-dir> [--group <group>] [--name <pattern>] [--start <t|d>] [--end <t>] [--limit <n>] [--format table|json]")
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "tail":
		return runInternalEventsTail(rest)
	default:
		return fmt.Errorf("unknown internal-events subcommand %q (only \"tail\" is supported offline; for live catalog/groups/set, use the engine UI or hit /api/v1/internal-events/* directly)", sub)
	}
}

func runInternalEventsTail(args []string) error {
	// Walks args twice: first to extract --group (which we resolve
	// against the static registry), then to forward everything else
	// to runEvents with --db internal prepended.
	group := ""
	out := []string{"--db", "internal"}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--group" || a == "-group":
			if i+1 >= len(args) {
				return fmt.Errorf("--group requires a value")
			}
			group = args[i+1]
			i++
		case strings.HasPrefix(a, "--group="):
			group = strings.TrimPrefix(a, "--group=")
		case strings.HasPrefix(a, "-group="):
			group = strings.TrimPrefix(a, "-group=")
		default:
			out = append(out, a)
		}
	}
	if group == "" {
		return runEvents(out)
	}

	names := engine.InternalEventNamesForGroup(group)
	if len(names) == 0 {
		return fmt.Errorf("unknown internal-events group %q", group)
	}
	// One pass per event name. The offline reader is cheap (header
	// scan + decode only matching frames), and the alternative
	// "merge in one read" path would require teaching runEvents
	// about multi-name filters. Per-name output blocks also let
	// operators see at a glance which event names in the group
	// had records in the window and which were empty.
	for _, name := range names {
		argsForName := append([]string(nil), out...)
		argsForName = append(argsForName, "--name", name)
		if err := runEvents(argsForName); err != nil {
			return err
		}
	}
	return nil
}
