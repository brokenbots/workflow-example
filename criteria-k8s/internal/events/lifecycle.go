package events

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// Lifecycle event kinds emitted by the engine for per-scope adapter
// provisioning.
const (
	EventProvisionWanted = "provision_wanted"
	EventRelease         = "release"
)

// Nested envelope shapes. The engine wraps adapter lifecycle events in an
// AdapterEvent payload envelope; the lifecycle kind is carried in
// payload.kind and the event content in payload.data. Top-level run_id on the
// envelope is authoritative (payload.data.run_id is empty at the emission
// site).
const (
	payloadTypeAdapterEvent    = "AdapterEvent"
	payloadKindProvisionWanted = "adapter.lifecycle.provision_wanted"
)

// payloadEnvelope is the probe for the nested engine envelope shape.
type payloadEnvelope struct {
	PayloadType string          `json:"payload_type"`
	RunID       string          `json:"run_id"`
	Payload     *payloadMessage `json:"payload"`
}

// payloadMessage is the AdapterEvent payload: kind plus event content.
type payloadMessage struct {
	Kind string           `json:"kind"`
	Data payloadEventData `json:"data"`
}

// payloadEventData carries the event content for a nested lifecycle event.
type payloadEventData struct {
	Adapter     string `json:"adapter"`
	AdapterType string `json:"adapter_type"`
	Digest      string `json:"digest"`
	ScopeID     string `json:"scope_instance_id"`
	ShimAddress string `json:"shim_listen_address"`
	// TokenRef is the engine-rotated accept-token file path (legacy
	// delivery channel). Runner commits from eae0181 (CRI-236) onward also
	// carry the raw accept_token; older engines deliver only via files.
	TokenFile string `json:"token_ref"`
	// ImageReference is the pinned adapter image reference resolved from
	// the workflow lockfile (CRI-214 M14, runner commit 4079d836), carried
	// verbatim on provision_wanted payload.data. Empty on events from
	// older engines; the operator then resolves from its configured
	// registry/tag defaults.
	ImageReference string `json:"image_reference"`
	// AcceptToken is the engine-minted per-scope accept token (CRI-236,
	// runner commit eae0181), delivered in provision payload.data. The
	// operator forwards it to the adapter pod's shim channel directly, so
	// no token file has to be readable from the shared volume. Empty on
	// events from older engines, which keep the token_ref file delivery.
	AcceptToken string `json:"accept_token"`
	// Environment identity (CRI-233, runner fc95449): the compiled
	// environment declaration's type and name, carried verbatim by the
	// engine's payload.data (internal/run/sink.go) in every wire shape.
	// Engines older than fc95449 carry neither key.
	EnvironmentType string `json:"environment_type"`
	EnvironmentName string `json:"environment_name"`
}

// LifecycleEvent describes a provision-wanted or release event in the run
// event stream. The engine emits these at subworkflow scope entry and exit.
type LifecycleEvent struct {
	// Event is one of "provision_wanted" or "release".
	Event string `json:"event"`

	// RunID identifies the CriteriaRun the event belongs to.
	RunID string `json:"run_id"`

	// ScopeID is a unique identifier for the scope instance (root or a
	// named subworkflow + UUID).
	ScopeID string `json:"scope_id"`

	// ScopeTag is a short human-readable scope tag used in the handshake.
	ScopeTag string `json:"scope_tag,omitempty"`

	// AdapterName is the workflow's adapter node name (e.g. "intake").
	AdapterName string `json:"adapter_name"`

	// AdapterType is the adapter implementation kind (shell, copilot, ...).
	// Empty on events from engines that predate its emission; fall back to
	// AdapterName then.
	AdapterType string `json:"adapter_type,omitempty"`

	// Digest is the pinned lockfile digest for the adapter image, including
	// the "sha256:" prefix.
	Digest string `json:"digest,omitempty"`

	// ImageReference is the adapter image reference the workflow lockfile
	// pinned, carried verbatim on provision_wanted payload.data (CRI-214
	// M14, runner commit 4079d836). The jobbuilder prefers it for adapter
	// pods once the event's digest is present, so only digest-verified
	// references are consumed. Empty on events from older engines, which
	// fall back to the operator's configured registry/tag defaults.
	ImageReference string `json:"image_reference,omitempty"`

	// ShimAddress is the runner dial address the adapter should phone home to.
	ShimAddress string `json:"shim_address,omitempty"`

	// TokenFile is an absolute path under /data to the per-scope bearer
	// token. Only present on provision-wanted events. Legacy delivery
	// channel: engines before eae0181 deliver the token only through this
	// file; from eae0181 (CRI-236) the token also rides the event in
	// AcceptToken and the operator prefers the wire copy.
	TokenFile string `json:"token_file,omitempty"`

	// AcceptToken is the engine-minted per-scope accept token carried on
	// provision-wanted events (CRI-236, runner commit eae0181). When set,
	// the operator delivers it to the adapter pod over the shim channel
	// (pod-spec env) instead of exposing a token file path. Empty on
	// pre-eae0181 engine events, where TokenFile remains the delivery
	// surface.
	AcceptToken string `json:"accept_token,omitempty"`

	// EnvironmentType is the compiled environment declaration's type (e.g.
	// "remote"), and EnvironmentName its declaration name (e.g. "prod").
	// The runner's adapter lifecycle events carry both verbatim (CRI-233,
	// runner commit fc95449); events from older engines carry neither.
	EnvironmentType string `json:"environment_type,omitempty"`
	EnvironmentName string `json:"environment_name,omitempty"`

	// Environment is the co-location grouping identity (CRI-234): the
	// "type/name" pair derived from EnvironmentType and EnvironmentName,
	// so remote/prod and remote/worktree stay distinct groups. Adapters
	// sharing one environment run as separate containers in one (scope,
	// environment) pod (CRI-234). Empty when the event carries neither
	// field (pre-fc95449 engines), where the reconcile falls back to
	// per-adapter pods. Derived by the parsers; not a wire key.
	Environment string `json:"-"`

	// Timestamp is an optional RFC3339 event timestamp.
	Timestamp string `json:"timestamp,omitempty"`
}

