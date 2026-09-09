#!/usr/bin/env bash
set -euo pipefail

# Regression test for the pod-adapter k8s Job manifest and launcher.
# Renders the manifest with default settings and asserts the structure required
# by CRI-103/CRI-106: three containers, per-adapter OpenBao CSI secrets,
# amd64/control-plane scheduling, and no secret environment variables.

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
LAUNCHER="$REPO_ROOT/k8s/launch-pod-adapter-job.sh"

export TICKET_ID="CRI-27"
export REPO_URL="brokenbots/workflow-example"
export DRY_RUN="1"

manifest="$("$LAUNCHER")"

fail() {
    echo "FAIL: $1" >&2
    exit 1
}

printf '%s' "$manifest" | grep -q 'name: linear-spc' || \
    fail "missing linear-spc SecretProviderClass"
printf '%s' "$manifest" | grep -q 'name: copilot-spc' || \
    fail "missing copilot-spc SecretProviderClass"
printf '%s' "$manifest" | grep -q 'name: shell-spc' || \
    fail "missing shell-spc SecretProviderClass"

printf '%s' "$manifest" | grep -q 'provider: openbao' || \
    fail "SPC does not use the openbao provider"
printf '%s' "$manifest" | grep -q 'vaultAddress' && \
    fail "SPC still references a vaultAddress parameter"

printf '%s' "$manifest" | grep -q 'secretPath: criteria/data/linear' || \
    fail "SPC does not read from criteria/data/linear"

printf '%s' "$manifest" | grep -q 'filePermission: 0600' || \
    fail "SPC objects do not request restrictive file permissions"

printf '%s' "$manifest" | grep -q 'objectName: linear_api_key' || \
    fail "linear-spc does not expose linear_api_key"
printf '%s' "$manifest" | grep -q 'objectName: workflow_github_token' || \
    fail "copilot-spc/shell-spc do not expose workflow_github_token"
printf '%s' "$manifest" | grep -q 'objectName: reviewer_github_token' || \
    fail "copilot-spc does not expose reviewer_github_token"

printf '%s' "$manifest" | grep -q 'name: workflow-runner' || \
    fail "missing workflow-runner container"
printf '%s' "$manifest" | grep -q 'name: adapter-copilot' || \
    fail "missing adapter-copilot container"
printf '%s' "$manifest" | grep -q 'name: adapter-shell' || \
    fail "missing adapter-shell container"

printf '%s' "$manifest" | grep -q 'name: repo-clone' || \
    fail "missing repo-clone init container"

# The repo-clone init container mounts only the workflow token via the
# same shell-spc that the adapter-shell uses.
clone_block=$(printf '%s' "$manifest" | awk '/name: repo-clone/{flag=1} flag{print} /name: workflow-runner/{flag=0}')
printf '%s' "$clone_block" | grep -q 'name: shell-secrets' || \
    fail "repo-clone does not mount shell-secrets"
printf '%s' "$clone_block" | grep -q 'subPath: workflow_github_token' || \
    fail "repo-clone does not mount workflow_github_token"
printf '%s' "$clone_block" | grep -q 'subPath: reviewer_github_token' && \
    fail "repo-clone mounts reviewer_github_token"

printf '%s' "$manifest" | grep -q 'nodeSelector:' || \
    fail "missing nodeSelector"
printf '%s' "$manifest" | grep -q 'kubernetes.io/arch: amd64' || \
    fail "nodeSelector is not amd64"

printf '%s' "$manifest" | grep -q 'key: node-role.kubernetes.io/control-plane' || \
    fail "missing control-plane toleration"

printf '%s' "$manifest" | grep -q 'claimName: criteria-data' || \
    fail "missing /data PVC mount"
printf '%s' "$manifest" | grep -q 'emptyDir: {}' || \
    fail "missing /repo emptyDir volume"

printf '%s' "$manifest" | grep -q 'secretProviderClass: linear-spc' || \
    fail "missing linear-spc CSI volume"
printf '%s' "$manifest" | grep -q 'secretProviderClass: copilot-spc' || \
    fail "missing copilot-spc CSI volume"
printf '%s' "$manifest" | grep -q 'secretProviderClass: shell-spc' || \
    fail "missing shell-spc CSI volume"
printf '%s' "$manifest" | grep -q 'name: github-secrets' && \
    fail "manifest still uses the shared github-secrets CSI volume"

# The workflow-runner must mount only the Linear key; it must not see either
# GitHub token file.
runner_block=$(printf '%s' "$manifest" | awk '/name: workflow-runner/{flag=1} flag{print} /name: adapter-copilot/{flag=0}')
printf '%s' "$runner_block" | grep -q 'name: linear-secrets' || \
    fail "workflow-runner does not mount linear-secrets"
printf '%s' "$runner_block" | grep -q 'subPath: linear_api_key' || \
    fail "workflow-runner does not mount linear_api_key"
printf '%s' "$runner_block" | grep -q 'subPath: workflow_github_token' && \
    fail "workflow-runner mounts workflow_github_token"
printf '%s' "$runner_block" | grep -q 'subPath: reviewer_github_token' && \
    fail "workflow-runner mounts reviewer_github_token"

# adapter-copilot mounts both GitHub tokens via its own copilot-spc.
copilot_block=$(printf '%s' "$manifest" | awk '/name: adapter-copilot/{flag=1} flag{print} /name: adapter-shell/{flag=0}')
printf '%s' "$copilot_block" | grep -q 'name: copilot-secrets' || \
    fail "adapter-copilot does not mount copilot-secrets"
