package engine

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMetricFileV2HeaderRoundTrip(t *testing.T) {
	hdr := metricFileV2Header{
		Flags:             1,
		PartitionKind:     MetricPartitionDay,
		FileMinTS:         100,
		FileMaxTS:         200,
		TimeFrameCount:    3,
		MetricFrameCount:  83,
		TimeIndexOffset:   4096,
		MetricIndexOffset: 8192,
	}

	blob := hdr.EncodeBinary()
	got, err := decodeMetricFileV2Header(blob[:])
	if err != nil {
		t.Fatalf("decodeMetricFileV2Header failed: %v", err)
	}
	if got != hdr {
		t.Fatalf("metric v2 file header mismatch: got=%+v want=%+v", got, hdr)
	}

	blob[56] = 1
	if _, err := decodeMetricFileV2Header(blob[:]); err == nil {
		t.Fatal("expected reserved field validation failure")
	}
}

func TestMetricTimeFrameHeaderV2RoundTrip(t *testing.T) {
	hdr := metricTimeFrameHeaderV2{
		CodecID:      CompressionCodecZstdFastestID,
		TimeFrameID:  7,
		TimeEncoding: metricFileV2TimeEncodingRawInt64,
		StartTS:      1000,
		EndTS:        2000,
		PointCount:   321,
		PayloadLen:   1234,
		DecodedLen:   2568,
	}

	blob := hdr.EncodeBinary()
	if got := binary.LittleEndian.Uint16(blob[4:6]); got != metricFileV2FrameHeaderLen {
		t.Fatalf("header len mismatch: got=%d want=%d", got, metricFileV2FrameHeaderLen)
	}
	got, err := decodeMetricTimeFrameHeaderV2(blob[:])
	if err != nil {
		t.Fatalf("decodeMetricTimeFrameHeaderV2 failed: %v", err)
	}
	if got != hdr {
		t.Fatalf("metric v2 time frame header mismatch: got=%+v want=%+v", got, hdr)
	}

	blob[40] = 1
	if _, err := decodeMetricTimeFrameHeaderV2(blob[:]); err == nil {
		t.Fatal("expected reserved field validation failure")
	}
}

func TestMetricMetricFrameHeaderV2RoundTrip(t *testing.T) {
	hdr := metricMetricFrameHeaderV2{
		CodecID:     CompressionCodecZstdFastestID,
		MetricID:    42,
		ValueType:   Float32Sample,
		StartTS:     500,
		EndTS:       900,
		TimeFrameID: 7,
		TimeOffset:  99,
		PayloadLen:  777,
		DecodedLen:  4 * 123,
	}

	blob := hdr.EncodeBinary()
	if got := binary.LittleEndian.Uint16(blob[4:6]); got != metricFileV2FrameHeaderLen {
		t.Fatalf("header len mismatch: got=%d want=%d", got, metricFileV2FrameHeaderLen)
	}
	if got := binary.LittleEndian.Uint16(blob[28:30]); got != hdr.TimeFrameID {
		t.Fatalf("time frame id offset mismatch: got=%d want=%d", got, hdr.TimeFrameID)
	}
	if got := binary.LittleEndian.Uint32(blob[32:36]); got != hdr.TimeOffset {
		t.Fatalf("time offset mismatch: got=%d want=%d", got, hdr.TimeOffset)
	}

	got, err := decodeMetricMetricFrameHeaderV2(blob[:])
	if err != nil {
		t.Fatalf("decodeMetricMetricFrameHeaderV2 failed: %v", err)
	}
	if got != hdr {
		t.Fatalf("metric v2 metric frame header mismatch: got=%+v want=%+v", got, hdr)
	}

	points, err := got.PointCount()
	if err != nil {
		t.Fatalf("PointCount failed: %v", err)
	}
	if points != 123 {
		t.Fatalf("derived point count mismatch: got=%d want=%d", points, 123)
	}

	blob[30] = 1
	if _, err := decodeMetricMetricFrameHeaderV2(blob[:]); err == nil {
		t.Fatal("expected reserved field validation failure")
	}
}

