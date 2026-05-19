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
	WebRoot        string
	APIBaseURL     string
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

func ExportAssets(dstDir string) error {
	dstDir = strings.TrimSpace(dstDir)
	if dstDir == "" {
		return os.ErrInvalid
	}
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return err
	}
	return fs.WalkDir(staticFS, "static", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(name, "static/")
		if rel == "static" || rel == "" {
			return nil
		}
		target := filepath.Join(dstDir, filepath.FromSlash(rel))
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		raw, err := fs.ReadFile(staticFS, name)
		if err != nil {
			return err
		}
		return os.WriteFile(target, raw, 0o644)
	})
}

func DefaultDashboardConfig() []byte {
	return append([]byte(nil), defaultDashboardConfig...)
}

func Register(mux *http.ServeMux, cfg Config, dataDir string) {
	cfg = normalizeConfig(cfg)
	if !cfg.Enabled {
		return
	}
	assetSource := newAssetSource(cfg, dataDir)
	mux.Handle(cfg.BasePath+"/assets/", http.StripPrefix(cfg.BasePath+"/assets/", assetSource.dashboardAssetsHandler()))
	mux.Handle(cfg.AdhocPath+"/assets/", http.StripPrefix(cfg.AdhocPath+"/assets/", assetSource.adhocAssetsHandler()))
	mux.Handle("/assets/", http.StripPrefix("/assets/", assetSource.commonAssetsHandler()))

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
		payload, _ := json.Marshal(map[string]interface{}{
			"basePath":       cfg.BasePath,
			"refreshSeconds": cfg.RefreshSeconds,
			"apiBaseURL":     cfg.APIBaseURL,
		})
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := assetSource.executeDashboardTemplate(w, indexTemplateData{
			Title:         cfg.Title,
			AssetBase:     cfg.BasePath + "/assets",
			ConfigJSON:    template.JS(payload),
			DashboardPath: cfg.BasePath,
			AdhocPath:     cfg.AdhocPath,
		}); err != nil {
			http.Error(w, "failed to render dashboard", http.StatusInternalServerError)
		}
	}

	serveAdhoc := func(w http.ResponseWriter, _ *http.Request) {
		payload, _ := json.Marshal(map[string]interface{}{
			"basePath":       cfg.BasePath,
			"refreshSeconds": cfg.RefreshSeconds,
			"apiBaseURL":     cfg.APIBaseURL,
		})
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := assetSource.executeAdhocTemplate(w, indexTemplateData{
			Title:         cfg.Title,
			AssetBase:     cfg.AdhocPath + "/assets",
			ConfigJSON:    template.JS(payload),
			DashboardPath: cfg.BasePath,
			AdhocPath:     cfg.AdhocPath,
		}); err != nil {
			http.Error(w, "failed to render adhoc explorer", http.StatusInternalServerError)
		}
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
	cfg.WebRoot = strings.TrimSpace(cfg.WebRoot)
	cfg.APIBaseURL = strings.TrimRight(strings.TrimSpace(cfg.APIBaseURL), "/")
	if cfg.RefreshSeconds <= 0 {
		cfg.RefreshSeconds = 10
	}
	return cfg
}

type assetSource struct {
	dashboardTemplatePath string
	adhocTemplatePath     string
	dashboardAssets       http.Handler
	adhocAssets           http.Handler
	commonAssets          http.Handler
}

func newAssetSource(cfg Config, dataDir string) assetSource {
	webRoot := resolveWebRoot(dataDir, cfg.WebRoot)
	if webRoot != "" {
		return assetSource{
			dashboardTemplatePath: filepath.Join(webRoot, "dashboard.html"),
			adhocTemplatePath:     filepath.Join(webRoot, "index.html"),
			dashboardAssets:       http.FileServer(http.Dir(filepath.Join(webRoot, "dashboard_assets"))),
			adhocAssets:           http.FileServer(http.Dir(filepath.Join(webRoot, "assets"))),
			commonAssets:          http.FileServer(http.Dir(filepath.Join(webRoot, "common_assets"))),
		}
	}

	dashboardAssets, err := fs.Sub(staticFS, "static/dashboard_assets")
	if err != nil {
		panic("internal/web: dashboard assets unavailable")
	}
	adhocAssets, err := fs.Sub(staticFS, "static/assets")
	if err != nil {
		panic("internal/web: adhoc assets unavailable")
	}
	commonAssets, err := fs.Sub(staticFS, "static/common_assets")
	if err != nil {
		panic("internal/web: common assets unavailable")
	}
	return assetSource{
		dashboardTemplatePath: "static/dashboard.html",
		adhocTemplatePath:     "static/index.html",
		dashboardAssets:       http.FileServer(http.FS(dashboardAssets)),
		adhocAssets:           http.FileServer(http.FS(adhocAssets)),
		commonAssets:          http.FileServer(http.FS(commonAssets)),
	}
}

func (s assetSource) dashboardAssetsHandler() http.Handler { return s.dashboardAssets }
func (s assetSource) adhocAssetsHandler() http.Handler     { return s.adhocAssets }
func (s assetSource) commonAssetsHandler() http.Handler    { return s.commonAssets }

func (s assetSource) executeDashboardTemplate(w http.ResponseWriter, data indexTemplateData) error {
	return s.executeTemplate(w, s.dashboardTemplatePath, "dashboard-index", data)
}

func (s assetSource) executeAdhocTemplate(w http.ResponseWriter, data indexTemplateData) error {
	return s.executeTemplate(w, s.adhocTemplatePath, "adhoc-index", data)
}

func (s assetSource) executeTemplate(w http.ResponseWriter, pathName, tmplName string, data indexTemplateData) error {
	raw, err := s.readFile(pathName)
	if err != nil {
		return err
	}
	tmpl, err := template.New(tmplName).Parse(string(raw))
	if err != nil {
		return err
	}
	return tmpl.Execute(w, data)
}

func (s assetSource) readFile(pathName string) ([]byte, error) {
	if strings.HasPrefix(pathName, "static/") {
		return fs.ReadFile(staticFS, pathName)
	}
	return os.ReadFile(pathName)
}

func resolveWebRoot(dataDir, webRoot string) string {
	webRoot = strings.TrimSpace(webRoot)
	if webRoot == "" {
		return ""
	}
	if filepath.IsAbs(webRoot) {
		return webRoot
	}
	return filepath.Join(dataDir, webRoot)
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
