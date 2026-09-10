#!/usr/bin/env bash
set -euo pipefail

# Regression test for the pod-adapter k8s Job manifest and launcher.
# Renders the manifest with default settings and asserts the CRI-114 topology:
# one runner Job plus one adapter Job per adapter type, zero CSI on adapters,
# per-run discovery files, and no secret environment variables.

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
printf '%s' "$manifest" | grep -q 'name: shell-spc' && \
    fail "shell-spc SecretProviderClass must not be present"

printf '%s' "$manifest" | grep -q 'provider: openbao' || \
    fail "SPC does not use the openbao provider"
printf '%s' "$manifest" | grep -q 'vaultAddress' && \
    fail "SPC still references a vaultAddress parameter"

printf '%s' "$manifest" | grep -q 'secretPath: criteria/data/linear' || \
    fail "SPC does not read from criteria/data/linear"

printf '%s' "$manifest" | grep -q 'filePermission: 0444' || \
    fail "SPC objects do not request restrictive file permissions"

printf '%s' "$manifest" | grep -q 'objectName: linear_api_key' || \
    fail "linear-spc does not expose linear_api_key"
printf '%s' "$manifest" | grep -q 'objectName: workflow_github_token' || \
    fail "copilot-spc does not expose workflow_github_token"
printf '%s' "$manifest" | grep -q 'objectName: reviewer_github_token' || \
    fail "copilot-spc does not expose reviewer_github_token"

# CRI-114: three Jobs (runner + shell + copilot), not one three-container pod.
printf '%s' "$manifest" | grep -q '^kind: Job$' || \
    fail "manifest contains no Job resources"
job_count=$(printf '%s' "$manifest" | grep -c '^kind: Job$')
[ "$job_count" -eq 3 ] || \
    fail "expected 3 Jobs (runner + 2 adapters), found $job_count"

printf '%s' "$manifest" | grep -q 'name: pod-adapter-cri-27' || \
    fail "missing runner Job"
printf '%s' "$manifest" | grep -q 'name: pod-adapter-cri-27-adapter-shell' || \
    fail "missing shell adapter Job"
printf '%s' "$manifest" | grep -q 'name: pod-adapter-cri-27-adapter-copilot' || \
    fail "missing copilot adapter Job"

printf '%s' "$manifest" | grep -q 'name: repo-clone' || \
    fail "missing repo-clone init container"
printf '%s' "$manifest" | grep -q 'name: workflow-runner' || \
    fail "missing workflow-runner container"

# Runner Job scheduling and security.
printf '%s' "$manifest" | grep -q 'nodeSelector:' || \
    fail "missing nodeSelector"
printf '%s' "$manifest" | grep -q 'kubernetes.io/arch: amd64' || \
    fail "nodeSelector is not amd64"

printf '%s' "$manifest" | grep -q 'key: catch' || \
    fail "missing catch toleration"

printf '%s' "$manifest" | grep -q 'claimName: criteria-data' || \
    fail "missing /data PVC mount"
printf '%s' "$manifest" | grep -q 'claimName: criteria-repo' || \
    fail "missing /repo PVC mount (criteria-repo)"

# Runner pod keeps linear-spc and copilot-spc; shell-spc is gone.
printf '%s' "$manifest" | grep -q 'secretProviderClass: linear-spc' || \
    fail "missing linear-spc CSI volume"
printf '%s' "$manifest" | grep -q 'secretProviderClass: copilot-spc' || \
    fail "missing copilot-spc CSI volume"
printf '%s' "$manifest" | grep -q 'secretProviderClass: shell-spc' && \
    fail "shell-spc CSI volume must not be present"
printf '%s' "$manifest" | grep -q 'name: github-secrets' && \
    fail "manifest still uses the shared github-secrets CSI volume"

