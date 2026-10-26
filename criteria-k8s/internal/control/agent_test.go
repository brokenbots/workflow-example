package control

// agent_test.go exercises the assignment agent lane (KB-338) at contract
// granularity against the fake client + stub feeds — no castle process
// needed: the consumer's scope filter and backpressure, the fail-closed
// translate and its castle-row close, the shared-path stamp with provenance
// + target metadata, the re-delivery reattach, the disposition gate, and
// the bridge's terminal mapping.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/castle"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/runspec"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// assignmentFixtureJSON is the dispatch envelope the adapter lane writes
// (payload.go's contract: kind + ticket + repo + class + slot + targets).
const assignmentFixtureJSON = `{"kind":"criteria-workflow-assignment/1","ticket":"KB-338",` +
	`"repo":"https://github.com/brokenbots/workflow-example.git","class":"default",` +
	`"targetImages":{"shell":"localhost:5000/shell:pinned"}}`

// assignmentFixture builds the Control-stream assignment the castle queue
// delivers for the fixture ticket: project routing label plus the
// per-ticket payload in workflow_source.
func assignmentFixture(runID string) *v1.WorkflowAssignment {
	return &v1.WorkflowAssignment{
		RunId:          runID,
		WorkflowName:   "kanboard-develop-url",
		WorkflowSource: assignmentFixtureJSON,
		Labels:         map[string]string{DefaultLabelProject: "castor"},
	}
}

// stubEvents records every Submit call.
type stubEvents struct {
	mu    sync.Mutex
	calls []string // "<runID>|<correlation>|<typeName>"
}

func (s *stubEvents) Submit(ctx context.Context, runID, correlationID string, payload any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, runID+"|"+correlationID+"|"+fmt.Sprintf("%T", payload))
	return nil
}

func (s *stubEvents) recorded() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

func (s *stubEvents) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// failingEvents always errors (feed outage posture).
type failingEvents struct{ cause error }

func (f failingEvents) Submit(ctx context.Context, runID, correlationID string, payload any) error {
	return f.cause
}

type stubDispositions struct {
	res map[string]*v1.GetAssignmentDispositionResponse
	err error
}

func (s *stubDispositions) Disposition(ctx context.Context, runID string) (*v1.GetAssignmentDispositionResponse, error) {
	if s.err != nil {
		return nil, s.err
	}
	if resp, ok := s.res[runID]; ok {
		return resp, nil
	}
	return &v1.GetAssignmentDispositionResponse{
		State: v1.WorkflowAssignmentState_WORKFLOW_ASSIGNMENT_STATE_LEASED,
	}, nil
}

func dispositionLeasedTo(criteriaID string) *v1.GetAssignmentDispositionResponse {
	return &v1.GetAssignmentDispositionResponse{
		State:            v1.WorkflowAssignmentState_WORKFLOW_ASSIGNMENT_STATE_LEASED,
		LeasedCriteriaId: criteriaID,
	}
}

type stubSink struct {
	messages []*v1.ControlMessage
	pos      int
}

func newStubSink(assignments ...*v1.WorkflowAssignment) *stubSink {
	sink := &stubSink{}
	for _, asg := range assignments {
		sink.messages = append(sink.messages, &v1.ControlMessage{
			Command: &v1.ControlMessage_WorkflowAssignment{WorkflowAssignment: asg},
		})
	}
	return sink
}

func (s *stubSink) next() (*v1.ControlMessage, error) {
	if s.pos >= len(s.messages) {
		return nil, io.EOF
	}
	message := s.messages[s.pos]
	s.pos++
	return message, nil
}

func (s *stubSink) close() {}

// agentFixture wires the lane with the run_test parity fixtures and a fake
// client. Bridge feeds default to working stubs; tests that need the
// stamp-only posture nil them locally.
func agentFixture(t *testing.T) (*AssignmentAgent, *stubEvents, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, criteriav1.AddToScheme(scheme))
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	events := &stubEvents{}
	builder := &RunBuilder{Defaults: parityDefaults(), Routes: parityPayload(), Client: cl, Namespace: "criteria-jobs", Log: logr.Discard()}
	dispositions := DispositionReader(&stubDispositions{})
	agent := NewAssignmentAgent(AgentConfig{
		Addr:           "http://castle-test:8080",
		CriteriaID:     "agent-a",
		Token:          "pre-shared",
		Name:           "operator-castor",
		Project:        "castor",
		HeartbeatEvery: time.Millisecond,
	}, builder, "criteria-jobs", cl, events, dispositions, logr.Discard())
	agent.identity = AgentIdentity{CriteriaID: "agent-a", Token: "pre-shared"}
	agent.pollEvery = 2 * time.Millisecond
	return agent, events, cl
}

