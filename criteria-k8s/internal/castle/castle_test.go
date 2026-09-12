package castle

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/brokenbots/workflow-example/criteria-k8s/internal/criteria/pb/criteria/v1"
	criteriav1connect "github.com/brokenbots/workflow-example/criteria-k8s/internal/criteria/pb/criteria/v1/criteriav1connect"
)

// stubServer implements the slice of v1connect.ServerServiceHandler the
// operator consumes. Every other method embeds the nil interface and is never
// called; a call panics and fails the test loudly.
type stubServer struct {
	criteriav1connect.ServerServiceHandler

	mu     sync.Mutex
	agents []*v1.Agent
	runs   []*v1.Run
	events map[string][]*v1.Envelope
	// observed list-run-event requests, for cursor assertions.
	eventReqs []*v1.ListRunEventsRequest
	// observed list-runs requests, for filter assertions.
	runReqs []*v1.ListRunsRequest
}

func (s *stubServer) ListAgents(ctx context.Context, req *connect.Request[v1.ListAgentsRequest]) (*connect.Response[v1.ListAgentsResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return connect.NewResponse(&v1.ListAgentsResponse{Agents: s.agents}), nil
}

func (s *stubServer) ListRuns(ctx context.Context, req *connect.Request[v1.ListRunsRequest]) (*connect.Response[v1.ListRunsResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runReqs = append(s.runReqs, req.Msg)
	out := &v1.ListRunsResponse{}
	for _, r := range s.runs {
		if req.Msg.CriteriaId != "" && r.CriteriaId != req.Msg.CriteriaId {
			continue
		}
		if req.Msg.Status != "" && r.Status != req.Msg.Status {
			continue
		}
		out.Runs = append(out.Runs, r)
	}
	return connect.NewResponse(out), nil
}

func (s *stubServer) GetRun(ctx context.Context, req *connect.Request[v1.GetRunRequest]) (*connect.Response[v1.Run], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.runs {
		if r.RunId == req.Msg.RunId {
			return connect.NewResponse(r), nil
		}
	}
	return nil, connect.NewError(connect.CodeNotFound, errors.New("run not found"))
}

func (s *stubServer) ListRunEvents(ctx context.Context, req *connect.Request[v1.ListRunEventsRequest]) (*connect.Response[v1.ListRunEventsResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.eventReqs = append(s.eventReqs, req.Msg)
	out := &v1.ListRunEventsResponse{}
	for _, env := range s.events[req.Msg.RunId] {
		if env.Seq > req.Msg.SinceSeq {
			out.Events = append(out.Events, env)
			out.LastSeq = env.Seq
			out.NextSinceSeq = env.Seq
		}
	}
	return connect.NewResponse(out), nil
}

func newTestServer(t *testing.T, handler criteriav1connect.ServerServiceHandler) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	path, h := criteriav1connect.NewServerServiceHandler(handler)
	mux.Handle(path, h)
	return httptest.NewServer(mux)
}

func provisionEnvelope(runID string, seq uint64, scopeID, adapter string) *v1.Envelope {
	data, err := structpb.NewStruct(map[string]any{
		"adapter":             adapter,
		"scope_instance_id":   scopeID,
		"shim_listen_address": "127.0.0.1:38625",
		"token_ref":           "/tmp/runs/" + runID + "/remote-tokens/" + scopeID + "/noop.token",
	})
	if err != nil {
		panic(err)
	}
	return &v1.Envelope{RunId: runID, Seq: seq,
		Payload: &v1.Envelope_AdapterEvent{AdapterEvent: &v1.AdapterEvent{
			Adapter: adapter, Kind: "adapter.lifecycle.provision_wanted", Data: data,
		}}}
}

func releaseEnvelope(runID string, seq uint64, scopeID, adapter string) *v1.Envelope {
	data, err := structpb.NewStruct(map[string]any{
		"adapter":           adapter,
		"scope_instance_id": scopeID,
	})
	if err != nil {
		panic(err)
	}
	return &v1.Envelope{RunId: runID, Seq: seq,
		Payload: &v1.Envelope_AdapterEvent{AdapterEvent: &v1.AdapterEvent{
			Adapter: adapter, Kind: "adapter.lifecycle.released", Data: data,
		}}}
}

func TestObserveDisabled(t *testing.T) {
	c := New(Config{}, nil)
	require.True(t, c.Disabled())
	obs, err := c.Observe(context.Background(), "cri-42", "")
	require.NoError(t, err)
	assert.Equal(t, &Observation{}, obs)
}

// Discovery path: no known run id -> agent lookup by runner job (the engine
// registers an agent named after the runner pod's hostname) -> run lookup by
// criteria id -> drain of that run's event history. The run carries only
// fields the engine writes (no ticket).
func TestObserveDiscoversRunByRunnerAgentAndDrains(t *testing.T) {
	server := &stubServer{
		agents: []*v1.Agent{{CriteriaId: "crit-42", Name: "cri-42-abc12", Status: "online"}},
		runs: []*v1.Run{{
			RunId: "run-1", CriteriaId: "crit-42", WorkflowName: "linear_intake_v1", Status: "running",
		}},
		events: map[string][]*v1.Envelope{
			"run-1": {
				provisionEnvelope("run-1", 1, "scope-a", "default"),
				releaseEnvelope("run-1", 2, "scope-a", "noop.default"),
			},
		},
	}
	srv := newTestServer(t, server)
	defer srv.Close()

	c := New(Config{Addr: srv.URL}, nil)
	obs, err := c.Observe(context.Background(), "cri-42", "")
	require.NoError(t, err)
	assert.Equal(t, "run-1", obs.RunID)
	require.Len(t, obs.Lifecycle, 2)
	assert.True(t, obs.Lifecycle[0].IsProvisionWanted())
	assert.Equal(t, "scope-a", obs.Lifecycle[0].ScopeID)
	assert.True(t, obs.Lifecycle[1].IsRelease())
	assert.Equal(t, "default", obs.Lifecycle[1].AdapterName, "release keyed to the provisioned adapter for the scope instance")
	assert.Nil(t, obs.Terminal, "running run is not terminal")
}

// Subsequent observations reuse the known run id and read incrementally from
// the cursor; the resolved release adapter is remembered across calls.
func TestObserveIncrementalCursorAndPersistentScopes(t *testing.T) {
	server := &stubServer{
		agents: []*v1.Agent{{CriteriaId: "crit-42", Name: "cri-42-abc12"}},
		runs:   []*v1.Run{{RunId: "run-1", CriteriaId: "crit-42", Status: "running"}},
		events: map[string][]*v1.Envelope{
			"run-1": {provisionEnvelope("run-1", 1, "scope-a", "default")},
		},
	}
	srv := newTestServer(t, server)
	defer srv.Close()

	c := New(Config{Addr: srv.URL}, nil)
	obs, err := c.Observe(context.Background(), "cri-42", "")
	require.NoError(t, err)
	require.Len(t, obs.Lifecycle, 1)

	// Engine releases the scope (with the nested adapter name) and a new
	// scope provisions between the two observations.
	server.mu.Lock()
	server.events["run-1"] = append(server.events["run-1"],
		releaseEnvelope("run-1", 2, "scope-a", "noop.default"),
		provisionEnvelope("run-1", 3, "scope-b", "default"))
	server.mu.Unlock()

	obs, err = c.Observe(context.Background(), "cri-42", obs.RunID)
	require.NoError(t, err)
	assert.Equal(t, "run-1", obs.RunID)
	require.Len(t, obs.Lifecycle, 3, "the observation carries the full accumulated history")
	assert.True(t, obs.Lifecycle[0].IsProvisionWanted())
	assert.True(t, obs.Lifecycle[1].IsRelease())
	assert.Equal(t, "scope-a", obs.Lifecycle[1].ScopeID)
	assert.Equal(t, "default", obs.Lifecycle[1].AdapterName)
	assert.True(t, obs.Lifecycle[2].IsProvisionWanted())
	assert.Equal(t, "scope-b", obs.Lifecycle[2].ScopeID)

	// Cursor: the drain read incrementally from the last seen seq.
	last := server.eventReqs[len(server.eventReqs)-1]
	assert.Equal(t, uint64(1), last.SinceSeq, "second read starts strictly after the first page")
}

// RunCompleted envelopes stamp terminal state with only the fields the real
// castle contract supplies: run records carry no final_state column and
// nothing populates pr_url, so the stub record carries just the terminal
// status and the terminal carries no PR number.
func TestObserveTerminalFromEnvelopeAndRunRecord(t *testing.T) {
	server := &stubServer{
		agents: []*v1.Agent{{CriteriaId: "crit-42", Name: "cri-42-abc12"}},
		runs:   []*v1.Run{{RunId: "run-1", CriteriaId: "crit-42", Status: "succeeded"}},
		events: map[string][]*v1.Envelope{
			"run-1": {
				provisionEnvelope("run-1", 1, "scope-a", "default"),
				{RunId: "run-1", Seq: 2, Payload: &v1.Envelope_RunCompleted{RunCompleted: &v1.RunCompleted{Success: true, FinalState: "done"}}},
			},
		},
	}
	srv := newTestServer(t, server)
	defer srv.Close()

	c := New(Config{Addr: srv.URL}, nil)
	obs, err := c.Observe(context.Background(), "cri-42", "")
	require.NoError(t, err)
	require.NotNil(t, obs.Terminal)
	assert.True(t, obs.Terminal.Success)
	assert.Equal(t, "done", obs.Terminal.FinalState)
	assert.Empty(t, obs.Terminal.PRNumber)
	assert.True(t, terminalFromRun(server.runs[0]).Success)
}

// When the event stream carries no terminal envelope yet the run record is
// already terminal (e.g. cancelled server-side), the client falls back to
// GetRun so the operator does not wait forever. The record contributes only
// the verdict: no final_state/failure_reason columns exist, so those stay
// empty on the record path.
func TestObserveTerminalFallbackFromRunRecord(t *testing.T) {
	server := &stubServer{
		agents: []*v1.Agent{{CriteriaId: "crit-42", Name: "cri-42-abc12"}},
		runs:   []*v1.Run{{RunId: "run-1", CriteriaId: "crit-42", Status: "failed"}},
		events: map[string][]*v1.Envelope{
			"run-1": {provisionEnvelope("run-1", 1, "scope-a", "default")},
		},
	}
	srv := newTestServer(t, server)
	defer srv.Close()

	c := New(Config{Addr: srv.URL}, nil)
	obs, err := c.Observe(context.Background(), "cri-42", "run-1")
	require.NoError(t, err)
	require.NotNil(t, obs.Terminal)
	assert.False(t, obs.Terminal.Success)
	assert.Empty(t, obs.Terminal.Reason)
	assert.Empty(t, obs.Terminal.FinalState)
}

// A run that is not registered yet is "unknown", not an authoritative empty
// history: it must surface as an error so the caller aborts the pass instead
// of converging desired state (which would delete live adapter pods). The
// same applies to an empty runner job name, and to an agent whose run has
// not appeared yet.
func TestObserveRunNotYetKnownIsAnError(t *testing.T) {
	server := &stubServer{}
	srv := newTestServer(t, server)
	defer srv.Close()

	c := New(Config{Addr: srv.URL}, nil)
	_, err := c.Observe(context.Background(), "cri-42", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no castle agent registered for runner job cri-42")

	_, err = c.Observe(context.Background(), "", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires a runner job name")

	// The runner registered, but the run has not been created yet.
	server.mu.Lock()
	server.agents = []*v1.Agent{{CriteriaId: "crit-42", Name: "cri-42-abc12"}}
	server.mu.Unlock()
	_, err = c.Observe(context.Background(), "cri-42", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no castle run for criteria crit-42")
}

// A conclusive discovery negative — no agent for the runner job, or no run
// for the criteria — is final for a runner Job that is already terminal (the
// agent registers from the runner pod, which no longer exists). It surfaces
// as ErrRunNotFound so the controller can stop polling, while transient
// failures (transport errors) stay ordinary errors that keep the retry loop.
func TestObserveRunNotFoundIsConclusive(t *testing.T) {
	server := &stubServer{}
	srv := newTestServer(t, server)
	defer srv.Close()

	c := New(Config{Addr: srv.URL}, nil)
	_, err := c.Observe(context.Background(), "cri-42", "")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrRunNotFound, "no agent for the runner job is conclusive")

	server.mu.Lock()
	server.agents = []*v1.Agent{{CriteriaId: "crit-42", Name: "cri-42-abc12"}}
	server.mu.Unlock()
	_, err = c.Observe(context.Background(), "cri-42", "")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrRunNotFound, "no run for the criteria is conclusive")

	unavailable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer unavailable.Close()

	transport := New(Config{Addr: unavailable.URL}, nil)
	_, err = transport.Observe(context.Background(), "cri-42", "")
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrRunNotFound, "a transport error is transient, not conclusive")
}

// Terminal envelopes fold last-wins: a RunFailed followed by a later
// RunCompleted converges on the RunCompleted verdict. This is the re-derivation
// path behind CRI-138 — a stamp from a pass that only saw the earlier
// envelope must be corrected once the final terminal envelope lands.
func TestObserveTerminalLaterEnvelopeWins(t *testing.T) {
	server := &stubServer{
		agents: []*v1.Agent{{CriteriaId: "crit-42", Name: "cri-42-abc12"}},
		runs:   []*v1.Run{{RunId: "run-1", CriteriaId: "crit-42", Status: "succeeded"}},
		events: map[string][]*v1.Envelope{
			"run-1": {
				{RunId: "run-1", Seq: 1, Payload: &v1.Envelope_RunFailed{RunFailed: &v1.RunFailed{Reason: "transient"}}},
				{RunId: "run-1", Seq: 2, Payload: &v1.Envelope_RunCompleted{RunCompleted: &v1.RunCompleted{Success: true, FinalState: "handler_complete"}}},
			},
		},
	}
	srv := newTestServer(t, server)
	defer srv.Close()

	c := New(Config{Addr: srv.URL}, nil)
	obs, err := c.Observe(context.Background(), "cri-42", "")
	require.NoError(t, err)
	require.NotNil(t, obs.Terminal)
	assert.True(t, obs.Terminal.Success, "the final terminal envelope decides the verdict")
	assert.Equal(t, "handler_complete", obs.Terminal.FinalState)
	assert.Empty(t, obs.Terminal.Reason)
}

// More than one agent matching the runner job with distinct criteria ids is
// ambiguous: the operator must not guess which run is the CR's.
func TestObserveAmbiguousAgentsIsAnError(t *testing.T) {
	server := &stubServer{
		agents: []*v1.Agent{
			{CriteriaId: "crit-42", Name: "cri-42-abc12"},
			{CriteriaId: "crit-43", Name: "cri-42-def34"},
		},
	}
	srv := newTestServer(t, server)
	defer srv.Close()

	c := New(Config{Addr: srv.URL}, nil)
	_, err := c.Observe(context.Background(), "cri-42", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ambiguous castle agents for runner job cri-42")
}

// Discovery that exhausts its page budget with more pages remaining is
// inconclusive and must not be reported as an empty observation — for both
// the agent walk and the run walk.
func TestObserveDiscoveryPageCapIsAnError(t *testing.T) {
	// The budgets are package-level vars so tests can lower them.
	origPages, origSize := maxDiscoveryPages, defaultPageSize
	t.Cleanup(func() {
		maxDiscoveryPages, defaultPageSize = origPages, origSize
	})
	maxDiscoveryPages = 2

	agentPages := &stubPagedAgents{totalPages: maxDiscoveryPages + 2}
	srv := newTestServer(t, agentPages)
	defer srv.Close()

	c := New(Config{Addr: srv.URL}, nil)
	_, err := c.Observe(context.Background(), "cri-42", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "agent discovery for runner job cri-42 did not conclude within")

	runPages := &stubPagedRuns{totalPages: maxDiscoveryPages + 2}
	srv2 := newTestServer(t, runPages)
	defer srv2.Close()

	c2 := New(Config{Addr: srv2.URL}, nil)
	_, err = c2.Observe(context.Background(), "cri-42", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "run discovery for criteria crit-27 did not conclude within")
}

// More than one non-terminal run for the same criteria id is ambiguous: the
// operator must not bind a run it cannot attribute. Two concurrently running
// CriteriaRuns with the same criteria id would otherwise bind each other's
// run and converge adapter pods off the wrong run's lifecycle. (Regression
// for the review defect: discovery used to prefer the newest active run.)
func TestObserveAmbiguousActiveRunsIsAnError(t *testing.T) {
	server := &stubServer{
		agents: []*v1.Agent{{CriteriaId: "crit-42", Name: "cri-42-abc12"}},
		runs: []*v1.Run{
			{RunId: "run-1", CriteriaId: "crit-42", Status: "running", CreatedAt: timestamppb.New(time.Unix(1, 0))},
			{RunId: "run-2", CriteriaId: "crit-42", Status: "running", CreatedAt: timestamppb.New(time.Unix(2, 0))},
		},
	}
	srv := newTestServer(t, server)
	defer srv.Close()

	c := New(Config{Addr: srv.URL}, nil)
	_, err := c.Observe(context.Background(), "cri-42", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ambiguous castle runs for criteria crit-42: 2 active")
}

// When every run for the criteria is terminal, discovery falls back to the
// newest terminal one so the controller can still stamp completion.
func TestObserveDiscoveryFallsBackToNewestTerminalRun(t *testing.T) {
	server := &stubServer{
		agents: []*v1.Agent{{CriteriaId: "crit-42", Name: "cri-42-abc12"}},
		runs: []*v1.Run{
			{RunId: "run-old", CriteriaId: "crit-42", Status: "failed", CreatedAt: timestamppb.New(time.Unix(1, 0))},
			{RunId: "run-new", CriteriaId: "crit-42", Status: "succeeded", CreatedAt: timestamppb.New(time.Unix(2, 0))},
		},
	}
	srv := newTestServer(t, server)
	defer srv.Close()

	c := New(Config{Addr: srv.URL}, nil)
	obs, err := c.Observe(context.Background(), "cri-42", "")
	require.NoError(t, err)
	assert.Equal(t, "run-new", obs.RunID)
}

// Run discovery filters server-side: the ListRuns request carries the
// criteria id instead of paging the whole run table client-side.
func TestObserveDiscoverySendsCriteriaIDFilter(t *testing.T) {
	server := &stubServer{
		agents: []*v1.Agent{{CriteriaId: "criterias-XYZ", Name: "cri-42-abc12"}},
		runs:   []*v1.Run{{RunId: "run-1", CriteriaId: "criterias-XYZ", Status: "running"}},
	}
	srv := newTestServer(t, server)
	defer srv.Close()

	c := New(Config{Addr: srv.URL}, nil)
	obs, err := c.Observe(context.Background(), "cri-42", "")
	require.NoError(t, err)
	assert.Equal(t, "run-1", obs.RunID)

	server.mu.Lock()
	defer server.mu.Unlock()
	require.NotEmpty(t, server.runReqs)
	assert.Equal(t, "criterias-XYZ", server.runReqs[0].GetCriteriaId(),
		"the first ListRuns page must carry the criteria_id filter")
}

// A fresh run becomes discoverable and reaches a terminal phase purely from
// castle: the first observation sees the active run, then the engine finishes
// it and the next observation stamps the terminal state.
func TestObserveFreshRunReachesTerminalFromCastle(t *testing.T) {
	server := &stubServer{
		agents: []*v1.Agent{{CriteriaId: "crit-42", Name: "cri-42-abc12"}},
		runs:   []*v1.Run{{RunId: "run-1", CriteriaId: "crit-42", Status: "running"}},
		events: map[string][]*v1.Envelope{
			"run-1": {provisionEnvelope("run-1", 1, "scope-a", "default")},
		},
	}
	srv := newTestServer(t, server)
	defer srv.Close()

	c := New(Config{Addr: srv.URL}, nil)
	obs, err := c.Observe(context.Background(), "cri-42", "")
	require.NoError(t, err)
	assert.Equal(t, "run-1", obs.RunID)
	assert.Nil(t, obs.Terminal)

	server.mu.Lock()
	server.runs[0].Status = "succeeded"
	server.events["run-1"] = append(server.events["run-1"],
		&v1.Envelope{RunId: "run-1", Seq: 2, Payload: &v1.Envelope_RunCompleted{RunCompleted: &v1.RunCompleted{Success: true, FinalState: "done"}}})
	server.mu.Unlock()

	obs, err = c.Observe(context.Background(), "cri-42", obs.RunID)
	require.NoError(t, err)
	require.NotNil(t, obs.Terminal)
	assert.True(t, obs.Terminal.Success)
	assert.Equal(t, "done", obs.Terminal.FinalState)
}

// The client authenticates with the shared token on every call.
func TestObserveSendsTokenHeader(t *testing.T) {
	server := &stubServer{
		agents: []*v1.Agent{{CriteriaId: "crit-42", Name: "cri-42-abc12"}},
		runs:   []*v1.Run{{RunId: "run-1", CriteriaId: "crit-42", Status: "running"}},
	}

	tokenSeen := make(chan string, 10)
	path, inner := criteriav1connect.NewServerServiceHandler(server)
	mux := http.NewServeMux()
	mux.Handle(path, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenSeen <- r.Header.Get("X-Criteria-Token")
		inner.ServeHTTP(w, r)
	}))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New(Config{Addr: srv.URL, Token: "sekrit-token"}, nil)
	_, err := c.Observe(context.Background(), "cri-42", "")
	require.NoError(t, err)
	assert.Equal(t, "sekrit-token", <-tokenSeen)
}

// Castle outages are transient: the client surfaces the error so the
// reconciler can abort the pass and retry on the next interval.
func TestObserveServerErrorIsSurfaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := New(Config{Addr: srv.URL}, nil)
	_, err := c.Observe(context.Background(), "cri-42", "")
	require.Error(t, err)
}

// stubPagedRuns serves totalPages pages of one run each, chaining
// NextPageToken, to exercise the run discovery page budget.
type stubPagedRuns struct {
	criteriav1connect.ServerServiceHandler

	totalPages int
}

func (s *stubPagedRuns) ListAgents(ctx context.Context, req *connect.Request[v1.ListAgentsRequest]) (*connect.Response[v1.ListAgentsResponse], error) {
	return connect.NewResponse(&v1.ListAgentsResponse{
		Agents: []*v1.Agent{{CriteriaId: "crit-27", Name: "cri-42-abc12"}},
	}), nil
}

func (s *stubPagedRuns) ListRuns(ctx context.Context, req *connect.Request[v1.ListRunsRequest]) (*connect.Response[v1.ListRunsResponse], error) {
	page := 0
	if req.Msg.PageToken != "" {
		fmt.Sscanf(req.Msg.PageToken, "%d", &page)
	}
	if page >= s.totalPages {
		return connect.NewResponse(&v1.ListRunsResponse{}), nil
	}
	return connect.NewResponse(&v1.ListRunsResponse{
		Runs:          []*v1.Run{{RunId: fmt.Sprintf("run-%d", page), CriteriaId: "crit-27", Status: "running"}},
		NextPageToken: fmt.Sprintf("%d", page+1),
	}), nil
}

// stubPagedAgents serves totalPages pages of one agent each, chaining
// NextPageToken, to exercise the agent discovery page budget.
type stubPagedAgents struct {
	criteriav1connect.ServerServiceHandler

	totalPages int
}

func (s *stubPagedAgents) ListAgents(ctx context.Context, req *connect.Request[v1.ListAgentsRequest]) (*connect.Response[v1.ListAgentsResponse], error) {
	page := 0
	if req.Msg.PageToken != "" {
		fmt.Sscanf(req.Msg.PageToken, "%d", &page)
	}
	if page >= s.totalPages {
		return connect.NewResponse(&v1.ListAgentsResponse{}), nil
	}
	return connect.NewResponse(&v1.ListAgentsResponse{
		Agents:        []*v1.Agent{{CriteriaId: "crit-27", Name: "other-agent"}},
		NextPageToken: fmt.Sprintf("%d", page+1),
	}), nil
}

// Per-run state (cursor, scopes, lifecycle history) survives a terminal
// observation: the controller may re-observe a terminal run, and evicting
// the cursor then would restart the ListRunEvents drain at since_seq=0 —
// re-reading the run's whole event stream. The repeat observation resumes
// strictly after the last read sequence and reconstructs the same terminal
// from the cached state.
func TestObserveRetainsTerminalStateAndResumesCursor(t *testing.T) {
	server := &stubServer{
		agents: []*v1.Agent{{CriteriaId: "crit-42", Name: "cri-42-abc12"}},
		// Real castle contract: the record carries only status — no
		// final_state, no pr_url.
		runs: []*v1.Run{{RunId: "run-1", CriteriaId: "crit-42", Status: "succeeded"}},
		events: map[string][]*v1.Envelope{
			"run-1": {
				provisionEnvelope("run-1", 1, "scope-a", "default"),
				{RunId: "run-1", Seq: 2, Payload: &v1.Envelope_RunCompleted{RunCompleted: &v1.RunCompleted{Success: true, FinalState: "handler_complete"}}},
			},
		},
	}
	srv := newTestServer(t, server)
	defer srv.Close()

	c := New(Config{Addr: srv.URL}, nil)
	obs, err := c.Observe(context.Background(), "cri-42", "")
	require.NoError(t, err)
	require.NotNil(t, obs.Terminal)
	assert.True(t, obs.Terminal.Success)
	assert.Equal(t, "handler_complete", obs.Terminal.FinalState)
	assert.Empty(t, obs.Terminal.PRNumber)
	require.Len(t, obs.Lifecycle, 1)

	c.mu.Lock()
	retained := len(c.cursors) == 1 && c.cursors["run-1"] == 2 &&
		len(c.scopes) == 1 && len(c.lifecycle) == 1 && len(c.terminals) == 1
	c.mu.Unlock()
	assert.True(t, retained, "terminal run state must be retained, including the event cursor")

	// A later observation of the same terminal run must not re-drain the
	// event stream from the beginning: the drain resumes strictly after the
	// last read sequence.
	obs, err = c.Observe(context.Background(), "cri-42", obs.RunID)
	require.NoError(t, err)
	require.NotNil(t, obs.Terminal)
	assert.True(t, obs.Terminal.Success)
	assert.Equal(t, "handler_complete", obs.Terminal.FinalState)
	require.Len(t, obs.Lifecycle, 1)
	assert.Len(t, server.eventReqs, 2)
	assert.Equal(t, uint64(2), server.eventReqs[1].SinceSeq,
		"post-terminal observation must resume from the retained cursor, not since_seq=0")
}
