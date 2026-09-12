package controller

// Regression tests for CRI-144: the adapter sweep must reap adapter Pods and
// legacy-path adapter Jobs whose owning CriteriaRun no longer exists —
// regardless of how the CR was deleted — and must adopt ownerReference-less
// adapter objects so GC reaps them at CR deletion. Objects that cannot be
// attributed to a CriteriaRun, adapter objects of a live run, and objects
// outside the swept namespace are left untouched.

import (
	"context"
	"testing"
	"time"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/jobbuilder"
	logr "github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func newSweepClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, criteriav1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, batchv1.AddToScheme(scheme))
	builder := fake.NewClientBuilder().WithScheme(scheme)
	if len(objs) > 0 {
		builder = builder.WithObjects(objs...)
	}
	return builder.Build()
}

func newSweepTestRun(name, uid string) *criteriav1.CriteriaRun {
	return &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			UID:       types.UID(uid),
		},
		Spec: criteriav1.CriteriaRunSpec{TicketID: name},
	}
}

// adapterPod builds an adapter-labeled pod like the per-scope reconciler and
// the legacy job builder do.
func adapterPod(name, namespace, runName string) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				jobbuilder.LabelRun:  runName,
				jobbuilder.LabelRole: jobbuilder.RoleAdapter,
			},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "adapter", Image: "adp:1"}}},
	}
	return pod
}

func adapterJob(name, namespace, runName string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				jobbuilder.LabelRun:  runName,
				jobbuilder.LabelRole: jobbuilder.RoleAdapter,
			},
		},
		Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "adapter", Image: "adp:1"}}},
		}},
	}
}

func withCriteriaRunOwnerRef(obj metav1.Object, runName, uid string, controller bool) {
	obj.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: "criteria.brokenbots.dev/v1",
		Kind:       "CriteriaRun",
		Name:       runName,
		UID:        types.UID(uid),
		Controller: &controller,
	}})
}

func getSweptPod(t *testing.T, cl client.Client, namespace, name string) (*corev1.Pod, error) {
	t.Helper()
	var pod corev1.Pod
	err := cl.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: name}, &pod)
	if err != nil {
		return nil, err
	}
	return &pod, nil
}

func TestAdapterSweeperDeletesOrphanPodByRunLabel(t *testing.T) {
	pod := adapterPod("adp-cri-130-0", "default", "cri-130")
	cl := newSweepClient(t, pod)
	s := &AdapterSweeper{Client: cl, Scheme: cl.Scheme(), Namespace: "default"}

	s.sweep(context.Background(), logr.Discard())

	_, err := getSweptPod(t, cl, "default", "adp-cri-130-0")
	require.True(t, apierrors.IsNotFound(err), "orphan adapter pod must be swept")
}

func TestAdapterSweeperDeletesOrphanPodByOwnerRefOnly(t *testing.T) {
	pod := adapterPod("adp-cri-130-1", "default", "")
	pod.Labels = map[string]string{jobbuilder.LabelRole: jobbuilder.RoleAdapter}
	withCriteriaRunOwnerRef(pod, "cri-130", "uid-130", true)
	cl := newSweepClient(t, pod)
	s := &AdapterSweeper{Client: cl, Scheme: cl.Scheme(), Namespace: "default"}

	s.sweep(context.Background(), logr.Discard())

	_, err := getSweptPod(t, cl, "default", "adp-cri-130-1")
	require.True(t, apierrors.IsNotFound(err), "adapter pod attributable only via ownerReference to a deleted run must be swept")
}

func TestAdapterSweeperKeepsPodOfLiveRun(t *testing.T) {
	run := newSweepTestRun("cri-132", "run-uid")
	pod := adapterPod("adp-cri-132-0", "default", "cri-132")
	withCriteriaRunOwnerRef(pod, "cri-132", "run-uid", true)
	cl := newSweepClient(t, run, pod)
	s := &AdapterSweeper{Client: cl, Scheme: cl.Scheme(), Namespace: "default"}

	s.sweep(context.Background(), logr.Discard())

	got, err := getSweptPod(t, cl, "default", "adp-cri-132-0")
	require.NoError(t, err)
	assert.Equal(t, "run-uid", string(got.OwnerReferences[0].UID), "live-run pod must not be touched")
}

