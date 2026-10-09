package castle

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/http2"
	"google.golang.org/protobuf/encoding/protojson"

	v1 "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	criteriav1connect "github.com/brokenbots/criteria/sdk/pb/criteria/v1/criteriav1connect"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/events"
)

// podStateCriteriaStub implements the slice of the SDK's
// CriteriaServiceHandler the pod-state feed consumes. Every other method
// embeds the nil interface and is never called; a call panics and fails the
// test loudly.
type podStateCriteriaStub struct {
	criteriav1connect.CriteriaServiceHandler

	mu        sync.Mutex
	envelopes map[string][]*v1.Envelope
	tokens    []string
	// reject makes every SubmitEvents call fail when true.
	reject bool
}

func (s *podStateCriteriaStub) SubmitEvents(ctx context.Context, stream *connect.BidiStream[v1.Envelope, v1.Ack]) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens = append(s.tokens, stream.RequestHeader().Get("X-Criteria-Token"))
	if s.reject {
		return connect.NewError(connect.CodeUnavailable, io.EOF)
	}
	for {
		msg, err := stream.Receive()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return err
			}
			break
		}
		s.envelopes[msg.RunId] = append(s.envelopes[msg.RunId], msg)
	}
	return stream.Send(&v1.Ack{RunId: "run-ack"})
}

func newPodStateTestServer(t *testing.T, stub *podStateCriteriaStub) string {
	t.Helper()
	path, handler := criteriav1connect.NewCriteriaServiceHandler(stub)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	// The connect bidi stream requires HTTP/2, and httptest only negotiates
	// it when EnableHTTP2 is set before StartTLS.
	srv := httptest.NewUnstartedServer(mux)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv.URL
}

// podStateTestReport builds a report mirroring a scope stuck Pending with a
// scheduling reason.
func podStateTestReport(phase, reason string) events.PodStateReport {
	return events.PodStateReport{
		AdapterName: "intake",
		AdapterType: "shell",
		ScopeName:   "develop",
		ScopeID:     "dev-001",
		Pod:         "shell-develop-dev-001-adapter",
		Phase:       phase,
		Reason:      reason,
		Message:     "0/4 nodes are available",
		ObservedAt:  time.Date(2026, 10, 7, 12, 34, 56, 0, time.UTC),
	}
}

func TestPodStateEventEnvelopeDataKeys(t *testing.T) {
	env, err := PodStateEventEnvelope("run-1", podStateTestReport(events.PodPhasePending, "Unschedulable"))
	require.NoError(t, err)

	// The typed payload must serialize to the pinned data shape: one flat
	// object with exactly the nine keys; no nested JSON strings anywhere.
	line, err := protojson.Marshal(env)
	require.NoError(t, err)
	var outer map[string]any
	require.NoError(t, json.Unmarshal(line, &outer))
	adapterPayload, ok := outer["adapterEvent"].(map[string]any)
	require.True(t, ok, "payload must be the AdapterEvent variant: %v", outer)
	assert.Equal(t, events.PodStateObservedKind, adapterPayload["kind"])
	data, ok := adapterPayload["data"].(map[string]any)
	require.True(t, ok)
	wantKeys := []string{
		"adapter", "adapter_type", "scope_instance_id", "scope_name", "pod",
		"phase", "reason", "message", "observed_at",
	}
	require.Len(t, data, len(wantKeys))
	for _, key := range wantKeys {
		assert.Contains(t, data, key)
	}
	assert.Equal(t, map[string]any{
		"adapter":           "intake",
		"adapter_type":      "shell",
		"scope_instance_id": "dev-001",
		"scope_name":        "develop",
		"pod":               "shell-develop-dev-001-adapter",
		"phase":             "Pending",
		"reason":            "Unschedulable",
		"message":           "0/4 nodes are available",
		"observed_at":       "2026-10-07T12:34:56Z",
	}, data)
	assert.Equal(t, "run-1", outer["runId"])
}

func TestPodStateEventEnvelopeValidation(t *testing.T) {
	noType := podStateTestReport(events.PodPhaseRunning, "")
	noType.AdapterType = ""
	_, err := PodStateEventEnvelope("run-1", noType)
	require.Error(t, err)
	_, err = PodStateEventEnvelope("", podStateTestReport(events.PodPhaseRunning, ""))
	require.Error(t, err)
}

func podStateEmitTestFeed(t *testing.T, url, token string, shrinkRetry bool) *PodStateFeed {
	t.Helper()
	// The connect bidi stream requires HTTP/2; negotiate it explicitly
	// because a custom TLS config disables the automatic upgrade in net/http.
	feed := NewPodStateFeed(PodStateFeedConfig{Addr: url, Token: token}, &http.Client{
		Transport: &http2.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // test-only self-signed cert
		},
	})
	if shrinkRetry {
		feed.attemptTimeout = 500 * time.Millisecond
		feed.maxAttempts = 2
		feed.backoffBase = time.Millisecond
	}
	return feed
}

