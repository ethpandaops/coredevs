package store

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testStore connects to the Postgres named by CORE_TEST_PG, skipping the test
// when it is unset so the suite stays runnable without a database.
func testStore(t *testing.T) *Store {
	t.Helper()

	dsn := os.Getenv("CORE_TEST_PG")
	if dsn == "" {
		t.Skip("CORE_TEST_PG not set; skipping Postgres integration test")
	}

	s, err := New(t.Context(), slog.New(slog.NewTextHandler(io.Discard, nil)), dsn)
	require.NoError(t, err)
	t.Cleanup(s.Close)

	_, err = s.pool.Exec(context.Background(), "DROP TABLE IF EXISTS coredevs_snapshot")
	require.NoError(t, err)
	require.NoError(t, s.ensureSchema(context.Background()))

	return s
}

func TestSaveLoadGeneration(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// No snapshot yet.
	_, ok, err := s.Generation(ctx)
	require.NoError(t, err)
	assert.False(t, ok, "no generation before first save")

	_, ok, err = s.Load(ctx)
	require.NoError(t, err)
	assert.False(t, ok, "no snapshot before first save")

	// First save.
	require.NoError(t, s.Save(ctx, 100, []byte(`{"hello":"world"}`)))

	gen, ok, err := s.Generation(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, int64(100), gen)

	snap, ok, err := s.Load(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, int64(100), snap.Generation)
	assert.Equal(t, []byte(`{"hello":"world"}`), snap.Data)
	assert.False(t, snap.UpdatedAt.IsZero())

	// Overwrite (last-write-wins, still one row).
	require.NoError(t, s.Save(ctx, 200, []byte(`{"v":2}`)))

	snap, ok, err = s.Load(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, int64(200), snap.Generation)
	assert.Equal(t, []byte(`{"v":2}`), snap.Data)

	// Singleton: exactly one row regardless of writes.
	var count int
	require.NoError(t, s.pool.QueryRow(ctx, "SELECT count(*) FROM coredevs_snapshot").Scan(&count))
	assert.Equal(t, 1, count)
}
