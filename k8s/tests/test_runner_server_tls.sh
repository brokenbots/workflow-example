#!/usr/bin/env bash
set -euo pipefail

# Regression test for CRI-138: in server mode the criteria engine client
# refuses plaintext connections to non-loopback hosts unless TLS mode is
# explicitly disabled, which crash-looped the runner pod at startup
# ("plaintext connections to non-loopback hosts are not allowed by default").
# The runner must export CRITERIA_SERVER_TLS=disable whenever CASTLE_ADDR is
# set, before invoking criteria apply --server, and must leave the variable
# untouched in local file mode (CASTLE_ADDR unset).
#
# The check is behavioral: the runner script is executed in a sandbox with a
# stub criteria binary that captures its environment and argv, once with
# CASTLE_ADDR set and once unset. Structural guards pin both runner.sh copies
# (canonical + chart-embedded) staying in sync.

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

RUNNER_CANONICAL="$REPO_ROOT/k8s/pod-adapter-runner.sh"
RUNNER_CHART="$REPO_ROOT/charts/criteria-k8s/scripts/runner.sh"

fail() {
    echo "FAIL: $1" >&2
    exit 1
}

for f in "$RUNNER_CANONICAL" "$RUNNER_CHART"; do
    [ -f "$f" ] || fail "missing runner script: $f"
done

# The canonical runner and its generated chart copy must stay in sync
# (k8s/generate-pod-adapter-manifest.sh regenerates the chart copy).
cmp -s "$RUNNER_CANONICAL" "$RUNNER_CHART" || \
    fail "charts/criteria-k8s/scripts/runner.sh drifted from k8s/pod-adapter-runner.sh (run k8s/generate-pod-adapter-manifest.sh)"

# Structural guard on the runner scripts: CRITERIA_SERVER_TLS may only be
# exported inside the [ -n "$CASTLE_ADDR" ] server-mode block, with the
# literal disable value.
for f in "$RUNNER_CANONICAL" "$RUNNER_CHART"; do
    # Count on comment-stripped code: explanatory comments legitimately
    # mention the variable (see the events-file regression test convention).
    count=$(sed 's/#.*$//' "$f" | grep -cF 'CRITERIA_SERVER_TLS' || true)
    [ "$count" -eq 1 ] || fail "$f references CRITERIA_SERVER_TLS $count times in code (expected exactly once, inside the CASTLE_ADDR guard)"

    guard_block="$(awk '/if \[ -n "\$CASTLE_ADDR" \]; then/{flag=1} flag{print} flag && /^fi$/{exit}' "$f")"
    [ -n "$guard_block" ] || fail "$f is missing the 'if [ -n \"\$CASTLE_ADDR\" ]; then' server-mode guard"
    grep -qF 'export CRITERIA_SERVER_TLS=disable' <<<"$guard_block" || \
        fail "$f does not export CRITERIA_SERVER_TLS=disable inside the [ -n \"\$CASTLE_ADDR\" ] guard"
done

# ------------------------------------------------------------- behavioral run
# Sandbox the runner script: rewrite its absolute container paths to the temp
# tree and stub the criteria binary so it records the environment and argv it
# was invoked with.
sandbox_runner() { # sandbox_dir -> stdout: path of the rewritten runner copy
    local sb="$1"
    sed -e "s|/usr/local/bin/criteria|$sb/bin/criteria|g" \
        -e "s|/secrets/linear_api_key|$sb/secrets/linear_api_key|g" \
        -e "s|/data/.criteria/runs/|$sb/data/.criteria/runs/|g" \
        -e "s|^workflow_src=/workflows$|workflow_src=$sb/workflows|" \
        -e "s|^workflow_tmp=/tmp/workflows$|workflow_tmp=$sb/workflows-tmp|" \
        "$RUNNER_CANONICAL"
}

