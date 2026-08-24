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

	return New(slog.Default(), store, []source.Source{src}, time.Hour, snapshot, nil, nil), store
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

func TestMemberFloorRejectsDegradedFetch(t *testing.T) {
	src := &fakeSource{
		name:    source.NameProtocolGuild,
		members: []source.Membership{{Handle: "a", Team: "teku", Source: source.NameProtocolGuild}},
	}
	store := index.NewStore()
	s := New(slog.Default(), store, []source.Source{src}, time.Hour, "",
		map[string]Floor{source.NameProtocolGuild: {MinMembers: 3}}, nil)

	require.NoError(t, s.Start(context.Background()))
	t.Cleanup(func() { _ = s.Stop() })

	// First fetch (1 member) is below the floor of 3, so nothing is published.
	assert.Nil(t, store.Get().Members("teku", ""))
	st := s.Statuses()
	require.Len(t, st, 1)
	assert.Contains(t, st[0].LastError, "below floor")

	// A healthy fetch (3 members) clears the floor and publishes.
	src.members = []source.Membership{
		{Handle: "a", Team: "teku", Source: source.NameProtocolGuild},
		{Handle: "b", Team: "teku", Source: source.NameProtocolGuild},
		{Handle: "c", Team: "teku", Source: source.NameProtocolGuild},
	}
	s.Sync(context.Background())
	assert.Len(t, store.Get().Members("teku", ""), 3)
}

func TestTeamFloorKeepsLastGood(t *testing.T) {
	healthy := []source.Membership{
		{Handle: "a", Team: "teku", Source: source.NameProtocolGuild},
		{Handle: "b", Team: "geth", Source: source.NameProtocolGuild},
		{Handle: "c", Team: "reth", Source: source.NameProtocolGuild},
	}

	src := &fakeSource{name: source.NameProtocolGuild, members: healthy}
	store := index.NewStore()
	s := New(slog.Default(), store, []source.Source{src}, time.Hour, "",
		map[string]Floor{source.NameProtocolGuild: {MinTeams: 3}}, nil)

	require.NoError(t, s.Start(context.Background()))
	t.Cleanup(func() { _ = s.Stop() })

	assert.Len(t, store.Get().Members("geth", ""), 1)

	// An upstream section rename keeps the member count but funnels everyone
	// onto one team. The member floor cannot see it; the team floor can.
	src.members = []source.Membership{
		{Handle: "a", Team: "teku", Source: source.NameProtocolGuild},
		{Handle: "b", Team: "teku", Source: source.NameProtocolGuild},
		{Handle: "c", Team: "teku", Source: source.NameProtocolGuild},
	}
	s.Sync(context.Background())

	st := s.Statuses()
	require.Len(t, st, 1)
	assert.Contains(t, st[0].LastError, "below floor of 3 teams")
	assert.Len(t, store.Get().Members("geth", ""), 1, "last good result is kept")
	assert.Len(t, store.Get().Members("teku", ""), 1, "collapsed result is not published")
}

