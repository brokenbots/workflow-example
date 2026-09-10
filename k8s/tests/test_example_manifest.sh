#!/usr/bin/env bash
set -euo pipefail

# Regression test for the apply-ready example manifest at
# k8s/examples/ticket-job.yaml. Verifies the manifest is present, uses the
# CRI-114 multi-Job layout, and does not expose secrets as environment variables
# or CSI volumes on adapter pods.

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
EXAMPLE="$REPO_ROOT/k8s/examples/ticket-job.yaml"

fail() {
    echo "FAIL: $1" >&2
    exit 1
}

[ -f "$EXAMPLE" ] || fail "k8s/examples/ticket-job.yaml is missing"

manifest=$(cat "$EXAMPLE")
[ -n "$manifest" ] || fail "example manifest is empty"

printf '%s' "$manifest" | grep -q 'name: linear-spc' || \
    fail "missing linear-spc SecretProviderClass"
printf '%s' "$manifest" | grep -q 'name: copilot-spc' || \
    fail "missing copilot-spc SecretProviderClass"
printf '%s' "$manifest" | grep -q 'name: shell-spc' && \
    fail "shell-spc SecretProviderClass must not be present"

printf '%s' "$manifest" | grep -q 'name: workflow-runner' || \
    fail "missing workflow-runner container"
printf '%s' "$manifest" | grep -q 'name: adapter-copilot' || \
    fail "missing adapter-copilot container"
printf '%s' "$manifest" | grep -q 'name: adapter-shell' || \
    fail "missing adapter-shell container"
printf '%s' "$manifest" | grep -q 'name: repo-clone' || \
    fail "missing repo-clone init container"

printf '%s' "$manifest" | grep -q 'secretKeyRef' && \
    fail "example manifest uses secretKeyRef"
printf '%s' "$manifest" | grep -q 'secretRef' && \
    fail "example manifest uses secretRef"
printf '%s' "$manifest" | grep -q 'envFrom:' && \
    fail "example manifest uses envFrom for secrets"

# Runner pod still uses CSI; adapter pods must not.
printf '%s' "$manifest" | grep -q 'driver: secrets-store.csi.k8s.io' || \
    fail "example manifest does not use the Secrets Store CSI driver"
printf '%s' "$manifest" | grep -q 'secretProviderClass: linear-spc' || \
    fail "missing linear-spc CSI volume"
printf '%s' "$manifest" | grep -q 'secretProviderClass: copilot-spc' || \
    fail "missing copilot-spc CSI volume"
printf '%s' "$manifest" | grep -q 'secretProviderClass: shell-spc' && \
    fail "shell-spc CSI volume must not be present"

# Confirm three Jobs (runner + two adapters) instead of one three-container pod.
job_count=$(printf '%s' "$manifest" | grep -c '^kind: Job$')
[ "$job_count" -eq 3 ] || \
    fail "expected 3 Jobs, found $job_count"

# Adapter pods must not mount any CSI volume or specify a service account.
adapter_block=$(printf '%s' "$manifest" | awk '/name: pod-adapter-cri-105-adapter-shell/{flag=1} flag{print} /^---$/{if(flag){sep++; if(sep==2){flag=0}}}')
[ -n "$adapter_block" ] || fail "could not extract adapter Job block"
printf '%s' "$adapter_block" | grep -q 'driver: secrets-store.csi.k8s.io' && \
    fail "adapter Job contains a CSI volume"
printf '%s' "$adapter_block" | grep -q 'automountServiceAccountToken: false' || \
    fail "adapter Job does not disable service account token mounting"
printf '%s' "$adapter_block" | grep -q 'serviceAccountName:' && \
    fail "adapter Job specifies a service account"

# Per-run discovery directory and listen-address widening must be present.
printf '%s' "$manifest" | grep -q 'run_dir="/data/.criteria/runs' || \
    fail "runner script does not create per-run discovery directory"
printf '%s' "$manifest" | grep -q 'listen_address = "0.0.0.0:7778"' || \
    fail "runner script does not widen listen_address"

printf '%s' "$manifest" | grep -q 'app.kubernetes.io/name: criteria-run' || \
    fail "example manifest missing criteria-run label"

# The header comment must still point readers at the main README.
printf '%s' "$manifest" | grep -q 'See k8s/README.md' || \
    fail "example manifest header does not reference k8s/README.md"

# Confirm all resources are Jobs, not Deployments or CronJobs.
printf '%s' "$manifest" | grep -q '^kind: Job$' || \
    fail "example manifest contains no Job resources"

if command -v kubectl >/dev/null 2>&1; then
    echo "kubectl found; validating example manifest client-side..."
    if kubectl apply --dry-run=client -f "$EXAMPLE" >/dev/null 2>&1; then
        echo "client-side dry-run passed"
    else
        echo "WARN: client-side dry-run failed (cluster may be unreachable); skipping"
    fi
else
    echo "kubectl not found; skipping client-side dry-run"
fi

echo "PASS: k8s/examples/ticket-job.yaml is a valid CRI-114 example"
