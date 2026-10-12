package controller

// KB-214 controller-level regressions: per-scope reconcile groups active
// provisions by environment identity — ONE pod per (scope, environment) with
// ONE container running the criteria peer (the Dockerfile.peer ENTRYPOINT
// shape); the hosted member set rides the engine's multi-adapter manifest;
// adapters NOT sharing an environment stay in separate pods; full per-scope
// teardown is preserved at group granularity; events without env identity
// fall back to the per-adapter pods (pre-CRI-234 shape); and — the peer
// flip's core guarantee — membership changes within a live scope's lifetime
// never delete or recreate the pod.

import (
	"context"
	"strings"
	"testing"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/events"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/jobbuilder"
	logr "github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func groupProvision(adapter, kind, scopeID, environment string) events.LifecycleEvent {
	return events.LifecycleEvent{
		Event:       events.EventProvisionWanted,
		RunID:       "CRI-234",
		ScopeID:     scopeID,
		AdapterName: adapter,
		AdapterType: kind,
		Digest:      "sha256:deadbeef",
		Environment: environment,
		// The fc95449/CRI-234 emission shape: the runner's shim bind
		// address and the static per-run token_ref peer pods derive their
		// discovery root from.
		ShimAddress: "[::]:7778",
		// KB-242 mint shape: token files carry the DEFINITION identity
		// ("<type>.<name>.token"), which the peer's scope scan reads back.
		TokenFile: "/data/.criteria/runs/cri-234/tokens/" + kind + "." + adapter + ".token",
	}
}

// groupRunnerPod seeds the provisioned run's runner pod (RoleRunner) with a
// routable IP: every environment-carrying provision needs the runner IP —
// wire-shaped delivery dials it to deliver the token, and the legacy peer
// shape bakes the dial host from it — so the reconcile defers until a
// runner pod exists. The shared runnerPod helper hard-codes the cri-237
// run label; peer tests run the per-scope fixture run instead.
func groupRunnerPod(run *criteriav1.CriteriaRun, ip string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      run.Name + "-runner",
			Namespace: run.Namespace,
			Labels: map[string]string{
				jobbuilder.LabelRun:  run.Name,
				jobbuilder.LabelRole: jobbuilder.RoleRunner,
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: ip},
	}
}

func groupRelease(adapter, scopeID, environment string) events.LifecycleEvent {
	return events.LifecycleEvent{
		Event:       events.EventRelease,
		RunID:       "CRI-234",
		ScopeID:     scopeID,
		AdapterName: adapter,
		Environment: environment,
	}
}

