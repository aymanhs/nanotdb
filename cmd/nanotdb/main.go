package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/aymanhs/nanotdb/internal/engine"
	"github.com/aymanhs/nanotdb/internal/web"
)

type runtimeConfig struct {
	DataDir       string
	EngineConfig  engine.EngineConfig
	StatsInterval time.Duration
	DBDefaults    engine.DBInfo
	WebConfig     web.Config
}

type vmResponse struct {
	Status    string      `json:"status"`
	Data      interface{} `json:"data,omitempty"`
	ErrorType string      `json:"errorType,omitempty"`
	Error     string      `json:"error,omitempty"`
}

type vmMetric map[string]string

type vmVectorItem struct {
	Metric vmMetric       `json:"metric"`
	Value  [2]interface{} `json:"value"`
}

type vmMatrixItem struct {
	Metric vmMetric         `json:"metric"`
	Values [][2]interface{} `json:"values"`
}

func main() {
	configPath := flag.String("config", "./devdata/engine.toml", "path to engine config TOML")
	initOnly := flag.Bool("init", false, "create default config file and exit")
	exportWebAssets := flag.String("export-web-assets", "", "export embedded web UI assets to a directory and exit")
	flag.Parse()

	if strings.TrimSpace(*exportWebAssets) != "" {
		if err := web.ExportAssets(*exportWebAssets); err != nil {
			fmt.Fprintf(os.Stderr, "export web assets failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "exported web assets to %s\n", *exportWebAssets)
		return
	}

	if *initOnly {
		if err := initConfigFile(*configPath); err != nil {
			fmt.Fprintf(os.Stderr, "init failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "initialized config at %s\n", *configPath)
		return
	}

	runtimeCfg, err := loadRuntimeConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load runtime config failed: %v\n", err)
		os.Exit(1)
	}
	logger, closeLogger, err := engine.NewLogger(runtimeCfg.EngineConfig.Logging)
	if err != nil {
		fmt.Fprintf(os.Stderr, "initialize logger failed: %v\n", err)
		os.Exit(1)
	}
	defer func() {
		if err := closeLogger(); err != nil {
			fmt.Fprintf(os.Stderr, "close logger failed: %v\n", err)
		}
	}()
	slog.SetDefault(logger)

	eng, err := engine.OpenEngineWithConfig(runtimeCfg.DataDir, runtimeCfg.EngineConfig, runtimeCfg.StatsInterval, runtimeCfg.DBDefaults, logger)
	if err != nil {
		logger.Error("open engine failed", "error", err)
		os.Exit(1)
	}
	defer func() {
		if err := eng.Close(); err != nil {
			logger.Error("engine close failed", "error", err)
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/api/v1/import", handleImport(eng))
	mux.HandleFunc("/api/v1/import/prometheus", handleImport(eng))
	mux.HandleFunc("/api/v1/rollup/backfill", handleRollupBackfill(eng))
	mux.HandleFunc("/api/v1/query", handleQuery(eng))
	mux.HandleFunc("/api/v1/query_range", handleQueryRange(eng))
	mux.HandleFunc("/api/v1/aggregates", handleAggregates())
	mux.HandleFunc("/api/v1/databases", handleDatabases(eng))
	mux.HandleFunc("/api/v1/metrics", handleMetrics(eng))
	mux.HandleFunc("/api/engine/overview", handleEngineOverview(eng, runtimeCfg))
	mux.HandleFunc("/api/engine/database", handleEngineDatabase(eng))
	mux.HandleFunc("/api/engine/files", handleEngineFiles(eng))
	mux.HandleFunc("/api/engine/compact_metric", handleEngineCompactMetric(eng))
	mux.HandleFunc("/api/engine/recompact", handleEngineRecompact(eng))
	mux.HandleFunc("/api/engine/runtime", handleEngineRuntime(eng))
	web.Register(mux, runtimeCfg.WebConfig, runtimeCfg.DataDir)

	srv := &http.Server{
		Addr:              runtimeCfg.EngineConfig.Engine.Listen,
		Handler:           withRequestLogging(logger, mux),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		logger.Info("shutdown signal received")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	logger.Info("nanotdb server listening", "listen", runtimeCfg.EngineConfig.Engine.Listen, "config", *configPath, "data_dir", runtimeCfg.DataDir)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server failed", "error", err)
		os.Exit(1)
	}
}

func configDataDir(configPath string) (string, error) {
	if filepath.Base(configPath) != "engine.toml" {
		return "", fmt.Errorf("config file must be named engine.toml: %s", configPath)
	}
	return filepath.Dir(configPath), nil
}

func initConfigFile(configPath string) error {
	dataDir, err := configDataDir(configPath)
	if err != nil {
		return err
	}
	eng, err := engine.OpenEngine(dataDir, 0)
	if err != nil {
		return err
	}
	if err := eng.Close(); err != nil {
		return err
	}

	// Create a sample dashboard.json if it doesn't already exist
	dashboardPath := filepath.Join(dataDir, "dashboard.json")
	if _, err := os.Stat(dashboardPath); os.IsNotExist(err) {
		if err := os.WriteFile(dashboardPath, append(web.DefaultDashboardConfig(), '\n'), 0o644); err != nil {
			return fmt.Errorf("write dashboard config: %w", err)
		}
	}

	return nil
}

func loadRuntimeConfig(configPath string) (runtimeConfig, error) {
	dataDir, err := configDataDir(configPath)
	if err != nil {
		return runtimeConfig{}, err
	}
	cfg, statsInterval, dbDefaults, err := engine.LoadEngineConfig(dataDir, 0)
	if err != nil {
		return runtimeConfig{}, err
	}
	var webTOML struct {
		Web struct {
			Enabled        *bool  `toml:"enabled"`
			BasePath       string `toml:"base_path"`
			ExplorePath    string `toml:"explore_path"`
			Title          string `toml:"title"`
			RefreshSeconds int    `toml:"refresh_seconds"`
			DashboardFile  string `toml:"dashboard_config"`
			WebRoot        string `toml:"web_root"`
			APIBaseURL     string `toml:"api_base_url"`
			EnginePath     string `toml:"engine_path"`
		} `toml:"web"`
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return runtimeConfig{}, err
	}
	if _, err := toml.Decode(string(raw), &webTOML); err != nil {
		return runtimeConfig{}, err
	}
	webCfg := web.DefaultConfig()
	if webTOML.Web.Enabled != nil {
		webCfg.Enabled = *webTOML.Web.Enabled
	}
	if v := strings.TrimSpace(webTOML.Web.BasePath); v != "" {
		webCfg.BasePath = v
	}
	if v := strings.TrimSpace(webTOML.Web.ExplorePath); v != "" {
		webCfg.ExplorePath = v
	}
	if v := strings.TrimSpace(webTOML.Web.Title); v != "" {
		webCfg.Title = v
	}
	if webTOML.Web.RefreshSeconds > 0 {
		webCfg.RefreshSeconds = webTOML.Web.RefreshSeconds
	}
	if v := strings.TrimSpace(webTOML.Web.DashboardFile); v != "" {
		webCfg.DashboardFile = v
	}
	if v := strings.TrimSpace(webTOML.Web.WebRoot); v != "" {
		webCfg.WebRoot = v
	}
	if v := strings.TrimSpace(webTOML.Web.APIBaseURL); v != "" {
		webCfg.APIBaseURL = v
	}
	if v := strings.TrimSpace(webTOML.Web.EnginePath); v != "" {
		webCfg.EnginePath = v
	}
	return runtimeConfig{DataDir: dataDir, EngineConfig: cfg, StatsInterval: statsInterval, DBDefaults: dbDefaults, WebConfig: webCfg}, nil
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (w *statusRecorder) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func withRequestLogging(logger *slog.Logger, next http.Handler) http.Handler {
	if logger == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !logger.Enabled(r.Context(), engine.TraceSlogLevel) {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		logger.Log(r.Context(), engine.TraceSlogLevel, "http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration", time.Since(start),
			"remote_addr", r.RemoteAddr,
		)
	})
}

func handleImport(eng *engine.Engine) http.HandlerFunc {
	type importReq struct {
		Lines string `json:"lines"`
	}

	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeVMError(w, http.StatusMethodNotAllowed, "bad_data", "method not allowed")
			return
		}

		var src io.Reader
		if strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
			var req importReq
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeVMError(w, http.StatusBadRequest, "bad_data", fmt.Sprintf("invalid JSON body: %v", err))
				return
			}
			src = strings.NewReader(req.Lines)
		} else {
			src = r.Body
		}

		imported, err := importLines(eng, src)
		if err != nil {
			writeVMError(w, http.StatusBadRequest, "bad_data", err.Error())
			return
		}

		writeJSON(w, http.StatusOK, vmResponse{
			Status: "success",
			Data: map[string]interface{}{
				"imported": imported,
			},
		})
	}
}

func handleRollupBackfill(eng *engine.Engine) http.HandlerFunc {
	type backfillReq struct {
		SourceDB  string   `json:"source_db"`
		SourceDBs []string `json:"source_dbs"`
	}

	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeVMError(w, http.StatusMethodNotAllowed, "bad_data", "method not allowed")
			return
		}

		var req backfillReq
		if r.Body != nil {
			defer r.Body.Close()
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
				writeVMError(w, http.StatusBadRequest, "bad_data", fmt.Sprintf("invalid JSON body: %v", err))
				return
			}
		}

		requested := make([]string, 0, len(req.SourceDBs)+1)
		if db := strings.TrimSpace(req.SourceDB); db != "" {
			requested = append(requested, db)
		}
		for _, db := range req.SourceDBs {
			if db = strings.TrimSpace(db); db != "" {
				requested = append(requested, db)
			}
		}

		report, err := eng.BackfillRollups(requested)
		if err != nil {
			writeVMError(w, http.StatusInternalServerError, "execution", err.Error())
			return
		}

		writeJSON(w, http.StatusOK, vmResponse{
			Status: "success",
			Data: map[string]interface{}{
				"resultType": "rollup_backfill",
				"result":     report,
			},
		})
	}
}

