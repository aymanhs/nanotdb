package engine

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"math/rand"
	"testing"
	"time"
)

// encodeInt32 serialises an int32 to the 4-byte little-endian form the catalog uses.
func encodeInt32(v int32) []byte {
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], uint32(v))
	return buf[:]
}

// decodeInt32 reads an int32 from a 4-byte little-endian slice at the given byte offset.
func decodeInt32(values []byte, idx int) int32 {
	return int32(binary.LittleEndian.Uint32(values[idx*4:]))
}

// encodeFloat32 serialises a float32 to 4-byte little-endian form.
func encodeFloat32(v float32) []byte {
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], math.Float32bits(v))
	return buf[:]
}

func assertPageEqual(t *testing.T, got, want *Page) {
	t.Helper()
	if got.Start != want.Start {
		t.Fatalf("start mismatch: got=%d want=%d", got.Start, want.Start)
	}
	if got.End != want.End {
		t.Fatalf("end mismatch: got=%d want=%d", got.End, want.End)
	}
	if len(got.Metrics) != len(want.Metrics) {
		t.Fatalf("metrics length mismatch: got=%d want=%d", len(got.Metrics), len(want.Metrics))
	}
	for i := range want.Metrics {
		if got.Metrics[i] != want.Metrics[i] {
			t.Fatalf("metric mismatch at %d: got=%d want=%d", i, got.Metrics[i], want.Metrics[i])
		}
	}
	if len(got.Times) != len(want.Times) {
		t.Fatalf("times length mismatch: got=%d want=%d", len(got.Times), len(want.Times))
	}
	for i := range want.Times {
		if got.Times[i] != want.Times[i] {
			t.Fatalf("time mismatch at %d: got=%d want=%d", i, got.Times[i], want.Times[i])
		}
	}
	if !bytes.Equal(got.Values.Bytes(), want.Values.Bytes()) {
		t.Fatalf("values blob mismatch: got %d bytes want %d bytes", got.Values.Len(), want.Values.Len())
	}
}

func TestPageRoundTripInt32(t *testing.T) {
	p := NewPage(1000)
	type sample struct {
		mid MetricID
		ts  Timestamp
		v   int32
	}
	samples := []sample{
		{1, 1000, 10},
		{2, 1001, -1},
		{1, 1004, 123456},
		{2, 1010, 0},
	}
	for _, s := range samples {
		if err := p.AddSample(s.mid, s.ts, encodeInt32(s.v)); err != nil {
			t.Fatalf("AddSample failed: %v", err)
		}
	}

	buf := bytes.NewBuffer(make([]byte, 0, 4096))
	if err := p.EncodeInto(buf); err != nil {
		t.Fatalf("EncodeInto failed: %v", err)
	}

	var decoded Page
	if err := decoded.DecodeFrom(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("DecodeFrom failed: %v", err)
	}

	assertPageEqual(t, &decoded, p)
}

func TestPageRoundTripFloat32(t *testing.T) {
	p := NewPage(500)
	type sample struct {
		mid MetricID
		ts  Timestamp
		v   float32
	}
	samples := []sample{
		{10, 500, 1.25},
		{11, 500, -0.5},
		{10, 503, float32(math.Pi)},
		{11, 510, 1000.125},
	}
	for _, s := range samples {
		if err := p.AddSample(s.mid, s.ts, encodeFloat32(s.v)); err != nil {
			t.Fatalf("AddSample failed: %v", err)
		}
	}

	buf := bytes.NewBuffer(make([]byte, 0, 4096))
	if err := p.EncodeInto(buf); err != nil {
		t.Fatalf("EncodeInto failed: %v", err)
	}

	var decoded Page
	if err := decoded.DecodeFrom(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("DecodeFrom failed: %v", err)
	}

	assertPageEqual(t, &decoded, p)
}