func containerEnvValue(c corev1.Container, name string) string {
	for _, e := range c.Env {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}

func podContainerKinds(pod corev1.Pod) []string {
	kinds := make([]string, 0, len(pod.Spec.Containers))
	for _, c := range pod.Spec.Containers {
		kinds = append(kinds, containerEnvValue(c, "ADAPTER_KIND"))
	}
	return kinds
}

// podManifestDefinitions returns the hosted kind set a peer pod's single container
// advertises through CRITERIA_REMOTE_ADAPTERS, as a sorted slice.
func podManifestDefinitions(pod corev1.Pod) []string {
	env := containerEnvValue(pod.Spec.Containers[0], "CRITERIA_REMOTE_ADAPTERS")
	if env == "" {
		return nil
	}
	return strings.Split(env, ",")
}

// Same environment -> one pod: two adapters sharing (scope, environment)
// co-locate as ONE peer container's manifest in exactly one pod.
func TestReconcilePerScopeAdaptersGroupsSameEnvironmentIntoOnePeerPod(t *testing.T) {
	run := perScopeTestRun(true)
	r, cl := newPerScopeTestReconciler(t, run, groupRunnerPod(run, "10.0.0.10"))

	events := []events.LifecycleEvent{
		groupProvision("intake", "shell", "scope-a", "ci"),
		groupProvision("review", "copilot", "scope-a", "ci"),
	}

	active, err := r.reconcilePerScopeAdapters(context.Background(), run, "", events, logr.Discard())
	require.NoError(t, err)
	assert.Equal(t, 2, active)

	pods := listAdapterRolePods(t, cl, "default")
	require.Len(t, pods, 1, "exactly ONE pod per environment, hosting every scope's adapters")
	// The per-environment pod name pins the owning job and the environment
	// identity; the scope-id segment is the empty-string hash because the
	// pod spans ALL of the environment's scope instances.
	assert.Equal(t, jobbuilder.PerScopePeerPodName(run, "", "ci"), pods[0].Name)
	require.Len(t, pods[0].Spec.Containers, 1, "exactly ONE peer container per pod")
	assert.Equal(t, jobbuilder.PerScopePeerContainerName, pods[0].Spec.Containers[0].Name)
	assert.ElementsMatch(t, []string{"shell.intake", "copilot.review"}, podManifestDefinitions(pods[0]),
		"the peer container's manifest hosts every adapter DEFINITION sharing the environment (KB-242 per-definition manifest)")
	assert.Equal(t, "ci", pods[0].Labels[jobbuilder.LabelEnvironment])
	assert.Equal(t, "copilot,shell", pods[0].Annotations[jobbuilder.AnnotationAdapterKinds])
	// Scope-set shape: two distinct scope instances of one environment, so
	// identity arrives per scanned token file — no single-scope env vars.
	env := containerEnvByName(pods[0].Spec.Containers[0])
	assert.Empty(t, env["CRITERIA_REMOTE_SCOPE"], "per-environment pods take identity from scanned tokens, not env")
	assert.Equal(t, "10.0.0.10:7778", env["CRITERIA_REMOTE_HOST"])
	assert.Equal(t, "/data/.criteria/runs/cri-234/remote-tokens", env["CRITERIA_REMOTE_SCOPES_DIR"])
	assert.Empty(t, env["CRITERIA_REMOTE_TOKEN"])
	assert.Empty(t, env["CRITERIA_REMOTE_TOKEN_FILE"])
}

// Separate envs -> separate pods: adapters declaring different environments
// never share a pod (co-location is config-declared trust only).
func TestReconcilePerScopeAdaptersSeparatesDifferentEnvironments(t *testing.T) {
	run := perScopeTestRun(true)
	r, cl := newPerScopeTestReconciler(t, run, groupRunnerPod(run, "10.0.0.10"))

	events := []events.LifecycleEvent{
		groupProvision("intake", "shell", "scope-a", "ci"),
		groupProvision("review", "copilot", "scope-a", "prod"),
		groupProvision("audit", "shell", "scope-a", "prod"),
	}

	active, err := r.reconcilePerScopeAdapters(context.Background(), run, "", events, logr.Discard())
	require.NoError(t, err)
	assert.Equal(t, 3, active)

	pods := listAdapterRolePods(t, cl, "default")
	require.Len(t, pods, 2, "one pod per (scope, environment), never fewer")

	byEnv := make(map[string]corev1.Pod, len(pods))
	for _, pod := range pods {
		byEnv[pod.Labels[jobbuilder.LabelEnvironment]] = pod
	}
	require.Contains(t, byEnv, "ci")
	require.Contains(t, byEnv, "prod")

	assert.ElementsMatch(t, []string{"shell.intake"}, podManifestDefinitions(byEnv["ci"]),
		"the ci pod's manifest hosts only its own member")
	assert.ElementsMatch(t, []string{"copilot.review", "shell.audit"}, podManifestDefinitions(byEnv["prod"]),
		"the prod pod's manifest hosts exactly its two same-environment members — no pod mixes adapters of different environments")
	assert.NotEqual(t, byEnv["ci"].Name, byEnv["prod"].Name)
}

// The runner is never a co-tenant of an adapter pod, and the peer container
// carries no command override: the image's ENTRYPOINT runs `criteria peer`
// directly (adapter.sh is never passed through in peer mode).
func TestReconcilePerScopeAdaptersPeerContainerRunsDirectEntrypoint(t *testing.T) {
	run := perScopeTestRun(true)
	r, cl := newPerScopeTestReconciler(t, run, groupRunnerPod(run, "10.0.0.10"))

	events := []events.LifecycleEvent{
		groupProvision("intake", "shell", "scope-a", "ci"),
		groupProvision("review", "copilot", "scope-a", "ci"),
	}

	_, err := r.reconcilePerScopeAdapters(context.Background(), run, "", events, logr.Discard())
	require.NoError(t, err)

	pods := listAdapterRolePods(t, cl, "default")
	require.Len(t, pods, 1)
	require.Len(t, pods[0].Spec.Containers, 1)
	container := pods[0].Spec.Containers[0]
	assert.Nil(t, container.Command,
		"the peer container must run the image's ENTRYPOINT (`criteria peer`) with no command override")
	for _, m := range container.VolumeMounts {
		assert.NotEqual(t, "scripts", m.Name,
			"the peer container never mounts the pod-adapter scripts: it never passes through adapter.sh")
	}
	assert.Equal(t, "adapter", pods[0].Labels[jobbuilder.LabelRole])
}

// Teardown at group granularity: releasing every member of the scope
// removes all of its environment-group pods (full per-scope teardown
// preserved).
func TestReconcilePerScopeAdaptersTeardownRemovesAllEnvironmentGroups(t *testing.T) {
	run := perScopeTestRun(true)
	r, cl := newPerScopeTestReconciler(t, run, groupRunnerPod(run, "10.0.0.10"))

	provisions := []events.LifecycleEvent{
		groupProvision("intake", "shell", "scope-a", "ci"),
		groupProvision("review", "copilot", "scope-a", "prod"),
		groupProvision("audit", "shell", "scope-b", "ci"),
	}
	_, err := r.reconcilePerScopeAdapters(context.Background(), run, "", provisions, logr.Discard())
	require.NoError(t, err)
	require.Len(t, listAdapterRolePods(t, cl, "default"), 2,
		"one pod per environment: ci hosts both ci scopes' members, prod its own")

	// Full per-environment teardown: releases for every member of the ci
	// and prod environments' scope-a instances, delivered over the
	// accumulated history as the castle client does.
	releases := append(append([]events.LifecycleEvent(nil), provisions...),
		groupRelease("intake", "scope-a", "ci"),
		groupRelease("review", "scope-a", "prod"),
	)
	// scope-b's ci member is still active, so the ci pod survives; only
	// prod's environment group fully tears down.
	active, err := r.reconcilePerScopeAdapters(context.Background(), run, "", releases, logr.Discard())
	require.NoError(t, err)
	assert.Equal(t, 1, active, "only scope-b's member stays active")

	pods := listAdapterRolePods(t, cl, "default")
	require.Len(t, pods, 1, "prod's environment group is torn down; ci survives on scope-b")
	assert.Equal(t, jobbuilder.PerScopePeerPodName(run, "", "ci"), pods[0].Name)
}

// KB-214 core guarantee: a partial release (one member goes away while its
// co-tenant stays active) does NOT delete or recreate the peer pod. Adapter
// supervision happens INSIDE the peer container, so membership drift within
// a live scope's lifetime stays podless: the pod's name — a function of the
// (scope, environment) pair alone — is unchanged, which removes the
// drift-recreate path the CRI-234 group pod needed.
func TestReconcilePerScopeAdaptersPeerPodPersistsThroughPartialRelease(t *testing.T) {
	run := perScopeTestRun(true)
	r, cl := newPerScopeTestReconciler(t, run, groupRunnerPod(run, "10.0.0.10"))

	provisions := []events.LifecycleEvent{
		groupProvision("intake", "shell", "scope-a", "ci"),
		groupProvision("review", "copilot", "scope-a", "ci"),
	}
	_, err := r.reconcilePerScopeAdapters(context.Background(), run, "", provisions, logr.Discard())
	require.NoError(t, err)
	pods := listAdapterRolePods(t, cl, "default")
	require.Len(t, pods, 1)
	require.Equal(t, jobbuilder.PerScopePeerPodName(run, "", "ci"), pods[0].Name)

	// Release one member; its co-tenant stays active. The castle client
	// delivers the full accumulated history on every pass.
	history := append(append([]events.LifecycleEvent(nil), provisions...),
		groupRelease("review", "scope-a", "ci"))
	active, err := r.reconcilePerScopeAdapters(context.Background(), run, "", history, logr.Discard())
	require.NoError(t, err)
	assert.Equal(t, 1, active)

	pods = listAdapterRolePods(t, cl, "default")

	// KB-251 manifest drift: the member set IS the peer container's manifest
	// identity — a released member must not stay hosted on a live pod (the
	// stale manifest wedges its future re-provision's dials), and the name
	// never changes, so the reconcile recreates the pod with the fresh
	// manifest. The recreate is async (delete → create on a later pass), so
	// within THIS pass the pod is gone or still the old shape.
	if len(pods) == 1 {
		assert.Equal(t, jobbuilder.PerScopePeerPodName(run, "", "ci"), pods[0].Name,
			"within the same pass the pod keeps its stable name")
	}

	// The next pass completes the recreate: the peer pod exists again with a
	// manifest matching the remaining member.
	_, err = r.reconcilePerScopeAdapters(context.Background(), run, "", history, logr.Discard())
	require.NoError(t, err)
	pods = listAdapterRolePods(t, cl, "default")
	require.Len(t, pods, 1, "the peer pod is recreated for the remaining member")
	assert.ElementsMatch(t, []string{"shell.intake"}, podManifestDefinitions(pods[0]),
		"the recreated manifest hosts exactly the remaining member (KB-251)")
}

// A count-preserving, same-kind membership change in one (scope,
// environment) — member "first"(shell) released while member "second"(shell)
// was provisioned — keeps the peer pod object as well: the name is a
// function of the pair and the manifest, both anchored at pod creation.
func TestReconcilePerScopeAdaptersPeerPodUnchangedBySameKindMemberSwap(t *testing.T) {
	run := perScopeTestRun(true)
	r, cl := newPerScopeTestReconciler(t, run, groupRunnerPod(run, "10.0.0.10"))

	first := groupProvision("first", "shell", "scope-a", "ci")
	first.Digest = "sha256:aaaaaaaa"
	_, err := r.reconcilePerScopeAdapters(context.Background(), run, "", []events.LifecycleEvent{first}, logr.Discard())
	require.NoError(t, err)
	pods := listAdapterRolePods(t, cl, "default")
	require.Len(t, pods, 1)

	// Same pass, same (scope, environment), same kind: release "first" and
	// provision "second". The castle client delivers the full accumulated
	// history on every pass.
	second := groupProvision("second", "shell", "scope-a", "ci")
	second.Digest = "sha256:cccccccc"
	history := append([]events.LifecycleEvent{first},
		groupRelease("first", "scope-a", "ci"), second)

	active, err := r.reconcilePerScopeAdapters(context.Background(), run, "", history, logr.Discard())
	require.NoError(t, err)
	assert.Equal(t, 1, active)

	pods = listAdapterRolePods(t, cl, "default")
	require.Len(t, pods, 1, "the (scope, environment) keeps exactly one pod name")
	// Same-kind swap = the manifest's DEFINITION set changes (shell.first →
	// shell.second): the manifest is the peer's identity, so the pod
	// recreates (async — the old object may persist within this pass).
	_, err = r.reconcilePerScopeAdapters(context.Background(), run, "", history, logr.Discard())
	require.NoError(t, err)
	pods = listAdapterRolePods(t, cl, "default")
	require.Len(t, pods, 1)
	assert.ElementsMatch(t, []string{"shell.second"}, podManifestDefinitions(pods[0]),
		"the manifest hosts exactly the swapped member (KB-251)")
}

// A provision for an OLD-shape group pod (from a rollout behind this
// change) must be gone once the peer pod is desired: the retire path is
// name-based eviction, never a spec comparison.
func TestReconcilePerScopeAdaptersEvictsRetiredGroupPodShape(t *testing.T) {
	run := perScopeTestRun(true)
	r, cl := newPerScopeTestReconciler(t, run, groupRunnerPod(run, "10.0.0.10"))

	// Seed the pre-flip group pod with the retired -adp- name shape
	// (`<job>-adp-<scope-hash>-<env-hash>`); the old builder is deleted, so
	// the retired family is represented by a representative literal name.
	retired := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobbuilder.JobName(run) + "-adp-aa11bb22cc33-112233445567",
			Namespace: run.Namespace,
			Labels: map[string]string{
				jobbuilder.LabelRun:         run.Name,
				jobbuilder.LabelRole:        jobbuilder.RoleAdapter,
				jobbuilder.LabelScopeID:     "scope-a",
				jobbuilder.LabelEnvironment: "ci",
			},
			Annotations: map[string]string{
				jobbuilder.AnnotationAdapterKinds: "copilot,shell",
			},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "adapter-shell", Command: []string{"/opt/criteria-pod-adapter/adapter.sh"}},
			{Name: "adapter-copilot", Command: []string{"/opt/criteria-pod-adapter/adapter.sh"}},
		}},
	}
	require.NoError(t, cl.Create(context.Background(), retired))

	events := []events.LifecycleEvent{
		groupProvision("intake", "shell", "scope-a", "ci"),
		groupProvision("review", "copilot", "scope-a", "ci"),
	}
	_, err := r.reconcilePerScopeAdapters(context.Background(), run, "", events, logr.Discard())
	require.NoError(t, err)

	pods := listAdapterRolePods(t, cl, "default")
	require.Len(t, pods, 1, "the retired group pod is evicted; only the peer pod remains")
	assert.Equal(t, jobbuilder.PerScopePeerPodName(run, "", "ci"), pods[0].Name)
	require.Len(t, pods[0].Spec.Containers, 1)
	assert.Nil(t, pods[0].Spec.Containers[0].Command,
		"the surviving pod is the peer pod, not the retired adapter.sh group pod")
}