func decisionFixture() Decision {
	return Decision{Ticket: "KB-338", Project: "castor", RepoURL: "https://github.com/brokenbots/workflow-example.git", Class: "default", WorkflowName: "kanboard-develop-url"}
}

// mustCreateRun stamps + creates the fixture run so bridge tests point at
// a real record.
func mustCreateRun(t *testing.T, a *AssignmentAgent, cl client.Client, runID string, status criteriav1.CriteriaRunStatus) *criteriav1.CriteriaRun {
	t.Helper()
	ctx := context.Background()
	run, reasons := a.builder.BuildRun(ctx, decisionFixture())
	require.Empty(t, reasons)
	if run.Annotations == nil {
		run.Annotations = map[string]string{}
	}
	run.Annotations[AnnAssignmentRunID] = runID
	run.Annotations[AnnAssignmentAgent] = a.identity.CriteriaID
	run.Annotations[AnnAssignmentAttempts] = "1"
	run.Status = status
	require.NoError(t, cl.Create(ctx, run))
	return run
}

func listCriteriaRuns(t *testing.T, cl client.Client) []criteriav1.CriteriaRun {
	t.Helper()
	var runs criteriav1.CriteriaRunList
	require.NoError(t, cl.List(context.Background(), &runs, client.InNamespace("criteria-jobs")))
	return runs.Items
}

func splitEventCall(call string) (runID, correlation, payloadType string) {
	parts := strings.SplitN(call, "|", 3)
	return parts[0], parts[1], parts[2]
}

func TestAgentTranslateMergesEnvelope(t *testing.T) {
	agent, _, _ := agentFixture(t)
	decision, envelope := agent.translate(assignmentFixture("r-1"))
	require.Empty(t, decision.Missing, "fixture assignment is fully attributable")
	assert.Equal(t, "KB-338", decision.Ticket)
	assert.Equal(t, "https://github.com/brokenbots/workflow-example.git", decision.RepoURL)
	assert.Equal(t, "default", decision.Class)
	assert.Equal(t, "castor", decision.Project)
	assert.Equal(t, "kanboard-develop-url", decision.WorkflowName)
	assert.Equal(t, map[string]string{"shell": "localhost:5000/shell:pinned"}, envelope.TargetImages)
}

func TestAgentTranslateFailsClosedOnForeignSource(t *testing.T) {
	agent, _, _ := agentFixture(t)
	for name, source := range map[string]string{
		"garbage": "some workflow source hcl",
		"empty":   "",
	} {
		asg := assignmentFixture("r-2")
		asg.WorkflowSource = source
		decision, _ := agent.translate(asg)
		require.NotEmpty(t, decision.Missing, "%s source must not translate", name)
		assert.True(t, strings.Contains(strings.Join(decision.Missing, "; "), "payload"),
			"%s: missing should mention the payload requirement: %q", name, decision.Missing)
	}
}

func TestAgentProcessFailClosedClosesRow(t *testing.T) {
	agent, events, cl := agentFixture(t)
	asg := assignmentFixture("run-fc")
	asg.WorkflowSource = "not the dispatch payload"

	agent.process(context.Background(), asg)

	assert.Empty(t, listCriteriaRuns(t, cl), "fail-closed must never stamp a CR")
	require.Equal(t, 1, events.count(), "exactly one RunFailed row close")
	runID, correlation, payloadType := splitEventCall(events.recorded()[0])
	assert.Equal(t, "run-fc", runID)
	assert.Equal(t, fmt.Sprintf("%s-failclosed", castle.CorrelationRunTerminal), correlation)
	assert.Equal(t, fmt.Sprintf("%T", runFailedPayload("x")), payloadType)
}

