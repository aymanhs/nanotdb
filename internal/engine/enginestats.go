package engine

import "sync"

// engineStatStore is a flat, thread-safe store of named float64 counters/accumulators.
// Keys use the format "{dbName}/{category}/{metric}", e.g. "prod/data/flush_count".
// All values are stored as float64; they are written to the internal DB as float32 samples.
type engineStatStore struct {
	mu     sync.Mutex
	vals   map[string]float64
	minSet map[string]struct{} // tracks which min keys have been initialised
}

func newEngineStatStore() engineStatStore {
	return engineStatStore{
		vals:   make(map[string]float64),
		minSet: make(map[string]struct{}),
	}
}

// incr adds delta to key (starts at 0).
func (s *engineStatStore) incr(key string, delta float64) {
	s.mu.Lock()
	s.vals[key] += delta
	s.mu.Unlock()
}

// setMax replaces the value only if v is larger than the current value.
func (s *engineStatStore) setMax(key string, v float64) {
	s.mu.Lock()
	if v > s.vals[key] {
		s.vals[key] = v
	}
	s.mu.Unlock()
}

// setMin replaces the value only if v is smaller than the current value (or key not yet set).
func (s *engineStatStore) setMin(key string, v float64) {
	s.mu.Lock()
	_, set := s.minSet[key]
	if !set || v < s.vals[key] {
		s.vals[key] = v
		s.minSet[key] = struct{}{}
	}
	s.mu.Unlock()
}

// set unconditionally overwrites the value (for WAL cumulative counters read at flush time).
func (s *engineStatStore) set(key string, v float64) {
	s.mu.Lock()
	s.vals[key] = v
	s.mu.Unlock()
}

// snapshot returns a copy of all current values.
func (s *engineStatStore) snapshot() map[string]float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]float64, len(s.vals))
	for k, v := range s.vals {
		out[k] = v
	}
	return out
}
