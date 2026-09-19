package castle

import (
	"testing"

	v1 "github.com/brokenbots/workflow-example/criteria-k8s/internal/criteria/pb/criteria/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// CRI-234: the castle wire must carry the provision event's config-declared
// environment identity (CRI-233, runner fc95449) — the per-scope reconcile's
// co-location grouping key — and keep it empty for pre-fc95449 events so the
// reconcile falls back to per-adapter pods.

func TestLifecycleFromEnvelopeCarriesEnvironment(t *testing.T) {
	data := mustStruct(t, map[string]any{
		"adapter":           "intake",
		"adapter_type":      "shell",
		"scope_instance_id": "9f1d3c2b-6a4e-4f8a-9c1d-3e7b5a2f0d46",
		"environment":       "ci",
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
	assert.Equal(t, "ci", got.Environment, "the canonical environment key is carried on the wire")
}

func TestLifecycleFromEnvelopeCarriesEnvironmentIDAlias(t *testing.T) {
	data := mustStruct(t, map[string]any{
		"adapter":           "intake",
		"adapter_type":      "shell",
		"scope_instance_id": "9f1d3c2b-6a4e-4f8a-9c1d-3e7b5a2f0d46",
		"environment_id":    "prod",
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
	assert.Equal(t, "prod", got.Environment, "the environment_id spelling is coalesced into Environment")
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
	assert.Empty(t, got.Environment, "pre-fc95449 events keep the empty fallback key")

	// Parity with the file parser on the env-less shape: both must agree
	// that Environment is empty so the reconcile picks the same fallback.
	parsed, err := events.ParseLifecycleEventsBytes([]byte(v0522ProvisionWantedJSON))
	require.NoError(t, err)
	require.Len(t, parsed, 1)
	assert.Equal(t, parsed[0].Environment, got.Environment)
}
