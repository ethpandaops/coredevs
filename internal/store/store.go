// Package store persists the single canonical coredevs snapshot in Postgres so
// every pod serves identical data. The writer upserts one row; readers poll it.
// It is deliberately a single-row, last-write-wins store: there is no per-handle
// schema and no leader election — the snapshot is always one coherent blob, so
// even a transient second writer cannot produce a split-brain read.
package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// schema creates the single-row snapshot table. The CHECK keeps it to one row.
const schema = `
CREATE TABLE IF NOT EXISTS coredevs_snapshot (
	id         integer PRIMARY KEY DEFAULT 1,
	generation bigint NOT NULL,
	data       bytea NOT NULL,
	updated_at timestamptz NOT NULL DEFAULT now(),
	CONSTRAINT coredevs_snapshot_singleton CHECK (id = 1)
);`

// Store is a Postgres-backed single-snapshot store.
type Store struct {
	logger *slog.Logger
	pool   *pgxpool.Pool
}

// Snapshot is one persisted generation of the canonical state.
type Snapshot struct {
	// Generation increases with every write; readers use it to detect changes
	// without transferring the data.
	Generation int64
	// Data is the opaque serialised snapshot payload.
	Data []byte
	// UpdatedAt is when the writer last saved.
	UpdatedAt time.Time
}

// New connects to Postgres, verifies connectivity and ensures the schema.
func New(ctx context.Context, logger *slog.Logger, dsn string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse postgres dsn: %w", err)
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}

	s := &Store{
		logger: logger.With(slog.String("component", "store")),
		pool:   pool,
	}

	if err := s.ensureSchema(ctx); err != nil {
		pool.Close()

		return nil, err
	}

	return s, nil
}

// Close releases the connection pool.
func (s *Store) Close() {
	s.pool.Close()
}

// Save upserts the snapshot row with the given generation and payload.
func (s *Store) Save(ctx context.Context, generation int64, data []byte) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO coredevs_snapshot (id, generation, data, updated_at)
		VALUES (1, $1, $2, now())
		ON CONFLICT (id) DO UPDATE SET generation = $1, data = $2, updated_at = now()`,
		generation, data)
	if err != nil {
		return fmt.Errorf("save snapshot: %w", err)
	}

	return nil
}

// Generation returns the current snapshot generation, or ok=false if none has
// been written yet. It transfers no payload, so readers can cheaply poll for
// change before fetching the data.
func (s *Store) Generation(ctx context.Context) (generation int64, ok bool, err error) {
	row := s.pool.QueryRow(ctx, `SELECT generation FROM coredevs_snapshot WHERE id = 1`)
	if err := row.Scan(&generation); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, false, nil
		}

		return 0, false, fmt.Errorf("read snapshot generation: %w", err)
	}

	return generation, true, nil
}

// Load returns the current snapshot, or ok=false if none has been written yet.
func (s *Store) Load(ctx context.Context) (snap Snapshot, ok bool, err error) {
	row := s.pool.QueryRow(ctx,
		`SELECT generation, data, updated_at FROM coredevs_snapshot WHERE id = 1`)
	if err := row.Scan(&snap.Generation, &snap.Data, &snap.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Snapshot{}, false, nil
		}

		return Snapshot{}, false, fmt.Errorf("load snapshot: %w", err)
	}

	return snap, true, nil
}

func (s *Store) ensureSchema(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, schema); err != nil {
		return fmt.Errorf("ensure schema: %w", err)
	}

	return nil
}
