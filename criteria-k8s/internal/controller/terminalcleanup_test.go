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
	corev1 "k8s.io/api/core/v1"
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
			deletesAfterFirstPass := len(cl.deletes)
			res, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
			require.NoError(t, err)
			assert.Equal(t, ctrl.Result{RequeueAfter: 10 * time.Second}, res)
			assert.Len(t, cl.deletes, deletesAfterFirstPass,
				"the second terminal pass must be a no-op delete-wise")
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
	// With the stall watchdog disabled (StallWindow unset) an active
	// non-terminal run does not self-poll; the Job's own watch drives the
	// next pass. No new polling is introduced here either way.
	assert.Equal(t, ctrl.Result{}, res)

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

// runnerPodFixture builds a runner-pod-shaped corev1.Pod (the labels the
// terminal-restart guard selects on, a RestartPolicy to classify against)
// and lets each test set the container states it exercises.
func runnerPodFixture(runName string, restartPolicy corev1.RestartPolicy, mutate func(*corev1.Pod)) corev1.Pod {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      runName + "-runner",
			Namespace: "default",
			Labels: map[string]string{
				jobbuilder.LabelRun:  runName,
				jobbuilder.LabelRole: jobbuilder.RoleRunner,
			},
		},
		Spec: corev1.PodSpec{RestartPolicy: restartPolicy},
	}
	if mutate != nil {
		mutate(&pod)
	}
	return pod
}

// containerStatus is a one-line builder for a pod container status with a
// single observed state, so the restart-pending fixtures read at a glance.
func containerStatus(name string, st corev1.ContainerState, restarts int32, last corev1.ContainerState) corev1.ContainerStatus {
	return corev1.ContainerStatus{
		Name:                 name,
		State:                st,
		RestartCount:         restarts,
		LastTerminationState: last,
	}
}

// A terminal run whose runner container is mid-flight (Running) defers the
// child-job deletion (KB-302): deleting the runner Job then would let the
// kubelet orphan the agent through background garbage collection, and that
// orphan registers with castle. The pass stamps a TerminalCleanup condition
// and requeues on the short reap interval instead; when the container
// settles, the next pass reaps and clears the condition.
func TestReconcileTerminalRunDefersChildJobDeletionWhileRunnerRunning(t *testing.T) {
	r, cl, run := terminalJobFixture(t, "kb-302-defer", criteriav1.PhaseFailed)
	pod := runnerPodFixture(run.Name, corev1.RestartPolicyOnFailure, func(p *corev1.Pod) {
		p.Status.Phase = corev1.PodRunning
		p.Status.ContainerStatuses = []corev1.ContainerStatus{
			containerStatus("runner", corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}}, 0, corev1.ContainerState{}),
		}
	})
	require.NoError(t, cl.Create(context.Background(), &pod))

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{RequeueAfter: 5 * time.Second}, res, "the deferred reap requeues on the short reap interval")
	assert.Empty(t, cl.deletes, "no child Job may be deleted while the runner container is running")
	require.Len(t, jobNamesForRun(t, cl, run.Name), 3, "all child Jobs must survive the deferred pass")

	var parked criteriav1.CriteriaRun
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(run), &parked))
	cond := metav1.Condition{}
	for _, c := range parked.Status.Conditions {
		if c.Type == controller.ConditionTerminalCleanup {
			cond = c
		}
	}
	require.NotEmpty(t, cond.Type, "the TerminalCleanup condition must be stamped while deferred")
	assert.Equal(t, controller.ReasonTerminalRestartPending, cond.Reason)
	assert.Contains(t, cond.Message, "is running")

	// The runner settles (clean exit, OnFailure starts nothing further):
	// the next pass reaps the Jobs and clears the deferral condition.
	pod.Status.Phase = corev1.PodSucceeded
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{
		containerStatus("runner", corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}, 0, corev1.ContainerState{}),
	}
	require.NoError(t, cl.Status().Update(context.Background(), &pod))

	res, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	require.NoError(t, err)
	assert.Len(t, cl.deletes, 3, "the settled pass reaps the child Jobs")
	assert.Empty(t, jobNamesForRun(t, cl, run.Name))

	var cleared criteriav1.CriteriaRun
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(run), &cleared))
	for _, c := range cleared.Status.Conditions {
		assert.NotEqual(t, controller.ConditionTerminalCleanup, c.Type,
			"the TerminalCleanup condition must be cleared once the reap completed")
	}
}

// A runner pod between restart attempts (a crashed prior container waiting
// in CrashLoopBackOff) also defers the reap: the kubelet's backoff is mid
// countdown, so deleting the runner Job now is exactly the corpse window
// the guard exists to close (KB-302).
func TestReconcileTerminalRunDefersChildJobDeletionWhileRestartPending(t *testing.T) {
	r, cl, run := terminalJobFixture(t, "kb-302-crash", criteriav1.PhaseFailed)
	pod := runnerPodFixture(run.Name, corev1.RestartPolicyOnFailure, func(p *corev1.Pod) {
		p.Status.Phase = corev1.PodRunning
		p.Status.ContainerStatuses = []corev1.ContainerStatus{
			containerStatus("runner", corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}, 3,
				corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137}}),
		}
	})
	require.NoError(t, cl.Create(context.Background(), &pod))

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	require.NoError(t, err)
	assert.Empty(t, cl.deletes, "no child Job may be deleted while a restart attempt is pending")

	// A nonzero-terminated container under OnFailure has a restart attempt
	// incoming just the same: defer.
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{
		containerStatus("runner", corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}}, 1, corev1.ContainerState{}),
	}
	require.NoError(t, cl.Status().Update(context.Background(), &pod))
	_, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	require.NoError(t, err)
	assert.Empty(t, cl.deletes, "no child Job may be deleted while an OnFailure restart is incoming")

	// A scheduled-but-unstarted container can still land its start attempt:
	// defer too.
	pod.Status.Phase = corev1.PodPending
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{
		containerStatus("runner", corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}, 0, corev1.ContainerState{}),
	}
	require.NoError(t, cl.Status().Update(context.Background(), &pod))
	_, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	require.NoError(t, err)
	assert.Empty(t, cl.deletes, "no child Job may be deleted while the container has not started")

	require.Len(t, jobNamesForRun(t, cl, run.Name), 3, "all child Jobs must survive every deferred pass")
}

