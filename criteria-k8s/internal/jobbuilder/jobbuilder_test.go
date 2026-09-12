package jobbuilder_test

import (
	"strings"
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
		if v.Name == "data" {
			require.NotNil(t, v.PersistentVolumeClaim)
			assert.Equal(t, "criteria-data", v.PersistentVolumeClaim.ClaimName)
		}
		if v.Name == "linear-secrets" {
			require.NotNil(t, v.CSI)
			assert.Equal(t, "secrets-store.csi.k8s.io", v.CSI.Driver)
		}
	}
	assert.False(t, volNames["repo"], "runner must not mount the shared repo PVC; it clones into /data/intake/<ticket>/repo")
	assert.True(t, volNames["data"])
	assert.True(t, volNames["linear-secrets"])
	assert.True(t, volNames["copilot-secrets"])
	assert.False(t, volNames["shell-secrets"], "runner pod must not mount shell-secrets")

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
	assert.NotContains(t, runner.Env, corev1.EnvVar{Name: "CASTLE_ADDR"},
		"without Defaults.CastleAddr the runner stays in local file mode")

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
	assert.Equal(t, "data", clone.VolumeMounts[0].Name)
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
	assert.Len(t, shell.Spec.Template.Spec.Volumes, 2)
	assert.Equal(t, "localhost:5000/criteria-adapter-shell:k8s-0.5.3", shell.Spec.Template.Spec.Containers[0].Image)

	copilot := findJob(t, jobs, "cri-42-adapter-copilot")
	assert.Equal(t, "adapter", copilot.Spec.Template.Labels["criteria.brokenbots.dev/role"])
	assert.Equal(t, "copilot", copilot.Spec.Template.Labels["criteria.brokenbots.dev/adapter-kind"])
	assert.Empty(t, copilot.Spec.Template.Spec.ServiceAccountName)
	require.NotNil(t, copilot.Spec.Template.Spec.AutomountServiceAccountToken)
	assert.False(t, *copilot.Spec.Template.Spec.AutomountServiceAccountToken)
	assert.Len(t, copilot.Spec.Template.Spec.Volumes, 2)
	assert.Equal(t, "localhost:5000/criteria-adapter-copilot:k8s-0.5.6", copilot.Spec.Template.Spec.Containers[0].Image)

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
		// Adapter containers must mount only data/scripts; the repo clone
		// lives on the data PVC at /data/intake/<ticket>/repo.
		for _, c := range job.Spec.Template.Spec.Containers {
			mountNames := make([]string, 0, len(c.VolumeMounts))
			for _, m := range c.VolumeMounts {
				mountNames = append(mountNames, m.Name)
			}
			assert.ElementsMatch(t, []string{"data", "scripts"}, mountNames)
		}
	}
}

func TestRunnerUsesPerTicketRepoClone(t *testing.T) {
	run := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{Name: "cri-99"},
		Spec: criteriav1.CriteriaRunSpec{
			TicketID: "CRI-99",
			RepoURL:  "https://github.com/brokenbots/workflow-example.git",
		},
	}

	jobs := jobbuilder.BuildAll(run, jobbuilder.Defaults{DataPVC: "criteria-data", RepoPVC: "criteria-repo"})
	require.Len(t, jobs, 3)

	// The runner's init container must clone into the per-ticket directory on
	// the data PVC so concurrent runs never clobber each other's git clone.
	runner := findJob(t, jobs, "cri-99")
	require.NotEmpty(t, runner.Spec.Template.Spec.InitContainers, "runner must have a repo-clone init container")
	init := runner.Spec.Template.Spec.InitContainers[0]
	assert.Equal(t, "/data/intake/CRI-99/repo", envValue(init.Env, "REPO_DIR"),
		"repo-clone must clone into the per-ticket repo path")
	script := strings.Join(init.Command, " ")
	assert.NotContains(t, script, "find /repo", "repo-clone must not wipe a shared /repo")
	assert.Contains(t, script, `gh repo clone "$REPO_URL" "$REPO_DIR"`,
		"repo-clone must clone into $REPO_DIR")

	// No job may mount the shared repo PVC; the runner and adapters reach the
	// clone through the data PVC.
	for _, job := range jobs {
		for i := range job.Spec.Template.Spec.Volumes {
			if job.Spec.Template.Spec.Volumes[i].Name == "repo" {
				t.Errorf("job %q still mounts the shared repo PVC; runs must use the per-ticket clone on the data PVC", job.Name)
			}
		}
	}

	// The runner must pass the per-ticket repo path to the workflow.
	for _, c := range runner.Spec.Template.Spec.Containers {
		assert.Equal(t, "/data/intake/CRI-99/repo", envValue(c.Env, "REPO_DIR"))
	}
}

func envValue(env []corev1.EnvVar, name string) string {
	for _, e := range env {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
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

// With Defaults.CastleAddr set, the runner receives CASTLE_ADDR so criteria
// publishes lifecycle to castle in server mode. When unset, the env var is
// absent entirely and the runner's local-mode behaviour is unchanged.
func TestBuildRunnerJobCastleAddr(t *testing.T) {
	run := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{Name: "cri-42"},
		Spec: criteriav1.CriteriaRunSpec{
			TicketID: "CRI-42",
			RepoURL:  "https://github.com/brokenbots/workflow-example.git",
		},
	}

	job := jobbuilder.BuildRunnerJob(run, jobbuilder.Defaults{DataPVC: "criteria-data", CastleAddr: "http://castle:9443"})
	runner := job.Spec.Template.Spec.Containers[0]
	assert.Contains(t, runner.Env, corev1.EnvVar{Name: "CASTLE_ADDR", Value: "http://castle:9443"})

	localJob := jobbuilder.BuildRunnerJob(run, jobbuilder.Defaults{DataPVC: "criteria-data"})
	localRunner := localJob.Spec.Template.Spec.Containers[0]
	assert.NotContains(t, localRunner.Env, corev1.EnvVar{Name: "CASTLE_ADDR"})
}

// CRI-136 retired the events.ndjson dual-write: by default the runner
// container must not receive EVENTS_FILE at all, so runner.sh passes no
// --events-file and the run writes no events.ndjson anywhere. Only an
// explicitly configured debug path injects the env var, keeping the flag
// available for debugging.
func TestBuildRunnerJobEventsFileDebugOnly(t *testing.T) {
	run := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{Name: "cri-42"},
		Spec: criteriav1.CriteriaRunSpec{
			TicketID: "CRI-42",
			RepoURL:  "https://github.com/brokenbots/workflow-example.git",
		},
	}

	defaultJob := jobbuilder.BuildRunnerJob(run, jobbuilder.Defaults{DataPVC: "criteria-data"})
	defaultRunner := defaultJob.Spec.Template.Spec.Containers[0]
	assert.NotContains(t, defaultRunner.Env, corev1.EnvVar{Name: "EVENTS_FILE"})

	debugJob := jobbuilder.BuildRunnerJob(run, jobbuilder.Defaults{
		DataPVC:         "criteria-data",
		DebugEventsFile: "/data/intake/CRI-42/debug-events.ndjson",
	})
	debugRunner := debugJob.Spec.Template.Spec.Containers[0]
	assert.Contains(t, debugRunner.Env, corev1.EnvVar{
		Name:  "EVENTS_FILE",
		Value: "/data/intake/CRI-42/debug-events.ndjson",
	})
}
