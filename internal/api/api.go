// Package api serves the coredevs HTTP API: the per-team superset of client
// developers and supporting lookup endpoints.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"gopkg.in/yaml.v3"

	"github.com/ethpandaops/coredevs/internal/config"
	"github.com/ethpandaops/coredevs/internal/index"
	"github.com/ethpandaops/coredevs/internal/source"
	"github.com/ethpandaops/coredevs/internal/syncer"
)

// errUnknownSource is returned when the ?source filter is not a recognised
// source name.
var errUnknownSource = errors.New("source must be one of protocol-guild, github-org, manual")

// OrgResolver returns the public members of a GitHub organisation on demand.
type OrgResolver interface {
	PublicMembers(ctx context.Context, org string) ([]string, error)
}

// Handler implements the coredevs HTTP API.
type Handler struct {
	logger *slog.Logger
	cfg    *config.Config
	store  *index.Store
	syncer *syncer.Syncer
	orgs   OrgResolver
	mux    *http.ServeMux
}

var _ http.Handler = (*Handler)(nil)

// New constructs the API handler and registers all routes.
func New(logger *slog.Logger, cfg *config.Config, store *index.Store, sync *syncer.Syncer, orgs OrgResolver) *Handler {
	h := &Handler{
		logger: logger.With(slog.String("component", "api")),
		cfg:    cfg,
		store:  store,
		syncer: sync,
		orgs:   orgs,
		mux:    http.NewServeMux(),
	}

	registry := prometheus.NewRegistry()
	registry.MustRegister(newCollector(store, sync))

	h.mux.HandleFunc("GET /{$}", h.handleIndex)
	h.mux.HandleFunc("GET /team/{slug}", h.handleIndex)
	h.mux.HandleFunc("GET /healthz", h.handleHealthz)
	h.mux.HandleFunc("GET /readyz", h.handleReadyz)
	h.mux.Handle("GET /metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	h.mux.HandleFunc("GET /api/v1/teams", h.handleTeams)
	h.mux.HandleFunc("GET /api/v1/users", h.handleAllUsers)
	h.mux.HandleFunc("GET /api/v1/users/{team}", h.handleUsers)
	h.mux.HandleFunc("GET /api/v1/handles/{handle}", h.handleHandle)
	h.mux.HandleFunc("GET /api/v1/orgs/{org}/members", h.handleOrgMembers)
	h.mux.HandleFunc("GET /api/v1/sources", h.handleSources)
	h.mux.HandleFunc("GET /api/v1/export", h.handleExport)

	return h
}

// ServeHTTP dispatches to the registered routes.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeText(w, http.StatusOK, "ok\n")
}

func (h *Handler) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	if h.store.Get() == nil {
		writeText(w, http.StatusServiceUnavailable, "no index\n")

		return
	}

	writeText(w, http.StatusOK, "ok\n")
}

func (h *Handler) handleTeams(w http.ResponseWriter, r *http.Request) {
	idx := h.store.Get()
	if idx == nil {
		h.writeError(w, r, http.StatusServiceUnavailable, "index not ready")

		return
	}

	type teamSummary struct {
		Slug        string         `json:"slug"`
		DisplayName string         `json:"displayName,omitempty"`
		Kind        string         `json:"kind,omitempty"`
		Layer       string         `json:"layer,omitempty"`
		Count       int            `json:"count"`
		Counts      map[string]int `json:"counts"`
	}

	kindFilter := r.URL.Query().Get("kind")

	summaries := make([]teamSummary, 0, len(h.cfg.Teams))
	for slug, team := range h.cfg.Teams {
		if kindFilter != "" && team.Kind != kindFilter {
			continue
		}
		members := idx.Members(slug, "")
		summaries = append(summaries, teamSummary{
			Slug:        slug,
			DisplayName: team.DisplayName,
			Kind:        team.Kind,
			Layer:       team.Layer,
			Count:       len(members),
			Counts:      countBySource(members),
		})
	}

	sort.Slice(summaries, func(a, b int) bool { return summaries[a].Slug < summaries[b].Slug })

	h.writeJSON(w, r, http.StatusOK, map[string]any{
		"generatedAt": idx.GeneratedAt,
		"teams":       summaries,
	})
}

func (h *Handler) handleUsers(w http.ResponseWriter, r *http.Request) {
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

	sourceFilter, err := normalizeSource(r.URL.Query().Get("source"))
	if err != nil {
		h.writeError(w, r, http.StatusBadRequest, err.Error())

		return
	}

	members := idx.Members(team, sourceFilter)
	handles := handlesOf(members)

	switch r.URL.Query().Get("format") {
	case "", "json":
		h.writeJSON(w, r, http.StatusOK, map[string]any{
			"team":        team,
			"generatedAt": idx.GeneratedAt,
			"source":      sourceOrAll(sourceFilter),
			"count":       len(handles),
			"handles":     handles,
			"members":     members,
		})
	case "txt":
		writeText(w, http.StatusOK, strings.Join(handles, "\n")+"\n")
	case "yaml":
		h.writeYAML(w, r, handles)
	default:
		h.writeError(w, r, http.StatusBadRequest, "format must be one of json, txt, yaml")
	}
}

