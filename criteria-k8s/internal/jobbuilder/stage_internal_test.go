package jobbuilder

// KB-306 digest staging: unit pins on the staging-decision helpers — the
// digest-pull reference derivation (repo@sha256:...), the shell-safety
// validation every interpolated value must pass, and the rendered staging
// script's structure. The builder-level pod-shape contract lives in
// per_scope_peer_test.go (external package).

import (
	"strings"
	"testing"

	"github.com/brokenbots/workflow-example/criteria-k8s/internal/events"
	"github.com/stretchr/testify/assert"
)

const stageTestDigest = "sha256:d9f306c29f4145da8bcc44187c9e4ae0f69ed30db3b3edac6e9b6350469bc635"

func TestDigestPullRefDerivesDigestAddressedReference(t *testing.T) {
	member := events.LifecycleEvent{
		Digest:         stageTestDigest,
		ImageReference: "ghcr.io/brokenbots/criteria-adapter-shell",
	}
	ref, ok := digestPullRef(member)
	assert.True(t, ok)
	assert.Equal(t, "ghcr.io/brokenbots/criteria-adapter-shell@"+stageTestDigest, ref,
		"the pull is digest-addressed from the bare artifact repo")

	// A pinned event reference (tag, digest suffix) is stripped to the bare
	// repo: the pull must never resolve a different version than the
	// lockfile digest.
	tagged := member
	tagged.ImageReference = "ghcr.io/brokenbots/criteria-adapter-shell:0.5.5"
	ref, ok = digestPullRef(tagged)
	assert.True(t, ok)
	assert.Equal(t, "ghcr.io/brokenbots/criteria-adapter-shell@"+stageTestDigest, ref)

	digested := member
	digested.ImageReference = "ghcr.io/brokenbots/criteria-adapter-shell@sha256:0000"
	ref, ok = digestPullRef(digested)
	assert.True(t, ok)
	assert.Equal(t, "ghcr.io/brokenbots/criteria-adapter-shell@"+stageTestDigest, ref)
}

func TestDigestPullRefRejectsUnpullableMembers(t *testing.T) {
	for name, member := range map[string]events.LifecycleEvent{
		"no digest":         {ImageReference: "ghcr.io/brokenbots/criteria-adapter-shell"},
		"no reference":      {Digest: stageTestDigest},
		"both empty":        {},
		"short digest":      {Digest: "sha256:deadbeef", ImageReference: "ghcr.io/brokenbots/criteria-adapter-shell"},
		"non-hex digest":    {Digest: "sha256:" + strings.Repeat("g", 64), ImageReference: "ghcr.io/brokenbots/criteria-adapter-shell"},
		"empty digest only": {Digest: "sha256:"},
	} {
		t.Run(name, func(t *testing.T) {
			_, ok := digestPullRef(member)
			assert.False(t, ok, "an unpullable member must stay on the image-stage lane")
		})
	}

	// A bare 64-hex digest normalizes like the CRITERIA_ADAPTER_<DEF>_DIGEST
	// pin env does (peerAdapterManifestEnv): both pin lanes accept the same
	// shapes.
	_, ok := digestPullRef(events.LifecycleEvent{
		Digest:         strings.Repeat("a", 64),
		ImageReference: "ghcr.io/brokenbots/criteria-adapter-shell",
	})
	assert.True(t, ok, "a bare 64-hex digest normalizes to sha256: and stays pullable")
}

func TestDigestPullRefRejectsUnsafeReferences(t *testing.T) {
	for name, ref := range map[string]string{
		"whitespace":  "ghcr.io/brokenbots/criteria-adapter-shell ; rm -rf /",
		"shell quote": "ghcr.io/brokenbots/criteria-adapter-shell'$(x)",
		"shell subst": "ghcr.io/brokenbots/criteria-adapter-shell$(x)",
		"backtick":    "ghcr.io/brokenbots/criteria-adapter`x`",
		"semicolon":   "ghcr.io/brokenbots/criteria-adapter;x",
		"redirect":    "ghcr.io/brokenbots/criteria-adapter>x",
		"pipe":        "ghcr.io/brokenbots/criteria-adapter|x",
		"newline":     "ghcr.io/brokenbots/criteria-adapter\nrm -rf /",
		"no registry": "criteria-adapter-shell",
		"tag only":    "criteria-adapter-shell:0.5.5",
	} {
		t.Run(name, func(t *testing.T) {
			_, ok := digestPullRef(events.LifecycleEvent{Digest: stageTestDigest, ImageReference: ref})
			assert.False(t, ok, "a reference outside the registry grammar must never reach the staging script")
		})
	}
}