// The peer pod is never built before the runner pod has a routable IP: the
// reconcile defers (no creates, no deletes) and builds on the next poll
// once the runner pod exists with an IP. The env-less fallback shape needs
// no runner IP (adapter.sh polls discovery files itself), so it is
// unaffected.
func TestReconcilePerScopeAdaptersPeerGroupDefersUntilRunnerIP(t *testing.T) {
	run := perScopeTestRun(true)
	r, cl := newPerScopeTestReconciler(t, run)

	events := []events.LifecycleEvent{
		groupProvision("intake", "shell", "scope-a", "ci"),
		groupProvision("review", "copilot", "scope-a", "ci"),
	}
	active, err := r.reconcilePerScopeAdapters(context.Background(), run, "", events, logr.Discard())
	require.NoError(t, err, "deferral is not an error: the caller requeues on the next poll")
	assert.Equal(t, 2, active, "the provisions stay active so the poll interval keeps requeueing")
	require.Empty(t, listAdapterRolePods(t, cl, "default"),
		"no peer pod may be created before the runner IP resolves: the direct ENTRYPOINT cannot boot without the baked host")

	// The runner pod appears with a routable IP; the next poll builds the
	// peer pods.
	require.NoError(t, cl.Create(context.Background(), groupRunnerPod(run, "10.0.0.10")))
	active, err = r.reconcilePerScopeAdapters(context.Background(), run, "", events, logr.Discard())
	require.NoError(t, err)
	assert.Equal(t, 2, active)

	pods := listAdapterRolePods(t, cl, "default")
	require.Len(t, pods, 1)
	assert.Equal(t, jobbuilder.PerScopePeerPodName(run, "", "ci"), pods[0].Name)
	env := containerEnvByName(pods[0].Spec.Containers[0])
	assert.Equal(t, "10.0.0.10:7778", env["CRITERIA_REMOTE_HOST"],
		"the deferred build bakes the runner dial host once the IP resolves")
}

