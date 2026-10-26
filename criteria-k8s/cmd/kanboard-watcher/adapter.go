// The castle dispatch adapter (KB-338 deliverable 4, P0.3): behind
// CRITERIA_DISPATCH_MODE=castle the watcher's fire path stops stamping
// CriteriaRuns and submits a WorkflowAssignment to castle instead; the
// project's operator agent (the assignment lane) leases it from the Control
// stream and stamps the CR through the shared jobbuilder/runspec paths. The
// adapter is deliberately thin and stateless across polls beyond one map:
// poll board → resolve route → submit assignment.
//
// Mirror (decision 7): run state reaches the board through the
// reconcile lane exactly as today — the operator's CR carries the same
// source=kanboard label set the watcher stamps, so indexRunPhases indexes
// it and reconcileTaskColumn makes the same column decisions it always
// made. The adapter additionally shadows the direct castle observation
// (WatchRun envelopes) over the same run and — when both lanes have a
// terminal decision for the ticket — logs the parity line and accumulates
// the per-poll agreement counters (shadow-run compare, decision 5). The
// logged parity report is the cutover evidence; acting on the WatchRun
// decisions (the mirror taking the board over) is the later cutover flip.
//
// The adapter's castle identity is a submitter only: it never opens the
// Control stream, so castle's dispatch loop (Registered() controls) can
// never lease work to it, and no heartbeat is needed — auth resolution
// reads the token row, not the heartbeat state. Assignment ownership
// (Castle keys it on the submitting criteria id) keeps the disposition
// readable for the run's whole life.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"connectrpc.com/connect"
	"github.com/go-logr/logr"

	sdkv1 "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"

	"github.com/brokenbots/workflow-example/criteria-k8s/internal/castle"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/control"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/kanboard"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/routes"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/runspec"
)

// Dispatch modes (the kill switch): legacy is today's behavior verbatim —
// the watcher stamps the CR itself; castle routes the fire through the
// assignment submission. The env var name CRITERIA_DISPATCH_MODE is the
// card's documented public surface.
const (
	dispatchModeLegacy = "legacy"
	dispatchModeCastle = "castle"
	dispatchModeEnv    = "CRITERIA_DISPATCH_MODE"
)

// Dispatch envelope label: the ONLY assignment label the adapter sends. The
// lease gate requires the agent to hold every assignment label, so labels
// carry operator-saturable routing values only; the per-ticket payload
// rides the workflow_source envelope.
type castleDispatchClient interface {
	Submit(ctx context.Context, req *sdkv1.SubmitWorkflowAssignmentRequest) (*sdkv1.SubmitWorkflowAssignmentResponse, error)
	WatchRun(ctx context.Context, runID string, sinceSeq uint64) (castle.EnvelopeStream, error)
}

// resolveDispatchMode validates the dispatch flag/env pair. Empty means
// legacy: the kill switch's off position is the default posture and zero
// new behavior without explicit configuration. An unknown value fails the
// boot (a typo must never silently pick either lane).
func resolveDispatchMode(mode string) (string, error) {
	switch mode {
	case "":
		return dispatchModeLegacy, nil
	case dispatchModeLegacy, dispatchModeCastle:
		return mode, nil
	default:
		return "", fmt.Errorf("dispatch mode %q: want one of %q, %q", mode, dispatchModeLegacy, dispatchModeCastle)
	}
}

// submittedRun is the adapter's book entry for one submitted assignment.
// It exists from submit until the run terminal is observed on either the
// watch stream or the disposition read.
type submittedRun struct {
	// runID is the server-created id from the submit response.
	runID    string
	workflow string
	// mirror state: the WatchRun ladder's observations.
	started bool
	// terminal marks the run's end observed on the mirror; endPhase /
	// endState seed the synthetic runPhases recordParity feeds through the
	// SHARED decision() — the parity compare measures divergence between
	// the two lanes' observations of one run, not between two
	// implementations of the ladder.
	terminal bool
	endPhase criteriav1.CriteriaRunPhase
	endState string
	decision string
}

// castleAdapter is the dispatch lane for one watcher project.
type castleAdapter struct {
	dispatch castleDispatchClient
	project  string
	log      logr.Logger

	// runs maps ticketID → book entry for THIS adapter's submissions.
	runs map[string]*submittedRun

	// mirrorWindow bounds each run's per-poll stream drain so a wedged
	// watch never stretches the poll (KB-22's one-blocking-call-at-a-time
	// posture). Tests may shrink it.
	mirrorWindow time.Duration

	// parity counters: the shadow-run report (decision 5). The log line is
	// the record; these are the aggregate the log-scrape keys on.
	parityDecided   int
	parityAgreed    int
	parityMismatch  int
	paritySubmitted int // submissions accepted by castle
	parityRejected  int // submissions castle rejected
}

