package castle

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/http2"

	v1 "github.com/brokenbots/workflow-example/criteria-k8s/internal/criteria/pb/criteria/v1"
	v1connect "github.com/brokenbots/workflow-example/criteria-k8s/internal/criteria/pb/criteria/v1/criteriav1connect"
)

const testBootstrapToken = "boot-token"

// fakeCastle implements enough of the criteria service to exercise the
// publisher: bootstrap-gated registration, token-authenticated CreateRun with
// ticket-backed dedupe, and a recording SubmitEvents stream.
type fakeCastle struct {
	v1connect.UnimplementedCriteriaServiceHandler

	mu sync.Mutex

	bootstrapToken string
	issuedTokens   []string

	// Behavior knobs.
	createErrors    []error // popped (front) before each CreateRun attempt
	rejectAuthOnce  bool    // reject the current token once, then accept
	failEventsEvery int     // fail every Nth SubmitEvents stream open (0 = never)

	// Recorded state.
	registerCalls int
	createReqs    []*v1.CreateRunRequest
	runIDByTicket map[string]string
	events        []*v1.Envelope
}

func newFakeCastle() *fakeCastle {
	return &fakeCastle{
		bootstrapToken: testBootstrapToken,
		runIDByTicket:  map[string]string{},
	}
}

func (f *fakeCastle) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.Handle(v1connect.NewCriteriaServiceHandler(f))
	return mux
}

func (f *fakeCastle) Register(ctx context.Context, req *connect.Request[v1.RegisterRequest]) (*connect.Response[v1.RegisterResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.registerCalls++
	if f.bootstrapToken == "" || req.Header().Get("X-Server-Bootstrap") != f.bootstrapToken {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("invalid bootstrap token"))
	}
	token := fmt.Sprintf("issued-token-%d", f.registerCalls)
	f.issuedTokens = append(f.issuedTokens, token)
	return connect.NewResponse(&v1.RegisterResponse{CriteriaId: "crit-1", Token: token}), nil
}

func (f *fakeCastle) CreateRun(ctx context.Context, req *connect.Request[v1.CreateRunRequest]) (*connect.Response[v1.Run], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.takeCreateErrorLocked(); err != nil {
		return nil, err
	}
	if f.rejectAuthOnce {
		f.rejectAuthOnce = false
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("token rejected"))
	}
	if !f.validTokenLocked(req.Header().Get("X-Criteria-Token")) {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("invalid token"))
	}
	f.createReqs = append(f.createReqs, req.Msg)
	runID, ok := f.runIDByTicket[req.Msg.Ticket]
	if !ok {
		runID = fmt.Sprintf("run-%d", len(f.createReqs))
		f.runIDByTicket[req.Msg.Ticket] = runID
	}
	return connect.NewResponse(&v1.Run{RunId: runID, Ticket: req.Msg.Ticket, RepoUrl: req.Msg.RepoUrl}), nil
}

func (f *fakeCastle) SubmitEvents(ctx context.Context, stream *connect.BidiStream[v1.Envelope, v1.Ack]) error {
	f.mu.Lock()
	fail := f.failEventsEvery > 0
	f.mu.Unlock()
	if fail {
		return connect.NewError(connect.CodeUnavailable, errors.New("castle is unhappy"))
	}
	for {
		env, err := stream.Receive()
		if err != nil {
			return nil
		}
		f.mu.Lock()
		f.events = append(f.events, env)
		f.mu.Unlock()
		if err := stream.Send(&v1.Ack{}); err != nil {
			return err
		}
	}
}

func (f *fakeCastle) takeCreateErrorLocked() error {
	if len(f.createErrors) == 0 {
		return nil
	}
	err := f.createErrors[0]
	f.createErrors = f.createErrors[1:]
	return err
}

func (f *fakeCastle) validTokenLocked(token string) bool {
	if len(f.issuedTokens) == 0 {
		// No registration flow in this test: any non-empty token accepted.
		return token != ""
	}
	for _, issued := range f.issuedTokens {
		if token == issued {
			return true
		}
	}
	return false
}

