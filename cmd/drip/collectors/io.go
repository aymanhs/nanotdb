package collectors

import (
	"bufio"
	"context"
	"log"
	"os"
	"strconv"
	"strings"
)

// IOCollector reads /proc/vmstat and emits system-wide paging and I/O counters.
// Metrics: io.pgpgin, io.pgpgout, io.pswpin, io.pswpout
type IOCollector struct{}

func NewIOCollector() *IOCollector { return &IOCollector{} }

func (c *IOCollector) Name() string { return "io" }

var vmstatKeys = map[string]string{
	"pgpgin":  "io.pgpgin",  // pages paged in
	"pgpgout": "io.pgpgout", // pages paged out
	"pswpin":  "io.pswpin",  // swap pages in
	"pswpout": "io.pswpout", // swap pages out
}

func (c *IOCollector) Collect(ctx context.Context, ch chan<- Metric) {
	f, err := os.Open("/proc/vmstat")
	if err != nil {
		log.Printf("io collector: open /proc/vmstat: %v", err)
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.Fields(line)
		if len(parts) != 2 {
			continue
		}
		metricName, ok := vmstatKeys[parts[0]]
		if !ok {
			continue
		}
		v, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			log.Printf("io collector: parse %s: %v", parts[0], err)
			continue
		}
		select {
		case ch <- Metric{Name: metricName, Value: float32(v)}:
		case <-ctx.Done():
			return
		}
	}
	if err := scanner.Err(); err != nil {
		log.Printf("io collector: scan /proc/vmstat: %v", err)
	}
}
