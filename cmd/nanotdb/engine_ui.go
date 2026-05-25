package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strings"
	"time"

	"github.com/aymanhs/nanotdb/internal/engine"
)

var engineProcessStartedAt = time.Now()

type serverDBContext struct {
	RootDir         string
	Database        string
	DatabaseDir     string
	DataFilePaths   []string
	MetricFilePaths []string
	WALFilePaths    []string
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

type engineMetricFile struct {
	Path            string `json:"path"`
	Part            string `json:"part,omitempty"`
	Bytes           int64  `json:"bytes"`
	Frames          int    `json:"frames"`
	DistinctMetrics int    `json:"distinct_metrics"`
	Points          int64  `json:"points"`
	AvgPayloadBytes int64  `json:"avg_payload_bytes,omitempty"`
	MinTimestamp    int64  `json:"min_timestamp_ns,omitempty"`
	MaxTimestamp    int64  `json:"max_timestamp_ns,omitempty"`
	MinUTC          string `json:"min_utc,omitempty"`
	MaxUTC          string `json:"max_utc,omitempty"`
	ScanError       string `json:"scan_error,omitempty"`
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

type engineWALPreview struct {
	Total int               `json:"total"`
	First []engineWALRecord `json:"first,omitempty"`
	Last  []engineWALRecord `json:"last,omitempty"`
}

type engineFilesReport struct {
	Database      string             `json:"database"`
	Data          []engineDataFile   `json:"data"`
	Metric        []engineMetricFile `json:"metric"`
	WAL           []engineWALFile    `json:"wal"`
	RecordPreview engineWALPreview   `json:"record_preview"`
}

type engineRuntimeReport struct {
	Runtime    engine.DBRuntimeInspect `json:"runtime"`
	WALPreview engineWALPreview        `json:"wal_preview"`
}

type engineRuntimeProcess struct {
	StartedAt    time.Time `json:"started_at"`
	AgeSeconds   int64     `json:"age_seconds"`
	RSSBytes     int64     `json:"rss_bytes"`
	NumGoroutine int       `json:"num_goroutine"`
	NumCPU       int       `json:"num_cpu"`
}

type engineRuntimeGoMem struct {
	AllocBytes      uint64    `json:"alloc_bytes"`
	TotalAllocBytes uint64    `json:"total_alloc_bytes"`
	SysBytes        uint64    `json:"sys_bytes"`
	HeapAllocBytes  uint64    `json:"heap_alloc_bytes"`
	HeapSysBytes    uint64    `json:"heap_sys_bytes"`
	HeapInuseBytes  uint64    `json:"heap_inuse_bytes"`
	HeapIdleBytes   uint64    `json:"heap_idle_bytes"`
	StackInuseBytes uint64    `json:"stack_inuse_bytes"`
	StackSysBytes   uint64    `json:"stack_sys_bytes"`
	NextGCBytes     uint64    `json:"next_gc_bytes"`
	NumGC           uint32    `json:"num_gc"`
	GCCPUFraction   float64   `json:"gc_cpu_fraction"`
	LastGCAt        time.Time `json:"last_gc_at,omitempty"`
}

type engineRuntimeOpenPage struct {
	Database     string `json:"database"`
	Day          string `json:"day"`
	Records      int    `json:"records"`
	MetricSlots  int    `json:"metric_slots"`
	UniqueMetric int    `json:"unique_metrics"`
	ValueBytes   int    `json:"value_bytes"`
	StartTS      int64  `json:"start_timestamp_ns"`
	EndTS        int64  `json:"end_timestamp_ns"`
	MaxRecords   int    `json:"max_records"`
	MaxBytes     int    `json:"max_bytes"`
	MaxAgeNS     int64  `json:"max_age_ns"`
	AgeNS        int64  `json:"age_ns"`
	WALSegmentID uint16 `json:"wal_segment_id"`
	Full         bool   `json:"full"`
	Persisted    bool   `json:"persisted"`
}

type engineRuntimeOverviewReport struct {
	DatabaseCount       int                     `json:"database_count"`
	ActiveDatabaseCount int                     `json:"active_database_count"`
	MetricCount         int                     `json:"metric_count"`
	Process             engineRuntimeProcess    `json:"process"`
	GoMem               engineRuntimeGoMem      `json:"go_mem"`
	OpenPages           []engineRuntimeOpenPage `json:"open_pages"`
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
			report, err := buildEngineRuntimeOverview(eng)
			if err != nil {
				writeVMError(w, http.StatusInternalServerError, "execution", err.Error())
				return
			}
			writeJSON(w, http.StatusOK, vmResponse{
				Status: "success",
				Data: map[string]interface{}{
					"resultType": "engine_runtime_overview",
					"result":     report,
				},
			})
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
					Runtime:    runtimeInspect,
					WALPreview: buildWALPreview(convertRuntimeWALRecords(records), 12),
				},
			},
		})
	}
}

