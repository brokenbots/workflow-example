package jobbuilder_test

import (
	"strings"
	"testing"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/events"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/jobbuilder"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPerScopeAdapterPodNameIsDNSSafeAndShort(t *testing.T) {
	run := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{Name: "cri-116"},
		Spec:       criteriav1.CriteriaRunSpec{TicketID: "CRI-116"},
	}
	name := jobbuilder.PerScopeAdapterPodName(run, "copilot", "subworkflow/name:uuid-1234")
	assert.LessOrEqual(t, len(name), 63)
	assert.Regexp(t, `^[a-z0-9-]+$`, name)
	assert.Contains(t, name, "copilot")
}

func TestBuildPerScopeAdapterPod(t *testing.T) {
	run := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{Name: "cri-116", Namespace: "criteria-jobs", UID: "run-uid"},
		Spec:       criteriav1.CriteriaRunSpec{TicketID: "CRI-116"},
	}
	scope := events.LifecycleEvent{
		Event:       events.EventProvisionWanted,
		RunID:       "CRI-116",
		ScopeID:     "root",
		ScopeTag:    "root-scope",
		AdapterName: "shell",
		Digest:      "deadbeef",
		ShimAddress: "10.0.0.5:7778",
		TokenFile:   "/data/intake/CRI-116/tokens/root-shell",
	}

	pod := jobbuilder.BuildPerScopeAdapterPod(run, jobbuilder.Defaults{DataPVC: "criteria-data", RepoPVC: "criteria-repo"}, scope)
	require.NotNil(t, pod)

	assert.Equal(t, run.Namespace, pod.Namespace)
	require.Len(t, pod.OwnerReferences, 1)
	assert.Equal(t, "CriteriaRun", pod.OwnerReferences[0].Kind)

	assert.Equal(t, "adapter", pod.Labels["criteria.brokenbots.dev/role"])
	assert.Equal(t, "shell", pod.Labels["criteria.brokenbots.dev/adapter-kind"])

	assert.Empty(t, pod.Spec.ServiceAccountName)
	require.NotNil(t, pod.Spec.AutomountServiceAccountToken)
	assert.False(t, *pod.Spec.AutomountServiceAccountToken)

	// No CSI volumes, no credential env vars.
	for _, v := range pod.Spec.Volumes {
		assert.Nil(t, v.CSI, "per-scope adapter volume %q must not be CSI", v.Name)
	}
	container := pod.Spec.Containers[0]
	envNames := make(map[string]string)
	for _, e := range container.Env {
		envNames[e.Name] = e.Value
	}
	assert.Equal(t, "10.0.0.5:7778", envNames["CRITERIA_REMOTE_HOST"])
	assert.Equal(t, "sha256:deadbeef", envNames["CRITERIA_REMOTE_DIGEST"])
	assert.Equal(t, "root", envNames["CRITERIA_SCOPE_ID"])
	assert.Equal(t, "root-scope", envNames["CRITERIA_SCOPE_TAG"])
	assert.Equal(t, "/data/intake/CRI-116/tokens/root-shell", envNames["CRITERIA_REMOTE_TOKEN_FILE"])

	assert.ElementsMatch(t, []string{"data", "scripts"}, volumeMountNames(container.VolumeMounts))

	assert.Equal(t, "amd64", pod.Spec.NodeSelector["kubernetes.io/arch"])
	require.Len(t, pod.Spec.Tolerations, 1)
	assert.Equal(t, "catch", pod.Spec.Tolerations[0].Key)
}

func TestBuildAllPerScopeSessionsOmitsAdapterJobs(t *testing.T) {
	run := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{Name: "cri-116"},
		Spec: criteriav1.CriteriaRunSpec{
			TicketID:         "CRI-116",
			RepoURL:          "https://github.com/brokenbots/workflow-example.git",
			PerScopeSessions: true,
		},
	}

	jobs := jobbuilder.BuildAll(run, jobbuilder.Defaults{DataPVC: "criteria-data"})
	require.Len(t, jobs, 1)
	assert.Equal(t, "cri-116", jobs[0].Name)
	assert.Equal(t, "runner", jobs[0].Spec.Template.Labels["criteria.brokenbots.dev/role"])
}

func TestBuildAllWithoutPerScopeSessionsKeepsAdapterJobs(t *testing.T) {
	run := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{Name: "cri-116"},
		Spec: criteriav1.CriteriaRunSpec{
			TicketID: "CRI-116",
			RepoURL:  "https://github.com/brokenbots/workflow-example.git",
		},
	}

	jobs := jobbuilder.BuildAll(run, jobbuilder.Defaults{DataPVC: "criteria-data"})
	require.Len(t, jobs, 3)
}

func TestPerScopeDigestPrefixAdded(t *testing.T) {
	run := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{Name: "cri-116", Namespace: "default"},
		Spec:       criteriav1.CriteriaRunSpec{TicketID: "CRI-116"},
	}
	scope := events.LifecycleEvent{Event: events.EventProvisionWanted, AdapterName: "shell", ScopeID: "root", Digest: "deadbeef"}
	pod := jobbuilder.BuildPerScopeAdapterPod(run, jobbuilder.Defaults{}, scope)
	env := findEnv(t, pod.Spec.Containers[0].Env, "CRITERIA_REMOTE_DIGEST")
	assert.Equal(t, "sha256:deadbeef", env.Value)
}

func TestPerScopeDigestPrefixPreserved(t *testing.T) {
	run := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{Name: "cri-116", Namespace: "default"},
		Spec:       criteriav1.CriteriaRunSpec{TicketID: "CRI-116"},
	}
	scope := events.LifecycleEvent{Event: events.EventProvisionWanted, AdapterName: "shell", ScopeID: "root", Digest: "sha256:deadbeef"}
	pod := jobbuilder.BuildPerScopeAdapterPod(run, jobbuilder.Defaults{}, scope)
	env := findEnv(t, pod.Spec.Containers[0].Env, "CRITERIA_REMOTE_DIGEST")
	assert.Equal(t, "sha256:deadbeef", env.Value)
}

func volumeMountNames(mounts []corev1.VolumeMount) []string {
	out := make([]string, len(mounts))
	for i, m := range mounts {
		out[i] = m.Name
	}
	return out
}

func findEnv(t *testing.T, envs []corev1.EnvVar, name string) corev1.EnvVar {
	t.Helper()
	for _, e := range envs {
		if e.Name == name {
			return e
		}
	}
	t.Fatalf("env var %q not found", name)
	return corev1.EnvVar{}
}

func TestPerScopeAdapterPodNameAvoidsCollisions(t *testing.T) {
	run := &criteriav1.CriteriaRun{ObjectMeta: metav1.ObjectMeta{Name: "cri-116"}, Spec: criteriav1.CriteriaRunSpec{TicketID: "CRI-116"}}
	names := make(map[string]bool)
	for i := 0; i < 100; i++ {
		name := jobbuilder.PerScopeAdapterPodName(run, "shell", "scope-"+strings.Repeat("x", i))
		require.LessOrEqual(t, len(name), 63)
		assert.False(t, names[name], "duplicate pod name %q", name)
		names[name] = true
	}
}
