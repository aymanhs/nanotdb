package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aymanhs/nanotdb/internal/engine"
)

type serverDBContext struct {
	RootDir       string
	Database      string
	DatabaseDir   string
	DataFilePaths []string
	WALFilePaths  []string
}

type engineDatabaseSummary struct {
	Name        string                  `json:"name"`
	MetricCount int                     `json:"metric_count"`
	Manifest    engine.DBInfo           `json:"manifest"`
	Stats       engine.DBStats          `json:"stats"`
	OpenPages   int                     `json:"open_pages"`
	DataFiles   int                     `json:"data_files"`
	DataBytes   int64                   `json:"data_bytes"`
	WALFiles    int                     `json:"wal_files"`
	WALBytes    int64                   `json:"wal_bytes"`
	Active      bool                    `json:"active"`
	Runtime     engine.DBRuntimeInspect `json:"runtime,omitempty"`
}

type engineMetricInfo struct {
	Name          string `json:"name"`
	ID            uint16 `json:"id"`
	Type          string `json:"type"`
	LastTimestamp string `json:"last_timestamp,omitempty"`
	LastTSNS      int64  `json:"last_timestamp_ns,omitempty"`
	LastValue     string `json:"last_value,omitempty"`
}

type engineDatabaseDetail struct {
	Summary engineDatabaseSummary `json:"summary"`
	Metrics []engineMetricInfo    `json:"metrics"`
}

type engineDataPage struct {
	Index                int     `json:"index"`
	Offset               int64   `json:"offset"`
	FrameBytes           int64   `json:"frame_bytes"`
	CompressedLen        uint64  `json:"compressed_len"`
	UncompressedLen      uint64  `json:"uncompressed_len"`
	Records              uint16  `json:"records"`
	StartTS              int64   `json:"start_timestamp_ns"`
	EndTS                int64   `json:"end_timestamp_ns"`
	DurationNS           int64   `json:"duration_ns"`
	AvgDiskBytesPerPoint float64 `json:"avg_disk_bytes_per_point"`
	StartUTC             string  `json:"start_utc"`
	EndUTC               string  `json:"end_utc"`
}

type engineDataFile struct {
	Path         string           `json:"path"`
	Part         string           `json:"part,omitempty"`
	Active       bool             `json:"active"`
	Bytes        int64            `json:"bytes"`
	Frames       int              `json:"frames"`
	Records      int64            `json:"records"`
	MinTimestamp int64            `json:"min_timestamp_ns,omitempty"`
	MaxTimestamp int64            `json:"max_timestamp_ns,omitempty"`
	MinUTC       string           `json:"min_utc,omitempty"`
	MaxUTC       string           `json:"max_utc,omitempty"`
	Pages        []engineDataPage `json:"pages"`
	ScanError    string           `json:"scan_error,omitempty"`
}

type engineWALFile struct {
	Path         string `json:"path"`
	Bytes        int64  `json:"bytes"`
	Records      int    `json:"records"`
	DecodedBytes int64  `json:"decoded_bytes"`
	MinTimestamp int64  `json:"min_timestamp_ns,omitempty"`
	MaxTimestamp int64  `json:"max_timestamp_ns,omitempty"`
	MinUTC       string `json:"min_utc,omitempty"`
	MaxUTC       string `json:"max_utc,omitempty"`
	HasTail      bool   `json:"has_tail"`
	TailBytes    int64  `json:"tail_bytes,omitempty"`
	StopOffset   int64  `json:"stop_offset,omitempty"`
	StopReason   string `json:"stop_reason,omitempty"`
	ScanError    string `json:"scan_error,omitempty"`
}

type engineWALRecord struct {
	Index       int         `json:"index"`
	Offset      int64       `json:"offset"`
	RecordBytes int64       `json:"record_bytes"`
	SegmentID   uint16      `json:"segment_id"`
	MetricID    uint16      `json:"metric_id"`
	MetricName  string      `json:"metric_name,omitempty"`
	TimestampNS int64       `json:"timestamp_ns"`
	Timestamp   string      `json:"timestamp"`
	ValueType   string      `json:"value_type"`
	Value       interface{} `json:"value"`
}

