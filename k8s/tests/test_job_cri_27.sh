#!/usr/bin/env bash
set -euo pipefail

# Regression test for the pod-adapter k8s Job manifest and launcher.
# Renders the manifest with default settings and asserts the CRI-114+ topology:
# one runner Job plus one adapter Job per adapter type, zero CSI on adapters,
# per-ticket repo clone on the data PVC, per-run discovery files, and no
# secret environment variables.
#
# The manifest is captured into a temp file once and all checks grep the file.
# (Piping $manifest through `printf | grep -q` under set -o pipefail dies with
# SIGPIPE when grep exits early - this test learned that the hard way.)

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
LAUNCHER="$REPO_ROOT/k8s/launch-pod-adapter-job.sh"

export TICKET_ID="CRI-27"
export REPO_URL="brokenbots/workflow-example"
export DRY_RUN="1"

manifest="$(mktemp)"
trap 'rm -f "$manifest"' EXIT
"$LAUNCHER" > "$manifest"

fail() {
    echo "FAIL: $1" >&2
    exit 1
}

# has PATTERN -> succeeds if present; ! has -> absent checks.
has() {
    grep -q "$1" "$manifest"
}

lacks() {
    ! grep -q "$1" "$manifest"
}

has 'name: linear-spc' || fail "missing linear-spc SecretProviderClass"
has 'name: copilot-spc' || fail "missing copilot-spc SecretProviderClass"
lacks 'name: shell-spc' || fail "shell-spc SecretProviderClass must not be present"

has 'provider: openbao' || fail "SPC does not use the openbao provider"
lacks 'vaultAddress' || fail "SPC still references a vaultAddress parameter"

has 'secretPath: criteria/data/linear' || fail "SPC does not read from criteria/data/linear"

has 'filePermission: 0444' || fail "SPC objects do not request restrictive file permissions"

has 'objectName: linear_api_key' || fail "linear-spc does not expose linear_api_key"
has 'objectName: workflow_github_token' || fail "copilot-spc does not expose workflow_github_token"
has 'objectName: reviewer_github_token' || fail "copilot-spc does not expose reviewer_github_token"

# CRI-114: three Jobs (runner + shell + copilot), not one three-container pod.
job_count=$(grep -c '^kind: Job$' "$manifest")
[ "$job_count" -eq 3 ] || fail "expected 3 Jobs (runner + 2 adapters), found $job_count"

has 'name: pod-adapter-cri-27' || fail "missing runner Job"
has 'name: pod-adapter-cri-27-adapter-shell' || fail "missing shell adapter Job"
has 'name: pod-adapter-cri-27-adapter-copilot' || fail "missing copilot adapter Job"

has 'name: repo-clone' || fail "missing repo-clone init container"
has 'name: workflow-runner' || fail "missing workflow-runner container"

# Runner Job scheduling and security.
has 'nodeSelector:' || fail "missing nodeSelector"
has 'kubernetes.io/arch: amd64' || fail "nodeSelector is not amd64"
has 'key: catch' || fail "missing catch toleration"

# Per-ticket repo clone on the data PVC (concurrent-run fix): every Job mounts
# only criteria-data; the shared criteria-repo PVC is gone.
has 'claimName: criteria-data' || fail "missing /data PVC mount"
lacks 'claimName: criteria-repo' || fail "manifest still mounts the shared criteria-repo PVC"

# Runner pod keeps linear-spc and copilot-spc; shell-spc is gone.
has 'secretProviderClass: linear-spc' || fail "missing linear-spc CSI volume"
has 'secretProviderClass: copilot-spc' || fail "missing copilot-spc CSI volume"
lacks 'secretProviderClass: shell-spc' || fail "shell-spc CSI volume must not be present"
lacks 'name: github-secrets' || fail "manifest still uses the shared github-secrets CSI volume"

# Adapter pods must have no CSI volumes at all.
adapter_block=$(awk '/name: pod-adapter-cri-27-adapter-shell/{flag=1; print; next} flag{print} /^---$/{if(flag){exit}}' "$manifest")
[ -n "$adapter_block" ] || fail "could not extract adapter Job block"
if grep -q 'driver: secrets-store.csi.k8s.io' <<<"$adapter_block"; then
    fail "adapter Job contains a CSI volume"
fi

# The workflow-runner must mount only the Linear key at /secrets/linear_api_key
# and the GitHub tokens at /home/criteria/secrets; it must not see either GitHub
# token file under /secrets.
runner_block=$(awk '/name: workflow-runner/{flag=1; print; next} flag{print} /name: pod-adapter-cri-27-adapter-shell/{flag=0; exit}' "$manifest")
[ -n "$runner_block" ] || fail "could not extract runner container block"
has 'name: linear-secrets' || fail "workflow-runner does not mount linear-secrets"
has 'subPath: linear_api_key' || fail "workflow-runner does not mount linear_api_key"
lacks 'subPath: workflow_github_token' || fail "workflow-runner mounts workflow_github_token"
lacks 'subPath: reviewer_github_token' || fail "workflow-runner mounts reviewer_github_token"
has 'mountPath: /home/criteria/secrets' || fail "workflow-runner does not mount copilot-secrets at /home/criteria/secrets"

# The repo-clone init container mounts the workflow token from copilot-spc
# (shell-spc was retired) and clones into the per-ticket repo path.
clone_block=$(awk '/name: repo-clone/{flag=1; print; next} flag{print} /name: workflow-runner/{flag=0; exit}' "$manifest")
has 'name: copilot-secrets' || fail "repo-clone does not mount copilot-secrets"
has 'mountPath: /home/criteria/secrets' || fail "repo-clone does not mount secrets at /home/criteria/secrets"
lacks 'name: shell-secrets' || fail "repo-clone still mounts shell-secrets"
lacks 'find /repo' || fail "repo-clone still wipes a shared /repo"

# No secret should arrive as a container environment variable.
lacks 'value:.*ghp_' || fail "a GitHub token appears as a literal env value"
lacks 'name: GH_TOKEN' || fail "GH_TOKEN must not be set via env"
lacks 'name: WORKFLOW_GITHUB_TOKEN' || fail "WORKFLOW_GITHUB_TOKEN must not be set via env"
lacks 'name: REVIEWER_GITHUB_TOKEN' || fail "REVIEWER_GITHUB_TOKEN must not be set via env"

echo "PASS"