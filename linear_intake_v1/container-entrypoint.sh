#!/bin/sh
set -eu

# Read k8s CSI secret mounts when the corresponding env var is not already set.
# Falls back to the existing environment variable for local Docker bootstrap.
read_secret_file() {
    target="$1"
    path="$2"
    eval "current=\${$target:-}"
    if [ -z "$current" ] && [ -r "$path" ]; then
        # Read the first line of the secret file verbatim. Tokens are single-line
        # values, and IFS= prevents trimming leading/trailing whitespace.
        IFS= read -r value < "$path"
        export "$target=$value"
    fi
}

read_secret_file LINEAR_API_KEY /secrets/linear_api_key
read_secret_file WORKFLOW_GITHUB_TOKEN /secrets/workflow_github_token
read_secret_file REVIEWER_GITHUB_TOKEN /secrets/reviewer_github_token

: "${LINEAR_API_KEY:?LINEAR_API_KEY is required (set env var or mount /secrets/linear_api_key)}"
: "${WORKFLOW_GITHUB_TOKEN:?WORKFLOW_GITHUB_TOKEN is required (set env var or mount /secrets/workflow_github_token)}"
: "${REVIEWER_GITHUB_TOKEN:?REVIEWER_GITHUB_TOKEN is required (set env var or mount /secrets/reviewer_github_token)}"
: "${TICKET_ID:?TICKET_ID is required}"

REPO_DIR=${REPO_DIR:-/repo}
REPO_URL=${REPO_URL:-}
INTAKE_ROOT=${INTAKE_ROOT:-/data/intake}
TRIAGE_ROOT=${TRIAGE_ROOT:-/data/triage}
LINEAR_REVIEW_STATE=${LINEAR_REVIEW_STATE:-In Review}
LINEAR_TRIAGE_STATE=${LINEAR_TRIAGE_STATE:-Triage}
LINEAR_WORK_STATE=${LINEAR_WORK_STATE:-In Progress}
LINEAR_DONE_STATE=${LINEAR_DONE_STATE:-Done}
BASE_BRANCH=${BASE_BRANCH:-main}
BUILD_CMD=${BUILD_CMD:-}
TEST_CMD=${TEST_CMD:-}
CI_GATE_CMD=${CI_GATE_CMD:-}
TEST_REFS=${TEST_REFS:-both}
MAIN_REF=${MAIN_REF:-origin/main}
STABLE_REF=${STABLE_REF:-}
DESIGN_INTENT_FILE=${DESIGN_INTENT_FILE:-}
REPRO_WORKFLOW_DIR=${REPRO_WORKFLOW_DIR:-}
ALLOW_DIRTY=${ALLOW_DIRTY:-false}
MAX_AGENT_VISITS=${MAX_AGENT_VISITS:-2}
PROVIDER_BASE_URL=${PROVIDER_BASE_URL:-http://host.docker.internal:11434/v1}

case "$ALLOW_DIRTY" in
    true|false) ;;
    *) echo "ALLOW_DIRTY must be true or false" >&2; exit 2 ;;
esac
case "$MAX_AGENT_VISITS" in
    ''|*[!0-9]*) echo "MAX_AGENT_VISITS must be a positive integer" >&2; exit 2 ;;
    0) echo "MAX_AGENT_VISITS must be a positive integer" >&2; exit 2 ;;
esac

workflow_user=$(GH_TOKEN="$WORKFLOW_GITHUB_TOKEN" gh api user --jq .login)
reviewer_user=$(GH_TOKEN="$REVIEWER_GITHUB_TOKEN" gh api user --jq .login)
if [ "$workflow_user" = "$reviewer_user" ]; then
    echo "workflow and reviewer tokens resolve to the same GitHub user: $workflow_user" >&2
    exit 2
fi

git config --global credential.https://github.com.helper '!gh auth git-credential'
git config --global user.name "$workflow_user"
git config --global user.email "$workflow_user@users.noreply.github.com"

