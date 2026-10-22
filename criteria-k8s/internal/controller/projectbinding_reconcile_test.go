// KB-311 (ADR 307), reconcile level: a run stamped with spec.
// projectBindingRef admits through the project's criteria-binding CM in
// its own namespace — the CM is READ but never written (the project owns
// it), its payload materializes onto the in-memory run, the checksum in
// the launch manifest is the BINDING payload's sha256, and every
// fail-closed path (missing CM, pin drift, unknown key, uncovered env)
// refuses before any Job is created or any manifest is stamped.
package controller_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

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

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/binding"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/controller"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/jobbuilder"
)

// projectBindingPayload mirrors k8s/examples/binding-criteria-go-dev.yaml
// (contract: one project namespace's COMPLETE execution config). The env
// and secret lanes cover the fixture tree's interface; the extras
// (GOFLAGS, the reviewer lane) are the project's own and stay legal.
const projectBindingPayload = `{
  "apiVersion": "criteria.brokenbots.dev/v1",
  "kind": "CriteriaBinding",
  "images": {
    "shell": "registry.dev/brokenbots/adapter-shell-peer-go:0.5.56",
    "copilot": "registry.dev/brokenbots/adapter-copilot-peer-go:0.5.56"
  },
  "env": {
    "CRITERIA_RUN_MODE": "k8s",
    "GOFLAGS": "-mod=mod"
  },
  "volumes": [
    {"name": "workspace", "kind": "pvc", "claim": "go-dev-workspace", "mountPath": "/home/criteria/workspace"},
    {"name": "cache", "kind": "tmp", "mountPath": "/tmp/criteria-cache", "sizeLimit": "4Gi"}
  ],
  "secrets": [
    {"name": "github", "secretProviderClass": "go-dev-github-spc", "mountPath": "/home/criteria/secrets", "env": {"WORKFLOW_GITHUB_TOKEN": "github_token", "REVIEWER_GITHUB_TOKEN": "reviewer_github_token"}}
  ],
  "config": {}
}`

// expectedBindingChecksum independently recomputes the manifest's
// binding checksum: a canonical-JSON sha256 of the parsed payload.
func expectedBindingChecksum(payload string) string {
	var want binding.Payload
	if err := json.Unmarshal([]byte(payload), &want); err != nil {
		panic(fmt.Sprintf("fixture payload must parse: %s", err))
	}
	canonical, err := json.Marshal(want)
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf("sha256:%x", sha256.Sum256(canonical))
}

func bindingConfigMap(namespace, resourceVersion, payload string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:            binding.CMName,
			Namespace:       namespace,
			ResourceVersion: resourceVersion,
		},
		Data: map[string]string{binding.BindingKey: payload},
	}
}

