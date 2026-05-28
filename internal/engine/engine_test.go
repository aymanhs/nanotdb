package engine

import (
	"archive/tar"
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestEngineAddLineAndQueryLast(t *testing.T) {
	e, err := OpenEngine(t.TempDir(), 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer e.Close()

	if err := e.AddLine("prod/system.cpu.user 42 1000000001"); err != nil {
		t.Fatalf("AddLine numeric failed: %v", err)
	}
	if err := e.AddLine("prod/system.cpu.user 45 1000000002"); err != nil {
		t.Fatalf("AddLine numeric failed: %v", err)
	}

	last, found, err := e.QueryLast("prod", "system.cpu.user")
	if err != nil {
		t.Fatalf("QueryLast failed: %v", err)
	}
	if !found {
		t.Fatalf("expected last sample")
	}
	if last.ValueType != Float32Sample {
		t.Fatalf("type mismatch: got=%d want=%d", last.ValueType, Float32Sample)
	}
	if last.Float32 != 45 {
		t.Fatalf("value mismatch: got=%v want=45", last.Float32)
	}
	if last.TS != Timestamp(1000000002) {
		t.Fatalf("timestamp mismatch: got=%d want=%d", last.TS, 1000000002)
	}
}

func TestEngineAddSampleAndQueryLast(t *testing.T) {
	e, err := OpenEngine(t.TempDir(), 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer e.Close()

	if err := e.AddSample("prod", "system.cpu.user", Timestamp(1000000001), float32(42)); err != nil {
		t.Fatalf("AddSample failed: %v", err)
	}
	if err := e.AddSample("prod", "system.cpu.user", Timestamp(1000000002), float32(45)); err != nil {
		t.Fatalf("AddSample failed: %v", err)
	}

	last, found, err := e.QueryLast("prod", "system.cpu.user")
	if err != nil {
		t.Fatalf("QueryLast failed: %v", err)
	}
	if !found {
		t.Fatalf("expected last sample")
	}
	if last.ValueType != Float32Sample {
		t.Fatalf("type mismatch: got=%d want=%d", last.ValueType, Float32Sample)
	}
	if last.Float32 != 45 {
		t.Fatalf("value mismatch: got=%v want=45", last.Float32)
	}
	if last.TS != Timestamp(1000000002) {
		t.Fatalf("timestamp mismatch: got=%d want=%d", last.TS, 1000000002)
	}
}

func TestEngineAddLineExplicitIntSuffix(t *testing.T) {
	e, err := OpenEngine(t.TempDir(), 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer e.Close()

	if err := e.AddLine("prod/system.cpu.idle 42i 1000000001"); err != nil {
		t.Fatalf("AddLine int-suffix failed: %v", err)
	}

	last, found, err := e.QueryLast("prod", "system.cpu.idle")
	if err != nil {
		t.Fatalf("QueryLast failed: %v", err)
	}
	if !found {
		t.Fatalf("expected last sample")
	}
	if last.ValueType != Int32Sample {
		t.Fatalf("type mismatch: got=%d want=%d", last.ValueType, Int32Sample)
	}
	if last.Int32 != 42 {
		t.Fatalf("value mismatch: got=%d want=42", last.Int32)
	}
}

func TestOpenEngineCreatesConfigFiles(t *testing.T) {
	root := t.TempDir()
	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer e.Close()

	path := filepath.Join(root, "engine.toml")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected engine.toml to be created: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile engine.toml failed: %v", err)
	}
	text := string(raw)
	if !strings.Contains(text, "[wal]") {
		t.Fatalf("expected engine.toml to contain [wal] section")
	}
	if !strings.Contains(text, "[stats]") {
		t.Fatalf("expected engine.toml to contain [stats] section")
	}
	if !strings.Contains(text, "[metrics]") {
		t.Fatalf("expected engine.toml to contain [metrics] section")
	}
	if !strings.Contains(text, "[[logging.logger]]") {
		t.Fatalf("expected engine.toml to contain [[logging.logger]] section")
	}
	if !strings.Contains(text, "[defaults]") {
		t.Fatalf("expected engine.toml to contain [defaults] section")
	}
	if !strings.Contains(text, "[manifest_defaults.retention]") {
		t.Fatalf("expected engine.toml to contain [manifest_defaults.retention] section")
	}
}

func TestOpenEngineReadsEngineConfig(t *testing.T) {
	root := t.TempDir()
	cfg := []byte("[wal]\nmax_segment_size = 777777\nfsync_policy = \"always\"\n\n[stats]\nenabled = false\ninterval = \"5s\"\n\n[metrics]\nenabled = true\ncompression = \"zstd_fastest\"\nraw_ingest_action = \"rename\"\ntime_cache_slots = 64\n\n[defaults]\ndatabases = [\"prod\"]\n")
	if err := os.WriteFile(filepath.Join(root, "engine.toml"), cfg, 0644); err != nil {
		t.Fatalf("write engine.toml failed: %v", err)
	}

	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer e.Close()

	if e.WALMaxSegSize != 777777 {
		t.Fatalf("wal max seg size mismatch: got=%d want=%d", e.WALMaxSegSize, 777777)
	}
	if e.WALFsyncPolicy != WALFsyncPolicyAlways {
		t.Fatalf("wal fsync policy mismatch: got=%q want=%q", e.WALFsyncPolicy, WALFsyncPolicyAlways)
	}
	if e.StatsEnabled != false {
		t.Fatalf("stats enabled mismatch: got=%t want=%t", e.StatsEnabled, false)
	}
	if e.StatsInterval != 5*time.Second {
		t.Fatalf("stats interval mismatch: got=%s want=5s", e.StatsInterval)
	}
	if !e.PreferMetricFiles {
		t.Fatalf("prefer metric files mismatch: got=%t want=%t", e.PreferMetricFiles, true)
	}
	if !e.AutoCreateMetricFiles {
		t.Fatalf("auto-create metric files mismatch: got=%t want=%t", e.AutoCreateMetricFiles, true)
	}
	if e.MetricFileCompression != CompressionCodecZstdFastestName {
		t.Fatalf("metric file compression mismatch: got=%q want=%q", e.MetricFileCompression, CompressionCodecZstdFastestName)
	}
	if e.MetricRawIngestAction != MetricRawIngestActionRename {
		t.Fatalf("metric raw ingest action mismatch: got=%q want=%q", e.MetricRawIngestAction, MetricRawIngestActionRename)
	}
	if e.MetricTimeCacheSlots != 64 {
		t.Fatalf("metric time cache slots mismatch: got=%d want=%d", e.MetricTimeCacheSlots, 64)
	}
	if len(e.Logging.Loggers) != 1 {
		t.Fatalf("logger count mismatch: got=%d want=1", len(e.Logging.Loggers))
	}
	if e.Logging.Loggers[0].Output != "console" || e.Logging.Loggers[0].Level != LogLevelInfo {
		t.Fatalf("default logger mismatch: got=%+v", e.Logging.Loggers[0])
	}
	if _, _, err := e.getOrCreateDB("prod"); err != nil {
		t.Fatalf("expected default database to be available: %v", err)
	}
}

func TestOpenEngineRejectsInvalidMetricCompression(t *testing.T) {
	root := t.TempDir()
	cfg := []byte("[metrics]\ncompression = \"bad_codec\"\n")
	if err := os.WriteFile(filepath.Join(root, "engine.toml"), cfg, 0644); err != nil {
		t.Fatalf("write engine.toml failed: %v", err)
	}

	_, err := OpenEngine(root, 1024*1024)
	if err == nil {
		t.Fatal("expected invalid compression config to fail")
	}
	if !strings.Contains(err.Error(), "metrics.compression") {
		t.Fatalf("expected metrics.compression error, got: %v", err)
	}
}

func TestOpenEngineRejectsInvalidMetricRawIngestAction(t *testing.T) {
	root := t.TempDir()
	cfg := []byte("[metrics]\nraw_ingest_action = \"archive\"\n")
	if err := os.WriteFile(filepath.Join(root, "engine.toml"), cfg, 0644); err != nil {
		t.Fatalf("write engine.toml failed: %v", err)
	}

	_, err := OpenEngine(root, 1024*1024)
	if err == nil {
		t.Fatal("expected invalid raw ingest action to fail")
	}
	if !strings.Contains(err.Error(), "metrics.raw_ingest_action") {
		t.Fatalf("expected metrics.raw_ingest_action error, got: %v", err)
	}
}

func TestOpenEngineReadsLoggingConfig(t *testing.T) {
	root := t.TempDir()
	cfg := []byte("[logging]\n\n[[logging.logger]]\noutput = \"console\"\nlevel = \"info\"\n\n[[logging.logger]]\noutput = \"nanotdb.log\"\nlevel = \"trace\"\n")
	if err := os.WriteFile(filepath.Join(root, "engine.toml"), cfg, 0644); err != nil {
		t.Fatalf("write engine.toml failed: %v", err)
	}

	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer e.Close()

	if len(e.Logging.Loggers) != 2 {
		t.Fatalf("logger count mismatch: got=%d want=2", len(e.Logging.Loggers))
	}
	if e.Logging.Loggers[0].Output != "console" || e.Logging.Loggers[0].Level != LogLevelInfo {
		t.Fatalf("first logger mismatch: got=%+v", e.Logging.Loggers[0])
	}
	if e.Logging.Loggers[1].Output != "nanotdb.log" || e.Logging.Loggers[1].Level != LogLevelTrace {
		t.Fatalf("second logger mismatch: got=%+v", e.Logging.Loggers[1])
	}
}

func TestOpenEngineWalFsyncPolicyAlways(t *testing.T) {
	root := t.TempDir()
	cfg := []byte("[wal]\nfsync_policy = \"always\"\n\n[defaults]\ndatabases = [\"prod\"]\n")
	if err := os.WriteFile(filepath.Join(root, "engine.toml"), cfg, 0644); err != nil {
		t.Fatalf("write engine.toml failed: %v", err)
	}

	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer e.Close()

	now := time.Now().UnixNano()
	if err := e.AddLine("prod/metric.wal 1 " + itoa64(now)); err != nil {
		t.Fatalf("AddLine failed: %v", err)
	}
	if err := e.AddLine("prod/metric.wal 2 " + itoa64(now+1)); err != nil {
		t.Fatalf("AddLine failed: %v", err)
	}

	db, _, err := e.getOrCreateDB("prod")
	if err != nil {
		t.Fatalf("getOrCreateDB failed: %v", err)
	}
	stats := db.wal.Stats()
	if stats.FsyncCount != 2 {
		t.Fatalf("wal fsync count mismatch: got=%d want=%d", stats.FsyncCount, 2)
	}
}

func TestOpenEngineCopiesDefaultsToDatabaseManifest(t *testing.T) {
	root := t.TempDir()
	cfg := []byte("[defaults]\ndatabases = [\"prod\"]\n\n[manifest_defaults.retention]\ngrace = \"1s\"\nretention_days = 7\nretention_action = \"delete\"\nmax_active_days = 3\npartition = \"year\"\n\n[manifest_defaults.wal]\nenabled = false\nskip_before = \"30m\"\n\n[manifest_defaults.page]\nmax_records = 3\nmax_bytes = 256\nmax_age = \"2s\"\n")
	if err := os.WriteFile(filepath.Join(root, "engine.toml"), cfg, 0644); err != nil {
		t.Fatalf("write engine.toml failed: %v", err)
	}

	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer e.Close()

	_, rt, err := e.getOrCreateDB("prod")
	if err != nil {
		t.Fatalf("getOrCreateDB failed: %v", err)
	}
	if rt.info.Grace != "1s" {
		t.Fatalf("grace mismatch: got=%q want=%q", rt.info.Grace, "1s")
	}
	if rt.info.RetentionDays != 7 {
		t.Fatalf("retention_days mismatch: got=%d want=%d", rt.info.RetentionDays, 7)
	}
	if rt.info.RetentionAction != RetentionActionDelete {
		t.Fatalf("retention_action mismatch: got=%q want=%q", rt.info.RetentionAction, RetentionActionDelete)
	}
	if rt.info.MaxActiveDays != 3 {
		t.Fatalf("max_active_days mismatch: got=%d want=%d", rt.info.MaxActiveDays, 3)
	}
	if rt.info.Partition != "year" {
		t.Fatalf("partition mismatch: got=%q want=%q", rt.info.Partition, "year")
	}
	if rt.info.WALEnabled {
		t.Fatalf("wal_enabled mismatch: got=true want=false")
	}
	if rt.info.WALSkipBefore != "30m" {
		t.Fatalf("wal_skip_before mismatch: got=%q want=%q", rt.info.WALSkipBefore, "30m")
	}
	if rt.info.PageMaxRecords != 3 {
		t.Fatalf("page_max_records mismatch: got=%d want=%d", rt.info.PageMaxRecords, 3)
	}
	if rt.info.PageMaxBytes != 256 {
		t.Fatalf("page_max_bytes mismatch: got=%d want=%d", rt.info.PageMaxBytes, 256)
	}
	if rt.info.PageMaxAge != "2s" {
		t.Fatalf("page_max_age mismatch: got=%q want=%q", rt.info.PageMaxAge, "2s")
	}

	manifestRaw, err := os.ReadFile(filepath.Join(root, "prod", "manifest.toml"))
	if err != nil {
		t.Fatalf("ReadFile manifest.toml failed: %v", err)
	}
	manifestText := string(manifestRaw)
	if !strings.Contains(manifestText, "[retention]") || !strings.Contains(manifestText, "grace = \"1s\"") {
		t.Fatalf("expected manifest.toml retention defaults from engine.toml")
	}
	if !strings.Contains(manifestText, "partition = \"year\"") {
		t.Fatalf("expected manifest.toml retention partition from engine.toml")
	}
	if !strings.Contains(manifestText, "retention_action = \"delete\"") {
		t.Fatalf("expected manifest.toml retention_action from engine.toml")
	}
	if !strings.Contains(manifestText, "[wal]") || !strings.Contains(manifestText, "enabled = false") {
		t.Fatalf("expected manifest.toml wal defaults from engine.toml")
	}
	if !strings.Contains(manifestText, "[page]") || !strings.Contains(manifestText, "max_records = 3") {
		t.Fatalf("expected manifest.toml page defaults from engine.toml")
	}
}

func TestEngineManifestRetentionPartitionInvalid(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "prod"), 0755); err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}
	manifest := []byte("[retention]\ngrace = \"5m\"\nretention_days = 30\nmax_active_days = 2\npartition = \"weekly\"\n\n[wal]\nenabled = true\nskip_before = \"1h\"\n\n[page]\nmax_records = 16000\nmax_bytes = 127000\nmax_age = \"60s\"\n")
	if err := os.WriteFile(filepath.Join(root, "prod", "manifest.toml"), manifest, 0644); err != nil {
		t.Fatalf("WriteFile manifest failed: %v", err)
	}

	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed unexpectedly: %v", err)
	}
	defer e.Close()

	if _, _, err := e.getOrCreateDB("prod"); err == nil {
		t.Fatalf("expected getOrCreateDB to fail for invalid partition")
	} else if !strings.Contains(err.Error(), "invalid partition") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEngineManifestRetentionActionInvalid(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "prod"), 0755); err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}
	manifest := []byte("[retention]\ngrace = \"5m\"\nretention_days = 30\nretention_action = \"compress\"\nmax_active_days = 2\npartition = \"day\"\n\n[wal]\nenabled = true\nskip_before = \"1h\"\n\n[page]\nmax_records = 16000\nmax_bytes = 127000\nmax_age = \"60s\"\n")
	if err := os.WriteFile(filepath.Join(root, "prod", "manifest.toml"), manifest, 0644); err != nil {
		t.Fatalf("WriteFile manifest failed: %v", err)
	}

	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed unexpectedly: %v", err)
	}
	defer e.Close()

	if _, _, err := e.getOrCreateDB("prod"); err == nil {
		t.Fatalf("expected getOrCreateDB to fail for invalid retention_action")
	} else if !strings.Contains(err.Error(), "invalid retention_action") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEngineManifestRetentionArchiveRejectsForeverPartition(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "prod"), 0755); err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}
	manifest := []byte("[retention]\ngrace = \"5m\"\nretention_days = 30\nretention_action = \"archive\"\nmax_active_days = 2\npartition = \"forever\"\n\n[wal]\nenabled = true\nskip_before = \"1h\"\n\n[page]\nmax_records = 16000\nmax_bytes = 127000\nmax_age = \"60s\"\n")
	if err := os.WriteFile(filepath.Join(root, "prod", "manifest.toml"), manifest, 0644); err != nil {
		t.Fatalf("WriteFile manifest failed: %v", err)
	}

	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed unexpectedly: %v", err)
	}
	defer e.Close()

	if _, _, err := e.getOrCreateDB("prod"); err == nil {
		t.Fatalf("expected getOrCreateDB to fail for archive retention on forever partition")
	} else if !strings.Contains(err.Error(), "partition=forever") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCleanupRetentionDeleteRemovesExpiredPartitionFamily(t *testing.T) {
	root := t.TempDir()
	oldPart := "1970-01-02"
	keepPart := "1970-01-04"
	for _, name := range []string{
		"data-" + oldPart + ".dat",
		"raw-" + oldPart + ".dat",
		"metric-" + oldPart + ".dat",
		"data-" + keepPart + ".dat",
		"raw-" + keepPart + ".dat",
		"metric-" + keepPart + ".dat",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0644); err != nil {
			t.Fatalf("WriteFile(%s) failed: %v", name, err)
		}
	}

	e := &Engine{logger: defaultEngineLogger()}
	db := &Database{Name: "prod", RootDataDir: root}
	rt := &dbRuntime{info: DBInfo{RetentionDays: 1, RetentionAction: RetentionActionDelete, Partition: "day"}}
	nowTS := Timestamp(4 * 24 * int64(time.Hour))

	e.cleanupRetention(db, rt, nowTS)

	for _, name := range []string{"data-" + oldPart + ".dat", "raw-" + oldPart + ".dat", "metric-" + oldPart + ".dat"} {
		if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
			t.Fatalf("expected expired file %s to be removed, got err=%v", name, err)
		}
	}
	for _, name := range []string{"data-" + keepPart + ".dat", "raw-" + keepPart + ".dat", "metric-" + keepPart + ".dat"} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Fatalf("expected live file %s to remain: %v", name, err)
		}
	}
}