type engineFilesReport struct {
	Database string            `json:"database"`
	Data     []engineDataFile  `json:"data"`
	WAL      []engineWALFile   `json:"wal"`
	Records  []engineWALRecord `json:"records"`
}

type engineRuntimeReport struct {
	Runtime engine.DBRuntimeInspect `json:"runtime"`
	WAL     []engineWALRecord       `json:"wal"`
}

type engineOverviewSettings struct {
	Listen                string `json:"listen"`
	WALMaxSegmentSize     int64  `json:"wal_max_segment_size"`
	WALFsyncPolicy        string `json:"wal_fsync_policy"`
	DurabilityProfile     string `json:"durability_profile"`
	SyncDataFile          bool   `json:"sync_data_file"`
	SyncCatalog           bool   `json:"sync_catalog"`
	StatsEnabled          bool   `json:"stats_enabled"`
	StatsInterval         string `json:"stats_interval"`
	WebEnabled            bool   `json:"web_enabled"`
	DashboardPath         string `json:"dashboard_path"`
	ExplorePath           string `json:"explore_path"`
	EnginePath            string `json:"engine_path"`
	DefaultWALEnabled     bool   `json:"default_wal_enabled"`
	DefaultWALSkipBefore  string `json:"default_wal_skip_before"`
	DefaultRetentionDays  int    `json:"default_retention_days"`
	DefaultMaxActiveDays  int    `json:"default_max_active_days"`
	DefaultPartition      string `json:"default_partition"`
	DefaultPageMaxRecords int    `json:"default_page_max_records"`
	DefaultPageMaxBytes   int    `json:"default_page_max_bytes"`
	DefaultPageMaxAge     string `json:"default_page_max_age"`
	DefaultGrace          string `json:"default_grace"`
}

func handleEngineOverview(eng *engine.Engine, runtimeCfg runtimeConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeVMError(w, http.StatusMethodNotAllowed, "bad_data", "method not allowed")
			return
		}

		names := eng.GetAllDatabaseNames()
		items := make([]engineDatabaseSummary, 0, len(names))
		for _, name := range names {
			item, err := buildEngineDatabaseSummary(eng, name)
			if err != nil {
				writeVMError(w, http.StatusInternalServerError, "execution", err.Error())
				return
			}
			items = append(items, item)
		}

		writeJSON(w, http.StatusOK, vmResponse{
			Status: "success",
			Data: map[string]interface{}{
				"resultType": "engine_overview",
				"result":     items,
				"settings":   buildEngineOverviewSettings(eng, runtimeCfg),
			},
		})
	}
}

func buildEngineOverviewSettings(eng *engine.Engine, runtimeCfg runtimeConfig) engineOverviewSettings {
	return engineOverviewSettings{
		Listen:                runtimeCfg.EngineConfig.Engine.Listen,
		WALMaxSegmentSize:     runtimeCfg.EngineConfig.WAL.MaxSegmentSize,
		WALFsyncPolicy:        runtimeCfg.EngineConfig.WAL.FsyncPolicy,
		DurabilityProfile:     runtimeCfg.EngineConfig.Durability.Profile,
		SyncDataFile:          eng.SyncDataFile,
		SyncCatalog:           eng.SyncCatalog,
		StatsEnabled:          runtimeCfg.EngineConfig.Stats.Enabled,
		StatsInterval:         runtimeCfg.EngineConfig.Stats.Interval,
		WebEnabled:            runtimeCfg.WebConfig.Enabled,
		DashboardPath:         runtimeCfg.WebConfig.BasePath,
		ExplorePath:           runtimeCfg.WebConfig.ExplorePath,
		EnginePath:            runtimeCfg.WebConfig.EnginePath,
		DefaultWALEnabled:     runtimeCfg.DBDefaults.WALEnabled,
		DefaultWALSkipBefore:  runtimeCfg.DBDefaults.WALSkipBefore,
		DefaultRetentionDays:  runtimeCfg.DBDefaults.RetentionDays,
		DefaultMaxActiveDays:  runtimeCfg.DBDefaults.MaxActiveDays,
		DefaultPartition:      runtimeCfg.DBDefaults.Partition,
		DefaultPageMaxRecords: runtimeCfg.DBDefaults.PageMaxRecords,
		DefaultPageMaxBytes:   runtimeCfg.DBDefaults.PageMaxBytes,
		DefaultPageMaxAge:     runtimeCfg.DBDefaults.PageMaxAge,
		DefaultGrace:          runtimeCfg.DBDefaults.Grace,
	}
}