// projectBindingFixture builds an interface-carrying run whose WORKFLOW
// entry carries NO inline binding data (empty images/env/volumes/
// secrets) — everything binding-shaped must materialize from the project
// CM, which the fixture installs at resourceVersion 111. A payload
// override installs a different (often malformed) payload under the same
// pin instead.
func projectBindingFixture(t *testing.T, mutate func(spec *criteriav1.CriteriaRunSpec), payloadOverride ...string) (*controller.CriteriaRunReconciler, *criteriav1.CriteriaRun, client.Client) {
	t.Helper()
	payload := projectBindingPayload
	if len(payloadOverride) > 0 {
		payload = payloadOverride[0]
	}
	scheme := runtime.NewScheme()
	require.NoError(t, criteriav1.AddToScheme(scheme))
	require.NoError(t, batchv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	run := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cri-311",
			Namespace: "criteria-go-dev",
			UID:       types.UID("run-uid-311"),
		},
		Spec: criteriav1.CriteriaRunSpec{
			TicketID:        "KB-311",
			RepoURL:         "https://github.com/brokenbots/workflow-example.git",
			Image:           "localhost:5000/criteria:dev",
			MaxAgentVisits:  2,
			ProviderBaseURL: "http://provider/v1",
			ProjectBindingRef: &criteriav1.CriteriaRunProjectBindingRef{
				Name:            binding.CMName,
				ResourceVersion: "111",
			},
			WorkflowSource: &criteriav1.RunWorkflowSource{
				Type: "url",
				URL:  "git::https://github.com/brokenbots/workflow-example.git",
			},
			Workflow: &criteriav1.RunWorkflow{
				Name:      "criteria-develop",
				Type:      "url",
				Namespace: "criteria-go-dev",
				Class:     "triage",
				URL:       "git::https://github.com/brokenbots/workflow-example.git//kanboard_develop_v1",
				Ref:       "0123456789abcdef0123456789abcdef01234567",
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
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(run).
		WithObjects(run, bindingConfigMap("criteria-go-dev", "111", payload)).Build()
	r := &controller.CriteriaRunReconciler{
		Client:   cl,
		Scheme:   scheme,
		Castle:   &fakeCastle{},
		Defaults: jobbuilder.Defaults{DataPVC: "criteria-data"},
		Queue:    controller.NewRunQueue(),
		Recorder: record.NewFakeRecorder(16),
	}
	return r, run, cl
}

func updateBindingCM(t *testing.T, cl client.Client, run *criteriav1.CriteriaRun, payload string) {
	t.Helper()
	var cm corev1.ConfigMap
	require.NoError(t, cl.Get(context.Background(), client.ObjectKey{Name: binding.CMName, Namespace: run.Namespace}, &cm))
	cm.Data = map[string]string{binding.BindingKey: payload}
	require.NoError(t, cl.Update(context.Background(), &cm))
}

// jobNamesForRunIn lists the child Jobs of a run in that run's namespace
// (jobNamesForRun is pinned to the default namespace).
func jobNamesForRunIn(t *testing.T, cl client.Client, run *criteriav1.CriteriaRun) []string {
	t.Helper()
	var jobs batchv1.JobList
	require.NoError(t, cl.List(context.Background(), &jobs, client.InNamespace(run.Namespace)))
	var names []string
	for _, j := range jobs.Items {
		if strings.HasPrefix(j.Name, run.Name) {
			names = append(names, j.Name)
		}
	}
	return names
}

func TestReconcileAdmitsThroughProjectBinding(t *testing.T) {
	r, run, cl := projectBindingFixture(t, nil)
	ctx := context.Background()

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	require.NoError(t, err)
	assert.Len(t, jobNamesForRunIn(t, cl, run), 3,
		"the runner plus the two interface-required adapters admit through the binding")

	var admitted criteriav1.CriteriaRun
	require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(run), &admitted))

	// The launch manifest's binding checksum is the BINDING payload's
	// sha256 (canonical JSON), not the materialized workflow object's.
	manifest := admitted.Status.LaunchManifest
	require.NotNil(t, manifest, "an admitted project-bound run stamps its manifest")
	assert.Equal(t, expectedBindingChecksum(projectBindingPayload), manifest.BindingChecksum,
		"the manifest's binding checksum pins the project binding payload")
	assert.Equal(t, "sha256:", manifest.InterfaceChecksum[:7])
	assert.Equal(t, []string{"WORKFLOW_GITHUB_TOKEN@github"}, manifest.Lanes,
		"the interface-required lane resolves from the binding's secret env map")

	// Config provenance pins the binding CM — the KB-103 shape delivered
	// from the project's own namespace.
	require.NotNil(t, admitted.Status.ConfigRender)
	assert.Equal(t, binding.CMName, admitted.Status.ConfigRender.ConfigMap)
	assert.Equal(t, "111", admitted.Status.ConfigRender.ResourceVersion)

	// The binding's execution config reaches the child Jobs.
	var runner batchv1.Job
	require.NoError(t, cl.Get(ctx, types.NamespacedName{
		Name: jobbuilder.RunnerJobName(run), Namespace: run.Namespace}, &runner))
	for _, c := range runner.Spec.Template.Spec.Containers {
		if c.Name != jobbuilder.RunnerContainerName {
			continue
		}
		env := map[string]string{}
		for _, ev := range c.Env {
			env[ev.Name] = ev.Value
		}
		assert.Equal(t, "-mod=mod", env["GOFLAGS"], "the runner carries the project binding's env")
		assert.Equal(t, "k8s", env["CRITERIA_RUN_MODE"], "the tree's required env is covered by the binding")
		assert.NotContains(t, env, "STALE_INLINE", "no workflow-entry binding data survives the materialization")
	}
	assert.Equal(t, "localhost:5000/criteria:dev", jobbuilder.RunnerJobImage(&runner),
		"the runner image is untouched by the binding (tree-owned process image)")

	claimBound, spcBound := false, false
	var jobs batchv1.JobList
	require.NoError(t, cl.List(ctx, &jobs, client.InNamespace(run.Namespace),
		client.MatchingLabels{jobbuilder.LabelRun: run.Name}))
	for _, job := range jobs.Items {
		for _, vol := range job.Spec.Template.Spec.Volumes {
			if vol.PersistentVolumeClaim != nil && vol.PersistentVolumeClaim.ClaimName == "go-dev-workspace" {
				claimBound = true
			}
			if vol.CSI != nil {
				assert.Equal(t, "go-dev-github-spc", vol.CSI.VolumeAttributes["secretProviderClass"],
					"the binding's own SecretProviderClass serves the run, in the project's namespace")
				spcBound = true
			}
		}
	}
	assert.True(t, claimBound, "the binding's own PVC claim serves the run")
	assert.True(t, spcBound, "the binding's CSI secret mount reaches the pods")

	// The adapter images come from the binding, not the workflow entry.
	for _, kind := range []string{"shell", "copilot"} {
		var adapter batchv1.Job
		require.NoError(t, cl.Get(ctx, types.NamespacedName{
			Name: jobbuilder.AdapterJobName(run, kind), Namespace: run.Namespace}, &adapter))
		require.NotEmpty(t, adapter.Spec.Template.Spec.Containers)
		assert.Equal(t, fmt.Sprintf("registry.dev/brokenbots/adapter-%s-peer-go:0.5.56", kind),
			adapter.Spec.Template.Spec.Containers[0].Image, "the adapter image comes from the project binding")
	}

	// Read-only ownership check: admission never writes the binding CM.
	var cm corev1.ConfigMap
	require.NoError(t, cl.Get(ctx, client.ObjectKey{Name: binding.CMName, Namespace: run.Namespace}, &cm))
	assert.Equal(t, projectBindingPayload, cm.Data[binding.BindingKey],
		"the project's binding CM is read-only to the platform")
}

