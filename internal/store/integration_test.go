package store_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/coredevs/internal/index"
	"github.com/ethpandaops/coredevs/internal/keys"
	"github.com/ethpandaops/coredevs/internal/snapshot"
	"github.com/ethpandaops/coredevs/internal/source"
	"github.com/ethpandaops/coredevs/internal/store"
)

// TestSnapshotRoundTrip is the regression guard for the no-split-brain design:
// a snapshot published to Postgres must load back identically, so every reader
// reconstructs the same index and keys.
func TestSnapshotRoundTrip(t *testing.T) {
	dsn := os.Getenv("CORE_TEST_PG")
	if dsn == "" {
		t.Skip("CORE_TEST_PG not set; skipping Postgres integration test")
	}

	ctx := context.Background()
	s, err := store.New(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), dsn)
	require.NoError(t, err)
	t.Cleanup(s.Close)

	idx := index.Build(time.Unix(1700000000, 0).UTC(), []source.Membership{
		{Handle: "Alice", Team: "geth", Source: source.NameManual},
		{Handle: "Bob", Team: "teku", Source: source.NameProtocolGuild},
	})

	want := &snapshot.Snapshot{
		Generation:  42,
		GeneratedAt: time.Unix(1700000000, 0).UTC(),
		Index:       idx,
		Keys: map[string]keys.Entry{
			"alice": {Handle: "Alice", Keys: []string{"ssh-ed25519 AAA"}, FetchedAt: time.Unix(1, 0).UTC()},
		},
	}

	data, err := snapshot.Marshal(want)
	require.NoError(t, err)
	require.NoError(t, s.Save(ctx, want.Generation, data))

	loaded, ok, err := s.Load(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, want.Generation, loaded.Generation)

	got, err := snapshot.Unmarshal(loaded.Data)
	require.NoError(t, err)

	// Two readers parsing the same bytes get the same teams and keys.
	assert.Equal(t, idx.TeamSlugs(), got.Index.TeamSlugs())
	assert.Equal(t, []string{"ssh-ed25519 AAA"}, got.Keys["alice"].Keys)
	assert.Len(t, got.Index.Members("geth", ""), 1)
}
