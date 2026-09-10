package jobbuilder_test

import (
	"testing"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/jobbuilder"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestBuildAllPodSecurity(t *testing.T) {
	run := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{Name: "cri-113"},
		Spec: criteriav1.CriteriaRunSpec{
			TicketID: "CRI-113",
			RepoURL:  "https://github.com/brokenbots/workflow-example.git",
			Image:    "localhost:5000/linear-intake-remote:dev",
		},
	}

	jobs := jobbuilder.BuildAll(run, jobbuilder.Defaults{})
	require.Len(t, jobs, 3)

	for _, c := range jobs[0].Spec.Template.Spec.InitContainers {
		require.NotNil(t, c.SecurityContext, "container %q must have a non-nil SecurityContext", c.Name)
		require.NotNil(t, c.SecurityContext.AllowPrivilegeEscalation)
		assert.False(t, *c.SecurityContext.AllowPrivilegeEscalation, "container %q must not allow privilege escalation", c.Name)
		require.NotNil(t, c.SecurityContext.Capabilities)
		assert.Equal(t, []corev1.Capability{"ALL"}, c.SecurityContext.Capabilities.Drop, "container %q must drop ALL capabilities", c.Name)
	}
	for _, c := range jobs[0].Spec.Template.Spec.Containers {
		require.NotNil(t, c.SecurityContext, "container %q must have a non-nil SecurityContext", c.Name)
		require.NotNil(t, c.SecurityContext.AllowPrivilegeEscalation)
		assert.False(t, *c.SecurityContext.AllowPrivilegeEscalation, "container %q must not allow privilege escalation", c.Name)
		require.NotNil(t, c.SecurityContext.Capabilities)
		assert.Equal(t, []corev1.Capability{"ALL"}, c.SecurityContext.Capabilities.Drop, "container %q must drop ALL capabilities", c.Name)
	}

	for _, kind := range []string{"shell", "copilot"} {
		job := findJob(t, jobs, "cri-113-adapter-"+kind)
		c := job.Spec.Template.Spec.Containers[0]
		assert.Equal(t, "adapter-"+kind, c.Name)
		sc := c.SecurityContext
		require.NotNil(t, sc)
		require.NotNil(t, sc.AllowPrivilegeEscalation)
		assert.False(t, *sc.AllowPrivilegeEscalation)
		require.NotNil(t, sc.Capabilities)
		assert.Equal(t, []corev1.Capability{"ALL"}, sc.Capabilities.Drop)
	}
}

