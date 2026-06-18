package manual

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/coredevs/internal/source"
)

func TestFetch(t *testing.T) {
	s := New(map[string][]string{
		"teku":       {"newjoiner", "anotherdev"},
		"lighthouse": {"freshface"},
		"empty":      {""},
	})

	got, err := s.Fetch(context.Background())
	require.NoError(t, err)

	byTeam := make(map[string][]string)
	for _, m := range got {
		assert.Equal(t, source.NameManual, m.Source)
		byTeam[m.Team] = append(byTeam[m.Team], m.Handle)
	}

	assert.ElementsMatch(t, []string{"newjoiner", "anotherdev"}, byTeam["teku"])
	assert.ElementsMatch(t, []string{"freshface"}, byTeam["lighthouse"])
	assert.Empty(t, byTeam["empty"], "blank handles are skipped")
}
