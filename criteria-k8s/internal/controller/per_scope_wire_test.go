package controller

// CRI-237 controller tests: wire token delivery over the reconcile path.
// Provisions carrying an accept_token (runner commit eae0181, CRI-236) must
// produce wire-shaped adapter pods (CRITERIA_REMOTE_TOKEN plus a routable
// CRITERIA_REMOTE_HOST, no shared data volume), resolved from the run's
// runner pod IP; until a runner pod with an IP exists, the reconcile must
// defer ALL adapter mutations rather than build a dangling pod. Legacy
// provisions (token_ref only) keep the pre-eae0181 shape and never need the
// runner resolution.

import (
	"context"
	"testing"

	"github.com/brokenbots/workflow-example/criteria-k8s/internal/events"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/jobbuilder"
	logr "github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func wireProvisionEvent() events.LifecycleEvent {
	return events.LifecycleEvent{
		Event:       events.EventProvisionWanted,
		RunID:       "cri-237",
		ScopeID:     "scope-wire",
		ScopeTag:    "root-scope",
		AdapterName: "intake",
		AdapterType: "shell",
		Digest:      "deadbeef",
		ShimAddress: "[::]:7778",
		TokenFile:   "/data/intake/CRI-237/remote-tokens/scope-wire/noop.token",
		AcceptToken: "accept-rotate-1",
	}
}

func runnerPod(name string, phase corev1.PodPhase, ip string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels: map[string]string{
				jobbuilder.LabelRun:  "cri-237",
				jobbuilder.LabelRole: jobbuilder.RoleRunner,
			},
		},
		Status: corev1.PodStatus{Phase: phase, PodIP: ip},
	}
}

func containerEnvByName(c corev1.Container) map[string]string {
	env := make(map[string]string, len(c.Env))
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	return env
}

func podVolumeNames(pod corev1.Pod) []string {
	names := make([]string, 0, len(pod.Spec.Volumes))
	for _, v := range pod.Spec.Volumes {
		names = append(names, v.Name)
	}
	return names
}

func listAdapterRolePods(t *testing.T, cl client.Client, namespace string) []corev1.Pod {
	t.Helper()
	var pods corev1.PodList
	require.NoError(t, cl.List(context.Background(), &pods, client.InNamespace(namespace),
		client.MatchingLabels{jobbuilder.LabelRole: jobbuilder.RoleAdapter}))
	return pods.Items
}

func TestReconcilePerScopeAdaptersWireTokenDeliveryResolvesRunnerIP(t *testing.T) {
	run := perScopeTestRun(true)
	run.Name = "cri-237"
	run.UID = types.UID("run-uid")
	r, cl := newPerScopeTestReconciler(t, run, runnerPod("cri-237-runner", corev1.PodRunning, "10.0.0.10"))

	active, err := r.reconcilePerScopeAdapters(context.Background(), run, []events.LifecycleEvent{wireProvisionEvent()}, logr.Discard())
	require.NoError(t, err)
	assert.Equal(t, 1, active)

	pods := listAdapterRolePods(t, cl, "default")
	require.Len(t, pods, 1)
	pod := pods[0]

	env := containerEnvByName(pod.Spec.Containers[0])
	assert.Equal(t, "accept-rotate-1", env["CRITERIA_REMOTE_TOKEN"],
		"the token from the provision event reaches the adapter pod on the wire")
	assert.Equal(t, "10.0.0.10:7778", env["CRITERIA_REMOTE_HOST"],
		"the dial address is the resolved runner pod IP plus the shim listen port")
	_, hasTokenFile := env["CRITERIA_REMOTE_TOKEN_FILE"]
	assert.False(t, hasTokenFile, "no token-file surface on a wire-delivered pod")

	assert.NotContains(t, podVolumeNames(pod), "data",
		"the adapter pod must not mount the shared /data PVC for wire delivery")
}

