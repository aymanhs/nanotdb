package engine

import (
	"fmt"
	"strings"
	"time"
)

type AggregateBucket struct {
	Database  string
	Metric    string
	Aggregate string
	Window    time.Duration
	StartTS   Timestamp
	EndTS     Timestamp
	Value     float32
}

type AggregateBucketCallback func(AggregateBucket) error

type aggregateBucketAccumulator struct {
	name  string
	kind  string
	sum   float32
	min   float32
	max   float32
	count int
}

func newAggregateBucketAccumulators(resolved []namedAggregator) ([]aggregateBucketAccumulator, bool) {
	out := make([]aggregateBucketAccumulator, 0, len(resolved))
	for _, agg := range resolved {
		switch agg.name {
		case "min", "max", "sum", "avg", "count":
			out = append(out, aggregateBucketAccumulator{name: agg.name, kind: agg.name})
		default:
			return nil, false
		}
	}
	return out, true
}

func addAggregateBucketSample(accumulators []aggregateBucketAccumulator, value float32) {
	for i := range accumulators {
		acc := &accumulators[i]
		if acc.count == 0 {
			acc.min = value
			acc.max = value
		} else {
			if value < acc.min {
				acc.min = value
			}
			if value > acc.max {
				acc.max = value
			}
		}
		acc.sum += value
		acc.count++
	}
}

func resetAggregateBucketAccumulators(accumulators []aggregateBucketAccumulator) {
	for i := range accumulators {
		accumulators[i].sum = 0
		accumulators[i].min = 0
		accumulators[i].max = 0
		accumulators[i].count = 0
	}
}

func aggregateBucketAccumulatorValue(acc aggregateBucketAccumulator) (float32, error) {
	if acc.count == 0 {
		return 0, fmt.Errorf("no points")
	}
	switch acc.kind {
	case "min":
		return acc.min, nil
	case "max":
		return acc.max, nil
	case "sum":
		return acc.sum, nil
	case "avg":
		return acc.sum / float32(acc.count), nil
	case "count":
		return float32(acc.count), nil
	default:
		return 0, fmt.Errorf("unsupported aggregate %q", acc.kind)
	}
}

func (e *Engine) QueryAggregateRange(database, metric string, fromTS, toTS Timestamp, window time.Duration, aggregates []string, fn AggregateBucketCallback) error {
	if toTS < fromTS {
		return fmt.Errorf("invalid range: toTS < fromTS")
	}
	if window <= 0 {
		return fmt.Errorf("window must be > 0")
	}
	if fn == nil {
		return fmt.Errorf("aggregate callback cannot be nil")
	}
	resolved, err := resolveAggregators(aggregates)
	if err != nil {
		return err
	}
	streamingAccumulators, useStreamingAccumulators := newAggregateBucketAccumulators(resolved)

	var currentBucketStart Timestamp
	var bucketPoints []float32
	haveBucket := false

	flushBucket := func(bucketStart Timestamp) error {
		if useStreamingAccumulators {
			if len(streamingAccumulators) == 0 || streamingAccumulators[0].count == 0 {
				return nil
			}
		} else if len(bucketPoints) == 0 {
			return nil
		}
		naturalEnd := Timestamp(int64(bucketStart) + int64(window))
		effectiveStart := bucketStart
		if effectiveStart < fromTS {
			effectiveStart = fromTS
		}
		effectiveEnd := naturalEnd
		if effectiveEnd > toTS {
			effectiveEnd = toTS
		}
		if useStreamingAccumulators {
			for _, acc := range streamingAccumulators {
				value, err := aggregateBucketAccumulatorValue(acc)
				if err != nil {
					return err
				}
				if err := fn(AggregateBucket{
					Database:  database,
					Metric:    metric,
					Aggregate: acc.name,
					Window:    window,
					StartTS:   effectiveStart,
					EndTS:     effectiveEnd,
					Value:     value,
				}); err != nil {
					return err
				}
			}
			return nil
		}
		for _, agg := range resolved {
			value, err := agg.impl.Compute(effectiveStart, effectiveEnd, bucketPoints)
			if err != nil {
				return err
			}
			if err := fn(AggregateBucket{
				Database:  database,
				Metric:    metric,
				Aggregate: agg.name,
				Window:    window,
				StartTS:   effectiveStart,
				EndTS:     effectiveEnd,
				Value:     value,
			}); err != nil {
				return err
			}
		}
		return nil
	}

	err = e.QueryRange(database, metric, fromTS, toTS, 1, func(s Sample) error {
		value := s.Float32
		if s.ValueType == Int32Sample {
			value = float32(s.Int32)
		}
		bucketStart := floorTimestamp(s.TS, window)
		if !haveBucket {
			currentBucketStart = bucketStart
			haveBucket = true
		} else if bucketStart != currentBucketStart {
			if err := flushBucket(currentBucketStart); err != nil {
				return err
			}
			if useStreamingAccumulators {
				resetAggregateBucketAccumulators(streamingAccumulators)
			} else {
				bucketPoints = bucketPoints[:0]
			}
			currentBucketStart = bucketStart
		}
		if useStreamingAccumulators {
			addAggregateBucketSample(streamingAccumulators, value)
		} else {
			bucketPoints = append(bucketPoints, value)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if haveBucket {
		if err := flushBucket(currentBucketStart); err != nil {
			return err
		}
	}
	return nil
}

type namedAggregator struct {
	name string
	impl Aggregator
}

func resolveAggregators(names []string) ([]namedAggregator, error) {
	resolved := make([]namedAggregator, 0, len(names))
	seen := make(map[string]struct{}, len(names))
	for _, raw := range names {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		agg, ok := getAggregator(name)
		if !ok {
			return nil, fmt.Errorf("unsupported aggregate %q (supported: %s)", name, strings.Join(supportedAggregates(), ","))
		}
		seen[name] = struct{}{}
		resolved = append(resolved, namedAggregator{name: name, impl: agg})
	}
	if len(resolved) == 0 {
		return nil, fmt.Errorf("at least one aggregate is required")
	}
	return resolved, nil
}
