// Package control: the assignment agent lane (KB-338 P0.2) — agent.go
// carries the Control consumer, the serial processor, and CR stamping;
// the bridge (the desired-run record's lifecycle feed) is agentbridge.go.
//
// The operator consumes the Control-stream WorkflowAssignments for ITS
// project as the executing agent — the executor half of the end-to-end
// dispatch fix. Where the KB-319 Window/Stream lane is observational (it
// never writes), this lane IS the leased agent:
//
//   - identity: a pre-provisioned criteria agent token (heartbeats keep the
//     overseer "online", which is what makes castle's server-side dispatch
//     lease queued work to it), or a per-boot Register fallback
//   - consume: the Control stream is the same assignment shape the shadow
//     stream decodes (assignSink); assignments enqueue onto ONE serial
//     processor — 1 run per replica (the dave serial rule). Castle holds
//     at most one unstarted lease per agent and re-delivers the rest as
//     they rotate; the queue's blocking send keeps that backlog in
//     castle's queue, not this replica's memory
//   - stamp: the assignment translates to a Decision (routing labels +
//     DispatchEnvelope payload) and stamps the CriteriaRun through the
//     SAME shared runspec path the watcher stamps live runs with — the run
//     pod shape is unchanged, and the run pods keep operator-scoped creds
//   - record: the CR carries criteria.brokenbots.dev/assignment-*
//     annotations (run id, agent, attempts) and the assignment's castle
//     run row is the desired-run record; the bridge feeds that row
//     lifecycle envelopes (RunStarted on Running; terminal on the CR's
//     terminal phase), gated on GetAssignmentDisposition so a run the
//     agent no longer owns is never touched
//
// Kill switch: the lane is OFF unless the operator is explicitly configured
// with AgentConfig (cmd/operator wires it behind --assignment-agent /
// CRITERIA_ASSIGNMENT_AGENT). Default-off is the shadow posture: nothing
// dispatches to the operator agent until the flag flips.
package control

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"time"

	v1 "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	connect "connectrpc.com/connect"
	"github.com/go-logr/logr"
	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/castle"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CR annotations the agent lane stamps on the CriteriaRuns it creates —
// the desired-run record's provenance: which castle assignment produced
// the run, under which agent identity, at which delivery attempt. The
// attempts annotation also keys the bridge's correlation ids, so each
// delivery's lifecycle envelopes dedupe independently.
const (
	// AnnAssignmentRunID is the assignment's castle run id.
	AnnAssignmentRunID = "criteria.brokenbots.dev/assignment-run-id"
	// AnnAssignmentAgent is the criteria id the assignment was dispatched to.
	AnnAssignmentAgent = "criteria.brokenbots.dev/assignment-agent"
	// AnnAssignmentAttempts counts deliveries of this assignment the agent
	// has seen (a re-delivery after a restart increments it).
	AnnAssignmentAttempts = "criteria.brokenbots.dev/assignment-attempts"
)

// AgentConfig configures the assignment agent lane.
type AgentConfig struct {
	// Addr is the castle Connect endpoint. Empty disables the lane.
	Addr string
	// Token is the pre-provisioned criteria agent token. Empty falls back
	// to a per-boot Register (a NEW overseer row each boot; its leases age
	// out under their own identity when the connection ends — castle's
	// offline-expiry is the cleanup).
	Token string
	// CriteriaID is the pre-provisioned overseer id the token belongs to.
	// Required with Token; the Register fallback ignores it.
	CriteriaID string
	// Name is the fallback Register's display name.
	Name string
	// Labels are the fallback Register's registered labels. Castle leases
	// an assignment to an agent only when every assignment label matches
	// one of the agent's registered labels, so these MUST saturate this
	// project's routing vocabulary — the wiring computes them from Project.
	Labels map[string]string
	// Project is the operator's own project: the routing scope this agent
	// processes assignments for. Assignments whose project label names a
	// different project are never executed. Empty executes everything
	// dispatched to this identity.
	Project string
	// HeartbeatEvery is the heartbeat cadence castle's online-expiry
	// evaluates against. Zero defaults to 30s — comfortably inside
	// castle's offline marking horizon.
	HeartbeatEvery time.Duration
}

// Disabled reports whether the agent lane is configured off (empty Addr).
func (c AgentConfig) Disabled() bool { return c.Addr == "" }