func newCastleAdapter(dispatch castleDispatchClient, project string, log logr.Logger) *castleAdapter {
	return &castleAdapter{
		dispatch:     dispatch,
		project:      project,
		log:          log,
		runs:         map[string]*submittedRun{},
		mirrorWindow: 2 * time.Second,
	}
}

// Disabled reports whether the adapter lane is off (nil adapter — the
// legacy mode's shape). Safe on a nil receiver so the fire/mirror call
// sites need no nil checks.
func (a *castleAdapter) Disabled() bool {
	return a == nil
}

// fire submits one assignment for a fired card. Best-effort against the
// shared fire gates: every failure logs and returns, never panics the
// poll; the column-entry gate re-fires the submission on the next poll so
// no dedicated retry loop is needed.
func (a *castleAdapter) fire(ctx context.Context, task kanboard.Task, repoURL string, sel *routes.Selection) {
	ticketID := task.Identifier()
	envelope := control.DispatchEnvelope{
		Ticket: ticketID,
		Repo:   repoURL,
		Class:  runspec.StampClass(sel.Workflow),
		Slot:   sel.ConfigName,
	}
	source, err := control.MarshalDispatchEnvelope(envelope)
	if err != nil {
		// Fail closed: never submit an assignment castle would queue for a
		// run the operator cannot attribute.
		a.log.Error(err, "castle dispatch: marshal envelope failed; not submitting", "ticket", ticketID)
		return
	}
	req := &sdkv1.SubmitWorkflowAssignmentRequest{
		WorkflowName:   sel.Name,
		WorkflowSource: source,
		// The project label is the lease-gate routing: it must match the
		// project operator agent's registered label exactly.
		Labels: map[string]string{control.DefaultLabelProject: a.project},
		// The idempotency key mirrors the legacy run-name uniqueness
		// (ticket+workflow+firing wall clock): a same-second double fire
		// dedupes (castle returns the existing run), a later re-entry
		// fires a fresh assignment exactly as the legacy lane's stamp
		// would. A restarted adapter re-submitting an untracked in-flight
		// assignment would mint a new key — the same window exists in the
		// legacy lane (the in-flight gate reads the CR index, which
		// survives restarts identically here: the operator's stamped CR
		// carries the same source label).
		IdempotencyKey: fmt.Sprintf("%s:%s:%d", ticketID, sel.Name, nowUnix()),
	}
	resp, err := a.dispatch.Submit(ctx, req)
	if err != nil {
		a.log.Error(err, "castle dispatch: submit failed", "ticket", ticketID, "workflow", sel.Name)
		return
	}
	if resp.GetState() == sdkv1.WorkflowAssignmentState_WORKFLOW_ASSIGNMENT_STATE_REJECTED {
		a.log.Error(fmt.Errorf("castle rejected assignment %s", resp.GetRejectionReason()),
			"castle dispatch: assignment rejected", "ticket", ticketID, "reason", resp.GetRejectionReason())
		a.parityRejected++
		return
	}
	a.paritySubmitted++
	entry := &submittedRun{runID: resp.GetRunId(), workflow: sel.Name}
	if old, ok := a.runs[ticketID]; ok && old.runID == entry.runID {
		// Idempotent re-ack from the same assignment: keep the mirror
		// state the earlier submit built.
		return
	}
	a.runs[ticketID] = entry
}

// mirror drains the watch stream for every tracked submission and records
// the decision ladder's output. runsIndex is the poll's existing CR index —
// the same map reconcileTaskColumn acts on.
func (a *castleAdapter) mirror(ctx context.Context, tasks []kanboard.Task, runsIndex map[string]runPhases) {
	if a == nil {
		return
	}
	for ticketID, entry := range a.runs {
		if entry.terminal {
			continue
		}
		if err := a.drainRun(ctx, entry); err != nil {
			a.log.Error(err, "castle mirror: drain failed; retrying next poll", "ticket", ticketID, "run", entry.runID)
			continue
		}
		a.recordParity(ticketID, entry, tasks, runsIndex)
	}
	a.logParityReport()
}