func handleQuery(eng *engine.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeVMError(w, http.StatusMethodNotAllowed, "bad_data", "method not allowed")
			return
		}

		database, metric, err := resolveDBAndMetric(r.URL.Query().Get("db"), r.URL.Query().Get("query"))
		if err != nil {
			writeVMError(w, http.StatusBadRequest, "bad_data", err.Error())
			return
		}

		sample, found, err := eng.QueryLast(database, metric)
		if err != nil {
			writeVMError(w, http.StatusInternalServerError, "execution", err.Error())
			return
		}

		result := make([]vmVectorItem, 0, 1)
		if found {
			result = append(result, vmVectorItem{
				Metric: vmMetric{"__name__": metric, "db": database},
				Value:  [2]interface{}{toUnixSeconds(sample.TS), sampleValueString(sample)},
			})
		}

		writeJSON(w, http.StatusOK, vmResponse{
			Status: "success",
			Data: map[string]interface{}{
				"resultType": "vector",
				"result":     result,
			},
		})
	}
}

func handleQueryRange(eng *engine.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeVMError(w, http.StatusMethodNotAllowed, "bad_data", "method not allowed")
			return
		}

		database, metric, err := resolveDBAndMetric(r.URL.Query().Get("db"), r.URL.Query().Get("query"))
		if err != nil {
			writeVMError(w, http.StatusBadRequest, "bad_data", err.Error())
			return
		}

		tsUnit, err := engine.NormalizeTimestampUnit(r.URL.Query().Get("timestamp_unit"))
		if err != nil {
			writeVMError(w, http.StatusBadRequest, "bad_data", err.Error())
			return
		}
		start, err := parseTimeParam(r.URL.Query().Get("start"), tsUnit)
		if err != nil {
			writeVMError(w, http.StatusBadRequest, "bad_data", fmt.Sprintf("invalid start: %v", err))
			return
		}
		end := engine.Timestamp(math.MaxInt64)
		if rawEnd := strings.TrimSpace(r.URL.Query().Get("end")); rawEnd != "" {
			end, err = parseTimeParam(rawEnd, tsUnit)
			if err != nil {
				writeVMError(w, http.StatusBadRequest, "bad_data", fmt.Sprintf("invalid end: %v", err))
				return
			}
		}
		if end < start {
			writeVMError(w, http.StatusBadRequest, "bad_data", "end must be >= start")
			return
		}

		stepNS, err := parseStepParam(r.URL.Query().Get("step"))
		if err != nil {
			writeVMError(w, http.StatusBadRequest, "bad_data", fmt.Sprintf("invalid step: %v", err))
			return
		}

		aggregateText := strings.TrimSpace(r.URL.Query().Get("aggregate"))
		windowText := strings.TrimSpace(r.URL.Query().Get("window"))
		if aggregateText == "" && windowText == "" && stepNS > 0 {
			aggregateText = engine.DefaultStepAggregate()
			windowText = time.Duration(stepNS).String()
		}
		aggregateMode := aggregateText != "" || windowText != ""
		if aggregateMode {
			if aggregateText == "" || windowText == "" {
				writeVMError(w, http.StatusBadRequest, "bad_data", "aggregate queries require both aggregate and window")
				return
			}
			window, err := time.ParseDuration(windowText)
			if err != nil || window <= 0 {
				writeVMError(w, http.StatusBadRequest, "bad_data", fmt.Sprintf("invalid window: %v", err))
				return
			}
			aggregateNames := splitCSVList(aggregateText)
			valuesByAggregate := make(map[string][][2]interface{}, len(aggregateNames))
			orderedAggregates := make([]string, 0, len(aggregateNames))
			err = eng.QueryAggregateRange(database, metric, start, end, window, aggregateNames, func(bucket engine.AggregateBucket) error {
				if _, ok := valuesByAggregate[bucket.Aggregate]; !ok {
					orderedAggregates = append(orderedAggregates, bucket.Aggregate)
				}
				valuesByAggregate[bucket.Aggregate] = append(valuesByAggregate[bucket.Aggregate], [2]interface{}{toUnixSeconds(bucket.EndTS), strconv.FormatFloat(float64(bucket.Value), 'f', -1, 32)})
				return nil
			})
			if err != nil {
				writeVMError(w, http.StatusBadRequest, "bad_data", err.Error())
				return
			}

			result := make([]vmMatrixItem, 0, len(orderedAggregates))
			for _, aggregate := range orderedAggregates {
				result = append(result, vmMatrixItem{
					Metric: vmMetric{"__name__": metric, "db": database, "aggregate": aggregate, "window": window.String()},
					Values: valuesByAggregate[aggregate],
				})
			}

			writeJSON(w, http.StatusOK, vmResponse{
				Status: "success",
				Data: map[string]interface{}{
					"resultType": "matrix",
					"result":     result,
				},
			})
			return
		}

		values := make([][2]interface{}, 0, 256)
		lastBucket := engine.Timestamp(math.MinInt64)
		var pendingSample *engine.Sample
		emitPending := func() {
			if pendingSample == nil {
				return
			}
			values = append(values, [2]interface{}{toUnixSeconds(pendingSample.TS), sampleValueString(*pendingSample)})
			pendingSample = nil
		}
		err = eng.QueryRange(database, metric, start, end, 1, func(s engine.Sample) error {
			if stepNS > 0 {
				bucket := alignTimestampToStep(s.TS, stepNS)
				if pendingSample == nil {
					pendingSample = &s
					lastBucket = bucket
					return nil
				}
				if bucket != lastBucket {
					emitPending()
					lastBucket = bucket
				}
				copySample := s
				pendingSample = &copySample
				return nil
			}
			values = append(values, [2]interface{}{toUnixSeconds(s.TS), sampleValueString(s)})
			return nil
		})
		if err != nil {
			writeVMError(w, http.StatusInternalServerError, "execution", err.Error())
			return
		}
		if stepNS > 0 {
			emitPending()
		}

		result := make([]vmMatrixItem, 0, 1)
		if len(values) > 0 {
			result = append(result, vmMatrixItem{
				Metric: vmMetric{"__name__": metric, "db": database},
				Values: values,
			})
		}

		writeJSON(w, http.StatusOK, vmResponse{
			Status: "success",
			Data: map[string]interface{}{
				"resultType": "matrix",
				"result":     result,
			},
		})
	}
}

