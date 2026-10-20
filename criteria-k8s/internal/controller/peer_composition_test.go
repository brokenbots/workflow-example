package controller

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/brokenbots/workflow-example/criteria-k8s/internal/events"
	logr "github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// graphsPayload builds a WorkflowGraphs payload with one layer whose body
// carries the given adapter bindings (the KB-253 wire shape).
func graphsPayload(t *testing.T, adapters []map[string]any) []byte {
	t.Helper()
	body := map[string]any{"adapters": adapters}
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	payload := map[string]any{
		"subworkflows": []map[string]any{
			{"name": "root", "sourcePath": "root", "body": string(raw)},
		},
	}
	out, err := json.Marshal(payload)
	require.NoError(t, err)
	return out
}

// TestReconcilePerScopeAdaptersComposesGraphMembers pins the KB-253
// composition graph-completion: a provisioned member's environment peer pod
// boots hosting ALSO the graph-declared definitions that have not
// provisioned yet (their digest pins ride the enriched graphs), and a later
// provision of one of those definitions does NOT recreate the pod — the
// child set was complete at boot.
func TestReconcilePerScopeAdaptersComposesGraphMembers(t *testing.T) {
	run := perScopeTestRun(true)
	r, cl := newPerScopeTestReconciler(t, run, groupRunnerPod(run, "10.0.0.10"))

	// Provision observed so far: coordinator only; branch_repair's scope
	// initialises later (pair-loop entry in the live trees).
	prov := groupProvision("coordinator", "copilot", "scope-a", "remote/worktree")
	prov.Digest = "sha256:aaaa"
	graphs := graphsPayload(t, []map[string]any{
		{"type": "copilot", "name": "coordinator", "environment": "remote.worktree", "pinned_digest": "sha256:aaaa"},
		{"type": "copilot", "name": "branch_repair", "environment": "remote.worktree", "pinned_digest": "sha256:bbbb"},
	})

	active, err := r.reconcilePerScopeAdapters(context.Background(), run, "", []events.LifecycleEvent{prov}, graphs, logr.Discard())
	require.NoError(t, err)
	assert.Equal(t, 1, active)

	pods := listAdapterRolePods(t, cl, "default")
	require.Len(t, pods, 1)
	defs := podManifestDefinitions(pods[0])
	assert.ElementsMatch(t, []string{"copilot.coordinator", "copilot.branch_repair"}, defs,
		"the boot manifest is the GRAPH-COMPLETE definition set, not provisions-seen-so-far")

	uid := pods[0].UID

	// branch_repair's provision arrives late: the pod must NOT recreate.
	late := groupProvision("branch_repair", "copilot", "scope-b", "remote/worktree")
	late.Digest = "sha256:bbbb"
	history := append([]events.LifecycleEvent{prov}, late)
	_, err = r.reconcilePerScopeAdapters(context.Background(), run, "", history, graphs, logr.Discard())
	require.NoError(t, err)
	pods = listAdapterRolePods(t, cl, "default")
	require.Len(t, pods, 1)
	assert.Equal(t, uid, pods[0].UID, "a late provision must never recreate the pod (manifest was graph-complete at boot)")
}

// TestReconcilePerScopeAdaptersGraphlessKeepsProvisionShape pins the
// back-compat arm: a nil/empty graphs payload (older engine) composes
// exactly as before — no synthetic members.
func TestReconcilePerScopeAdaptersGraphlessKeepsProvisionShape(t *testing.T) {
	run := perScopeTestRun(true)
	r, cl := newPerScopeTestReconciler(t, run, groupRunnerPod(run, "10.0.0.10"))

	prov := groupProvision("coordinator", "copilot", "scope-a", "remote/worktree")
	prov.Digest = "sha256:aaaa"
	_, err := r.reconcilePerScopeAdapters(context.Background(), run, "", []events.LifecycleEvent{prov}, nil, logr.Discard())
	require.NoError(t, err)
	pods := listAdapterRolePods(t, cl, "default")
	require.Len(t, pods, 1)
	assert.ElementsMatch(t, []string{"copilot.coordinator"}, podManifestDefinitions(pods[0]))
}