// drainRun opens a short-lived watch for one run and applies the ladder to
// everything already streamed. Stateless across polls: re-observed
// envelopes are idempotent (the ladder latches), since=0 re-plays from the
// beginning each poll.
func (a *castleAdapter) drainRun(ctx context.Context, entry *submittedRun) error {
	watchCtx, cancel := context.WithTimeout(ctx, a.mirrorWindow)
	defer cancel()
	stream, err := a.dispatch.WatchRun(watchCtx, entry.runID, 0)
	if err != nil {
		return err
	}
	defer stream.Close()
	for stream.Receive() {
		a.applyLadder(entry, stream.Msg())
		if entry.terminal {
			return nil
		}
	}
	if err := stream.Err(); err != nil {
		if isMirrorEnd(err) {
			return nil
		}
		return err
	}
	return nil
}

// isMirrorEnd reports whether a Recv error is the drain's natural stop: a
// clean stream end, the poll's mirror window expiring, or the server
// shutting the stream down (a per-poll client-held stream ends every way
// but error paths without a run reaching its terminal).
func isMirrorEnd(err error) bool {
	if errors.Is(err, io.EOF) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	code := connect.CodeOf(err)
	return code == connect.CodeUnavailable || code == connect.CodeCanceled
}

// applyLadder maps one envelope onto the run's mirror state — decision 7's
// board ladder over castle envelopes. It records only the OBSERVATION
// (terminal, phase, final state); the decision itself is computed by
// recordParity through the shared decision() so both lanes' decisions come
// from the same rule.
func (a *castleAdapter) applyLadder(entry *submittedRun, env *sdkv1.Envelope) {
	if env == nil {
		return
	}
	switch p := env.Payload.(type) {
	case *sdkv1.Envelope_RunStarted:
		entry.started = true
	case *sdkv1.Envelope_RunCompleted:
		if entry.terminal {
			return
		}
		entry.terminal = true
		entry.endPhase = criteriav1.PhaseSucceeded
		entry.endState = p.RunCompleted.GetFinalState()
	case *sdkv1.Envelope_RunFailed:
		if entry.terminal {
			return
		}
		entry.terminal = true
		entry.endPhase = criteriav1.PhaseFailed
		entry.endState = finalStateFailed
	}
}

// recordParity emits the shadow-run parity line once per (ticket, run)
// when BOTH the mirror and the reconcile lane have observed a terminal.
// The castle side's decision is the mirror's observation fed through the
// SAME decision() the legacy lane uses (synthetic runPhases) — so a
// mismatch means the two lanes DISAGREED about the run, never that two
// ladders disagree about one input (the compare would be uninformative in
// that case: it would just re-test this code against itself).
func (a *castleAdapter) recordParity(ticketID string, entry *submittedRun, tasks []kanboard.Task, runsIndex map[string]runPhases) {
	if !entry.terminal || entry.decision != "" {
		return
	}
	ph, ok := runsIndex[ticketID]
	if !ok || !ph.anyRun {
		// The reconcile lane has no run for the ticket: nothing to compare
		// yet (the operator's CR may be one poll behind the assignment
		// flow). Leave the entry undecided; the next poll re-records.
		return
	}
	var currentCol string
	for _, task := range tasks {
		if task.Identifier() == ticketID {
			currentCol = task.ColumnName
			break
		}
	}
	legacy := ph.decision(currentCol)
	// The synthetic record mirrors the CR's class (both lanes watched the
	// same workflow; castle envelopes carry no class) and the mirror's
	// terminal phase + final state.
	castle := (&runPhases{
		latestPhase:      entry.endPhase,
		latestClass:      ph.latestClass,
		latestFinalState: entry.endState,
	}).decision(currentCol)
	entry.decision = castle
	a.parityDecided++
	if castle != legacy {
		a.parityMismatch++
		a.log.Error(fmt.Errorf("legacy decide %s (phase %s finalState %q), castle decide %s (finalState %q)",
			legacy, ph.latestPhase, ph.latestFinalState, castle, entry.endState),
			"shadow parity: lanes disagree",
			"ticket", ticketID, "run", entry.runID,
			"legacyDecision", legacy, "castleDecision", castle,
			"legacyFinalState", ph.latestFinalState, "castleFinalState", entry.endState)
		return
	}
	a.parityAgreed++
	a.log.Info("shadow parity: lanes agree",
		"ticket", ticketID, "run", entry.runID, "decision", castle)
}

// logParityReport writes the per-poll aggregate — the acceptance's parity
// report (log line only; the counters are the report).
func (a *castleAdapter) logParityReport() {
	a.log.Info("castle dispatch summary",
		"submittedTotal", a.paritySubmitted, "rejectedTotal", a.parityRejected,
		"parityDecided", a.parityDecided, "parityAgree", a.parityAgreed, "parityMismatch", a.parityMismatch)
}
