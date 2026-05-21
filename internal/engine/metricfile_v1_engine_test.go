package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBuildAndCompareMetricFileV1(t *testing.T) {
	root := t.TempDir()
	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer e.Close()

	base := Timestamp(time.Date(2023, 11, 14, 0, 0, 0, 0, time.UTC).UnixNano())
	// Batch 1
	if err := e.AddSample("prod", "cpu.temp", base+1, float32(41.25)); err != nil {
		t.Fatalf("AddSample batch1 failed: %v", err)
	}
	if err := e.AddSample("prod", "cpu.idle", base+2, int32(80)); err != nil {
		t.Fatalf("AddSample batch1 failed: %v", err)
	}
	if err := e.AddSample("prod", "cpu.temp", base+3, float32(41.5)); err != nil {
		t.Fatalf("AddSample batch1 failed: %v", err)
	}
	if err := e.flushDatabases([]string{"prod"}); err != nil {
		t.Fatalf("flushDatabases batch1 failed: %v", err)
	}

	// Batch 2 (same day, second persisted frame)
	if err := e.AddSample("prod", "cpu.idle", base+4, int32(82)); err != nil {
		t.Fatalf("AddSample batch2 failed: %v", err)
	}
	if err := e.AddSample("prod", "cpu.temp", base+5, float32(42.0)); err != nil {
		t.Fatalf("AddSample batch2 failed: %v", err)
	}
	if err := e.AddSample("prod", "cpu.idle", base+6, int32(83)); err != nil {
		t.Fatalf("AddSample batch2 failed: %v", err)
	}
	if err := e.flushDatabases([]string{"prod"}); err != nil {
		t.Fatalf("flushDatabases batch2 failed: %v", err)
	}

	partition := dayKey(base)
	metricPath, err := e.BuildMetricFileV1("prod", partition)
	if err != nil {
		t.Fatalf("BuildMetricFileV1 failed: %v", err)
	}

	dataPath := filepath.Join(root, "prod", "data-"+partition+".dat")
	if _, err := os.Stat(dataPath); err != nil {
		t.Fatalf("expected data partition file: %v", err)
	}
	if _, err := os.Stat(metricPath); err != nil {
		t.Fatalf("expected metric partition file: %v", err)
	}

	pages, err := ReadMetricFileV1(metricPath)
	if err != nil {
		t.Fatalf("ReadMetricFileV1 failed: %v", err)
	}
	if len(pages) != 2 {
		t.Fatalf("expected one metric frame per metric, got %d", len(pages))
	}
	if pages[0].MetricID == pages[1].MetricID {
		t.Fatalf("expected distinct metrics in merged frames, got duplicate metric id %d", pages[0].MetricID)
	}
	if pages[0].PointCount != 3 || pages[1].PointCount != 3 {
		t.Fatalf("expected merged frames with 3 points each, got %d and %d", pages[0].PointCount, pages[1].PointCount)
	}

	if err := e.CompareDataAndMetricPartitionV1("prod", partition); err != nil {
		t.Fatalf("CompareDataAndMetricPartitionV1 failed: %v", err)
	}
}

