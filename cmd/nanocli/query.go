package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/aymanhs/nanotdb/internal/engine"
)

type queryRow struct {
	Metric       string `json:"metric"`
	Aggregate    string `json:"aggregate,omitempty"`
	Window       string `json:"window,omitempty"`
	StartNS      int64  `json:"start_ns,omitempty"`
	StartUTC     string `json:"start_utc,omitempty"`
	EndNS        int64  `json:"end_ns,omitempty"`
	EndUTC       string `json:"end_utc,omitempty"`
	TimestampNS  int64  `json:"timestamp_ns"`
	TimestampUTC string `json:"timestamp_utc"`
	ValueType    string `json:"value_type"`
	Value        string `json:"value"`
}

type queryReport struct {
	RootDir      string     `json:"root_dir"`
	Database     string     `json:"database"`
	MetricRegex  string     `json:"metric_regex"`
	Start        *int64     `json:"start_ns,omitempty"`
	End          *int64     `json:"end_ns,omitempty"`
	OutputFormat string     `json:"output_format"`
	Rows         []queryRow `json:"rows"`
	RowCount     int        `json:"row_count"`
	DurationMS   int64      `json:"duration_ms"`
}

var queryTimeNow = time.Now

func runQuery(args []string) error {
	fs := flag.NewFlagSet("query", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	rootDir := fs.String("root", "", "root data directory that contains engine.toml")
	dbName := fs.String("db", "", "database name")
	metricRegex := fs.String("metric", ".*", "optional regex applied to metric names; defaults to all metrics")
	startText := fs.String("start", "", "optional range start time (RFC3339Nano, unix seconds, unix nanos, or relative duration like 2m for now-2m)")
	endText := fs.String("end", "", "optional range end time (RFC3339Nano, unix seconds, unix nanos, or relative duration like 30s for now-30s); defaults to now when --start is set")
	aggregateText := fs.String("aggregate", "", "optional comma-separated aggregate list: min,max,sum,avg,count")
	windowText := fs.String("window", "", "aggregate bucket window (for example: 5m, 1h)")
	metricFiles := fs.String("metric-files", "config", "metric file routing mode: config, on, or off")
	format := fs.String("format", "table", "output format: table or json")
	timestampUnit := fs.String("timestamp-unit", "ns", "unit for bare numeric --start/--end values: ns, us, ms, s")
	jsonOut := fs.Bool("json", false, "alias for --format json")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("usage: nanocli query --root <root-dir> --db <database> [--metric <regex>] [--start <time|duration>] [--end <time>] [--aggregate <list> --window <duration>] [--format table|json] [--timestamp-unit ns|us|ms|s]")
	}
	tsUnit, err := engine.NormalizeTimestampUnit(*timestampUnit)
	if err != nil {
		return fmt.Errorf("invalid --timestamp-unit: %w", err)
	}

	ctx, err := resolveDBContext(*rootDir, *dbName)
	if err != nil {
		return err
	}

	rx, err := regexp.Compile(*metricRegex)
	if err != nil {
		return fmt.Errorf("invalid --metric regex: %w", err)
	}

	var fromTS, toTS engine.Timestamp
	var useRange bool
	if strings.TrimSpace(*startText) != "" {
		useRange = true
		fromTS, err = parseQueryTimeText(*startText, true, tsUnit)
		if err != nil {
			return fmt.Errorf("invalid --start: %w", err)
		}
	}
	if strings.TrimSpace(*endText) != "" {
		useRange = true
		toTS, err = parseQueryTimeText(*endText, true, tsUnit)
		if err != nil {
			return fmt.Errorf("invalid --end: %w", err)
		}
	}
	if useRange {
		if strings.TrimSpace(*startText) == "" {
			fromTS = 0
		}
		if strings.TrimSpace(*endText) == "" {
			// Unbounded end: include future-dated samples (clock skew, scheduled
			// backfill writes, replication lag). Defaulting to now() silently
			// dropped them.
			toTS = engine.Timestamp(math.MaxInt64)
		}
		if toTS < fromTS {
			return fmt.Errorf("--end must be >= --start")
		}
	}

	aggregateMode := strings.TrimSpace(*aggregateText) != "" || strings.TrimSpace(*windowText) != ""
	if aggregateMode {
		if strings.TrimSpace(*aggregateText) == "" || strings.TrimSpace(*windowText) == "" {
			return fmt.Errorf("aggregate queries require both --aggregate and --window")
		}
		if !useRange || strings.TrimSpace(*startText) == "" {
			return fmt.Errorf("aggregate queries require --start")
		}
	}

	var aggregateNames []string
	var aggregateWindow time.Duration
	if aggregateMode {
		aggregateWindow, err = engine.ParseDuration(strings.TrimSpace(*windowText))
		if err != nil || aggregateWindow <= 0 {
			return fmt.Errorf("invalid --window: %w", err)
		}
		aggregateNames = splitCSVList(*aggregateText)
	}

	outFormat := strings.ToLower(strings.TrimSpace(*format))
	if *jsonOut {
		outFormat = "json"
	}
	if outFormat != "table" && outFormat != "json" {
		return fmt.Errorf("invalid --format %q (expected table or json)", outFormat)
	}

	started := time.Now()
	eng, err := engine.OpenEngine(ctx.RootDir, 0)
	if err != nil {
		return err
	}
	defer eng.Close()
	switch strings.ToLower(strings.TrimSpace(*metricFiles)) {
	case "", "config":
	case "on":
		eng.PreferMetricFiles = true
	case "off":
		eng.PreferMetricFiles = false
	default:
		return fmt.Errorf("invalid --metric-files %q (expected config, on, or off)", *metricFiles)
	}

	metrics, err := loadMetricNamesFromCatalog(ctx.CatalogPath)
	if err != nil {
		return err
	}
	matchedMetrics := make([]string, 0, len(metrics))
	for _, metric := range metrics {
		if rx.MatchString(metric) {
			matchedMetrics = append(matchedMetrics, metric)
		}
	}

	rows := make([]queryRow, 0, 256)
	if aggregateMode {
		if len(matchedMetrics) != 1 {
			return fmt.Errorf("aggregate queries require exactly one matching metric, got %d", len(matchedMetrics))
		}
		metric := matchedMetrics[0]
		err := eng.QueryAggregateRange(ctx.Database, metric, fromTS, toTS, aggregateWindow, aggregateNames, func(bucket engine.AggregateBucket) error {
			rows = append(rows, aggregateBucketToQueryRow(bucket))
			return nil
		})
		if err != nil {
			return err
		}
	} else {
		if useRange && len(matchedMetrics) > 1 {
			err := eng.QueryRangeMany(ctx.Database, matchedMetrics, fromTS, toTS, 1, func(s engine.Sample) error {
				rows = append(rows, sampleToQueryRow(s))
				return nil
			})
			if err != nil {
				return err
			}
		} else {
			for _, metric := range matchedMetrics {
				if !useRange {
					s, found, err := eng.QueryLast(ctx.Database, metric)
					if err != nil {
						return err
					}
					if !found {
						continue
					}
					rows = append(rows, sampleToQueryRow(s))
					continue
				}
				err := eng.QueryRange(ctx.Database, metric, fromTS, toTS, 1, func(s engine.Sample) error {
					rows = append(rows, sampleToQueryRow(s))
					return nil
				})
				if err != nil {
					return err
				}
			}
		}
	}

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Metric == rows[j].Metric {
			if rows[i].Aggregate == rows[j].Aggregate {
				return rows[i].TimestampNS < rows[j].TimestampNS
			}
			return rows[i].Aggregate < rows[j].Aggregate
		}
		return rows[i].Metric < rows[j].Metric
	})

	report := queryReport{
		RootDir:      ctx.RootDir,
		Database:     ctx.Database,
		MetricRegex:  *metricRegex,
		OutputFormat: outFormat,
		Rows:         rows,
		RowCount:     len(rows),
		DurationMS:   time.Since(started).Milliseconds(),
	}
	if useRange {
		from := int64(fromTS)
		report.Start = &from
		to := int64(toTS)
		report.End = &to
	}

	if outFormat == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	if aggregateMode {
		fmt.Fprintln(tw, "METRIC\tAGG\tWINDOW\tSTART_NS\tSTART_UTC\tEND_NS\tEND_UTC\tTYPE\tVALUE")
		for _, row := range rows {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%d\t%s\t%s\t%s\n", row.Metric, row.Aggregate, row.Window, row.StartNS, row.StartUTC, row.EndNS, row.EndUTC, row.ValueType, row.Value)
		}
	} else {
		fmt.Fprintln(tw, "METRIC\tTS_NS\tTS_UTC\tTYPE\tVALUE")
		for _, row := range rows {
			fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\n", row.Metric, row.TimestampNS, row.TimestampUTC, row.ValueType, row.Value)
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "rows=%d duration_ms=%d\n", report.RowCount, report.DurationMS)
	return nil
}

