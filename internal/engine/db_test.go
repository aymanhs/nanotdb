package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestAddSampleMultiMetricPageFill tests that AddSample correctly:
// 1. Auto-creates metrics in catalog for unknown names
// 2. Stores raw bytes in page with proper metric/time association
// 3. Triggers IsFull when record count threshold is hit
// 4. Allows catalog lookup of metric types
func TestAddSampleMultiMetricPageFill(t *testing.T) {
	db, err := NewDatabase(filepath.Join(t.TempDir(), "test-db"), 1024*1024)
	if err != nil {
		t.Fatalf("NewDatabase failed: %v", err)
	}
	defer db.Close()

	// Add samples for 3 different metrics, mixed types
	type sampleSpec struct {
		name  string
		ts    Timestamp
		value interface{} // int32 or float32
	}
	samples := []sampleSpec{
		{"temp_celsius", 1000, int32(2500)},       // 25.00°C in millidegrees
		{"humidity_percent", 1000, int32(6500)},   // 65.00% in hundredths
		{"temp_celsius", 1005, int32(2510)},       // 25.10°C
		{"humidity_percent", 1005, int32(6480)},   // 64.80%
		{"pressure_mbar", 1005, float32(1013.25)}, // new metric, float type
	}

	for _, s := range samples {
		switch v := s.value.(type) {
		case int32:
			if err := addSampleToDB(db, s.name, s.ts, v); err != nil {
				t.Fatalf("AddSample(%s, %d, %d) failed: %v", s.name, s.ts, v, err)
			}
		case float32:
			if err := addSampleToDB(db, s.name, s.ts, v); err != nil {
				t.Fatalf("AddSample(%s, %d, %f) failed: %v", s.name, s.ts, v, err)
			}
		}
	}

	// Verify page state
	if db.page == nil {
		t.Fatal("page is nil after AddSample calls")
	}
	if len(db.page.Metrics) != 5 {
		t.Fatalf("expected 5 samples in page, got %d", len(db.page.Metrics))
	}
	if len(db.page.Times) != 5 {
		t.Fatalf("expected 5 timestamps, got %d", len(db.page.Times))
	}

	// Verify metrics were created in catalog
	if len(db.catalog.metrics) != 3 {
		t.Fatalf("expected 3 metrics in catalog, got %d", len(db.catalog.metrics))
	}

	// Verify we can look up metric types
	tempID, _ := GetMetricID[int32](db.catalog, "temp_celsius")
	tempType, err := db.catalog.GetMetricType(tempID)
	if err != nil {
		t.Fatalf("GetMetricType failed: %v", err)
	}
	if tempType != Int32Sample {
		t.Fatalf("expected Int32Sample, got %d", tempType)
	}

	pressureID, _ := GetMetricID[float32](db.catalog, "pressure_mbar")
	pressureType, err := db.catalog.GetMetricType(pressureID)
	if err != nil {
		t.Fatalf("GetMetricType failed: %v", err)
	}
	if pressureType != Float32Sample {
		t.Fatalf("expected Float32Sample, got %d", pressureType)
	}

	// Verify value bytes are correctly stored
	if db.page.Values.Len() != 5*4 {
		t.Fatalf("expected 20 bytes in values buffer, got %d", db.page.Values.Len())
	}

	// Verify value widths
	if db.catalog.GetValueWidth(tempID) != 4 {
		t.Fatalf("expected GetValueWidth to return 4")
	}
}

// TestAddSamplePageFillTrigger tests that IsFull triggers when record count exceeds threshold
func TestAddSamplePageFillTrigger(t *testing.T) {
	db, err := NewDatabase(filepath.Join(t.TempDir(), "test-db-fill"), 1024*1024)
	if err != nil {
		t.Fatalf("NewDatabase failed: %v", err)
	}
	defer db.Close()

	// Add PageMaxRecords samples; page should not be full
	for i := 0; i < PageMaxRecords-1; i++ {
		ts := Timestamp(1000 + i)
		if err := addSampleToDB(db, "metric1", ts, int32(i)); err != nil {
			t.Fatalf("AddSample at i=%d failed: %v", i, err)
		}
	}

	if db.page == nil {
		t.Fatal("page is nil")
	}
	if db.page.IsFull() {
		t.Fatal("page should not be full at PageMaxRecords-1")
	}

	// Add one more; page should now be full
	if err := addSampleToDB(db, "metric1", Timestamp(1000+PageMaxRecords-1), int32(999)); err != nil {
		t.Fatalf("AddSample at max failed: %v", err)
	}

	if !db.page.IsFull() {
		t.Fatal("page should be full at PageMaxRecords")
	}
}