// A runner pod whose restart policy starts no further attempt (Never) is
// settled even when its container exited nonzero: the reap proceeds and a
// stamped deferral condition is cleared (KB-302).
func TestReconcileTerminalRunReapsSettledRunnerPod(t *testing.T) {
	r, cl, run := terminalJobFixture(t, "kb-302-never", criteriav1.PhaseFailed)
	pod := runnerPodFixture(run.Name, corev1.RestartPolicyNever, func(p *corev1.Pod) {
		p.Status.Phase = corev1.PodFailed
		p.Status.ContainerStatuses = []corev1.ContainerStatus{
			containerStatus("runner", corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}}, 1, corev1.ContainerState{}),
		}
	})
	require.NoError(t, cl.Create(context.Background(), &pod))

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	require.NoError(t, err)
	assert.Len(t, cl.deletes, 3, "a settled runner pod reaps immediately")
	assert.Empty(t, jobNamesForRun(t, cl, run.Name))
}

// The deferral is bounded (KB-302): with the force window elapsed against a
// still-unsettled runner pod, the reap is forced and logs loudly, then the
// TerminalCleanup condition is cleared. This is the wedge-killer for a pod
// that never settles (ImagePullBackOff forever, a hung restart, ...).
func TestReconcileTerminalRunForcesChildJobDeletionPastGuardWindow(t *testing.T) {
	r, cl, run := terminalJobFixture(t, "kb-302-force", criteriav1.PhaseFailed)
	r.TerminalReapForceAfter = time.Second
	pod := runnerPodFixture(run.Name, corev1.RestartPolicyOnFailure, func(p *corev1.Pod) {
		p.Status.Phase = corev1.PodRunning
		p.Status.ContainerStatuses = []corev1.ContainerStatus{
			containerStatus("runner", corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}}, 0, corev1.ContainerState{}),
		}
	})
	require.NoError(t, cl.Create(context.Background(), &pod))

	// Pre-stamp the deferral condition with a start time behind the window,
	// so the force path fires deterministically on this first pass.
	var fresh criteriav1.CriteriaRun
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(run), &fresh))
	stale := fresh.DeepCopy()
	stale.Status.Conditions = []metav1.Condition{{
		Type:               controller.ConditionTerminalCleanup,
		Status:             metav1.ConditionTrue,
		Reason:             controller.ReasonTerminalRestartPending,
		Message:            "terminal child-job deletion deferred (KB-302)",
		LastTransitionTime: metav1.NewTime(time.Now().Add(-2 * time.Minute)),
	}}
	require.NoError(t, cl.Status().Update(context.Background(), stale))

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	require.NoError(t, err)
	_ = res
	assert.Len(t, cl.deletes, 3, "the expired deferral forces the child-job deletion")
	assert.Empty(t, jobNamesForRun(t, cl, run.Name))

	var cleared criteriav1.CriteriaRun
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(run), &cleared))
	for _, c := range cleared.Status.Conditions {
		assert.NotEqual(t, controller.ConditionTerminalCleanup, c.Type,
			"the TerminalCleanup condition must be cleared after the forced reap")
	}
}

// The same pass that provisions the run's Jobs resolves the terminal view
// with a live runner container (KB-302): this pass defers the reap instead
// of the old provision-guard skip, so the next pass decides via the guard.
// The deferred stamp keeps the cleanup provable either way.
func TestReconcileTerminalRunWithLiveRunnerProvisionsWithoutReaping(t *testing.T) {
	scheme := newScheme(t)
	run := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{Name: "kb-302-live", Namespace: "default", UID: types.UID("kb-302-live-uid")},
		Spec: criteriav1.CriteriaRunSpec{
			TicketID:        "KB-302",
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
		Castle:   &fakeCastle{observation: &castle.Observation{RunID: "castle-run-kb-302", Terminal: castleTerminal(true)}},
		Defaults: jobbuilder.Defaults{DataPVC: "criteria-data"},
		Queue:    controller.NewRunQueue(),
	}
	pod := runnerPodFixture(run.Name, corev1.RestartPolicyOnFailure, func(p *corev1.Pod) {
		p.Status.Phase = corev1.PodRunning
		p.Status.ContainerStatuses = []corev1.ContainerStatus{
			containerStatus("runner", corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}}, 0, corev1.ContainerState{}),
		}
	})
	require.NoError(t, cl.Create(context.Background(), &pod))

	// Pass 1 provisions the Jobs + resolves terminal: no delete this pass.
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, res)
	assert.Empty(t, cl.deletes, "the provisioning pass must not delete the Jobs it just created")

	// Pass 2 sees the persisted terminal view and the live runner: defer.
	res, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{RequeueAfter: 5 * time.Second}, res, "the deferred reap requeues on the short reap interval")
	assert.Empty(t, cl.deletes, "no child Job may be deleted while the runner container is running")
	require.Len(t, jobNamesForRun(t, cl, run.Name), 3)
}