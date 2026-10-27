// Package proxy implements the operator's run-event proxy (KB-344): a
// criteria CriteriaService the workflow runner's engine dials instead of
// castle. The engine keeps speaking the criteria protocol unchanged, but
// its --server target becomes the operator's svc endpoint (the bootgate
// server), so run pods learn only the operator's address and never receive
// a castle credential. Each engine stream maps onto the CriteriaRun CR
// that owns the pod — the runner passes the CR name via --name — and the
// proxy re-emits envelopes to castle's SubmitEvents under the run's owning
// agent identity, making the run's event stream owner-consistent by
// construction per castle's per-envelope ownership gate (KB-324/330): the
// class rules that denied a per-boot registered engine agent from
// submitting a run it doesn't own stay on; the identity presenting them is
// now the run's owner.
package proxy

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	connect "connectrpc.com/connect"
	v1 "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	control "github.com/brokenbots/workflow-example/criteria-k8s/internal/control"
	jobbuilder "github.com/brokenbots/workflow-example/criteria-k8s/internal/jobbuilder"
	"github.com/go-logr/logr"
	timestamppb "google.golang.org/protobuf/types/known/timestamppb"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// runSubmitter forwards pre-built envelopes onto a castle run's event
// stream under the submit identity the castle.Config of the feed chose.
// *castle.AssignmentEventFeed satisfies it.
type runSubmitter interface {
	SubmitEnvelope(ctx context.Context, env *v1.Envelope) (*v1.Ack, error)
}

// runCreator creates castle runs under the same identity the submitter
// uses (watcher lane: no assignment annotation carries the run id, so the
// proxy mints one and stamps it on the CR). *castle.AssignmentEventFeed
// satisfies it.
type runCreator interface {
	CreateRun(ctx context.Context, req *v1.CreateRunRequest) (*v1.Run, error)
}

// agentEntry is the proxy-minted criteria identity of one run pod's
// engine. Unlike castle, the proxy registers ONLY identities that resolve
// to a live CriteriaRun CR — an engine whose name maps to no run has
// nothing to attribute its stream to and gets no identity at all.
type agentEntry struct {
	criteriaID string // proxy-minted criteria id
	token      string // proxy-minted bearer token, presented on every RPC
	crName     string // CriteriaRun the stream attributes to
}

// runEntry is one engine run: the local run id the engine submits against,
// the agent that owns it, and (resolved lazily, once) the castle run id
// its envelopes land on.
type runEntry struct {
	localID      string // proxy-minted run id the engine sees
	agent        *agentEntry
	workflowName string
	workflowHash string

	mu          sync.Mutex
	castleRunID string // castle run id the envelopes forward onto
}

// Proxy serves the criteria run-event surface for runner Jobs (KB-344).
// cmd/operator mounts it on the bootgate server via criteria's
// NewServiceHandler wrapper; this package stays protocol + k8s only.
type Proxy struct {
	log    logr.Logger
	client client.Client // DIRECT client: attribution must not ride a stale informer view
	ns     string

	submit runSubmitter
	create runCreator

	// now is a test seam for Heartbeat's ServerTime.
	now func() time.Time

	mu           sync.Mutex
	byToken      map[string]*agentEntry // minted token → identity
	byCriteriaID map[string]*agentEntry // minted criteria id → identity
	runs         map[string]*runEntry   // proxy run id → run
}

// Config wires the proxy's dependency edges.
type Config struct {
	Log    logr.Logger
	Client client.Client
	NS     string
	// Submit forwards engine envelopes to castle under the run's owning
	// identity (KB-344). Typed *castle.AssignmentEventFeed in production;
	// the interface keeps tests protocol-local.
	Submit runSubmitter
	// Create mints castle runs under the same identity (watcher lane).
	// Optional: nil keeps the dispatch-lane-only surface.
	Create runCreator
}

// New builds the proxy.
func New(cfg Config) *Proxy {
	return &Proxy{
		log:          cfg.Log,
		client:       cfg.Client,
		ns:           cfg.NS,
		submit:       cfg.Submit,
		create:       cfg.Create,
		now:          time.Now,
		byToken:      make(map[string]*agentEntry),
		byCriteriaID: make(map[string]*agentEntry),
		runs:         make(map[string]*runEntry),
	}
}

