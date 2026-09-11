package events

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseLifecycleEvents(t *testing.T) {
	input := strings.Join([]string{
		`{"event":"run.started"}`,
		`{"event":"provision_wanted","run_id":"CRI-42","scope_id":"root","adapter_name":"shell","digest":"abc123","shim_address":"10.0.0.1:7778","token_file":"/data/intake/CRI-42/tokens/root-shell"}`,
		`{"event":"provision_wanted","run_id":"CRI-42","scope_id":"sub-1","adapter_name":"copilot","digest":"def456","shim_address":"10.0.0.1:7778","token_file":"/data/intake/CRI-42/tokens/sub-1-copilot"}`,
		`not json`,
		`{"event":"release","run_id":"CRI-42","scope_id":"root","adapter_name":"shell"}`,
	}, "\n")

	events, err := ParseLifecycleEventsBytes([]byte(input))
	require.NoError(t, err)
	require.Len(t, events, 3)

	assert.Equal(t, EventProvisionWanted, events[0].Event)
	assert.Equal(t, "root", events[0].ScopeID)
	assert.Equal(t, "shell", events[0].AdapterName)
	assert.Equal(t, "10.0.0.1:7778", events[0].ShimAddress)

	assert.Equal(t, EventRelease, events[2].Event)
	assert.Equal(t, "root", events[2].ScopeID)
}

func TestActiveProvisions(t *testing.T) {
	events := []LifecycleEvent{
		{Event: EventProvisionWanted, AdapterName: "shell", ScopeID: "root"},
		{Event: EventProvisionWanted, AdapterName: "copilot", ScopeID: "root"},
		{Event: EventProvisionWanted, AdapterName: "shell", ScopeID: "sub-1"},
		{Event: EventRelease, AdapterName: "shell", ScopeID: "root"},
	}

	active := ActiveProvisions(events)
	require.Len(t, active, 2)

	keys := make(map[string]bool)
	for _, ev := range active {
		keys[ev.ScopeKey()] = true
	}
	assert.True(t, keys["copilot/root"])
	assert.True(t, keys["shell/sub-1"])
	assert.False(t, keys["shell/root"])
}

func TestActiveProvisionsReleaseBeforeProvision(t *testing.T) {
	events := []LifecycleEvent{
		{Event: EventRelease, AdapterName: "shell", ScopeID: "root"},
		{Event: EventProvisionWanted, AdapterName: "shell", ScopeID: "root"},
	}

	active := ActiveProvisions(events)
	require.Len(t, active, 1)
	assert.Equal(t, "root", active[0].ScopeID)
}

func TestParseLifecycleEventsIgnoresMissingAdapter(t *testing.T) {
	input := `{"event":"provision_wanted","run_id":"CRI-42","scope_id":"root"}`
	events, err := ParseLifecycleEventsBytes([]byte(input))
	require.NoError(t, err)
	assert.Empty(t, events)
}
