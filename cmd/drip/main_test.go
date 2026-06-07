package main

import (
	"encoding/json"
	"testing"
)

// TestEventRecordMarshalFloatHasDecimal locks in the workaround for
// the historical ErrEventTypeMismatch / HTTP 400 we hit in production
// when drip emitted a whole-number float (e.g. float32(15.0)) for an
// event whose catalog entry was Float32Sample. Go's default
// encoding/json marshals float32(15.0) as "15", which the server's
// JSON-number heuristic then classifies as int32 — and the catalog
// rejects the type change. The custom MarshalJSON on eventRecord
// forces a trailing ".0" so the value stays a float on the wire.
func TestEventRecordMarshalFloatHasDecimal(t *testing.T) {
	cases := []struct {
		name string
		val  any
		want string
	}{
		{"float32_whole", float32(15.0), `15.0`},
		{"float32_fractional", float32(15.5), `15.5`},
		{"float64_whole", float64(42.0), `42.0`},
		{"int32_stays_int", int32(7), `7`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := eventRecord{DB: "internal", Name: "x", TS: 1, Value: tc.val}
			raw, err := json.Marshal(rec)
			if err != nil {
				t.Fatalf("marshal failed: %v", err)
			}
			var got struct {
				Value json.RawMessage `json:"value"`
			}
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("unmarshal failed: %v\nraw=%s", err, raw)
			}
			if string(got.Value) != tc.want {
				t.Fatalf("value got %s want %s\nraw=%s", got.Value, tc.want, raw)
			}
		})
	}
}

// TestEventRecordMarshalOmitsNilValue ensures Value is omitted when
// the caller passes nil (none-typed event), matching the existing
// omitempty contract callers and the server both rely on.
func TestEventRecordMarshalOmitsNilValue(t *testing.T) {
	rec := eventRecord{DB: "internal", Name: "x", TS: 1, Value: nil}
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal failed: %v\nraw=%s", err, raw)
	}
	if _, ok := got["value"]; ok {
		t.Fatalf("expected no value key when Value is nil, raw=%s", raw)
	}
}
