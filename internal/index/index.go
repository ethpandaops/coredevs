// Package index builds and serves the superset of client developers across all
// datasources, tracking the provenance of every handle.
package index

import (
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ethpandaops/coredevs/internal/source"
)

// Index is the immutable, queryable superset of memberships for one point in
// time. Build a new Index and publish it via Store.Set rather than mutating.
type Index struct {
	// GeneratedAt is when the index was built.
	GeneratedAt time.Time `json:"generatedAt"`
	// Teams maps a team slug to its members.
	Teams map[string]*Team `json:"teams"`
}

// Team is the resolved membership of a single team.
type Team struct {
	// Slug is the canonical team key.
	Slug string `json:"slug"`
	// Members maps a lowercased handle to its provenance, for O(1) lookup.
	Members map[string]*Member `json:"members"`
}

// Member is a single developer's presence on a team and where it came from.
type Member struct {
	// Handle is the GitHub login in its first-seen display casing.
	Handle string `json:"handle"`
	// Name is a display name if any source provided one.
	Name string `json:"name,omitempty"`
	// Sources are the source names that contributed this member, sorted.
	Sources []string `json:"sources"`
	// Orgs are the GitHub orgs this member was discovered through, sorted.
	Orgs []string `json:"orgs,omitempty"`
}

// Store holds the current Index and swaps it atomically on each successful sync.
type Store struct {
	current atomic.Pointer[Index]
}

// NewStore returns an empty Store. It holds no Index until Set is called.
func NewStore() *Store {
	return &Store{}
}

// Build constructs an Index from a flat slice of memberships, computing the
// per-team superset and folding provenance (sources, orgs) together.
func Build(generatedAt time.Time, memberships []source.Membership) *Index {
	idx := &Index{
		GeneratedAt: generatedAt,
		Teams:       make(map[string]*Team, 0),
	}

	for _, m := range memberships {
		if m.Handle == "" || m.Team == "" {
			continue
		}

		team, ok := idx.Teams[m.Team]
		if !ok {
			team = &Team{Slug: m.Team, Members: make(map[string]*Member, 0)}
			idx.Teams[m.Team] = team
		}

		key := strings.ToLower(m.Handle)

		member, ok := team.Members[key]
		if !ok {
			member = &Member{Handle: m.Handle}
			team.Members[key] = member
		}

		if member.Name == "" && m.Name != "" {
			member.Name = m.Name
		}

		member.Sources = appendUnique(member.Sources, m.Source)
		if m.Org != "" {
			member.Orgs = appendUnique(member.Orgs, m.Org)
		}
	}

	idx.sortProvenance()

	return idx
}

// Get returns the current Index, or nil if none has been published yet.
func (s *Store) Get() *Index {
	return s.current.Load()
}

// Set publishes a new Index, replacing the previous one atomically.
func (s *Store) Set(idx *Index) {
	s.current.Store(idx)
}

// TeamSlugs returns the team slugs present in the index, sorted.
func (i *Index) TeamSlugs() []string {
	slugs := make([]string, 0, len(i.Teams))
	for slug := range i.Teams {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)

	return slugs
}

// Members returns the members of a team, sorted by handle and optionally
// filtered to a single source. The returned slice is a fresh copy safe to use
// without holding any lock.
func (i *Index) Members(team, sourceFilter string) []*Member {
	t, ok := i.Teams[team]
	if !ok {
		return nil
	}

	out := make([]*Member, 0, len(t.Members))
	for _, m := range t.Members {
		if sourceFilter != "" && !contains(m.Sources, sourceFilter) {
			continue
		}
		out = append(out, m)
	}

	sort.Slice(out, func(a, b int) bool {
		return strings.ToLower(out[a].Handle) < strings.ToLower(out[b].Handle)
	})

	return out
}

// Lookup returns every team a handle appears on, with the member record for
// each, keyed by team slug. Matching is case-insensitive.
func (i *Index) Lookup(handle string) map[string]*Member {
	key := strings.ToLower(handle)
	out := make(map[string]*Member, 0)

	for slug, team := range i.Teams {
		if m, ok := team.Members[key]; ok {
			out[slug] = m
		}
	}

	return out
}

func (i *Index) sortProvenance() {
	for _, team := range i.Teams {
		for _, m := range team.Members {
			sort.Strings(m.Sources)
			sort.Strings(m.Orgs)
		}
	}
}

func appendUnique(s []string, v string) []string {
	if contains(s, v) {
		return s
	}

	return append(s, v)
}

func contains(s []string, v string) bool {
	for _, e := range s {
		if e == v {
			return true
		}
	}

	return false
}
