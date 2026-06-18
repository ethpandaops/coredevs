// Package protocolguild discovers client developers from the Protocol Guild
// membership document, mapping its working-group sections onto canonical teams.
package protocolguild

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"github.com/ethpandaops/coredevs/internal/source"
)

// teamHeaderRe matches a working-group header row whose first cell is a bold
// name followed by a "(N contributors)" annotation, e.g.
// "| **Teku** (7 contributors) | **7** | ... |".
var teamHeaderRe = regexp.MustCompile(`^\|\s*\*\*([^*]+?)\*\*\s*\((\d+)\s+contributors?\)`)

// memberLinkRe matches the leading markdown link of a member row and captures
// its target URL, e.g. "| [Sean Anderson](https://github.com/realbigsean/) | ...".
var memberLinkRe = regexp.MustCompile(`^\|\s*\[([^\]]+)\]\(([^)]*)`)

// githubUserRe extracts a bare GitHub login from a profile URL. It deliberately
// rejects org/repo paths (which contain a second segment) so only user profiles
// match.
var githubUserRe = regexp.MustCompile(`github\.com/([A-Za-z0-9-]+)/?$`)

// Source parses the Protocol Guild membership markdown.
type Source struct {
	logger   *slog.Logger
	client   *http.Client
	url      string
	sections map[string]string
}

var _ source.Source = (*Source)(nil)

// New constructs a Protocol Guild source. sections maps Protocol Guild section
// headers (as they appear in the document) to canonical team slugs.
func New(logger *slog.Logger, client *http.Client, url string, sections map[string]string) *Source {
	return &Source{
		logger:   logger.With(slog.String("source", source.NameProtocolGuild)),
		client:   client,
		url:      url,
		sections: sections,
	}
}

// Name returns the source identifier.
func (s *Source) Name() string {
	return source.NameProtocolGuild
}

// Fetch downloads and parses the membership document.
func (s *Source) Fetch(ctx context.Context) ([]source.Membership, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch membership document: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch membership document: unexpected status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read membership document: %w", err)
	}

	return s.parse(ctx, string(body)), nil
}

// parse walks the markdown line by line. A team-header row sets the current
// team (when its section is mapped); subsequent member rows are attributed to
// it until the next header. Rows under unmapped sections are skipped.
func (s *Source) parse(ctx context.Context, doc string) []source.Membership {
	var memberships []source.Membership

	currentTeam := ""

	for _, line := range strings.Split(doc, "\n") {
		line = strings.TrimSpace(line)

		if m := teamHeaderRe.FindStringSubmatch(line); m != nil {
			section := strings.TrimSpace(m[1])
			currentTeam = s.sections[section]
			if currentTeam == "" {
				s.logger.DebugContext(ctx, "skipping unmapped section", slog.String("section", section))
			}

			continue
		}

		if currentTeam == "" {
			continue
		}

		name, handle, ok := parseMemberRow(line)
		if !ok {
			continue
		}

		memberships = append(memberships, source.Membership{
			Handle: handle,
			Team:   currentTeam,
			Source: source.NameProtocolGuild,
			Name:   name,
		})
	}

	s.logger.DebugContext(ctx, "parsed membership document",
		slog.Int("memberships", len(memberships)),
	)

	return memberships
}

// parseMemberRow extracts the display name and GitHub handle from a member row.
// It returns ok=false for header/divider rows and rows whose first link is not
// a GitHub user profile (e.g. personal websites).
func parseMemberRow(line string) (name, handle string, ok bool) {
	m := memberLinkRe.FindStringSubmatch(line)
	if m == nil {
		return "", "", false
	}

	name = strings.TrimSpace(m[1])
	url := m[2]

	// Some rows carry a malformed nested link such as
	// "[joshieDo]([https://github.com/x](http://github.com/joshieDo))"; the last
	// github.com/<user> occurrence is the correct profile.
	matches := githubUserRe.FindAllStringSubmatch(url, -1)
	if len(matches) == 0 {
		return "", "", false
	}

	handle = matches[len(matches)-1][1]

	return name, handle, true
}