func handleEngineDatabase(eng *engine.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeVMError(w, http.StatusMethodNotAllowed, "bad_data", "method not allowed")
			return
		}
		database := strings.TrimSpace(r.URL.Query().Get("db"))
		if database == "" {
			writeVMError(w, http.StatusBadRequest, "bad_data", "missing db parameter")
			return
		}

		summary, err := buildEngineDatabaseSummary(eng, database)
		if err != nil {
			if strings.Contains(err.Error(), "database not found") {
				writeVMError(w, http.StatusNotFound, "not_found", err.Error())
				return
			}
			writeVMError(w, http.StatusInternalServerError, "execution", err.Error())
			return
		}
		metrics, err := eng.ListMetrics(database)
		if err != nil {
			if strings.Contains(err.Error(), "database not found") {
				writeVMError(w, http.StatusNotFound, "not_found", err.Error())
				return
			}
			writeVMError(w, http.StatusInternalServerError, "execution", err.Error())
			return
		}
		items := make([]engineMetricInfo, 0, len(metrics))
		for _, metric := range metrics {
			item := engineMetricInfo{Name: metric.Name, ID: uint16(metric.MetricID), Type: metricTypeName(metric.ValueType)}
			last, found, err := eng.QueryLast(database, metric.Name)
			if err != nil {
				writeVMError(w, http.StatusInternalServerError, "execution", err.Error())
				return
			}
			if found {
				item.LastTSNS = int64(last.TS)
				item.LastTimestamp = engine.FormatTimestamp(last.TS)
				if last.ValueType == engine.Int32Sample {
					item.LastValue = fmt.Sprintf("%d", last.Int32)
				} else {
					item.LastValue = fmt.Sprintf("%g", last.Float32)
				}
			}
			items = append(items, item)
		}

		writeJSON(w, http.StatusOK, vmResponse{
			Status: "success",
			Data: map[string]interface{}{
				"resultType": "engine_database",
				"result": engineDatabaseDetail{
					Summary: summary,
					Metrics: items,
				},
			},
		})
	}
}

func handleEngineFiles(eng *engine.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeVMError(w, http.StatusMethodNotAllowed, "bad_data", "method not allowed")
			return
		}
		database := strings.TrimSpace(r.URL.Query().Get("db"))
		if database == "" {
			writeVMError(w, http.StatusBadRequest, "bad_data", "missing db parameter")
			return
		}
		selectedDataFile := strings.TrimSpace(r.URL.Query().Get("data_file"))
		report, err := buildEngineFilesReport(eng, database, selectedDataFile)
		if err != nil {
			if strings.Contains(err.Error(), "database not found") {
				writeVMError(w, http.StatusNotFound, "not_found", err.Error())
				return
			}
			writeVMError(w, http.StatusInternalServerError, "execution", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, vmResponse{Status: "success", Data: map[string]interface{}{"resultType": "engine_files", "result": report}})
	}
}

