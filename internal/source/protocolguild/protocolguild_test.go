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
## 1.1 Active Members

### Client Development

#### Consensus Clients

| **Team** | Weight | Contributions |
|:---|:---|:---|
| **Lighthouse (Sigma Prime) - 3 Members** | **3** | [sigp/lighthouse](https://github.com/sigp/lighthouse) |
| [Sean Anderson](https://github.com/realbigsean/) | 0.5 | [sigp/lighthouse](https://github.com/sigp/lighthouse) |
| [Jimmy Chen](https://github.com/jimmygchen) | 1 | [sigp/lighthouse](https://github.com/sigp/lighthouse) |
| [Someone Web](https://example.com/me) | 1 | |
| **Teku (Consensys) - 2 Members** | **2** | [ConsenSys/teku](https://github.com/ConsenSys/teku) |
| [Paul Harris](https://github.com/rolfyone/) | 1 | [Consensys/teku](https://github.com/Consensys/teku) |
| [Stefan Bratanov](https://github.com/StefanBratanov/) | 1 | [Consensys/teku](https://github.com/Consensys/teku) |

#### Execution Clients

| **Team** | Weight | Contributions |
|:---|:---|:---|
| **Reth (Paradigm) - 1 Member** | **1** | [paradigmxyz/reth](https://github.com/paradigmxyz/reth) |
| [joshieDo]([https://github.com/fgimenez](http://github.com/joshieDo)) | 0.5 | [paradigmxyz/reth](https://github.com/paradigmxyz/reth) |
| **Unmapped Client - 1 Member** | **1** | |
| [Nobody](https://github.com/nobody) | 1 | |

### Upgrade + Maintenance Support

| **Team** | **Weight** | **Contributions** |
|:---|:---|:---|
| **Execution Coordination** (1 contributors) | **1** | |
| [Nixo](https://github.com/nixorokish/) | 1 | |
| **EIPs** (1 contributors) | **1** | |
| [Editor](https://github.com/editor) | 1 | |

## 1.2 Alumni Members

| [Former Member](https://github.com/formermember) | 1 | |
`

func TestParse(t *testing.T) {
	sections := map[string]string{
		"Lighthouse (Sigma Prime)": "lighthouse",
		"Teku (Consensys)":         "teku",
		"Reth (Paradigm)":          "reth",
		"Execution Coordination":   "el-specs",
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(sampleDoc))
	}))
	defer srv.Close()

	s := New(slog.Default(), srv.Client(), srv.URL, sections, 0)

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
	assert.ElementsMatch(t, []string{"nixorokish"}, byTeam["el-specs"],
		"the older \"(N contributors)\" header form is still recognised")

	for _, m := range got {
		assert.NotEqual(t, "nobody", m.Handle, "unmapped client section must be skipped")
		assert.NotEqual(t, "editor", m.Handle, "non-client section must be skipped")
		assert.NotEqual(t, "formermember", m.Handle, "a heading ends the preceding table")
	}
}

func TestSectionName(t *testing.T) {
	tests := []struct {
		name string
		cell string
		want string
	}{
		{"members suffix", "Teku (Consensys) - 8 Members", "Teku (Consensys)"},
		{"singular member suffix", "Ethereum Cat Herders - 1 Member", "Ethereum Cat Herders"},
		{"en dash", "Nethermind – 17 Members", "Nethermind"},
		{"no suffix", "Robust Incentive Group (RIG)", "Robust Incentive Group (RIG)"},
		{"digits in name kept", "zkEVM (EF) - 5 Members", "zkEVM (EF)"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, sectionName(tt.cell))
		})
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
		{"team header", "| **Teku (Consensys) - 8 Members** | **8** | |", "", false},
		{"legacy team header", "| **Teku** (7 contributors) | **7** | |", "", false},
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

func TestMissingSectionsReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(sampleDoc))
	}))
	defer srv.Close()

	// "Teku" without its "(Consensys)" suffix is what a rename looks like: the
	// header is gone from the document even though the members are still there.
	sections := map[string]string{
		"Lighthouse (Sigma Prime)": "lighthouse",
		"Teku":                     "teku",
		"Geth (EF)":                "geth",
	}

	t.Run("within budget", func(t *testing.T) {
		s := New(slog.Default(), srv.Client(), srv.URL, sections, 2)

		got, err := s.Fetch(context.Background())
		require.NoError(t, err)
		assert.NotEmpty(t, got, "sections that do still match are returned")
		assert.Equal(t, []string{"Geth (EF)", "Teku"}, s.Warnings())
	})

	t.Run("over budget", func(t *testing.T) {
		s := New(slog.Default(), srv.Client(), srv.URL, sections, 1)

		_, err := s.Fetch(context.Background())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "2 of 3 configured sections missing")
		assert.Contains(t, err.Error(), "Teku")
	})

	t.Run("all present", func(t *testing.T) {
		s := New(slog.Default(), srv.Client(), srv.URL,
			map[string]string{"Lighthouse (Sigma Prime)": "lighthouse"}, 1)

		_, err := s.Fetch(context.Background())
		require.NoError(t, err)
		assert.Empty(t, s.Warnings())
	})
}
