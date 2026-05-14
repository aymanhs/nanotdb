package engine

import (
	"fmt"
	"math"
	"sort"
)

// RollupAggregator computes one aggregate value for a rollup period.
// points are source metric values in [periodStart, periodEnd).
type RollupAggregator interface {
	Name() string
	Compute(periodStart, periodEnd Timestamp, points []float32) (float32, error)
}

type minRollupAggregator struct{}

type maxRollupAggregator struct{}

type sumRollupAggregator struct{}

type avgRollupAggregator struct{}

type countRollupAggregator struct{}

func (minRollupAggregator) Name() string { return "min" }

func (minRollupAggregator) Compute(_ Timestamp, _ Timestamp, points []float32) (float32, error) {
	if len(points) == 0 {
		return 0, fmt.Errorf("no points")
	}
	minVal := float32(math.MaxFloat32)
	for _, v := range points {
		if v < minVal {
			minVal = v
		}
	}
	return minVal, nil
}

func (maxRollupAggregator) Name() string { return "max" }

func (maxRollupAggregator) Compute(_ Timestamp, _ Timestamp, points []float32) (float32, error) {
	if len(points) == 0 {
		return 0, fmt.Errorf("no points")
	}
	maxVal := float32(-math.MaxFloat32)
	for _, v := range points {
		if v > maxVal {
			maxVal = v
		}
	}
	return maxVal, nil
}

func (sumRollupAggregator) Name() string { return "sum" }

func (sumRollupAggregator) Compute(_ Timestamp, _ Timestamp, points []float32) (float32, error) {
	if len(points) == 0 {
		return 0, fmt.Errorf("no points")
	}
	sum := float64(0)
	for _, v := range points {
		sum += float64(v)
	}
	return float32(sum), nil
}

func (avgRollupAggregator) Name() string { return "avg" }

func (avgRollupAggregator) Compute(_ Timestamp, _ Timestamp, points []float32) (float32, error) {
	if len(points) == 0 {
		return 0, fmt.Errorf("no points")
	}
	sum := float64(0)
	for _, v := range points {
		sum += float64(v)
	}
	return float32(sum / float64(len(points))), nil
}

func (countRollupAggregator) Name() string { return "count" }

func (countRollupAggregator) Compute(_ Timestamp, _ Timestamp, points []float32) (float32, error) {
	if len(points) == 0 {
		return 0, fmt.Errorf("no points")
	}
	return float32(len(points)), nil
}

var rollupAggregatorRegistry = map[string]RollupAggregator{
	"min":   minRollupAggregator{},
	"max":   maxRollupAggregator{},
	"sum":   sumRollupAggregator{},
	"avg":   avgRollupAggregator{},
	"count": countRollupAggregator{},
}

func getRollupAggregator(name string) (RollupAggregator, bool) {
	agg, ok := rollupAggregatorRegistry[name]
	return agg, ok
}

func isSupportedRollupAggregate(name string) bool {
	_, ok := getRollupAggregator(name)
	return ok
}

func defaultRollupAggregates() []string {
	return []string{"min", "max", "sum", "avg", "count"}
}

func supportedRollupAggregates() []string {
	out := make([]string, 0, len(rollupAggregatorRegistry))
	for name := range rollupAggregatorRegistry {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
