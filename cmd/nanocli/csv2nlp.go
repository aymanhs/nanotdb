package main

import (
	"bufio"
	"container/heap"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

type csv2NLPReport struct {
	InputDir      string `json:"input_dir"`
	OutputFile    string `json:"output_file"`
	Database      string `json:"database"`
	MetadataFile  string `json:"metadata_file,omitempty"`
	AppendMode    bool   `json:"append_mode"`
	FilesSeen     int    `json:"files_seen"`
	FilesIncluded int    `json:"files_included"`
	FilesSkipped  int    `json:"files_skipped"`
	RowsWritten   int    `json:"rows_written"`
	DurationMS    int64  `json:"duration_ms"`
}

type csvMetricMetadata struct {
	Metrics map[string]csvMetricRule `json:"metrics"`
}

type csvMetricRule struct {
	Enabled      *bool   `json:"enabled,omitempty"`
	Metric       string  `json:"metric,omitempty"`
	ValueType    string  `json:"value_type,omitempty"`
	Scale        float64 `json:"scale,omitempty"`
	AppendSensor *bool   `json:"append_sensor,omitempty"`
}

type csvMetricSource struct {
	index      int
	path       string
	tempPath   string
	baseMetric string
	rule       csvMetricRule
	file       *os.File
	reader     *csv.Reader
	lineNo     int
	current    csvMetricRow
}

type csvMetricRow struct {
	metric      string
	value       string
	timestampMS int64
	sourceIndex int
}

type csvMetricHeap []csvMetricSource

func (h csvMetricHeap) Len() int { return len(h) }

func (h csvMetricHeap) Less(i, j int) bool {
	if h[i].current.timestampMS == h[j].current.timestampMS {
		if h[i].current.metric == h[j].current.metric {
			return h[i].current.sourceIndex < h[j].current.sourceIndex
		}
		return h[i].current.metric < h[j].current.metric
	}
	return h[i].current.timestampMS < h[j].current.timestampMS
}

func (h csvMetricHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

func (h *csvMetricHeap) Push(x any) { *h = append(*h, x.(csvMetricSource)) }

func (h *csvMetricHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

func runCSV2NLP(args []string) error {
	fs := flag.NewFlagSet("csv2nlp", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	inDir := fs.String("in-dir", "", "directory containing VM export CSV files")
	outPath := fs.String("out", "", "output line protocol file")
	database := fs.String("db", "", "target database name for all output metrics")
	metaPath := fs.String("meta", "", "optional JSON metadata file")
	appendMode := fs.Bool("append", false, "append only rows newer than the existing NLP tail")
	jsonOut := fs.Bool("json", false, "emit JSON output")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("usage: nanocli csv2nlp --in-dir <csv-dir> --out <line-protocol-file> --db <database> [--meta <json>] [--append] [--json]")
	}
	if strings.TrimSpace(*inDir) == "" {
		return fmt.Errorf("--in-dir is required")
	}
	if strings.TrimSpace(*outPath) == "" {
		return fmt.Errorf("--out is required")
	}
	if strings.TrimSpace(*database) == "" {
		return fmt.Errorf("--db is required")
	}

	started := time.Now()
	meta, err := loadCSVMetricMetadata(*metaPath)
	if err != nil {
		return err
	}

	report, err := convertCSVDirToNLP(strings.TrimSpace(*inDir), strings.TrimSpace(*outPath), strings.TrimSpace(*database), strings.TrimSpace(*metaPath), meta, *appendMode)
	if err != nil {
		return err
	}
	report.DurationMS = time.Since(started).Milliseconds()

	ow := outputWriter{w: os.Stdout, asJSON: *jsonOut}
	return ow.emit(report, func(w io.Writer) {
		fmt.Fprintf(w, "Wrote %d NLP lines from %d/%d CSV files to %s (%d skipped, %dms)\n", report.RowsWritten, report.FilesIncluded, report.FilesSeen, report.OutputFile, report.FilesSkipped, report.DurationMS)
		fmt.Fprintf(w, "Database: %s\n", report.Database)
		if report.AppendMode {
			fmt.Fprintln(w, "Mode: append")
		}
		if report.MetadataFile != "" {
			fmt.Fprintf(w, "Metadata: %s\n", report.MetadataFile)
		}
	})
}

func loadCSVMetricMetadata(path string) (csvMetricMetadata, error) {
	if strings.TrimSpace(path) == "" {
		return csvMetricMetadata{}, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return csvMetricMetadata{}, err
	}
	var meta csvMetricMetadata
	if err := json.Unmarshal(raw, &meta); err != nil {
		return csvMetricMetadata{}, fmt.Errorf("parse metadata: %w", err)
	}
	if meta.Metrics == nil {
		meta.Metrics = make(map[string]csvMetricRule)
	}
	return meta, nil
}

func convertCSVDirToNLP(inDir, outPath, database, metaPath string, meta csvMetricMetadata, appendMode bool) (csv2NLPReport, error) {
	entries, err := os.ReadDir(inDir)
	if err != nil {
		return csv2NLPReport{}, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	if err := os.MkdirAll(filepath.Dir(outPath), 0755); err != nil {
		return csv2NLPReport{}, err
	}
	lastTimestampMS := int64(math.MinInt64)
	if appendMode {
		lastTimestampMS, err = readNLPTailTimestampMS(outPath)
		if err != nil {
			return csv2NLPReport{}, err
		}
	}
	openFlags := os.O_CREATE | os.O_WRONLY
	if appendMode {
		openFlags |= os.O_APPEND
	} else {
		openFlags |= os.O_TRUNC
	}
	out, err := os.OpenFile(outPath, openFlags, 0644)
	if err != nil {
		return csv2NLPReport{}, err
	}
	defer out.Close()

	report := csv2NLPReport{InputDir: inDir, OutputFile: outPath, Database: database, MetadataFile: metaPath, AppendMode: appendMode}
	metricHeap := &csvMetricHeap{}
	heap.Init(metricHeap)

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".csv") {
			continue
		}
		report.FilesSeen++
		baseMetric := strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
		rule := meta.ruleForMetric(baseMetric)
		if !rule.isEnabled() {
			report.FilesSkipped++
			continue
		}
		source, ok, err := openCSVMetricSource(report.FilesSeen, filepath.Join(inDir, entry.Name()), baseMetric, rule, lastTimestampMS)
		if err != nil {
			return csv2NLPReport{}, err
		}
		if !ok {
			report.FilesSkipped++
			continue
		}
		report.FilesIncluded++
		heap.Push(metricHeap, source)
	}

	for metricHeap.Len() > 0 {
		source := heap.Pop(metricHeap).(csvMetricSource)
		if _, err := fmt.Fprintf(out, "%s/%s %s %d\n", database, source.current.metric, source.current.value, source.current.timestampMS*1_000_000); err != nil {
			source.close()
			closeCSVMetricHeap(metricHeap)
			return csv2NLPReport{}, err
		}
		report.RowsWritten++

		ok, err := source.advance()
		if err != nil {
			source.close()
			closeCSVMetricHeap(metricHeap)
			return csv2NLPReport{}, err
		}
		if ok {
			heap.Push(metricHeap, source)
			continue
		}
		source.close()
	}

	if err := out.Close(); err != nil {
		return csv2NLPReport{}, err
	}
	return report, nil
}

func readNLPTailTimestampMS(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return int64(math.MinInt64), nil
		}
		return 0, err
	}
	defer f.Close()

	var lastTimestampMS = int64(math.MinInt64)
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 3 {
			continue
		}
		tsNS, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil {
			continue
		}
		lastTimestampMS = tsNS / 1_000_000
	}
	if err := s.Err(); err != nil {
		return 0, err
	}
	return lastTimestampMS, nil
}

