// KB-310: admission validation of a run's project binding against the
// workflow tree's machine-readable interface. The gate is fail closed
// (the declaration is the only override surface), names both sides on a
// mismatch, and stamps the launch manifest — the reproducibility contract.
package controller

import (
	"testing"

	logr "github.com/go-logr/logr"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
)

func discardLogger() logr.Logger { return logr.Discard() }

// testInterface builds the interface declaration the tests validate
// against: two adapters, one tmp mount, one pvc mount, one secret lane,
// one env name.
func testInterface() *criteriav1.RunWorkflowInterface {
	return &criteriav1.RunWorkflowInterface{
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
	}
}

func testBinding() *criteriav1.RunWorkflow {
	return &criteriav1.RunWorkflow{
		Name:          "criteria-develop",
		Type:          "url",
		URL:           "git::https://github.com/brokenbots/workflow-example.git//kanboard_develop_v1",
		Ref:           "0123456789abcdef0123456789abcdef01234567",
		AdapterImages: map[string]string{"shell": "reg/shell:1", "copilot": "reg/copilot:1"},
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
	}
}

func testRun() *criteriav1.CriteriaRun {
	return &criteriav1.CriteriaRun{Spec: criteriav1.CriteriaRunSpec{
		Workflow:       testBinding(),
		WorkflowSource: &criteriav1.RunWorkflowSource{Type: "url", URL: "git::https://github.com/brokenbots/workflow-example.git"},
	}}
}

func TestInterfaceBindingApplies(t *testing.T) {
	assert.False(t, interfaceBindingApplies(&criteriav1.CriteriaRun{}),
		"a workflow-less run keeps the legacy image-mode path")
	wfless := testRun()
	wfless.Spec.Workflow.Interface = nil
	assert.False(t, interfaceBindingApplies(wfless),
		"a workflow without a stamped interface is the pre-KB-310 legacy carve-out")
	assert.True(t, interfaceBindingApplies(func() *criteriav1.CriteriaRun {
		run := testRun()
		run.Spec.Workflow.Interface = testInterface()
		return run
	}()), "a workflow with a stamped interface is admissible under the gate")
}

func TestValidateInterfaceBinding(t *testing.T) {
	good := testRun()
	good.Spec.Workflow.Interface = testInterface()
	require.NoError(t, validateInterfaceBinding(good), "the matching binding admits")

	extraAdapters := testRun()
	extraAdapters.Spec.Workflow.Interface = testInterface()
	extraAdapters.Spec.Workflow.AdapterImages["intake"] = "reg/intake:1"
	require.NoError(t, validateInterfaceBinding(extraAdapters),
		"extra binding adapter kinds are tolerated; only required kinds are validated")

	for _, tc := range []struct {
		name string
		m    func(wf *criteriav1.RunWorkflow)
		want []string
	}{
		{
			name: "missing required adapter kind names the tree and both sides",
			m:    func(wf *criteriav1.RunWorkflow) { delete(wf.AdapterImages, "copilot") },
			want: []string{`adapter kind "copilot"`, "kanboard_develop_v1", "criteria-develop", "shell"},
		},
		{
			name: "an undeclared extra mount rejects as unknown-slot",
			m: func(wf *criteriav1.RunWorkflow) {
				wf.Volumes = append(wf.Volumes, criteriav1.RunWorkflowVolume{Name: "scratch", Kind: "tmp", MountPath: "/tmp/scratch"})
			},
			want: []string{"volume \"scratch\"", "does not require", "kanboard_develop_v1", "unknown-slot"},
		},
		{
			name: "extra binding volume at an undeclared path rejects",
			m: func(wf *criteriav1.RunWorkflow) {
				wf.Volumes = append(wf.Volumes, criteriav1.RunWorkflowVolume{Name: "data", Kind: "pvc", MountPath: "/data"})
			},
			want: []string{"volume \"data\"", "pvc at /data", "kanboard_develop_v1"},
		},
		{
			name: "wrong mount kind on a required path rejects",
			m: func(wf *criteriav1.RunWorkflow) {
				wf.Volumes[0].Kind = "tmp"
			},
			want: []string{"wants kind \"tmp\"", "declares \"pvc\"", "workspace"},
		},
		{
			name: "missing required mount names the gap",
			m:    func(wf *criteriav1.RunWorkflow) { wf.Volumes = wf.Volumes[1:] },
			want: []string{"requires mount \"workspace\"", "pvc", "/home/criteria/workspace", "criteria-develop"},
		},
		{
			name: "unresolvable secret env lane names the lane",
			m:    func(wf *criteriav1.RunWorkflow) { wf.Secrets[0].Env = map[string]string{} },
			want: []string{`requires secret env "WORKFLOW_GITHUB_TOKEN"`, "github-tokens", "criteria-develop"},
		},
		{
			name: "no secrets at all names the missing lane",
			m:    func(wf *criteriav1.RunWorkflow) { wf.Secrets = nil },
			want: []string{`requires secret env "WORKFLOW_GITHUB_TOKEN"`, "[] (none)"},
		},
		{
			name: "unknown binding secret env key rejects as unknown-slot",
			m:    func(wf *criteriav1.RunWorkflow) { wf.Secrets[0].Env["WORKFLOW_SLACK_TOKEN"] = "slack_token" },
			want: []string{`secret "github-tokens" carries env "WORKFLOW_SLACK_TOKEN"`, "does not require", "unknown-slot"},
		},
		{
			name: "plain env key outside the declaration rejects, including a secret-lane name",
			m:    func(wf *criteriav1.RunWorkflow) { wf.Env["CRITERIA_EXTRA"] = "1" },
			want: []string{`env key "CRITERIA_EXTRA"`, "not declared", "kanboard_develop_v1"},
		},
		{
			name: "secret-lane name delivered as plain env rejects",
			m:    func(wf *criteriav1.RunWorkflow) { wf.Env["WORKFLOW_GITHUB_TOKEN"] = "leak" },
			want: []string{`env key "WORKFLOW_GITHUB_TOKEN"`, "not declared"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := testRun()
			tc.m(run.Spec.Workflow)
			run.Spec.Workflow.Interface = testInterface()
			err := validateInterfaceBinding(run)
			require.Error(t, err, "the mutated binding must reject")
			for _, want := range tc.want {
				assert.Contains(t, err.Error(), want)
			}
			// Both sides are always named: the tree and the binding workflow.
			assert.Contains(t, err.Error(), "criteria-develop")
		})
	}
}

