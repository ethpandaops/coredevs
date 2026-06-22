package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/ethpandaops/coredevs/internal/index"
	"github.com/ethpandaops/coredevs/internal/keys"
	"github.com/ethpandaops/coredevs/internal/snapshot"
	"github.com/ethpandaops/coredevs/internal/store"
	"github.com/ethpandaops/coredevs/internal/syncer"
)

// atomicStatuses holds the per-source status published in the latest snapshot,
// so reader pods can serve /api/v1/sources without running a syncer.
type atomicStatuses struct {
	mu sync.RWMutex
	st []syncer.SourceStatus
}

func (a *atomicStatuses) Get() []syncer.SourceStatus {
	a.mu.RLock()
	defer a.mu.RUnlock()

	return a.st
}

func (a *atomicStatuses) Set(st []syncer.SourceStatus) {
	a.mu.Lock()
	a.st = st
	a.mu.Unlock()
}

// publisher periodically serialises the writer's live state (index, keys, source
// status) into one snapshot and writes it to the shared store, so every reader
// converges on the same data.
type publisher struct {
	logger   *slog.Logger
	store    *store.Store
	index    *index.Store
	keys     *keys.Cache
	statuses func() []syncer.SourceStatus
	interval time.Duration

	done chan struct{}
	wg   sync.WaitGroup
}

func newPublisher(logger *slog.Logger, st *store.Store, idx *index.Store, kc *keys.Cache, statuses func() []syncer.SourceStatus, interval time.Duration) *publisher {
	return &publisher{
		logger:   logger.With(slog.String("component", "publisher")),
		store:    st,
		index:    idx,
		keys:     kc,
		statuses: statuses,
		interval: interval,
		done:     make(chan struct{}),
	}
}

// Start publishes once immediately, then on the interval until Stop or ctx done.
func (p *publisher) Start(ctx context.Context) {
	p.publish(ctx)

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()

		ticker := time.NewTicker(p.interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-p.done:
				return
			case <-ticker.C:
				p.publish(ctx)
			}
		}
	}()
}

func (p *publisher) Stop() {
	close(p.done)
	p.wg.Wait()
}

func (p *publisher) publish(ctx context.Context) {
	idx := p.index.Get()
	if idx == nil {
		return
	}

	var keyEntries map[string]keys.Entry
	if p.keys != nil {
		keyEntries = p.keys.Snapshot()
	}

	snap := &snapshot.Snapshot{
		Generation:  time.Now().UnixNano(),
		GeneratedAt: time.Now().UTC(),
		Index:       idx,
		Keys:        keyEntries,
		Sources:     p.statuses(),
	}

	data, err := snapshot.Marshal(snap)
	if err != nil {
		p.logger.ErrorContext(ctx, "failed to marshal snapshot", slog.Any("error", err))

		return
	}

	if err := p.store.Save(ctx, snap.Generation, data); err != nil {
		p.logger.ErrorContext(ctx, "failed to publish snapshot", slog.Any("error", err))

		return
	}

	p.logger.InfoContext(ctx, "published snapshot",
		slog.Int("teams", len(idx.Teams)),
		slog.Int("keys", len(keyEntries)),
	)
}

// poller loads the published snapshot from the shared store into the serving
// structures on every reader pod, so all readers serve identical data.
type poller struct {
	logger   *slog.Logger
	store    *store.Store
	index    *index.Store
	keys     *keys.Reader
	statuses *atomicStatuses
	interval time.Duration

	lastGen int64
	done    chan struct{}
	wg      sync.WaitGroup
}

func newPoller(logger *slog.Logger, st *store.Store, idx *index.Store, kr *keys.Reader, statuses *atomicStatuses, interval time.Duration) *poller {
	return &poller{
		logger:   logger.With(slog.String("component", "poller")),
		store:    st,
		index:    idx,
		keys:     kr,
		statuses: statuses,
		interval: interval,
		done:     make(chan struct{}),
	}
}

// Start loads once immediately (so readiness can clear promptly), then polls.
func (p *poller) Start(ctx context.Context) {
	if err := p.load(ctx); err != nil {
		p.logger.WarnContext(ctx, "initial snapshot load failed", slog.Any("error", err))
	}

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()

		ticker := time.NewTicker(p.interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-p.done:
				return
			case <-ticker.C:
				if err := p.load(ctx); err != nil {
					p.logger.WarnContext(ctx, "snapshot load failed; serving last good",
						slog.Any("error", err))
				}
			}
		}
	}()
}

func (p *poller) Stop() {
	close(p.done)
	p.wg.Wait()
}

// load pulls the latest snapshot if its generation changed and swaps it in.
func (p *poller) load(ctx context.Context) error {
	gen, ok, err := p.store.Generation(ctx)
	if err != nil {
		return err
	}
	if !ok || gen == p.lastGen {
		return nil
	}

	snap, ok, err := p.store.Load(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}

	parsed, err := snapshot.Unmarshal(snap.Data)
	if err != nil {
		return err
	}

	if parsed.Index != nil {
		p.index.Set(parsed.Index)
	}
	p.keys.Replace(parsed.Keys, parsed.GeneratedAt)
	p.statuses.Set(parsed.Sources)
	p.lastGen = gen

	p.logger.InfoContext(ctx, "loaded snapshot",
		slog.Int64("generation", gen),
		slog.Int("keys", len(parsed.Keys)),
	)

	return nil
}
