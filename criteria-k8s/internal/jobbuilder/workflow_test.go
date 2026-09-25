package jobbuilder_test

// CRI-222: the workflow object stamped on the run (CRI-217) is the
// pod-construction source for image-mode runs. These tests pin the jobbuilder
// contract: target namespace, pvc/nfs/tmp volumes mapped into the
// environments that declare them, and secret delivery to adapters through
// OpenBao/CSI name references. url/source modes (CRI-231) must stay
// unconsumed.

import (
	"reflect"
	"strings"
	"testing"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/events"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/jobbuilder"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// operatorDefaultImage is the DEFAULT_CRITERIA_IMAGE the operator flags in
// with: the fresh linear-intake-remote tag from the 20260917 build.
const operatorDefaultImage = "localhost:5000/linear-intake-remote:20260917-205328-01a3800"

func workflowRun(name string, wf *criteriav1.RunWorkflow) *criteriav1.CriteriaRun {
	return &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "criteria-jobs", UID: "run-uid"},
		Spec: criteriav1.CriteriaRunSpec{
			TicketID: "CRI-222",
			RepoURL:  "https://github.com/brokenbots/workflow-example.git",
			Workflow: wf,
		},
	}
}

func exampleWorkflow() *criteriav1.RunWorkflow {
	return &criteriav1.RunWorkflow{
		Name:      "linear-intake-v1",
		Type:      "image",
		Namespace: "wf-jobs",
		Env:       map[string]string{"WORKFLOW_MODE": "image"},
		Volumes: []criteriav1.RunWorkflowVolume{
			{Name: "data", Kind: "pvc", MountPath: "/data", Claim: "wf-data",
				Env: map[string]string{"CRITERIA_RUN_DIR_ROOT": "/data/.criteria/runs"}},
			{Name: "repo", Kind: "pvc", MountPath: "/repo", Claim: "criteria-repo", SubPath: "runs", ReadOnly: true,
				Env: map[string]string{"WORKFLOW_REPO_ROOT": "/repo"}},
			{Name: "cache", Kind: "nfs", MountPath: "/mnt/cache", Server: "192.168.17.20", Path: "/exports/cache", ReadOnly: true},
			{Name: "scratch", Kind: "tmp", MountPath: "/tmp/scratch", SizeLimit: "8Gi",
				Env: map[string]string{"SCRATCH_DIR": "/tmp/scratch"}},
		},
		Secrets: []criteriav1.RunWorkflowSecret{
			{Name: "linear-api-key", SecretProviderClass: "linear-spc", MountPath: "/secrets",
				Env: map[string]string{"LINEAR_API_KEY": "linear_api_key"}},
			{Name: "github-tokens", SecretProviderClass: "copilot-spc", MountPath: "/home/criteria/secrets",
				Env: map[string]string{"WORKFLOW_GITHUB_TOKEN": "workflow_github_token", "REVIEWER_GITHUB_TOKEN": "reviewer_github_token"}},
		},
	}
}

func findVolume(t *testing.T, volumes []corev1.Volume, name string) corev1.Volume {
	t.Helper()
	for _, v := range volumes {
		if v.Name == name {
			return v
		}
	}
	t.Fatalf("volume %q not found", name)
	return corev1.Volume{}
}

func findMount(t *testing.T, mounts []corev1.VolumeMount, name string) corev1.VolumeMount {
	t.Helper()
	for _, m := range mounts {
		if m.Name == name {
			return m
		}
	}
	t.Fatalf("volume mount %q not found", name)
	return corev1.VolumeMount{}
}

func perScopePod(t *testing.T, run *criteriav1.CriteriaRun) *corev1.Pod {
	t.Helper()
	scope := events.LifecycleEvent{
		Event:       events.EventProvisionWanted,
		RunID:       run.Name,
		ScopeID:     "root",
		AdapterName: "intake",
		AdapterType: "shell",
		Digest:      "deadbeef",
	}
	return jobbuilder.BuildPerScopeAdapterPod(run, jobbuilder.Defaults{DataPVC: "criteria-data"}, scope, "10.0.0.10")
}

