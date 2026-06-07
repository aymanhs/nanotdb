package main

// drip-side internal events emitter. Same shape as the engine-side
// emitter (see internal/engine/internal_events.go) but smaller: drip
// has no destination-db recursion to worry about, it ships events via
// the same HTTP path it already uses for the SD-write-probe slow
// event.
//
// See docs/INTERNAL_EVENTS.md for the spec.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// dripInternalEventGroups maps every drip group → default enabled
// state. Must match the "drip groups" rows of the Group taxonomy
// table in docs/INTERNAL_EVENTS.md.
var dripInternalEventGroups = map[string]bool{
	"drip.lifecycle": true,
	"drip.target":    true,
	"drip.buffer":    true,
	"drip.collector": true,
	"drip.host":      false,
	"drip.threshold": true,
}

// dripInternalEventGroupKnown reports whether the given group is
// known to drip. Used to validate config and runtime toggle calls.
func dripInternalEventGroupKnown(group string) bool {
	_, ok := dripInternalEventGroups[group]
	return ok
}

// dripInternalEventEmitter is drip's per-process emitter. The drain
// goroutine ships each accumulated record to the configured target_db
// via the same /api/v1/events HTTP path the SD-write-probe slow event
// already uses.
type dripInternalEventEmitter struct {
	cfg          DripInternalEventsConfig
	targetURL    string
	client       *http.Client
	timeout      time.Duration
	activeGroups atomic.Pointer[map[string]bool]
	sourceMu     sync.RWMutex
	groupSource  map[string]string
	ch           chan eventRecord
	stopOnce     sync.Once
	done         chan struct{}
	dropCounter  atomic.Uint64
}

func newDripInternalEventEmitter(cfg DripInternalEventsConfig, eventsURL string, client *http.Client, timeout time.Duration) *dripInternalEventEmitter {
	em := &dripInternalEventEmitter{
		cfg:         cfg,
		targetURL:   eventsURL,
		client:      client,
		timeout:     timeout,
		groupSource: map[string]string{},
		done:        make(chan struct{}),
	}
	groups := buildDripActiveGroups(cfg)
	em.activeGroups.Store(&groups)
	for g := range groups {
		if _, ok := cfg.Groups[g]; ok {
			em.groupSource[g] = "drip.toml"
		} else {
			em.groupSource[g] = "default"
		}
	}
	depth := cfg.QueueDepth
	if depth <= 0 {
		depth = 1024
	}
	em.ch = make(chan eventRecord, depth)
	return em
}

func buildDripActiveGroups(cfg DripInternalEventsConfig) map[string]bool {
	out := make(map[string]bool, len(dripInternalEventGroups))
	for g, def := range dripInternalEventGroups {
		out[g] = def
	}
	for g, v := range cfg.Groups {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "on", "true", "yes":
			out[g] = true
		case "off", "false", "no":
			out[g] = false
		}
	}
	return out
}

// start spins up the drain goroutine. Safe to call when Enabled is
// false (returns immediately).
func (em *dripInternalEventEmitter) start() {
	if em == nil || !em.cfg.Enabled {
		return
	}
	go em.drainLoop()
}

// stop closes the channel and joins the drain goroutine.
func (em *dripInternalEventEmitter) stop() {
	if em == nil || !em.cfg.Enabled {
		return
	}
	em.stopOnce.Do(func() {
		close(em.ch)
		<-em.done
	})
}

// active reports whether emitting into the given group would produce
// a record. Encouraged at call sites for cheap pre-flight checking.
func (em *dripInternalEventEmitter) active(group string) bool {
	if em == nil || !em.cfg.Enabled {
		return false
	}
	g := em.activeGroups.Load()
	if g == nil {
		return false
	}
	return (*g)[group]
}

// emit ships a record to the drain goroutine. Drops on a full channel
// (counted via dropCounter) — same drop-and-count semantic as the
// engine side. Every emit also writes a single log line so the same
// event the user sees in the Internal Events UI is also visible in
// `journalctl -u drip` without having to correlate via HTTP.
func (em *dripInternalEventEmitter) emit(group, name string, value any, payload map[string]any) {
	if em == nil || !em.cfg.Enabled {
		return
	}
	g := em.activeGroups.Load()
	if g == nil || !(*g)[group] {
		return
	}
	rec := eventRecord{
		DB:      em.cfg.TargetDB,
		Name:    name,
		TS:      time.Now().UnixNano(),
		Value:   value,
		Payload: payload,
	}
	logInternalEvent(name, value, payload)
	select {
	case em.ch <- rec:
	default:
		em.dropCounter.Add(1)
	}
}

// logInternalEvent renders an emitted record as a single logfmt-ish
// line. Payload keys are sorted so the output is stable and grep-able.
func logInternalEvent(name string, value any, payload map[string]any) {
	var sb strings.Builder
	sb.WriteString("drip: internal-event ")
	sb.WriteString(name)
	if value != nil {
		fmt.Fprintf(&sb, " value=%v", value)
	}
	if len(payload) > 0 {
		keys := make([]string, 0, len(payload))
		for k := range payload {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&sb, " %s=%q", k, fmt.Sprint(payload[k]))
		}
	}
	log.Print(sb.String())
}

// drainLoop reads each pending record and ships it via HTTP.
func (em *dripInternalEventEmitter) drainLoop() {
	defer close(em.done)
	for rec := range em.ch {
		// Send as a single-element batch to reuse sendEvents.
		if err := sendEvents(em.client, em.targetURL, []eventRecord{rec}); err != nil {
			// Per-record failures are logged but do not stop the
			// drain — losing a single internal event is preferable
			// to bringing down drip.
			log.Printf("drip: internal-event send failed (%s): %v", rec.Name, err)
		}
	}
}