// Backward-compat: events without environment identity (pre-fc95449
// engines) produce the per-adapter pods with the pre-CRI-234 names and
// shapes, one pod per adapter.
func TestReconcilePerScopeAdaptersEnvLessEventsFallBackToPerAdapterPods(t *testing.T) {
	run := perScopeTestRun(true)
	r, cl := newPerScopeTestReconciler(t, run)

	envLess := []events.LifecycleEvent{
		capturedProvisionEvent,
		{Event: events.EventProvisionWanted, RunID: "58f12fe6-5144-4438-8e99-13f701be454c",
			ScopeID: "13d83326-f18d-49ed-942d-29c5f291a305", AdapterName: "copilot"},
	}

	active, err := r.reconcilePerScopeAdapters(context.Background(), run, "", envLess, logr.Discard())
	require.NoError(t, err)
	assert.Equal(t, 2, active)

	pods := listAdapterRolePods(t, cl, "default")
	require.Len(t, pods, 2, "env-less events keep one pod per adapter")
	for _, pod := range pods {
		assert.Empty(t, pod.Labels[jobbuilder.LabelEnvironment],
			"fallback pods must not carry the environment label")
		assert.Equal(t, "13d83326-f18d-49ed-942d-29c5f291a305", pod.Labels[jobbuilder.LabelScopeID])
		require.Len(t, pod.Spec.Containers, 1, "one container per fallback pod")
		assert.Regexp(t, `-adp-(default|copilot)-[0-9a-f]{12}$`, pod.Name,
			"fallback pod names match the pre-CRI-234 <job>-adp-<kind>-<scopehash> shape")
	}
}

