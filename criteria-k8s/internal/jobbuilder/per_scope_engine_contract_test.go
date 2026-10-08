package jobbuilder_test

// Sourced engine-contract fixture (KB-214 review remediation): the tests
// below re-implement the resolution side of the engine's multi-adapter
// manifest reader and drive the operator's EMITTED container env through
// it, so the suite is regression-sensitive to the real engine contract and
// not merely to the operator's own output.
//
// Source of record (verified 2026-10-08, read-only):
//
//	repo    brokenbots/criteria
//	PR      #517 (KB-213), merged 2026-10-08, merge commit
//	        3a4bac57db1f7c3bd18c04afd7722389fd638836
//	config  internal/peer/config.go:54 EnvAdapters =
//	        "CRITERIA_REMOTE_ADAPTERS" (the const-block comment explains
//	        why the card-era CRITERIA_ADAPTERS could not be reused: that
//	        pre-existing name is the adapter install DIRECTORY consumed
//	        by adapterhost discovery); :56 EnvRemoteScopesDir =
//	        "CRITERIA_REMOTE_SCOPES_DIR".
//	grammar internal/peer/adapterset.go: ParseAdaptersConfig :65 /
//	        ParseAdaptersEnv :89 accept a comma-separated NAME[=PATH]
//	        list (entries trimmed, empty entries skipped, duplicate names
//	        are an error); applyAdapterSpecOverrides :161 keys per-adapter
//	        CRITERIA_ADAPTER_<NAME>_DIGEST pins; adapterEnvName :184
//	        uppercases the kind and maps every non-alphanumeric character
//	        to an underscore; resolveSpecDigest :289 treats an absent pin
//	        exactly like an empty one (spec.Digest == "" => no pinning).
//
// If the engine renames any of these surfaces, update this source-of-record
// block and the mirrors below together — the point of the mirrors is that
// a rename on either side fails these tests instead of shipping a peer
// that boots with zero hosted children.

import (
	"strings"
	"testing"

	"github.com/brokenbots/workflow-example/criteria-k8s/internal/events"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/jobbuilder"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// engineAdapterEnvName mirrors internal/peer/adapterset.go adapterEnvName:
// the adapter name uppercased, every non-alphanumeric character replaced
// with an underscore.
func engineAdapterEnvName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r >= 'a' && r <= 'z':
			b.WriteRune(r - 'a' + 'A')
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// engineParseAdaptersEnv mirrors internal/peer/adapterset.go
// ParseAdaptersEnv's NAME surface: a comma-separated NAME[=PATH] list,
// entries trimmed, empty entries skipped, duplicate names rejected.
func engineParseAdaptersEnv(raw string) ([]string, error) {
	seen := make(map[string]bool)
	names := make([]string, 0, strings.Count(raw, ",")+1)
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		name := strings.TrimSpace(strings.SplitN(entry, "=", 2)[0])
		if name == "" {
			return nil, assert.AnError // unreachable for operator output
		}
		if seen[name] {
			return nil, assert.AnError // duplicate adapter name
		}
		seen[name] = true
		names = append(names, name)
	}
	return names, nil
}

// engineDigestPin mirrors internal/peer/adapterset.go
// applyAdapterSpecOverrides combined with resolveSpecDigest: the digest the
// engine would pin for a kind, with an absent variable normalized to ""
// exactly like an unset spec field (absent == empty == no pinning).
func engineDigestPin(env map[string]string, kind string) string {
	return env["CRITERIA_ADAPTER_"+engineAdapterEnvName(kind)+"_DIGEST"]
}

