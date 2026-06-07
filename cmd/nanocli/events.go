package main

// `nanocli events` is an OFFLINE reader for the events stored under a
// database's data directory.
//
// It NEVER opens the engine and NEVER talks to a running server. It
// reads the sealed `events-<partition>.dat` frames directly using the
// engine's exported decode primitives (WalkEventsFileHeaders +
// CollectEventsFrame + LoadEventCatalog).
//
// Why offline-only:
//
//   - The previous engine.OpenEngine() implementation emitted a
//     nanotdb.engine.started event on open and a
//     nanotdb.engine.shutdown.clean on close, so every nanocli
//     invocation silently grew the internal-events catalog by two
//     records.
//   - Opening the engine also re-replays the events WAL into memory.
//     Repeated runs on the same root cumulatively re-played those
//     records each time and eventually drifted the catalog (the
//     "event id N already assigned" failure mode you hit on the pi).
//
// Disk-only is the right architectural fit: nanocli is for inspecting
// files when no live system is using them. The running server is the
// source of truth for live state and is what the web UI's Internal
// Events tab and `event_log` widget query.
//
// We deliberately read SEALED events-*.dat files only. The events
// WAL holds records that the engine has not yet flushed to a page;
// for those, `nanocli inspect events-wal` already exists and decodes
// the WAL frame-by-frame (with the catalog supplied).

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/aymanhs/nanotdb/internal/engine"
)

type eventsQueryRow struct {
	Name      string `json:"name"`
	ID        uint16 `json:"id"`
	Timestamp int64  `json:"timestamp_ns"`
	UTC       string `json:"timestamp_utc"`
	ValueType string `json:"value_type"`
	Value     string `json:"value,omitempty"`
	Payload   string `json:"payload,omitempty"`
}

type eventsAggregateRow struct {
	Timestamp int64  `json:"timestamp_ns"`
	UTC       string `json:"timestamp_utc"`
	Count     int64  `json:"count"`
}

type eventsReport struct {
	RootDir      string               `json:"root_dir"`
	Database     string               `json:"database"`
	NameFilter   string               `json:"name_filter,omitempty"`
	Start        int64                `json:"start_ns"`
	End          int64                `json:"end_ns"`
	Limit        int                  `json:"limit,omitempty"`
	Aggregate    string               `json:"aggregate,omitempty"`
	Window       string               `json:"window,omitempty"`
	Rows         []eventsQueryRow     `json:"rows,omitempty"`
	Buckets      []eventsAggregateRow `json:"buckets,omitempty"`
	RowCount     int                  `json:"row_count"`
	DurationMS   int64                `json:"duration_ms"`
	OutputFormat string               `json:"output_format"`
}

