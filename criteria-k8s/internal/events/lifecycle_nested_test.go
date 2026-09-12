package events

// Regression tests for CRI-132: the parser must accept the engine's nested
// AdapterEvent envelope. The captured lines below are copied byte-for-byte
// from the triage artifact evidence/cri132-r3-emission-lines.ndjson (a real
// runApply emission) — never hand-reformatted.

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Verbatim capture, seq 1: nested provision_wanted at scope entry.
const capturedNestedProvisionWanted = `{"schema_version":1,"seq":1,"run_id":"58f12fe6-5144-4438-8e99-13f701be454c","payload_type":"AdapterEvent","payload":{"adapter":"default","kind":"adapter.lifecycle.provision_wanted","data":{"adapter":"default","digest":"","run_id":"","scope_instance_id":"13d83326-f18d-49ed-942d-29c5f291a305","scope_name":"","shim_listen_address":"127.0.0.1:38625","token_ref":"/tmp/TestCRI132CaptureEmissionLines1209998812/001/runs/58f12fe6-5144-4438-8e99-13f701be454c/remote-tokens/13d83326-f18d-49ed-942d-29c5f291a305/noop.token"}}}`

// Verbatim capture, seq 7: nested released. payload.adapter is "noop.default"
// here vs "default" on provision — an engine-side asymmetry, so release
// handling is deliberately not activated in this workstream.
const capturedNestedReleased = `{"schema_version":1,"seq":7,"run_id":"58f12fe6-5144-4438-8e99-13f701be454c","payload_type":"AdapterEvent","payload":{"adapter":"noop.default","kind":"adapter.lifecycle.released","data":{"adapter":"noop.default","digest":"","run_id":"","scope_instance_id":"13d83326-f18d-49ed-942d-29c5f291a305","scope_name":"","shim_listen_address":"127.0.0.1:38625","token_ref":"/tmp/TestCRI132CaptureEmissionLines1209998812/001/runs/58f12fe6-5144-4438-8e99-13f701be454c/remote-tokens/13d83326-f18d-49ed-942d-29c5f291a305/noop.token"}}}`

// The captured nested provision_wanted line must parse to exactly one event
// with every field sourced from the captured emission.
func TestParseLifecycleEventsNestedProvisionWanted(t *testing.T) {
	events, err := ParseLifecycleEventsBytes([]byte(capturedNestedProvisionWanted))
	require.NoError(t, err)
	require.Len(t, events, 1)

	ev := events[0]
	assert.Equal(t, EventProvisionWanted, ev.Event)
	assert.True(t, ev.IsProvisionWanted())
	assert.Equal(t, "default", ev.AdapterName)
	assert.Equal(t, "13d83326-f18d-49ed-942d-29c5f291a305", ev.ScopeID)
	assert.Equal(t, "58f12fe6-5144-4438-8e99-13f701be454c", ev.RunID)
	assert.Equal(t, "127.0.0.1:38625", ev.ShimAddress)
	assert.True(t, strings.HasSuffix(ev.TokenFile, "/noop.token"),
		"TokenFile %q must end in /noop.token", ev.TokenFile)
	// Captured digest is empty and must not be synthesized.
	assert.Equal(t, "", ev.Digest)
	assert.Equal(t, "default/13d83326-f18d-49ed-942d-29c5f291a305", ev.ScopeKey())

	active := ActiveProvisions(events)
	require.Len(t, active, 1)
	assert.Equal(t, ev.ScopeKey(), active[0].ScopeKey())
}

// The captured nested released line must yield no lifecycle event: only
// provision_wanted is mapped in this workstream.
func TestParseLifecycleEventsNestedReleasedSkipped(t *testing.T) {
	events, err := ParseLifecycleEventsBytes([]byte(capturedNestedReleased))
	require.NoError(t, err)
	assert.Empty(t, events)
}

// The full captured two-line stream yields exactly the provision event.
func TestParseLifecycleEventsCapturedStream(t *testing.T) {
	stream := capturedNestedProvisionWanted + "\n" + capturedNestedReleased + "\n"
	events, err := ParseLifecycleEventsBytes([]byte(stream))
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, EventProvisionWanted, events[0].Event)
}

// An AdapterEvent line whose kind is not a recognized lifecycle kind must be
// skipped without error.
func TestParseLifecycleEventsNestedNonLifecycleKindsSkipped(t *testing.T) {
	input := `{"schema_version":1,"seq":2,"run_id":"run-1","payload_type":"AdapterEvent","payload":{"adapter":"default","kind":"agent.message","data":{"text":"hello"}}}
{"schema_version":1,"seq":3,"run_id":"run-1","payload_type":"AdapterEvent","payload":{"adapter":"default","kind":"permission.granted","data":{}}}
{"schema_version":1,"seq":4,"run_id":"run-1","payload_type":"AdapterEvent","payload":{"adapter":"default","kind":"tool.invocation","data":{}}}`
	events, err := ParseLifecycleEventsBytes([]byte(input))
	require.NoError(t, err)
	assert.Empty(t, events)
}