func TestWorkflowNamespaceTargetsEveryEnvironment(t *testing.T) {
	// The workflow object's namespace is the pod construction target: the
	// runner job, the adapter jobs, and the per-scope adapter pods are all
	// created there.
	run := workflowRun("cri-222-ns", exampleWorkflow())

	jobs := jobbuilder.BuildAll(run, jobbuilder.Defaults{DataPVC: "criteria-data"})
	require.Len(t, jobs, 3)
	for _, job := range jobs {
		assert.Equal(t, "wf-jobs", job.Namespace, "job %s must be created in the workflow's namespace", job.Name)
	}

	pod := perScopePod(t, run)
	assert.Equal(t, "wf-jobs", pod.Namespace)
}

func TestWorkflowNamespaceWithoutDeclarationFallsBackToRun(t *testing.T) {
	// A stamped workflow without a namespace declaration keeps the run's
	// own namespace, as do runs with no workflow object at all.
	wf := exampleWorkflow()
	wf.Namespace = ""
	run := workflowRun("cri-222-ns-fallback", wf)

	jobs := jobbuilder.BuildAll(run, jobbuilder.Defaults{})
	require.Len(t, jobs, 3)
	for _, job := range jobs {
		assert.Equal(t, "criteria-jobs", job.Namespace)
	}
	assert.Equal(t, "criteria-jobs", perScopePod(t, run).Namespace)

	noWorkflow := workflowRun("cri-222-no-wf", nil)
	for _, job := range jobbuilder.BuildAll(noWorkflow, jobbuilder.Defaults{}) {
		assert.Equal(t, "criteria-jobs", job.Namespace)
	}
}

