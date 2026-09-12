package controller_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/castle"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/controller"
	v1 "github.com/brokenbots/workflow-example/criteria-k8s/internal/criteria/pb/criteria/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/events"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/jobbuilder"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// stubPublisher records every RunPublisher call and lets tests inject castle
// outages via the error fields.
type stubPublisher struct {
	mu          sync.Mutex
	disabled    bool
	ensureErr   error
	eventErr    error
	ensureRunID string

	ensureReqs []castle.EnsureRunRequest
	events     []stubEvent
}

type stubEvent struct {
	runID   string
	payload any
}

func (s *stubPublisher) Disabled() bool { return s.disabled }

func (s *stubPublisher) EnsureRun(ctx context.Context, req castle.EnsureRunRequest) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureReqs = append(s.ensureReqs, req)
	if s.ensureErr != nil {
		return "", s.ensureErr
	}
	return s.ensureRunID, nil
}

func (s *stubPublisher) PublishEvent(ctx context.Context, runID string, payload any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.eventErr != nil {
		return s.eventErr
	}
	s.events = append(s.events, stubEvent{runID: runID, payload: payload})
	return nil
}

func (s *stubPublisher) ensureRequests() []castle.EnsureRunRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]castle.EnsureRunRequest(nil), s.ensureReqs...)
}

func (s *stubPublisher) publishedEvents() []stubEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]stubEvent(nil), s.events...)
}

func completedRunnerJob(name string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
			},
		},
	}
}

func TestReconcilePublishesRunCreationToCastle(t *testing.T) {
	scheme := newScheme(t)
	run := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{Name: "cri-42", Namespace: "default", UID: types.UID("run-uid")},
		Spec: criteriav1.CriteriaRunSpec{
			TicketID: "CRI-42",
			RepoURL:  "https://github.com/brokenbots/workflow-example.git",
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(run).WithObjects(run).Build()

	castleStub := &stubPublisher{ensureRunID: "castle-run-1"}
	r := &controller.CriteriaRunReconciler{
		Client:   cl,
		Scheme:   scheme,
		Reader:   &fakeReader{},
		Defaults: jobbuilder.Defaults{DataPVC: "criteria-data"},
		Queue:    controller.NewRunQueue(),
		Castle:   castleStub,
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	require.NoError(t, err)

	reqs := castleStub.ensureRequests()
	require.Len(t, reqs, 1)
	assert.Equal(t, "CRI-42", reqs[0].Ticket)
	assert.Equal(t, "https://github.com/brokenbots/workflow-example.git", reqs[0].RepoURL)
	assert.Equal(t, "criteriarun/cri-42", reqs[0].WorkflowName)
	assert.Empty(t, castleStub.publishedEvents(), "pending runs have no overlord lifecycle event")

	var updated criteriav1.CriteriaRun
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(run), &updated))
	assert.Equal(t, "castle-run-1", updated.Status.CastleRunID)

	// A second reconcile (idempotency) must not re-create the castle run.
	_, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	require.NoError(t, err)
	assert.Len(t, castleStub.ensureRequests(), 1, "EnsureRun must run once per CriteriaRun")
}

func TestReconcilePublishesSucceededPhaseWithPRLink(t *testing.T) {
	scheme := newScheme(t)
	run := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "cri-42",
			Namespace:  "default",
			UID:        types.UID("run-uid"),
			Finalizers: []string{"criteriarun.criteria.brokenbots.dev/finalizer"},
		},
		Spec: criteriav1.CriteriaRunSpec{
			TicketID: "CRI-42",
			RepoURL:  "https://github.com/brokenbots/workflow-example.git",
		},
		Status: criteriav1.CriteriaRunStatus{
			CastleRunID: "castle-run-1",
			Phase:       criteriav1.PhaseRunning,
		},
	}
	job := completedRunnerJob("cri-42")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(run).WithObjects(run, job).Build()

	castleStub := &stubPublisher{ensureRunID: "castle-run-1"}
	r := &controller.CriteriaRunReconciler{
		Client:   cl,
		Scheme:   scheme,
		Reader:   &fakeReader{outcome: &events.Outcome{PRNumber: "42", TicketState: "Done"}},
		Defaults: jobbuilder.Defaults{DataPVC: "criteria-data"},
		Queue:    controller.NewRunQueue(),
		Castle:   castleStub,
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	require.NoError(t, err)
	assert.Empty(t, castleStub.ensureRequests(), "the castle run id is already recorded")
	require.Len(t, castleStub.publishedEvents(), 2)

	prLink, ok := castleStub.publishedEvents()[0].payload.(*v1.AdapterEvent)
	require.True(t, ok, "expected pr_link AdapterEvent, got %T", castleStub.publishedEvents()[0].payload)
	assert.Equal(t, "pr_link", prLink.Kind)
	assert.Equal(t, "https://github.com/brokenbots/workflow-example/pull/42",
		prLink.Data.GetFields()["url"].GetStringValue())

	completed, ok := castleStub.publishedEvents()[1].payload.(*v1.RunCompleted)
	require.True(t, ok, "expected RunCompleted, got %T", castleStub.publishedEvents()[1].payload)
	assert.Equal(t, "succeeded", completed.FinalState)
	assert.True(t, completed.Success)
	assert.Equal(t, "castle-run-1", castleStub.publishedEvents()[1].runID)

	var updated criteriav1.CriteriaRun
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(run), &updated))
	assert.Equal(t, string(criteriav1.PhaseSucceeded), updated.Status.CastlePhase)
	assert.Equal(t, "42", updated.Status.PRNumber)
}

