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
)

// CPUCollector reads /proc/stat and emits per-field tick counts.
// Metrics: cpu.user, cpu.nice, cpu.system, cpu.idle, cpu.iowait, cpu.irq, cpu.softirq,
//
//	cpu.temp_mdeg (if available)
type CPUCollector struct {
	tempPath   string
	tempMetric string
}

func NewCPUCollector(tempPath, tempMetric string) *CPUCollector {
	if strings.TrimSpace(tempMetric) == "" {
		tempMetric = "cpu.temp_mdeg"
	}
	return &CPUCollector{tempPath: strings.TrimSpace(tempPath), tempMetric: tempMetric}
}

func (c *CPUCollector) Name() string { return "cpu" }

func (c *CPUCollector) Collect(ctx context.Context, ch chan<- Metric) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		log.Printf("cpu collector: open /proc/stat: %v", err)
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)
		// fields: cpu user nice system idle iowait irq softirq [steal guest guest_nice]
		names := []string{"user", "nice", "system", "idle", "iowait", "irq", "softirq"}
		for i, name := range names {
			if i+1 >= len(fields) {
				break
			}
			v, err := strconv.ParseInt(fields[i+1], 10, 64)
			if err != nil {
				log.Printf("cpu collector: parse %s: %v", name, err)
				continue
			}
			select {
			case ch <- Metric{Name: fmt.Sprintf("cpu.%s", name), Value: float32(v)}:
			case <-ctx.Done():
				return
			}
		}
		break // only aggregate "cpu " line
	}
	if err := scanner.Err(); err != nil {
		log.Printf("cpu collector: scan /proc/stat: %v", err)
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
