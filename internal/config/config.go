// Package config loads and validates the coredevs service configuration: the
// canonical team registry and how each team maps onto upstream datasources.
package config

import (
	"fmt"
	"os"
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
	// Teams is the canonical team registry keyed by team slug.
	Teams map[string]Team `yaml:"teams"`
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
