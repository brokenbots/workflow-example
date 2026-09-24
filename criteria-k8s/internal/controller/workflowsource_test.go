// KB-6: a url-type CriteriaRun without spec.workflowSource must fail closed
// at admission with an explicit reason — never silently resolve the baked
// image-mode fallback (observed: the runner and repo-clone both pulled the
// baked Linear image and ImagePullBackOff'd on a cluster without it). The
// genuinely legacy (non-url) image-mode path, and its KB-3 volume gating,
// stay intact.
package controller_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/controller"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/jobbuilder"
)

// kb6URLRun builds a CriteriaRun stamped with the named workflow object and
// workflow source (nil keeps image mode).
func kb6URLRun(name, ticket string, wf *criteriav1.RunWorkflow, source *criteriav1.RunWorkflowSource) *criteriav1.CriteriaRun {
	return &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			UID:       types.UID(name + "-uid"),
		},
		Spec: criteriav1.CriteriaRunSpec{
			TicketID:        ticket,
			RepoURL:         "https://github.com/brokenbots/workflow-example.git",
			MaxAgentVisits:  2,
			ProviderBaseURL: "http://provider/v1",
			Workflow:        wf,
			WorkflowSource:  source,
		},
	}
}

// kb6Reconciler wires a reconciler with a fake recorder, fake castle and a
// fresh admission queue.
func kb6Reconciler(cl client.Client, scheme *runtime.Scheme, recorder record.EventRecorder) *controller.CriteriaRunReconciler {
	return &controller.CriteriaRunReconciler{
		Client:   cl,
		Scheme:   scheme,
		Recorder: recorder,
		Castle:   &fakeCastle{},
		Defaults: jobbuilder.Defaults{DataPVC: "criteria-data"},
		Queue:    controller.NewRunQueue(),
	}
}

// drainEvent pops one recorded event, failing when none is buffered.
func drainEvent(t *testing.T, recorder *record.FakeRecorder) string {
	t.Helper()
	select {
	case ev := <-recorder.Events:
		return ev
	default:
		t.Fatal("expected a recorded event")
		return ""
	}
}

// TestReconcileRefusesURLWorkflowRunWithoutWorkflowSource covers the KB-6
// fail-closed contract: a url-type workflow run with no spec.workflowSource
// is refused at admission — marked Failed with an explicit condition and a
// warning event, no child Jobs created, no image pinned, and the repo's
// admission slot left free for the next run.
func TestReconcileRefusesURLWorkflowRunWithoutWorkflowSource(t *testing.T) {
	scheme := newScheme(t)
	run := kb6URLRun("kb6-url-no-source", "KB6-URL-NO-SOURCE", &criteriav1.RunWorkflow{
		Name:      "criteria-intake",
		Type:      "url",
		Namespace: "default",
		URL:       "git::https://github.com/brokenbots/workflow-example.git//linear_intake_v1",
	}, nil)

	recorder := record.NewFakeRecorder(10)
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&criteriav1.CriteriaRun{}).
		WithObjects(run).
		Build()
	r := kb6Reconciler(cl, scheme, recorder)

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, res, "a refused run is terminal; no requeue churn")

	var refused criteriav1.CriteriaRun
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(run), &refused))
	assert.Equal(t, criteriav1.PhaseFailed, refused.Status.Phase, "the run fails closed instead of falling to the baked image")
	assert.Empty(t, refused.Status.JobName, "no child Job may be created for a refused run")
	assert.Empty(t, refused.Status.BaseImage, "no runner image may be pinned for a refused run")

	cond, ok := conditionFor(refused.Status, controller.ConditionWorkflowSourceMissing)
	require.True(t, ok, "the refusal stamps an explicit WorkflowSourceMissing condition")
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, controller.ConditionWorkflowSourceMissing, cond.Reason)
	assert.Equal(t, refused.Generation, cond.ObservedGeneration)
	assert.Contains(t, cond.Message, "workflowSource", "the condition names the missing field")
	assert.Contains(t, cond.Message, "criteria-intake", "the condition names the workflow")

	ev := drainEvent(t, recorder)
	assert.Contains(t, ev, "Warning")
	assert.Contains(t, ev, "WorkflowSourceMissing")
	assert.Contains(t, ev, "workflowSource")

	assert.Empty(t, jobNamesForRun(t, cl, run.Name), "no child Jobs may exist for a refused run")

	// The refused run never held the repo's admission slot: a legacy
	// (non-url) run for the same repo admits immediately behind it.
	legacy := kb6URLRun("kb6-legacy-behind", "KB6-LEGACY-BEHIND", nil, nil)
	require.NoError(t, cl.Create(context.Background(), legacy))
	_, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(legacy)})
	require.NoError(t, err)
	assert.NotEmpty(t, jobNamesForRun(t, cl, legacy.Name),
		"the refused run must not starve the repo queue: the next run admits")
}

