package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aymanhs/nanotdb/internal/engine"
)

// handleEvents dispatches /api/v1/events by HTTP method: POST ingests
// a JSON batch (handleEventsImport), GET runs a range query
// (handleEventsQuery). Other methods return 405.
func handleEvents(eng *engine.Engine) http.HandlerFunc {
	imp := handleEventsImport(eng)
	qry := handleEventsQuery(eng)
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			imp(w, r)
		case http.MethodGet:
			qry(w, r)
		default:
			writeVMError(w, http.StatusMethodNotAllowed, "bad_data", "method not allowed")
		}
	}
}

// handleEventsImport handles POST /api/v1/events.
//
// Body shape (JSON array):
//
//	[
//	  {"db":"sensors","name":"disc.write.slow","ts":1717238400000000000,"value":542,"payload":{"path":"/tmp"}},
//	  {"db":"sensors","name":"heartbeat"}
//	]
//
// Field rules per docs/EVENTS.md:
//   - db and name are required.
//   - ts is optional; nil/missing → engine substitutes time.Now().
//   - value is one of: missing/null (→ none-typed event), JSON integer (→
//     int32), JSON float (→ float32). JSON strings are rejected — strings
//     belong in payload.
//   - payload is arbitrary JSON kept opaque; we encode it back to bytes
//     and hand them to engine.AddEvent verbatim. Payload size cap is
//     enforced by the engine.
//
// Returns a vmResponse with data.imported set to the count of accepted
// events. On any per-event error, the whole batch fails with a 4xx — we
// do not partially apply.
func handleEventsImport(eng *engine.Engine) http.HandlerFunc {
	type eventReq struct {
		DB      string          `json:"db"`
		Name    string          `json:"name"`
		TS      *int64          `json:"ts,omitempty"`
		Value   json.RawMessage `json:"value,omitempty"`
		Payload json.RawMessage `json:"payload,omitempty"`
	}

	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeVMError(w, http.StatusMethodNotAllowed, "bad_data", "method not allowed")
			return
		}
		ct := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
		if !strings.HasPrefix(ct, "application/json") && ct != "" {
			writeVMError(w, http.StatusBadRequest, "bad_data", "events import requires Content-Type: application/json")
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, 8*1024*1024))
		if err != nil {
			writeVMError(w, http.StatusBadRequest, "bad_data", fmt.Sprintf("read body: %v", err))
			return
		}
		trimmed := bytes.TrimSpace(body)
		if len(trimmed) == 0 {
			writeVMError(w, http.StatusBadRequest, "bad_data", "empty request body")
			return
		}

		var reqs []eventReq
		// Accept both a JSON array and a single object for convenience.
		if trimmed[0] == '[' {
			if err := json.Unmarshal(trimmed, &reqs); err != nil {
				writeVMError(w, http.StatusBadRequest, "bad_data", fmt.Sprintf("invalid JSON array: %v", err))
				return
			}
		} else {
			var one eventReq
			if err := json.Unmarshal(trimmed, &one); err != nil {
				writeVMError(w, http.StatusBadRequest, "bad_data", fmt.Sprintf("invalid JSON: %v", err))
				return
			}
			reqs = []eventReq{one}
		}

		imported := 0
		for i, rec := range reqs {
			value, err := parseJSONEventValue(rec.Value)
			if err != nil {
				writeVMError(w, http.StatusBadRequest, "bad_data", fmt.Sprintf("event %d: %v", i, err))
				return
			}
			payload, err := normalizeJSONEventPayload(rec.Payload)
			if err != nil {
				writeVMError(w, http.StatusBadRequest, "bad_data", fmt.Sprintf("event %d: %v", i, err))
				return
			}
			ts := engine.Timestamp(0)
			if rec.TS != nil {
				ts = engine.Timestamp(*rec.TS)
			}
			if err := eng.AddEvent(rec.DB, rec.Name, ts, value, payload); err != nil {
				status := http.StatusBadRequest
				if errors.Is(err, engine.ErrEventsDisabled) {
					status = http.StatusConflict
				}
				writeVMError(w, status, "bad_data", fmt.Sprintf("event %d: %v", i, err))
				return
			}
			imported++
		}

		writeJSON(w, http.StatusOK, vmResponse{
			Status: "success",
			Data: map[string]interface{}{
				"resultType": "events_import",
				"imported":   imported,
			},
		})
	}
}

