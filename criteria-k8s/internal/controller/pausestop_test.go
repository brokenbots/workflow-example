// CRI-208: the operator's pause/stop signals. The signals ride the castle
// run record's status (obs.RunStatus) — the same authoritative read the
// terminal derivation uses — and actuate per scope only:
//
//	paused  -> hold: per-scope adapter pods stay RUNNING in place, whatever
//	           the provision stream says (no teardown, no restart).
//	stopped -> teardown per scope via the existing deleteRunAdapterPods path;
//	           the still-active provisions must not be re-created while the
//	           stop signal stands.
//	running -> converge from the existing provision-event stream; on resume
//	           the engine re-emits provision_wanted from its stored scope
//	           state and the reconcile re-provisions off that stream.
//
// Paused/stopped are deliberately not terminal (no terminal cleanup fires
// off them) and spare the stall watchdog (absent progress is the engine's
// choice there). These tests go through the full Reconcile so the signal
// handling is exercised exactly as production reaches it.
package controller_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/castle"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/controller"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/events"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/jobbuilder"
)

// pauseStopFixture builds a per-scope run with an Active runner Job (the
// pause/stop signals arrive while the engine holds it) plus a reconciler
// carrying the given castle stub and stall window.
func pauseStopFixture(t *testing.T, name string, obs *fakeCastle, window time.Duration) (*controller.CriteriaRunReconciler, *criteriav1.CriteriaRun, client.Client) {
	t.Helper()
	scheme := newScheme(t)
	run := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			UID:       types.UID("uid-" + name),
		},
		Spec: criteriav1.CriteriaRunSpec{
			TicketID:         "CRI-208",
			RepoURL:          "https://github.com/brokenbots/workflow-example.git",
			PerScopeSessions: true,
		},
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: jobbuilder.RunnerJobName(run), Namespace: run.Namespace},
		Status:     batchv1.JobStatus{Active: 1},
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(run).
		WithObjects(run, job).
		Build()
	r := &controller.CriteriaRunReconciler{
		Client:      cl,
		Scheme:      scheme,
		Castle:      obs,
		Defaults:    jobbuilder.Defaults{DataPVC: "criteria-data"},
		Queue:       controller.NewRunQueue(),
		StallWindow: window,
	}
	return r, run, cl
}

// adapterPodNames lists the names of the run's per-scope adapter pods.
func adapterPodNames(t *testing.T, cl client.Client, run *criteriav1.CriteriaRun) []string {
	t.Helper()
	var pods corev1.PodList
	require.NoError(t, cl.List(context.Background(), &pods,
		client.InNamespace(run.Namespace),
		client.MatchingLabels{
			jobbuilder.LabelRun:  run.Name,
			jobbuilder.LabelRole: jobbuilder.RoleAdapter,
		}))
	names := make([]string, 0, len(pods.Items))
	for _, p := range pods.Items {
		names = append(names, p.Name)
	}
	return names
}

// adapterEnvByName returns the env map of the pod's first container (the
// adapter container) so tests can pin the provision event the pod was built
// from.
func adapterEnvByName(pod corev1.Pod) map[string]string {
	env := make(map[string]string, len(pod.Spec.Containers[0].Env))
	for _, e := range pod.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	return env
}

// The full pause/stop/resume lifecycle through Reconcile:
//
//	running -> two adapter pods created from the provision stream;
//	paused  -> BOTH pods held in place even while the stream drifts (a
//	           release is already in the history): no teardown, no restart,
//	           no churn;
//	stopped -> both pods torn down per scope, and the stream's still-active
//	           provision does not re-create anything while the stop stands.
func TestReconcilePauseAndStopSignalsHoldThenTeardown(t *testing.T) {
	obs := &fakeCastle{observation: &castle.Observation{
		RunID:     "castle-run-208",
		RunStatus: "running",
		Lifecycle: []events.LifecycleEvent{provisionShell, provisionCopilot},
	}}
	r, run, cl := pauseStopFixture(t, "cri-208-signals", obs, 0)

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{RequeueAfter: 10 * time.Second}, res)
	held := adapterPodNames(t, cl, run)
	require.Len(t, held, 2, "baseline: the provision stream actuates both scopes")
	for _, podName := range held {
		assert.Contains(t, podName, "-adp-",
			"the pods are the per-scope provisions' pods")
	}

	// Paused while the stream has already drifted (a scope released): the
	// hold beats the stream. Nothing is deleted, nothing is re-created, and
	// the existing pods are not churned.
	obs.observation.RunStatus = castle.RunStatusPaused
	obs.observation.Lifecycle = []events.LifecycleEvent{provisionShell, provisionCopilot, releaseShell}
	res, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{RequeueAfter: 10 * time.Second}, res,
		"a pausing run keeps polling so the operator honors a later resume")
	assert.ElementsMatch(t, held, adapterPodNames(t, cl, run),
		"pause must hold the exact pods in place: no teardown, no restart")

	// Stopped: per-scope teardown runs and the still-active provision
	// (copilot, the unreleased one) re-creates nothing while the stop stands.
	obs.observation.RunStatus = castle.RunStatusStopped
	res, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	require.NoError(t, err)
	assert.Empty(t, adapterPodNames(t, cl, run),
		"stop tears down the pods per scope, including the scope the stream still marks active")

	var runnerJob batchv1.Job
	require.NoError(t, cl.Get(context.Background(),
		types.NamespacedName{Name: jobbuilder.RunnerJobName(run), Namespace: run.Namespace}, &runnerJob),
		"stop must not touch the runner Job: the engine manages its own lifecycle")
}

