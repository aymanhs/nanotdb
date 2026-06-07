package collectors

import (
	"context"
	"testing"
)

// TestSDWriteProbeEventFiresOnlyOnFreshProbe pins the regression where a
// slow-probe value above the configured threshold was re-emitted as an
// event on EVERY collection cycle while the cached value lived (i.e.
// every_n_cycles - 1 duplicate events between real probes). The fix
// gates event emission on the cycle that actually ran a fresh probe.
//
// We drive the collector directly via Collect() and count events on the
// metric channel across a window of cycles. With every_n_cycles=3 and a
// threshold that is always exceeded, we expect exactly one event per
// probe cycle (cycles 1, 3, 6, 9), not one per Collect call.
func TestSDWriteProbeEventFiresOnlyOnFreshProbe(t *testing.T) {
	dir := t.TempDir()
	c := NewSDWriteProbeCollector(dir, 1024, 3, "disk.sd_write_probe_ms", 0, "drip.test.sd.slow")
	// Force the over-threshold event by giving the collector a zero
	// threshold and rewriting it post-construction. We use a tiny
	// positive threshold so the > comparison always wins for any
	// observed latency (real I/O always takes >0ms).
	c.eventWhenOverMS = 0.000001

	cycles := 9
	events := 0
	metrics := 0
	for i := 0; i < cycles; i++ {
		ch := make(chan Metric, 4)
		c.Collect(context.Background(), ch)
		close(ch)
		for m := range ch {
			if m.EmitAsEvent {
				events++
			} else {
				metrics++
			}
		}
	}
	// Probe cycles with every_n_cycles=3: cycle 1 (firstProbe), 3, 6, 9.
	const wantEvents = 4
	if events != wantEvents {
		t.Fatalf("event count mismatch: got=%d want=%d (one per fresh probe, not one per Collect)", events, wantEvents)
	}
	// Non-probe cycles still emit the cached metric (no event), so we
	// expect cycles - wantEvents non-event metrics.
	wantMetrics := cycles - wantEvents
	if metrics != wantMetrics {
		t.Fatalf("metric (non-event) count mismatch: got=%d want=%d", metrics, wantMetrics)
	}
}
