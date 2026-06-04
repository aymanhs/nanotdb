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

// EventCatalog is the in-memory registry of event names, IDs, and value
// types for one database. It is persisted as events.json next to the
// database data files.
//
// Mirrors Catalog in catalog.go. The two registries are independent —
// EventIDs and MetricIDs do not share namespace.
type EventCatalog struct {
	mu       sync.RWMutex
	file     *os.File
	events   map[string]EventEntry
	idToName map[EventID]string
	dirty    bool
}

type eventDiskEntry struct {
	Name      string  `json:"name"`
	EventID   EventID `json:"id"`
	ValueType string  `json:"value_type"`
}

type eventCatalogDisk struct {
	Events []eventDiskEntry `json:"events"`
}

// LoadEventCatalog opens (creating if missing) the events.json file at the
// given path and returns its in-memory representation. Validation rules on
// load are strict — duplicate names/ids, id=0, invalid value_type strings,
// empty names, and exceeding MaxEventsPerDatabase are all hard errors.
// Mirrors LoadCatalog.
func LoadEventCatalog(filename string) (*EventCatalog, error) {
	if err := os.MkdirAll(filepath.Dir(filename), 0755); err != nil {
		return nil, err
	}

	file, err := os.OpenFile(filename, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return nil, err
	}

	c := &EventCatalog{
		file:     file,
		events:   make(map[string]EventEntry),
		idToName: make(map[EventID]string),
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

	var onDisk eventCatalogDisk
	if err := json.NewDecoder(file).Decode(&onDisk); err != nil {
		file.Close()
		return nil, err
	}

	// Validate every entry on load. Same discipline as LoadCatalog.
	for _, e := range onDisk.Events {
		if e.Name == "" {
			file.Close()
			return nil, fmt.Errorf("event catalog %q: entry with empty name", filename)
		}
		if e.EventID == 0 {
			file.Close()
			return nil, fmt.Errorf("event catalog %q: invalid event id 0 for %q (ids must be >= 1)", filename, e.Name)
		}
		if uint16(e.EventID) > MaxEventsPerDatabase {
			file.Close()
			return nil, fmt.Errorf("event catalog %q: event id %d for %q exceeds MaxEventsPerDatabase (%d)", filename, e.EventID, e.Name, MaxEventsPerDatabase)
		}
		if len(e.Name) > MaxEventNameLen {
			file.Close()
			return nil, fmt.Errorf("event catalog %q: event name %q exceeds MaxEventNameLen (%d)", filename, e.Name, MaxEventNameLen)
		}
		vt, err := ParseEventValueTypeName(e.ValueType)
		if err != nil {
			file.Close()
			return nil, fmt.Errorf("event catalog %q: %q: %w", filename, e.Name, err)
		}
		if _, dup := c.events[e.Name]; dup {
			file.Close()
			return nil, fmt.Errorf("event catalog %q: duplicate event name %q", filename, e.Name)
		}
		if existing, dup := c.idToName[e.EventID]; dup {
			file.Close()
			return nil, fmt.Errorf("event catalog %q: duplicate event id %d (used by %q and %q)", filename, e.EventID, existing, e.Name)
		}
		c.events[e.Name] = EventEntry{EventID: e.EventID, ValueType: vt}
		c.idToName[e.EventID] = e.Name
	}

	if len(c.events) > MaxEventsPerDatabase {
		file.Close()
		return nil, fmt.Errorf("event catalog %q: %d events exceeds MaxEventsPerDatabase (%d)", filename, len(c.events), MaxEventsPerDatabase)
	}

	return c, nil
}

// GetOrAssignEventID returns the EventID for a named event, registering it
// if it has not been seen before. The valueType parameter is the pinned
// value-type byte (EventValueNone, Int32Sample, or Float32Sample).
//
// If the event already exists with a different value type, returns
// ErrEventTypeMismatch. If the per-database cap is reached on a new name,
// returns ErrTooManyEvents.
func (c *EventCatalog) GetOrAssignEventID(name string, valueType byte) (EventID, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if name == "" {
		return 0, ErrEventNameEmpty
	}
	if len(name) > MaxEventNameLen {
		return 0, ErrEventNameTooLong
	}
	if !IsValidEventValueType(valueType) {
		return 0, fmt.Errorf("invalid event value_type byte %d", valueType)
	}

	if e, exists := c.events[name]; exists {
		if e.ValueType != valueType {
			return 0, ErrEventTypeMismatch
		}
		return e.EventID, nil
	}

	if len(c.events) >= MaxEventsPerDatabase {
		return 0, ErrTooManyEvents
	}
	newID := EventID(len(c.events) + 1)
	c.events[name] = EventEntry{EventID: newID, ValueType: valueType}
	c.idToName[newID] = name
	c.dirty = true
	return newID, nil
}

// EnsureEventEntry asserts that (name, id, valueType) is consistent with
// the catalog state, registering a new entry if absent. Used during WAL
// replay to materialize entries from newEvent records.
//
// Mirrors Catalog.EnsureMetricEntry. Per the design's crash-safety contract
// (rule 4 in docs/EVENTS.md), the WAL record is treated as the source of
// truth when the catalog has no prior entry for this id; if a prior entry
// exists, mismatches are hard errors.
func (c *EventCatalog) EnsureEventEntry(name string, id EventID, valueType byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if strings.TrimSpace(name) == "" {
		return ErrEventNameEmpty
	}
	if len(name) > MaxEventNameLen {
		return ErrEventNameTooLong
	}
	if id == 0 || uint16(id) > MaxEventsPerDatabase {
		return fmt.Errorf("event id %d out of range [1..%d]", id, MaxEventsPerDatabase)
	}
	if !IsValidEventValueType(valueType) {
		return fmt.Errorf("invalid event value_type byte %d", valueType)
	}
	if existingName, ok := c.idToName[id]; ok && existingName != name {
		return fmt.Errorf("event id %d already assigned to %q", id, existingName)
	}
	if existing, ok := c.events[name]; ok {
		if existing.EventID != id {
			return fmt.Errorf("event %q id mismatch: got=%d want=%d", name, existing.EventID, id)
		}
		if existing.ValueType != valueType {
			return fmt.Errorf("event %q value_type mismatch: got=%d want=%d", name, existing.ValueType, valueType)
		}
		c.idToName[id] = name
		return nil
	}
	if len(c.events) >= MaxEventsPerDatabase {
		return ErrTooManyEvents
	}
	c.events[name] = EventEntry{EventID: id, ValueType: valueType}
	c.idToName[id] = name
	c.dirty = true
	return nil
}

// GetEventEntry returns the catalog entry for name and whether it exists.
func (c *EventCatalog) GetEventEntry(name string) (EventEntry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	e, ok := c.events[name]
	return e, ok
}

// GetEventByID returns (name, entry, ok) for an EventID. Mirrors
// Catalog.GetMetricByID.
func (c *EventCatalog) GetEventByID(id EventID) (string, EventEntry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	name, ok := c.idToName[id]
	if !ok {
		return "", EventEntry{}, false
	}
	return name, c.events[name], true
}

// GetEventType returns the value-type byte for a given EventID. Returns
// an error if the id is unknown. Mirrors Catalog.GetMetricType.
func (c *EventCatalog) GetEventType(id EventID) (byte, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	name, ok := c.idToName[id]
	if !ok {
		return 0, fmt.Errorf("event id %d not found", id)
	}
	return c.events[name].ValueType, nil
}

// ListEvents returns a stable, name-sorted snapshot of events. Mirrors
// Catalog.ListMetrics.
func (c *EventCatalog) ListEvents() []EventInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()

	out := make([]EventInfo, 0, len(c.events))
	for name, entry := range c.events {
		out = append(out, EventInfo{
			Name:      name,
			EventID:   entry.EventID,
			ValueType: entry.ValueType,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Name < out[j].Name
	})
	return out
}

// UpdateLastByEventID records the most-recent accepted timestamp for an
// event id, used to enforce the per-event-name monotonic ordering rule
// without scanning storage. Runtime-only state — never persisted.
func (c *EventCatalog) UpdateLastByEventID(id EventID, ts Timestamp) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	name, ok := c.idToName[id]
	if !ok {
		return fmt.Errorf("event id %d not found", id)
	}
	entry := c.events[name]
	if !entry.LastValid || ts >= entry.LastTS {
		entry.LastTS = ts
		entry.LastValid = true
		c.events[name] = entry
	}
	return nil
}

