package syncer

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/coredevs/internal/index"
	"github.com/ethpandaops/coredevs/internal/source"
)

// fakeSource returns a scripted result and counts calls.
type fakeSource struct {
	name    string
	members []source.Membership
	err     error
	calls   atomic.Int32
}

func (f *fakeSource) Name() string { return f.name }

func (f *fakeSource) Fetch(_ context.Context) ([]source.Membership, error) {
	f.calls.Add(1)
	if f.err != nil {
		return nil, f.err
	}

	return f.members, nil
}

func newTestSyncer(t *testing.T, src source.Source, snapshot string) (*Syncer, *index.Store) {
	t.Helper()
	store := index.NewStore()

	return New(slog.Default(), store, []source.Source{src}, time.Hour, snapshot), store
}

func TestStartSeedsFromSnapshotWhenSyncDegrades(t *testing.T) {
	snapPath := filepath.Join(t.TempDir(), "index.json")

	// Persist a good snapshot, then start with a source that fails. The service
	// must serve the snapshot rather than an empty index.
	seed := index.Build(time.Now().UTC(), []source.Membership{
		{Handle: "rolfyone", Team: "teku", Source: source.NameProtocolGuild},
	})
	require.NoError(t, index.Save(seed, snapPath))

	src := &fakeSource{name: source.NameProtocolGuild, err: errors.New("upstream down")}
	s, store := newTestSyncer(t, src, snapPath)

	require.NoError(t, s.Start(context.Background()))
	t.Cleanup(func() { _ = s.Stop() })

	require.NotNil(t, store.Get())
	assert.Len(t, store.Get().Members("teku", ""), 1, "snapshot served despite failed sync")
}

func TestSyncPublishesFreshDataOverSnapshot(t *testing.T) {
	src := &fakeSource{
		name:    source.NameProtocolGuild,
		members: []source.Membership{{Handle: "a", Team: "teku", Source: source.NameProtocolGuild}},
	}
	s, store := newTestSyncer(t, src, "")

	require.NoError(t, s.Start(context.Background()))
	t.Cleanup(func() { _ = s.Stop() })

	require.NotNil(t, store.Get())
	assert.Len(t, store.Get().Members("teku", ""), 1)

	st := s.Statuses()
	require.Len(t, st, 1)
	assert.Equal(t, 1, st[0].Members)
	assert.Empty(t, st[0].LastError)
}

func TestSyncKeepsLastGoodOnLaterFailure(t *testing.T) {
	src := &fakeSource{
		name:    source.NameProtocolGuild,
		members: []source.Membership{{Handle: "a", Team: "teku", Source: source.NameProtocolGuild}},
	}
	s, store := newTestSyncer(t, src, "")

	require.NoError(t, s.Start(context.Background()))
	t.Cleanup(func() { _ = s.Stop() })
	require.Len(t, store.Get().Members("teku", ""), 1)

	// Source now fails; a manual Sync must not wipe the served team.
	src.err = errors.New("now failing")
	s.Sync(context.Background())

	assert.Len(t, store.Get().Members("teku", ""), 1, "last-good retained on failure")
}