func closeCSVMetricHeap(h *csvMetricHeap) {
	for h.Len() > 0 {
		source := heap.Pop(h).(csvMetricSource)
		source.close()
	}
}

func openCSVMetricSource(index int, path, baseMetric string, rule csvMetricRule, minTimestampMS int64) (csvMetricSource, bool, error) {
	tempPath, rowCount, err := buildSortedCSVMetricTemp(index, path, baseMetric, rule, minTimestampMS)
	if err != nil {
		return csvMetricSource{}, false, err
	}
	if rowCount == 0 {
		return csvMetricSource{}, false, nil
	}

	f, err := os.Open(tempPath)
	if err != nil {
		_ = os.Remove(tempPath)
		return csvMetricSource{}, false, err
	}
	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	source := csvMetricSource{index: index, path: path, tempPath: tempPath, baseMetric: baseMetric, rule: rule, file: f, reader: r}
	ok, err := source.advance()
	if err != nil {
		_ = f.Close()
		_ = os.Remove(tempPath)
		return csvMetricSource{}, false, err
	}
	if !ok {
		_ = f.Close()
		_ = os.Remove(tempPath)
		return csvMetricSource{}, false, nil
	}
	return source, true, nil
}

func buildSortedCSVMetricTemp(index int, path, baseMetric string, rule csvMetricRule, minTimestampMS int64) (string, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	header, err := r.Read()
	if err != nil {
		if err == io.EOF {
			return "", 0, nil
		}
		return "", 0, fmt.Errorf("read csv header %s: %w", path, err)
	}
	if len(header) < 4 {
		return "", 0, fmt.Errorf("csv header too short in %s", path)
	}

	type sortableRow struct {
		seq         int
		metric      string
		rawValue    string
		timestampMS int64
		sourceIndex int
	}
	rows := make([]sortableRow, 0)
	metricModes := make(map[string]string)
	source := csvMetricSource{index: index, path: path, baseMetric: baseMetric, rule: rule, lineNo: 1}
	for {
		record, err := r.Read()
		if err != nil {
			if err == io.EOF {
				break
			}
			return "", 0, fmt.Errorf("read csv row %s:%d: %w", path, source.lineNo+1, err)
		}
		source.lineNo++
		if len(record) < 4 {
			return "", 0, fmt.Errorf("csv row too short %s:%d", path, source.lineNo)
		}
		metricName, err := source.resolveMetricName(record[0], record[1])
		if err != nil {
			return "", 0, fmt.Errorf("resolve metric %s:%d: %w", path, source.lineNo, err)
		}
		rawValue := strings.TrimSpace(record[2])
		mode, err := determineCSVMetricMode(rawValue, rule)
		if err != nil {
			return "", 0, fmt.Errorf("parse value %s:%d: %w", path, source.lineNo, err)
		}
		metricModes[metricName] = mergeCSVMetricMode(metricModes[metricName], mode)
		timestampMS, err := strconv.ParseInt(strings.TrimSpace(record[3]), 10, 64)
		if err != nil {
			return "", 0, fmt.Errorf("parse timestamp %s:%d: %w", path, source.lineNo, err)
		}
		if timestampMS <= minTimestampMS {
			continue
		}
		rows = append(rows, sortableRow{
			seq:         source.lineNo,
			metric:      metricName,
			rawValue:    rawValue,
			timestampMS: timestampMS,
			sourceIndex: index,
		})
	}
	if len(rows) == 0 {
		return "", 0, nil
	}

	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].timestampMS == rows[j].timestampMS {
			if rows[i].metric == rows[j].metric {
				return rows[i].seq < rows[j].seq
			}
			return rows[i].metric < rows[j].metric
		}
		return rows[i].timestampMS < rows[j].timestampMS
	})

	tempFile, err := os.CreateTemp("", "nanotdb-csv2nlp-*.csv")
	if err != nil {
		return "", 0, err
	}
	tempPath := tempFile.Name()
	w := csv.NewWriter(tempFile)
	for _, row := range rows {
		value, err := formatCSVMetricValueWithMode(row.rawValue, rule, metricModes[row.metric])
		if err != nil {
			_ = tempFile.Close()
			_ = os.Remove(tempPath)
			return "", 0, fmt.Errorf("format value %s metric %s: %w", path, row.metric, err)
		}
		record := []string{row.metric, value, strconv.FormatInt(row.timestampMS, 10)}
		if err := w.Write(record); err != nil {
			_ = tempFile.Close()
			_ = os.Remove(tempPath)
			return "", 0, err
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		_ = tempFile.Close()
		_ = os.Remove(tempPath)
		return "", 0, err
	}
	if err := tempFile.Close(); err != nil {
		_ = os.Remove(tempPath)
		return "", 0, err
	}
	return tempPath, len(rows), nil
}

