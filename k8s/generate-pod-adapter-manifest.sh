#!/usr/bin/env bash
set -euo pipefail

# Render k8s/operator-prereqs.yaml from k8s/operator-prereqs.yaml.tmpl by
# embedding the pod-adapter runner/adapter wrapper scripts and rewriting the
# __NAMESPACE__ placeholders. The manifest carries the copilot-spc
# SecretProviderClass and the pod-adapter-scripts ConfigMap the operator-side
# runner and adapter containers consume (KB-281: the linear-spc document and
# the job-cri-27 Job-group example went with the retired linear_* trees).
#
# The wrapper scripts are also copied into the criteria-k8s Helm chart
# (charts/criteria-k8s/scripts/) whose scripts ConfigMap template embeds them;
# test_criteria_k8s_chart.sh fails when the chart copies drift.

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TEMPLATE="$REPO_ROOT/k8s/operator-prereqs.yaml.tmpl"
OUTPUT="$REPO_ROOT/k8s/operator-prereqs.yaml"
CHART_SCRIPTS="$REPO_ROOT/charts/criteria-k8s/scripts"

if [ ! -d "$CHART_SCRIPTS" ]; then
    CHART_SCRIPTS=""
fi

awk -v RUNNER="$REPO_ROOT/k8s/pod-adapter-runner.sh" \
    -v ADAPTER="$REPO_ROOT/k8s/pod-adapter-adapter.sh" '
    /__RUNNER_SCRIPT__/ { while ((getline line < RUNNER) > 0) print "    " line; close(RUNNER); next }
    /__ADAPTER_SCRIPT__/ { while ((getline line < ADAPTER) > 0) print "    " line; close(ADAPTER); next }
    { print }
' "$TEMPLATE" | sed 's/namespace: __NAMESPACE__/namespace: criteria-jobs/g' > "$OUTPUT"

echo "Generated $OUTPUT"

# Keep the Helm chart's embedded copies of the wrapper scripts in sync.
if [ -n "$CHART_SCRIPTS" ]; then
    cp "$REPO_ROOT/k8s/pod-adapter-runner.sh" "$CHART_SCRIPTS/runner.sh"
    cp "$REPO_ROOT/k8s/pod-adapter-adapter.sh" "$CHART_SCRIPTS/adapter.sh"
    echo "Synced $CHART_SCRIPTS/{runner.sh,adapter.sh}"
fi
