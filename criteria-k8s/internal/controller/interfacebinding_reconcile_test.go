// KB-310, reconcile level: the interface admission gate refuses a run whose
// project binding fails the tree's machine-readable interface (fail closed,
// both sides named, no Jobs, no launch manifest), and an admitted run has
// its launch manifest stamped — the reproducibility contract.
package controller_test

import (
	"context"
	"slices"
	"testing"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/controller"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/jobbuilder"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// interfaceBindingFixture builds a source-mode CriteriaRun carrying a
// stamped workflow interface, its own fake client and reconciler.
func interfaceBindingFixture(t *testing.T, mutate func(spec *criteriav1.CriteriaRunSpec)) (*controller.CriteriaRunReconciler, *criteriav1.CriteriaRun, *record.FakeRecorder, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, criteriav1.AddToScheme(scheme))
	require.NoError(t, batchv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	run := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cri-310",
			Namespace: "default",
			UID:       types.UID("run-uid-310"),
		},
		Spec: criteriav1.CriteriaRunSpec{
			TicketID:        "KB-310",
			RepoURL:         "https://github.com/brokenbots/workflow-example.git",
			Image:           "localhost:5000/criteria:dev",
			MaxAgentVisits:  2,
			ProviderBaseURL: "http://provider/v1",
			WorkflowSource: &criteriav1.RunWorkflowSource{
				Type: "url",
				URL:  "git::https://github.com/brokenbots/workflow-example.git",
			},
			Workflow: &criteriav1.RunWorkflow{
				Name:      "criteria-develop",
				Type:      "url",
				Namespace: "default",
				Class:     "triage",
				URL:       "git::https://github.com/brokenbots/workflow-example.git//kanboard_develop_v1",
				Ref:       "0123456789abcdef0123456789abcdef01234567",
				AdapterImages: map[string]string{
					"shell":   "localhost:5000/criteria-adapter-shell:dev",
					"copilot": "localhost:5000/criteria-adapter-copilot:dev",
				},
				Volumes: []criteriav1.RunWorkflowVolume{
					{Name: "workspace", Kind: "pvc", MountPath: "/home/criteria/workspace", Claim: "criteria-data"},
					{Name: "cache", Kind: "tmp", MountPath: "/tmp/criteria-cache"},
				},
				Secrets: []criteriav1.RunWorkflowSecret{{
					Name:                "github-tokens",
					SecretProviderClass: "criteria-github",
					MountPath:           "/home/criteria/secrets",
					Env:                 map[string]string{"WORKFLOW_GITHUB_TOKEN": "github_token"},
				}},
				Env: map[string]string{"CRITERIA_RUN_MODE": "k8s"},
				Interface: &criteriav1.RunWorkflowInterface{
					APIVersion: "criteria.brokenbots.dev/v1",
					Kind:       "WorkflowInterface",
					Workflow:   "kanboard_develop_v1",
					Adapters:   []string{"shell", "copilot"},
					Mounts: []criteriav1.RunWorkflowInterfaceMount{
						{Name: "workspace", MountPath: "/home/criteria/workspace", Kind: "pvc"},
						{Name: "cache", MountPath: "/tmp/criteria-cache", Kind: "tmp"},
					},
					SecretEnv: []string{"WORKFLOW_GITHUB_TOKEN"},
					Env:       []string{"CRITERIA_RUN_MODE"},
				},
			},
		},
	}
	if mutate != nil {
		mutate(&run.Spec)
	}
	builder := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(run).WithObjects(run)
	cl := builder.Build()
	recorder := record.NewFakeRecorder(16)
	r := &controller.CriteriaRunReconciler{
		Client:   cl,
		Scheme:   scheme,
		Castle:   &fakeCastle{},
		Defaults: jobbuilder.Defaults{DataPVC: "criteria-data"},
		Queue:    controller.NewRunQueue(),
		Recorder: recorder,
	}
	return r, run, recorder, cl
}