func handleEngineRuntime(eng *engine.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeVMError(w, http.StatusMethodNotAllowed, "bad_data", "method not allowed")
			return
		}
		database := strings.TrimSpace(r.URL.Query().Get("db"))
		if database == "" {
			writeVMError(w, http.StatusBadRequest, "bad_data", "missing db parameter")
			return
		}
		runtimeInspect, ok := eng.InspectDBRuntime(database)
		if !ok {
			writeVMError(w, http.StatusNotFound, "not_found", fmt.Sprintf("database not found: %s", database))
			return
		}
		records, ok, err := eng.InspectDBWAL(database)
		if err != nil {
			writeVMError(w, http.StatusInternalServerError, "execution", err.Error())
			return
		}
		if !ok {
			writeVMError(w, http.StatusNotFound, "not_found", fmt.Sprintf("database not found: %s", database))
			return
		}

		writeJSON(w, http.StatusOK, vmResponse{
			Status: "success",
			Data: map[string]interface{}{
				"resultType": "engine_runtime",
				"result": engineRuntimeReport{
					Runtime: runtimeInspect,
					WAL:     convertRuntimeWALRecords(records),
				},
			},
		})
	}
}

func handleEngineRecompact(eng *engine.Engine) http.HandlerFunc {
	type recompactReq struct {
		Database string `json:"db"`
		Part     string `json:"part"`
	}

	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeVMError(w, http.StatusMethodNotAllowed, "bad_data", "method not allowed")
			return
		}

		var req recompactReq
		if r.Body != nil {
			defer r.Body.Close()
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeVMError(w, http.StatusBadRequest, "bad_data", fmt.Sprintf("invalid JSON body: %v", err))
				return
			}
		}

		report, err := eng.RecompactDataFile(strings.TrimSpace(req.Database), strings.TrimSpace(req.Part))
		if err != nil {
			switch {
			case errors.Is(err, engine.ErrDataFileActive):
				writeVMError(w, http.StatusConflict, "conflict", err.Error())
			case errors.Is(err, os.ErrNotExist):
				writeVMError(w, http.StatusNotFound, "not_found", err.Error())
			case strings.Contains(err.Error(), "required") || strings.Contains(err.Error(), "invalid partition key") || strings.Contains(err.Error(), "does not match database partitioning"):
				writeVMError(w, http.StatusBadRequest, "bad_data", err.Error())
			default:
				writeVMError(w, http.StatusInternalServerError, "execution", err.Error())
			}
			return
		}

		writeJSON(w, http.StatusOK, vmResponse{
			Status: "success",
			Data: map[string]interface{}{
				"resultType": "engine_recompact",
				"result":     report,
			},
		})
	}
}

func buildEngineDatabaseSummary(eng *engine.Engine, database string) (engineDatabaseSummary, error) {
	active := eng.IsDatabaseActive(database)
	runtimeInspect, ok := eng.InspectDBRuntime(database)
	if !ok {
		return engineDatabaseSummary{}, fmt.Errorf("database not found: %s", database)
	}
	ctx, err := resolveServerDBContext(eng.RootDataDir, database)
	if err != nil {
		return engineDatabaseSummary{}, err
	}
	var dataBytes int64
	for _, path := range ctx.DataFilePaths {
		st, err := os.Stat(path)
		if err != nil {
			return engineDatabaseSummary{}, err
		}
		dataBytes += st.Size()
	}
	var walBytes int64
	for _, path := range ctx.WALFilePaths {
		st, err := os.Stat(path)
		if err != nil {
			return engineDatabaseSummary{}, err
		}
		walBytes += st.Size()
	}
	return engineDatabaseSummary{
		Name:        database,
		MetricCount: runtimeInspect.MetricCount,
		Manifest:    runtimeInspect.Manifest,
		Stats:       runtimeInspect.Stats,
		OpenPages:   len(runtimeInspect.OpenPages),
		DataFiles:   len(ctx.DataFilePaths),
		DataBytes:   dataBytes,
		WALFiles:    len(ctx.WALFilePaths),
		WALBytes:    walBytes,
		Active:      active,
		Runtime:     runtimeInspect,
	}, nil
}

