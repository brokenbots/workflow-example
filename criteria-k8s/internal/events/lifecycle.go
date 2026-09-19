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
	TokenFile   string `json:"token_ref"`
	// Environment identity (CRI-233, runner fc95449): the config-declared
	// environment the adapter runs in. The canonical key is "environment";
	// "environment_id" is the engine's alternate spelling.
	Environment   string `json:"environment"`
	EnvironmentID string `json:"environment_id"`
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

	// ShimAddress is the runner dial address the adapter should phone home to.
	ShimAddress string `json:"shim_address,omitempty"`

	// TokenFile is an absolute path under /data to the per-scope bearer token.
	// Only present on provision-wanted events.
	TokenFile string `json:"token_file,omitempty"`

	// Environment is the config-declared environment identity the adapter
	// runs in (CRI-233, runner commit fc95449). It is the co-location
	// grouping key: adapters sharing one environment run as separate
	// containers in one (scope, environment) pod (CRI-234). Empty on events
	// from older engines, where the reconcile falls back to per-adapter
	// pods.
	Environment string `json:"environment,omitempty"`

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
		if ev.Environment == "" {
			// The flat shape spells the identity "environment_id" on some
			// engine versions; coalesce it into the canonical field.
			var probe flatEnvIdentity
			if err := json.Unmarshal(line, &probe); err == nil {
				ev.Environment = probe.EnvironmentID
			}
		}
		events = append(events, ev)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scanning lifecycle events: %w", err)
	}
	return events, nil
}

// flatEnvIdentity probes the flat event shape's alternate environment
// identity key ("environment_id"); the canonical "environment" key is
// carried by LifecycleEvent's own tag.
type flatEnvIdentity struct {
	EnvironmentID string `json:"environment_id"`
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
		Event:       EventProvisionWanted,
		RunID:       envelope.RunID,
		ScopeID:     envelope.Payload.Data.ScopeID,
		AdapterName: envelope.Payload.Data.Adapter,
		AdapterType: envelope.Payload.Data.AdapterType,
		Digest:      envelope.Payload.Data.Digest,
		ShimAddress: envelope.Payload.Data.ShimAddress,
		TokenFile:   envelope.Payload.Data.TokenFile,
		Environment: firstNonEmpty(envelope.Payload.Data.Environment, envelope.Payload.Data.EnvironmentID),
	}
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
