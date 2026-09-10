package controller_test

import (
	"context"
	"testing"
	"time"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/controller"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/events"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/jobbuilder"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newScheme(t *testing.T) *runtime.Scheme {
	s := runtime.NewScheme()
	require.NoError(t, criteriav1.AddToScheme(s))
	require.NoError(t, batchv1.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))
	return s
}

func TestReconcileCreatesJobs(t *testing.T) {
	scheme := newScheme(t)
	run := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cri-42",
			Namespace: "default",
			UID:       types.UID("run-uid"),
		},
		Spec: criteriav1.CriteriaRunSpec{
			TicketID:        "CRI-42",
			RepoURL:         "https://github.com/brokenbots/workflow-example.git",
			Image:           "localhost:5000/linear-intake-remote:dev",
			MaxAgentVisits:  2,
			ProviderBaseURL: "http://provider/v1",
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(run).
		WithObjects(run).
		Build()

	r := &controller.CriteriaRunReconciler{
		Client:   cl,
		Scheme:   scheme,
		Reader:   &fakeReader{outcome: &events.Outcome{PRNumber: "42", TicketState: "Done"}},
		Defaults: jobbuilder.Defaults{DataPVC: "criteria-data"},
	}

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, res)

	var runner batchv1.Job
	require.NoError(t, cl.Get(context.Background(), types.NamespacedName{Name: "cri-42", Namespace: "default"}, &runner))
	require.Len(t, runner.OwnerReferences, 1)
	assert.Equal(t, "CriteriaRun", runner.OwnerReferences[0].Kind)

	for _, kind := range []string{"shell", "copilot"} {
		var adapter batchv1.Job
		require.NoError(t, cl.Get(context.Background(), types.NamespacedName{Name: "cri-42-adapter-" + kind, Namespace: "default"}, &adapter))
		require.Len(t, adapter.OwnerReferences, 1)
		assert.Equal(t, "CriteriaRun", adapter.OwnerReferences[0].Kind)
		assert.Empty(t, adapter.Spec.Template.Spec.ServiceAccountName)
		require.NotNil(t, adapter.Spec.Template.Spec.AutomountServiceAccountToken)
		assert.False(t, *adapter.Spec.Template.Spec.AutomountServiceAccountToken)
		for _, v := range adapter.Spec.Template.Spec.Volumes {
			assert.Nil(t, v.CSI, "adapter volume %q must not be CSI", v.Name)
		}
	}

	var updated criteriav1.CriteriaRun
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(run), &updated))
	assert.NotEmpty(t, updated.Finalizers)
	assert.Equal(t, "cri-42", updated.Status.JobName)
}

func TestReconcileMirrorsJobCompletion(t *testing.T) {
	scheme := newScheme(t)
	run := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "cri-42",
			Namespace:  "default",
			UID:        types.UID("run-uid"),
			Finalizers: []string{"criteriarun.criteria.brokenbots.dev/finalizer"},
		},
		Spec: criteriav1.CriteriaRunSpec{TicketID: "CRI-42"},
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cri-42",
			Namespace: "default",
		},
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
			},
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(run).
		WithObjects(run, job).
		Build()

	r := &controller.CriteriaRunReconciler{
		Client: cl,
		Scheme: scheme,
		Reader: &fakeReader{outcome: &events.Outcome{PRNumber: "42", TicketState: "Done"}},
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	require.NoError(t, err)

	var updated criteriav1.CriteriaRun
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(run), &updated))
	assert.Equal(t, criteriav1.PhaseSucceeded, updated.Status.Phase)
	assert.Equal(t, "42", updated.Status.PRNumber)
	assert.Equal(t, "Done", updated.Status.TicketState)
}

func TestFinalizeDeletesJobs(t *testing.T) {
	scheme := newScheme(t)
	now := metav1.NewTime(time.Now())
	run := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "cri-42",
			Namespace:         "default",
			UID:               types.UID("run-uid"),
			DeletionTimestamp: &now,
			Finalizers:        []string{"criteriarun.criteria.brokenbots.dev/finalizer"},
		},
		Spec: criteriav1.CriteriaRunSpec{TicketID: "CRI-42"},
	}
	jobs := []*batchv1.Job{
		{ObjectMeta: metav1.ObjectMeta{Name: "cri-42", Namespace: "default"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "cri-42-adapter-shell", Namespace: "default"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "cri-42-adapter-copilot", Namespace: "default"}},
	}
	objects := []client.Object{run}
	for _, j := range jobs {
		objects = append(objects, j)
	}

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(run).
		WithObjects(objects...).
		Build()

	r := &controller.CriteriaRunReconciler{Client: cl, Scheme: scheme}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	require.NoError(t, err)

	for _, name := range []string{"cri-42", "cri-42-adapter-shell", "cri-42-adapter-copilot"} {
		var deleted batchv1.Job
		err = cl.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "default"}, &deleted)
		assert.True(t, err != nil, "expected job %s to be deleted", name)
	}

	var updated criteriav1.CriteriaRun
	err = cl.Get(context.Background(), client.ObjectKeyFromObject(run), &updated)
	assert.True(t, err != nil, "expected CriteriaRun to be deleted after finalizer removal")
}

type fakeReader struct {
	outcome *events.Outcome
	data    []byte
}

func (f *fakeReader) Read(ctx context.Context, run *criteriav1.CriteriaRun) ([]byte, error) {
	if len(f.data) > 0 {
		return f.data, nil
	}
	return []byte(`{"pr_number":"` + f.outcome.PRNumber + `","state":"` + f.outcome.TicketState + `"}`), nil
}