// parseJSONEventValue maps a JSON value field to one of:
//   - nil (for null / absent / explicit "none")
//   - int32 (for whole-number JSON numbers that fit)
//   - float32 (for JSON numbers with a fractional / exponent part)
//
// Returns an error for JSON strings (use payload), booleans, arrays, or
// objects — only numeric scalars or null are valid event values per
// docs/EVENTS.md.
func parseJSONEventValue(raw json.RawMessage) (any, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	first := trimmed[0]
	if first == '"' {
		return nil, fmt.Errorf("event value cannot be a string (use payload for string data)")
	}
	if first == '{' || first == '[' {
		return nil, fmt.Errorf("event value cannot be an object or array")
	}
	if first == 't' || first == 'f' {
		return nil, fmt.Errorf("event value cannot be a boolean")
	}
	// Numeric. Decide int32 vs float32 by whether the literal contains
	// a decimal point or exponent — same heuristic the line-protocol
	// parser uses for metric values.
	s := string(trimmed)
	hasFloatChar := strings.ContainsAny(s, ".eE")
	if !hasFloatChar {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid integer event value %q: %v", s, err)
		}
		if n < -2147483648 || n > 2147483647 {
			return nil, fmt.Errorf("integer event value %d out of int32 range", n)
		}
		return int32(n), nil
	}
	f, err := strconv.ParseFloat(s, 32)
	if err != nil {
		return nil, fmt.Errorf("invalid float event value %q: %v", s, err)
	}
	return float32(f), nil
}

// normalizeJSONEventPayload turns the raw payload RawMessage into the
// opaque bytes engine.AddEvent expects. Missing/null payload returns
// nil. We preserve the exact JSON bytes the client sent (after a
// json.Compact round-trip to drop incidental whitespace).
func normalizeJSONEventPayload(raw json.RawMessage) ([]byte, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, trimmed); err != nil {
		return nil, fmt.Errorf("invalid JSON payload: %v", err)
	}
	return compact.Bytes(), nil
}

// handleEventsQuery handles GET /api/v1/events.
//
// Query parameters:
//   - db (required)
//   - name (optional, exact match — wildcard not yet supported)
//   - start (RFC3339 / Unix ns / engine.ParseTimestamp formats)
//   - end (same; defaults to now)
//   - limit (default 100, hard cap 1000)
//
// Response shape mirrors the other /api/v1 endpoints: status / data /
// resultType. Payload bytes are returned as parsed JSON when valid, or
// as {"raw_base64": "..."} when not — keeps the response valid JSON
// regardless of what bytes the producer stored.
func handleEventsQuery(eng *engine.Engine) http.HandlerFunc {
	type respEvent struct {
		Name      string          `json:"name"`
		EventID   uint16          `json:"id"`
		TS        int64           `json:"ts"`
		ValueType string          `json:"value_type"`
		Int32     *int32          `json:"int32,omitempty"`
		Float32   *float32        `json:"float32,omitempty"`
		Payload   json.RawMessage `json:"payload,omitempty"`
	}

	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeVMError(w, http.StatusMethodNotAllowed, "bad_data", "method not allowed")
			return
		}

		q := r.URL.Query()
		database := strings.TrimSpace(q.Get("db"))
		if database == "" {
			writeVMError(w, http.StatusBadRequest, "bad_data", "missing db parameter")
			return
		}
		if err := engine.ValidateDatabaseName(database); err != nil {
			writeVMError(w, http.StatusBadRequest, "bad_data", err.Error())
			return
		}
		name := strings.TrimSpace(q.Get("name"))
		patternFilter := ""
		queryName := name
		if hasWildcardPattern(name) {
			patternFilter = name
			queryName = ""
		}

		tsUnit := strings.TrimSpace(q.Get("timestamp_unit"))
		startRaw := strings.TrimSpace(q.Get("start"))
		endRaw := strings.TrimSpace(q.Get("end"))

		var fromTS, toTS engine.Timestamp
		var err error
		if startRaw != "" {
			fromTS, err = parseTimeParam(startRaw, tsUnit)
			if err != nil {
				writeVMError(w, http.StatusBadRequest, "bad_data", fmt.Sprintf("invalid start: %v", err))
				return
			}
		}
		if endRaw != "" {
			toTS, err = parseTimeParam(endRaw, tsUnit)
			if err != nil {
				writeVMError(w, http.StatusBadRequest, "bad_data", fmt.Sprintf("invalid end: %v", err))
				return
			}
		} else {
			toTS = engine.Timestamp(time.Now().UnixNano())
		}
		if toTS < fromTS {
			writeVMError(w, http.StatusBadRequest, "bad_data", "end must be >= start")
			return
		}

		const defaultLimit = 100
		const maxLimit = 1000
		limit := defaultLimit
		if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed < 1 {
				writeVMError(w, http.StatusBadRequest, "bad_data", "limit must be a positive integer")
				return
			}
			if parsed > maxLimit {
				parsed = maxLimit
			}
			limit = parsed
		}

		results := make([]respEvent, 0, limit)
		var stopSignal = errStopQuery
		queryErr := eng.QueryEvents(database, queryName, fromTS, toTS, func(ev engine.EventQueryResult) error {
			if patternFilter != "" && !matchEventNamePattern(patternFilter, ev.Name) {
				return nil
			}
			if len(results) >= limit {
				return stopSignal
			}
			r := respEvent{
				Name:      ev.Name,
				EventID:   uint16(ev.EventID),
				TS:        int64(ev.TS),
				ValueType: engine.EventValueTypeName(ev.ValueType),
			}
			switch ev.ValueType {
			case engine.Int32Sample:
				v := ev.Int32
				r.Int32 = &v
			case engine.Float32Sample:
				v := ev.Float32
				r.Float32 = &v
			}
			r.Payload = formatEventPayloadForResponse(ev.Payload)
			results = append(results, r)
			return nil
		})
		if queryErr != nil && !errors.Is(queryErr, stopSignal) {
			status := http.StatusInternalServerError
			if errors.Is(queryErr, engine.ErrEventsDisabled) {
				status = http.StatusConflict
			}
			writeVMError(w, status, "execution", queryErr.Error())
			return
		}

		writeJSON(w, http.StatusOK, vmResponse{
			Status: "success",
			Data: map[string]interface{}{
				"resultType": "events",
				"db":         database,
				"result":     results,
			},
		})
	}
}

