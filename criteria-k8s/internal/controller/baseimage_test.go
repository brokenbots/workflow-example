// CRI-264: a CriteriaRun admitted while the operator Deployment is
// mid-rollout must not run on the stale CRITERIA_BASE_IMAGE /
// DEFAULT_CRITERIA_IMAGE value the reconciling pod carries. The reconciler
// resolves the runner image from the live operator Deployment env, stamps
// it on status.baseImage at admission, and fails the run fast with a
// BaseImageMismatch condition when the pinned image disagrees — instead of
// letting the runner Job replay the stale image through backoff until
// BackoffLimitExceeded.
package controller_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/controller"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/jobbuilder"
)

// stubProbe is a scripted OperatorEnvProbe: it answers from a fixed env map
// (or a fixed error) and counts reads so tests can assert the probe is
// actually consulted.
type stubProbe struct {
	env   map[string]string
	err   error
	calls int
}

func (p *stubProbe) OperatorEnv(ctx context.Context, keys ...string) (map[string]string, error) {
	p.calls++
	if p.err != nil {
		return nil, p.err
	}
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		if v, ok := p.env[k]; ok {
			out[k] = v
		}
	}
	return out, nil
}

func newProbeRun(name string) *criteriav1.CriteriaRun {
	return &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			UID:       types.UID(name + "-uid"),
		},
		Spec: criteriav1.CriteriaRunSpec{
			TicketID:       "CRI-264",
			RepoURL:        "https://github.com/brokenbots/workflow-example.git",
			MaxAgentVisits: 2,
			WorkflowSource: &criteriav1.RunWorkflowSource{
				Type: "url",
				URL:  "git::https://github.com/brokenbots/workflow-example.git//linear_intake_v1",
			},
		},
	}
}

const (
	baseImageOldTag = "localhost:5000/criteria-base:v25"
	baseImageNewTag = "localhost:5000/criteria-base:v27"
)

func newProbeReconciler(cl client.Client, scheme *runtime.Scheme, probe controller.OperatorEnvProbe) *controller.CriteriaRunReconciler {
	return &controller.CriteriaRunReconciler{
		Client:   cl,
		Scheme:   scheme,
		Castle:   &fakeCastle{},
		EnvProbe: probe,
		Defaults: jobbuilder.Defaults{DataPVC: "criteria-data"},
		Queue:    controller.NewRunQueue(),
	}
}

func reconcileOnce(t *testing.T, r *controller.CriteriaRunReconciler, run *criteriav1.CriteriaRun) {
	t.Helper()
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	require.NoError(t, err)
}

func runnerJobImageFor(t *testing.T, cl client.Client, run *criteriav1.CriteriaRun) (batchv1.Job, bool) {
	t.Helper()
	var job batchv1.Job
	err := cl.Get(context.Background(), types.NamespacedName{Name: jobbuilder.RunnerJobName(run), Namespace: run.Namespace}, &job)
	return job, err == nil
}

func conditionFor(status criteriav1.CriteriaRunStatus, condType string) (metav1.Condition, bool) {
	for _, c := range status.Conditions {
		if c.Type == condType {
			return c, true
		}
	}
	return metav1.Condition{}, false
}

// A run admitted with the live base image gets the stamp recorded at
// admission, and an unchanged env on the next pass never disturbs it.
func TestReconcileStampsBaseImageAtAdmission(t *testing.T) {
	scheme := newScheme(t)
	run := newProbeRun("cri-264-stamp")

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(run).
		WithObjects(run).
		Build()
	probe := &stubProbe{env: map[string]string{jobbuilder.EnvCriteriaBaseImage: baseImageOldTag}}
	r := newProbeReconciler(cl, scheme, probe)

	reconcileOnce(t, r, run)

	job, found := runnerJobImageFor(t, cl, run)
	require.True(t, found)
	assert.Equal(t, baseImageOldTag, jobbuilder.RunnerJobImage(&job))

	var updated criteriav1.CriteriaRun
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(run), &updated))
	assert.Equal(t, baseImageOldTag, updated.Status.BaseImage)

	// The env is unchanged: the second pass must not fail the run.
	reconcileOnce(t, r, run)
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(run), &updated))
	assert.NotEqual(t, criteriav1.PhaseFailed, updated.Status.Phase)
	_, ok := conditionFor(updated.Status, controller.ConditionBaseImageMismatch)
	assert.False(t, ok, "unchanged env must not stamp a BaseImageMismatch condition")
}

