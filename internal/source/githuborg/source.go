package githuborg

import (
	"context"
	"log/slog"
	"sort"

	"github.com/ethpandaops/coredevs/internal/source"
)

// Source resolves the configured GitHub organisations into team memberships.
type Source struct {
	logger   *slog.Logger
	client   *Client
	orgTeams map[string][]string
}

var _ source.Source = (*Source)(nil)

// NewSource constructs the org-backed membership source. orgTeams maps a GitHub
// org to the teams whose members it provides.
func NewSource(logger *slog.Logger, client *Client, orgTeams map[string][]string) *Source {
	return &Source{
		logger:   logger.With(slog.String("source", source.NameGitHubOrg)),
		client:   client,
		orgTeams: orgTeams,
	}
}

// Name returns the source identifier.
func (s *Source) Name() string {
	return source.NameGitHubOrg
}

// Fetch resolves every configured org's public members into memberships. A
// single org failure is logged and skipped so one bad org does not fail the
// whole source.
func (s *Source) Fetch(ctx context.Context) ([]source.Membership, error) {
	var memberships []source.Membership

	for _, org := range s.sortedOrgs() {
		logins, err := s.client.PublicMembers(ctx, org)
		if err != nil {
			s.logger.WarnContext(ctx, "skipping org",
				slog.String("org", org), slog.Any("error", err))

			continue
		}

		for _, login := range logins {
			for _, team := range s.orgTeams[org] {
				memberships = append(memberships, source.Membership{
					Handle: login,
					Team:   team,
					Source: source.NameGitHubOrg,
					Org:    org,
				})
			}
		}
	}

	return memberships, nil
}

func (s *Source) sortedOrgs() []string {
	orgs := make([]string, 0, len(s.orgTeams))
	for org := range s.orgTeams {
		orgs = append(orgs, org)
	}
	sort.Strings(orgs)

	return orgs
}