func runEvents(args []string) error {
	fs := flag.NewFlagSet("events", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	rootDir := fs.String("root", "", "root data directory that contains engine.toml")
	dbName := fs.String("db", "", "database name")
	nameFilter := fs.String("name", "", "optional event name filter (exact or wildcard pattern)")
	startText := fs.String("start", "", "optional range start time (RFC3339Nano, unix seconds, unix nanos, or relative duration like 2m)")
	endText := fs.String("end", "", "optional range end time (defaults to now)")
	limit := fs.Int("limit", 100, "maximum events to return for non-aggregate mode")
	aggregate := fs.String("aggregate", "", "optional aggregate mode; currently supports count")
	windowText := fs.String("window", "", "aggregate bucket window (for example: 5m, 1h)")
	format := fs.String("format", "table", "output format: table or json")
	timestampUnit := fs.String("timestamp-unit", "ns", "unit for bare numeric --start/--end values: ns, us, ms, s")
	jsonOut := fs.Bool("json", false, "alias for --format json")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("usage: nanocli events --root <root-dir> --db <database> [--name <pattern>] [--start <time|duration>] [--end <time>] [--limit <n>] [--aggregate count --window <duration>] [--format table|json]")
	}
	if *limit < 1 {
		return fmt.Errorf("--limit must be >= 1")
	}
	tsUnit, err := engine.NormalizeTimestampUnit(*timestampUnit)
	if err != nil {
		return fmt.Errorf("invalid --timestamp-unit: %w", err)
	}
	ctx, err := resolveDBContext(*rootDir, *dbName)
	if err != nil {
		return err
	}

	fromTS := engine.Timestamp(0)
	if strings.TrimSpace(*startText) != "" {
		fromTS, err = parseQueryTimeText(*startText, true, tsUnit)
		if err != nil {
			return fmt.Errorf("invalid --start: %w", err)
		}
	}
	toTS := engine.Timestamp(time.Now().UnixNano())
	if strings.TrimSpace(*endText) != "" {
		toTS, err = parseQueryTimeText(*endText, true, tsUnit)
		if err != nil {
			return fmt.Errorf("invalid --end: %w", err)
		}
	}
	if toTS < fromTS {
		return fmt.Errorf("--end must be >= --start")
	}

	outFormat := strings.ToLower(strings.TrimSpace(*format))
	if *jsonOut {
		outFormat = "json"
	}
	if outFormat != "table" && outFormat != "json" {
		return fmt.Errorf("invalid --format %q (expected table or json)", outFormat)
	}

	agg := strings.ToLower(strings.TrimSpace(*aggregate))
	aggMode := agg != "" || strings.TrimSpace(*windowText) != ""
	if aggMode {
		if agg != "count" {
			return fmt.Errorf("events aggregate supports only count")
		}
		if strings.TrimSpace(*windowText) == "" {
			return fmt.Errorf("aggregate queries require --window")
		}
	}

	started := time.Now()
	report := eventsReport{
		RootDir:      ctx.RootDir,
		Database:     ctx.Database,
		NameFilter:   strings.TrimSpace(*nameFilter),
		Start:        int64(fromTS),
		End:          int64(toTS),
		Limit:        *limit,
		Aggregate:    agg,
		OutputFormat: outFormat,
	}

	// Resolve the name filter into an exact match or a path.Match
	// wildcard. Stays consistent with how the HTTP layer interprets
	// the same param.
	exactName := strings.TrimSpace(*nameFilter)
	pattern := ""
	queryName := exactName
	if strings.ContainsAny(exactName, "*?[") {
		pattern = exactName
		queryName = ""
	}

	// Read the events catalog once; required to decode page frames.
	catPath := filepath.Join(ctx.DatabaseDir, "events.json")
	cat, err := engine.LoadEventCatalog(catPath)
	if err != nil {
		// Missing catalog means there are no events for this db
		// yet; nothing to read. We don't treat this as an error so
		// "nanocli events --db internal" on a fresh install just
		// produces an empty result.
		if os.IsNotExist(err) {
			return finishEmpty(&report, outFormat, started)
		}
		return fmt.Errorf("load events catalog %s: %w", catPath, err)
	}

	keep := func(name string) bool {
		if queryName != "" {
			return name == queryName
		}
		if pattern != "" {
			ok, mErr := path.Match(pattern, name)
			return mErr == nil && ok
		}
		return true
	}

	if aggMode {
		window, werr := engine.ParseDuration(strings.TrimSpace(*windowText))
		if werr != nil || window <= 0 {
			return fmt.Errorf("invalid --window: %w", werr)
		}
		report.Window = window.String()
		windowNS := int64(window)
		counts := map[int64]int64{}
		err = readEventsFromSealedPages(ctx.DatabaseDir, cat, fromTS, toTS, keep, func(ev decodedEvent) error {
			ts := int64(ev.TS)
			bucketStart := ts - (ts % windowNS)
			if ts < 0 && ts%windowNS != 0 {
				bucketStart -= windowNS
			}
			counts[bucketStart]++
			return nil
		})
		if err != nil {
			return err
		}
		keys := make([]int64, 0, len(counts))
		for k := range counts {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
		report.Buckets = make([]eventsAggregateRow, 0, len(keys))
		for _, k := range keys {
			report.Buckets = append(report.Buckets, eventsAggregateRow{
				Timestamp: k,
				UTC:       engine.FormatTimestamp(engine.Timestamp(k)),
				Count:     counts[k],
			})
		}
		report.RowCount = len(report.Buckets)
	} else {
		// Collect every matching event first; sort newest-first; then
		// apply the limit. Sealed pages are append-ordered per-name
		// but not globally newest-first, so we have to materialize the
		// candidates before trimming.
		all := make([]decodedEvent, 0, *limit*2)
		err = readEventsFromSealedPages(ctx.DatabaseDir, cat, fromTS, toTS, keep, func(ev decodedEvent) error {
			all = append(all, ev)
			return nil
		})
		if err != nil {
			return err
		}
		sort.Slice(all, func(i, j int) bool { return all[i].TS > all[j].TS })
		if len(all) > *limit {
			all = all[:*limit]
		}
		rows := make([]eventsQueryRow, 0, len(all))
		for _, ev := range all {
			row := eventsQueryRow{
				Name:      ev.Name,
				ID:        uint16(ev.EventID),
				Timestamp: int64(ev.TS),
				UTC:       engine.FormatTimestamp(ev.TS),
				ValueType: engine.EventValueTypeName(ev.ValueType),
			}
			switch ev.ValueType {
			case engine.Int32Sample:
				row.Value = strconv.FormatInt(int64(ev.Int32), 10)
			case engine.Float32Sample:
				row.Value = strconv.FormatFloat(float64(ev.Float32), 'f', -1, 32)
			}
			if len(ev.Payload) > 0 {
				row.Payload = string(ev.Payload)
			}
			rows = append(rows, row)
		}
		report.Rows = rows
		report.RowCount = len(rows)
	}

	report.DurationMS = time.Since(started).Milliseconds()
	return renderEventsReport(&report, outFormat)
}

// decodedEvent is a per-record decoded view of one entry in a sealed
// events page. Built locally so the WAL-vs-page distinction stays
// inside this file.
type decodedEvent struct {
	Name      string
	EventID   engine.EventID
	TS        engine.Timestamp
	ValueType byte
	Int32     int32
	Float32   float32
	Payload   []byte
}

// readEventsFromSealedPages walks every events-*.dat under dbDir,
// decodes the frames whose time window intersects [fromTS, toTS],
// and calls visit for each record matching `keep`. Errors on
// individual files are propagated; we don't silently swallow.
//
// Files are walked in lexicographic order (partition keys sort
// chronologically by construction); records within a frame are
// emitted in their on-disk append order. The caller is responsible
// for any final sort.
func readEventsFromSealedPages(
	dbDir string,
	cat *engine.EventCatalog,
	fromTS, toTS engine.Timestamp,
	keep func(name string) bool,
	visit func(decodedEvent) error,
) error {
	paths, err := filepath.Glob(filepath.Join(dbDir, "events-*.dat"))
	if err != nil {
		return err
	}
	sort.Strings(paths)
	for _, p := range paths {
		offsets := make([]int64, 0, 8)
		_, err := engine.WalkEventsFileHeaders(p, func(hdr engine.EventsFrameHeader) error {
			// Frame is in range iff [StartTime, EndTime] overlaps
			// the requested window. Inclusive on both ends.
			if hdr.EndTime < fromTS || hdr.StartTime > toTS {
				return nil
			}
			offsets = append(offsets, hdr.Offset)
			return nil
		})
		if err != nil {
			return fmt.Errorf("walk %s: %w", p, err)
		}
		for _, off := range offsets {
			page, err := engine.CollectEventsFrame(p, off, cat)
			if err != nil {
				return fmt.Errorf("decode %s offset %d: %w", p, off, err)
			}
			for i := 0; i < len(page.EventIDs); i++ {
				ts := page.Times[i]
				if ts < fromTS || ts > toTS {
					continue
				}
				name, _, ok := cat.GetEventByID(page.EventIDs[i])
				if !ok {
					continue
				}
				if keep != nil && !keep(name) {
					continue
				}
				ev := decodedEvent{
					Name:      name,
					EventID:   page.EventIDs[i],
					TS:        ts,
					ValueType: page.ValueTypes[i],
				}
				switch page.ValueTypes[i] {
				case engine.Int32Sample:
					ev.Int32 = int32(page.ValuesRaw[i])
				case engine.Float32Sample:
					ev.Float32 = math.Float32frombits(page.ValuesRaw[i])
				}
				if i < len(page.Payloads) && len(page.Payloads[i]) > 0 {
					ev.Payload = page.Payloads[i]
				}
				if err := visit(ev); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func finishEmpty(report *eventsReport, outFormat string, started time.Time) error {
	report.DurationMS = time.Since(started).Milliseconds()
	return renderEventsReport(report, outFormat)
}

func renderEventsReport(report *eventsReport, outFormat string) error {
	if outFormat == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	if report.Aggregate == "count" || len(report.Buckets) > 0 {
		fmt.Fprintln(tw, "TS_NS\tTS_UTC\tCOUNT")
		for _, b := range report.Buckets {
			fmt.Fprintf(tw, "%d\t%s\t%d\n", b.Timestamp, b.UTC, b.Count)
		}
	} else {
		fmt.Fprintln(tw, "NAME\tID\tTS_NS\tTS_UTC\tTYPE\tVALUE\tPAYLOAD")
		for _, r := range report.Rows {
			fmt.Fprintf(tw, "%s\t%d\t%d\t%s\t%s\t%s\t%s\n", r.Name, r.ID, r.Timestamp, r.UTC, r.ValueType, r.Value, r.Payload)
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "rows=%d duration_ms=%d\n", report.RowCount, report.DurationMS)
	return nil
}
