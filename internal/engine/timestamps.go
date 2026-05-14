package engine

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

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
