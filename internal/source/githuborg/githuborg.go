// Package githuborg discovers client developers from the public membership of
// GitHub organisations.
package githuborg

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/ethpandaops/coredevs/internal/source"
)

const pageSize = 100

// Client fetches the public members of GitHub organisations. It is also used
// directly by the API to resolve arbitrary orgs on demand.
type Client struct {
	logger  *slog.Logger
	http    *http.Client
	baseURL string
	token   string
}

// publicMember is the subset of the GitHub user object we consume.
type publicMember struct {
	Login string `json:"login"`
}

// NewClient constructs a GitHub org client. An empty token is allowed but
// subjects requests to the lower unauthenticated rate limit.
func NewClient(logger *slog.Logger, httpClient *http.Client, baseURL, token string) *Client {
	return &Client{
		logger:  logger.With(slog.String("source", source.NameGitHubOrg)),
		http:    httpClient,
		baseURL: baseURL,
		token:   token,
	}
}

// PublicMembers returns the logins of an organisation's public members,
// following pagination until exhausted.
func (c *Client) PublicMembers(ctx context.Context, org string) ([]string, error) {
	var logins []string

	for page := 1; ; page++ {
		batch, err := c.fetchPage(ctx, org, page)
		if err != nil {
			return nil, err
		}

		for _, m := range batch {
			if m.Login != "" {
				logins = append(logins, m.Login)
			}
		}

		if len(batch) < pageSize {
			break
		}
	}

	return logins, nil
}

func (c *Client) fetchPage(ctx context.Context, org string, page int) ([]publicMember, error) {
	endpoint := fmt.Sprintf("%s/orgs/%s/public_members?per_page=%d&page=%d",
		c.baseURL, url.PathEscape(org), pageSize, page)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch org %q members: %w", org, err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		// continue below
	case http.StatusNotFound:
		return nil, fmt.Errorf("org %q not found", org)
	case http.StatusForbidden, http.StatusTooManyRequests:
		return nil, fmt.Errorf("org %q rate limited (status %d, %s)",
			org, resp.StatusCode, rateLimitReset(resp))
	default:
		return nil, fmt.Errorf("fetch org %q members: unexpected status %d", org, resp.StatusCode)
	}

	var members []publicMember
	if err := json.NewDecoder(resp.Body).Decode(&members); err != nil {
		return nil, fmt.Errorf("decode org %q members: %w", org, err)
	}

	return members, nil
}

// rateLimitReset renders a human-readable reset hint from response headers.
func rateLimitReset(resp *http.Response) string {
	reset := resp.Header.Get("X-RateLimit-Reset")
	if reset == "" {
		return "reset unknown"
	}

	secs, err := strconv.ParseInt(reset, 10, 64)
	if err != nil {
		return "reset unknown"
	}

	return "resets " + time.Unix(secs, 0).UTC().Format(time.RFC3339)
}
