package collectors

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// SDWriteProbeCollector measures write+fsync latency in milliseconds by writing
// a small probe file periodically. It emits the latest measured value each cycle.
type SDWriteProbeCollector struct {
	directory    string
	bytes        int
	everyNCycles int
	metric       string

	mu       sync.Mutex
	cycle    int
	hasLast  bool
	lastMS   float32
	payload  []byte
	fileName string
}

func NewSDWriteProbeCollector(directory string, bytes int, everyNCycles int, metric string) *SDWriteProbeCollector {
	dir := strings.TrimSpace(directory)
	if dir == "" {
		dir = "/tmp"
	}
	if bytes <= 0 {
		bytes = 1024 * 256
	}
	if everyNCycles <= 0 {
		everyNCycles = 6
	}
	metric = strings.TrimSpace(metric)
	if metric == "" {
		metric = "disk.sd_write_probe_ms"
	}
	return &SDWriteProbeCollector{
		directory:    dir,
		bytes:        bytes,
		everyNCycles: everyNCycles,
		metric:       metric,
		payload:      make([]byte, bytes),
		fileName:     ".drip-sd-write-probe.tmp",
	}
}

func (c *SDWriteProbeCollector) Name() string { return "sd_write_probe" }

func (c *SDWriteProbeCollector) Collect(ctx context.Context, ch chan<- Metric) {
	c.mu.Lock()
	c.cycle++
	cycle := c.cycle
	hasLast := c.hasLast
	last := c.lastMS
	c.mu.Unlock()

	firstProbe := !hasLast
	runProbe := firstProbe || (cycle%c.everyNCycles == 0)
	if runProbe {
		probeCtx := ctx
		if firstProbe {
			// Always run the very first probe so -once mode has an SD metric.
			probeCtx = context.Background()
		}
		if ms, err := c.runProbe(probeCtx); err != nil {
			log.Printf("sd_write_probe collector: probe failed (dir=%s bytes=%d): %v", c.directory, c.bytes, err)
			if firstProbe {
				// Emit a sentinel value so the metric is visible even when first probe fails.
				c.mu.Lock()
				c.lastMS = -1
				c.hasLast = true
				last = -1
				hasLast = true
				c.mu.Unlock()
			}
		} else {
			c.mu.Lock()
			c.lastMS = ms
			c.hasLast = true
			last = ms
			hasLast = true
			c.mu.Unlock()
		}
	}

	if !hasLast {
		return
	}
	// Channel is buffered and only closed after this collector returns.
	// Emit latest value even if ctx is done so first probe is visible in -once mode.
	ch <- Metric{Name: c.metric, Value: last}
}

func (c *SDWriteProbeCollector) runProbe(ctx context.Context) (float32, error) {
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	default:
	}

	if err := os.MkdirAll(c.directory, 0755); err != nil {
		return 0, err
	}

	path := filepath.Join(c.directory, c.fileName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	defer func() {
		_ = os.Remove(path)
	}()

	start := time.Now()
	if _, err := f.Write(c.payload); err != nil {
		return 0, err
	}
	if err := f.Sync(); err != nil {
		return 0, err
	}
	return float32(time.Since(start).Seconds() * 1000.0), nil
}
