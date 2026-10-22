// KB-310: admission validation of a run's project binding against the
// workflow tree's machine-readable interface. The gate is fail closed
// (the declaration is the only override surface), names both sides on a
// mismatch, and stamps the launch manifest — the reproducibility contract.
package controller

import (
	"strings"
	"testing"

	logr "github.com/go-logr/logr"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/routes"
)

func discardLogger() logr.Logger { return logr.Discard() }

// testSlotInterface builds an interface that declares the develop tree's
// config surface (base branch + CI gate only) — a tree that honors base
// branch and CI gate but not build/test commands.
func testSlotInterface() *criteriav1.RunWorkflowInterface {
	iface := testInterface()
	iface.ConfigSlots = []criteriav1.RunWorkflowInterfaceConfigSlot{
		{Slot: "baseBranch", Variable: "base_branch", Target: "branch_manager"},
		{Slot: "ciGateCmd", Variable: "ci_gate_cmd", Target: "developer"},
	}
	return iface
}

// KB-310: a binding's config surface is validated against the tree's
// declared slots when the per-run config render is derived — an override
// the tree does not honor rejects unknown-slot (the declaration is the
// supported surface); declared slots render; unset slots override nothing
// and are free.
func TestKB310ConfigSlotDetail(t *testing.T) {
	valid := func() *criteriav1.CriteriaRun {
		run := testRun()
		run.Spec.Workflow.Interface = testSlotInterface()
		return run
	}

	t.Run("undeclared spec-field override rejects naming both sides", func(t *testing.T) {
		run := valid()
		run.Spec.BuildCmd = "npm run build"
		detail := kb310ConfigSlotDetail(run, specFieldsRender(run.Spec), nil)
		require.NotEmpty(t, detail, "an undeclared buildCmd override must reject")
		assert.Contains(t, detail, `config slot "buildCmd"`)
		assert.Contains(t, detail, `"npm run build"`)
		assert.Contains(t, detail, `"kanboard_develop_v1"`, "the tree side is named")
		assert.Contains(t, detail, "unknown-slot")
	})

	t.Run("declared slots render", func(t *testing.T) {
		run := valid()
		run.Spec.BaseBranch = "release/1.2"
		run.Spec.CIGateCmd = "make check"
		assert.Empty(t, kb310ConfigSlotDetail(run, specFieldsRender(run.Spec), nil))
	})

	t.Run("unset undeclared slots override nothing", func(t *testing.T) {
		run := valid()
		assert.Empty(t, kb310ConfigSlotDetail(run, specFieldsRender(run.Spec), nil),
			"empty values override nothing and are free to deliver")
	})

	t.Run("CM-carried override for an undeclared slot rejects with the config side named", func(t *testing.T) {
		run := valid()
		render := &criteriav1.RunConfigRender{
			ConfigMap: "castle", ResourceVersion: "7",
			TestCmd: "npm test",
		}
		detail := kb310ConfigSlotDetail(run, render, map[string]string{"testCmd": "npm test"})
		require.NotEmpty(t, detail)
		assert.Contains(t, detail, `config slot "testCmd"`)
		assert.Contains(t, detail, `in the project's config ConfigMap`)
	})

	t.Run("unknown ConfigMap data key rejects even valueless in the render", func(t *testing.T) {
		run := valid()
		render := &criteriav1.RunConfigRender{ConfigMap: "castle", ResourceVersion: "7"}
		detail := kb310ConfigSlotDetail(run, render, map[string]string{"deployCmd": "./deploy"})
		require.NotEmpty(t, detail)
		assert.Contains(t, detail, `config key "deployCmd"`)
		assert.Contains(t, detail, "no silent ignore")
	})

	t.Run("interface-less runs keep the pre-KB-310 behavior", func(t *testing.T) {
		run := testRun()
		run.Spec.Workflow.Interface = nil
		run.Spec.BuildCmd = "npm run build"
		render := &criteriav1.RunConfigRender{ConfigMap: "castle", ResourceVersion: "7", BuildCmd: "x"}
		assert.Empty(t, kb310ConfigSlotDetail(run, render, map[string]string{"deployCmd": "./deploy"}))
	})

	t.Run("workflow-less runs are the legacy image-mode path", func(t *testing.T) {
		assert.Empty(t, kb310ConfigSlotDetail(&criteriav1.CriteriaRun{Spec: criteriav1.CriteriaRunSpec{BuildCmd: "make"}},
			&criteriav1.RunConfigRender{BuildCmd: "make"}, nil))
	})

	t.Run("long values truncate on a rune boundary", func(t *testing.T) {
		run := valid()
		run.Spec.BuildCmd = strings.Repeat("é", 80)
		detail := kb310ConfigSlotDetail(run, specFieldsRender(run.Spec), nil)
		require.NotEmpty(t, detail)
		assert.NotContains(t, detail, "\ufffd", "the preview must not split a rune into a replacement char")
		assert.Contains(t, detail, "…", "long values preview truncated")
	})
}

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
	imageRun := testRun()
	imageRun.Spec.Workflow.Interface = nil
	imageRun.Spec.Workflow.Type = routes.TypeImage
	imageRun.Spec.Workflow.URL = ""
	assert.False(t, interfaceBindingApplies(imageRun),
		"interface-nil type=image stays the pre-KB-310 image-mode carve-out")
	urlNil := testRun()
	urlNil.Spec.Workflow.Interface = nil
	assert.False(t, interfaceBindingApplies(urlNil),
		"an interface-nil url workflow never enters validateInterfaceBinding — interfaceBindingGap must refuse it first")
	assert.True(t, interfaceBindingApplies(func() *criteriav1.CriteriaRun {
		run := testRun()
		run.Spec.Workflow.Interface = testInterface()
		return run
	}()), "a workflow with a stamped interface is admissible under the gate")
}