if ! git -C "$REPO_DIR" rev-parse --show-toplevel >/dev/null 2>&1; then
    : "${REPO_URL:?REPO_URL is required when REPO_DIR is not a Git repository}"
    if [ -d "$REPO_DIR" ] && [ -n "$(find "$REPO_DIR" -mindepth 1 -maxdepth 1 -print -quit)" ]; then
        echo "REPO_DIR is not a Git repository and is not empty: $REPO_DIR" >&2
        exit 2
    fi
    mkdir -p "$(dirname "$REPO_DIR")"
    GH_TOKEN="$WORKFLOW_GITHUB_TOKEN" gh repo clone "$REPO_URL" "$REPO_DIR"
fi

mkdir -p "$INTAKE_ROOT/$TICKET_ID" "$TRIAGE_ROOT"
runtime_dir=$(mktemp -d)
runtime_vars="$runtime_dir/vars.json"
trap 'rm -rf "$runtime_dir"' EXIT
jq -n \
    --arg ticket_id "$TICKET_ID" \
    --arg repo_dir "$REPO_DIR" \
    --arg intake_root "$INTAKE_ROOT" \
    --arg triage_root "$TRIAGE_ROOT" \
    --arg linear_review_state "$LINEAR_REVIEW_STATE" \
    --arg linear_triage_state "$LINEAR_TRIAGE_STATE" \
    --arg linear_work_state "$LINEAR_WORK_STATE" \
    --arg linear_done_state "$LINEAR_DONE_STATE" \
    --arg base_branch "$BASE_BRANCH" \
    --arg build_cmd "$BUILD_CMD" \
    --arg test_cmd "$TEST_CMD" \
    --arg ci_gate_cmd "$CI_GATE_CMD" \
    --arg test_refs "$TEST_REFS" \
    --arg main_ref "$MAIN_REF" \
    --arg stable_ref "$STABLE_REF" \
    --arg design_intent_file "$DESIGN_INTENT_FILE" \
    --arg repro_workflow_dir "$REPRO_WORKFLOW_DIR" \
    --arg provider_base_url "$PROVIDER_BASE_URL" \
    --argjson allow_dirty "$ALLOW_DIRTY" \
    --argjson max_agent_visits "$MAX_AGENT_VISITS" \
    '{
        ticket_id: $ticket_id,
        repo_dir: $repo_dir,
        intake_root: $intake_root,
        triage_root: $triage_root,
        linear_review_state: $linear_review_state,
        linear_triage_state: $linear_triage_state,
        linear_work_state: $linear_work_state,
        linear_done_state: $linear_done_state,
        base_branch: $base_branch,
        build_cmd: $build_cmd,
        test_cmd: $test_cmd,
        ci_gate_cmd: $ci_gate_cmd,
        test_refs: $test_refs,
        main_ref: $main_ref,
        stable_ref: $stable_ref,
        design_intent_file: $design_intent_file,
        repro_workflow_dir: $repro_workflow_dir,
        provider_base_url: $provider_base_url,
        allow_dirty: $allow_dirty,
        max_agent_visits: $max_agent_visits
    }' > "$runtime_vars"

# Secret variables are NOT placed in the var-file (it is a plain JSON artifact
# on disk and would persist credentials). They are passed as --var overrides,
# which criteria accepts for secret-typed variables and reports as (sensitive).
# Remote adapters reconnect to the shim over a bearer token; generate one unless
# the operator supplied it.
CRITERIA_REMOTE_TOKEN=${CRITERIA_REMOTE_TOKEN:-$(head -c 48 /dev/urandom | base64 | tr -cd 'a-zA-Z0-9' | head -c 32)}
export CRITERIA_REMOTE_TOKEN
# The Criteria parser evaluates environment-block attributes as static literals,
# so `env("CRITERIA_REMOTE_TOKEN")` cannot be used inside `environment "remote"`
# blocks.  Copy the workflow tree into the runtime directory and substitute the
# placeholder with the generated token so the shim and adapters agree on the
# bearer token at run time.  The original /workflows tree is left untouched.
workflow_src=${CRITERIA_WORKFLOW_SOURCE:-/workflows}
workflow_tmp="$runtime_dir/workflows"
cp -a "$workflow_src" "$workflow_tmp"
find "$workflow_tmp" -name 'adapters.chcl' -exec sh -c '
    token="$1"
    shift
    for f; do
        # The generated token contains only alphanumerics, so a fixed string
        # delimiter is safe.  Printf defends against leading "-" in sed args.
        sed -i "s|__CRITERIA_REMOTE_TOKEN__|$token|g" "$f"
    done
