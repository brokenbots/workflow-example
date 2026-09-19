#!/usr/bin/env bash
set -euo pipefail

# CRI-234 deploy-pairing guard for the criteria build input (fail closed).
#
# The per-(scope,environment) co-location reads the environment_type /
# environment_name keys the engine publishes on adapter lifecycle events
# from criteria commit fc95449 (CRI-233) onward. Older runners emit no
# environment identity, so every provision takes the env-less per-adapter
# fallback and the co-location never activates - silently, with all tests
# still green, because only this build input pairs the operator with a
# runner that emits the keys.
#
# The criteria-base image builds the rolled runner from CRITERIA_COMMIT,
# so the pin must be exactly the audited commit:
#
#   - a pin predating fc95449 ships a runner that never emits the identity
#     keys, leaving the co-location inert;
#   - a pin beyond fc95449 ships an emitted contract that has not been
#     audited against the operator's event parser.
#
# To move off fc95449, re-audit the emitted adapter event contract, then
# update AUDITED_COMMIT here and the equality checks in
# k8s/tests/test_criteria_base.sh and criteria-base/tests/smoke_test.sh in
# the same change.
#
# Image-mode runs keep the baked workflow image (linear_intake_v1) built
# from a released criteria tarball; no criteria release contains fc95449,
# so image-mode pairing is a coordinator decision and is NOT guarded here.
# It is documented in linear_intake_v1/Dockerfile and the Makefile deploy
# targets.

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
DOCKERFILE="$REPO_ROOT/criteria-base/Dockerfile"
AUDITED_COMMIT="fc9544979ee698f111035368c654b415db943e66"

fail() {
    echo "FAIL: $1" >&2
    exit 1
}

[ -f "$DOCKERFILE" ] || fail "criteria-base/Dockerfile missing"

pin="$(awk -F= '/^ARG CRITERIA_COMMIT=/ {print $2}' "$DOCKERFILE" | tr -d '[:space:]')"
[ -n "$pin" ] || fail "Dockerfile does not pin CRITERIA_COMMIT"

if [ "$pin" != "$AUDITED_COMMIT" ]; then
    fail "criteria build input $pin is not the audited commit $AUDITED_COMMIT: a pin predating fc95449 never emits environment_type / environment_name, so the CRI-234 co-location stays inert, and a pin beyond fc95449 carries an unaudited event contract; re-audit the emitted contract, then update AUDITED_COMMIT in k8s/tests/test_criteria_base_pin.sh (and the equality checks in k8s/tests/test_criteria_base.sh and criteria-base/tests/smoke_test.sh) in the same change"
fi

echo "PASS: criteria-base pins the audited criteria commit $AUDITED_COMMIT (CRI-234 deploy pairing)"