// TestInterfaceBindingGap drives the fail-open fix (KB-310): a url CR
// authored directly against the API with no stamped interface carries a
// named gap admission refuses closed — only type=image legitimately skips
// validation, and a stamped interface or no workflow is no gap at all.
func TestInterfaceBindingGap(t *testing.T) {
	urlNil := testRun()
	urlNil.Spec.Workflow.Interface = nil
	message, gap := interfaceBindingGap(urlNil)
	require.True(t, gap, "an interface-nil url workflow is the fail-open hole; admission must refuse it")
	for _, want := range []string{
		"no stamped interface",
		"criteria-develop",
		"kanboard_develop_v1",
		"url git::https://github.com/brokenbots/workflow-example.git//kanboard_develop_v1",
		"refused before job creation",
		"KB-310",
	} {
		assert.Contains(t, message, want, "the gap message names the binding, the addressed tree, and the failure mode")
	}

	imageNil := testRun()
	imageNil.Spec.Workflow.Interface = nil
	imageNil.Spec.Workflow.Type = routes.TypeImage
	imageNil.Spec.Workflow.URL = ""
	_, gap = interfaceBindingGap(imageNil)
	assert.False(t, gap, "interface-nil type=image is the pre-KB-310 carve-out, not a gap")

	stamped := testRun()
	stamped.Spec.Workflow.Interface = testInterface()
	_, gap = interfaceBindingGap(stamped)
	assert.False(t, gap, "a stamped interface is the validation arm, not a gap")

	_, gap = interfaceBindingGap(&criteriav1.CriteriaRun{})
	assert.False(t, gap, "a workflow-less run keeps the legacy path")
}

func TestValidateInterfaceBinding(t *testing.T) {
	good := testRun()
	good.Spec.Workflow.Interface = testInterface()
	require.NoError(t, validateInterfaceBinding(good, false), "the matching binding admits")

	extraAdapters := testRun()
	extraAdapters.Spec.Workflow.Interface = testInterface()
	extraAdapters.Spec.Workflow.AdapterImages["intake"] = "reg/intake:1"
	require.NoError(t, validateInterfaceBinding(extraAdapters, false),
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
			err := validateInterfaceBinding(run, false)
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
	require.NoError(t, validateInterfaceBinding(run, false),
		"empty secret env maps and volume-level plumbing env are exempt from the interface contract")
}

func TestLaunchManifest(t *testing.T) {
	run := testRun()
	run.Spec.Workflow.Interface = testInterface()
	require.NoError(t, validateInterfaceBinding(run, false))

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
	stamper.stampLaunchManifest(update, "", discardLogger())
	require.NotNil(t, update.Status.LaunchManifest, "an interface-carrying run gets a manifest")
	first := *update.Status.LaunchManifest

	stamper.stampLaunchManifest(update, "", discardLogger())
	assert.Equal(t, first, *update.Status.LaunchManifest,
		"stampLaunchManifest is idempotent: the manifest pins the first admission")

	legacy := testRun()
	legacy.Spec.Workflow.Interface = nil
	legacyUpdate := legacy.DeepCopy()
	stamper.stampLaunchManifest(legacyUpdate, "", discardLogger())
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