func buildEngineRuntimeOverview(eng *engine.Engine) (engineRuntimeOverviewReport, error) {
	names := eng.GetAllDatabaseNames()
	memStats := goruntime.MemStats{}
	goruntime.ReadMemStats(&memStats)
	report := engineRuntimeOverviewReport{
		DatabaseCount: len(names),
		Process: engineRuntimeProcess{
			StartedAt:    engineProcessStartedAt,
			AgeSeconds:   int64(time.Since(engineProcessStartedAt).Seconds()),
			RSSBytes:     readProcessRSSBytes(),
			NumGoroutine: goruntime.NumGoroutine(),
			NumCPU:       goruntime.NumCPU(),
		},
		GoMem: engineRuntimeGoMem{
			AllocBytes:      memStats.Alloc,
			TotalAllocBytes: memStats.TotalAlloc,
			SysBytes:        memStats.Sys,
			HeapAllocBytes:  memStats.HeapAlloc,
			HeapSysBytes:    memStats.HeapSys,
			HeapInuseBytes:  memStats.HeapInuse,
			HeapIdleBytes:   memStats.HeapIdle,
			StackInuseBytes: memStats.StackInuse,
			StackSysBytes:   memStats.StackSys,
			NextGCBytes:     memStats.NextGC,
			NumGC:           memStats.NumGC,
			GCCPUFraction:   memStats.GCCPUFraction,
		},
		OpenPages: make([]engineRuntimeOpenPage, 0),
	}
	if memStats.LastGC > 0 {
		report.GoMem.LastGCAt = time.Unix(0, int64(memStats.LastGC)).UTC()
	}
	for _, name := range names {
		if eng.IsDatabaseActive(name) {
			report.ActiveDatabaseCount++
		}
		runtimeInspect, ok := eng.InspectDBRuntime(name)
		if !ok {
			continue
		}
		report.MetricCount += runtimeInspect.MetricCount
		for _, page := range runtimeInspect.OpenPages {
			report.OpenPages = append(report.OpenPages, engineRuntimeOpenPage{
				Database:     name,
				Day:          page.Day,
				Records:      page.Records,
				MetricSlots:  page.MetricSlots,
				UniqueMetric: page.UniqueMetric,
				ValueBytes:   page.ValueBytes,
				StartTS:      int64(page.StartTS),
				EndTS:        int64(page.EndTS),
				MaxRecords:   page.MaxRecords,
				MaxBytes:     page.MaxBytes,
				MaxAgeNS:     int64(page.MaxAge),
				AgeNS:        int64(page.Age),
				WALSegmentID: page.WALSegmentID,
				Full:         page.Full,
				Persisted:    page.Persisted,
			})
		}
	}
	sort.Slice(report.OpenPages, func(i, j int) bool {
		if report.OpenPages[i].Database != report.OpenPages[j].Database {
			return report.OpenPages[i].Database < report.OpenPages[j].Database
		}
		if report.OpenPages[i].Day != report.OpenPages[j].Day {
			return report.OpenPages[i].Day < report.OpenPages[j].Day
		}
		return report.OpenPages[i].StartTS < report.OpenPages[j].StartTS
	})
	return report, nil
}

func readProcessRSSBytes() int64 {
	raw, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		var kb int64
		if _, err := fmt.Sscanf(fields[1], "%d", &kb); err != nil {
			return 0
		}
		return kb * 1024
	}
	return 0
}