// buildRealisticPage builds a page with numMetrics metrics, count total samples
// distributed round-robin across metrics, int32 millidegree values that walk
// smoothly, timestamps advancing ~10s with jitter.
func buildRealisticPage(numMetrics int, count int, seed int64) (*Page, error) {
	if count <= 0 || numMetrics <= 0 {
		return nil, fmt.Errorf("count and numMetrics must be > 0")
	}
	rng := rand.New(rand.NewSource(seed))
	startTS := Timestamp(time.Now().Add(-90 * 24 * time.Hour).UnixNano())
	p := NewPage(startTS)

	// per-metric walking value and current timestamp
	values := make([]int32, numMetrics)
	ts := make([]Timestamp, numMetrics)
	for i := range values {
		values[i] = 20000 + int32(rng.Intn(20001)) // 20000–40000 millidegrees
		ts[i] = startTS
	}

	for i := 0; i < count; i++ {
		m := i % numMetrics
		mid := MetricID(m + 1)

		// advance this metric's timestamp
		if i >= numMetrics {
			jitter := rng.Intn(7) - 3
			delta := 10 + jitter
			if delta < 1 {
				delta = 1
			}
			ts[m] += Timestamp(delta)
		}

		// small walk in value
		step := int32(rng.Intn(61) - 30)
		values[m] += step
		if values[m] < 20000 {
			values[m] = 20000
		}
		if values[m] > 40000 {
			values[m] = 40000
		}

		// page requires non-decreasing timestamps globally; use the max seen so far
		pageTS := ts[m]
		if len(p.Times) > 0 && pageTS < p.Times[len(p.Times)-1] {
			pageTS = p.Times[len(p.Times)-1]
		}

		if err := p.AddSample(mid, pageTS, encodeInt32(values[m])); err != nil {
			return nil, err
		}
	}
	return p, nil
}

func TestPageRealisticRoundTripAndCompression(t *testing.T) {
	t.Parallel()
	testCases := []struct{ n, metrics int }{
		{20, 1},
		{1000, 4},
		{4000, 8},
	}
	for _, tc := range testCases {
		tc := tc
		t.Run(fmt.Sprintf("samples_%d_metrics_%d", tc.n, tc.metrics), func(t *testing.T) {
			p, err := buildRealisticPage(tc.metrics, tc.n, int64(42+tc.n))
			if err != nil {
				t.Fatalf("buildRealisticPage: %v", err)
			}

			rawSize := HeaderSize + len(p.Metrics)*2 + len(p.Times)*8 + p.Values.Len()

			encodeStart := time.Now()
			buf := bytes.NewBuffer(make([]byte, 0, rawSize))
			if err := p.EncodeInto(buf); err != nil {
				t.Fatalf("EncodeInto failed: %v", err)
			}
			encodeDur := time.Since(encodeStart)

			decodeStart := time.Now()
			var decoded Page
			if err := decoded.DecodeFrom(bytes.NewReader(buf.Bytes())); err != nil {
				t.Fatalf("DecodeFrom failed: %v", err)
			}
			decodeDur := time.Since(decodeStart)

			assertPageEqual(t, &decoded, p)

			encodedSize := buf.Len()
			ratio := float64(encodedSize) / float64(rawSize)
			t.Logf("samples=%d metrics=%d raw=%dB compressed_block=%dB ratio=%.3f encode=%s decode=%s",
				tc.n, tc.metrics, rawSize, encodedSize, ratio, encodeDur, decodeDur)
		})
	}
}

func BenchmarkPageEncodeDecodeInt32(b *testing.B) {
	testCases := []struct{ n, metrics int }{
		{20, 1},
		{1000, 4},
		{4000, 8},
	}
	for _, tc := range testCases {
		tc := tc
		b.Run(fmt.Sprintf("samples_%d_metrics_%d", tc.n, tc.metrics), func(b *testing.B) {
			p, err := buildRealisticPage(tc.metrics, tc.n, int64(900+tc.n))
			if err != nil {
				b.Fatalf("buildRealisticPage: %v", err)
			}
			rawSize := HeaderSize + len(p.Metrics)*2 + len(p.Times)*8 + p.Values.Len()

			buf := bytes.NewBuffer(make([]byte, 0, rawSize))
			if err := p.EncodeInto(buf); err != nil {
				b.Fatalf("warmup encode: %v", err)
			}
			b.ReportMetric(float64(buf.Len()), "encoded_B")
			b.ReportMetric(float64(rawSize), "raw_B")
			b.ReportMetric(float64(buf.Len())/float64(rawSize), "ratio")

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				buf.Reset()
				if err := p.EncodeInto(buf); err != nil {
					b.Fatalf("EncodeInto: %v", err)
				}
				var decoded Page
				if err := decoded.DecodeFrom(bytes.NewReader(buf.Bytes())); err != nil {
					b.Fatalf("DecodeFrom: %v", err)
				}
			}
		})
	}
}
