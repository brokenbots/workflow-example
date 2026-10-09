package events

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// podStateFixture is a captured-shape fixture of the line the operator
// emits for one per-scope adapter pod observation, in the exact nested
// AdapterEvent envelope shape stored on the run's castle event stream
// (seq is 0 pre-castle-stamp). The fixture is intentionally written as a
// byte-for-byte literal: consumers of this channel must parse THIS shape,
// not a hand-modeled one.
const podStateFixture = `{"schema_version":1,"seq":0,"run_id":"019a2f3e-4c5b-7cc1-a1de-32f4a5b6c7d8","payload_type":"AdapterEvent","payload":{"adapter":"intake","kind":"adapter.podstate.observed","data":{"adapter":"intake","adapter_type":"shell","scope_instance_id":"dev-001","scope_name":"develop","pod":"shell-develop-dev-001-adapter","phase":"Pending","reason":"Unschedulable","message":"0/4 nodes are available: 2 node(s) were not ready, 2 Insufficient cpu.","observed_at":"2026-10-07T12:34:56Z"}}}`

func TestMarshalPodStateEventMatchesFixtureShape(t *testing.T) {
	report := PodStateReport{
		AdapterName: "intake",
		AdapterType: "shell",
		ScopeName:   "develop",
		ScopeID:     "dev-001",
		Pod:         "shell-develop-dev-001-adapter",
		Phase:       PodPhasePending,
		Reason:      "Unschedulable",
		Message:     "0/4 nodes are available: 2 node(s) were not ready, 2 Insufficient cpu.",
		ObservedAt:  time.Date(2026, 10, 7, 12, 34, 56, 0, time.UTC),
	}
	got, err := MarshalPodStateEvent("019a2f3e-4c5b-7cc1-a1de-32f4a5b6c7d8", report)
	if err != nil {
		t.Fatalf("MarshalPodStateEvent: %v", err)
	}
	if string(got) != podStateFixture {
		t.Fatalf("pod-state event shape drifted from pinned fixture:\n got: %s\nwant: %s", got, podStateFixture)
	}
}

func TestMarshalPodStateEventKeySetNeverVaries(t *testing.T) {
	// A running pod with no wait reason must emit the same keys as the
	// fixture's pending-with-reason form: empty strings, not omitted keys.
	report := PodStateReport{
		AdapterName: "intake",
		AdapterType: "copilot",
		ScopeID:     "dev-002",
		Pod:         "copilot-dev-002-adapter",
		Phase:       PodPhaseRunning,
		ObservedAt:  time.Date(2026, 10, 7, 12, 35, 0, 0, time.UTC),
	}
	got, err := MarshalPodStateEvent("run-2", report)
	if err != nil {
		t.Fatalf("MarshalPodStateEvent: %v", err)
	}

	var outer map[string]any
	if err := json.Unmarshal(got, &outer); err != nil {
		t.Fatalf("outer unmarshal: %v", err)
	}
	wantOuter := map[string]bool{"schema_version": true, "seq": true, "run_id": true, "payload_type": true, "payload": true}
	if len(outer) != len(wantOuter) {
		t.Fatalf("outer keys = %v, want exactly %v", outer, wantOuter)
	}
	if outer["payload_type"] != "AdapterEvent" {
		t.Fatalf("payload_type = %v, want AdapterEvent", outer["payload_type"])
	}

	payload, ok := outer["payload"].(map[string]any)
	if !ok {
		t.Fatalf("payload missing or not an object: %v", outer["payload"])
	}
	wantPayload := map[string]bool{"adapter": true, "kind": true, "data": true}
	if len(payload) != len(wantPayload) {
		t.Fatalf("payload keys = %v, want exactly %v", payload, wantPayload)
	}
	if payload["kind"] != PodStateObservedKind {
		t.Fatalf("kind = %v, want %s", payload["kind"], PodStateObservedKind)
	}

	data, ok := payload["data"].(map[string]any)
	if !ok {
		t.Fatalf("data missing or not an object: %v", payload["data"])
	}
	wantData := map[string]bool{
		"adapter": true, "adapter_type": true, "scope_instance_id": true, "scope_name": true,
		"pod": true, "phase": true, "reason": true, "message": true, "observed_at": true,
	}
	if len(data) != len(wantData) {
		t.Fatalf("data keys = %v, want exactly %v", data, wantData)
	}
	for key, want := range map[string]any{
		"adapter_type": "copilot", "scope_instance_id": "dev-002", "scope_name": "",
		"phase": PodPhaseRunning, "reason": "", "message": "",
	} {
		if data[key] != want {
			t.Fatalf("data[%q] = %v, want %q (empty-string keys must be emitted)", key, data[key], want)
		}
	}

	// Flat scalars only: no value anywhere in data may itself be JSON
	// (no nested JSON strings per the wire-contract rules).
	for key, value := range data {
		str, ok := value.(string)
		if !ok {
			continue
		}
		trimmed := strings.TrimSpace(str)
		if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
			t.Fatalf("data[%q] is a nested JSON string: %q", key, str)
		}
	}
}

func TestMarshalPodStateEventValidation(t *testing.T) {
	base := PodStateReport{
		AdapterName: "intake",
		AdapterType: "shell",
		ScopeID:     "dev-001",
		Phase:       PodPhaseRunning,
	}
	if _, err := MarshalPodStateEvent("", base); err == nil {
		t.Fatal("empty run_id must be rejected")
	}
	noType := base
	noType.AdapterType = ""
	if _, err := MarshalPodStateEvent("run-1", noType); err == nil {
		t.Fatal("empty adapter_type must be rejected")
	}
	noScope := base
	noScope.ScopeID = ""
	if _, err := MarshalPodStateEvent("run-1", noScope); err == nil {
		t.Fatal("empty scope_instance_id must be rejected")
	}
	noPhase := base
	noPhase.Phase = ""
	if _, err := MarshalPodStateEvent("run-1", noPhase); err == nil {
		t.Fatal("empty phase must be rejected")
	}
}

func TestPodStateReportKeys(t *testing.T) {
	report := PodStateReport{AdapterType: "shell", ScopeName: "develop", ScopeID: "dev-001", Phase: PodPhasePending, Reason: "Unschedulable"}
	if got, want := report.ScopeKey(), "shell/develop/dev-001"; got != want {
		t.Fatalf("ScopeKey = %q, want %q", got, want)
	}
	if got, want := report.PodStateSignal(), "Pending|Unschedulable"; got != want {
		t.Fatalf("PodStateSignal = %q, want %q", got, want)
	}
	if got := (PodStateReport{AdapterType: "shell", ScopeID: "dev-001", Phase: PodPhaseRunning}).PodStateSignal(); got != "Running|" {
		t.Fatalf("running PodStateSignal = %q, want Running|", got)
	}
}