func TestReconcilePublishesFailedPhase(t *testing.T) {
	scheme := newScheme(t)
	run := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "cri-42",
			Namespace:  "default",
			UID:        types.UID("run-uid"),
			Finalizers: []string{"criteriarun.criteria.brokenbots.dev/finalizer"},
		},
		Spec: criteriav1.CriteriaRunSpec{TicketID: "CRI-42"},
		Status: criteriav1.CriteriaRunStatus{
			CastleRunID: "castle-run-1",
			Phase:       criteriav1.PhaseRunning,
		},
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "cri-42", Namespace: "default"},
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobFailed, Status: corev1.ConditionTrue},
			},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(run).WithObjects(run, job).Build()

	castleStub := &stubPublisher{ensureRunID: "castle-run-1"}
	r := &controller.CriteriaRunReconciler{
		Client:   cl,
		Scheme:   scheme,
		Reader:   &fakeReader{data: []byte(`{"terminal":"budget_exhausted"}`)},
		Defaults: jobbuilder.Defaults{DataPVC: "criteria-data"},
		Queue:    controller.NewRunQueue(),
		Castle:   castleStub,
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	require.NoError(t, err)

	eventsPublished := castleStub.publishedEvents()
	require.Len(t, eventsPublished, 1)
	failed, ok := eventsPublished[0].payload.(*v1.RunFailed)
	require.True(t, ok, "expected RunFailed, got %T", eventsPublished[0].payload)
	assert.Equal(t, "criteriarun job failed: budget_exhausted", failed.Reason)
}

func TestReconcileSurvivesCastleOutage(t *testing.T) {
	scheme := newScheme(t)
	run := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "cri-42",
			Namespace:  "default",
			UID:        types.UID("run-uid"),
			Finalizers: []string{"criteriarun.criteria.brokenbots.dev/finalizer"},
		},
		Spec: criteriav1.CriteriaRunSpec{TicketID: "CRI-42", RepoURL: "https://github.com/octo/repo"},
		Status: criteriav1.CriteriaRunStatus{
			CastleRunID: "castle-run-1",
			Phase:       criteriav1.PhaseRunning,
		},
	}
	job := completedRunnerJob("cri-42")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(run).WithObjects(run, job).Build()

	castleStub := &stubPublisher{ensureErr: errors.New("connection refused"), eventErr: errors.New("connection refused")}
	r := &controller.CriteriaRunReconciler{
		Client:   cl,
		Scheme:   scheme,
		Reader:   &fakeReader{outcome: &events.Outcome{PRNumber: "42"}},
		Defaults: jobbuilder.Defaults{DataPVC: "criteria-data"},
		Queue:    controller.NewRunQueue(),
		Castle:   castleStub,
	}

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	require.NoError(t, err, "a castle outage must never fail the reconcile")
	assert.Equal(t, ctrl.Result{RequeueAfter: 10 * time.Second}, res, "castle publish retry must be scheduled")

	var updated criteriav1.CriteriaRun
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(run), &updated))
	assert.Equal(t, criteriav1.PhaseSucceeded, updated.Status.Phase, "run processing must continue")
	assert.Equal(t, "42", updated.Status.PRNumber)
	assert.Empty(t, updated.Status.CastlePhase, "phase must not be marked published when the publish failed")
	assert.Empty(t, castleStub.publishedEvents(), "nothing is published during an outage")
}