run_runner() { # out_env out_args castle_addr
    local out_env="$1" out_args="$2" castle_addr="$3"
    local sb
    sb=$(mktemp -d)
    SANDBOXES+=("$sb")

    mkdir -p "$sb/bin" "$sb/secrets" "$sb/home" "$sb/data" \
        "$sb/workflows/linear_intake_v1" "$sb/intake" "$sb/triage"
    printf 'test-linear-api-key' > "$sb/secrets/linear_api_key"

    # Minimal per-repo fixtures: the lockfile entries the runner resolves for
    # adapter digest discovery, and an adapters.chcl for the token/listen sed.
    cat > "$sb/workflows/linear_intake_v1/.criteria.lock.hcl" <<'EOF'
adapter "shell" "intake" {
  reference       = "ghcr.io/brokenbots/criteria-adapter-shell:0.0.0-test"
  resolved_digest = "sha256:000000000000000000000000000000000000000000000000000000000000shell"
}
adapter "copilot" "intake_classifier" {
  reference       = "ghcr.io/brokenbots/criteria-adapter-copilot:0.0.0-test"
  resolved_digest = "sha256:000000000000000000000000000000000000000000000000000000000copilot"
}
EOF
    printf 'listen_address = "127.0.0.1:7778"\n' > "$sb/workflows/linear_intake_v1/adapters.chcl"

    cat > "$sb/bin/criteria" <<'EOF'
#!/bin/sh
# Test stub: capture argv and environment, then succeed.
printf '%s\n' "$@" > "$CRITERIA_STUB_ARGS"
env | LC_ALL=C sort > "$CRITERIA_STUB_ENV"
exit 0
EOF
    chmod +x "$sb/bin/criteria"

    sandbox_runner "$sb" > "$sb/runner.sh"
    chmod +x "$sb/runner.sh"

    # Full job env block: the runner dereferences every one of these under
    # set -u (they are injected by the k8s Job manifest in production).
    local run_env=(
        PATH="$PATH"
        HOME="$sb/home"
        TICKET_ID=CRI-138
        REPO_URL=brokenbots/workflow-example
        REPO_DIR=/data/repos/CRI-138
        JOB_NAME=cri-138-server-tls
        POD_IP=10.0.0.1
        ALLOW_DIRTY=true
        MAX_AGENT_VISITS=1
        INTAKE_ROOT="$sb/intake"
        TRIAGE_ROOT="$sb/triage"
        LINEAR_REVIEW_STATE="In Review"
        LINEAR_TRIAGE_STATE=Triage
        LINEAR_WORK_STATE="In Progress"
        LINEAR_DONE_STATE=Done
        BASE_BRANCH=main
        BUILD_CMD="make build"
        TEST_CMD="make test"
        CI_GATE_CMD="make lint"
        TEST_REFS=HEAD
        MAIN_REF=origin/main
        STABLE_REF=origin/stable
        DESIGN_INTENT_FILE=DESIGN.md
        REPRO_WORKFLOW_DIR=
        PROVIDER_BASE_URL=http://provider:8080
        CRITERIA_STUB_ARGS="$out_args"
        CRITERIA_STUB_ENV="$out_env"
    )
    if [ -n "$castle_addr" ]; then
        run_env+=("CASTLE_ADDR=$castle_addr")
    fi

    env -i "${run_env[@]}" "$sb/runner.sh" || \
        fail "runner script failed under set -eu (CASTLE_ADDR=${castle_addr:-unset})"
}

# Server mode: the engine process must see CRITERIA_SERVER_TLS=disable and the
# --server flag with the castle address verbatim.
env_file=$(mktemp)
args_file=$(mktemp)
SANDBOXES=()
cleanup() {
    rm -f "$env_file" "$args_file"
    if [ "${#SANDBOXES[@]}" -gt 0 ]; then
        rm -rf "${SANDBOXES[@]}"
    fi
}
trap cleanup EXIT

run_runner "$env_file" "$args_file" "http://castle:8080"

grep -qx 'CRITERIA_SERVER_TLS=disable' "$env_file" || \
    fail "criteria apply was invoked without CRITERIA_SERVER_TLS=disable in its environment (CRI-138 plaintext rejection)"
grep -qF -- '--server' "$args_file" || fail "criteria apply was invoked without --server in server mode"
grep -qF 'http://castle:8080' "$args_file" || fail "criteria apply --server did not receive CASTLE_ADDR verbatim"

# Local file mode: no CASTLE_ADDR, no TLS opt-out, no --server flag.
run_runner "$env_file" "$args_file" ""

if grep -q '^CRITERIA_SERVER_TLS=' "$env_file"; then
    fail "runner exported CRITERIA_SERVER_TLS outside server mode (CASTLE_ADDR unset)"
fi
grep -qF -- '--server' "$args_file" && fail "criteria apply received --server without CASTLE_ADDR"

echo "PASS: runner server mode exports CRITERIA_SERVER_TLS=disable (and only then) before criteria apply --server"