// Acceptance 1 (admission side): a run admitted while the operator
// Deployment's env already carries a new tag (the reconciling pod's own
// process env still holds the stale one) resolves the runner image from the
// live deployment env, so the Job pins the new image and the stamp records
// it.
func TestReconcileAdmitsWithLiveBaseImageMidRollout(t *testing.T) {
	scheme := newScheme(t)
	run := newProbeRun("cri-264-midrollout")

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(run).
		WithObjects(run).
		Build()
	// The reconciler's process-env defaults carry the OLD tag (a stale pod
	// mid-rollout); the deployment's declared env is already the new one.
	r := newProbeReconciler(cl, scheme, &stubProbe{env: map[string]string{
		jobbuilder.EnvCriteriaBaseImage: baseImageNewTag,
		jobbuilder.EnvDefaultImage:      "localhost:5000/linear-intake-remote:new",
	}})
	r.Defaults.CriteriaBaseImage = baseImageOldTag

	reconcileOnce(t, r, run)

	job, found := runnerJobImageFor(t, cl, run)
	require.True(t, found)
	assert.Equal(t, baseImageNewTag, jobbuilder.RunnerJobImage(&job),
		"a mid-rollout admission must pin the live deployment env value, not the stale process env")

	var updated criteriav1.CriteriaRun
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(run), &updated))
	assert.Equal(t, baseImageNewTag, updated.Status.BaseImage)
	assert.NotEqual(t, criteriav1.PhaseFailed, updated.Status.Phase)
}

// Acceptance 1 (fail-fast): a run admitted on the old tag fails fast with a
// BaseImageMismatch condition once the env moves on, the pinned child Jobs
// are deleted so nothing keeps replaying the stale image through backoff,
// and no replacement Job is created with the stale pin.
func TestReconcileFailsFastOnBaseImageMismatch(t *testing.T) {
	scheme := newScheme(t)
	run := newProbeRun("cri-264-mismatch")

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(run).
		WithObjects(run).
		Build()
	probe := &stubProbe{env: map[string]string{jobbuilder.EnvCriteriaBaseImage: baseImageOldTag}}
	r := newProbeReconciler(cl, scheme, probe)

	reconcileOnce(t, r, run)
	_, found := runnerJobImageFor(t, cl, run)
	require.True(t, found, "precondition: the run is admitted on the old tag")

	// The operator env moves to a new tag while the run is in flight.
	probe.env[jobbuilder.EnvCriteriaBaseImage] = baseImageNewTag
	reconcileOnce(t, r, run)

	_, found = runnerJobImageFor(t, cl, run)
	assert.False(t, found, "the stale-image runner Job must be deleted, not left replaying through backoff")
	for _, kind := range []string{"shell", "copilot"} {
		var adapter batchv1.Job
		err := cl.Get(context.Background(), types.NamespacedName{Name: jobbuilder.AdapterJobName(run, kind), Namespace: run.Namespace}, &adapter)
		assert.Error(t, err, "adapter job %s must be deleted on mismatch", kind)
	}

	var updated criteriav1.CriteriaRun
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(run), &updated))
	assert.Equal(t, criteriav1.PhaseFailed, updated.Status.Phase)
	assert.Equal(t, baseImageOldTag, updated.Status.BaseImage, "the admission-time stamp is kept for diagnosis")
	cond, ok := conditionFor(updated.Status, controller.ConditionBaseImageMismatch)
	require.True(t, ok, "the run must carry the BaseImageMismatch condition")
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, "BaseImageMismatch", cond.Reason)
	assert.Contains(t, cond.Message, baseImageOldTag)
	assert.Contains(t, cond.Message, baseImageNewTag)

	// The failed run is terminal: later passes must not recreate Jobs.
	reconcileOnce(t, r, run)
	_, found = runnerJobImageFor(t, cl, run)
	assert.False(t, found)
}

