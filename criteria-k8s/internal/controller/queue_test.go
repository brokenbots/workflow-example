package controller_test

import (
	"context"
	"testing"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/controller"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestRunQueueRecover(t *testing.T) {
	scheme := newScheme(t)
	repoA := "https://github.com/org/repo-a.git"
	repoB := "https://github.com/org/repo-b.git"
	now := metav1.Now()

	runs := []criteriav1.CriteriaRun{
		// Deleting runs should not be recovered.
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "repo-a-deleting",
				Namespace:         "ns",
				DeletionTimestamp: &now,
				Finalizers:        []string{"criteriarun.criteria.brokenbots.dev/finalizer"},
			},
			Spec: criteriav1.CriteriaRunSpec{RepoURL: repoA},
		},
		// Terminal runs should not be recovered.
		{
			ObjectMeta: metav1.ObjectMeta{Name: "repo-a-terminal", Namespace: "ns"},
			Spec:       criteriav1.CriteriaRunSpec{RepoURL: repoA},
			Status:     criteriav1.CriteriaRunStatus{Phase: criteriav1.PhaseSucceeded},
		},
		// Non-terminal repo A runs, in order.
		{
			ObjectMeta: metav1.ObjectMeta{Name: "repo-a-pending-1", Namespace: "ns"},
			Spec:       criteriav1.CriteriaRunSpec{RepoURL: repoA},
			Status:     criteriav1.CriteriaRunStatus{Phase: criteriav1.PhasePending},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "repo-a-pending-2", Namespace: "ns"},
			Spec:       criteriav1.CriteriaRunSpec{RepoURL: repoA},
			Status:     criteriav1.CriteriaRunStatus{Phase: criteriav1.PhasePending},
		},
		// Terminal repo B run should not be recovered.
		{
			ObjectMeta: metav1.ObjectMeta{Name: "repo-b-terminal", Namespace: "ns"},
			Spec:       criteriav1.CriteriaRunSpec{RepoURL: repoB},
			Status:     criteriav1.CriteriaRunStatus{Phase: criteriav1.PhaseFailed},
		},
		// Non-terminal repo B run.
		{
			ObjectMeta: metav1.ObjectMeta{Name: "repo-b-pending-1", Namespace: "ns"},
			Spec:       criteriav1.CriteriaRunSpec{RepoURL: repoB},
			Status:     criteriav1.CriteriaRunStatus{Phase: criteriav1.PhasePending},
		},
	}

	objects := make([]client.Object, len(runs))
	for i := range runs {
		objects[i] = &runs[i]
	}

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&criteriav1.CriteriaRun{}).
		WithObjects(objects...).
		Build()

	queue := controller.NewRunQueue()
	require.NoError(t, queue.Recover(context.Background(), cl))

	headA := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{Name: "repo-a-pending-1", Namespace: "ns"},
		Spec:       criteriav1.CriteriaRunSpec{RepoURL: repoA},
	}
	admitted, status, _ := queue.Enqueue(headA)
	assert.True(t, admitted, "the head of repo A should be admitted")
	assert.Equal(t, "repo-a-pending-1", status.Running)
	assert.Equal(t, 0, status.Position)
	assert.Equal(t, 2, status.Length)
	assert.Equal(t, []string{"repo-a-pending-2"}, status.Pending)

	tailA := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{Name: "repo-a-pending-2", Namespace: "ns"},
		Spec:       criteriav1.CriteriaRunSpec{RepoURL: repoA},
	}
	admitted, status, _ = queue.Enqueue(tailA)
	assert.False(t, admitted, "the second repo A run should be pending")
	assert.Equal(t, 1, status.Position)
	assert.Equal(t, 2, status.Length)

	headB := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{Name: "repo-b-pending-1", Namespace: "ns"},
		Spec:       criteriav1.CriteriaRunSpec{RepoURL: repoB},
	}
	admitted, status, _ = queue.Enqueue(headB)
	assert.True(t, admitted, "the head of repo B should be admitted")
	assert.Equal(t, "repo-b-pending-1", status.Running)
	assert.Equal(t, 1, status.Length)

	// Recovering again must not duplicate entries.
	require.NoError(t, queue.Recover(context.Background(), cl))
	_, status, _ = queue.Enqueue(tailA)
	assert.Equal(t, 2, status.Length, "duplicate recovery entries should not increase the queue length")

	// Terminal and deleting runs were skipped, so adding a new repo A run
	// should land behind exactly the two recovered pending runs.
	newA := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{Name: "repo-a-new", Namespace: "ns"},
		Spec:       criteriav1.CriteriaRunSpec{RepoURL: repoA},
	}
	admitted, status, _ = queue.Enqueue(newA)
	assert.False(t, admitted)
	assert.Equal(t, 3, status.Length)
	assert.Equal(t, []string{"repo-a-pending-2", "repo-a-new"}, status.Pending)
}
