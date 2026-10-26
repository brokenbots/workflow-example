package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"connectrpc.com/connect"

	sdkv1 "github.com/brokenbots/criteria/sdk/pb/criteria/v1"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"

	"github.com/brokenbots/workflow-example/criteria-k8s/internal/castle"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/control"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/kanboard"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/routes"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/runspec"
)

// fakeStream is the WatchRun stream fake: it hands the adapter a fixed
// envelope list (drained in order, then io.EOF) or a stream error, and
// counts Close calls.
type fakeStream struct {
	mu   sync.Mutex
	envs []*sdkv1.Envelope
	err  error
	clos int
}

func (s *fakeStream) Receive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.envs) > 0
}

func (s *fakeStream) Msg() *sdkv1.Envelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.envs) == 0 {
		return nil
	}
	env := s.envs[0]
	s.envs = s.envs[1:]
	return env
}

func (s *fakeStream) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *fakeStream) Close() error {
	s.mu.Lock()
	s.clos++
	s.mu.Unlock()
	return nil
}

func (s *fakeStream) closeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.clos
}

// fakeDispatch captures submits and hands a per-run stream to WatchRun.
type fakeDispatch struct {
	mu        sync.Mutex
	submits   []*sdkv1.SubmitWorkflowAssignmentRequest
	submitErr error
	resp      *sdkv1.SubmitWorkflowAssignmentResponse

	streams   map[string]*fakeStream
	streamErr error
	opens     []string
}

func (d *fakeDispatch) Submit(_ context.Context, req *sdkv1.SubmitWorkflowAssignmentRequest) (*sdkv1.SubmitWorkflowAssignmentResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.submits = append(d.submits, req)
	if d.submitErr != nil {
		return nil, d.submitErr
	}
	if d.resp == nil {
		d.resp = &sdkv1.SubmitWorkflowAssignmentResponse{RunId: fmt.Sprintf("run-%d", len(d.submits))}
	}
	return d.resp, nil
}

func (d *fakeDispatch) WatchRun(_ context.Context, runID string, _ uint64) (castle.EnvelopeStream, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.opens = append(d.opens, runID)
	if d.streamErr != nil {
		return nil, d.streamErr
	}
	if len(d.streams) == 0 {
		return nil, io.EOF
	}
	stream, ok := d.streams[runID]
	if !ok {
		return nil, io.EOF
	}
	return stream, nil
}

func (d *fakeDispatch) submitCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.submits)
}

func (d *fakeDispatch) lastSubmit() *sdkv1.SubmitWorkflowAssignmentRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.submits) == 0 {
		return nil
	}
	return d.submits[len(d.submits)-1]
}

func newTestAdapter(t *testing.T, dispatch castleDispatchClient) (*castleAdapter, *logRecorder) {
	t.Helper()
	rec := &logRecorder{}
	adapter := newCastleAdapter(dispatch, "Kanboard Tickets", logr.New(rec))
	adapter.mirrorWindow = 50 * time.Millisecond
	return adapter, rec
}

