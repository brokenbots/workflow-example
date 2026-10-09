package controller

// Tests for the KB-225 pod-state feed wiring: the reconcile derives a
// pod-state report per active scope from the observed adapter pods and
// emits it through castle.PodStateFeed on (phase, reason) transitions only,
// using the report shape pinned in internal/events/podstate_test.go.

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	v1 "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	v1connect "github.com/brokenbots/criteria/sdk/pb/criteria/v1/criteriav1connect"
	logr "github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/http2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/brokenbots/workflow-example/criteria-k8s/internal/castle"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/events"
)

// podStateProvisionEvent is a provision-wanted from an engine carrying
// adapter_type (>= v0.5.22): the pod-state reports join on it.
var podStateProvisionEvent = events.LifecycleEvent{
	Event:       events.EventProvisionWanted,
	RunID:       "58f12fe6-5144-4438-8e99-13f701be454c",
	ScopeID:     "dev-001",
	ScopeTag:    "develop",
	AdapterName: "intake",
	AdapterType: "shell",
	ShimAddress: "127.0.0.1:38625",
}

func TestPodStateReportsExtraction(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 34, 56, 0, time.UTC)

	tests := []struct {
		name        string
		status      corev1.PodStatus
		wantPhase   string
		wantReason  string
		wantMessage string
	}{
		{
			name: "pending with unschedulable condition",
			status: corev1.PodStatus{
				Phase: corev1.PodPending,
				Conditions: []corev1.PodCondition{
					{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable",
						Message: "0/4 nodes are available"},
				},
			},
			wantPhase: "Pending", wantReason: "Unschedulable", wantMessage: "0/4 nodes are available",
		},
		{
			name: "pending with image pull wait",
			status: corev1.PodStatus{
				Phase: corev1.PodPending,
				ContainerStatuses: []corev1.ContainerStatus{
					{State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
						Reason: "ImagePullBackOff", Message: "Back-off pulling image"}}},
				},
			},
			wantPhase: "Pending", wantReason: "ImagePullBackOff", wantMessage: "Back-off pulling image",
		},
		{
			name:       "running without waits",
			status:     corev1.PodStatus{Phase: corev1.PodRunning},
			wantPhase:  "Running",
			wantReason: "",
		},
		{
			name: "failed with terminated container",
			status: corev1.PodStatus{
				Phase: corev1.PodFailed,
				ContainerStatuses: []corev1.ContainerStatus{
					{State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
						Reason: "Error", Message: "connection refused"}}},
				},
			},
			wantPhase: "Failed", wantReason: "Error", wantMessage: "connection refused",
		},
		{
			name:        "pod message fallback",
			status:      corev1.PodStatus{Phase: corev1.PodPending, Message: "quota exceeded"},
			wantPhase:   "Pending",
			wantReason:  "",
			wantMessage: "quota exceeded",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scope := podStateProvisionEvent
			memberScope := map[string]events.LifecycleEvent{"pod-1": scope}
			existing := &corev1.PodList{Items: []corev1.Pod{{
				ObjectMeta: metav1.ObjectMeta{Name: "pod-1"},
				Status:     tt.status,
			}}}
			reports := podStateReports(existing, memberScope, now, logr.Discard())
			require.Len(t, reports, 1)
			report := reports[0]
			assert.Equal(t, tt.wantPhase, report.Phase)
			assert.Equal(t, tt.wantReason, report.Reason)
			assert.Equal(t, tt.wantMessage, report.Message)
			assert.Equal(t, "shell", report.AdapterType)
			assert.Equal(t, "develop", report.ScopeName)
			assert.Equal(t, "dev-001", report.ScopeID)
			assert.Equal(t, "pod-1", report.Pod)
			assert.Equal(t, now, report.ObservedAt)
		})
	}
}

func TestPodStateReportsSkipsUnreportableMembers(t *testing.T) {
	pending := corev1.PodStatus{Phase: corev1.PodPending}
	pod := func(name string, status corev1.PodStatus) corev1.Pod {
		return corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name}, Status: status}
	}

	// A scope from an engine without adapter_type cannot be joined by the
	// consumer's probe key: skip rather than emit an ambiguous event.
	noType := podStateProvisionEvent
	noType.AdapterType = ""
	assert.Empty(t, podStateReports(
		&corev1.PodList{Items: []corev1.Pod{pod("pod-1", pending)}},
		map[string]events.LifecycleEvent{"pod-1": noType}, time.Now(), logr.Discard()))

	// A pod whose status has not been observed at all reports nothing.
	assert.Empty(t, podStateReports(
		&corev1.PodList{Items: []corev1.Pod{pod("pod-1", corev1.PodStatus{})}},
		map[string]events.LifecycleEvent{"pod-1": podStateProvisionEvent}, time.Now(), logr.Discard()))

	// A pod that exists but hosts no active scope reports nothing.
	assert.Empty(t, podStateReports(
		&corev1.PodList{Items: []corev1.Pod{pod("pod-1", pending)}},
		map[string]events.LifecycleEvent{}, time.Now(), logr.Discard()))
}

