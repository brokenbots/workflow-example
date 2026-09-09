#!/usr/bin/env bash
set -euo pipefail

# Regression test for the pod-adapter k8s Job manifest and launcher.
# Renders the manifest with default settings and asserts the structure required
# by CRI-103: three containers, CSI-only secrets, amd64/control-plane
# scheduling, and no secret environment variables.

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
printf '%s' "$manifest" | grep -q 'name: github-spc' || \
    fail "missing github-spc SecretProviderClass"

printf '%s' "$manifest" | grep -q 'secretPath: criteria/data/linear' || \
    fail "SPC does not read from criteria/data/linear"

printf '%s' "$manifest" | grep -q 'objectName: linear_api_key' || \
    fail "linear-spc does not expose linear_api_key"
printf '%s' "$manifest" | grep -q 'objectName: workflow_github_token' || \
    fail "github-spc does not expose workflow_github_token"
printf '%s' "$manifest" | grep -q 'objectName: reviewer_github_token' || \
    fail "github-spc does not expose reviewer_github_token"

printf '%s' "$manifest" | grep -q 'name: workflow-runner' || \
    fail "missing workflow-runner container"
printf '%s' "$manifest" | grep -q 'name: adapter-copilot' || \
    fail "missing adapter-copilot container"
printf '%s' "$manifest" | grep -q 'name: adapter-shell' || \
    fail "missing adapter-shell container"

printf '%s' "$manifest" | grep -q 'name: repo-clone' || \
    fail "missing repo-clone init container"

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
printf '%s' "$manifest" | grep -q 'secretProviderClass: github-spc' || \
    fail "missing github-spc CSI volume"

# The workflow-runner must mount only the Linear key; it must not see either
# GitHub token file.
runner_block=$(printf '%s' "$manifest" | awk '/name: workflow-runner/{flag=1} flag{print} /name: adapter-copilot/{flag=0}')
printf '%s' "$runner_block" | grep -q 'subPath: linear_api_key' || \
    fail "workflow-runner does not mount linear_api_key"
printf '%s' "$runner_block" | grep -q 'subPath: workflow_github_token' && \
    fail "workflow-runner mounts workflow_github_token"
printf '%s' "$runner_block" | grep -q 'subPath: reviewer_github_token' && \
    fail "workflow-runner mounts reviewer_github_token"

# adapter-copilot mounts both GitHub tokens.
copilot_block=$(printf '%s' "$manifest" | awk '/name: adapter-copilot/{flag=1} flag{print} /name: adapter-shell/{flag=0}')
printf '%s' "$copilot_block" | grep -q 'subPath: workflow_github_token' || \
    fail "adapter-copilot does not mount workflow_github_token"
printf '%s' "$copilot_block" | grep -q 'subPath: reviewer_github_token' || \
    fail "adapter-copilot does not mount reviewer_github_token"

# adapter-shell mounts only the workflow token.
shell_block=$(printf '%s' "$manifest" | awk '/name: adapter-shell/{flag=1} flag{print} /volumes:/{flag=0}')
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

echo "PASS: rendered pod-adapter Job manifest meets CRI-103 requirements"