// IsProvisionWanted returns true for a provision-wanted event.
func (e LifecycleEvent) IsProvisionWanted() bool {
	return e.Event == EventProvisionWanted
}

// IsRelease returns true for a release event.
func (e LifecycleEvent) IsRelease() bool {
	return e.Event == EventRelease
}

// ScopeKey returns a stable key combining the adapter kind and scope id.
func (e LifecycleEvent) ScopeKey() string {
	return e.AdapterName + "/" + e.ScopeID
}

// EnvironmentIdentity joins the compiled environment declaration's type and
// name (CRI-233, runner fc95449) into the per-scope co-location grouping key
// (CRI-234): remote/prod and remote/worktree are distinct groups. The
// identity is empty only when the event carries neither field — engines
// older than fc95449 — which the reconcile maps to the per-adapter pod
// fallback.
func EnvironmentIdentity(envType, envName string) string {
	if envType == "" && envName == "" {
		return ""
	}
	return envType + "/" + envName
}

// ParseLifecycleEvents scans an ndjson event stream and returns all lifecycle
// events in stream order. Non-JSON lines and unrelated events are ignored.
// Both the flat shape (top-level event/adapter_name keys) and the engine's
// nested AdapterEvent envelope are recognized.
func ParseLifecycleEvents(r io.Reader) ([]LifecycleEvent, error) {
	var events []LifecycleEvent
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var envelope payloadEnvelope
		if err := json.Unmarshal(line, &envelope); err == nil &&
			envelope.PayloadType == payloadTypeAdapterEvent && envelope.Payload != nil {
			// Nested envelope shape: interpret from payload.data, never
			// from top-level keys.
			if ev, ok := lifecycleEventFromPayload(envelope); ok {
				events = append(events, ev)
			}
			continue
		}
		var ev LifecycleEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		if ev.Event != EventProvisionWanted && ev.Event != EventRelease {
			continue
		}
		if ev.AdapterName == "" {
			continue
		}
		// The identity is derived from the environment declaration's
		// type/name pair (CRI-233, runner fc95449) — never from a
		// hand-provided key. Both keys empty (pre-fc95449 engines) keeps
		// the per-adapter pod fallback.
		ev.Environment = EnvironmentIdentity(ev.EnvironmentType, ev.EnvironmentName)
		events = append(events, ev)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scanning lifecycle events: %w", err)
	}
	return events, nil
}

// lifecycleEventFromPayload maps a nested AdapterEvent envelope to a
// LifecycleEvent. Only provision_wanted is recognized; every other kind —
// including released — is skipped silently (release handling is separately
// scheduled). Events with an empty adapter name are skipped, as with flat
// events. Unknown fields are ignored.
func lifecycleEventFromPayload(envelope payloadEnvelope) (LifecycleEvent, bool) {
	if envelope.Payload.Kind != payloadKindProvisionWanted {
		return LifecycleEvent{}, false
	}
	ev := LifecycleEvent{
		Event:           EventProvisionWanted,
		RunID:           envelope.RunID,
		ScopeID:         envelope.Payload.Data.ScopeID,
		AdapterName:     envelope.Payload.Data.Adapter,
		AdapterType:     envelope.Payload.Data.AdapterType,
		Digest:          envelope.Payload.Data.Digest,
		ImageReference:  envelope.Payload.Data.ImageReference,
		ShimAddress:     envelope.Payload.Data.ShimAddress,
		TokenFile:       envelope.Payload.Data.TokenFile,
		AcceptToken:     envelope.Payload.Data.AcceptToken,
		EnvironmentType: envelope.Payload.Data.EnvironmentType,
		EnvironmentName: envelope.Payload.Data.EnvironmentName,
	}
	ev.Environment = EnvironmentIdentity(ev.EnvironmentType, ev.EnvironmentName)
	if ev.AdapterName == "" {
		return LifecycleEvent{}, false
	}
	return ev, true
}

// ParseLifecycleEventsBytes is a convenience wrapper around ParseLifecycleEvents.
func ParseLifecycleEventsBytes(data []byte) ([]LifecycleEvent, error) {
	return ParseLifecycleEvents(bytes.NewReader(data))
}

// ActiveProvisions returns the provision-wanted events that have not been
// released by a later release event for the same adapter/scope pair. The
// result is re-derivable from the full event stream and therefore idempotent.
func ActiveProvisions(events []LifecycleEvent) []LifecycleEvent {
	active := make(map[string]LifecycleEvent)
	for _, ev := range events {
		key := ev.ScopeKey()
		switch ev.Event {
		case EventProvisionWanted:
			active[key] = ev
		case EventRelease:
			delete(active, key)
		}
	}
	out := make([]LifecycleEvent, 0, len(active))
	for _, ev := range active {
		out = append(out, ev)
	}
	return out
}
