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
	require.NoError(t, queue.Recover(context.Background(), cl, "ns"))

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
	require.NoError(t, queue.Recover(context.Background(), cl, "ns"))
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

// queueRun builds a run for queue tests: class "" stamps no workflow object
// (the pre-classes shape), any other value stamps the declared class.
func queueRun(name, repo, class string) *criteriav1.CriteriaRun {
	run := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec:       criteriav1.CriteriaRunSpec{RepoURL: repo},
	}
	if class != "" {
		run.Spec.Workflow = &criteriav1.RunWorkflow{
			Name:      "wf",
			Type:      "image",
			Namespace: "criteria-jobs",
			Class:     class,
		}
	}
	return run
}

// CRI-242: admission queues are keyed by (repoURL, class). dev-class runs
// serialize per repoURL as before; triage-class runs live in their own
// queue and admit concurrently with any run on the same repo.
func TestRunQueueClassKeyedAdmission(t *testing.T) {
	repo := "https://github.com/org/repo.git"

	t.Run("triage run is admitted concurrently with a dev run on the same repo", func(t *testing.T) {
		q := controller.NewRunQueue()
		dev := queueRun("dev-1", repo, "")
		triage := queueRun("triage-1", repo, criteriav1.RunClassTriage)

		admitted, status, _ := q.Enqueue(dev)
		require.True(t, admitted)
		require.Equal(t, "dev", status.Class)
		require.Equal(t, repo, status.RepoURL)
		require.Equal(t, "dev-1", status.Running)

		admitted, status, _ = q.Enqueue(triage)
		require.True(t, admitted, "triage must not be blocked by the dev run on the same repo")
		require.Equal(t, "triage", status.Class)
		require.Equal(t, repo, status.RepoURL)
		require.Equal(t, "triage-1", status.Running)
		require.Equal(t, 0, status.Position)
	})

	t.Run("two dev runs on one repo serialize", func(t *testing.T) {
		q := controller.NewRunQueue()
		first := queueRun("dev-1", repo, "")
		second := queueRun("dev-2", repo, "")

		admitted, _, _ := q.Enqueue(first)
		require.True(t, admitted)

		admitted, status, prev := q.Enqueue(second)
		assert.False(t, admitted, "the second dev run waits behind the first")
		assert.Equal(t, 1, status.Position)
		require.NotNil(t, prev)
		assert.Equal(t, "dev-1", prev.Name)
	})

	t.Run("two triage runs on one repo serialize with each other", func(t *testing.T) {
		q := controller.NewRunQueue()
		first := queueRun("triage-1", repo, criteriav1.RunClassTriage)
		second := queueRun("triage-2", repo, criteriav1.RunClassTriage)

		admitted, _, _ := q.Enqueue(first)
		require.True(t, admitted)

		admitted, status, prev := q.Enqueue(second)
		assert.False(t, admitted, "triage queue concurrency stays at 1")
		assert.Equal(t, 1, status.Position)
		require.NotNil(t, prev)
		assert.Equal(t, "triage-1", prev.Name)
	})

	t.Run("unknown class behaves as dev", func(t *testing.T) {
		q := controller.NewRunQueue()
		dev := queueRun("dev-1", repo, "")
		unknown := queueRun("unknown-1", repo, "concurrent")

		admitted, _, _ := q.Enqueue(dev)
		require.True(t, admitted)

		admitted, _, _ = q.Enqueue(unknown)
		assert.False(t, admitted, "a workflow without a recognized class serializes with dev")
	})

	t.Run("releasing a dev run admits the next dev run and leaves the triage queue alone", func(t *testing.T) {
		q := controller.NewRunQueue()
		dev1 := queueRun("dev-1", repo, "")
		dev2 := queueRun("dev-2", repo, "")
		triage := queueRun("triage-1", repo, criteriav1.RunClassTriage)
		q.Enqueue(dev1)
		q.Enqueue(triage)
		_, _, prev := q.Enqueue(dev2)
		require.NotNil(t, prev)

		next := q.Release(dev1)
		require.NotNil(t, next, "the queued dev run is admitted on release")
		assert.Equal(t, "dev-2", next.Name)

		// The triage queue is untouched: its run stays admitted and the dev
		// release does not admit anything from it.
		_, status, _ := q.Enqueue(triage)
		assert.True(t, status.Running == "triage-1", "triage keeps its own admitted run")
		assert.Equal(t, 1, status.Length)
		assert.Equal(t, "triage", status.Class)
	})

	t.Run("releasing a triage run admits the next triage run and leaves the dev queue alone", func(t *testing.T) {
		q := controller.NewRunQueue()
		triage1 := queueRun("triage-1", repo, criteriav1.RunClassTriage)
		triage2 := queueRun("triage-2", repo, criteriav1.RunClassTriage)
		dev := queueRun("dev-1", repo, "")
		q.Enqueue(triage1)
		q.Enqueue(dev)
		_, _, prev := q.Enqueue(triage2)
		require.NotNil(t, prev)

		next := q.Release(triage1)
		require.NotNil(t, next, "the queued triage run is admitted on release")
		assert.Equal(t, "triage-2", next.Name)

		_, status, _ := q.Enqueue(dev)
		assert.Equal(t, "dev-1", status.Running, "the dev run stays admitted across the triage release")
		assert.Equal(t, "dev", status.Class)
	})
}

// CRI-242: Recover files runs into the (repoURL, class) queue their stamped
// workflow declares, so a triage run left mid-flight by a restart is
// admitted alongside — not behind — dev runs on the same repo.
func TestRunQueueRecoverClassKeyed(t *testing.T) {
	scheme := newScheme(t)
	repo := "https://github.com/org/repo.git"

	triageRun := criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{Name: "repo-triage", Namespace: "ns"},
		Spec: criteriav1.CriteriaRunSpec{
			RepoURL: repo,
			Workflow: &criteriav1.RunWorkflow{
				Name:      "wf",
				Type:      "image",
				Namespace: "criteria-jobs",
				Class:     criteriav1.RunClassTriage,
			},
		},
		Status: criteriav1.CriteriaRunStatus{Phase: criteriav1.PhaseRunning},
	}
	devRun := criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{Name: "repo-dev", Namespace: "ns"},
		Spec:       criteriav1.CriteriaRunSpec{RepoURL: repo},
		Status:     criteriav1.CriteriaRunStatus{Phase: criteriav1.PhasePending},
	}

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&criteriav1.CriteriaRun{}).
		WithObjects(&triageRun, &devRun).
		Build()

	q := controller.NewRunQueue()
	require.NoError(t, q.Recover(context.Background(), cl, "ns"))

	admitted, status, _ := q.Enqueue(queueRun("repo-dev", repo, ""))
	assert.True(t, admitted, "the dev head is admitted")
	assert.Equal(t, "dev", status.Class)
	assert.Equal(t, "repo-dev", status.Running)

	admitted, status, _ = q.Enqueue(queueRun("repo-triage", repo, criteriav1.RunClassTriage))
	assert.True(t, admitted, "the recovered triage run is admitted concurrently with dev")
	assert.Equal(t, "triage", status.Class)
	assert.Equal(t, "repo-triage", status.Running)
	assert.Equal(t, 1, status.Length)
}
