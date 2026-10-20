package jobbuilder

// KB-306 digest staging: unit pins on the staging-decision helpers — the
// digest-pull reference derivation (repo@sha256:...), the shell-safety
// validation every interpolated value must pass, and the rendered staging
// script's structure. The builder-level pod-shape contract lives in
// per_scope_peer_test.go (external package).

import (
	"strings"
	"testing"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/events"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const stageTestDigest = "sha256:d9f306c29f4145da8bcc44187c9e4ae0f69ed30db3b3edac6e9b6350469bc635"

// stageTestRun builds the run peerStageScript derives the runner cache
// home from: source=false renders the image-mode home, source=true the
// per-run source-mode home (JobName falls back to run.Name).
func stageTestRun(source bool) *criteriav1.CriteriaRun {
	run := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{Name: "cri-306", Namespace: "criteria-jobs", UID: "run-uid"},
		Spec:       criteriav1.CriteriaRunSpec{TicketID: "CRI-306"},
	}
	if source {
		run.Spec.WorkflowSource = &criteriav1.RunWorkflowSource{
			Type: "url",
			URL:  "git::https://github.com/brokenbots/workflow-example.git//kanboard_develop_v1",
		}
	}
	return run
}

func stageTestStage() adapterStage {
	return adapterStage{
		kind:    "shell",
		pullRef: "ghcr.io/brokenbots/criteria-adapter-shell@" + stageTestDigest,
	}
}

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
// path (under the image-mode run's home), the sigstore-verified digest
// pull, and the loud image-baked downgrade fallback whose marker honestly
// refuses to claim a staged digest.
func TestPeerStageScriptContractPinsShellShape(t *testing.T) {
	script := peerStageScript(stageTestRun(false), stageTestStage())

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
		"staged_digest=unknown",
		"kind=%s",          // the runtime-filled marker line, literal
		"staged_sha256=%s", // resolved from the content-addressed blob basename
		"CRITERIA_HOME='" + imageRunnerCriteriaHomeOnData + "'",
		"CRITERIA_HOME='" + peerStageContainerCache + "'",
	} {
		assert.Contains(t, script, want)
	}
	assert.NotContains(t, script, ";rm", "no unguarded shell metacharacter reaches the script")
	assert.NotContains(t, script, "staged_sha256="+stageTestDigest,
		"no lane may claim the requested (index) digest as the staged file's hash")
}

// TestPeerStageScriptUsesPerModeRunnerCacheHome pins the fast-path cache
// home derivation: source-mode runs (every develop run since eae0181) read
// the runner's sigstore-verified cache under /data/criteria-home/<job>,
// image-mode runs stay on the legacy /data/criteria — the same predicate
// the runner containers' CRITERIA_HOME env slices use.
func TestPeerStageScriptUsesPerModeRunnerCacheHome(t *testing.T) {
	sourceScript := peerStageScript(stageTestRun(true), stageTestStage())
	sourceHome := sourceCriteriaHome("cri-306")
	assert.Equal(t, "/data/criteria-home/cri-306", sourceHome,
		"JobName falls back to run.Name, so the per-run home is derived from it")
	assert.Contains(t, sourceScript, "CRITERIA_HOME='"+sourceHome+"'",
		"the source-mode fast path reads the per-run cache the source-mode runner populates")
	assert.NotContains(t, sourceScript, imageRunnerCriteriaHomeOnData+"'",
		"the image-mode home must not survive into a source-mode script")

	imageScript := peerStageScript(stageTestRun(false), stageTestStage())
	assert.Contains(t, imageScript, "CRITERIA_HOME='"+imageRunnerCriteriaHomeOnData+"'",
		"the image-mode fast path reads the legacy data-volume cache")
	assert.NotContains(t, imageScript, sourceCriteriaHome("cri-306"),
		"the per-run home must not survive into an image-mode script")
}

// TestPeerStageImageFallbackScriptMarkerHonesty pins the degraded stage's
// marker: the image-baked binary carries no lockfile digest, so the marker
// records staged_digest=unknown and never asserts the requested pin (or
// any hash) as what was staged.
func TestPeerStageImageFallbackScriptMarkerHonesty(t *testing.T) {
	script := peerStageImageFallbackScript("shell", stageScriptPaths{})
	for _, want := range []string{
		"cp -- \"$image_bin\" \"$staged\" && chmod 0755 \"$staged\"",
		"PEER STAGE DOWNGRADE",
		"source=image-baked",
		"staged_digest=unknown",
		peerStageMarkerPath("shell"),
		peerBinBinaryPath("shell"),
	} {
		assert.Contains(t, script, want)
	}
	assert.NotContains(t, script, "digest="+stageTestDigest,
		"the fallback marker must not carry the lockfile digest it did not stage")
	assert.NotContains(t, script, "staged_sha256",
		"the fallback marker must not claim any staged hash")

	// The peerStageScript guard degrades to this exact script when the
	// pullRef loses its digest suffix (unreachable via digestPullRef).
	guardScript := peerStageScript(stageTestRun(false), adapterStage{kind: "shell", pullRef: "ghcr.io/brokenbots/criteria-adapter-shell"})
	assert.Equal(t, script, guardScript,
		"a malformed pullRef renders the honest image-baked fallback, not a script that pulls an empty digest")
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
