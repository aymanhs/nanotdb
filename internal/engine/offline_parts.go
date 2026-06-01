package engine

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type OfflineLPImportOptions struct {
	CatalogPath   string
	OutDir        string
	PartitionMode string
	// TimestampUnit controls how bare numeric timestamps are interpreted.
	// "" defaults to nanoseconds. Valid values: ns, us, ms, s.
	TimestampUnit string
}

type OfflineLPImportPartReport struct {
	Partition      string `json:"partition"`
	MetricFilePath string `json:"metric_file_path"`
	Metrics        int    `json:"metrics"`
	Points         int64  `json:"points"`
}

type OfflineLPImportReport struct {
	CatalogPath   string                      `json:"catalog_path"`
	CatalogMode   string                      `json:"catalog_mode"`
	Partitions    []OfflineLPImportPartReport `json:"partitions"`
	TotalLines    int                         `json:"total_lines"`
	ImportedLines int                         `json:"imported_lines"`
	SkippedLines  int                         `json:"skipped_lines"`
}

type OfflineLPExportOptions struct {
	InputPath   string
	CatalogPath string
	InputKind   string
	WithDB      string
}

type OfflineLPExportFileReport struct {
	Path  string `json:"path"`
	Kind  string `json:"kind"`
	Lines int64  `json:"lines"`
}

type OfflineLPExportReport struct {
	CatalogPath string                      `json:"catalog_path"`
	Files       []OfflineLPExportFileReport `json:"files"`
	TotalLines  int64                       `json:"total_lines"`
}

type offlineLPRow struct {
	Metric    string
	Value     string
	Timestamp Timestamp
}

type offlineValuePoint struct {
	TS    Timestamp
	Value string
}

type offlineExportSample struct {
	Metric    string
	ValueType byte
	Raw       uint32
	TS        Timestamp
}

func ImportOfflineLPToMetricParts(r io.Reader, opts OfflineLPImportOptions) (OfflineLPImportReport, error) {
	outDir := strings.TrimSpace(opts.OutDir)
	if outDir == "" {
		outDir = "."
	}
	partitionKind, err := partitionModeToMetricPartitionKind(opts.PartitionMode)
	if err != nil {
		return OfflineLPImportReport{}, err
	}
	tsUnit, err := NormalizeTimestampUnit(opts.TimestampUnit)
	if err != nil {
		return OfflineLPImportReport{}, err
	}
	if err := os.MkdirAll(outDir, 0755); err != nil {
		return OfflineLPImportReport{}, err
	}

	_, catalogMode, catalog, err := loadOfflineImportCatalog(opts.CatalogPath, outDir)
	if err != nil {
		return OfflineLPImportReport{}, err
	}
	defer catalog.Close()

	rowsByPartition := make(map[string]map[string][]offlineValuePoint)
	metricNames := make(map[string]struct{})
	totalLines := 0
	importedLines := 0
	skippedLines := 0

	s := bufio.NewScanner(r)
	lineNo := 0
	for s.Scan() {
		lineNo++
		totalLines++
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			skippedLines++
			continue
		}
		row, err := parseOfflineLPLine(line, tsUnit)
		if err != nil {
			return OfflineLPImportReport{}, fmt.Errorf("line %d: %w", lineNo, err)
		}
		part, err := offlinePartitionKey(opts.PartitionMode, row.Timestamp)
		if err != nil {
			return OfflineLPImportReport{}, fmt.Errorf("line %d: %w", lineNo, err)
		}
		if rowsByPartition[part] == nil {
			rowsByPartition[part] = make(map[string][]offlineValuePoint)
		}
		rowsByPartition[part][row.Metric] = append(rowsByPartition[part][row.Metric], offlineValuePoint{TS: row.Timestamp, Value: row.Value})
		metricNames[row.Metric] = struct{}{}
		importedLines++
	}
	if err := s.Err(); err != nil {
		return OfflineLPImportReport{}, err
	}

	if catalogMode == "created" {
		if err := bootstrapOfflineCatalog(catalog, metricNames, rowsByPartition); err != nil {
			return OfflineLPImportReport{}, err
		}
	}

	parts := make([]string, 0, len(rowsByPartition))
	for part := range rowsByPartition {
		parts = append(parts, part)
	}
	sort.Strings(parts)

	report := OfflineLPImportReport{
		CatalogPath:   filepath.Join(outDir, "catalog.json"),
		CatalogMode:   catalogMode,
		Partitions:    make([]OfflineLPImportPartReport, 0, len(parts)),
		TotalLines:    totalLines,
		ImportedLines: importedLines,
		SkippedLines:  skippedLines,
	}

	for _, part := range parts {
		pages, metrics, points, err := buildMetricPagesForOfflinePartition(catalog, rowsByPartition[part])
		if err != nil {
			return OfflineLPImportReport{}, err
		}
		metricPath := filepath.Join(outDir, "metric-"+part+".dat")
		if err := WriteMetricFileV2(metricPath, partitionKind, nil, pages); err != nil {
			return OfflineLPImportReport{}, err
		}
		report.Partitions = append(report.Partitions, OfflineLPImportPartReport{
			Partition:      part,
			MetricFilePath: metricPath,
			Metrics:        metrics,
			Points:         points,
		})
	}

	// The catalog was opened via LoadCatalog on report.CatalogPath, so its
	// canonical file IS the output path. Persist via WriteCatalog() so we
	// reuse and refresh the existing fd rather than rebinding to a snapshot.
	if err := catalog.WriteCatalog(); err != nil {
		return OfflineLPImportReport{}, err
	}
	return report, nil
}

