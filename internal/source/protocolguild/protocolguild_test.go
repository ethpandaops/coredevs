package protocolguild

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/coredevs/internal/source"
)

const sampleDoc = `
## CLIENT IMPLEMENTATIONS

| **CONSENSUS CLIENTS** | Weight | Contributions |
|:---|:---|:---|
| **Lighthouse** (3 contributors) | **3** | [sigp/lighthouse](https://github.com/sigp/lighthouse) |
| [Sean Anderson](https://github.com/realbigsean/) | 0.5 | [sigp/lighthouse](https://github.com/sigp/lighthouse) |
| [Jimmy Chen](https://github.com/jimmygchen) | 1 | [sigp/lighthouse](https://github.com/sigp/lighthouse) |
| [Someone Web](https://example.com/me) | 1 | |
| **Teku** (2 contributors) | **2** | [ConsenSys/teku](https://github.com/ConsenSys/teku) |
| [Paul Harris](https://github.com/rolfyone/) | 1 | [Consensys/teku](https://github.com/Consensys/teku) |
| [Stefan Bratanov](https://github.com/StefanBratanov/) | 1 | [Consensys/teku](https://github.com/Consensys/teku) |

| **EXECUTION CLIENTS** | Weight | Contributions |
|:---|:---|:---|
| **Reth** (1 contributors) | **1** | [paradigmxyz/reth](https://github.com/paradigmxyz/reth) |
| [joshieDo]([https://github.com/fgimenez](http://github.com/joshieDo)) | 0.5 | [paradigmxyz/reth](https://github.com/paradigmxyz/reth) |
| **Unmapped Client** (1 contributors) | **1** | |
| [Nobody](https://github.com/nobody) | 1 | |

## GOVERNANCE
| **EIPs** (1 contributors) | **1** | |
| [Editor](https://github.com/editor) | 1 | |
`

func TestParse(t *testing.T) {
	sections := map[string]string{
		"Lighthouse": "lighthouse",
		"Teku":       "teku",
		"Reth":       "reth",
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(sampleDoc))
	}))
	defer srv.Close()

	s := New(slog.Default(), srv.Client(), srv.URL, sections)

	got, err := s.Fetch(context.Background())
	require.NoError(t, err)

	byTeam := make(map[string][]string)
	for _, m := range got {
		assert.Equal(t, source.NameProtocolGuild, m.Source)
		byTeam[m.Team] = append(byTeam[m.Team], m.Handle)
	}

	assert.ElementsMatch(t, []string{"realbigsean", "jimmygchen"}, byTeam["lighthouse"],
		"trailing slashes stripped, non-github personal link skipped")
	assert.ElementsMatch(t, []string{"rolfyone", "StefanBratanov"}, byTeam["teku"])
	assert.ElementsMatch(t, []string{"joshieDo"}, byTeam["reth"],
		"last github.com/<user> in a malformed nested link wins")

	for _, m := range got {
		assert.NotEqual(t, "nobody", m.Handle, "unmapped client section must be skipped")
		assert.NotEqual(t, "editor", m.Handle, "non-client section must be skipped")
	}
}

func TestParseMemberRow(t *testing.T) {
	tests := []struct {
		name       string
		line       string
		wantHandle string
		wantOK     bool
	}{
		{"trailing slash", "| [A](https://github.com/foo/) | 1 | |", "foo", true},
		{"no slash", "| [A](https://github.com/bar) | 1 | |", "bar", true},
		{"malformed nested", "| [A]([https://github.com/x](http://github.com/joshieDo)) | 1 | |", "joshieDo", true},
		{"personal site", "| [A](https://example.com/me) | 1 | |", "", false},
		{"repo link not user", "| [A](https://github.com/org/repo) | 1 | |", "", false},
		{"team header", "| **Teku** (7 contributors) | **7** | |", "", false},
		{"divider", "| **CONSENSUS CLIENTS** | Weight | Contributions |", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, handle, ok := parseMemberRow(tt.line)
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.wantHandle, handle)
		})
	}
}
