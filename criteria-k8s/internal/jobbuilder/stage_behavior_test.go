package jobbuilder

// KB-306 review 5590351303 behavior tests: the staging script is EXECUTED
// (not merely spelled) — a stubbed `criteria` on PATH and 0444-only OCI
// blob fixtures exercise the real contract the engine depends on. Every
// lane's staged copy must be EXECUTABLE (`criteria adapter where`
// resolves to the read-only content-addressed blob, mode 0444, and
// `cp --` preserves that mode; the engine execs
// CRITERIA_ADAPTER_<KIND>_BINARY and never repairs it, so an unchmod'ed
// stage means every pod whose member carries a digest cannot boot —
// cp exits 0, the fallback never fires, exec dies with EACCES), and the
// marker's staged_sha256 must be the sha256 of what actually landed.

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeStubCriteria installs a stub `criteria` executable on the injected
// PATH dir: `adapter where <digest>` prints $STUB_WHERE_PATH only when
// $CRITERIA_HOME equals $STUB_WHERE_HOME (emulating the per-home cache
// lookup); `adapter pull` exits per $STUB_PULL_OK (1 = success); anything
// else exits 64 so an unexpected invocation fails its lane loudly.
func writeStubCriteria(t *testing.T) string {
	t.Helper()
	stubDir := filepath.Join(t.TempDir(), "stubbin")
	require.NoError(t, os.MkdirAll(stubDir, 0o755))
	stub := `#!/bin/sh
# dispatched on $2: the script invokes criteria adapter <sub> <args...>
[ "$1" = "adapter" ] || { echo "stub criteria: unexpected invocation: $*" >&2; exit 64; }
case "$2" in
where)
	if [ "$CRITERIA_HOME" = "$STUB_WHERE_HOME" ] && [ -n "$STUB_WHERE_PATH" ]; then
		printf '%s\n' "$STUB_WHERE_PATH"
		exit 0
	fi
	exit 1
	;;
pull)
	if [ "$STUB_PULL_OK" = "1" ]; then
		exit 0
	fi
	echo "stub criteria: pull refused" >&2
	exit 1
	;;
*)
	echo "stub criteria: unexpected invocation: $*" >&2
	exit 64
	;;
esac
`
	require.NoError(t, os.WriteFile(filepath.Join(stubDir, "criteria"), []byte(stub), 0o755))
	return stubDir
}

// writeOCIBlobFixture seeds a criteria home's content-addressed blob store
// exactly as `criteria adapter pull` leaves it: an immutable, READ-ONLY
// 0444 blob at cache/oci/blobs/sha256/<hex of the content> — the layout
// `criteria adapter where <digest>` resolves in reality.
func writeOCIBlobFixture(t *testing.T, home, content string) (blobPath, hexDigest string) {
	t.Helper()
	sum := sha256.Sum256([]byte(content))
	hexDigest = hex.EncodeToString(sum[:])
	blobDir := filepath.Join(home, "cache", "oci", "blobs", "sha256")
	require.NoError(t, os.MkdirAll(blobDir, 0o755))
	blobPath = filepath.Join(blobDir, hexDigest)
	require.NoError(t, os.WriteFile(blobPath, []byte(content), 0o444))
	return blobPath, hexDigest
}

// stageBehaviorFixture is the per-test stage layout: a writable stage dir
// plus a 0755 image-baked source binary. The runner/pull homes and their
// blobs are seeded per test from the fixture.
type stageBehaviorFixture struct {
	stageDir   string
	runnerHome string
	pullHome   string
	imageBin   string
	imageHex   string
	stagedPath string
	markerPath string
	adaptStage adapterStage
}

// newStageBehaviorFixture builds the layout for one kind. The requested
// digest pin is the sha256 of the IMAGE content (arbitrary for the test —
// the script treats it as opaque), so the marker's requested `digest`
// field differs from any staged blob's hash, exactly as the
// index-digest-vs-binary-sha256 distinction prescribes.
func newStageBehaviorFixture(t *testing.T, kind, repo string) *stageBehaviorFixture {
	t.Helper()
	root := t.TempDir()
	f := &stageBehaviorFixture{
		stageDir:   filepath.Join(root, "stage"),
		runnerHome: filepath.Join(root, "runner-home"),
		pullHome:   filepath.Join(root, "pull-home"),
		imageBin:   filepath.Join(root, "image-usr-local-bin", "criteria-adapter-"+kind),
	}
	const imageContent = "#!/bin/sh\necho image-baked-stub\n"
	sum := sha256.Sum256([]byte(imageContent))
	f.imageHex = hex.EncodeToString(sum[:])
	require.NoError(t, os.MkdirAll(filepath.Dir(f.imageBin), 0o755))
	require.NoError(t, os.WriteFile(f.imageBin, []byte(imageContent), 0o755))
	require.NoError(t, os.MkdirAll(f.stageDir, 0o755))
	f.stagedPath = peerBinBinaryPathIn(f.stageDir, kind)
	f.markerPath = peerStageMarkerPathIn(f.stageDir, kind)
	f.adaptStage = adapterStage{
		kind:    kind,
		pullRef: repo + "@sha256:" + f.imageHex,
	}
	return f
}