func (s *csvMetricSource) advance() (bool, error) {
	record, err := s.reader.Read()
	if err != nil {
		if err == io.EOF {
			return false, nil
		}
		return false, fmt.Errorf("read csv row %s:%d: %w", s.path, s.lineNo+1, err)
	}
	s.lineNo++
	if len(record) < 3 {
		return false, fmt.Errorf("csv row too short %s:%d", s.path, s.lineNo)
	}
	timestampMS, err := strconv.ParseInt(strings.TrimSpace(record[2]), 10, 64)
	if err != nil {
		return false, fmt.Errorf("parse timestamp %s:%d: %w", s.path, s.lineNo, err)
	}
	s.current = csvMetricRow{metric: strings.TrimSpace(record[0]), value: strings.TrimSpace(record[1]), timestampMS: timestampMS, sourceIndex: s.index}
	return true, nil
}

func (s csvMetricSource) close() {
	if s.file != nil {
		_ = s.file.Close()
	}
	if s.tempPath != "" {
		_ = os.Remove(s.tempPath)
	}
}

func (s csvMetricSource) resolveMetricName(rawMetric, sensor string) (string, error) {
	metricName := strings.TrimSpace(s.rule.Metric)
	if metricName == "" {
		metricName = strings.TrimSpace(rawMetric)
	}
	if metricName == "" {
		metricName = s.baseMetric
	}
	metricName = sanitizeMetricSegment(metricName)
	if metricName == "" {
		return "", fmt.Errorf("metric name cannot be empty")
	}
	if s.rule.shouldAppendSensor() {
		sensor = sanitizeMetricSegment(sensor)
		if sensor != "" {
			metricName += "." + sensor
		}
	}
	return metricName, nil
}