func TestMetricValuePointCountFromDecodedLen(t *testing.T) {
	if got, err := metricValuePointCountFromDecodedLen(Int32Sample, 16); err != nil || got != 4 {
		t.Fatalf("int32 decoded len mismatch: got=%d err=%v", got, err)
	}
	if got, err := metricValuePointCountFromDecodedLen(Float32Sample, 12); err != nil || got != 3 {
		t.Fatalf("float32 decoded len mismatch: got=%d err=%v", got, err)
	}
	if _, err := metricValuePointCountFromDecodedLen(Int32Sample, 10); err == nil {
		t.Fatal("expected non-divisible decoded length failure")
	}
	if _, err := metricValuePointCountFromDecodedLen(99, 8); err == nil {
		t.Fatal("expected unsupported value type failure")
	}
}

func TestWriteMetricFileV2SharedTimeLayout(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "metric-v2.dat")
	pages := []MetricFilePageInput{
		{
			MetricID:  10,
			ValueType: Int32Sample,
			Times:     []Timestamp{100, 200, 300, 400},
			Int32:     []int32{1, 2, 3, 4},
		},
		{
			MetricID:  11,
			ValueType: Float32Sample,
			Times:     []Timestamp{100, 200, 300, 400},
			Float32:   []float32{1.5, 2.5, 3.5, 4.5},
		},
		{
			MetricID:  12,
			ValueType: Int32Sample,
			Times:     []Timestamp{300, 400},
			Int32:     []int32{30, 40},
		},
	}

	if err := WriteMetricFileV2(path, MetricPartitionDay, nil, pages); err != nil {
		t.Fatalf("WriteMetricFileV2 failed: %v", err)
	}
	if _, err := readMetricFileV2Footer(path); err != nil {
		t.Fatalf("readMetricFileV2Footer failed: %v", err)
	}
	hdr, err := readMetricFileV2Header(path)
	if err != nil {
		t.Fatalf("readMetricFileV2Header failed: %v", err)
	}
	if hdr.TimeFrameCount != 1 {
		t.Fatalf("time frame count mismatch: got=%d want=%d", hdr.TimeFrameCount, 1)
	}
	if hdr.MetricFrameCount != 3 {
		t.Fatalf("metric frame count mismatch: got=%d want=%d", hdr.MetricFrameCount, 3)
	}
	if hdr.FileMinTS != 100 || hdr.FileMaxTS != 400 {
		t.Fatalf("file timestamp range mismatch: got=[%d,%d] want=[100,400]", hdr.FileMinTS, hdr.FileMaxTS)
	}

	timeEntries, err := readMetricTimeFrameIndexEntriesV2(path)
	if err != nil {
		t.Fatalf("readMetricTimeFrameIndexEntriesV2 failed: %v", err)
	}
	if len(timeEntries) != 1 {
		t.Fatalf("time index entry count mismatch: got=%d want=%d", len(timeEntries), 1)
	}
	if timeEntries[0].PointCount != 4 {
		t.Fatalf("time index point count mismatch: got=%d want=%d", timeEntries[0].PointCount, 4)
	}

	metricEntries, err := readMetricMetricFrameIndexEntriesV2(path)
	if err != nil {
		t.Fatalf("readMetricMetricFrameIndexEntriesV2 failed: %v", err)
	}
	if len(metricEntries) != 3 {
		t.Fatalf("metric index entry count mismatch: got=%d want=%d", len(metricEntries), 3)
	}
	if metricEntries[0].MetricID != 10 || metricEntries[0].TimeFrameID != 1 || metricEntries[0].TimeOffset != 0 {
		t.Fatalf("metric 10 frame mismatch: %+v", metricEntries[0])
	}
	if metricEntries[1].MetricID != 11 || metricEntries[1].TimeFrameID != 1 || metricEntries[1].TimeOffset != 0 {
		t.Fatalf("metric 11 frame mismatch: %+v", metricEntries[1])
	}
	if metricEntries[2].MetricID != 12 || metricEntries[2].TimeFrameID != 1 || metricEntries[2].TimeOffset != 2 {
		t.Fatalf("metric 12 frame mismatch: %+v", metricEntries[2])
	}
	if points, err := metricValuePointCountFromDecodedLen(metricEntries[2].ValueType, metricEntries[2].DecodedLen); err != nil || points != 2 {
		t.Fatalf("metric 12 derived points mismatch: got=%d err=%v", points, err)
	}
	if hdr.TimeIndexOffset <= uint64(timeEntries[0].FrameOffset) {
		t.Fatalf("time index offset should be after frames: header=%d frame=%d", hdr.TimeIndexOffset, timeEntries[0].FrameOffset)
	}
	if hdr.MetricIndexOffset <= hdr.TimeIndexOffset {
		t.Fatalf("metric index offset should be after time index: metric=%d time=%d", hdr.MetricIndexOffset, hdr.TimeIndexOffset)
	}
}

