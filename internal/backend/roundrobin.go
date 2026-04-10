package backend

import (
	"sync"
	"sync/atomic"
)

// RoundRobinTracker maintains per-model atomic counters for round-robin backend selection.
// It is safe for concurrent use without locks.
type RoundRobinTracker struct {
	mu       sync.Mutex
	counters map[string]*atomic.Uint64
}

func newRoundRobinTracker() *RoundRobinTracker {
	return &RoundRobinTracker{counters: make(map[string]*atomic.Uint64)}
}

// Next rotates the entries slice so that the backend selected by the round-robin counter
// comes first. All other backends remain in their original relative order (for fallback).
// The counter is incremented atomically; concurrent callers each get a distinct slot.
func (t *RoundRobinTracker) Next(model string, entries []RouteEntry) []RouteEntry {
	if len(entries) <= 1 {
		return entries
	}

	t.mu.Lock()
	ctr, ok := t.counters[model]
	if !ok {
		ctr = &atomic.Uint64{}
		t.counters[model] = ctr
	}
	t.mu.Unlock()

	idx := int(ctr.Add(1)-1) % len(entries)

	// Rotate: build a new slice starting at idx, wrapping around.
	rotated := make([]RouteEntry, len(entries))
	for i, e := range entries {
		rotated[(i-idx+len(entries))%len(entries)] = e
	}
	return rotated
}
