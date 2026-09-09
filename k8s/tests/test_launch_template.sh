#!/usr/bin/env bash
set -euo pipefail

# Regression test for the k8s Job template and launcher.
# Renders the manifest with default settings and asserts that it uses the
# remote-mode image, runs unprivileged, and keeps the required k8s attributes.

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
LAUNCHER="$REPO_ROOT/k8s/launch-ticket-job.sh"

export TICKET_ID="CRI-TEST"
export REPO_URL="brokenbots/workflow-example"
export LINEAR_API_KEY="fake-linear"
export WORKFLOW_GITHUB_TOKEN="fake-workflow"
export REVIEWER_GITHUB_TOKEN="fake-reviewer"
export DRY_RUN="1"
export CREATE_SECRET="false"

manifest="$("$LAUNCHER")"

fail() {
    echo "FAIL: $1" >&2
    exit 1
}

printf '%s' "$manifest" | grep -q 'image: localhost:5000/linear-intake-remote:dev' || \
    fail "template does not reference remote-mode image"

printf '%s' "$manifest" | grep -q 'localhost:5000/linear-intake-k8s:dev' && \
    fail "template still references the old sandbox-era image"

printf '%s' "$manifest" | grep -qi 'seccompProfile' && \
    fail "template contains a seccompProfile setting"

printf '%s' "$manifest" | grep -qi 'privileged: true' && \
    fail "template requests a privileged container"

printf '%s' "$manifest" | grep -q 'security-opt' && \
    fail "template contains a --security-opt reference"

printf '%s' "$manifest" | grep -q 'kubernetes.io/arch: amd64' || \
    fail "template missing amd64 nodeSelector"

printf '%s' "$manifest" | grep -q 'key: catch' || \
    fail "template missing catch-node toleration"

printf '%s' "$manifest" | grep -q 'claimName: criteria-data' || \
    fail "template missing /data PVC mount"

printf '%s' "$manifest" | grep -q 'claimName: criteria-repo' || \
    fail "template missing /repo PVC mount"

printf '%s' "$manifest" | grep -q 'mountPath: /data' || \
    fail "template missing /data volumeMount"

printf '%s' "$manifest" | grep -q 'mountPath: /repo' || \
    fail "template missing /repo volumeMount"

printf '%s' "$manifest" | grep -q 'runAsNonRoot: true' || \
    fail "template does not request a non-root securityContext"

echo "PASS: rendered k8s Job template meets CRI-110 requirements"
