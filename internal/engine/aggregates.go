package engine

import (
	"fmt"
	"math"
	"sort"
)

// Aggregator computes one aggregate value for a time window.
// points are source metric values collected for that window.
type Aggregator interface {
	Name() string
	Compute(periodStart, periodEnd Timestamp, points []float32) (float32, error)
}

type minAggregator struct{}

type maxAggregator struct{}

type sumAggregator struct{}

type avgAggregator struct{}

type countAggregator struct{}

func (minAggregator) Name() string { return "min" }

func (minAggregator) Compute(_ Timestamp, _ Timestamp, points []float32) (float32, error) {
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

func (maxAggregator) Name() string { return "max" }

func (maxAggregator) Compute(_ Timestamp, _ Timestamp, points []float32) (float32, error) {
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

func (sumAggregator) Name() string { return "sum" }

func (sumAggregator) Compute(_ Timestamp, _ Timestamp, points []float32) (float32, error) {
	if len(points) == 0 {
		return 0, fmt.Errorf("no points")
	}
	sum := float64(0)
	for _, v := range points {
		sum += float64(v)
	}
	return float32(sum), nil
}

func (avgAggregator) Name() string { return "avg" }

func (avgAggregator) Compute(_ Timestamp, _ Timestamp, points []float32) (float32, error) {
	if len(points) == 0 {
		return 0, fmt.Errorf("no points")
	}
	sum := float64(0)
	for _, v := range points {
		sum += float64(v)
	}
	return float32(sum / float64(len(points))), nil
}

func (countAggregator) Name() string { return "count" }

func (countAggregator) Compute(_ Timestamp, _ Timestamp, points []float32) (float32, error) {
	if len(points) == 0 {
		return 0, fmt.Errorf("no points")
	}
	return float32(len(points)), nil
}

var aggregatorRegistry = map[string]Aggregator{
	"min":   minAggregator{},
	"max":   maxAggregator{},
	"sum":   sumAggregator{},
	"avg":   avgAggregator{},
	"count": countAggregator{},
}

func getAggregator(name string) (Aggregator, bool) {
	agg, ok := aggregatorRegistry[name]
	return agg, ok
}

func isSupportedAggregate(name string) bool {
	_, ok := getAggregator(name)
	return ok
}

func defaultRollupAggregates() []string {
	return []string{"min", "max", "sum", "avg", "count"}
}

func supportedAggregates() []string {
	out := make([]string, 0, len(aggregatorRegistry))
	for name := range aggregatorRegistry {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
