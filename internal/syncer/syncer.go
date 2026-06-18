// Package syncer periodically refreshes every datasource and publishes the
// resulting superset index, preserving the last good result of any source that
// transiently fails.
package syncer

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/ethpandaops/coredevs/internal/index"
	"github.com/ethpandaops/coredevs/internal/source"
)

// Syncer drives datasource refreshes on an interval and publishes results into
// an index Store.
type Syncer struct {
	logger   *slog.Logger
	store    *index.Store
	sources  []source.Source
	interval time.Duration
	snapshot string
	floors   map[string]int

	mu       sync.Mutex
	lastGood map[string][]source.Membership
	status   map[string]*SourceStatus

	done chan struct{}
	wg   sync.WaitGroup
}

// SourceStatus records the outcome of the most recent fetch for a source.
type SourceStatus struct {
	// Name is the source identifier.
	Name string `json:"name"`
	// LastAttempt is when the source was last fetched.
	LastAttempt time.Time `json:"lastAttempt"`
	// LastSuccess is when the source last succeeded, zero if never.
	LastSuccess time.Time `json:"lastSuccess,omitempty"`
	// LastError is the most recent error message, empty on success.
	LastError string `json:"lastError,omitempty"`
	// Members is the membership count from the last successful fetch.
	Members int `json:"members"`
}

// New constructs a Syncer. snapshot is an optional path for last-good
// persistence; an empty string disables it. floors maps a source name to a
// minimum membership count below which a fetch is treated as a soft failure; a
// missing or zero entry disables the floor for that source.
func New(logger *slog.Logger, store *index.Store, sources []source.Source, interval time.Duration, snapshot string, floors map[string]int) *Syncer {
	return &Syncer{
		logger:   logger.With(slog.String("component", "syncer")),
		store:    store,
		sources:  sources,
		interval: interval,
		snapshot: snapshot,
		floors:   floors,
		lastGood: make(map[string][]source.Membership, len(sources)),
		status:   make(map[string]*SourceStatus, len(sources)),
		done:     make(chan struct{}),
	}
}

// Start seeds the index from the on-disk snapshot for immediate availability,
// performs an initial synchronous sync, then refreshes on the interval in a
// background goroutine until Stop is called or ctx is cancelled.
//
// The snapshot is loaded first so the service serves last-good data even if the
// initial sync degrades; the publish guard in Sync ensures a fresh sync only
// replaces the snapshot when it actually has data.
func (s *Syncer) Start(ctx context.Context) error {
	if snap, err := index.LoadSnapshot(s.snapshot); err != nil {
		s.logger.WarnContext(ctx, "failed to load snapshot", slog.Any("error", err))
	} else if snap != nil {
		s.logger.InfoContext(ctx, "serving index from snapshot",
			slog.Time("generatedAt", snap.GeneratedAt),
			slog.Int("teams", len(snap.Teams)),
		)
		s.store.Set(snap)
	}

	s.Sync(ctx)

	s.wg.Add(1)
	go s.loop(ctx)

	return nil
}

// Stop halts the refresh loop and waits for it to exit.
func (s *Syncer) Stop() error {
	close(s.done)
	s.wg.Wait()

	return nil
}

// Statuses returns a snapshot of per-source fetch status, sorted by name.
func (s *Syncer) Statuses() []SourceStatus {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]SourceStatus, 0, len(s.status))
	for _, st := range s.status {
		out = append(out, *st)
	}

	return out
}

// Sync fetches every source once and returns the freshly built index. Sources
// that fail fall back to their last good result so a transient outage never
// drops a team. It does not publish; callers decide whether to Set the result.
func (s *Syncer) Sync(ctx context.Context) *index.Index {
	var (
		mu       sync.Mutex
		all      []source.Membership
		wg       sync.WaitGroup
		anyFresh bool
	)

	for _, src := range s.sources {
		wg.Add(1)
		go func(src source.Source) {
			defer wg.Done()

			memberships, fresh := s.fetchOne(ctx, src)

			mu.Lock()
			all = append(all, memberships...)
			anyFresh = anyFresh || fresh
			mu.Unlock()
		}(src)
	}

	wg.Wait()

	idx := index.Build(time.Now().UTC(), all)

	// Never replace a populated index with an empty one — an all-sources-fail
	// tick must not wipe last-good data already being served.
	if len(idx.Teams) > 0 || s.store.Get() == nil {
		s.store.Set(idx)
	}

	if anyFresh && len(idx.Teams) > 0 {
		if err := index.Save(idx, s.snapshot); err != nil {
			s.logger.WarnContext(ctx, "failed to save snapshot", slog.Any("error", err))
		}
	}

	return idx
}

func (s *Syncer) loop(ctx context.Context) {
	defer s.wg.Done()

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.done:
			return
		case <-ticker.C:
			s.Sync(ctx)
		}
	}
}

// fetchOne fetches a single source, recording status and falling back to the
// last good result on error. fresh is true when this call produced new data.
func (s *Syncer) fetchOne(ctx context.Context, src source.Source) (memberships []source.Membership, fresh bool) {
	name := src.Name()

	status := s.ensureStatus(name)
	status.LastAttempt = time.Now().UTC()

	memberships, err := src.Fetch(ctx)
	if err != nil {
		status.LastError = err.Error()
		s.logger.ErrorContext(ctx, "source fetch failed",
			slog.String("source", name),
			slog.Any("error", err),
		)

		return s.lastGoodFor(name), false
	}

	if floor := s.floors[name]; floor > 0 && len(memberships) < floor {
		status.LastError = fmt.Sprintf("fetched %d members, below floor of %d — keeping last good", len(memberships), floor)
		s.logger.ErrorContext(ctx, "source below member floor",
			slog.String("source", name),
			slog.Int("members", len(memberships)),
			slog.Int("floor", floor),
		)

		return s.lastGoodFor(name), false
	}

	status.LastError = ""
	status.LastSuccess = time.Now().UTC()
	status.Members = len(memberships)

	s.mu.Lock()
	s.lastGood[name] = memberships
	s.mu.Unlock()

	s.logger.InfoContext(ctx, "source fetched",
		slog.String("source", name),
		slog.Int("members", len(memberships)),
	)

	return memberships, true
}

func (s *Syncer) ensureStatus(name string) *SourceStatus {
	s.mu.Lock()
	defer s.mu.Unlock()

	st, ok := s.status[name]
	if !ok {
		st = &SourceStatus{Name: name}
		s.status[name] = st
	}

	return st
}

func (s *Syncer) lastGoodFor(name string) []source.Membership {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.lastGood[name]
}
