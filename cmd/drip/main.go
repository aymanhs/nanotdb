package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/aymanhs/nanotdb/cmd/drip/collectors"
)

func main() {
	configPath := flag.String("config", "drip.toml", "path to drip config TOML file")
	once := flag.Bool("once", false, "run a single collection cycle and exit")
	debug := flag.Bool("debug", false, "print collected LP lines to stdout each cycle")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("drip: load config: %v", err)
	}

	cs := buildCollectors(cfg)
	if len(cs) == 0 {
		log.Fatal("drip: no collectors enabled")
	}
	names := make([]string, len(cs))
	for i, c := range cs {
		names[i] = c.Name()
	}
	log.Printf("drip: loaded %d collector(s) [%s], target=%s db=%s interval=%dms timeout=%dms",
		len(cs), strings.Join(names, ","), cfg.Drip.ServerURL, cfg.Drip.Database,
		cfg.Drip.CollectionIntervalMS, cfg.Drip.TimeoutMS)

	httpClient := &http.Client{Timeout: 10 * time.Second}
	importURL := strings.TrimRight(cfg.Drip.ServerURL, "/") + "/api/v1/import"
	eventsURL := strings.TrimRight(cfg.Drip.ServerURL, "/") + "/api/v1/events"
	interval := time.Duration(cfg.Drip.CollectionIntervalMS) * time.Millisecond
	timeout := time.Duration(cfg.Drip.TimeoutMS) * time.Millisecond

	if *once {
		runCycle(cfg.Drip.Database, cs, timeout, httpClient, importURL, eventsURL, *debug)
		return
	}

	// Infinite loop; exit on SIGINT / SIGTERM.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-sigCh:
			log.Println("drip: shutting down")
			return
		case <-ticker.C:
			runCycle(cfg.Drip.Database, cs, timeout, httpClient, importURL, eventsURL, *debug)
		}
	}
}

// buildCollectors instantiates enabled collectors from config.
func buildCollectors(cfg Config) []Collector {
	var cs []Collector
	if cfg.Collectors.CPU.Enabled {
		cs = append(cs, collectors.NewCPUCollector(cfg.Collectors.CPU.TempPath, cfg.Collectors.CPU.TempMetric))
	}
	if cfg.Collectors.Memory.Enabled {
		cs = append(cs, collectors.NewMemoryCollector())
	}
	if cfg.Collectors.Process.Enabled {
		cs = append(cs, collectors.NewProcessCollector(cfg.Collectors.Process.ExeNames))
	}
	if cfg.Collectors.Disk.Enabled {
		cs = append(cs, collectors.NewDiskCollector())
	}
	if cfg.Collectors.IO.Enabled {
		cs = append(cs, collectors.NewIOCollector())
	}
	if cfg.Collectors.OneWire.Enabled {
		cs = append(cs, collectors.NewOneWireCollector(
			cfg.Collectors.OneWire.Devices,
			cfg.Collectors.OneWire.AutoDiscover,
			cfg.Collectors.OneWire.BasePath,
			cfg.Collectors.OneWire.MaxValidMdeg,
		))
	}
	if cfg.Collectors.Network.Enabled {
		cs = append(cs, collectors.NewNetworkCollector(cfg.Collectors.Network.Skip))
	}
	if cfg.Collectors.LoadAvg.Enabled {
		cs = append(cs, collectors.NewLoadAvgCollector())
	}
	if cfg.Collectors.SDWriteProbe.Enabled {
		cs = append(cs, collectors.NewSDWriteProbeCollector(
			cfg.Collectors.SDWriteProbe.Directory,
			cfg.Collectors.SDWriteProbe.Bytes,
			cfg.Collectors.SDWriteProbe.EveryNCycles,
			cfg.Collectors.SDWriteProbe.Metric,
			cfg.Collectors.SDWriteProbe.EventWhenOverMS,
			cfg.Collectors.SDWriteProbe.EventName,
		))
	}
	return cs
}