// TestCatalogAutoCreateRejectsTooMany tests that GetMetricID rejects when too many metrics
func TestCatalogAutoCreateRejectsTooMany(t *testing.T) {
	db, err := NewDatabase(filepath.Join(t.TempDir(), "test-db-limit"), 1024*1024)
	if err != nil {
		t.Fatalf("NewDatabase failed: %v", err)
	}
	defer db.Close()

	// Try to add more than MaxMetricsPerDatabase
	for i := 0; i < MaxMetricsPerDatabase+1; i++ {
		metricName := fmt.Sprintf("metric_%d", i)
		_, err := GetMetricID[int32](db.catalog, metricName)
		if i < MaxMetricsPerDatabase {
			if err != nil {
				t.Fatalf("GetMetricID(%s) at i=%d should succeed, got: %v", metricName, i, err)
			}
		} else {
			if err == nil {
				t.Fatalf("GetMetricID(%s) at i=%d should fail (too many), but succeeded", metricName, i)
			}
		}
	}
}

// TestAddSamplePageRoundTrip tests encode/decode of a page with multi-metric samples
func TestAddSamplePageRoundTrip(t *testing.T) {
	db, err := NewDatabase(filepath.Join(t.TempDir(), "test-db-roundtrip"), 1024*1024)
	if err != nil {
		t.Fatalf("NewDatabase failed: %v", err)
	}
	defer db.Close()

	// Add samples across multiple metrics
	samples := [][3]interface{}{
		{"temp", Timestamp(1000), int32(2500)},
		{"humidity", Timestamp(1000), int32(6500)},
		{"temp", Timestamp(1005), int32(2510)},
		{"humidity", Timestamp(1005), int32(6480)},
	}

	for _, s := range samples {
		name := s[0].(string)
		ts := s[1].(Timestamp)
		value := s[2].(int32)
		if err := addSampleToDB(db, name, ts, value); err != nil {
			t.Fatalf("AddSample failed: %v", err)
		}
	}

	// Encode the page
	buf := bytes.NewBuffer(make([]byte, 0, 4096))
	if err := db.page.EncodeInto(buf); err != nil {
		t.Fatalf("EncodeInto failed: %v", err)
	}

	// Decode into a fresh page
	var decoded Page
	if err := decoded.DecodeFrom(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("DecodeFrom failed: %v", err)
	}

	// Verify metrics, times, and values match
	if len(decoded.Metrics) != len(db.page.Metrics) {
		t.Fatalf("metrics length mismatch: %d vs %d", len(decoded.Metrics), len(db.page.Metrics))
	}
	if len(decoded.Times) != len(db.page.Times) {
		t.Fatalf("times length mismatch: %d vs %d", len(decoded.Times), len(db.page.Times))
	}
	if !bytes.Equal(decoded.Values.Bytes(), db.page.Values.Bytes()) {
		t.Fatalf("values buffer mismatch")
	}

	// Verify that we can look up the decoded MetricID values in the original catalog
	for i := 0; i < len(decoded.Metrics); i++ {
		mid := decoded.Metrics[i]
		mtype, err := db.catalog.GetMetricType(mid)
		if err != nil {
			t.Fatalf("GetMetricType(%d) failed: %v", mid, err)
		}
		if mtype != Int32Sample {
			t.Fatalf("metric %d type mismatch: expected Int32Sample, got %d", mid, mtype)
		}
	}
}