// runStagingScript executes the rendered script under `sh -ec` with the
// stub `criteria` first on PATH. STUB_* env values override the stub's
// behavior; unset keys default to empty (cache miss / pull refused). The
// overrides are appended AFTER os.Environ() on purpose: Go's exec dedup
// keeps the last occurrence of a key, so this is what makes the stub (not
// the host's real criteria, which would attempt a live network pull) win.
func runStagingScript(t *testing.T, stubBin, script string, env map[string]string) string {
	t.Helper()
	cmd := exec.Command("sh", "-ec", script)
	cmd.Env = append(os.Environ(), "PATH="+stubBin+string(os.PathListSeparator)+"/usr/bin:/bin")
	for _, key := range []string{"STUB_WHERE_HOME", "STUB_WHERE_PATH", "STUB_PULL_OK"} {
		cmd.Env = append(cmd.Env, key+"="+env[key])
	}
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "staging script must succeed; output:\n%s", out)
	return string(out)
}

// readStageMarker parses the staging marker's key=value lines into a map.
func readStageMarker(t *testing.T, path string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	fields := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		key, value, _ := strings.Cut(line, "=")
		fields[key] = value
	}
	return fields
}

// requireExecutableStagedFile asserts the staged copy exists, carries an
// exec bit, and hashes to the given hex digest.
func requireExecutableStagedFile(t *testing.T, path, wantHex string) {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.NotZero(t, info.Mode().Perm()&0o111,
		"the staged binary must be executable (the engine execs it; the source blob is 0444): mode %v", info.Mode())
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	sum := sha256.Sum256(raw)
	assert.Equal(t, wantHex, hex.EncodeToString(sum[:]),
		"the staged file's sha256 must equal the blob the lane actually resolved")
}

// TestPeerStageScriptRunnerCacheLaneStagesExecutable pins the primary
// KB-306 contract END TO END: the runner-cache lane copies the 0444 OCI
// blob out of the run's criteria home and stages an EXECUTABLE binary;
// the marker records the hash of what was staged.
func TestPeerStageScriptRunnerCacheLaneStagesExecutable(t *testing.T) {
	f := newStageBehaviorFixture(t, "shell", "ghcr.io/brokenbots/criteria-adapter-shell")
	blobPath, blobHex := writeOCIBlobFixture(t, f.runnerHome, "adapter-binary-content-shell")
	stubBin := writeStubCriteria(t)

	script := stageScriptWithPaths(stageTestRun(false), f.adaptStage, stageScriptPaths{
		stageDir:   f.stageDir,
		runnerHome: f.runnerHome,
		pullHome:   f.pullHome,
		imageBin:   f.imageBin,
	})
	out := runStagingScript(t, stubBin, script, map[string]string{
		"STUB_WHERE_HOME": f.runnerHome,
		"STUB_WHERE_PATH": blobPath,
	})

	assert.Contains(t, out, "peer-stage: kind=shell digest=sha256:"+f.imageHex)
	assert.Contains(t, out, "source=runner-cache staged_sha256="+blobHex)
	requireExecutableStagedFile(t, f.stagedPath, blobHex)
	marker := readStageMarker(t, f.markerPath)
	assert.Equal(t, "runner-cache", marker["source"])
	assert.Equal(t, blobHex, marker["staged_sha256"],
		"the marker must record the sha256 of the blob the lane actually staged")
	assert.Equal(t, "sha256:"+f.imageHex, marker["digest"],
		"the marker keeps the REQUESTED lockfile pin distinct from what was staged")
	assert.NotEqual(t, marker["digest"], marker["staged_sha256"],
		"a 0444 blob's hash is a binary sha256, never the requested artifact pin")
}

