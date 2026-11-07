package jobbuilder

import (
	"strings"
	"testing"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/stretchr/testify/require"
)

// The KB-385 binding contract, pinned at the derivation site: a declared
// secret's env map keys are the binding env lane names (SCREAMING names
// validated by validateSecretEnv) and the values are the rendered secret
// file keys inside the CSI mount. The derived --var pair set is
// lower(lane)=file:<mountPath>/<fileKey> — the varname must be the
// lowercase tree variable exactly, because the engine binds --var overrides
// by exact name (KB-304 name parity); deriving the name from the mount file
// key instead (the pre-KB-385 defect) silently leaves such lanes unbound
// and the run_handler dies with "missing required input(s)".
func TestSecretVarBindings_NameShapeIsLowercaseLane(t *testing.T) {
	// The live go-dev shape: one CSI mount, classic lanes whose file keys
	// equal their lowercased lane names plus the remaster's upstream lane
	// reusing the workflow_github_token file. Pre-KB-385 the upstream lane
	// emitted NO pair (its file key was already consumed as a varname) and
	// the workflow_upstream_token lane stayed unbound.
	plan := &workflowPlan{secrets: []criteriav1.RunWorkflowSecret{
		{SecretProviderClass: "copilot-spc", MountPath: "/home/criteria/secrets", Env: map[string]string{
			"WORKFLOW_GITHUB_TOKEN":   "workflow_github_token",
			"REVIEWER_GITHUB_TOKEN":   "reviewer_github_token",
			"WORKFLOW_UPSTREAM_TOKEN": "workflow_github_token",
		}},
	}}

	got := secretVarBindings(plan)
	require.Equal(t,
		"reviewer_github_token=file:/home/criteria/secrets/reviewer_github_token\n"+
			"workflow_github_token=file:/home/criteria/secrets/workflow_github_token\n"+
			"workflow_upstream_token=file:/home/criteria/secrets/workflow_github_token",
		got,
		"every secret lane must bind a lowercase --var name to its rendered file")
	getPair := func(substr string) string {
		for _, p := range stringsSplit(got) {
			if strings.Contains(p, substr) {
				return p
			}
		}
		return ""
	}
	require.Empty(t, getPair("WORKFLOW"), "no screaming lane name may reach a --var pair as a varname")
	require.Equal(t, "workflow_upstream_token=file:/home/criteria/secrets/workflow_github_token",
		getPair("workflow_upstream_token="))
}

// The varname side of every pair must be lowercase: the engine's --var
// override lookup is exact-name against the tree's lowercase secret-typed
// variables. Whatever the lane name's spelling, the pair name is
// lower(lane) and the file side is the lane's rendered file key.
func TestSecretVarBindings_AlwaysLowercaseTreeVarNames(t *testing.T) {
	plan := &workflowPlan{secrets: []criteriav1.RunWorkflowSecret{
		{Name: "mixed", MountPath: "/m", Env: map[string]string{
			"WORKFLOW_UPSTREAM_TOKEN": "workflow_upstream_file",
			"kanboard_app_token":      "kanboard_app_token",
		}},
	}}

	for _, line := range stringsSplit(secretVarBindings(plan)) {
		varName := strings.TrimSuffix(line, "="+valueAfterEq(line))
		require.Equal(t, strings.ToLower(varName), varName,
			"varname %q must be lowercase (exact-name engine lookup)", varName)
	}
	require.Contains(t, secretVarBindings(plan),
		"workflow_upstream_token=file:/m/workflow_upstream_file",
		"the file side comes from the env map's value, not the lane name")
}

// The kanboard shape (KB-1: file keys equal the lowercased lane names) must
// keep deriving exactly the same pairs as before the KB-385 change.
func TestSecretVarBindings_KanboardPairsUnchanged(t *testing.T) {
	plan := &workflowPlan{secrets: []criteriav1.RunWorkflowSecret{
		{Name: "kanboard-secrets", MountPath: "/home/criteria/kanboard-secrets", Env: map[string]string{
			"KANBOARD_URL":       "kanboard_url",
			"KANBOARD_APP_TOKEN": "kanboard_app_token",
		}},
	}}

	require.Equal(t,
		"kanboard_app_token=file:/home/criteria/kanboard-secrets/kanboard_app_token\n"+
			"kanboard_url=file:/home/criteria/kanboard-secrets/kanboard_url",
		secretVarBindings(plan))
}

// Two lanes lowering to the same tree varname are deduped deterministically
// (lexicographically first lane wins) no matter how Go enumerates the map.
func TestSecretVarBindings_DedupesLanesToLowercaseDeterministically(t *testing.T) {
	plan := &workflowPlan{secrets: []criteriav1.RunWorkflowSecret{
		{Name: "dup", MountPath: "/m", Env: map[string]string{
			"BB_TOKEN": "bb_file", "aa_token": "aa_file", "AA_TOKEN": "aa_file2",
		}},
	}}

	// ASCII (byte) order puts the SCREAMING spelling first, so its file key
	// wins; what is pinned is determinism — the map order never decides.
	want := "aa_token=file:/m/aa_file2\nbb_token=file:/m/bb_file"
	for i := 0; i < 10; i++ {
		require.Equal(t, want, secretVarBindings(plan), "iteration %d", i)
	}
}

func TestSecretVarBindings_EmptyAndNil(t *testing.T) {
	require.Empty(t, secretVarBindings(nil))
	require.Empty(t, secretVarBindings(&workflowPlan{}))
	require.Empty(t, secretVarBindings(&workflowPlan{secrets: []criteriav1.RunWorkflowSecret{{MountPath: "/m"}}}))
}
