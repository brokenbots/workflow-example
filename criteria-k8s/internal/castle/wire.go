package castle

import (
	v1 "github.com/brokenbots/workflow-example/criteria-k8s/internal/criteria/pb/criteria/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/events"
	"google.golang.org/protobuf/types/known/structpb"
)

// Adapter lifecycle kinds carried in AdapterEvent payloads. These are the
// engine's emission conventions observed in the CRI-132 wire captures
// (evidence/cri132-r3-emission-lines.ndjson): the file's nested AdapterEvent
// lines are renders of the same castle envelopes.
const (
	kindProvisionWanted = "adapter.lifecycle.provision_wanted"
	kindReleased        = "adapter.lifecycle.released"
)

// lifecycleFromEnvelope converts a castle Envelope into the operator's
// LifecycleEvent. Only adapter lifecycle kinds are mapped; every other
// payload is skipped, mirroring the CRI-132 file parser. One deliberate
// deviation: unlike the file parser, the castle client resolves the
// release-event adapter asymmetry (released events report the shim
// registration name, e.g. "noop.default") after this mapping, via
// resolveReleaseAdapter. The envelope's top-level run_id is authoritative
// (payload data.run_id is empty at the engine's emission site).
func lifecycleFromEnvelope(env *v1.Envelope) (events.LifecycleEvent, bool) {
	if env == nil {
		return events.LifecycleEvent{}, false
	}
	payload := env.GetPayload()
	adapterEvent, ok := payload.(*v1.Envelope_AdapterEvent)
	if !ok || adapterEvent.AdapterEvent == nil {
		return events.LifecycleEvent{}, false
	}
	ae := adapterEvent.AdapterEvent

	ev := events.LifecycleEvent{RunID: env.GetRunId()}
	switch ae.GetKind() {
	case kindProvisionWanted:
		ev.Event = events.EventProvisionWanted
	case kindReleased:
		ev.Event = events.EventRelease
	default:
		// Not an adapter lifecycle event: skipped silently, as in the
		// CRI-132 parser.
		return events.LifecycleEvent{}, false
	}

	data := ae.GetData()
	ev.AdapterName = firstNonEmpty(dataString(data, "adapter", "adapter_name"), ae.GetAdapter())
	ev.ScopeID = dataString(data, "scope_instance_id", "scope_id")
	// The engine emits adapter_type (CRI-141): the implementation kind
	// (shell/copilot/...), distinct from the workflow's adapter node name.
	// The per-scope pod builder needs it to resolve adapter images.
	ev.AdapterType = dataString(data, "adapter_type")
	ev.Digest = dataString(data, "digest")
	ev.ShimAddress = dataString(data, "shim_listen_address", "shim_address")
	ev.TokenFile = dataString(data, "token_ref", "token_file")
	// The adapter implementation kind (shell, copilot, ...) is distinct from
	// the workflow's adapter node name above. Emitted since v0.5.22; empty on
	// older engines. The per-scope reconciler resolves the pod image kind
	// from it, so it must survive the wire conversion.
	ev.AdapterType = dataString(data, "adapter_type")
	if ev.AdapterName == "" {
		// Same skip rule as flat and nested events in the CRI-132 parser.
		return events.LifecycleEvent{}, false
	}
	return ev, true
}

// terminalFromEnvelope converts a terminal castle Envelope (RunCompleted or
// RunFailed) into a Terminal, or nil for any other payload. FinalState is the
// engine's workflow terminal state name (RunCompleted.final_state), not the
// CRI-132 Linear ticket state; the wire carries no Linear ticket state, so
// CriteriaRun.Status.TicketState is not populated from it.
func terminalFromEnvelope(env *v1.Envelope) *Terminal {
	if env == nil {
		return nil
	}
	switch payload := env.GetPayload().(type) {
	case *v1.Envelope_RunCompleted:
		completed := payload.RunCompleted
		if completed == nil {
			return nil
		}
		return &Terminal{
			Success:    completed.GetSuccess(),
			FinalState: completed.GetFinalState(),
		}
	case *v1.Envelope_RunFailed:
		failed := payload.RunFailed
		if failed == nil {
			return nil
		}
		return &Terminal{
			Success: false,
			Reason:  failed.GetReason(),
		}
	default:
		return nil
	}
}

// dataString returns the first non-empty string field among keys in a
// structpb payload. Non-string values are ignored.
func dataString(data *structpb.Struct, keys ...string) string {
	if data == nil {
		return ""
	}
	fields := data.GetFields()
	for _, key := range keys {
		if value, ok := fields[key]; ok {
			if s := value.GetStringValue(); s != "" {
				return s
			}
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
