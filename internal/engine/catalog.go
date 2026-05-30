package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Catalog is the in-memory registry of metric names, IDs, and value types for one database.
// It is persisted as catalog.json next to the database data files.
type Catalog struct {
	mu       sync.RWMutex
	file     *os.File
	metrics  map[string]MetricEntry
	idToName map[int16]string // reverse index for O(1) GetMetricByID
	dirty    bool
}

type MetricInfo struct {
	Name      string
	MetricID  MetricID
	ValueType byte
}

// MetricEntry holds the catalog record for one metric: its assigned ID, value type,
// and an in-memory cache of the last written sample (for QueryLast).
type MetricEntry struct {
	MetricID  MetricID
	ValueType byte

	// In-memory only cache for QueryLast. Never persisted to catalog JSON.
	LastTS    Timestamp
	LastRaw   [4]byte
	LastValid bool
}

type metricDiskEntry struct {
	Name      string   `json:"name"`
	MetricID  MetricID `json:"id"`
	ValueType byte     `json:"type"`
}

type catalogDisk struct {
	Metrics []metricDiskEntry `json:"metrics"`
}

// GetMetricID returns the MetricID for a named metric, registering it if it has not been seen before.
// The type parameter T determines whether an Int32Sample or Float32Sample entry is created.
// Returns an error if the metric already exists with a different value type, or if the
// per-database metric limit (65535) has been reached.
func GetMetricID[T SampleType](c *Catalog, name string) (MetricID, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var zero T
	var want byte
	switch any(zero).(type) {
	case int32:
		want = Int32Sample
	case float32:
		want = Float32Sample
	}

	if m, exists := c.metrics[name]; exists {
		if m.ValueType != want {
			return 0, fmt.Errorf("metric type mismatch")
		}
		return m.MetricID, nil
	}

	if len(c.metrics) >= MaxMetricsPerDatabase {
		return 0, ErrTooManyMetrics
	}
	newID := MetricID(len(c.metrics) + 1)
	c.metrics[name] = MetricEntry{MetricID: newID, ValueType: want}
	c.idToName[int16(newID)] = name
	c.dirty = true
	return newID, nil
}

func (c *Catalog) WriteCatalog() error {
	return c.writeCatalogToPath("")
}

func (c *Catalog) WriteCatalogTo(path string) error {
	return c.writeCatalogToPath(path)
}

func (c *Catalog) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.file == nil {
		return nil
	}
	err := c.file.Close()
	c.file = nil
	return err
}

func (c *Catalog) writeCatalogToPath(path string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.dirty {
		if strings.TrimSpace(path) != "" && c.file != nil && filepath.Clean(path) != filepath.Clean(c.file.Name()) {
			return writeCatalogEntries(path, c.catalogEntriesLocked())
		}
		return nil
	}
	if strings.TrimSpace(path) == "" {
		if c.file == nil {
			return fmt.Errorf("catalog file is closed")
		}
		path = c.file.Name()
	}
	if err := writeCatalogEntries(path, c.catalogEntriesLocked()); err != nil {
		return err
	}
	if c.file != nil && filepath.Clean(path) == filepath.Clean(c.file.Name()) {
		refreshed, err := os.OpenFile(path, os.O_RDWR, 0644)
		if err != nil {
			return err
		}
		if err := c.file.Close(); err != nil {
			refreshed.Close()
			return err
		}
		c.file = refreshed
		c.dirty = false
		return nil
	}
	if c.file != nil && filepath.Clean(path) != filepath.Clean(c.file.Name()) {
		return nil
	}
	refreshed, err := os.OpenFile(path, os.O_RDWR, 0644)
	if err != nil {
		return err
	}
	c.file = refreshed
	c.dirty = false
	return nil
}

func (c *Catalog) catalogEntriesLocked() []metricDiskEntry {
	entries := make([]metricDiskEntry, 0, len(c.metrics))
	for name, m := range c.metrics {
		entries = append(entries, metricDiskEntry{
			Name:      name,
			MetricID:  m.MetricID,
			ValueType: m.ValueType,
		})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name < entries[j].Name
	})
	return entries
}

