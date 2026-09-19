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
	"strings"
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
	assert.ElementsMatch(t, []string{"shell", "copilot"}, podContainerKinds(pods[0]),
		"each adapter sharing the environment runs as a separate container")
	assert.Equal(t, "ci", pods[0].Labels[jobbuilder.LabelEnvironment])
	assert.Equal(t, "copilot,shell", pods[0].Annotations[jobbuilder.AnnotationAdapterKinds])
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

	assert.ElementsMatch(t, []string{"shell"}, podContainerKinds(byEnv["ci"]),
		"the ci pod hosts only its own member")
	assert.ElementsMatch(t, []string{"copilot", "shell"}, podContainerKinds(byEnv["prod"]),
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
	assert.ElementsMatch(t, []string{"shell"}, podContainerKinds(pods[0]),
		"the released member's container must not linger: pod container sets are immutable, so the group is recreated")

	// The recreated pod keeps the same (scope, environment) identity.
	assert.Equal(t, jobbuilder.PerScopeAdapterGroupName(run, "scope-a", "ci"), pods[0].Name)

	// The reverse drift — re-provisioning the released member — restores
	// the two-container shape.
	_, err = r.reconcilePerScopeAdapters(context.Background(), run, provisions, logr.Discard())
	require.NoError(t, err)
	pods = listAdapterPods(t, cl, "default")
	require.Len(t, pods, 1)
	assert.ElementsMatch(t, []string{"shell", "copilot"}, podContainerKinds(pods[0]))
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

// Review R1 regression: a count-preserving, same-kind membership change in
// one (scope, environment) — member "first"(shell) released while member
// "second"(shell) is provisioned in the same pass — is invisible to a
// kind-derived container NAME set. The group pod must still be recreated so
// the newly provisioned member gets a container and the released member's
// handshake env (stale digest against a deregistered shim) does not linger.
func TestReconcilePerScopeAdaptersRecreatesGroupAfterSameKindMemberSwap(t *testing.T) {
	run := perScopeTestRun(true)
	r, cl := newPerScopeTestReconciler(t, run)

	first := groupProvision("first", "shell", "scope-a", "ci")
	first.Digest = "sha256:aaaaaaaa"
	_, err := r.reconcilePerScopeAdapters(context.Background(), run, []events.LifecycleEvent{first}, logr.Discard())
	require.NoError(t, err)
	pods := listAdapterPods(t, cl, "default")
	require.Len(t, pods, 1)
	require.Len(t, pods[0].Spec.Containers, 1)
	assert.Equal(t, "sha256:aaaaaaaa",
		containerEnvValue(pods[0].Spec.Containers[0], "CRITERIA_REMOTE_DIGEST"))
	beforeUID := string(pods[0].UID)

	// Same pass, same (scope, environment), same kind, same container count:
	// release "first" and provision "second". The castle client delivers the
	// full accumulated history on every pass.
	second := groupProvision("second", "shell", "scope-a", "ci")
	second.Digest = "sha256:cccccccc"
	history := append([]events.LifecycleEvent{first},
		groupRelease("first", "scope-a", "ci"), second)

	active, err := r.reconcilePerScopeAdapters(context.Background(), run, history, logr.Discard())
	require.NoError(t, err)
	assert.Equal(t, 1, active)

	pods = listAdapterPods(t, cl, "default")
	require.Len(t, pods, 1, "the (scope, environment) keeps exactly one pod")
	assert.Equal(t, jobbuilder.PerScopeAdapterGroupName(run, "scope-a", "ci"), pods[0].Name)
	if beforeUID != "" {
		assert.NotEqual(t, beforeUID, string(pods[0].UID),
			"the group pod must be recreated: pod container sets are immutable, and the name set alone cannot detect a count-preserving same-kind member swap")
	}
	require.Len(t, pods[0].Spec.Containers, 1)
	digest := containerEnvValue(pods[0].Spec.Containers[0], "CRITERIA_REMOTE_DIGEST")
	assert.Equal(t, "sha256:cccccccc", digest,
		"the newly provisioned member's handshake env must be in place")
	assert.NotEqual(t, "sha256:aaaaaaaa", digest,
		"no container carrying the released member's handshake env may survive")
	assert.ElementsMatch(t, []string{"shell"}, podContainerKinds(pods[0]),
		"the same-kind swap preserves the container count and kind set")
}

// Verbatim fc95449-shaped provision_wanted emissions (CRI-233): the pinned
// engine carries the environment identity as the environment_type /
// environment_name pair in payload.data — never as an "environment" key
// (internal/run/sink.go). Mirrors the v0522ProvisionWantedJSON convention.
const (
	fc95449WorktreeShellIntake = `{"schema_version":1,"seq":1,"run_id":"CRI-234","payload_type":"AdapterEvent","payload":{"adapter":"intake","kind":"adapter.lifecycle.provision_wanted","data":{"adapter":"intake","adapter_type":"shell","digest":"sha256:d9f306c29f4145da8bcc44187c9e4ae0f69ed30db3b3edac6e9b6350469bc635","environment_name":"worktree","environment_type":"remote","run_id":"","scope_instance_id":"9f1d3c2b-6a4e-4f8a-9c1d-3e7b5a2f0d46","scope_name":"","shim_listen_address":"[::]:7778","token_ref":"/data/.criteria/runs/cri-234/tokens/intake.token"}}}`

	fc95449WorktreeCopilotReview = `{"schema_version":1,"seq":2,"run_id":"CRI-234","payload_type":"AdapterEvent","payload":{"adapter":"review","kind":"adapter.lifecycle.provision_wanted","data":{"adapter":"review","adapter_type":"copilot","digest":"sha256:8f2b0ba4c2c3d0e5b0b0a6e2b3f0e2a1b0a2a6e2b3f0e2a1b0a2a6e2b3f0e2a1","environment_name":"worktree","environment_type":"remote","run_id":"","scope_instance_id":"9f1d3c2b-6a4e-4f8a-9c1d-3e7b5a2f0d46","scope_name":"","shim_listen_address":"[::]:7778","token_ref":"/data/.criteria/runs/cri-234/tokens/review.token"}}}`

	fc95449ProdCopilotReview = `{"schema_version":1,"seq":3,"run_id":"CRI-234","payload_type":"AdapterEvent","payload":{"adapter":"review","kind":"adapter.lifecycle.provision_wanted","data":{"adapter":"review","adapter_type":"copilot","digest":"sha256:8f2b0ba4c2c3d0e5b0b0a6e2b3f0e2a1b0a2a6e2b3f0e2a1b0a2a6e2b3f0e2a1","environment_name":"prod","environment_type":"remote","run_id":"","scope_instance_id":"9f1d3c2b-6a4e-4f8a-9c1d-3e7b5a2f0d46","scope_name":"","shim_listen_address":"[::]:7778","token_ref":"/data/.criteria/runs/cri-234/tokens/review.token"}}}`
)

// Review B1 regression: the pinned engine's verbatim emission shape
// (criteria fc95449, CRI-233) carries the environment identity as the
// environment_type / environment_name pair in payload.data. The reconcile
// must group the parsed stream by the derived "type/name" identity: two
// adapters sharing one environment co-locate in exactly one group pod.
func TestReconcilePerScopeAdaptersGroupsFromVerbatimFC95449Stream(t *testing.T) {
	run := perScopeTestRun(true)
	r, cl := newPerScopeTestReconciler(t, run)

	stream := strings.Join([]string{fc95449WorktreeShellIntake, fc95449WorktreeCopilotReview}, "\n")
	parsed, err := events.ParseLifecycleEventsBytes([]byte(stream))
	require.NoError(t, err)
	require.Len(t, parsed, 2)
	for _, ev := range parsed {
		assert.Equal(t, "remote/worktree", ev.Environment,
			"the fc95449 emission keys must form the co-location identity")
	}

	active, err := r.reconcilePerScopeAdapters(context.Background(), run, events.ActiveProvisions(parsed), logr.Discard())
	require.NoError(t, err)
	assert.Equal(t, 2, active)

	pods := listAdapterPods(t, cl, "default")
	require.Len(t, pods, 1, "one (scope, environment) -> exactly one group pod")
	assert.Equal(t, jobbuilder.PerScopeAdapterGroupName(run, "9f1d3c2b-6a4e-4f8a-9c1d-3e7b5a2f0d46", "remote/worktree"), pods[0].Name)
	assert.ElementsMatch(t, []string{"shell", "copilot"}, podContainerKinds(pods[0]),
		"each adapter sharing the environment runs as a separate container")
	assert.Equal(t, "copilot,shell", pods[0].Annotations[jobbuilder.AnnotationAdapterKinds])
	assert.Equal(t, "remote-worktree", pods[0].Labels[jobbuilder.LabelEnvironment])
}

// Review B1 regression: two different (type, name) pairs from the pinned
// engine's verbatim emission shape produce two distinct group pods; no pod
// mixes adapters of different environments.
func TestReconcilePerScopeAdaptersSeparatesVerbatimFC95449EnvironmentPairs(t *testing.T) {
	run := perScopeTestRun(true)
	r, cl := newPerScopeTestReconciler(t, run)

	stream := strings.Join([]string{fc95449WorktreeShellIntake, fc95449ProdCopilotReview}, "\n")
	parsed, err := events.ParseLifecycleEventsBytes([]byte(stream))
	require.NoError(t, err)
	require.Len(t, parsed, 2)
	require.NotEqual(t, parsed[0].Environment, parsed[1].Environment,
		"different (type,name) pairs parse to distinct identities")

	active, err := r.reconcilePerScopeAdapters(context.Background(), run, events.ActiveProvisions(parsed), logr.Discard())
	require.NoError(t, err)
	assert.Equal(t, 2, active)

	pods := listAdapterPods(t, cl, "default")
	require.Len(t, pods, 2, "one group pod per (scope, environment) pair")

	byEnv := make(map[string]corev1.Pod, len(pods))
	for _, pod := range pods {
		byEnv[pod.Labels[jobbuilder.LabelEnvironment]] = pod
	}
	require.Contains(t, byEnv, "remote-worktree")
	require.Contains(t, byEnv, "remote-prod")
	assert.ElementsMatch(t, []string{"shell"}, podContainerKinds(byEnv["remote-worktree"]),
		"the worktree pod hosts only its own member")
	assert.ElementsMatch(t, []string{"copilot"}, podContainerKinds(byEnv["remote-prod"]),
		"the prod pod hosts only its own member — adapters of different environments never mix")
	assert.NotEqual(t, byEnv["remote-worktree"].Name, byEnv["remote-prod"].Name)
}

// Review B1 regression: the pre-fc95449 verbatim emission shape (CRI-132
// capture) carries no environment pair, so the reconcile keeps the
// per-adapter fallback pod instead of the group shape.
func TestReconcilePerScopeAdaptersVerbatimPreFC95449StreamFallsBack(t *testing.T) {
	run := perScopeTestRun(true)
	r, cl := newPerScopeTestReconciler(t, run)

	parsed, err := events.ParseLifecycleEventsBytes([]byte(cri132CapturedNestedProvision))
	require.NoError(t, err)
	require.Len(t, parsed, 1)
	assert.Empty(t, parsed[0].Environment, "the pre-fc95449 emission carries no environment pair")

	active, err := r.reconcilePerScopeAdapters(context.Background(), run, events.ActiveProvisions(parsed), logr.Discard())
	require.NoError(t, err)
	assert.Equal(t, 1, active)

	pods := listAdapterPods(t, cl, "default")
	require.Len(t, pods, 1)
	assert.Empty(t, pods[0].Labels[jobbuilder.LabelEnvironment],
		"env-less events keep the per-adapter fallback shape")
	assert.Empty(t, pods[0].Annotations[jobbuilder.AnnotationAdapterKinds])
	assert.Regexp(t, `-adp-default-[0-9a-f]{12}$`, pods[0].Name)
}