// TestReconcileRefusalCleansUpActuatedRunChildJobs covers the upgrade edge:
// a run already actuated by an operator predating the KB-6 gate carries
// child Jobs pinned to the silently-resolved baked image. The gate's first
// pass deletes those Jobs and fails the run instead of letting them replay
// the wrong image through backoff.
func TestReconcileRefusalCleansUpActuatedRunChildJobs(t *testing.T) {
	scheme := newScheme(t)
	run := kb6URLRun("kb6-actuated-no-source", "KB6-ACTUATED-NO-SOURCE", &criteriav1.RunWorkflow{
		Name:      "criteria-intake",
		Type:      "url",
		Namespace: "default",
		URL:       "git::https://github.com/brokenbots/workflow-example.git//linear_intake_v1",
	}, nil)

	recorder := record.NewFakeRecorder(10)
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&criteriav1.CriteriaRun{}).
		WithObjects(run).
		Build()
	r := kb6Reconciler(cl, scheme, recorder)

	// Pre-gate operator state: runner plus adapter Jobs pinned with the
	// baked workflow image the resolver silently fell back to.
	ctx := context.Background()
	for _, desired := range jobbuilder.BuildAll(run, r.Defaults) {
		require.NoError(t, cl.Create(ctx, desired))
	}
	pinned, ok := runnerJobImageFor(t, cl, run)
	require.True(t, ok)
	assert.Equal(t, jobbuilder.DefaultWorkflowImage, pinned.Spec.Template.Spec.Containers[0].Image,
		"precondition: the actuated run replayed the baked Linear image")
	update := run.DeepCopy()
	update.Status.JobName = jobbuilder.RunnerJobName(run)
	update.Status.BaseImage = jobbuilder.DefaultWorkflowImage
	require.NoError(t, cl.Status().Update(ctx, update))

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	require.NoError(t, err)

	assert.Empty(t, jobNamesForRun(t, cl, run.Name),
		"the Jobs pinning the silently-resolved baked image are deleted, not left in backoff")
	var refused criteriav1.CriteriaRun
	require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(run), &refused))
	assert.Equal(t, criteriav1.PhaseFailed, refused.Status.Phase)
	_, ok = conditionFor(refused.Status, controller.ConditionWorkflowSourceMissing)
	assert.True(t, ok, "the refusal stamps an explicit WorkflowSourceMissing condition")
	assert.Contains(t, drainEvent(t, recorder), "WorkflowSourceMissing")
}

// TestReconcileAdmitsURLWorkflowRunWithWorkflowSource is the positive
// control for KB-6: a url-type run that declares spec.workflowSource admits
// normally and runs source mode on the criteria-base image — never the
// baked Linear image.
func TestReconcileAdmitsURLWorkflowRunWithWorkflowSource(t *testing.T) {
	scheme := newScheme(t)
	run := kb6URLRun("kb6-url-with-source", "KB6-URL-WITH-SOURCE", &criteriav1.RunWorkflow{
		Name:      "criteria-intake",
		Type:      "url",
		Namespace: "default",
		URL:       "git::https://github.com/brokenbots/workflow-example.git//linear_intake_v1",
	}, &criteriav1.RunWorkflowSource{
		Type: "url",
		URL:  "git::https://github.com/brokenbots/workflow-example.git//linear_intake_v1",
	})

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&criteriav1.CriteriaRun{}).
		WithObjects(run).
		Build()
	r := kb6Reconciler(cl, scheme, record.NewFakeRecorder(10))

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	require.NoError(t, err)

	job, ok := runnerJobImageFor(t, cl, run)
	require.True(t, ok, "a declared-source url run admits and creates its runner Job")
	assert.Equal(t, jobbuilder.CriteriaBaseImageDefault, job.Spec.Template.Spec.Containers[0].Image,
		"source mode runs on the criteria-base image")
	assert.NotEqual(t, jobbuilder.DefaultWorkflowImage, job.Spec.Template.Spec.Containers[0].Image,
		"the baked Linear image must never serve a source-mode run")
	for _, c := range append(job.Spec.Template.Spec.InitContainers, job.Spec.Template.Spec.Containers...) {
		assert.NotEqual(t, jobbuilder.DefaultWorkflowImage, c.Image,
			"container %q: the baked Linear image must not appear anywhere in the pod", c.Name)
	}

	var admitted criteriav1.CriteriaRun
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(run), &admitted))
	assert.Equal(t, job.Name, admitted.Status.JobName)
	assert.NotEqual(t, criteriav1.PhaseFailed, admitted.Status.Phase)
}