func hasWildcardPattern(s string) bool {
	if s == "" {
		return false
	}
	return strings.ContainsAny(s, "*?[")
}

func matchEventNamePattern(pattern, name string) bool {
	ok, err := path.Match(pattern, name)
	return err == nil && ok
}

// errStopQuery is a sentinel returned from the QueryEvents callback when
// the response limit is hit. errors.Is unwraps it cleanly so the handler
// knows to stop without surfacing it as a real error.
var errStopQuery = errors.New("events query: response limit reached")

// formatEventPayloadForResponse returns the payload bytes as parsed JSON
// when they are valid JSON, or as {"raw_base64": "..."} otherwise. The
// distinction keeps the outer response document valid JSON regardless of
// what bytes the event producer happened to store.
func formatEventPayloadForResponse(raw []byte) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	// Round-trip through the standard library: if Unmarshal succeeds,
	// the bytes were valid JSON and we can hand them through verbatim
	// (the engine stores compact bytes; no whitespace to normalize).
	var probe interface{}
	if err := json.Unmarshal(raw, &probe); err == nil {
		return append(json.RawMessage(nil), raw...)
	}
	wrapped := struct {
		RawBase64 string `json:"raw_base64"`
	}{RawBase64: base64.StdEncoding.EncodeToString(raw)}
	encoded, err := json.Marshal(wrapped)
	if err != nil {
		// Practically unreachable — json.Marshal on a struct with a
		// string field cannot fail. Surface as a clean fallback.
		return json.RawMessage(`null`)
	}
	return encoded
}

// handleEventsCatalog handles GET /api/v1/events/catalog.
//
// Query parameters: db (required). Returns the events catalog for the
// named database — name + numeric id + value type. Mirrors
// /api/v1/metrics?details=true.
func handleEventsCatalog(eng *engine.Engine) http.HandlerFunc {
	type entry struct {
		Name            string `json:"name"`
		ID              uint16 `json:"id"`
		ValueType       string `json:"value_type"`
		LastTimestamp   string `json:"last_timestamp,omitempty"`
		LastTimestampNS int64  `json:"last_timestamp_ns,omitempty"`
	}

	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeVMError(w, http.StatusMethodNotAllowed, "bad_data", "method not allowed")
			return
		}
		database := strings.TrimSpace(r.URL.Query().Get("db"))
		if database == "" {
			writeVMError(w, http.StatusBadRequest, "bad_data", "missing db parameter")
			return
		}
		if err := engine.ValidateDatabaseName(database); err != nil {
			writeVMError(w, http.StatusBadRequest, "bad_data", err.Error())
			return
		}

		events, err := eng.ListEvents(database)
		if err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, engine.ErrEventsDisabled) {
				status = http.StatusConflict
			}
			writeVMError(w, status, "execution", err.Error())
			return
		}
		out := make([]entry, 0, len(events))
		for _, e := range events {
			item := entry{
				Name:      e.Name,
				ID:        uint16(e.EventID),
				ValueType: engine.EventValueTypeName(e.ValueType),
			}
			if e.LastValid {
				item.LastTimestampNS = int64(e.LastTS)
				item.LastTimestamp = engine.FormatTimestamp(e.LastTS)
			}
			out = append(out, item)
		}
		writeJSON(w, http.StatusOK, vmResponse{
			Status: "success",
			Data: map[string]interface{}{
				"resultType": "events_catalog",
				"db":         database,
				"result":     out,
			},
		})
	}
}

