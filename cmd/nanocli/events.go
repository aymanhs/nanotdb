package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
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
	eng, err := engine.OpenEngine(ctx.RootDir, 0)
	if err != nil {
		return err
	}
	defer eng.Close()

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

	if aggMode {
		window, err := engine.ParseDuration(strings.TrimSpace(*windowText))
		if err != nil || window <= 0 {
			return fmt.Errorf("invalid --window: %w", err)
		}
		report.Window = window.String()
		windowNS := int64(window)
		counts := map[int64]int64{}
		exactName := strings.TrimSpace(*nameFilter)
		queryName := exactName
		pattern := ""
		if strings.ContainsAny(exactName, "*?[") {
			pattern = exactName
			queryName = ""
		}
		err = eng.QueryEvents(ctx.Database, queryName, fromTS, toTS, func(ev engine.EventQueryResult) error {
			if pattern != "" {
				ok, mErr := path.Match(pattern, ev.Name)
				if mErr != nil || !ok {
					return nil
				}
			}
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
			report.Buckets = append(report.Buckets, eventsAggregateRow{Timestamp: k, UTC: engine.FormatTimestamp(engine.Timestamp(k)), Count: counts[k]})
		}
		report.RowCount = len(report.Buckets)
	} else {
		exactName := strings.TrimSpace(*nameFilter)
		queryName := exactName
		pattern := ""
		if strings.ContainsAny(exactName, "*?[") {
			pattern = exactName
			queryName = ""
		}
		rows := make([]eventsQueryRow, 0, *limit)
		err = eng.QueryEvents(ctx.Database, queryName, fromTS, toTS, func(ev engine.EventQueryResult) error {
			if pattern != "" {
				ok, mErr := path.Match(pattern, ev.Name)
				if mErr != nil || !ok {
					return nil
				}
			}
			if len(rows) >= *limit {
				return io.EOF
			}
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
			return nil
		})
		if err != nil && err != io.EOF {
			return err
		}
		report.Rows = rows
		report.RowCount = len(rows)
	}

	report.DurationMS = time.Since(started).Milliseconds()

	if outFormat == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	if aggMode {
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