// TestReconcileLegacyNonURLRunsKeepImageMode guards the other half of the
// KB-6 scope note: the genuinely legacy (non-url) path keeps its image-mode
// fallback. A workflow-less run and a type=image run both admit and resolve
// the image-mode chain, untouched by the url gate.
func TestReconcileLegacyNonURLRunsKeepImageMode(t *testing.T) {
	scheme := newScheme(t)
	workflowless := kb6URLRun("kb6-legacy-workflowless", "KB6-LEGACY-WORKFLOWLESS", nil, nil)
	imageTyped := kb6URLRun("kb6-legacy-image-typed", "KB6-LEGACY-IMAGE-TYPED", &criteriav1.RunWorkflow{
		Name:      "baked-intake",
		Type:      "image",
		Namespace: "default",
	}, nil)
	imageTyped.Spec.Image = "localhost:5000/baked-intake:v9"
	// Distinct repos: the dev-class admission queue serializes per repoURL,
	// and the first admitted run would otherwise keep the second queued.
	workflowless.Spec.RepoURL = "https://github.com/brokenbots/workflow-example.git"
	imageTyped.Spec.RepoURL = "https://github.com/brokenbots/other-example.git"

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&criteriav1.CriteriaRun{}).
		WithObjects(workflowless, imageTyped).
		Build()
	r := kb6Reconciler(cl, scheme, record.NewFakeRecorder(10))
	// The operator's baked-workflow default drives the workflow-less run;
	// the image-typed run carries its own spec pin.
	r.Defaults.Image = jobbuilder.DefaultWorkflowImage

	for _, run := range []*criteriav1.CriteriaRun{workflowless, imageTyped} {
		_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
		require.NoError(t, err)
		job, ok := runnerJobImageFor(t, cl, run)
		require.True(t, ok, "run %s: the legacy (non-url) path still admits", run.Name)
		want := jobbuilder.DefaultWorkflowImage
		if run.Spec.Image != "" {
			want = run.Spec.Image
		}
		assert.Equal(t, want, job.Spec.Template.Spec.Containers[0].Image,
			"run %s: the image-mode chain resolves untouched", run.Name)
		var admitted criteriav1.CriteriaRun
		require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(run), &admitted))
		assert.NotEqual(t, criteriav1.PhaseFailed, admitted.Status.Phase)
		_, ok = conditionFor(admitted.Status, controller.ConditionWorkflowSourceMissing)
		assert.False(t, ok, "run %s: the url gate must not stamp non-url runs", run.Name)
	}
}

// TestReconcileRefusalEmitsEventOnceThenStaysTerminal asserts the refusal is
// stamped exactly once: later reconcile passes of the terminal run re-enter
// the terminal path and must not re-fire the event or churn the condition.
func TestReconcileRefusalEmitsEventOnceThenStaysTerminal(t *testing.T) {
	scheme := newScheme(t)
	run := kb6URLRun("kb6-refusal-once", "KB6-REFUSAL-ONCE", &criteriav1.RunWorkflow{
		Name:      "criteria-intake",
		Type:      "url",
		Namespace: "default",
		URL:       "git::https://github.com/brokenbots/workflow-example.git//linear_intake_v1",
	}, nil)

	recorder := record.NewFakeRecorder(10)
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&criteriav1.CriteriaRun{}).
		WithObjects(run).
		Build()
	r := kb6Reconciler(cl, scheme, recorder)
	ctx := context.Background()

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	require.NoError(t, err)
	drainEvent(t, recorder)

	var refused criteriav1.CriteriaRun
	require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(run), &refused))
	firstCond, ok := conditionFor(refused.Status, controller.ConditionWorkflowSourceMissing)
	require.True(t, ok)
	firstTransition := firstCond.LastTransitionTime

	_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	require.NoError(t, err)
	select {
	case ev := <-recorder.Events:
		t.Fatalf("terminal passes must not re-fire the refusal event, got %q", ev)
	default:
	}

	require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(run), &refused))
	assert.Equal(t, criteriav1.PhaseFailed, refused.Status.Phase)
	secondCond, ok := conditionFor(refused.Status, controller.ConditionWorkflowSourceMissing)
	require.True(t, ok)
	assert.Equal(t, firstTransition.Time, secondCond.LastTransitionTime.Time,
		"the condition timestamp must not churn on later passes")
}

// TestReconcileRefusesPerScopeURLRunWithoutWorkflowSource covers the
// per-scope flavor of the same run shape: the gate fires before any
// per-scope adapter pod could be provisioned, and the run fails closed.
func TestReconcileRefusesPerScopeURLRunWithoutWorkflowSource(t *testing.T) {
	scheme := newScheme(t)
	run := kb6URLRun("kb6-perscope-no-source", "KB6-PERSCOPE-NO-SOURCE", &criteriav1.RunWorkflow{
		Name:      "criteria-intake",
		Type:      "url",
		Namespace: "default",
		URL:       "git::https://github.com/brokenbots/workflow-example.git//linear_intake_v1",
	}, nil)
	run.Spec.PerScopeSessions = true

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&criteriav1.CriteriaRun{}).
		WithObjects(run).
		Build()
	r := kb6Reconciler(cl, scheme, record.NewFakeRecorder(10))

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	require.NoError(t, err)

	var refused criteriav1.CriteriaRun
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(run), &refused))
	assert.Equal(t, criteriav1.PhaseFailed, refused.Status.Phase)
	assert.Empty(t, refused.Status.JobName)
	assert.Empty(t, jobNamesForRun(t, cl, run.Name), "no runner Job may exist for a refused run")

	var pods corev1.PodList
	require.NoError(t, cl.List(context.Background(), &pods, client.InNamespace("default")))
	assert.Empty(t, pods.Items, "no adapter pods may be provisioned for a refused run")
}