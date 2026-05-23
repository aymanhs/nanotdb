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

//go:embed static
var staticFS embed.FS

//go:embed default_dashboard.json
var defaultDashboardConfig []byte

type Config struct {
	Enabled        bool
	BasePath       string
	ExplorePath    string
	Title          string
	RefreshSeconds int
	DashboardFile  string
	WebRoot        string
	APIBaseURL     string
	EnginePath     string
}

type indexTemplateData struct {
	Title         string
	AssetBase     string
	ConfigJSON    template.JS
	DashboardPath string
	EditorPath    string
	ExplorePath   string
	EnginePath    string
}

func DefaultConfig() Config {
	return Config{
		Enabled:        true,
		BasePath:       "/dashboard",
		ExplorePath:    "/explore",
		Title:          "NanoTDB Dashboard",
		RefreshSeconds: 10,
		DashboardFile:  "dashboard.json",
		EnginePath:     "/engine",
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
	mux.Handle(cfg.BasePath+"/assets/", http.StripPrefix(cfg.BasePath+"/assets/", assetSource.dashboardAssets))
	mux.Handle(cfg.ExplorePath+"/assets/", http.StripPrefix(cfg.ExplorePath+"/assets/", assetSource.exploreAssets))
	mux.Handle(cfg.EnginePath+"/assets/", http.StripPrefix(cfg.EnginePath+"/assets/", assetSource.engineAssets))
	mux.Handle("/assets/", http.StripPrefix("/assets/", assetSource.commonAssets))

	dashboardJSONPath := resolveDashboardPath(dataDir, cfg.DashboardFile)
	mux.HandleFunc("/api/dashboard-config", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			payload, err := loadDashboardConfig(dashboardJSONPath, cfg)
			if err != nil {
				http.Error(w, "failed to load dashboard config", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(payload)
		case http.MethodPut:
			dashboardCfg, errs := readDashboardConfigRequest(r)
			if len(errs) > 0 {
				writeDashboardMutationResponse(w, http.StatusBadRequest, dashboardMutationResponse{OK: false, Errors: errs})
				return
			}
			backupPath, savedPayload, err := saveDashboardConfig(dashboardJSONPath, dashboardCfg)
			if err != nil {
				http.Error(w, "failed to save dashboard config", http.StatusInternalServerError)
				return
			}
			writeDashboardMutationResponse(w, http.StatusOK, dashboardMutationResponse{
				OK:         true,
				BackupPath: backupPath,
				Config:     &dashboardCfg,
				Payload:    savedPayload,
			})
		default:
			w.Header().Set("Allow", "GET, PUT")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc("/api/dashboard-config/validate", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		dashboardCfg, errs := readDashboardConfigRequest(r)
		if len(errs) > 0 {
			writeDashboardMutationResponse(w, http.StatusBadRequest, dashboardMutationResponse{OK: false, Errors: errs})
			return
		}
		writeDashboardMutationResponse(w, http.StatusOK, dashboardMutationResponse{OK: true, Config: &dashboardCfg})
	})

	editorPath := cfg.BasePath + "/edit"

	serveDashboard := func(w http.ResponseWriter, _ *http.Request) {
		payload, _ := json.Marshal(map[string]interface{}{
			"basePath":       cfg.BasePath,
			"editorPath":     editorPath,
			"refreshSeconds": cfg.RefreshSeconds,
			"apiBaseURL":     cfg.APIBaseURL,
		})
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := assetSource.executeTemplate(w, assetSource.dashboardTemplatePath, "dashboard-index", indexTemplateData{
			Title:         cfg.Title,
			AssetBase:     cfg.BasePath + "/assets",
			ConfigJSON:    template.JS(payload),
			DashboardPath: cfg.BasePath,
			EditorPath:    editorPath,
			ExplorePath:   cfg.ExplorePath,
			EnginePath:    cfg.EnginePath,
		}); err != nil {
			http.Error(w, "failed to render dashboard", http.StatusInternalServerError)
		}
	}

	serveExplore := func(w http.ResponseWriter, _ *http.Request) {
		payload, _ := json.Marshal(map[string]interface{}{
			"basePath":       cfg.BasePath,
			"editorPath":     editorPath,
			"refreshSeconds": cfg.RefreshSeconds,
			"apiBaseURL":     cfg.APIBaseURL,
		})
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := assetSource.executeTemplate(w, assetSource.exploreTemplatePath, "explore-index", indexTemplateData{
			Title:         cfg.Title,
			AssetBase:     cfg.ExplorePath + "/assets",
			ConfigJSON:    template.JS(payload),
			DashboardPath: cfg.BasePath,
			EditorPath:    editorPath,
			ExplorePath:   cfg.ExplorePath,
			EnginePath:    cfg.EnginePath,
		}); err != nil {
			http.Error(w, "failed to render explorer", http.StatusInternalServerError)
		}
	}

	serveEngine := func(w http.ResponseWriter, _ *http.Request) {
		payload, _ := json.Marshal(map[string]interface{}{
			"basePath":       cfg.BasePath,
			"editorPath":     editorPath,
			"refreshSeconds": cfg.RefreshSeconds,
			"apiBaseURL":     cfg.APIBaseURL,
			"enginePath":     cfg.EnginePath,
		})
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := assetSource.executeTemplate(w, assetSource.engineTemplatePath, "engine-index", indexTemplateData{
			Title:         cfg.Title,
			AssetBase:     cfg.EnginePath + "/assets",
			ConfigJSON:    template.JS(payload),
			DashboardPath: cfg.BasePath,
			EditorPath:    editorPath,
			ExplorePath:   cfg.ExplorePath,
			EnginePath:    cfg.EnginePath,
		}); err != nil {
			http.Error(w, "failed to render engine explorer", http.StatusInternalServerError)
		}
	}

	serveEditor := func(w http.ResponseWriter, _ *http.Request) {
		payload, _ := json.Marshal(map[string]interface{}{
			"basePath":       cfg.BasePath,
			"editorPath":     editorPath,
			"refreshSeconds": cfg.RefreshSeconds,
			"apiBaseURL":     cfg.APIBaseURL,
		})
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := assetSource.executeTemplate(w, assetSource.editorTemplatePath, "dashboard-editor", indexTemplateData{
			Title:         cfg.Title,
			AssetBase:     cfg.BasePath + "/assets",
			ConfigJSON:    template.JS(payload),
			DashboardPath: cfg.BasePath,
			EditorPath:    editorPath,
			ExplorePath:   cfg.ExplorePath,
			EnginePath:    cfg.EnginePath,
		}); err != nil {
			http.Error(w, "failed to render dashboard editor", http.StatusInternalServerError)
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
	mux.HandleFunc(editorPath, serveEditor)
	mux.HandleFunc(editorPath+"/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == editorPath+"/" {
			serveEditor(w, r)
			return
		}
		http.NotFound(w, r)
	})

	mux.HandleFunc(cfg.ExplorePath, serveExplore)
	mux.HandleFunc(cfg.ExplorePath+"/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == cfg.ExplorePath+"/" {
			serveExplore(w, r)
			return
		}
		http.NotFound(w, r)
	})

	mux.HandleFunc(cfg.EnginePath, serveEngine)
	mux.HandleFunc(cfg.EnginePath+"/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == cfg.EnginePath+"/" {
			serveEngine(w, r)
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
	if strings.TrimSpace(cfg.ExplorePath) == "" {
		cfg.ExplorePath = "/explore"
	}
	if !strings.HasPrefix(cfg.ExplorePath, "/") {
		cfg.ExplorePath = "/" + cfg.ExplorePath
	}
	cfg.ExplorePath = path.Clean(cfg.ExplorePath)
	if cfg.ExplorePath == "." || cfg.ExplorePath == "/" || cfg.ExplorePath == cfg.BasePath {
		cfg.ExplorePath = "/explore"
	}
	if strings.TrimSpace(cfg.EnginePath) == "" {
		cfg.EnginePath = "/engine"
	}
	if !strings.HasPrefix(cfg.EnginePath, "/") {
		cfg.EnginePath = "/" + cfg.EnginePath
	}
	cfg.EnginePath = path.Clean(cfg.EnginePath)
	if cfg.EnginePath == "." || cfg.EnginePath == "/" || cfg.EnginePath == cfg.BasePath || cfg.EnginePath == cfg.ExplorePath {
		cfg.EnginePath = "/engine"
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
	editorTemplatePath    string
	exploreTemplatePath   string
	engineTemplatePath    string
	dashboardAssets       http.Handler
	exploreAssets         http.Handler
	engineAssets          http.Handler
	commonAssets          http.Handler
}

func newAssetSource(cfg Config, dataDir string) assetSource {
	webRoot := resolveWebRoot(dataDir, cfg.WebRoot)
	if webRoot != "" {
		return assetSource{
			dashboardTemplatePath: filepath.Join(webRoot, "dashboard.html"),
			editorTemplatePath:    filepath.Join(webRoot, "editor.html"),
			exploreTemplatePath:   filepath.Join(webRoot, "index.html"),
			engineTemplatePath:    filepath.Join(webRoot, "engine.html"),
			dashboardAssets:       http.FileServer(http.Dir(filepath.Join(webRoot, "dashboard_assets"))),
			exploreAssets:         http.FileServer(http.Dir(filepath.Join(webRoot, "assets"))),
			engineAssets:          http.FileServer(http.Dir(filepath.Join(webRoot, "engine_assets"))),
			commonAssets:          http.FileServer(http.Dir(filepath.Join(webRoot, "common_assets"))),
		}
	}

	dashboardAssets, err := fs.Sub(staticFS, "static/dashboard_assets")
	if err != nil {
		panic("internal/web: dashboard assets unavailable")
	}
	exploreAssets, err := fs.Sub(staticFS, "static/assets")
	if err != nil {
		panic("internal/web: explore assets unavailable")
	}
	engineAssets, err := fs.Sub(staticFS, "static/engine_assets")
	if err != nil {
		panic("internal/web: engine assets unavailable")
	}
	commonAssets, err := fs.Sub(staticFS, "static/common_assets")
	if err != nil {
		panic("internal/web: common assets unavailable")
	}
	return assetSource{
		dashboardTemplatePath: "static/dashboard.html",
		editorTemplatePath:    "static/editor.html",
		exploreTemplatePath:   "static/index.html",
		engineTemplatePath:    "static/engine.html",
		dashboardAssets:       http.FileServer(http.FS(dashboardAssets)),
		exploreAssets:         http.FileServer(http.FS(exploreAssets)),
		engineAssets:          http.FileServer(http.FS(engineAssets)),
		commonAssets:          http.FileServer(http.FS(commonAssets)),
	}
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

func writeDashboardMutationResponse(w http.ResponseWriter, status int, resp dashboardMutationResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(resp)
}
