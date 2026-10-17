package controller_test

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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// jobDelete records one Delete call observed by deleteTrackingClient.
type jobDelete struct {
	name        string
	propagation string
}

// deleteTrackingClient records every Delete issued through it (including the
// propagation policy requested) and then delegates to the wrapped client.
type deleteTrackingClient struct {
	client.Client
	deletes []jobDelete
}

func (d *deleteTrackingClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	ops := &client.DeleteOptions{}
	ops.ApplyOptions(opts)
	propagation := ""
	if ops.PropagationPolicy != nil {
		propagation = string(*ops.PropagationPolicy)
	}
	d.deletes = append(d.deletes, jobDelete{name: obj.GetName(), propagation: propagation})
	return d.Client.Delete(ctx, obj, opts...)
}

// terminalJobFixture builds a CriteriaRun with live child Jobs for the
// KB-275 terminal-cleanup tests: the runner job is fresh-complete (its own
// completion already resolved the run), and the child Jobs are reaped on the
// next pass.
func terminalJobFixture(t *testing.T, name string, phase criteriav1.CriteriaRunPhase) (*controller.CriteriaRunReconciler, *deleteTrackingClient, *criteriav1.CriteriaRun) {
	t.Helper()
	scheme := newScheme(t)
	run := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  "default",
			UID:        types.UID(name + "-uid"),
			Finalizers: []string{"criteriarun.criteria.brokenbots.dev/finalizer"},
		},
		Spec: criteriav1.CriteriaRunSpec{
			TicketID:        "KB-275",
			RepoURL:         "https://github.com/brokenbots/workflow-example.git",
			Image:           "localhost:5000/linear-intake-remote:dev",
			MaxAgentVisits:  2,
			ProviderBaseURL: "http://provider/v1",
		},
		Status: criteriav1.CriteriaRunStatus{
			Phase: phase,
		},
	}
	run.Status.JobName = jobbuilder.RunnerJobName(run)

	cl := &deleteTrackingClient{Client: fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(run).
		WithObjects(run).
		Build()}
	for _, job := range jobbuilder.BuildAll(run, jobbuilder.Defaults{DataPVC: "criteria-data"}) {
		require.NoError(t, cl.Create(context.Background(), job))
	}

	r := &controller.CriteriaRunReconciler{
		Client:   cl,
		Scheme:   scheme,
		Castle:   &fakeCastle{},
		Defaults: jobbuilder.Defaults{DataPVC: "criteria-data"},
		Queue:    controller.NewRunQueue(),
	}
	return r, cl, run
}

// A run whose resolved phase is terminal (Succeeded or Failed) never keeps
// child Jobs (KB-275): the first terminal pass deletes them with background
// propagation (their pods fall with the Job), and every later pass is a
// no-op — no re-creates, no spurious deletes, no requeue beyond the normal
// per-scope interval.
func TestReconcileTerminalRunDeletesChildJobs(t *testing.T) {
	for _, tc := range []struct {
		phase criteriav1.CriteriaRunPhase
	}{
		{phase: criteriav1.PhaseSucceeded},
		{phase: criteriav1.PhaseFailed},
	} {
		t.Run(string(tc.phase), func(t *testing.T) {
			r, cl, run := terminalJobFixture(t, "cri-275", tc.phase)

			res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
			require.NoError(t, err)
			assert.Equal(t, ctrl.Result{RequeueAfter: 10 * time.Second}, res,
				"the normal terminal-poll requeue must not change")

			deleted := cl.deletes
			require.Len(t, deleted, 3, "runner and both adapter jobs must go on the first terminal pass")
			assert.Empty(t, jobNamesForRun(t, cl, run.Name), "no child Job may survive the terminal pass")
			for _, d := range deleted {
				assert.Equal(t, string(metav1.DeletePropagationBackground), d.propagation,
					"terminal cleanup must delete with background propagation")
			}
			assert.ElementsMatch(t, []string{
				jobbuilder.RunnerJobName(run),
				jobbuilder.RunnerJobName(run) + "-adapter-shell",
				jobbuilder.RunnerJobName(run) + "-adapter-copilot",
			}, []string{deleted[0].name, deleted[1].name, deleted[2].name})

			// Second pass on the same terminal run: no jobs left, and the
			// reconciler must not re-create or delete anything.
			res, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
			require.NoError(t, err)
			assert.Equal(t, ctrl.Result{RequeueAfter: 10 * time.Second}, res)
			assert.Empty(t, cl.deletes, "the second terminal pass must be a no-op delete-wise")
			assert.Empty(t, jobNamesForRun(t, cl, run.Name), "no child Job may be re-created for a terminal run")

			var updated criteriav1.CriteriaRun
			require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(run), &updated))
			assert.Equal(t, tc.phase, updated.Status.Phase)
			assert.Equal(t, jobbuilder.RunnerJobName(run), updated.Status.JobName)
		})
	}
}

