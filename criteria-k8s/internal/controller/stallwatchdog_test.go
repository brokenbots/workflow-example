// KB-24: the criteria-run stall watchdog. A run whose castle event stream
// stops showing step progress while its phase is still Running is failed —
// child Jobs and per-scope adapter pods deleted, phase Failed with a
// StallWatchdog condition, queue released — so CriteriaRun reflects Failed
// instead of hanging in Running forever (the CRI-271 wedge signature: a
// reviewer-loop step whose adapter session died emits only heartbeats, no
// Job failure ever arrives, and the ticket is parked silently).
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
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/jobbuilder"
)

// stalledRunFixture builds a run with an Active runner Job (the CRI-271
// wedge: the job never fails, it just hangs) plus a reconciler wired to a
// fake castle carrying the given observation and the given stall window.
func stalledRunFixture(t *testing.T, name string, perScope bool, obs *castle.Observation, window time.Duration) (*controller.CriteriaRunReconciler, *criteriav1.CriteriaRun, client.Client) {
	t.Helper()
	scheme := newScheme(t)
	run := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			UID:       types.UID("uid-" + name),
		},
		Spec: criteriav1.CriteriaRunSpec{
			TicketID:         "KB-24",
			RepoURL:          "https://github.com/brokenbots/workflow-example.git",
			PerScopeSessions: perScope,
		},
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: jobbuilder.RunnerJobName(run), Namespace: run.Namespace},
		Status:     batchv1.JobStatus{Active: 1},
	}

	objects := []client.Object{run, job}
	if perScope {
		objects = append(objects, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name + "-adapter-copilot",
				Namespace: run.Namespace,
				Labels: map[string]string{
					jobbuilder.LabelRun:  run.Name,
					jobbuilder.LabelRole: jobbuilder.RoleAdapter,
				},
			},
		})
	}

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(run).
		WithObjects(objects...).
		Build()

	r := &controller.CriteriaRunReconciler{
		Client:      cl,
		Scheme:      scheme,
		Castle:      &fakeCastle{observation: obs},
		Defaults:    jobbuilder.Defaults{DataPVC: "criteria-data"},
		Queue:       controller.NewRunQueue(),
		StallWindow: window,
	}
	return r, run, cl
}

// The wedged signature: the stream stopped at the last step event 40 minutes
// ago and has emitted only heartbeats since. The run is failed with the
// StallWatchdog condition, the wedged runner Job and its per-scope adapter
// pod are deleted, and the queue slot is released so the next run for the
// repo is admitted.
func TestStallWatchdogFailsStalledRun(t *testing.T) {
	lastProgress := time.Now().UTC().Add(-40 * time.Minute).Truncate(time.Second)
	r, run, cl := stalledRunFixture(t, "kb-24-stalled", true, &castle.Observation{
		RunID:        "castle-run-24",
		LastProgress: lastProgress,
	}, 30*time.Minute)

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, res, "the watchdog-failed pass must not requeue")

	var updated criteriav1.CriteriaRun
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(run), &updated))
	assert.Equal(t, criteriav1.PhaseFailed, updated.Status.Phase)
	require.NotNil(t, updated.Status.LastStepProgress, "the observed step progress must be stamped")
	assert.True(t, updated.Status.LastStepProgress.Time.Equal(lastProgress), "LastStepProgress must be the newest step event, not now")
	cond, ok := conditionFor(updated.Status, controller.ConditionStallWatchdog)
	require.True(t, ok, "the run must carry the StallWatchdog condition")
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, "StallWatchdog", cond.Reason)
	assert.Contains(t, cond.Message, "no step progress")

	var runnerJob batchv1.Job
	err = cl.Get(context.Background(), types.NamespacedName{Name: jobbuilder.RunnerJobName(run), Namespace: run.Namespace}, &runnerJob)
	assert.Error(t, err, "the wedged runner Job must be deleted")

	var pods corev1.PodList
	require.NoError(t, cl.List(context.Background(), &pods, client.InNamespace(run.Namespace)))
	assert.Empty(t, pods.Items, "the per-scope adapter pods must be deleted with the runner")

	// The queue slot is released: the next run for the repo is admitted.
	queued := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{Name: "kb-24-queued", Namespace: "default", UID: types.UID("uid-queued")},
		Spec:       criteriav1.CriteriaRunSpec{TicketID: "KB-25", RepoURL: run.Spec.RepoURL},
	}
	require.NoError(t, cl.Create(context.Background(), queued))
	_, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(queued)})
	require.NoError(t, err)
	var admitted criteriav1.CriteriaRun
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(queued), &admitted))
	require.NotNil(t, admitted.Status.Queue)
	assert.Equal(t, 0, admitted.Status.Queue.Position, "the next run must be admitted after the stalled run's slot is released")

	// Later passes are terminal passes: the watchdog-failed run's engine is
	// gone, so castle will never record a terminal and the polling stops.
	res, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, res, "a watchdog-failed run must not poll castle for a terminal forever")
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(run), &updated))
	assert.Equal(t, criteriav1.PhaseFailed, updated.Status.Phase)
	var jobs batchv1.JobList
	require.NoError(t, cl.List(context.Background(), &jobs, client.InNamespace(run.Namespace), client.MatchingLabels{jobbuilder.LabelRun: run.Name}))
	assert.Empty(t, jobs.Items, "a watchdog-failed run must not have its Jobs recreated")
}