func handleAggregates() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeVMError(w, http.StatusMethodNotAllowed, "bad_data", "method not allowed")
			return
		}
		writeJSON(w, http.StatusOK, vmResponse{
			Status: "success",
			Data: map[string]interface{}{
				"resultType": "aggregates",
				"result":     engine.SupportedAggregates(),
				"default":    engine.DefaultStepAggregate(),
			},
		})
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

func handleDatabases(eng *engine.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeVMError(w, http.StatusMethodNotAllowed, "bad_data", "method not allowed")
			return
		}

		includeInternal := false
		if raw := strings.TrimSpace(r.URL.Query().Get("include_internal")); raw != "" {
			parsed, err := strconv.ParseBool(raw)
			if err != nil {
				writeVMError(w, http.StatusBadRequest, "bad_data", "invalid include_internal: must be true or false")
				return
			}
			includeInternal = parsed
		}

		names := eng.GetAllDatabaseNames()
		if !includeInternal {
			filtered := make([]string, 0, len(names))
			for _, name := range names {
				if name == "internal" {
					continue
				}
				filtered = append(filtered, name)
			}
			names = filtered
		}

		writeJSON(w, http.StatusOK, vmResponse{
			Status: "success",
			Data: map[string]interface{}{
				"resultType": "databases",
				"result":     names,
			},
		})
	}
}