// runCycle launches all collectors, waits up to timeout, gathers results,
// sorts by metric name, then POSTs as line protocol to nanotdb.
// If debug is true, LP lines are also printed to stdout.
func runCycle(database string, cs []Collector, timeout time.Duration, client *http.Client, importURL, eventsURL string, debug bool) {
	cycleStart := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ch := make(chan Metric, 256)

	var wg sync.WaitGroup
	for _, c := range cs {
		wg.Add(1)
		go func(c Collector) {
			defer wg.Done()
			c.Collect(ctx, ch)
		}(c)
	}

	// Close ch once all collectors are done so the drain loop below terminates.
	go func() {
		wg.Wait()
		close(ch)
	}()

	var metrics []Metric
	for m := range ch {
		metrics = append(metrics, m)
	}

	collectElapsed := time.Since(cycleStart)

	if len(metrics) == 0 {
		log.Printf("drip: cycle collected 0 metrics (elapsed=%s)", collectElapsed)
		return
	}
	tsNS := cycleStart.UnixNano()

	regularMetrics := make([]Metric, 0, len(metrics))
	eventBatch := make([]eventRecord, 0)
	for _, m := range metrics {
		if m.EmitAsEvent {
			name := strings.TrimSpace(m.EventName)
			if name == "" {
				name = m.Name
			}
			eventBatch = append(eventBatch, eventRecord{
				DB:      database,
				Name:    name,
				TS:      tsNS,
				Value:   m.Value,
				Payload: m.EventPayload,
			})
			continue
		}
		regularMetrics = append(regularMetrics, m)
	}

	// Sort by name for better nanotdb WAL/page compression locality.
	sort.Slice(regularMetrics, func(i, j int) bool {
		return regularMetrics[i].Name < regularMetrics[j].Name
	})
	sort.Slice(eventBatch, func(i, j int) bool {
		return eventBatch[i].Name < eventBatch[j].Name
	})

	lp := formatLP(database, regularMetrics, tsNS)

	if debug {
		if lp != "" {
			fmt.Print(lp)
		}
	}

	sendStart := time.Now()
	if lp != "" {
		if err := sendLP(client, importURL, lp); err != nil {
			log.Printf("drip: metric send failed (%d metrics): %v", len(regularMetrics), err)
			return
		}
	}
	if len(eventBatch) > 0 {
		if err := sendEvents(client, eventsURL, eventBatch); err != nil {
			log.Printf("drip: event send failed (%d events): %v", len(eventBatch), err)
			return
		}
	}
	sendElapsed := time.Since(sendStart)

	if debug {
		log.Printf("drip: cycle: collected %d samples (%d metrics, %d events) in %s, sent in %s (total %s)",
			len(metrics), len(regularMetrics), len(eventBatch), collectElapsed, sendElapsed, time.Since(cycleStart))
	}
}

type eventRecord struct {
	DB      string `json:"db"`
	Name    string `json:"name"`
	TS      int64  `json:"ts"`
	Value   any    `json:"value,omitempty"`
	Payload any    `json:"payload,omitempty"`
}

// formatLP serialises metrics as nanotdb line protocol lines:
//
//	{database}/{metric_name} {value} {ts_ns}
func formatLP(database string, metrics []Metric, tsNS int64) string {
	var sb strings.Builder
	for _, m := range metrics {
		switch v := m.Value.(type) {
		case int32:
			fmt.Fprintf(&sb, "%s/%s %d %d\n", database, m.Name, v, tsNS)
		case float32:
			fmt.Fprintf(&sb, "%s/%s %g %d\n", database, m.Name, v, tsNS)
		default:
			log.Printf("drip: unsupported metric value type for %s: %T", m.Name, m.Value)
		}
	}
	return sb.String()
}

// sendLP POSTs raw line-protocol text to the nanotdb import endpoint.
func sendLP(client *http.Client, url string, lp string) error {
	resp, err := client.Post(url, "text/plain", strings.NewReader(lp)) //nolint:noctx
	if err != nil {
		return fmt.Errorf("post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("server returned %s", resp.Status)
	}
	return nil
}

func sendEvents(client *http.Client, url string, records []eventRecord) error {
	body, err := json.Marshal(records)
	if err != nil {
		return fmt.Errorf("marshal events: %w", err)
	}
	resp, err := client.Post(url, "application/json", bytes.NewReader(body)) //nolint:noctx
	if err != nil {
		return fmt.Errorf("post events: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("events server returned %s", resp.Status)
	}
	return nil
}