func TestResolveDispatchMode(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		wantErr  bool
	}{
		{in: "", want: dispatchModeLegacy},
		{in: dispatchModeLegacy, want: dispatchModeLegacy},
		{in: dispatchModeCastle, want: dispatchModeCastle},
		{in: "Legacy", wantErr: true},
		{in: "bogus", wantErr: true},
	} {
		got, err := resolveDispatchMode(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("resolveDispatchMode(%q): want error, got %q", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("resolveDispatchMode(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("resolveDispatchMode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestAdapterFireSubmitsEnvelope(t *testing.T) {
	defer func(orig func() int64) { nowUnix = orig }(nowUnix)
	nowUnix = func() int64 { return 1234 }

	adapter, logRec := newTestAdapter(t, &fakeDispatch{})
	task := kanboard.Task{ID: 123, Title: "Fix the flake", ColumnID: 5, ColumnName: "Backlog"}
	sel := &routes.Selection{
		Name:       "kb-triage",
		Workflow:   routes.Workflow{Type: routes.TypeURL, Class: routes.ClassTriage},
		ConfigName: "workflow-example",
	}
	adapter.fire(context.Background(), task, "https://github.com/acme/widget.git", sel)

	d := adapter.dispatch.(*fakeDispatch)
	if got := d.submitCount(); got != 1 {
		t.Fatalf("Submit calls = %d, want 1", got)
	}
	req := d.lastSubmit()
	if req.WorkflowName != "kb-triage" {
		t.Errorf("WorkflowName = %q, want kb-triage", req.WorkflowName)
	}
	if req.IdempotencyKey != "KB-123:kb-triage:1234" {
		t.Errorf("IdempotencyKey = %q, want the ticket:workflow:unix shape", req.IdempotencyKey)
	}
	if got := req.Labels[control.DefaultLabelProject]; got != "Kanboard Tickets" {
		t.Errorf("project label = %q, want the adapter's project", got)
	}
	if len(req.Labels) != 1 {
		t.Errorf("labels = %v; the envelope carries per-ticket vocabulary, not labels", req.Labels)
	}
	env, err := control.ParseDispatchEnvelope(req.WorkflowSource)
	if err != nil {
		t.Fatalf("workflow_source does not round-trip: %v", err)
	}
	if env.Ticket != "KB-123" || env.Repo != "https://github.com/acme/widget.git" {
		t.Errorf("envelope identity = %+v, want KB-123/acme repo", env)
	}
	if env.Class != runspec.StampClass(sel.Workflow) {
		t.Errorf("envelope class = %q, want the stamped class %q", env.Class, runspec.StampClass(sel.Workflow))
	}
	if env.Slot != "workflow-example" {
		t.Errorf("envelope slot = %q, want the route's config name", env.Slot)
	}
	if adapter.paritySubmitted != 1 {
		t.Errorf("submitted counter = %d, want 1", adapter.paritySubmitted)
	}
	entry := adapter.runs["KB-123"]
	if entry == nil || entry.runID != "run-1" || entry.workflow != "kb-triage" {
		t.Errorf("adapter book after fire = %+v, want a run-1 entry", entry)
	}
	if logRec.contains("submit failed") || logRec.contains("marshal envelope failed") {
		t.Error("successful fire must not log failures")
	}
}

func TestAdapterFireFailClosed(t *testing.T) {
	// Repo missing: the marshal gate fails before castle sees anything.
	adapter, logRec := newTestAdapter(t, &fakeDispatch{})
	adapter.fire(context.Background(), kanboard.Task{ID: 7}, "", &routes.Selection{Name: "kb-triage"})
	if adapter.paritySubmitted != 0 || len(adapter.runs) != 0 {
		t.Errorf("empty repo must submit nothing; runs=%d", len(adapter.runs))
	}
	if adapter.dispatch.(*fakeDispatch).submitCount() != 0 {
		t.Error("fail-closed marshal must not reach castle")
	}
	if !logRec.contains("castle dispatch") {
		t.Error("marshal failure must log")
	}

	// Submit transport error: logged, no book entry, no counters.
	adapter2, _ := newTestAdapter(t, &fakeDispatch{submitErr: errors.New("castle down")})
	adapter2.fire(context.Background(), kanboard.Task{ID: 7}, "https://github.com/acme/widget.git", &routes.Selection{Name: "kb-triage"})
	if adapter2.paritySubmitted != 0 || len(adapter2.runs) != 0 {
		t.Error("submit failure must not book a run")
	}

	// Castle rejection: counted, not booked.
	adapter3, logRec3 := newTestAdapter(t, &fakeDispatch{resp: &sdkv1.SubmitWorkflowAssignmentResponse{
		RunId:           "run-x",
		State:           sdkv1.WorkflowAssignmentState_WORKFLOW_ASSIGNMENT_STATE_REJECTED,
		RejectionReason: "unknown workflow",
	}})
	adapter3.fire(context.Background(), kanboard.Task{ID: 7}, "https://github.com/acme/widget.git", &routes.Selection{Name: "kb-triage"})
	if adapter3.parityRejected != 1 || len(adapter3.runs) != 0 {
		t.Errorf("rejection must count and not book: rejected=%d runs=%d", adapter3.parityRejected, len(adapter3.runs))
	}
	if !logRec3.contains("assignment rejected") {
		t.Error("rejection must log")
	}
}

func runCompletedState(finalState string) *sdkv1.Envelope {
	return &sdkv1.Envelope{Payload: &sdkv1.Envelope_RunCompleted{RunCompleted: &sdkv1.RunCompleted{Success: true, FinalState: finalState}}}
}

// TestAdapterMirrorLadder pins the observation mapping: the mirror records
// only what castle streamed (terminal phase + final state), latches on the
// first terminal envelope, ignores late duplicates and unrelated payloads,
// and closes the stream it holds.
func TestAdapterMirrorLadder(t *testing.T) {
	t.Run("run_started_then_unknown_final_state", func(t *testing.T) {
		adapter, _ := newTestAdapter(t, &fakeDispatch{})
		adapter.runs["KB-1"] = &submittedRun{runID: "r1", workflow: "kb-develop"}
		stream := &fakeStream{envs: []*sdkv1.Envelope{
			{Payload: &sdkv1.Envelope_RunStarted{RunStarted: &sdkv1.RunStarted{WorkflowName: "kb-develop"}}},
			runCompletedState(""),
		}}
		fd := adapter.dispatch.(*fakeDispatch)
		fd.streams = map[string]*fakeStream{"r1": stream}

		if err := adapter.drainRun(context.Background(), adapter.runs["KB-1"]); err != nil {
			t.Fatalf("drainRun: %v", err)
		}
		entry := adapter.runs["KB-1"]
		if !entry.started || !entry.terminal || entry.endState != "" {
			t.Errorf("ladder state = %+v", entry)
		}
		if entry.endPhase != criteriav1.PhaseSucceeded {
			t.Errorf("endPhase = %v, want Succeeded", entry.endPhase)
		}
		if stream.closeCount() != 1 {
			t.Errorf("stream closed %d times, want 1", stream.closeCount())
		}
	})

	t.Run("handler_complete", func(t *testing.T) {
		adapter, _ := newTestAdapter(t, &fakeDispatch{})
		adapter.runs["KB-1"] = &submittedRun{runID: "r1"}
		fd := adapter.dispatch.(*fakeDispatch)
		fd.streams = map[string]*fakeStream{"r1": {envs: []*sdkv1.Envelope{runCompletedState(finalStateHandlerComplete)}}}
		if err := adapter.drainRun(context.Background(), adapter.runs["KB-1"]); err != nil {
			t.Fatalf("drainRun: %v", err)
		}
		if entry := adapter.runs["KB-1"]; entry.endState != finalStateHandlerComplete {
			t.Errorf("endState = %q, want handler_complete", entry.endState)
		}
	})

	t.Run("run_failed", func(t *testing.T) {
		adapter, _ := newTestAdapter(t, &fakeDispatch{})
		adapter.runs["KB-1"] = &submittedRun{runID: "r1"}
		fd := adapter.dispatch.(*fakeDispatch)
		fd.streams = map[string]*fakeStream{"r1": {envs: []*sdkv1.Envelope{
			{Payload: &sdkv1.Envelope_RunStarted{RunStarted: &sdkv1.RunStarted{WorkflowName: "kb-develop"}}},
			{Payload: &sdkv1.Envelope_RunFailed{RunFailed: &sdkv1.RunFailed{Reason: "engine crashed", Step: "build"}}},
		}}}
		if err := adapter.drainRun(context.Background(), adapter.runs["KB-1"]); err != nil {
			t.Fatalf("drainRun: %v", err)
		}
		entry := adapter.runs["KB-1"]
		if entry.endPhase != criteriav1.PhaseFailed || entry.endState != finalStateFailed {
			t.Errorf("RunFailed must park as phase Failed/finalState failed: %+v", entry)
		}
		if entry.decision != "" {
			t.Errorf("observation must not precompute the parity decision (recordParity owns it); got %q", entry.decision)
		}
	})

	t.Run("late_terminal_and_unrelated_payloads_ignored", func(t *testing.T) {
		adapter, _ := newTestAdapter(t, &fakeDispatch{})
		adapter.runs["KB-1"] = &submittedRun{runID: "r1"}
		fd := adapter.dispatch.(*fakeDispatch)
		fd.streams = map[string]*fakeStream{"r1": {envs: []*sdkv1.Envelope{
			runCompletedState(finalStateHandlerComplete),
			runCompletedState(""),
			{Payload: &sdkv1.Envelope_RunFailed{RunFailed: &sdkv1.RunFailed{Reason: "late"}}},
		}}}
		if err := adapter.drainRun(context.Background(), adapter.runs["KB-1"]); err != nil {
			t.Fatalf("drainRun: %v", err)
		}
		entry := adapter.runs["KB-1"]
		if entry.endState != finalStateHandlerComplete || entry.endPhase != criteriav1.PhaseSucceeded {
			t.Errorf("first terminal must win: %+v", entry)
		}
	})

	t.Run("nil_envelope_no_panic", func(t *testing.T) {
		adapter, _ := newTestAdapter(t, &fakeDispatch{})
		entry := &submittedRun{}
		adapter.applyLadder(entry, nil)
		if entry.started || entry.terminal {
			t.Errorf("nil envelope must be a no-op: %+v", entry)
		}
	})

	t.Run("stream_error_surfaces", func(t *testing.T) {
		adapter, logRec := newTestAdapter(t, &fakeDispatch{})
		adapter.runs["KB-1"] = &submittedRun{runID: "r1"}
		fd := adapter.dispatch.(*fakeDispatch)
		fd.streams = map[string]*fakeStream{"r1": {err: connect.NewError(connect.CodeInternal, errors.New("stream exploded"))}}
		// The mirror owns the failure log (drainRun only surfaces the
		// error): a real stream error goes loud for the next-poll retry.
		adapter.mirror(context.Background(), nil, nil)
		if !logRec.contains("castle mirror") {
			t.Error("drain failure must log for the next-poll retry")
		}
		if adapter.parityDecided != 0 {
			t.Errorf("a failed drain must not decide parity; decided=%d", adapter.parityDecided)
		}
		// And when the stream never opens at all, the same retry shape.
		fd.streamErr = errors.New("no address")
		fd.streams = nil
		adapter2Runs := adapter.runs
		adapter2Runs["KB-1"].terminal = false
		adapter.mirror(context.Background(), nil, nil)
		if !logRec.contains("castle mirror") {
			t.Error("watch-open failure must log too")
		}
	})
}

func syntheticRunPhases(phase criteriav1.CriteriaRunPhase, class, finalState string) runPhases {
	return runPhases{latestPhase: phase, latestClass: class, latestFinalState: finalState, anyRun: true}
}

// TestAdapterMirrorParity pins the shadow-run compare: legacy and castle
// decisions derive from the SHARED decision(), mismatches log loud with
// both sides' observations, agreements log once, and undecided entries
// wait for the CR index to catch up.
func TestAdapterMirrorParity(t *testing.T) {
	t.Run("agree", func(t *testing.T) {
		adapter, logRec := newTestAdapter(t, &fakeDispatch{})
		adapter.runs["KB-1"] = &submittedRun{runID: "r1", terminal: true, endPhase: criteriav1.PhaseSucceeded, endState: finalStateHandlerComplete}
		runsIndex := map[string]runPhases{
			"KB-1": syntheticRunPhases(criteriav1.PhaseSucceeded, routes.ClassDefault, finalStateHandlerComplete),
		}
		adapter.recordParity("KB-1", adapter.runs["KB-1"], []kanboard.Task{{ID: 1, ColumnName: "Backlog"}}, runsIndex)
		if adapter.parityAgreed != 1 || adapter.parityMismatch != 0 || adapter.parityDecided != 1 {
			t.Errorf("counters = agree:%d mismatch:%d decided:%d", adapter.parityAgreed, adapter.parityMismatch, adapter.parityDecided)
		}
		if !logRec.contains("lanes agree") {
			t.Error("agreement must log")
		}
	})

	t.Run("mismatch_loud", func(t *testing.T) {
		adapter, logRec := newTestAdapter(t, &fakeDispatch{})
		adapter.runs["KB-1"] = &submittedRun{runID: "r1", terminal: true, endPhase: criteriav1.PhaseSucceeded, endState: ""}
		runsIndex := map[string]runPhases{
			// The CR ended in the NeedAssistance terminal from Done — the
			// legacy lane repairs, the castle mirror saw no verdict.
			"KB-1": syntheticRunPhases(criteriav1.PhaseSucceeded, routes.ClassDefault, finalStateNeedAssistance),
		}
		adapter.recordParity("KB-1", adapter.runs["KB-1"], []kanboard.Task{{ID: 1, ColumnName: "Done"}}, runsIndex)
		if adapter.parityMismatch != 1 || adapter.parityAgreed != 0 {
			t.Errorf("counters = agree:%d mismatch:%d", adapter.parityAgreed, adapter.parityMismatch)
		}
		if !logRec.contains("lanes disagree") {
			t.Error("mismatch must log loud")
		}
	})

	t.Run("no_reconcile_run_yet_waits", func(t *testing.T) {
		adapter, _ := newTestAdapter(t, &fakeDispatch{})
		adapter.runs["KB-1"] = &submittedRun{runID: "r1", terminal: true, endPhase: criteriav1.PhaseSucceeded, endState: finalStateHandlerComplete}
		adapter.recordParity("KB-1", adapter.runs["KB-1"], nil, map[string]runPhases{})
		if adapter.parityDecided != 0 {
			t.Errorf("no CR observed yet must not decide; decided=%d", adapter.parityDecided)
		}
		if adapter.runs["KB-1"].decision != "" {
			t.Errorf("entry must stay undecided for the next poll; got %q", adapter.runs["KB-1"].decision)
		}
		// The CR lands the next poll: the latch then records exactly once.
		runsIndex := map[string]runPhases{
			"KB-1": syntheticRunPhases(criteriav1.PhaseSucceeded, routes.ClassDefault, finalStateHandlerComplete),
		}
		adapter.recordParity("KB-1", adapter.runs["KB-1"], []kanboard.Task{{ID: 1, ColumnName: "Backlog"}}, runsIndex)
		adapter.recordParity("KB-1", adapter.runs["KB-1"], []kanboard.Task{{ID: 1, ColumnName: "Backlog"}}, runsIndex)
		if adapter.parityDecided != 1 {
			t.Errorf("parity must latch once per (ticket,run); decided=%d", adapter.parityDecided)
		}
	})

	t.Run("triage_class_judges_noop_consistently", func(t *testing.T) {
		adapter, _ := newTestAdapter(t, &fakeDispatch{})
		// Triage run with an unrecognized empty verdict on both sides: the
		// class rule must apply on BOTH sides of the compare (legacy noop
		// despite Succeeded; castle mirror's synthetic record carries the
		// CR's class too).
		adapter.runs["KB-1"] = &submittedRun{runID: "r1", terminal: true, endPhase: criteriav1.PhaseSucceeded, endState: ""}
		adapter.recordParity("KB-1", adapter.runs["KB-1"], []kanboard.Task{{ID: 1, ColumnName: "Review"}}, map[string]runPhases{
			"KB-1": syntheticRunPhases(criteriav1.PhaseSucceeded, routes.ClassTriage, ""),
		})
		if adapter.parityAgreed != 1 || adapter.parityMismatch != 0 {
			t.Errorf("counters = agree:%d mismatch:%d", adapter.parityAgreed, adapter.parityMismatch)
		}
	})
}

// TestAdapterMirrorWindowEnd pins the drain's natural-stop semantics: EOF
// (server ended the watch window) and the mirror-window deadline are
// NOT drain errors; everything else surfaces.
func TestAdapterMirrorWindowEnd(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "eof", err: io.EOF, want: true},
		{name: "window deadline", err: context.DeadlineExceeded, want: true},
		{name: "watch ctx canceled", err: context.Canceled, want: true},
		{name: "castle unavailable", err: connect.NewError(connect.CodeUnavailable, errors.New("restarting")), want: true},
		{name: "stream canceled", err: connect.NewError(connect.CodeCanceled, errors.New("server closed")), want: true},
		{name: "internal", err: connect.NewError(connect.CodeInternal, errors.New("boom"))},
		{name: "plain error", err: errors.New("boom")},
	} {
		if got := isMirrorEnd(tc.err); got != tc.want {
			t.Errorf("%s: isMirrorEnd(%v) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
}

// TestDisabledAdapter pins the nil-safety promise: a legacy-mode watcher
// holds a nil adapter and Disabled() reports the off state without panic.
func TestDisabledAdapter(t *testing.T) {
	var adapter *castleAdapter
	if !adapter.Disabled() {
		t.Error("nil adapter must report disabled")
	}
	adapter.mirror(context.Background(), nil, nil) // must not panic
}

// TestRunPhasesDecision pins the shared board ladder verbatim (the parity
// compare rides it, so a change here is a parity-semantics change):
// KB-23 dev-class-triage successes stay parked; KB-269 Done repair only in
// Done; handler_complete lands Done; KB-50 empty/unknown verdicts abstain;
// KB-302 Failed parks into Review and in-flight phases do nothing.
func TestRunPhasesDecision(t *testing.T) {
	for _, tc := range []struct {
		name       string
		ph         runPhases
		currentCol string
		want       string
	}{
		{name: "triage success stays parked", ph: syntheticRunPhases(criteriav1.PhaseSucceeded, routes.ClassTriage, finalStateHandlerComplete), currentCol: "Backlog", want: decisionNoop},
		{name: "dev handler_complete lands Done", ph: syntheticRunPhases(criteriav1.PhaseSucceeded, routes.ClassDefault, finalStateHandlerComplete), currentCol: "Backlog", want: decisionDone},
		{name: "need_assistance repairs Done", ph: syntheticRunPhases(criteriav1.PhaseSucceeded, routes.ClassDefault, finalStateNeedAssistance), currentCol: "Done", want: decisionReview},
		{name: "need_assistance outside Done parks", ph: syntheticRunPhases(criteriav1.PhaseSucceeded, routes.ClassDefault, finalStateNeedAssistance), currentCol: "Backlog", want: decisionNoop},
		{name: "awaiting_human repairs Done", ph: syntheticRunPhases(criteriav1.PhaseSucceeded, routes.ClassDefault, finalStateAwaitingHuman), currentCol: "Done", want: decisionReview},
		{name: "failed verdict repairs Done", ph: syntheticRunPhases(criteriav1.PhaseSucceeded, routes.ClassDefault, finalStateFailed), currentCol: "Done", want: decisionReview},
		{name: "failed verdict outside Done parks", ph: syntheticRunPhases(criteriav1.PhaseSucceeded, routes.ClassDefault, finalStateFailed), currentCol: "Ready", want: decisionNoop},
		{name: "empty verdict abstains", ph: syntheticRunPhases(criteriav1.PhaseSucceeded, routes.ClassDefault, ""), currentCol: "Backlog", want: decisionNoop},
		{name: "unknown verdict abstains", ph: syntheticRunPhases(criteriav1.PhaseSucceeded, routes.ClassDefault, "weird-terminal-v2"), currentCol: "Backlog", want: decisionNoop},
		{name: "already Done stays Done", ph: syntheticRunPhases(criteriav1.PhaseSucceeded, routes.ClassDefault, finalStateHandlerComplete), currentCol: "Done", want: decisionDone},
		{name: "run failed parks into Review", ph: syntheticRunPhases(criteriav1.PhaseFailed, routes.ClassDefault, ""), currentCol: "Backlog", want: decisionReview},
		{name: "in flight parks", ph: syntheticRunPhases(criteriav1.PhaseRunning, routes.ClassDefault, ""), currentCol: "Ready", want: decisionNoop},
		{name: "unknown phase parks", ph: syntheticRunPhases(criteriav1.PhasePending, routes.ClassDefault, ""), currentCol: "Ready", want: decisionNoop},
	} {
		if got := tc.ph.decision(tc.currentCol); got != tc.want {
			t.Errorf("%s: decision(%q) = %q, want %q", tc.name, tc.currentCol, got, tc.want)
		}
	}
}

// TestWatcherPollCastleModeSubmitsAssignment pins the fire-path flip: with
// the adapter attached, a trigger-tagged card's first-sighting in a route
// trigger column submits an assignment through castle — and stamps NO
// local CriteriaRun. The column-entry gate still governs re-fires.
func TestWatcherPollCastleModeSubmitsAssignment(t *testing.T) {
	tw := newTestWatcher(t, testRoutesJSON)
	fd := &fakeDispatch{}
	adapter := newCastleAdapter(fd, "Kanboard Tickets", logr.New(tw.logs))
	adapter.mirrorWindow = 50 * time.Millisecond
	tw.w.adapter = adapter
	tw.w.dispatchMode = dispatchModeCastle

	tw.kbS.addTask(30, 5, "k8s-run") // Backlog, trigger-tagged
	tw.pollOnce(t)

	require.Equal(t, 1, fd.submitCount(), "castle mode submits one assignment")
	req := fd.lastSubmit()
	env, err := control.ParseDispatchEnvelope(req.WorkflowSource)
	require.NoError(t, err)
	assert.Equal(t, "KB-30", env.Ticket)
	assert.Equal(t, "brokenbots/workflow-example", env.Repo)
	assert.Equal(t, "Kanboard Tickets", req.Labels[control.DefaultLabelProject])
	assert.Empty(t, tw.runs(t), "castle mode stamps no local CriteriaRun; the operator's agent owns the CR")

	// Second poll, same column (residency, not entry): no re-fire.
	tw.pollOnce(t)
	require.Equal(t, 1, fd.submitCount(), "entry gate holds in castle mode")
}

// TestWatcherPollCastleModeParityPinsAgreement runs the whole shadow cycle
// over one poll: assignment submitted, mirror drains the WatchRun stream,
// the CR the lane shares settles, and the parity line logs an agreement —
// legacy and castle deciding identically through the shared decision().
func TestWatcherPollCastleModeParityPinsAgreement(t *testing.T) {
	tw := newTestWatcher(t, testRoutesJSON)
	fd := &fakeDispatch{}
	adapter := newCastleAdapter(fd, "Kanboard Tickets", logr.New(tw.logs))
	adapter.mirrorWindow = 50 * time.Millisecond
	tw.w.adapter = adapter
	tw.w.dispatchMode = dispatchModeCastle

	tw.kbS.addTask(31, 5, "k8s-run")
	tw.pollOnce(t)
	require.Equal(t, 1, fd.submitCount())
	runID := fd.resp.RunId

	// The mirror streams the workflow's handler_complete terminal; the CR
	// (stamped by the operator agent in production, pre-created here)
	// settles Succeeded with the same verdict.
	fd.streams = map[string]*fakeStream{runID: {envs: []*sdkv1.Envelope{runCompletedState(finalStateHandlerComplete)}}}
	tw.addRun(t, "kb-31-0000000001", "KB-31", routes.ClassDefault, criteriav1.PhaseSucceeded)
	tw.setRunFinalStateByName(t, "kb-31-0000000001", "handler_complete")
	tw.pollOnce(t)

	if !tw.logs.contains("lanes agree") {
		t.Error("both lanes deciding done must log the parity agreement")
	}
	if adapter.parityAgreed != 1 || adapter.parityMismatch != 0 {
		t.Errorf("parity counters = agree:%d mismatch:%d, want 1/0", adapter.parityAgreed, adapter.parityMismatch)
	}
	if !tw.logs.contains("castle dispatch summary") {
		t.Error("the per-poll parity report must close every mirror pass")
	}
	assert.Equal(t, 8, tw.kbS.taskColumn(31), "the board still follows the reconcile lane's Done decision in castle mode")
}

// TestWatcherPollLegacyModeStampsLocalRun pins the kill switch's off
// position: a nil adapter keeps every legacy fire-path behavior verbatim
// (the shared fire gates above the create are untouched by this
// workstream, but the branch itself must not leak into legacy).
func TestWatcherPollLegacyModeStampsLocalRun(t *testing.T) {
	tw := newTestWatcher(t, testRoutesJSON)
	tw.kbS.addTask(32, 5, "k8s-run")
	tw.pollOnce(t)
	if !tw.w.adapter.Disabled() {
		t.Error("legacy mode leaves the adapter nil")
	}
	require.Len(t, tw.runs(t), 1, "legacy mode stamps the CriteriaRun locally")
}
