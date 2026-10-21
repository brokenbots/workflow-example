package controller_test

// Terminal-clock wiring tests (KB-309): the reconcile stamps status.terminalAt
// on both paths that carry the terminal phase — the persisted-terminal branch
// and the fresh-transition convergence write — and never rewrites it. The
// stamp is what the retention sweep ages runs off.

import (
	"context"
	"testing"
	"time"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/castle"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/controller"
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

// persistedTerminalHarness is a fully persisted-terminal run (finalizer, live
// runner Job, archiver landing its record) with a castle terminal envelope
// stamped at observedAt — the KB-314 gate setup, now also feeding KB-309's
// terminal clock.
func persistedTerminalHarness(t *testing.T) (*gateHarness, *criteriav1.CriteriaRun, time.Time, ctrl.Result) {
	t.Helper()
	// RFC3339 second precision: a live apiserver (and the fake client)
	// stores metav1.Time without sub-second digits, so assertions compare
	// at seconds.
	observedAt := time.Now().Add(-90 * time.Minute).Truncate(time.Second)
	run := terminalArchivableRun("cri-42", "cr-uid-1", "Succeeded", "castle-run-1")
	job := terminalRunnerJob("cri-42")
	harness := newGateHarness(t, run, job)
	harness.castle.observation = &castle.Observation{
		RunID:    "castle-run-1",
		Terminal: &castle.Terminal{Success: true, At: observedAt},
	}
	res, err := harness.reconcile(t, run)
	require.NoError(t, err)
	return harness, run, observedAt, res
}

func TestReconcileTerminalStampCarriesCastleTerminalTime(t *testing.T) {
	harness, run, observedAt, _ := persistedTerminalHarness(t)

	var updated criteriav1.CriteriaRun
	require.NoError(t, harness.cl.Get(context.Background(), client.ObjectKeyFromObject(run), &updated))
	require.NotNil(t, updated.Status.TerminalAt, "a persisted-terminal run carries its terminal clock after the reconcile")
	assert.Equal(t, observedAt.UTC(), updated.Status.TerminalAt.Time.UTC(), "the stamp prefers the castle terminal envelope's own timestamp")
}

func TestReconcileTerminalStampWrittenOnce(t *testing.T) {
	harness, run, observedAt, _ := persistedTerminalHarness(t)

	// A resync re-reconciles the persisted-terminal run: the stamp must not
	// move (the retention window's age basis is stable), even when a later
	// envelope would carry a different timestamp.
	laterAt := observedAt.Add(-time.Hour)
	harness.castle.observation = &castle.Observation{
		RunID:    "castle-run-1",
		Terminal: &castle.Terminal{Success: true, At: laterAt},
	}
	_, err := harness.reconcile(t, run)
	require.NoError(t, err)

	var updated criteriav1.CriteriaRun
	require.NoError(t, harness.cl.Get(context.Background(), client.ObjectKeyFromObject(run), &updated))
	require.NotNil(t, updated.Status.TerminalAt)
	assert.Equal(t, observedAt.UTC(), updated.Status.TerminalAt.Time.UTC(), "the stamp is written once; re-reconciles never rewrite it")
}

// A run that turns terminal on a convergence pass (Job-complete while its
// phase is still Running) is stamped on the same status write that carries
// the terminal phase.
func TestReconcileFreshTerminalTransitionStampsClock(t *testing.T) {
	scheme := newScheme(t)
	run := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "cri-42",
			Namespace:  "default",
			UID:        types.UID("cr-uid-1"),
			Finalizers: []string{"criteriarun.criteria.brokenbots.dev/finalizer"},
		},
		Spec: criteriav1.CriteriaRunSpec{
			TicketID:        "KB-275",
			RepoURL:         "https://github.com/brokenbots/workflow-example.git",
			Image:           "localhost:5000/linear-intake-remote:dev",
			MaxAgentVisits:  2,
			ProviderBaseURL: "http://provider/v1",
		},
		Status: criteriav1.CriteriaRunStatus{Phase: criteriav1.PhaseRunning},
	}
	run.Status.JobName = jobbuilder.RunnerJobName(run)
	jobs := []*batchv1.Job{{
		ObjectMeta: metav1.ObjectMeta{Name: run.Status.JobName, Namespace: "default"},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{
			{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
		}},
	}}
	observedAt := time.Now().Add(-30 * time.Minute).Truncate(time.Second)
	objs := []client.Object{run}
	for _, job := range jobs {
		objs = append(objs, job)
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(objs...).
		WithObjects(objs...).
		Build()

	reconciler := &controller.CriteriaRunReconciler{
		Client: cl,
		Scheme: scheme,
		Castle: &fakeCastle{observation: &castle.Observation{
			RunID:    "castle-run-1",
			Terminal: &castle.Terminal{Success: true, At: observedAt},
		}},
		Queue:           controller.NewRunQueue(),
		OperatorVersion: "test-1.2.3",
	}
	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	require.NoError(t, err)

	var updated criteriav1.CriteriaRun
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(run), &updated))
	assert.Equal(t, criteriav1.PhaseSucceeded, updated.Status.Phase)
	require.NotNil(t, updated.Status.TerminalAt, "the transitioning status write carries the terminal clock")
	assert.Equal(t, observedAt.UTC(), updated.Status.TerminalAt.Time.UTC())
}