func TestAgentProcessFailClosedSurvivesFeedsOutage(t *testing.T) {
	agent, _, cl := agentFixture(t)
	// Feed outage ON the fail-closed path: the close submit fails — the
	// lane logs and moves on (castle's lease expiry is the backstop),
	// never stamping a ghost CR.
	agent.events = failingEvents{cause: errors.New("castle unavailable")}
	asg := assignmentFixture("run-fc2")
	asg.WorkflowSource = "not the dispatch payload"
	agent.process(context.Background(), asg)
	assert.Empty(t, listCriteriaRuns(t, cl), "a feed failure on the fail-closed path must never stamp a run")
}

func TestAgentProcessStampsProvenanceAndTargets(t *testing.T) {
	agent, events, cl := agentFixture(t)
	agent.events = nil
	agent.dispositions = nil // stamp-only posture: deliver never blocks

	agent.process(context.Background(), assignmentFixture("run-stamp"))

	runs := listCriteriaRuns(t, cl)
	require.Len(t, runs, 1)
	run := runs[0]
	assert.Equal(t, "criteria-jobs", run.Namespace)
	assert.Equal(t, "KB-338", run.Spec.TicketID, "shared stamp keeps ticket resolution identical")
	assert.Equal(t, "run-stamp", run.Annotations[AnnAssignmentRunID])
	assert.Equal(t, "agent-a", run.Annotations[AnnAssignmentAgent])
	assert.Equal(t, "1", run.Annotations[AnnAssignmentAttempts])
	assert.Equal(t, runspec.ManagedBy, run.Labels[runspec.LabelManagedBy],
		"agent lane stamps the same managed-by vocabulary as the watcher")
	assert.Equal(t, "localhost:5000/shell:pinned", run.Spec.Workflow.AdapterImages["shell"],
		"assignment target metadata lands in adapter images")
	assert.Equal(t, 0, events.count(), "stamp-only lane emits nothing, including terminal")
}

// assignmentWithTarget rebuilds the fixture envelope with one explicit
// per-card target pin (the metadata rung under test).
func assignmentWithTarget(runID, kind, ref string) *v1.WorkflowAssignment {
	asg := assignmentFixture(runID)
	asg.WorkflowSource = fmt.Sprintf(`{"kind":"criteria-workflow-assignment/1","ticket":"KB-338",`+
		`"repo":"https://github.com/brokenbots/workflow-example.git","class":"default",`+
		`"targetImages":{%q:%q}}`, kind, ref)
	return asg
}

// TestAgentProcessFailsClosedOnInvalidTargetPin pins the metadata rung's
// validation (PR #232 review): the envelope's per-card target pins pass
// the SAME routes adapter-image rules every other lane enforces BEFORE
// they reach the spec — an invalid kind or ref fails the assignment
// closed (RunFailed row close, nothing created).
func TestAgentProcessFailsClosedOnInvalidTargetPin(t *testing.T) {
	for name, kindRef := range map[string][2]string{
		"invalid adapter kind":  {"Shell_1", "localhost:5000/shell:pinned"},
		"whitespace in ref":     {"shell", "localhost:5000/sh ell:pinned"},
		"empty image reference": {"shell", ""},
	} {
		t.Run(name, func(t *testing.T) {
			agent, events, cl := agentFixture(t)
			agent.process(context.Background(), assignmentWithTarget("run-badpin", kindRef[0], kindRef[1]))

			assert.Empty(t, listCriteriaRuns(t, cl), "an invalid target pin must never stamp a CR")
			require.Equal(t, 1, events.count(), "an invalid target pin must close the row as RunFailed")
			runID, correlation, payloadType := splitEventCall(events.recorded()[0])
			assert.Equal(t, "run-badpin", runID)
			assert.Equal(t, fmt.Sprintf("%s-failclosed", castle.CorrelationRunTerminal), correlation)
			assert.Equal(t, fmt.Sprintf("%T", runFailedPayload("x")), payloadType)
		})
	}
}