func aggregateBucketToQueryRow(bucket engine.AggregateBucket) queryRow {
	return queryRow{
		Metric:       bucket.Metric,
		Aggregate:    bucket.Aggregate,
		Window:       bucket.Window.String(),
		StartNS:      int64(bucket.StartTS),
		StartUTC:     engine.FormatTimestamp(bucket.StartTS),
		EndNS:        int64(bucket.EndTS),
		EndUTC:       engine.FormatTimestamp(bucket.EndTS),
		TimestampNS:  int64(bucket.EndTS),
		TimestampUTC: engine.FormatTimestamp(bucket.EndTS),
		ValueType:    "float32",
		Value:        strconv.FormatFloat(float64(bucket.Value), 'f', -1, 32),
	}
}

func splitCSVList(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		out = append(out, part)
	}
	return out
}

func sampleToQueryRow(s engine.Sample) queryRow {
	valueType := "float32"
	value := strconv.FormatFloat(float64(s.Float32), 'f', -1, 32)
	if s.ValueType == engine.Int32Sample {
		valueType = "int32"
		value = strconv.FormatInt(int64(s.Int32), 10)
	}
	return queryRow{
		Metric:       s.Metric,
		TimestampNS:  int64(s.TS),
		TimestampUTC: engine.FormatTimestamp(s.TS),
		ValueType:    valueType,
		Value:        value,
	}
}