// A mixed history — an env-less event and an environment-carrying event for
// the same scope — reconciles to one fallback pod plus one peer pod: the
// grouping only applies where env identity exists.
func TestReconcilePerScopeAdaptersMixedShapesStaySeparate(t *testing.T) {
	run := perScopeTestRun(true)
	r, cl := newPerScopeTestReconciler(t, run, groupRunnerPod(run, "10.0.0.10"))

	events := []events.LifecycleEvent{
		{Event: events.EventProvisionWanted, RunID: "CRI-234", ScopeID: "scope-a", AdapterName: "legacy", AdapterType: "shell"},
		groupProvision("intake", "copilot", "scope-a", "ci"),
	}

	active, err := r.reconcilePerScopeAdapters(context.Background(), run, "", events, logr.Discard())
	require.NoError(t, err)
	assert.Equal(t, 2, active)

	pods := listAdapterRolePods(t, cl, "default")
	require.Len(t, pods, 2, "the env-less adapter keeps its own pod; the env-carrying adapter gets the peer pod")

	fallbackSeen, peerSeen := false, false
	for _, pod := range pods {
		switch {
		case pod.Labels[jobbuilder.LabelEnvironment] == "":
			fallbackSeen = true
			assert.Len(t, pod.Spec.Containers, 1)
			assert.Equal(t, "shell", pod.Labels[jobbuilder.LabelAdapterKind])
			assert.Equal(t, []string{"/opt/criteria-pod-adapter/adapter.sh"}, pod.Spec.Containers[0].Command,
				"the fallback pod keeps the legacy adapter.sh entrypoint")
		default:
			peerSeen = true
			assert.Equal(t, "ci", pod.Labels[jobbuilder.LabelEnvironment])
		}
	}
	assert.True(t, fallbackSeen, "the per-adapter fallback pod is present")
	assert.True(t, peerSeen, "the environment peer pod is present")
}