// HeartbeatInterval resolves the heartbeat cadence.
func (c AgentConfig) HeartbeatInterval() time.Duration {
	if c.HeartbeatEvery <= 0 {
		return 30 * time.Second
	}
	return c.HeartbeatEvery
}

// controlConfig renders the shadow Config the Translate contract takes
// (the agent lane shares the assignment-decoding vocabulary).
func (c AgentConfig) controlConfig() Config {
	return Config{Addr: c.Addr, Token: c.Token, CriteriaID: c.CriteriaID}
}

// AgentIdentity is one resolved agent identity: the criteria id and token
// stream, heartbeat, and event submissions authenticate with.
type AgentIdentity struct {
	CriteriaID string
	Token      string
}

// agentDialer is the Control-stream dialer capable of swapping its token
// after the Register fallback mints a fresh one. Each dial rebuilds the
// client, so a swapped token takes effect on the next connect.
type agentDialer struct {
	mu sync.Mutex
	// addr is the castle Connect endpoint; criteriaID the request-supplied
	// identity fallback (castle resolves the authenticated caller when the
	// auth interceptor is wired — the deployed shape); token the bearer.
	addr       string
	criteriaID string
	token      string
}

func (d *agentDialer) swapToken(token string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.token = token
}

func (d *agentDialer) currentToken() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.token
}

func (d *agentDialer) dial(ctx context.Context) (assignSink, error) {
	d.mu.Lock()
	addr, criteriaID, token := d.addr, d.criteriaID, d.token
	d.mu.Unlock()
	svc := castle.NewAgentClient(castle.Config{Addr: addr, Token: token}, nil)
	stream, err := svc.Control(ctx, connect.NewRequest(
		&v1.ControlSubscribeRequest{CriteriaId: criteriaID}))
	if err != nil {
		return nil, err
	}
	return connectSink{stream: stream}, nil
}

// AssignmentAgent consumes dispatched assignments for one project and
// stamps their CriteriaRuns. Serial by construction: one assignment is
// processed to terminal (CR bridge included) before the next; further
// deliveries queue on the processor (1 run per replica).
type AssignmentAgent struct {
	cfg          AgentConfig
	builder      *RunBuilder
	namespace    string
	client       client.Client
	events       EventSubmitter
	dispositions DispositionReader

	// connect/test seams: NewAssignmentAgent installs the nil defaults.
	register  func(ctx context.Context, req *v1.RegisterRequest) (*v1.RegisterResponse, error)
	heartbeat func(ctx context.Context, req *v1.HeartbeatRequest) error
	dial      func(ctx context.Context) (assignSink, error)
	dialer    *agentDialer

	// pollEvery is the bridge's CR-status poll interval (default 2s).
	pollEvery time.Duration

	// identity is the resolved agent identity: stamped on CRs and compared
	// in the disposition gate. Run sets it via attach; tests seed it.
	identity AgentIdentity

	// queue carries fresh assignments to the serial processor. Capacity 1
	// is the backpressure boundary: a full queue blocks the receive, so
	// the runtime backlog is castle's queue, not this replica's memory.
	queue chan *v1.ControlMessage

	log logr.Logger

	// bridge ledger: per assignment run id, which lifecycle events have
	// been acked, so a re-delivery never re-emits them (CRI-142: terminal
	// never rewrites terminal; dedupe makes re-submits safe, the ledger
	// keeps them off the wire in the first place).
	mu       sync.Mutex
	started  map[string]bool
	terminal map[string]bool
}

// NewAssignmentAgent builds the agent lane.
//
// The identity is the pre-provisioned agent when cfg.Token is set (the
// credentials must be provisioned for the operator's overseer identity in
// castle); without it the lane falls back to a per-boot Register. Nil
// events/dispositions degrades the bridge to CR-stamping only (logged —
// the desired-run row closes only at castle's expiry); a nil client has
// nothing to stamp — wire both.
func NewAssignmentAgent(cfg AgentConfig, builder *RunBuilder, ns string, cl client.Client, events EventSubmitter, dispositions DispositionReader, log logr.Logger) *AssignmentAgent {
	d := &agentDialer{addr: cfg.Addr, criteriaID: cfg.CriteriaID, token: cfg.Token}
	a := &AssignmentAgent{
		cfg:          cfg,
		builder:      builder,
		namespace:    ns,
		client:       cl,
		events:       events,
		dispositions: dispositions,
		dialer:       d,
		dial:         d.dial,
		pollEvery:    2 * time.Second,
		queue:        make(chan *v1.ControlMessage, 1),
		log:          log,
		started:      map[string]bool{},
		terminal:     map[string]bool{},
	}
	a.register = a.defaultRegister
	a.heartbeat = a.defaultHeartbeat
	return a
}

