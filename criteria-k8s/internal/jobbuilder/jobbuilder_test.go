package jobbuilder_test

import (
	"testing"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/jobbuilder"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

func TestBuildJob(t *testing.T) {
	run := &criteriav1.CriteriaRun{
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

	job := jobbuilder.Build(run, jobbuilder.Defaults{
		Image:           "default-image:dev",
		DataPVC:         "criteria-data",
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

	// Verify volumes.
	volNames := make(map[string]bool)
	for _, v := range job.Spec.Template.Spec.Volumes {
		volNames[v.Name] = true
		if v.Name == "repo" {
			assert.NotNil(t, v.EmptyDir)
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
	assert.True(t, volNames["shell-secrets"])

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
		if c.Name == "workflow-runner" {
			assert.Equal(t, "localhost:5000/linear-intake-remote:dev", c.Image)
			assert.Contains(t, c.Env, corev1.EnvVar{Name: "PROVIDER_BASE_URL", Value: "http://provider/v1"})
		}
		if c.Name == "adapter-copilot" || c.Name == "adapter-shell" {
			assert.Contains(t, c.Command, "/opt/criteria-pod-adapter/sidecar.sh")
		}
	}
}