// Resume from stopped: the engine re-emits provision_wanted from its stored
// scope state, so a "running" record over the re-provision stream actuates
// the pods again — from the event content, never from operator-side state
// reconstruction (the recreated pod carries the re-emitted event's data).
func TestReconcileResumedRunReprovisionsViaProvisionEvents(t *testing.T) {
	reprovisionShell := provisionShell
	reprovisionShell.TokenFile = "/data/intake/CRI-208/remote-tokens/root-shell/resumed.token"
	obs := &fakeCastle{observation: &castle.Observation{
		RunID:     "castle-run-208",
		RunStatus: castle.RunStatusStopped,
		Lifecycle: []events.LifecycleEvent{provisionShell, provisionCopilot},
	}}
	r, run, cl := pauseStopFixture(t, "cri-208-resume", obs, 0)

	// The stop first tears down whatever exists (nothing here).
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	require.NoError(t, err)
	assert.Empty(t, adapterPodNames(t, cl, run))

	// Resume: running again over the re-emitted provision stream.
	obs.observation.RunStatus = "running"
	obs.observation.Lifecycle = []events.LifecycleEvent{reprovisionShell, provisionCopilot}
	res, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{RequeueAfter: 10 * time.Second}, res)

	pods := adapterPodNames(t, cl, run)
	require.Len(t, pods, 2, "resume re-provisions every scope from the stream")

	var shell corev1.PodList
	require.NoError(t, cl.List(context.Background(), &shell,
		client.InNamespace(run.Namespace), client.MatchingLabels{
			jobbuilder.LabelRun:  run.Name,
			jobbuilder.LabelRole: jobbuilder.RoleAdapter,
		}))
	found := false
	for _, pod := range shell.Items {
		env := adapterEnvByName(pod)
		if env["CRITERIA_REMOTE_TOKEN_FILE"] == reprovisionShell.TokenFile {
			found = true
		}
	}
	assert.True(t, found,
		"the recreated pod is built from the re-emitted provision event, not operator-side state")
}

// The stall watchdog must read a pausing/stopping run's absent progress as
// the engine's choice, not a wedge: no failure, no condition, and the runner
// Job survives (a later resume reattaches to it). Stopped still runs its
// per-scope teardown; paused holds even the pods.
func TestStallWatchdogSparesPausingAndStoppingRuns(t *testing.T) {
	lastProgress := time.Now().UTC().Add(-40 * time.Minute).Truncate(time.Second)
	for _, status := range []string{castle.RunStatusPaused, castle.RunStatusStopped} {
		t.Run(status, func(t *testing.T) {
			obs := &castle.Observation{
				RunID:        "castle-run-24",
				RunStatus:    status,
				Lifecycle:    []events.LifecycleEvent{provisionShell},
				LastProgress: lastProgress,
			}
			r, run, cl := stalledRunFixture(t, "cri-208-idle", true, obs, 30*time.Minute)

			res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
			require.NoError(t, err)
			assert.Equal(t, ctrl.Result{RequeueAfter: 10 * time.Second}, res,
				"the idle run keeps polling so the operator honors a later signal")

			var updated criteriav1.CriteriaRun
			require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(run), &updated))
			assert.Equal(t, criteriav1.PhaseRunning, updated.Status.Phase,
				"run status %q is deliberately idle: the watchdog must not fail it", status)
			_, ok := conditionFor(updated.Status, controller.ConditionStallWatchdog)
			assert.False(t, ok, "run status %q must not carry the StallWatchdog condition", status)

			var runnerJob batchv1.Job
			require.NoError(t, cl.Get(context.Background(),
				types.NamespacedName{Name: jobbuilder.RunnerJobName(run), Namespace: run.Namespace}, &runnerJob),
				"the runner Job is the engine's: no signal handling deletes it")

			pods := adapterPodNames(t, cl, run)
			if status == castle.RunStatusPaused {
				assert.Len(t, pods, 1, "pause holds the pod")
			} else {
				assert.Empty(t, pods, "stop tears the pod down per scope")
			}
		})
	}
}

// A pausing legacy (non-per-scope) run: signal handling never fires — the
// legacy shape actuates through Jobs the engine owns, and pause does not
// touch them; the reconcile stays a clean no-op for pods.
func TestReconcilePausedLegacyRunKeepsJobsUnowned(t *testing.T) {
	scheme := newScheme(t)
	run := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cri-208-legacy",
			Namespace: "default",
			UID:       types.UID("uid-cri-208-legacy"),
		},
		Spec: criteriav1.CriteriaRunSpec{
			TicketID:         "CRI-208",
			RepoURL:          "https://github.com/brokenbots/workflow-example.git",
			PerScopeSessions: false,
		},
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: jobbuilder.RunnerJobName(run), Namespace: run.Namespace},
		Status:     batchv1.JobStatus{Active: 1},
	}
	adapterJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: run.Name + "-adapter-shell", Namespace: run.Namespace},
		Status:     batchv1.JobStatus{Active: 1},
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(run).
		WithObjects(run, job, adapterJob).
		Build()
	r := &controller.CriteriaRunReconciler{
		Client:   cl,
		Scheme:   scheme,
		Castle:   &fakeCastle{observation: &castle.Observation{RunID: "castle-run-208", RunStatus: castle.RunStatusPaused}},
		Defaults: jobbuilder.Defaults{DataPVC: "criteria-data"},
		Queue:    controller.NewRunQueue(),
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	require.NoError(t, err)

	var runnerJob batchv1.Job
	require.NoError(t, cl.Get(context.Background(),
		types.NamespacedName{Name: jobbuilder.RunnerJobName(run), Namespace: run.Namespace}, &runnerJob),
		"pause must not touch legacy child Jobs")
	var adapter batchv1.Job
	require.NoError(t, cl.Get(context.Background(),
		types.NamespacedName{Name: run.Name + "-adapter-shell", Namespace: run.Namespace}, &adapter),
		"pause must not touch legacy child Jobs")
}