func loadMetricNamesFromCatalog(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read catalog %s: %w", path, err)
	}
	var cat catalogDisk
	if err := json.Unmarshal(raw, &cat); err != nil {
		return nil, fmt.Errorf("parse catalog %s: %w", path, err)
	}
	metrics := make([]string, 0, len(cat.Metrics))
	for _, m := range cat.Metrics {
		if strings.TrimSpace(m.Name) == "" {
			continue
		}
		metrics = append(metrics, m.Name)
	}
	sort.Strings(metrics)
	return metrics, nil
}

// parseTimeText parses a CLI/HTTP time value. Bare numeric values are
// interpreted with the supplied unit (default "ns" when empty); the old
// magnitude heuristic that silently treated ms-since-epoch as ns has been
// removed in favour of an explicit unit.
func parseTimeText(v string, tsUnit string) (engine.Timestamp, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, fmt.Errorf("missing time value")
	}
	return engine.ParseTimestampWithUnit(v, tsUnit)
}

func parseQueryTimeText(v string, allowRelativeDuration bool, tsUnit string) (engine.Timestamp, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, fmt.Errorf("missing time value")
	}
	if allowRelativeDuration {
		if d, err := parseExtendedDuration(v); err == nil {
			return engine.Timestamp(queryTimeNow().Add(-d).UnixNano()), nil
		}
	}
	return parseTimeText(v, tsUnit)
}

// parseExtendedDuration delegates to engine.ParseDuration, which accepts the
// same units as time.ParseDuration plus `d` (days) and `w` (weeks).
func parseExtendedDuration(v string) (time.Duration, error) {
	return engine.ParseDuration(v)
}