func TestReconcilePerScopeAdaptersWireTokenDefersWithoutRunnerPod(t *testing.T) {
	run := perScopeTestRun(true)
	run.Name = "cri-237"
	run.UID = types.UID("run-uid")

	// A live adapter pod must stay untouched while the runner IP is
	// unresolved: no deletes, no creates.
	existing := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cri-237-adp-shell-1234567890ab",
			Namespace: "default",
			Labels: map[string]string{
				jobbuilder.LabelRun:  "cri-237",
				jobbuilder.LabelRole: jobbuilder.RoleAdapter,
			},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "adapter-shell"}}},
	}
	r, cl := newPerScopeTestReconciler(t, run, existing)

	active, err := r.reconcilePerScopeAdapters(context.Background(), run, []events.LifecycleEvent{wireProvisionEvent()}, logr.Discard())
	require.NoError(t, err, "deferral is not an error: the caller requeues on the next poll")
	assert.Equal(t, 1, active, "the provision stays active so the poll interval keeps requeueing")

	pods := listAdapterRolePods(t, cl, "default")
	require.Len(t, pods, 1)
	assert.Equal(t, existing.Name, pods[0].Name, "the existing pod must not be churned by the deferral")

	var created corev1.PodList
	require.NoError(t, cl.List(context.Background(), &created, client.MatchingLabels{
		jobbuilder.LabelRole: jobbuilder.RoleAdapter,
	}))
	assert.Len(t, created.Items, 1,
		"no wire-shaped pod may be created before the runner IP resolves")
}

func TestReconcilePerScopeAdaptersLegacyProvisionStaysLegacy(t *testing.T) {
	// Image-mode parity (CRI-237): a provision without accept_token keeps the
	// legacy token-file shape even though a runner pod with an IP exists —
	// the file surface is still the delivery channel for pre-eae0181 engines.
	run := perScopeTestRun(true)
	run.Name = "cri-237"
	run.UID = types.UID("run-uid")
	r, cl := newPerScopeTestReconciler(t, run, runnerPod("cri-237-runner", corev1.PodRunning, "10.0.0.10"))

	active, err := r.reconcilePerScopeAdapters(context.Background(), run, []events.LifecycleEvent{capturedProvisionEvent}, logr.Discard())
	require.NoError(t, err)
	assert.Equal(t, 1, active)

	pods := listAdapterRolePods(t, cl, "default")
	require.Len(t, pods, 1)
	env := containerEnvByName(pods[0].Spec.Containers[0])
	assert.Equal(t, capturedProvisionEvent.TokenFile, env["CRITERIA_REMOTE_TOKEN_FILE"])
	_, hasToken := env["CRITERIA_REMOTE_TOKEN"]
	assert.False(t, hasToken)
	_, hasHost := env["CRITERIA_REMOTE_HOST"]
	assert.False(t, hasHost)
	assert.Contains(t, podVolumeNames(pods[0]), "data",
		"the legacy shape keeps the shared data volume for the token file")
}

func TestReconcilePerScopeAdaptersWirePrefersRunningRunnerPod(t *testing.T) {
	run := perScopeTestRun(true)
	run.Name = "cri-237"
	run.UID = types.UID("run-uid")
	r, _ := newPerScopeTestReconciler(t, run,
		runnerPod("cri-237-runner-restart", corev1.PodPending, "10.0.0.9"),
		runnerPod("cri-237-runner", corev1.PodRunning, "10.0.0.10"))

	ip, err := r.resolveRunnerIP(context.Background(), run)
	require.NoError(t, err)
	assert.Equal(t, "10.0.0.10", ip,
		"a Running runner pod beats a Pending one, regardless of list order")

	// Deterministic re-resolution while the runner restarts.
	ip, err = r.resolveRunnerIP(context.Background(), run)
	require.NoError(t, err)
	assert.Equal(t, "10.0.0.10", ip)
}

func TestResolveRunnerIPEmptyWithoutRunnerPod(t *testing.T) {
	run := perScopeTestRun(true)
	run.Name = "cri-237"
	run.UID = types.UID("run-uid")
	r, _ := newPerScopeTestReconciler(t, run)

	ip, err := r.resolveRunnerIP(context.Background(), run)
	require.NoError(t, err)
	assert.Empty(t, ip, "no runner pod: no IP, and the caller defers wire delivery")
}