func ExportOfflinePartsToLP(w io.Writer, opts OfflineLPExportOptions) (OfflineLPExportReport, error) {
	catalogPath := strings.TrimSpace(opts.CatalogPath)
	if catalogPath == "" {
		return OfflineLPExportReport{}, fmt.Errorf("catalog path is required")
	}
	if _, err := os.Stat(catalogPath); err != nil {
		return OfflineLPExportReport{}, err
	}
	catalog, err := LoadCatalog(catalogPath)
	if err != nil {
		return OfflineLPExportReport{}, err
	}
	defer catalog.Close()

	files, err := resolveOfflineExportFiles(opts.InputPath, opts.InputKind)
	if err != nil {
		return OfflineLPExportReport{}, err
	}

	bw := bufio.NewWriterSize(w, 64*1024)
	report := OfflineLPExportReport{CatalogPath: catalogPath, Files: make([]OfflineLPExportFileReport, 0, len(files))}
	for _, file := range files {
		samples, err := collectOfflineExportSamples(catalog, file.path, file.kind)
		if err != nil {
			return OfflineLPExportReport{}, err
		}
		sort.Slice(samples, func(i, j int) bool {
			if samples[i].TS == samples[j].TS {
				return samples[i].Metric < samples[j].Metric
			}
			return samples[i].TS < samples[j].TS
		})
		for _, sample := range samples {
			key := sample.Metric
			if strings.TrimSpace(opts.WithDB) != "" {
				key = strings.TrimSpace(opts.WithDB) + "/" + sample.Metric
			}
			if _, err := fmt.Fprintf(bw, "%s %s %s\n", key, offlineLPValue(sample.ValueType, sample.Raw), FormatTimestamp(sample.TS)); err != nil {
				return OfflineLPExportReport{}, err
			}
		}
		report.Files = append(report.Files, OfflineLPExportFileReport{Path: file.path, Kind: file.kind, Lines: int64(len(samples))})
		report.TotalLines += int64(len(samples))
	}
	if err := bw.Flush(); err != nil {
		return OfflineLPExportReport{}, err
	}
	return report, nil
}

func parseOfflineLPLine(line string, tsUnit string) (offlineLPRow, error) {
	parts := strings.Fields(line)
	if len(parts) < 3 || len(parts) > 4 {
		return offlineLPRow{}, fmt.Errorf("invalid line protocol")
	}
	key := strings.TrimSpace(parts[0])
	if key == "" {
		return offlineLPRow{}, fmt.Errorf("metric key cannot be empty")
	}
	// Strip an optional "<db>/" prefix produced by ExportOfflinePartsToLP
	// --with-db. Anything beyond a single leading "/" segment is treated as
	// part of the metric name, which is then rejected by validateMetricName
	// (the engine forbids '/' in metric names, so a key like "a/b/c" is an
	// error regardless of --with-db).
	if idx := strings.Index(key, "/"); idx > 0 && idx < len(key)-1 {
		key = key[idx+1:]
	}
	if err := validateMetricName(key); err != nil {
		return offlineLPRow{}, err
	}
	timestampText := parts[2]
	if len(parts) == 4 {
		timestampText = parts[2] + " " + parts[3]
	}
	ts, err := parseOfflineTimestamp(timestampText, tsUnit)
	if err != nil {
		return offlineLPRow{}, err
	}
	return offlineLPRow{Metric: key, Value: parts[1], Timestamp: ts}, nil
}

// parseOfflineTimestamp interprets bare numeric timestamps in the configured
// unit (default ns). Heuristic magnitude guessing was removed because it
// mis-bucketed ms timestamps (year 2023, value >1e12) as nanoseconds.
func parseOfflineTimestamp(v string, tsUnit string) (Timestamp, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, fmt.Errorf("missing timestamp")
	}
	return ParseTimestampWithUnit(v, tsUnit)
}

