// KB-103: a CriteriaRun pinned to a per-repo config ConfigMap executes on
// that ConfigMap's values rendered ONCE at admission, with the render
// recorded into status.configRender (config provenance). Later passes
// re-verify the pin only; a ConfigMap replaced under an admitted run fails
// it fast with a ConfigRenderDrift condition instead of silently switching
// its config — the workflowSource ref-pin fail-closed symmetry (CRI-226).
package controller_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/controller"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/jobbuilder"
)

const (
	repoCM        = "workflow-example"
	repoCMStamped = "77"
	repoCMPatched = "99"
)

// newRepoCM builds the per-repo config ConfigMap as a routes configLibrary
// entry renders it: camelCase command keys in the execution namespace.
func newRepoCM(resourceVersion string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:            repoCM,
			Namespace:       "default",
			ResourceVersion: resourceVersion,
		},
		Data: map[string]string{
			"testCmd":   "make ci-test",
			"ciGateCmd": "make ci && make vuln-scan",
		},
	}
}

// newConfigRun builds a run like the KB-103 watchers stamp them: fallback
// command fields plus the config pin.
func newConfigRun(name string, pin *criteriav1.CriteriaRunConfigRef) *criteriav1.CriteriaRun {
	run := newProbeRun(name)
	run.Spec.BuildCmd = "flag-build"
	run.Spec.TestCmd = "flag-test"
	run.Spec.CIGateCmd = "flag-gate"
	run.Spec.ConfigRef = pin
	return run
}

// runnerCommandEnv reads the run's runner-job env for BUILD_CMD /
// TEST_CMD / CI_GATE_CMD.
func runnerCommandEnv(t *testing.T, cl client.Client, run *criteriav1.CriteriaRun) map[string]string {
	t.Helper()
	var job batchv1.Job
	err := cl.Get(context.Background(), types.NamespacedName{
		Name: jobbuilder.RunnerJobName(run), Namespace: run.Namespace,
	}, &job)
	require.NoError(t, err, "runner Job must exist")
	for i := range job.Spec.Template.Spec.Containers {
		c := &job.Spec.Template.Spec.Containers[i]
		if c.Name != jobbuilder.RunnerContainerName {
			continue
		}
		env := make(map[string]string)
		for _, e := range c.Env {
			env[e.Name] = e.Value
		}
		return env
	}
	t.Fatal("runner job has no workflow-runner container")
	return nil
}

func configRunClient(t *testing.T, scheme *runtime.Scheme, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&criteriav1.CriteriaRun{}).
		WithObjects(objs...).
		Build()
}

// The admission pass renders the pinned ConfigMap: its data keys outrank
// the spec fields per key, the render is stamped into status with the CM's
// live resourceVersion, and the runner Job's env carries the rendered
// commands.
func TestReconcileStampsConfigRenderAtAdmission(t *testing.T) {
	scheme := newScheme(t)
	run := newConfigRun("kb103-render", &criteriav1.CriteriaRunConfigRef{Name: repoCM, ResourceVersion: repoCMStamped})
	// A key the CM does not carry keeps the spec field value.
	run.Spec.BuildCmd = "spec-kept-build"

	cl := configRunClient(t, scheme, run, newRepoCM(repoCMStamped))
	r := newProbeReconciler(cl, scheme, nil)

	reconcileOnce(t, r, run)

	var after criteriav1.CriteriaRun
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(run), &after))
	require.NotNil(t, after.Status.ConfigRender, "the render must be recorded for provenance")
	render := *after.Status.ConfigRender
	assert.Equal(t, repoCM, render.ConfigMap)
	assert.Equal(t, repoCMStamped, render.ResourceVersion, "the render records the CM rv observed")
	assert.Equal(t, "make ci-test", render.TestCmd, "CM data outranks the spec field")
	assert.Equal(t, "make ci && make vuln-scan", render.CIGateCmd)
	assert.Equal(t, "spec-kept-build", render.BuildCmd, "a key the CM omits keeps the spec value")

	env := runnerCommandEnv(t, cl, run)
	assert.Equal(t, "make ci-test", env["TEST_CMD"], "the runner env carries the CM value")
	assert.Equal(t, "make ci && make vuln-scan", env["CI_GATE_CMD"])
	assert.Equal(t, "spec-kept-build", env["BUILD_CMD"])
}