// TestAgentProcessNilClientNeverPanics pins the lane's ONE nil-client
// policy (PR #232 review): process on a deployment whose direct cluster
// client never initialized stamps nothing, closes nothing (castle expiry
// is the backstop — nil client is infrastructure, not a bad assignment),
// and above all never panics, the way findRecordedRun/bridgeStep already
// degraded before the guards.
func TestAgentProcessNilClientNeverPanics(t *testing.T) {
	builder := &RunBuilder{Defaults: parityDefaults(), Routes: parityPayload(), Namespace: "criteria-jobs", Log: logr.Discard()}
	agent := NewAssignmentAgent(AgentConfig{
		Addr:       "http://castle-test:8080",
		CriteriaID: "agent-a",
		Token:      "pre-shared",
		Name:       "operator-castor",
		Project:    "castor",
	}, builder, "criteria-jobs", nil, &stubEvents{}, DispositionReader(&stubDispositions{}), logr.Discard())
	agent.identity = AgentIdentity{CriteriaID: "agent-a", Token: "pre-shared"}

	assert.NotPanics(t, func() {
		agent.process(context.Background(), assignmentFixture("run-nilcl"))
	})
}

func TestAgentAttachRunRedeliveryReattaches(t *testing.T) {
	agent, _, cl := agentFixture(t)
	existing, reasons := agent.builder.BuildRun(context.Background(), decisionFixture())
	require.Empty(t, reasons)
	if existing.Annotations == nil {
		existing.Annotations = map[string]string{}
	}
	existing.Annotations[AnnAssignmentRunID] = "run-again"
	existing.Annotations[AnnAssignmentAgent] = "agent-a"
	existing.Annotations[AnnAssignmentAttempts] = "1"
	require.NoError(t, cl.Create(context.Background(), existing))

	stale, reasons := agent.builder.BuildRun(context.Background(), decisionFixture())
	require.Empty(t, reasons)
	reattached, attempts, ok := agent.attachRun(context.Background(), stale, assignmentFixture("run-again"))
	require.True(t, ok)
	assert.Equal(t, existing.Name, reattached.Name)
	assert.Equal(t, 2, attempts)
	assert.Equal(t, "2", reattached.Annotations[AnnAssignmentAttempts])
	assert.Len(t, listCriteriaRuns(t, cl), 1, "no second CR on a re-delivery")
}

func TestAgentOwnedByUsGate(t *testing.T) {
	agent, _, _ := agentFixture(t)
	ctx := context.Background()

	t.Run("leased to another agent", func(t *testing.T) {
		agent.dispositions = &stubDispositions{res: map[string]*v1.GetAssignmentDispositionResponse{
			"r-o": dispositionLeasedTo("agent-b"),
		}}
		assert.False(t, agent.ownedByUs(ctx, "r-o"))
	})
	t.Run("terminal disposition", func(t *testing.T) {
		agent.dispositions = &stubDispositions{res: map[string]*v1.GetAssignmentDispositionResponse{
			"r-t": {State: v1.WorkflowAssignmentState_WORKFLOW_ASSIGNMENT_STATE_TERMINAL},
		}}
		assert.False(t, agent.ownedByUs(ctx, "r-t"))
	})
	t.Run("rejected disposition", func(t *testing.T) {
		agent.dispositions = &stubDispositions{res: map[string]*v1.GetAssignmentDispositionResponse{
			"r-r": {State: v1.WorkflowAssignmentState_WORKFLOW_ASSIGNMENT_STATE_REJECTED},
		}}
		assert.False(t, agent.ownedByUs(ctx, "r-r"))
	})
	t.Run("read error fails open", func(t *testing.T) {
		agent.dispositions = &stubDispositions{err: errors.New("castle unreachable")}
		assert.True(t, agent.ownedByUs(ctx, "r-e"), "courtesy gate fails open on read errors")
	})
}