func writeCatalogEntries(path string, entries []metricDiskEntry) error {
	payload, err := json.MarshalIndent(catalogDisk{Metrics: entries}, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	tmpPath := path + ".tmp"
	tmp, err := os.OpenFile(tmpPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}

	if _, err := tmp.Write(payload); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	return nil
}

func (c *Catalog) IsDirty() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.dirty
}

// GetMetricType returns the value type (Int32Sample or Float32Sample) for a given metric.
func (c *Catalog) GetMetricType(mid MetricID) (byte, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	for _, entry := range c.metrics {
		if entry.MetricID == mid {
			return entry.ValueType, nil
		}
	}
	return 0, fmt.Errorf("metric %d not found", mid)
}

// GetValueWidth returns the number of bytes per value for this metric.
// Currently all metrics are 4 bytes (int32 or float32).
func (c *Catalog) GetValueWidth(mid MetricID) int {
	return 4
}

func (c *Catalog) GetMetricEntry(name string) (MetricEntry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	m, ok := c.metrics[name]
	return m, ok
}

// ListMetrics returns a stable, name-sorted snapshot of metrics in this catalog.
func (c *Catalog) ListMetrics() []MetricInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()

	out := make([]MetricInfo, 0, len(c.metrics))
	for name, entry := range c.metrics {
		out = append(out, MetricInfo{
			Name:      name,
			MetricID:  entry.MetricID,
			ValueType: entry.ValueType,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Name < out[j].Name
	})
	return out
}

func (c *Catalog) EnsureMetricEntry(name string, mid MetricID, valueType byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("metric name cannot be empty")
	}
	if existingName, ok := c.idToName[int16(mid)]; ok && existingName != name {
		return fmt.Errorf("metric id %d already assigned to %q", mid, existingName)
	}
	if existing, ok := c.metrics[name]; ok {
		if existing.MetricID != mid {
			return fmt.Errorf("metric %q id mismatch: got=%d want=%d", name, existing.MetricID, mid)
		}
		if existing.ValueType != valueType {
			return fmt.Errorf("metric %q type mismatch: got=%d want=%d", name, existing.ValueType, valueType)
		}
		c.idToName[int16(mid)] = name
		return nil
	}
	c.metrics[name] = MetricEntry{MetricID: mid, ValueType: valueType}
	c.idToName[int16(mid)] = name
	c.dirty = true
	return nil
}

func (c *Catalog) GetMetricByID(mid MetricID) (string, MetricEntry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	name, ok := c.idToName[int16(mid)]
	if !ok {
		return "", MetricEntry{}, false
	}
	entries := c.metrics[name]
	return name, entries, true
}

func (c *Catalog) UpdateLastByMetricID(mid MetricID, ts Timestamp, raw []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(raw) != 4 {
		return fmt.Errorf("invalid sample width: got=%d want=4", len(raw))
	}
	name, ok := c.idToName[int16(mid)]
	if !ok {
		return fmt.Errorf("metric %d not found", mid)
	}
	entry := c.metrics[name]
	if !entry.LastValid || ts >= entry.LastTS {
		copy(entry.LastRaw[:], raw)
		entry.LastTS = ts
		entry.LastValid = true
		c.metrics[name] = entry
	}
	return nil
}

func LoadCatalog(filename string) (*Catalog, error) {
	if err := os.MkdirAll(filepath.Dir(filename), 0755); err != nil {
		return nil, err
	}

	file, err := os.OpenFile(filename, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return nil, err
	}

	c := &Catalog{
		file:     file,
		metrics:  make(map[string]MetricEntry),
		idToName: make(map[int16]string),
		dirty:    false,
	}

	stat, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	if stat.Size() == 0 {
		return c, nil
	}

	if _, err := file.Seek(0, 0); err != nil {
		file.Close()
		return nil, err
	}

	var onDisk catalogDisk
	if err := json.NewDecoder(file).Decode(&onDisk); err != nil {
		file.Close()
		return nil, err
	}

	for _, m := range onDisk.Metrics {
		c.metrics[m.Name] = MetricEntry{MetricID: m.MetricID, ValueType: m.ValueType}
		c.idToName[int16(m.MetricID)] = m.Name
	}

	return c, nil
}
