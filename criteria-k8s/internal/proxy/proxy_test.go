package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	connect "connectrpc.com/connect"
	sdk "github.com/brokenbots/criteria/sdk"
	v1 "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	"github.com/brokenbots/criteria/sdk/pb/criteria/v1/criteriav1connect"
	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/control"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/jobbuilder"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type stubSubmitter struct {
	forw  []*v1.Envelope
	ack   *v1.Ack
	err   error
	calls int
}

func (s *stubSubmitter) SubmitEnvelope(ctx context.Context, env *v1.Envelope) (*v1.Ack, error) {
	s.forw = append(s.forw, env)
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	if s.ack != nil {
		return s.ack, nil
	}
	return &v1.Ack{RunId: env.GetRunId(), Seq: 1}, nil
}

func (s *stubSubmitter) forwarded(t *testing.T) []*v1.Envelope {
	t.Helper()
	return s.forw
}

type stubCreator struct {
	created []*v1.CreateRunRequest
	runID   string
	err     error
}

func (s *stubCreator) CreateRun(ctx context.Context, req *v1.CreateRunRequest) (*v1.Run, error) {
	s.created = append(s.created, req)
	if s.err != nil {
		return nil, s.err
	}
	if s.runID == "" {
		return &v1.Run{RunId: "default-run"}, nil
	}
	return &v1.Run{RunId: s.runID}, nil
}

const proxyNS = "criteria-jobs"

type proxyHarness struct {
	t       *testing.T
	proxy   *Proxy
	k8s     client.Client
	submit  *stubSubmitter
	create  *stubCreator
	client  criteriav1connect.CriteriaServiceClient
	baseURL string
}

// newHarness boots a live h2c HTTP server speaking the full criteria
// protocol against a fake k8s client.
func newHarness(t *testing.T, submit runSubmitter, create runCreator, crs ...client.Object) *proxyHarness {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, criteriav1.AddToScheme(scheme))
	k8s := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(crs...).
		Build()
	return newHarnessOnClient(t, submit, create, k8s)
}

// newHarnessOnClient boots the harness against a caller-built fake k8s
// client — the restart tests share one client across two proxy instances.
func newHarnessOnClient(t *testing.T, submit runSubmitter, create runCreator, k8s client.Client) *proxyHarness {
	t.Helper()

	p := New(Config{
		Log:    logr.Discard(),
		Client: k8s,
		NS:     proxyNS,
		Submit: submit,
		Create: create,
	})

	mux := http.NewServeMux()
	prefix, handler := sdk.NewServiceHandler(p)
	mux.Handle(prefix, handler)
	srv := httptest.NewServer(h2c.NewHandler(mux, &http2.Server{}))
	t.Cleanup(srv.Close)

	hClient := &http.Client{
		Transport: &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, network, addr)
			},
		},
		Timeout: 5 * time.Second,
	}

	submitStub, submitIsStub := submit.(*stubSubmitter)
	createStub, createIsStub := create.(*stubCreator)
	if !submitIsStub || !createIsStub {
		// harness consumers only pass stubs; a nil interface is allowed.
		if submit != nil && !submitIsStub {
			t.Fatal("harness: forwarder must be a stub or nil")
		}
		if create != nil && !createIsStub {
			t.Fatal("harness: creator must be a stub or nil")
		}
		submitStub, createStub = nil, nil
	}

	return &proxyHarness{
		t:       t,
		proxy:   p,
		k8s:     k8s,
		submit:  submitStub,
		create:  createStub,
		client:  criteriav1connect.NewCriteriaServiceClient(hClient, srv.URL),
		baseURL: srv.URL,
	}
}

// bearer returns the auth header the engine sends on every RPC after
// Register.
func bearer(token string) http.Header {
	return http.Header{"Authorization": {"Bearer " + token}}
}

// authReq builds a connect unary request carrying the engine's
// post-Register bearer identification.
func authReq[Msg any](msg *Msg, tok string) *connect.Request[Msg] {
	req := connect.NewRequest(msg)
	req.Header().Set("Authorization", "Bearer "+tok)
	return req
}

// dispatchRun builds the dispatch-lane CR: the assignment bridge already
// stamped the castle run id.
func dispatchRun(name, castleRunID string) *criteriav1.CriteriaRun {
	return &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: proxyNS,
			Annotations: map[string]string{
				control.AnnAssignmentRunID: castleRunID,
			},
		},
	}
}

