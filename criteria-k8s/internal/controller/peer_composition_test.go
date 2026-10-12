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