// LastTS returns the most-recent accepted timestamp for an event id and
// whether one has been observed. Used to reject out-of-order ingest.
func (c *EventCatalog) LastTS(id EventID) (Timestamp, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	name, ok := c.idToName[id]
	if !ok {
		return 0, false
	}
	entry := c.events[name]
	return entry.LastTS, entry.LastValid
}

// IsDirty reports whether the in-memory state diverges from the on-disk
// events.json. Mirrors Catalog.IsDirty.
func (c *EventCatalog) IsDirty() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.dirty
}

// WriteCatalog persists the events catalog to its canonical on-disk file
// (the path passed to LoadEventCatalog). Clears the dirty flag on success.
// Mirrors Catalog.WriteCatalog including the tmp+fsync+rename+dir-fsync
// atomicity discipline.
func (c *EventCatalog) WriteCatalog() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.file == nil {
		return fmt.Errorf("event catalog file is closed")
	}
	canonical := c.file.Name()
	if err := writeEventCatalogEntries(canonical, c.catalogEntriesLocked()); err != nil {
		return err
	}
	// Re-open after the atomic rename so the file descriptor points at the
	// new inode. Mirrors Catalog.WriteCatalog.
	refreshed, err := os.OpenFile(canonical, os.O_RDWR, 0644)
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

// WriteCatalogTo writes a *snapshot* of the events catalog to an external
// path. Never mutates the canonical file binding and never clears the
// dirty flag. Mirrors Catalog.WriteCatalogTo.
func (c *EventCatalog) WriteCatalogTo(path string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("WriteCatalogTo: path must be non-empty")
	}
	if c.file != nil && filepath.Clean(path) == filepath.Clean(c.file.Name()) {
		return fmt.Errorf("WriteCatalogTo: refusing to overwrite canonical event catalog file %q; use WriteCatalog() instead", path)
	}
	return writeEventCatalogEntries(path, c.catalogEntriesLocked())
}

// Close releases the underlying file descriptor. Safe to call repeatedly.
func (c *EventCatalog) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.file == nil {
		return nil
	}
	err := c.file.Close()
	c.file = nil
	return err
}

func (c *EventCatalog) catalogEntriesLocked() []eventDiskEntry {
	entries := make([]eventDiskEntry, 0, len(c.events))
	for name, e := range c.events {
		entries = append(entries, eventDiskEntry{
			Name:      name,
			EventID:   e.EventID,
			ValueType: EventValueTypeName(e.ValueType),
		})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name < entries[j].Name
	})
	return entries
}

func writeEventCatalogEntries(path string, entries []eventDiskEntry) error {
	payload, err := json.MarshalIndent(eventCatalogDisk{Events: entries}, "", "  ")
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
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}

	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	// Directory fsync (matches catalog.go writeCatalogEntries): without
	// it, on ext3 / ext4 data=writeback / XFS edge cases a crash right
	// after rename can leave the new directory entry unjournaled and
	// the catalog unreachable.
	return syncParentDir(path)
}
