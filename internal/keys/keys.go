// Package keys maintains a rate-paced, in-memory cache of developers' GitHub
// SSH public keys, refreshing them continuously in the background so consumers
// can read a team's authorized_keys without ever calling GitHub themselves.
//
// Refresh strategy: a single walker goroutine walks the desired handle set
// round-robin, fetching one developer's keys at a time. The delay between
// fetches is derived from two knobs — a target refresh interval (how stale a
// handle may get) and a hard requests-per-second ceiling — so the work is
// spread evenly across the window rather than bursting against GitHub.
package keys

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// Source is the provenance label reported for cached keys.
const Source = "github-keys"

// HandlesFunc returns the current set of handles whose keys should be cached.
// It is called at the start of every walk so newly indexed handles are picked
// up and removed handles are pruned.
type HandlesFunc func() []string

// Config tunes the key cache's refresh pacing and upstream endpoint.
type Config struct {
	// Enabled toggles the cache.
	Enabled bool
	// BaseURL is the host serving the `.keys` endpoint (default
	// https://github.com). The cache requests "{BaseURL}/{handle}.keys".
	BaseURL string
	// RefreshInterval is the target staleness: the walker paces itself so every
	// handle is refreshed roughly once per this window.
	RefreshInterval time.Duration
	// MaxRequestsPerSecond is a hard ceiling on the upstream request rate. It
	// bounds load when the handle set is small enough that the even-spread pace
	// would otherwise fetch faster than this.
	MaxRequestsPerSecond float64
	// CacheDir holds one file per handle, persisting each developer's keys as
	// soon as they are fetched so partial progress survives a restart. Empty
	// disables persistence.
	CacheDir string
}

// Entry is a single developer's cached keys and the freshness of that cache.
type Entry struct {
	// Handle is the GitHub login in its requested casing.
	Handle string `json:"handle"`
	// Keys are the developer's SSH public keys, one per element, in upstream
	// order. Empty when the developer publishes none.
	Keys []string `json:"keys"`
	// FetchedAt is when these keys were last successfully fetched. Zero if never.
	FetchedAt time.Time `json:"fetchedAt,omitzero"`
	// LastAttempt is when a fetch was last attempted, success or not.
	LastAttempt time.Time `json:"lastAttempt,omitzero"`
	// LastError is the most recent fetch error, empty on success.
	LastError string `json:"lastError,omitempty"`
}

// Status summarises the cache for the status API and metrics.
type Status struct {
	// Enabled reports whether the cache is running.
	Enabled bool `json:"enabled"`
	// Handles is the number of handles currently tracked.
	Handles int `json:"handles"`
	// Cached is the number of handles with at least one successfully fetched key.
	Cached int `json:"cached"`
	// Errors is the number of handles whose most recent fetch failed.
	Errors int `json:"errors"`
	// LastFetch is the timestamp of the most recent successful single fetch.
	LastFetch time.Time `json:"lastFetch,omitzero"`
	// LastCycle is when the walker last completed a full pass of the handle set.
	LastCycle time.Time `json:"lastCycle,omitzero"`
	// PaceSeconds is the current delay between fetches, in seconds.
	PaceSeconds float64 `json:"paceSeconds"`
}

// Cache is the rate-paced GitHub key cache.
type Cache struct {
	logger    *slog.Logger
	http      *http.Client
	cfg       Config
	handlesFn HandlesFunc
	minDelay  time.Duration

	mu        sync.RWMutex
	entries   map[string]*Entry
	lastFetch time.Time
	lastCycle time.Time
	pace      time.Duration

	done chan struct{}
	wg   sync.WaitGroup
}

// New constructs a key cache. handlesFn supplies the desired handle set on each
// walk; it must be safe to call concurrently.
func New(logger *slog.Logger, httpClient *http.Client, cfg Config, handlesFn HandlesFunc) *Cache {
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://github.com"
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")

	minDelay := time.Duration(0)
	if cfg.MaxRequestsPerSecond > 0 {
		minDelay = time.Duration(float64(time.Second) / cfg.MaxRequestsPerSecond)
	}

	return &Cache{
		logger:    logger.With(slog.String("component", "keys")),
		http:      httpClient,
		cfg:       cfg,
		handlesFn: handlesFn,
		minDelay:  minDelay,
		entries:   make(map[string]*Entry, 0),
		done:      make(chan struct{}),
	}
}

// Start seeds the cache from the on-disk per-handle files for immediate
// availability, then walks the handle set continuously in the background until
// Stop is called or ctx is cancelled.
func (c *Cache) Start(ctx context.Context) error {
	if cached, err := loadCache(c.cfg.CacheDir); err != nil {
		c.logger.WarnContext(ctx, "failed to load keys cache", slog.Any("error", err))
	} else if len(cached) > 0 {
		c.mu.Lock()
		c.entries = cached
		c.mu.Unlock()
		c.logger.InfoContext(ctx, "serving keys from cache", slog.Int("handles", len(cached)))
	}

	c.wg.Add(1)
	go c.walk(ctx)

	return nil
}

// Stop halts the walker and waits for it to exit.
func (c *Cache) Stop() error {
	close(c.done)
	c.wg.Wait()

	return nil
}

// Get returns the cached entry for a handle, matching case-insensitively. The
// second return is false when the handle has never been seen.
func (c *Cache) Get(handle string) (Entry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	e, ok := c.entries[strings.ToLower(handle)]
	if !ok {
		return Entry{}, false
	}

	return *e, true
}