// TestReconcileRefusesMissingBindingCM: a run whose project binding CM is
// gone admits nothing — fail closed before any Job or manifest is stamped.
func TestReconcileRefusesMissingBindingCM(t *testing.T) {
	r, run, cl := projectBindingFixture(t, nil)
	ctx := context.Background()

	var cm corev1.ConfigMap
	require.NoError(t, cl.Get(ctx, client.ObjectKey{Name: binding.CMName, Namespace: run.Namespace}, &cm))
	require.NoError(t, cl.Delete(ctx, &cm))

	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, res)

	var refused criteriav1.CriteriaRun
	require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(run), &refused))
	assert.Equal(t, criteriav1.PhaseFailed, refused.Status.Phase)
	cond := condByType(t, refused, criteriav1.ConditionInterfaceBindingMismatch)
	for _, want := range []string{"criteria-binding", "no longer exists", "111"} {
		assert.Contains(t, cond.Message, want)
	}
	assert.Empty(t, jobNamesForRunIn(t, cl, run))
	assert.Nil(t, refused.Status.LaunchManifest)
}

// TestReconcileRefusesBindingPinMismatch: a binding replaced (or its pin
// unmet) refuses fast — admission re-verifies the stamped
// resourceVersion and names both sides.
func TestReconcileRefusesBindingPinMismatch(t *testing.T) {
	r, run, cl := projectBindingFixture(t, nil)
	ctx := context.Background()

	// Replace the binding under the run's pin: same payload at a fresh
	// resourceVersion (the fake client bumps RV on Update).
	updateBindingCM(t, cl, run, projectBindingPayload)

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	require.NoError(t, err)

	var refused criteriav1.CriteriaRun
	require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(run), &refused))
	assert.Equal(t, criteriav1.PhaseFailed, refused.Status.Phase)
	cond := condByType(t, refused, criteriav1.ConditionInterfaceBindingMismatch)
	assert.Contains(t, cond.Message, "pinned at resourceVersion")
	assert.Contains(t, cond.Message, "criteria-binding")
	assert.Empty(t, jobNamesForRunIn(t, cl, run))
}

