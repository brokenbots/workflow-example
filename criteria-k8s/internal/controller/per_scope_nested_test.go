package controller

// Regression tests for CRI-132: with PerScopeSessions=true, a run event
// stream containing the engine's captured nested provision_wanted line must
// drive reconcilePerScopeAdapters to create the per-scope adapter pod. The
// captured bytes below are copied byte-for-byte from the triage artifact
// evidence/cri132-r3-emission-lines.ndjson (seq 1) — never hand-reformatted.

import (
	"context"
	"testing"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/jobbuilder"
	logr "github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Verbatim capture, seq 1: nested provision_wanted at scope entry.
const cri132CapturedNestedProvision = `{"schema_version":1,"seq":1,"run_id":"58f12fe6-5144-4438-8e99-13f701be454c","payload_type":"AdapterEvent","payload":{"adapter":"default","kind":"adapter.lifecycle.provision_wanted","data":{"adapter":"default","digest":"","run_id":"","scope_instance_id":"13d83326-f18d-49ed-942d-29c5f291a305","scope_name":"","shim_listen_address":"127.0.0.1:38625","token_ref":"/tmp/TestCRI132CaptureEmissionLines1209998812/001/runs/58f12fe6-5144-4438-8e99-13f701be454c/remote-tokens/13d83326-f18d-49ed-942d-29c5f291a305/noop.token"}}}`

// stubEventsReader returns fixed bytes and counts how many times Read was
// called, so a no-op reconciliation is never mistaken for a stub failure.
type stubEventsReader struct {
	data      []byte
	readCount int
}

func (s *stubEventsReader) Read(ctx context.Context, run *criteriav1.CriteriaRun) ([]byte, error) {
	s.readCount++
	return s.data, nil
}

func newPerScopeTestReconciler(t *testing.T, reader *stubEventsReader, objs ...client.Object) (*CriteriaRunReconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, criteriav1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &CriteriaRunReconciler{
		Client:   cl,
		Scheme:   scheme,
		Reader:   reader,
		Defaults: jobbuilder.Defaults{DataPVC: "criteria-data"},
	}, cl
}

func perScopeTestRun(perScopeSessions bool) *criteriav1.CriteriaRun {
	return &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cri-132",
			Namespace: "default",
			UID:       types.UID("run-uid"),
		},
		Spec: criteriav1.CriteriaRunSpec{
			TicketID:         "CRI-132",
			PerScopeSessions: perScopeSessions,
		},
	}
}

func listAdapterPods(t *testing.T, cl client.Client, namespace string) []corev1.Pod {
	t.Helper()
	var pods corev1.PodList
	require.NoError(t, cl.List(context.Background(), &pods, client.InNamespace(namespace)))
	return pods.Items
}

func containerEnv(pod corev1.Pod, name string) string {
	for _, env := range pod.Spec.Containers[0].Env {
		if env.Name == name {
			return env.Value
		}
	}
	return ""
}

// With PerScopeSessions=true, the captured nested provision_wanted stream
// yields one active provision and the reconciler creates the per-scope
// adapter pod with the captured shim address, scope id, and token file.
func TestReconcilePerScopeAdaptersCreatesPodForNestedProvision(t *testing.T) {
	run := perScopeTestRun(true)
	reader := &stubEventsReader{data: []byte(cri132CapturedNestedProvision)}
	r, cl := newPerScopeTestReconciler(t, reader, run)

	active, err := r.reconcilePerScopeAdapters(context.Background(), run, logr.Discard())
	require.NoError(t, err)
	assert.Equal(t, 1, active)

	wantName := jobbuilder.PerScopeAdapterPodName(run, "default", "13d83326-f18d-49ed-942d-29c5f291a305")
	pods := listAdapterPods(t, cl, "default")
	require.Len(t, pods, 1, "exactly one adapter pod must be created")
	assert.Equal(t, wantName, pods[0].Name)

	env := map[string]string{}
	for _, e := range pods[0].Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	assert.Equal(t, "127.0.0.1:38625", env["CRITERIA_REMOTE_HOST"])
	assert.Equal(t, "13d83326-f18d-49ed-942d-29c5f291a305", env["CRITERIA_SCOPE_ID"])
	assert.Equal(t, "/tmp/TestCRI132CaptureEmissionLines1209998812/001/runs/58f12fe6-5144-4438-8e99-13f701be454c/remote-tokens/13d83326-f18d-49ed-942d-29c5f291a305/noop.token", env["CRITERIA_REMOTE_TOKEN_FILE"])
	assert.Equal(t, "default", env["ADAPTER_KIND"])
	assert.Equal(t, "", env["CRITERIA_REMOTE_DIGEST"], "captured digest is empty and must not be synthesized")
}

// The reconcile is idempotent: a second pass over the same stream keeps the
// existing pod and reports the same active count.
func TestReconcilePerScopeAdaptersNestedStreamIdempotent(t *testing.T) {
	run := perScopeTestRun(true)
	reader := &stubEventsReader{data: []byte(cri132CapturedNestedProvision)}
	r, cl := newPerScopeTestReconciler(t, reader, run)

	active, err := r.reconcilePerScopeAdapters(context.Background(), run, logr.Discard())
	require.NoError(t, err)
	require.Equal(t, 1, active)

	active, err = r.reconcilePerScopeAdapters(context.Background(), run, logr.Discard())
	require.NoError(t, err)
	assert.Equal(t, 1, active)

	pods := listAdapterPods(t, cl, "default")
	require.Len(t, pods, 1)
	assert.Equal(t, jobbuilder.PerScopeAdapterPodName(run, "default", "13d83326-f18d-49ed-942d-29c5f291a305"), pods[0].Name)
}

// With PerScopeSessions=false the reconcile is a complete no-op: zero reader
// reads, zero active provisions, and no pod mutation.
func TestReconcilePerScopeAdaptersDisabledIsNoOp(t *testing.T) {
	run := perScopeTestRun(false)
	reader := &stubEventsReader{data: []byte(cri132CapturedNestedProvision)}
	seed := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cri-132-adp-default-deadbeef1234",
			Namespace: "default",
			Labels: map[string]string{
				"criteria.brokenbots.dev/run":  "cri-132",
				"criteria.brokenbots.dev/role": "adapter",
			},
		},
	}
	r, cl := newPerScopeTestReconciler(t, reader, run, seed)

	active, err := r.reconcilePerScopeAdapters(context.Background(), run, logr.Discard())
	require.NoError(t, err)
	assert.Equal(t, 0, active)
	assert.Equal(t, 0, reader.readCount, "Reader must not be called when per-scope sessions are disabled")

	pods := listAdapterPods(t, cl, "default")
	assert.Len(t, pods, 1, "seeded pod must survive untouched")
}