func (f *fakeCastle) registerCallsCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.registerCalls
}

func (f *fakeCastle) recordedEvents() []*v1.Envelope {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*v1.Envelope(nil), f.events...)
}

// startFakeCastle runs the fake over TLS with HTTP/2 enabled: the connect
// bidi stream requires HTTP/2, and httptest only negotiates it when
// EnableHTTP2 is set before StartTLS.
func startFakeCastle(t *testing.T, fake *fakeCastle) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(fake.handler(t))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// newTestPublisher builds a publisher with small retry timings so tests stay
// fast.
func newTestPublisher(cfg Config, srv *httptest.Server) *Publisher {
	// The connect bidi stream requires HTTP/2; negotiate it explicitly because
	// a custom TLS config disables the automatic upgrade in net/http.
	p := New(cfg, &http.Client{
		Transport: &http2.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // test-only self-signed cert
		},
	})
	p.attemptTimeout = 100 * time.Millisecond
	p.maxAttempts = 3
	p.backoffBase = 200 * time.Millisecond
	return p
}

func TestEnsureRunCreatesRunWithAgentToken(t *testing.T) {
	fake := newFakeCastle()
	srv := startFakeCastle(t, fake)

	p := newTestPublisher(Config{Addr: srv.URL, Token: "agent-token", AgentName: "criteria-k8s-operator"}, srv)
	runID, err := p.EnsureRun(context.Background(), EnsureRunRequest{
		Ticket:       "CRI-42",
		RepoURL:      "https://github.com/brokenbots/workflow-example.git",
		WorkflowName: "criteriarun/cri-42",
	})
	require.NoError(t, err)
	assert.Equal(t, "run-1", runID)

	reqs := fake.createReqs
	require.Len(t, reqs, 1)
	assert.Equal(t, "CRI-42", reqs[0].Ticket)
	assert.Equal(t, "https://github.com/brokenbots/workflow-example.git", reqs[0].RepoUrl)
	assert.Equal(t, "criteriarun/cri-42", reqs[0].WorkflowName)
}

func TestEnsureRunIsRepeatablePerTicket(t *testing.T) {
	fake := newFakeCastle()
	srv := startFakeCastle(t, fake)

	p := newTestPublisher(Config{Addr: srv.URL, Token: "agent-token"}, srv)
	req := EnsureRunRequest{Ticket: "CRI-42", RepoURL: "https://github.com/octo/repo", WorkflowName: "criteriarun/x"}
	first, err := p.EnsureRun(context.Background(), req)
	require.NoError(t, err)
	second, err := p.EnsureRun(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, first, second, "castle dedupes ticket-backed creates; repeated reconciles stay idempotent")
}

func TestEnsureRunRetriesThroughTransientFailures(t *testing.T) {
	fake := newFakeCastle()
	fake.createErrors = []error{
		connect.NewError(connect.CodeUnavailable, errors.New("boom")),
		connect.NewError(connect.CodeUnavailable, errors.New("boom")),
	}
	srv := startFakeCastle(t, fake)

	p := newTestPublisher(Config{Addr: srv.URL, Token: "agent-token"}, srv)
	runID, err := p.EnsureRun(context.Background(), EnsureRunRequest{Ticket: "CRI-7", WorkflowName: "criteriarun/cri-7"})
	require.NoError(t, err)
	assert.Equal(t, "run-1", runID, "failed attempts must not mint run ids")
}

func TestEnsureRunFailsAfterRetries(t *testing.T) {
	fake := newFakeCastle()
	fake.createErrors = []error{
		connect.NewError(connect.CodeUnavailable, errors.New("down")),
		connect.NewError(connect.CodeUnavailable, errors.New("down")),
		connect.NewError(connect.CodeUnavailable, errors.New("down")),
	}
	srv := startFakeCastle(t, fake)

	p := newTestPublisher(Config{Addr: srv.URL, Token: "agent-token"}, srv)
	_, err := p.EnsureRun(context.Background(), EnsureRunRequest{Ticket: "CRI-7", WorkflowName: "w"})
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "castle create_run"), err.Error())
}