// An AdapterEvent envelope without a recognized lifecycle kind and a line
// with payload_type but no payload object are both skipped silently.
func TestParseLifecycleEventsNestedUnknownKindAndMissingPayloadSkipped(t *testing.T) {
	input := `{"schema_version":1,"seq":1,"run_id":"run-1","payload_type":"AdapterEvent","payload":{"adapter":"default","kind":"adapter.lifecycle.unknown_kind","data":{"adapter":"default"}}}
{"schema_version":1,"seq":2,"run_id":"run-1","payload_type":"RunStarted","payload":{"workflowName":"x"}}`
	events, err := ParseLifecycleEventsBytes([]byte(input))
	require.NoError(t, err)
	assert.Empty(t, events)
}

// The adapter_name == "" skip applies to nested-derived events.
func TestParseLifecycleEventsNestedMissingAdapterSkipped(t *testing.T) {
	input := `{"schema_version":1,"seq":1,"run_id":"run-1","payload_type":"AdapterEvent","payload":{"kind":"adapter.lifecycle.provision_wanted","data":{"digest":"","scope_instance_id":"scope-1","shim_listen_address":"127.0.0.1:1"}}}`
	events, err := ParseLifecycleEventsBytes([]byte(input))
	require.NoError(t, err)
	assert.Empty(t, events)
}

// Unknown fields — envelope, payload, and data level — must never fail
// parsing, and scope_name must be ignored.
func TestParseLifecycleEventsNestedUnknownFieldsIgnored(t *testing.T) {
	input := `{"schema_version":2,"seq":9,"run_id":"run-1","future_field":true,"payload_type":"AdapterEvent","payload":{"adapter":"default","kind":"adapter.lifecycle.provision_wanted","future_payload":1,"data":{"adapter":"default","digest":"abc123","run_id":"","scope_instance_id":"scope-1","scope_name":"root","shim_listen_address":"127.0.0.1:2","token_ref":"/data/tok","unknown_nested":{"x":1}}}}`
	events, err := ParseLifecycleEventsBytes([]byte(input))
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, EventProvisionWanted, events[0].Event)
	assert.Equal(t, "default", events[0].AdapterName)
	assert.Equal(t, "scope-1", events[0].ScopeID)
	assert.Equal(t, "127.0.0.1:2", events[0].ShimAddress)
	assert.Equal(t, "/data/tok", events[0].TokenFile)
	assert.Equal(t, "abc123", events[0].Digest)
}

// The flat operator shape must continue to parse: the control stream yields
// 3 events in stream order.
func TestParseLifecycleEventsFlatControlStream(t *testing.T) {
	input := `{"event":"run.started"}
{"event":"provision_wanted","run_id":"CRI-42","scope_id":"root","adapter_name":"shell","digest":"abc123","shim_address":"10.0.0.1:7778","token_file":"/data/intake/CRI-42/tokens/root-shell"}
{"event":"provision_wanted","run_id":"CRI-42","scope_id":"sub-1","adapter_name":"copilot","digest":"def456","shim_address":"10.0.0.1:7778","token_file":"/data/intake/CRI-42/tokens/sub-1-copilot"}
not json
{"event":"release","run_id":"CRI-42","scope_id":"root","adapter_name":"shell"}`

	events, err := ParseLifecycleEventsBytes([]byte(input))
	require.NoError(t, err)
	require.Len(t, events, 3)
	assert.Equal(t, EventProvisionWanted, events[0].Event)
	assert.Equal(t, "shell", events[0].AdapterName)
	assert.Equal(t, EventProvisionWanted, events[1].Event)
	assert.Equal(t, "copilot", events[1].AdapterName)
	assert.Equal(t, EventRelease, events[2].Event)
}

// Flat and nested shapes coexist in one stream and keep stream order.
func TestParseLifecycleEventsMixedShapes(t *testing.T) {
	input := `{"schema_version":1,"seq":1,"run_id":"run-nested","payload_type":"AdapterEvent","payload":{"adapter":"default","kind":"adapter.lifecycle.provision_wanted","data":{"adapter":"default","scope_instance_id":"scope-nested","shim_listen_address":"127.0.0.1:9","token_ref":"/data/toks/scope-nested"}}}
{"event":"provision_wanted","run_id":"run-flat","scope_id":"scope-flat","adapter_name":"shell"}
{"event":"run.started"}`
	events, err := ParseLifecycleEventsBytes([]byte(input))
	require.NoError(t, err)
	require.Len(t, events, 2)
	assert.Equal(t, EventProvisionWanted, events[0].Event)
	assert.Equal(t, "default", events[0].AdapterName)
	assert.Equal(t, "scope-nested", events[0].ScopeID)
	assert.Equal(t, "run-nested", events[0].RunID)
	assert.Equal(t, EventProvisionWanted, events[1].Event)
	assert.Equal(t, "shell", events[1].AdapterName)
	assert.Equal(t, "scope-flat", events[1].ScopeID)
}