func TestCollectMetricFromMetricFileMatchesRawPartition(t *testing.T) {
	root := t.TempDir()
	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer e.Close()

	base := Timestamp(time.Date(2023, 11, 14, 0, 0, 0, 0, time.UTC).UnixNano())
	for batch := 0; batch < 4; batch++ {
		for i := 0; i < 24; i++ {
			ts := base + Timestamp(batch*1_000+i*3)
			if err := e.AddSample("prod", "cpu.temp", ts, float32(30+i)); err != nil {
				t.Fatalf("AddSample cpu.temp failed: %v", err)
			}
			if err := e.AddSample("prod", "cpu.idle", ts+1, int32(70+i)); err != nil {
				t.Fatalf("AddSample cpu.idle failed: %v", err)
			}
			if err := e.AddSample("prod", "cpu.user", ts+2, int32(20+i)); err != nil {
				t.Fatalf("AddSample cpu.user failed: %v", err)
			}
		}
		if err := e.flushDatabases([]string{"prod"}); err != nil {
			t.Fatalf("flushDatabases batch %d failed: %v", batch, err)
		}
	}

	partition := dayKey(base)
	if _, err := e.BuildMetricFileV1("prod", partition); err != nil {
		t.Fatalf("BuildMetricFileV1 failed: %v", err)
	}

	db, _, err := e.getOrCreateDB("prod")
	if err != nil {
		t.Fatalf("getOrCreateDB failed: %v", err)
	}
	entry, ok := db.catalog.GetMetricEntry("cpu.temp")
	if !ok {
		t.Fatal("cpu.temp missing from catalog")
	}

	dataPath := filepath.Join(root, "prod", "data-"+partition+".dat")
	metricPath := filepath.Join(root, "prod", "metric-"+partition+".dat")
	fromTS := base
	toTS := base + 10_000

	var rawSamples []Sample
	rawCount := 0
	if err := collectMetricFromFile("prod", "cpu.temp", entry, dataPath, fromTS, toTS, 1, &rawCount, func(s Sample) error {
		rawSamples = append(rawSamples, s)
		return nil
	}); err != nil {
		t.Fatalf("collectMetricFromFile failed: %v", err)
	}

	var metricSamples []Sample
	metricCount := 0
	if err := collectMetricFromMetricFile("prod", "cpu.temp", entry, metricPath, fromTS, toTS, 1, &metricCount, func(s Sample) error {
		metricSamples = append(metricSamples, s)
		return nil
	}); err != nil {
		t.Fatalf("collectMetricFromMetricFile failed: %v", err)
	}

	if len(rawSamples) != len(metricSamples) {
		t.Fatalf("sample count mismatch: raw=%d metric=%d", len(rawSamples), len(metricSamples))
	}
	for i := range rawSamples {
		if rawSamples[i] != metricSamples[i] {
			t.Fatalf("sample %d mismatch: raw=%+v metric=%+v", i, rawSamples[i], metricSamples[i])
		}
	}
	if rawCount != metricCount {
		t.Fatalf("count mismatch: raw=%d metric=%d", rawCount, metricCount)
	}
}

func TestBuildMetricFileV1WithConfiguredCodecs(t *testing.T) {
	codecs := []string{
		CompressionCodecS2Name,
		CompressionCodecS2BetterName,
		CompressionCodecZstdFastestName,
		CompressionCodecZstdDefaultName,
	}

	for _, codecName := range codecs {
		codecName := codecName
		t.Run(codecName, func(t *testing.T) {
			root := t.TempDir()
			cfg := []byte(fmt.Sprintf("[metrics]\ncompression = %q\n", codecName))
			if err := os.WriteFile(filepath.Join(root, "engine.toml"), cfg, 0644); err != nil {
				t.Fatalf("write engine.toml failed: %v", err)
			}

			e, err := OpenEngine(root, 1024*1024)
			if err != nil {
				t.Fatalf("OpenEngine failed: %v", err)
			}
			defer e.Close()

			base := Timestamp(time.Date(2023, 11, 14, 0, 0, 0, 0, time.UTC).UnixNano())
			if err := e.AddSample("prod", "cpu.temp", base+1, float32(41.25)); err != nil {
				t.Fatalf("AddSample cpu.temp failed: %v", err)
			}
			if err := e.AddSample("prod", "cpu.idle", base+2, int32(80)); err != nil {
				t.Fatalf("AddSample cpu.idle failed: %v", err)
			}
			if err := e.AddSample("prod", "cpu.temp", base+3, float32(41.5)); err != nil {
				t.Fatalf("AddSample cpu.temp failed: %v", err)
			}
			if err := e.flushDatabases([]string{"prod"}); err != nil {
				t.Fatalf("flushDatabases failed: %v", err)
			}

			partition := dayKey(base)
			metricPath, err := e.BuildMetricFileV1("prod", partition)
			if err != nil {
				t.Fatalf("BuildMetricFileV1 failed: %v", err)
			}

			pages, err := ReadMetricFileV1(metricPath)
			if err != nil {
				t.Fatalf("ReadMetricFileV1 failed: %v", err)
			}
			codec, err := BlockCompressionCodecByName(codecName)
			if err != nil {
				t.Fatalf("BlockCompressionCodecByName failed: %v", err)
			}
			for _, page := range pages {
				if page.CodecID != codec.ID() {
					t.Fatalf("codec id mismatch: got=%d want=%d", page.CodecID, codec.ID())
				}
			}
			if err := e.CompareDataAndMetricPartitionV1("prod", partition); err != nil {
				t.Fatalf("CompareDataAndMetricPartitionV1 failed: %v", err)
			}
		})
	}
}

