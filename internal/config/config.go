// Package config loads and validates the coredevs service configuration: the
// canonical team registry and how each team maps onto upstream datasources.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level service configuration.
type Config struct {
	// HTTP configures the API server.
	HTTP HTTP `yaml:"http"`
	// SyncInterval is how often sources are refreshed.
	SyncInterval time.Duration `yaml:"syncInterval"`
	// SnapshotPath is where the last-good index is persisted between restarts.
	// Empty disables snapshotting.
	SnapshotPath string `yaml:"snapshotPath"`
	// Sources configures the upstream datasources.
	Sources Sources `yaml:"sources"`
	// Keys configures the GitHub SSH public-key cache.
	Keys Keys `yaml:"keys"`
	// Teams is the canonical team registry keyed by team slug.
	Teams map[string]Team `yaml:"teams"`
	// Exclude lists GitHub handles to drop from every team, regardless of source.
	// Used to suppress stale handles an upstream still lists — e.g. a renamed or
	// deleted GitHub account whose key fetches would otherwise 404 for consumers.
	Exclude []string `yaml:"exclude"`
}

// HTTP configures the API server.
type HTTP struct {
	// Addr is the listen address (e.g. ":8080").
	Addr string `yaml:"addr"`
}

// Sources configures the upstream datasources.
type Sources struct {
	// ProtocolGuild configures the Protocol Guild membership source.
	ProtocolGuild ProtocolGuild `yaml:"protocolGuild"`
	// GitHubOrg configures the GitHub public-org-membership source.
	GitHubOrg GitHubOrg `yaml:"githubOrg"`
}

// ProtocolGuild configures the Protocol Guild membership source.
type ProtocolGuild struct {
	// Enabled toggles the source.
	Enabled bool `yaml:"enabled"`
	// URL is the raw membership markdown document.
	URL string `yaml:"url"`
	// MinMembers is a sanity floor: if a fetch parses fewer than this many
	// members, it is treated as a soft failure and the last good result is kept,
	// guarding against an upstream format change silently gutting the index. 0
	// disables the floor.
	MinMembers int `yaml:"minMembers"`
}

// GitHubOrg configures the GitHub public-org-membership source.
type GitHubOrg struct {
	// Enabled toggles the source.
	Enabled bool `yaml:"enabled"`
	// BaseURL is the GitHub API base (override for GitHub Enterprise/testing).
	BaseURL string `yaml:"baseURL"`
	// TokenEnv is the environment variable holding a GitHub token. A token is
	// optional for public data but lifts the unauthenticated rate limit.
	TokenEnv string `yaml:"tokenEnv"`
	// MinMembers is a sanity floor; see ProtocolGuild.MinMembers. 0 disables it.
	MinMembers int `yaml:"minMembers"`
}

// Keys configures the GitHub SSH public-key cache: a rate-paced proxy that
// fetches each indexed developer's keys in the background so consumers read a
// team's authorized_keys from coredevs without ever calling GitHub themselves.
type Keys struct {
	// Enabled toggles the key cache and its endpoints.
	Enabled bool `yaml:"enabled"`
	// BaseURL is the host serving the `.keys` endpoint (default
	// https://github.com). The cache requests "{baseURL}/{handle}.keys".
	BaseURL string `yaml:"baseURL"`
	// RefreshInterval is the target staleness: the cache paces itself so every
	// handle is refreshed roughly once per this window. A full pass is spread
	// evenly across the window rather than fetched in a burst.
	RefreshInterval time.Duration `yaml:"refreshInterval"`
	// MaxRequestsPerSecond is a hard ceiling on the upstream request rate. It
	// guards GitHub when the handle set is small enough that the even-spread pace
	// would otherwise fetch faster than this.
	MaxRequestsPerSecond float64 `yaml:"maxRequestsPerSecond"`
	// CacheDir is a directory holding one file per handle, persisting each
	// developer's keys as soon as they are fetched so partial progress survives a
	// restart. Empty disables persistence.
	CacheDir string `yaml:"cacheDir"`
}

