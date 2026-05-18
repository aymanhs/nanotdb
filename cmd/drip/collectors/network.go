package collectors

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"path"
	"strconv"
	"strings"
)

// NetworkCollector reads /proc/net/dev and emits per-interface byte/packet counters.
// Metrics: net.{iface}.rx_bytes, rx_packets, rx_errors, rx_drop,
//
//	net.{iface}.tx_bytes, tx_packets, tx_errors, tx_drop
type NetworkCollector struct {
	// skipPatterns are glob patterns (supporting *) for interfaces to ignore.
	skipPatterns []string
}

func NewNetworkCollector(skip []string) *NetworkCollector {
	patterns := make([]string, 0, len(skip))
	for _, s := range skip {
		if s = strings.TrimSpace(s); s != "" {
			patterns = append(patterns, s)
		}
	}
	return &NetworkCollector{skipPatterns: patterns}
}

func (c *NetworkCollector) skipIface(name string) bool {
	for _, pat := range c.skipPatterns {
		if ok, _ := path.Match(pat, name); ok {
			return true
		}
	}
	return false
}

func (c *NetworkCollector) Name() string { return "network" }

func (c *NetworkCollector) Collect(ctx context.Context, ch chan<- Metric) {
	f, err := os.Open("/proc/net/dev")
	if err != nil {
		log.Printf("network collector: open /proc/net/dev: %v", err)
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	// Skip 2 header lines.
	scanner.Scan()
	scanner.Scan()
	for scanner.Scan() {
		line := scanner.Text()
		// Format: "  eth0: 123 456 7 8 0 0 0 0  99 88 1 2 ..."
		colon := strings.Index(line, ":")
		if colon < 0 {
			continue
		}
		iface := strings.TrimSpace(line[:colon])
		if c.skipIface(iface) {
			continue
		}
		fields := strings.Fields(line[colon+1:])
		if len(fields) < 16 {
			continue
		}
		type counter struct {
			suffix string
			idx    int
		}
		counters := []counter{
			{"rx_bytes", 0}, {"rx_packets", 1}, {"rx_errors", 2}, {"rx_drop", 3},
			{"tx_bytes", 8}, {"tx_packets", 9}, {"tx_errors", 10}, {"tx_drop", 11},
		}
		for _, cnt := range counters {
			v, err := strconv.ParseInt(fields[cnt.idx], 10, 64)
			if err != nil {
				continue
			}
			select {
			case ch <- Metric{Name: fmt.Sprintf("net.%s.%s", iface, cnt.suffix), Value: float32(v)}:
			case <-ctx.Done():
				return
			}
		}
	}
	if err := scanner.Err(); err != nil {
		log.Printf("network collector: scan /proc/net/dev: %v", err)
	}
}