func TestValidateInterfaceBindingEmptyEnvSecretExempt(t *testing.T) {
	run := testRun()
	run.Spec.Workflow.Interface = testInterface()
	// The CSI file-mount lane: a secret entry with an empty env map
	// contributes no keys and must not reject.
	run.Spec.Workflow.Secrets = append(run.Spec.Workflow.Secrets, criteriav1.RunWorkflowSecret{
		Name:                "netrc",
		SecretProviderClass: "criteria-netrc",
		MountPath:           "/home/criteria/.netrc",
	})
	// Operator plumbing rides volume-level env maps (CRITERIA_*), exempt.
	run.Spec.Workflow.Volumes[0].Env = map[string]string{"CRITERIA_RUN_ID": "run-id"}
	require.NoError(t, validateInterfaceBinding(run),
		"empty secret env maps and volume-level plumbing env are exempt from the interface contract")
}

func TestLaunchManifest(t *testing.T) {
	run := testRun()
	run.Spec.Workflow.Interface = testInterface()
	require.NoError(t, validateInterfaceBinding(run))

	manifest, err := launchManifest(run, "k8s-9.9.9")
	require.NoError(t, err)
	wf := run.Spec.Workflow
	assert.Equal(t, wf.URL, manifest.WorkflowRef)
	assert.Equal(t, wf.Ref, manifest.WorkflowDigest)
	assert.Equal(t, "sha256:", manifest.InterfaceChecksum[:7])
	assert.Equal(t, "sha256:", manifest.BindingChecksum[:7])
	assert.Equal(t, "k8s-9.9.9", manifest.OperatorVersion)
	assert.Equal(t, []string{"WORKFLOW_GITHUB_TOKEN@github-tokens"}, manifest.Lanes)

	// Checksums are deterministic: re-marshaling the same declaration and
	// binding yields identical values.
	again, err2 := launchManifest(run, "k8s-9.9.9")
	require.NoError(t, err2)
	assert.Equal(t, manifest.InterfaceChecksum, again.InterfaceChecksum)
	assert.Equal(t, manifest.BindingChecksum, again.BindingChecksum)

	// The binding checksum moves when the binding moves; the interface
	// checksum does not (it pins the tree's declaration).
	moved := testRun()
	moved.Spec.Workflow.Interface = testInterface()
	moved.Spec.Workflow.Env = map[string]string{"CRITERIA_RUN_MODE": "other"}
	movedManifest, err3 := launchManifest(moved, "k8s-9.9.9")
	require.NoError(t, err3)
	assert.NotEqual(t, manifest.BindingChecksum, movedManifest.BindingChecksum)
	assert.Equal(t, manifest.InterfaceChecksum, movedManifest.InterfaceChecksum)

	// Lanes resolve per interface SecretEnv only: an env-name key inside a
	// secret's env map is not a lane, and a second secret entry without the
	// required lane contributes nothing.
	variant := testRun()
	variant.Spec.Workflow.Interface = testInterface()
	variant.Spec.Workflow.Secrets = append(variant.Spec.Workflow.Secrets, criteriav1.RunWorkflowSecret{
		Name: "slack", SecretProviderClass: "criteria-slack", MountPath: "/tmp/slack",
	})
	variantManifest, err4 := launchManifest(variant, "k8s-9.9.9")
	require.NoError(t, err4)
	assert.Equal(t, []string{"WORKFLOW_GITHUB_TOKEN@github-tokens"}, variantManifest.Lanes)
}

func TestStampLaunchManifestIdempotent(t *testing.T) {
	run := testRun()
	run.Spec.Workflow.Interface = testInterface()
	update := run.DeepCopy()
	stamper := &CriteriaRunReconciler{}
	stamper.stampLaunchManifest(update, discardLogger())
	require.NotNil(t, update.Status.LaunchManifest, "an interface-carrying run gets a manifest")
	first := *update.Status.LaunchManifest

	stamper.stampLaunchManifest(update, discardLogger())
	assert.Equal(t, first, *update.Status.LaunchManifest,
		"stampLaunchManifest is idempotent: the manifest pins the first admission")

	legacy := testRun()
	legacy.Spec.Workflow.Interface = nil
	legacyUpdate := legacy.DeepCopy()
	stamper.stampLaunchManifest(legacyUpdate, discardLogger())
	assert.Nil(t, legacyUpdate.Status.LaunchManifest,
		"the legacy carve-out (no stamped interface) carries no manifest")
}

func TestManifestSecretLanesOmitUnresolved(t *testing.T) {
	run := testRun()
	run.Spec.Workflow.Interface = testInterface()
	run.Spec.Workflow.Secrets[0].Env = map[string]string{}
	lanes := manifestSecretLanes(testInterface(), run.Spec.Workflow)
	assert.Empty(t, lanes,
		"a lane the binding does not resolve is omitted; admission validates before stamping")
}
