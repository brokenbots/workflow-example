#!/usr/bin/env bash
set -euo pipefail

# CRI-140: the operator's per-scope reconciler resolves the adapter KIND from
# the engine's adapter_type lifecycle field, first published in criteria
# v0.5.22 (CRI-141). The workflow image therefore must pin criteria >= v0.5.22;
# with an older engine the reconciler falls back to the adapter node name and
# builds image references that do not exist in the registry (e.g.
# criteria-adapter-intake), wedging per-scope pods in ImagePull.

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
DOCKERFILE="$REPO_ROOT/linear_intake_v1/Dockerfile"
MIN_VERSION="v0.5.22"

fail() {
    echo "FAIL: $1" >&2
    exit 1
}

[ -f "$DOCKERFILE" ] || fail "workflow Dockerfile is missing"

pin="$(awk '$1 == "ARG" && $2 ~ /^CRITERIA_VERSION=/ { sub(/^CRITERIA_VERSION=/, "", $2); print $2 }' "$DOCKERFILE")"
[ -n "$pin" ] || fail "Dockerfile does not pin CRITERIA_VERSION"

# The engine pin is only trustworthy when the image still verifies the
# downloaded archive against the release's SHA256SUMS.
grep -q 'criteria/releases/download/' "$DOCKERFILE" || \
    fail "Dockerfile no longer downloads the criteria archive from the release"
grep -q 'SHA256SUMS' "$DOCKERFILE" || \
    fail "Dockerfile no longer downloads SHA256SUMS"
grep -q 'sha256sum -c' "$DOCKERFILE" || \
    fail "Dockerfile no longer verifies the criteria archive against SHA256SUMS"

if [ "$(printf '%s\n' "$MIN_VERSION" "$pin" | sort -V | tail -1)" != "$pin" ]; then
    fail "pinned engine ($pin) predates $MIN_VERSION, which publishes adapter_type; per-scope adapter image resolution would fall back to the adapter node name and reference a non-existent registry image"
fi

echo "PASS: workflow image pins criteria $pin (>= $MIN_VERSION, publishes adapter_type)"