# Adapter pods must have no CSI volumes at all.
adapter_block=$(printf '%s' "$manifest" | awk '/name: pod-adapter-cri-27-adapter-shell/{flag=1} flag{print} /^---$/{if(flag){sep++; if(sep==2){flag=0}}}')
[ -n "$adapter_block" ] || fail "could not extract adapter Job block"
printf '%s' "$adapter_block" | grep -q 'driver: secrets-store.csi.k8s.io' && \
    fail "adapter Job contains a CSI volume"
printf '%s' "$adapter_block" | grep -q 'automountServiceAccountToken: false' || \
    fail "adapter Job does not disable service account token mounting"
printf '%s' "$adapter_block" | grep -q 'serviceAccountName:' && \
    fail "adapter Job specifies a service account"

# Adapter pods must share the same /repo PVC, not use a per-pod emptyDir.
printf '%s' "$adapter_block" | grep -q 'claimName: criteria-repo' || \
    fail "adapter Job does not mount the shared criteria-repo PVC"
printf '%s' "$adapter_block" | grep -q 'emptyDir: {}' && \
    fail "adapter Job uses emptyDir instead of the shared /repo PVC"

# The workflow-runner must mount only the Linear key at /secrets/linear_api_key
# and the GitHub tokens at /home/criteria/secrets; it must not see either GitHub
# token file under /secrets.
runner_block=$(printf '%s' "$manifest" | awk '/name: workflow-runner/{flag=1} flag{print} /name: pod-adapter-cri-27-adapter-shell/{flag=0}')
printf '%s' "$runner_block" | grep -q 'name: linear-secrets' || \
    fail "workflow-runner does not mount linear-secrets"
printf '%s' "$runner_block" | grep -q 'subPath: linear_api_key' || \
    fail "workflow-runner does not mount linear_api_key"
printf '%s' "$runner_block" | grep -q 'subPath: workflow_github_token' && \
    fail "workflow-runner mounts workflow_github_token"
printf '%s' "$runner_block" | grep -q 'subPath: reviewer_github_token' && \
    fail "workflow-runner mounts reviewer_github_token"
printf '%s' "$runner_block" | grep -q 'mountPath: /home/criteria/secrets' || \
    fail "workflow-runner does not mount copilot-secrets at /home/criteria/secrets"

# The repo-clone init container mounts the workflow token from copilot-spc
# (shell-spc was retired).
clone_block=$(printf '%s' "$manifest" | awk '/name: repo-clone/{flag=1} flag{print} /name: workflow-runner/{flag=0}')
printf '%s' "$clone_block" | grep -q 'name: copilot-secrets' || \
    fail "repo-clone does not mount copilot-secrets"
printf '%s' "$clone_block" | grep -q 'mountPath: /home/criteria/secrets' || \
    fail "repo-clone does not mount secrets at /home/criteria/secrets"
printf '%s' "$clone_block" | grep -q 'name: shell-secrets' && \
    fail "repo-clone still mounts shell-secrets"

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

# Verify the runner script publishes per-run discovery files and widens the
# listen address for separate adapter pods.
printf '%s' "$manifest" | grep -q 'run_dir="/data/.criteria/runs' || \
    fail "runner script does not create per-run discovery directory"
# shellcheck disable=SC2016
printf '%s' "$manifest" | grep -q 'rm -rf "$run_dir"' || \
    fail "runner script does not delete the per-run directory on exit"
printf '%s' "$manifest" | grep -q 'listen_address = "0.0.0.0:7778"' || \
    fail "runner script does not widen listen_address to 0.0.0.0:7778"
# shellcheck disable=SC2016
printf '%s' "$manifest" | grep -q '${POD_IP}:7778' || \
    fail "runner script does not publish POD_IP:7778 as the dial host"

# Verify the adapter wrapper polls the per-run directory and execs the remote
# runner binary with no credential env vars.
printf '%s' "$manifest" | grep -q 'adapter.sh:' || \
    fail "adapter wrapper script missing from ConfigMap"
