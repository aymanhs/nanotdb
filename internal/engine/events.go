package engine

import "fmt"

// MaxEventsPerDatabase is the hard architectural cap on distinct event names
// per database. Unlike MaxMetricsPerDatabase (which spans the full uint16
// address space), the events cap is intentionally smaller because the
// events-<partition>.dat page header carries a fixed-width event-id bitmap
// sized for exactly this range (ceil(1023/8) = 128 bytes per page header).
// Raising the cap would require a coordinated change to the page format,
// WAL replay, and inspect tooling — see docs/EVENTS.md.
const MaxEventsPerDatabase = 1023

// MaxEventNameLen bounds the name length in bytes. The events WAL encodes
// NameLen as uint8 on newEvent records (see docs/EVENTS.md), so 255 is the
// hard ceiling. We mirror the same cap that metrics use (MaxMetricNameLen).
const MaxEventNameLen = 255

// EventID is a compact uint16 identifier assigned to each event name within
// a database. The space is independent of MetricID — no collision because
// events live in their own catalog (events.json) and their own WAL.
type EventID uint16

// Event value-type byte codes, used in both the events catalog and the
// on-disk events WAL/page records.
//
// EventValueNone identifies an event whose only data is the occurrence
// itself (no numeric value, payload optional). Int32Sample and Float32Sample
// are reused from db.go so the byte-code space stays consistent across
// metric and event storage layers.
const EventValueNone byte = 0

// IsValidEventValueType reports whether v is a value-type byte the events
// layer can persist (none, int32, or float32). Strings are intentionally
// not first-class event values; they belong in the payload.
func IsValidEventValueType(v byte) bool {
	return v == EventValueNone || v == Int32Sample || v == Float32Sample
}

// EventValueTypeName returns a human-readable name for a value-type byte.
// Used by inspect/catalog listings and JSON responses.
func EventValueTypeName(v byte) string {
	switch v {
	case EventValueNone:
		return "none"
	case Int32Sample:
		return "int32"
	case Float32Sample:
		return "float32"
	default:
		return fmt.Sprintf("unknown(%d)", v)
	}
}

// ParseEventValueTypeName is the inverse of EventValueTypeName, used when
// loading catalogs that store the type as a readable string. Empty input
// is treated as EventValueNone for forward-compatibility with a hand-edited
// or partially-migrated catalog.
func ParseEventValueTypeName(s string) (byte, error) {
	switch s {
	case "", "none":
		return EventValueNone, nil
	case "int32":
		return Int32Sample, nil
	case "float32":
		return Float32Sample, nil
	default:
		return 0, fmt.Errorf("invalid event value_type %q (must be none|int32|float32)", s)
	}
}

// Event is the in-memory representation of one event occurrence. It is the
// shape produced by QueryEvents and consumed by AddEvent.
type Event struct {
	// Name is the user-supplied event identifier (e.g. "disc.write.slow").
	// Within a database, Name maps to a stable EventID via the events catalog.
	Name string

	// EventID is the compact id assigned to Name. Zero is reserved as an
	// invalid sentinel; valid ids are 1..MaxEventsPerDatabase.
	EventID EventID

	// TS is the event timestamp in Unix nanoseconds. Per-event-name
	// monotonic-non-decreasing rule applies, same as metrics.
	TS Timestamp

	// ValueType is one of EventValueNone, Int32Sample, Float32Sample.
	// Pinned at first write for a given event name and persisted in the
	// events catalog.
	ValueType byte

	// Int32Value carries the value when ValueType == Int32Sample.
	// Float32Value carries the value when ValueType == Float32Sample.
	// Both are zero when ValueType == EventValueNone.
	Int32Value   int32
	Float32Value float32

	// Payload is the opaque bytes the caller attached (typically JSON).
	// nil when no payload was stored. Capped by [events].max_payload_bytes
	// at ingress; never parsed or validated by the engine.
	Payload []byte
}

// EventInfo is a small snapshot of a catalog entry, used by inspect and
// the GET /api/v1/events/catalog response. Mirrors MetricInfo.
type EventInfo struct {
	Name      string
	EventID   EventID
	ValueType byte

	// LastTS / LastValid mirror EventEntry runtime state. LastValid is
	// false until the first accepted append. Useful for inspect/UI to
	// answer "is anything coming in for this event?" without scanning
	// storage.
	LastTS    Timestamp
	LastValid bool
}

// EventEntry is the in-memory catalog record for one event. It holds the
// assigned id, value type, and an in-memory cache of the last accepted
// timestamp so the per-name monotonic ordering rule can be enforced
// without scanning storage on every write. Mirrors MetricEntry.
type EventEntry struct {
	EventID   EventID
	ValueType byte

	// LastTS / LastValid are runtime-only state for the monotonic ordering
	// check. Never persisted to events.json — replay rebuilds them from the
	// events WAL and any sealed events-<partition>.dat content.
	LastTS    Timestamp
	LastValid bool
}

// Errors exposed by the events layer. Mirrors the metric-side error vars
// (ErrTooManyMetrics, ErrOutOfOrderTimestamp).
var (
	// ErrTooManyEvents is returned by the events catalog when an attempt is
	// made to register a new event name and the per-database cap
	// (MaxEventsPerDatabase) has already been reached.
	ErrTooManyEvents = fmt.Errorf("too many events in database")

	// ErrEventTypeMismatch is returned when an AddEvent call's value type
	// does not match the type already pinned for that event name in the
	// catalog.
	ErrEventTypeMismatch = fmt.Errorf("event value type mismatch")

	// ErrEventPayloadTooLarge is returned when the payload exceeds the
	// configured max_payload_bytes. Carrying it as a sentinel lets HTTP /
	// CLI layers map it to a clean 400-class error.
	ErrEventPayloadTooLarge = fmt.Errorf("event payload too large")

	// ErrEventNameTooLong is returned when a registered event name exceeds
	// MaxEventNameLen bytes. Bounded by the uint8 NameLen field in the WAL.
	ErrEventNameTooLong = fmt.Errorf("event name too long")

	// ErrEventNameEmpty is returned when an empty name is supplied at
	// ingest or catalog load time.
	ErrEventNameEmpty = fmt.Errorf("event name cannot be empty")
)
