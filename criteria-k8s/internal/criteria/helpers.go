package criteria

import (
	"fmt"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/brokenbots/workflow-example/criteria-k8s/internal/criteria/pb/criteria/v1"
)

// SchemaVersion is the current event protocol version. It matches the proto
// package major version (criteria.v1). A bump requires a new proto package
// (criteria.v2) and a coordinated SDK minor release.
const SchemaVersion = 1

// NewEnvelope builds an [Envelope] for runID stamped with [SchemaVersion] and
// the current UTC time. The seq field is left at zero; the orchestrator assigns
// the real value on ingest.
//
// payload must be one of the concrete payload pointer types exported by this
// package (e.g. *[RunStarted], *[StepLog]). Passing nil leaves Payload unset.
// Passing a non-nil value of an unknown type panics — this surfaces caller bugs
// at construction time rather than silently producing an empty envelope.
func NewEnvelope(runID string, payload any) *Envelope {
	env := &pb.Envelope{
		SchemaVersion: SchemaVersion,
		RunId:         runID,
		Ts:            timestamppb.New(time.Now().UTC()),
	}
	setPayload(env, payload)
	return env
}

func setPayload(env *pb.Envelope, payload any) { //nolint:funlen,gocyclo // one case per generated payload type
	switch p := payload.(type) {
	case nil:
		return
	case *pb.RunStarted:
		env.Payload = &pb.Envelope_RunStarted{RunStarted: p}
	case *pb.RunCompleted:
		env.Payload = &pb.Envelope_RunCompleted{RunCompleted: p}
	case *pb.RunFailed:
		env.Payload = &pb.Envelope_RunFailed{RunFailed: p}
	case *pb.StepEntered:
		env.Payload = &pb.Envelope_StepEntered{StepEntered: p}
	case *pb.StepOutcome:
		env.Payload = &pb.Envelope_StepOutcome{StepOutcome: p}
	case *pb.StepTransition:
		env.Payload = &pb.Envelope_StepTransition{StepTransition: p}
	case *pb.StepLog:
		env.Payload = &pb.Envelope_StepLog{StepLog: p}
	case *pb.AdapterEvent:
		env.Payload = &pb.Envelope_AdapterEvent{AdapterEvent: p}
	case *pb.CriteriaHeartbeat:
		env.Payload = &pb.Envelope_CriteriaHeartbeat{CriteriaHeartbeat: p}
	case *pb.CriteriaDisconnected:
		env.Payload = &pb.Envelope_CriteriaDisconnected{CriteriaDisconnected: p}
	case *pb.StepResumed:
		env.Payload = &pb.Envelope_StepResumed{StepResumed: p}
	case *pb.WatchReady:
		env.Payload = &pb.Envelope_WatchReady{WatchReady: p}
	case *pb.VariableSet:
		env.Payload = &pb.Envelope_VariableSet{VariableSet: p}
	case *pb.StepOutputCaptured:
		env.Payload = &pb.Envelope_StepOutputCaptured{StepOutputCaptured: p}
	case *pb.WaitEntered:
		env.Payload = &pb.Envelope_WaitEntered{WaitEntered: p}
	case *pb.WaitResumed:
		env.Payload = &pb.Envelope_WaitResumed{WaitResumed: p}
	case *pb.ApprovalRequested:
		env.Payload = &pb.Envelope_ApprovalRequested{ApprovalRequested: p}
	case *pb.ApprovalDecision:
		env.Payload = &pb.Envelope_ApprovalDecision{ApprovalDecision: p}
	case *pb.BranchEvaluated:
		env.Payload = &pb.Envelope_BranchEvaluated{BranchEvaluated: p}
	case *pb.ForEachEntered:
		env.Payload = &pb.Envelope_ForEachEntered{ForEachEntered: p}
	case *pb.StepIterationStarted:
		env.Payload = &pb.Envelope_StepIterationStarted{StepIterationStarted: p}
	case *pb.StepIterationCompleted:
		env.Payload = &pb.Envelope_StepIterationCompleted{StepIterationCompleted: p}
	case *pb.ScopeIterCursorSet:
		env.Payload = &pb.Envelope_ScopeIterCursorSet{ScopeIterCursorSet: p}
	case *pb.StepIterationItem:
		env.Payload = &pb.Envelope_StepIterationItem{StepIterationItem: p}
	case *pb.RunOutputs:
		env.Payload = &pb.Envelope_RunOutputs{RunOutputs: p}
	default:
		panic(fmt.Sprintf("criteria.NewEnvelope: unsupported payload type %T", payload))
	}
}

// TypeString returns a stable discriminator string for env's payload
// (e.g. "step.log"). It is used as the event-type column in the orchestrator's
// event store and by tests that inspect events without reaching into the oneof.
// Returns the empty string for nil or payload-less envelopes.
func TypeString(env *Envelope) string { //nolint:funlen,gocyclo // one case per generated payload type
	if env == nil {
		return ""
	}
	switch env.Payload.(type) {
	case *pb.Envelope_RunStarted:
		return "run.started"
	case *pb.Envelope_RunCompleted:
		return "run.completed"
	case *pb.Envelope_RunFailed:
		return "run.failed"
	case *pb.Envelope_StepEntered:
		return "step.entered"
	case *pb.Envelope_StepOutcome:
		return "step.outcome"
	case *pb.Envelope_StepTransition:
		return "step.transition"
	case *pb.Envelope_StepLog:
		return "step.log"
	case *pb.Envelope_AdapterEvent:
		return "adapter.event"
	case *pb.Envelope_CriteriaHeartbeat:
		return "criteria.heartbeat"
	case *pb.Envelope_CriteriaDisconnected:
		return "criteria.disconnected"
	case *pb.Envelope_StepResumed:
		return "step.resumed"
	case *pb.Envelope_WatchReady:
		return "watch.ready"
	case *pb.Envelope_VariableSet:
		return "variable.set"
	case *pb.Envelope_StepOutputCaptured:
		return "step.output_captured"
	case *pb.Envelope_WaitEntered:
		return "wait.entered"
	case *pb.Envelope_WaitResumed:
		return "wait.resumed"
	case *pb.Envelope_ApprovalRequested:
		return "approval.requested"
	case *pb.Envelope_ApprovalDecision:
		return "approval.decision"
	case *pb.Envelope_BranchEvaluated:
		return "branch.evaluated"
	case *pb.Envelope_ForEachEntered:
		return "for_each.entered"
	case *pb.Envelope_StepIterationStarted:
		return "step.iteration_started"
	case *pb.Envelope_StepIterationCompleted:
		return "step.iteration_completed"
	case *pb.Envelope_ScopeIterCursorSet:
		return "scope.iter_cursor_set"
	case *pb.Envelope_StepIterationItem:
		return "step.iteration_item"
	case *pb.Envelope_RunOutputs:
		return "run.outputs"
	default:
		return ""
	}
}

// IsTerminal reports whether env is a terminal run event (run.completed or
// run.failed). Orchestrators use this to close WatchRun streams after the
// final event for a run.
func IsTerminal(env *Envelope) bool {
	if env == nil {
		return false
	}
	switch env.Payload.(type) {
	case *pb.Envelope_RunCompleted, *pb.Envelope_RunFailed:
		return true
	default:
		return false
	}
}