// The reconcile is idempotent under the peer grouping: repeated passes keep
// the existing peer pods instead of recreating them.
func TestReconcilePerScopeAdaptersGroupedIdempotent(t *testing.T) {
	run := perScopeTestRun(true)
	r, cl := newPerScopeTestReconciler(t, run, groupRunnerPod(run, "10.0.0.10"))

	events := []events.LifecycleEvent{
		groupProvision("intake", "shell", "scope-a", "ci"),
		groupProvision("review", "copilot", "scope-a", "ci"),
	}

	_, err := r.reconcilePerScopeAdapters(context.Background(), run, "", events, logr.Discard())
	require.NoError(t, err)
	uid := func() string {
		pods := listAdapterRolePods(t, cl, "default")
		require.Len(t, pods, 1)
		return string(pods[0].UID)
	}()

	for i := 0; i < 2; i++ {
		_, err = r.reconcilePerScopeAdapters(context.Background(), run, "", events, logr.Discard())
		require.NoError(t, err)
		pods := listAdapterRolePods(t, cl, "default")
		require.Len(t, pods, 1)
		assert.Equal(t, uid, string(pods[0].UID), "repeat reconciles must keep the existing peer pod")
	}
}

// Verbatim fc95449-shaped provision_wanted emissions (CRI-233): the pinned
// engine carries the environment identity as the environment_type /
// environment_name pair in payload.data — never as an "environment" key
// (internal/run/sink.go). Mirrors the v0522ProvisionWantedJSON convention.
const (
	fcWorktreeShellIntake = `{"schema_version":1,"seq":1,"run_id":"CRI-234","payload_type":"AdapterEvent","payload":{"adapter":"intake","kind":"adapter.lifecycle.provision_wanted","data":{"adapter":"intake","adapter_type":"shell","digest":"sha256:d9f306c29f4145da8bcc44187c9e4ae0f69ed30db3b3edac6e9b6350469bc635","environment_name":"worktree","environment_type":"remote","run_id":"","scope_instance_id":"9f1d3c2b-6a4e-4f8a-9c1d-3e7b5a2f0d46","scope_name":"","shim_listen_address":"[::]:7778","token_ref":"/data/.criteria/runs/cri-234/tokens/intake.token"}}}`

	fcWorktreeCopilotReview = `{"schema_version":1,"seq":2,"run_id":"CRI-234","payload_type":"AdapterEvent","payload":{"adapter":"review","kind":"adapter.lifecycle.provision_wanted","data":{"adapter":"review","adapter_type":"copilot","digest":"sha256:8f2b0ba4c2c3d0e5b0b0a6e2b3f0e2a1b0a2a6e2b3f0e2a1b0a2a6e2b3f0e2a1","environment_name":"worktree","environment_type":"remote","run_id":"","scope_instance_id":"9f1d3c2b-6a4e-4f8a-9c1d-3e7b5a2f0d46","scope_name":"","shim_listen_address":"[::]:7778","token_ref":"/data/.criteria/runs/cri-234/tokens/review.token"}}}`

	fcProdCopilotReview = `{"schema_version":1,"seq":3,"run_id":"CRI-234","payload_type":"AdapterEvent","payload":{"adapter":"review","kind":"adapter.lifecycle.provision_wanted","data":{"adapter":"review","adapter_type":"copilot","digest":"sha256:8f2b0ba4c2c3d0e5b0b0a6e2b3f0e2a1b0a2a6e2b3f0e2a1b0a2a6e2b3f0e2a1","environment_name":"prod","environment_type":"remote","run_id":"","scope_instance_id":"9f1d3c2b-6a4e-4f8a-9c1d-3e7b5a2f0d46","scope_name":"","shim_listen_address":"[::]:7778","token_ref":"/data/.criteria/runs/cri-234/tokens/review.token"}}}`
)

