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

func TestProxyDispatchLaneFlowForwardsCastleRunID(t *testing.T) {
	submit := &stubSubmitter{ack: &v1.Ack{Seq: 42}}
	create := &stubCreator{}
	h := newHarness(t, submit, create, dispatchRun("run-alpha", "castle-run-1"))

	token, criteriaID := "", ""
	{
		resp, err := h.client.Register(context.Background(), connect.NewRequest(&v1.RegisterRequest{Name: "run-alpha"}))
		require.NoError(t, err)
		token = resp.Msg.Token
		criteriaID = resp.Msg.CriteriaId
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
	assert.Equal(t, step.CorrelationId, fwd[1].CorrelationId)
	// Dispatch lane: no run is minted remotely, the annotation rules.
	assert.Empty(t, create.created)
}

func TestProxySubmitEventsWatchReadyRejected(t *testing.T) {
	submit := &stubSubmitter{}
	h := newHarness(t, submit, &stubCreator{}, dispatchRun("run-deny", "castle-run-9"))

	resp, err := h.client.Register(context.Background(), connect.NewRequest(&v1.RegisterRequest{Name: "run-deny"}))
	require.NoError(t, err)
	token := resp.Msg.Token

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

	resp, err := h.client.Register(context.Background(), connect.NewRequest(&v1.RegisterRequest{Name: "run-unknown"}))
	require.NoError(t, err)
	token := resp.Msg.Token

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := h.client.SubmitEvents(ctx)
	stream.RequestHeader().Set("Authorization", "Bearer "+token)
	require.NoError(t, stream.Send(&v1.Envelope{RunId: "run-nothing"}))
	_, err = stream.Receive()
	require.Error(t, err)
	assert.Equal(t, connect.CodePermissionDenied, connect.CodeOf(err), "first not-owned envelope kills the stream")
	require.Empty(t, submit.forwarded(t))
}

func TestProxyCrossAgentSubmitDenied(t *testing.T) {
	submit := &stubSubmitter{}
	h := newHarness(t, submit, &stubCreator{},
		dispatchRun("run-a", "castle-run-a"), dispatchRun("run-b", "castle-run-b"))

	respA, err := h.client.Register(context.Background(), connect.NewRequest(&v1.RegisterRequest{Name: "run-a"}))
	require.NoError(t, err)
	tokenA := respA.Msg.Token
	localA, err := h.client.CreateRun(context.Background(), authReq(&v1.CreateRunRequest{WorkflowName: "wf"}, tokenA))
	require.NoError(t, err)

	respB, err := h.client.Register(context.Background(), connect.NewRequest(&v1.RegisterRequest{Name: "run-b"}))
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := h.client.SubmitEvents(ctx)
	stream.RequestHeader().Set("Authorization", "Bearer "+respB.Msg.Token)
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

	resp, err := h.client.Register(context.Background(), connect.NewRequest(&v1.RegisterRequest{Name: "run-watcher"}))
	require.NoError(t, err)
	token := resp.Msg.Token
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

func TestProxyHeartbeatAuth(t *testing.T) {
	submit := &stubSubmitter{}
	h := newHarness(t, submit, &stubCreator{}, dispatchRun("run-beat", "castle-run-3"))

	resp, err := h.client.Register(context.Background(), connect.NewRequest(&v1.RegisterRequest{Name: "run-beat"}))
	require.NoError(t, err)
	token := resp.Msg.Token
	criteriaID := resp.Msg.CriteriaId

	// The alternative credential header castle accepts also works.
	reqAlt := connect.NewRequest(&v1.HeartbeatRequest{})
	reqAlt.Header().Set("X-Criteria-Token", token)
	_, err = h.client.Heartbeat(context.Background(), reqAlt)
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

	resp, err := h.client.Register(context.Background(), connect.NewRequest(&v1.RegisterRequest{Name: "run-re"}))
	require.NoError(t, err)
	token := resp.Msg.Token
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

	resp, err := h.client.Register(context.Background(), connect.NewRequest(&v1.RegisterRequest{Name: "run-orphan"}))
	require.NoError(t, err)
	token := resp.Msg.Token
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

	resp, err := h.client.Register(context.Background(), connect.NewRequest(&v1.RegisterRequest{Name: "run-ctrl"}))
	require.NoError(t, err)
	token := resp.Msg.Token

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
