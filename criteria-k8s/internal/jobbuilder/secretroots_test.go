package jobbuilder

import (
	"testing"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/stretchr/testify/require"
)

func TestTrustedSecretFileRoots_DerivedFromDeclaredSecrets(t *testing.T) {
	plan := &workflowPlan{secrets: []criteriav1.RunWorkflowSecret{
		{Name: "kanboard-secrets", MountPath: "/home/criteria/kanboard-secrets", Env: map[string]string{"KANBOARD_URL": "kanboard_url", "KANBOARD_APP_TOKEN": "kanboard_app_token"}},
		{Name: "github-tokens", MountPath: "/home/criteria/secrets", Env: map[string]string{"WORKFLOW_GITHUB_TOKEN": "workflow_github_token", "REVIEWER_GITHUB_TOKEN": "reviewer_github_token"}},
	}}

	got := trustedSecretFileRoots(plan)
	require.Equal(t, "/home/criteria/kanboard-secrets\n/home/criteria/secrets", got)

	// The root set and the var bindings MUST derive from the same entries:
	// every var's file: origin lives inside one of the declared roots.
	for _, line := range stringsSplit(secretVarBindings(plan)) {
		origin := valueAfterEq(line)
		origin = trimFileScheme(origin)
		require.True(t, rootContains(got, origin), "var origin %q must sit under a CRITERIA_SECRET_FILE_ROOTS entry", origin)
	}
}

func TestTrustedSecretFileRoots_EmptyAndNil(t *testing.T) {
	require.Empty(t, trustedSecretFileRoots(nil))
	require.Empty(t, trustedSecretFileRoots(&workflowPlan{}))
}

func TestTrustedSecretFileRoots_DedupesMountPaths(t *testing.T) {
	plan := &workflowPlan{secrets: []criteriav1.RunWorkflowSecret{
		{Name: "s1", MountPath: "/m", Env: map[string]string{"A": "a"}},
		{Name: "s2", MountPath: "/m", Env: map[string]string{"B": "b"}},
	}}
	require.Equal(t, "/m", trustedSecretFileRoots(plan))
}

// helpers: stringsSplit splits on sep keeping empties; valueAfterEq returns
// everything after the FIRST '='; rootContains reports whether the value has
// a declared root as a path ancestor (or equals one).
func stringsSplit(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

func valueAfterEq(line string) string {
	for i := 0; i < len(line); i++ {
		if line[i] == '=' {
			return line[i+1:]
		}
	}
	return line
}

// trimFileScheme strips a leading "file:" from an origin value so path
// containment compares against the bare filesystem path.
func trimFileScheme(v string) string {
	for i := 0; i+5 <= len(v); i++ {
		if v[i:i+5] == "file:" {
			return v[i+5:]
		}
	}
	return v
}

func rootContains(roots, value string) bool {
	for _, r := range stringsSplit(roots) {
		if r == "" {
			continue
		}
		if r == value {
			return true
		}
		if len(value) > len(r)+1 && value[:len(r)] == r && value[len(r)] == '/' {
			return true
		}
	}
	return false
}