func TestPodStateFeedEmitsTransitionsOnly(t *testing.T) {
	stub := &podStateCriteriaStub{envelopes: map[string][]*v1.Envelope{}}
	url := newPodStateTestServer(t, stub)
	feed := podStateEmitTestFeed(t, url, "shared-castle-token", true)
	logger := testLogger()

	ctx := context.Background()
	pending := podStateTestReport(events.PodPhasePending, "Unschedulable")

	// First observation of the stuck scope submits one event.
	feed.Emit(ctx, "run-1", []events.PodStateReport{pending}, logger)
	assert.Equal(t, 1, stub.received("run-1"))

	// Re-emitting the same (phase, reason) signal does not resubmit.
	feed.Emit(ctx, "run-1", []events.PodStateReport{pending}, logger)
	assert.Equal(t, 1, stub.received("run-1"))

	// A message refresh with an unchanged signal does not resubmit either.
	refreshed := pending
	refreshed.Message = "0/4 nodes are available (still)"
	feed.Emit(ctx, "run-1", []events.PodStateReport{refreshed}, logger)
	assert.Equal(t, 1, stub.received("run-1"))

	// Running is a new signal: the consumer's handshake budget boundary.
	running := podStateTestReport(events.PodPhaseRunning, "")
	running.Message = ""
	feed.Emit(ctx, "run-1", []events.PodStateReport{running}, logger)
	assert.Equal(t, 2, stub.received("run-1"))

	got := stub.envelopesFor("run-1")
	first := got[0].GetAdapterEvent()
	require.NotNil(t, first)
	assert.Equal(t, events.PodStateObservedKind, first.Kind)
	firstFields := first.GetData().AsMap()
	assert.Equal(t, "Pending", firstFields["phase"])
	assert.Equal(t, "Unschedulable", firstFields["reason"])

	second := got[1].GetAdapterEvent()
	require.NotNil(t, second)
	fields := second.GetData().AsMap()
	assert.Equal(t, events.PodPhaseRunning, fields["phase"])
	assert.Equal(t, "", fields["reason"])

	// Runs are separated: another run's scope emits from scratch.
	feed.Emit(ctx, "run-2", []events.PodStateReport{pending}, logger)
	assert.Equal(t, 1, stub.received("run-2"))

	// The pre-shared token rode the request header.
	assert.Equal(t, "shared-castle-token", stub.tokenAt(0))
}

func TestPodStateFeedBestEffortRetries(t *testing.T) {
	stub := &podStateCriteriaStub{envelopes: map[string][]*v1.Envelope{}, reject: true}
	url := newPodStateTestServer(t, stub)
	feed := podStateEmitTestFeed(t, url, "", true)
	logger := testLogger()

	ctx := context.Background()
	pending := podStateTestReport(events.PodPhasePending, "Unschedulable")

	// A rejected submit must be swallowed: Emit neither panics nor fails the
	// caller... and advances nothing, so the next pass re-emits.
	feed.Emit(ctx, "run-1", []events.PodStateReport{pending}, logger)
	assert.Zero(t, stub.received("run-1"))

	stub.mu.Lock()
	stub.reject = false
	stub.mu.Unlock()
	feed.Emit(ctx, "run-1", []events.PodStateReport{pending}, logger)
	assert.Equal(t, 1, stub.received("run-1"))

	// ...and only then does the accepted state advance.
	feed.Emit(ctx, "run-1", []events.PodStateReport{pending}, logger)
	assert.Equal(t, 1, stub.received("run-1"))
}

func TestPodStateFeedDisabled(t *testing.T) {
	var nilFeed *PodStateFeed
	assert.True(t, nilFeed.Disabled())
	assert.True(t, NewPodStateFeed(PodStateFeedConfig{}, nil).Disabled())
	logger := testLogger()
	nilFeed.Emit(context.Background(), "run-1", []events.PodStateReport{podStateTestReport(events.PodPhaseRunning, "")}, logger)
}

func (s *podStateCriteriaStub) received(runID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.envelopes[runID])
}

func (s *podStateCriteriaStub) envelopesFor(runID string) []*v1.Envelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.envelopes[runID]
}

func (s *podStateCriteriaStub) tokenAt(i int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i >= len(s.tokens) {
		return ""
	}
	return s.tokens[i]
}

// testLogger returns the discard logger.
func testLogger() logr.Logger {
	return logr.Discard()
}
