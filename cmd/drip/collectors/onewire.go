package collectors

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const defaultOneWireBasePath = "/sys/bus/w1/devices"

var errTemperatureFileEmpty = errors.New("temperature file is empty")

// OneWireCollector reads DS18B20-family temperature sensors from
// the Linux 1-Wire sysfs interface (/sys/bus/w1/devices).
// Devices are mapped to friendly metric names via configuration.
// Metrics: temp.{friendly_name}.mdeg (int32, millidegrees Celsius)
type OneWireCollector struct {
	// devices maps 1-Wire device IDs (e.g. "28-0000xxxxxxxx")
	// to friendly metric name suffixes (e.g. "outdoor").
	devices      map[string]string
	autoDiscover bool
	basePath     string
	maxValidMdeg int32

	mu              sync.Mutex
	emptyReadMisses map[string]int
}

func NewOneWireCollector(devices map[string]string, autoDiscover bool, basePath string, maxValidMdeg int32) *OneWireCollector {
	if strings.TrimSpace(basePath) == "" {
		basePath = defaultOneWireBasePath
	}
	return &OneWireCollector{devices: devices, autoDiscover: autoDiscover, basePath: basePath, maxValidMdeg: maxValidMdeg, emptyReadMisses: make(map[string]int)}
}

func (c *OneWireCollector) Name() string { return "onewire" }

func (c *OneWireCollector) Collect(ctx context.Context, ch chan<- Metric) {
	discovered, err := c.discoverDeviceIDs()
	if err != nil {
		log.Printf("onewire collector: discover devices: %v", err)
	}
	discoveredSet := make(map[string]struct{}, len(discovered))
	for _, id := range discovered {
		discoveredSet[id] = struct{}{}
	}

	targets := make(map[string]string, len(c.devices)+len(discovered))
	for id, name := range c.devices {
		normID := normalizeDeviceID(id)
		if normID == "" {
			continue
		}
		targets[normID] = strings.TrimSpace(name)
	}

	if c.autoDiscover || len(targets) == 0 {
		for _, id := range discovered {
			if _, ok := targets[id]; !ok {
				targets[id] = id
			}
		}
	}

	if len(targets) == 0 {
		log.Printf("onewire collector: no devices configured or discovered under %s", c.basePath)
		return
	}

	discoveredList := strings.Join(discovered, ",")
	for id, name := range c.devices {
		normID := normalizeDeviceID(id)
		if normID == "" {
			continue
		}
		if _, ok := discoveredSet[normID]; !ok {
			if discoveredList == "" {
				discoveredList = "none"
			}
			log.Printf("onewire collector: configured device not found: id=%s name=%s discovered=%s", normID, name, discoveredList)
		}
	}

	orderedIDs := make([]string, 0, len(targets))
	for deviceID := range targets {
		orderedIDs = append(orderedIDs, deviceID)
	}
	sort.Strings(orderedIDs)

	var wg sync.WaitGroup
	var mu sync.Mutex
	received := 0
	emitted := 0

	for _, deviceID := range orderedIDs {
		wg.Add(1)
		go func(deviceID string) {
			defer wg.Done()
			friendlyName := targets[deviceID]

			tempMdeg, err := readOneWireTempMilliDegrees(c.basePath, deviceID)
			mu.Lock()
			received++
			mu.Unlock()

			if err != nil {
				if errors.Is(err, errTemperatureFileEmpty) {
					if c.shouldLogEmptyRead(friendlyName) {
						log.Printf("onewire collector: %s: transient empty temperature read; retrying", friendlyName)
					}
					return
				}
				log.Printf("onewire collector: %s: %v", friendlyName, err)
				return
			}
			c.clearEmptyReadMiss(friendlyName)
			if c.maxValidMdeg > 0 && tempMdeg > c.maxValidMdeg {
				log.Printf("onewire collector: %s: skipping outlier value=%d (max_valid_mdeg=%d)", friendlyName, tempMdeg, c.maxValidMdeg)
				return
			}

			select {
			case ch <- Metric{Name: fmt.Sprintf("temp.%s.mdeg", friendlyName), Value: tempMdeg}:
				mu.Lock()
				emitted++
				mu.Unlock()
			case <-ctx.Done():
				return
			}
		}(deviceID)
	}

	wg.Wait()
}

