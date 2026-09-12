package castle

// Wire conversion tests pin the castle Envelope → operator LifecycleEvent
// mapping to the engine's captured emission (CRI-132 triage artifact
// evidence/cri132-r3-emission-lines.ndjson). The captured ndjson lines are
// byte-for-byte renders of the castle envelopes the engine submits, so the
// castle-sourced lifecycle stream must derive the exact same
// events.LifecycleEvent values the CRI-132 file parser produces.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"

	v1 "github.com/brokenbots/workflow-example/criteria-k8s/internal/criteria/pb/criteria/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/events"
)

// Verbatim capture, seq 1: nested provision_wanted at scope entry. Copied
// byte-for-byte from the triage artifact — never hand-reformatted.
const capturedNestedProvisionWanted = `{"schema_version":1,"seq":1,"run_id":"58f12fe6-5144-4438-8e99-13f701be454c","payload_type":"AdapterEvent","payload":{"adapter":"default","kind":"adapter.lifecycle.provision_wanted","data":{"adapter":"default","digest":"","run_id":"","scope_instance_id":"13d83326-f18d-49ed-942d-29c5f291a305","scope_name":"","shim_listen_address":"127.0.0.1:38625","token_ref":"/tmp/runs/58f12fe6/remote-tokens/13d83326/noop.token"}}}`

// Verbatim capture, seq 7: nested released. payload.adapter is "noop.default"
// here vs "default" on provision — an engine-side asymmetry the client
// resolves against the provisioned adapter for the scope instance.
const capturedNestedReleased = `{"schema_version":1,"seq":7,"run_id":"58f12fe6-5144-4438-8e99-13f701be454c","payload_type":"AdapterEvent","payload":{"adapter":"noop.default","kind":"adapter.lifecycle.released","data":{"adapter":"noop.default","digest":"","run_id":"","scope_instance_id":"13d83326-f18d-49ed-942d-29c5f291a305","scope_name":"","shim_listen_address":"127.0.0.1:38625","token_ref":"/tmp/runs/58f12fe6/remote-tokens/13d83326/noop.token"}}}`

// The castle conversion of the envelope equivalent to the captured
// provision_wanted line must yield exactly the event the CRI-132 file parser
// derives from that line.
func TestLifecycleFromEnvelopeMatchesFileParserOnCapturedProvision(t *testing.T) {
	data := mustStruct(t, map[string]any{
		"adapter":             "default",
		"digest":              "",
		"run_id":              "",
		"scope_instance_id":   "13d83326-f18d-49ed-942d-29c5f291a305",
		"scope_name":          "",
		"shim_listen_address": "127.0.0.1:38625",
		"token_ref":           "/tmp/runs/58f12fe6/remote-tokens/13d83326/noop.token",
	})
	env := &v1.Envelope{
		SchemaVersion: 1,
		RunId:         "58f12fe6-5144-4438-8e99-13f701be454c",
		Seq:           1,
		Payload: &v1.Envelope_AdapterEvent{AdapterEvent: &v1.AdapterEvent{
			Adapter: "default",
			Kind:    "adapter.lifecycle.provision_wanted",
			Data:    data,
		}},
	}

	got, ok := lifecycleFromEnvelope(env)
	require.True(t, ok)
	assert.Equal(t, events.EventProvisionWanted, got.Event)
	assert.True(t, got.IsProvisionWanted())
	assert.Equal(t, "default", got.AdapterName)
	assert.Equal(t, "13d83326-f18d-49ed-942d-29c5f291a305", got.ScopeID)
	assert.Equal(t, "58f12fe6-5144-4438-8e99-13f701be454c", got.RunID)
	assert.Equal(t, "127.0.0.1:38625", got.ShimAddress)
	assert.Equal(t, "/tmp/runs/58f12fe6/remote-tokens/13d83326/noop.token", got.TokenFile)
	assert.Equal(t, "", got.Digest, "captured digest is empty and must not be synthesized")
	assert.Equal(t, "default/13d83326-f18d-49ed-942d-29c5f291a305", got.ScopeKey())

	// Parity with the CRI-132 file parser over the verbatim capture.
	parsed, err := events.ParseLifecycleEventsBytes([]byte(capturedNestedProvisionWanted))
	require.NoError(t, err)
	require.Len(t, parsed, 1)
	assert.Equal(t, parsed[0], got, "castle wire conversion must match the file parser on the captured emission")
}

