package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func BenchmarkMetricPageCoalesceAllocations(b *testing.B) {
	const (
		metricCount    = 128
		pagesPerMetric = 24
		pointsPerPage  = 256
	)
	pages := buildBenchmarkMetricPageInputs(metricCount, pagesPerMetric, pointsPerPage)
	totalPoints := 0
	for _, page := range pages {
		totalPoints += len(page.Times)
	}
	b.ReportMetric(float64(len(pages)), "pages")
	b.ReportMetric(float64(totalPoints), "points")

	b.Run("legacy_append_growth", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			merged, err := coalesceMetricPageInputsLegacy(pages)
			if err != nil {
				b.Fatalf("coalesceMetricPageInputsLegacy failed: %v", err)
			}
			if len(merged) != metricCount {
				b.Fatalf("unexpected merged metrics: got=%d want=%d", len(merged), metricCount)
			}
		}
	})

	b.Run("exact_capacity", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			merged, err := coalesceMetricPageInputs(pages)
			if err != nil {
				b.Fatalf("coalesceMetricPageInputs failed: %v", err)
			}
			if len(merged) != metricCount {
				b.Fatalf("unexpected merged metrics: got=%d want=%d", len(merged), metricCount)
			}
		}
	})
}

func BenchmarkMetricFrameEncodeAllocations(b *testing.B) {
	pages := buildBenchmarkMetricPageInputs(128, 24, 256)
	codec := DefaultMetricFileCompressionCodec()
	totalPoints := 0
	for _, page := range pages {
		totalPoints += len(page.Times)
	}
	b.ReportMetric(float64(len(pages)), "pages")
	b.ReportMetric(float64(totalPoints), "points")

	b.Run("fresh_buffers_per_page", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			curOffset := uint64(metricFileV1HeaderLen)
			for _, page := range pages {
				frame, info, err := encodeMetricFrame(nil, codec, page, curOffset)
				if err != nil {
					b.Fatalf("encodeMetricFrame failed: %v", err)
				}
				curOffset = info.PageOffset + uint64(len(frame))
			}
		}
	})

	b.Run("shared_build_workspace", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			workspace := metricFrameEncodeWorkspace{}
			curOffset := uint64(metricFileV1HeaderLen)
			for _, page := range pages {
				frame, info, err := encodeMetricFrame(&workspace, codec, page, curOffset)
				if err != nil {
					b.Fatalf("encodeMetricFrame failed: %v", err)
				}
				curOffset = info.PageOffset + uint64(len(frame))
			}
		}
	})
}

func BenchmarkBuildMetricFileV1(b *testing.B) {
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
	dataPath := filepath.Join(root, "prod", "data-"+partition+".dat")
	dataStat, err := os.Stat(dataPath)
	if err != nil {
		b.Fatalf("stat raw data path failed: %v", err)
	}
	b.ReportMetric(float64(dataStat.Size()), "raw_partition_B")
	b.ReportMetric(float64(metricCount), "metrics")
	b.ReportMetric(float64(pointsPerMetric), "points_per_metric")

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		metricPath, err := e.BuildMetricFileV1("prod", partition)
		if err != nil {
			b.Fatalf("BuildMetricFileV1 failed: %v", err)
		}
		if _, err := os.Stat(metricPath); err != nil {
			b.Fatalf("stat metric path failed: %v", err)
		}
	}
}

func buildBenchmarkMetricPageInputs(metricCount, pagesPerMetric, pointsPerPage int) []MetricFilePageInput {
	base := Timestamp(time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC).UnixNano())
	pages := make([]MetricFilePageInput, 0, metricCount*pagesPerMetric)
	for pageIdx := 0; pageIdx < pagesPerMetric; pageIdx++ {
		for metricIdx := 0; metricIdx < metricCount; metricIdx++ {
			metricID := MetricID(metricIdx + 1)
			start := base + Timestamp(pageIdx*pointsPerPage*1_000_000+metricIdx)
			if metricIdx%2 == 0 {
				page := MetricFilePageInput{
					MetricID:  metricID,
					ValueType: Float32Sample,
					Times:     make([]Timestamp, pointsPerPage),
					Float32:   make([]float32, pointsPerPage),
				}
				for i := 0; i < pointsPerPage; i++ {
					page.Times[i] = start + Timestamp(i*1_000_000)
					page.Float32[i] = float32(metricIdx*pagesPerMetric + i)
				}
				pages = append(pages, page)
				continue
			}

			page := MetricFilePageInput{
				MetricID:  metricID,
				ValueType: Int32Sample,
				Times:     make([]Timestamp, pointsPerPage),
				Int32:     make([]int32, pointsPerPage),
			}
			for i := 0; i < pointsPerPage; i++ {
				page.Times[i] = start + Timestamp(i*1_000_000)
				page.Int32[i] = int32(metricIdx*pagesPerMetric + i)
			}
			pages = append(pages, page)
		}
	}
	return pages
}

func coalesceMetricPageInputsLegacy(pages []MetricFilePageInput) ([]MetricFilePageInput, error) {
	if len(pages) == 0 {
		return nil, nil
	}

	byMetric := make(map[MetricID]int, len(pages))
	out := make([]MetricFilePageInput, 0, len(pages))
	for _, page := range pages {
		if page.MetricID == 0 {
			return nil, fmt.Errorf("metric id cannot be 0")
		}
		if len(page.Times) == 0 {
			return nil, fmt.Errorf("empty times for metric %d", page.MetricID)
		}

		idx, ok := byMetric[page.MetricID]
		if !ok {
			copyPage := MetricFilePageInput{
				MetricID:  page.MetricID,
				ValueType: page.ValueType,
				Times:     append([]Timestamp(nil), page.Times...),
				Int32:     append([]int32(nil), page.Int32...),
				Float32:   append([]float32(nil), page.Float32...),
			}
			byMetric[page.MetricID] = len(out)
			out = append(out, copyPage)
			continue
		}

		merged := &out[idx]
		if merged.ValueType != page.ValueType {
			return nil, fmt.Errorf("value type mismatch while merging metric %d", page.MetricID)
		}
		if merged.Times[len(merged.Times)-1] > page.Times[0] {
			return nil, fmt.Errorf("non-monotonic merge order for metric %d", page.MetricID)
		}

		merged.Times = append(merged.Times, page.Times...)
		switch page.ValueType {
		case Int32Sample:
			if len(page.Int32) != len(page.Times) || len(page.Float32) != 0 {
				return nil, fmt.Errorf("invalid int32 value vector for metric %d", page.MetricID)
			}
			merged.Int32 = append(merged.Int32, page.Int32...)
		case Float32Sample:
			if len(page.Float32) != len(page.Times) || len(page.Int32) != 0 {
				return nil, fmt.Errorf("invalid float32 value vector for metric %d", page.MetricID)
			}
			merged.Float32 = append(merged.Float32, page.Float32...)
		default:
			return nil, fmt.Errorf("unsupported value type: %d", page.ValueType)
		}
	}

	return out, nil
}