// enrollAndRegister walks the production flow (KB-344 review remediation):
// the controller enrolls the run first — the proxy stamps the per-run
// bootstrap token on the CR — then the engine's Register presents it as
// the deployment-defined bootstrap credential. Returns the minted
// credentials.
func enrollAndRegister(t *testing.T, h *proxyHarness, name string) (token, criteriaID string) {
	t.Helper()
	tok, err := h.proxy.Enroll(context.Background(), name)
	require.NoError(t, err)
	req := connect.NewRequest(&v1.RegisterRequest{Name: name})
	req.Header().Set("X-Server-Bootstrap", tok)
	resp, err := h.client.Register(context.Background(), req)
	require.NoError(t, err)
	return resp.Msg.GetToken(), resp.Msg.GetCriteriaId()
}

func TestProxyDispatchLaneFlowForwardsCastleRunID(t *testing.T) {
	submit := &stubSubmitter{ack: &v1.Ack{Seq: 42}}
	create := &stubCreator{}
	h := newHarness(t, submit, create, dispatchRun("run-alpha", "castle-run-1"))

	token, criteriaID := "", ""
	{
		token, criteriaID = enrollAndRegister(t, h, "run-alpha")
		require.NotEmpty(t, token)
	}

	localRun, err := h.client.CreateRun(context.Background(), authReq(
		&v1.CreateRunRequest{CriteriaId: criteriaID, WorkflowName: "kanboard-develop-url"}, token))
	require.NoError(t, err)
	require.True(t, len(localRun.Msg.GetRunId()) > 4, "proxy mints a run id")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := h.client.SubmitEvents(ctx)
	stream.RequestHeader().Set("Authorization", "Bearer "+token)

	started := sdk.NewEnvelope(localRun.Msg.GetRunId(), &v1.RunStarted{})
	require.NoError(t, stream.Send(started))
	ack, err := stream.Receive()
	require.NoError(t, err)
	assert.Equal(t, localRun.Msg.GetRunId(), ack.RunId, "ack echoes the local run id")
	assert.Equal(t, uint64(42), ack.Seq, "ack carries the server-assigned seq")

	step := sdk.NewEnvelope(localRun.Msg.GetRunId(), &v1.StepEntered{Step: "clone"})
	step.CorrelationId = "corr-2"
	require.NoError(t, stream.Send(step))
	ack2, err := stream.Receive()
	require.NoError(t, err)
	assert.Equal(t, "corr-2", ack2.CorrelationId)

	require.NoError(t, stream.CloseRequest())
	_, err = stream.Receive()
	require.True(t, err == nil || errors.Is(err, io.EOF),
		"clean stream end is not an error")
	require.NoError(t, stream.CloseResponse())

	fwd := submit.forwarded(t)
	require.Len(t, fwd, 2, "both envelopes forwarded exactly once")
	assert.Equal(t, "castle-run-1", fwd[0].RunId, "dispatch-lane run id from the assignment annotation")
	assert.Equal(t, "castle-run-1", fwd[1].RunId)
	assert.Equal(t, started.RunId, localRun.Msg.GetRunId())
	// The payload and correlation survive the hop verbatim.
	assert.True(t, proto.Equal(started.GetRunStarted(), fwd[0].GetRunStarted()),
		"the payload survives the hop verbatim")
	assert.Equal(t, step.CorrelationId, fwd[1].CorrelationId)
	assert.True(t, proto.Equal(step.GetStepEntered(), fwd[1].GetStepEntered()),
		"the step payload survives the hop verbatim")
	// Dispatch lane: no run is minted remotely, the annotation rules.
	assert.Empty(t, create.created)
}

func TestProxySubmitEventsWatchReadyRejected(t *testing.T) {
	submit := &stubSubmitter{}
	h := newHarness(t, submit, &stubCreator{}, dispatchRun("run-deny", "castle-run-9"))

	token := ""
	token, _ = enrollAndRegister(t, h, "run-deny")

	localRun, err := h.client.CreateRun(context.Background(), authReq(&v1.CreateRunRequest{}, token))
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := h.client.SubmitEvents(ctx)
	stream.RequestHeader().Set("Authorization", "Bearer "+token)
	env := &v1.Envelope{RunId: localRun.Msg.GetRunId(), Payload: &v1.Envelope_WatchReady{WatchReady: &v1.WatchReady{}}}
	require.NoError(t, stream.Send(env))
	_, err = stream.Receive()
	require.Error(t, err)
	assert.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err))
	require.Empty(t, submit.forwarded(t))
}

