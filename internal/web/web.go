package web

import (
	"embed"
	"encoding/json"
	"html/template"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

//go:embed static/* static/assets/* static/dashboard_assets/* static/common_assets/*
var staticFS embed.FS

//go:embed default_dashboard.json
var defaultDashboardConfig []byte

type Config struct {
	Enabled        bool
	BasePath       string
	AdhocPath      string
	Title          string
	RefreshSeconds int
	DashboardFile  string
}

type indexTemplateData struct {
	Title         string
	AssetBase     string
	ConfigJSON    template.JS
	DashboardPath string
	AdhocPath     string
}

func DefaultConfig() Config {
	return Config{
		Enabled:        true,
		BasePath:       "/dashboard",
		AdhocPath:      "/adhoc",
		Title:          "NanoTDB Dashboard",
		RefreshSeconds: 10,
		DashboardFile:  "dashboard.json",
	}
}

func DefaultDashboardConfig() []byte {
	return append([]byte(nil), defaultDashboardConfig...)
}

func Register(mux *http.ServeMux, cfg Config, dataDir string) {
	cfg = normalizeConfig(cfg)
	if !cfg.Enabled {
		return
	}

	dashboardIndexRaw, err := fs.ReadFile(staticFS, "static/dashboard.html")
	if err != nil {
		panic("internal/web: missing static/dashboard.html")
	}
	dashboardTmpl, err := template.New("dashboard-index").Parse(string(dashboardIndexRaw))
	if err != nil {
		panic("internal/web: invalid dashboard template")
	}

	adhocIndexRaw, err := fs.ReadFile(staticFS, "static/index.html")
	if err != nil {
		panic("internal/web: missing static/index.html")
	}
	adhocTmpl, err := template.New("adhoc-index").Parse(string(adhocIndexRaw))
	if err != nil {
		panic("internal/web: invalid adhoc template")
	}

	dashboardAssets, err := fs.Sub(staticFS, "static/dashboard_assets")
	if err != nil {
		panic("internal/web: dashboard assets unavailable")
	}
	mux.Handle(cfg.BasePath+"/assets/", http.StripPrefix(cfg.BasePath+"/assets/", http.FileServer(http.FS(dashboardAssets))))

	adhocAssets, err := fs.Sub(staticFS, "static/assets")
	if err != nil {
		panic("internal/web: adhoc assets unavailable")
	}
	mux.Handle(cfg.AdhocPath+"/assets/", http.StripPrefix(cfg.AdhocPath+"/assets/", http.FileServer(http.FS(adhocAssets))))

	commonAssets, err := fs.Sub(staticFS, "static/common_assets")
	if err != nil {
		panic("internal/web: common assets unavailable")
	}
	mux.Handle("/assets/", http.StripPrefix("/assets/", http.FileServer(http.FS(commonAssets))))

	dashboardJSONPath := resolveDashboardPath(dataDir, cfg.DashboardFile)
	mux.HandleFunc("/api/dashboard-config", func(w http.ResponseWriter, _ *http.Request) {
		payload, err := loadDashboardConfig(dashboardJSONPath, cfg)
		if err != nil {
			http.Error(w, "failed to load dashboard config", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(payload)
	})

	serveDashboard := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = dashboardTmpl.Execute(w, indexTemplateData{
			Title:         cfg.Title,
			AssetBase:     cfg.BasePath + "/assets",
			DashboardPath: cfg.BasePath,
			AdhocPath:     cfg.AdhocPath,
		})
	}

	serveAdhoc := func(w http.ResponseWriter, _ *http.Request) {
		payload, _ := json.Marshal(map[string]interface{}{
			"basePath":       cfg.BasePath,
			"refreshSeconds": cfg.RefreshSeconds,
		})
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = adhocTmpl.Execute(w, indexTemplateData{
			Title:         cfg.Title,
			AssetBase:     cfg.AdhocPath + "/assets",
			ConfigJSON:    template.JS(payload),
			DashboardPath: cfg.BasePath,
			AdhocPath:     cfg.AdhocPath,
		})
	}

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		serveDashboard(w, r)
	})

	mux.HandleFunc(cfg.BasePath, serveDashboard)
	mux.HandleFunc(cfg.BasePath+"/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == cfg.BasePath+"/" {
			serveDashboard(w, r)
			return
		}
		http.NotFound(w, r)
	})

	mux.HandleFunc(cfg.AdhocPath, serveAdhoc)
	mux.HandleFunc(cfg.AdhocPath+"/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == cfg.AdhocPath+"/" {
			serveAdhoc(w, r)
			return
		}
		http.NotFound(w, r)
	})
}

func normalizeConfig(cfg Config) Config {
	if strings.TrimSpace(cfg.BasePath) == "" {
		cfg.BasePath = "/dashboard"
	}
	if !strings.HasPrefix(cfg.BasePath, "/") {
		cfg.BasePath = "/" + cfg.BasePath
	}
	cfg.BasePath = path.Clean(cfg.BasePath)
	if cfg.BasePath == "." || cfg.BasePath == "/" {
		cfg.BasePath = "/dashboard"
	}
	if strings.TrimSpace(cfg.Title) == "" {
		cfg.Title = "NanoTDB Dashboard"
	}
	if strings.TrimSpace(cfg.AdhocPath) == "" {
		cfg.AdhocPath = "/adhoc"
	}
	if !strings.HasPrefix(cfg.AdhocPath, "/") {
		cfg.AdhocPath = "/" + cfg.AdhocPath
	}
	cfg.AdhocPath = path.Clean(cfg.AdhocPath)
	if cfg.AdhocPath == "." || cfg.AdhocPath == "/" || cfg.AdhocPath == cfg.BasePath {
		cfg.AdhocPath = "/adhoc"
	}
	if strings.TrimSpace(cfg.DashboardFile) == "" {
		cfg.DashboardFile = "dashboard.json"
	}
	if cfg.RefreshSeconds <= 0 {
		cfg.RefreshSeconds = 10
	}
	return cfg
}

func resolveDashboardPath(dataDir, dashboardFile string) string {
	if filepath.IsAbs(dashboardFile) {
		return dashboardFile
	}
	return filepath.Join(dataDir, dashboardFile)
}

func loadDashboardConfig(path string, cfg Config) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err == nil {
		return raw, nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	defaultCfg := DefaultDashboardConfig()
	if strings.TrimSpace(cfg.Title) == "" || cfg.Title == "NanoTDB Dashboard" {
		return defaultCfg, nil
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(defaultCfg, &payload); err != nil {
		return nil, err
	}
	payload["title"] = cfg.Title
	return json.Marshal(payload)
}