// A run without a config pin behaves exactly as before KB-103: the render
// (when anything is configured) records the spec fields alone, and the
// runner env threads them as before.
func TestReconcileConfigRenderWithoutPin(t *testing.T) {
	t.Run("spec commands only", func(t *testing.T) {
		scheme := newScheme(t)
		run := newConfigRun("kb103-nopin", nil)
		cl := configRunClient(t, scheme, run)
		r := newProbeReconciler(cl, scheme, nil)

		reconcileOnce(t, r, run)

		var after criteriav1.CriteriaRun
		require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(run), &after))
		require.NotNil(t, after.Status.ConfigRender)
		render := *after.Status.ConfigRender
		assert.Empty(t, render.ConfigMap)
		assert.Equal(t, "flag-build", render.BuildCmd)
		assert.Equal(t, "flag-test", render.TestCmd)
		assert.Equal(t, "flag-gate", render.CIGateCmd)
	})

	t.Run("fully unconfigured run stays render-free", func(t *testing.T) {
		scheme := newScheme(t)
		run := newProbeRun("kb103-unconfigured")
		cl := configRunClient(t, scheme, run)
		r := newProbeReconciler(cl, scheme, nil)

		reconcileOnce(t, r, run)

		var after criteriav1.CriteriaRun
		require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(run), &after))
		assert.Nil(t, after.Status.ConfigRender, "no pin and no commands: nil keeps the pre-KB-103 shape")
	})
}

// A name-only pin whose ConfigMap does not exist at admission renders the
// spec fields alone instead of failing the run (a run may be admitted
// before its repo's config CM is first created).
func TestReconcileNameonlyPinAbsentConfigMapRendersSpecFields(t *testing.T) {
	scheme := newScheme(t)
	run := newConfigRun("kb103-absent", &criteriav1.CriteriaRunConfigRef{Name: repoCM})
	cl := configRunClient(t, scheme, run)
	r := newProbeReconciler(cl, scheme, nil)

	reconcileOnce(t, r, run)

	var after criteriav1.CriteriaRun
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(run), &after))
	require.NotNil(t, after.Status.ConfigRender)
	render := *after.Status.ConfigRender
	assert.Empty(t, render.ConfigMap, "no ConfigMap behind the render")
	assert.Equal(t, "flag-build", render.BuildCmd)
	env := runnerCommandEnv(t, cl, run)
	assert.Equal(t, "flag-build", env["BUILD_CMD"])
	_, ok := conditionFor(after.Status, controller.ConditionConfigRenderDrift)
	assert.False(t, ok, "an absent CM with no declared rv is not drift")
}

// A pinned resourceVersion that no longer matches the ConfigMap live read
// fails the run fast BEFORE any child Job is created or the ConfigMap is
// adopted (fail closed on drift).
func TestReconcileFailsClosedWhenPinnedConfigMapDrifts(t *testing.T) {
	scheme := newScheme(t)
	run := newConfigRun("kb103-drift-admission", &criteriav1.CriteriaRunConfigRef{Name: repoCM, ResourceVersion: repoCMStamped})
	cl := configRunClient(t, scheme, run, newRepoCM(repoCMPatched))
	r := newProbeReconciler(cl, scheme, nil)

	reconcileOnce(t, r, run)

	var after criteriav1.CriteriaRun
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(run), &after))
	assert.Equal(t, criteriav1.PhaseFailed, after.Status.Phase)
	cond, ok := conditionFor(after.Status, controller.ConditionConfigRenderDrift)
	require.True(t, ok, "the ConfigRenderDrift condition must be stamped")
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Contains(t, cond.Message, "was pinned at resourceVersion \"77\"")
	assert.Contains(t, cond.Message, "now reads \"99\"")
	assert.Nil(t, after.Status.ConfigRender, "nothing was rendered: no adopt")
	_, found := runnerJobImageFor(t, cl, run)
	assert.False(t, found, "no child Job may be admitted on a drifted pin")
}