// Acceptance 2 (validation-run shape): once the env has settled on a new
// tag, a refired run is admitted on the new image — never on the old one —
// regardless of what the reconciling pod's process env carries.
func TestReconcileAdmitsRefiredRunOnNewTag(t *testing.T) {
	scheme := newScheme(t)
	run := newProbeRun("cri-264-refire")

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(run).
		WithObjects(run).
		Build()
	r := newProbeReconciler(cl, scheme, &stubProbe{env: map[string]string{
		jobbuilder.EnvCriteriaBaseImage: baseImageNewTag,
	}})
	r.Defaults.CriteriaBaseImage = baseImageOldTag

	reconcileOnce(t, r, run)

	job, found := runnerJobImageFor(t, cl, run)
	require.True(t, found)
	assert.Equal(t, baseImageNewTag, jobbuilder.RunnerJobImage(&job),
		"the next refire after the env change must run the current image, never the old one")
	var updated criteriav1.CriteriaRun
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(run), &updated))
	assert.Equal(t, baseImageNewTag, updated.Status.BaseImage)
}

// A run whose spec pins its own image is immune to env changes: the
// resolution is deterministic in the spec pin on both sides, so the run is
// never failed and keeps its Job.
func TestReconcileSpecPinnedImageIgnoresBaseImageEnv(t *testing.T) {
	scheme := newScheme(t)
	run := newProbeRun("cri-264-pinned")
	run.Spec.Image = "localhost:5000/linear-intake-remote:dev"
	run.Spec.WorkflowSource = nil

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(run).
		WithObjects(run).
		Build()
	probe := &stubProbe{env: map[string]string{
		jobbuilder.EnvCriteriaBaseImage: baseImageOldTag,
		jobbuilder.EnvDefaultImage:      baseImageNewTag,
	}}
	r := newProbeReconciler(cl, scheme, probe)

	reconcileOnce(t, r, run)
	_, found := runnerJobImageFor(t, cl, run)
	require.True(t, found)

	// The image-mode env moves; the spec pin wins on both sides.
	probe.env[jobbuilder.EnvDefaultImage] = "localhost:5000/linear-intake-remote:newer"
	reconcileOnce(t, r, run)

	var updated criteriav1.CriteriaRun
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(run), &updated))
	assert.NotEqual(t, criteriav1.PhaseFailed, updated.Status.Phase)
	_, ok := conditionFor(updated.Status, controller.ConditionBaseImageMismatch)
	assert.False(t, ok)
	_, found = runnerJobImageFor(t, cl, run)
	assert.True(t, found)
}

// A Job admitted by a pre-fix operator carries no stamp: the image pinned
// on its runner container is the ground truth. An env change fails it fast;
// an unchanged env leaves it alone and stamps the pinned image for future
// passes.
func TestReconcileStamplessLegacyJob(t *testing.T) {
	scheme := newScheme(t)
	run := newProbeRun("cri-264-legacy")
	run.Status.JobName = jobbuilder.RunnerJobName(run)

	legacyJob := jobbuilder.BuildRunnerJob(run, jobbuilder.Defaults{CriteriaBaseImage: baseImageOldTag, DataPVC: "criteria-data"})

	t.Run("env unchanged", func(t *testing.T) {
		cl := fake.NewClientBuilder().
			WithScheme(scheme).
			WithStatusSubresource(run).
			WithObjects(run.DeepCopy(), legacyJob.DeepCopy()).
			Build()
		probe := &stubProbe{env: map[string]string{jobbuilder.EnvCriteriaBaseImage: baseImageOldTag}}
		r := newProbeReconciler(cl, scheme, probe)

		reconcileOnce(t, r, run)

		var updated criteriav1.CriteriaRun
		require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(run), &updated))
		assert.Equal(t, baseImageOldTag, updated.Status.BaseImage,
			"a stamp-less legacy Job must be stamped from its pinned image")
		assert.NotEqual(t, criteriav1.PhaseFailed, updated.Status.Phase)
		_, ok := conditionFor(updated.Status, controller.ConditionBaseImageMismatch)
		assert.False(t, ok)
	})

	t.Run("env changed", func(t *testing.T) {
		cl := fake.NewClientBuilder().
			WithScheme(scheme).
			WithStatusSubresource(run).
			WithObjects(run.DeepCopy(), legacyJob.DeepCopy()).
			Build()
		r := newProbeReconciler(cl, scheme, &stubProbe{env: map[string]string{
			jobbuilder.EnvCriteriaBaseImage: baseImageNewTag,
		}})

		reconcileOnce(t, r, run)

		_, found := runnerJobImageFor(t, cl, run)
		assert.False(t, found)
		var updated criteriav1.CriteriaRun
		require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(run), &updated))
		assert.Equal(t, criteriav1.PhaseFailed, updated.Status.Phase)
		cond, ok := conditionFor(updated.Status, controller.ConditionBaseImageMismatch)
		require.True(t, ok)
		assert.Contains(t, cond.Message, baseImageOldTag)
		assert.Contains(t, cond.Message, baseImageNewTag)
	})
}