// Register implements CriteriaService.Register for run pods. The engine
// registers with its --name (the CriteriaRun the runner carries); the
// proxy issues a criteria identity bound to that CR. No bootstrap-token
// gate is repeated here: the bootgate server terminates in-pod traffic
// inside the operator's own namespace, and the CR-name gate below IS the
// authorization boundary — an unknown name yields no identity.
func (p *Proxy) Register(ctx context.Context, req *connect.Request[v1.RegisterRequest]) (*connect.Response[v1.RegisterResponse], error) {
	name := req.Msg.GetName()
	if name == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("proxy: name is required (the runner passes the owning CriteriaRun)"))
	}
	cr := &criteriav1.CriteriaRun{}
	if err := p.client.Get(ctx, client.ObjectKey{Namespace: p.ns, Name: name}, cr); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, connect.NewError(connect.CodePermissionDenied,
				fmt.Errorf("proxy: unknown CriteriaRun %s/%s", p.ns, name))
		}
		return nil, connect.NewError(connect.CodeInternal,
			fmt.Errorf("proxy: reading CriteriaRun %s/%s: %w", p.ns, name, err))
	}

	agent, err := mintAgent(name)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	p.mu.Lock()
	p.byToken[agent.token] = agent
	p.byCriteriaID[agent.criteriaID] = agent
	p.mu.Unlock()
	p.log.Info("run-event proxy: registered engine identity", "criteriaRun", name)
	return connect.NewResponse(&v1.RegisterResponse{CriteriaId: agent.criteriaID, Token: agent.token}), nil
}

// Heartbeat implements CriteriaService.Heartbeat: an identity check plus a
// server time — the proxy holds no lease state.
func (p *Proxy) Heartbeat(ctx context.Context, req *connect.Request[v1.HeartbeatRequest]) (*connect.Response[v1.HeartbeatResponse], error) {
	agent, err := p.authenticate(req.Header())
	if err != nil {
		return nil, err
	}
	if req.Msg.GetCriteriaId() != "" && req.Msg.GetCriteriaId() != agent.criteriaID {
		return nil, connect.NewError(connect.CodePermissionDenied,
			errors.New("proxy: heartbeat criteria id does not match the authenticated agent"))
	}
	return connect.NewResponse(&v1.HeartbeatResponse{ServerTime: timestamppb.New(p.now().UTC())}), nil
}

// CreateRun implements CriteriaService.CreateRun. The caller's identity
// comes from the auth header (castle's caller-owns-the-run rule); the
// request's criteria_id is advisory. The proxy mints a LOCAL run id for
// the engine to submit against; the castle run id resolves lazily at the
// first envelope (dispatch-lane annotation read; watcher-lane castle
// create + CR stamp), so a transient k8s or castle blip kills one stream
// instead of the engine's whole run.
func (p *Proxy) CreateRun(ctx context.Context, req *connect.Request[v1.CreateRunRequest]) (*connect.Response[v1.Run], error) {
	agent, err := p.authenticate(req.Header())
	if err != nil {
		return nil, err
	}
	localID, err := newRandomID("run-")
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("proxy: minting run id: %w", err))
	}

	entry := &runEntry{
		localID:      localID,
		agent:        agent,
		workflowName: req.Msg.GetWorkflowName(),
		workflowHash: req.Msg.GetWorkflowHash(),
	}
	p.mu.Lock()
	p.runs[entry.localID] = entry
	p.mu.Unlock()
	p.log.Info("run-event proxy: minted run for engine",
		"criteriaRun", agent.crName, "proxyRunId", entry.localID, "workflow", entry.workflowName)
	return connect.NewResponse(&v1.Run{
		RunId:        entry.localID,
		CriteriaId:   agent.criteriaID,
		WorkflowName: entry.workflowName,
		WorkflowHash: entry.workflowHash,
		Status:       "running",
	}), nil
}

// ReattachRun implements CriteriaService.ReattachRun: lookup-only. The
// proxy holds no replayable state, so a run the proxy saw is reported
// running with no sequence to resume from; an unknown run id is NotFound,
// which is also what a pod restarting against a restarted operator sees.
func (p *Proxy) ReattachRun(ctx context.Context, req *connect.Request[v1.ReattachRunRequest]) (*connect.Response[v1.ReattachRunResponse], error) {
	agent, err := p.authenticate(req.Header())
	if err != nil {
		return nil, err
	}
	entry := p.runByID(req.Msg.GetRunId())
	switch {
	case entry == nil:
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("proxy: unknown run %q", req.Msg.GetRunId()))
	case entry.agent != agent:
		return nil, connect.NewError(connect.CodePermissionDenied,
			errors.New("proxy: run belongs to a different agent"))
	}
	return connect.NewResponse(&v1.ReattachRunResponse{Status: "running"}), nil
}

