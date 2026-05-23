package collectors

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// CPUCollector reads /proc/stat and emits per-field tick counts.
// Metrics: cpu.user, cpu.nice, cpu.system, cpu.idle, cpu.iowait, cpu.irq, cpu.softirq,
//
//	         cpu.busy_pct, sys.uptime_sec,
//
//		cpu.temp_mdeg (if available)
type CPUCollector struct {
	tempPath   string
	tempMetric string
	mu         sync.Mutex
	lastTotal  uint64
	lastIdle   uint64
	lastIOWait uint64
}

func NewCPUCollector(tempPath, tempMetric string) *CPUCollector {
	if strings.TrimSpace(tempMetric) == "" {
		tempMetric = "cpu.temp_mdeg"
	}
	return &CPUCollector{tempPath: strings.TrimSpace(tempPath), tempMetric: tempMetric}
}

func (c *CPUCollector) Name() string { return "cpu" }

func (c *CPUCollector) Collect(ctx context.Context, ch chan<- Metric) {
	statFields, err := readCPUStatFields()
	if err != nil {
		log.Printf("cpu collector: %v", err)
		return
	}

	names := []string{"user", "nice", "system", "idle", "iowait", "irq", "softirq"}
	for i, name := range names {
		if i >= len(statFields) {
			break
		}
		select {
		case ch <- Metric{Name: fmt.Sprintf("cpu.%s", name), Value: float32(statFields[i])}:
		case <-ctx.Done():
			return
		}
	}

	total := sumCPUStatFields(statFields)
	idle := uint64(0)
	iowait := uint64(0)
	if len(statFields) > 3 {
		idle = statFields[3]
	}
	if len(statFields) > 4 {
		iowait = statFields[4]
	}
	if busyPct, ok := c.computeBusyPct(total, idle, iowait); ok {
		select {
		case ch <- Metric{Name: "cpu.busy_pct", Value: busyPct}:
		case <-ctx.Done():
			return
		}
	}

	if uptimeSec, ok := readSystemUptimeSeconds(); ok {
		select {
		case ch <- Metric{Name: "sys.uptime_sec", Value: uptimeSec}:
		case <-ctx.Done():
			return
		}
	}

	if temp, ok := readCPUTempMilliDegrees(c.tempPath); ok {
		select {
		case ch <- Metric{Name: c.tempMetric, Value: temp}:
		case <-ctx.Done():
			return
		}
	} else if c.tempPath != "" {
		log.Printf("cpu collector: no temperature from configured path %s", c.tempPath)
	}

	if freq, ok := readCPUFreqKHz(); ok {
		select {
		case ch <- Metric{Name: "cpu.freq_khz", Value: int32(freq)}:
		case <-ctx.Done():
			return
		}
	}
}

func (c *CPUCollector) computeBusyPct(total, idle, iowait uint64) (float32, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lastTotal == 0 || total <= c.lastTotal {
		c.lastTotal = total
		c.lastIdle = idle
		c.lastIOWait = iowait
		return 0, false
	}
	deltaTotal := total - c.lastTotal
	deltaIdle := uint64(0)
	if idle >= c.lastIdle {
		deltaIdle = idle - c.lastIdle
	}
	deltaIOWait := uint64(0)
	if iowait >= c.lastIOWait {
		deltaIOWait = iowait - c.lastIOWait
	}
	c.lastTotal = total
	c.lastIdle = idle
	c.lastIOWait = iowait
	if deltaTotal == 0 {
		return 0, false
	}
	idleLike := deltaIdle + deltaIOWait
	if idleLike > deltaTotal {
		idleLike = deltaTotal
	}
	busyTicks := deltaTotal - idleLike
	return float32(float64(busyTicks) * 100 / float64(deltaTotal)), true
}

func readCPUStatFields() ([]uint64, error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return nil, fmt.Errorf("open /proc/stat: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return nil, fmt.Errorf("parse /proc/stat cpu line: missing fields")
		}
		values := make([]uint64, 0, len(fields)-1)
		for _, field := range fields[1:] {
			v, err := strconv.ParseUint(field, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("parse /proc/stat cpu field %q: %w", field, err)
			}
			values = append(values, v)
		}
		return values, nil
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan /proc/stat: %w", err)
	}
	return nil, fmt.Errorf("cpu line not found in /proc/stat")
}

func sumCPUStatFields(fields []uint64) uint64 {
	var total uint64
	for _, field := range fields {
		total += field
	}
	return total
}

func readSystemUptimeSeconds() (int32, bool) {
	raw, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, false
	}
	fields := strings.Fields(string(raw))
	if len(fields) == 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, false
	}
	if v < 0 {
		return 0, false
	}
	if v > float64(^uint32(0)>>1) {
		v = float64(^uint32(0) >> 1)
	}
	return int32(v), true
}

// readCPUFreqKHz reads the current CPU frequency for cpu0 from cpufreq sysfs.
// Returns the frequency in kHz and true on success.
func readCPUFreqKHz() (int64, bool) {
	paths := []string{
		"/sys/devices/system/cpu/cpu0/cpufreq/scaling_cur_freq",
		"/sys/devices/system/cpu/cpu0/cpufreq/cpuinfo_cur_freq",
	}
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		v, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
		if err != nil {
			continue
		}
		return v, true
	}
	return 0, false
}

func readCPUTempMilliDegrees(preferredPath string) (int32, bool) {
	paths := make([]string, 0, 2)
	if strings.TrimSpace(preferredPath) != "" {
		paths = append(paths, preferredPath)
	}
	paths = append(paths, "/sys/class/thermal/thermal_zone0/temp")

	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err == nil {
			v, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 32)
			if err != nil {
				continue
			}
			return int32(v), true
		}
	}

	// Fallback: find any thermal_zone*/temp
	matches, err := filepath.Glob("/sys/class/thermal/thermal_zone*/temp")
	if err != nil {
		return 0, false
	}
	for _, p := range matches {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		v, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 32)
		if err != nil {
			continue
		}
		return int32(v), true
	}
	return 0, false
}