func TestBuildMetricFileV2FromEngineData(t *testing.T) {
	root := t.TempDir()
	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer e.Close()

	base := Timestamp(time.Date(2026, 5, 22, 0, 0, 0, 0, time.UTC).UnixNano())
	for i := 0; i < 4; i++ {
		ts := base + Timestamp(i+1)
		if err := e.AddSample("prod", "cpu.busy_pct", ts, float32(10+i)); err != nil {
			t.Fatalf("AddSample cpu.busy_pct failed: %v", err)
		}
		if err := e.AddSample("prod", "mem.free", ts, int32(100-i)); err != nil {
			t.Fatalf("AddSample mem.free failed: %v", err)
		}
	}
	if err := e.flushDatabases([]string{"prod"}); err != nil {
		t.Fatalf("flushDatabases failed: %v", err)
	}

	partition := dayKey(base)
	metricPath, err := e.BuildMetricFileV2("prod", partition)
	if err != nil {
		t.Fatalf("BuildMetricFileV2 failed: %v", err)
	}
	if _, err := os.Stat(metricPath); err != nil {
		t.Fatalf("metric file missing: %v", err)
	}

	hdr, err := readMetricFileV2Header(metricPath)
	if err != nil {
		t.Fatalf("readMetricFileV2Header failed: %v", err)
	}
	if hdr.TimeFrameCount != 1 {
		t.Fatalf("time frame count mismatch: got=%d want=%d", hdr.TimeFrameCount, 1)
	}
	if hdr.MetricFrameCount != 2 {
		t.Fatalf("metric frame count mismatch: got=%d want=%d", hdr.MetricFrameCount, 2)
	}
	metricEntries, err := readMetricMetricFrameIndexEntriesV2(metricPath)
	if err != nil {
		t.Fatalf("readMetricMetricFrameIndexEntriesV2 failed: %v", err)
	}
	if len(metricEntries) != 2 {
		t.Fatalf("metric entry count mismatch: got=%d want=%d", len(metricEntries), 2)
	}
	for _, entry := range metricEntries {
		if entry.TimeFrameID != 1 {
			t.Fatalf("expected shared time frame id 1, got=%d", entry.TimeFrameID)
		}
		if entry.TimeOffset != 0 {
			t.Fatalf("expected zero time offset, got=%d", entry.TimeOffset)
		}
		points, err := metricValuePointCountFromDecodedLen(entry.ValueType, entry.DecodedLen)
		if err != nil {
			t.Fatalf("metricValuePointCountFromDecodedLen failed: %v", err)
		}
		if points != 4 {
			t.Fatalf("derived point count mismatch: got=%d want=%d", points, 4)
		}
	}
}

