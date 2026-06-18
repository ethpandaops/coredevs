// Package source defines the datasource abstraction used to discover client
// developers and the canonical membership record every source emits.
package source

import "context"

// Names of the built-in sources. These are stable identifiers exposed via the
// API (e.g. ?source=protocol-guild) and used as provenance keys.
const (
	NameProtocolGuild = "protocol-guild"
	NameGitHubOrg     = "github-org"
	NameManual        = "manual"
)

// Membership is a single (handle, team) association discovered by a source.
// One developer on one team via one source produces exactly one Membership.
type Membership struct {
	// Handle is the GitHub login, preserving its original casing. GitHub logins
	// are case-insensitive, so consumers must compare case-insensitively.
	Handle string
	// Team is the canonical team key (e.g. "teku", "geth").
	Team string
	// Source is the originating source name (one of the Name* constants).
	Source string
	// Org is the GitHub organisation the membership was derived from, when the
	// source is org-based. Empty otherwise.
	Org string
	// Name is the developer's display name when the source provides one.
	Name string
}

// Source discovers client-developer memberships from an upstream system.
//
// Implementations must be safe to call repeatedly. Fetch performs network I/O
// and must honour context cancellation.
type Source interface {
	// Name returns the stable source identifier (one of the Name* constants).
	Name() string
	// Fetch returns every membership the source can currently observe.
	Fetch(ctx context.Context) ([]Membership, error)
}