func offlinePartitionKey(mode string, ts Timestamp) (string, error) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "day":
		return dayKey(ts), nil
	case "month":
		return monthKey(ts), nil
	case "year":
		return yearKey(ts), nil
	default:
		return "", fmt.Errorf("unsupported partition mode: %q", mode)
	}
}

func loadOfflineImportCatalog(explicitPath, outDir string) (string, string, *Catalog, error) {
	if strings.TrimSpace(explicitPath) != "" {
		if _, err := os.Stat(explicitPath); err != nil {
			return "", "", nil, err
		}
		catalog, err := LoadCatalog(explicitPath)
		if err != nil {
			return "", "", nil, err
		}
		return explicitPath, "validated", catalog, nil
	}
	defaultPath := filepath.Join(outDir, "catalog.json")
	mode := "created"
	if _, err := os.Stat(defaultPath); err == nil {
		mode = "validated"
	} else if !os.IsNotExist(err) {
		return "", "", nil, err
	}
	catalog, err := LoadCatalog(defaultPath)
	if err != nil {
		return "", "", nil, err
	}
	return defaultPath, mode, catalog, nil
}

func bootstrapOfflineCatalog(catalog *Catalog, metricNames map[string]struct{}, rowsByPartition map[string]map[string][]offlineValuePoint) error {
	names := make([]string, 0, len(metricNames))
	for name := range metricNames {
		names = append(names, name)
	}
	sort.Strings(names)
	for idx, name := range names {
		valueType, err := inferOfflineMetricType(name, rowsByPartition)
		if err != nil {
			return err
		}
		if err := catalog.EnsureMetricEntry(name, MetricID(idx+1), valueType); err != nil {
			return err
		}
	}
	return nil
}

func inferOfflineMetricType(metric string, rowsByPartition map[string]map[string][]offlineValuePoint) (byte, error) {
	hasExplicitInt := false
	hasFloat := false
	for _, metricRows := range rowsByPartition {
		for _, point := range metricRows[metric] {
			value := strings.TrimSpace(point.Value)
			if strings.HasSuffix(value, "i") {
				hasExplicitInt = true
				continue
			}
			if strings.ContainsAny(value, ".eE") {
				hasFloat = true
			}
		}
	}
	if hasExplicitInt && hasFloat {
		return 0, fmt.Errorf("metric %q mixes explicit int and float values", metric)
	}
	if hasExplicitInt {
		return Int32Sample, nil
	}
	return Float32Sample, nil
}

func buildMetricPagesForOfflinePartition(catalog *Catalog, metricRows map[string][]offlineValuePoint) ([]MetricFilePageInput, int, int64, error) {
	names := make([]string, 0, len(metricRows))
	for name := range metricRows {
		names = append(names, name)
	}
	sort.Strings(names)
	pages := make([]MetricFilePageInput, 0, len(names))
	var totalPoints int64
	for _, name := range names {
		entry, ok := catalog.GetMetricEntry(name)
		if !ok {
			return nil, 0, 0, fmt.Errorf("metric %q missing from catalog", name)
		}
		page := MetricFilePageInput{MetricID: entry.MetricID, ValueType: entry.ValueType, Times: make([]Timestamp, 0, len(metricRows[name]))}
		switch entry.ValueType {
		case Int32Sample:
			page.Int32 = make([]int32, 0, len(metricRows[name]))
		case Float32Sample:
			page.Float32 = make([]float32, 0, len(metricRows[name]))
		default:
			return nil, 0, 0, fmt.Errorf("unsupported metric value type: %d", entry.ValueType)
		}
		for _, point := range metricRows[name] {
			page.Times = append(page.Times, point.TS)
			switch entry.ValueType {
			case Int32Sample:
				value, err := parseOfflineIntValue(point.Value)
				if err != nil {
					return nil, 0, 0, fmt.Errorf("metric %q: %w", name, err)
				}
				page.Int32 = append(page.Int32, value)
			case Float32Sample:
				value, err := parseOfflineFloatValue(point.Value)
				if err != nil {
					return nil, 0, 0, fmt.Errorf("metric %q: %w", name, err)
				}
				page.Float32 = append(page.Float32, value)
			}
		}
		normalizeMetricFilePageInputOrder(&page)
		pages = append(pages, page)
		totalPoints += int64(len(page.Times))
	}
	sort.Slice(pages, func(i, j int) bool {
		return pages[i].MetricID < pages[j].MetricID
	})
	return pages, len(pages), totalPoints, nil
}

