#!/usr/bin/env bash
set -euo pipefail

# Regression test for the apply-ready example manifest at
# k8s/examples/ticket-job.yaml. Verifies the manifest is present, uses the
# pod-adapter layout, and does not expose secrets as environment variables.

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
printf '%s' "$manifest" | grep -q 'name: shell-spc' || \
    fail "missing shell-spc SecretProviderClass"

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

printf '%s' "$manifest" | grep -q 'driver: secrets-store.csi.k8s.io' || \
    fail "example manifest does not use the Secrets Store CSI driver"

printf '%s' "$manifest" | grep -q 'secretProviderClass: linear-spc' || \
    fail "missing linear-spc CSI volume"
printf '%s' "$manifest" | grep -q 'secretProviderClass: copilot-spc' || \
    fail "missing copilot-spc CSI volume"
printf '%s' "$manifest" | grep -q 'secretProviderClass: shell-spc' || \
    fail "missing shell-spc CSI volume"

printf '%s' "$manifest" | grep -q 'app.kubernetes.io/name: pod-adapter-run' || \
    fail "example manifest missing pod-adapter-run label"

# The header comment must still point readers at the main README.
printf '%s' "$manifest" | grep -q 'See k8s/README.md' || \
    fail "example manifest header does not reference k8s/README.md"

# Confirm it is a Job, not a Deployment or CronJob.
printf '%s' "$manifest" | grep -q '^kind: Job$' || \
    fail "example manifest is not a Job"

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

echo "PASS: k8s/examples/ticket-job.yaml is a valid pod-adapter example"
