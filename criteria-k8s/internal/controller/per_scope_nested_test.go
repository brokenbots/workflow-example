package controller

// Regression tests for CRI-132 and CRI-135: with PerScopeSessions=true, the
// run lifecycle stream consumed from castle must drive
// reconcilePerScopeAdapters to create the per-scope adapter pod, and release
// events must delete it again. The captured emission below is copied
// byte-for-byte from the triage artifact
// evidence/cri132-r3-emission-lines.ndjson (seq 1) — never hand-reformatted.
// The castle wire conversion of the equivalent envelope is pinned to the
// CRI-132 file parser output in internal/castle/wire_test.go; these tests
// consume the resulting events.LifecycleEvent values directly, exactly as
// the controller receives them from the castle client.

import (
	"context"
	"strings"
	"testing"
	"time"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/castle"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/events"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/jobbuilder"
	logr "github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
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

// Verbatim capture, seq 1: nested provision_wanted at scope entry.
const cri132CapturedNestedProvision = `{"schema_version":1,"seq":1,"run_id":"58f12fe6-5144-4438-8e99-13f701be454c","payload_type":"AdapterEvent","payload":{"adapter":"default","kind":"adapter.lifecycle.provision_wanted","data":{"adapter":"default","digest":"","run_id":"","scope_instance_id":"13d83326-f18d-49ed-942d-29c5f291a305","scope_name":"","shim_listen_address":"127.0.0.1:38625","token_ref":"/tmp/runs/58f12fe6/remote-tokens/13d83326-f18d-49ed-942d-29c5f291a305/noop.token"}}}`

// The castle-sourced lifecycle event equivalent to the captured emission:
// every value is taken verbatim from the captured line.
var capturedProvisionEvent = events.LifecycleEvent{
	Event:       events.EventProvisionWanted,
	RunID:       "58f12fe6-5144-4438-8e99-13f701be454c",
	ScopeID:     "13d83326-f18d-49ed-942d-29c5f291a305",
	AdapterName: "default",
	ShimAddress: "127.0.0.1:38625",
	TokenFile:   "/tmp/runs/58f12fe6/remote-tokens/13d83326-f18d-49ed-942d-29c5f291a305/noop.token",
	Digest:      "",
}

// The release for the captured scope. Over the castle wire the engine reports
// the shim registration name ("noop.default") on release; the castle client
// resolves it against the provisioned adapter ("default") for the scope
// instance, so the controller sees the resolved adapter here.
var capturedReleaseEvent = events.LifecycleEvent{
	Event:       events.EventRelease,
	RunID:       "58f12fe6-5144-4438-8e99-13f701be454c",
	ScopeID:     "13d83326-f18d-49ed-942d-29c5f291a305",
	AdapterName: "default",
}

// stubCastle is a castle.RunSource stub returning a fixed observation.
type stubCastle struct {
	observation   *castle.Observation
	calls         int
	lastRunnerJob string
	lastKnownID   string
}

func (s *stubCastle) Observe(ctx context.Context, runnerJob, knownRunID string) (*castle.Observation, error) {
	s.calls++
	s.lastRunnerJob = runnerJob
	s.lastKnownID = knownRunID
	if s.observation == nil {
		return &castle.Observation{}, nil
	}
	return s.observation, nil
}

func (s *stubCastle) Disabled() bool { return false }

func newPerScopeTestReconciler(t *testing.T, objs ...client.Object) (*CriteriaRunReconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, criteriav1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, batchv1.AddToScheme(scheme))
	builder := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&criteriav1.CriteriaRun{})
	if len(objs) > 0 {
		builder = builder.WithObjects(objs...)
	}
	cl := builder.Build()
	return &CriteriaRunReconciler{
		Client:   cl,
		Scheme:   scheme,
		Defaults: jobbuilder.Defaults{DataPVC: "criteria-data"},
		Queue:    NewRunQueue(),
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

// With PerScopeSessions=true, the castle-sourced provision event equivalent
// to the captured nested provision_wanted emission yields one active
// provision and the reconciler creates the per-scope adapter pod with the
// captured shim address, scope id, and token file.
func TestReconcilePerScopeAdaptersCreatesPodForNestedProvision(t *testing.T) {
	run := perScopeTestRun(true)
	r, cl := newPerScopeTestReconciler(t, run)

	active, err := r.reconcilePerScopeAdapters(context.Background(), run, []events.LifecycleEvent{capturedProvisionEvent}, logr.Discard())
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
	assert.Equal(t, "13d83326-f18d-49ed-942d-29c5f291a305", env["CRITERIA_SCOPE_ID"])
	assert.Equal(t, "/tmp/runs/58f12fe6/remote-tokens/13d83326-f18d-49ed-942d-29c5f291a305/noop.token", env["CRITERIA_REMOTE_TOKEN_FILE"])
	assert.Equal(t, "default", env["ADAPTER_KIND"])
	assert.Equal(t, "", env["CRITERIA_REMOTE_DIGEST"], "captured digest is empty and must not be synthesized")

	// CRITERIA_REMOTE_HOST must be absent: the event's shim listen address is
	// the runner's own-loopback bind, unreachable from a separate pod. The
	// adapter discovers the routable address from the shared discovery file.
	_, hasHost := env["CRITERIA_REMOTE_HOST"]
	assert.False(t, hasHost, "CRITERIA_REMOTE_HOST must not be set from the event's loopback shim address")
}

// The v0.5.22-shaped provision event (adapter instance "intake", kind
// "shell") exercises the production reconcile path end-to-end: the built pod
// must resolve the image, kind label, and env from adapter_type — never from
// the instance name. A regression in the castle wire conversion
// (castle/lifecycleFromEnvelope) surfaces here as the wedged
// criteria-adapter-intake image.
func TestReconcilePerScopeAdaptersResolvesKindFromAdapterType(t *testing.T) {
	run := perScopeTestRun(true)
	r, cl := newPerScopeTestReconciler(t, run)

	// The castle-derived event equivalent to the v0.5.22-shaped emission:
	// every value is what lifecycleFromEnvelope yields for it.
	scope := events.LifecycleEvent{
		Event:       events.EventProvisionWanted,
		RunID:       "CRI-140",
		ScopeID:     "root",
		AdapterName: "intake",
		AdapterType: "shell",
		Digest:      "sha256:d9f306c29f4145da8bcc44187c9e4ae0f69ed30db3b3edac6e9b6350469bc635",
	}

	active, err := r.reconcilePerScopeAdapters(context.Background(), run, []events.LifecycleEvent{scope}, logr.Discard())
	require.NoError(t, err)
	assert.Equal(t, 1, active)

	pods := listAdapterPods(t, cl, "default")
	require.Len(t, pods, 1, "exactly one per-scope adapter pod must be created")
	assert.Equal(t, jobbuilder.PerScopeAdapterPodName(run, "shell", "root"), pods[0].Name)

	container := pods[0].Spec.Containers[0]
	assert.Equal(t, "localhost:5000/criteria-adapter-shell:k8s-0.5.3", container.Image,
		"the image must resolve to an existing registry image for the adapter KIND, never criteria-adapter-intake")
	assert.Equal(t, "shell", pods[0].Labels["criteria.brokenbots.dev/adapter-kind"])

	envNames := make(map[string]string)
	for _, e := range container.Env {
		envNames[e.Name] = e.Value
	}
	assert.Equal(t, "shell", envNames["ADAPTER_KIND"])
	assert.Equal(t, "intake", envNames["CRITERIA_ADAPTER_NAME"],
		"the pod carries the adapter instance name so adapter.sh resolves the instance-keyed digest file")
	assert.Equal(t, "sha256:d9f306c29f4145da8bcc44187c9e4ae0f69ed30db3b3edac6e9b6350469bc635", envNames["CRITERIA_REMOTE_DIGEST"])
}

// A provision event without adapter_type (engines older than the pinned
// v0.5.22) still builds the pod with the kind resolved from the adapter node
// name — the reconciler's chosen CRI-140 semantics is log-and-continue, not
// skip — and the error-level log carries the alertable structured field.
func TestReconcilePerScopeAdaptersBuildsFallbackPodWithoutAdapterType(t *testing.T) {
	run := perScopeTestRun(true)
	r, cl := newPerScopeTestReconciler(t, run)

	var logLines []string
	logger := funcr.NewJSON(func(obj string) { logLines = append(logLines, obj) }, funcr.Options{})

	scope := events.LifecycleEvent{
		Event:       events.EventProvisionWanted,
		RunID:       "CRI-140",
		ScopeID:     "root",
		AdapterName: "intake",
	}

	active, err := r.reconcilePerScopeAdapters(context.Background(), run, []events.LifecycleEvent{scope}, logger)
	require.NoError(t, err, "a missing adapter_type must not fail the reconcile; it falls back")
	assert.Equal(t, 1, active)

	pods := listAdapterPods(t, cl, "default")
	require.Len(t, pods, 1, "the fallback pod must still be built for older engines")
	assert.Equal(t, "intake", pods[0].Labels["criteria.brokenbots.dev/adapter-kind"])
	envNames := make(map[string]string)
	for _, e := range pods[0].Spec.Containers[0].Env {
		envNames[e.Name] = e.Value
	}
	assert.Equal(t, "intake", envNames["ADAPTER_KIND"])

	// The alert hook: exactly the missing-adapter_type log, structured so the
	// operator can alert on it. Other (info) lines may accompany it; filter
	// for the alert line itself.
	var alerts []string
	for _, line := range logLines {
		if strings.Contains(line, `"reason":"adapter_type_missing"`) {
			alerts = append(alerts, line)
		}
	}
	require.Len(t, alerts, 1, "exactly one alert log for the missing adapter_type")
	assert.Contains(t, alerts[0], `"adapter":"intake"`)
	assert.Contains(t, alerts[0], `"run":"cri-132"`)
}

// The reconcile is idempotent: a second pass over the same (full history)
// stream keeps the existing pod and reports the same active count.
func TestReconcilePerScopeAdaptersNestedStreamIdempotent(t *testing.T) {
	run := perScopeTestRun(true)
	r, cl := newPerScopeTestReconciler(t, run)

	active, err := r.reconcilePerScopeAdapters(context.Background(), run, []events.LifecycleEvent{capturedProvisionEvent}, logr.Discard())
	require.NoError(t, err)
	require.Equal(t, 1, active)

	active, err = r.reconcilePerScopeAdapters(context.Background(), run, []events.LifecycleEvent{capturedProvisionEvent}, logr.Discard())
	require.NoError(t, err)
	assert.Equal(t, 1, active)

	pods := listAdapterPods(t, cl, "default")
	require.Len(t, pods, 1)
	assert.Equal(t, jobbuilder.PerScopeAdapterPodName(run, "default", "13d83326-f18d-49ed-942d-29c5f291a305"), pods[0].Name)
}

// A release event (post resolution, as delivered by the castle client)
// removes the scope's pod.
func TestReconcilePerScopeAdaptersReleaseDeletesPod(t *testing.T) {
	run := perScopeTestRun(true)
	r, cl := newPerScopeTestReconciler(t, run)

	active, err := r.reconcilePerScopeAdapters(context.Background(), run, []events.LifecycleEvent{capturedProvisionEvent}, logr.Discard())
	require.NoError(t, err)
	require.Equal(t, 1, active)
	require.Len(t, listAdapterPods(t, cl, "default"), 1)

	// The castle client delivers the full accumulated history on every pass.
	history := []events.LifecycleEvent{capturedProvisionEvent, capturedReleaseEvent}
	active, err = r.reconcilePerScopeAdapters(context.Background(), run, history, logr.Discard())
	require.NoError(t, err)
	assert.Equal(t, 0, active)
	assert.Empty(t, listAdapterPods(t, cl, "default"), "released scope's pod must be deleted")
}

// With PerScopeSessions=false the reconcile is a complete no-op: zero active
// provisions and no pod mutation, even when lifecycle events are delivered.
func TestReconcilePerScopeAdaptersDisabledIsNoOp(t *testing.T) {
	run := perScopeTestRun(false)
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
	r, cl := newPerScopeTestReconciler(t, run, seed)

	active, err := r.reconcilePerScopeAdapters(context.Background(), run, []events.LifecycleEvent{capturedProvisionEvent}, logr.Discard())
	require.NoError(t, err)
	assert.Equal(t, 0, active)

	pods := listAdapterPods(t, cl, "default")
	assert.Len(t, pods, 1, "seeded pod must survive untouched")
}

// End-to-end through Reconcile: the castle-sourced provision event creates
// the pod and the run is requeued on the 10s reconcile interval, with the
// discovered castle run id persisted after the first pass.
func TestReconcilePerScopeRequeuesOnIntervalWithCastleRunID(t *testing.T) {
	run := perScopeTestRun(true)
	castleStub := &stubCastle{observation: &castle.Observation{
		RunID:     "castle-run-9",
		Lifecycle: []events.LifecycleEvent{capturedProvisionEvent},
	}}
	r, cl := newPerScopeTestReconciler(t, run)
	r.Castle = castleStub

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: run.Name, Namespace: run.Namespace}})
	require.NoError(t, err)
	assert.Equal(t, 10*time.Second, res.RequeueAfter, "per-scope runs poll castle on the reconcile interval")

	var updated criteriav1.CriteriaRun
	require.NoError(t, cl.Get(context.Background(), types.NamespacedName{Name: run.Name, Namespace: run.Namespace}, &updated))
	assert.Equal(t, "castle-run-9", updated.Status.CastleRunID)
	assert.Len(t, listAdapterPods(t, cl, "default"), 1)

	// The next observation passes the persisted castle run id so discovery
	// is skipped.
	_, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: run.Name, Namespace: run.Namespace}})
	require.NoError(t, err)
	assert.Equal(t, 2, castleStub.calls)
	assert.Equal(t, "castle-run-9", castleStub.lastKnownID, "persisted run id must short-circuit discovery")
	assert.Equal(t, "cri-132", castleStub.lastRunnerJob)
}