func TestFinalizePublishesTerminalEvent(t *testing.T) {
	scheme := newScheme(t)
	now := metav1.NewTime(time.Now())
	run := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "cri-42",
			Namespace:         "default",
			UID:               types.UID("run-uid"),
			DeletionTimestamp: &now,
			Finalizers:        []string{"criteriarun.criteria.brokenbots.dev/finalizer"},
		},
		Spec:   criteriav1.CriteriaRunSpec{TicketID: "CRI-42"},
		Status: criteriav1.CriteriaRunStatus{CastleRunID: "castle-run-1", Phase: criteriav1.PhaseRunning, CastlePhase: "Running"},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(run).WithObjects(run).Build()

	castleStub := &stubPublisher{ensureRunID: "castle-run-1"}
	r := &controller.CriteriaRunReconciler{
		Client:   cl,
		Scheme:   scheme,
		Queue:    controller.NewRunQueue(),
		Castle:   castleStub,
		Defaults: jobbuilder.Defaults{DataPVC: "criteria-data"},
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	require.NoError(t, err)

	eventsPublished := castleStub.publishedEvents()
	require.Len(t, eventsPublished, 1)
	terminal, ok := eventsPublished[0].payload.(*v1.RunFailed)
	require.True(t, ok, "expected RunFailed, got %T", eventsPublished[0].payload)
	assert.Equal(t, "criteriarun finalized (deleted)", terminal.Reason)

	var deleted criteriav1.CriteriaRun
	err = cl.Get(context.Background(), client.ObjectKeyFromObject(run), &deleted)
	assert.True(t, err != nil, "finalizer must be removed after the terminal event")
}

func TestFinalizeRemovesFinalizerEvenWhenCastleIsDown(t *testing.T) {
	scheme := newScheme(t)
	now := metav1.NewTime(time.Now())
	run := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "cri-42",
			Namespace:         "default",
			UID:               types.UID("run-uid"),
			DeletionTimestamp: &now,
			Finalizers:        []string{"criteriarun.criteria.brokenbots.dev/finalizer"},
		},
		Spec:   criteriav1.CriteriaRunSpec{TicketID: "CRI-42"},
		Status: criteriav1.CriteriaRunStatus{CastleRunID: "castle-run-1", Phase: criteriav1.PhaseRunning, CastlePhase: "Running"},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(run).WithObjects(run).Build()

	castleStub := &stubPublisher{ensureRunID: "castle-run-1", eventErr: errors.New("connection refused")}
	r := &controller.CriteriaRunReconciler{
		Client:   cl,
		Scheme:   scheme,
		Queue:    controller.NewRunQueue(),
		Castle:   castleStub,
		Defaults: jobbuilder.Defaults{DataPVC: "criteria-data"},
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	require.NoError(t, err)

	var deleted criteriav1.CriteriaRun
	err = cl.Get(context.Background(), client.ObjectKeyFromObject(run), &deleted)
	assert.True(t, err != nil, "deletion must proceed even when castle is unreachable")
}

func TestFinalizeSkipsTerminalEventWhenAlreadyPublished(t *testing.T) {
	scheme := newScheme(t)
	now := metav1.NewTime(time.Now())
	run := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "cri-42",
			Namespace:         "default",
			UID:               types.UID("run-uid"),
			DeletionTimestamp: &now,
			Finalizers:        []string{"criteriarun.criteria.brokenbots.dev/finalizer"},
		},
		Spec:   criteriav1.CriteriaRunSpec{TicketID: "CRI-42"},
		Status: criteriav1.CriteriaRunStatus{CastleRunID: "castle-run-1", Phase: criteriav1.PhaseSucceeded, CastlePhase: string(criteriav1.PhaseSucceeded)},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(run).WithObjects(run).Build()

	castleStub := &stubPublisher{ensureRunID: "castle-run-1"}
	r := &controller.CriteriaRunReconciler{
		Client:   cl,
		Scheme:   scheme,
		Queue:    controller.NewRunQueue(),
		Castle:   castleStub,
		Defaults: jobbuilder.Defaults{DataPVC: "criteria-data"},
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	require.NoError(t, err)
	assert.Empty(t, castleStub.publishedEvents(), "a terminal phase already published must not be re-emitted")
}

func TestReconcileSkipsCastleWhenDisabled(t *testing.T) {
	scheme := newScheme(t)
	run := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{Name: "cri-42", Namespace: "default", UID: types.UID("run-uid")},
		Spec:       criteriav1.CriteriaRunSpec{TicketID: "CRI-42", RepoURL: "https://github.com/octo/repo"},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(run).WithObjects(run).Build()

	castleStub := &stubPublisher{disabled: true}
	r := &controller.CriteriaRunReconciler{
		Client:   cl,
		Scheme:   scheme,
		Reader:   &fakeReader{},
		Defaults: jobbuilder.Defaults{DataPVC: "criteria-data"},
		Queue:    controller.NewRunQueue(),
		Castle:   castleStub,
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	require.NoError(t, err)
	assert.Empty(t, castleStub.ensureRequests(), "a disabled publisher must not be called")

	var updated criteriav1.CriteriaRun
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(run), &updated))
	assert.Empty(t, updated.Status.CastleRunID)
}