func TestWorkflowVolumesMountIntoEveryEnvironment(t *testing.T) {
	// Volumes declared in the workflow object mount into every pod built
	// from it (runner job, adapter jobs, per-scope adapter pods) with their
	// declared kind, mount path, subPath, read-only flag, and size limit.
	run := workflowRun("cri-222-vol", exampleWorkflow())
	defaults := jobbuilder.Defaults{DataPVC: "criteria-data"}

	runner := jobbuilder.BuildRunnerJob(run, defaults)
	shell := jobbuilder.BuildAdapterJob(run, defaults, "shell")
	pod := perScopePod(t, run)

	wantVolumeNames := []string{"data", "repo", "cache", "scratch", "secret-linear-api-key", "secret-github-tokens", "scripts"}
	for _, env := range []struct {
		where   string
		volumes []corev1.Volume
	}{{"runner job", runner.Spec.Template.Spec.Volumes}, {"adapter job", shell.Spec.Template.Spec.Volumes}, {"per-scope pod", pod.Spec.Volumes}} {
		got := make([]string, 0, len(env.volumes))
		for _, v := range env.volumes {
			got = append(got, v.Name)
		}
		assert.ElementsMatch(t, wantVolumeNames, got, "%s: unexpected pod volume set", env.where)
	}

	// pvc: claim, subPath, read-only.
	for _, m := range []struct {
		where   string
		mounts  []corev1.VolumeMount
		volumes []corev1.Volume
	}{{"runner", runner.Spec.Template.Spec.Containers[0].VolumeMounts, runner.Spec.Template.Spec.Volumes},
		{"adapter job", shell.Spec.Template.Spec.Containers[0].VolumeMounts, shell.Spec.Template.Spec.Volumes},
		{"per-scope pod", pod.Spec.Containers[0].VolumeMounts, pod.Spec.Volumes}} {
		repoMount := findMount(t, m.mounts, "repo")
		assert.Equal(t, "/repo", repoMount.MountPath, "%s: repo mount path", m.where)
		assert.Equal(t, "runs", repoMount.SubPath, "%s: repo subPath", m.where)
		assert.True(t, repoMount.ReadOnly, "%s: repo read-only", m.where)

		cacheMount := findMount(t, m.mounts, "cache")
		assert.Equal(t, "/mnt/cache", cacheMount.MountPath, "%s: cache mount path", m.where)
		assert.True(t, cacheMount.ReadOnly, "%s: nfs read-only", m.where)

		scratchMount := findMount(t, m.mounts, "scratch")
		assert.Equal(t, "/tmp/scratch", scratchMount.MountPath, "%s: scratch mount path", m.where)

		cacheVolume := findVolume(t, m.volumes, "cache")
		require.NotNil(t, cacheVolume.NFS, "%s: nfs volume source", m.where)
		assert.Equal(t, "192.168.17.20", cacheVolume.NFS.Server)
		assert.Equal(t, "/exports/cache", cacheVolume.NFS.Path)

		scratchVolume := findVolume(t, m.volumes, "scratch")
		require.NotNil(t, scratchVolume.EmptyDir, "%s: tmp volume source", m.where)
		assert.True(t, scratchVolume.EmptyDir.SizeLimit.Equal(resource.MustParse("8Gi")), "%s: tmp size limit", m.where)

		repoVolume := findVolume(t, m.volumes, "repo")
		require.NotNil(t, repoVolume.PersistentVolumeClaim, "%s: pvc volume source", m.where)
		assert.Equal(t, "criteria-repo", repoVolume.PersistentVolumeClaim.ClaimName)
	}

	// The data declaration re-sourced the operator's default data volume:
	// every environment binds the workflow's claim under the stable "data"
	// name, with no duplicate /data mount.
	for _, env := range []struct {
		where   string
		volumes []corev1.Volume
		mounts  []corev1.VolumeMount
	}{{"runner", runner.Spec.Template.Spec.Volumes, runner.Spec.Template.Spec.Containers[0].VolumeMounts},
		{"adapter job", shell.Spec.Template.Spec.Volumes, shell.Spec.Template.Spec.Containers[0].VolumeMounts},
		{"per-scope pod", pod.Spec.Volumes, pod.Spec.Containers[0].VolumeMounts}} {
		dataVolume := findVolume(t, env.volumes, "data")
		require.NotNil(t, dataVolume.PersistentVolumeClaim, "%s: re-sourced data volume", env.where)
		assert.Equal(t, "wf-data", dataVolume.PersistentVolumeClaim.ClaimName)
		dataMounts := 0
		for _, m := range env.mounts {
			if m.MountPath == "/data" {
				dataMounts++
			}
		}
		assert.Equal(t, 1, dataMounts, "%s: exactly one /data mount", env.where)
	}

	// The repo-clone init container mounts the declared volumes too.
	clone := runner.Spec.Template.Spec.InitContainers[0]
	assert.Equal(t, "repo-clone", clone.Name)
	assert.ElementsMatch(t, []string{"data", "repo", "cache", "scratch", "secret-linear-api-key", "secret-github-tokens", "scripts"}, volumeMountNames(clone.VolumeMounts))
}

func TestWorkflowVolumeEnvsReachContainersThatMountThem(t *testing.T) {
	// A declared volume's env map is injected into the containers mounting
	// it. All workflow-built containers mount every declaration, so the
	// runner, the adapter jobs, the per-scope pods, and the clone init all
	// carry them; containers built for runs without the declarations do not.
	run := workflowRun("cri-222-env", exampleWorkflow())

	runner := jobbuilder.BuildRunnerJob(run, jobbuilder.Defaults{DataPVC: "criteria-data"})
	runnerEnv := runner.Spec.Template.Spec.Containers[0].Env
	assert.Equal(t, "/data/.criteria/runs", envValue(runnerEnv, "CRITERIA_RUN_DIR_ROOT"))
	assert.Equal(t, "/repo", envValue(runnerEnv, "WORKFLOW_REPO_ROOT"))
	assert.Equal(t, "/tmp/scratch", envValue(runnerEnv, "SCRATCH_DIR"))
	// The builder's own env contract is never displaced by a declaration.
	// CRITERIA_HOME stays on the shared /data PVC for the image-mode runner:
	// its frozen pre-eae0181 engine still publishes token files the per-scope
	// adapters read through the legacy delivery (CRI-237 mode-keyed scoping).
	assert.Equal(t, "/data/criteria", envValue(runnerEnv, "CRITERIA_HOME"))
}

