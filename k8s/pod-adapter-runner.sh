#!/bin/sh
set -eu

# Read the Linear API key from the CSI mount. The workflow-runner container is
# intentionally not given any GitHub token mounts.
LINEAR_API_KEY=""
if [ -r /secrets/linear_api_key ]; then
    LINEAR_API_KEY=$(cat /secrets/linear_api_key)
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
if [ -z "${JOB_NAME:-}" ]; then
    echo "JOB_NAME is required" >&2
    exit 1
fi
if [ -z "${POD_IP:-}" ]; then
    echo "POD_IP is required" >&2
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

# Git refuses repositories whose on-disk owner differs from the current user
# (the emptyDir/PV clone runs under a different effective ownership view).
git config --global --add safe.directory "$REPO_DIR"
git config --global --add safe.directory "$REPO_DIR/**"

# Per-run discovery directory. Adapters poll this path by convention to learn
# the runner's dial address, the per-run bearer token, and the pinned adapter
# digests. The directory is removed on exit so a subsequent run cannot reuse
# the token.
run_dir="/data/.criteria/runs/$JOB_NAME"
mkdir -p "$run_dir"

# Generate a per-run bearer token and publish it for the adapters.
token=$(head -c 48 /dev/urandom | base64 | tr -cd 'a-zA-Z0-9' | head -c 32)
printf '%s' "$token" > "$run_dir/token"

# Resolve the pinned adapter digests from the workflow lockfile using the same
# awk the local container entrypoint uses.
lockfile=/workflows/linear_intake_v1/.criteria.lock.hcl
adapter_digest() {
    kind="$1"
    digest=$(awk -v k="criteria-adapter-${kind}" '
        $0 ~ "reference.*"k { in_entry=1 }
        in_entry && /resolved_digest/ { gsub(/[" ]/, ""); sub(/^resolved_digest=sha256:/, ""); print; exit }
    ' "$lockfile")
    if [ -z "$digest" ]; then
        echo "criteria adapter $kind not found in $lockfile" >&2
        exit 1
    fi
    printf '%s' "$digest"
}
printf '%s' "$(adapter_digest shell)" > "$run_dir/digest-shell"
printf '%s' "$(adapter_digest copilot)" > "$run_dir/digest-copilot"

# Publish the runner dial address. The Kubernetes runtime widens the shim to
# 0.0.0.0:7778 so separate adapter pods can reach it; local Docker runs keep
# 127.0.0.1:7778 unchanged.
printf '%s' "${POD_IP}:7778" > "$run_dir/host"

workflow_src=/workflows
workflow_tmp=/tmp/workflows
rm -rf "$workflow_tmp"
cp -a "$workflow_src" "$workflow_tmp"

# Substitute the placeholders in every subworkflow's remote environment block
# so each shim and the adapter pods share the generated bearer token. The
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

# The declarative workflow keeps 127.0.0.1:7778 as its portable default. Only
# the Kubernetes runtime widens the listen address so separate adapter pods can
# phone home. accept_token stays enabled because the engine requires it for
# non-loopback listen addresses.
find "$workflow_tmp" -name 'adapters.chcl' -exec sed -i \
    -e 's|listen_address = "127\.0\.0\.1:7778"|listen_address = "0.0.0.0:7778"|g' \
    -e 's|listen_address = "127\.0\.0\.1:7778"|listen_address = "0.0.0.0:7778"|g' {} +

# The workflow HCL keeps its var.workflow_github_token / var.reviewer_github_token
# secret references untouched: the compiler requires direct var.<name> bindings.
# Instead the runner passes file: OriginRefs for those variables (--var below);
# the engine's resolveSecretVarOrigins reads them through the secrets provider
# stack (FileProvider) at run start and delivers the values to adapters over
# OpenSession - the SDK contract (D69: adapters never read their own process
# environment for workflow secrets). Tokens never appear as env vars or argv.

runtime_dir=$(mktemp -d)
runtime_vars="$runtime_dir/vars.json"
cleanup() {
    rm -rf "$run_dir" "$runtime_dir"
}
trap cleanup EXIT

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

# Secret variables are passed as file: OriginRefs: the engine resolves them
# through the FileProvider at run start (resolveSecretVarOrigins). Raw token
# values never appear in argv - only the mount paths.
/usr/local/bin/criteria apply "$workflow_tmp/linear_intake_v1" \
    --var-file "$runtime_vars" \
    --var "linear_api_key=$LINEAR_API_KEY" \
    --var "workflow_github_token=file:/home/criteria/secrets/workflow_github_token" \
    --var "reviewer_github_token=file:/home/criteria/secrets/reviewer_github_token" \
    --events-file "$events_file" \
    --output concise