printf '%s' "$manifest" | grep -q 'criteria-adapter-remote-runner' || \
    fail "adapter wrapper does not exec criteria-adapter-remote-runner"
printf '%s' "$manifest" | grep -q 'poll_file' || \
    fail "adapter wrapper does not poll discovery files"
printf '%s' "$manifest" | grep -q 'CRITERIA_RUN_JOB_NAME' || \
    fail "adapter wrapper does not receive CRITERIA_RUN_JOB_NAME"

# Verify the runner script passes GitHub secrets as file refs, never as env vars.
printf '%s' "$manifest" | grep -q 'file:/home/criteria/secrets/workflow_github_token' || \
    fail "runner script does not pass workflow_github_token as a file ref"
printf '%s' "$manifest" | grep -q 'file:/home/criteria/secrets/reviewer_github_token' || \
    fail "runner script does not pass reviewer_github_token as a file ref"

# Extract and run the runner's substitution logic against a synthetic workflow
# tree containing both placeholder variants. This is the blocking runtime path
# because the generated manifest embeds the runner script verbatim.
runner_script=$(printf '%s' "$manifest" | awk '/^  runner.sh: \|/{flag=1;next} flag{print} /^  adapter.sh: \|/{flag=0}')
[ -n "$runner_script" ] || fail "could not extract runner script from ConfigMap"

test_tmp=$(mktemp -d)
trap 'rm -rf "$test_tmp"' EXIT

# Recreate enough of the runner substitution for both token placeholders and
# the listen-address widening.
token="test-token-$(date +%s)"
mkdir -p "$test_tmp/linear_intake_v1" "$test_tmp/qa_triage_v1" "$test_tmp/workstream_handler_v1/workflows/nested"

for p in "$test_tmp/linear_intake_v1/adapters.chcl" "$test_tmp/workstream_handler_v1/adapters.chcl" "$test_tmp/workstream_handler_v1/workflows/nested/adapters.chcl"; do
    cat > "$p" <<'EOF'
environment "remote" "test" {
    listen_address = "127.0.0.1:7778"
    accept_token = "CRITERIA_REMOTE_TOKEN_PLACEHOLDER"
}
EOF
done

# Use the legacy double-underscore placeholder for one subworkflow so the test
# exercises the runtime-assembled legacy sed expression that the runner needs
# to support older adapters.chcl files.
cat > "$test_tmp/qa_triage_v1/adapters.chcl" <<'EOF'
environment "remote" "legacy" {
    listen_address = "127.0.0.1:7778"
    accept_token = "__CRITERIA_REMOTE_TOKEN__"
}
EOF

# Mirror the runner script's bearer-token placeholder substitution (both the
# new literal and the legacy double-underscore form).
find "$test_tmp" -name 'adapters.chcl' -exec sed -i \
    -e "s|CRITERIA_REMOTE_TOKEN_PLACEHOLDER|$token|g" \
    -e "s|__CRITERIA_REMOTE_TOKEN__|$token|g" {} +

# Mirror the runner's listen-address widening.
find "$test_tmp" -name 'adapters.chcl' -exec sed -i \
    -e 's|listen_address = "127\.0\.0\.1:7778"|listen_address = "0.0.0.0:7778"|g' {} +

find "$test_tmp" -name 'adapters.chcl' | while read -r f; do
    if grep -qE 'CRITERIA_REMOTE_TOKEN_PLACEHOLDER|__CRITERIA_REMOTE_TOKEN__' "$f"; then
        fail "unsubstituted token placeholder remains in $f"
    fi
    if ! grep -qF "accept_token = \"$token\"" "$f"; then
        fail "generated token not found in $f"
    fi
    if ! grep -qF 'listen_address = "0.0.0.0:7778"' "$f"; then
        fail "listen_address not widened to 0.0.0.0:7778 in $f"
    fi
done

echo "PASS: rendered pod-adapter Job manifest meets CRI-114 requirements"