func TestWorkflowVolumeEnvsReachAdapterEnvironments(t *testing.T) {
	run := workflowRun("cri-222-env", exampleWorkflow())
	defaults := jobbuilder.Defaults{DataPVC: "criteria-data"}

	shell := jobbuilder.BuildAdapterJob(run, defaults, "shell")
	shellEnv := shell.Spec.Template.Spec.Containers[0].Env
	assert.Equal(t, "/data/.criteria/runs", envValue(shellEnv, "CRITERIA_RUN_DIR_ROOT"))
	assert.Equal(t, "/repo", envValue(shellEnv, "WORKFLOW_REPO_ROOT"))
	assert.Equal(t, "/tmp/scratch", envValue(shellEnv, "SCRATCH_DIR"))
	// Builder env contract wins over workflow declarations.
	assert.Equal(t, "shell", envValue(shellEnv, "ADAPTER_KIND"))

	pod := perScopePod(t, run)
	podEnv := pod.Spec.Containers[0].Env
	assert.Equal(t, "/data/.criteria/runs", envValue(podEnv, "CRITERIA_RUN_DIR_ROOT"))
	assert.Equal(t, "/repo", envValue(podEnv, "WORKFLOW_REPO_ROOT"))

	clone := jobbuilder.BuildRunnerJob(run, defaults).Spec.Template.Spec.InitContainers[0]
	assert.Equal(t, "/data/.criteria/runs", envValue(clone.Env, "CRITERIA_RUN_DIR_ROOT"))
}

// KB-7: a k8s-secret volume declaration renders as a native Secret volume
// in every environment the workflow builds, mounts at the declared
// mountPath, anchors the volume env, and never triggers the PVC-only host
// affinity (a Secret is namespace-local, not node-local).
func TestWorkflowK8sSecretVolumeRendersNativeSecretVolume(t *testing.T) {
	wf := &criteriav1.RunWorkflow{
		Name:      "repo-less-scan-v1",
		Type:      "image",
		Namespace: "wf-jobs",
		Env:       map[string]string{"WORKFLOW_GITHUB_TOKEN": "/home/criteria/secrets/workflow_github_token"},
		Volumes: []criteriav1.RunWorkflowVolume{
			{Name: "tokens", Kind: "k8s-secret", SecretName: "github-tokens",
				MountPath: "/home/criteria/secrets", ReadOnly: true},
		},
	}
	run := workflowRun("kb-7-secret", wf)
	defaults := jobbuilder.Defaults{DataPVC: "criteria-data"}

	runner := jobbuilder.BuildRunnerJob(run, defaults)
	runnerSpec := runner.Spec.Template.Spec
	vol := findVolume(t, runnerSpec.Volumes, "tokens")
	require.NotNil(t, vol.Secret, "k8s-secret must render a native SecretVolumeSource, not a PVC")
	assert.Equal(t, "github-tokens", vol.Secret.SecretName)
	mount := findMount(t, runnerSpec.Containers[0].VolumeMounts, "tokens")
	assert.Equal(t, "/home/criteria/secrets", mount.MountPath)
	assert.True(t, mount.ReadOnly)
	assert.Equal(t, "/home/criteria/secrets/workflow_github_token",
		envValue(runnerSpec.Containers[0].Env, "WORKFLOW_GITHUB_TOKEN"))

	// Adapter jobs and per-scope pods carry the same native volume.
	adapter := jobbuilder.BuildAdapterJob(run, defaults, "shell")
	adapterSpec := adapter.Spec.Template.Spec
	assert.Equal(t, "github-tokens",
		findVolume(t, adapterSpec.Volumes, "tokens").Secret.SecretName)
	assert.Equal(t, "/home/criteria/secrets",
		findMount(t, adapterSpec.Containers[0].VolumeMounts, "tokens").MountPath)

	pod := perScopePod(t, run)
	assert.Equal(t, "github-tokens",
		findVolume(t, pod.Spec.Volumes, "tokens").Secret.SecretName)
	assert.Equal(t, "/home/criteria/secrets",
		findMount(t, pod.Spec.Containers[0].VolumeMounts, "tokens").MountPath)

	// The clone init container mounts the declaration too, so the clone
	// script can read the token files it serves (repo-bearing run here).
	clone := jobbuilder.BuildRunnerJob(run, defaults).Spec.Template.Spec.InitContainers[0]
	assert.Equal(t, "repo-clone", clone.Name)
	assert.Equal(t, "/home/criteria/secrets", findMount(t, clone.VolumeMounts, "tokens").MountPath)

	// Host affinity is PVC-only: the pod labels carry no affinity key and
	// the pod spec carries no nodeAffinity for the secret volume.
	for key := range runner.Spec.Template.Labels {
		assert.NotEqual(t, "affinity-tokens", strings.TrimPrefix(key, jobbuilder.LabelHostAffinityPrefix),
			"a k8s-secret volume must not become a host-affinity group")
	}
	assert.Nil(t, runnerSpec.Affinity, "a workflow with no PVC declaration carries no affinity")
}

