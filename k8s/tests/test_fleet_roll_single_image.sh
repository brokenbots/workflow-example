#!/usr/bin/env bash
set -euo pipefail

# Regression test for KB-241: the criteria-k8s fleet rolls ONE image into
# EVERY deployment that carries a criteria-k8s binary.
#
# The criteria-k8s image bakes all three components (manager/operator,
# linear-watcher, kanboard-watcher), so operator and watcher deployments must
# ride the same image tag and move on every fleet roll. KB-241's live
# evidence: the operator rolled ahead (criteria-k8s:operator-m24) while the
# kanboard watcher sat on its private tag (kanboard-watcher-8) with a binary
# predating the KB-213/214 multi-adapter peer builder — a fleet where the
# components disagree about the deployed shape, which engine v0.5.48+'s
# fail-closed declared-adapter check turns into "every adapter dial rejected".
#
# This suite guards the static faces of that discipline:
#
#   1. the image bakes all three binaries (the mechanical precondition for
#      one image = one fleet binary set);
#   2. every shipped deployment manifest that runs a criteria-k8s binary
#      (operator, linear watcher, kanboard watcher) resolves the SAME
#      criteria-k8s image — no private tags, no per-component divergence;
#      the helm chart reuses the operator image for its watcher too;
#   3. make deploy-images rolls the operator and the kanboard watcher from
#      ONE freshly built tag (the executable discipline);
#   4. the roll gates on the rollout status of BOTH rolled deployments and
#      records the post-roll imageID digest read-back (the live verification
#      step the discipline names).

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
DOCKERFILE="$REPO_ROOT/criteria-k8s/Dockerfile"
INSTALL="$REPO_ROOT/criteria-k8s/config/install.yaml"
DEPLOY="$REPO_ROOT/criteria-k8s/config/manager/deployment.yaml"
KANBOARD="$REPO_ROOT/k8s/kanboard-watcher.yaml"
CHART_WATCHER="$REPO_ROOT/charts/criteria-k8s/templates/watcher-deployment.yaml"
MAKEFILE="$REPO_ROOT/Makefile"

fail() {
    echo "FAIL: $1" >&2
    exit 1
}

for file in "$DOCKERFILE" "$INSTALL" "$DEPLOY" "$KANBOARD" "$CHART_WATCHER" "$MAKEFILE"; do
    [ -f "$file" ] || fail "$file is missing"
done

# --- 1. one image bakes the whole component set ------------------------------
for binary in manager linear-watcher kanboard-watcher; do
    grep -Eq "COPY --from=builder /workspace/$binary /$binary" "$DOCKERFILE" || \
        fail "criteria-k8s/Dockerfile does not bake /$binary — the fleet-roll discipline needs ONE image carrying every component, not a per-component tag scheme"
done

# --- 2. every criteria-k8s deployment resolves the same image ----------------
# The criteria-k8s image references declared by a manifest (repository:tag).
criteria_k8s_images() {
    grep -E '^[[:space:]]*image:[[:space:]]*localhost:5000/criteria-k8s:' "$1" \
        | sed -E 's/^[[:space:]]*image:[[:space:]]*//; s/[[:space:]]*$//'
}

# The kanboard watcher historically fossilized a private tag while the
# operator rolled ahead (KB-241). Name that class explicitly first so the
# failure points at the regression instead of a bare tag mismatch.
if grep -Eq 'localhost:5000/criteria-k8s:kanboard-watcher-' "$KANBOARD"; then
    fail "k8s/kanboard-watcher.yaml pins a private kanboard-watcher-N tag — KB-241: that tag fossilizes a binary that predates the fleet; roll this deployment with the operator via make deploy-images"
fi

ref="$(criteria_k8s_images "$INSTALL" | head -1)"
[ -n "$ref" ] || fail "criteria-k8s/config/install.yaml declares no criteria-k8s image"

for file in "$INSTALL" "$DEPLOY" "$KANBOARD"; do
    imgs="$(criteria_k8s_images "$file")"
    [ -n "$imgs" ] || fail "$file runs no criteria-k8s image — every criteria-k8s component deployment must ride the shared fleet image"
    mismatched="$(printf '%s\n' "$imgs" | grep -vxF "$ref" || true)"
    if [ -n "$mismatched" ]; then
        fail "$file resolves criteria-k8s image(s) other than the fleet image $ref (KB-241 fleet-roll discipline: one image, all criteria-k8s deployments): $mismatched"
    fi
done

grep -qF 'image: "{{ .Values.images.operator.repository }}:{{ .Values.images.operator.tag }}"' "$CHART_WATCHER" || \
    fail "the helm chart's linear watcher does not reuse the operator image (images.operator.*) — the chart must keep one image per criteria-k8s component set"

# --- 3. deploy-images rolls operator + kanboard watcher from ONE tag ---------
tag='$(CRITERIA_K8S_IMAGE):$(BUILD_TAG)'
if ! grep -qF "kubectl -n criteria-jobs set image deploy/criteria-k8s-operator operator=$tag" "$MAKEFILE" \
    || ! grep -qF "kubectl -n criteria-jobs set image deploy/criteria-kanboard-watcher watcher=$tag" "$MAKEFILE"; then
    fail "make deploy-images must set deploy/criteria-k8s-operator AND deploy/criteria-kanboard-watcher from the SAME \$(CRITERIA_K8S_IMAGE):\$(BUILD_TAG) — rolling one without the other leaves a criterion-shaped deployment on a stale binary (KB-241)"
fi

# --- 4. rollout gates + the imageID digest read-back ------------------------
for dep in criteria-k8s-operator criteria-kanboard-watcher; do
    grep -qE "kubectl -n criteria-jobs rollout status deploy/$dep --timeout=" "$MAKEFILE" || \
        fail "make deploy-images does not gate on the rollout status of deploy/$dep — a half-rolled fleet must fail the command, not succeed silently"
done

grep -qF 'status.containerStatuses[*].imageID' "$MAKEFILE" || \
    fail "make deploy-images does not record the post-roll imageID digest read-back (KB-241: verify pod imageID digests — spec.image tags alone prove nothing when a reused tag hits an IfNotPresent cache)"

echo "PASS: KB-241 fleet-roll discipline — one criteria-k8s image ($ref) serves the operator and both watchers; deploy-images rolls operator + kanboard watcher together with rollout gates and the imageID digest read-back"