func TestCleanupRetentionArchiveBundlesExpiredPartitionFamily(t *testing.T) {
	root := t.TempDir()
	part := "1970-01-02"
	files := map[string][]byte{
		"data-" + part + ".dat":   []byte("data payload"),
		"raw-" + part + ".dat":    []byte("raw payload"),
		"metric-" + part + ".dat": []byte("metric payload"),
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(root, name), body, 0644); err != nil {
			t.Fatalf("WriteFile(%s) failed: %v", name, err)
		}
	}

	e := &Engine{logger: defaultEngineLogger()}
	db := &Database{Name: "prod", RootDataDir: root}
	rt := &dbRuntime{info: DBInfo{RetentionDays: 1, RetentionAction: RetentionActionArchive, Partition: "day"}}
	nowTS := Timestamp(4 * 24 * int64(time.Hour))

	e.cleanupRetention(db, rt, nowTS)

	archivePath := filepath.Join(root, "archive-1970-01.tar")
	f, err := os.Open(archivePath)
	if err != nil {
		t.Fatalf("Open(%s) failed: %v", archivePath, err)
	}
	defer f.Close()
	tr := tar.NewReader(f)
	archived := make(map[string]string)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar.Next failed: %v", err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("ReadAll tar entry failed: %v", err)
		}
		archived[hdr.Name] = string(body)
	}
	for name, body := range files {
		if got := archived[name]; got != string(body) {
			t.Fatalf("archive entry mismatch for %s: got=%q want=%q", name, got, string(body))
		}
		if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
			t.Fatalf("expected archived source file %s to be removed, got err=%v", name, err)
		}
	}
}

