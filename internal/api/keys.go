package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/ethpandaops/coredevs/internal/keys"
)

// onDemandKeyTimeout bounds a single cold-miss fetch for a handle's keys.
const onDemandKeyTimeout = 15 * time.Second

// handleHandleKeys serves a single developer's cached SSH public keys. On a
// cold miss it performs one bounded on-demand fetch so a first request is not
// empty. format=txt returns the raw authorized_keys fragment.
func (h *Handler) handleHandleKeys(w http.ResponseWriter, r *http.Request) {
	if h.keys == nil {
		h.writeError(w, r, http.StatusServiceUnavailable, "key cache disabled")

		return
	}

	handle := r.PathValue("handle")

	entry, ok := h.keys.Get(handle)
	if !ok {
		// Only fetch on demand for handles we actually index, so the endpoint
		// can't be used as an open GitHub-key proxy for arbitrary logins.
		idx := h.store.Get()
		if idx == nil || len(idx.Lookup(handle)) == 0 {
			h.writeError(w, r, http.StatusNotFound, "unknown handle")

			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), onDemandKeyTimeout)
		defer cancel()

		fetched, err := h.keys.Refresh(ctx, handle)
		if err != nil {
			h.logger.WarnContext(ctx, "on-demand key fetch failed",
				slog.String("handle", handle), slog.Any("error", err))
			h.writeError(w, r, http.StatusBadGateway, err.Error())

			return
		}
		entry = fetched
	}

	if r.URL.Query().Get("format") == "txt" {
		writeText(w, http.StatusOK, strings.Join(entry.Keys, "\n")+"\n")

		return
	}

	h.writeJSON(w, r, http.StatusOK, map[string]any{
		"handle":    entry.Handle,
		"source":    keys.Source,
		"count":     len(entry.Keys),
		"keys":      entry.Keys,
		"fetchedAt": entry.FetchedAt,
	})
}

// handleTeamKeys serves the assembled authorized_keys for an entire team from
// cache. It never fetches on demand — a team can have hundreds of members, and
// the background walker keeps them warm. format=txt (the default) returns a
// ready-to-use authorized_keys file; format=json returns per-handle detail
// including which handles are still pending a first fetch.
func (h *Handler) handleTeamKeys(w http.ResponseWriter, r *http.Request) {
	if h.keys == nil {
		h.writeError(w, r, http.StatusServiceUnavailable, "key cache disabled")

		return
	}

	idx := h.store.Get()
	if idx == nil {
		h.writeError(w, r, http.StatusServiceUnavailable, "index not ready")

		return
	}

	team := r.PathValue("team")
	if _, ok := h.cfg.Teams[team]; !ok {
		h.writeError(w, r, http.StatusNotFound, "unknown team")

		return
	}

	type handleKeys struct {
		Handle    string    `json:"handle"`
		Name      string    `json:"name,omitempty"`
		Keys      []string  `json:"keys"`
		FetchedAt time.Time `json:"fetchedAt,omitzero"`
		Pending   bool      `json:"pending,omitempty"`
	}

	members := idx.Members(team, "")
	entries := make([]handleKeys, 0, len(members))

	var totalKeys, pending int
	for _, m := range members {
		hk := handleKeys{Handle: m.Handle, Name: m.Name, Keys: []string{}}

		entry, ok := h.keys.Get(m.Handle)
		if ok {
			hk.Keys = entry.Keys
			hk.FetchedAt = entry.FetchedAt
		}
		if !ok || entry.FetchedAt.IsZero() {
			hk.Pending = true
			pending++
		}

		totalKeys += len(hk.Keys)
		entries = append(entries, hk)
	}

	switch r.URL.Query().Get("format") {
	case "", "txt":
		var b strings.Builder
		for _, e := range entries {
			if len(e.Keys) == 0 {
				continue
			}
			fmt.Fprintf(&b, "# %s\n", e.Handle)
			for _, k := range e.Keys {
				b.WriteString(k)
				b.WriteByte('\n')
			}
		}
		writeText(w, http.StatusOK, b.String())
	case "json":
		h.writeJSON(w, r, http.StatusOK, map[string]any{
			"team":        team,
			"generatedAt": idx.GeneratedAt,
			"source":      keys.Source,
			"handles":     len(entries),
			"pending":     pending,
			"keyCount":    totalKeys,
			"members":     entries,
		})
	default:
		h.writeError(w, r, http.StatusBadRequest, "format must be one of txt, json")
	}
}