// The pinned engine's verbatim emission shape (criteria fc95449, CRI-233)
// carries the environment identity as the environment_type /
// environment_name pair in payload.data. The reconcile must group the parsed
// stream by the derived "type/name" identity: two adapters sharing one
// environment co-locate in exactly one peer pod.
func TestReconcilePerScopeAdaptersGroupsFromVerbatimFC95449Stream(t *testing.T) {
	run := perScopeTestRun(true)
	r, cl := newPerScopeTestReconciler(t, run, groupRunnerPod(run, "10.0.0.10"))

	stream := strings.Join([]string{fcWorktreeShellIntake, fcWorktreeCopilotReview}, "\n")
	parsed, err := events.ParseLifecycleEventsBytes([]byte(stream))
	require.NoError(t, err)
	require.Len(t, parsed, 2)
	for _, ev := range parsed {
		assert.Equal(t, "remote/worktree", ev.Environment,
			"the fc95449 emission keys must form the co-location identity")
	}

	active, err := r.reconcilePerScopeAdapters(context.Background(), run, "", events.ActiveProvisions(parsed), logr.Discard())
	require.NoError(t, err)
	assert.Equal(t, 2, active)

	pods := listAdapterRolePods(t, cl, "default")
	require.Len(t, pods, 1, "one environment -> exactly one peer pod")
	assert.Equal(t, jobbuilder.PerScopePeerPodName(run, "", "remote/worktree"), pods[0].Name)
	require.Len(t, pods[0].Spec.Containers, 1)
	assert.ElementsMatch(t, []string{"shell.intake", "copilot.review"}, podManifestDefinitions(pods[0]),
		"the peer container's manifest hosts every adapter DEFINITION sharing the environment (KB-242 per-definition manifest)")
	assert.Equal(t, "copilot,shell", pods[0].Annotations[jobbuilder.AnnotationAdapterKinds])
	assert.Equal(t, "remote-worktree", pods[0].Labels[jobbuilder.LabelEnvironment])
}