func TestAgentBridgeRunningEmitsRunStartedOnce(t *testing.T) {
	agent, events, cl := agentFixture(t)
	run := mustCreateRun(t, agent, cl, "run-start", criteriav1.CriteriaRunStatus{Phase: criteriav1.PhasePending})
	ctx := context.Background()
	wf := decisionFixture().WorkflowName

	assert.False(t, agent.bridgeStep(ctx, run.Name, "run-start", wf, 1), "Pending holds the lane")
	assert.Equal(t, 0, events.count(), "Pending emits nothing")

	// The pod starts: the phase flips to Running.
	run.Status.Phase = criteriav1.PhaseRunning
	require.NoError(t, cl.Update(ctx, run))
	assert.False(t, agent.bridgeStep(ctx, run.Name, "run-start", wf, 1), "Running still holds the lane")
	require.Equal(t, 1, events.count(), "exactly one RunStarted on first Running observation")
	runID, correlation, payloadType := splitEventCall(events.recorded()[0])
	assert.Equal(t, "run-start", runID)
	assert.Equal(t, fmt.Sprintf("%s-1", castle.CorrelationRunStarted), correlation)
	assert.Equal(t, fmt.Sprintf("%T", &v1.RunStarted{}), payloadType)

	// A re-observation still holds the lane but must not re-emit.
	assert.False(t, agent.bridgeStep(ctx, run.Name, "run-start", wf, 1))
	assert.Equal(t, 1, events.count(), "RunStarted dedupes on the ledger")
}

func TestAgentBridgeTerminalMappings(t *testing.T) {
	ctx := context.Background()
	cases := map[string]struct {
		phase      criteriav1.CriteriaRunPhase
		finalState string
	}{
		"verdict complete":                     {criteriav1.PhaseSucceeded, "handler_complete"},
		"verdict need assistance":              {criteriav1.PhaseSucceeded, "need_assistance"},
		"verdict failed":                       {criteriav1.PhaseFailed, "failed"},
		"succeeded without completing verdict": {criteriav1.PhaseSucceeded, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			agent, events, _ := agentFixture(t)
			status := criteriav1.CriteriaRunStatus{Phase: tc.phase, FinalState: tc.finalState}
			payload := terminalPayload(&status)
			require.NotNil(t, payload, "terminal mapping must produce an event payload")
			require.True(t, agent.bridgeStatus(ctx, "run-map", decisionFixture().WorkflowName, 1, &status),
				"terminal status releases the lane")
			require.Equal(t, 1, events.count())
			_, correlation, payloadType := splitEventCall(events.recorded()[0])
			assert.Equal(t, fmt.Sprintf("%s-1", castle.CorrelationRunTerminal), correlation)
			assert.Equal(t, fmt.Sprintf("%T", payload), payloadType)
		})
	}
}

func TestAgentBridgeTerminalRunFailedCarriesReason(t *testing.T) {
	agent, events, _ := agentFixture(t)
	status := criteriav1.CriteriaRunStatus{Phase: criteriav1.PhaseFailed, FinalState: "need_assistance"}
	payload := terminalPayload(&status)
	failed, ok := payload.(*v1.RunFailed)
	require.True(t, ok, "Failure phase maps to RunFailed: %T", payload)
	assert.Contains(t, failed.Reason, "need_assistance (human gate)")
	require.True(t, agent.bridgeStatus(context.Background(), "run-fail", decisionFixture().WorkflowName, 1, &status))
	require.Equal(t, 1, events.count())
}

func TestAgentBridgeRecordLostClosesWithError(t *testing.T) {
	agent, events, _ := agentFixture(t)
	// The record vanished (manual deletion): bridgeStep closes the row as
	// failed — the submit succeeds, so the lane releases instead of wedging.
	assert.True(t, agent.bridgeStep(context.Background(), "never-created", "run-lost", decisionFixture().WorkflowName, 1))
	require.Equal(t, 1, events.count())
	runID, _, payloadType := splitEventCall(events.recorded()[0])
	assert.Equal(t, "run-lost", runID)
	assert.Equal(t, fmt.Sprintf("%T", runFailedPayload("x")), payloadType)
}