func TestReadMetricFilePageInfosV1MatchesDecodedPages(t *testing.T) {
	root := t.TempDir()
	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer e.Close()

	base := Timestamp(time.Date(2023, 11, 14, 0, 0, 0, 0, time.UTC).UnixNano())
	if err := e.AddSample("prod", "cpu.temp", base+1, float32(41.25)); err != nil {
		t.Fatalf("AddSample cpu.temp failed: %v", err)
	}
	if err := e.AddSample("prod", "cpu.idle", base+2, int32(80)); err != nil {
		t.Fatalf("AddSample cpu.idle failed: %v", err)
	}
	if err := e.AddSample("prod", "cpu.temp", base+3, float32(41.5)); err != nil {
		t.Fatalf("AddSample cpu.temp failed: %v", err)
	}
	if err := e.flushDatabases([]string{"prod"}); err != nil {
		t.Fatalf("flushDatabases failed: %v", err)
	}

	partition := dayKey(base)
	metricPath, err := e.BuildMetricFileV1("prod", partition)
	if err != nil {
		t.Fatalf("BuildMetricFileV1 failed: %v", err)
	}

	pages, err := ReadMetricFileV1(metricPath)
	if err != nil {
		t.Fatalf("ReadMetricFileV1 failed: %v", err)
	}
	infos, err := ReadMetricFilePageInfosV1(metricPath)
	if err != nil {
		t.Fatalf("ReadMetricFilePageInfosV1 failed: %v", err)
	}
	if len(infos) != len(pages) {
		t.Fatalf("page info count mismatch: got=%d want=%d", len(infos), len(pages))
	}
	for i := range pages {
		if infos[i].Index != i {
			t.Fatalf("index mismatch at %d: got=%d want=%d", i, infos[i].Index, i)
		}
		if infos[i].MetricID != pages[i].MetricID || infos[i].ValueType != pages[i].ValueType {
			t.Fatalf("page identity mismatch at %d", i)
		}
		if infos[i].PageOffset != pages[i].PageOffset || infos[i].PointCount != pages[i].PointCount {
			t.Fatalf("page shape mismatch at %d", i)
		}
		if infos[i].PayloadLen != pages[i].PayloadLen || infos[i].UncompressedLen != pages[i].UncompressedLen {
			t.Fatalf("page length mismatch at %d", i)
		}
		if infos[i].MetricMinTS != pages[i].MetricMinTS || infos[i].MetricMaxTS != pages[i].MetricMaxTS {
			t.Fatalf("page timestamp bounds mismatch at %d", i)
		}
	}
}

func TestCoalesceMetricPageInputsPreallocatesExactMetricCapacity(t *testing.T) {
	pages := []MetricFilePageInput{
		{
			MetricID:  7,
			ValueType: Float32Sample,
			Times:     []Timestamp{10, 11},
			Float32:   []float32{1.5, 1.6},
		},
		{
			MetricID:  9,
			ValueType: Int32Sample,
			Times:     []Timestamp{12},
			Int32:     []int32{7},
		},
		{
			MetricID:  7,
			ValueType: Float32Sample,
			Times:     []Timestamp{12, 13, 14},
			Float32:   []float32{1.7, 1.8, 1.9},
		},
		{
			MetricID:  9,
			ValueType: Int32Sample,
			Times:     []Timestamp{15, 16},
			Int32:     []int32{8, 9},
		},
	}

	merged, err := coalesceMetricPageInputs(pages)
	if err != nil {
		t.Fatalf("coalesceMetricPageInputs failed: %v", err)
	}
	if len(merged) != 2 {
		t.Fatalf("expected 2 merged metrics, got=%d", len(merged))
	}

	if got, want := len(merged[0].Times), 5; got != want {
		t.Fatalf("metric 7 merged time len mismatch: got=%d want=%d", got, want)
	}
	if got, want := cap(merged[0].Times), 5; got != want {
		t.Fatalf("metric 7 merged time cap mismatch: got=%d want=%d", got, want)
	}
	if got, want := cap(merged[0].Float32), 5; got != want {
		t.Fatalf("metric 7 merged float cap mismatch: got=%d want=%d", got, want)
	}
	if got, want := len(merged[1].Times), 3; got != want {
		t.Fatalf("metric 9 merged time len mismatch: got=%d want=%d", got, want)
	}
	if got, want := cap(merged[1].Times), 3; got != want {
		t.Fatalf("metric 9 merged time cap mismatch: got=%d want=%d", got, want)
	}
	if got, want := cap(merged[1].Int32), 3; got != want {
		t.Fatalf("metric 9 merged int cap mismatch: got=%d want=%d", got, want)
	}
}

