package main

import (
	"fmt"
	"os"

	"github.com/BurntSushi/toml"
)

// Config is the top-level drip configuration loaded from drip.toml.
type Config struct {
	Drip           DripConfig               `toml:"drip"`
	Collectors     CollectorsConfig         `toml:"collectors"`
	InternalEvents DripInternalEventsConfig `toml:"internal_events"`
	Admin          DripAdminConfig          `toml:"admin"`
}

// DripInternalEventsConfig holds drip's per-install internal-events
// settings. See docs/INTERNAL_EVENTS.md for the spec.
type DripInternalEventsConfig struct {
	Enabled    bool              `toml:"enabled"`
	TargetDB   string            `toml:"target_db"`
	QueueDepth int               `toml:"queue_depth"`
	Groups     map[string]string `toml:"groups"`
}

// DripAdminConfig holds the admin HTTP listener config. Empty Listen
// disables the listener.
type DripAdminConfig struct {
	Listen string `toml:"listen"`
}

// DripConfig holds global runtime settings.
type DripConfig struct {
	ServerURL            string `toml:"server_url"`
	Database             string `toml:"database"`
	CollectionIntervalMS int    `toml:"collection_interval_ms"`
	TimeoutMS            int    `toml:"timeout_ms"`
}

// CollectorsConfig holds per-collector enable flags and options.
type CollectorsConfig struct {
	CPU          CPUCollectorConfig          `toml:"cpu"`
	Memory       MemoryCollectorConfig       `toml:"memory"`
	Process      ProcessCollectorConfig      `toml:"process"`
	Disk         DiskCollectorConfig         `toml:"disk"`
	IO           IOCollectorConfig           `toml:"io"`
	Network      NetworkCollectorConfig      `toml:"network"`
	LoadAvg      LoadAvgCollectorConfig      `toml:"loadavg"`
	OneWire      OneWireCollectorConfig      `toml:"onewire"`
	SDWriteProbe SDWriteProbeCollectorConfig `toml:"sd_write_probe"`
}

type CPUCollectorConfig struct {
	Enabled    bool   `toml:"enabled"`
	TempPath   string `toml:"temp_path"`
	TempMetric string `toml:"temp_metric"`
}

type MemoryCollectorConfig struct {
	Enabled bool `toml:"enabled"`
}

type ProcessCollectorConfig struct {
	Enabled  bool     `toml:"enabled"`
	ExeNames []string `toml:"exe_names"`
}

type DiskCollectorConfig struct {
	Enabled bool `toml:"enabled"`
}

type IOCollectorConfig struct {
	Enabled bool `toml:"enabled"`
}

type NetworkCollectorConfig struct {
	Enabled bool `toml:"enabled"`
	// Interfaces to skip (e.g. "lo"). Default: ["lo"].
	Skip []string `toml:"skip"`
}

type LoadAvgCollectorConfig struct {
	Enabled bool `toml:"enabled"`
}

type OneWireCollectorConfig struct {
	Enabled      bool              `toml:"enabled"`
	AutoDiscover bool              `toml:"auto_discover"`
	BasePath     string            `toml:"base_path"`
	MaxValidMdeg int32             `toml:"max_valid_mdeg"`
	Devices      map[string]string `toml:"devices"` // device id -> friendly name
}

type SDWriteProbeCollectorConfig struct {
	Enabled         bool   `toml:"enabled"`
	Directory       string `toml:"directory"`
	Bytes           int    `toml:"bytes"`
	EveryNCycles    int    `toml:"every_n_cycles"`
	Metric          string `toml:"metric"`
	EventWhenOverMS int    `toml:"event_when_over_ms"`
	EventName       string `toml:"event_name"`
}

func defaultConfig() Config {
	return Config{
		Drip: DripConfig{
			ServerURL:            "http://localhost:8428",
			Database:             "metrics",
			CollectionIntervalMS: 10000,
			TimeoutMS:            1500,
		},
		InternalEvents: DripInternalEventsConfig{
			Enabled:    true,
			TargetDB:   "internal",
			QueueDepth: 1024,
			Groups:     map[string]string{},
		},
		Admin: DripAdminConfig{Listen: ""},
		Collectors: CollectorsConfig{
			CPU: CPUCollectorConfig{
				Enabled:    true,
				TempPath:   "/sys/class/thermal/thermal_zone0/temp",
				TempMetric: "cpu.temp_mdeg",
			},
			Memory:  MemoryCollectorConfig{Enabled: true},
			Process: ProcessCollectorConfig{Enabled: true, ExeNames: []string{"drip", "nanotdb"}},
			Network: NetworkCollectorConfig{Enabled: true, Skip: []string{"lo"}},
			LoadAvg: LoadAvgCollectorConfig{Enabled: true},
			OneWire: OneWireCollectorConfig{
				Enabled:      false,
				AutoDiscover: true,
				BasePath:     "/sys/bus/w1/devices",
				MaxValidMdeg: 85000,
			},
			SDWriteProbe: SDWriteProbeCollectorConfig{
				Enabled:         false,
				Directory:       "/tmp",
				Bytes:           1024 * 256,
				EveryNCycles:    6,
				Metric:          "disk.sd_write_probe_ms",
				EventWhenOverMS: 0,
				// Per docs/INTERNAL_EVENTS.md: drip threshold events live
				// under the drip.threshold group with names of the form
				// drip.threshold.<metric>.<state>. Pre-1.x installs that
				// still emit the old "disk.sd_write_probe.slow" name will
				// keep working — only the default has changed.
				EventName: "drip.threshold.disk.sd_write_probe.slow",
			},
		},
	}
}

func loadConfig(path string) (Config, error) {
	cfg := defaultConfig()
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config %s: %w", path, err)
	}
	if _, err := toml.Decode(string(raw), &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := validateConfig(cfg); err != nil {
		return Config{}, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return cfg, nil
}

func validateConfig(cfg Config) error {
	if cfg.Drip.ServerURL == "" {
		return fmt.Errorf("drip.server_url is required")
	}
	if cfg.Drip.Database == "" {
		return fmt.Errorf("drip.database is required")
	}
	if cfg.Drip.CollectionIntervalMS <= 0 {
		return fmt.Errorf("drip.collection_interval_ms must be > 0")
	}
	if cfg.Drip.TimeoutMS <= 0 {
		return fmt.Errorf("drip.timeout_ms must be > 0")
	}
	if cfg.Collectors.SDWriteProbe.Enabled {
		if cfg.Collectors.SDWriteProbe.Bytes <= 0 {
			return fmt.Errorf("collectors.sd_write_probe.bytes must be > 0")
		}
		if cfg.Collectors.SDWriteProbe.EveryNCycles <= 0 {
			return fmt.Errorf("collectors.sd_write_probe.every_n_cycles must be > 0")
		}
		if cfg.Collectors.SDWriteProbe.EventWhenOverMS < 0 {
			return fmt.Errorf("collectors.sd_write_probe.event_when_over_ms must be >= 0")
		}
	}
	if cfg.Collectors.Process.Enabled && len(cfg.Collectors.Process.ExeNames) == 0 {
		return fmt.Errorf("collectors.process.exe_names must contain at least one executable name")
	}
	return nil
}