func TestAgentDeliverReleasesOnlyOnTerminal(t *testing.T) {
	agent, events, cl := agentFixture(t)
	mustCreateRun(t, agent, cl, "run-release", criteriav1.CriteriaRunStatus{
		Phase:      criteriav1.PhaseSucceeded,
		FinalState: "handler_complete",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	agent.deliver(ctx, "run-release", "run-release", decisionFixture().WorkflowName, 1)
	require.Eventually(t, func() bool { return agent.terminalSeen("run-release") },
		time.Second, 2*time.Millisecond, "terminal CR must release the delivery lane")
	assert.Equal(t, 1, events.count(), "exactly one terminal event")

	agent.deliver(ctx, "run-release", "run-release", decisionFixture().WorkflowName, 1)
	assert.Equal(t, 1, events.count(), "second delivery reuses terminalSeen instead of re-submitting")
}

func TestAgentConsumeFiltersOutOfScope(t *testing.T) {
	agent, _, _ := agentFixture(t)
	foreign := assignmentFixture("r-out")
	foreign.Labels = map[string]string{DefaultLabelProject: "pollux"}

	sink := newStubSink(assignmentFixture("r-in"), foreign)
	err := agent.consume(context.Background(), sink)
	require.True(t, err == nil || errors.Is(err, io.EOF),
		"exhausted sink ends consume with EOF, not an error lane: %v", err)
	assert.Equal(t, 1, len(agent.queue), "out-of-project assignments drop before enqueue")
}

func TestAgentQueueBackpressureBlocks(t *testing.T) {
	agent, _, _ := agentFixture(t)
	agent.queue <- &v1.ControlMessage{}
	select {
	case agent.queue <- &v1.ControlMessage{}:
		t.Fatal("queue capacity 1 must backpressure while a run is held")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestAgentSerialProcessing(t *testing.T) {
	agent, _, cl := agentFixture(t)
	agent.events = nil
	agent.dispositions = nil // stamp-only: process never blocks on the bridge

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	release := make(chan struct{})
	go agent.processLoop(ctx)
	filled := make(chan struct{})
	go func() {
		defer close(filled)
		agent.queue <- &v1.ControlMessage{Command: &v1.ControlMessage_WorkflowAssignment{WorkflowAssignment: assignmentFixture("run-serial-1")}}
		agent.queue <- &v1.ControlMessage{Command: &v1.ControlMessage_WorkflowAssignment{WorkflowAssignment: assignmentFixture("run-serial-2")}}
	}()
	go func() {
		defer close(release)
		require.Eventually(t, func() bool { return len(listCriteriaRuns(t, cl)) == 2 },
			3*time.Second, 5*time.Millisecond)
	}()
	select {
	case <-release:
	case <-ctx.Done():
		t.Fatal("serial processor did not drain both assignments")
	}
}

func TestAgentAttachPreProvisionedRequiresCriteriaID(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, criteriav1.AddToScheme(scheme))
	agent := NewAssignmentAgent(AgentConfig{Addr: "http://castle-test:8080", Token: "pre-shared", Project: "castor"},
		&RunBuilder{}, "criteria-jobs", nil, nil, nil, logr.Discard())
	_, err := agent.attach(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "criteria-id")
}

func TestAgentAttachRegisterFallbackMintsAndSwapsToken(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, criteriav1.AddToScheme(scheme))
	var captured *v1.RegisterRequest
	agent := NewAssignmentAgent(AgentConfig{Addr: "http://castle-test:8080", Name: "operator-castor", Project: "castor"},
		&RunBuilder{}, "criteria-jobs", nil, nil, nil, logr.Discard())
	agent.register = func(ctx context.Context, req *v1.RegisterRequest) (*v1.RegisterResponse, error) {
		captured = req
		return &v1.RegisterResponse{CriteriaId: "agent-x", Token: "one-time-token"}, nil
	}

	identity, err := agent.attach(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "agent-x", identity.CriteriaID)
	assert.Equal(t, "one-time-token", agent.dialer.currentToken(),
		"register fallback must swap the dialer to the minted one-time token")
	require.NotNil(t, captured)
	assert.Equal(t, "castor", captured.GetLabels()[DefaultLabelProject],
		"register always carries the project label (lease gate)")
}

func TestHeartbeatLoopBeats(t *testing.T) {
	agent, _, _ := agentFixture(t)
	var mu sync.Mutex
	beats := 0
	agent.heartbeat = func(ctx context.Context, req *v1.HeartbeatRequest) error {
		mu.Lock()
		defer mu.Unlock()
		beats++
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		agent.heartbeatLoop(ctx)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("heartbeatLoop did not stop with ctx")
	}
	mu.Lock()
	defer mu.Unlock()
	require.GreaterOrEqual(t, beats, 1, "heartbeat must beat at least once (beats before the first tick)")
	agent.mu.Lock()
	defer agent.mu.Unlock()
	assert.Equal(t, "agent-a", agent.identity.CriteriaID, "heartbeat carries the resolved identity")
}