func (a *AssignmentAgent) defaultRegister(ctx context.Context, req *v1.RegisterRequest) (*v1.RegisterResponse, error) {
	svc := castle.NewAgentClient(castle.Config{Addr: a.cfg.Addr}, nil)
	resp, err := svc.Register(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (a *AssignmentAgent) defaultHeartbeat(ctx context.Context, req *v1.HeartbeatRequest) error {
	// The heartbeat MUST carry the current token: after a Register fallback
	// the minted one-time token is the only credential that authenticates
	// the fresh identity.
	svc := castle.NewAgentClient(castle.Config{Addr: a.cfg.Addr, Token: a.dialer.currentToken()}, nil)
	_, err := svc.Heartbeat(ctx, connect.NewRequest(req))
	return err
}

// attach resolves the agent identity. The pre-provisioned token path does
// not verify locally — castle rejects an unusable token at first use and
// the lane surfaces that as the shared lane-rejection disable. The Register
// fallback creates a fresh overseer per boot and swaps its one-time token
// into the defaults (heartbeat + dial).
func (a *AssignmentAgent) attach(ctx context.Context) (AgentIdentity, error) {
	if a.cfg.Token != "" {
		if a.cfg.CriteriaID == "" {
			return AgentIdentity{}, errors.New("assignment agent: criteria-id required with a pre-provisioned agent token")
		}
		a.identity = AgentIdentity{CriteriaID: a.cfg.CriteriaID, Token: a.cfg.Token}
		return a.identity, nil
	}
	labels := map[string]string{}
	for k, v := range a.cfg.Labels {
		labels[k] = v
	}
	// The project routing label MUST be registered for the lease gate to
	// ever hand this agent the project's work.
	if a.cfg.Project != "" && a.cfg.Labels[DefaultLabelProject] == "" {
		labels[DefaultLabelProject] = a.cfg.Project
	}
	resp, err := a.register(ctx, &v1.RegisterRequest{Name: a.cfg.Name, Labels: labels})
	if err != nil {
		return AgentIdentity{}, fmt.Errorf("assignment agent register: %w", err)
	}
	a.dialer.swapToken(resp.GetToken())
	a.identity = AgentIdentity{CriteriaID: resp.GetCriteriaId(), Token: resp.GetToken()}
	a.log.Info("assignment agent registered (per-boot identity)", "criteriaId", a.identity.CriteriaID, "name", a.cfg.Name, "labels", labels)
	return a.identity, nil
}

// Run serves the lane until ctx is done: attach, heartbeat loop, serial
// processor, Control consume. Transport failures reconnect with the shadow
// stream's bounded backoff; an identity rejection disables the lane loudly
// (same classification as the shadow stream).
func (a *AssignmentAgent) Run(ctx context.Context) error {
	if a.cfg.Disabled() {
		return errors.New("assignment agent: castle address required")
	}
	if _, err := a.attach(ctx); err != nil {
		return err
	}

	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		a.heartbeatLoop(ctx)
	}()
	go a.processLoop(ctx)

	backoff := backoffBase
	defer func() { <-heartbeatDone }()
	for {
		sink, err := a.dial(ctx)
		if err == nil && sink == nil {
			// A dialer returning neither stream nor error is the dialer's
			// bug; retry through the transport-failure path rather than a
			// nil dereference.
			err = errors.New("dial returned no stream and no error")
		}
		if err != nil {
			if !isLaneRejected(err) {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return ctxErr
				}
				a.log.Error(err, "assignment agent: castle control dial failed; retrying", "addr", a.cfg.Addr)
				if !sleepCtx(ctx, backoff) {
					return ctx.Err()
				}
				backoff = nextBackoff(backoff)
				continue
			}
			return a.rejectDisable(err)
		}
		backoff = backoffBase
		if err := a.consume(ctx, sink); err != nil {
			sink.close()
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			if isLaneRejected(err) {
				return a.rejectDisable(err)
			}
			// The subscribe is lazy (connect resolves the server stream at
			// the first Receive), so an identity rejection surfaces here —
			// both phases classify it.
			if errors.Is(err, io.EOF) {
				a.log.Info("assignment agent: castle control stream ended; reconnecting", "addr", a.cfg.Addr)
			} else {
				a.log.Error(err, "assignment agent: control stream dropped; reconnecting", "addr", a.cfg.Addr)
			}
			if !sleepCtx(ctx, backoff) {
				return ctx.Err()
			}
			backoff = nextBackoff(backoff)
		}
	}
}

