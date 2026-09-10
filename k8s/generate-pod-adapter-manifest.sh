#!/usr/bin/env bash
set -euo pipefail

# Embed the pod-adapter runner/sidecar scripts into the job-cri-27 template
# and write the rendered manifest to k8s/job-cri-27.yaml. The launcher later
# substitutes the remaining ${IMAGE}, ${TICKET_ID}, etc. placeholders.

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TEMPLATE="$REPO_ROOT/k8s/job-cri-27.yaml.tmpl"
OUTPUT="$REPO_ROOT/k8s/job-cri-27.yaml"

indent() {
    sed 's/^/    /'
}

RUNNER_SCRIPT=$(indent < "$REPO_ROOT/k8s/pod-adapter-runner.sh")
ADAPTER_SCRIPT=$(indent < "$REPO_ROOT/k8s/pod-adapter-sidecar.sh")
export RUNNER_SCRIPT ADAPTER_SCRIPT

perl -pe 's/__(RUNNER_SCRIPT|ADAPTER_SCRIPT)__/exists $ENV{$1} ? $ENV{$1} : die "template placeholder $1 is missing\n"/ge' \
    "$TEMPLATE" > "$OUTPUT"

echo "Generated $OUTPUT"
