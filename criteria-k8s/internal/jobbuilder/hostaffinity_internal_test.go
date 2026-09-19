package jobbuilder

// CRI-235: applyHostAffinity defers to explicit host constraints — a pod
// already carrying an affinity or a nodeName keeps its scheduling intent,
// and no affinity labels are merged. The builders never pre-set either
// field today, so this contract is pinned directly on the plan API.

import (
	"testing"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func affinityGuardRun() *criteriav1.CriteriaRun {
	return &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{Name: "cri-235-guard", Namespace: "criteria-jobs"},
		Spec: criteriav1.CriteriaRunSpec{
			TicketID: "CRI-235",
			Workflow: &criteriav1.RunWorkflow{
				Volumes: []criteriav1.RunWorkflowVolume{
					{Name: "data", Kind: "pvc", MountPath: "/data", Claim: "wf-data"},
				},
			},
		},
	}
}

func TestApplyHostAffinityDefersToExistingAffinity(t *testing.T) {
	plan := newWorkflowPlan(affinityGuardRun())
	require.NotNil(t, plan)

	preexisting := &corev1.Affinity{
		PodAffinity: &corev1.PodAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
				{TopologyKey: "topology.kubernetes.io/zone"},
			},
		},
	}
	spec := &corev1.PodSpec{Affinity: preexisting}
	labels := map[string]string{"criteria.brokenbots.dev/role": "runner"}

	plan.applyHostAffinity(labels, spec)
	assert.Same(t, preexisting, spec.Affinity, "an existing affinity must be left untouched")
	assert.Equal(t, map[string]string{"criteria.brokenbots.dev/role": "runner"}, labels,
		"no affinity labels may be merged when an explicit affinity exists")
}

func TestApplyHostAffinityDefersToNodeName(t *testing.T) {
	plan := newWorkflowPlan(affinityGuardRun())
	require.NotNil(t, plan)

	spec := &corev1.PodSpec{NodeName: "host-a"}
	labels := map[string]string{"criteria.brokenbots.dev/role": "runner"}

	plan.applyHostAffinity(labels, spec)
	assert.Empty(t, spec.Affinity, "a pinned nodeName must not gain affinity")
	assert.Equal(t, map[string]string{"criteria.brokenbots.dev/role": "runner"}, labels,
		"no affinity labels may be merged when the pod is node-pinned")
}

func TestApplyHostAffinityNilPlanIsNoop(t *testing.T) {
	var plan *workflowPlan

	spec := &corev1.PodSpec{}
	labels := map[string]string{"criteria.brokenbots.dev/role": "runner"}
	plan.applyHostAffinity(labels, spec)
	assert.Nil(t, spec.Affinity)
	assert.Equal(t, map[string]string{"criteria.brokenbots.dev/role": "runner"}, labels)
}