func TestEngineManifestRollupDefaultsApplyToPatternJobs(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "prod"), 0755); err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}

	manifest := []byte("[retention]\n" +
		"grace = \"5m\"\n" +
		"retention_days = 30\n" +
		"max_active_days = 2\n" +
		"partition = \"day\"\n\n" +
		"[wal]\n" +
		"enabled = true\n" +
		"skip_before = \"1h\"\n\n" +
		"[page]\n" +
		"max_records = 16000\n" +
		"max_bytes = 127000\n" +
		"max_age = \"60s\"\n\n" +
		"[rollups]\n" +
		"enabled = true\n" +
		"default_interval = \"1h\"\n" +
		"default_destination_db = \"prod_rollup_1h\"\n" +
		"default_aggregates = [\"min\",\"max\"]\n" +
		"global_exclude_patterns = [\"nanotdb.*\"]\n\n" +
		"[[rollups.jobs]]\n" +
		"id = \"auto-temp\"\n" +
		"source_pattern = \"temp.*\"\n")

	if err := os.WriteFile(filepath.Join(root, "prod", "manifest.toml"), manifest, 0644); err != nil {
		t.Fatalf("WriteFile manifest failed: %v", err)
	}

	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer e.Close()

	_, rt, err := e.getOrCreateDB("prod")
	if err != nil {
		t.Fatalf("getOrCreateDB failed: %v", err)
	}

	if !rt.info.Rollups.Enabled {
		t.Fatalf("expected rollups to be enabled")
	}
	if len(rt.info.Rollups.Jobs) != 1 {
		t.Fatalf("jobs count mismatch: got=%d want=1", len(rt.info.Rollups.Jobs))
	}
	job := rt.info.Rollups.Jobs[0]
	if job.SourcePattern != "temp.*" {
		t.Fatalf("source_pattern mismatch: got=%q want=%q", job.SourcePattern, "temp.*")
	}
	if job.Interval != "1h" {
		t.Fatalf("interval mismatch: got=%q want=%q", job.Interval, "1h")
	}
	if job.DestinationDB != "prod_rollup_1h" {
		t.Fatalf("destination_db mismatch: got=%q want=%q", job.DestinationDB, "prod_rollup_1h")
	}
	if len(job.Aggregates) != 2 || job.Aggregates[0] != "min" || job.Aggregates[1] != "max" {
		t.Fatalf("aggregates mismatch: got=%v want=[min max]", job.Aggregates)
	}
}

func TestEngineYearPartitionWritesYearlyDatFile(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "prod"), 0755); err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}
	manifest := []byte("[retention]\ngrace = \"5m\"\nretention_days = 30\nmax_active_days = 30\npartition = \"year\"\n\n[wal]\nenabled = true\nskip_before = \"1h\"\n\n[page]\nmax_records = 2\nmax_bytes = 127000\nmax_age = \"60s\"\n")
	if err := os.WriteFile(filepath.Join(root, "prod", "manifest.toml"), manifest, 0644); err != nil {
		t.Fatalf("WriteFile manifest failed: %v", err)
	}

	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer e.Close()

	ts1 := time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC).UnixNano()
	ts2 := time.Date(2026, 12, 31, 23, 0, 0, 0, time.UTC).UnixNano()
	if err := e.AddLine("prod/metric.a 1 " + itoa64(ts1)); err != nil {
		t.Fatalf("AddLine ts1 failed: %v", err)
	}
	if err := e.AddLine("prod/metric.a 2 " + itoa64(ts2)); err != nil {
		t.Fatalf("AddLine ts2 failed: %v", err)
	}

	if _, err := os.Stat(filepath.Join(root, "prod", "data-2026.dat")); err != nil {
		t.Fatalf("expected yearly data file, got error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "prod", "data-2026-01-02.dat")); err == nil {
		t.Fatalf("did not expect daily data file when partition=year")
	}
}

