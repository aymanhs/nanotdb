package engine

import "testing"

func TestRollupAggregatorRegistrySupportsBuiltins(t *testing.T) {
	points := []float32{10, 20, 40}
	start := Timestamp(0)
	end := Timestamp(1)

	cases := []struct {
		name string
		want float32
	}{
		{name: "min", want: 10},
		{name: "max", want: 40},
		{name: "sum", want: 70},
		{name: "avg", want: float32(70.0 / 3.0)},
		{name: "count", want: 3},
	}

	for _, tt := range cases {
		agg, ok := getRollupAggregator(tt.name)
		if !ok {
			t.Fatalf("expected aggregator %q to be registered", tt.name)
		}
		got, err := agg.Compute(start, end, points)
		if err != nil {
			t.Fatalf("Compute(%q) failed: %v", tt.name, err)
		}
		if tt.name == "avg" {
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

func TestRollupAggregatorRegistryRejectsUnknown(t *testing.T) {
	if isSupportedRollupAggregate("median") {
		t.Fatalf("did not expect unknown aggregate to be supported")
	}
}