// TestPeerStageScriptPullLaneStagesExecutable pins the pull lane END TO
// END: a runner-cache miss routes into `criteria adapter pull`, and the
// blob the lane resolves after the pull stages EXECUTABLE with
// source=pull and its own staged hash.
func TestPeerStageScriptPullLaneStagesExecutable(t *testing.T) {
	f := newStageBehaviorFixture(t, "copilot", "ghcr.io/brokenbots/criteria-adapter-copilot")
	pullBlobPath, pullHex := writeOCIBlobFixture(t, f.pullHome, "pulled-binary-content-copilot")
	stubBin := writeStubCriteria(t)

	script := stageScriptWithPaths(stageTestRun(true), f.adaptStage, stageScriptPaths{
		stageDir:   f.stageDir,
		runnerHome: f.runnerHome,
		pullHome:   f.pullHome,
		imageBin:   f.imageBin,
	})
	out := runStagingScript(t, stubBin, script, map[string]string{
		// The stub's where only succeeds inside the PULL home: the
		// runner-cache lane misses (nothing seeded there), the pull
		// succeeds, and the post-pull where resolves the seeded blob.
		"STUB_WHERE_HOME": f.pullHome,
		"STUB_WHERE_PATH": pullBlobPath,
		"STUB_PULL_OK":    "1",
	})

	assert.Contains(t, out, "peer-stage: kind=copilot digest=sha256:"+f.imageHex)
	assert.Contains(t, out, "source=pull staged_sha256="+pullHex)

	requireExecutableStagedFile(t, f.stagedPath, pullHex)
	marker := readStageMarker(t, f.markerPath)
	assert.Equal(t, "pull", marker["source"])
	assert.Equal(t, pullHex, marker["staged_sha256"])
}

// TestPeerStageScriptDigestLanesFailDowngradesToImageExecutable pins the
// honest fallback END TO END: both digest lanes miss, the image-baked
// copy stages EXECUTABLE, and the marker claims NO staged hash and NO
// lockfile digest — staged_digest=unknown, with the downgrade said loudly
// on stderr.
func TestPeerStageScriptDigestLanesFailDowngradesToImageExecutable(t *testing.T) {
	f := newStageBehaviorFixture(t, "shell", "ghcr.io/brokenbots/criteria-adapter-shell")
	stubBin := writeStubCriteria(t)

	script := stageScriptWithPaths(stageTestRun(false), f.adaptStage, stageScriptPaths{
		stageDir:   f.stageDir,
		runnerHome: f.runnerHome,
		pullHome:   f.pullHome,
		imageBin:   f.imageBin,
	})
	out := runStagingScript(t, stubBin, script, map[string]string{
		// where never matches a home and pull refuses: both digest lanes
		// miss, the image-baked lane fires.
		"STUB_WHERE_HOME": filepath.Join(f.runnerHome, "no-such-home"),
	})

	assert.Contains(t, out, "PEER STAGE DOWNGRADE",
		"the downgrade must be loud on stderr when the digest pull fails")
	requireExecutableStagedFile(t, f.stagedPath, f.imageHex)
	marker := readStageMarker(t, f.markerPath)
	assert.Equal(t, "image-baked", marker["source"])
	assert.Equal(t, "unknown", marker["staged_digest"],
		"the image-baked marker must never claim the lockfile pin it did not stage")
	assert.NotContains(t, marker, "staged_sha256")
}

// TestPeerStageImageFallbackScriptStagesExecutable executes the
// degraded-only script (the pullRef-guard shape): its staged copy is
// executable and its marker stays honest.
func TestPeerStageImageFallbackScriptStagesExecutable(t *testing.T) {
	f := newStageBehaviorFixture(t, "triage", "ghcr.io/brokenbots/criteria-adapter-triage")
	require.NoError(t, os.MkdirAll(f.stageDir, 0o755))

	script := peerStageImageFallbackScript("triage", stageScriptPaths{
		stageDir: f.stageDir,
		imageBin: f.imageBin,
	})
	out, err := exec.Command("sh", "-ec", script).CombinedOutput()
	require.NoError(t, err, "fallback script must succeed; output:\n%s", out)

	assert.Contains(t, string(out), "PEER STAGE DOWNGRADE")
	requireExecutableStagedFile(t, f.stagedPath, f.imageHex)
	marker := readStageMarker(t, f.markerPath)
	assert.Equal(t, "image-baked", marker["source"])
	assert.Equal(t, "unknown", marker["staged_digest"])
}