// rejectDisable emits the shared lane-rejection disable log and returns the
// error for the caller to surface.
func (a *AssignmentAgent) rejectDisable(err error) error {
	a.log.Error(err, "assignment agent disabled: castle rejected this identity on the Control stream (agent-owned lane); a criteria agent token for the operator's overseer identity is required, or leave the lane off")
	return err
}

// heartbeatLoop keeps the overseer online while ctx lives — beat BEFORE the
// first tick: the very first lease-eligibility window depends on it. A
// failed beat is logged and retried next tick; expiry, not a single failed
// beat, is what removes the agent from dispatch.
func (a *AssignmentAgent) heartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(a.cfg.HeartbeatInterval())
	defer ticker.Stop()
	for {
		if err := a.heartbeat(ctx, &v1.HeartbeatRequest{CriteriaId: a.identity.CriteriaID}); err != nil {
			if ctx.Err() != nil {
				return
			}
			a.log.Error(err, "assignment agent: heartbeat failed; retrying next tick", "criteriaId", a.identity.CriteriaID)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// processLoop is the serial executor: one assignment fully processed (CR
// created, bridge terminalized) before the next. A processing failure is
// logged, never fatal: the assignment's castle row stays under its lease
// until expiry (castle's own recovery) and the lane keeps serving.
func (a *AssignmentAgent) processLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-a.queue:
			a.process(ctx, msg.GetWorkflowAssignment())
		}
	}
}