// The stamp-at-creation guarantee: a run admitted under config X keeps
// config X. Patching the ConfigMap after admission fails the run fast
// with ConfigRenderDrift instead of silently switching its config; the
// recorded render survives.
func TestReconcileConfigPatchUnderRunningRunFailsFast(t *testing.T) {
	scheme := newScheme(t)
	run := newConfigRun("kb103-inflight", &criteriav1.CriteriaRunConfigRef{Name: repoCM, ResourceVersion: repoCMStamped})
	cl := configRunClient(t, scheme, run, newRepoCM(repoCMStamped))
	r := newProbeReconciler(cl, scheme, nil)

	reconcileOnce(t, r, run)

	var after criteriav1.CriteriaRun
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(run), &after))
	require.NotNil(t, after.Status.ConfigRender)
	recorded := *after.Status.ConfigRender
	require.Equal(t, repoCMStamped, recorded.ResourceVersion)

	// The routed config ConfigMap is patched in place after admission;
	// the fake client bumps the resourceVersion on update.
	var cm corev1.ConfigMap
	require.NoError(t, cl.Get(context.Background(), types.NamespacedName{Name: repoCM, Namespace: "default"}, &cm))
	cm.Data["ciGateCmd"] = "make new-gate"
	require.NoError(t, cl.Update(context.Background(), &cm))
	var patched corev1.ConfigMap
	require.NoError(t, cl.Get(context.Background(), types.NamespacedName{Name: repoCM, Namespace: "default"}, &patched))
	require.NotEqual(t, repoCMStamped, patched.ResourceVersion, "the patch must move the rv for the drift to be observable")

	reconcileOnce(t, r, run)

	var drifted criteriav1.CriteriaRun
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(run), &drifted))
	require.NotNil(t, drifted.Status.ConfigRender, "the render is never re-derived")
	assert.Equal(t, recorded, *drifted.Status.ConfigRender, "the run keeps executing config X")
	assert.Equal(t, "make ci && make vuln-scan", drifted.Status.ConfigRender.CIGateCmd)
	assert.Equal(t, criteriav1.PhaseFailed, drifted.Status.Phase)
	cond, ok := conditionFor(drifted.Status, controller.ConditionConfigRenderDrift)
	require.True(t, ok, "the drift fires instead of a silent switch")
	assert.Contains(t, cond.Message, "was pinned at resourceVersion \"77\"")
	assert.Contains(t, cond.Message, "now reads \""+patched.ResourceVersion+"\"")
	_, found := runnerJobImageFor(t, cl, run)
	assert.False(t, found, "child Jobs are torn down so nothing keeps running on stale config")
}

// A pinned CM deleted after admission also fails the run fast: the pin
// cannot be rendered.
func TestReconcilePinnedConfigMapDeletedFailsFast(t *testing.T) {
	scheme := newScheme(t)
	run := newConfigRun("kb103-deleted", &criteriav1.CriteriaRunConfigRef{Name: repoCM, ResourceVersion: repoCMStamped})
	// The CM never existed: the pin declares an rv the cluster cannot honor.
	cl := configRunClient(t, scheme, run)
	r := newProbeReconciler(cl, scheme, nil)

	reconcileOnce(t, r, run)

	var after criteriav1.CriteriaRun
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(run), &after))
	assert.Equal(t, criteriav1.PhaseFailed, after.Status.Phase)
	cond, ok := conditionFor(after.Status, controller.ConditionConfigRenderDrift)
	require.True(t, ok, "deleting the pinned CM under a run is drift, not a silent spec-fields fallback")
	assert.Contains(t, cond.Message, "no longer exists")
}

// A CM read failure that is neither drift nor not-found must NOT fail the
// run: the reconcile error propagates so the controller backoff retries.
func TestReconcileConfigMapReadErrorDefersInsteadOfFailing(t *testing.T) {
	scheme := newScheme(t)
	run := newConfigRun("kb103-readerr", &criteriav1.CriteriaRunConfigRef{Name: repoCM, ResourceVersion: repoCMStamped})

	failingGet := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&criteriav1.CriteriaRun{}).
		WithObjects(run).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(_ context.Context, _ client.WithWatch, key client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
				if key.Name == repoCM {
					return errors.New("simulated ConfigMap read flake (non-404)")
				}
				return nil
			},
		}).
		Build()
	r := newProbeReconciler(failingGet, scheme, nil)

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	require.Error(t, err, "the CM read error propagates for backoff retry (the run is not failed)")
	var after criteriav1.CriteriaRun
	require.NoError(t, failingGet.Get(context.Background(), client.ObjectKeyFromObject(run), &after))
	assert.NotEqual(t, criteriav1.PhaseFailed, after.Status.Phase, "an infrastructure read failure must not fail the run")
}