func TestAdapterSweeperDeletesPodWithStaleOwnerRefUID(t *testing.T) {
	// The run name was recycled: the pod's ownerReference points at the
	// deleted run (uid-1) while the live run has uid-2.
	run := newSweepTestRun("cri-132", "uid-2")
	pod := adapterPod("adp-stale", "default", "cri-132")
	withCriteriaRunOwnerRef(pod, "cri-132", "uid-1", true)
	cl := newSweepClient(t, run, pod)
	s := &AdapterSweeper{Client: cl, Scheme: cl.Scheme(), Namespace: "default"}

	s.sweep(context.Background(), logr.Discard())

	_, err := getSweptPod(t, cl, "default", "adp-stale")
	require.True(t, apierrors.IsNotFound(err), "pod owned by a deleted run whose name was recycled must be swept")
}

func TestAdapterSweeperAdoptsOwnerReflessPodOfLiveRun(t *testing.T) {
	run := newSweepTestRun("cri-132", "run-uid")
	pod := adapterPod("adp-adopt", "default", "cri-132")
	cl := newSweepClient(t, run, pod)
	s := &AdapterSweeper{Client: cl, Scheme: cl.Scheme(), Namespace: "default"}

	s.sweep(context.Background(), logr.Discard())

	got, err := getSweptPod(t, cl, "default", "adp-adopt")
	require.NoError(t, err)
	require.Len(t, got.OwnerReferences, 1, "ownerReference must be added so GC reaps the pod at CR deletion")
	ref := got.OwnerReferences[0]
	assert.Equal(t, "CriteriaRun", ref.Kind)
	assert.Equal(t, "cri-132", ref.Name)
	assert.Equal(t, types.UID("run-uid"), ref.UID)
	require.NotNil(t, ref.Controller)
	assert.True(t, *ref.Controller)
}

func TestAdapterSweeperKeepsForeignControlledPodOfLiveRun(t *testing.T) {
	// Legacy-path adapter pods are owned by their adapter Job: the sweep must
	// not adopt them for the run (the Job already controls them) and must
	// not delete them while the run lives.
	run := newSweepTestRun("cri-132", "run-uid")
	pod := adapterPod("adp-legacy", "default", "cri-132")
	controller := true
	pod.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "batch/v1",
		Kind:       "Job",
		Name:       "cri-132-adapter",
		UID:        "job-uid",
		Controller: &controller,
	}}
	cl := newSweepClient(t, run, pod)
	s := &AdapterSweeper{Client: cl, Scheme: cl.Scheme(), Namespace: "default"}

	s.sweep(context.Background(), logr.Discard())

	got, err := getSweptPod(t, cl, "default", "adp-legacy")
	require.NoError(t, err)
	assert.Len(t, got.OwnerReferences, 1, "Job-owned pod must not gain a CriteriaRun ownerReference")
	assert.Equal(t, "Job", got.OwnerReferences[0].Kind)
}

func TestAdapterSweeperKeepsUnattributablePod(t *testing.T) {
	pod := adapterPod("adp-unknown", "default", "")
	pod.Labels = map[string]string{jobbuilder.LabelRole: jobbuilder.RoleAdapter}
	cl := newSweepClient(t, pod)
	s := &AdapterSweeper{Client: cl, Scheme: cl.Scheme(), Namespace: "default"}

	s.sweep(context.Background(), logr.Discard())

	_, err := getSweptPod(t, cl, "default", "adp-unknown")
	require.NoError(t, err, "unattributable adapter pod must be left untouched")
}

func TestAdapterSweeperIsNamespaceScoped(t *testing.T) {
	pod := adapterPod("adp-elsewhere", "other-ns", "cri-130")
	cl := newSweepClient(t, pod)
	s := &AdapterSweeper{Client: cl, Scheme: cl.Scheme(), Namespace: "default"}

	s.sweep(context.Background(), logr.Discard())

	_, err := getSweptPod(t, cl, "other-ns", "adp-elsewhere")
	require.NoError(t, err, "pods outside the swept namespace must never be touched")
}

func TestAdapterSweeperDeletesOrphanLegacyAdapterJob(t *testing.T) {
	job := adapterJob("cri-130-adapter", "default", "cri-130")
	cl := newSweepClient(t, job)
	s := &AdapterSweeper{Client: cl, Scheme: cl.Scheme(), Namespace: "default"}

	s.sweep(context.Background(), logr.Discard())

	var jobs batchv1.JobList
	require.NoError(t, cl.List(context.Background(), &jobs, client.InNamespace("default")))
	assert.Empty(t, jobs.Items, "legacy adapter Job of a deleted run must be swept")
}