// consume decodes one live Control stream into the work queue. The send is
// BLOCKING: while the serial processor holds a run, the receive stalls and
// castle's server-side lease queue is the backlog.
// Non-assignment commands are ignored (CRI-60: clients process only the
// assigned work).
func (a *AssignmentAgent) consume(ctx context.Context, sink assignSink) error {
	for {
		msg, err := sink.next()
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		asg := msg.GetWorkflowAssignment()
		if asg == nil {
			continue
		}
		if !a.inScope(a.decisionScope(asg)) {
			a.log.Info("assignment agent: assignment outside this agent's routing scope; not executing",
				"run", asg.GetRunId(), "workflow", asg.GetWorkflowName(), "labels", asg.GetLabels(), "project", a.cfg.Project)
			continue
		}
		select {
		case a.queue <- msgFor(asg):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// decisionScope derives only the routing-relevant part of the decision:
// the project label. Out-of-scope assignments are dropped BEFORE enqueue —
// they must not occupy the serial lane.
func (a *AssignmentAgent) decisionScope(asg *v1.WorkflowAssignment) Decision {
	return Translate(a.cfg.controlConfig(), Assignment{
		RunID:          asg.GetRunId(),
		WorkflowName:   asg.GetWorkflowName(),
		WorkflowSource: asg.GetWorkflowSource(),
		LockfileSource: asg.GetLockfileSource(),
		Labels:         asg.GetLabels(),
	})
}

// inScope filters assignments to this agent's project label. An empty
// project label means the assignment carried no routing constraint (an
// empty label set matches every agent at the lease gate) — executing it is
// the only way the row ever closes.
func (a *AssignmentAgent) inScope(d Decision) bool {
	if a.cfg.Project == "" {
		return true
	}
	return d.Project == "" || d.Project == a.cfg.Project
}

// translate derives the stamping decision: the label vocabulary first, then
// the dispatch envelope (the richer per-ticket contract) overrides and
// fills. A non-envelope payload (plain HCL workflow source — possible on a
// queue not yet carrying dispatch envelopes) fails closed in the caller:
// an un-attributable assignment must not stamp a run.
func (a *AssignmentAgent) translate(asg *v1.WorkflowAssignment) (Decision, DispatchEnvelope) {
	d := a.decisionScope(asg)
	source := asg.GetWorkflowSource()
	if source == "" {
		d.Missing = append(d.Missing, "payload: workflow_source empty — no dispatch envelope, no HCL source")
		return d, DispatchEnvelope{}
	}
	env, err := ParseDispatchEnvelope(source)
	if err != nil {
		d.Missing = append(d.Missing, fmt.Sprintf("payload: %v", err))
		return d, DispatchEnvelope{}
	}
	d.Ticket = env.Ticket
	d.RepoURL = env.Repo
	d.Slot = env.Slot
	if env.Class != "" {
		d.Class = env.Class
	}
	d.Missing = decisionMissing(d)
	return d, env
}

// decisionMissing re-derives the required-decision-field list AFTER the
// envelope merge (envelope fields satisfy the identity checks the label
// pass made — ticket and repo do not ride assignment labels).
func decisionMissing(d Decision) []string {
	var missing []string
	for _, miss := range []struct{ have, what string }{
		{d.Ticket, "ticket"},
		{d.RepoURL, "repo"},
		{d.Class, "class"},
		{d.WorkflowName, "workflow_name"},
	} {
		if miss.have == "" {
			missing = append(missing, miss.what)
		}
	}
	return missing
}

// msgFor wraps an assignment into its Control message (the queue is typed
// on the message; only assignments pass).
func msgFor(asg *v1.WorkflowAssignment) *v1.ControlMessage {
	return &v1.ControlMessage{Command: &v1.ControlMessage_WorkflowAssignment{WorkflowAssignment: asg}}
}

// process executes one assignment on the serial processor:
//
//  1. translate — fail closed on an un-attributable assignment (close the
//     castle row with RunFailed; never stamp a run from it)
//  2. disposition gate — skip when the assignment is no longer leased to
//     this identity (expired and re-leased elsewhere, re-triaged terminal)
//  3. stamp — the same stamping path as the watcher (RunBuilder),
//     annotated with the assignment provenance; then admit
//  4. bridge — follow the CR to terminal and feed the desired-run row's
//     lifecycle events (agentbridge.go)
func (a *AssignmentAgent) process(ctx context.Context, asg *v1.WorkflowAssignment) {
	if asg == nil || asg.GetRunId() == "" {
		a.log.Error(errors.New("assignment without run id"), "assignment agent: undecodable delivery")
		return
	}
	runID := asg.GetRunId()
	d, env := a.translate(asg)
	failClosedReasons := append([]string(nil), d.Missing...)
	run, stampReasons := a.builder.BuildRun(ctx, d, WithNameSuffix(runID))
	failClosedReasons = append(failClosedReasons, stampReasons...)
	if len(failClosedReasons) > 0 {
		a.failAssignmentClosed(ctx, asg, failClosedReasons)
		return
	}
	// The envelope's per-card target pins merge into the stamped workflow
	// BEFORE admission (the metadata rung lands on the record complete).
	applyTargetMetadata(run, env.TargetImages)
	if !a.ownedByUs(ctx, runID) {
		return
	}
	run, attempts, ok := a.attachRun(ctx, run, asg)
	if !ok {
		return
	}
	a.deliver(ctx, run.Name, runID, d.WorkflowName, attempts)
}

// ownedByUs consults the assignment's disposition: only a run still leased
// to THIS identity proceeds. A read failure is fail-open — castle's own
// per-envelope ownership gate (runs.overseer_id) is the enforcement; this
// check is the courtesy.
func (a *AssignmentAgent) ownedByUs(ctx context.Context, runID string) bool {
	if a.dispositions == nil {
		return true
	}
	resp, err := a.dispositions.Disposition(ctx, runID)
	if err != nil {
		a.log.Error(err, "assignment agent: disposition read failed; proceeding (castle's ownership gate is the enforcement)", "run", runID)
		return true
	}
	if leased := resp.GetLeasedCriteriaId(); leased != "" && leased != a.identity.CriteriaID {
		a.log.Info("assignment agent: assignment no longer leased to this identity; skipping", "run", runID, "leasedTo", leased)
		return false
	}
	if st := resp.GetState(); st == v1.WorkflowAssignmentState_WORKFLOW_ASSIGNMENT_STATE_TERMINAL ||
		st == v1.WorkflowAssignmentState_WORKFLOW_ASSIGNMENT_STATE_REJECTED {
		a.log.Info("assignment agent: assignment already dispositioned; skipping", "run", runID, "state", st.String(), "reason", resp.GetRejectionReason())
		return false
	}
	return true
}

// attachRun returns the run record for this assignment delivery: the
// existing CR on a re-delivery (attempts incremented), or the freshly
// stamped and admitted CR on first delivery.
func (a *AssignmentAgent) attachRun(ctx context.Context, run *criteriav1.CriteriaRun, asg *v1.WorkflowAssignment) (*criteriav1.CriteriaRun, int, bool) {
	if existing := a.findRecordedRun(ctx, asg.GetRunId()); existing != nil {
		attempts := a.markAttempts(ctx, existing)
		a.log.Info("assignment agent: re-delivered assignment — reattaching to the recorded run",
			"run", asg.GetRunId(), "criteriaRun", existing.Name, "attempts", attempts)
		return existing, attempts, true
	}
	if run.Annotations == nil {
		run.Annotations = map[string]string{}
	}
	run.Annotations[AnnAssignmentRunID] = asg.GetRunId()
	run.Annotations[AnnAssignmentAgent] = a.identity.CriteriaID
	run.Annotations[AnnAssignmentAttempts] = "1"
	if err := a.client.Create(ctx, run); err != nil {
		// Same-second name collision from the timestamp seam (rare on the
		// serial lane): the second Create loses, the winner is the record.
		if apierrors.IsAlreadyExists(err) {
			if existing := a.findRecordedRun(ctx, asg.GetRunId()); existing != nil {
				attempts := a.markAttempts(ctx, existing)
				return existing, attempts, true
			}
		}
		a.log.Error(err, "assignment agent: CriteriaRun create failed; assignment stays under lease (castle expiry recovers)",
			"run", asg.GetRunId(), "criteriaRun", run.Name)
		return nil, 0, false
	}
	a.log.Info("assignment agent: run admitted from assignment", "criteriaRun", run.Name, "run", asg.GetRunId(),
		"ticket", run.Spec.TicketID, "project", a.cfg.Project)
	return run, 1, true
}

// findRecordedRun locates the CR this assignment's previous delivery
// created: the provenance annotation is the key. A re-delivery of the SAME
// assignment must reattach, not stamp a second run.
func (a *AssignmentAgent) findRecordedRun(ctx context.Context, runID string) *criteriav1.CriteriaRun {
	if a.client == nil || runID == "" {
		return nil
	}
	var runs criteriav1.CriteriaRunList
	if err := a.client.List(ctx, &runs, client.InNamespace(a.namespace)); err != nil {
		a.log.Error(err, "assignment agent: run-list read failed; treating this delivery as first", "run", runID)
		return nil
	}
	for i := range runs.Items {
		if runs.Items[i].Annotations[AnnAssignmentRunID] == runID {
			r := runs.Items[i]
			return &r
		}
	}
	return nil
}

// markAttempts increments the delivery counter on the record.
func (a *AssignmentAgent) markAttempts(ctx context.Context, run *criteriav1.CriteriaRun) int {
	attempts := 1
	if n, err := strconv.Atoi(run.Annotations[AnnAssignmentAttempts]); err == nil && n > 0 {
		attempts = n
	}
	if run.Annotations == nil {
		run.Annotations = map[string]string{}
	}
	run.Annotations[AnnAssignmentAttempts] = strconv.Itoa(attempts + 1)
	if err := a.client.Update(ctx, run); err != nil {
		a.log.Error(err, "assignment agent: attempts annotation update failed (bridge continues on the read-back copy)", "criteriaRun", run.Name)
	}
	return attempts + 1
}

// applyTargetMetadata merges the envelope's per-card target pins into the
// stamped workflow's adapterImages (the metadata rung of the KB-319
// resolution chain: target_metadata > digest-verified event > binding >
// defaults; conflicts gate on the stamped ImageConflictPolicy at
// admission). Applied inline in the build path so the CR arrives complete.
func applyTargetMetadata(run *criteriav1.CriteriaRun, targets map[string]string) {
	if run == nil || len(targets) == 0 {
		return
	}
	if run.Spec.Workflow.AdapterImages == nil {
		run.Spec.Workflow.AdapterImages = map[string]string{}
	}
	for kind, ref := range targets {
		run.Spec.Workflow.AdapterImages[kind] = ref
	}
}