func TestProxySubmitEventsUnknownRunDenied(t *testing.T) {
	submit := &stubSubmitter{}
	h := newHarness(t, submit, &stubCreator{}, dispatchRun("run-unknown", "castle-run-2"))

	token := ""
	token, _ = enrollAndRegister(t, h, "run-unknown")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := h.client.SubmitEvents(ctx)
	stream.RequestHeader().Set("Authorization", "Bearer "+token)
	require.NoError(t, stream.Send(&v1.Envelope{RunId: "run-nothing"}))
	_, err := stream.Receive()
	require.Error(t, err)
	assert.Equal(t, connect.CodePermissionDenied, connect.CodeOf(err), "first not-owned envelope kills the stream")
	require.Empty(t, submit.forwarded(t))
}

func TestProxyCrossAgentSubmitDenied(t *testing.T) {
	submit := &stubSubmitter{}
	h := newHarness(t, submit, &stubCreator{},
		dispatchRun("run-a", "castle-run-a"), dispatchRun("run-b", "castle-run-b"))

	tokenA := ""
	tokenA, _ = enrollAndRegister(t, h, "run-a")
	localA, err := h.client.CreateRun(context.Background(), authReq(&v1.CreateRunRequest{WorkflowName: "wf"}, tokenA))
	require.NoError(t, err)

	tokenB := ""
	tokenB, _ = enrollAndRegister(t, h, "run-b")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := h.client.SubmitEvents(ctx)
	stream.RequestHeader().Set("Authorization", "Bearer "+tokenB)
	require.NoError(t, stream.Send(&v1.Envelope{RunId: localA.Msg.GetRunId()}))
	_, err = stream.Receive()
	require.Error(t, err)
	assert.Equal(t, connect.CodePermissionDenied, connect.CodeOf(err))
	require.Empty(t, submit.forwarded(t))
}

func TestProxyWatcherLaneCreatesCastleRunAndStampsCR(t *testing.T) {
	submit := &stubSubmitter{ack: &v1.Ack{Seq: 5}}
	create := &stubCreator{runID: "castle-watcher-1"}
	cr := &criteriav1.CriteriaRun{ObjectMeta: metav1.ObjectMeta{Name: "run-watcher", Namespace: proxyNS}}
	h := newHarness(t, submit, create, cr)

	token := ""
	token, _ = enrollAndRegister(t, h, "run-watcher")
	localRun, err := h.client.CreateRun(context.Background(), authReq(
		&v1.CreateRunRequest{WorkflowName: "triage-flow", WorkflowHash: "abc"}, token))
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := h.client.SubmitEvents(ctx)
	stream.RequestHeader().Set("Authorization", "Bearer "+token)
	for i := 0; i < 3; i++ {
		env := sdk.NewEnvelope(localRun.Msg.GetRunId(), &v1.RunStarted{})
		env.CorrelationId = fmt.Sprintf("corr-%d", i)
		require.NoError(t, stream.Send(env))
		_, err := stream.Receive()
		require.NoError(t, err)
	}
	require.NoError(t, stream.CloseRequest())
	_, err = stream.Receive()
	require.True(t, err == nil || errors.Is(err, io.EOF),
		"clean stream end is not an error")
	require.NoError(t, stream.CloseResponse())

	require.Len(t, create.created, 1, "exactly one run minted remotely across the whole stream")
	assert.Equal(t, "triage-flow", create.created[0].WorkflowName)
	assert.Equal(t, "abc", create.created[0].WorkflowHash)

	var got criteriav1.CriteriaRun
	require.NoError(t, h.k8s.Get(context.Background(), client.ObjectKey{Namespace: proxyNS, Name: "run-watcher"}, &got))
	assert.Equal(t, "castle-watcher-1", got.Annotations[jobbuilder.AnnotationProxyCastleRunID])

	fwd := submit.forwarded(t)
	require.Len(t, fwd, 3)
	assert.Equal(t, "castle-watcher-1", fwd[0].RunId)
}

func TestProxyRegisterGatesCRName(t *testing.T) {
	submit := &stubSubmitter{}
	h := newHarness(t, submit, &stubCreator{})

	_, err := h.client.Register(context.Background(), connect.NewRequest(&v1.RegisterRequest{Name: ""}))
	require.Error(t, err)
	assert.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err))

	_, err = h.client.Register(context.Background(), connect.NewRequest(&v1.RegisterRequest{Name: "does-not-exist"}))
	require.Error(t, err)
	assert.Equal(t, connect.CodePermissionDenied, connect.CodeOf(err), "unknown CR names yield no identity")

	// No identity handed out, so the registry stays empty.
	h.proxy.mu.Lock()
	defer h.proxy.mu.Unlock()
	require.Empty(t, h.proxy.byToken)
}

