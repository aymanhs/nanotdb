package collectors

import "testing"

func TestParseProcStatTicks(t *testing.T) {
	stat := "1234 (nanotdb) S 1 2 3 4 5 6 7 8 9 10 111 222 13 14 15 16 17 18 19 20 21 22"
	got, err := parseProcStatTicks(stat)
	if err != nil {
		t.Fatalf("parseProcStatTicks failed: %v", err)
	}
	if want := uint64(333); got != want {
		t.Fatalf("ticks mismatch: got=%d want=%d", got, want)
	}
}

func TestParseStatmBytes(t *testing.T) {
	vsize, rss, err := parseStatmBytes("100 25 0 0 0 0 0", 4096)
	if err != nil {
		t.Fatalf("parseStatmBytes failed: %v", err)
	}
	if vsize != 409600 {
		t.Fatalf("vsize mismatch: got=%d want=%d", vsize, 409600)
	}
	if rss != 102400 {
		t.Fatalf("rss mismatch: got=%d want=%d", rss, 102400)
	}
}

func TestSanitizeMetricSegment(t *testing.T) {
	if got := sanitizeMetricSegment("Nano-TDB"); got != "nano_tdb" {
		t.Fatalf("sanitize mismatch: got=%q want=%q", got, "nano_tdb")
	}
	if got := sanitizeMetricSegment("   "); got != "unknown" {
		t.Fatalf("sanitize empty mismatch: got=%q want=%q", got, "unknown")
	}
}