func TestExcludeDropsStaleHandle(t *testing.T) {
	src := &fakeSource{
		name: source.NameProtocolGuild,
		members: []source.Membership{
			{Handle: "somnathb1", Team: "erigon", Source: source.NameProtocolGuild},
			{Handle: "yperbasis", Team: "erigon", Source: source.NameProtocolGuild},
		},
	}
	store := index.NewStore()
	// Exclude is matched case-insensitively against a lowercased set.
	s := New(slog.Default(), store, []source.Source{src}, time.Hour, "", nil,
		map[string]bool{"somnathb1": true})

	require.NoError(t, s.Start(context.Background()))
	t.Cleanup(func() { _ = s.Stop() })

	got := store.Get().Members("erigon", "")
	require.Len(t, got, 1)
	assert.Equal(t, "yperbasis", got[0].Handle)
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

// TestRestartDuringUpstreamBreakKeepsSnapshot covers the failure this guard
// exists for: the process restarts while the upstream document is broken, so
// the in-memory last good result is gone and the rejected source contributes
// nothing. The healthy source still returns data, which is enough to build a
// non-empty index — and that index must neither be served nor written over the
// snapshot, because the snapshot is all that remains of the good state.
func TestRestartDuringUpstreamBreakKeepsSnapshot(t *testing.T) {
	snapPath := filepath.Join(t.TempDir(), "index.json")

	seed := index.Build(time.Now().UTC(), []source.Membership{
		{Handle: "rolfyone", Team: "teku", Source: source.NameProtocolGuild},
		{Handle: "gballet", Team: "geth", Source: source.NameProtocolGuild},
		{Handle: "samcm", Team: "ethpandaops", Source: source.NameManual},
	})
	require.NoError(t, index.Save(seed, snapPath))

	broken := &fakeSource{name: source.NameProtocolGuild, err: errors.New("sections missing")}
	healthy := &fakeSource{
		name:    source.NameManual,
		members: []source.Membership{{Handle: "samcm", Team: "ethpandaops", Source: source.NameManual}},
	}

	store := index.NewStore()
	s := New(slog.Default(), store, []source.Source{broken, healthy}, time.Hour, snapPath, nil, nil)

	require.NoError(t, s.Start(context.Background()))
	t.Cleanup(func() { _ = s.Stop() })

	// The snapshot restored the Protocol Guild source's last good result, so the
	// failing fetch falls back to it instead of contributing nothing.
	assert.Len(t, store.Get().Members("teku", ""), 1)
	assert.Len(t, store.Get().Members("geth", ""), 1)

	// Because that source is no longer starved, the index still moves: a change
	// to a healthy source publishes rather than being held back for as long as
	// the upstream stays broken.
	healthy.members = append(healthy.members,
		source.Membership{Handle: "savid", Team: "ethpandaops", Source: source.NameManual})
	s.Sync(context.Background())

	assert.Len(t, store.Get().Members("ethpandaops", ""), 2, "healthy sources keep updating")
	assert.Len(t, store.Get().Members("teku", ""), 1, "restored fallback is still served")

	onDisk, err := index.LoadSnapshot(snapPath)
	require.NoError(t, err)
	assert.Len(t, onDisk.Members("teku", ""), 1, "snapshot still holds the good state")
	assert.Len(t, onDisk.Members("ethpandaops", ""), 2, "and takes the healthy update")
}

// TestStarvedSourceDoesNotOverwriteSnapshot covers the same failure with no
// last good result available at all: the partial index must stay off disk.
func TestStarvedSourceDoesNotOverwriteSnapshot(t *testing.T) {
	snapPath := filepath.Join(t.TempDir(), "index.json")

	seed := index.Build(time.Now().UTC(), []source.Membership{
		{Handle: "rolfyone", Team: "teku", Source: source.NameProtocolGuild},
	})
	require.NoError(t, index.Save(seed, snapPath))

	// A source name the snapshot carries nothing for, so there is no fallback.
	broken := &fakeSource{name: source.NameGitHubOrg, err: errors.New("github down")}
	healthy := &fakeSource{
		name:    source.NameManual,
		members: []source.Membership{{Handle: "samcm", Team: "ethpandaops", Source: source.NameManual}},
	}

	store := index.NewStore()
	s := New(slog.Default(), store, []source.Source{broken, healthy}, time.Hour, snapPath, nil, nil)

	require.NoError(t, s.Start(context.Background()))
	t.Cleanup(func() { _ = s.Stop() })

	onDisk, err := index.LoadSnapshot(snapPath)
	require.NoError(t, err)
	assert.Len(t, onDisk.Members("teku", ""), 1, "partial index must not reach disk")
	assert.Empty(t, onDisk.Members("ethpandaops", ""))

	assert.Len(t, store.Get().Members("teku", ""), 1, "keeps serving the snapshot")
}

// warningSource reports a non-fatal problem alongside a successful fetch.
type warningSource struct {
	fakeSource
	warnings []string
}

func (w *warningSource) Warnings() []string { return w.warnings }

func TestSourceWarningsSurfaceInStatus(t *testing.T) {
	src := &warningSource{
		fakeSource: fakeSource{
			name: source.NameProtocolGuild,
			members: []source.Membership{
				{Handle: "a", Team: "teku", Source: source.NameProtocolGuild},
				{Handle: "b", Team: "geth", Source: source.NameProtocolGuild},
			},
		},
		warnings: []string{"Nethermind"},
	}

	s, _ := newTestSyncer(t, src, "")
	require.NoError(t, s.Start(context.Background()))
	t.Cleanup(func() { _ = s.Stop() })

	st := s.Statuses()
	require.Len(t, st, 1)
	assert.Equal(t, 2, st[0].Teams)
	assert.Equal(t, []string{"Nethermind"}, st[0].Warnings)
}

// A source that rejects its own fetch still knows what was wrong with the
// document, and that is the case most worth alerting on — so the warnings must
// survive the error path.
func TestSourceWarningsSurviveFetchFailure(t *testing.T) {
	src := &warningSource{
		fakeSource: fakeSource{
			name: source.NameProtocolGuild,
			err:  errors.New("5 of 24 configured sections missing from document"),
		},
		warnings: []string{"Geth (EF)", "Nethermind"},
	}

	s, _ := newTestSyncer(t, src, "")
	require.NoError(t, s.Start(context.Background()))
	t.Cleanup(func() { _ = s.Stop() })

	st := s.Statuses()
	require.Len(t, st, 1)
	assert.Equal(t, []string{"Geth (EF)", "Nethermind"}, st[0].Warnings)
	assert.Contains(t, st[0].LastError, "sections missing")
}

// A writer pod's disk is ephemeral, so it seeds from the snapshot the cluster
// published rather than from a file. Seed must restore the per-source fallback
// as well as the served index, or the first degraded fetch after a restart
// republishes a gutted index over the good one.
func TestSeedRestoresFallbackForEphemeralDisk(t *testing.T) {
	published := index.Build(time.Now().UTC(), []source.Membership{
		{Handle: "rolfyone", Team: "teku", Source: source.NameProtocolGuild},
		{Handle: "gballet", Team: "geth", Source: source.NameProtocolGuild},
	})

	broken := &fakeSource{name: source.NameProtocolGuild, err: errors.New("sections missing")}
	healthy := &fakeSource{
		name:    source.NameManual,
		members: []source.Membership{{Handle: "samcm", Team: "ethpandaops", Source: source.NameManual}},
	}

	store := index.NewStore()
	// No snapshot path: the only starting point is what Seed provides.
	s := New(slog.Default(), store, []source.Source{broken, healthy}, time.Hour, "", nil, nil)

	s.Seed(context.Background(), published)
	require.NoError(t, s.Start(context.Background()))
	t.Cleanup(func() { _ = s.Stop() })

	assert.Len(t, store.Get().Members("teku", ""), 1)
	assert.Len(t, store.Get().Members("geth", ""), 1)

	// The seeded fallback also keeps the index live: the broken source is no
	// longer starved, so healthy sources still publish their updates.
	healthy.members = append(healthy.members,
		source.Membership{Handle: "savid", Team: "ethpandaops", Source: source.NameManual})
	s.Sync(context.Background())

	assert.Len(t, store.Get().Members("ethpandaops", ""), 2, "healthy sources keep updating")
	assert.Len(t, store.Get().Members("teku", ""), 1, "seeded fallback is still served")
}

func TestSeedIgnoresEmptyIndex(t *testing.T) {
	src := &fakeSource{
		name:    source.NameManual,
		members: []source.Membership{{Handle: "samcm", Team: "ethpandaops", Source: source.NameManual}},
	}

	store := index.NewStore()
	s := New(slog.Default(), store, []source.Source{src}, time.Hour, "", nil, nil)

	// A first deploy has nothing published yet; seeding must not block the sync.
	s.Seed(context.Background(), nil)
	s.Seed(context.Background(), index.Build(time.Now().UTC(), nil))

	require.NoError(t, s.Start(context.Background()))
	t.Cleanup(func() { _ = s.Stop() })

	assert.Len(t, store.Get().Members("ethpandaops", ""), 1)
}