// An unavailable probe must not fail runs off unverified data: the
// resolution degrades to the process env, Jobs are still built, and the
// mismatch check is deferred to a later pass.
func TestReconcileDefersMismatchWhenProbeFails(t *testing.T) {
	scheme := newScheme(t)
	run := newProbeRun("cri-264-probe-down")

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(run).
		WithObjects(run).
		Build()
	probe := &stubProbe{err: errors.New("operator deployment is not readable")}
	probe.env = map[string]string{jobbuilder.EnvCriteriaBaseImage: baseImageNewTag}
	r := newProbeReconciler(cl, scheme, probe)
	r.Defaults.CriteriaBaseImage = baseImageOldTag

	reconcileOnce(t, r, run)

	job, found := runnerJobImageFor(t, cl, run)
	require.True(t, found, "the run must still be admitted on the process-env resolution")
	assert.Equal(t, baseImageOldTag, jobbuilder.RunnerJobImage(&job))
	var updated criteriav1.CriteriaRun
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(run), &updated))
	assert.Equal(t, criteriav1.PhasePending, updated.Status.Phase)
	assert.Equal(t, baseImageOldTag, updated.Status.BaseImage)
	assert.NotEqual(t, criteriav1.PhaseFailed, updated.Status.Phase)
}

// The image-mode default image (DEFAULT_CRITERIA_IMAGE) is covered by the
// same mechanism: an env change on the baked-tree image fails in-flight
// runs the same way.
func TestReconcileFailsFastOnDefaultImageMismatch(t *testing.T) {
	scheme := newScheme(t)
	run := newProbeRun("cri-264-imgmode")
	run.Spec.WorkflowSource = nil
	run.Spec.Image = ""
	run.Spec.TicketID = "CRI-264"

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(run).
		WithObjects(run).
		Build()
	defaultImageOld := "localhost:5000/linear-intake-remote:dev"
	defaultImageNew := "localhost:5000/linear-intake-remote:new"
	probe := &stubProbe{env: map[string]string{jobbuilder.EnvDefaultImage: defaultImageOld}}
	r := newProbeReconciler(cl, scheme, probe)

	reconcileOnce(t, r, run)
	job, found := runnerJobImageFor(t, cl, run)
	require.True(t, found)
	assert.Equal(t, defaultImageOld, jobbuilder.RunnerJobImage(&job))

	probe.env[jobbuilder.EnvDefaultImage] = defaultImageNew
	reconcileOnce(t, r, run)

	_, found = runnerJobImageFor(t, cl, run)
	assert.False(t, found)
	var updated criteriav1.CriteriaRun
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(run), &updated))
	assert.Equal(t, criteriav1.PhaseFailed, updated.Status.Phase)
	cond, ok := conditionFor(updated.Status, controller.ConditionBaseImageMismatch)
	require.True(t, ok)
	assert.Equal(t, "BaseImageMismatch", cond.Reason)
	assert.Contains(t, cond.Message, defaultImageOld)
	assert.Contains(t, cond.Message, defaultImageNew)
}

