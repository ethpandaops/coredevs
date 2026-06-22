// Package snapshot is the serialised canonical state the writer publishes to
// Postgres and every reader loads: the built index, the cached keys, and the
// per-source sync status, so all pods serve byte-identical data.
package snapshot

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/ethpandaops/coredevs/internal/index"
	"github.com/ethpandaops/coredevs/internal/keys"
	"github.com/ethpandaops/coredevs/internal/syncer"
)

// Snapshot is one coherent generation of everything coredevs serves.
type Snapshot struct {
	// Generation increases with every write the writer publishes.
	Generation int64 `json:"generation"`
	// GeneratedAt is when the writer built this snapshot.
	GeneratedAt time.Time `json:"generatedAt"`
	// Index is the built superset of teams and members.
	Index *index.Index `json:"index"`
	// Keys maps a lowercased handle to its cached SSH keys.
	Keys map[string]keys.Entry `json:"keys"`
	// Sources is the per-source sync status reported by the writer.
	Sources []syncer.SourceStatus `json:"sources"`
}

// Marshal serialises a snapshot to the bytes persisted in the store.
func Marshal(s *Snapshot) ([]byte, error) {
	data, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("marshal snapshot: %w", err)
	}

	return data, nil
}

// Unmarshal parses a snapshot from stored bytes.
func Unmarshal(data []byte) (*Snapshot, error) {
	var s Snapshot
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("unmarshal snapshot: %w", err)
	}

	return &s, nil
}
