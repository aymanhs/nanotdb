package engine

import (
	"testing"
	"time"
)

func TestFormatTimestamp(t *testing.T) {
	tests := []struct {
		ts   Timestamp
		want string
	}{
		{Timestamp(1000000000), "1970-01-01 00:00:01.000000000"},
		{Timestamp(1609459200000000000), "2021-01-01 00:00:00.000000000"}, // 2021-01-01
		{Timestamp(1683000000123456789), "2023-05-02 04:00:00.123456789"},
	}

	for _, tt := range tests {
		got := FormatTimestamp(tt.ts)
		if got != tt.want {
			t.Errorf("FormatTimestamp(%d) = %q, want %q", tt.ts, got, tt.want)
		}
	}
}

func TestParseTimestamp(t *testing.T) {
	tests := []struct {
		input   string
		wantErr bool
		check   func(Timestamp) bool
	}{
		// Raw nanoseconds
		{"1609459200000000000", false, func(ts Timestamp) bool {
			expected := time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano()
			return int64(ts) == expected
		}},

		// Date only
		{"2021-01-01", false, func(ts Timestamp) bool {
			expected := time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano()
			return int64(ts) == expected
		}},

		// Date and time
		{"2021-01-01 12:30:45", false, func(ts Timestamp) bool {
			// Parse and verify
			expected := time.Date(2021, 1, 1, 12, 30, 45, 0, time.UTC).UnixNano()
			return int64(ts) == expected
		}},

		// Date, time, and nanoseconds
		{"2021-01-01 12:30:45.123456789", false, func(ts Timestamp) bool {
			expected := time.Date(2021, 1, 1, 12, 30, 45, 123456789, time.UTC).UnixNano()
			return int64(ts) == expected
		}},

		// Date and time without seconds
		{"2021-01-01 12:30", false, func(ts Timestamp) bool {
			expected := time.Date(2021, 1, 1, 12, 30, 0, 0, time.UTC).UnixNano()
			return int64(ts) == expected
		}},

		// Date and hour
		{"2021-01-01 12", false, func(ts Timestamp) bool {
			expected := time.Date(2021, 1, 1, 12, 0, 0, 0, time.UTC).UnixNano()
			return int64(ts) == expected
		}},

		// Nanoseconds with padding
		{"2021-01-01 12:30:45.1", false, func(ts Timestamp) bool {
			expected := time.Date(2021, 1, 1, 12, 30, 45, 100000000, time.UTC).UnixNano()
			return int64(ts) == expected
		}},

		// Invalid
		{"not-a-timestamp", true, nil},
		{"", true, nil},
	}

	for _, tt := range tests {
		got, err := ParseTimestamp(tt.input)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParseTimestamp(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			continue
		}
		if !tt.wantErr && !tt.check(got) {
			t.Errorf("ParseTimestamp(%q) = %d, check failed", tt.input, got)
		}
	}
}

func TestRoundtripTimestamp(t *testing.T) {
	ts := Timestamp(time.Date(2026, 5, 14, 12, 34, 56, 123456789, time.UTC).UnixNano())
	formatted := FormatTimestamp(ts)
	parsed, err := ParseTimestamp(formatted)
	if err != nil {
		t.Fatalf("ParseTimestamp(%q) failed: %v", formatted, err)
	}
	if parsed != ts {
		t.Errorf("roundtrip failed: %d -> %q -> %d", ts, formatted, parsed)
	}
}