// Team describes a canonical team and how it maps onto upstream sources.
type Team struct {
	// DisplayName is a human-readable label.
	DisplayName string `yaml:"displayName"`
	// Kind groups teams by role: "client", "research", "coordination",
	// "delivery", or "" if unclassified.
	Kind string `yaml:"kind"`
	// Layer is the client layer: "cl", "el", or "" for non-client teams.
	Layer string `yaml:"layer"`
	// ProtocolGuildSections are the Protocol Guild working-group headers that
	// map onto this team (e.g. ["Teku"]).
	ProtocolGuildSections []string `yaml:"protocolGuildSections"`
	// GitHubOrgs are GitHub organisations whose public members belong to this
	// team. Curated deliberately — broad umbrella orgs are left unset.
	GitHubOrgs []string `yaml:"githubOrgs"`
	// Members are manually-defined GitHub handles for this team, for people not
	// yet reflected in an upstream source (e.g. a new joiner awaiting Protocol
	// Guild listing). They are always included in the superset.
	Members []string `yaml:"members"`
}

// Load reads and validates configuration from a YAML file, applying defaults.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}

	cfg := Default()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid config %q: %w", path, err)
	}

	return cfg, nil
}

// Default returns a Config populated with sensible defaults. Fields set in the
// YAML document override these.
func Default() *Config {
	return &Config{
		HTTP:         HTTP{Addr: ":8080"},
		SyncInterval: 3 * time.Hour,
		SnapshotPath: "",
		Sources: Sources{
			ProtocolGuild: ProtocolGuild{
				Enabled: true,
				URL:     "https://raw.githubusercontent.com/protocolguild/documentation/main/docs/01-membership.md",
			},
			GitHubOrg: GitHubOrg{
				Enabled:  true,
				BaseURL:  "https://api.github.com",
				TokenEnv: "GITHUB_TOKEN",
			},
		},
		Keys: Keys{
			Enabled:              true,
			BaseURL:              "https://github.com",
			RefreshInterval:      3 * time.Hour,
			MaxRequestsPerSecond: 5,
		},
		Teams: make(map[string]Team, 0),
	}
}

// OrgTeams inverts the team registry into a map of GitHub org to the teams that
// org contributes to. Org keys are returned in their configured casing.
func (c *Config) OrgTeams() map[string][]string {
	out := make(map[string][]string, len(c.Teams))
	for slug, team := range c.Teams {
		for _, org := range team.GitHubOrgs {
			out[org] = append(out[org], slug)
		}
	}

	return out
}

// ManualMembers returns a map of team slug to its manually-defined handles,
// including only teams that have any.
func (c *Config) ManualMembers() map[string][]string {
	out := make(map[string][]string, len(c.Teams))
	for slug, team := range c.Teams {
		if len(team.Members) > 0 {
			out[slug] = team.Members
		}
	}

	return out
}

// ExcludedHandles returns the configured Exclude list as a set of lowercased
// handles, or nil if none are configured.
func (c *Config) ExcludedHandles() map[string]bool {
	if len(c.Exclude) == 0 {
		return nil
	}

	out := make(map[string]bool, len(c.Exclude))
	for _, h := range c.Exclude {
		out[strings.ToLower(h)] = true
	}

	return out
}

// SectionTeams inverts the team registry into a map of Protocol Guild section
// header to the team it maps onto.
func (c *Config) SectionTeams() map[string]string {
	out := make(map[string]string, len(c.Teams))
	for slug, team := range c.Teams {
		for _, section := range team.ProtocolGuildSections {
			out[section] = slug
		}
	}

	return out
}

func (c *Config) validate() error {
	if c.SyncInterval <= 0 {
		return fmt.Errorf("syncInterval must be positive, got %s", c.SyncInterval)
	}

	if c.HTTP.Addr == "" {
		return fmt.Errorf("http.addr must be set")
	}

	if len(c.Teams) == 0 {
		return fmt.Errorf("at least one team must be configured")
	}

	if c.Keys.Enabled {
		if c.Keys.RefreshInterval <= 0 {
			return fmt.Errorf("keys.refreshInterval must be positive, got %s", c.Keys.RefreshInterval)
		}
		if c.Keys.MaxRequestsPerSecond <= 0 {
			return fmt.Errorf("keys.maxRequestsPerSecond must be positive, got %g", c.Keys.MaxRequestsPerSecond)
		}
	}

	for slug, team := range c.Teams {
		if team.Layer != "" && team.Layer != "cl" && team.Layer != "el" {
			return fmt.Errorf("team %q: layer must be one of cl, el, or empty, got %q", slug, team.Layer)
		}

		switch team.Kind {
		case "", "client", "research", "coordination", "delivery":
		default:
			return fmt.Errorf("team %q: kind must be one of client, research, coordination, delivery, or empty, got %q", slug, team.Kind)
		}
	}

	return nil
}