func buildEngineFilesReport(eng *engine.Engine, database string, selectedDataFile string) (engineFilesReport, error) {
	ctx, err := resolveServerDBContext(eng.RootDataDir, database)
	if err != nil {
		return engineFilesReport{}, err
	}
	runtimeInspect, ok := eng.InspectDBRuntime(database)
	if !ok {
		return engineFilesReport{}, fmt.Errorf("database not found: %s", database)
	}
	activeParts := make(map[string]bool, len(runtimeInspect.OpenPages))
	for _, page := range runtimeInspect.OpenPages {
		if !page.Persisted {
			activeParts[page.Day] = true
		}
	}
	metricNames, err := loadMetricNames(filepath.Join(ctx.DatabaseDir, "catalog.json"))
	if err != nil {
		return engineFilesReport{}, err
	}
	selectedDataFile = strings.TrimSpace(selectedDataFile)
	if selectedDataFile == "" && len(ctx.DataFilePaths) > 0 {
		selectedDataFile = ctx.DataFilePaths[0]
	}

	report := engineFilesReport{Database: database, Data: make([]engineDataFile, 0, len(ctx.DataFilePaths)), WAL: make([]engineWALFile, 0, len(ctx.WALFilePaths))}
	for _, path := range ctx.DataFilePaths {
		stats, err := engine.ScanDataFileStats(path)
		part := dataFilePart(path)
		if err != nil {
			report.Data = append(report.Data, engineDataFile{Path: path, Part: part, Active: activeParts[part], ScanError: err.Error()})
			continue
		}
		item := engineDataFile{
			Path:    path,
			Part:    part,
			Active:  activeParts[part],
			Bytes:   stats.FileBytes,
			Frames:  stats.Frames,
			Records: stats.TotalRecords,
		}
		if stats.Frames > 0 {
			item.MinTimestamp = int64(stats.MinStart)
			item.MaxTimestamp = int64(stats.MaxEnd)
			item.MinUTC = engine.FormatTimestamp(stats.MinStart)
			item.MaxUTC = engine.FormatTimestamp(stats.MaxEnd)
		}
		if selectedDataFile != "" && path == selectedDataFile {
			_, frames, err := engine.ScanDataFileHeaders(path)
			if err != nil {
				item.ScanError = err.Error()
				report.Data = append(report.Data, item)
				continue
			}
			item.Pages = make([]engineDataPage, 0, len(frames))
			for _, frame := range frames {
				durationNS := int64(frame.EndTime) - int64(frame.StartTime)
				if durationNS < 0 {
					durationNS = 0
				}
				avgDiskBytesPerPoint := 0.0
				if frame.NumRecords > 0 {
					avgDiskBytesPerPoint = float64(frame.FrameBytes) / float64(frame.NumRecords)
				}
				item.Pages = append(item.Pages, engineDataPage{
					Index:                frame.Index,
					Offset:               frame.Offset,
					FrameBytes:           frame.FrameBytes,
					CompressedLen:        frame.CompressedLen,
					UncompressedLen:      frame.UncompressedLen,
					Records:              frame.NumRecords,
					StartTS:              int64(frame.StartTime),
					EndTS:                int64(frame.EndTime),
					DurationNS:           durationNS,
					AvgDiskBytesPerPoint: avgDiskBytesPerPoint,
					StartUTC:             engine.FormatTimestamp(frame.StartTime),
					EndUTC:               engine.FormatTimestamp(frame.EndTime),
				})
			}
		}
		report.Data = append(report.Data, item)
	}

	for _, path := range ctx.WALFilePaths {
		stats, records, err := engine.ScanWALFile(path)
		if err != nil {
			report.WAL = append(report.WAL, engineWALFile{Path: path, ScanError: err.Error()})
			continue
		}
		item := engineWALFile{
			Path:         path,
			Bytes:        stats.FileBytes,
			Records:      stats.Records,
			DecodedBytes: stats.DecodedBytes,
			HasTail:      stats.HasTail,
			TailBytes:    stats.TailBytes,
			StopOffset:   stats.StopOffset,
			StopReason:   stats.StopReason,
		}
		if stats.Records > 0 {
			item.MinTimestamp = int64(stats.MinTS)
			item.MaxTimestamp = int64(stats.MaxTS)
			item.MinUTC = engine.FormatTimestamp(stats.MinTS)
			item.MaxUTC = engine.FormatTimestamp(stats.MaxTS)
		}
		for _, record := range records {
			report.Records = append(report.Records, engineWALRecord{
				Index:       record.Index,
				Offset:      record.Offset,
				RecordBytes: record.RecordBytes,
				MetricID:    uint16(record.MetricID),
				MetricName:  metricNames[record.MetricID],
				TimestampNS: int64(record.Timestamp),
				Timestamp:   engine.FormatTimestamp(record.Timestamp),
				ValueType:   metricTypeName(record.ValueType),
				Value:       record.Value,
			})
		}
		report.WAL = append(report.WAL, item)
	}

	sort.Slice(report.Records, func(i, j int) bool {
		if report.Records[i].TimestampNS != report.Records[j].TimestampNS {
			return report.Records[i].TimestampNS < report.Records[j].TimestampNS
		}
		return report.Records[i].Index < report.Records[j].Index
	})
	return report, nil
}