// Resume implements CriteriaService.Resume. The proxy holds nothing
// paused: accepting keeps a server-mode run moving. Human gates stay on
// the local need_assistance path — the run terminates and the operator
// verdicts the CR.
func (p *Proxy) Resume(ctx context.Context, req *connect.Request[v1.ResumeRequest]) (*connect.Response[v1.ResumeResponse], error) {
	agent, err := p.authenticate(req.Header())
	if err != nil {
		return nil, err
	}
	entry := p.runByID(req.Msg.GetRunId())
	switch {
	case entry == nil:
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("proxy: unknown run %q", req.Msg.GetRunId()))
	case entry.agent != agent:
		return nil, connect.NewError(connect.CodePermissionDenied,
			errors.New("proxy: run belongs to a different agent"))
	}
	return connect.NewResponse(&v1.ResumeResponse{Accepted: true, Reason: "ok"}), nil
}

// SubmitEvents implements CriteriaService.SubmitEvents — the proxy's core.
// Envelopes arrive under their LOCAL run id, are re-emitted to castle with
// the castle run id, and are acked with the local id plus castle's
// server-assigned seq, so the engine's bookkeeping sees exactly what a
// direct-castle server would say, minus the run re-identification. Per the
// handler contract, the first envelope the agent does not own — or that
// has no known run — kills the stream; nothing drops silently.
func (p *Proxy) SubmitEvents(ctx context.Context, stream *connect.BidiStream[v1.Envelope, v1.Ack]) error {
	agent, err := p.authenticate(stream.RequestHeader())
	if err != nil {
		return err
	}
	for {
		env, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			p.log.Error(err, "run-event proxy: engine stream receive failed", "criteriaRun", agent.crName)
			return connect.NewError(connect.CodeUnknown, err)
		}
		entry := p.runByID(env.GetRunId())
		if entry == nil || entry.agent != agent {
			return connect.NewError(connect.CodePermissionDenied,
				fmt.Errorf("proxy: agent does not own run %q", env.GetRunId()))
		}
		if _, serverOnly := env.Payload.(*v1.Envelope_WatchReady); serverOnly {
			// Castle rejects these server-synthesised payloads with
			// InvalidArgument; a proxy that produced one would fail every
			// forward anyway, so reject the send loudly here.
			return connect.NewError(connect.CodeInvalidArgument,
				errors.New("proxy: watch.ready is server-only and cannot be submitted"))
		}

		castleRunID, err := p.castleRunID(ctx, entry, env.GetRunId())
		if err != nil {
			return connect.NewError(connect.CodeFailedPrecondition, err)
		}

		forwarded := proto.Clone(env).(*v1.Envelope)
		forwarded.RunId = castleRunID
		ack, err := p.submit.SubmitEnvelope(ctx, forwarded)
		if err != nil {
			p.log.Error(err, "run-event proxy: castle forward failed",
				"criteriaRun", agent.crName, "proxyRunId", env.GetRunId(), "castleRunId", castleRunID)
			return connect.NewError(connect.CodeUnavailable, err)
		}
		seq := env.GetSeq()
		if ack != nil {
			seq = ack.GetSeq()
		}
		if err := stream.Send(&v1.Ack{RunId: env.GetRunId(), Seq: seq, CorrelationId: env.GetCorrelationId()}); err != nil {
			return connect.NewError(connect.CodeUnknown, err)
		}
	}
}

// Control implements CriteriaService.Control. Mirror castle's ControlReady
// handshake first — castle sends it immediately on subscribe so the
// Connect server flushes response headers, and the engine waits for it
// before trusting the stream — then park: the operator is the control
// plane, and run pods receive no castle dispatch. That surface is the
// KB-344 protocol ruling's whole point.
func (p *Proxy) Control(ctx context.Context, req *connect.Request[v1.ControlSubscribeRequest], stream *connect.ServerStream[v1.ControlMessage]) error {
	agent, err := p.authenticate(req.Header())
	if err != nil {
		return err
	}
	if req.Msg.GetCriteriaId() != "" && req.Msg.GetCriteriaId() != agent.criteriaID {
		return connect.NewError(connect.CodePermissionDenied,
			errors.New("proxy: control criteria id does not match the authenticated agent"))
	}
	if err := stream.Send(&v1.ControlMessage{Command: &v1.ControlMessage_ControlReady{ControlReady: &v1.ControlReady{}}}); err != nil {
		return connect.NewError(connect.CodeUnknown, err)
	}
	<-ctx.Done()
	return nil
}

