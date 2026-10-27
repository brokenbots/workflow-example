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
# CASTLE_ADDR set and once unset. KB-344: the sandbox also proves the proxy
# identity carry — with CRITERIA_RUN_NAME/CRITERIA_PROXY_ENROLLMENT_TOKEN
# set the engine argv must include --name and --server-bootstrap-token (and
# neither of the two when the env is unset). Structural guards pin both
# runner.sh copies (canonical + chart-embedded) staying in sync.

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
sandbox_runner() { # src_script sandbox_dir -> stdout: path of the rewritten runner copy
    local src_script="$1"
    local sb="$2"
    sed -e "s|/usr/local/bin/criteria|$sb/bin/criteria|g" \
        -e "s|/secrets/linear_api_key|$sb/secrets/linear_api_key|g" \
        -e "s|/data/.criteria/runs/|$sb/data/.criteria/runs/|g" \
        -e "s|^workflow_src=/workflows$|workflow_src=$sb/workflows|" \
        -e "s|^workflow_tmp=/tmp/workflows$|workflow_tmp=$sb/workflows-tmp|" \
        "$src_script"
}

run_runner() { # out_env out_args castle_addr src_script [EXTRA_ENV=value...]
    local out_env="$1" out_args="$2" castle_addr="$3" src_script="$4"
    shift 4
    local sb
    sb=$(mktemp -d)
    SANDBOXES+=("$sb")

    mkdir -p "$sb/bin" "$sb/secrets" "$sb/home" "$sb/data" \
        "$sb/workflows/kanboard_triage_v1" "$sb/intake" "$sb/triage"
    printf 'test-linear-api-key' > "$sb/secrets/linear_api_key"

    # Minimal per-repo fixtures: the lockfile entries the runner resolves for
    # adapter digest discovery, and an adapters.chcl for the token/listen sed.
    cat > "$sb/workflows/kanboard_triage_v1/.criteria.lock.hcl" <<'EOF'
adapter "shell" "intake" {
  reference       = "ghcr.io/brokenbots/criteria-adapter-shell:0.0.0-test"
  resolved_digest = "sha256:000000000000000000000000000000000000000000000000000000000000shell"
}
adapter "copilot" "intake_classifier" {
  reference       = "ghcr.io/brokenbots/criteria-adapter-copilot:0.0.0-test"
  resolved_digest = "sha256:000000000000000000000000000000000000000000000000000000000copilot"
}
EOF
    printf 'listen_address = "127.0.0.1:7778"\n' > "$sb/workflows/kanboard_triage_v1/adapters.chcl"

    cat > "$sb/bin/criteria" <<'EOF'
#!/bin/sh
# Test stub: capture argv and environment, then succeed.
printf '%s\n' "$@" > "$CRITERIA_STUB_ARGS"
env | LC_ALL=C sort > "$CRITERIA_STUB_ENV"
exit 0
EOF
    chmod +x "$sb/bin/criteria"

    sandbox_runner "$src_script" "$sb" > "$sb/runner.sh"
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
    if [ "$#" -gt 0 ]; then
        run_env+=("$@")
    fi

    env -i "${run_env[@]}" "$sb/runner.sh" || \
        fail "runner script failed under set -eu (CASTLE_ADDR=${castle_addr:-unset}, src=$src_script)"
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

# Server mode (tokenless): the engine process must see CRITERIA_SERVER_TLS=disable
# and the --server flag with the configured address verbatim. Without the
# per-run env, argv must carry neither attribution nor bootstrap flags:
# a run identity or enrollment credential is per-run state, never a
# boot-time default (KB-344).
for runner_src in "$RUNNER_CANONICAL" "$RUNNER_CHART"; do
    run_runner "$env_file" "$args_file" "http://castle:8080" "$runner_src"

    grep -qx 'CRITERIA_SERVER_TLS=disable' "$env_file" || \
        fail "criteria apply was invoked without CRITERIA_SERVER_TLS=disable in its environment (CRI-138 plaintext rejection)"
    grep -qx -- '--server' "$args_file" || fail "criteria apply was invoked without --server in server mode"
    grep -qF 'http://castle:8080' "$args_file" || fail "criteria apply --server did not receive CASTLE_ADDR verbatim"
    if grep -qx -- '--name' "$args_file"; then
        fail "$runner_src passed --name with no run identity in the env (attribution is per-run state)"
    fi
    if grep -qx -- '--server-bootstrap-token' "$args_file"; then
        fail "$runner_src passed --server-bootstrap-token with no enrollment credential in the env"
    fi
done

# Local file mode: no CASTLE_ADDR, no TLS opt-out, no --server flag.
for runner_src in "$RUNNER_CANONICAL" "$RUNNER_CHART"; do
    run_runner "$env_file" "$args_file" "" "$runner_src"

    if grep -q '^CRITERIA_SERVER_TLS=' "$env_file"; then
        fail "runner exported CRITERIA_SERVER_TLS outside server mode (CASTLE_ADDR unset)"
    fi
    if grep -qx -- '--server' "$args_file"; then
        fail "criteria apply received --server without CASTLE_ADDR"
    fi
    if grep -qx -- '--name' "$args_file"; then
        fail "$runner_src passed --name without CASTLE_ADDR (nothing to attribute)"
    fi
    if grep -qx -- '--server-bootstrap-token' "$args_file"; then
        fail "$runner_src passed --server-bootstrap-token without CASTLE_ADDR (nothing to present it to)"
    fi
done

# KB-344: the proxy identity carry — with the enrolled run's env
# (CRITERIA_RUN_NAME + CRITERIA_PROXY_ENROLLMENT_TOKEN), both runner copies
# pass --name <run> and --server-bootstrap-token <token> so the operator's
# run-event proxy attributes the streams to the owning CriteriaRun and the
# engine presents the CR-stamped bootstrap credential on Register.
for runner_src in "$RUNNER_CANONICAL" "$RUNNER_CHART"; do
    run_runner "$env_file" "$args_file" "http://castle:8080" "$runner_src" \
        CRITERIA_RUN_NAME=cri-138-server-tls CRITERIA_PROXY_ENROLLMENT_TOKEN=prenrol-138

    grep -qx -- '--name' "$args_file" || \
        fail "$runner_src dropped the --name run-attribution flag (the proxy could not attribute the streams)"
    grep -qx 'cri-138-server-tls' "$args_file" || \
        fail "$runner_src did not pass the run identity as the --name value"
    grep -qx -- '--server-bootstrap-token' "$args_file" || \
        fail "$runner_src dropped --server-bootstrap-token (the enrollment credential would never reach the proxy)"
    grep -qx 'prenrol-138' "$args_file" || \
        fail "$runner_src did not pass the enrollment token as the --server-bootstrap-token value"
done

echo "PASS: runner server mode exports CRITERIA_SERVER_TLS=disable (and only then), carries --name and --server-bootstrap-token from the per-run env (and only then), before criteria apply --server"