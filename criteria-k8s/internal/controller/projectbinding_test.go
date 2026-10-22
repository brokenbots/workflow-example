// KB-311 (ADR 307): unit-level coverage for the project-binding admission
// path — the projectBound validation-mode flip and the in-memory
// materialization of the binding payload onto the run.
package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/binding"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/routes"
)

// TestValidateInterfaceBindingProjectBoundMode drives the KB-311 mode flip:
// a project-bound run's interface becomes the REQUIREMENT side — the tree's
// declared env names must be covered by the binding env and the declared
// secret lanes must resolve; binding-owned extras (tool paths, extra
// credential lanes, extra adapter kinds) are the project's own config and
// stay legal. The pre-KB-311 closed-surface rules remain exactly the
// KB-310 ones for runs without a binding pin.
func TestValidateInterfaceBindingProjectBoundMode(t *testing.T) {
	boundRun := func() *criteriav1.CriteriaRun {
		run := testRun()
		run.Spec.Workflow.Interface = testInterface()
		// The project's own extras: an undeclared env key, an undeclared
		// credential lane, and an adapter kind the tree does not require.
		run.Spec.Workflow.Env["GOFLAGS"] = "-mod=mod"
		run.Spec.Workflow.Secrets[0].Env["REVIEWER_GITHUB_TOKEN"] = "reviewer_github_token"
		run.Spec.Workflow.AdapterImages["linter"] = "reg/linter:1"
		return run
	}
	r := boundRun()
	require.NoError(t, validateInterfaceBinding(r, true),
		"project-owned extras are legal in binding mode: the interface is the requirement side, not the closed surface")

	t.Run("missing required env coverage rejects naming both sides", func(t *testing.T) {
		run := boundRun()
		delete(run.Spec.Workflow.Env, "CRITERIA_RUN_MODE")
		err := validateInterfaceBinding(run, true)
		require.Error(t, err, "an interface-required env name the binding env does not cover rejects")
		for _, want := range []string{
			`requires env "CRITERIA_RUN_MODE"`,
			"does not cover it",
			"kanboard_develop_v1",
			"criteria-develop",
			"KB-311",
		} {
			assert.Contains(t, err.Error(), want)
		}
		// The same run admits under the legacy mode's rules: only the
		// flip's coverage direction is new, the shared checks do not
		// regress.
		assert.Error(t, validateInterfaceBinding(run, false),
			"the KB-310 closed-surface rule still names the same gap in legacy mode")
	})

	t.Run("missing required secret env lane still rejects in binding mode", func(t *testing.T) {
		run := boundRun()
		delete(run.Spec.Workflow.Secrets[0].Env, "WORKFLOW_GITHUB_TOKEN")
		err := validateInterfaceBinding(run, true)
		require.Error(t, err, "a declared lane the binding secrets do not resolve rejects in binding mode too")
		assert.Contains(t, err.Error(), `requires secret env "WORKFLOW_GITHUB_TOKEN"`)
		assert.Contains(t, err.Error(), "criteria-develop")
	})

	t.Run("binding mode does not drop the unknown adapter-kind and mount rules", func(t *testing.T) {
		run := boundRun()
		run.Spec.Workflow.Volumes = nil
		err := validateInterfaceBinding(run, true)
		require.Error(t, err, "the byte-exact mount contract holds in binding mode")
		assert.Contains(t, err.Error(), "workspace")
	})
}

// TestMaterializeProjectBinding drives the in-memory materialization: the
// binding payload's images/env/volumes/secrets replace the run's
// binding-shaped workflow fields (deep copies, no aliasing), while the
// tree references stay as the workflow entry carried them.
func TestMaterializeProjectBinding(t *testing.T) {
	payload := binding.Payload{
		Images: map[string]string{"shell": "reg/shell:2", "copilot": "reg/copilot:2"},
		Env:    map[string]string{"CRITERIA_RUN_MODE": "k8s", "GOFLAGS": "-mod=mod"},
		Volumes: []routes.Volume{
			{Name: "workspace", Kind: "pvc", MountPath: "/home/criteria/workspace", Claim: "go-dev-workspace"},
			{Name: "cache", Kind: "tmp", MountPath: "/tmp/criteria-cache", SizeLimit: "4Gi"},
		},
		Secrets: []routes.Secret{{
			Name:                "github",
			SecretProviderClass: "go-dev-github-spc",
			MountPath:           "/home/criteria/secrets",
			Env:                 map[string]string{"WORKFLOW_GITHUB_TOKEN": "github_token"},
		}},
	}

	run := testRun()
	run.Spec.Workflow.Image = "legacy-image:1"
	run.Spec.Workflow.Interface = testInterface()

	// Stale inline binding data must not survive materialization: the
	// payload is the sole source of the binding-shaped fields.
	run.Spec.Workflow.AdapterImages["stale"] = "reg/stale:1"
	run.Spec.Workflow.Env["STALE_KEY"] = "1"

	materializeProjectBinding(run, payload)
	wf := run.Spec.Workflow
	assert.Equal(t, payload.Images, wf.AdapterImages, "adapter images come from the binding")
	assert.Equal(t, payload.Env, wf.Env, "env comes from the binding")
	require.Len(t, wf.Volumes, 2)
	assert.Equal(t, "go-dev-workspace", wf.Volumes[0].Claim, "volumes convert into the run shape")
	assert.Equal(t, "4Gi", wf.Volumes[1].SizeLimit)
	require.Len(t, wf.Secrets, 1)
	assert.Equal(t, "go-dev-github-spc", wf.Secrets[0].SecretProviderClass)
	assert.Equal(t, map[string]string{"WORKFLOW_GITHUB_TOKEN": "github_token"}, wf.Secrets[0].Env,
		"secret entries carry name-only references; no credential material rides the CR")

	// The tree references survive untouched.
	assert.Equal(t, "criteria-develop", wf.Name)
	assert.Equal(t, "url", wf.Type)
	assert.Equal(t, "git::https://github.com/brokenbots/workflow-example.git//kanboard_develop_v1", wf.URL)
	assert.Equal(t, "0123456789abcdef0123456789abcdef01234567", wf.Ref)
	assert.Equal(t, "legacy-image:1", wf.Image, "the url+image process image stays tree-owned, not binding-owned")
	assert.Same(t, run.Spec.Workflow.Interface, wf.Interface, "the interface declaration is untouched")

	// The materialized run satisfies the interface in binding mode.
	require.NoError(t, validateInterfaceBinding(run, true),
		"a payload mirroring the interface's requirements materializes admissibly")

	t.Run("no aliasing between payload maps and the run", func(t *testing.T) {
		payload.Env["LATE"] = "1"
		payload.Images["late"] = "reg/late:1"
		assert.NotContains(t, run.Spec.Workflow.Env, "LATE")
		assert.NotContains(t, run.Spec.Workflow.AdapterImages, "late")
	})

	t.Run("empty payload clears the binding-shaped fields", func(t *testing.T) {
		run := testRun()
		run.Spec.Workflow.Interface = testInterface()
		materializeProjectBinding(run, binding.Payload{})
		assert.Nil(t, run.Spec.Workflow.Env)
		assert.Nil(t, run.Spec.Workflow.Volumes)
		assert.Nil(t, run.Spec.Workflow.Secrets)
		assert.Nil(t, run.Spec.Workflow.AdapterImages)
	})

	t.Run("workflow-less runs are untouched", func(t *testing.T) {
		run := &criteriav1.CriteriaRun{}
		assert.NotPanics(t, func() { materializeProjectBinding(run, payload) })
	})
}
