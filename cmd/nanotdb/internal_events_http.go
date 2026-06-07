package main

// HTTP handlers for the internal-events surface. See
// docs/INTERNAL_EVENTS.md for the spec.
//
// Two routes:
//
//   GET  /api/v1/internal-events/catalog
//        Returns the static registry of every internal event the
//        engine and drip may emit, with the current group enable
//        state and source.
//
//   GET  /api/v1/internal-events/groups
//        Returns just the groups + their state + source, in a flatter
//        shape than catalog. Useful for ops dashboards that don't
//        need the per-event detail.
//
//   POST /api/v1/internal-events/groups
//        Body: {"<group>":"on"|"off", ...}. Validates each key
//        against the registry, applies all changes, and emits the
//        audit event nanotdb.internal_events.group.toggled per
//        change. Atomic: an unknown group key returns 400 and no
//        change is applied.

import (
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/aymanhs/nanotdb/internal/engine"
)

func handleInternalEventsCatalog(eng *engine.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeVMError(w, http.StatusMethodNotAllowed, "bad_data", "method not allowed")
			return
		}
		snap := eng.InternalEventsCatalog()
		writeJSON(w, http.StatusOK, vmResponse{
			Status: "success",
			Data: map[string]interface{}{
				"resultType":     "internal_events_catalog",
				"master_enabled": snap.MasterEnabled,
				"destination_db": snap.DestinationDB,
				"groups":         snap.Groups,
			},
		})
	}
}

func handleInternalEventsGroups(eng *engine.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			handleInternalEventsGroupsGet(eng, w)
		case http.MethodPost:
			handleInternalEventsGroupsPost(eng, w, r)
		default:
			writeVMError(w, http.StatusMethodNotAllowed, "bad_data", "method not allowed")
		}
	}
}

func handleInternalEventsGroupsGet(eng *engine.Engine, w http.ResponseWriter) {
	groups := eng.InternalEventsGroups()
	writeJSON(w, http.StatusOK, vmResponse{
		Status: "success",
		Data: map[string]interface{}{
			"resultType": "internal_events_groups",
			"groups":     groups,
		},
	})
}

func handleInternalEventsGroupsPost(eng *engine.Engine, w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64*1024))
	if err != nil {
		writeVMError(w, http.StatusBadRequest, "bad_data", "read body: "+err.Error())
		return
	}
	var raw map[string]string
	if err := json.Unmarshal(body, &raw); err != nil {
		writeVMError(w, http.StatusBadRequest, "bad_data", "parse JSON body: "+err.Error())
		return
	}
	if len(raw) == 0 {
		writeVMError(w, http.StatusBadRequest, "bad_data", "body must contain at least one group key")
		return
	}
	// Two-pass: validate everything first so a single typo aborts the
	// whole call without leaving a partial change behind.
	type pending struct {
		group string
		on    bool
	}
	parsed := make([]pending, 0, len(raw))
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := strings.ToLower(strings.TrimSpace(raw[k]))
		var on bool
		switch v {
		case "on", "true", "yes":
			on = true
		case "off", "false", "no":
			on = false
		default:
			writeVMError(w, http.StatusBadRequest, "bad_data", "invalid value for group "+k+": expected on|off, got "+raw[k])
			return
		}
		parsed = append(parsed, pending{group: k, on: on})
	}
	for _, p := range parsed {
		if err := eng.SetInternalEventsGroup(p.group, p.on); err != nil {
			writeVMError(w, http.StatusBadRequest, "bad_data", err.Error())
			return
		}
	}
	// Return the updated state so the caller can confirm the change in
	// one round-trip.
	groups := eng.InternalEventsGroups()
	applied := make([]string, 0, len(parsed))
	for _, p := range parsed {
		applied = append(applied, p.group)
	}
	writeJSON(w, http.StatusOK, vmResponse{
		Status: "success",
		Data: map[string]interface{}{
			"resultType": "internal_events_groups",
			"applied":    applied,
			"groups":     groups,
		},
	})
}
