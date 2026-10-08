package controller

// CRI-234: the (scope, environment) GROUP pods — retired from provisioning
// by KB-214 (peer pods) but still alive in the mixed fleet until the
// sweeper's eviction pass removes existing ones — carry the same run/role
// labels and CriteriaRun ownerReferences as the per-adapter fallback pods.
// The sweeper's attribution rules must treat them identically: orphaned
// group-shaped pods are swept, labeled-but-unowned group-shaped pods of
// live runs are adopted, and label-only group-shaped pods of missing runs
// are never touched.

import (
	"context"
	"testing"

	"github.com/brokenbots/workflow-example/criteria-k8s/internal/jobbuilder"
	logr "github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// adapterGroupPod builds the RETIRED CRI-234 group shape (one container per
// adapter member): mixed-fleet eviction and attribution still apply to it.
func adapterGroupPod(name, namespace, runName string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				jobbuilder.LabelRun:         runName,
				jobbuilder.LabelRole:        jobbuilder.RoleAdapter,
				jobbuilder.LabelScopeID:     "scope-a",
				jobbuilder.LabelEnvironment: "ci",
			},
			Annotations: map[string]string{
				jobbuilder.AnnotationAdapterKinds: "copilot,shell",
			},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "adapter-shell", Image: "adp:1"},
			{Name: "adapter-copilot", Image: "adp:1"},
		}},
	}
}

// An orphaned group-shaped pod — ownerReference to a deleted CriteriaRun
// — is swept exactly like a fallback pod.
func TestAdapterSweeperDeletesOrphanGroupPodByOwnerRef(t *testing.T) {
	pod := adapterGroupPod("cri-234-adp-aa11bb22cc33-44556677889a", "default", "")
	withCriteriaRunOwnerRef(pod, "cri-234", "uid-234", true)
	cl := newSweepClient(t, pod)
	s := &AdapterSweeper{Client: cl, Scheme: cl.Scheme(), Namespace: "default"}

	s.sweep(context.Background(), logr.Discard())

	_, err := getSweptPod(t, cl, "default", "cri-234-adp-aa11bb22cc33-44556677889a")
	require.True(t, apierrors.IsNotFound(err), "orphaned group pod must be swept")
}

// A labeled-but-unowned group-shaped pod of a live run is adopted so GC
// reaps it when the CriteriaRun is deleted.
func TestAdapterSweeperAdoptsUnownedGroupPodOfLiveRun(t *testing.T) {
	run := newSweepTestRun("cri-234", "run-uid-234")
	pod := adapterGroupPod("cri-234-adp-aa11bb22cc33-44556677889a", "default", "cri-234")
	cl := newSweepClient(t, run, pod)
	s := &AdapterSweeper{Client: cl, Scheme: cl.Scheme(), Namespace: "default"}

	s.sweep(context.Background(), logr.Discard())

	got, err := getSweptPod(t, cl, "default", "cri-234-adp-aa11bb22cc33-44556677889a")
	require.NoError(t, err, "group pod of a live run must not be swept")
	require.Len(t, got.OwnerReferences, 1)
	assert.Equal(t, types.UID("run-uid-234"), got.OwnerReferences[0].UID,
		"unowned group pod of a live run must be adopted")
}

// A label-only group-shaped pod (manually launched run, no CriteriaRun) is
// never swept.
func TestAdapterSweeperKeepsLabelOnlyGroupPodOfMissingRun(t *testing.T) {
	pod := adapterGroupPod("manual-adp-aa11bb22cc33-44556677889a", "default", "manual-job")
	cl := newSweepClient(t, pod)
	s := &AdapterSweeper{Client: cl, Scheme: cl.Scheme(), Namespace: "default"}

	s.sweep(context.Background(), logr.Discard())

	_, err := getSweptPod(t, cl, "default", "manual-adp-aa11bb22cc33-44556677889a")
	require.NoError(t, err, "label-only group pod must never be swept")
}