// Refresh fetches a single handle's keys immediately, bypassing the walker's
// pace, and stores the result. It is intended for serving a cold handle on its
// first request; callers must bound it with a context timeout. It performs at
// most one upstream request.
func (c *Cache) Refresh(ctx context.Context, handle string) (Entry, error) {
	return c.fetchOne(ctx, handle), nil
}

// Status returns a snapshot of the cache state.
func (c *Cache) Status() Status {
	c.mu.RLock()
	defer c.mu.RUnlock()

	st := Status{
		Enabled:     true,
		Handles:     len(c.entries),
		LastFetch:   c.lastFetch,
		LastCycle:   c.lastCycle,
		PaceSeconds: c.pace.Seconds(),
	}
	for _, e := range c.entries {
		if !e.FetchedAt.IsZero() {
			st.Cached++
		}
		if e.LastError != "" {
			st.Errors++
		}
	}

	return st
}

// walk continuously refreshes the desired handle set round-robin, pacing each
// fetch so a full pass spans RefreshInterval without exceeding the RPS ceiling.
func (c *Cache) walk(ctx context.Context) {
	defer c.wg.Done()

	for {
		handles := c.handlesFn()
		c.prune(ctx, handles)

		if len(handles) == 0 {
			if !c.sleep(ctx, time.Minute) {
				return
			}

			continue
		}

		delay := c.paceFor(len(handles))

		c.mu.Lock()
		c.pace = delay
		c.mu.Unlock()

		c.logger.InfoContext(ctx, "starting key refresh cycle",
			slog.Int("handles", len(handles)),
			slog.Duration("pace", delay),
		)

		for _, h := range handles {
			select {
			case <-ctx.Done():
				return
			case <-c.done:
				return
			default:
			}

			c.fetchOne(ctx, h)

			if !c.sleep(ctx, delay) {
				return
			}
		}

		c.mu.Lock()
		c.lastCycle = time.Now().UTC()
		c.mu.Unlock()
	}
}

// paceFor returns the delay between fetches: the desired set spread evenly over
// RefreshInterval, clamped so it never fetches faster than the RPS ceiling.
func (c *Cache) paceFor(n int) time.Duration {
	if n <= 0 {
		return c.minDelay
	}

	return max(c.cfg.RefreshInterval/time.Duration(n), c.minDelay)
}

// fetchOne fetches a single handle's keys, updates the cache and returns the
// resulting entry. On error it preserves the last good keys and records the
// error so a transient failure never drops a developer's keys.
func (c *Cache) fetchOne(ctx context.Context, handle string) Entry {
	now := time.Now().UTC()

	keys, err := c.fetch(ctx, handle)

	key := strings.ToLower(handle)

	c.mu.Lock()
	e, ok := c.entries[key]
	if !ok {
		e = &Entry{Handle: handle}
		c.entries[key] = e
	}
	e.LastAttempt = now

	if err != nil {
		e.LastError = err.Error()
		snapshot := *e
		c.mu.Unlock()

		c.logger.WarnContext(ctx, "key fetch failed",
			slog.String("handle", handle), slog.Any("error", err))

		return snapshot
	}

	e.Keys = keys
	e.FetchedAt = now
	e.LastError = ""
	c.lastFetch = now
	snapshot := *e
	c.mu.Unlock()

	// Persist this one handle immediately so a restart keeps every developer
	// fetched so far, rather than waiting for a full pass to complete.
	if err := writeEntry(c.cfg.CacheDir, key, snapshot); err != nil {
		c.logger.WarnContext(ctx, "failed to persist keys",
			slog.String("handle", handle), slog.Any("error", err))
	}

	return snapshot
}

// fetch performs the single upstream request for a handle's keys.
func (c *Cache) fetch(ctx context.Context, handle string) ([]string, error) {
	endpoint := fmt.Sprintf("%s/%s.keys", c.cfg.BaseURL, url.PathEscape(handle))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "text/plain")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %q keys: %w", handle, err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		// continue below
	case http.StatusNotFound:
		return nil, fmt.Errorf("handle %q not found", handle)
	case http.StatusForbidden, http.StatusTooManyRequests:
		return nil, fmt.Errorf("handle %q rate limited (status %d)", handle, resp.StatusCode)
	default:
		return nil, fmt.Errorf("fetch %q keys: unexpected status %d", handle, resp.StatusCode)
	}

	return parseKeys(resp.Body)
}

// prune drops cached entries, in memory and on disk, for handles no longer in
// the desired set.
func (c *Cache) prune(ctx context.Context, handles []string) {
	want := make(map[string]struct{}, len(handles))
	for _, h := range handles {
		want[strings.ToLower(h)] = struct{}{}
	}

	var removed []string

	c.mu.Lock()
	for key := range c.entries {
		if _, ok := want[key]; !ok {
			delete(c.entries, key)
			removed = append(removed, key)
		}
	}
	c.mu.Unlock()

	for _, key := range removed {
		if err := removeEntry(c.cfg.CacheDir, key); err != nil {
			c.logger.WarnContext(ctx, "failed to remove cached keys",
				slog.String("handle", key), slog.Any("error", err))
		}
	}
}

// sleep waits for d or returns false if the cache is shutting down.
func (c *Cache) sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}

	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-c.done:
		return false
	case <-t.C:
		return true
	}
}

// parseKeys reads newline-separated SSH public keys from r, skipping blank
// lines, and returns them in order.
func parseKeys(r io.Reader) ([]string, error) {
	var keys []string

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			keys = append(keys, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read keys: %w", err)
	}

	sort.Strings(keys)

	return keys, nil
}