func TestBuildMetricFileV1AppliesRawIngestAction(t *testing.T) {
	actions := []struct {
		name           string
		action         string
		wantDataExists bool
		wantRawExists  bool
	}{
		{name: MetricRawIngestActionKeep, action: MetricRawIngestActionKeep, wantDataExists: true, wantRawExists: false},
		{name: MetricRawIngestActionRename, action: MetricRawIngestActionRename, wantDataExists: false, wantRawExists: true},
		{name: MetricRawIngestActionDelete, action: MetricRawIngestActionDelete, wantDataExists: false, wantRawExists: false},
	}

	for _, tc := range actions {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			cfg := []byte(fmt.Sprintf("[metrics]\nraw_ingest_action = %q\n", tc.action))
			if err := os.WriteFile(filepath.Join(root, "engine.toml"), cfg, 0644); err != nil {
				t.Fatalf("write engine.toml failed: %v", err)
			}

			e, err := OpenEngine(root, 1024*1024)
			if err != nil {
				t.Fatalf("OpenEngine failed: %v", err)
			}
			defer e.Close()

			base := Timestamp(time.Date(2023, 11, 14, 0, 0, 0, 0, time.UTC).UnixNano())
			if err := e.AddSample("prod", "cpu.temp", base+1, float32(41.25)); err != nil {
				t.Fatalf("AddSample failed: %v", err)
			}
			if err := e.flushDatabases([]string{"prod"}); err != nil {
				t.Fatalf("flushDatabases failed: %v", err)
			}

			partition := dayKey(base)
			if _, err := e.BuildMetricFileV1("prod", partition); err != nil {
				t.Fatalf("BuildMetricFileV1 failed: %v", err)
			}

			dataPath := metricRawPartitionPath(filepath.Join(root, "prod"), partition)
			rawPath := metricRenamedRawPartitionPath(filepath.Join(root, "prod"), partition)
			_, dataErr := os.Stat(dataPath)
			_, rawErr := os.Stat(rawPath)
			if tc.wantDataExists != (dataErr == nil) {
				t.Fatalf("data path existence mismatch: want=%t err=%v", tc.wantDataExists, dataErr)
			}
			if tc.wantRawExists != (rawErr == nil) {
				t.Fatalf("raw path existence mismatch: want=%t err=%v", tc.wantRawExists, rawErr)
			}
		})
	}
}

