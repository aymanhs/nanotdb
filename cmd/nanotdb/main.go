package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
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
)

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
	flag.Parse()

	if *initOnly {
		if err := initConfigFile(*configPath); err != nil {
			log.Fatalf("init failed: %v", err)
		}
		log.Printf("initialized config at %s", *configPath)
		return
	}

	listenAddr, dataDir, walMaxSegBytes, err := loadRuntimeConfig(*configPath)
	if err != nil {
		log.Fatalf("load runtime config failed: %v", err)
	}

	eng, err := engine.OpenEngine(dataDir, walMaxSegBytes)
	if err != nil {
		log.Fatalf("open engine failed: %v", err)
	}
	defer func() {
		if err := eng.Close(); err != nil {
			log.Printf("engine close failed: %v", err)
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
	mux.HandleFunc("/api/v1/databases", handleDatabases(eng))
	mux.HandleFunc("/api/v1/metrics", handleMetrics(eng))

	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	log.Printf("nanotdb server listening on %s (config=%s data-dir=%s)", listenAddr, *configPath, dataDir)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server failed: %v", err)
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
	return eng.Close()
}

func loadRuntimeConfig(configPath string) (listenAddr string, dataDir string, walMaxSegBytes int64, err error) {
	dataDir, err = configDataDir(configPath)
	if err != nil {
		return "", "", 0, err
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return "", "", 0, err
	}
	var cfg struct {
		Engine struct {
			Listen string `toml:"listen"`
		} `toml:"engine"`
		WAL struct {
			MaxSegmentSize int64 `toml:"max_segment_size"`
		} `toml:"wal"`
	}
	if _, err := toml.Decode(string(raw), &cfg); err != nil {
		return "", "", 0, err
	}
	listenAddr = strings.TrimSpace(cfg.Engine.Listen)
	if listenAddr == "" {
		listenAddr = ":8428"
	}
	walMaxSegBytes = cfg.WAL.MaxSegmentSize
	if walMaxSegBytes <= 0 {
		walMaxSegBytes = 64 * 1024 * 1024
	}
	return listenAddr, dataDir, walMaxSegBytes, nil
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

		start, err := parseTimeParam(r.URL.Query().Get("start"))
		if err != nil {
			writeVMError(w, http.StatusBadRequest, "bad_data", fmt.Sprintf("invalid start: %v", err))
			return
		}
		end, err := parseTimeParam(r.URL.Query().Get("end"))
		if err != nil {
			writeVMError(w, http.StatusBadRequest, "bad_data", fmt.Sprintf("invalid end: %v", err))
			return
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

		values := make([][2]interface{}, 0, 256)
		lastEmitted := engine.Timestamp(0)
		err = eng.QueryRange(database, metric, start, end, 1, func(s engine.Sample) error {
			if stepNS > 0 && lastEmitted != 0 && s.TS-lastEmitted < stepNS {
				return nil
			}
			lastEmitted = s.TS
			values = append(values, [2]interface{}{toUnixSeconds(s.TS), sampleValueString(s)})
			return nil
		})
		if err != nil {
			writeVMError(w, http.StatusInternalServerError, "execution", err.Error())
			return
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
	type metricDetails struct {
		Name string `json:"name"`
		ID   uint16 `json:"id"`
		Type string `json:"type"`
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
			items = append(items, metricDetails{
				Name: m.Name,
				ID:   uint16(m.MetricID),
				Type: metricTypeName(m.ValueType),
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

func parseTimeParam(v string) (engine.Timestamp, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, fmt.Errorf("missing time value")
	}
	if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
		return engine.Timestamp(t.UnixNano()), nil
	}
	if strings.Contains(v, ".") {
		f, err := strconv.ParseFloat(v, 64)
		if err == nil {
			return engine.Timestamp(f * float64(time.Second)), nil
		}
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err == nil {
		if n > 1_000_000_000_000 {
			return engine.Timestamp(n), nil
		}
		return engine.Timestamp(n * int64(time.Second)), nil
	}
	ts, err := engine.ParseTimestamp(v)
	if err != nil {
		return 0, err
	}
	return ts, nil
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
