#!/usr/bin/env bash
set -euo pipefail

# Guard test (KB-24 review): every field of the Go CriteriaRunStatus struct
# must exist as a property in the status schema of every CriteriaRun CRD
# copy. The CRD status schemas enumerate status.properties explicitly
# (x-kubernetes-preserve-unknown-fields is not set), so the API server
# silently prunes any status field the Go types carry but the manifests
# omit — the field reads back as nil and every code path depending on it
# (e.g. the KB-24 stall watchdog's persisted lastStepProgress baseline)
# becomes dead code in-cluster while unit tests against the fake client
# still pass. The CRD copies are hand-maintained (no controller-gen
# generation step in this repo), so this test is the drift tripwire.
#
# Covers all three shipped copies: the chart CRD, the criteria-k8s config
# CRD, and the install.yaml bundle CRD.

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
TYPES="$REPO_ROOT/criteria-k8s/api/v1/criteriarun_types.go"
CRDS=(
    "$REPO_ROOT/charts/criteria-k8s/crds/criteria.brokenbots.dev_criteriaruns.yaml"
    "$REPO_ROOT/criteria-k8s/config/crd/bases/criteria.brokenbots.dev_criteriaruns.yaml"
    "$REPO_ROOT/criteria-k8s/config/install.yaml"
)

fail() {
    echo "FAIL: $1" >&2
    exit 1
}

[ -f "$TYPES" ] || fail "CriteriaRun types file missing: $TYPES"

# The status schema block of a CRD: from the 12-space-indented `status:`
# key until the next key at that indent, a document separator, or EOF.
crd_status_block() {
    awk '/^            status:$/{on = 1; next}
         on && /^---$/{on = 0}
         on && /^            [^ ]/{on = 0}
         on' "$1"
}

# The JSON names of every CriteriaRunStatus field, in declaration order,
# skipping json:"-" exclusions.
status_json_tags() {
    sed -n '/^type CriteriaRunStatus struct {/,/^}/p' "$TYPES" \
        | grep -o 'json:"[^"]*"' \
        | sed 's/^json:"//; s/"$//' \
        | cut -d, -f1 \
        | grep -v -- '-$'
}

TAGS="$(status_json_tags)"
[ -n "$TAGS" ] || fail "no JSON tags extracted from CriteriaRunStatus — extraction is broken, fix the test"

for crd in "${CRDS[@]}"; do
    [ -f "$crd" ] || fail "CRD missing: $crd"
    BLOCK="$(crd_status_block "$crd")"
    [ -n "$BLOCK" ] || fail "no status schema block found in $crd"
    while IFS= read -r tag; do
        [ -n "$tag" ] || continue
        printf '%s\n' "$BLOCK" | grep -q "^                ${tag}:$" || \
            fail "CriteriaRunStatus field \"$tag\" is absent from the status schema of $crd — the API server would prune it (update every CRD copy in the same commit as the Go type)"
    done <<EOF
$TAGS
EOF
done

# The three copies must stay in sync with each other: any status.properties
# set difference means a copy was updated without the others.
for crd in "${CRDS[@]:1}"; do
    diff <(crd_status_block "${CRDS[0]}") <(crd_status_block "$crd") > /dev/null 2>&1 || \
        fail "CRD status schemas have drifted: ${CRDS[0]} differs from $crd (the copies are hand-maintained; keep them byte-consistent in their status schema)"
done

echo "OK: every CriteriaRunStatus field is present in the status schema of all ${#CRDS[@]} CRD copies"