func handleMetrics(eng *engine.Engine) http.HandlerFunc {
	type metricRollupDownstream struct {
		Hop       int    `json:"hop"`
		JobID     string `json:"job_id"`
		Interval  string `json:"interval"`
		Aggregate string `json:"aggregate"`
		DB        string `json:"db"`
		Metric    string `json:"metric"`
	}
	type metricRollups struct {
		Downstream []metricRollupDownstream `json:"downstream"`
		Truncated  bool                     `json:"truncated"`
		MaxHops    int                      `json:"max_hops"`
	}
	type metricDetails struct {
		Name    string         `json:"name"`
		ID      uint16         `json:"id"`
		Type    string         `json:"type"`
		Rollups *metricRollups `json:"rollups,omitempty"`
	}

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

		details := false
		if raw := strings.TrimSpace(r.URL.Query().Get("details")); raw != "" {
			parsed, err := strconv.ParseBool(raw)
			if err != nil {
				writeVMError(w, http.StatusBadRequest, "bad_data", "invalid details: must be true or false")
				return
			}
			details = parsed
		}

		lineageMode := strings.TrimSpace(r.URL.Query().Get("lineage"))
		includeRollups := false
		maxHops := 1
		if lineageMode != "" {
			if !details {
				writeVMError(w, http.StatusBadRequest, "bad_data", "lineage requires details=true")
				return
			}
			if lineageMode != "rollups" {
				writeVMError(w, http.StatusBadRequest, "bad_data", "invalid lineage: supported values are rollups")
				return
			}
			includeRollups = true

			if raw := strings.TrimSpace(r.URL.Query().Get("max_hops")); raw != "" {
				parsed, err := strconv.Atoi(raw)
				if err != nil || parsed < 1 || parsed > 5 {
					writeVMError(w, http.StatusBadRequest, "bad_data", "invalid max_hops: must be in range [1,5]")
					return
				}
				maxHops = parsed
			}
		}

		metrics, err := eng.ListMetrics(database)
		if err != nil {
			if strings.HasPrefix(err.Error(), "database not found: ") {
				writeVMError(w, http.StatusNotFound, "not_found", err.Error())
				return
			}
			writeVMError(w, http.StatusBadRequest, "bad_data", err.Error())
			return
		}

		if !details {
			names := make([]string, 0, len(metrics))
			for _, m := range metrics {
				names = append(names, m.Name)
			}
			writeJSON(w, http.StatusOK, vmResponse{
				Status: "success",
				Data: map[string]interface{}{
					"resultType": "metrics",
					"db":         database,
					"result":     names,
				},
			})
			return
		}

		items := make([]metricDetails, 0, len(metrics))
		for _, m := range metrics {
			item := metricDetails{
				Name: m.Name,
				ID:   uint16(m.MetricID),
				Type: metricTypeName(m.ValueType),
			}
			if includeRollups {
				downstream, truncated, err := eng.GetMetricRollupDownstream(database, m.Name, maxHops)
				if err != nil {
					if strings.HasPrefix(err.Error(), "database not found: ") {
						writeVMError(w, http.StatusNotFound, "not_found", err.Error())
						return
					}
					writeVMError(w, http.StatusBadRequest, "bad_data", err.Error())
					return
				}
				rollupItems := make([]metricRollupDownstream, 0, len(downstream))
				for _, d := range downstream {
					rollupItems = append(rollupItems, metricRollupDownstream{
						Hop:       d.Hop,
						JobID:     d.JobID,
						Interval:  d.Interval,
						Aggregate: d.Aggregate,
						DB:        d.Database,
						Metric:    d.Metric,
					})
				}
				item.Rollups = &metricRollups{Downstream: rollupItems, Truncated: truncated, MaxHops: maxHops}
			}
			items = append(items, metricDetails{
				Name:    item.Name,
				ID:      item.ID,
				Type:    item.Type,
				Rollups: item.Rollups,
			})
		}

		writeJSON(w, http.StatusOK, vmResponse{
			Status: "success",
			Data: map[string]interface{}{
				"resultType": "metrics",
				"db":         database,
				"result":     items,
			},
		})
	}
}