func TestBuildRunnerJob(t *testing.T) {
	run := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{Name: "cri-42"},
		Spec: criteriav1.CriteriaRunSpec{
			TicketID:        "CRI-42",
			RepoURL:         "https://github.com/brokenbots/workflow-example.git",
			Image:           "localhost:5000/linear-intake-remote:dev",
			BuildCmd:        "make build",
			TestCmd:         "make test",
			CIGateCmd:       "make ci-gate",
			MaxAgentVisits:  3,
			ProviderBaseURL: "http://provider/v1",
		},
	}

	job := jobbuilder.BuildRunnerJob(run, jobbuilder.Defaults{
		Image:           "default-image:dev",
		DataPVC:         "criteria-data",
		RepoPVC:         "criteria-repo",
		ProviderBaseURL: "http://default-provider/v1",
	})

	require.NotNil(t, job)
	assert.Contains(t, job.Name, "cri-42")
	assert.Equal(t, run.Namespace, job.Namespace)
	require.Len(t, job.OwnerReferences, 1)
	assert.Equal(t, "CriteriaRun", job.OwnerReferences[0].Kind)

	// Verify scheduling constraints.
	assert.Equal(t, "amd64", job.Spec.Template.Spec.NodeSelector["kubernetes.io/arch"])
	require.Len(t, job.Spec.Template.Spec.Tolerations, 1)
	assert.Equal(t, "catch", job.Spec.Template.Spec.Tolerations[0].Key)
	assert.Equal(t, corev1.TolerationOpExists, job.Spec.Template.Spec.Tolerations[0].Operator)

	// Verify security context.
	sc := job.Spec.Template.Spec.SecurityContext
	require.NotNil(t, sc)
	assert.True(t, *sc.RunAsNonRoot)
	assert.NotNil(t, sc.SeccompProfile)
	assert.Equal(t, corev1.SeccompProfileTypeRuntimeDefault, sc.SeccompProfile.Type)

	// Verify only the runner pod uses the criteria-runner service account.
	assert.Equal(t, "criteria-runner", job.Spec.Template.Spec.ServiceAccountName)

	// Verify volumes.
	volNames := make(map[string]bool)
	for _, v := range job.Spec.Template.Spec.Volumes {
		volNames[v.Name] = true
		if v.Name == "repo" {
			require.NotNil(t, v.PersistentVolumeClaim)
			assert.Equal(t, "criteria-repo", v.PersistentVolumeClaim.ClaimName)
		}
		if v.Name == "data" {
			require.NotNil(t, v.PersistentVolumeClaim)
			assert.Equal(t, "criteria-data", v.PersistentVolumeClaim.ClaimName)
		}
		if v.Name == "linear-secrets" {
			require.NotNil(t, v.CSI)
			assert.Equal(t, "secrets-store.csi.k8s.io", v.CSI.Driver)
		}
	}
	assert.True(t, volNames["repo"])
	assert.True(t, volNames["data"])
	assert.True(t, volNames["linear-secrets"])
	assert.True(t, volNames["copilot-secrets"])
	assert.False(t, volNames["shell-secrets"], "runner pod must not mount shell-secrets")

	// /repo must be a shared PVC, not a per-pod emptyDir.
	assert.Nil(t, job.Spec.Template.Spec.Volumes[1].EmptyDir, "repo volume must not be emptyDir")

	// Runner pod must have exactly one init and one container.
	require.Len(t, job.Spec.Template.Spec.InitContainers, 1)
	require.Len(t, job.Spec.Template.Spec.Containers, 1)
	assert.Equal(t, "repo-clone", job.Spec.Template.Spec.InitContainers[0].Name)
	assert.Equal(t, "workflow-runner", job.Spec.Template.Spec.Containers[0].Name)

	// Verify no credential environment variables are injected.
	for _, c := range job.Spec.Template.Spec.InitContainers {
		for _, e := range c.Env {
			assert.NotContains(t, e.Name, "TOKEN")
			assert.NotContains(t, e.Name, "SECRET")
		}
	}
	for _, c := range job.Spec.Template.Spec.Containers {
		for _, e := range c.Env {
			assert.NotContains(t, e.Name, "TOKEN")
			assert.NotContains(t, e.Name, "SECRET")
		}
	}

	runner := job.Spec.Template.Spec.Containers[0]
	assert.Equal(t, "localhost:5000/linear-intake-remote:dev", runner.Image)
	assert.Contains(t, runner.Env, corev1.EnvVar{Name: "PROVIDER_BASE_URL", Value: "http://provider/v1"})
	assert.Contains(t, runner.Env, corev1.EnvVar{Name: "JOB_NAME", Value: job.Name})

	var podIP corev1.EnvVar
	for _, e := range runner.Env {
		if e.Name == "POD_IP" {
			podIP = e
		}
	}
	require.NotNil(t, podIP.ValueFrom)
	require.NotNil(t, podIP.ValueFrom.FieldRef)
	assert.Equal(t, "status.podIP", podIP.ValueFrom.FieldRef.FieldPath)

	// repo-clone must mount the workflow token from the copilot-spc, not shell-spc.
	clone := job.Spec.Template.Spec.InitContainers[0]
	require.Len(t, clone.VolumeMounts, 2)
	assert.Equal(t, "repo", clone.VolumeMounts[0].Name)
	assert.Equal(t, "copilot-secrets", clone.VolumeMounts[1].Name)
}

