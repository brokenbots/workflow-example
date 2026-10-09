package events

import (
	"encoding/json"
	"fmt"
	"time"
)

// PodStateObservedKind is the AdapterEvent kind the operator emits when a
// per-scope adapter pod's observed state changes (KB-225). It rides the same
// adapter-event channel as adapter.lifecycle.provision_wanted (the run's
// castle event stream, Envelope.AdapterEvent), so the runner consumes the
// pod phase through the established AdapterEvent shape rather than a new
// side-channel.
const PodStateObservedKind = "adapter.podstate.observed"

// Pod phases carried in the PodStateReport.Phase field. These mirror the
// Kubernetes PodStatus lifecycle the operator observes per-scope adapter
// pods in. The consumer's remote-adapter session wait treats Running as the
// handshake-budget boundary and Succeeded/Failed as terminal, matching the
// KB-70 PodStateProbe vocabulary.
const (
	PodPhasePending   = "Pending"
	PodPhaseRunning   = "Running"
	PodPhaseSucceeded = "Succeeded"
	PodPhaseFailed    = "Failed"
	PodPhaseUnknown   = "Unknown"
)

// podStateEnvelope is the wire shape of one emitted pod-state event: the
// nested AdapterEvent envelope the engine and castle already use for adapter
// lifecycle events, serialized as protojson-style snake_case JSON with the
// event content in payload.data. One shape at every hop: the operator builds
// the typed Envelope proto for submission and this struct pins the stored
// line shape; payload.data carries flat scalar keys only (no nested JSON
// strings).
type podStateEnvelope struct {
	SchemaVersion int             `json:"schema_version"`
	Seq           uint64          `json:"seq"`
	RunID         string          `json:"run_id"`
	PayloadType   string          `json:"payload_type"`
	Payload       podStatePayload `json:"payload"`
}

type podStatePayload struct {
	Adapter string            `json:"adapter"`
	Kind    string            `json:"kind"`
	Data    podStateEventData `json:"data"`
}

// podStateEventData is the event content: exactly these flat keys and
// nothing else. Key order is part of the pinned shape (declaration order).
// Every key is emitted on every event, including empty strings, so the shape
// is deterministic regardless of the observed state.
type podStateEventData struct {
	// Adapter is the workflow's adapter node name (e.g. "intake"), matching
	// the provision_wanted envelope's adapter identity.
	Adapter string `json:"adapter"`
	// AdapterType is the adapter implementation kind (shell, copilot, ...)
	// — the first PodStateProbe key at the consumer.
	AdapterType string `json:"adapter_type"`
	// ScopeInstanceID + ScopeName are the scope identity the adapter
	// presents in its handshake ("<scope_name>/<scope_instance_id>",
	// scope_name empty for the root scope) — the second PodStateProbe key.
	ScopeInstanceID string `json:"scope_instance_id"`
	ScopeName       string `json:"scope_name"`
	// Pod is the Kubernetes pod name the state was observed on (peer pods
	// host every adapter kind of a (scope, environment) group, so the pod
	// name is diagnostics, not identity).
	Pod string `json:"pod"`
	// Phase is the observed Kubernetes pod phase (PodPhase* constants);
	// Reason is the wait reason (e.g. Unschedulable, ImagePullBackOff) and
	// Message its human-readable message, both empty when the pod is
	// running without waits.
	Phase   string `json:"phase"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
	// ObservedAt is the RFC3339 operator observation timestamp (UTC).
	ObservedAt string `json:"observed_at"`
}

// PodStateReport is the operator-observed state of one per-scope adapter
// pod, as emitted to the runner through the adapter-event channel (KB-225).
type PodStateReport struct {
	// AdapterName is the workflow's adapter node name; AdapterType the
	// adapter implementation kind (shell, copilot, ...).
	AdapterName string
	AdapterType string
	// ScopeName (empty for the root scope) and ScopeID identify the scope
	// session the pod provisions; both together are the consumer's session
	// key alongside AdapterType.
	ScopeName string
	ScopeID   string
	// Pod is the observed pod's name.
	Pod string
	// Phase/Reason/Message mirror the pod's Kubernetes status: Phase is a
	// PodPhase* constant, Reason the wait reason (pod scheduling condition
	// or container wait reason) and Message its message. Empty Reason
	// accompanies a Running pod.
	Phase   string
	Reason  string
	Message string
	// ObservedAt is the observation timestamp (UTC).
	ObservedAt time.Time
}

// ScopeKey returns the dedupe key the feed emits on change: adapter kind and
// scope identity, matching the session-key pair the consumer probes.
func (r PodStateReport) ScopeKey() string {
	return r.AdapterType + "/" + r.ScopeName + "/" + r.ScopeID
}

// PodStateSignal returns the emitted-state key the emitter dedupes on:
// phase and reason only. The message is diagnostics garnish that may change
// between reconciles (e.g. scheduler-fit wording), so it rides the event of
// the phase/reason transition without re-emitting on its own refresh.
func (r PodStateReport) PodStateSignal() string {
	return r.Phase + "|" + r.Reason
}

// Validate reports whether the report carries the identity the consumer
// needs to join it against a pending session wait.
func (r PodStateReport) Validate() error {
	if r.AdapterType == "" {
		return fmt.Errorf("pod-state report: empty adapter_type")
	}
	if r.ScopeID == "" {
		return fmt.Errorf("pod-state report: empty scope_instance_id")
	}
	if r.Phase == "" {
		return fmt.Errorf("pod-state report: empty phase")
	}
	return nil
}

// MarshalPodStateEvent renders the report as the pinned adapter-event JSON
// line: the nested AdapterEvent envelope shape, snake_case keys, flat data
// keys, empty strings included. Seq is 0 (castle stamps the real sequence
// before persistence and fan-out, as with agent submissions).
func MarshalPodStateEvent(runID string, report PodStateReport) ([]byte, error) {
	if err := report.Validate(); err != nil {
		return nil, err
	}
	if runID == "" {
		return nil, fmt.Errorf("pod-state report: empty run_id")
	}
	line := podStateEnvelope{
		SchemaVersion: 1,
		Seq:           0,
		RunID:         runID,
		PayloadType:   payloadTypeAdapterEvent,
		Payload: podStatePayload{
			Adapter: report.AdapterName,
			Kind:    PodStateObservedKind,
			Data: podStateEventData{
				Adapter:         report.AdapterName,
				AdapterType:     report.AdapterType,
				ScopeInstanceID: report.ScopeID,
				ScopeName:       report.ScopeName,
				Pod:             report.Pod,
				Phase:           report.Phase,
				Reason:          report.Reason,
				Message:         report.Message,
				ObservedAt:      report.ObservedAt.UTC().Format(time.RFC3339),
			},
		},
	}
	out, err := json.Marshal(line)
	if err != nil {
		return nil, fmt.Errorf("marshaling pod-state event: %w", err)
	}
	return out, nil
}