func importLines(eng *engine.Engine, source io.Reader) (int, error) {
	s := bufio.NewScanner(source)
	s.Buffer(make([]byte, 64*1024), 4*1024*1024)
	imported := 0
	lineNo := 0
	for s.Scan() {
		lineNo++
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if err := eng.AddLine(line); err != nil {
			return imported, fmt.Errorf("line %d: %w", lineNo, err)
		}
		imported++
	}
	if err := s.Err(); err != nil {
		return imported, err
	}
	return imported, nil
}

func resolveDBAndMetric(db, query string) (string, string, error) {
	query = strings.TrimSpace(query)
	db = strings.TrimSpace(db)
	if query == "" {
		return "", "", fmt.Errorf("missing query parameter")
	}
	if db != "" {
		return db, query, nil
	}
	if i := strings.IndexByte(query, '/'); i > 0 && i < len(query)-1 {
		return query[:i], query[i+1:], nil
	}
	return "", "", fmt.Errorf("missing db parameter or DB/metric query")
}

// parseTimeParam parses an HTTP query-string time value. Bare numerics are
// interpreted with `tsUnit` (ns/us/ms/s); the empty string defaults to ns.
// Magnitude-guessing heuristics were removed because they silently misclassified
// ms-since-epoch values (year 2023, value > 1e12) as nanoseconds.
func parseTimeParam(v string, tsUnit string) (engine.Timestamp, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, fmt.Errorf("missing time value")
	}
	return engine.ParseTimestampWithUnit(v, tsUnit)
}

