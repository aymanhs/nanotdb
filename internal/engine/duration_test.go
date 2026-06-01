package engine

import (
	"strings"
	"testing"
	"time"
)

func TestParseDuration_StdUnitsDelegate(t *testing.T) {
	cases := []struct {
		input string
		want  time.Duration
	}{
		{"1ns", time.Nanosecond},
		{"500us", 500 * time.Microsecond},
		{"2ms", 2 * time.Millisecond},
		{"30s", 30 * time.Second},
		{"5m", 5 * time.Minute},
		{"2h", 2 * time.Hour},
		{"1h30m", 90 * time.Minute},
		{"-5m", -5 * time.Minute},
	}
	for _, tc := range cases {
		got, err := ParseDuration(tc.input)
		if err != nil {
			t.Fatalf("ParseDuration(%q) failed: %v", tc.input, err)
		}
		if got != tc.want {
			t.Fatalf("ParseDuration(%q) = %s, want %s", tc.input, got, tc.want)
		}
	}
}

func TestParseDuration_DaysAndWeeks(t *testing.T) {
	cases := []struct {
		input string
		want  time.Duration
	}{
		{"1d", 24 * time.Hour},
		{"7d", 7 * 24 * time.Hour},
		{"1w", 7 * 24 * time.Hour},
		{"2w", 14 * 24 * time.Hour},
		{"1.5d", 36 * time.Hour},
		{"1d2h30m", 26*time.Hour + 30*time.Minute},
		{"1w2d", 9 * 24 * time.Hour},
		{"-1d", -24 * time.Hour},
	}
	for _, tc := range cases {
		got, err := ParseDuration(tc.input)
		if err != nil {
			t.Fatalf("ParseDuration(%q) failed: %v", tc.input, err)
		}
		if got != tc.want {
			t.Fatalf("ParseDuration(%q) = %s, want %s", tc.input, got, tc.want)
		}
	}
}

func TestParseDuration_RejectsEmpty(t *testing.T) {
	if _, err := ParseDuration(""); err == nil {
		t.Fatal("expected error for empty input")
	}
	if _, err := ParseDuration("   "); err == nil {
		t.Fatal("expected error for whitespace-only input")
	}
}

func TestParseDuration_RejectsUnknownUnit(t *testing.T) {
	_, err := ParseDuration("1fortnight")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "unknown unit") {
		t.Fatalf("unexpected error message: %v", err)
	}
}