func TestAdapterSweeperKeepsLegacyAdapterJobOfLiveRun(t *testing.T) {
	run := newSweepTestRun("cri-132", "run-uid")
	job := adapterJob("cri-132-adapter", "default", "cri-132")
	withCriteriaRunOwnerRef(job, "cri-132", "run-uid", true)
	cl := newSweepClient(t, run, job)
	s := &AdapterSweeper{Client: cl, Scheme: cl.Scheme(), Namespace: "default"}

	s.sweep(context.Background(), logr.Discard())

	var jobs batchv1.JobList
	require.NoError(t, cl.List(context.Background(), &jobs, client.InNamespace("default")))
	require.Len(t, jobs.Items, 1, "legacy adapter Job of a live run must not be touched")
	assert.Equal(t, "run-uid", string(jobs.Items[0].OwnerReferences[0].UID))
}

func TestNewAdapterSweeperDefaultsInterval(t *testing.T) {
	cl := newSweepClient(t)
	assert.Equal(t, 30*time.Second, NewAdapterSweeper(cl, cl.Scheme(), "default", 0).Interval)
	assert.Equal(t, 30*time.Second, NewAdapterSweeper(cl, cl.Scheme(), "default", -time.Second).Interval)
	assert.Equal(t, 5*time.Second, NewAdapterSweeper(cl, cl.Scheme(), "default", 5*time.Second).Interval)
}

func TestAdapterSweeperRunSweepsAtStartup(t *testing.T) {
	pod := adapterPod("adp-cri-130-0", "default", "cri-130")
	cl := newSweepClient(t, pod)
	s := NewAdapterSweeper(cl, cl.Scheme(), "default", time.Hour)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Run(ctx)
	}()

	require.Eventually(t, func() bool {
		_, err := getSweptPod(t, cl, "default", "adp-cri-130-0")
		return apierrors.IsNotFound(err)
	}, 2*time.Second, 10*time.Millisecond, "startup sweep must reap orphaned adapter pods")
	cancel()
	<-done
}

// Pin the adoption contract: an adopted adapter object ends up with a
// controller ownerReference to the CriteriaRun, i.e. exactly what GC needs
// at CR deletion.
func TestAdapterSweeperAdoptionUsesSetControllerReference(t *testing.T) {
	run := newSweepTestRun("cri-132", "run-uid")
	pod := adapterPod("adp-adopt", "default", "cri-132")
	cl := newSweepClient(t, run, pod)
	s := &AdapterSweeper{Client: cl, Scheme: cl.Scheme(), Namespace: "default"}

	assert.Equal(t, sweepAdopted, s.sweepObject(context.Background(), "Pod", pod, logr.Discard()))

	ref := metav1.GetControllerOf(pod)
	require.NotNil(t, ref)
	assert.Equal(t, "CriteriaRun", ref.Kind)
	assert.Equal(t, types.UID("run-uid"), ref.UID)
}

// An adapter object whose run label and ownerRef disagree with the live run
// must not be re-owned: the ownerRef UID check fires first.
func TestAdapterSweeperPrefersUIDCheckOverAdoption(t *testing.T) {
	run := newSweepTestRun("cri-132", "uid-2")
	pod := adapterPod("adp-stale", "default", "cri-132")
	withCriteriaRunOwnerRef(pod, "cri-132", "uid-1", true)
	cl := newSweepClient(t, run, pod)
	s := &AdapterSweeper{Client: cl, Scheme: cl.Scheme(), Namespace: "default"}

	assert.Equal(t, sweepDeleted, s.sweepObject(context.Background(), "Pod", pod, logr.Discard()))
}

// Guard the adoption precondition directly: SetControllerReference must be
// reachable only for objects without any controller ownerReference.
func TestAdapterSweeperAdoptionPrecondition(t *testing.T) {
	run := newSweepTestRun("cri-132", "run-uid")

	owned := adapterPod("adp-owned", "default", "cri-132")
	controller := true
	owned.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "batch/v1", Kind: "Job", Name: "j", UID: "job-uid", Controller: &controller,
	}}
	assert.NotNil(t, metav1.GetControllerOf(owned), "Job-owned pods must fail the adoption precondition")

	free := adapterPod("adp-free", "default", "cri-132")
	assert.Nil(t, metav1.GetControllerOf(free), "ownerRef-less pods must pass the adoption precondition")
	require.NoError(t, controllerutil.SetControllerReference(run, free, newSweepClient(t).Scheme()))
}