func handleEngineCompactMetric(eng *engine.Engine) http.HandlerFunc {
	type compactReq struct {
		Database string `json:"db"`
		Part     string `json:"part"`
	}

	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeVMError(w, http.StatusMethodNotAllowed, "bad_data", "method not allowed")
			return
		}

		var req compactReq
		if r.Body != nil {
			defer r.Body.Close()
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeVMError(w, http.StatusBadRequest, "bad_data", fmt.Sprintf("invalid JSON body: %v", err))
				return
			}
		}

		report, err := eng.CompactDataFileToMetricV2(strings.TrimSpace(req.Database), strings.TrimSpace(req.Part))
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
				"resultType": "engine_compact_metric",
				"result":     report,
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

	report := engineFilesReport{Database: database, Data: make([]engineDataFile, 0, len(ctx.DataFilePaths)), Metric: make([]engineMetricFile, 0, len(ctx.MetricFilePaths)), WAL: make([]engineWALFile, 0, len(ctx.WALFilePaths))}
	allRecords := make([]engineWALRecord, 0)
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

	for _, path := range ctx.MetricFilePaths {
		st, err := os.Stat(path)
		part := metricFilePart(path)
		if err != nil {
			report.Metric = append(report.Metric, engineMetricFile{Path: path, Part: part, ScanError: err.Error()})
			continue
		}
		item := engineMetricFile{Path: path, Part: part, Bytes: st.Size()}
		summary, err := engine.ReadMetricFileSummary(path)
		if err != nil {
			item.ScanError = err.Error()
			report.Metric = append(report.Metric, item)
			continue
		}
		seenMetrics := make(map[engine.MetricID]struct{}, len(summary.MetricFrames))
		var totalPayload int64
		for _, info := range summary.MetricFrames {
			item.Frames++
			seenMetrics[info.MetricID] = struct{}{}
			item.Points += int64(info.PointCount)
			totalPayload += int64(info.PayloadLen)
			if item.MinTimestamp == 0 || int64(info.MetricMinTS) < item.MinTimestamp {
				item.MinTimestamp = int64(info.MetricMinTS)
			}
			if item.MaxTimestamp == 0 || int64(info.MetricMaxTS) > item.MaxTimestamp {
				item.MaxTimestamp = int64(info.MetricMaxTS)
			}
		}
		item.DistinctMetrics = len(seenMetrics)
		if item.Frames > 0 {
			item.AvgPayloadBytes = totalPayload / int64(item.Frames)
			item.MinUTC = engine.FormatTimestamp(engine.Timestamp(item.MinTimestamp))
			item.MaxUTC = engine.FormatTimestamp(engine.Timestamp(item.MaxTimestamp))
		}
		report.Metric = append(report.Metric, item)
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
			allRecords = append(allRecords, engineWALRecord{
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

	sortEngineWALRecords(allRecords)
	report.RecordPreview = buildWALPreview(allRecords, 12)
	return report, nil
}

func sortEngineWALRecords(records []engineWALRecord) {
	sort.Slice(records, func(i, j int) bool {
		if records[i].TimestampNS != records[j].TimestampNS {
			return records[i].TimestampNS < records[j].TimestampNS
		}
		if records[i].SegmentID != records[j].SegmentID {
			return records[i].SegmentID < records[j].SegmentID
		}
		return records[i].Index < records[j].Index
	})
}

func buildWALPreview(records []engineWALRecord, limit int) engineWALPreview {
	preview := engineWALPreview{Total: len(records)}
	if len(records) == 0 || limit <= 0 {
		return preview
	}
	if limit > len(records) {
		limit = len(records)
	}
	preview.First = append([]engineWALRecord(nil), records[:limit]...)
	if len(records) <= limit {
		return preview
	}
	preview.Last = append([]engineWALRecord(nil), records[len(records)-limit:]...)
	return preview
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

func metricFilePart(path string) string {
	base := filepath.Base(path)
	if !strings.HasPrefix(base, "metric-") || !strings.HasSuffix(base, ".dat") {
		return ""
	}
	part := strings.TrimSuffix(strings.TrimPrefix(base, "metric-"), ".dat")
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
	metricFiles, err := filepath.Glob(filepath.Join(dbDir, "metric-*.dat"))
	if err != nil {
		return serverDBContext{}, err
	}
	walFiles, err := filepath.Glob(filepath.Join(dbDir, "*.wal"))
	if err != nil {
		return serverDBContext{}, err
	}
	sort.Strings(dataFiles)
	sort.Strings(metricFiles)
	sort.Strings(walFiles)
	return serverDBContext{RootDir: rootDir, Database: database, DatabaseDir: dbDir, DataFilePaths: dataFiles, MetricFilePaths: metricFiles, WALFilePaths: walFiles}, nil
}