func parseOfflineIntValue(v string) (int32, error) {
	v = strings.TrimSpace(v)
	v = strings.TrimSuffix(v, "i")
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid int32 value %q", v)
	}
	return int32(n), nil
}

func parseOfflineFloatValue(v string) (float32, error) {
	v = strings.TrimSpace(v)
	if strings.HasSuffix(v, "i") {
		return 0, fmt.Errorf("unexpected int suffix for float metric")
	}
	f, err := strconv.ParseFloat(v, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid float32 value %q", v)
	}
	return float32(f), nil
}

type offlineExportFile struct {
	path string
	kind string
}

func resolveOfflineExportFiles(inputPath, inputKind string) ([]offlineExportFile, error) {
	inputKind = strings.ToLower(strings.TrimSpace(inputKind))
	if inputKind == "" {
		inputKind = "auto"
	}
	st, err := os.Stat(inputPath)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		kind, err := inferOfflineExportKind(filepath.Base(inputPath), inputKind)
		if err != nil {
			return nil, err
		}
		return []offlineExportFile{{path: inputPath, kind: kind}}, nil
	}

	entries, err := os.ReadDir(inputPath)
	if err != nil {
		return nil, err
	}
	metricByPart := make(map[string]string)
	datByPart := make(map[string]string)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		switch {
		case strings.HasPrefix(name, "metric-") && strings.HasSuffix(name, ".dat"):
			metricByPart[strings.TrimSuffix(strings.TrimPrefix(name, "metric-"), ".dat")] = filepath.Join(inputPath, name)
		case strings.HasPrefix(name, "data-") && strings.HasSuffix(name, ".dat"):
			datByPart[strings.TrimSuffix(strings.TrimPrefix(name, "data-"), ".dat")] = filepath.Join(inputPath, name)
		case strings.HasPrefix(name, "raw-") && strings.HasSuffix(name, ".dat"):
			datByPart[strings.TrimSuffix(strings.TrimPrefix(name, "raw-"), ".dat")] = filepath.Join(inputPath, name)
		}
	}
	partsSet := make(map[string]struct{})
	for part := range metricByPart {
		partsSet[part] = struct{}{}
	}
	for part := range datByPart {
		partsSet[part] = struct{}{}
	}
	parts := make([]string, 0, len(partsSet))
	for part := range partsSet {
		parts = append(parts, part)
	}
	sort.Strings(parts)
	out := make([]offlineExportFile, 0, len(parts))
	for _, part := range parts {
		switch inputKind {
		case "metric":
			if metricByPart[part] == "" {
				continue
			}
			out = append(out, offlineExportFile{path: metricByPart[part], kind: "metric"})
		case "dat":
			if datByPart[part] == "" {
				continue
			}
			out = append(out, offlineExportFile{path: datByPart[part], kind: "dat"})
		case "auto":
			if metricByPart[part] != "" {
				out = append(out, offlineExportFile{path: metricByPart[part], kind: "metric"})
			} else if datByPart[part] != "" {
				out = append(out, offlineExportFile{path: datByPart[part], kind: "dat"})
			}
		default:
			return nil, fmt.Errorf("invalid input kind %q", inputKind)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no offline partition files found in %s", inputPath)
	}
	return out, nil
}

func inferOfflineExportKind(name, requested string) (string, error) {
	if requested == "metric" || requested == "dat" {
		return requested, nil
	}
	switch {
	case strings.HasPrefix(name, "metric-") && strings.HasSuffix(name, ".dat"):
		return "metric", nil
	case (strings.HasPrefix(name, "data-") || strings.HasPrefix(name, "raw-")) && strings.HasSuffix(name, ".dat"):
		return "dat", nil
	default:
		return "", fmt.Errorf("cannot infer input kind for %s", name)
	}
}

func collectOfflineExportSamples(catalog *Catalog, path, kind string) ([]offlineExportSample, error) {
	switch kind {
	case "dat":
		return collectOfflineDataFileSamples(catalog, path)
	case "metric":
		return collectOfflineMetricFileSamples(catalog, path)
	default:
		return nil, fmt.Errorf("unsupported input kind %q", kind)
	}
}