func TestWorkflowWithoutVolumeDeclarationsMountsNone(t *testing.T) {
	// Environments built for a workflow that declares no volumes mount none:
	// only the environments a declaration covers receive it. The stamped
	// workflow object is the source of truth — a declaration of nothing
	// means nothing, and the runner fails loud if it needs an undeclared
	// secret.
	wf := &criteriav1.RunWorkflow{Name: "linear-intake-v1", Type: "image", Namespace: "wf-jobs"}
	run := workflowRun("cri-222-novol", wf)

	runner := jobbuilder.BuildRunnerJob(run, jobbuilder.Defaults{})
	shell := jobbuilder.BuildAdapterJob(run, jobbuilder.Defaults{}, "shell")
	pod := perScopePod(t, run)

	assert.ElementsMatch(t, []string{"data", "scripts"}, volumeMountNames(runner.Spec.Template.Spec.Containers[0].VolumeMounts))
	assert.ElementsMatch(t, []string{"data", "scripts"}, volumeMountNames(shell.Spec.Template.Spec.Containers[0].VolumeMounts))
	assert.ElementsMatch(t, []string{"data", "scripts"}, volumeMountNames(pod.Spec.Containers[0].VolumeMounts))

	// The run-state data volume keeps the operator's default claim.
	assert.Equal(t, "criteria-data", findVolume(t, runner.Spec.Template.Spec.Volumes, "data").PersistentVolumeClaim.ClaimName)
}

func TestWorkflowInvalidSizeLimitDoesNotPanic(t *testing.T) {
	// A sizeLimit is a free-form string at every boundary (the routes
	// schema only requires a non-empty string; the CRD carries no
	// pattern), so a non-quantity value can reach the builder. The
	// builder must neither panic — an unrecovered panic would crash-loop
	// the operator and stall every run — nor set a size limit it could
	// not parse: the tmp volume is built without a limit and still mounts
	// at its declared path.
	wf := &criteriav1.RunWorkflow{
		Name:      "linear-intake-v1",
		Type:      "image",
		Namespace: "wf-jobs",
		Volumes: []criteriav1.RunWorkflowVolume{
			{Name: "scratch", Kind: "tmp", MountPath: "/tmp/scratch", SizeLimit: "banana"},
		},
	}
	run := workflowRun("cri-222-bad-sizelimit", wf)
	defaults := jobbuilder.Defaults{DataPVC: "criteria-data"}

	require.NotPanics(t, func() {
		runner := jobbuilder.BuildRunnerJob(run, defaults)
		scratch := findVolume(t, runner.Spec.Template.Spec.Volumes, "scratch")
		require.NotNil(t, scratch.EmptyDir, "tmp volume source")
		assert.Nil(t, scratch.EmptyDir.SizeLimit, "an unparsable sizeLimit is omitted, not crashed on")
		assert.Equal(t, "/tmp/scratch", findMount(t, runner.Spec.Template.Spec.Containers[0].VolumeMounts, "scratch").MountPath)

		shell := jobbuilder.BuildAdapterJob(run, defaults, "shell")
		require.NotNil(t, findVolume(t, shell.Spec.Template.Spec.Volumes, "scratch").EmptyDir)

		pod := perScopePod(t, run)
		require.NotNil(t, findVolume(t, pod.Spec.Volumes, "scratch").EmptyDir)
	})
}

