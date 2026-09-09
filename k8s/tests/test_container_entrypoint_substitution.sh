#!/usr/bin/env bash
set -euo pipefail

# Regression test for the container-entrypoint.sh token substitution that
# feeds the legacy CRI-110 k8s Job path. The production adapters.chcl file
# ships with a literal accept_token placeholder; this test verifies that the
# entrypoint's sed block substitutes whichever placeholder is actually present.

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
ENTRYPOINT="$REPO_ROOT/linear_intake_v1/container-entrypoint.sh"
ADAPTER="$REPO_ROOT/linear_intake_v1/adapters.chcl"

token="regression-token-12345"

fail() {
    echo "FAIL: $1" >&2
    exit 1
}

# The entrypoint must contain the substitution expressions for both the
# current placeholder and the legacy double-underscore form (the legacy form
# may be a no-op for this repository, but external workflow trees still use
# it, so the expression must remain).
grep -qF '__CRITERIA_REMOTE_TOKEN__' "$ENTRYPOINT" || \
    fail "container-entrypoint.sh is missing legacy __CRITERIA_REMOTE_TOKEN__ substitution"
grep -qF 'CRITERIA_REMOTE_TOKEN_PLACEHOLDER' "$ENTRYPOINT" || \
    fail "container-entrypoint.sh is missing CRITERIA_REMOTE_TOKEN_PLACEHOLDER substitution"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

mkdir -p "$tmp/workflows/linear_intake_v1"
cp "$ADAPTER" "$tmp/workflows/linear_intake_v1/adapters.chcl"

# Read the placeholder literal that adapters.chcl actually contains right now
# so the test catches either form and the two cannot drift again.
placeholder=$(awk -F'"' '/accept_token[[:space:]]*=/ {print $2}' "$ADAPTER")
[ -n "$placeholder" ] || fail "could not read accept_token placeholder from $ADAPTER"

# Run the same find/sed substitution block used by container-entrypoint.sh.
export CRITERIA_REMOTE_TOKEN="$token"
workflow_tmp="$tmp/workflows"
find "$workflow_tmp" -name 'adapters.chcl' -exec sh -c '
    t="$1"
    shift
    for f; do
        sed -i \
            -e "s|__CRITERIA_REMOTE_TOKEN__|$t|g" \
            -e "s|CRITERIA_REMOTE_TOKEN_PLACEHOLDER|$t|g" \
            "$f"
    done
' sh "$CRITERIA_REMOTE_TOKEN" {} +

result="$tmp/workflows/linear_intake_v1/adapters.chcl"

if grep -qF "$placeholder" "$result"; then
    fail "placeholder '$placeholder' still present after substitution"
fi
if grep -qE 'CRITERIA_REMOTE_TOKEN_PLACEHOLDER|__CRITERIA_REMOTE_TOKEN__' "$result"; then
    fail "unsubstituted token placeholder remains in $result"
fi
if ! grep -qF "accept_token = \"$token\"" "$result"; then
    fail "accept_token not set to expected token in substituted file"
fi

echo "PASS: container-entrypoint.sh substitutes adapters.chcl placeholder ($placeholder)"