func collectOfflineDataFileSamples(catalog *Catalog, path string) ([]offlineExportSample, error) {
	out := make([]offlineExportSample, 0, 256)
	err := walkDataPages(path, func(p *Page) error {
		values, err := dataPageValues(p)
		if err != nil {
			return err
		}
		for i, mid := range p.Metrics {
			name, entry, ok := catalog.GetMetricByID(mid)
			if !ok {
				return fmt.Errorf("metric id %d missing from catalog", mid)
			}
			out = append(out, offlineExportSample{Metric: name, ValueType: entry.ValueType, Raw: binary.LittleEndian.Uint32(values[i*4 : i*4+4]), TS: p.Times[i]})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func collectOfflineMetricFileSamples(catalog *Catalog, path string) ([]offlineExportSample, error) {
	version, err := readMetricFrameVersion(path)
	if err != nil {
		return nil, err
	}
	switch version {
	case metricFileV1Version:
		return collectOfflineMetricFileSamplesV1(catalog, path)
	case metricFileV2Version:
		return collectOfflineMetricFileSamplesV2(catalog, path)
	default:
		return nil, fmt.Errorf("unsupported metric file version: %d", version)
	}
}

func collectOfflineMetricFileSamplesV1(catalog *Catalog, path string) ([]offlineExportSample, error) {
	out := make([]offlineExportSample, 0, 256)
	err := WalkMetricFileV1(path, func(page MetricFilePage) error {
		name, _, ok := catalog.GetMetricByID(page.MetricID)
		if !ok {
			return fmt.Errorf("metric id %d missing from catalog", page.MetricID)
		}
		switch page.ValueType {
		case Int32Sample:
			for i, ts := range page.Times {
				out = append(out, offlineExportSample{Metric: name, ValueType: page.ValueType, Raw: uint32(page.Int32[i]), TS: ts})
			}
		case Float32Sample:
			for i, ts := range page.Times {
				out = append(out, offlineExportSample{Metric: name, ValueType: page.ValueType, Raw: math.Float32bits(page.Float32[i]), TS: ts})
			}
		default:
			return fmt.Errorf("unsupported metric value type: %d", page.ValueType)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func collectOfflineMetricFileSamplesV2(catalog *Catalog, path string) ([]offlineExportSample, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	hdr, err := readMetricFileV2HeaderFromFile(f)
	if err != nil {
		return nil, err
	}
	if _, err := readMetricFileV2FooterFromFile(f, st.Size()); err != nil {
		return nil, err
	}
	timeEntries, metricEntries, err := resolveMetricFileIndexesV2(f, path, st, hdr)
	if err != nil {
		return nil, err
	}
	timeByID := make(map[uint16]metricTimeFrameIndexEntryV2, len(timeEntries))
	for _, entry := range timeEntries {
		timeByID[entry.TimeFrameID] = entry
	}
	localTimes := make(map[uint16][]Timestamp, len(timeEntries))
	identity := metricTimeFrameCacheIdentityV2(path, st)
	out := make([]offlineExportSample, 0, 256)
	for _, info := range metricEntries {
		name, _, ok := catalog.GetMetricByID(info.MetricID)
		if !ok {
			return nil, fmt.Errorf("metric id %d missing from catalog", info.MetricID)
		}
		timeInfo, ok := timeByID[info.TimeFrameID]
		if !ok {
			return nil, fmt.Errorf("metric %d references missing time frame %d", info.MetricID, info.TimeFrameID)
		}
		times, err := resolveMetricTimeFrameV2(f, identity, localTimes, timeInfo)
		if err != nil {
			return nil, err
		}
		frame, err := readOneMetricMetricFrameV2(f, st.Size(), info)
		if err != nil {
			return nil, err
		}
		pointCount, err := metricValuePointCountFromDecodedLen(frame.ValueType, frame.DecodedLen)
		if err != nil {
			return nil, err
		}
		start := int(info.TimeOffset)
		end := start + int(pointCount)
		if start < 0 || end > len(times) {
			return nil, fmt.Errorf("metric %d time slice out of bounds", info.MetricID)
		}
		frameTimes := times[start:end]
		switch frame.ValueType {
		case Int32Sample:
			for i, ts := range frameTimes {
				out = append(out, offlineExportSample{Metric: name, ValueType: frame.ValueType, Raw: uint32(frame.Int32[i]), TS: ts})
			}
		case Float32Sample:
			for i, ts := range frameTimes {
				out = append(out, offlineExportSample{Metric: name, ValueType: frame.ValueType, Raw: math.Float32bits(frame.Float32[i]), TS: ts})
			}
		default:
			return nil, fmt.Errorf("unsupported metric value type: %d", frame.ValueType)
		}
	}
	return out, nil
}

func offlineLPValue(valueType byte, raw uint32) string {
	switch valueType {
	case Int32Sample:
		return strconv.FormatInt(int64(int32(raw)), 10) + "i"
	case Float32Sample:
		return strconv.FormatFloat(float64(math.Float32frombits(raw)), 'f', -1, 32)
	default:
		return "0"
	}
}
