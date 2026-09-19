package events

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// CRI-234: provision/release events carry the compiled environment
// declaration's identity (CRI-233, runner fc95449) as the environment_type /
// environment_name pair — the keys the pinned engine actually emits — and
// the parsers derive the co-location grouping identity "type/name" from
// that pair. Events carrying neither key (pre-fc95449 engines) keep the
// empty identity, which the reconcile maps to the per-adapter pod fallback.

func TestParseLifecycleEventsFlatCarriesEnvironmentPair(t *testing.T) {
	input := strings.Join([]string{
		`{"event":"provision_wanted","run_id":"CRI-234","scope_id":"root","adapter_name":"shell","environment_type":"remote","environment_name":"worktree"}`,
		`{"event":"provision_wanted","run_id":"CRI-234","scope_id":"sub-1","adapter_name":"copilot","environment_type":"remote","environment_name":"prod"}`,
		`{"event":"provision_wanted","run_id":"CRI-234","scope_id":"legacy","adapter_name":"shell"}`,
	}, "\n")

	parsed, err := ParseLifecycleEventsBytes([]byte(input))
	require.NoError(t, err)
	require.Len(t, parsed, 3)

	assert.Equal(t, "remote", parsed[0].EnvironmentType)
	assert.Equal(t, "worktree", parsed[0].EnvironmentName)
	assert.Equal(t, "remote/worktree", parsed[0].Environment, "the (type,name) pair forms the co-location identity")
	assert.Equal(t, "remote/prod", parsed[1].Environment, "a different (type,name) pair is a distinct identity")
	assert.Empty(t, parsed[2].Environment, "events without the pair keep the empty fallback identity")
}

func TestParseLifecycleEventsNestedCarriesEnvironmentPair(t *testing.T) {
	input := `{"schema_version":1,"seq":1,"run_id":"CRI-234","payload_type":"AdapterEvent","payload":{"adapter":"default","kind":"adapter.lifecycle.provision_wanted","data":{"adapter":"default","scope_instance_id":"scope-a","environment_type":"remote","environment_name":"prod"}}}`
	parsed, err := ParseLifecycleEventsBytes([]byte(input))
	require.NoError(t, err)
	require.Len(t, parsed, 1)
	assert.Equal(t, "remote", parsed[0].EnvironmentType)
	assert.Equal(t, "prod", parsed[0].EnvironmentName)
	assert.Equal(t, "remote/prod", parsed[0].Environment)
}

func TestParseLifecycleEventsNestedWithoutEnvironmentPairStaysEmpty(t *testing.T) {
	input := `{"schema_version":1,"seq":1,"run_id":"CRI-234","payload_type":"AdapterEvent","payload":{"adapter":"default","kind":"adapter.lifecycle.provision_wanted","data":{"adapter":"default","scope_instance_id":"scope-a"}}}`
	parsed, err := ParseLifecycleEventsBytes([]byte(input))
	require.NoError(t, err)
	require.Len(t, parsed, 1)
	assert.Empty(t, parsed[0].Environment)
	assert.Empty(t, parsed[0].EnvironmentType)
	assert.Empty(t, parsed[0].EnvironmentName)
}

// EnvironmentIdentity: the grouping key derives from the (type, name) pair
// and stays empty only when both halves are absent.
func TestEnvironmentIdentity(t *testing.T) {
	assert.Equal(t, "remote/worktree", EnvironmentIdentity("remote", "worktree"))
	assert.Equal(t, "remote/prod", EnvironmentIdentity("remote", "prod"))
	assert.Equal(t, "sandbox/ci", EnvironmentIdentity("sandbox", "ci"))
	assert.Empty(t, EnvironmentIdentity("", ""), "neither half -> empty identity (per-adapter fallback)")
}