// A terminal run without child Jobs reconciles as a pure no-op: no deletes,
// no creates, no requeue beyond the normal interval (KB-275 idempotency).
func TestReconcileTerminalRunWithoutChildJobsIsNoOp(t *testing.T) {
	scheme := newScheme(t)
	run := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "cri-276",
			Namespace:  "default",
			UID:        types.UID("cri-276-uid"),
			Finalizers: []string{"criteriarun.criteria.brokenbots.dev/finalizer"},
		},
		Spec: criteriav1.CriteriaRunSpec{TicketID: "KB-275"},
		Status: criteriav1.CriteriaRunStatus{
			Phase:                 criteriav1.PhaseSucceeded,
			CastleTerminalObserved: true,
		},
	}
	cl := &deleteTrackingClient{Client: fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(run).
		WithObjects(run).
		Build()}
	r := &controller.CriteriaRunReconciler{
		Client:   cl,
		Scheme:   scheme,
		Castle:   &fakeCastle{},
		Defaults: jobbuilder.Defaults{DataPVC: "criteria-data"},
		Queue:    controller.NewRunQueue(),
	}

	for i := 0; i < 2; i++ {
		res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
		require.NoError(t, err)
		assert.Equal(t, ctrl.Result{}, res, "an already-settled terminal run must not requeue")
		assert.Empty(t, cl.deletes, "no spurious delete may fire for a terminal run without child Jobs")
		assert.Empty(t, jobNamesForRun(t, cl, "cri-276"), "no Job may be created for a terminal run")
	}
}

// A non-terminal run keeps its child Jobs exactly as before (KB-275 must
// only tighten the terminal case): the active runner resolves Running and no
// delete fires on the reconcile.
func TestReconcileNonTerminalRunKeepsChildJobs(t *testing.T) {
	r, cl, run := terminalJobFixture(t, "cri-277", "")
	var runner batchv1.Job
	require.NoError(t, cl.Get(context.Background(), types.NamespacedName{Name: jobbuilder.RunnerJobName(run), Namespace: "default"}, &runner))
	runner.Status.Active = 1
	require.NoError(t, cl.Status().Update(context.Background(), &runner))

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{RequeueAfter: 10 * time.Second}, res)

	assert.Empty(t, cl.deletes, "a running run's child Jobs must not be deleted")
	assert.Len(t, jobNamesForRun(t, cl, "cri-277"), 3, "all child Jobs must survive the running pass")

	var updated criteriav1.CriteriaRun
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(run), &updated))
	assert.Equal(t, criteriav1.PhaseRunning, updated.Status.Phase)
}

// The resolved view is the only source: a freshly restarted operator (no
// in-memory state) with a disabled castle still deletes a terminal run's
// child Jobs on the first reconcile after boot.
func TestReconcileTerminalRunDeletesChildJobsWithCastleDisabled(t *testing.T) {
	scheme := newScheme(t)
	run := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "cri-278",
			Namespace:  "default",
			UID:        types.UID("cri-278-uid"),
			Finalizers: []string{"criteriarun.criteria.brokenbots.dev/finalizer"},
		},
		Spec: criteriav1.CriteriaRunSpec{TicketID: "KB-275"},
		Status: criteriav1.CriteriaRunStatus{
			Phase:   criteriav1.PhaseFailed,
			JobName: "cri-278",
		},
	}
	cl := &deleteTrackingClient{Client: fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(run).
		WithObjects(run).
		Build()}
	for _, job := range jobbuilder.BuildAll(run, jobbuilder.Defaults{DataPVC: "criteria-data"}) {
		require.NoError(t, cl.Create(context.Background(), job))
	}
	r := &controller.CriteriaRunReconciler{
		Client:   cl,
		Scheme:   scheme,
		Castle:   disabledCastle{},
		Defaults: jobbuilder.Defaults{DataPVC: "criteria-data"},
		Queue:    controller.NewRunQueue(),
	}

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, res, "a disabled castle stops the terminal polling loop")
	assert.Empty(t, jobNamesForRun(t, cl, "cri-278"), "the terminal run's child Jobs must go even without castle")
}

// The mid-provisioning guard: when the castle terminal observation lands on
// the same pass that provisions the run's Jobs, that pass may not delete the
// Jobs it just created. The terminal view is persisted, so the next pass —
// the persisted-terminal branch — reaps them.
func TestReconcileCastleTerminalProvisionPassDefersJobDeletion(t *testing.T) {
	scheme := newScheme(t)
	run := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{Name: "cri-279", Namespace: "default", UID: types.UID("cri-279-uid")},
		Spec: criteriav1.CriteriaRunSpec{
			TicketID:        "KB-275",
			RepoURL:         "https://github.com/brokenbots/workflow-example.git",
			Image:           "localhost:5000/linear-intake-remote:dev",
			MaxAgentVisits:  2,
			ProviderBaseURL: "http://provider/v1",
		},
	}
	cl := &deleteTrackingClient{Client: fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(run).
		WithObjects(run).
		Build()}
	r := &controller.CriteriaRunReconciler{
		Client:   cl,
		Scheme:   scheme,
		Castle:   &fakeCastle{observation: &castle.Observation{RunID: "castle-run-1", Terminal: castleTerminal(true)}},
		Defaults: jobbuilder.Defaults{DataPVC: "criteria-data"},
		Queue:    controller.NewRunQueue(),
	}

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, res)
	assert.Empty(t, cl.deletes, "the provisioning pass must not delete the Jobs it just created")
	require.Len(t, jobNamesForRun(t, cl, "cri-279"), 3)

	var updated criteriav1.CriteriaRun
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(run), &updated))
	assert.Equal(t, criteriav1.PhaseSucceeded, updated.Status.Phase)
	assert.True(t, updated.Status.CastleTerminalObserved)

	// The next pass resolves the persisted terminal view and reaps the Jobs.
	res, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, res)
	assert.Len(t, cl.deletes, 3, "the persisted-terminal pass reaps the child Jobs")
	assert.Empty(t, jobNamesForRun(t, cl, "cri-279"))
}