// castleRunID resolves the castle run id a proxy run's envelopes land on.
// Dispatch lane: the assignment bridge's annotation. Watcher lane: create
// the castle run under the owning identity now and stamp the CR so the
// controller observes it without the discovery pass. The value is computed
// once per run and cached: during a live run the CR's castle identity does
// not change (the bridge re-stamps attempts only before re-dispatch, never
// mid-run).
func (p *Proxy) castleRunID(ctx context.Context, entry *runEntry, proxyRunID string) (string, error) {
	entry.mu.Lock()
	cached := entry.castleRunID
	entry.mu.Unlock()
	if cached != "" {
		return cached, nil
	}

	cr := &criteriav1.CriteriaRun{}
	if err := p.client.Get(ctx, client.ObjectKey{Namespace: p.ns, Name: entry.agent.crName}, cr); err != nil {
		return "", fmt.Errorf("proxy: reading CriteriaRun %s/%s: %w", p.ns, entry.agent.crName, err)
	}
	resolved := cr.GetAnnotations()[control.AnnAssignmentRunID]
	if resolved == "" && p.create != nil {
		created, err := p.create.CreateRun(ctx, &v1.CreateRunRequest{
			WorkflowName: entry.workflowName,
			WorkflowHash: entry.workflowHash,
		})
		if err != nil {
			return "", fmt.Errorf("proxy: creating castle run for %s/%s: %w", p.ns, entry.agent.crName, err)
		}
		resolved = created.GetRunId()
		p.stampAnnotation(ctx, entry.agent.crName, jobbuilder.AnnotationProxyCastleRunID, resolved)
	}
	if resolved == "" {
		return "", fmt.Errorf("proxy: run %s/%s carries no castle run id (neither the assignment annotation nor a create lane)", p.ns, entry.agent.crName)
	}
	entry.mu.Lock()
	entry.castleRunID = resolved
	entry.mu.Unlock()
	p.log.Info("run-event proxy: attributed run stream",
		"criteriaRun", entry.agent.crName, "proxyRunId", proxyRunID, "castleRunId", resolved)
	return resolved, nil
}

// stampAnnotation merges one annotation onto the run's CR. Best-effort:
// the proxy's event path does not gate on the stamp (the controller
// observes it whenever it lands; missed stamps re-stamp on reconnect).
func (p *Proxy) stampAnnotation(ctx context.Context, crName, key, value string) {
	cr := &criteriav1.CriteriaRun{}
	if err := p.client.Get(ctx, client.ObjectKey{Namespace: p.ns, Name: crName}, cr); err != nil {
		p.log.Error(err, "run-event proxy: reading CriteriaRun for annotation stamp", "criteriaRun", crName)
		return
	}
	base := cr.DeepCopy()
	if cr.Annotations == nil {
		cr.Annotations = map[string]string{}
	}
	cr.Annotations[key] = value
	if err := p.client.Patch(ctx, cr, client.MergeFrom(base)); err != nil {
		p.log.Error(err, "run-event proxy: stamping run annotation", "criteriaRun", crName, "key", key)
	}
}

func (p *Proxy) runByID(id string) *runEntry {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.runs[id]
}

// authenticate resolves the agent identity from the request headers,
// mirroring castle's TokenFromHeaders precedence (Authorization bearer,
// then X-Criteria-Token).
func (p *Proxy) authenticate(h http.Header) (*agentEntry, error) {
	token := ""
	if v := h.Get("Authorization"); v != "" {
		if rest, ok := strings.CutPrefix(v, "Bearer "); ok && rest != "" {
			token = rest
		}
	}
	if token == "" {
		token = h.Get("X-Criteria-Token")
	}
	if token == "" {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("proxy: no criteria token"))
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	agent, ok := p.byToken[token]
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("proxy: unknown criteria token"))
	}
	return agent, nil
}

// mintAgent issues the criteria identity for one engine (name = the CriteriaRun).
func mintAgent(crName string) (*agentEntry, error) {
	criteriaID, err := newRandomID("criteria-")
	if err != nil {
		return nil, fmt.Errorf("proxy: minting criteria id: %w", err)
	}
	token, err := newRandomID("crtoken-")
	if err != nil {
		return nil, fmt.Errorf("proxy: minting token: %w", err)
	}
	return &agentEntry{criteriaID: criteriaID, token: token, crName: crName}, nil
}

// newRandomID returns a prefix + 128 bits of crypto/rand hex. Tokens ride
// request headers, so the entropy bar is the auth-token bar (16 bytes),
// not a UUID collision bar.
func newRandomID(prefix string) (string, error) {
	b := make([]byte, 16)
	if _, err := cryptorand.Read(b); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(b), nil
}