// TestProxyRegisterRequiresEnrollmentToken pins the Register authorization
// boundary (KB-344 review remediation): the operator's ClusterIP Service is
// reachable from anywhere in the cluster network, so identity minting is
// bound to the per-run enrollment token the controller stamps on the CR
// before the runner Job renders. A caller that names an existing CR without
// the token — absent header, wrong value, or a CR never enrolled — mints
// nothing and reaches no castle forwarder.
func TestProxyRegisterRequiresEnrollmentToken(t *testing.T) {
	submit := &stubSubmitter{ack: &v1.Ack{Seq: 1}}
	h := newHarness(t, submit, &stubCreator{runID: "castle-locked"},
		dispatchRun("run-locked", "castle-run-x"),
		&criteriav1.CriteriaRun{ObjectMeta: metav1.ObjectMeta{Name: "run-unenrolled", Namespace: proxyNS}})

	// Absent bootstrap header: denied.
	_, err := h.client.Register(context.Background(), connect.NewRequest(&v1.RegisterRequest{Name: "run-locked"}))
	require.Error(t, err)
	assert.Equal(t, connect.CodePermissionDenied, connect.CodeOf(err))

	// Wrong token: denied.
	req := connect.NewRequest(&v1.RegisterRequest{Name: "run-locked"})
	req.Header().Set("X-Server-Bootstrap", "prenrol-wrong")
	_, err = h.client.Register(context.Background(), req)
	require.Error(t, err)
	assert.Equal(t, connect.CodePermissionDenied, connect.CodeOf(err))

	// A CR the controller never enrolled carries no annotation: denied
	// regardless of what the caller presents.
	req = connect.NewRequest(&v1.RegisterRequest{Name: "run-unenrolled"})
	req.Header().Set("X-Server-Bootstrap", "prenrol-guess")
	_, err = h.client.Register(context.Background(), req)
	require.Error(t, err)
	assert.Equal(t, connect.CodePermissionDenied, connect.CodeOf(err))

	// No mint, no forward: the boundary is enforced before anything moves.
	h.proxy.mu.Lock()
	require.Empty(t, h.proxy.byToken)
	require.Empty(t, h.proxy.byCriteriaID)
	h.proxy.mu.Unlock()
	require.Empty(t, submit.forwarded(t))
	assert.Zero(t, submit.calls)

	// Enrolled correctly, the minted identity works end to end: Register
	// succeeds and the returned token authenticates Heartbeat.
	enrolledToken, _ := h.proxy.Enroll(context.Background(), "run-locked")
	require.NotEmpty(t, enrolledToken)
	req = connect.NewRequest(&v1.RegisterRequest{Name: "run-locked"})
	req.Header().Set("X-Server-Bootstrap", enrolledToken)
	resp, err := h.client.Register(context.Background(), req)
	require.NoError(t, err)
	require.NotEmpty(t, resp.Msg.GetToken())
	_, err = h.client.Heartbeat(context.Background(), authReq(&v1.HeartbeatRequest{}, resp.Msg.GetToken()))
	require.NoError(t, err)
}

// TestProxySubmitEventsForwardFailureKillsStream covers the failure path
// (KB-344 review remediation follow-up): a castle SubmitEnvelope error
// turns into CodeUnavailable and the stream dies — an envelope is never
// acked or dropped silently.
func TestProxySubmitEventsForwardFailureKillsStream(t *testing.T) {
	submit := &stubSubmitter{ack: &v1.Ack{Seq: 7}, err: errors.New("castle unreachable")}
	h := newHarness(t, submit, &stubCreator{runID: "castle-fwd"},
		dispatchRun("run-fwd", "castle-run-f"))

	token, _ := enrollAndRegister(t, h, "run-fwd")
	localRun, err := h.client.CreateRun(context.Background(), authReq(&v1.CreateRunRequest{WorkflowName: "wf"}, token))
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := h.client.SubmitEvents(ctx)
	stream.RequestHeader().Set("Authorization", "Bearer "+token)
	require.NoError(t, stream.Send(sdk.NewEnvelope(localRun.Msg.GetRunId(), &v1.RunStarted{})))
	_, err = stream.Receive()
	require.Error(t, err)
	assert.Equal(t, connect.CodeUnavailable, connect.CodeOf(err), "forward failure kills the stream")
	require.Len(t, submit.forwarded(t), 1, "exactly one forward attempt is made")
	assert.Equal(t, 1, submit.calls, "no retry or replay after a failed forward")

	// The stream is dead after the failure: no second envelope is
	// accepted and nothing is forwarded for it.
	_ = stream.Send(sdk.NewEnvelope(localRun.Msg.GetRunId(), &v1.StepEntered{Step: "build"}))
	_, err = stream.Receive()
	require.Error(t, err)
	assert.Equal(t, 1, submit.calls, "the dead stream forwards nothing further")
}