// A run whose newest step event is inside the window keeps running and only
// gets its progress stamp refreshed; the enabling window also makes the
// non-per-scope run poll on the poll interval (a wedged Job emits no watch
// events while it hangs).
func TestStallWatchdogKeepsProgressingRun(t *testing.T) {
	lastProgress := time.Now().UTC().Add(-5 * time.Minute).Truncate(time.Second)
	r, run, cl := stalledRunFixture(t, "kb-24-progressing", false, &castle.Observation{
		RunID:        "castle-run-24",
		LastProgress: lastProgress,
	}, 30*time.Minute)

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	require.NoError(t, err)
	assert.Equal(t, 10*time.Second, res.RequeueAfter, "a running non-per-scope run polls while the watchdog is enabled")

	var updated criteriav1.CriteriaRun
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(run), &updated))
	assert.Equal(t, criteriav1.PhaseRunning, updated.Status.Phase)
	require.NotNil(t, updated.Status.LastStepProgress)
	assert.True(t, updated.Status.LastStepProgress.Time.Equal(lastProgress))
	_, ok := conditionFor(updated.Status, controller.ConditionStallWatchdog)
	assert.False(t, ok, "a progressing run must not carry the StallWatchdog condition")
}

// A stream of heartbeats only (no step event ever consumed) keeps the
// already-stamped baseline instead of resetting it every pass, so a run
// wedged before its first step event still trips the watchdog once the
// baseline ages past the window.
func TestStallWatchdogBaselinePreservedOnHeartbeatOnlyStream(t *testing.T) {
	baseline := time.Now().UTC().Add(-40 * time.Minute).Truncate(time.Second)
	scheme := newScheme(t)
	run := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{Name: "kb-24-baseline", Namespace: "default", UID: types.UID("uid-baseline")},
		Spec:       criteriav1.CriteriaRunSpec{TicketID: "KB-24", RepoURL: "https://github.com/brokenbots/workflow-example.git"},
		Status:     criteriav1.CriteriaRunStatus{LastStepProgress: ptrTime(baseline)},
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
		Castle:      &fakeCastle{observation: &castle.Observation{RunID: "castle-run-24"}},
		Defaults:    jobbuilder.Defaults{DataPVC: "criteria-data"},
		Queue:       controller.NewRunQueue(),
		StallWindow: 30 * time.Minute,
	}

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, res)

	var updated criteriav1.CriteriaRun
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(run), &updated))
	assert.Equal(t, criteriav1.PhaseFailed, updated.Status.Phase)
	require.NotNil(t, updated.Status.LastStepProgress)
	assert.True(t, updated.Status.LastStepProgress.Time.Equal(baseline),
		"the baseline stamp must be preserved, not reset to now by a heartbeat-only stream")
	cond, ok := conditionFor(updated.Status, controller.ConditionStallWatchdog)
	require.True(t, ok)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
}

// A healthy long copilot step must not be failed (KB-24 review B): the
// turn entered 40m ago — inside its 60m budget — and its in-step stream
// (agent.message deltas, tool round-trips, permission gates, heartbeats; no
// StepLog, no StepOutcome) folded to a LastProgress 25m old. The watchdog
// clock measures time since the newest activity event, not step duration,
// so the run keeps running and the stamp refreshes; the stream→baseline
// mapping itself is covered by the castle classifier tests
// (TestObserveHealthyCopilotStreamLongerThanWindowKeepsFreshProgress).
func TestStallWatchdogSparesHealthyLongCopilotStep(t *testing.T) {
	lastActivity := time.Now().UTC().Add(-25 * time.Minute).Truncate(time.Second)
	r, run, cl := stalledRunFixture(t, "kb-24-healthy-copilot", true, &castle.Observation{
		RunID:        "castle-run-24",
		LastProgress: lastActivity,
	}, 30*time.Minute)

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	require.NoError(t, err)
	assert.Equal(t, 10*time.Second, res.RequeueAfter,
		"a running run keeps polling on the poll interval while the watchdog is enabled")

	var updated criteriav1.CriteriaRun
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(run), &updated))
	assert.Equal(t, criteriav1.PhaseRunning, updated.Status.Phase,
		"a 40m-old copilot step with fresh agent activity must not be failed on step duration")
	require.NotNil(t, updated.Status.LastStepProgress)
	assert.True(t, updated.Status.LastStepProgress.Time.Equal(lastActivity))
	_, ok := conditionFor(updated.Status, controller.ConditionStallWatchdog)
	assert.False(t, ok, "a healthy long copilot step must not carry the StallWatchdog condition")

	var runnerJob batchv1.Job
	err = cl.Get(context.Background(), types.NamespacedName{Name: jobbuilder.RunnerJobName(run), Namespace: run.Namespace}, &runnerJob)
	require.NoError(t, err, "the healthy runner Job must not be deleted")
}

