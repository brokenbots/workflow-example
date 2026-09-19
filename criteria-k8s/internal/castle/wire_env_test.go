package castle

import (
	"testing"

	v1 "github.com/brokenbots/workflow-example/criteria-k8s/internal/criteria/pb/criteria/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// CRI-234: the castle wire must carry the provision event's environment
// identity (CRI-233, runner fc95449) — the environment_type /
// environment_name pair the pinned engine actually emits, from which the
// per-scope reconcile derives its co-location grouping key — and keep it
// empty for pre-fc95449 events so the reconcile falls back to per-adapter
// pods.

// Verbatim fc95449-shaped provision_wanted emission (CRI-233): the pinned
// engine publishes the compiled environment declaration's type and name in
// payload.data (internal/run/sink.go) — the pair the per-(scope,environment)
// co-location groups on. Key order follows the engine's structpb encoding,
// mirroring the v0522ProvisionWantedJSON convention.
const fc95449ProvisionWantedJSON = `{"schema_version":1,"seq":1,"run_id":"CRI-234","payload_type":"AdapterEvent","payload":{"adapter":"intake","kind":"adapter.lifecycle.provision_wanted","data":{"adapter":"intake","adapter_type":"shell","digest":"sha256:d9f306c29f4145da8bcc44187c9e4ae0f69ed30db3b3edac6e9b6350469bc635","environment_name":"worktree","environment_type":"remote","run_id":"","scope_instance_id":"9f1d3c2b-6a4e-4f8a-9c1d-3e7b5a2f0d46","scope_name":"","shim_listen_address":"[::]:7778","token_ref":"/data/.criteria/runs/cri-234/token"}}}`

// A fc95449-shaped provision_wanted envelope must carry the environment
// pair: the per-scope reconciler groups on the derived "type/name"
// identity, and a conversion that drops it silently disables the
// co-location feature end to end.
func TestLifecycleFromEnvelopeCarriesEnvironmentPair(t *testing.T) {
	data := mustStruct(t, map[string]any{
		"adapter":             "intake",
		"adapter_type":        "shell",
		"digest":              "sha256:d9f306c29f4145da8bcc44187c9e4ae0f69ed30db3b3edac6e9b6350469bc635",
		"environment_name":    "worktree",
		"environment_type":    "remote",
		"run_id":              "",
		"scope_instance_id":   "9f1d3c2b-6a4e-4f8a-9c1d-3e7b5a2f0d46",
		"scope_name":          "",
		"shim_listen_address": "[::]:7778",
		"token_ref":           "/data/.criteria/runs/cri-234/token",
	})
	env := &v1.Envelope{
		SchemaVersion: 1,
		RunId:         "CRI-234",
		Seq:           1,
		Payload: &v1.Envelope_AdapterEvent{AdapterEvent: &v1.AdapterEvent{
			Adapter: "intake",
			Kind:    "adapter.lifecycle.provision_wanted",
			Data:    data,
		}},
	}

	got, ok := lifecycleFromEnvelope(env)
	require.True(t, ok)
	assert.Equal(t, "remote", got.EnvironmentType, "the real emission key must not be lost on the wire")
	assert.Equal(t, "worktree", got.EnvironmentName, "the real emission key must not be lost on the wire")
	assert.Equal(t, "remote/worktree", got.Environment, "the (type,name) pair forms the co-location identity")

	// Parity with the file parser over the verbatim fc95449 emission; both
	// parsers must derive the identical event so the fixture-shaped
	// payloads stay honest.
	parsed, err := events.ParseLifecycleEventsBytes([]byte(fc95449ProvisionWantedJSON))
	require.NoError(t, err)
	require.Len(t, parsed, 1)
	assert.Equal(t, parsed[0], got, "castle wire conversion must match the file parser on the fc95449 emission")
}

// A second (type, name) pair on the same wire must map to a distinct
// grouping identity: remote/prod never co-locates with remote/worktree.
func TestLifecycleFromEnvelopeDistinctEnvironmentPairs(t *testing.T) {
	build := func(envName string) *v1.Envelope {
		return &v1.Envelope{
			SchemaVersion: 1,
			RunId:         "CRI-234",
			Seq:           1,
			Payload: &v1.Envelope_AdapterEvent{AdapterEvent: &v1.AdapterEvent{
				Adapter: "intake",
				Kind:    "adapter.lifecycle.provision_wanted",
				Data: mustStruct(t, map[string]any{
					"adapter":             "intake",
					"adapter_type":        "shell",
					"environment_name":    envName,
					"environment_type":    "remote",
					"scope_instance_id":   "9f1d3c2b-6a4e-4f8a-9c1d-3e7b5a2f0d46",
					"shim_listen_address": "[::]:7778",
					"token_ref":           "/data/.criteria/runs/cri-234/token",
				}),
			}},
		}
	}

	worktree, ok := lifecycleFromEnvelope(build("worktree"))
	require.True(t, ok)
	prod, ok := lifecycleFromEnvelope(build("prod"))
	require.True(t, ok)
	assert.Equal(t, "remote/worktree", worktree.Environment)
	assert.Equal(t, "remote/prod", prod.Environment)
	assert.NotEqual(t, worktree.Environment, prod.Environment,
		"different (type,name) pairs must never collapse into one co-location group")
}

func TestLifecycleFromEnvelopeWithoutEnvironmentStaysEmpty(t *testing.T) {
	// Mirrors the CRI-132 captured emission shape: pre-fc95449 events carry
	// no environment identity at all.
	data := mustStruct(t, map[string]any{
		"adapter":             "default",
		"scope_instance_id":   "13d83326-f18d-49ed-942d-29c5f291a305",
		"shim_listen_address": "127.0.0.1:38625",
		"token_ref":           "/tmp/runs/58f12fe6/remote-tokens/13d83326-f18d-49ed-942d-29c5f291a305/noop.token",
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
	assert.Empty(t, got.Environment, "pre-fc95449 events keep the empty fallback identity")
	assert.Empty(t, got.EnvironmentType)
	assert.Empty(t, got.EnvironmentName)

	// Parity with the file parser on the env-less shape: both must agree
	// that Environment is empty so the reconcile picks the same fallback.
	parsed, err := events.ParseLifecycleEventsBytes([]byte(v0522ProvisionWantedJSON))
	require.NoError(t, err)
	require.Len(t, parsed, 1)
	assert.Equal(t, parsed[0].Environment, got.Environment)
}

// CRI-237: the castle wire must carry the provision event's accept token
// (CRI-236, runner eae0181) so the operator can deliver it to the adapter
// pod's shim channel directly. token_ref stays parsed as the legacy
// fallback identity.
func TestLifecycleFromEnvelopeCarriesAcceptToken(t *testing.T) {
	data := mustStruct(t, map[string]any{
		"adapter":             "intake",
		"adapter_type":        "shell",
		"digest":              "sha256:d9f306c29f4145da8bcc44187c9e4ae0f69ed30db3b3edac6e9b6350469bc635",
		"run_id":              "",
		"scope_instance_id":   "9f1d3c2b-6a4e-4f8a-9c1d-3e7b5a2f0d46",
		"scope_name":          "",
		"shim_listen_address": "[::]:7778",
		"token_ref":           "/data/.criteria/runs/cri-237/token",
		"accept_token":        "accept-rotate-1",
	})
	env := &v1.Envelope{
		SchemaVersion: 1,
		RunId:         "CRI-237",
		Seq:           1,
		Payload: &v1.Envelope_AdapterEvent{AdapterEvent: &v1.AdapterEvent{
			Adapter: "intake",
			Kind:    "adapter.lifecycle.provision_wanted",
			Data:    data,
		}},
	}

	got, ok := lifecycleFromEnvelope(env)
	require.True(t, ok)
	assert.Equal(t, "accept-rotate-1", got.AcceptToken,
		"the real eae0181 emission key must not be lost on the wire")
	assert.Equal(t, "/data/.criteria/runs/cri-237/token", got.TokenFile,
		"token_ref stays the parsed legacy fallback identity")

	// Parity with the file parser: both parsers must derive the identical
	// event from an accept_token-bearing emission.
	parsed, err := events.ParseLifecycleEventsBytes([]byte(`{"payload_type":"AdapterEvent","run_id":"CRI-237","payload":{"kind":"adapter.lifecycle.provision_wanted","data":{"adapter":"intake","adapter_type":"shell","digest":"sha256:d9f306c29f4145da8bcc44187c9e4ae0f69ed30db3b3edac6e9b6350469bc635","run_id":"","scope_instance_id":"9f1d3c2b-6a4e-4f8a-9c1d-3e7b5a2f0d46","scope_name":"","shim_listen_address":"[::]:7778","token_ref":"/data/.criteria/runs/cri-237/token","accept_token":"accept-rotate-1"}}}`))
	require.NoError(t, err)
	require.Len(t, parsed, 1)
	assert.Equal(t, parsed[0], got, "castle wire conversion must match the file parser on the eae0181 emission")
}