func TestQueryRangePrefersMetricPartitionsWhenEnabled(t *testing.T) {
	root := t.TempDir()
	cfg := []byte("[metrics]\nenabled = true\nraw_ingest_action = \"rename\"\n")
	if err := os.WriteFile(filepath.Join(root, "engine.toml"), cfg, 0644); err != nil {
		t.Fatalf("write engine.toml failed: %v", err)
	}

	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer e.Close()

	base := Timestamp(time.Date(2023, 11, 14, 0, 0, 0, 0, time.UTC).UnixNano())
	for i := 0; i < 4; i++ {
		ts := base + Timestamp(i*10)
		if err := e.AddSample("prod", "cpu.temp", ts, float32(40+i)); err != nil {
			t.Fatalf("AddSample cpu.temp failed: %v", err)
		}
		if err := e.AddSample("prod", "cpu.idle", ts+1, int32(80+i)); err != nil {
			t.Fatalf("AddSample cpu.idle failed: %v", err)
		}
	}
	if err := e.flushDatabases([]string{"prod"}); err != nil {
		t.Fatalf("flushDatabases failed: %v", err)
	}

	partition := dayKey(base)
	if _, err := e.BuildMetricFileV1("prod", partition); err != nil {
		t.Fatalf("BuildMetricFileV1 failed: %v", err)
	}

	dataPath := metricRawPartitionPath(filepath.Join(root, "prod"), partition)
	if _, err := os.Stat(dataPath); !os.IsNotExist(err) {
		t.Fatalf("expected active raw data path to be absent after rename, got err=%v", err)
	}

	var got []Sample
	if err := e.QueryRange("prod", "cpu.temp", base, base+100, 1, func(s Sample) error {
		got = append(got, s)
		return nil
	}); err != nil {
		t.Fatalf("QueryRange failed: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("unexpected sample count: got=%d want=4", len(got))
	}
	for i, sample := range got {
		wantTS := base + Timestamp(i*10)
		if sample.TS != wantTS {
			t.Fatalf("sample %d timestamp mismatch: got=%d want=%d", i, sample.TS, wantTS)
		}
	}
}

func BenchmarkCollectMetricFromSinglePartition(b *testing.B) {
	root := b.TempDir()
	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		b.Fatalf("OpenEngine failed: %v", err)
	}
	defer e.Close()

	const (
		metricCount      = 96
		pointsPerMetric  = 4096
		flushEveryPoints = 256
		intervalNS       = 1_000_000
	)
	base := Timestamp(time.Date(2023, 11, 14, 0, 0, 0, 0, time.UTC).UnixNano())
	metricNames := make([]string, metricCount)
	for metricIdx := 0; metricIdx < metricCount; metricIdx++ {
		metricNames[metricIdx] = fmt.Sprintf("bench.metric.%03d", metricIdx)
	}
	for start := 0; start < pointsPerMetric; start += flushEveryPoints {
		end := start + flushEveryPoints
		for pointIdx := start; pointIdx < end; pointIdx++ {
			pointBase := base + Timestamp(pointIdx*intervalNS)
			for metricIdx, name := range metricNames {
				ts := pointBase + Timestamp(metricIdx)
				if err := e.AddSample("prod", name, ts, float32(metricIdx+pointIdx)); err != nil {
					b.Fatalf("AddSample failed: %v", err)
				}
			}
		}
		if err := e.flushDatabases([]string{"prod"}); err != nil {
			b.Fatalf("flushDatabases failed: %v", err)
		}
	}

	partition := dayKey(base)
	if _, err := e.BuildMetricFileV1("prod", partition); err != nil {
		b.Fatalf("BuildMetricFileV1 failed: %v", err)
	}

	db, _, err := e.getOrCreateDB("prod")
	if err != nil {
		b.Fatalf("getOrCreateDB failed: %v", err)
	}
	targetMetric := metricNames[metricCount/2]
	entry, ok := db.catalog.GetMetricEntry(targetMetric)
	if !ok {
		b.Fatalf("metric missing from catalog: %s", targetMetric)
	}

	dataPath := filepath.Join(root, "prod", "data-"+partition+".dat")
	metricPath := filepath.Join(root, "prod", "metric-"+partition+".dat")
	dataStat, err := os.Stat(dataPath)
	if err != nil {
		b.Fatalf("stat data path failed: %v", err)
	}
	metricStat, err := os.Stat(metricPath)
	if err != nil {
		b.Fatalf("stat metric path failed: %v", err)
	}
	b.ReportMetric(float64(dataStat.Size()), "raw_partition_B")
	b.ReportMetric(float64(metricStat.Size()), "metric_partition_B")

	fromTS := base
	toTS := base + Timestamp((pointsPerMetric-1)*intervalNS+metricCount)
	callback := func(Sample) error { return nil }

	b.Run("raw_partition_scan", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			count := 0
			if err := collectMetricFromFile("prod", targetMetric, entry, dataPath, fromTS, toTS, 1, &count, callback); err != nil {
				b.Fatalf("collectMetricFromFile failed: %v", err)
			}
			if count != pointsPerMetric {
				b.Fatalf("unexpected raw count: got %d want %d", count, pointsPerMetric)
			}
		}
	})

	b.Run("metric_partition_lookup", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			count := 0
			if err := collectMetricFromMetricFile("prod", targetMetric, entry, metricPath, fromTS, toTS, 1, &count, callback); err != nil {
				b.Fatalf("collectMetricFromMetricFile failed: %v", err)
			}
			if count != pointsPerMetric {
				b.Fatalf("unexpected metric count: got %d want %d", count, pointsPerMetric)
			}
		}
	})
}
