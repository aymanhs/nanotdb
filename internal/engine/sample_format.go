package engine

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// This file is the single source of truth for value-type stringification,
// sample-value formatting, and line-protocol value parsing. Every consumer
// (online ingest, offline import/export, inspect tooling, HTTP responses)
// goes through these helpers — keeping the rules consistent across the
// codebase and giving us one place to teach about a future ValueType.

// ValueTypeName returns the canonical lowercase name for a sample value type.
// Used in JSON/HTTP responses and inspect output. Unknown types return
// "unknown(<byte>)" so the caller can surface a diagnostic rather than mask
// a corrupt manifest.
func ValueTypeName(valueType byte) string {
	switch valueType {
	case Int32Sample:
		return "int32"
	case Float32Sample:
		return "float32"
	default:
		return fmt.Sprintf("unknown(%d)", valueType)
	}
}

// FormatSampleValue renders a Sample's value as a bare numeric string with
// NO line-protocol type suffix. Use this for human display, JSON payloads,
// and tabular output (HTTP /api responses, query CLI tables).
//
// For line-protocol output (which requires the `i` suffix on int32 values to
// disambiguate from floats), use FormatLPValue.
func FormatSampleValue(s Sample) string {
	switch s.ValueType {
	case Int32Sample:
		return strconv.FormatInt(int64(s.Int32), 10)
	case Float32Sample:
		return strconv.FormatFloat(float64(s.Float32), 'f', -1, 32)
	default:
		return "0"
	}
}

// FormatLPValue renders a raw 4-byte value as a line-protocol token. Int32
// values are emitted with a trailing `i` (the LP convention to distinguish
// them from floats); float values are emitted without a suffix.
//
// raw is the LE-encoded 4 bytes as stored in the metric file: for Int32Sample
// it is the int32 bit pattern; for Float32Sample it is the float32 bit
// pattern (math.Float32bits).
func FormatLPValue(valueType byte, raw uint32) string {
	switch valueType {
	case Int32Sample:
		return strconv.FormatInt(int64(int32(raw)), 10) + "i"
	case Float32Sample:
		return strconv.FormatFloat(float64(math.Float32frombits(raw)), 'f', -1, 32)
	default:
		return "0"
	}
}

// ParseLPValue parses a single line-protocol value token into the
// corresponding (valueType, int32, float32) triple. Trailing `i` selects
// Int32Sample; anything else is parsed as a float32. Integer overflow and
// malformed numbers produce errors with the token preserved for diagnostics.
//
// This is the same value-parsing logic used by Engine.parseLineProtocol; the
// offline importer uses it too via ParseLPIntValue / ParseLPFloatValue when
// the value type has already been inferred for a metric.
func ParseLPValue(token string) (valueType byte, i32 int32, f32 float32, err error) {
	token = strings.TrimSpace(token)
	if strings.HasSuffix(token, "i") {
		v, perr := ParseLPIntValue(token)
		if perr != nil {
			err = perr
			return
		}
		valueType = Int32Sample
		i32 = v
		return
	}
	v, perr := ParseLPFloatValue(token)
	if perr != nil {
		err = perr
		return
	}
	valueType = Float32Sample
	f32 = v
	return
}

// ParseLPIntValue parses an LP int token. The trailing `i` is optional —
// callers that have already determined the metric is int-typed may pass a
// bare integer string.
func ParseLPIntValue(token string) (int32, error) {
	v := strings.TrimSpace(token)
	v = strings.TrimSuffix(v, "i")
	if v == "" {
		return 0, fmt.Errorf("invalid int value %q", token)
	}
	parsed, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid int value %q", token)
	}
	if parsed < math.MinInt32 || parsed > math.MaxInt32 {
		return 0, fmt.Errorf("int value out of int32 range %q", token)
	}
	return int32(parsed), nil
}

// ParseLPFloatValue parses an LP float token. Refuses tokens with the `i`
// suffix so a typo doesn't silently land in a float-typed metric.
func ParseLPFloatValue(token string) (float32, error) {
	v := strings.TrimSpace(token)
	if strings.HasSuffix(v, "i") {
		return 0, fmt.Errorf("unexpected int suffix for float value %q", token)
	}
	parsed, err := strconv.ParseFloat(v, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid numeric value %q", token)
	}
	return float32(parsed), nil
}

// LPValueLooksLikeFloat reports whether a raw LP value token (no leading/
// trailing whitespace) looks like a float literal (decimal point or scientific
// notation). Used during offline-import type inference to disambiguate
// metric types when the explicit `i` suffix is absent.
func LPValueLooksLikeFloat(token string) bool {
	return strings.ContainsAny(token, ".eE")
}

// LPValueHasIntSuffix reports whether token ends in the LP int-suffix `i`.
func LPValueHasIntSuffix(token string) bool {
	return strings.HasSuffix(strings.TrimSpace(token), "i")
}
