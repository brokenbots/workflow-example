#!/usr/bin/env bash
set -euo pipefail

# Embed the pod-adapter runner/adapter wrapper scripts into the job-cri-27
# template, write the rendered manifest to k8s/job-cri-27.yaml, and generate
# k8s/operator-prereqs.yaml with the SecretProviderClasses and ConfigMap that
# the operator needs. The launcher later substitutes the remaining ${IMAGE},
# ${TICKET_ID}, etc. placeholders in job-cri-27.yaml.
#
# The wrapper scripts are also copied into the criteria-k8s Helm chart
# (charts/criteria-k8s/scripts/) whose scripts ConfigMap template embeds them;
# test_criteria_k8s_chart.sh fails when the chart copies drift.

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TEMPLATE="$REPO_ROOT/k8s/job-cri-27.yaml.tmpl"
OUTPUT="$REPO_ROOT/k8s/job-cri-27.yaml"
PREREQS="$REPO_ROOT/k8s/operator-prereqs.yaml"
CHART_SCRIPTS="$REPO_ROOT/charts/criteria-k8s/scripts"

if [ ! -d "$CHART_SCRIPTS" ]; then
    CHART_SCRIPTS=""
fi

indent() {
    sed 's/^/    /'
}

RUNNER_SCRIPT=$(indent < "$REPO_ROOT/k8s/pod-adapter-runner.sh")
ADAPTER_SCRIPT=$(indent < "$REPO_ROOT/k8s/pod-adapter-adapter.sh")
export RUNNER_SCRIPT ADAPTER_SCRIPT

perl -pe 's/__(RUNNER_SCRIPT|ADAPTER_SCRIPT)__/exists $ENV{$1} ? $ENV{$1} : die "template placeholder $1 is missing\n"/ge' \
    "$TEMPLATE" > "$OUTPUT"

# The first three documents of the rendered template are the two
# SecretProviderClasses and the pod-adapter-scripts ConfigMap; the operator
# install manifest expects these to exist already.
awk 'BEGIN{n=0} /^---$/{n++} n<3{print}' "$OUTPUT" | \
    sed 's/namespace: __NAMESPACE__/namespace: criteria-jobs/g' > "$PREREQS"

echo "Generated $OUTPUT"
echo "Generated $PREREQS"

# Keep the Helm chart's embedded copies of the wrapper scripts in sync.
if [ -n "$CHART_SCRIPTS" ]; then
    cp "$REPO_ROOT/k8s/pod-adapter-runner.sh" "$CHART_SCRIPTS/runner.sh"
    cp "$REPO_ROOT/k8s/pod-adapter-adapter.sh" "$CHART_SCRIPTS/adapter.sh"
    echo "Synced $CHART_SCRIPTS/{runner.sh,adapter.sh}"
fi