// podStateCriteriaWatcher records the envelopes submitted through the feed.
type podStateCriteriaWatcher struct {
	v1connect.CriteriaServiceHandler

	mu        sync.Mutex
	envelopes map[string][]*v1.Envelope
}

func (s *podStateCriteriaWatcher) SubmitEvents(ctx context.Context, stream *connect.BidiStream[v1.Envelope, v1.Ack]) error {
	for {
		msg, err := stream.Receive()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return err
			}
			break
		}
		s.mu.Lock()
		s.envelopes[msg.RunId] = append(s.envelopes[msg.RunId], msg)
		s.mu.Unlock()
	}
	return stream.Send(&v1.Ack{RunId: "run-ack"})
}

func (s *podStateCriteriaWatcher) received(runID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.envelopes[runID])
}

func (s *podStateCriteriaWatcher) dataFor(t *testing.T, runID string, i int) map[string]any {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	envelopes := s.envelopes[runID]
	require.Greater(t, len(envelopes), i)
	ev := envelopes[i].GetAdapterEvent()
	require.NotNil(t, ev)
	assert.Equal(t, events.PodStateObservedKind, ev.Kind)
	return ev.GetData().AsMap()
}

// podStateTestFeed builds a feed against a fake CriteriaService over TLS
// with HTTP/2 (the connect bidi stream requires it), reusing the feed's
// default retry timings — success paths only here; submit-rejection and
// retry mechanics are pinned in internal/castle/podstate_test.go.
func podStateTestFeed(t *testing.T) (*castle.PodStateFeed, *podStateCriteriaWatcher) {
	t.Helper()
	stub := &podStateCriteriaWatcher{envelopes: map[string][]*v1.Envelope{}}
	path, handler := v1connect.NewCriteriaServiceHandler(stub)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewUnstartedServer(mux)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	feed := castle.NewPodStateFeed(castle.PodStateFeedConfig{Addr: srv.URL, Token: "tok"}, &http.Client{
		Transport: &http2.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // test-only self-signed cert
		},
	})
	return feed, stub
}

// dataWithoutObservedAt drops the always-present observation timestamp from
// the data map so the remaining keys can be compared exactly; every other
// payload key is asserted as-is, including empty strings.
func dataWithoutObservedAt(t *testing.T, data map[string]any) map[string]any {
	t.Helper()
	_, ok := data["observed_at"]
	require.True(t, ok, "observed_at must always be present")
	out := make(map[string]any, len(data)-1)
	for key, value := range data {
		if key == "observed_at" {
			continue
		}
		out[key] = value
	}
	require.Len(t, out, 8)
	return out
}