func TestWorkflowDataDeclarationIsNotDuplicated(t *testing.T) {
	// A workflow volume named other than "data" but mounted at /data still
	// re-sources the run-state volume; a workflow volume NAMED "data" but
	// mounted elsewhere must not shadow it (wf- prefix).
	run := workflowRun("cri-222-data", exampleWorkflow())
	runner := jobbuilder.BuildRunnerJob(run, jobbuilder.Defaults{})
	assert.Equal(t, "wf-data", findVolume(t, runner.Spec.Template.Spec.Volumes, "data").PersistentVolumeClaim.ClaimName)

	colliding := workflowRun("cri-222-collide", &criteriav1.RunWorkflow{
		Name:      "linear-intake-v1",
		Type:      "image",
		Namespace: "wf-jobs",
		Volumes: []criteriav1.RunWorkflowVolume{
			{Name: "data", Kind: "pvc", MountPath: "/other", Claim: "other-claim"},
			{Name: "scripts", Kind: "tmp", MountPath: "/tmp/other"},
		},
	})
	jobs := jobbuilder.BuildAll(colliding, jobbuilder.Defaults{DataPVC: "criteria-data"})
	for _, job := range jobs {
		volumeNames := make(map[string]bool)
		for _, v := range job.Spec.Template.Spec.Volumes {
			require.False(t, volumeNames[v.Name], "duplicate pod volume %q in job %s", v.Name, job.Name)
			volumeNames[v.Name] = true
		}
		assert.True(t, volumeNames["wf-data"], "job %s: colliding declaration renamed", job.Name)
		assert.True(t, volumeNames["wf-scripts"], "job %s: colliding declaration renamed", job.Name)
		assert.True(t, volumeNames["data"], "job %s: run-state data volume preserved", job.Name)
		assert.True(t, volumeNames["scripts"], "job %s: scripts volume preserved", job.Name)
	}
}

func TestWorkflowSecretsReachAdaptersViaCSINameReferences(t *testing.T) {
	// Declared secrets are delivered as OpenBao/CSI name references: one CSI
	// volume per declaration carrying the SecretProviderClass name, mounted
	// at the declared path, plus name-only path env entries — never secret
	// material, and never a copy of the credential into the spec.
	run := workflowRun("cri-222-secrets", exampleWorkflow())
	defaults := jobbuilder.Defaults{DataPVC: "criteria-data"}

	shell := jobbuilder.BuildAdapterJob(run, defaults, "shell")
	spec := shell.Spec.Template.Spec

	linearVolume := findVolume(t, spec.Volumes, "secret-linear-api-key")
	require.NotNil(t, linearVolume.CSI, "adapter secret volume must be a CSI volume")
	assert.Equal(t, "secrets-store.csi.k8s.io", linearVolume.CSI.Driver)
	assert.Equal(t, "linear-spc", linearVolume.CSI.VolumeAttributes["secretProviderClass"])
	require.NotNil(t, linearVolume.CSI.ReadOnly)
	assert.True(t, *linearVolume.CSI.ReadOnly)

	githubVolume := findVolume(t, spec.Volumes, "secret-github-tokens")
	require.NotNil(t, githubVolume.CSI)
	assert.Equal(t, "copilot-spc", githubVolume.CSI.VolumeAttributes["secretProviderClass"])

	linearMount := findMount(t, spec.Containers[0].VolumeMounts, "secret-linear-api-key")
	assert.Equal(t, "/secrets", linearMount.MountPath)
	assert.True(t, linearMount.ReadOnly)
	assert.Empty(t, linearMount.SubPath, "the whole SPC sync dir is mounted so every rendered key is present")

	assert.Equal(t, "criteria-runner", spec.ServiceAccountName,
		"the OpenBao provider authenticates the pod through its service account token")
	require.NotNil(t, spec.AutomountServiceAccountToken)
	assert.True(t, *spec.AutomountServiceAccountToken)

	// Name-only env: values are rendered file paths inside the CSI mounts.
	shellEnv := spec.Containers[0].Env
	assert.Equal(t, "/secrets/linear_api_key", envValue(shellEnv, "LINEAR_API_KEY"))
	assert.Equal(t, "/home/criteria/secrets/workflow_github_token", envValue(shellEnv, "WORKFLOW_GITHUB_TOKEN"))
	assert.Equal(t, "/home/criteria/secrets/reviewer_github_token", envValue(shellEnv, "REVIEWER_GITHUB_TOKEN"))
	for _, e := range shellEnv {
		assert.NotContains(t, e.Value, "op://", "env %s must not carry secret material", e.Name)
	}

	pod := perScopePod(t, run)
	podSpec := pod.Spec
	assert.Equal(t, "criteria-runner", podSpec.ServiceAccountName)
	require.NotNil(t, podSpec.AutomountServiceAccountToken)
	assert.True(t, *podSpec.AutomountServiceAccountToken)
	linearPodVolume := findVolume(t, podSpec.Volumes, "secret-linear-api-key")
	require.NotNil(t, linearPodVolume.CSI)
	assert.Equal(t, "linear-spc", linearPodVolume.CSI.VolumeAttributes["secretProviderClass"])
	podEnv := pod.Spec.Containers[0].Env
	assert.Equal(t, "/secrets/linear_api_key", envValue(podEnv, "LINEAR_API_KEY"))
	assert.Equal(t, "/home/criteria/secrets/workflow_github_token", envValue(podEnv, "WORKFLOW_GITHUB_TOKEN"))
}