func TestBuildMetricFileV2SortsOutOfOrderRawPartitionSamples(t *testing.T) {
	root := t.TempDir()
	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer e.Close()

	db, _, err := e.getOrCreateDB("prod")
	if err != nil {
		t.Fatalf("getOrCreateDB failed: %v", err)
	}
	metricID, err := GetMetricID[float32](db.catalog, "cpu.temp")
	if err != nil {
		t.Fatalf("GetMetricID failed: %v", err)
	}

	base := Timestamp(time.Date(2026, 5, 24, 0, 0, 0, 0, time.UTC).UnixNano())
	partition := dayKey(base)

	newer := NewPage(base + 200)
	var newerRaw [4]byte
	binary.LittleEndian.PutUint32(newerRaw[:], math.Float32bits(42.5))
	if err := newer.AddSample(metricID, base+200, newerRaw[:]); err != nil {
		t.Fatalf("newer.AddSample failed: %v", err)
	}
	if err := writePage(db, partition, newer); err != nil {
		t.Fatalf("writePage newer failed: %v", err)
	}

	older := NewPage(base + 100)
	var olderRaw [4]byte
	binary.LittleEndian.PutUint32(olderRaw[:], math.Float32bits(41.5))
	if err := older.AddSample(metricID, base+100, olderRaw[:]); err != nil {
		t.Fatalf("older.AddSample failed: %v", err)
	}
	if err := writePage(db, partition, older); err != nil {
		t.Fatalf("writePage older failed: %v", err)
	}

	metricPath, err := e.BuildMetricFileV2("prod", partition)
	if err != nil {
		t.Fatalf("BuildMetricFileV2 failed: %v", err)
	}
	if err := e.CompareDataAndMetricPartitionV2("prod", partition); err != nil {
		t.Fatalf("CompareDataAndMetricPartitionV2 failed: %v", err)
	}

	entry, ok := db.catalog.GetMetricEntry("cpu.temp")
	if !ok {
		t.Fatal("cpu.temp missing from catalog")
	}
	count := 0
	var got []Sample
	if err := collectMetricFromMetricFile("prod", "cpu.temp", entry, metricPath, base, base+500, 1, &count, func(s Sample) error {
		got = append(got, s)
		return nil
	}); err != nil {
		t.Fatalf("collectMetricFromMetricFile failed: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("sample count mismatch: got=%d want=2", len(got))
	}
	if got[0].TS != base+100 || got[1].TS != base+200 {
		t.Fatalf("timestamps not sorted: got=[%d,%d] want=[%d,%d]", got[0].TS, got[1].TS, base+100, base+200)
	}
}

func TestCollectMetricFromMetricFileV2MatchesRawPartition(t *testing.T) {
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
	if _, err := e.BuildMetricFileV2("prod", partition); err != nil {
		t.Fatalf("BuildMetricFileV2 failed: %v", err)
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

func TestQueryRangePrefersMetricFileV2(t *testing.T) {
	root := t.TempDir()
	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer e.Close()

	base := Timestamp(time.Date(2023, 11, 14, 0, 0, 0, 0, time.UTC).UnixNano())
	for i := 0; i < 6; i++ {
		ts := base + Timestamp(i*10)
		if err := e.AddSample("prod", "cpu.temp", ts, float32(40+i)); err != nil {
			t.Fatalf("AddSample cpu.temp failed: %v", err)
		}
		if err := e.AddSample("prod", "cpu.idle", ts, int32(80-i)); err != nil {
			t.Fatalf("AddSample cpu.idle failed: %v", err)
		}
	}
	if err := e.flushDatabases([]string{"prod"}); err != nil {
		t.Fatalf("flushDatabases failed: %v", err)
	}

	partition := dayKey(base)
	if _, err := e.BuildMetricFileV2("prod", partition); err != nil {
		t.Fatalf("BuildMetricFileV2 failed: %v", err)
	}
	dataPath := filepath.Join(root, "prod", "data-"+partition+".dat")
	if err := os.Remove(dataPath); err != nil {
		t.Fatalf("Remove raw data file failed: %v", err)
	}

	var got []Sample
	if err := e.QueryRange("prod", "cpu.temp", base, base+100, 1, func(s Sample) error {
		got = append(got, s)
		return nil
	}); err != nil {
		t.Fatalf("QueryRange failed: %v", err)
	}
	if len(got) != 6 {
		t.Fatalf("unexpected sample count: got=%d want=%d", len(got), 6)
	}
	for i, sample := range got {
		if sample.TS != base+Timestamp(i*10) {
			t.Fatalf("sample %d timestamp mismatch: got=%d want=%d", i, sample.TS, base+Timestamp(i*10))
		}
	}
}

func TestCompareDataAndMetricPartitionV2(t *testing.T) {
	root := t.TempDir()
	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer e.Close()

	base := Timestamp(time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC).UnixNano())
	for i := 0; i < 5; i++ {
		ts := base + Timestamp(i*15)
		if err := e.AddSample("prod", "cpu.temp", ts, float32(50+i)); err != nil {
			t.Fatalf("AddSample cpu.temp failed: %v", err)
		}
		if err := e.AddSample("prod", "mem.free", ts, int32(100-i)); err != nil {
			t.Fatalf("AddSample mem.free failed: %v", err)
		}
	}
	if err := e.flushDatabases([]string{"prod"}); err != nil {
		t.Fatalf("flushDatabases failed: %v", err)
	}

	partition := dayKey(base)
	if _, err := e.BuildMetricFileV2("prod", partition); err != nil {
		t.Fatalf("BuildMetricFileV2 failed: %v", err)
	}
	if err := e.CompareDataAndMetricPartitionV2("prod", partition); err != nil {
		t.Fatalf("CompareDataAndMetricPartitionV2 failed: %v", err)
	}
	if err := e.CompareDataAndMetricPartition("prod", partition); err != nil {
		t.Fatalf("CompareDataAndMetricPartition failed: %v", err)
	}
}

func TestCaptureRuntimeStatsIncludesMetricTimeCacheMetrics(t *testing.T) {
	root := t.TempDir()
	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer e.Close()

	before := metricTimeFrameCacheStatsSnapshotV2()

	base := Timestamp(time.Date(2024, 1, 3, 0, 0, 0, 0, time.UTC).UnixNano())
	for i := 0; i < 4; i++ {
		ts := base + Timestamp(i*20)
		if err := e.AddSample("prod", "cpu.temp", ts, float32(60+i)); err != nil {
			t.Fatalf("AddSample failed: %v", err)
		}
	}
	if err := e.flushDatabases([]string{"prod"}); err != nil {
		t.Fatalf("flushDatabases failed: %v", err)
	}

	partition := dayKey(base)
	metricPath, err := e.BuildMetricFileV2("prod", partition)
	if err != nil {
		t.Fatalf("BuildMetricFileV2 failed: %v", err)
	}

	db, _, err := e.getOrCreateDB("prod")
	if err != nil {
		t.Fatalf("getOrCreateDB failed: %v", err)
	}
	entry, ok := db.catalog.GetMetricEntry("cpu.temp")
	if !ok {
		t.Fatal("cpu.temp missing from catalog")
	}

	count := 0
	for i := 0; i < 2; i++ {
		count = 0
		if err := collectMetricFromMetricFile("prod", "cpu.temp", entry, metricPath, base, base+100, 1, &count, func(Sample) error {
			return nil
		}); err != nil {
			t.Fatalf("collectMetricFromMetricFile run %d failed: %v", i, err)
		}
	}

	e.captureRuntimeStats()
	after := metricTimeFrameCacheStatsSnapshotV2()
	stats := e.stats.snapshot()
	if after.Misses <= before.Misses {
		t.Fatalf("expected cache misses to increase: before=%d after=%d", before.Misses, after.Misses)
	}
	if after.Hits <= before.Hits {
		t.Fatalf("expected cache hits to increase: before=%d after=%d", before.Hits, after.Hits)
	}
	for _, key := range []string{
		"metric_file/time_cache_entries",
		"metric_file/time_cache_bytes",
		"metric_file/time_cache_max_entries",
		"metric_file/time_cache_hits",
		"metric_file/time_cache_misses",
		"metric_file/time_cache_evictions",
	} {
		if _, ok := stats[key]; !ok {
			t.Fatalf("expected runtime stats key %q", key)
		}
	}
}
