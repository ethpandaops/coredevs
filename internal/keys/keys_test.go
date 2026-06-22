package keys

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testCache(t *testing.T, cfg Config, handlesFn HandlesFunc) *Cache {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	return New(logger, &http.Client{Timeout: 5 * time.Second}, cfg, handlesFn)
}

func TestParseKeys(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []string
	}{
		{name: "empty", body: "", want: nil},
		{name: "blank lines only", body: "\n\n  \n", want: nil},
		{
			name: "two keys sorted",
			body: "ssh-ed25519 BBB bob\nssh-rsa AAA alice\n",
			want: []string{"ssh-ed25519 BBB bob", "ssh-rsa AAA alice"},
		},
		{
			name: "trims and skips blanks",
			body: "  ssh-ed25519 AAA  \n\nssh-rsa BBB\n",
			want: []string{"ssh-ed25519 AAA", "ssh-rsa BBB"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseKeys(strings.NewReader(tt.body))
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestPaceFor(t *testing.T) {
	tests := []struct {
		name     string
		interval time.Duration
		maxRPS   float64
		n        int
		want     time.Duration
	}{
		{
			name:     "even spread within interval",
			interval: time.Hour,
			maxRPS:   100,
			n:        60,
			want:     time.Minute,
		},
		{
			name:     "rps ceiling clamps a large set",
			interval: time.Hour,
			maxRPS:   1, // min delay 1s; 3600 handles would want 1s, equal
			n:        7200,
			want:     time.Second, // 3600s/7200 = 0.5s < 1s floor -> 1s
		},
		{
			name:     "no handles falls back to min delay",
			interval: time.Hour,
			maxRPS:   2,
			n:        0,
			want:     500 * time.Millisecond,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := testCache(t, Config{
				RefreshInterval:      tt.interval,
				MaxRequestsPerSecond: tt.maxRPS,
			}, func() []string { return nil })

			assert.Equal(t, tt.want, c.paceFor(tt.n))
		})
	}
}

func TestRefreshFetchesAndCaches(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/alice.keys":
			_, _ = io.WriteString(w, "ssh-ed25519 AAA\nssh-rsa BBB\n")
		case "/ghost.keys":
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	c := testCache(t, Config{
		BaseURL:              srv.URL,
		RefreshInterval:      time.Hour,
		MaxRequestsPerSecond: 100,
	}, func() []string { return nil })

	entry, err := c.Refresh(context.Background(), "alice")
	require.NoError(t, err)
	assert.Equal(t, []string{"ssh-ed25519 AAA", "ssh-rsa BBB"}, entry.Keys)
	assert.Empty(t, entry.LastError)
	assert.False(t, entry.FetchedAt.IsZero())

	cached, ok := c.Get("ALICE") // case-insensitive
	require.True(t, ok)
	assert.Equal(t, entry.Keys, cached.Keys)

	// A 404 records an error but does not panic or cache keys.
	ghost, err := c.Refresh(context.Background(), "ghost")
	require.NoError(t, err)
	assert.Empty(t, ghost.Keys)
	assert.NotEmpty(t, ghost.LastError)
}

func TestRefreshPreservesLastGoodOnError(t *testing.T) {
	var fail atomic.Bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusTooManyRequests)

			return
		}
		_, _ = io.WriteString(w, "ssh-ed25519 GOOD\n")
	}))
	defer srv.Close()

	c := testCache(t, Config{
		BaseURL:              srv.URL,
		RefreshInterval:      time.Hour,
		MaxRequestsPerSecond: 100,
	}, func() []string { return nil })

	_, err := c.Refresh(context.Background(), "alice")
	require.NoError(t, err)

	fail.Store(true)
	entry, err := c.Refresh(context.Background(), "alice")
	require.NoError(t, err)

	// Keys are retained from the last good fetch; the error is recorded.
	assert.Equal(t, []string{"ssh-ed25519 GOOD"}, entry.Keys)
	assert.NotEmpty(t, entry.LastError)
}

func TestPruneDropsUnwantedHandles(t *testing.T) {
	c := testCache(t, Config{
		RefreshInterval:      time.Hour,
		MaxRequestsPerSecond: 100,
	}, func() []string { return nil })

	c.entries["alice"] = &Entry{Handle: "alice"}
	c.entries["bob"] = &Entry{Handle: "bob"}

	c.prune([]string{"Alice"}) // case-insensitive keep

	_, hasAlice := c.Get("alice")
	_, hasBob := c.Get("bob")
	assert.True(t, hasAlice)
	assert.False(t, hasBob)
}

func TestWalkRefreshesDesiredSet(t *testing.T) {
	var hits sync.Map

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handle := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), ".keys")
		hits.Store(handle, true)
		_, _ = io.WriteString(w, "ssh-ed25519 "+handle+"\n")
	}))
	defer srv.Close()

	c := testCache(t, Config{
		BaseURL:              srv.URL,
		RefreshInterval:      10 * time.Millisecond,
		MaxRequestsPerSecond: 1000,
	}, func() []string { return []string{"alice", "bob"} })

	require.NoError(t, c.Start(t.Context()))
	defer func() { require.NoError(t, c.Stop()) }()

	require.Eventually(t, func() bool {
		a, ok := c.Get("alice")
		if !ok || len(a.Keys) == 0 {
			return false
		}
		b, ok := c.Get("bob")

		return ok && len(b.Keys) > 0
	}, 2*time.Second, 10*time.Millisecond)

	st := c.Status()
	assert.Equal(t, 2, st.Handles)
	assert.Equal(t, 2, st.Cached)
}
