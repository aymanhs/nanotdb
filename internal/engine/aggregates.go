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
	Compute(points []float32) (float32, error)
}

type minAggregator struct{}

type maxAggregator struct{}

type sumAggregator struct{}

type avgAggregator struct{}

type countAggregator struct{}

type percentileAggregator struct {
	name       string
	percentile float64
}

type trimmedAvgAggregator struct{}

func (minAggregator) Name() string { return "min" }

func (minAggregator) Compute(points []float32) (float32, error) {
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

func (maxAggregator) Compute(points []float32) (float32, error) {
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

func (sumAggregator) Compute(points []float32) (float32, error) {
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

func (avgAggregator) Compute(points []float32) (float32, error) {
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

func (countAggregator) Compute(points []float32) (float32, error) {
	if len(points) == 0 {
		return 0, fmt.Errorf("no points")
	}
	return float32(len(points)), nil
}

func (p percentileAggregator) Name() string { return p.name }

func (p percentileAggregator) Compute(points []float32) (float32, error) {
	if len(points) == 0 {
		return 0, fmt.Errorf("no points")
	}
	if len(points) == 1 {
		return points[0], nil
	}
	sorted := append([]float32(nil), points...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	position := (float64(len(sorted)) - 1) * p.percentile
	lower := int(math.Floor(position))
	upper := int(math.Ceil(position))
	if lower == upper {
		return sorted[lower], nil
	}
	weight := float32(position - float64(lower))
	return sorted[lower] + (sorted[upper]-sorted[lower])*weight, nil
}

func (trimmedAvgAggregator) Name() string { return "trimmed_avg" }

func (trimmedAvgAggregator) Compute(points []float32) (float32, error) {
	if len(points) == 0 {
		return 0, fmt.Errorf("no points")
	}
	sorted := append([]float32(nil), points...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	// 5% trim from each tail when there are ≥20 samples, else trim 1 from
	// each tail. The thresholds keep small windows from collapsing to zero.
	trim := 1
	if len(sorted) >= 20 {
		trim = len(sorted) / 20
	}
	if trim*2 >= len(sorted) {
		trim = 0
	}
	trimmed := sorted[trim : len(sorted)-trim]
	if len(trimmed) == 0 {
		return 0, fmt.Errorf("no points")
	}
	sum := float64(0)
	for _, v := range trimmed {
		sum += float64(v)
	}
	return float32(sum / float64(len(trimmed))), nil
}

var aggregatorRegistry = map[string]Aggregator{
	"avg":             avgAggregator{},
	"count":           countAggregator{},
	"max":             maxAggregator{},
	"median":          percentileAggregator{name: "median", percentile: 0.50},
	"min":             minAggregator{},
	"p50":             percentileAggregator{name: "p50", percentile: 0.50},
	"p95":             percentileAggregator{name: "p95", percentile: 0.95},
	"p99":             percentileAggregator{name: "p99", percentile: 0.99},
	"sum":             sumAggregator{},
	"trimmed_avg":     trimmedAvgAggregator{},
	"trimmed_average": trimmedAvgAggregator{},
}

const defaultStepAggregate = "avg"

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

func SupportedAggregates() []string {
	out := make([]string, 0, len(aggregatorRegistry))
	for name := range aggregatorRegistry {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func Aggregators() map[string]Aggregator {
	out := make(map[string]Aggregator, len(aggregatorRegistry))
	for name, agg := range aggregatorRegistry {
		out[name] = agg
	}
	return out
}

func DefaultStepAggregate() string {
	return defaultStepAggregate
}