func TestBuildAll(t *testing.T) {
	run := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{Name: "cri-42"},
		Spec: criteriav1.CriteriaRunSpec{
			TicketID: "CRI-42",
			RepoURL:  "https://github.com/brokenbots/workflow-example.git",
		},
	}

	jobs := jobbuilder.BuildAll(run, jobbuilder.Defaults{DataPVC: "criteria-data"})
	require.Len(t, jobs, 3)

	runner := findJob(t, jobs, "cri-42")
	assert.Equal(t, "runner", runner.Spec.Template.Labels["criteria.brokenbots.dev/role"])

	shell := findJob(t, jobs, "cri-42-adapter-shell")
	assert.Equal(t, "adapter", shell.Spec.Template.Labels["criteria.brokenbots.dev/role"])
	assert.Equal(t, "shell", shell.Spec.Template.Labels["criteria.brokenbots.dev/adapter-kind"])
	assert.Empty(t, shell.Spec.Template.Spec.ServiceAccountName)
	require.NotNil(t, shell.Spec.Template.Spec.AutomountServiceAccountToken)
	assert.False(t, *shell.Spec.Template.Spec.AutomountServiceAccountToken)
	assert.Len(t, shell.Spec.Template.Spec.Volumes, 3)
	assert.Equal(t, "localhost:5000/criteria-adapter-shell:0.5.3", shell.Spec.Template.Spec.Containers[0].Image)

	copilot := findJob(t, jobs, "cri-42-adapter-copilot")
	assert.Equal(t, "adapter", copilot.Spec.Template.Labels["criteria.brokenbots.dev/role"])
	assert.Equal(t, "copilot", copilot.Spec.Template.Labels["criteria.brokenbots.dev/adapter-kind"])
	assert.Empty(t, copilot.Spec.Template.Spec.ServiceAccountName)
	require.NotNil(t, copilot.Spec.Template.Spec.AutomountServiceAccountToken)
	assert.False(t, *copilot.Spec.Template.Spec.AutomountServiceAccountToken)
	assert.Len(t, copilot.Spec.Template.Spec.Volumes, 3)
	assert.Equal(t, "localhost:5000/criteria-adapter-copilot:0.5.6", copilot.Spec.Template.Spec.Containers[0].Image)

	for _, job := range jobs[1:] {
		// Adapter pods must have no CSI volumes.
		for _, v := range job.Spec.Template.Spec.Volumes {
			assert.Nil(t, v.CSI, "adapter job volume %q must not be a CSI volume", v.Name)
		}
		// Adapter containers must have no credential env vars.
		for _, c := range job.Spec.Template.Spec.Containers {
			for _, e := range c.Env {
				assert.NotContains(t, e.Name, "TOKEN")
				assert.NotContains(t, e.Name, "SECRET")
			}
		}
		// Adapter containers must mount only data/repo/scripts.
		for _, c := range job.Spec.Template.Spec.Containers {
			mountNames := make([]string, 0, len(c.VolumeMounts))
			for _, m := range c.VolumeMounts {
				mountNames = append(mountNames, m.Name)
			}
			assert.ElementsMatch(t, []string{"data", "repo", "scripts"}, mountNames)
		}
	}
}

func TestRepoPVCSharedAcrossJobs(t *testing.T) {
	run := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{Name: "cri-99"},
		Spec: criteriav1.CriteriaRunSpec{
			TicketID: "CRI-99",
			RepoURL:  "https://github.com/brokenbots/workflow-example.git",
		},
	}

	jobs := jobbuilder.BuildAll(run, jobbuilder.Defaults{DataPVC: "criteria-data", RepoPVC: "criteria-repo"})
	require.Len(t, jobs, 3)

	var claimName string
	for _, job := range jobs {
		var repo *corev1.Volume
		for i := range job.Spec.Template.Spec.Volumes {
			if job.Spec.Template.Spec.Volumes[i].Name == "repo" {
				repo = &job.Spec.Template.Spec.Volumes[i]
				break
			}
		}
		require.NotNil(t, repo, "job %q must have a repo volume", job.Name)
		require.NotNil(t, repo.PersistentVolumeClaim, "job %q repo volume must be a PersistentVolumeClaim", job.Name)
		assert.Nil(t, repo.EmptyDir, "job %q repo volume must not be emptyDir", job.Name)
		if claimName == "" {
			claimName = repo.PersistentVolumeClaim.ClaimName
		} else {
			assert.Equal(t, claimName, repo.PersistentVolumeClaim.ClaimName, "all Jobs must share the same repo PVC")
		}
	}
	assert.Equal(t, "criteria-repo", claimName)
}

func findJob(t *testing.T, jobs []*batchv1.Job, name string) *batchv1.Job {
	t.Helper()
	for _, j := range jobs {
		if j.Name == name {
			return j
		}
	}
	t.Fatalf("job %q not found", name)
	return nil
}