func TestEngineAddLineOptionalTimestamp(t *testing.T) {
	e, err := OpenEngine(t.TempDir(), 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer e.Close()

	if err := e.AddLine("prod/system.mem.used 12.5"); err != nil {
		t.Fatalf("AddLine float no-ts failed: %v", err)
	}

	last, found, err := e.QueryLast("prod", "system.mem.used")
	if err != nil {
		t.Fatalf("QueryLast failed: %v", err)
	}
	if !found {
		t.Fatalf("expected last sample")
	}
	if last.ValueType != Float32Sample {
		t.Fatalf("type mismatch: got=%d want=%d", last.ValueType, Float32Sample)
	}
	if last.TS <= 0 {
		t.Fatalf("expected unix nanos timestamp > 0")
	}
}

func TestEngineFloatMetricAcceptsIntegerLiteral(t *testing.T) {
	e, err := OpenEngine(t.TempDir(), 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer e.Close()

	if err := e.AddLine("prod/system.mem.used 12.5 100"); err != nil {
		t.Fatalf("AddLine float failed: %v", err)
	}
	if err := e.AddLine("prod/system.mem.used 0 101"); err != nil {
		t.Fatalf("AddLine int-literal for float metric failed: %v", err)
	}

	last, found, err := e.QueryLast("prod", "system.mem.used")
	if err != nil {
		t.Fatalf("QueryLast failed: %v", err)
	}
	if !found {
		t.Fatalf("expected last sample")
	}
	if last.ValueType != Float32Sample {
		t.Fatalf("type mismatch: got=%d want=%d", last.ValueType, Float32Sample)
	}
	if last.Float32 != 0 {
		t.Fatalf("value mismatch: got=%v want=0", last.Float32)
	}
}

func TestEngineAddLineLargeIntegerLiteralAsFloat(t *testing.T) {
	e, err := OpenEngine(t.TempDir(), 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer e.Close()

	if err := e.AddLine("prod/system.big.counter 19563757568 100"); err != nil {
		t.Fatalf("AddLine large integer literal failed: %v", err)
	}

	last, found, err := e.QueryLast("prod", "system.big.counter")
	if err != nil {
		t.Fatalf("QueryLast failed: %v", err)
	}
	if !found {
		t.Fatalf("expected last sample")
	}
	if last.ValueType != Float32Sample {
		t.Fatalf("type mismatch: got=%d want=%d", last.ValueType, Float32Sample)
	}
}

func TestEngineQueryRangeActiveAndPersisted(t *testing.T) {
	e, err := OpenEngine(t.TempDir(), 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer e.Close()

	// Fill one full page to force persisted records.
	base := int64(1_000_000_000)
	for i := 0; i < PageMaxRecords; i++ {
		line := "prod/sensors.room1.temp " + itoa(i) + " " + itoa64(base+int64(i))
		if err := e.AddLine(line); err != nil {
			t.Fatalf("AddLine(%d) failed: %v", i, err)
		}
	}
	// Add extra samples to remain in active page.
	extraTS1 := base + int64(PageMaxRecords) + 1
	extraTS2 := base + int64(PageMaxRecords) + 2
	if err := e.AddLine("prod/sensors.room1.temp 1001 " + itoa64(extraTS1)); err != nil {
		t.Fatalf("AddLine extra failed: %v", err)
	}
	if err := e.AddLine("prod/sensors.room1.temp 1002 " + itoa64(extraTS2)); err != nil {
		t.Fatalf("AddLine extra failed: %v", err)
	}

	var rows []Sample
	err = e.QueryRange("prod", "sensors.room1.temp", Timestamp(base+900), Timestamp(extraTS2), 1, func(s Sample) error {
		rows = append(rows, s)
		return nil
	})
	if err != nil {
		t.Fatalf("QueryRange failed: %v", err)
	}
	if len(rows) < 100 {
		t.Fatalf("expected range rows from persisted+active pages, got=%d", len(rows))
	}

	// Sanity: last row should be one of the active-page samples.
	last := rows[len(rows)-1]
	if last.TS != Timestamp(extraTS2) {
		t.Fatalf("unexpected final timestamp: got=%d want=%d", last.TS, extraTS2)
	}
}

func TestEngineQueryRangeWithStride(t *testing.T) {
	e, err := OpenEngine(t.TempDir(), 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer e.Close()

	// Add 100 samples
	for i := 0; i < 100; i++ {
		line := "prod/test.metric " + itoa(i) + " " + itoa64(1_000_000_000+int64(i))
		if err := e.AddLine(line); err != nil {
			t.Fatalf("AddLine(%d) failed: %v", i, err)
		}
	}

	// Query all with stride=1 (every sample)
	var allRows []Sample
	if err := e.QueryRange("prod", "test.metric", Timestamp(1_000_000_000), Timestamp(1_000_000_099), 1, func(s Sample) error {
		allRows = append(allRows, s)
		return nil
	}); err != nil {
		t.Fatalf("QueryRange stride=1 failed: %v", err)
	}

	// Query with stride=5 (every 5th sample)
	var strideRows []Sample
	if err := e.QueryRange("prod", "test.metric", Timestamp(1_000_000_000), Timestamp(1_000_000_099), 5, func(s Sample) error {
		strideRows = append(strideRows, s)
		return nil
	}); err != nil {
		t.Fatalf("QueryRange stride=5 failed: %v", err)
	}

	if len(allRows) != 100 {
		t.Fatalf("expected 100 rows with stride=1, got %d", len(allRows))
	}
	if len(strideRows)*5 > len(allRows)+5 || len(strideRows)*5 < len(allRows)-5 {
		t.Fatalf("expected ~%d rows with stride=5, got %d", len(allRows)/5, len(strideRows))
	}
}

func TestEngineQueryRangeSkipsOutOfScopeCorruptFrame(t *testing.T) {
	root := t.TempDir()
	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}

	base := int64(1_000_000_000)
	for i := 0; i < 2*PageMaxRecords; i++ {
		line := "prod/metric.gc " + itoa(i) + " " + itoa64(base+int64(i))
		if err := e.AddLine(line); err != nil {
			t.Fatalf("AddLine(%d) failed: %v", i, err)
		}
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	day := dayKey(Timestamp(base))
	path := filepath.Join(root, "prod", "data-"+day+".dat")
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if len(blob) < HeaderSize+1+4 {
		t.Fatalf("unexpected short blob: %d", len(blob))
	}

	compressedLen, n := binary.Uvarint(blob[HeaderSize:])
	if n <= 0 {
		t.Fatalf("invalid first frame varint")
	}
	payloadStart := HeaderSize + n
	if payloadStart+int(compressedLen)+4 > len(blob) {
		t.Fatalf("unexpected truncated first frame")
	}
	if compressedLen == 0 {
		t.Fatalf("unexpected empty first frame payload")
	}
	// Corrupt the first frame payload; query should skip it when outside requested range.
	blob[payloadStart] ^= 0xFF
	if err := os.WriteFile(path, blob, 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	e2, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine reopen failed: %v", err)
	}
	defer e2.Close()

	rows := 0
	from := Timestamp(base + int64(PageMaxRecords) + 500)
	to := Timestamp(base + int64(PageMaxRecords) + 999)
	err = e2.QueryRange("prod", "metric.gc", from, to, 1, func(s Sample) error {
		rows++
		return nil
	})
	if err != nil {
		t.Fatalf("QueryRange failed: %v", err)
	}
	if rows != 500 {
		t.Fatalf("unexpected row count after skip: got=%d want=500", rows)
	}
}

func TestEngineWALSkipBeforeBackfill(t *testing.T) {
	e, err := OpenEngine(t.TempDir(), 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer e.Close()

	now := time.Now().UnixNano()
	oldTS := now - int64(2*time.Hour)
	if err := e.AddLine("prod/metric.wal 1 " + itoa64(oldTS)); err != nil {
		t.Fatalf("AddLine old sample failed: %v", err)
	}

	db, _, err := e.getOrCreateDB("prod")
	if err != nil {
		t.Fatalf("getOrCreateDB failed: %v", err)
	}
	recs, err := db.wal.Records()
	if err != nil {
		t.Fatalf("WAL records failed: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("expected no WAL records for old backfill sample, got=%d", len(recs))
	}

	if err := e.AddLine("prod/metric.wal 2 " + itoa64(now)); err != nil {
		t.Fatalf("AddLine current sample failed: %v", err)
	}
	recs, err = db.wal.Records()
	if err != nil {
		t.Fatalf("WAL records failed: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("expected one WAL record for near-realtime sample, got=%d", len(recs))
	}
}

func TestEngineWALDisabledPerDatabase(t *testing.T) {
	root := t.TempDir()
	dbDir := filepath.Join(root, "prod")
	if err := os.MkdirAll(dbDir, 0755); err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}
	manifest := []byte("[wal]\nenabled = false\n")
	if err := os.WriteFile(filepath.Join(dbDir, "manifest.toml"), manifest, 0644); err != nil {
		t.Fatalf("WriteFile manifest failed: %v", err)
	}

	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer e.Close()

	if err := e.AddLine("prod/metric.now 10 " + itoa64(time.Now().UnixNano())); err != nil {
		t.Fatalf("AddLine failed: %v", err)
	}
	db, _, err := e.getOrCreateDB("prod")
	if err != nil {
		t.Fatalf("getOrCreateDB failed: %v", err)
	}
	recs, err := db.wal.Records()
	if err != nil {
		t.Fatalf("WAL records failed: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("expected zero WAL records when wal.enabled=false, got=%d", len(recs))
	}
}

func TestEngineManifestFrameFlushConfigApplied(t *testing.T) {
	root := t.TempDir()
	dbDir := filepath.Join(root, "prod")
	if err := os.MkdirAll(dbDir, 0755); err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}
	manifest := []byte("[page]\nmax_records = 3\nmax_bytes = 128\nmax_age = \"30s\"\n")
	if err := os.WriteFile(filepath.Join(dbDir, "manifest.toml"), manifest, 0644); err != nil {
		t.Fatalf("WriteFile manifest failed: %v", err)
	}

	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer e.Close()

	base := int64(1_000_000_000)
	if err := e.AddLine("prod/metric.frame 1 " + itoa64(base)); err != nil {
		t.Fatalf("AddLine 1 failed: %v", err)
	}
	if err := e.AddLine("prod/metric.frame 2 " + itoa64(base+1)); err != nil {
		t.Fatalf("AddLine 2 failed: %v", err)
	}
	db, rt, err := e.getOrCreateDB("prod")
	if err != nil {
		t.Fatalf("getOrCreateDB failed: %v", err)
	}
	day := dayKey(Timestamp(base))
	p := rt.openDays[day]
	if p == nil {
		t.Fatalf("expected open page")
	}
	if p.MaxRecords != 3 {
		t.Fatalf("page max records mismatch: got=%d want=3", p.MaxRecords)
	}
	if p.MaxBytes != 128 {
		t.Fatalf("page max bytes mismatch: got=%d want=128", p.MaxBytes)
	}
	if p.MaxAge != 30*time.Second {
		t.Fatalf("page max age mismatch: got=%s want=%s", p.MaxAge, 30*time.Second)
	}

	// 3rd record should trigger record-count-based flush and clear active page.
	if err := e.AddLine("prod/metric.frame 3 " + itoa64(base+2)); err != nil {
		t.Fatalf("AddLine 3 failed: %v", err)
	}
	if rt.openDays[day] != nil {
		t.Fatalf("expected page flushed and cleared after reaching page.max_records")
	}
	if _, err := os.Stat(filepath.Join(db.RootDataDir, "data-"+day+".dat")); err != nil {
		t.Fatalf("expected daily data file written: %v", err)
	}
}

func TestEngineManifestGraceDurationStringAccepted(t *testing.T) {
	root := t.TempDir()
	dbDir := filepath.Join(root, "prod")
	if err := os.MkdirAll(dbDir, 0755); err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}
	manifest := []byte("[retention]\ngrace = \"1s\"\n")
	if err := os.WriteFile(filepath.Join(dbDir, "manifest.toml"), manifest, 0644); err != nil {
		t.Fatalf("WriteFile manifest failed: %v", err)
	}

	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer e.Close()

	_, rt, err := e.getOrCreateDB("prod")
	if err != nil {
		t.Fatalf("getOrCreateDB failed: %v", err)
	}
	if rt.info.Grace != "1s" {
		t.Fatalf("grace mismatch: got=%q want=%q", rt.info.Grace, "1s")
	}
}

func TestEngineManifestGraceDurationStringInvalid(t *testing.T) {
	root := t.TempDir()
	dbDir := filepath.Join(root, "prod")
	if err := os.MkdirAll(dbDir, 0755); err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}
	manifest := []byte("[retention]\ngrace = \"not_a_duration\"\n")
	if err := os.WriteFile(filepath.Join(dbDir, "manifest.toml"), manifest, 0644); err != nil {
		t.Fatalf("WriteFile manifest failed: %v", err)
	}

	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer e.Close()

	if _, _, err := e.getOrCreateDB("prod"); err == nil {
		t.Fatalf("expected invalid grace duration to fail")
	}
}

// TestEngineInternalStatsEmissionEnabledByManifest verifies that after Close(),
// engine stats are flushed to the internal DB.  Stats are now engine-level (not
// manifest-driven), so we just use the default engine and check after Close.
func TestEngineInternalStatsEmissionEnabledByManifest(t *testing.T) {
	root := t.TempDir()
	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}

	now := time.Now().UnixNano()
	if err := e.AddLine("prod/metric.a 1 " + itoa64(now)); err != nil {
		t.Fatalf("AddLine failed: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Reopen and query via QueryRange (LastValid may not be set in new engine).
	e2, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine2 failed: %v", err)
	}
	defer e2.Close()

	found := false
	err = e2.QueryRange("internal", "nanotdb/prod/wal/append_count",
		Timestamp(now-int64(time.Second)), Timestamp(now+int64(time.Hour)), 1,
		func(s Sample) error { found = true; return nil })
	if err != nil {
		t.Fatalf("QueryRange failed: %v", err)
	}
	if !found {
		t.Fatalf("expected internal stats sample to be written")
	}
}

// TestEngineInternalStatsEnabledByDefault verifies that after Close() stats are
// persisted to the internal DB by default.
func TestEngineInternalStatsEnabledByDefault(t *testing.T) {
	root := t.TempDir()
	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}

	now := time.Now().UnixNano()
	if err := e.AddLine("prod/metric.a 1 " + itoa64(now)); err != nil {
		t.Fatalf("AddLine failed: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	e2, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine2 failed: %v", err)
	}
	defer e2.Close()

	found := false
	err = e2.QueryRange("internal", "nanotdb/prod/wal/append_count",
		Timestamp(now-int64(time.Second)), Timestamp(now+int64(time.Hour)), 1,
		func(s Sample) error { found = true; return nil })
	if err != nil {
		t.Fatalf("QueryRange failed: %v", err)
	}
	if !found {
		t.Fatalf("expected internal stats sample when feature is enabled by default")
	}
}

// TestEngineInternalStatsCanBeDisabled verifies that when StatsEnabled is set
// to false no internal stats are persisted.
func TestEngineInternalStatsCanBeDisabled(t *testing.T) {
	root := t.TempDir()
	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	e.StatsEnabled = false

	now := time.Now().UnixNano()
	if err := e.AddLine("prod/metric.a 1 " + itoa64(now)); err != nil {
		t.Fatalf("AddLine failed: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	e2, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine2 failed: %v", err)
	}
	defer e2.Close()

	found := false
	_ = e2.QueryRange("internal", "nanotdb/prod/wal/append_count",
		Timestamp(now-int64(time.Second)), Timestamp(now+int64(time.Hour)), 1,
		func(s Sample) error { found = true; return nil })
	if found {
		t.Fatalf("expected no internal stats samples when feature is disabled")
	}
}

func TestEngineWALResetAfterPageFlush(t *testing.T) {
	root := t.TempDir()
	dbDir := filepath.Join(root, "prod")
	if err := os.MkdirAll(dbDir, 0755); err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}
	manifest := []byte("[page]\nmax_records = 3\nmax_bytes = 1048576\nmax_age = \"1h\"\n")
	if err := os.WriteFile(filepath.Join(dbDir, "manifest.toml"), manifest, 0644); err != nil {
		t.Fatalf("WriteFile manifest failed: %v", err)
	}

	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer e.Close()

	_, rt, err := e.getOrCreateDB("prod")
	if err != nil {
		t.Fatalf("getOrCreateDB failed: %v", err)
	}

	base := time.Now().UnixNano()
	for i := 0; i < rt.info.PageMaxRecords; i++ {
		line := "prod/metric.flush " + itoa(i) + " " + itoa64(base+int64(i))
		if err := e.AddLine(line); err != nil {
			t.Fatalf("AddLine(%d) failed: %v", i, err)
		}
	}

	db, _, err := e.getOrCreateDB("prod")
	if err != nil {
		t.Fatalf("getOrCreateDB failed: %v", err)
	}
	recs, err := db.wal.Records()
	if err != nil {
		t.Fatalf("WAL records failed: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("expected WAL to be reset after flush, got=%d records", len(recs))
	}
}

func TestEngineWALResetAfterMidnightFlushesStalePreviousDay(t *testing.T) {
	root := t.TempDir()
	dbDir := filepath.Join(root, "prod")
	if err := os.MkdirAll(dbDir, 0755); err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}
	manifest := []byte("[retention]\nmax_active_days = 2\npartition = \"day\"\n\n[page]\nmax_records = 16000\nmax_bytes = 1048576\nmax_age = \"1ms\"\n")
	if err := os.WriteFile(filepath.Join(dbDir, "manifest.toml"), manifest, 0644); err != nil {
		t.Fatalf("WriteFile manifest failed: %v", err)
	}

	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer e.Close()

	day1 := time.Date(2026, 5, 18, 23, 59, 59, 0, time.UTC).UnixNano()
	day2a := time.Date(2026, 5, 19, 0, 0, 1, 0, time.UTC).UnixNano()
	day2b := time.Date(2026, 5, 19, 0, 0, 2, 0, time.UTC).UnixNano()

	if err := e.AddLine("prod/metric.flush 1 " + itoa64(day1)); err != nil {
		t.Fatalf("AddLine day1 failed: %v", err)
	}
	time.Sleep(3 * time.Millisecond)
	if err := e.AddLine("prod/metric.flush 2 " + itoa64(day2a)); err != nil {
		t.Fatalf("AddLine day2a failed: %v", err)
	}
	time.Sleep(3 * time.Millisecond)
	if err := e.AddLine("prod/metric.flush 3 " + itoa64(day2b)); err != nil {
		t.Fatalf("AddLine day2b failed: %v", err)
	}

	db, _, err := e.getOrCreateDB("prod")
	if err != nil {
		t.Fatalf("getOrCreateDB failed: %v", err)
	}
	recs, err := db.wal.Records()
	if err != nil {
		t.Fatalf("WAL records failed: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("expected WAL reset after flushing stale previous-day page and current page, got=%d records", len(recs))
	}
}

func TestEngineReplaysWALOnStartup(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "prod", "prod")

	// Simulate pre-crash state: sample appended to WAL, catalog persisted, no page flush.
	db, err := NewDatabase(base, 1024*1024)
	if err != nil {
		t.Fatalf("NewDatabase failed: %v", err)
	}
	if err := addSampleToDB(db, "metric.a", Timestamp(1000), int32(42)); err != nil {
		t.Fatalf("AddSample failed: %v", err)
	}
	if err := db.catalog.WriteCatalog(); err != nil {
		t.Fatalf("WriteCatalog failed: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close database failed: %v", err)
	}

	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer e.Close()

	s, found, err := e.QueryLast("prod", "metric.a")
	if err != nil {
		t.Fatalf("QueryLast failed: %v", err)
	}
	if !found {
		t.Fatalf("expected metric to be recovered from WAL on startup")
	}
	if s.ValueType != Int32Sample || s.Int32 != 42 {
		t.Fatalf("recovered value mismatch: type=%d int32=%d", s.ValueType, s.Int32)
	}

	stats, ok := e.DBStats("prod")
	if !ok {
		t.Fatalf("DBStats not found for prod")
	}
	if stats.WAL.AppendCount != 1 {
		t.Fatalf("expected recovered wal append_count=1, got=%d", stats.WAL.AppendCount)
	}
	if stats.WAL.AppendBytes <= 0 {
		t.Fatalf("expected recovered wal append_bytes > 0")
	}

	snap := e.stats.snapshot()
	if got := snap["prod/wal/replay_records"]; got != 1 {
		t.Fatalf("expected replay_records=1, got=%f", got)
	}
	if got := snap["prod/wal/replay_bytes"]; got <= 0 {
		t.Fatalf("expected replay_bytes>0, got=%f", got)
	}
	if got := snap["prod/wal/replay_success_count"]; got != 1 {
		t.Fatalf("expected replay_success_count=1, got=%f", got)
	}
	if got := snap["prod/wal/replay_error_count"]; got != 0 {
		t.Fatalf("expected replay_error_count=0, got=%f", got)
	}

	flushTS := Timestamp(1000 + int64(time.Second))
	e.flushStatsToInternal(flushTS)

	is, found, err := e.QueryLast("internal", "nanotdb/prod/wal/append_count")
	if err != nil {
		t.Fatalf("QueryLast internal append_count failed: %v", err)
	}
	if !found {
		t.Fatalf("expected internal wal append_count after startup stats flush")
	}
	if is.Float32 < 1 {
		t.Fatalf("expected internal wal append_count >= 1, got=%f", is.Float32)
	}

	rs, found, err := e.QueryLast("internal", "nanotdb/prod/wal/replay_success_count")
	if err != nil {
		t.Fatalf("QueryLast internal replay_success_count failed: %v", err)
	}
	if !found {
		t.Fatalf("expected internal replay_success_count after startup stats flush")
	}
	if rs.Float32 != 1 {
		t.Fatalf("expected internal replay_success_count=1, got=%f", rs.Float32)
	}

	runtimeHeap, found, err := e.QueryLast("internal", "nanotdb/runtime/heap_alloc_bytes")
	if err != nil {
		t.Fatalf("QueryLast internal runtime heap_alloc_bytes failed: %v", err)
	}
	if !found {
		t.Fatalf("expected internal runtime heap_alloc_bytes after stats flush")
	}
	if runtimeHeap.Float32 <= 0 {
		t.Fatalf("expected internal runtime heap_alloc_bytes > 0, got=%f", runtimeHeap.Float32)
	}
}

func TestEngineReplaysWALAndRebuildsCatalogFromWALMetadata(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "prod", "prod")

	db, err := NewDatabase(base, 1024*1024)
	if err != nil {
		t.Fatalf("NewDatabase failed: %v", err)
	}
	if err := addSampleToDB(db, "metric.a", Timestamp(1000), int32(42)); err != nil {
		t.Fatalf("AddSample failed: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close database failed: %v", err)
	}

	catPath := filepath.Join(root, "prod", "catalog.json")
	st, err := os.Stat(catPath)
	if err != nil {
		t.Fatalf("Stat catalog failed: %v", err)
	}
	if st.Size() != 0 {
		t.Fatalf("expected empty catalog before replay, got size=%d", st.Size())
	}

	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer e.Close()

	s, found, err := e.QueryLast("prod", "metric.a")
	if err != nil {
		t.Fatalf("QueryLast failed: %v", err)
	}
	if !found {
		t.Fatalf("expected last sample")
	}
	if s.ValueType != Int32Sample || s.Int32 != 42 {
		t.Fatalf("recovered value mismatch: type=%d int32=%d", s.ValueType, s.Int32)
	}

	db2, _, err := e.getOrCreateDB("prod")
	if err != nil {
		t.Fatalf("getOrCreateDB failed: %v", err)
	}
	if _, ok := db2.catalog.GetMetricEntry("metric.a"); !ok {
		t.Fatalf("expected catalog entry rebuilt from wal metadata")
	}
	if !db2.catalog.IsDirty() {
		t.Fatalf("expected rebuilt catalog to remain dirty until persisted")
	}
}

func TestMaybeResetWALPersistsDirtyCatalogBeforeTruncate(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "prod", "prod")

	db, err := NewDatabase(base, 1024*1024)
	if err != nil {
		t.Fatalf("NewDatabase failed: %v", err)
	}
	defer db.Close()

	if err := addSampleToDB(db, "metric.a", Timestamp(1000), int32(42)); err != nil {
		t.Fatalf("AddSample failed: %v", err)
	}
	if !db.catalog.IsDirty() {
		t.Fatalf("expected dirty catalog after new metric")
	}

	rt := &dbRuntime{info: DBInfo{WALEnabled: true}, openDays: make(map[string]*Page), sealedDays: make(map[string]struct{})}
	if err := maybeResetWAL(db, rt); err != nil {
		t.Fatalf("maybeResetWAL failed: %v", err)
	}
	if db.catalog.IsDirty() {
		t.Fatalf("expected catalog to be persisted before wal reset")
	}

	catPath := filepath.Join(root, "prod", "catalog.json")
	raw, err := os.ReadFile(catPath)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", catPath, err)
	}
	if !bytes.Contains(raw, []byte(`"name": "metric.a"`)) {
		t.Fatalf("expected persisted catalog to contain metric.a, got: %s", raw)
	}
	recs, err := db.wal.Records()
	if err != nil {
		t.Fatalf("Records failed: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("expected empty wal after reset, got=%d records", len(recs))
	}
}

func TestEngineReplayRespectsMaxActiveDays(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "prod", "prod")

	// Create WAL with samples spanning 3 UTC days.
	db, err := NewDatabase(base, 1024*1024)
	if err != nil {
		t.Fatalf("NewDatabase failed: %v", err)
	}
	day1 := Timestamp(24 * int64(time.Hour))
	day2 := Timestamp(2 * 24 * int64(time.Hour))
	day3 := Timestamp(3 * 24 * int64(time.Hour))
	if err := addSampleToDB(db, "metric.a", day1, int32(1)); err != nil {
		t.Fatalf("AddSample day1 failed: %v", err)
	}
	if err := addSampleToDB(db, "metric.a", day2, int32(2)); err != nil {
		t.Fatalf("AddSample day2 failed: %v", err)
	}
	if err := addSampleToDB(db, "metric.a", day3, int32(3)); err != nil {
		t.Fatalf("AddSample day3 failed: %v", err)
	}
	if err := db.catalog.WriteCatalog(); err != nil {
		t.Fatalf("WriteCatalog failed: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close database failed: %v", err)
	}

	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer e.Close()

	_, rt, err := e.getOrCreateDB("prod")
	if err != nil {
		t.Fatalf("getOrCreateDB failed: %v", err)
	}

	if len(rt.openDays) != 2 {
		t.Fatalf("expected exactly 2 open days after replay, got=%d", len(rt.openDays))
	}
	if _, ok := rt.openDays["1970-01-02"]; ok {
		t.Fatalf("expected oldest day to be sealed during replay")
	}

	sealedPath := filepath.Join(root, "prod", "data-1970-01-02.dat")
	if _, err := os.Stat(sealedPath); err != nil {
		t.Fatalf("expected sealed day file %s to exist: %v", sealedPath, err)
	}
}

