// Package syncer periodically refreshes every datasource and publishes the
// resulting superset index, preserving the last good result of any source that
// transiently fails.
package syncer

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
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
	floors   map[string]Floor
	exclude  map[string]bool

	mu       sync.Mutex
	lastGood map[string][]source.Membership
	status   map[string]*SourceStatus

	done chan struct{}
	wg   sync.WaitGroup
}

// Floor is a per-source sanity bound on a fetch. A result that falls below
// either bound is discarded in favour of the source's last good result.
type Floor struct {
	// MinMembers is the fewest memberships a fetch may produce. 0 disables it.
	MinMembers int
	// MinTeams is the fewest distinct teams those memberships may span. 0
	// disables it. A member count alone cannot catch an upstream restructure
	// that still parses plenty of people but collapses them onto a handful of
	// teams, which is what a section rename does.
	MinTeams int
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
	// Teams is the number of distinct teams that fetch spanned.
	Teams int `json:"teams"`
	// Warnings are non-fatal problems the source reported about its most recent
	// fetch, such as an upstream section that no longer exists.
	Warnings []string `json:"warnings,omitempty"`
}

// Reporter is implemented by a source that can report non-fatal problems with
// its most recent fetch. A source that parses an upstream document uses this to
// flag structure it expected and did not find, which is a break in the making
// even while the fetch still returns plenty of data.
type Reporter interface {
	Warnings() []string
}

// New constructs a Syncer. snapshot is an optional path for last-good
// persistence; an empty string disables it. floors maps a source name to the
// sanity bounds below which a fetch is treated as a soft failure; a missing
// entry disables both bounds for that source. exclude is a set of lowercased
// handles dropped from every team before indexing — used to suppress stale
// handles an upstream source still lists (e.g. a renamed GitHub account).
func New(logger *slog.Logger, store *index.Store, sources []source.Source, interval time.Duration, snapshot string, floors map[string]Floor, exclude map[string]bool) *Syncer {
	return &Syncer{
		logger:   logger.With(slog.String("component", "syncer")),
		store:    store,
		sources:  sources,
		interval: interval,
		snapshot: snapshot,
		floors:   floors,
		exclude:  exclude,
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
		s.restoreLastGood(ctx, snap)
	}

	s.Sync(ctx)

	s.wg.Add(1)
	go s.loop(ctx)

	return nil
}

