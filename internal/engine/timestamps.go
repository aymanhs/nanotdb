package engine

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// TimestampUnit identifies the unit of a bare numeric timestamp. The zero value
// (or "") is treated as nanoseconds — bare integers are assumed to be ns unless
// the caller has explicitly opted into a different unit (e.g. via a CLI flag or
// HTTP query parameter).
const (
	TimestampUnitNanoseconds  = "ns"
	TimestampUnitMicroseconds = "us"
	TimestampUnitMilliseconds = "ms"
	TimestampUnitSeconds      = "s"
)

// NormalizeTimestampUnit validates and normalizes a user-supplied unit value.
// Empty input maps to "ns". Returns an error on unknown values.
func NormalizeTimestampUnit(unit string) (string, error) {
	u := strings.ToLower(strings.TrimSpace(unit))
	switch u {
	case "":
		return TimestampUnitNanoseconds, nil
	case TimestampUnitNanoseconds, TimestampUnitMicroseconds, TimestampUnitMilliseconds, TimestampUnitSeconds:
		return u, nil
	}
	return "", fmt.Errorf("invalid timestamp unit: %q (expected one of: ns, us, ms, s)", unit)
}

// timestampUnitScale returns the number of nanoseconds in one unit.
func timestampUnitScale(unit string) int64 {
	switch unit {
	case TimestampUnitSeconds:
		return int64(time.Second)
	case TimestampUnitMilliseconds:
		return int64(time.Millisecond)
	case TimestampUnitMicroseconds:
		return int64(time.Microsecond)
	default:
		return 1
	}
}

// ParseTimestampWithUnit parses a timestamp string using the given unit for
// bare numeric values. Human-readable forms (RFC3339 / "2006-01-02 ..." /
// "YYYY-MM-DD" etc.) are always interpreted with their textual semantics
// regardless of unit. Bare integers and floats are multiplied by the unit's
// scale in nanoseconds.
//
// Unit "" defaults to nanoseconds.
func ParseTimestampWithUnit(input string, unit string) (Timestamp, error) {
	u, err := NormalizeTimestampUnit(unit)
	if err != nil {
		return 0, err
	}
	v := strings.TrimSpace(input)
	if v == "" {
		return 0, fmt.Errorf("timestamp cannot be empty")
	}
	if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
		return Timestamp(t.UnixNano()), nil
	}
	if t, err := time.Parse("2006-01-02 15:04:05.999999999", v); err == nil {
		return Timestamp(t.UTC().UnixNano()), nil
	}
	scale := timestampUnitScale(u)
	if strings.Contains(v, ".") {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid timestamp %q", v)
		}
		return Timestamp(int64(f * float64(scale))), nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err == nil {
		return Timestamp(n * scale), nil
	}
	// Fall through to the human-readable parser (handles "YYYY-MM-DD",
	// "YYYY-MM-DD HH:MM", etc.). Unit is irrelevant in that path.
	return ParseTimestamp(v)
}

// FormatTimestamp converts a nanosecond Unix timestamp to human-readable UTC format.
// Format: YYYY-MM-DD HH:MM:SS.nnnnnnnnn
func FormatTimestamp(ts Timestamp) string {
	sec := int64(ts) / 1_000_000_000
	nsec := int64(ts) % 1_000_000_000
	t := time.Unix(sec, nsec).UTC()
	return t.Format("2006-01-02 15:04:05") + fmt.Sprintf(".%09d", nsec)
}

// ParseTimestamp converts a human-readable timestamp or nanosecond value to Timestamp.
// Accepts formats:
//   - Raw nanosecond integer: "1234567890123456789"
//   - Date only: "2026-05-14" (time set to 00:00:00 UTC)
//   - Date and time: "2026-05-14 12:34:56" (nanoseconds set to 0)
//   - Date, time, and nanos: "2026-05-14 12:34:56.123456789"
func ParseTimestamp(input string) (Timestamp, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return 0, fmt.Errorf("timestamp cannot be empty")
	}

	// Try parsing as a raw nanosecond integer
	if ns, err := strconv.ParseInt(input, 10, 64); err == nil {
		// Successfully parsed as integer, treat as nanoseconds
		return Timestamp(ns), nil
	}

	// Try parsing as human-readable format
	// First, split into date/time part and nanoseconds part
	var baseFmt string
	var nanoStr string
	if dotIdx := strings.LastIndex(input, "."); dotIdx != -1 {
		baseFmt = input[:dotIdx]
		nanoStr = input[dotIdx+1:]
	} else {
		baseFmt = input
		nanoStr = ""
	}

	// Try different time formats
	var t time.Time
	var err error

	formats := []string{
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"2006-01-02 15",
		"2006-01-02",
	}

	for _, fmt := range formats {
		t, err = time.Parse(fmt, baseFmt)
		if err == nil {
			break
		}
	}

	if err != nil {
		return 0, fmt.Errorf("invalid timestamp format: %q (expected nanoseconds or YYYY-MM-DD [HH:MM:SS[.nnnnnnnnn]])", input)
	}

	sec := t.Unix()
	nsec := int64(0)

	// Parse nanoseconds if provided
	if nanoStr != "" {
		// Pad or truncate to 9 digits
		if len(nanoStr) < 9 {
			nanoStr = nanoStr + strings.Repeat("0", 9-len(nanoStr))
		} else if len(nanoStr) > 9 {
			nanoStr = nanoStr[:9]
		}
		var err error
		nsec, err = strconv.ParseInt(nanoStr, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid nanoseconds in timestamp: %q", input)
		}
	}

	return Timestamp(sec*1_000_000_000 + nsec), nil
}