func (h *Handler) handleAllUsers(w http.ResponseWriter, r *http.Request) {
	idx := h.store.Get()
	if idx == nil {
		h.writeError(w, r, http.StatusServiceUnavailable, "index not ready")

		return
	}

	sourceFilter, err := normalizeSource(r.URL.Query().Get("source"))
	if err != nil {
		h.writeError(w, r, http.StatusBadRequest, err.Error())

		return
	}

	kindFilter := r.URL.Query().Get("kind")

	users := make([]*index.User, 0)
	for _, u := range idx.Users() {
		if sourceFilter != "" && !contains(u.Sources, sourceFilter) {
			continue
		}
		if kindFilter != "" && !h.userInKind(u, kindFilter) {
			continue
		}
		users = append(users, u)
	}

	handles := make([]string, 0, len(users))
	for _, u := range users {
		handles = append(handles, u.Handle)
	}

	switch r.URL.Query().Get("format") {
	case "", "json":
		h.writeJSON(w, r, http.StatusOK, map[string]any{
			"generatedAt": idx.GeneratedAt,
			"source":      sourceOrAll(sourceFilter),
			"count":       len(handles),
			"handles":     handles,
			"users":       users,
		})
	case "txt":
		writeText(w, http.StatusOK, strings.Join(handles, "\n")+"\n")
	case "yaml":
		h.writeYAML(w, r, handles)
	default:
		h.writeError(w, r, http.StatusBadRequest, "format must be one of json, txt, yaml")
	}
}

// userInKind reports whether a user belongs to any team of the given kind.
func (h *Handler) userInKind(u *index.User, kind string) bool {
	for _, slug := range u.Teams {
		if t, ok := h.cfg.Teams[slug]; ok && t.Kind == kind {
			return true
		}
	}

	return false
}

func (h *Handler) handleHandle(w http.ResponseWriter, r *http.Request) {
	idx := h.store.Get()
	if idx == nil {
		h.writeError(w, r, http.StatusServiceUnavailable, "index not ready")

		return
	}

	handle := r.PathValue("handle")
	memberships := idx.Lookup(handle)

	type teamMembership struct {
		Team    string   `json:"team"`
		Sources []string `json:"sources"`
		Orgs    []string `json:"orgs,omitempty"`
	}

	teams := make([]teamMembership, 0, len(memberships))
	display := handle
	for slug, m := range memberships {
		display = m.Handle
		teams = append(teams, teamMembership{Team: slug, Sources: m.Sources, Orgs: m.Orgs})
	}

	sort.Slice(teams, func(a, b int) bool { return teams[a].Team < teams[b].Team })

	h.writeJSON(w, r, http.StatusOK, map[string]any{
		"handle": display,
		"teams":  teams,
	})
}

func (h *Handler) handleOrgMembers(w http.ResponseWriter, r *http.Request) {
	if h.orgs == nil {
		h.writeError(w, r, http.StatusServiceUnavailable, "github org source disabled")

		return
	}

	org := r.PathValue("org")

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	members, err := h.orgs.PublicMembers(ctx, org)
	if err != nil {
		h.logger.WarnContext(ctx, "org members lookup failed",
			slog.String("org", org), slog.Any("error", err))
		h.writeError(w, r, http.StatusBadGateway, err.Error())

		return
	}

	sort.Slice(members, func(a, b int) bool {
		return strings.ToLower(members[a]) < strings.ToLower(members[b])
	})

	if r.URL.Query().Get("format") == "txt" {
		writeText(w, http.StatusOK, strings.Join(members, "\n")+"\n")

		return
	}

	h.writeJSON(w, r, http.StatusOK, map[string]any{
		"org":     org,
		"count":   len(members),
		"members": members,
	})
}

func (h *Handler) handleSources(w http.ResponseWriter, r *http.Request) {
	h.writeJSON(w, r, http.StatusOK, map[string]any{
		"sources": h.syncer.Statuses(),
	})
}

func (h *Handler) handleExport(w http.ResponseWriter, r *http.Request) {
	idx := h.store.Get()
	if idx == nil {
		h.writeError(w, r, http.StatusServiceUnavailable, "index not ready")

		return
	}

	h.writeJSON(w, r, http.StatusOK, idx)
}

func (h *Handler) writeJSON(w http.ResponseWriter, r *http.Request, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(body); err != nil {
		h.logger.WarnContext(r.Context(), "failed to encode response", slog.Any("error", err))
	}
}

func (h *Handler) writeYAML(w http.ResponseWriter, r *http.Request, body any) {
	w.Header().Set("Content-Type", "application/yaml")
	w.WriteHeader(http.StatusOK)

	if err := yaml.NewEncoder(w).Encode(body); err != nil {
		h.logger.WarnContext(r.Context(), "failed to encode yaml response", slog.Any("error", err))
	}
}

func (h *Handler) writeError(w http.ResponseWriter, r *http.Request, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(map[string]string{"error": msg}); err != nil {
		h.logger.WarnContext(r.Context(), "failed to encode error response", slog.Any("error", err))
	}
}

func writeText(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// normalizeSource validates and canonicalises the ?source filter, accepting
// both the public names and their short forms.
func normalizeSource(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return "", nil
	case source.NameProtocolGuild, "pg", "protocolguild":
		return source.NameProtocolGuild, nil
	case source.NameGitHubOrg, "github", "githuborg", "org":
		return source.NameGitHubOrg, nil
	case source.NameManual, "static":
		return source.NameManual, nil
	default:
		return "", errUnknownSource
	}
}

func sourceOrAll(s string) string {
	if s == "" {
		return "all"
	}

	return s
}

func countBySource(members []*index.Member) map[string]int {
	counts := make(map[string]int, 2)
	for _, m := range members {
		for _, src := range m.Sources {
			counts[src]++
		}
	}

	return counts
}

func contains(s []string, v string) bool {
	for _, e := range s {
		if e == v {
			return true
		}
	}

	return false
}

func handlesOf(members []*index.Member) []string {
	handles := make([]string, 0, len(members))
	for _, m := range members {
		handles = append(handles, m.Handle)
	}

	return handles
}