func determineCSVMetricMode(raw string, rule csvMetricRule) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("value cannot be empty")
	}
	mode := strings.ToLower(strings.TrimSpace(rule.ValueType))
	scale := rule.Scale
	if scale == 0 {
		scale = 1
	}
	switch mode {
	case "", "auto":
		if scale == 1 {
			if _, err := strconv.ParseInt(raw, 10, 32); err == nil {
				return "int", nil
			}
		}
		if _, err := strconv.ParseFloat(raw, 64); err != nil {
			return "", err
		}
		return "float", nil
	case "float":
		if _, err := strconv.ParseFloat(raw, 64); err != nil {
			return "", err
		}
		return "float", nil
	case "int":
		fv, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return "", err
		}
		iv := int64(math.Round(fv * scale))
		if iv < math.MinInt32 || iv > math.MaxInt32 {
			return "", fmt.Errorf("int32 overflow after scaling: %d", iv)
		}
		return "int", nil
	default:
		return "", fmt.Errorf("unsupported value_type %q", rule.ValueType)
	}
}

func mergeCSVMetricMode(current, next string) string {
	if current == "" {
		return next
	}
	if current == next {
		return current
	}
	return "float"
}

func formatCSVMetricValueWithMode(raw string, rule csvMetricRule, mode string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("value cannot be empty")
	}
	scale := rule.Scale
	if scale == 0 {
		scale = 1
	}
	switch mode {
	case "", "float":
		fv, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return "", err
		}
		fv *= scale
		return strconv.FormatFloat(fv, 'f', -1, 64), nil
	case "int":
		fv, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return "", err
		}
		iv := int64(math.Round(fv * scale))
		if iv < math.MinInt32 || iv > math.MaxInt32 {
			return "", fmt.Errorf("int32 overflow after scaling: %d", iv)
		}
		return strconv.FormatInt(iv, 10) + "i", nil
	default:
		return "", fmt.Errorf("unsupported effective mode %q", mode)
	}
}

func sanitizeMetricSegment(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	var b strings.Builder
	prevUnderscore := false
	for _, r := range s {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r), r == '.', r == '-', r == '_', r == '/':
			b.WriteRune(r)
			prevUnderscore = false
		default:
			if !prevUnderscore {
				b.WriteByte('_')
				prevUnderscore = true
			}
		}
	}
	return strings.Trim(b.String(), "_.-/")
}

func (m csvMetricMetadata) ruleForMetric(baseMetric string) csvMetricRule {
	if m.Metrics == nil {
		return csvMetricRule{}
	}
	return m.Metrics[baseMetric]
}

func (r csvMetricRule) isEnabled() bool {
	if r.Enabled == nil {
		return true
	}
	return *r.Enabled
}

func (r csvMetricRule) shouldAppendSensor() bool {
	if r.AppendSensor == nil {
		return true
	}
	return *r.AppendSensor
}
