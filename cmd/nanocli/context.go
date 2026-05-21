package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type dbContext struct {
	RootDir         string
	Database        string
	EngineConfig    string
	DatabaseDir     string
	CatalogPath     string
	ManifestPath    string
	DataFilePaths   []string
	MetricFilePaths []string
	WALFilePaths    []string
}

func resolveRootDir(rootDir string) (string, string, error) {
	if rootDir == "" {
		return "", "", fmt.Errorf("--root is required")
	}

	rootAbs, err := filepath.Abs(rootDir)
	if err != nil {
		return "", "", err
	}
	engineCfg := filepath.Join(rootAbs, "engine.toml")
	st, err := os.Stat(engineCfg)
	if err != nil {
		return "", "", fmt.Errorf("engine config not found at %s: %w", engineCfg, err)
	}
	if st.IsDir() {
		return "", "", fmt.Errorf("engine config path is a directory: %s", engineCfg)
	}
	return rootAbs, engineCfg, nil
}

func resolveDBContext(rootDir string, database string) (dbContext, error) {
	database, err := normalizeDatabaseName(database)
	if err != nil {
		return dbContext{}, err
	}

	if database == "" {
		return dbContext{}, fmt.Errorf("--db is required")
	}

	rootAbs, engineCfg, err := resolveRootDir(rootDir)
	if err != nil {
		return dbContext{}, err
	}

	dbDir := filepath.Join(rootAbs, database)
	dataFiles, err := filepath.Glob(filepath.Join(dbDir, "data-*.dat"))
	if err != nil {
		return dbContext{}, err
	}
	metricFiles, err := filepath.Glob(filepath.Join(dbDir, "metric-*.dat"))
	if err != nil {
		return dbContext{}, err
	}
	walFiles, err := filepath.Glob(filepath.Join(dbDir, "*.wal"))
	if err != nil {
		return dbContext{}, err
	}
	sort.Strings(dataFiles)
	sort.Strings(metricFiles)
	sort.Strings(walFiles)

	return dbContext{
		RootDir:         rootAbs,
		Database:        database,
		EngineConfig:    engineCfg,
		DatabaseDir:     dbDir,
		CatalogPath:     filepath.Join(dbDir, "catalog.json"),
		ManifestPath:    filepath.Join(dbDir, "manifest.toml"),
		DataFilePaths:   dataFiles,
		MetricFilePaths: metricFiles,
		WALFilePaths:    walFiles,
	}, nil
}

func normalizeDatabaseName(database string) (string, error) {
	database = strings.TrimSpace(database)
	database = strings.Trim(database, "/")
	if database == "" {
		return "", fmt.Errorf("--db is required")
	}
	return database, nil
}