// A zero stall window disables the watchdog entirely: no stamping and no
// failure, and a non-per-scope run gets no poll either (the pre-KB-24
// requeue behavior).
func TestStallWatchdogDisabledWhenWindowZero(t *testing.T) {
	r, run, cl := stalledRunFixture(t, "kb-24-disabled", false, &castle.Observation{
		RunID:        "castle-run-24",
		LastProgress: time.Now().UTC().Add(-40 * time.Minute),
	}, 0)

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, res, "a disabled watchdog must not add the running-run poll")

	var updated criteriav1.CriteriaRun
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(run), &updated))
	assert.Equal(t, criteriav1.PhaseRunning, updated.Status.Phase)
	assert.Nil(t, updated.Status.LastStepProgress, "a disabled watchdog must not stamp step progress")
	_, ok := conditionFor(updated.Status, controller.ConditionStallWatchdog)
	assert.False(t, ok)
}

// An inconclusive castle observation must never drive the watchdog: no
// stamping and no failure off a history the controller could not see.
func TestStallWatchdogInertOnObservationError(t *testing.T) {
	scheme := newScheme(t)
	run := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{Name: "kb-24-obs-error", Namespace: "default", UID: types.UID("uid-obs-error")},
		Spec:       criteriav1.CriteriaRunSpec{TicketID: "KB-24", RepoURL: "https://github.com/brokenbots/workflow-example.git"},
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
		Castle:      &fakeCastle{err: castle.ErrRunNotFound},
		Defaults:    jobbuilder.Defaults{DataPVC: "criteria-data"},
		Queue:       controller.NewRunQueue(),
		StallWindow: 30 * time.Minute,
	}

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	require.NoError(t, err)
	assert.Equal(t, 10*time.Second, res.RequeueAfter, "an observation error retries on the poll interval")

	var updated criteriav1.CriteriaRun
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(run), &updated))
	assert.Equal(t, criteriav1.PhaseRunning, updated.Status.Phase)
	assert.Nil(t, updated.Status.LastStepProgress)
	_, ok := conditionFor(updated.Status, controller.ConditionStallWatchdog)
	assert.False(t, ok)
}

// A disabled castle source means no observation and hence an inert watchdog:
// without the event stream there is no step-progress signal.
func TestStallWatchdogInertWhenCastleDisabled(t *testing.T) {
	scheme := newScheme(t)
	run := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{Name: "kb-24-no-castle", Namespace: "default", UID: types.UID("uid-no-castle")},
		Spec:       criteriav1.CriteriaRunSpec{TicketID: "KB-24", RepoURL: "https://github.com/brokenbots/workflow-example.git"},
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
		Castle:      disabledCastle{},
		Defaults:    jobbuilder.Defaults{DataPVC: "criteria-data"},
		Queue:       controller.NewRunQueue(),
		StallWindow: 30 * time.Minute,
	}

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, res, "castle disabled and the run running: no requeue is expected pre-KB-24 behavior aside from the disabled watchdog")

	var updated criteriav1.CriteriaRun
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(run), &updated))
	assert.Equal(t, criteriav1.PhaseRunning, updated.Status.Phase)
	assert.Nil(t, updated.Status.LastStepProgress)
	_, ok := conditionFor(updated.Status, controller.ConditionStallWatchdog)
	assert.False(t, ok)
}

// disabledCastle is a castle.Interface stub with the source disabled.
type disabledCastle struct{}

func (disabledCastle) Observe(context.Context, string, string) (*castle.Observation, error) {
	return &castle.Observation{}, nil
}
func (disabledCastle) Disabled() bool { return true }

// ptrTime is a tiny helper for status fixtures.
func ptrTime(t time.Time) *metav1.Time {
	mt := metav1.NewTime(t)
	return &mt
}