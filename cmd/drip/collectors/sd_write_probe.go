package collectors

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// tmpfsMagic is the f_type returned by statfs for a Linux tmpfs
// mount. Used by the constructor to warn when the probe directory is
// in RAM — a near-universal misconfiguration on Raspberry Pi where
// the default "/tmp" is tmpfs and an fsync there is a no-op (so the
// probe never measures real SD latency and the over-threshold event
// never fires).
const tmpfsMagic = 0x01021994

// SDWriteProbeCollector measures write+fsync latency in milliseconds by writing
// a small probe file periodically. It emits the latest measured value each cycle.
type SDWriteProbeCollector struct {
	directory       string
	bytes           int
	everyNCycles    int
	metric          string
	eventWhenOverMS float32
	eventName       string

	mu       sync.Mutex
	cycle    int
	hasLast  bool
	lastMS   float32
	payload  []byte
	fileName string
}

func NewSDWriteProbeCollector(directory string, bytes int, everyNCycles int, metric string, eventWhenOverMS int, eventName string) *SDWriteProbeCollector {
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
	eventName = strings.TrimSpace(eventName)
	if eventName == "" {
		eventName = metric + ".slow"
	}
	if eventWhenOverMS < 0 {
		eventWhenOverMS = 0
	}
	if err := warnIfTmpfs(dir); err != nil {
		// Non-fatal — the probe still runs, but the log line makes it
		// obvious the latency reading is meaningless and the
		// threshold event will never fire.
		log.Printf("sd_write_probe collector: WARN: %v", err)
	}
	log.Printf("sd_write_probe collector: configured directory=%s bytes=%d every_n_cycles=%d metric=%s event_name=%s event_when_over_ms=%d",
		dir, bytes, everyNCycles, metric, eventName, eventWhenOverMS)
	return &SDWriteProbeCollector{
		directory:       dir,
		bytes:           bytes,
		everyNCycles:    everyNCycles,
		metric:          metric,
		eventWhenOverMS: float32(eventWhenOverMS),
		eventName:       eventName,
		payload:         make([]byte, bytes),
		fileName:        ".drip-sd-write-probe.tmp",
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
	// freshProbe stays true only when this cycle actually ran a new
	// probe (vs. emitting the cached value between probes). The
	// over-threshold event must fire only on fresh readings — otherwise
	// the same stale measurement gets re-fired every cycle for the
	// every_n_cycles window, drowning the event log in duplicates of
	// the same value (the bug this guard fixes).
	freshProbe := false
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
			freshProbe = true
		}
	}

	if !hasLast {
		return
	}
	if freshProbe && c.eventWhenOverMS > 0 && last > c.eventWhenOverMS {
		ch <- Metric{
			Name:        c.metric,
			Value:       last,
			EmitAsEvent: true,
			EventName:   c.eventName,
			EventPayload: map[string]any{
				"metric":            c.metric,
				"latency_ms":        last,
				"threshold_over_ms": c.eventWhenOverMS,
			},
		}
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

// warnIfTmpfs returns a descriptive error when dir resides on a
// tmpfs (in-RAM) filesystem. Callers log this as a warning, not a
// hard failure — the probe will still run; the operator just
// needs to know that the readings have nothing to do with the SD
// card. Best-effort: if statfs fails (non-Linux, missing path, etc.)
// we silently return nil.
func warnIfTmpfs(dir string) error {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return nil
	}
	if int64(st.Type) == tmpfsMagic {
		return &tmpfsWarning{dir: dir}
	}
	return nil
}

type tmpfsWarning struct{ dir string }

func (w *tmpfsWarning) Error() string {
	return "directory " + w.dir + " is on tmpfs (in-RAM); fsync is a no-op there so the probe will measure RAM latency, not SD I/O — the over-threshold event will never fire. Point [collectors.sd_write_probe].directory at an SD-backed path (e.g. /home/pi)."
}
