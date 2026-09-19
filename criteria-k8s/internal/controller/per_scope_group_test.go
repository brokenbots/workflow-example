package controller

// CRI-234 M7.2 controller-level regressions: per-scope reconcile groups
// active provisions by environment identity — ONE pod per (scope,
// environment) with each adapter as a separate container; adapters NOT
// sharing an environment stay in separate pods; full per-scope teardown is
// preserved at group granularity; events without env identity fall back to
// the per-adapter pods (pre-CRI-234 shape); a group pod whose membership
// changed is recreated with the remaining members.

import (
	"context"
	"testing"

	"github.com/brokenbots/workflow-example/criteria-k8s/internal/events"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/jobbuilder"
	logr "github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
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

func podContainerNames(pod corev1.Pod) []string {
	names := make([]string, 0, len(pod.Spec.Containers))
	for _, c := range pod.Spec.Containers {
		names = append(names, c.Name)
	}
	return names
}

// Same environment -> one pod: two adapters sharing (scope, environment)
// co-locate as separate containers in exactly one pod.
func TestReconcilePerScopeAdaptersGroupsSameEnvironmentIntoOnePod(t *testing.T) {
	run := perScopeTestRun(true)
	r, cl := newPerScopeTestReconciler(t, run)

	events := []events.LifecycleEvent{
		groupProvision("intake", "shell", "scope-a", "ci"),
		groupProvision("review", "copilot", "scope-a", "ci"),
	}

	active, err := r.reconcilePerScopeAdapters(context.Background(), run, events, logr.Discard())
	require.NoError(t, err)
	assert.Equal(t, 2, active)

	pods := listAdapterPods(t, cl, "default")
	require.Len(t, pods, 1, "exactly ONE pod per (scope, environment)")
	assert.Equal(t, jobbuilder.PerScopeAdapterGroupName(run, "scope-a", "ci"), pods[0].Name)
	assert.ElementsMatch(t, []string{"adapter-shell", "adapter-copilot"}, podContainerNames(pods[0]),
		"each adapter sharing the environment runs as a separate container")
	assert.Equal(t, "ci", pods[0].Labels[jobbuilder.LabelEnvironment])
	assert.Equal(t, "copilot,shell", pods[0].Labels[jobbuilder.LabelAdapterKinds])
	assert.Equal(t, "scope-a", pods[0].Labels[jobbuilder.LabelScopeID])
}

// Separate envs -> separate pods: adapters declaring different environments
// never share a pod (co-location is config-declared trust only).
func TestReconcilePerScopeAdaptersSeparatesDifferentEnvironments(t *testing.T) {
	run := perScopeTestRun(true)
	r, cl := newPerScopeTestReconciler(t, run)

	events := []events.LifecycleEvent{
		groupProvision("intake", "shell", "scope-a", "ci"),
		groupProvision("review", "copilot", "scope-a", "prod"),
		groupProvision("audit", "shell", "scope-a", "prod"),
	}

	active, err := r.reconcilePerScopeAdapters(context.Background(), run, events, logr.Discard())
	require.NoError(t, err)
	assert.Equal(t, 3, active)

	pods := listAdapterPods(t, cl, "default")
	require.Len(t, pods, 2, "one pod per (scope, environment), never fewer")

	byEnv := make(map[string]corev1.Pod, len(pods))
	for _, pod := range pods {
		byEnv[pod.Labels[jobbuilder.LabelEnvironment]] = pod
	}
	require.Contains(t, byEnv, "ci")
	require.Contains(t, byEnv, "prod")

	assert.ElementsMatch(t, []string{"adapter-shell"}, podContainerNames(byEnv["ci"]),
		"the ci pod hosts only its own member")
	assert.ElementsMatch(t, []string{"adapter-copilot", "adapter-shell"}, podContainerNames(byEnv["prod"]),
		"the prod pod hosts exactly its two same-environment members — no pod mixes adapters of different environments")
	assert.NotEqual(t, byEnv["ci"].Name, byEnv["prod"].Name)
}

// The runner is never a co-tenant of an adapter pod: every adapter-labeled
// pod hosts only adapter containers, and the runner job pod carries the
// runner role label.
func TestReconcilePerScopeAdaptersNeverColocatesRunner(t *testing.T) {
	run := perScopeTestRun(true)
	r, cl := newPerScopeTestReconciler(t, run)

	events := []events.LifecycleEvent{
		groupProvision("intake", "shell", "scope-a", "ci"),
		groupProvision("review", "copilot", "scope-a", "ci"),
	}

	_, err := r.reconcilePerScopeAdapters(context.Background(), run, events, logr.Discard())
	require.NoError(t, err)

	pods := listAdapterPods(t, cl, "default")
	require.Len(t, pods, 1)
	for _, c := range pods[0].Spec.Containers {
		assert.Equal(t, []string{"/opt/criteria-pod-adapter/adapter.sh"}, c.Command,
			"every co-tenant of an adapter pod is an adapter container; the runner is never a co-tenant")
	}
	assert.Equal(t, "adapter", pods[0].Labels[jobbuilder.LabelRole])
}

// Teardown at group granularity: releasing every member of the scope
// removes all of its environment-group pods (full per-scope teardown
// preserved).
func TestReconcilePerScopeAdaptersTeardownRemovesAllEnvironmentGroups(t *testing.T) {
	run := perScopeTestRun(true)
	r, cl := newPerScopeTestReconciler(t, run)

	provisions := []events.LifecycleEvent{
		groupProvision("intake", "shell", "scope-a", "ci"),
		groupProvision("review", "copilot", "scope-a", "prod"),
		groupProvision("audit", "shell", "scope-b", "ci"),
	}
	_, err := r.reconcilePerScopeAdapters(context.Background(), run, provisions, logr.Discard())
	require.NoError(t, err)
	require.Len(t, listAdapterPods(t, cl, "default"), 3)

	// Full per-scope teardown: releases for every member of scope-a,
	// delivered over the accumulated history as the castle client does. The
	// release path resolves the adapter before the controller sees it.
	releases := append(append([]events.LifecycleEvent(nil), provisions...),
		groupRelease("intake", "scope-a", "ci"),
		groupRelease("review", "scope-a", "prod"),
	)
	active, err := r.reconcilePerScopeAdapters(context.Background(), run, releases, logr.Discard())
	require.NoError(t, err)
	assert.Equal(t, 1, active, "only scope-b's member stays active")

	pods := listAdapterPods(t, cl, "default")
	require.Len(t, pods, 1, "scope-a's environment groups are fully torn down; scope-b survives")
	assert.Equal(t, jobbuilder.PerScopeAdapterGroupName(run, "scope-b", "ci"), pods[0].Name)
}

// Partial release of a co-located group: the released member's container
// must not linger — the group pod is recreated with the remaining members.
func TestReconcilePerScopeAdaptersRecreatesGroupAfterPartialRelease(t *testing.T) {
	run := perScopeTestRun(true)
	r, cl := newPerScopeTestReconciler(t, run)

	provisions := []events.LifecycleEvent{
		groupProvision("intake", "shell", "scope-a", "ci"),
		groupProvision("review", "copilot", "scope-a", "ci"),
	}
	_, err := r.reconcilePerScopeAdapters(context.Background(), run, provisions, logr.Discard())
	require.NoError(t, err)
	pods := listAdapterPods(t, cl, "default")
	require.Len(t, pods, 1)
	require.Len(t, pods[0].Spec.Containers, 2)

	// Release one member; its co-tenant stays active. The castle client
	// delivers the full accumulated history on every pass.
	history := append(append([]events.LifecycleEvent(nil), provisions...),
		groupRelease("review", "scope-a", "ci"))
	active, err := r.reconcilePerScopeAdapters(context.Background(), run, history, logr.Discard())
	require.NoError(t, err)
	assert.Equal(t, 1, active)

	pods = listAdapterPods(t, cl, "default")
	require.Len(t, pods, 1, "the group pod persists for the remaining member")
	assert.ElementsMatch(t, []string{"adapter-shell"}, podContainerNames(pods[0]),
		"the released member's container must not linger: pod container sets are immutable, so the group is recreated")

	// The recreated pod keeps the same (scope, environment) identity.
	assert.Equal(t, jobbuilder.PerScopeAdapterGroupName(run, "scope-a", "ci"), pods[0].Name)

	// The reverse drift — re-provisioning the released member — restores
	// the two-container shape.
	_, err = r.reconcilePerScopeAdapters(context.Background(), run, provisions, logr.Discard())
	require.NoError(t, err)
	pods = listAdapterPods(t, cl, "default")
	require.Len(t, pods, 1)
	assert.ElementsMatch(t, []string{"adapter-shell", "adapter-copilot"}, podContainerNames(pods[0]))
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

	active, err := r.reconcilePerScopeAdapters(context.Background(), run, envLess, logr.Discard())
	require.NoError(t, err)
	assert.Equal(t, 2, active)

	pods := listAdapterPods(t, cl, "default")
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
// the same scope — reconciles to one fallback pod plus one group pod: the
// grouping only applies where env identity exists.
func TestReconcilePerScopeAdaptersMixedShapesStaySeparate(t *testing.T) {
	run := perScopeTestRun(true)
	r, cl := newPerScopeTestReconciler(t, run)

	events := []events.LifecycleEvent{
		{Event: events.EventProvisionWanted, RunID: "CRI-234", ScopeID: "scope-a", AdapterName: "legacy", AdapterType: "shell"},
		groupProvision("intake", "copilot", "scope-a", "ci"),
	}

	active, err := r.reconcilePerScopeAdapters(context.Background(), run, events, logr.Discard())
	require.NoError(t, err)
	assert.Equal(t, 2, active)

	pods := listAdapterPods(t, cl, "default")
	require.Len(t, pods, 2, "the env-less adapter keeps its own pod; the env-carrying adapter gets the group pod")

	fallbackSeen, groupSeen := false, false
	for _, pod := range pods {
		switch {
		case pod.Labels[jobbuilder.LabelEnvironment] == "":
			fallbackSeen = true
			assert.Len(t, pod.Spec.Containers, 1)
			assert.Equal(t, "shell", pod.Labels[jobbuilder.LabelAdapterKind])
		default:
			groupSeen = true
			assert.Equal(t, "ci", pod.Labels[jobbuilder.LabelEnvironment])
		}
	}
	assert.True(t, fallbackSeen, "the per-adapter fallback pod is present")
	assert.True(t, groupSeen, "the environment group pod is present")
}

// The reconcile is idempotent under the new grouping: repeated passes keep
// the existing group pods instead of recreating them.
func TestReconcilePerScopeAdaptersGroupedIdempotent(t *testing.T) {
	run := perScopeTestRun(true)
	r, cl := newPerScopeTestReconciler(t, run)

	events := []events.LifecycleEvent{
		groupProvision("intake", "shell", "scope-a", "ci"),
		groupProvision("review", "copilot", "scope-a", "ci"),
	}

	_, err := r.reconcilePerScopeAdapters(context.Background(), run, events, logr.Discard())
	require.NoError(t, err)
	uid := func() string {
		pods := listAdapterPods(t, cl, "default")
		require.Len(t, pods, 1)
		return string(pods[0].UID)
	}()

	for i := 0; i < 2; i++ {
		_, err = r.reconcilePerScopeAdapters(context.Background(), run, events, logr.Discard())
		require.NoError(t, err)
		pods := listAdapterPods(t, cl, "default")
		require.Len(t, pods, 1)
		assert.Equal(t, uid, string(pods[0].UID), "repeat reconciles must keep the existing group pod")
	}
}