func TestBootstrapTokenRegistersOnce(t *testing.T) {
	fake := newFakeCastle()
	srv := startFakeCastle(t, fake)

	p := newTestPublisher(Config{Addr: srv.URL, BootstrapToken: testBootstrapToken, AgentName: "criteria-k8s-operator"}, srv)
	req := EnsureRunRequest{Ticket: "CRI-9", WorkflowName: "criteriarun/cri-9"}
	first, err := p.EnsureRun(context.Background(), req)
	require.NoError(t, err)
	second, err := p.EnsureRun(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, first, second)
	assert.Equal(t, 1, fake.registerCallsCount(), "agent token is minted once per process")
}

func TestStaleRegisteredTokenReRegisters(t *testing.T) {
	fake := newFakeCastle()
	srv := startFakeCastle(t, fake)

	p := newTestPublisher(Config{Addr: srv.URL, BootstrapToken: testBootstrapToken, AgentName: "criteria-k8s-operator"}, srv)
	fake.rejectAuthOnce = true // reject the first registered token once
	runID, err := p.EnsureRun(context.Background(), EnsureRunRequest{Ticket: "CRI-11", WorkflowName: "w"})
	require.NoError(t, err)
	assert.Equal(t, "run-1", runID)
	assert.Equal(t, 2, fake.registerCallsCount(), "castle rejecting the token must trigger re-registration")
}

func TestPublishEventSubmitsEnvelope(t *testing.T) {
	fake := newFakeCastle()
	srv := startFakeCastle(t, fake)

	p := newTestPublisher(Config{Addr: srv.URL, Token: "agent-token"}, srv)
	require.NoError(t, p.PublishEvent(context.Background(), "run-1", &v1.RunStarted{}))

	events := fake.recordedEvents()
	require.Len(t, events, 1)
	env := events[0]
	assert.Equal(t, "run-1", env.RunId)
	assert.Equal(t, int32(1), env.SchemaVersion)
	assert.NotNil(t, env.GetRunStarted(), "payload must be carried on the envelope")
	assert.NotZero(t, env.Ts)
}

func TestPublishEventFailsWhenCastleIsDown(t *testing.T) {
	srv := httptest.NewUnstartedServer(newFakeCastle().handler(t))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	srv.Close()

	p := newTestPublisher(Config{Addr: srv.URL, Token: "agent-token"}, srv)
	err := p.PublishEvent(context.Background(), "run-1", &v1.RunCompleted{FinalState: "succeeded", Success: true})
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "castle submit_events"), err.Error())
}

func TestDisabledPublisherIsANoOp(t *testing.T) {
	var nilPublisher *Publisher
	assert.True(t, nilPublisher.Disabled())
	assert.True(t, (&Publisher{}).Disabled())

	p := New(Config{}, nil)
	assert.True(t, p.Disabled())
	runID, err := p.EnsureRun(context.Background(), EnsureRunRequest{Ticket: "CRI-1", WorkflowName: "w"})
	require.NoError(t, err)
	assert.Empty(t, runID)
	assert.NoError(t, p.PublishEvent(context.Background(), "run-1", &v1.RunStarted{}))
}

func TestRegisterRejectsWrongBootstrapToken(t *testing.T) {
	fake := newFakeCastle()
	srv := startFakeCastle(t, fake)

	p := newTestPublisher(Config{Addr: srv.URL, BootstrapToken: "wrong-token", AgentName: "criteria-k8s-operator"}, srv)
	_, err := p.EnsureRun(context.Background(), EnsureRunRequest{Ticket: "CRI-13", WorkflowName: "w"})
	require.Error(t, err)
	assert.Equal(t, 3, fake.registerCallsCount(), "every retry must present the bootstrap token")
}
