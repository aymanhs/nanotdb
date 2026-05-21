package collectors

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
)

type ProcessCollector struct {
	mu          sync.Mutex
	exeNames    []string
	nameSet     map[string]struct{}
	lastProcCPU map[string]uint64
	lastSysCPU  uint64
	pageSize    uint64
	numCPU      float64
}

type processSnapshot struct {
	count      int
	procTicks  uint64
	rssBytes   uint64
	vsizeBytes uint64
}

func NewProcessCollector(exeNames []string) *ProcessCollector {
	seen := make(map[string]struct{}, len(exeNames))
	normalized := make([]string, 0, len(exeNames))
	for _, name := range exeNames {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		normalized = append(normalized, name)
	}
	sort.Strings(normalized)
	return &ProcessCollector{
		exeNames:    normalized,
		nameSet:     seen,
		lastProcCPU: make(map[string]uint64, len(normalized)),
		pageSize:    uint64(os.Getpagesize()),
		numCPU:      float64(runtime.NumCPU()),
	}
}

func (c *ProcessCollector) Name() string { return "process" }

func (c *ProcessCollector) Collect(ctx context.Context, ch chan<- Metric) {
	if len(c.exeNames) == 0 {
		return
	}

	sysTicks, err := readSystemCPUTicks()
	if err != nil {
		log.Printf("process collector: read /proc/stat: %v", err)
		return
	}

	snapshots, err := c.collectSnapshots()
	if err != nil {
		log.Printf("process collector: collect snapshots: %v", err)
		return
	}

	c.mu.Lock()
	prevSysTicks := c.lastSysCPU
	for _, exe := range c.exeNames {
		snap := snapshots[exe]
		metricPrefix := "proc." + sanitizeMetricSegment(exe)
		cpuPct := float32(0)
		if prevSysTicks > 0 && sysTicks > prevSysTicks && c.numCPU > 0 {
			if prevProcTicks, ok := c.lastProcCPU[exe]; ok && snap.procTicks >= prevProcTicks {
				deltaProc := float64(snap.procTicks - prevProcTicks)
				deltaSys := float64(sysTicks - prevSysTicks)
				cpuPct = float32((deltaProc / deltaSys) * c.numCPU * 100)
			}
		}
		c.lastProcCPU[exe] = snap.procTicks
		metrics := []Metric{
			{Name: metricPrefix + ".count", Value: float32(snap.count)},
			{Name: metricPrefix + ".rss_bytes", Value: float32(snap.rssBytes)},
			{Name: metricPrefix + ".vsize_bytes", Value: float32(snap.vsizeBytes)},
			{Name: metricPrefix + ".cpu_pct", Value: cpuPct},
		}
		for _, metric := range metrics {
			select {
			case ch <- metric:
			case <-ctx.Done():
				c.lastSysCPU = sysTicks
				c.mu.Unlock()
				return
			}
		}
	}
	c.lastSysCPU = sysTicks
	c.mu.Unlock()
}

func (c *ProcessCollector) collectSnapshots() (map[string]processSnapshot, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	out := make(map[string]processSnapshot, len(c.exeNames))
	for _, exe := range c.exeNames {
		out[exe] = processSnapshot{}
	}
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		pid := ent.Name()
		if _, err := strconv.Atoi(pid); err != nil {
			continue
		}
		exeName, ok := processExecutableName(pid)
		if !ok {
			continue
		}
		if _, want := c.nameSet[exeName]; !want {
			continue
		}
		procTicks, err := readProcessCPUTicks(pid)
		if err != nil {
			continue
		}
		vsizeBytes, rssBytes, err := readProcessMemoryBytes(pid, c.pageSize)
		if err != nil {
			continue
		}
		snap := out[exeName]
		snap.count++
		snap.procTicks += procTicks
		snap.rssBytes += rssBytes
		snap.vsizeBytes += vsizeBytes
		out[exeName] = snap
	}
	return out, nil
}

func processExecutableName(pid string) (string, bool) {
	path := filepath.Join("/proc", pid, "exe")
	if target, err := os.Readlink(path); err == nil {
		name := strings.TrimSpace(filepath.Base(target))
		if name != "" {
			return name, true
		}
	}
	raw, err := os.ReadFile(filepath.Join("/proc", pid, "comm"))
	if err != nil {
		return "", false
	}
	name := strings.TrimSpace(string(raw))
	if name == "" {
		return "", false
	}
	return name, true
}

func readSystemCPUTicks() (uint64, error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)
		var total uint64
		for _, field := range fields[1:] {
			v, err := strconv.ParseUint(field, 10, 64)
			if err != nil {
				return 0, err
			}
			total += v
		}
		return total, nil
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("cpu line not found in /proc/stat")
}

func readProcessCPUTicks(pid string) (uint64, error) {
	raw, err := os.ReadFile(filepath.Join("/proc", pid, "stat"))
	if err != nil {
		return 0, err
	}
	return parseProcStatTicks(string(raw))
}

func parseProcStatTicks(stat string) (uint64, error) {
	end := strings.LastIndex(stat, ")")
	if end < 0 || end+2 >= len(stat) {
		return 0, fmt.Errorf("invalid /proc stat format")
	}
	fields := strings.Fields(stat[end+2:])
	if len(fields) <= 12 {
		return 0, fmt.Errorf("short /proc stat payload")
	}
	utime, err := strconv.ParseUint(fields[11], 10, 64)
	if err != nil {
		return 0, err
	}
	stime, err := strconv.ParseUint(fields[12], 10, 64)
	if err != nil {
		return 0, err
	}
	return utime + stime, nil
}

func readProcessMemoryBytes(pid string, pageSize uint64) (uint64, uint64, error) {
	raw, err := os.ReadFile(filepath.Join("/proc", pid, "statm"))
	if err != nil {
		return 0, 0, err
	}
	return parseStatmBytes(string(raw), pageSize)
}

func parseStatmBytes(statm string, pageSize uint64) (uint64, uint64, error) {
	fields := strings.Fields(statm)
	if len(fields) < 2 {
		return 0, 0, fmt.Errorf("short /proc statm payload")
	}
	sizePages, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return 0, 0, err
	}
	rssPages, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0, 0, err
	}
	return sizePages * pageSize, rssPages * pageSize, nil
}

func sanitizeMetricSegment(name string) string {
	name = strings.TrimSpace(strings.ToLower(name))
	if name == "" {
		return "unknown"
	}
	var b strings.Builder
	b.Grow(len(name))
	lastUnderscore := false
	for _, r := range name {
		keep := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if keep {
			b.WriteRune(r)
			lastUnderscore = false
			continue
		}
		if !lastUnderscore {
			b.WriteByte('_')
			lastUnderscore = true
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		return "unknown"
	}
	return out
}