// TestEngineDBStatsTrackDataAndWALFlush verifies that data and WAL flush stats
// are tracked at the engine level after an explicit page flush.
func TestEngineDBStatsTrackDataAndWALFlush(t *testing.T) {
	e, err := OpenEngine(t.TempDir(), 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer e.Close()

	ts := time.Now().UnixNano()
	if err := e.AddLine("prod/metric.stats 1 " + itoa64(ts)); err != nil {
		t.Fatalf("AddLine failed: %v", err)
	}

	_, rt, err := e.getOrCreateDB("prod")
	if err != nil {
		t.Fatalf("getOrCreateDB failed: %v", err)
	}
	day := dayKey(Timestamp(ts))
	p := rt.openDays[day]
	if p == nil {
		t.Fatalf("expected open page for day %s", day)
	}

	// Use the Engine method so stats are updated.
	db := e.dbs["prod"]
	if err := e.writePageToDailyFile(db, "prod", day, p); err != nil {
		t.Fatalf("writePageToDailyFile failed: %v", err)
	}
	rt.openDays[day] = nil
	e.captureWALStats(db, "prod")
	if err := maybeResetWAL(db, rt); err != nil {
		t.Fatalf("maybeResetWAL failed: %v", err)
	}

	stats, ok := e.DBStats("prod")
	if !ok {
		t.Fatalf("DBStats not found for prod")
	}
	if stats.DataFile.FlushCount != 1 {
		t.Fatalf("data flush count mismatch: got=%d want=1", stats.DataFile.FlushCount)
	}
	if stats.DataFile.TotalFlushRecords != 1 {
		t.Fatalf("data flush records mismatch: got=%d want=1", stats.DataFile.TotalFlushRecords)
	}
	if stats.DataFile.TotalFlushBytes <= 0 {
		t.Fatalf("expected positive data flush bytes")
	}
	if stats.DataFile.TotalFlushCompressed <= 0 {
		t.Fatalf("expected positive compressed flush bytes")
	}
	if stats.DataFile.MinFlushRecords != 1 || stats.DataFile.MaxFlushRecords != 1 {
		t.Fatalf("expected min/max flush records to be 1, got min=%d max=%d", stats.DataFile.MinFlushRecords, stats.DataFile.MaxFlushRecords)
	}
	if stats.DataFile.FlushDurationTotal <= 0 {
		t.Fatalf("expected positive data flush duration")
	}
	if stats.DataFile.SyncCount != 1 {
		t.Fatalf("expected sync count to be 1, got=%d", stats.DataFile.SyncCount)
	}
	if stats.DataFile.SyncDurationTotal <= 0 {
		t.Fatalf("expected positive data sync duration")
	}
	if stats.WAL.FlushCount != 1 {
		t.Fatalf("wal flush count mismatch: got=%d want=1", stats.WAL.FlushCount)
	}
	if stats.WAL.FlushedBytes <= 0 {
		t.Fatalf("expected positive wal flushed bytes")
	}
	if stats.WAL.MinFlushBytes <= 0 || stats.WAL.MaxFlushBytes <= 0 {
		t.Fatalf("expected positive wal min/max flush bytes, got min=%d max=%d", stats.WAL.MinFlushBytes, stats.WAL.MaxFlushBytes)
	}
	if stats.WAL.ResetDurationTotal <= 0 {
		t.Fatalf("expected positive wal reset duration")
	}
	if stats.WAL.FsyncDurationTotal <= 0 {
		t.Fatalf("expected positive wal fsync duration")
	}
}

// TestEngineStatsEmittedOnClose verifies that engine stats are persisted on Close().
func TestEngineStatsEmittedOnClose(t *testing.T) {
	root := t.TempDir()
	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}

	base := time.Now().UnixNano()
	for i := 0; i < 100; i++ {
		if err := e.AddLine("prod/metric " + itoa(i) + " " + itoa64(base+int64(i))); err != nil {
			t.Fatalf("AddLine failed: %v", err)
		}
	}

	if err := e.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	internalE, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine internal failed: %v", err)
	}
	defer internalE.Close()

	var lastFlushCount float32
	err = internalE.QueryRange("internal", "nanotdb/prod/data/flush_count",
		Timestamp(base-int64(time.Second)), Timestamp(base+int64(time.Hour)), 1,
		func(s Sample) error { lastFlushCount = s.Float32; return nil })
	if err != nil {
		t.Fatalf("QueryRange failed: %v", err)
	}
	if lastFlushCount < 1 {
		t.Fatalf("expected flush_count >= 1, got=%f", lastFlushCount)
	}
}