func TestReconcileEmitsPodStateOnTransitions(t *testing.T) {
	feed, stub := podStateTestFeed(t)
	run := perScopeTestRun(true)
	r, cl := newPerScopeTestReconciler(t, run)
	r.PodState = feed

	ctx := context.Background()
	scope := podStateProvisionEvent

	// Pass 1: the pod does not exist yet; reconcile creates it and reports
	// nothing — nothing was observed.
	created, err := r.reconcilePerScopeAdapters(ctx, run, "castle-run-1", []events.LifecycleEvent{scope}, logr.Discard())
	require.NoError(t, err)
	require.Equal(t, 1, created)
	assert.Zero(t, stub.received("castle-run-1"))

	pods := listAdapterPods(t, cl, "default")
	require.Len(t, pods, 1)
	pod := pods[0].DeepCopy()
	const unschedulableMessage = "0/4 nodes are available: 2 node(s) were not ready, 2 Insufficient cpu."
	pod.Status = corev1.PodStatus{
		Phase: corev1.PodPending,
		Conditions: []corev1.PodCondition{
			{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable", Message: unschedulableMessage},
		},
	}
	require.NoError(t, cl.Status().Update(ctx, pod))

	// Pass 2: the stuck scope reports Pending/Unschedulable with the
	// scheduler's message — the named-phase verdict the session wait needs.
	_, err = r.reconcilePerScopeAdapters(ctx, run, "castle-run-1", []events.LifecycleEvent{scope}, logr.Discard())
	require.NoError(t, err)
	assert.Equal(t, 1, stub.received("castle-run-1"))
	data := stub.dataFor(t, "castle-run-1", 0)
	_, err = time.Parse(time.RFC3339, data["observed_at"].(string))
	require.NoError(t, err, "observed_at must be RFC3339")
	assert.Equal(t, dataWithoutObservedAt(t, data), map[string]any{
		"adapter":           "intake",
		"adapter_type":      "shell",
		"scope_instance_id": "dev-001",
		"scope_name":        "develop",
		"pod":               pod.Name,
		"phase":             "Pending",
		"reason":            "Unschedulable",
		"message":           unschedulableMessage,
	})

	// Pass 3: unchanged state — transition-only emission, no resubmission.
	_, err = r.reconcilePerScopeAdapters(ctx, run, "castle-run-1", []events.LifecycleEvent{scope}, logr.Discard())
	require.NoError(t, err)
	assert.Equal(t, 1, stub.received("castle-run-1"))

	// Pass 4: the pod reaches Running — the handshake budget boundary.
	pod.Status = corev1.PodStatus{Phase: corev1.PodRunning}
	require.NoError(t, cl.Status().Update(ctx, pod))
	_, err = r.reconcilePerScopeAdapters(ctx, run, "castle-run-1", []events.LifecycleEvent{scope}, logr.Discard())
	require.NoError(t, err)
	assert.Equal(t, 2, stub.received("castle-run-1"))
	running := stub.dataFor(t, "castle-run-1", 1)
	assert.Equal(t, "Running", running["phase"])
	assert.Equal(t, "", running["reason"])
	assert.Equal(t, "", running["message"])
	for _, key := range []string{"adapter", "adapter_type", "scope_instance_id", "scope_name", "pod", "observed_at"} {
		assert.NotNil(t, running[key], "key %s must be present even when empty", key)
	}
	_, err = time.Parse(time.RFC3339, running["observed_at"].(string))
	require.NoError(t, err, "observed_at must be RFC3339")

	// Pass 5: unchanged again — no resubmission.
	_, err = r.reconcilePerScopeAdapters(ctx, run, "castle-run-1", []events.LifecycleEvent{scope}, logr.Discard())
	require.NoError(t, err)
	assert.Equal(t, 2, stub.received("castle-run-1"))

	// A reconciler with a nil feed is safe and silently emits nothing (also
	// the state exercised by every pre-existing nested test).
	r.PodState = nil
	_, err = r.reconcilePerScopeAdapters(ctx, run, "castle-run-1", []events.LifecycleEvent{scope}, logr.Discard())
	require.NoError(t, err)
	assert.Equal(t, 2, stub.received("castle-run-1"))
}

func TestReconcileSkipsEmitWithoutCastleRunID(t *testing.T) {
	feed, stub := podStateTestFeed(t)
	run := perScopeTestRun(true)
	r, cl := newPerScopeTestReconciler(t, run)
	r.PodState = feed

	ctx := context.Background()
	created, err := r.reconcilePerScopeAdapters(ctx, run, "castle-run-1", []events.LifecycleEvent{podStateProvisionEvent}, logr.Discard())
	require.NoError(t, err)
	require.Equal(t, 1, created)
	pods := listAdapterPods(t, cl, "default")
	require.Len(t, pods, 1)
	pod := pods[0].DeepCopy()
	pod.Status = corev1.PodStatus{Phase: corev1.PodPending}
	require.NoError(t, cl.Status().Update(ctx, pod))

	// An unknown castle run id reports nothing: the engine cannot join an
	// event to a run it has not registered.
	_, err = r.reconcilePerScopeAdapters(ctx, run, "", []events.LifecycleEvent{podStateProvisionEvent}, logr.Discard())
	require.NoError(t, err)
	assert.Zero(t, stub.received("castle-run-1"))

	// The same reconcile with the real run id emits, proving the skip above
	// was the run-id guard and not unreportable pod state.
	_, err = r.reconcilePerScopeAdapters(ctx, run, "castle-run-1", []events.LifecycleEvent{podStateProvisionEvent}, logr.Discard())
	require.NoError(t, err)
	assert.Equal(t, 1, stub.received("castle-run-1"))
}