// A terminal run is never failed by a later env change: the outcome is
// already recorded and no Job replays.
func TestReconcileTerminalRunIgnoresBaseImageEnv(t *testing.T) {
	scheme := newScheme(t)
	run := newProbeRun("cri-264-terminal")
	run.Status.Phase = criteriav1.PhaseSucceeded
	run.Status.BaseImage = baseImageOldTag
	run.Status.JobName = jobbuilder.RunnerJobName(run)
	// The completed Job still exists and pins the old image.
	doneJob := jobbuilder.BuildRunnerJob(run, jobbuilder.Defaults{CriteriaBaseImage: baseImageOldTag, DataPVC: "criteria-data"})
	doneJob.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: "True"}}

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(run).
		WithObjects(run, doneJob).
		Build()
	r := newProbeReconciler(cl, scheme, &stubProbe{env: map[string]string{
		jobbuilder.EnvCriteriaBaseImage: baseImageNewTag,
	}})

	reconcileOnce(t, r, run)

	var updated criteriav1.CriteriaRun
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(run), &updated))
	assert.Equal(t, criteriav1.PhaseSucceeded, updated.Status.Phase)
	_, found := runnerJobImageFor(t, cl, run)
	assert.True(t, found, "a completed run's Job must not be deleted")
	_, ok := conditionFor(updated.Status, controller.ConditionBaseImageMismatch)
	assert.False(t, ok)
}

// DeploymentEnvProbe reads the requested literal env values off the
// operator Deployment's containers, skipping valueFrom entries; a missing
// Deployment is an error the reconciler degrades on.
func TestDeploymentEnvProbe(t *testing.T) {
	scheme := newScheme(t)
	require.NoError(t, appsv1.AddToScheme(scheme))
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "criteria-k8s-operator", Namespace: "criteria-jobs"},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "operator",
							Env: []corev1.EnvVar{
								{Name: jobbuilder.EnvCriteriaBaseImage, Value: baseImageNewTag},
								{Name: "OTHER", Value: "x"},
								{Name: "FROM_FIELD", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}},
							},
						},
					},
				},
			},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy).Build()
	probe := &controller.DeploymentEnvProbe{Client: cl, Namespace: "criteria-jobs", Deployment: "criteria-k8s-operator"}

	env, err := probe.OperatorEnv(context.Background(), jobbuilder.EnvCriteriaBaseImage, jobbuilder.EnvDefaultImage)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{jobbuilder.EnvCriteriaBaseImage: baseImageNewTag}, env)

	_, err = (&controller.DeploymentEnvProbe{Client: cl, Namespace: "criteria-jobs", Deployment: "absent-operator"}).
		OperatorEnv(context.Background(), jobbuilder.EnvCriteriaBaseImage)
	assert.Error(t, err, "a missing operator Deployment must surface as a probe error")
}

// An admission pass on a run whose Jobs are gone and whose admission-time
// stamp disagrees with the current resolution fails fast instead of
// re-admitting on the old stamp.
func TestReconcileAdmissionPassFailsFastOnStaleStamp(t *testing.T) {
	scheme := newScheme(t)
	run := newProbeRun("cri-264-stalestamp")
	run.Status.BaseImage = baseImageOldTag

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(run).
		WithObjects(run).
		Build()
	r := newProbeReconciler(cl, scheme, &stubProbe{env: map[string]string{
		jobbuilder.EnvCriteriaBaseImage: baseImageNewTag,
	}})

	reconcileOnce(t, r, run)

	var updated criteriav1.CriteriaRun
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(run), &updated))
	assert.Equal(t, criteriav1.PhaseFailed, updated.Status.Phase)
	_, found := runnerJobImageFor(t, cl, run)
	assert.False(t, found, "no Job may be created for a stale-stamp run after the env moved on")
}

// Ensure the exported condition name used by failBaseImageMismatch matches
// the stamped condition type in a failed run (contract pin).
func TestBaseImageMismatchConditionContract(t *testing.T) {
	assert.Equal(t, "BaseImageMismatch", controller.ConditionBaseImageMismatch)
}
