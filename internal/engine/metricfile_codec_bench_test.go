package engine

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
)

var metricCodecSink []byte

func BenchmarkMetricPayloadCodecsFixture(b *testing.B) {
	payloads := loadMetricFixturePayloads(b)
	totalRawBytes := 0
	for _, payload := range payloads {
		totalRawBytes += len(payload)
	}

	codecNames := []string{
		CompressionCodecS2Name,
		CompressionCodecS2BetterName,
		CompressionCodecZstdFastestName,
		CompressionCodecZstdDefaultName,
	}

	for _, codecName := range codecNames {
		codec, err := BlockCompressionCodecByName(codecName)
		if err != nil {
			b.Fatalf("BlockCompressionCodecByName(%q): %v", codecName, err)
		}
		compressedPayloads := make([][]byte, len(payloads))
		totalCompressedBytes := 0
		for i, payload := range payloads {
			compressed, err := codec.Encode(payload)
			if err != nil {
				b.Fatalf("encode %s payload %d: %v", codec.Name(), i, err)
			}
			compressedPayloads[i] = compressed
			totalCompressedBytes += len(compressed)
		}

		b.Run(fmt.Sprintf("encode/%s", codec.Name()), func(b *testing.B) {
			b.ReportAllocs()
			b.ReportMetric(float64(totalRawBytes), "raw_B")
			b.ReportMetric(float64(totalCompressedBytes), "compressed_B")
			b.ReportMetric(float64(totalCompressedBytes)/float64(totalRawBytes), "ratio")
			b.ReportMetric(float64(totalCompressedBytes)/float64(len(payloads)), "avg_payload_B")
			for i := 0; i < b.N; i++ {
				for _, payload := range payloads {
					encoded, err := codec.Encode(payload)
					if err != nil {
						b.Fatalf("encode %s: %v", codec.Name(), err)
					}
					metricCodecSink = encoded
				}
			}
		})

		b.Run(fmt.Sprintf("decode/%s", codec.Name()), func(b *testing.B) {
			b.ReportAllocs()
			b.ReportMetric(float64(totalRawBytes), "raw_B")
			b.ReportMetric(float64(totalCompressedBytes), "compressed_B")
			b.ReportMetric(float64(totalCompressedBytes)/float64(totalRawBytes), "ratio")
			b.ReportMetric(float64(totalCompressedBytes)/float64(len(payloads)), "avg_payload_B")
			for i := 0; i < b.N; i++ {
				for idx, compressed := range compressedPayloads {
					decoded, err := codec.Decode(compressed)
					if err != nil {
						b.Fatalf("decode %s payload %d: %v", codec.Name(), idx, err)
					}
					if len(decoded) != len(payloads[idx]) {
						b.Fatalf("decoded len mismatch for %s payload %d: got %d want %d", codec.Name(), idx, len(decoded), len(payloads[idx]))
					}
					metricCodecSink = decoded
				}
			}
		})
	}
}

func loadMetricFixturePayloads(tb testing.TB) [][]byte {
	tb.Helper()
	metricPath := filepath.Join("..", "..", "test-data", "metric-poc-big", "sensors", "metric-2026-05-03.dat")
	if _, err := os.Stat(metricPath); err != nil {
		tb.Skipf("metric fixture missing: %v", err)
	}
	pages, err := ReadMetricFileV1(metricPath)
	if err != nil {
		tb.Fatalf("ReadMetricFileV1: %v", err)
	}
	payloads := make([][]byte, 0, len(pages))
	for _, page := range pages {
		payloads = append(payloads, encodeMetricPayloadRaw(tb, page))
	}
	return payloads
}

func encodeMetricPayloadRaw(tb testing.TB, page MetricFilePage) []byte {
	tb.Helper()
	if len(page.Times) != int(page.PointCount) {
		tb.Fatalf("metric page payload shape mismatch: times=%d point_count=%d", len(page.Times), page.PointCount)
	}
	payload := make([]byte, int(page.PointCount)*12)
	for i, ts := range page.Times {
		binary.LittleEndian.PutUint64(payload[i*8:i*8+8], uint64(ts))
	}
	valuesOff := len(page.Times) * 8
	switch page.ValueType {
	case Int32Sample:
		if len(page.Int32) != len(page.Times) {
			tb.Fatalf("metric page int32 payload mismatch: values=%d times=%d", len(page.Int32), len(page.Times))
		}
		for i, value := range page.Int32 {
			binary.LittleEndian.PutUint32(payload[valuesOff+i*4:valuesOff+i*4+4], uint32(value))
		}
	case Float32Sample:
		if len(page.Float32) != len(page.Times) {
			tb.Fatalf("metric page float32 payload mismatch: values=%d times=%d", len(page.Float32), len(page.Times))
		}
		for i, value := range page.Float32 {
			binary.LittleEndian.PutUint32(payload[valuesOff+i*4:valuesOff+i*4+4], math.Float32bits(value))
		}
	default:
		tb.Fatalf("unsupported page value type: %d", page.ValueType)
	}
	return payload
}
