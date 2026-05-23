package collectors

import "testing"

func TestDeriveDiskRates(t *testing.T) {
	prev := diskSnapshot{
		reads:        100,
		writes:       50,
		readSectors:  2000,
		writeSectors: 1000,
		ioMS:         5000,
	}
	curr := diskSnapshot{
		reads:        120,
		writes:       70,
		readSectors:  2400,
		writeSectors: 1400,
		ioMS:         6500,
	}
	rates, ok := deriveDiskRates(prev, curr, 10)
	if !ok {
		t.Fatal("expected derived rates")
	}
	if rates.busyPct != 15 {
		t.Fatalf("busy pct mismatch: got=%v want=15", rates.busyPct)
	}
	if rates.iops != 4 {
		t.Fatalf("iops mismatch: got=%v want=4", rates.iops)
	}
	if rates.readKBps != 20 {
		t.Fatalf("read KB/s mismatch: got=%v want=20", rates.readKBps)
	}
	if rates.writeKBps != 20 {
		t.Fatalf("write KB/s mismatch: got=%v want=20", rates.writeKBps)
	}
}

func TestDeriveDiskRatesRejectsCounterReset(t *testing.T) {
	_, ok := deriveDiskRates(
		diskSnapshot{reads: 20, writes: 20, readSectors: 100, writeSectors: 100, ioMS: 100},
		diskSnapshot{reads: 10, writes: 30, readSectors: 110, writeSectors: 120, ioMS: 120},
		10,
	)
	if ok {
		t.Fatal("expected counter reset to be rejected")
	}
}
