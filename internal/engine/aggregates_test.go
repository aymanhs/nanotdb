package engine

import "testing"

func TestAggregatorRegistrySupportsBuiltins(t *testing.T) {
	points := []float32{10, 20, 40, 50, 100}
	start := Timestamp(0)
	end := Timestamp(1)

	cases := []struct {
		name string
		want float32
	}{
		{name: "min", want: 10},
		{name: "max", want: 100},
		{name: "sum", want: 220},
		{name: "avg", want: 44},
		{name: "count", want: 5},
		{name: "p50", want: 40},
		{name: "median", want: 40},
		{name: "p95", want: 90},
		{name: "p99", want: 98},
	}

	for _, tt := range cases {
		agg, ok := getAggregator(tt.name)
		if !ok {
			t.Fatalf("expected aggregator %q to be registered", tt.name)
		}
		got, err := agg.Compute(start, end, points)
		if err != nil {
			t.Fatalf("Compute(%q) failed: %v", tt.name, err)
		}
		switch tt.name {
		case "avg", "p95", "p99":
			if got < tt.want-0.0001 || got > tt.want+0.0001 {
				t.Fatalf("Compute(%q) got=%f want=%f", tt.name, got, tt.want)
			}
			continue
		}
		if got != tt.want {
			t.Fatalf("Compute(%q) got=%f want=%f", tt.name, got, tt.want)
		}
	}
}

func TestAggregatorRegistrySupportsTrimmedAverage(t *testing.T) {
	points := []float32{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 1000}
	agg, ok := getAggregator("trimmed_avg")
	if !ok {
		t.Fatal("expected trimmed_avg to be registered")
	}
	got, err := agg.Compute(0, 1, points)
	if err != nil {
		t.Fatalf("trimmed_avg compute failed: %v", err)
	}
	if got < 6-0.0001 || got > 6+0.0001 {
		t.Fatalf("trimmed_avg got=%f want=6", got)
	}
	alias, ok := getAggregator("trimmed_average")
	if !ok {
		t.Fatal("expected trimmed_average alias to be registered")
	}
	aliasGot, err := alias.Compute(0, 1, points)
	if err != nil {
		t.Fatalf("trimmed_average compute failed: %v", err)
	}
	if aliasGot != got {
		t.Fatalf("trimmed_average alias mismatch: got=%f want=%f", aliasGot, got)
	}
}

func TestTrimmedAverageTrimsAtLeastOnePointPerTailWhenPossible(t *testing.T) {
	agg := trimmedAvgAggregator{}
	got, err := agg.Compute(0, 1, []float32{1, 2, 1000})
	if err != nil {
		t.Fatalf("trimmed_avg compute failed: %v", err)
	}
	if got != 2 {
		t.Fatalf("trimmed_avg got=%f want=2", got)
	}
}

func TestTrimmedAverageUsesFivePercentPerTailForLargerSamples(t *testing.T) {
	points := make([]float32, 0, 50)
	for i := 0; i < 5; i++ {
		points = append(points, 0)
	}
	for i := 0; i < 45; i++ {
		points = append(points, 10)
	}

	agg := trimmedAvgAggregator{}
	got, err := agg.Compute(0, 1, points)
	if err != nil {
		t.Fatalf("trimmed_avg compute failed: %v", err)
	}
	want := float32(430.0 / 46.0)
	if got < want-0.0001 || got > want+0.0001 {
		t.Fatalf("trimmed_avg got=%f want=%f", got, want)
	}
}

func TestAggregatorRegistryRejectsUnknown(t *testing.T) {
	if isSupportedAggregate("unknown") {
		t.Fatalf("did not expect unknown aggregate to be supported")
	}
}

func TestSupportedAggregatesReturnsSortedCopy(t *testing.T) {
	aggs := SupportedAggregates()
	if len(aggs) == 0 {
		t.Fatal("expected supported aggregates")
	}
	for i := 1; i < len(aggs); i++ {
		if aggs[i-1] > aggs[i] {
			t.Fatalf("aggregates not sorted: %v", aggs)
		}
	}
	registry := Aggregators()
	delete(registry, "avg")
	if !isSupportedAggregate("avg") {
		t.Fatal("expected registry copy mutation not to affect engine registry")
	}
	if DefaultStepAggregate() != "avg" {
		t.Fatalf("default step aggregate mismatch: got=%q want=avg", DefaultStepAggregate())
	}
}
