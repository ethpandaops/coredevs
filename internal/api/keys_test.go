package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/coredevs/internal/config"
	"github.com/ethpandaops/coredevs/internal/index"
	"github.com/ethpandaops/coredevs/internal/keys"
	"github.com/ethpandaops/coredevs/internal/source"
	"github.com/ethpandaops/coredevs/internal/syncer"
)

// fakeKeys is an in-memory KeyProvider for handler tests.
type fakeKeys struct {
	entries map[string]keys.Entry
	warmed  bool
}

func (f *fakeKeys) Warmed() bool { return f.warmed }

func (f *fakeKeys) Get(handle string) (keys.Entry, bool) {
	e, ok := f.entries[strings.ToLower(handle)]

	return e, ok
}

func (f *fakeKeys) Refresh(_ context.Context, handle string) (keys.Entry, error) {
	e := keys.Entry{
		Handle:    handle,
		Keys:      []string{"ssh-ed25519 FRESH " + handle},
		FetchedAt: time.Unix(1700000000, 0).UTC(),
	}
	f.entries[strings.ToLower(handle)] = e

	return e, nil
}

func (f *fakeKeys) Status() keys.Status {
	return keys.Status{Enabled: true, Handles: len(f.entries)}
}

func newTestHandler(t *testing.T, keyProvider KeyProvider) *Handler {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Config{
		HTTP:         config.HTTP{Addr: ":0"},
		SyncInterval: time.Hour,
		Teams:        map[string]config.Team{"geth": {DisplayName: "Geth"}},
	}

	store := index.NewStore()
	store.Set(index.Build(time.Unix(1700000000, 0).UTC(), []source.Membership{
		{Handle: "Alice", Team: "geth", Source: source.NameManual},
		{Handle: "Bob", Team: "geth", Source: source.NameManual},
	}))

	sync := syncer.New(logger, store, nil, time.Hour, "", nil, nil)

	return New(logger, cfg, store, sync.Statuses, nil, keyProvider)
}

func TestTeamKeysTxtAssemblesAuthorizedKeys(t *testing.T) {
	kp := &fakeKeys{entries: map[string]keys.Entry{
		"alice": {Handle: "Alice", Keys: []string{"ssh-ed25519 AAA", "ssh-rsa BBB"}, FetchedAt: time.Now()},
		// bob is intentionally absent -> pending, contributes no keys.
	}}

	h := newTestHandler(t, kp)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/users/geth/keys", nil))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "# Alice\nssh-ed25519 AAA\nssh-rsa BBB\n", rec.Body.String())
}

func TestTeamKeysJSONReportsPending(t *testing.T) {
	kp := &fakeKeys{entries: map[string]keys.Entry{
		"alice": {Handle: "Alice", Keys: []string{"ssh-ed25519 AAA"}, FetchedAt: time.Now()},
	}}

	h := newTestHandler(t, kp)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/users/geth/keys?format=json", nil))

	require.Equal(t, http.StatusOK, rec.Code)

	var body struct {
		Handles  int `json:"handles"`
		Pending  int `json:"pending"`
		KeyCount int `json:"keyCount"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, 2, body.Handles)
	assert.Equal(t, 1, body.Pending)
	assert.Equal(t, 1, body.KeyCount)
}

func TestTeamKeysUnknownTeam(t *testing.T) {
	h := newTestHandler(t, &fakeKeys{entries: map[string]keys.Entry{}})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/users/nope/keys", nil))

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestHandleKeysColdMissFetchesIndexedHandle(t *testing.T) {
	kp := &fakeKeys{entries: map[string]keys.Entry{}}
	h := newTestHandler(t, kp)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/handles/alice/keys", nil))

	require.Equal(t, http.StatusOK, rec.Code)

	var body struct {
		Handle string   `json:"handle"`
		Keys   []string `json:"keys"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "alice", body.Handle)
	assert.Equal(t, []string{"ssh-ed25519 FRESH alice"}, body.Keys)
}

func TestHandleKeysUnknownHandleNotFetched(t *testing.T) {
	kp := &fakeKeys{entries: map[string]keys.Entry{}}
	h := newTestHandler(t, kp)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/handles/stranger/keys", nil))

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Empty(t, kp.entries, "must not fetch keys for a non-indexed handle")
}

func TestKeysEndpointsDisabledWithoutProvider(t *testing.T) {
	h := newTestHandler(t, nil)

	for _, path := range []string{"/api/v1/users/geth/keys", "/api/v1/handles/alice/keys"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		assert.Equal(t, http.StatusServiceUnavailable, rec.Code, path)
	}
}

func TestReadyzGatesOnKeysWarm(t *testing.T) {
	cold := newTestHandler(t, &fakeKeys{entries: map[string]keys.Entry{}, warmed: false})
	rec := httptest.NewRecorder()
	cold.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code, "cold keys cache must not be ready")

	warm := newTestHandler(t, &fakeKeys{entries: map[string]keys.Entry{}, warmed: true})
	rec = httptest.NewRecorder()
	warm.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	assert.Equal(t, http.StatusOK, rec.Code, "warm keys cache is ready")

	// With keys disabled, readiness depends only on the index being present.
	noKeys := newTestHandler(t, nil)
	rec = httptest.NewRecorder()
	noKeys.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	assert.Equal(t, http.StatusOK, rec.Code, "keys disabled: ready once index is up")
}