' sh "$CRITERIA_REMOTE_TOKEN" {} +

# Locate the locked remote adapter binaries in the local OCI cache. The
# installed-cache listing may not carry the reference (unattributed when the
# adapter was pulled without annotation), so resolve the digest straight from
# the workflow lockfile — the same trust anchor `criteria apply` enforces.
# The digest doubles as the adapter's identity over the handshake: the shim
# rejects connections whose presented digest does not match the pinned one,
# so each adapter is launched with CRITERIA_REMOTE_DIGEST=<pinned digest>.
adapter_digest() {
    kind="$1"
    lockfile="$2"
    digest=$(awk -v k="criteria-adapter-${kind}" '
        $0 ~ "reference.*"k { in_entry=1 }
        in_entry && /resolved_digest/ { gsub(/[\" ]/, ""); sub(/^resolved_digest=sha256:/, ""); print; exit }
    ' "$lockfile")
    if [ -z "$digest" ]; then
        echo "criteria adapter $kind not found in $lockfile" >&2
        exit 1
    fi
    printf '%s' "$digest"
}

adapter_binary() {
    kind="$1"
    digest="$2"
    printf '%s' "/home/criteria/.local/criteria/adapters/sha256-${digest}/criteria-adapter-${kind}"
}

main_lockfile="$workflow_src/linear_intake_v1/.criteria.lock.hcl"
shell_digest=$(adapter_digest shell "$main_lockfile")
copilot_digest=$(adapter_digest copilot "$main_lockfile")
shell_adapter=$(adapter_binary shell "$shell_digest")
copilot_adapter=$(adapter_binary copilot "$copilot_digest")

# Launch the adapters in phone-home mode. They retry until the criteria shim
# starts listening on 127.0.0.1:7778.  CRITERIA_REMOTE_HOST is passed only to
# the adapters; the engine uses the listen_address from the workflow config
# and must not see this variable (otherwise adapter verification would run in
# remote mode and time out).
env CRITERIA_REMOTE_HOST="127.0.0.1:7778" \
    CRITERIA_REMOTE_TOKEN="$CRITERIA_REMOTE_TOKEN" \
    CRITERIA_REMOTE_DIGEST="sha256:$shell_digest" \
    "$shell_adapter" &
shell_pid=$!
env CRITERIA_REMOTE_HOST="127.0.0.1:7778" \
    CRITERIA_REMOTE_TOKEN="$CRITERIA_REMOTE_TOKEN" \
    CRITERIA_REMOTE_DIGEST="sha256:$copilot_digest" \
    "$copilot_adapter" &
copilot_pid=$!

cleanup() {
    kill "$shell_pid" "$copilot_pid" 2>/dev/null || true
    rm -rf "$runtime_dir"
}
trap cleanup EXIT INT TERM

/usr/local/bin/criteria apply "$workflow_tmp/linear_intake_v1" \
    --var-file "$runtime_vars" \
    --var "linear_api_key=$LINEAR_API_KEY" \
    --var "workflow_github_token=$WORKFLOW_GITHUB_TOKEN" \
    --var "reviewer_github_token=$REVIEWER_GITHUB_TOKEN" \
    --events-file "${EVENTS_FILE:-$INTAKE_ROOT/$TICKET_ID/events.ndjson}" \
    "$@"