func alignTimestampToStep(ts, step engine.Timestamp) engine.Timestamp {
	if step <= 0 {
		return ts
	}
	return engine.Timestamp((int64(ts) / int64(step)) * int64(step))
}

func parseStepParam(v string) (engine.Timestamp, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, nil
	}
	if d, err := time.ParseDuration(v); err == nil {
		if d < 0 {
			return 0, fmt.Errorf("step must be >= 0")
		}
		return engine.Timestamp(d.Nanoseconds()), nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, err
	}
	if f < 0 {
		return 0, fmt.Errorf("step must be >= 0")
	}
	return engine.Timestamp(f * float64(time.Second)), nil
}

func sampleValueString(s engine.Sample) string {
	if s.ValueType == engine.Int32Sample {
		return strconv.FormatInt(int64(s.Int32), 10)
	}
	return strconv.FormatFloat(float64(s.Float32), 'f', -1, 32)
}

func metricTypeName(vt byte) string {
	if vt == engine.Int32Sample {
		return "int32"
	}
	if vt == engine.Float32Sample {
		return "float32"
	}
	return "unknown"
}

func toUnixSeconds(ts engine.Timestamp) float64 {
	return float64(ts) / float64(time.Second)
}

func writeVMError(w http.ResponseWriter, code int, errorType, msg string) {
	writeJSON(w, code, vmResponse{Status: "error", ErrorType: errorType, Error: msg})
}

func writeJSON(w http.ResponseWriter, code int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(payload)
}
