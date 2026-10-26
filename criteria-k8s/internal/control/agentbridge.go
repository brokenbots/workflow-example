// Package control: the assignment bridge (agentbridge.go) — the desired-run
// record's lifecycle feed (KB-338 deliverable 3).
//
// The bridge follows the stamped CriteriaRun from admission to terminal
// phase and mirrors that lifecycle onto the assignment's castle run row:
//
//   - Running → RunStarted (the row's status moves queued→running, which
//     both documents the replica picked it up and frees castle's
//     unstarted-lease slot; further rows rotate in)
//   - Succeeded with the handler_complete verdict → RunCompleted{Success}
//   - Succeeded with a failed / need_assistance / awaiting_human verdict,
//     or a Failed phase → RunFailed (the human-gate spellings surface the
//     verdict in the reason — the same mapping the watcher's
//     reconcileTaskColumn applies to the board, here applied to the row)
//
// Every submit is gated on the assignment's disposition, so a run this
// identity no longer owns is never written, and deduped on
// (run_id, correlation_id) so re-deliveries ack instead of duplicating.
package control

import (
	"context"
	"fmt"
	"strings"
	"time"

	v1 "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	castle "github.com/brokenbots/workflow-example/criteria-k8s/internal/castle"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Workflow final states the bridge maps to terminal envelopes (mirroring
// the watcher's reconcileTaskColumn vocabulary — KB-50/KB-239).
const (
	finalStateHandlerComplete = "handler_complete"
	finalStateFailed          = "failed"
	finalStateNeedAssistance  = "need_assistance"
	// The pre-KB-269 spelling of need_assistance stays valid on old pinned
	// trees (see CriteriaRunStatus.FinalState).
	finalStateAwaitingHuman = "awaiting_human"
)

// DispositionReader consults the assignment's current queue state
// (GetAssignmentDisposition): the bridge's ownership gate.
type DispositionReader interface {
	Disposition(ctx context.Context, runID string) (*v1.GetAssignmentDispositionResponse, error)
}

// EventSubmitter submits one lifecycle payload onto an assignment run's
// castle event stream. *castle.AssignmentEventFeed implements it; tests
// stub it.
type EventSubmitter interface {
	Submit(ctx context.Context, runID, correlationID string, payload any) error
}

// deliver follows the CR until the bridge has acked its terminal event.
// The serial lane holds here — that holding IS the 1-run-per-replica rule.
// A run that wedges outside terminal wedges the lane, which is also how an
// out-of-control run is contained: the operator's stall watchdog (KB-24)
// fails wedged runs, which releases this loop.
func (a *AssignmentAgent) deliver(ctx context.Context, runName, runID, workflowName string, attempts int) {
	if a.bridgeDisabled() {
		// CR-only posture: the lane stays usable without a castle writer —
		// the row closes only at castle's expiry backstop. Log at the
		// scrapeable Error level; this deployment posture is worth a page.
		a.log.Error(fmt.Errorf("assignment bridge disabled"), "assignment agent: no castle event feed wired (or no disposition reader); CR stamped without lifecycle mirroring",
			"run", runID)
		return
	}
	if a.terminalSeen(runID) {
		return
	}
	ticker := time.NewTicker(a.pollEvery)
	defer ticker.Stop()
	for {
		if a.bridgeStep(ctx, runName, runID, workflowName, attempts) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// bridgeDisabled reports whether the bridge's castle writer pair is wired.
func (a *AssignmentAgent) bridgeDisabled() bool {
	return a.events == nil || a.dispositions == nil
}

// terminalSeen reads the bridge ledger.
func (a *AssignmentAgent) terminalSeen(runID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.terminal[runID]
}

// startedSeen reads the bridge ledger.
func (a *AssignmentAgent) startedSeen(runID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.started[runID]
}

// markStarted records the acked RunStarted.
func (a *AssignmentAgent) markStarted(runID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.started[runID] = true
}

// markTerminal records the acked terminal envelope.
func (a *AssignmentAgent) markTerminal(runID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.terminal[runID] = true
}

// bridgeStep polls the CR once and emits whatever the bridge owes. It
// returns true only when the lane can release: the terminal envelope is
// acked, or the record vanished under a successful terminal submission.
func (a *AssignmentAgent) bridgeStep(ctx context.Context, runName, runID, workflowName string, attempts int) bool {
	if a.client == nil {
		// No record store: stamp-only lane (the wiring gates this posture,
		// but the bridge must not nil-panic). Hold without terminal — the
		// row closes at castle's expiry backstop.
		return false
	}
	var run criteriav1.CriteriaRun
	err := a.client.Get(ctx, client.ObjectKey{Namespace: a.namespace, Name: runName}, &run)
	switch {
	case err == nil:
		return a.bridgeStatus(ctx, runID, workflowName, attempts, &run.Status)
	case apierrors.IsNotFound(err):
		// The record was deleted underneath the bridge (manual cleanup):
		// nothing left to follow, so the row closes as failed — it cannot
		// wedge the lane without a terminal submission first.
		return a.bridgeTerminal(ctx, runID, workflowName, attempts,
			runFailedPayload("assignment run record deleted"), true)
	default:
		a.log.Error(err, "assignment bridge: run record read failed; retrying", "run", runID, "criteriaRun", runName)
		return false
	}
}

// bridgeStatus maps one CR status snapshot onto lifecycle emissions.
func (a *AssignmentAgent) bridgeStatus(ctx context.Context, runID, workflowName string, attempts int, status *criteriav1.CriteriaRunStatus) bool {
	switch status.Phase {
	case criteriav1.PhaseSucceeded, criteriav1.PhaseFailed:
		if a.terminalSeen(runID) {
			return true
		}
		return a.bridgeTerminal(ctx, runID, workflowName, attempts, terminalPayload(status), false)
	default:
		if status.Phase == criteriav1.PhaseRunning {
			a.ensureStarted(ctx, runID, workflowName, attempts)
		}
		return false
	}
}

// ensureStarted emits RunStarted exactly once per delivery: only once the
// CR is Running (the pod actually started) does the row move to running.
func (a *AssignmentAgent) ensureStarted(ctx context.Context, runID, workflowName string, attempts int) {
	if a.events == nil || a.startedSeen(runID) {
		return
	}
	correlation := fmt.Sprintf("%s-%d", castle.CorrelationRunStarted, attempts)
	err := a.events.Submit(ctx, runID, correlation, &v1.RunStarted{WorkflowName: workflowName})
	if err != nil {
		a.log.Error(err, "assignment bridge: RunStarted submit failed; retrying next poll", "run", runID)
		return
	}
	a.markStarted(runID)
	a.log.Info("assignment bridge: RunStarted emitted", "run", runID, "workflow", workflowName)
}

// bridgeTerminal emits the terminal envelope and records it. It returns
// true when the lane can release the assignment: on recordLost the release
// happens only if the submit succeeded (a failed submit retries on the
// next poll — the event submit needs only the run id, not the record).
func (a *AssignmentAgent) bridgeTerminal(ctx context.Context, runID, workflowName string, attempts int, payload any, recordLost bool) bool {
	correlation := fmt.Sprintf("%s-%d", castle.CorrelationRunTerminal, attempts)
	err := a.events.Submit(ctx, runID, correlation, payload)
	if err != nil {
		a.log.Error(err, "assignment bridge: terminal submit failed; retrying on the next poll", "run", runID, "recordLost", recordLost)
		return recordLost
	}
	a.markTerminal(runID)
	a.log.Info("assignment bridge: terminal emitted", "run", runID, "workflow", workflowName, "payload", fmt.Sprintf("%T", payload))
	return true
}

// terminalPayload maps the CR's terminal status onto the row's terminal
// event — the same mapping the watcher's reconcileTaskColumn applies to the
// board (KB-50/KB-239 vocabulary), here applied to the desired-run row.
func terminalPayload(status *criteriav1.CriteriaRunStatus) any {
	if status.Phase == criteriav1.PhaseFailed {
		return runFailedPayload(fmt.Sprintf("workflow run failed (verdict %s)", finalStateDescription(status.FinalState)))
	}
	if status.FinalState == finalStateHandlerComplete {
		return &v1.RunCompleted{FinalState: status.FinalState, Success: true}
	}
	return runFailedPayload("workflow run ended without a completing verdict: " + finalStateDescription(status.FinalState))
}

// runFailedPayload is the RunFailed envelope the failure paths submit.
func runFailedPayload(reason string) *v1.RunFailed {
	return &v1.RunFailed{Reason: reason}
}

// finalStateDescription renders a FinalState for reasons, including the
// legacy human-gate spelling (see CriteriaRunStatus.FinalState).
func finalStateDescription(fs string) string {
	switch fs {
	case finalStateHandlerComplete:
		return "handler_complete"
	case finalStateFailed:
		return "failed"
	case finalStateNeedAssistance:
		return "need_assistance (human gate)"
	case finalStateAwaitingHuman:
		return "awaiting_human (legacy human gate)"
	case "":
		return "empty (no verdict recorded)"
	default:
		return fmt.Sprintf("%q", fs)
	}
}

// failAssignmentClosed closes an assignment the lane cannot attribute or
// cannot stamp: RunFailed carries the reasons, the castle row ends failed
// (never wedged queued until expiry), and nothing is created in the
// cluster. Read failures on the way there are fail-open — the row closes
// via the ownership-gate-free submit because the lane still holds the
// lease at this point.
func (a *AssignmentAgent) failAssignmentClosed(ctx context.Context, asg *v1.WorkflowAssignment, reasons []string) {
	runID := asg.GetRunId()
	a.log.Error(fmt.Errorf("%d fail-closed reasons", len(reasons)),
		"assignment agent: assignment failed closed — it is un-attributable or unre-stampable; the castle row is closed as RunFailed, nothing created",
		"run", runID, "ticket", asg.GetWorkflowName(), "labels", asg.GetLabels(), "reasons", reasons)
	if a.events == nil {
		return
	}
	payload := runFailedPayload(fmt.Sprintf("assignment failed closed: %s", strings.Join(reasons, "; ")))
	correlation := fmt.Sprintf("%s-failclosed", castle.CorrelationRunTerminal)
	if err := a.events.Submit(ctx, runID, correlation, payload); err != nil {
		// Retrying here would wedge the lane; the row closes at castle's
		// expiry backstop and the operator gets the page from the log.
		a.log.Error(err, "assignment bridge: fail-closed terminal submit failed; row closes at castle expiry", "run", runID)
	}
}
