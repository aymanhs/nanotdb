package collectors

import "context"

// Metric is a single collected data point.
// Value must be int32 or float32 — matching nanotdb's native types.
type Metric struct {
	Name  string // e.g., "cpu.usage.user"
	Value any    // int32 | float32

	// EmitAsEvent marks this sample for event ingestion instead of metric
	// line-protocol ingestion. When true, main sends an event payload to
	// /api/v1/events and skips this sample in the metric batch.
	EmitAsEvent  bool
	EventName    string
	EventPayload any
}

// Collector defines the interface for metric collectors.
type Collector interface {
	Name() string
	Collect(ctx context.Context, ch chan<- Metric)
}