// TestReconcileRefusesInterfaceBindingMismatch drives the reconcile-level
// refusal: a binding missing an interface-required adapter kind is failed
// before any Job is created, with the gap named on both sides.
func TestReconcileRefusesInterfaceBindingMismatch(t *testing.T) {
	r, run, recorder, cl := interfaceBindingFixture(t, func(spec *criteriav1.CriteriaRunSpec) {
		delete(spec.Workflow.AdapterImages, "copilot")
	})
	ctx := context.Background()

	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, res)

	var refused criteriav1.CriteriaRun
	require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(run), &refused))
	assert.Equal(t, criteriav1.PhaseFailed, refused.Status.Phase)
	cond := condByType(t, refused, criteriav1.ConditionInterfaceBindingMismatch)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	for _, want := range []string{`adapter kind "copilot"`, "kanboard_develop_v1", "criteria-develop", "KB-310"} {
		assert.Contains(t, cond.Message, want, "the refusal names the gap and both sides")
	}
	assert.Empty(t, jobNamesForRun(t, cl, "cri-310"), "no Job may be created for the refused run")
	assert.Nil(t, refused.Status.LaunchManifest, "a refused run stamps no launch manifest")

	select {
	case ev := <-recorder.Events:
		assert.Contains(t, ev, "Warning")
		assert.Contains(t, ev, "InterfaceBindingMismatch")
		assert.Contains(t, ev, `adapter kind "copilot"`)
	default:
		t.Fatal("expected an interface-binding-mismatch refusal event")
	}
}

// TestReconcileRefusesUrlWorkflowWithoutStampedInterface drives the
// fail-open fix (KB-310): a url CR authored directly against the API with
// no stamped interface is refused closed at admission — nothing Job-wise,
// no manifest, and the refusal names the binding and the addressed tree.
func TestReconcileRefusesUrlWorkflowWithoutStampedInterface(t *testing.T) {
	r, run, recorder, cl := interfaceBindingFixture(t, func(spec *criteriav1.CriteriaRunSpec) {
		spec.Workflow.Interface = nil
	})
	ctx := context.Background()

	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, res)

	var refused criteriav1.CriteriaRun
	require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(run), &refused))
	assert.Equal(t, criteriav1.PhaseFailed, refused.Status.Phase)
	cond := condByType(t, refused, criteriav1.ConditionInterfaceBindingMismatch)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	for _, want := range []string{"no stamped interface", "kanboard_develop_v1", "criteria-develop", "KB-310"} {
		assert.Contains(t, cond.Message, want, "the refusal names the gap and both sides")
	}
	assert.Empty(t, jobNamesForRun(t, cl, "cri-310"), "no Job may be created for the refused run")
	assert.Nil(t, refused.Status.LaunchManifest, "a refused run stamps no launch manifest")

	select {
	case ev := <-recorder.Events:
		assert.Contains(t, ev, "Warning")
		assert.Contains(t, ev, "InterfaceBindingMismatch")
		assert.Contains(t, ev, "no stamped interface")
	default:
		t.Fatal("expected an interface-binding-mismatch refusal event")
	}
}

// TestReconcileImageWorkflowWithoutInterfaceKeepsCarveout pins the other
// arm of the fail-open fix (KB-310): a type=image run with a nil interface
// remains the pre-KB-310 carve-out and reconciles exactly as before — no
// refusal condition, no interface validation, and no launch manifest.
func TestReconcileImageWorkflowWithoutInterfaceKeepsCarveout(t *testing.T) {
	r, run, _, cl := interfaceBindingFixture(t, func(spec *criteriav1.CriteriaRunSpec) {
		spec.Workflow.Interface = nil
		spec.Workflow.Type = "image"
		spec.Workflow.Image = "localhost:5000/criteria:dev"
		spec.Workflow.URL = ""
		spec.Workflow.Ref = ""
	})
	ctx := context.Background()

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	require.NoError(t, err)

	assert.NotEmpty(t, jobNamesForRun(t, cl, "cri-310"),
		"the carved-out image-mode run admits without an interface")
	var admitted criteriav1.CriteriaRun
	require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(run), &admitted))
	assert.Nil(t, admitted.Status.LaunchManifest,
		"runs without a stamped interface carry no launch manifest")
	for _, c := range admitted.Status.Conditions {
		assert.NotEqual(t, criteriav1.ConditionInterfaceBindingMismatch, c.Type,
			"no interface-binding refusal on the carved-out image-mode arm")
	}
}