func dataFilePart(path string) string {
	base := filepath.Base(path)
	if !strings.HasPrefix(base, "data-") || !strings.HasSuffix(base, ".dat") {
		return ""
	}
	part := strings.TrimSuffix(strings.TrimPrefix(base, "data-"), ".dat")
	if part == base {
		return ""
	}
	return part
}

func loadMetricNames(catalogPath string) (map[engine.MetricID]string, error) {
	type metricDiskEntry struct {
		Name     string          `json:"name"`
		MetricID engine.MetricID `json:"id"`
	}
	type catalogDisk struct {
		Metrics []metricDiskEntry `json:"metrics"`
	}
	raw, err := os.ReadFile(catalogPath)
	if err != nil {
		if os.IsNotExist(err) {
			return map[engine.MetricID]string{}, nil
		}
		return nil, err
	}
	if len(raw) == 0 {
		return map[engine.MetricID]string{}, nil
	}
	var cat catalogDisk
	if err := json.Unmarshal(raw, &cat); err != nil {
		return nil, err
	}
	metricNames := make(map[engine.MetricID]string, len(cat.Metrics))
	for _, metric := range cat.Metrics {
		metricNames[metric.MetricID] = metric.Name
	}
	return metricNames, nil
}

func convertRuntimeWALRecords(records []engine.WALRecord) []engineWALRecord {
	out := make([]engineWALRecord, 0, len(records))
	for i, record := range records {
		out = append(out, engineWALRecord{
			Index:       i,
			SegmentID:   record.SegmentID,
			MetricID:    uint16(record.MetricID),
			MetricName:  record.MetricName,
			TimestampNS: int64(record.Timestamp),
			Timestamp:   engine.FormatTimestamp(record.Timestamp),
			ValueType:   metricTypeName(record.ValueType),
			Value:       record.Value,
		})
	}
	return out
}

func resolveServerDBContext(rootDir string, database string) (serverDBContext, error) {
	database = strings.Trim(strings.TrimSpace(database), "/")
	if database == "" {
		return serverDBContext{}, fmt.Errorf("missing db parameter")
	}
	dbDir := filepath.Join(rootDir, database)
	st, err := os.Stat(dbDir)
	if err != nil {
		if os.IsNotExist(err) {
			return serverDBContext{}, fmt.Errorf("database not found: %s", database)
		}
		return serverDBContext{}, err
	}
	if !st.IsDir() {
		return serverDBContext{}, fmt.Errorf("database not found: %s", database)
	}
	dataFiles, err := filepath.Glob(filepath.Join(dbDir, "data-*.dat"))
	if err != nil {
		return serverDBContext{}, err
	}
	walFiles, err := filepath.Glob(filepath.Join(dbDir, "*.wal"))
	if err != nil {
		return serverDBContext{}, err
	}
	sort.Strings(dataFiles)
	sort.Strings(walFiles)
	return serverDBContext{RootDir: rootDir, Database: database, DatabaseDir: dbDir, DataFilePaths: dataFiles, WALFilePaths: walFiles}, nil
}