func TestWorkflowSecretsReplaceBuiltinRunnerSecrets(t *testing.T) {
	// The workflow object's secret declarations are the source of truth for
	// the runner too: they replace the built-in linear-spc/copilot-spc
	// mounts. The runner consumes its credentials from the CSI-synced files,
	// so no path-valued secret env is injected into the runner container.
	run := workflowRun("cri-222-runner-secrets", exampleWorkflow())
	runner := jobbuilder.BuildRunnerJob(run, jobbuilder.Defaults{DataPVC: "criteria-data"})
	spec := runner.Spec.Template.Spec

	volumeNames := make(map[string]bool)
	for _, v := range spec.Volumes {
		volumeNames[v.Name] = true
	}
	assert.True(t, volumeNames["secret-linear-api-key"], "workflow-declared secret volume present")
	assert.True(t, volumeNames["secret-github-tokens"])
	assert.False(t, volumeNames["linear-secrets"], "built-in secret volume replaced by the workflow declaration")
	assert.False(t, volumeNames["copilot-secrets"])

	assert.Equal(t, "/secrets", findMount(t, spec.Containers[0].VolumeMounts, "secret-linear-api-key").MountPath)
	assert.Equal(t, "/home/criteria/secrets", findMount(t, spec.InitContainers[0].VolumeMounts, "secret-github-tokens").MountPath,
		"the repo-clone init container reads the GitHub token from the declared mount")

	runnerEnv := spec.Containers[0].Env
	for _, e := range runnerEnv {
		assert.NotEqual(t, "LINEAR_API_KEY", e.Name, "the runner must not carry path-valued secret env entries")
		assert.NotEqual(t, "WORKFLOW_GITHUB_TOKEN", e.Name)
	}
	// The workflow env map reaches the runner only.
	assert.Equal(t, "image", envValue(runnerEnv, "WORKFLOW_MODE"))
}

func TestWorkflowWithoutSecretDeclarationsKeepsAdaptersPrivilegeless(t *testing.T) {
	// Adapter environments carry CSI volumes and the service account only
	// when the workflow object declares secrets; runs without declarations
	// keep the zero-privilege adapter contract.
	wf := &criteriav1.RunWorkflow{Name: "linear-intake-v1", Type: "image", Namespace: "wf-jobs"}
	run := workflowRun("cri-222-nosecrets", wf)

	shell := jobbuilder.BuildAdapterJob(run, jobbuilder.Defaults{}, "shell")
	assert.Empty(t, shell.Spec.Template.Spec.ServiceAccountName)
	require.NotNil(t, shell.Spec.Template.Spec.AutomountServiceAccountToken)
	assert.False(t, *shell.Spec.Template.Spec.AutomountServiceAccountToken)
	for _, v := range shell.Spec.Template.Spec.Volumes {
		assert.Nil(t, v.CSI, "adapter volume %q must not be CSI without a declaration", v.Name)
	}

	pod := perScopePod(t, run)
	assert.Empty(t, pod.Spec.ServiceAccountName)
	require.NotNil(t, pod.Spec.AutomountServiceAccountToken)
	assert.False(t, *pod.Spec.AutomountServiceAccountToken)
	for _, v := range pod.Spec.Volumes {
		assert.Nil(t, v.CSI, "per-scope adapter volume %q must not be CSI without a declaration", v.Name)
	}
}