// restoreLastGood seeds each source's fallback from a snapshot, so a process
// that restarts while an upstream is broken still has that upstream's last good
// result to serve. Without it the first degraded fetch after a restart has
// nothing to fall back on and publishes an index missing that source entirely.
func (s *Syncer) restoreLastGood(ctx context.Context, snap *index.Index) {
	bySource := make(map[string][]source.Membership)
	for _, m := range snap.Memberships() {
		bySource[m.Source] = append(bySource[m.Source], m)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for name, memberships := range bySource {
		s.lastGood[name] = memberships
		s.logger.InfoContext(ctx, "restored last good result from snapshot",
			slog.String("source", name),
			slog.Int("members", len(memberships)),
		)
	}
}

// Seed publishes an index the syncer did not fetch and restores each source's
// last good result from it. A writer pod whose disk is ephemeral calls this with
// the snapshot the cluster already published, so a restart begins from the last
// healthy state instead of from nothing. Call it before Start.
func (s *Syncer) Seed(ctx context.Context, idx *index.Index) {
	if idx == nil || len(idx.Teams) == 0 {
		return
	}

	s.logger.InfoContext(ctx, "seeding index from published snapshot",
		slog.Time("generatedAt", idx.GeneratedAt),
		slog.Int("teams", len(idx.Teams)),
	)

	s.store.Set(idx)
	s.restoreLastGood(ctx, idx)
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
		starved  []string
	)

	for _, src := range s.sources {
		wg.Add(1)
		go func(src source.Source) {
			defer wg.Done()

			memberships, fresh := s.fetchOne(ctx, src)

			mu.Lock()
			all = append(all, memberships...)
			anyFresh = anyFresh || fresh
			if !fresh && len(memberships) == 0 {
				starved = append(starved, src.Name())
			}
			mu.Unlock()
		}(src)
	}

	wg.Wait()

	sort.Strings(starved)

	if len(s.exclude) > 0 {
		kept := all[:0]
		for _, m := range all {
			if s.exclude[strings.ToLower(m.Handle)] {
				continue
			}
			kept = append(kept, m)
		}
		all = kept
	}

	idx := index.Build(time.Now().UTC(), all)

	// A starved source failed and had no last good result to stand in for it, so
	// this index is missing everything that source contributes. Keep serving
	// what is already published rather than replacing it with a partial view,
	// and never let a partial view reach disk — the snapshot is what a restart
	// rebuilds from, so overwriting it turns a transient upstream break into a
	// permanent one.
	if len(starved) > 0 && s.store.Get() != nil {
		s.logger.ErrorContext(ctx, "skipping publish, sources contributed nothing",
			slog.Any("sources", starved),
			slog.Int("teams", len(idx.Teams)),
		)

		return idx
	}

	// Never replace a populated index with an empty one — an all-sources-fail
	// tick must not wipe last-good data already being served.
	if len(idx.Teams) > 0 || s.store.Get() == nil {
		s.store.Set(idx)
	}

	if anyFresh && len(idx.Teams) > 0 && len(starved) == 0 {
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

	// Recorded before the error check: a source reports what it found wrong with
	// the document whether or not it then rejected the fetch, and the rejected
	// case is the one worth alerting on.
	status.Warnings = sourceWarnings(src)

	if err != nil {
		status.LastError = err.Error()
		s.logger.ErrorContext(ctx, "source fetch failed",
			slog.String("source", name),
			slog.Any("error", err),
		)

		return s.lastGoodFor(name), false
	}

	floor := s.floors[name]

	if floor.MinMembers > 0 && len(memberships) < floor.MinMembers {
		status.LastError = fmt.Sprintf("fetched %d members, below floor of %d — keeping last good", len(memberships), floor.MinMembers)
		s.logger.ErrorContext(ctx, "source below member floor",
			slog.String("source", name),
			slog.Int("members", len(memberships)),
			slog.Int("floor", floor.MinMembers),
		)

		return s.lastGoodFor(name), false
	}

	if teams := distinctTeams(memberships); floor.MinTeams > 0 && teams < floor.MinTeams {
		status.LastError = fmt.Sprintf("fetched %d members across %d teams, below floor of %d teams — keeping last good", len(memberships), teams, floor.MinTeams)
		s.logger.ErrorContext(ctx, "source below team floor",
			slog.String("source", name),
			slog.Int("members", len(memberships)),
			slog.Int("teams", teams),
			slog.Int("floor", floor.MinTeams),
		)

		return s.lastGoodFor(name), false
	}

	status.LastError = ""
	status.LastSuccess = time.Now().UTC()
	status.Members = len(memberships)
	status.Teams = distinctTeams(memberships)

	s.mu.Lock()
	s.lastGood[name] = memberships
	s.mu.Unlock()

	s.logger.InfoContext(ctx, "source fetched",
		slog.String("source", name),
		slog.Int("members", len(memberships)),
	)

	return memberships, true
}

// sourceWarnings collects the non-fatal problems a source reports, if it
// reports any at all.
func sourceWarnings(src source.Source) []string {
	r, ok := src.(Reporter)
	if !ok {
		return nil
	}

	return r.Warnings()
}

// distinctTeams counts the teams a set of memberships spans.
func distinctTeams(memberships []source.Membership) int {
	seen := make(map[string]struct{}, len(memberships))
	for _, m := range memberships {
		seen[m.Team] = struct{}{}
	}

	return len(seen)
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