func TestValidArtifactDigestRequiresSha256Hex64(t *testing.T) {
	assert.True(t, validArtifactDigest(stageTestDigest))
	assert.False(t, validArtifactDigest("sha256:d9f3"), "truncated digest")
	assert.False(t, validArtifactDigest("SHA256:"+strings.Repeat("a", 64)), "uppercase algorithm")
	assert.False(t, validArtifactDigest("sha256:"+strings.ToUpper("d9f306c29f4145da8bcc44187c9e4ae0f69ed30db3b3edac6e9b6350469bc635")), "uppercase hex")
	assert.False(t, validArtifactDigest("sha1:"+strings.Repeat("a", 40)), "wrong algorithm")
	assert.False(t, validArtifactDigest(""))
}

func TestAdapterStagesSortedPerKindFirstMemberDecides(t *testing.T) {
	members := []events.LifecycleEvent{
		{AdapterType: "shell", AdapterName: "intake", Digest: stageTestDigest, ImageReference: "ghcr.io/org/criteria-adapter-shell"},
		{AdapterType: "copilot", AdapterName: "review", Digest: "", ImageReference: ""},
		{AdapterType: "shell", AdapterName: "poll", Digest: "sha256:" + strings.Repeat("b", 64), ImageReference: "other.repo/shell"}, // later member never overrides the kind's first
	}
	stages := adapterStages(members)
	requireStages(t, stages, []adapterStage{
		{kind: "copilot"},
		{kind: "shell", pullRef: "ghcr.io/org/criteria-adapter-shell@" + stageTestDigest},
	})
	assert.True(t, stagesAdapterBinaries(stages), "a digest-staged kind requires staging machinery even single-kind")
}

func TestAdapterStagesImageLaneWhenNoPullablePin(t *testing.T) {
	members := []events.LifecycleEvent{
		{AdapterType: "shell", AdapterName: "intake", Digest: "sha256:deadbeef"},
	}
	stages := adapterStages(members)
	requireStages(t, stages, []adapterStage{{kind: "shell"}})
	assert.False(t, stagesAdapterBinaries(stages), "a single kind without a pullable pin stays image-supplied")
}

// TestPeerStageScriptContractPinsShellShape pins the staged script's
// structure: quoted values (validated upstream), the runner-cache fast
// path, the sigstore-verified digest pull, and the loud image-baked
// downgrade fallback.
func TestPeerStageScriptContractPinsShellShape(t *testing.T) {
	stage := adapterStage{
		kind:    "shell",
		pullRef: "ghcr.io/brokenbots/criteria-adapter-shell@" + stageTestDigest,
	}
	script := peerStageScript(stage)

	for _, want := range []string{
		"set -eu",
		"'shell'",
		"'" + stageTestDigest + "'",
		"'ghcr.io/brokenbots/criteria-adapter-shell@" + stageTestDigest + "'",
		"adapter where \"$digest\"",
		"adapter pull \"$pull_ref\"",
		"PEER STAGE DOWNGRADE",
		"cp -- \"$image_bin\" \"$staged\"",
		peerStageMarkerPath("shell"),
		peerBinBinaryPath("shell"),
		"staged_from=runner-cache",
		"staged_from=pull",
		"source=image-baked",
		"kind=%s", // the runtime-filled marker line, literal
		"CRITERIA_HOME='" + runnerCriteriaHomeOnData + "'",
		"CRITERIA_HOME='" + peerStageContainerCache + "'",
	} {
		assert.Contains(t, script, want)
	}
	assert.NotContains(t, script, ";rm", "no unguarded shell metacharacter reaches the script")
}

func requireStages(t *testing.T, got []adapterStage, want []adapterStage) {
	t.Helper()
	assert.Len(t, got, len(want))
	for i := range want {
		if i >= len(got) {
			return
		}
		assert.Equal(t, want[i].kind, got[i].kind, "stage %d kind", i)
		assert.Equal(t, want[i].pullRef, got[i].pullRef, "stage %d pull ref", i)
	}
}