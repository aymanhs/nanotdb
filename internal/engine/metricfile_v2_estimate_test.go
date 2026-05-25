package engine

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func TestEstimateSharedTimeMetricLayoutFromScratchPiData(t *testing.T) {
	root := filepath.Join("..", "..", ".tmp", "metric-investigation")
	enginePath := filepath.Join(root, "engine.toml")
	if _, err := os.Stat(enginePath); err != nil {
		t.Skipf("scratch Pi metric investigation data missing: %v", err)
	}

	e, err := OpenEngine(root, 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer e.Close()

	db, _, err := e.getOrCreateDB("metrics")
	if err != nil {
		t.Fatalf("getOrCreateDB failed: %v", err)
	}

	partitions := []string{"2026-05-22", "2026-05-23"}
	codec := DefaultMetricFileCompressionCodec()
	for _, partition := range partitions {
		partition := partition
		t.Run(partition, func(t *testing.T) {
			dataPath, err := resolveMetricRawPartitionPath(db.RootDataDir, partition)
			if err != nil {
				t.Fatalf("resolveMetricRawPartitionPath failed: %v", err)
			}

			pages, err := buildCoalescedMetricInputsFromDataFile(db, dataPath)
			if err != nil {
				t.Fatalf("buildCoalescedMetricPagesFromDataFile failed: %v", err)
			}
			if len(pages) == 0 {
				t.Fatal("expected metric pages")
			}

			metricPath := filepath.Join(db.RootDataDir, "metric-"+partition+".dat")
			metricInfo, err := os.Stat(metricPath)
			if err != nil {
				t.Fatalf("stat metric file failed: %v", err)
			}
			pageInfos, err := ReadMetricFilePageInfosV1(metricPath)
			if err != nil {
				t.Fatalf("ReadMetricFilePageInfosV1 failed: %v", err)
			}

			actualPayloadBytes := int64(0)
			for _, info := range pageInfos {
				actualPayloadBytes += int64(info.PayloadLen)
			}

			uniqueTimes := map[string][]Timestamp{}
			metricsPerTimeVector := map[string]int{}
			timeCompressedBytes := int64(0)
			valueCompressedBytes := int64(0)
			totalPoints := 0

			for _, page := range pages {
				totalPoints += len(page.Times)
				key, rawTimes := encodeTimesKey(page.Times)
				if _, exists := uniqueTimes[key]; !exists {
					uniqueTimes[key] = append([]Timestamp(nil), page.Times...)
					compressed, err := codec.Encode(rawTimes)
					if err != nil {
						t.Fatalf("encode shared times payload failed: %v", err)
					}
					timeCompressedBytes += int64(len(compressed))
				}
				metricsPerTimeVector[key]++

				valuesPayload, err := encodeMetricValuesOnlyPayload(page)
				if err != nil {
					t.Fatalf("encodeMetricValuesOnlyPayload failed: %v", err)
				}
				compressedValues, err := codec.Encode(valuesPayload)
				if err != nil {
					t.Fatalf("encode metric values payload failed: %v", err)
				}
				valueCompressedBytes += int64(len(compressedValues))
			}

			frameCount := len(pages) + len(uniqueTimes)
			roughOverheadBytes := int64(metricFileV1HeaderLen+metricFileV1FooterLen) +
				int64(frameCount)*(metricFileV1FrameHeaderLen+4) +
				int64(frameCount)*metricFileV1PageInfoLen
			roughSharedLayoutBytes := timeCompressedBytes + valueCompressedBytes + roughOverheadBytes

			dist := make([]int, 0, len(metricsPerTimeVector))
			for _, n := range metricsPerTimeVector {
				dist = append(dist, n)
			}
			sort.Ints(dist)

			t.Logf("partition=%s metric_frames=%d unique_time_frames=%d points=%d", partition, len(pages), len(uniqueTimes), totalPoints)
			t.Logf("partition=%s metrics_per_time_frame=%v", partition, dist)
			t.Logf("partition=%s actual_v1_file_bytes=%d actual_v1_payload_bytes=%d", partition, metricInfo.Size(), actualPayloadBytes)
			t.Logf("partition=%s shared_time_payload_bytes=%d values_only_payload_bytes=%d", partition, timeCompressedBytes, valueCompressedBytes)
			t.Logf("partition=%s rough_shared_layout_bytes=%d savings_vs_v1=%.2f%% payload_savings_vs_v1=%.2f%%", partition, roughSharedLayoutBytes, 100*(1-float64(roughSharedLayoutBytes)/float64(metricInfo.Size())), 100*(1-float64(timeCompressedBytes+valueCompressedBytes)/float64(actualPayloadBytes)))

			if len(uniqueTimes) >= len(pages) {
				t.Fatalf("expected fewer shared time frames than metric frames: time_frames=%d metric_frames=%d", len(uniqueTimes), len(pages))
			}
			if roughSharedLayoutBytes >= metricInfo.Size() {
				t.Fatalf("expected rough shared-time layout to beat v1: rough=%d v1=%d", roughSharedLayoutBytes, metricInfo.Size())
			}
		})
	}
}

func encodeTimesKey(times []Timestamp) (string, []byte) {
	var payload bytes.Buffer
	payload.Grow(len(times) * 8)
	for _, ts := range times {
		var b [8]byte
		binary.LittleEndian.PutUint64(b[:], uint64(ts))
		_, _ = payload.Write(b[:])
	}
	blob := payload.Bytes()
	return string(blob), blob
}

func encodeMetricValuesOnlyPayload(page MetricFilePageInput) ([]byte, error) {
	var payload bytes.Buffer
	payload.Grow(len(page.Times) * 4)
	switch page.ValueType {
	case Int32Sample:
		if len(page.Int32) != len(page.Times) || len(page.Float32) != 0 {
			return nil, fmt.Errorf("invalid int32 payload for metric %d", page.MetricID)
		}
		for _, value := range page.Int32 {
			var b [4]byte
			binary.LittleEndian.PutUint32(b[:], uint32(value))
			if _, err := payload.Write(b[:]); err != nil {
				return nil, err
			}
		}
	case Float32Sample:
		if len(page.Float32) != len(page.Times) || len(page.Int32) != 0 {
			return nil, fmt.Errorf("invalid float32 payload for metric %d", page.MetricID)
		}
		for _, value := range page.Float32 {
			var b [4]byte
			binary.LittleEndian.PutUint32(b[:], math.Float32bits(value))
			if _, err := payload.Write(b[:]); err != nil {
				return nil, err
			}
		}
	default:
		return nil, fmt.Errorf("unsupported value type: %d", page.ValueType)
	}
	return payload.Bytes(), nil
}