// TestProxySeedsCastleRunIDAndEnrollmentAcrossRestart proves the CR
// annotations are durable enough to survive an operator restart (KB-344
// review remediation): a fresh proxy instance — new in-memory maps —
// re-reads the recorded castle run id and enrollment token, reuses the
// existing castle run instead of minting a second one, and accepts a
// restarted engine's Register without the controller re-enrolling.
func TestProxySeedsCastleRunIDAndEnrollmentAcrossRestart(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, criteriav1.AddToScheme(scheme))
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&criteriav1.CriteriaRun{ObjectMeta: metav1.ObjectMeta{Name: "run-seed", Namespace: proxyNS}},
	).Build()

	create1 := &stubCreator{runID: "castle-seed-1"}
	submit1 := &stubSubmitter{ack: &v1.Ack{Seq: 1}}
	h1 := newHarnessOnClient(t, submit1, create1, k8s)
	enroll1, err := h1.proxy.Enroll(context.Background(), "run-seed")
	require.NoError(t, err)
	token1, _ := enrollAndRegister(t, h1, "run-seed")
	localRun, err := h1.client.CreateRun(context.Background(), authReq(&v1.CreateRunRequest{WorkflowName: "wf"}, token1))
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := h1.client.SubmitEvents(ctx)
	stream.RequestHeader().Set("Authorization", "Bearer "+token1)
	require.NoError(t, stream.Send(sdk.NewEnvelope(localRun.Msg.GetRunId(), &v1.RunStarted{})))
	_, err = stream.Receive()
	require.NoError(t, err)
	require.NoError(t, stream.CloseRequest())
	require.NoError(t, stream.CloseResponse())
	require.Len(t, create1.created, 1, "watcher lane mints the castle run")

	// "Restart": a second proxy on the same CR store. Enroll must return
	// the annotation-backed token (no second mint) and the castle run id
	// seeded from the proxy stamp — no second remote run.
	create2 := &stubCreator{runID: "castle-seed-2"}
	submit2 := &stubSubmitter{ack: &v1.Ack{Seq: 2}}
	h2 := newHarnessOnClient(t, submit2, create2, k8s)
	token2, err := h2.proxy.Enroll(context.Background(), "run-seed")
	require.NoError(t, err)
	assert.Equal(t, enroll1, token2, "the enrollment token is durable on the CR")

	token3, _ := enrollAndRegister(t, h2, "run-seed")
	require.NotEmpty(t, token3)
	localRun2, err := h2.client.CreateRun(context.Background(), authReq(&v1.CreateRunRequest{WorkflowName: "wf"}, token3))
	require.NoError(t, err)

	stream2 := h2.client.SubmitEvents(ctx)
	stream2.RequestHeader().Set("Authorization", "Bearer "+token3)
	require.NoError(t, stream2.Send(sdk.NewEnvelope(localRun2.Msg.GetRunId(), &v1.RunStarted{})))
	_, err = stream2.Receive()
	require.NoError(t, err)
	require.NoError(t, stream2.CloseRequest())
	require.NoError(t, stream2.CloseResponse())

	assert.Empty(t, create2.created, "the stamped castle run id is reused, not minted again")
	fwd := submit2.forwarded(t)
	require.Len(t, fwd, 1)
	assert.Equal(t, "castle-seed-1", fwd[0].RunId, "the stream binds to the pre-restart castle run")
}

