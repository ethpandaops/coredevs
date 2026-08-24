//go:build upstream

// This test talks to the live Protocol Guild document and is excluded from the
// normal build. It runs on a schedule so an upstream restructure is caught as a
// failing check, rather than as a dashboard that quietly attributes everyone to
// the wrong team until somebody notices.
//
//	go test -tags upstream ./internal/source/protocolguild/
package protocolguild

import (
	"context"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/coredevs/internal/config"
)

func TestUpstreamDocumentStillMatchesConfig(t *testing.T) {
	cfg, err := config.Load("../../../config.yaml")
	require.NoError(t, err)

	pg := cfg.Sources.ProtocolGuild
	if !pg.Enabled {
		t.Skip("protocol guild source disabled")
	}

	s := New(slog.Default(), &http.Client{Timeout: 30 * time.Second}, pg.URL,
		cfg.SectionTeams(), pg.MaxMissingSections)

	memberships, err := s.Fetch(context.Background())
	require.NoError(t, err, "the document no longer carries the configured section headers")

	assert.Empty(t, s.Warnings(),
		"configured protocolGuildSections absent from the document — rename them in config.yaml")

	teams := make(map[string]struct{}, len(memberships))
	for _, m := range memberships {
		teams[m.Team] = struct{}{}
	}

	t.Logf("parsed %d members across %d teams", len(memberships), len(teams))

	assert.GreaterOrEqual(t, len(memberships), pg.MinMembers,
		"parsed fewer members than the configured minMembers floor")
	assert.GreaterOrEqual(t, len(teams), pg.MinTeams,
		"parsed fewer teams than the configured minTeams floor")
}