func (c *OneWireCollector) discoverDeviceIDs() ([]string, error) {
	outSet := make(map[string]struct{})

	// Prefer concrete sensor files; this avoids listing non-sensor dirs.
	patterns := []string{
		filepath.Join(c.basePath, "28-*", "w1_slave"),
		filepath.Join(c.basePath, "28-*", "temperature"),
	}
	for _, pat := range patterns {
		matches, err := filepath.Glob(pat)
		if err != nil {
			return nil, err
		}
		for _, p := range matches {
			id := normalizeDeviceID(filepath.Base(filepath.Dir(p)))
			if id != "" {
				outSet[id] = struct{}{}
			}
		}
	}

	// Fallback to directory listing.
	if len(outSet) == 0 {
		ents, err := os.ReadDir(c.basePath)
		if err != nil {
			return nil, err
		}
		for _, ent := range ents {
			if !ent.IsDir() {
				continue
			}
			id := normalizeDeviceID(ent.Name())
			if id != "" {
				outSet[id] = struct{}{}
			}
		}
	}

	out := make([]string, 0, len(outSet))
	for id := range outSet {
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

func normalizeDeviceID(id string) string {
	s := strings.ToLower(strings.TrimSpace(id))
	if strings.HasPrefix(s, "28-") {
		return s
	}
	return ""
}

// readOneWireTempMilliDegrees reads temperature in millidegrees Celsius.
// Path: /sys/bus/w1/devices/{deviceID}/temperature (raw millidegrees Celsius)
// Falls back to parsing w1_slave file if temperature sysfs entry is absent.
func readOneWireTempMilliDegrees(basePath, deviceID string) (int32, error) {
	// Modern kernels expose a direct "temperature" file (millidegrees Celsius)
	tempPath := filepath.Join(basePath, deviceID, "temperature")
	if raw, err := os.ReadFile(tempPath); err == nil {
		v, err := parseTemperatureMilliDegrees(raw)
		if err != nil {
			return 0, fmt.Errorf("parse temperature file: %w", err)
		}
		return v, nil
	}

	// Older kernels: parse w1_slave text format
	slavePath := filepath.Join(basePath, deviceID, "w1_slave")
	raw, err := os.ReadFile(slavePath)
	if err != nil {
		return 0, fmt.Errorf("read w1_slave: %w", err)
	}
	return parseW1Slave(string(raw))
}

func parseTemperatureMilliDegrees(raw []byte) (int32, error) {
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return 0, errTemperatureFileEmpty
	}
	v, err := strconv.ParseInt(s, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("temperature file has non-integer value %q", s)
	}
	return int32(v), nil
}

func (c *OneWireCollector) shouldLogEmptyRead(sensor string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.emptyReadMisses[sensor]++
	n := c.emptyReadMisses[sensor]
	return n == 1 || n%60 == 0
}

func (c *OneWireCollector) clearEmptyReadMiss(sensor string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.emptyReadMisses, sensor)
}

// parseW1Slave parses the w1_slave file format:
//
//	50 05 4b 46 7f ff 0c 10 1c : crc=1c YES
//	50 05 4b 46 7f ff 0c 10 1c t=21250
func parseW1Slave(content string) (int32, error) {
	for _, line := range strings.Split(content, "\n") {
		idx := strings.Index(line, "t=")
		if idx < 0 {
			continue
		}
		val := strings.TrimSpace(line[idx+2:])
		v, err := strconv.ParseInt(val, 10, 32)
		if err != nil {
			return 0, fmt.Errorf("parse t= value: %w", err)
		}
		return int32(v), nil
	}
	return 0, fmt.Errorf("t= value not found in w1_slave")
}