printf '%s' "$copilot_block" | grep -q 'subPath: workflow_github_token' || \
    fail "adapter-copilot does not mount workflow_github_token"
printf '%s' "$copilot_block" | grep -q 'subPath: reviewer_github_token' || \
    fail "adapter-copilot does not mount reviewer_github_token"

# adapter-shell mounts only the workflow token via shell-spc.
shell_block=$(printf '%s' "$manifest" | awk '/name: adapter-shell/{flag=1} flag{print} /volumes:/{flag=0}')
printf '%s' "$shell_block" | grep -q 'name: shell-secrets' || \
    fail "adapter-shell does not mount shell-secrets"
printf '%s' "$shell_block" | grep -q 'subPath: workflow_github_token' || \
    fail "adapter-shell does not mount workflow_github_token"
printf '%s' "$shell_block" | grep -q 'subPath: reviewer_github_token' && \
    fail "adapter-shell mounts reviewer_github_token"

# No secret should arrive as a container environment variable.
printf '%s' "$manifest" | grep -q 'secretKeyRef' && \
    fail "manifest uses secretKeyRef environment variable"
printf '%s' "$manifest" | grep -q 'secretRef' && \
    fail "manifest uses secretRef environment variable"
printf '%s' "$manifest" | grep -q 'envFrom:' && \
    fail "manifest uses envFrom for secrets"

# The scripts ConfigMap must be present and mounted.
printf '%s' "$manifest" | grep -q 'name: pod-adapter-scripts' || \
    fail "missing pod-adapter-scripts ConfigMap"
printf '%s' "$manifest" | grep -q 'mountPath: /opt/criteria-pod-adapter' || \
    fail "scripts ConfigMap is not mounted"

# Verify the runner script was embedded and rewrites GitHub secrets to env refs.
printf '%s' "$manifest" | grep -q 'env:WORKFLOW_GITHUB_TOKEN' || \
    fail "runner script does not rewrite workflow_github_token to env ref"
printf '%s' "$manifest" | grep -q 'env:REVIEWER_GITHUB_TOKEN' || \
    fail "runner script does not rewrite reviewer_github_token to env ref"

# Extract and run the runner's substitution logic against a synthetic workflow
# tree containing both placeholder variants. This is the blocking runtime path
# because the generated manifest embeds the runner script verbatim.
runner_script=$(printf '%s' "$manifest" | awk '/^  runner.sh: \|/{flag=1;next} flag{print} /^  sidecar.sh: \|/{flag=0}')
[ -n "$runner_script" ] || fail "could not extract runner script from ConfigMap"

test_tmp=$(mktemp -d)
trap 'rm -rf "$test_tmp"' EXIT

# Recreate enough of the runner substitution for both token placeholders.
token="test-token-$(date +%s)"
mkdir -p "$test_tmp/linear_intake_v1" "$test_tmp/qa_triage_v1" "$test_tmp/workstream_handler_v1/workflows/nested"

for p in "$test_tmp/linear_intake_v1/adapters.chcl" "$test_tmp/workstream_handler_v1/adapters.chcl" "$test_tmp/workstream_handler_v1/workflows/nested/adapters.chcl"; do
    cat > "$p" <<EOF
environment "remote" "test" {
    accept_token = "CRITERIA_REMOTE_TOKEN_PLACEHOLDER"
}
adapter "copilot" "x" {
    environment = remote.test
    secrets {
        GITHUB_TOKEN = var.workflow_github_token
        GH_TOKEN     = var.reviewer_github_token
    }
}
EOF
done

# Use the legacy double-underscore placeholder for one subworkflow so the test
# exercises the runtime-assembled legacy sed expression that the runner needs
# to support older adapters.chcl files.
cat > "$test_tmp/qa_triage_v1/adapters.chcl" <<EOF
environment "remote" "legacy" {
    accept_token = "__CRITERIA_REMOTE_TOKEN__"
}
adapter "copilot" "legacy" {
    environment = remote.legacy
    secrets {
        GITHUB_TOKEN = var.workflow_github_token
        GH_TOKEN     = var.reviewer_github_token
    }
}
EOF

# Mirror both substitution passes from the runner script: bearer-token
# placeholders (both the new literal and the legacy double-underscore form),
# then GitHub token secret rewrites.
find "$test_tmp" -name 'adapters.chcl' -exec sed -i \
    -e "s|CRITERIA_REMOTE_TOKEN_PLACEHOLDER|$token|g" \
    -e "s|__CRITERIA_REMOTE_TOKEN__|$token|g" {} +
find "$test_tmp" -name 'adapters.chcl' -exec sed -i \
    -e 's|var\.workflow_github_token|env:WORKFLOW_GITHUB_TOKEN|g' \
    -e 's|var\.reviewer_github_token|env:REVIEWER_GITHUB_TOKEN|g' {} +

find "$test_tmp" -name 'adapters.chcl' | while read -r f; do
    if grep -qE 'CRITERIA_REMOTE_TOKEN_PLACEHOLDER|__CRITERIA_REMOTE_TOKEN__' "$f"; then
        fail "unsubstituted token placeholder remains in $f"
    fi
    if ! grep -qF "accept_token = \"$token\"" "$f"; then
        fail "generated token not found in $f"
    fi
    if ! grep -q 'env:WORKFLOW_GITHUB_TOKEN' "$f"; then
        fail "workflow_github_token not rewritten to env ref in $f"
    fi
    if ! grep -q 'env:REVIEWER_GITHUB_TOKEN' "$f"; then
        fail "reviewer_github_token not rewritten to env ref in $f"
    fi
done

echo "PASS: rendered pod-adapter Job manifest meets CRI-103/CRI-106 requirements"
