package keys

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Reader is a read-only KeyProvider backed by a published snapshot. Reader pods
// serve from it and never call GitHub themselves — all fetching happens on the
// single writer — so every pod returns identical keys (no split brain).
type Reader struct {
	mu          sync.RWMutex
	entries     map[string]Entry
	generatedAt time.Time

	loaded atomic.Bool
}

// NewReader returns an empty Reader. It reports not-warm until the first Replace.
func NewReader() *Reader {
	return &Reader{entries: make(map[string]Entry, 0)}
}

// Replace atomically swaps in a new set of entries from a freshly loaded
// snapshot and marks the Reader warm.
func (r *Reader) Replace(entries map[string]Entry, generatedAt time.Time) {
	r.mu.Lock()
	r.entries = entries
	r.generatedAt = generatedAt
	r.mu.Unlock()

	r.loaded.Store(true)
}

// Get returns the cached entry for a handle, matching case-insensitively.
func (r *Reader) Get(handle string) (Entry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	e, ok := r.entries[strings.ToLower(handle)]
	if !ok {
		return Entry{}, false
	}

	return e, true
}

// Refresh never fetches: a reader serves only the published snapshot, so it
// returns the cached entry if present and a miss otherwise. This keeps every
// reader consistent rather than each fetching GitHub independently.
func (r *Reader) Refresh(_ context.Context, handle string) (Entry, error) {
	if e, ok := r.Get(handle); ok {
		return e, nil
	}

	return Entry{Handle: handle}, nil
}

// Status summarises the loaded snapshot.
func (r *Reader) Status() Status {
	r.mu.RLock()
	defer r.mu.RUnlock()

	st := Status{
		Enabled:   true,
		Handles:   len(r.entries),
		LastCycle: r.generatedAt,
	}
	for _, e := range r.entries {
		if !e.FetchedAt.IsZero() {
			st.Cached++
		}
		if e.LastError != "" {
			st.Errors++
		}
	}

	return st
}

// Warmed reports whether a snapshot has been loaded, so readiness can withhold
// traffic from a reader that has not yet pulled the first snapshot.
func (r *Reader) Warmed() bool {
	return r.loaded.Load()
}
