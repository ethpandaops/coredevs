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
	"sort"
	"strings"
	"sync"

	"github.com/ethpandaops/coredevs/internal/source"
)

// teamHeaderRe matches a working-group header row: a table row whose first cell
// is a bold team name. The document annotates the member count in two different
// places — inside the bold as "Teku (Consensys) - 8 Members", and after it as
// "**Teku** (7 contributors)" — so the row is matched on the bold cell alone and
// the count is stripped separately.
var teamHeaderRe = regexp.MustCompile(`^\|\s*\*\*([^*]+?)\*\*[^|]*\|`)

// headerCountRe matches the member-count annotation carried inside a team
// header's bold name, e.g. the "- 8 Members" of "Teku (Consensys) - 8 Members".
var headerCountRe = regexp.MustCompile(`\s*[-–—]\s*\d+\s+[Mm]embers?$`)

// memberLinkRe matches the leading markdown link of a member row and captures
// its target URL, e.g. "| [Sean Anderson](https://github.com/realbigsean/) | ...".
var memberLinkRe = regexp.MustCompile(`^\|\s*\[([^\]]+)\]\(([^)]*)`)

// githubUserRe extracts a bare GitHub login from a profile URL. It deliberately
// rejects org/repo paths (which contain a second segment) so only user profiles
// match.
var githubUserRe = regexp.MustCompile(`github\.com/([A-Za-z0-9-]+)/?$`)

// Source parses the Protocol Guild membership markdown.
type Source struct {
	logger     *slog.Logger
	client     *http.Client
	url        string
	sections   map[string]string
	maxMissing int

	mu      sync.RWMutex
	missing []string
}

var _ source.Source = (*Source)(nil)

// New constructs a Protocol Guild source. sections maps Protocol Guild section
// headers (as they appear in the document) to canonical team slugs. maxMissing
// is how many of those headers may vanish from the document before a fetch is
// treated as a failure; 0 tolerates any number.
func New(logger *slog.Logger, client *http.Client, url string, sections map[string]string, maxMissing int) *Source {
	return &Source{
		logger:     logger.With(slog.String("source", source.NameProtocolGuild)),
		client:     client,
		url:        url,
		sections:   sections,
		maxMissing: maxMissing,
	}
}

// Warnings returns the configured section headers that were absent from the
// most recent document, empty when every one of them was found. A header goes
// missing when the document renames or drops it, which silently strands every
// member underneath it, so it is reported even when the fetch itself succeeded.
func (s *Source) Warnings() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return append([]string(nil), s.missing...)
}

// setMissing records the configured headers absent from a parse.
func (s *Source) setMissing(missing []string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.missing = missing
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

	memberships, seen := s.parse(ctx, string(body))

	missing := s.missingSections(seen)
	s.setMissing(missing)

	if len(missing) > 0 {
		s.logger.ErrorContext(ctx, "configured sections missing from document",
			slog.Int("missing", len(missing)),
			slog.Int("configured", len(s.sections)),
			slog.Any("sections", missing),
		)

		if s.maxMissing > 0 && len(missing) > s.maxMissing {
			return nil, fmt.Errorf(
				"%d of %d configured sections missing from document (budget %d): %s",
				len(missing), len(s.sections), s.maxMissing, strings.Join(missing, ", "),
			)
		}
	}

	return memberships, nil
}

// missingSections returns the configured headers that the document no longer
// carries, sorted for a stable error message.
func (s *Source) missingSections(seen map[string]bool) []string {
	var missing []string

	for section := range s.sections {
		if !seen[section] {
			missing = append(missing, section)
		}
	}

	sort.Strings(missing)

	return missing
}

// parse walks the markdown line by line. A team-header row sets the current
// team (when its section is mapped); subsequent member rows are attributed to
// it until the next header. Rows under unmapped sections are skipped, and a
// markdown heading clears the current team so rows that follow a table are
// never attributed to the team it ended on.
//
// seen collects every section header the document carried, mapped or not, so
// the caller can tell a header that vanished from one that simply has no
// members.
func (s *Source) parse(ctx context.Context, doc string) (memberships []source.Membership, seen map[string]bool) {
	seen = make(map[string]bool)

	currentTeam := ""

	for _, line := range strings.Split(doc, "\n") {
		line = strings.TrimSpace(line)

		if strings.HasPrefix(line, "#") {
			currentTeam = ""

			continue
		}

		if m := teamHeaderRe.FindStringSubmatch(line); m != nil {
			section := sectionName(m[1])
			seen[section] = true
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
		slog.Int("sections", len(seen)),
	)

	return memberships, seen
}

// sectionName normalises a team header's bold cell into the section key used by
// the team registry, dropping the member-count annotation the document appends
// to it: "Teku (Consensys) - 8 Members" becomes "Teku (Consensys)".
func sectionName(cell string) string {
	return strings.TrimSpace(headerCountRe.ReplaceAllString(strings.TrimSpace(cell), ""))
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