// Verbatim v0.5.22-shaped provision_wanted emission: the pinned engine
// publishes adapter_type (the implementation kind) alongside the workflow's
// adapter node name (the instance, "intake") in payload.data — the shape the
// production castle client actually receives, never the CRI-132 capture which
// predates adapter_type. scope_instance_id is UUID-shaped, as the engine
// emits (uuid.NewString()).
const v0522ProvisionWantedJSON = `{"schema_version":1,"seq":1,"run_id":"CRI-140","payload_type":"AdapterEvent","payload":{"adapter":"intake","kind":"adapter.lifecycle.provision_wanted","data":{"adapter":"intake","adapter_type":"shell","digest":"sha256:d9f306c29f4145da8bcc44187c9e4ae0f69ed30db3b3edac6e9b6350469bc635","scope_instance_id":"9f1d3c2b-6a4e-4f8a-9c1d-3e7b5a2f0d46","shim_listen_address":"[::]:7778","token_ref":"/data/.criteria/runs/cri-140/token"}}}`

// The castle conversion of a v0.5.22-shaped provision_wanted envelope must
// carry the adapter_type field: the per-scope reconciler resolves the pod
// image kind from it, and scope.AdapterType is what the reconciler sees. A
// conversion that drops it silently wedges per-scope pods in ImagePull again.
func TestLifecycleFromEnvelopeResolvesAdapterType(t *testing.T) {
	data := mustStruct(t, map[string]any{
		"adapter":             "intake",
		"adapter_type":        "shell",
		"digest":              "sha256:d9f306c29f4145da8bcc44187c9e4ae0f69ed30db3b3edac6e9b6350469bc635",
		"scope_instance_id":   "9f1d3c2b-6a4e-4f8a-9c1d-3e7b5a2f0d46",
		"shim_listen_address": "[::]:7778",
		"token_ref":           "/data/.criteria/runs/cri-140/token",
	})
	env := &v1.Envelope{
		SchemaVersion: 1,
		RunId:         "CRI-140",
		Seq:           1,
		Payload: &v1.Envelope_AdapterEvent{AdapterEvent: &v1.AdapterEvent{
			Adapter: "intake",
			Kind:    "adapter.lifecycle.provision_wanted",
			Data:    data,
		}},
	}

	got, ok := lifecycleFromEnvelope(env)
	require.True(t, ok)
	assert.Equal(t, events.EventProvisionWanted, got.Event)
	assert.True(t, got.IsProvisionWanted())
	assert.Equal(t, "intake", got.AdapterName, "the adapter field is the workflow's adapter node (instance)")
	assert.Equal(t, "shell", got.AdapterType, "adapter_type is the implementation kind and must not be lost on the wire")
	assert.Equal(t, "9f1d3c2b-6a4e-4f8a-9c1d-3e7b5a2f0d46", got.ScopeID)
	assert.Equal(t, "CRI-140", got.RunID)
	assert.Equal(t, "[::]:7778", got.ShimAddress)
	assert.Equal(t, "/data/.criteria/runs/cri-140/token", got.TokenFile)
	assert.Equal(t, "sha256:d9f306c29f4145da8bcc44187c9e4ae0f69ed30db3b3edac6e9b6350469bc635", got.Digest)

	// Parity with the CRI-132 file parser over the equivalent v0.5.22
	// emission; the reconciler only ever consumes castle-derived events, but
	// the two parsers must agree so the fixture-shaped payloads stay honest.
	parsed, err := events.ParseLifecycleEventsBytes([]byte(v0522ProvisionWantedJSON))
	require.NoError(t, err)
	require.Len(t, parsed, 1)
	assert.Equal(t, parsed[0], got, "castle wire conversion must match the file parser on the v0.5.22 emission")
}