func TestCatalogDirtyAndSafeJSONWrite(t *testing.T) {
	base := filepath.Join(t.TempDir(), "nanotdb")
	db, err := NewDatabase(base, 1024*1024)
	if err != nil {
		t.Fatalf("NewDatabase failed: %v", err)
	}
	defer db.Close()

	if err := addSampleToDB(db, "temp", Timestamp(1000), int32(2500)); err != nil {
		t.Fatalf("AddSample failed: %v", err)
	}
	if !db.catalog.IsDirty() {
		t.Fatalf("catalog should be dirty after creating a new metric")
	}

	if err := writePage(db, dayKey(db.page.Start), db.page); err != nil {
		t.Fatalf("writePage failed: %v", err)
	}
	if db.catalog.IsDirty() {
		t.Fatalf("catalog should be clean after WriteCatalog")
	}

	catPath := filepath.Join(filepath.Dir(base), "catalog.json")
	raw, err := os.ReadFile(catPath)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", catPath, err)
	}

	var got struct {
		Metrics []struct {
			Name string   `json:"name"`
			ID   MetricID `json:"id"`
			Typ  byte     `json:"type"`
		} `json:"metrics"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("catalog json unmarshal: %v", err)
	}
	if len(got.Metrics) != 1 {
		t.Fatalf("catalog metrics length mismatch: got=%d want=1", len(got.Metrics))
	}
	if got.Metrics[0].Name != "temp" {
		t.Fatalf("metric name mismatch: got=%q want=temp", got.Metrics[0].Name)
	}
	if got.Metrics[0].ID != 1 {
		t.Fatalf("MetricID mismatch: got=%d want=1", got.Metrics[0].ID)
	}
	if got.Metrics[0].Typ != Int32Sample {
		t.Fatalf("metric type mismatch: got=%d want=%d", got.Metrics[0].Typ, Int32Sample)
	}

	tmpPath := catPath + ".tmp"
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Fatalf("catalog temp file should not remain after safe rename")
	}
}

func TestCatalogDirtyOnlyForNewMetrics(t *testing.T) {
	base := filepath.Join(t.TempDir(), "nanotdb")
	db, err := NewDatabase(base, 1024*1024)
	if err != nil {
		t.Fatalf("NewDatabase failed: %v", err)
	}
	defer db.Close()

	if err := addSampleToDB(db, "temp", Timestamp(1000), int32(2500)); err != nil {
		t.Fatalf("AddSample temp failed: %v", err)
	}
	if !db.catalog.IsDirty() {
		t.Fatalf("catalog should be dirty after first metric")
	}

	if err := writePage(db, dayKey(db.page.Start), db.page); err != nil {
		t.Fatalf("writePage failed: %v", err)
	}
	if db.catalog.IsDirty() {
		t.Fatalf("catalog should be clean after write")
	}

	if err := addSampleToDB(db, "temp", Timestamp(1001), int32(2501)); err != nil {
		t.Fatalf("AddSample temp existing metric failed: %v", err)
	}
	if db.catalog.IsDirty() {
		t.Fatalf("catalog should stay clean for existing metrics")
	}

	if err := addSampleToDB(db, "humidity", Timestamp(1002), int32(6500)); err != nil {
		t.Fatalf("AddSample humidity failed: %v", err)
	}
	if !db.catalog.IsDirty() {
		t.Fatalf("catalog should be dirty after adding a new metric")
	}
}

func TestDatabaseRootDataFolderPaths(t *testing.T) {
	root := filepath.Join(t.TempDir(), "data")
	name := filepath.Join(root, "site-a")

	db, err := NewDatabase(name, 1024*1024)
	if err != nil {
		t.Fatalf("NewDatabase failed: %v", err)
	}
	defer db.Close()

	if db.RootDataDir != root {
		t.Fatalf("RootDataDir mismatch: got=%q want=%q", db.RootDataDir, root)
	}
	if db.Name != "site-a" {
		t.Fatalf("Name mismatch: got=%q want=%q", db.Name, "site-a")
	}

	// Trigger a new metric and grouped write so catalog file is flushed.
	if err := addSampleToDB(db, "temp", Timestamp(1000), int32(2500)); err != nil {
		t.Fatalf("AddSample failed: %v", err)
	}
	if err := writePage(db, dayKey(db.page.Start), db.page); err != nil {
		t.Fatalf("writePage failed: %v", err)
	}

	dayPath := filepath.Join(root, "data-1970-01-01.dat")
	if _, err := os.Stat(dayPath); err != nil {
		t.Fatalf("expected daily data file at %s: %v", dayPath, err)
	}

	catPath := filepath.Join(root, "catalog.json")
	if _, err := os.Stat(catPath); err != nil {
		t.Fatalf("expected catalog file at %s: %v", catPath, err)
	}
}
