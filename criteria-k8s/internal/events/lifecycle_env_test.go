package events

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// CRI-234: provision/release events carry the config-declared environment
// identity (CRI-233, runner fc95449) as the co-location grouping key. Both
// event shapes (flat file lines and nested AdapterEvent envelopes) must
// surface it on LifecycleEvent.Environment, with the "environment_id"
// spelling coalesced, and stay empty for pre-fc95449 events.

func TestParseLifecycleEventsFlatCarriesEnvironment(t *testing.T) {
	input := strings.Join([]string{
		`{"event":"provision_wanted","run_id":"CRI-234","scope_id":"root","adapter_name":"shell","environment":"ci"}`,
		`{"event":"provision_wanted","run_id":"CRI-234","scope_id":"sub-1","adapter_name":"copilot","environment_id":"prod"}`,
		`{"event":"provision_wanted","run_id":"CRI-234","scope_id":"legacy","adapter_name":"shell"}`,
	}, "\n")

	parsed, err := ParseLifecycleEventsBytes([]byte(input))
	require.NoError(t, err)
	require.Len(t, parsed, 3)

	assert.Equal(t, "ci", parsed[0].Environment, "the canonical environment key is carried")
	assert.Equal(t, "prod", parsed[1].Environment, "the environment_id spelling is coalesced into Environment")
	assert.Empty(t, parsed[2].Environment, "events without env identity keep the empty fallback key")
}

func TestParseLifecycleEventsNestedCarriesEnvironment(t *testing.T) {
	input := `{"schema_version":1,"seq":1,"run_id":"CRI-234","payload_type":"AdapterEvent","payload":{"adapter":"default","kind":"adapter.lifecycle.provision_wanted","data":{"adapter":"default","scope_instance_id":"scope-a","environment":"ci"}}}`
	parsed, err := ParseLifecycleEventsBytes([]byte(input))
	require.NoError(t, err)
	require.Len(t, parsed, 1)
	assert.Equal(t, "ci", parsed[0].Environment)
}

func TestParseLifecycleEventsNestedCarriesEnvironmentIDAlias(t *testing.T) {
	input := `{"schema_version":1,"seq":1,"run_id":"CRI-234","payload_type":"AdapterEvent","payload":{"adapter":"default","kind":"adapter.lifecycle.provision_wanted","data":{"adapter":"default","scope_instance_id":"scope-a","environment_id":"prod"}}}`
	parsed, err := ParseLifecycleEventsBytes([]byte(input))
	require.NoError(t, err)
	require.Len(t, parsed, 1)
	assert.Equal(t, "prod", parsed[0].Environment, "the environment_id spelling is coalesced into Environment")
}

func TestParseLifecycleEventsNestedWithoutEnvironmentStaysEmpty(t *testing.T) {
	input := `{"schema_version":1,"seq":1,"run_id":"CRI-234","payload_type":"AdapterEvent","payload":{"adapter":"default","kind":"adapter.lifecycle.provision_wanted","data":{"adapter":"default","scope_instance_id":"scope-a"}}}`
	parsed, err := ParseLifecycleEventsBytes([]byte(input))
	require.NoError(t, err)
	require.Len(t, parsed, 1)
	assert.Empty(t, parsed[0].Environment)
}