// handleEventsAggregate handles GET /api/v1/events/aggregate.
//
// Query parameters:
//   - db (required)
//   - name (optional; exact or wildcard pattern)
//   - start (required)
//   - end (optional; defaults to now)
//   - window (required; duration, for example 1m)
func handleEventsAggregate(eng *engine.Engine) http.HandlerFunc {
	type bucket struct {
		TS    int64 `json:"ts"`
		Count int64 `json:"count"`
	}

	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeVMError(w, http.StatusMethodNotAllowed, "bad_data", "method not allowed")
			return
		}

		q := r.URL.Query()
		database := strings.TrimSpace(q.Get("db"))
		if database == "" {
			writeVMError(w, http.StatusBadRequest, "bad_data", "missing db parameter")
			return
		}
		if err := engine.ValidateDatabaseName(database); err != nil {
			writeVMError(w, http.StatusBadRequest, "bad_data", err.Error())
			return
		}
		name := strings.TrimSpace(q.Get("name"))
		patternFilter := ""
		queryName := name
		if hasWildcardPattern(name) {
			patternFilter = name
			queryName = ""
		}

		tsUnit := strings.TrimSpace(q.Get("timestamp_unit"))
		startRaw := strings.TrimSpace(q.Get("start"))
		if startRaw == "" {
			writeVMError(w, http.StatusBadRequest, "bad_data", "missing start parameter")
			return
		}
		fromTS, err := parseTimeParam(startRaw, tsUnit)
		if err != nil {
			writeVMError(w, http.StatusBadRequest, "bad_data", fmt.Sprintf("invalid start: %v", err))
			return
		}

		endRaw := strings.TrimSpace(q.Get("end"))
		toTS := engine.Timestamp(time.Now().UnixNano())
		if endRaw != "" {
			toTS, err = parseTimeParam(endRaw, tsUnit)
			if err != nil {
				writeVMError(w, http.StatusBadRequest, "bad_data", fmt.Sprintf("invalid end: %v", err))
				return
			}
		}
		if toTS < fromTS {
			writeVMError(w, http.StatusBadRequest, "bad_data", "end must be >= start")
			return
		}

		windowRaw := strings.TrimSpace(q.Get("window"))
		if windowRaw == "" {
			writeVMError(w, http.StatusBadRequest, "bad_data", "missing window parameter")
			return
		}
		window, err := engine.ParseDuration(windowRaw)
		if err != nil || window <= 0 {
			writeVMError(w, http.StatusBadRequest, "bad_data", fmt.Sprintf("invalid window: %v", err))
			return
		}
		windowNS := int64(window)
		if windowNS <= 0 {
			writeVMError(w, http.StatusBadRequest, "bad_data", "window must be > 0")
			return
		}

		counts := map[int64]int64{}
		queryErr := eng.QueryEvents(database, queryName, fromTS, toTS, func(ev engine.EventQueryResult) error {
			if patternFilter != "" && !matchEventNamePattern(patternFilter, ev.Name) {
				return nil
			}
			ts := int64(ev.TS)
			bucketStart := ts - (ts % windowNS)
			if ts < 0 && ts%windowNS != 0 {
				bucketStart -= windowNS
			}
			counts[bucketStart]++
			return nil
		})
		if queryErr != nil {
			status := http.StatusInternalServerError
			if errors.Is(queryErr, engine.ErrEventsDisabled) {
				status = http.StatusConflict
			}
			writeVMError(w, status, "execution", queryErr.Error())
			return
		}

		keys := make([]int64, 0, len(counts))
		for k := range counts {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
		result := make([]bucket, 0, len(keys))
		for _, k := range keys {
			result = append(result, bucket{TS: k, Count: counts[k]})
		}

		writeJSON(w, http.StatusOK, vmResponse{
			Status: "success",
			Data: map[string]interface{}{
				"resultType": "events_aggregate",
				"db":         database,
				"window":     window.String(),
				"result":     result,
			},
		})
	}
}