// TestReconcileStampsLaunchManifest admits a satisfying binding and records
// the launch manifest on the run status: workflow ref+digest, interface and
// binding checksums, operator version, resolved lanes.
func TestReconcileStampsLaunchManifest(t *testing.T) {
	r, run, _, cl := interfaceBindingFixture(t, nil)
	ctx := context.Background()

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	require.NoError(t, err)
	assert.Len(t, jobNamesForRun(t, cl, "cri-310"), 3, "runner plus two adapters admit")

	var admitted criteriav1.CriteriaRun
	require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(run), &admitted))
	require.NotNil(t, admitted.Status.LaunchManifest, "an admitted interface-carrying run stamps its manifest")
	manifest := admitted.Status.LaunchManifest
	wf := run.Spec.Workflow
	assert.Equal(t, wf.URL, manifest.WorkflowRef)
	assert.Equal(t, wf.Ref, manifest.WorkflowDigest)
	assert.Equal(t, "sha256:", manifest.InterfaceChecksum[:7])
	assert.Equal(t, "sha256:", manifest.BindingChecksum[:7])
	assert.Equal(t, "dev", manifest.OperatorVersion, "the zero reconciler falls back to dev")
	assert.Equal(t, []string{"WORKFLOW_GITHUB_TOKEN@github-tokens"}, manifest.Lanes)

	// The manifest is stamped once: a later pass must not re-stamp it.
	before := *manifest
	_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	require.NoError(t, err)
	require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(run), &admitted))
	assert.Equal(t, before, *admitted.Status.LaunchManifest,
		"the manifest pins the first admission, not the latest pass")
}

// TestReconcileRefusesUnknownConfigSlot drives the KB-310 config-slot side
// of admission: the tree's interface declares its override surface, and a
// binding config value for a slot the tree does not honor rejects
// unknown-slot with both sides named (no Jobs, no launch manifest).
func TestReconcileRefusesUnknownConfigSlot(t *testing.T) {
	r, run, _, cl := interfaceBindingFixture(t, func(spec *criteriav1.CriteriaRunSpec) {
		spec.BuildCmd = "make ci" // the developer tree honors no buildCmd slot
	})
	ctx := context.Background()

	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, res)

	var refused criteriav1.CriteriaRun
	require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(run), &refused))
	assert.Equal(t, criteriav1.PhaseFailed, refused.Status.Phase)
	cond := condByType(t, refused, criteriav1.ConditionConfigRenderDrift)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	for _, want := range []string{`config slot "buildCmd"`, "kanboard_develop_v1", "unknown-slot", "KB-310"} {
		assert.Contains(t, cond.Message, want, "the refusal names the offending slot and the tree")
	}
	assert.Empty(t, jobNamesForRun(t, cl, "cri-310"), "no Job may be created for the refused run")
	assert.Nil(t, refused.Status.LaunchManifest, "a refused run stamps no launch manifest")
}

func condByType(t *testing.T, run criteriav1.CriteriaRun, condType string) metav1.Condition {
	t.Helper()
	if idx := slices.IndexFunc(run.Status.Conditions, func(c metav1.Condition) bool { return c.Type == condType }); idx >= 0 {
		return run.Status.Conditions[idx]
	}
	t.Fatalf("no %s condition on %s", condType, run.Name)
	return metav1.Condition{}
}