// TestReconcileRefusesBindingUnknownKey: a binding payload carrying an
// unknown key is refused at parse — fail closed, the refusal names the
// key, no Jobs.
func TestReconcileRefusesBindingUnknownKey(t *testing.T) {
	r, run, cl := projectBindingFixture(t, nil, `{"apiVersion": "criteria.brokenbots.dev/v1", "kind": "CriteriaBinding", "images": {"shell": "reg/shell:2"}, "workflowLibrary": {}}`)
	ctx := context.Background()

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	require.NoError(t, err)

	var refused criteriav1.CriteriaRun
	require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(run), &refused))
	assert.Equal(t, criteriav1.PhaseFailed, refused.Status.Phase)
	cond := condByType(t, refused, criteriav1.ConditionInterfaceBindingMismatch)
	assert.Contains(t, cond.Message, "workflowLibrary", "the refusal names the unknown key")
	assert.Empty(t, jobNamesForRunIn(t, cl, run))
}

// TestReconcileRefusesBindingEnvCoverageGap: the KB-311 mode flip — the
// interface is the requirement side, so a binding whose env does not
// cover a declared env name refuses admission.
func TestReconcileRefusesBindingEnvCoverageGap(t *testing.T) {
	r, run, cl := projectBindingFixture(t, nil, `{
	  "apiVersion": "criteria.brokenbots.dev/v1", "kind": "CriteriaBinding",
	  "images": {"shell": "reg/shell:2", "copilot": "reg/copilot:2"},
	  "env": {"GOFLAGS": "-mod=mod"},
	  "volumes": [
	    {"name": "workspace", "kind": "pvc", "claim": "go-dev-workspace", "mountPath": "/home/criteria/workspace"},
	    {"name": "cache", "kind": "tmp", "mountPath": "/tmp/criteria-cache"}
	  ],
	  "secrets": [{"name": "github", "secretProviderClass": "go-dev-github-spc", "mountPath": "/home/criteria/secrets", "env": {"WORKFLOW_GITHUB_TOKEN": "github_token"}}],
	  "config": {}}`)
	ctx := context.Background()

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	require.NoError(t, err)

	var refused criteriav1.CriteriaRun
	require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(run), &refused))
	assert.Equal(t, criteriav1.PhaseFailed, refused.Status.Phase)
	cond := condByType(t, refused, criteriav1.ConditionInterfaceBindingMismatch)
	assert.Contains(t, cond.Message, `requires env "CRITERIA_RUN_MODE"`)
	assert.Contains(t, cond.Message, "does not cover it")
	assert.Empty(t, jobNamesForRunIn(t, cl, run))
}

// TestReconcileProjectBindingRenderDrift: an admitted run whose recorded
// binding render no longer matches the live CM fails fast on the next
// pass with ConfigRenderDrift — the render's provenance pin watches the
// whole binding, not just the config surface.
func TestReconcileProjectBindingRenderDrift(t *testing.T) {
	// A resourceVersion-less pin (an operator-authored CR) leaves the CM
	// re-verification to the recorded render: the render's provenance pin
	// is the admission-time CM, so a replaced CM drifts the run fast with
	// the ConfigRenderDrift condition.
	r, run, cl := projectBindingFixture(t, func(spec *criteriav1.CriteriaRunSpec) {
		spec.ProjectBindingRef.ResourceVersion = ""
	})
	ctx := context.Background()

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	require.NoError(t, err)

	var admitted criteriav1.CriteriaRun
	require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(run), &admitted))
	require.NotNil(t, admitted.Status.ConfigRender)
	assert.Equal(t, "111", admitted.Status.ConfigRender.ResourceVersion)

	// The binding CM changes under the admitted run.
	updateBindingCM(t, cl, run, projectBindingPayload)

	_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	require.NoError(t, err)

	var drifted criteriav1.CriteriaRun
	require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(run), &drifted))
	assert.Equal(t, criteriav1.PhaseFailed, drifted.Status.Phase)
	cond := condByType(t, drifted, criteriav1.ConditionConfigRenderDrift)
	assert.Contains(t, cond.Message, "criteria-binding")
}
