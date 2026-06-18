// Package manual provides a static, config-defined membership source for
// handles that should be on a team before they appear in an upstream source —
// for example a developer who has joined a client team but is not yet listed in
// Protocol Guild.
package manual

import (
	"context"
	"sort"

	"github.com/ethpandaops/coredevs/internal/source"
)

// Source emits memberships from a static team-to-handles map.
type Source struct {
	teamHandles map[string][]string
}

var _ source.Source = (*Source)(nil)

// New constructs a manual source from a map of team slug to manually-defined
// handles.
func New(teamHandles map[string][]string) *Source {
	return &Source{teamHandles: teamHandles}
}

// Name returns the source identifier.
func (s *Source) Name() string {
	return source.NameManual
}

// Fetch returns the configured manual memberships. It performs no I/O and never
// fails, so manual entries are always present in the superset.
func (s *Source) Fetch(_ context.Context) ([]source.Membership, error) {
	var memberships []source.Membership

	for _, team := range s.sortedTeams() {
		for _, handle := range s.teamHandles[team] {
			if handle == "" {
				continue
			}

			memberships = append(memberships, source.Membership{
				Handle: handle,
				Team:   team,
				Source: source.NameManual,
			})
		}
	}

	return memberships, nil
}

func (s *Source) sortedTeams() []string {
	teams := make([]string, 0, len(s.teamHandles))
	for team := range s.teamHandles {
		teams = append(teams, team)
	}
	sort.Strings(teams)

	return teams
}
