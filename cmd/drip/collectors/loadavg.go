package collectors

import (
	"context"
	"log"
	"os"
	"strconv"
	"strings"
)

// LoadAvgCollector reads /proc/loadavg and emits the three load averages
// plus the count of runnable and total processes.
// Metrics: sys.load1, sys.load5, sys.load15, sys.procs_running, sys.procs_total
type LoadAvgCollector struct{}

func NewLoadAvgCollector() *LoadAvgCollector { return &LoadAvgCollector{} }

func (c *LoadAvgCollector) Name() string { return "loadavg" }

func (c *LoadAvgCollector) Collect(ctx context.Context, ch chan<- Metric) {
	raw, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		log.Printf("loadavg collector: %v", err)
		return
	}
	// Format: 0.52 0.58 0.59 2/512 1234
	fields := strings.Fields(string(raw))
	if len(fields) < 4 {
		log.Printf("loadavg collector: unexpected format: %q", string(raw))
		return
	}

	type metric struct {
		name  string
		raw   string
		float bool
	}
	metrics := []metric{
		{"sys.load1", fields[0], true},
		{"sys.load5", fields[1], true},
		{"sys.load15", fields[2], true},
	}

	// fields[3] is "running/total"
	parts := strings.SplitN(fields[3], "/", 2)
	if len(parts) == 2 {
		metrics = append(metrics,
			metric{"sys.procs_running", parts[0], false},
			metric{"sys.procs_total", parts[1], false},
		)
	}

	for _, m := range metrics {
		var val any
		if m.float {
			v, err := strconv.ParseFloat(m.raw, 32)
			if err != nil {
				log.Printf("loadavg collector: parse %s: %v", m.name, err)
				continue
			}
			val = float32(v)
		} else {
			v, err := strconv.ParseInt(m.raw, 10, 32)
			if err != nil {
				log.Printf("loadavg collector: parse %s: %v", m.name, err)
				continue
			}
			val = int32(v)
		}
		select {
		case ch <- Metric{Name: m.name, Value: val}:
		case <-ctx.Done():
			return
		}
	}
}