// A nested released envelope converts to a release event carrying the
// engine's verbatim adapter name; the client resolves the asymmetry against
// the provisioned adapter for the scope instance.
func TestLifecycleFromEnvelopeReleased(t *testing.T) {
	data := mustStruct(t, map[string]any{
		"adapter":             "noop.default",
		"scope_instance_id":   "13d83326-f18d-49ed-942d-29c5f291a305",
		"shim_listen_address": "127.0.0.1:38625",
	})
	env := &v1.Envelope{
		RunId: "58f12fe6-5144-4438-8e99-13f701be454c",
		Seq:   7,
		Payload: &v1.Envelope_AdapterEvent{AdapterEvent: &v1.AdapterEvent{
			Adapter: "noop.default",
			Kind:    "adapter.lifecycle.released",
			Data:    data,
		}},
	}

	got, ok := lifecycleFromEnvelope(env)
	require.True(t, ok)
	assert.Equal(t, events.EventRelease, got.Event)
	assert.True(t, got.IsRelease())
	assert.Equal(t, "noop.default", got.AdapterName, "conversion carries the verbatim engine adapter")
	assert.Equal(t, "13d83326-f18d-49ed-942d-29c5f291a305", got.ScopeID)
	assert.Equal(t, "58f12fe6-5144-4438-8e99-13f701be454c", got.RunID)
}

// Non-lifecycle payloads and unknown kinds are skipped silently, exactly as
// the CRI-132 file parser skips them.
func TestLifecycleFromEnvelopeSkipsNonLifecyclePayloads(t *testing.T) {
	cases := []*v1.Envelope{
		{RunId: "r1", Payload: &v1.Envelope_RunStarted{RunStarted: &v1.RunStarted{WorkflowName: "x"}}},
		{RunId: "r1", Payload: &v1.Envelope_AdapterEvent{AdapterEvent: &v1.AdapterEvent{Kind: "agent.message"}}},
		{RunId: "r1", Payload: &v1.Envelope_AdapterEvent{AdapterEvent: &v1.AdapterEvent{Kind: "adapter.lifecycle.unknown_kind"}}},
		{RunId: "r1", Payload: &v1.Envelope_AdapterEvent{AdapterEvent: &v1.AdapterEvent{
			Kind: "adapter.lifecycle.provision_wanted",
			Data: mustStruct(t, map[string]any{"scope_instance_id": "s1"}),
		}}},
		{RunId: "r1"},
		nil,
	}
	for _, env := range cases {
		_, ok := lifecycleFromEnvelope(env)
		assert.False(t, ok, "envelope %v must not map to a lifecycle event", env)
	}
}

// Terminal envelopes map to Terminal values with the same semantics the
// operator stamps into CriteriaRun status.
func TestTerminalFromEnvelope(t *testing.T) {
	completed := &v1.Envelope{RunId: "r1", Seq: 9,
		Payload: &v1.Envelope_RunCompleted{RunCompleted: &v1.RunCompleted{FinalState: "done", Success: true}}}
	term := terminalFromEnvelope(completed)
	require.NotNil(t, term)
	assert.True(t, term.Success)
	assert.Equal(t, "done", term.FinalState)

	failed := &v1.Envelope{RunId: "r1", Seq: 10,
		Payload: &v1.Envelope_RunFailed{RunFailed: &v1.RunFailed{Reason: "workflow step build failed", Step: "build"}}}
	term = terminalFromEnvelope(failed)
	require.NotNil(t, term)
	assert.False(t, term.Success)
	assert.Equal(t, "workflow step build failed", term.Reason)

	assert.Nil(t, terminalFromEnvelope(&v1.Envelope{RunId: "r1",
		Payload: &v1.Envelope_RunStarted{RunStarted: &v1.RunStarted{}}}))
	assert.Nil(t, terminalFromEnvelope(nil))
}

// prNumberFromURL parses the trailing pull request number.
func TestPRNumberFromURL(t *testing.T) {
	assert.Equal(t, "42", prNumberFromURL("https://github.com/brokenbots/workflow-example/pull/42"))
	assert.Equal(t, "", prNumberFromURL(""))
	assert.Equal(t, "", prNumberFromURL("https://github.com/brokenbots/workflow-example"))
	assert.Equal(t, "", prNumberFromURL("https://github.com/brokenbots/workflow-example/pull/"))
	assert.Equal(t, "", prNumberFromURL("https://example.com/notanumber/"))
}

// mustStruct builds a structpb.Struct for payload data.
func mustStruct(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(m)
	require.NoError(t, err)
	return s
}
