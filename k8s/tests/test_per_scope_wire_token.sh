#!/usr/bin/env bash
set -euo pipefail

# Regression test for CRI-237: the per-scope adapter pod receives its accept
# token on the wire (pod-spec env from the operator, CRI-236 eae0181
# emission) and must NOT read any rotated token file from the shared
# /data/criteria PVC. The script's wire shape is guarded fail-fast (exit 78)
# when the operator supplies a token without the host/digest pair, since a
# data-volume-less pod cannot poll the discovery directory without spinning
# forever. The legacy pre-eae0181 shape (CRITERIA_REMOTE_TOKEN_FILE on the
# shared volume) must keep working for image-mode runners.
#
# The stubbed criteria-adapter-remote-runner binary captures the exported
# CRITERIA_* environment; a nonexistent CRITERIA_RUN_DIR_ROOT proves zero
# discovery-file reads on the wire path.

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
ADAPTER="$REPO_ROOT/k8s/pod-adapter-adapter.sh"

fail() {
    echo "FAIL: $1" >&2
    exit 1
}

[ -f "$ADAPTER" ] || fail "k8s/pod-adapter-adapter.sh is missing"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

stubbin="$tmp/bin"
mkdir -p "$stubbin"
cat > "$stubbin/criteria-adapter-remote-runner" <<'EOF'
#!/usr/bin/env sh
env | grep '^CRITERIA_' | sort > "$CRITERIA_STUB_OUT"
EOF
chmod +x "$stubbin/criteria-adapter-remote-runner"

# The wire-shaped pod mounts no data volume: point the discovery root at a
# path that does not exist. Any discovery-file read (or poll) would wedge or
# fail the run.
absent_root="$tmp/no-such-run-dir"

run_adapter() {
    local out="$1"
    shift
    env -i PATH="$stubbin:/usr/bin:/bin" HOME="$tmp" CRITERIA_STUB_OUT="$out" "$@" \
        timeout 20 "$ADAPTER" || fail "pod-adapter-adapter.sh did not terminate within 20s (poll loop wedged)"
    [ -s "$out" ] || fail "pod-adapter-adapter.sh never exec'd the adapter binary"
}

# --- part 1: the full wire shape runs with ZERO discovery-file reads.
run_adapter "$tmp/wire-env" \
    ADAPTER_KIND=shell \
    CRITERIA_RUN_JOB_NAME=cri-237 \
    CRITERIA_RUN_DIR_ROOT="$absent_root" \
    CRITERIA_SCOPE_ID=root \
    CRITERIA_SCOPE_TAG=root-scope \
    CRITERIA_REMOTE_HOST=10.0.0.10:7778 \
    CRITERIA_REMOTE_TOKEN=accept-rotate-1 \
    CRITERIA_REMOTE_DIGEST=sha256:deadbeef
resolved="$(cat "$tmp/wire-env")"
printf '%s' "$resolved" | grep -qx 'CRITERIA_REMOTE_TOKEN=accept-rotate-1' || \
    fail "wire delivery must pass the accept token from the event through to the shim channel"
printf '%s' "$resolved" | grep -qx 'CRITERIA_REMOTE_HOST=10.0.0.10:7778' || \
    fail "wire delivery must pass the routable runner dial address through"
printf '%s' "$resolved" | grep -qx 'CRITERIA_REMOTE_DIGEST=sha256:deadbeef' || \
    fail "wire delivery must pass the event digest through"
printf '%s' "$resolved" | grep -q 'CRITERIA_REMOTE_TOKEN_FILE' && \
    fail "the wire shape must not reference a token file"
[ ! -e "$absent_root" ] || fail "the wire path must not touch the discovery directory (zero token-file reads)"

# --- part 2: a token without a host cannot be resolved from discovery —
# fail fast (exit 78) instead of a silent poll-forever wedge.
wire_env_incomplete() {
    local label="$1"
    shift
    set +e
    out="$(env -i PATH="$stubbin:/usr/bin:/bin" HOME="$tmp" \
        ADAPTER_KIND=shell \
        CRITERIA_RUN_JOB_NAME=cri-237 \
        CRITERIA_RUN_DIR_ROOT="$absent_root" \
        CRITERIA_SCOPE_ID=root \
        CRITERIA_REMOTE_TOKEN=accept-rotate-1 \
        "$@" timeout 20 "$ADAPTER" 2>&1)"
    rc=$?
    set -e
    [ "$rc" = "78" ] || fail "$label: expected the wire-shape guard to fail fast with exit 78 (got $rc)"
    printf '%s' "$out" | grep -q 'wire token delivery requires' || \
        fail "$label: the guard failure must name the missing wire inputs"
}

wire_env_incomplete "token without host" CRITERIA_REMOTE_DIGEST=sha256:deadbeef
wire_env_incomplete "token without digest" CRITERIA_REMOTE_HOST=10.0.0.10:7778

# --- part 3: the legacy pre-eae0181 shape keeps working — the token file on
# the shared volume is read when no wire token is supplied.
mkdir -p "$tmp/tokens"
token_file="$tmp/tokens/root-shell"
printf 'legacy-file-token' > "$token_file"
run_adapter "$tmp/legacy-env" \
    ADAPTER_KIND=shell \
    CRITERIA_RUN_JOB_NAME=cri-237 \
    CRITERIA_RUN_DIR_ROOT="$tmp/runs" \
    CRITERIA_SCOPE_ID=root \
    CRITERIA_REMOTE_TOKEN_FILE="$token_file" \
    CRITERIA_REMOTE_HOST=10.0.0.10:7778 \
    CRITERIA_REMOTE_DIGEST=sha256:deadbeef
resolved="$(cat "$tmp/legacy-env")"
printf '%s' "$resolved" | grep -qx 'CRITERIA_REMOTE_TOKEN=legacy-file-token' || \
    fail "the legacy token-file delivery must keep working for pre-eae0181 engines"

echo "PASS: per-scope adapter wire token delivery (CRI-237)"