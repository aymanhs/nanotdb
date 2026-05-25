package engine

import (
	"math"
	"testing"
	"time"
)

func TestEngineQueryAggregateRangeEmitsBucketEnds(t *testing.T) {
	e, err := OpenEngine(t.TempDir(), 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer e.Close()

	base := Timestamp(time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC).UnixNano())
	points := []struct {
		offset time.Duration
		value  float32
	}{
		{offset: 10 * time.Second, value: 10},
		{offset: 4 * time.Minute, value: 20},
		{offset: 5*time.Minute + 10*time.Second, value: 30},
		{offset: 9*time.Minute + 59*time.Second, value: 40},
	}
	for _, point := range points {
		if err := e.AddSample("prod", "temp.out_dry", base+Timestamp(point.offset), point.value); err != nil {
			t.Fatalf("AddSample failed: %v", err)
		}
	}

	var buckets []AggregateBucket
	err = e.QueryAggregateRange("prod", "temp.out_dry", base, base+Timestamp(10*time.Minute), 5*time.Minute, []string{"sum", "count"}, func(bucket AggregateBucket) error {
		buckets = append(buckets, bucket)
		return nil
	})
	if err != nil {
		t.Fatalf("QueryAggregateRange failed: %v", err)
	}
	if len(buckets) != 4 {
		t.Fatalf("bucket count mismatch: got=%d want=4", len(buckets))
	}
	assertAggregateBucket(t, buckets[0], "sum", base, base+Timestamp(5*time.Minute), 30)
	assertAggregateBucket(t, buckets[1], "count", base, base+Timestamp(5*time.Minute), 2)
	assertAggregateBucket(t, buckets[2], "sum", base+Timestamp(5*time.Minute), base+Timestamp(10*time.Minute), 70)
	assertAggregateBucket(t, buckets[3], "count", base+Timestamp(5*time.Minute), base+Timestamp(10*time.Minute), 2)
	if buckets[0].EndTS != base+Timestamp(5*time.Minute) || buckets[2].EndTS != base+Timestamp(10*time.Minute) {
		t.Fatalf("expected bucket-end timestamps, got first=%d second=%d", buckets[0].EndTS, buckets[2].EndTS)
	}
}

func TestEngineQueryAggregateRangeClipsEdgeBuckets(t *testing.T) {
	e, err := OpenEngine(t.TempDir(), 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer e.Close()

	base := Timestamp(time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC).UnixNano())
	points := []struct {
		offset time.Duration
		value  float32
	}{
		{offset: 2*time.Minute + 30*time.Second, value: 5},
		{offset: 4 * time.Minute, value: 7},
		{offset: 5*time.Minute + 30*time.Second, value: 11},
	}
	for _, point := range points {
		if err := e.AddSample("prod", "temp.out_dry", base+Timestamp(point.offset), point.value); err != nil {
			t.Fatalf("AddSample failed: %v", err)
		}
	}

	fromTS := base + Timestamp(2*time.Minute)
	toTS := base + Timestamp(7*time.Minute)
	var buckets []AggregateBucket
	err = e.QueryAggregateRange("prod", "temp.out_dry", fromTS, toTS, 5*time.Minute, []string{"sum"}, func(bucket AggregateBucket) error {
		buckets = append(buckets, bucket)
		return nil
	})
	if err != nil {
		t.Fatalf("QueryAggregateRange failed: %v", err)
	}
	if len(buckets) != 2 {
		t.Fatalf("bucket count mismatch: got=%d want=2", len(buckets))
	}
	assertAggregateBucket(t, buckets[0], "sum", fromTS, base+Timestamp(5*time.Minute), 12)
	assertAggregateBucket(t, buckets[1], "sum", base+Timestamp(5*time.Minute), toTS, 11)
}

func TestEngineQueryAggregateRangeSupportsBuiltins(t *testing.T) {
	e, err := OpenEngine(t.TempDir(), 1024*1024)
	if err != nil {
		t.Fatalf("OpenEngine failed: %v", err)
	}
	defer e.Close()

	base := Timestamp(time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC).UnixNano())
	values := []float32{10, 20, 40}
	for i, value := range values {
		if err := e.AddSample("prod", "temp.out_dry", base+Timestamp(time.Duration(i)*time.Minute), value); err != nil {
			t.Fatalf("AddSample failed: %v", err)
		}
	}

	got := make(map[string]AggregateBucket)
	err = e.QueryAggregateRange("prod", "temp.out_dry", base, base+Timestamp(5*time.Minute), 5*time.Minute, []string{"min", "max", "sum", "avg", "count"}, func(bucket AggregateBucket) error {
		got[bucket.Aggregate] = bucket
		return nil
	})
	if err != nil {
		t.Fatalf("QueryAggregateRange failed: %v", err)
	}
	assertAggregateBucket(t, got["min"], "min", base, base+Timestamp(5*time.Minute), 10)
	assertAggregateBucket(t, got["max"], "max", base, base+Timestamp(5*time.Minute), 40)
	assertAggregateBucket(t, got["sum"], "sum", base, base+Timestamp(5*time.Minute), 70)
	assertAggregateBucket(t, got["avg"], "avg", base, base+Timestamp(5*time.Minute), float32(70.0/3.0))
	assertAggregateBucket(t, got["count"], "count", base, base+Timestamp(5*time.Minute), 3)
}

func assertAggregateBucket(t *testing.T, got AggregateBucket, wantAgg string, wantStart, wantEnd Timestamp, wantValue float32) {
	t.Helper()
	if got.Aggregate != wantAgg {
		t.Fatalf("aggregate mismatch: got=%q want=%q", got.Aggregate, wantAgg)
	}
	if got.StartTS != wantStart {
		t.Fatalf("start mismatch for %q: got=%d want=%d", wantAgg, got.StartTS, wantStart)
	}
	if got.EndTS != wantEnd {
		t.Fatalf("end mismatch for %q: got=%d want=%d", wantAgg, got.EndTS, wantEnd)
	}
	if math.Abs(float64(got.Value-wantValue)) > 0.0001 {
		t.Fatalf("value mismatch for %q: got=%f want=%f", wantAgg, got.Value, wantValue)
	}
}
