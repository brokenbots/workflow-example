#!/bin/sh
set -eu

# Read the Linear API key from the CSI mount. The workflow-runner container is
# intentionally not given any GitHub token mounts.
LINEAR_API_KEY=""
if [ -r /secrets/linear_api_key ]; then
    IFS= read -r LINEAR_API_KEY < /secrets/linear_api_key
fi
if [ -z "$LINEAR_API_KEY" ]; then
    echo "LINEAR_API_KEY is required via /secrets/linear_api_key" >&2
    exit 1
fi

if [ -z "${TICKET_ID:-}" ]; then
    echo "TICKET_ID is required" >&2
    exit 1
fi
if [ -z "${REPO_URL:-}" ]; then
    echo "REPO_URL is required" >&2
    exit 1
fi

case "$ALLOW_DIRTY" in
    true|false) ;;
    *) echo "ALLOW_DIRTY must be true or false" >&2; exit 2 ;;
esac
case "$MAX_AGENT_VISITS" in
    ''|*[!0-9]*) echo "MAX_AGENT_VISITS must be a positive integer" >&2; exit 2 ;;
    0) echo "MAX_AGENT_VISITS must be a positive integer" >&2; exit 2 ;;
esac

mkdir -p "$INTAKE_ROOT/$TICKET_ID" "$TRIAGE_ROOT"

# Generate a per-run bearer token for the remote adapters and publish it to the
# shared PVC so all three containers agree on the token.
token=$(head -c 48 /dev/urandom | base64 | tr -cd 'a-zA-Z0-9' | head -c 32)
printf '%s' "$token" > /data/.criteria-remote-token

workflow_src=/workflows
workflow_tmp=/tmp/workflows
rm -rf "$workflow_tmp"
cp -a "$workflow_src" "$workflow_tmp"

# Substitute the placeholders in every subworkflow's remote environment block
# so each shim and the adapter sidecars share the generated bearer token. The
# legacy placeholder literal is assembled at runtime so the launcher template
# renderer does not try to replace it while rendering the manifest.
old_token_placeholder="_""_CRITERIA_REMOTE_TOKEN_""_"
find "$workflow_tmp" -name 'adapters.chcl' -exec sh -c '
    tok="$1"
    old="$2"
    shift 2
    for f; do
        sed -i \
            -e "s|CRITERIA_REMOTE_TOKEN_PLACEHOLDER|$tok|g" \
            -e "s|$old|$tok|g" \
            "$f"
    done
' sh "$token" "$old_token_placeholder" {} +

# Rewrite GitHub token secrets so each adapter reads its own token from its CSI
# mount instead of receiving it from the workflow-runner over the secret
# channel. This lets the runner mount only the Linear API key.
find "$workflow_tmp" -name 'adapters.chcl' -exec sed -i \
    -e 's|var\.workflow_github_token|env:WORKFLOW_GITHUB_TOKEN|g' \
    -e 's|var\.reviewer_github_token|env:REVIEWER_GITHUB_TOKEN|g' {} +

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

# shellcheck disable=SC2153
: "${EVENTS_FILE:=}"
events_file="$EVENTS_FILE"
if [ -z "$events_file" ]; then
    events_file="$INTAKE_ROOT/$TICKET_ID/events.ndjson"
fi

# Dummy values for GitHub token variables: the real values live in each adapter
# container's CSI mount and are resolved via env: references at runtime.
/usr/local/bin/criteria apply "$workflow_tmp/linear_intake_v1" \
    --var-file "$runtime_vars" \
    --var "linear_api_key=$LINEAR_API_KEY" \
    --var "workflow_github_token=csi-supplied" \
    --var "reviewer_github_token=csi-supplied" \
    --events-file "$events_file" \
    --output concise