func TestEngineCreatesDatabaseInRootFolder(t *testing.T) {
	root := filepath.Join(t.TempDir(), "data")
	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}

	if err := e.AddLine("plantA/room.temp 33 1000"); err != nil {
		t.Fatalf("AddLine failed: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	if _, err := os.Stat(filepath.Join(root, "plantA", "catalog.json")); err != nil {
		t.Fatalf("expected catalog in db folder: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "plantA", "data-1970-01-01.dat")); err != nil {
		t.Fatalf("expected daily data file in db folder: %v", err)
	}
}

func TestEngineSealsOldestWhenThirdDayArrives(t *testing.T) {
	e, err := OpenEngine(t.TempDir(), 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer e.Close()

	day1 := int64(24 * time.Hour)
	day2 := int64(2 * 24 * time.Hour)
	day3 := int64(3 * 24 * time.Hour)

	if err := e.AddLine("prod/sensors.temp 10 " + itoa64(day1)); err != nil {
		t.Fatalf("AddLine day1 failed: %v", err)
	}
	if err := e.AddLine("prod/sensors.temp 20 " + itoa64(day2)); err != nil {
		t.Fatalf("AddLine day2 failed: %v", err)
	}
	if err := e.AddLine("prod/sensors.temp 30 " + itoa64(day3)); err != nil {
		t.Fatalf("AddLine day3 failed: %v", err)
	}

	if err := e.AddLine("prod/sensors.temp 11 " + itoa64(day1+1)); err == nil {
		t.Fatalf("expected sealed-day rejection for day1")
	}
}

func TestEngineImportExportIdentical(t *testing.T) {
	root := filepath.Join(t.TempDir(), "devdata")
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}
	importPath := filepath.Join(root, "import.lp")
	exportPath := filepath.Join(root, "export.lp")

	base := int64(1_700_000_000_000_000_000)
	day := int64(24 * time.Hour)
	ts := []Timestamp{
		Timestamp(base + 10),
		Timestamp(base + 20),
		Timestamp(base + day + 10),
		Timestamp(base + day + 20),
		Timestamp(base + 2*day + 10),
		Timestamp(base + 2*day + 20),
	}
	lines := []string{
		"prod/sensors.room1.temp 2000 " + FormatTimestamp(ts[0]),
		"prod/sensors.room1.temp 2001 " + FormatTimestamp(ts[1]),
		"prod/sensors.room1.temp 2002 " + FormatTimestamp(ts[2]),
		"prod/sensors.room1.temp 2003 " + FormatTimestamp(ts[3]),
		"prod/sensors.room1.temp 2004 " + FormatTimestamp(ts[4]),
		"prod/sensors.room1.temp 2005 " + FormatTimestamp(ts[5]),
	}
	importBody := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(importPath, []byte(importBody), 0644); err != nil {
		t.Fatalf("WriteFile import failed: %v", err)
	}

	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	if err := e.ImportFile(importPath); err != nil {
		t.Fatalf("ImportFile failed: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	e2, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine reopen failed: %v", err)
	}
	defer e2.Close()
	if err := e2.ExportFile("prod", exportPath); err != nil {
		t.Fatalf("ExportFile failed: %v", err)
	}

	got, err := os.ReadFile(exportPath)
	if err != nil {
		t.Fatalf("ReadFile export failed: %v", err)
	}
	if string(got) != importBody {
		t.Fatalf("export mismatch\nwant:\n%s\ngot:\n%s", importBody, string(got))
	}
}

func TestGetMetricRollupDownstream_MultiHopAndTruncation(t *testing.T) {
	e, err := OpenEngine(t.TempDir(), 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer e.Close()

	if _, _, err := e.getOrCreateDB("prod"); err != nil {
		t.Fatalf("getOrCreateDB prod failed: %v", err)
	}
	if _, _, err := e.getOrCreateDB("prod_rollup_1h"); err != nil {
		t.Fatalf("getOrCreateDB prod_rollup_1h failed: %v", err)
	}
	if _, _, err := e.getOrCreateDB("prod_rollup_1d"); err != nil {
		t.Fatalf("getOrCreateDB prod_rollup_1d failed: %v", err)
	}

	_, prodRT, err := e.getOrCreateDB("prod")
	if err != nil {
		t.Fatalf("getOrCreateDB prod runtime failed: %v", err)
	}
	prodRT.info.Rollups = DBManifestRollups{
		Enabled: true,
		Jobs: []DBManifestRollupJob{{
			ID:                      "temp_1h",
			SourceMetric:            "temp.out_dry",
			Interval:                "1h",
			Aggregates:              []string{"sum"},
			DestinationDB:           "prod_rollup_1h",
			DestinationMetricPrefix: "temp.out_dry",
		}},
	}

	_, rollup1hRT, err := e.getOrCreateDB("prod_rollup_1h")
	if err != nil {
		t.Fatalf("getOrCreateDB prod_rollup_1h runtime failed: %v", err)
	}
	rollup1hRT.info.Rollups = DBManifestRollups{
		Enabled: true,
		Jobs: []DBManifestRollupJob{{
			ID:                      "temp_1d_from_1h",
			SourceMetric:            "temp.out_dry.sum",
			Interval:                "24h",
			Aggregates:              []string{"avg"},
			DestinationDB:           "prod_rollup_1d",
			DestinationMetricPrefix: "temp.out_dry",
		}},
	}

	_, rollup1dRT, err := e.getOrCreateDB("prod_rollup_1d")
	if err != nil {
		t.Fatalf("getOrCreateDB prod_rollup_1d runtime failed: %v", err)
	}
	rollup1dRT.info.Rollups = DBManifestRollups{
		Enabled: true,
		Jobs: []DBManifestRollupJob{{
			ID:                      "temp_1w_from_1d",
			SourceMetric:            "temp.out_dry.avg",
			Interval:                "168h",
			Aggregates:              []string{"max"},
			DestinationDB:           "prod_rollup_1w",
			DestinationMetricPrefix: "temp.out_dry",
		}},
	}

	steps, truncated, err := e.GetMetricRollupDownstream("prod", "temp.out_dry", 2)
	if err != nil {
		t.Fatalf("GetMetricRollupDownstream failed: %v", err)
	}
	if !truncated {
		t.Fatalf("expected truncated=true when lineage exceeds max_hops")
	}
	if len(steps) != 2 {
		t.Fatalf("steps length mismatch: got=%d want=2", len(steps))
	}
	if steps[0].Hop != 1 || steps[0].Database != "prod_rollup_1h" || steps[0].Metric != "temp.out_dry.sum" || steps[0].Aggregate != "sum" {
		t.Fatalf("unexpected hop1 step: %+v", steps[0])
	}
	if steps[1].Hop != 2 || steps[1].Database != "prod_rollup_1d" || steps[1].Metric != "temp.out_dry.avg" || steps[1].Aggregate != "avg" {
		t.Fatalf("unexpected hop2 step: %+v", steps[1])
	}
}

func TestGetMetricRollupDownstream_ValidatesInputs(t *testing.T) {
	e, err := OpenEngine(t.TempDir(), 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer e.Close()

	if _, _, err := e.getOrCreateDB("prod"); err != nil {
		t.Fatalf("getOrCreateDB prod failed: %v", err)
	}

	if _, _, err := e.GetMetricRollupDownstream("prod", "temp.out_dry", 0); err == nil {
		t.Fatalf("expected max_hops validation error")
	}
	if _, _, err := e.GetMetricRollupDownstream("", "temp.out_dry", 1); err == nil {
		t.Fatalf("expected empty database validation error")
	}
}

func itoa(v int) string {
	return strconv.Itoa(v)
}

func itoa64(v int64) string {
	return strconv.FormatInt(v, 10)
}