func TestProxyHeartbeatAuth(t *testing.T) {
	submit := &stubSubmitter{}
	h := newHarness(t, submit, &stubCreator{}, dispatchRun("run-beat", "castle-run-3"))

	token, criteriaID := "", ""
	token, criteriaID = enrollAndRegister(t, h, "run-beat")

	// The alternative credential header castle accepts also works.
	reqAlt := connect.NewRequest(&v1.HeartbeatRequest{})
	reqAlt.Header().Set("X-Criteria-Token", token)
	_, err := h.client.Heartbeat(context.Background(), reqAlt)
	require.NoError(t, err)

	// An unknown credential is Unauthenticated.
	reqBogus := connect.NewRequest(&v1.HeartbeatRequest{})
	reqBogus.Header().Set("Authorization", "Bearer definitely-invalid")
	_, err = h.client.Heartbeat(context.Background(), reqBogus)
	require.Error(t, err)
	assert.Equal(t, connect.CodeUnauthenticated, connect.CodeOf(err))

	// A criteria id that does not match the authenticated agent is denied.
	_, err = h.client.Heartbeat(context.Background(), authReq(
		&v1.HeartbeatRequest{CriteriaId: criteriaID + "-zz"}, token))
	require.Error(t, err)
	assert.Equal(t, connect.CodePermissionDenied, connect.CodeOf(err))
}

func TestProxyReattachAndResumeOwnership(t *testing.T) {
	submit := &stubSubmitter{}
	h := newHarness(t, submit, &stubCreator{}, dispatchRun("run-re", "castle-run-4"))

	token := ""
	token, _ = enrollAndRegister(t, h, "run-re")
	localRun, err := h.client.CreateRun(context.Background(), authReq(&v1.CreateRunRequest{}, token))
	require.NoError(t, err)
	runID := localRun.Msg.GetRunId()

	re, err := h.client.ReattachRun(context.Background(), authReq(&v1.ReattachRunRequest{RunId: runID}, token))
	require.NoError(t, err)
	assert.Equal(t, "running", re.Msg.GetStatus())

	rs, err := h.client.Resume(context.Background(), authReq(&v1.ResumeRequest{RunId: runID}, token))
	require.NoError(t, err)
	assert.True(t, rs.Msg.GetAccepted())

	// Unknown run ids are NotFound in both.
	for _, unknown := range []string{"run-nope"} {
		_, err = h.client.ReattachRun(context.Background(), authReq(&v1.ReattachRunRequest{RunId: unknown}, token))
		require.Error(t, err)
		assert.Equal(t, connect.CodeNotFound, connect.CodeOf(err))
	}
}

func TestProxyMissingCastleIDFailsPrecondition(t *testing.T) {
	submit := &stubSubmitter{}
	// Dispatch-lane shape without the annotation and no create lane: the
	// run cannot be attributed to any remote run, so the stream fails
	// loudly instead of acking into a void.
	cr := &criteriav1.CriteriaRun{ObjectMeta: metav1.ObjectMeta{Name: "run-orphan", Namespace: proxyNS}}
	h := newHarness(t, submit, nil, cr)

	token := ""
	token, _ = enrollAndRegister(t, h, "run-orphan")
	localRun, err := h.client.CreateRun(context.Background(), authReq(&v1.CreateRunRequest{WorkflowName: "wf"}, token))
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := h.client.SubmitEvents(ctx)
	stream.RequestHeader().Set("Authorization", "Bearer "+token)
	require.NoError(t, stream.Send(sdk.NewEnvelope(localRun.Msg.GetRunId(), &v1.RunStarted{})))
	_, err = stream.Receive()
	require.Error(t, err)
	assert.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(err))
	require.Empty(t, submit.forwarded(t))
}

func TestProxyControlHandshakeThenParks(t *testing.T) {
	submit := &stubSubmitter{}
	h := newHarness(t, submit, &stubCreator{}, dispatchRun("run-ctrl", "castle-run-5"))

	token := ""
	token, _ = enrollAndRegister(t, h, "run-ctrl")

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	req := connect.NewRequest(&v1.ControlSubscribeRequest{})
	req.Header().Set("Authorization", "Bearer "+token)
	ctrl, err := h.client.Control(ctx, req)
	require.NoError(t, err)

	// castle sends ControlReady immediately so the connect response
	// headers flush; mirror it before parking.
	require.True(t, ctrl.Receive(), "first message is ControlReady")
	msg := ctrl.Msg()
	assert.IsType(t, &v1.ControlMessage_ControlReady{}, msg.GetCommand())
	require.NoError(t, ctrl.Err())

	// The operator is the control plane: nothing else ever arrives. The
	// stream stays open until the context ends; the 1.5s test budget
	// already proves the proxy parks instead of closing the stream.
	parked := ctrl.Receive()
	require.False(t, parked)
	_ = ctrl.Err() // cancellation ends the stream from the caller's side

	h.proxy.mu.Lock()
	defer h.proxy.mu.Unlock()
	require.Len(t, h.proxy.byToken, 1)
}