func TestWorkflowImageResolution(t *testing.T) {
	// Image mode only: an unset spec.image builds with the operator default
	// image; a set spec.image wins over the operator default and over the
	// workflow object's image field, which is not consumed by the jobbuilder.
	wf := exampleWorkflow()
	wf.Image = "localhost:5000/linear-intake-remote:from-workflow-object"

	run := workflowRun("cri-222-img-default", wf)
	runner := jobbuilder.BuildRunnerJob(run, jobbuilder.Defaults{Image: operatorDefaultImage})
	assert.Equal(t, operatorDefaultImage, runner.Spec.Template.Spec.Containers[0].Image)
	assert.Equal(t, operatorDefaultImage, runner.Spec.Template.Spec.InitContainers[0].Image)

	explicit := workflowRun("cri-222-img-explicit", wf)
	explicit.Spec.Image = "localhost:5000/linear-intake-remote:pinned"
	runner = jobbuilder.BuildRunnerJob(explicit, jobbuilder.Defaults{Image: operatorDefaultImage})
	assert.Equal(t, "localhost:5000/linear-intake-remote:pinned", runner.Spec.Template.Spec.Containers[0].Image,
		"spec.image wins over the operator default")
	assert.NotEqual(t, "localhost:5000/linear-intake-remote:from-workflow-object", runner.Spec.Template.Spec.Containers[0].Image,
		"the workflow object's image field is not consumed for pod construction")
}

func TestWorkflowURLAndRefAreNotConsumed(t *testing.T) {
	// M1.7 is image mode only: url/source modes arrive with CRI-231, so the
	// jobbuilder must build identical pods whether or not the stamped
	// workflow object carries url/ref fields.
	wfWithURL := exampleWorkflow()
	wfWithURL.URL = "https://example.invalid/workflow.tgz"
	wfWithURL.Ref = "sha256:deadbeef"

	plain := jobbuilder.BuildAll(workflowRun("cri-222-urlmode", exampleWorkflow()), jobbuilder.Defaults{DataPVC: "criteria-data"})
	withURL := jobbuilder.BuildAll(workflowRun("cri-222-urlmode", wfWithURL), jobbuilder.Defaults{DataPVC: "criteria-data"})

	require.Len(t, plain, len(withURL))
	for i := range plain {
		assert.True(t, reflect.DeepEqual(plain[i], withURL[i]),
			"job %s must be identical regardless of workflow url/ref", plain[i].Name)
	}
	assert.True(t, reflect.DeepEqual(perScopePod(t, workflowRun("cri-222-urlmode", exampleWorkflow())), perScopePod(t, workflowRun("cri-222-urlmode", wfWithURL))),
		"per-scope adapter pods must be identical regardless of workflow url/ref")
}

func TestWorkflowEnvReachesRunnerOnly(t *testing.T) {
	// The workflow object's own env map is workflow configuration: the
	// runner container receives it, adapter environments do not (their env
	// is the handshake contract).
	run := workflowRun("cri-222-wfenv", exampleWorkflow())
	shell := jobbuilder.BuildAdapterJob(run, jobbuilder.Defaults{}, "shell")
	assert.Empty(t, envValue(shell.Spec.Template.Spec.Containers[0].Env, "WORKFLOW_MODE"),
		"the workflow env map must not reach adapter containers")
	pod := perScopePod(t, run)
	assert.Empty(t, envValue(pod.Spec.Containers[0].Env, "WORKFLOW_MODE"))
}
