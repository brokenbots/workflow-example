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

grep -q <<< "$manifest" 'image: localhost:5000/linear-intake-remote:dev' || \
    fail "template does not reference remote-mode image"

grep -q <<< "$manifest" 'localhost:5000/linear-intake-k8s:dev' && \
    fail "template still references the old sandbox-era image"

grep -q <<< "$manifest"i 'seccompProfile' && \
    fail "template contains a seccompProfile setting"

grep -q <<< "$manifest"i 'privileged: true' && \
    fail "template requests a privileged container"

grep -q <<< "$manifest" 'security-opt' && \
    fail "template contains a --security-opt reference"

grep -q <<< "$manifest" 'kubernetes.io/arch: amd64' || \
    fail "template missing amd64 nodeSelector"

grep -q <<< "$manifest" 'key: catch' || \
    fail "template missing catch-node toleration"

grep -q <<< "$manifest" 'claimName: criteria-data' || \
    fail "template missing /data PVC mount"

grep -q <<< "$manifest" 'claimName: criteria-repo' || \
    fail "template missing /repo PVC mount"

grep -q <<< "$manifest" 'mountPath: /data' || \
    fail "template missing /data volumeMount"

grep -q <<< "$manifest" 'mountPath: /repo' || \
    fail "template missing /repo volumeMount"

grep -q <<< "$manifest" 'runAsNonRoot: true' || \
    fail "template does not request a non-root securityContext"

echo "PASS: rendered k8s Job template meets CRI-110 requirements"
