package index

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/coredevs/internal/source"
)

func TestBuildSuperset(t *testing.T) {
	now := time.Now().UTC()

	idx := Build(now, []source.Membership{
		{Handle: "jimmygchen", Team: "lighthouse", Source: source.NameProtocolGuild, Name: "Jimmy Chen"},
		// Same handle, different casing, from the org source — must dedupe and
		// merge provenance rather than create a second member.
		{Handle: "JimmyGchen", Team: "lighthouse", Source: source.NameGitHubOrg, Org: "sigp"},
		{Handle: "paulhauner", Team: "lighthouse", Source: source.NameGitHubOrg, Org: "sigp"},
		{Handle: "rolfyone", Team: "teku", Source: source.NameProtocolGuild},
		// Empty fields are ignored.
		{Handle: "", Team: "teku", Source: source.NameProtocolGuild},
		{Handle: "x", Team: "", Source: source.NameProtocolGuild},
	})

	require.Equal(t, now, idx.GeneratedAt)
	assert.ElementsMatch(t, []string{"lighthouse", "teku"}, idx.TeamSlugs())

	lh := idx.Members("lighthouse", "")
	require.Len(t, lh, 2)

	// Sorted by handle: jimmygchen, paulhauner.
	jimmy := lh[0]
	assert.Equal(t, "jimmygchen", jimmy.Handle, "first-seen casing preserved")
	assert.Equal(t, "Jimmy Chen", jimmy.Name)
	assert.Equal(t, []string{source.NameGitHubOrg, source.NameProtocolGuild}, jimmy.Sources,
		"provenance merged across sources and sorted")
	assert.Equal(t, []string{"sigp"}, jimmy.Orgs)
}

func TestMembersSourceFilter(t *testing.T) {
	idx := Build(time.Now().UTC(), []source.Membership{
		{Handle: "a", Team: "teku", Source: source.NameProtocolGuild},
		{Handle: "b", Team: "teku", Source: source.NameGitHubOrg, Org: "Consensys"},
	})

	pg := idx.Members("teku", source.NameProtocolGuild)
	require.Len(t, pg, 1)
	assert.Equal(t, "a", pg[0].Handle)

	org := idx.Members("teku", source.NameGitHubOrg)
	require.Len(t, org, 1)
	assert.Equal(t, "b", org[0].Handle)

	assert.Len(t, idx.Members("teku", ""), 2)
	assert.Nil(t, idx.Members("nonexistent", ""))
}

func TestLookup(t *testing.T) {
	idx := Build(time.Now().UTC(), []source.Membership{
		{Handle: "marcopolo", Team: "lighthouse", Source: source.NameProtocolGuild},
		{Handle: "marcopolo", Team: "prysm", Source: source.NameProtocolGuild},
	})

	// Case-insensitive lookup across teams.
	got := idx.Lookup("MarcoPolo")
	assert.ElementsMatch(t, []string{"lighthouse", "prysm"}, keys(got))

	assert.Empty(t, idx.Lookup("unknown"))
}

func TestUsersDedupesAcrossTeams(t *testing.T) {
	idx := Build(time.Now().UTC(), []source.Membership{
		{Handle: "marcopolo", Team: "lighthouse", Source: source.NameProtocolGuild},
		// Same person, different team, different casing, different source.
		{Handle: "MarcoPolo", Team: "prysm", Source: source.NameGitHubOrg, Org: "OffchainLabs"},
		{Handle: "rolfyone", Team: "teku", Source: source.NameProtocolGuild, Name: "Paul Harris"},
	})

	users := idx.Users()
	require.Len(t, users, 2, "marcopolo collapsed to one user across two teams")

	// Sorted by handle: marcopolo, rolfyone.
	mp := users[0]
	assert.Equal(t, "marcopolo", mp.Handle, "first-seen casing preserved")
	assert.Equal(t, []string{"lighthouse", "prysm"}, mp.Teams)
	assert.Equal(t, []string{source.NameGitHubOrg, source.NameProtocolGuild}, mp.Sources)
	assert.Equal(t, []string{"OffchainLabs"}, mp.Orgs)

	assert.Equal(t, "Paul Harris", users[1].Name)
}

func TestStoreAtomicSwap(t *testing.T) {
	s := NewStore()
	assert.Nil(t, s.Get())

	first := Build(time.Now().UTC(), []source.Membership{
		{Handle: "a", Team: "teku", Source: source.NameProtocolGuild},
	})
	s.Set(first)
	assert.Same(t, first, s.Get())

	second := Build(time.Now().UTC(), nil)
	s.Set(second)
	assert.Same(t, second, s.Get())
}

func keys(m map[string]*Member) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	return out
}