// Two different (type, name) pairs from the pinned engine's verbatim
// emission shape produce two distinct peer pods; no pod mixes adapters of
// different environments.
func TestReconcilePerScopeAdaptersSeparatesVerbatimFC95449EnvironmentPairs(t *testing.T) {
	run := perScopeTestRun(true)
	r, cl := newPerScopeTestReconciler(t, run, groupRunnerPod(run, "10.0.0.10"))

	stream := strings.Join([]string{fcWorktreeShellIntake, fcProdCopilotReview}, "\n")
	parsed, err := events.ParseLifecycleEventsBytes([]byte(stream))
	require.NoError(t, err)
	require.Len(t, parsed, 2)
	require.NotEqual(t, parsed[0].Environment, parsed[1].Environment,
		"different (type,name) pairs parse to distinct identities")

	active, err := r.reconcilePerScopeAdapters(context.Background(), run, "", events.ActiveProvisions(parsed), logr.Discard())
	require.NoError(t, err)
	assert.Equal(t, 2, active)

	pods := listAdapterRolePods(t, cl, "default")
	require.Len(t, pods, 2, "one peer pod per (scope, environment) pair")

	byEnv := make(map[string]corev1.Pod, len(pods))
	for _, pod := range pods {
		byEnv[pod.Labels[jobbuilder.LabelEnvironment]] = pod
	}
	require.Contains(t, byEnv, "remote-worktree")
	require.Contains(t, byEnv, "remote-prod")
	assert.ElementsMatch(t, []string{"shell.intake"}, podManifestDefinitions(byEnv["remote-worktree"]),
		"the worktree pod's manifest hosts only its own member")
	assert.ElementsMatch(t, []string{"copilot.review"}, podManifestDefinitions(byEnv["remote-prod"]),
		"the prod pod's manifest hosts only its own member — adapters of different environments never mix")
	assert.NotEqual(t, byEnv["remote-worktree"].Name, byEnv["remote-prod"].Name)
}

// The pre-fc95449 verbatim emission shape (CRI-132 capture) carries no
// environment pair, so the reconcile keeps the per-adapter fallback pod
// instead of the peer shape.
func TestReconcilePerScopeAdaptersVerbatimPreFC95449StreamFallsBack(t *testing.T) {
	run := perScopeTestRun(true)
	r, cl := newPerScopeTestReconciler(t, run)

	parsed, err := events.ParseLifecycleEventsBytes([]byte(cri132CapturedNestedProvision))
	require.NoError(t, err)
	require.Len(t, parsed, 1)
	assert.Empty(t, parsed[0].Environment, "the pre-fc95449 emission carries no environment pair")

	active, err := r.reconcilePerScopeAdapters(context.Background(), run, "", events.ActiveProvisions(parsed), logr.Discard())
	require.NoError(t, err)
	assert.Equal(t, 1, active)

	pods := listAdapterRolePods(t, cl, "default")
	require.Len(t, pods, 1)
	assert.Empty(t, pods[0].Labels[jobbuilder.LabelEnvironment],
		"env-less events keep the per-adapter fallback shape")
	assert.Empty(t, pods[0].Annotations[jobbuilder.AnnotationAdapterKinds])
	assert.Regexp(t, `-adp-default-[0-9a-f]{12}$`, pods[0].Name)
}
