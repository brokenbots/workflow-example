// Package control: assignment payload contract for the kanboard dispatch
// lane (KB-338 P0.2/P0.3).
//
// Castle's WorkflowAssignment carries three identity fields (workflow_name,
// workflow_source, lockfile_source) and a label map. The label map is a
// REQUIRED-match surface: castle leases an assignment to an agent only when
// every assignment label matches one of the agent's registered labels
// (labelsSatisfy — every required key=value must be present on the agent),
// so per-ticket values can never ride labels without unsatisfiable agents.
//
// The dispatch lane therefore splits the assignment into two channels:
//
//   - routing labels — ONLY operator-saturable values (today: the
//     criteria.brokenbots.dev/project label) — scoping assignments to the
//     project's operator agent. Empty labels match every agent.
//   - the payload — ticket, repo, config slot, and target pins — rides
//     workflow_source as a DispatchEnvelope. workflow_source is an opaque
//     string field to castle: it stores and redelivers it verbatim, and the
//     executing agent is the reader. A real HCL content lane (the
//     castle-side data model) supersedes this envelope when the proto grows
//     payload fields; until then the envelope is the defined wire format
//     both this repo's adapter (writer) and operator agent (reader) share
//     by construction.
package control

import (
	"encoding/json"
	"errors"
	"fmt"
)

// DispatchEnvelopeKind is the envelope's type discriminator. A
// workflow_source that does not parse into this kind is rejected
// fail-closed: the operator never stamps a run from an assignment it
// cannot attribute.
const DispatchEnvelopeKind = "criteria-workflow-assignment/1"

// DispatchEnvelope is the per-ticket payload an assignment carries in
// workflow_source. JSON by design: the reader is a k8s operator, the
// writer a Go watcher, and json round-trips without a custom grammar.
type DispatchEnvelope struct {
	// Kind is the discriminator (DispatchEnvelopeKind). Unknown kinds are
	// rejected.
	Kind string `json:"kind"`
	// Ticket is the intake ticket id (KB-<n>).
	Ticket string `json:"ticket"`
	// Repo is the repository the run clones and works on (the ticket's
	// repo URL).
	Repo string `json:"repo"`
	// Class is the stamped workflow class (the watcher's
	// criteria.brokenbots.dev/class label value). It rides the envelope
	// rather than assignment labels because castle's lease gate requires
	// the agent to hold EVERY assignment label — routing labels stay
	// operator-saturable (project) and per-ticket vocabulary stays out.
	Class string `json:"class,omitempty"`
	// Slot optionally names the routes configurationLibrary entry the run
	// executes under (KB-103 per-repo config). Empty keeps the workflow's
	// default resolution.
	Slot string `json:"slot,omitempty"`
	// TargetImages optionally carries per-card target_metadata: adapter
	// kind -> container image reference pinned into the run's
	// spec.workflow.adapterImages (the metadata lane of the KB-319
	// resolution chain: target_metadata > digest-verified event > binding
	// > defaults, conflicts gated by the stamped ImageConflictPolicy).
	TargetImages map[string]string `json:"targetImages,omitempty"`
}

// MarshalDispatchEnvelope serializes the payload envelope for submission in
// workflow_source. Validated strictly: the adapter fail-closes before
// submitting an envelope castle would queue for an assignment the operator
// cannot attribute.
func MarshalDispatchEnvelope(e DispatchEnvelope) (string, error) {
	if e.Kind == "" {
		e.Kind = DispatchEnvelopeKind
	}
	if err := e.validate(); err != nil {
		return "", err
	}
	data, err := json.Marshal(e)
	if err != nil {
		return "", fmt.Errorf("marshal dispatch envelope: %w", err)
	}
	return string(data), nil
}

// ParseDispatchEnvelope decodes one assignment's workflow_source. Unknown
// kinds, malformed JSON, and missing identity fields all fail closed.
func ParseDispatchEnvelope(source string) (DispatchEnvelope, error) {
	var e DispatchEnvelope
	if source == "" {
		return e, errors.New("dispatch envelope: workflow_source empty")
	}
	if err := json.Unmarshal([]byte(source), &e); err != nil {
		return e, fmt.Errorf("dispatch envelope: workflow_source is not a dispatch payload: %w", err)
	}
	if err := e.validate(); err != nil {
		return e, err
	}
	return e, nil
}

func (e DispatchEnvelope) validate() error {
	if e.Kind != DispatchEnvelopeKind {
		return fmt.Errorf("dispatch envelope: unknown kind %q (want %q)", e.Kind, DispatchEnvelopeKind)
	}
	if e.Ticket == "" {
		return errors.New("dispatch envelope: ticket empty")
	}
	if e.Repo == "" {
		return errors.New("dispatch envelope: repo empty")
	}
	for kind, ref := range e.TargetImages {
		if kind == "" {
			// JSON objects cannot carry empty keys natively; this is the
			// belt for a hand-constructed payload.
			return errors.New("dispatch envelope: target image with empty adapter kind")
		}
		if ref == "" {
			return fmt.Errorf("dispatch envelope: target image %q empty reference", kind)
		}
	}
	return nil
}