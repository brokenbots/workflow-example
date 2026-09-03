#!/bin/sh
set -eu

: "${LINEAR_API_KEY:?LINEAR_API_KEY is required}"
: "${WORKFLOW_GITHUB_TOKEN:?WORKFLOW_GITHUB_TOKEN is required}"
: "${REVIEWER_GITHUB_TOKEN:?REVIEWER_GITHUB_TOKEN is required}"
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
# The sandbox scrubs the host token env vars before launch, so the adapters
# reintroduce only the declared secret channel — but that channel resolves
# var.<name>, which is empty unless a value is supplied here. An empty
# linear_api_key previously reached the shell adapter as "LINEAR_API_KEY is not
# set" and aborted the run at fetch_ticket.
exec /usr/local/bin/criteria apply /workflows/linear_intake_v1 \
    --var-file "$runtime_vars" \
    --var "linear_api_key=$LINEAR_API_KEY" \
    --var "workflow_github_token=$WORKFLOW_GITHUB_TOKEN" \
    --var "reviewer_github_token=$REVIEWER_GITHUB_TOKEN" \
    --events-file "${EVENTS_FILE:-$INTAKE_ROOT/$TICKET_ID/events.ndjson}" \
    "$@"