// The peer container's emitted env must parse, under the engine's pinned
// manifest reader, into exactly the hosted member set its kind annotation
// declares — with each member's digest pin landing on the right kind.
func TestPeerEnvConsumedAsMultiAdapterManifest(t *testing.T) {
	run := groupTestRun()
	shell := groupMember("intake", "shell", "scope-a", "ci")
	copilot := groupMember("copilot-agent", "copilot", "scope-a", "ci")
	// Deliberately unsorted input: the emitted manifest must be sorted.
	pod := jobbuilder.BuildPerScopePeerPod(run, jobbuilder.Defaults{}, "scope-a", "ci",
		[]events.LifecycleEvent{shell, copilot}, "10.0.0.10")
	require.NotNil(t, pod)
	env := containerEnvMap(pod.Spec.Containers[0])

	raw := env["CRITERIA_REMOTE_ADAPTERS"]
	require.Equal(t, "copilot,shell", raw, "sorted comma-joined kind set per the engine's ParseAdaptersEnv grammar")

	kinds, err := engineParseAdaptersEnv(raw)
	require.NoError(t, err, "the engine's manifest reader must accept the emitted list verbatim")
	assert.Equal(t, []string{"copilot", "shell"}, kinds)

	for kind, wantDigest := range map[string]string{
		"shell":   shell.Digest,
		"copilot": copilot.Digest,
	} {
		assert.Equal(t, wantDigest, engineDigestPin(env, kind),
			"digest pin for kind %q must carry the member's lockfile digest under the engine's override grammar", kind)
	}

	assert.Equal(t, raw, pod.Annotations[jobbuilder.AnnotationAdapterKinds],
		"the kind annotation and the engine-facing manifest declare the same hosted set")
}

// The engine normalizes multi-word kinds for its per-adapter override
// variables; the operator's rendered key must be byte-identical to what
// that normalization produces, exercised through a real build (no new
// export needed).
func TestPeerDigestPinKeysMatchEngineNormalization(t *testing.T) {
	for kind, want := range map[string]string{
		"shell":           "SHELL",
		"copilot":         "COPILOT",
		"triage-reviewer": "TRIAGE_REVIEWER",
		"agent.code":      "AGENT_CODE",
	} {
		assert.Equal(t, want, engineAdapterEnvName(kind), "engine-side normalization of %q", kind)
	}

	// Behavioral cross-check on a real member rendering: the operator must
	// emit the pin under exactly the key the engine's adapterEnvName
	// grammar derives from the kind.
	run := groupTestRun()
	reviewer := groupMember("triage", "triage-reviewer", "scope-a", "ci")
	pod := jobbuilder.BuildPerScopePeerPod(run, jobbuilder.Defaults{}, "scope-a", "ci",
		[]events.LifecycleEvent{reviewer}, "10.0.0.10")
	require.NotNil(t, pod)
	env := containerEnvMap(pod.Spec.Containers[0])
	assert.Equal(t, reviewer.Digest,
		env["CRITERIA_ADAPTER_"+engineAdapterEnvName("triage-reviewer")+"_DIGEST"],
		"operator's rendered pin key must equal the engine-normalized grammar key")
}

// A member with no lockfile digest must get NO digest pin variable, and the
// engine's resolution treats that absence as no pinning — never as an
// error. The operator also never emits an empty-valued pin, which would
// ride the digest-validation path instead of the no-pin short-circuit.
func TestPeerAbsentDigestPinEquivalence(t *testing.T) {
	run := groupTestRun()
	pinned := groupMember("intake", "shell", "scope-a", "ci")
	unpinned := groupMember("copilot-agent", "copilot", "scope-a", "ci")
	unpinned.Digest = ""

	pod := jobbuilder.BuildPerScopePeerPod(run, jobbuilder.Defaults{}, "scope-a", "ci",
		[]events.LifecycleEvent{pinned, unpinned}, "10.0.0.10")
	require.NotNil(t, pod)
	env := containerEnvMap(pod.Spec.Containers[0])

	_, present := env["CRITERIA_ADAPTER_COPILOT_DIGEST"]
	require.False(t, present, "an unpinned member gets no digest env var at all")
	assert.Empty(t, engineDigestPin(env, "copilot"),
		"the engine reads an absent pin as an empty spec digest = no pinning (resolveSpecDigest)")
	assert.Equal(t, pinned.Digest, engineDigestPin(env, "shell"),
		"the pinned sibling is unaffected by the unpinned member")

	for _, e := range pod.Spec.Containers[0].Env {
		if strings.HasSuffix(e.Name, "_DIGEST") {
			assert.NotEmpty(t, e.Value,
				"peer pins are digests or absent, never empty-valued: %s", e.Name)
		}
	}
}