// setGroup flips a group at runtime. Returns an error for unknown
// groups.
func (em *dripInternalEventEmitter) setGroup(group string, on bool) error {
	if !dripInternalEventGroupKnown(group) {
		return fmt.Errorf("unknown internal-events group: %q", group)
	}
	current := em.activeGroups.Load()
	updated := make(map[string]bool, len(*current))
	for k, v := range *current {
		updated[k] = v
	}
	updated[group] = on
	em.activeGroups.Store(&updated)
	em.sourceMu.Lock()
	em.groupSource[group] = "runtime"
	em.sourceMu.Unlock()
	return nil
}

// groupsView returns the current state for the admin-HTTP GET handler.
type dripInternalEventGroupView struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	Default bool   `json:"default"`
	Source  string `json:"source"`
}

func (em *dripInternalEventEmitter) groupsView() []dripInternalEventGroupView {
	g := em.activeGroups.Load()
	if g == nil {
		return nil
	}
	em.sourceMu.RLock()
	defer em.sourceMu.RUnlock()
	names := make([]string, 0, len(*g))
	for n := range *g {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]dripInternalEventGroupView, 0, len(names))
	for _, n := range names {
		src := em.groupSource[n]
		if src == "" {
			src = "default"
		}
		out = append(out, dripInternalEventGroupView{
			Name:    n,
			Enabled: (*g)[n],
			Default: dripInternalEventGroups[n],
			Source:  src,
		})
	}
	return out
}

// dripVersion exposes a build-time version string for the
// drip.started event payload. Kept as a var so a future ldflags wire
// can override it.
var dripVersion = "dev"

// internalEvents is the process-wide drip emitter. Initialized in
// main() before the ticker loop; package-level so the runCycle free
// function can reach it without threading it through every call.
// nil-safe — all methods short-circuit when em is nil or disabled.
var internalEvents *dripInternalEventEmitter

// lastTargetOK tracks the connect/disconnect state machine for the
// drip.target group. Atomic so the cycle loop and any future async
// reaches see the same value.
var (
	lastTargetOK         atomic.Bool
	lastTargetOutageFrom atomic.Int64 // unix nanos
)

// emitDripStarted is the per-startup lifecycle event. Pulled into a
// helper so the call site stays one line.
func (em *dripInternalEventEmitter) emitStarted(collectorCount int, targetURL string) {
	em.emit("drip.lifecycle", "drip.started", int32(collectorCount), map[string]any{
		"version":    dripVersion,
		"target_db":  em.cfg.TargetDB,
		"target_url": targetURL,
	})
}

// emitStoppedClean is the per-shutdown lifecycle event.
func (em *dripInternalEventEmitter) emitStoppedClean(msToDrain int32) {
	em.emit("drip.lifecycle", "drip.stopped.clean", msToDrain, nil)
}

// emitTargetTransition records connect/disconnect transitions. Caller
// is responsible for tracking the lastOK state machine and only
// calling this on a flip.
func (em *dripInternalEventEmitter) emitTargetDisconnected(url string, err error) {
	em.emit("drip.target", "drip.target.disconnected", nil, map[string]any{
		"url": url,
		"err": err.Error(),
	})
}

func (em *dripInternalEventEmitter) emitTargetReconnected(url string, outageMS int32) {
	em.emit("drip.target", "drip.target.reconnected", outageMS, map[string]any{
		"url": url,
	})
}

// adminHTTPServer wraps the small admin listener.
func startDripAdminServer(listen string, em *dripInternalEventEmitter) (*http.Server, error) {
	if strings.TrimSpace(listen) == "" {
		return nil, nil
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/version", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"version": dripVersion})
	})
	mux.HandleFunc("/api/v1/internal-events/groups", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "success",
				"data": map[string]any{
					"resultType": "internal_events_groups",
					"groups":     em.groupsView(),
				},
			})
		case http.MethodPost:
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "bad JSON: "+err.Error(), http.StatusBadRequest)
				return
			}
			for k, v := range body {
				on := false
				switch strings.ToLower(strings.TrimSpace(v)) {
				case "on", "true", "yes":
					on = true
				case "off", "false", "no":
					on = false
				default:
					http.Error(w, "invalid value for "+k, http.StatusBadRequest)
					return
				}
				if err := em.setGroup(k, on); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "success",
				"data":   map[string]any{"groups": em.groupsView()},
			})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	srv := &http.Server{
		Addr:              listen,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("drip: admin listener failed: %v", err)
		}
	}()
	return srv, nil
}

// noteTargetTransition tracks the connect/disconnect state machine
// and emits drip.target.disconnected / drip.target.reconnected on a
// flip. nil-safe — when the emitter is unconfigured or the group is
// off the work boils down to two atomic loads.
func noteTargetTransition(url string, ok bool, err error) {
	if internalEvents == nil || !internalEvents.active("drip.target") {
		// Still update the state so a later enable doesn't fire a
		// stale transition event from before the toggle.
		lastTargetOK.Store(ok)
		return
	}
	prev := lastTargetOK.Load()
	if prev == ok {
		return
	}
	lastTargetOK.Store(ok)
	if !ok {
		lastTargetOutageFrom.Store(time.Now().UnixNano())
		internalEvents.emitTargetDisconnected(url, err)
		return
	}
	// ok == true after a disconnect — emit reconnected with outage_ms.
	startNS := lastTargetOutageFrom.Load()
	outageMS := int32(0)
	if startNS > 0 {
		outageMS = int32((time.Now().UnixNano() - startNS) / 1_000_000)
	}
	internalEvents.emitTargetReconnected(url, outageMS)
}

// silence unused-import warnings for transient drafts.
var _ = bytes.NewReader
