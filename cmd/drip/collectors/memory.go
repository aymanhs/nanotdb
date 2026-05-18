package collectors

import (
	"bufio"
	"context"
	"log"
	"os"
	"strconv"
	"strings"
)

// MemoryCollector reads /proc/meminfo and emits key memory statistics in kB.
// Metrics: mem.total, mem.free, mem.available, mem.buffers, mem.cached,
//
//	mem.swapcached, mem.swaptotal, mem.swapfree
type MemoryCollector struct{}

func NewMemoryCollector() *MemoryCollector { return &MemoryCollector{} }

func (c *MemoryCollector) Name() string { return "memory" }

var memInfoKeys = map[string]string{
	"MemTotal":     "mem.total",
	"MemFree":      "mem.free",
	"MemAvailable": "mem.available",
	"Buffers":      "mem.buffers",
	"Cached":       "mem.cached",
	"SwapCached":   "mem.swapcached",
	"SwapTotal":    "mem.swaptotal",
	"SwapFree":     "mem.swapfree",
}

func (c *MemoryCollector) Collect(ctx context.Context, ch chan<- Metric) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		log.Printf("memory collector: open /proc/meminfo: %v", err)
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		// Format: "MemTotal:       16384000 kB"
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		key := strings.TrimSuffix(parts[0], ":")
		metricName, ok := memInfoKeys[key]
		if !ok {
			continue
		}
		v, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			log.Printf("memory collector: parse %s: %v", key, err)
			continue
		}
		select {
		case ch <- Metric{Name: metricName, Value: float32(v)}:
		case <-ctx.Done():
			return
		}
	}
	if err := scanner.Err(); err != nil {
		log.Printf("memory collector: scan /proc/meminfo: %v", err)
	}
}