// TestReconcilePerScopeAdaptersGraphSourceFeedsDigestStaging pins the
// KB-306 controller arm: a provisioned member (digest present,
// image_reference empty — the live kb-291 shape) is enriched with the
// compiled graph's lockfile source, and the built peer pod's init
// container therefore stages the binary by DIGEST pull — pull-<kind>, not
// the KB-214 copy-<kind> image stage — while the digest pin env still
// carries the event's own digest.
func TestReconcilePerScopeAdaptersGraphSourceFeedsDigestStaging(t *testing.T) {
	run := perScopeTestRun(true)
	r, cl := newPerScopeTestReconciler(t, run, groupRunnerPod(run, "10.0.0.10"))

	digest := "sha256:d9f306c29f4145da8bcc44187c9e4ae0f69ed30db3b3edac6e9b6350469bc635"
	prov := groupProvision("coordinator", "copilot", "scope-a", "remote/worktree")
	prov.Digest = digest
	graphs := graphsPayload(t, []map[string]any{
		{"type": "copilot", "name": "coordinator", "environment": "remote.worktree",
			"source": "ghcr.io/brokenbots/criteria-adapter-copilot", "pinned_digest": digest},
	})

	active, err := r.reconcilePerScopeAdapters(context.Background(), run, "", []events.LifecycleEvent{prov}, graphs, logr.Discard())
	require.NoError(t, err)
	assert.Equal(t, 1, active, "the provision must be active")
	pods := listAdapterRolePods(t, cl, "default")
	require.Len(t, pods, 1)

	require.Len(t, pods[0].Spec.InitContainers, 1)
	ic := pods[0].Spec.InitContainers[0]
	assert.Equal(t, "pull-copilot", ic.Name,
		"the enriched member's pullable pin stages the binary by digest, not the image stage")
	assert.Contains(t, ic.Command[2], "ghcr.io/brokenbots/criteria-adapter-copilot@"+digest,
		"the staging script pulls the graph-published artifact repo at the event's lockfile digest")

	assert.Equal(t, digest, containerEnvValue(pods[0].Spec.Containers[0], "CRITERIA_ADAPTER_COPILOT_COORDINATOR_DIGEST"),
		"the digest pin still carries the EVENT's digest (the runtime authority)")
	assert.Equal(t, "/usr/local/share/criteria-peer-adapters/criteria-adapter-copilot",
		containerEnvValue(pods[0].Spec.Containers[0], "CRITERIA_ADAPTER_COPILOT_COORDINATOR_BINARY"),
		"the binary override points at the digest-staged copy")
}

// TestReconcilePerScopeAdaptersGraphlessDigestLeavesImageSupplied pins the
// enrichment guard: without graphs the member stays reference-less — no
// pullable pin, no digest lane — so the single-kind pod keeps the KB-214
// image-supplied shape (no staging machinery, in-image binary override).
func TestReconcilePerScopeAdaptersGraphlessDigestLeavesImageSupplied(t *testing.T) {
	run := perScopeTestRun(true)
	r, cl := newPerScopeTestReconciler(t, run, groupRunnerPod(run, "10.0.0.10"))

	prov := groupProvision("coordinator", "copilot", "scope-a", "remote/worktree")
	prov.Digest = "sha256:d9f306c29f4145da8bcc44187c9e4ae0f69ed30db3b3edac6e9b6350469bc635"
	_, err := r.reconcilePerScopeAdapters(context.Background(), run, "", []events.LifecycleEvent{prov}, nil, logr.Discard())
	require.NoError(t, err)
	pods := listAdapterRolePods(t, cl, "default")
	require.Len(t, pods, 1)

	assert.Empty(t, pods[0].Spec.InitContainers,
		"no graph source means no pullable pin: a single-kind pod stays image-supplied with no staging machinery")
	assert.Equal(t, "/usr/local/bin/criteria-adapter-copilot",
		containerEnvValue(pods[0].Spec.Containers[0], "CRITERIA_ADAPTER_COPILOT_COORDINATOR_BINARY"),
		"the binary override points at the image's conventional install path")
}
