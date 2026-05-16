package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
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

func runQuery(args []string) error {
	fs := flag.NewFlagSet("query", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	rootDir := fs.String("root", "", "root data directory that contains engine.toml")
	dbName := fs.String("db", "", "database name")
	metricRegex := fs.String("metric", ".*", "regex applied to metric names")
	startText := fs.String("start", "", "optional range start time (RFC3339Nano, unix seconds, or unix nanos)")
	endText := fs.String("end", "", "optional range end time (RFC3339Nano, unix seconds, or unix nanos)")
	format := fs.String("format", "table", "output format: table or json")
	jsonOut := fs.Bool("json", false, "alias for --format json")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("usage: nanocli query --root <root-dir> --db <database> --metric <regex> [--start <time>] [--end <time>] [--format table|json]")
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
		fromTS, err = parseTimeText(*startText)
		if err != nil {
			return fmt.Errorf("invalid --start: %w", err)
		}
	}
	if strings.TrimSpace(*endText) != "" {
		useRange = true
		toTS, err = parseTimeText(*endText)
		if err != nil {
			return fmt.Errorf("invalid --end: %w", err)
		}
	}
	if useRange {
		if strings.TrimSpace(*startText) == "" {
			fromTS = 0
		}
		if strings.TrimSpace(*endText) == "" {
			toTS = engine.Timestamp(time.Now().UnixNano())
		}
		if toTS < fromTS {
			return fmt.Errorf("--end must be >= --start")
		}
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

	metrics, err := loadMetricNamesFromCatalog(ctx.CatalogPath)
	if err != nil {
		return err
	}

	rows := make([]queryRow, 0, 256)
	for _, metric := range metrics {
		if !rx.MatchString(metric) {
			continue
		}
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

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Metric == rows[j].Metric {
			return rows[i].TimestampNS < rows[j].TimestampNS
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
		to := int64(toTS)
		report.Start = &from
		report.End = &to
	}

	if outFormat == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "METRIC\tTS_NS\tTS_UTC\tTYPE\tVALUE")
	for _, row := range rows {
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\n", row.Metric, row.TimestampNS, row.TimestampUTC, row.ValueType, row.Value)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "rows=%d duration_ms=%d\n", report.RowCount, report.DurationMS)
	return nil
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

func parseTimeText(v string) (engine.Timestamp, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, fmt.Errorf("missing time value")
	}
	if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
		return engine.Timestamp(t.UnixNano()), nil
	}
	if strings.Contains(v, ".") {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return 0, err
		}
		return engine.Timestamp(f * float64(time.Second)), nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, err
	}
	if n > 1_000_000_000_000 {
		return engine.Timestamp(n), nil
	}
	return engine.Timestamp(n * int64(time.Second)), nil
}
