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
	runs   []*v1.Run
	events map[string][]*v1.Envelope
	// observed list-run-event requests, for cursor assertions.
	eventReqs []*v1.ListRunEventsRequest
}

func (s *stubServer) ListRuns(ctx context.Context, req *connect.Request[v1.ListRunsRequest]) (*connect.Response[v1.ListRunsResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := &v1.ListRunsResponse{}
	for _, r := range s.runs {
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
	obs, err := c.Observe(context.Background(), "CRI-42", "")
	require.NoError(t, err)
	assert.Equal(t, &Observation{}, obs)
}

// Discovery path: no known run id -> paged ListRuns by ticket, then drain of
// that run's event history.
func TestObserveDiscoversRunByTicketAndDrains(t *testing.T) {
	server := &stubServer{
		runs: []*v1.Run{{
			RunId: "run-1", Ticket: "CRI-42", Status: "running",
			RepoUrl: "https://github.com/brokenbots/workflow-example.git",
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
	obs, err := c.Observe(context.Background(), "CRI-42", "")
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
		runs: []*v1.Run{{RunId: "run-1", Ticket: "CRI-42", Status: "running"}},
		events: map[string][]*v1.Envelope{
			"run-1": {provisionEnvelope("run-1", 1, "scope-a", "default")},
		},
	}
	srv := newTestServer(t, server)
	defer srv.Close()

	c := New(Config{Addr: srv.URL}, nil)
	obs, err := c.Observe(context.Background(), "CRI-42", "")
	require.NoError(t, err)
	require.Len(t, obs.Lifecycle, 1)

	// Engine releases the scope (with the nested adapter name) and a new
	// scope provisions between the two observations.
	server.mu.Lock()
	server.events["run-1"] = append(server.events["run-1"],
		releaseEnvelope("run-1", 2, "scope-a", "noop.default"),
		provisionEnvelope("run-1", 3, "scope-b", "default"))
	server.mu.Unlock()

	obs, err = c.Observe(context.Background(), "CRI-42", obs.RunID)
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

// RunCompleted envelopes stamp terminal state; PR number comes from the run
// record's pr_url.
func TestObserveTerminalFromEnvelopeAndRunRecord(t *testing.T) {
	server := &stubServer{
		runs: []*v1.Run{{
			RunId: "run-1", Ticket: "CRI-42", Status: "succeeded", FinalState: "done",
			PrUrl: "https://github.com/brokenbots/workflow-example/pull/42",
		}},
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
	obs, err := c.Observe(context.Background(), "CRI-42", "")
	require.NoError(t, err)
	require.NotNil(t, obs.Terminal)
	assert.True(t, obs.Terminal.Success)
	assert.Equal(t, "42", obs.Terminal.PRNumber)
	assert.Equal(t, "done", obs.Terminal.TicketState)
	assert.True(t, terminalFromRun(server.runs[0]).Success)
}

// When the event stream carries no terminal envelope yet the run record is
// already terminal (e.g. cancelled server-side), the client falls back to
// GetRun so the operator does not wait forever.
func TestObserveTerminalFallbackFromRunRecord(t *testing.T) {
	server := &stubServer{
		runs: []*v1.Run{{
			RunId: "run-1", Ticket: "CRI-42", Status: "failed", FinalState: "failed",
			FailureReason: "workflow step crashed",
		}},
		events: map[string][]*v1.Envelope{
			"run-1": {provisionEnvelope("run-1", 1, "scope-a", "default")},
		},
	}
	srv := newTestServer(t, server)
	defer srv.Close()

	c := New(Config{Addr: srv.URL}, nil)
	obs, err := c.Observe(context.Background(), "CRI-42", "run-1")
	require.NoError(t, err)
	require.NotNil(t, obs.Terminal)
	assert.False(t, obs.Terminal.Success)
	assert.Equal(t, "workflow step crashed", obs.Terminal.Reason)
	assert.Equal(t, "failed", obs.Terminal.FinalState)
}

// No run for the ticket yet is "unknown", not an authoritative empty
// history: it must surface as an error so the caller aborts the pass
// instead of converging desired state (which would delete live adapter
// pods). The same applies to an empty ticket.
func TestObserveNoRunForTicketIsAnError(t *testing.T) {
	server := &stubServer{}
	srv := newTestServer(t, server)
	defer srv.Close()

	c := New(Config{Addr: srv.URL}, nil)
	_, err := c.Observe(context.Background(), "CRI-42", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no castle run registered for ticket CRI-42")

	_, err = c.Observe(context.Background(), "", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires a ticket")
}

// Discovery that exhausts its page budget with more pages remaining is
// inconclusive and must not be reported as an empty observation.
func TestObserveDiscoveryPageCapIsAnError(t *testing.T) {
	pages := &stubPagedRuns{totalPages: maxDiscoveryPages + 2}
	srv := newTestServer(t, pages)
	defer srv.Close()

	c := New(Config{Addr: srv.URL}, nil)
	_, err := c.Observe(context.Background(), "CRI-42", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "did not conclude within")
}

// Discovery prefers the newest non-terminal run for the ticket and falls
// back to the newest terminal one (a retried ticket's earlier failed run).
func TestObserveDiscoveryPrefersNewestNonTerminalRun(t *testing.T) {
	server := &stubServer{
		runs: []*v1.Run{
			{RunId: "run-old", Ticket: "CRI-42", Status: "failed", CreatedAt: timestamppb.New(time.Unix(1, 0))},
			{RunId: "run-new", Ticket: "CRI-42", Status: "running", CreatedAt: timestamppb.New(time.Unix(2, 0))},
		},
	}
	srv := newTestServer(t, server)
	defer srv.Close()

	c := New(Config{Addr: srv.URL}, nil)
	obs, err := c.Observe(context.Background(), "CRI-42", "")
	require.NoError(t, err)
	assert.Equal(t, "run-new", obs.RunID)
}

// The client authenticates with the shared token on every call.
func TestObserveSendsTokenHeader(t *testing.T) {
	server := &stubServer{runs: []*v1.Run{{RunId: "run-1", Ticket: "CRI-42", Status: "running"}}}

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
	_, err := c.Observe(context.Background(), "CRI-42", "")
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
	_, err := c.Observe(context.Background(), "CRI-42", "")
	require.Error(t, err)
}

// stubPagedRuns serves totalPages pages of one run each, chaining
// NextPageToken, to exercise the discovery page budget.
type stubPagedRuns struct {
	criteriav1connect.ServerServiceHandler

	totalPages int
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
		Runs:          []*v1.Run{{RunId: fmt.Sprintf("run-%d", page), Ticket: "CRI-27", Status: "running"}},
		NextPageToken: fmt.Sprintf("%d", page+1),
	}), nil
}

// A terminal observation evicts the per-run client state, so a long-lived
// operator does not accumulate caches for every finished run; a repeat
// observation of the terminal run still reconstructs the same result.
func TestObserveEvictsTerminalRunState(t *testing.T) {
	server := &stubServer{
		runs: []*v1.Run{{
			RunId: "run-1", Ticket: "CRI-42", Status: "succeeded", FinalState: "done",
			PrUrl: "https://github.com/brokenbots/workflow-example/pull/7",
		}},
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
	obs, err := c.Observe(context.Background(), "CRI-42", "")
	require.NoError(t, err)
	require.NotNil(t, obs.Terminal)
	assert.Equal(t, "7", obs.Terminal.PRNumber)
	require.Len(t, obs.Lifecycle, 1)

	c.mu.Lock()
	evicted := len(c.cursors) == 0 && len(c.scopes) == 0 && len(c.lifecycle) == 0 && len(c.terminals) == 0
	c.mu.Unlock()
	assert.True(t, evicted, "terminal run state must be evicted from the client caches")

	// A later observation of the same terminal run re-drains from scratch and
	// reconstructs the same terminal result.
	obs, err = c.Observe(context.Background(), "CRI-42", obs.RunID)
	require.NoError(t, err)
	require.NotNil(t, obs.Terminal)
	assert.True(t, obs.Terminal.Success)
	assert.Equal(t, "7", obs.Terminal.PRNumber)
	assert.Equal(t, "done", obs.Terminal.TicketState)
	require.Len(t, obs.Lifecycle, 1)
}
