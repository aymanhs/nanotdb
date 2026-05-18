package collectors

import "context"

// Metric is a single collected data point.
// Value must be int32 or float32 — matching nanotdb's native types.
type Metric struct {
	Name  string // e.g., "cpu.usage.user"
	Value any    // int32 | float32
}

// Collector defines the interface for metric collectors.
type Collector interface {
	Name() string
	Collect(ctx context.Context, ch chan<- Metric)
}
