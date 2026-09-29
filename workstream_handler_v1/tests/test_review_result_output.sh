#!/usr/bin/env bash
# Regression test for KB-51: the handler must expose its final reviewer
# verdict as a workflow output.
#
# kanboard_develop_v1 copies subworkflow.review_result into its own
# data.internal channel on a handler success and spends it on the closing
# evidence comment (the KB-51 verdict/workstream path/PR url/commit-range
# comment). criteria compile does not validate subworkflow.<output> names —
# a dropped or renamed output resolves to null at runtime and compiles
# clean — so the declaration itself is pinned here at the layer that owns
# it:
#
#   - compiled form: the graph serializes an output named review_result
#     with type string;
#   - source: the output's value is data.internal.review_result.value (the
#     channel the reviewer loop feeds), scoped to the output block so a
#     match elsewhere in the file cannot satisfy it.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TREE_ROOT="${SCRIPT_DIR}/.."
COMPILE_OUT="$(mktemp)"
trap 'rm -f "${COMPILE_OUT}"' EXIT

command -v criteria >/dev/null 2>&1 || {
    echo "FAIL: criteria is not installed" >&2
    exit 1
}
command -v jq >/dev/null 2>&1 || {
    echo "FAIL: jq not found" >&2
    exit 1
}

FAILED=0
ok() {
    echo "PASS: $1"
}
fail() {
    FAILED=$((FAILED + 1))
    echo "FAIL: $1" >&2
}

echo "==> Compiling workstream_handler_v1..."
criteria compile "${TREE_ROOT}" >"${COMPILE_OUT}" 2>/dev/null \
    || { echo "FAIL: criteria compile workstream_handler_v1" >&2; exit 1; }

if jq -e '.outputs[] | select(.name == "review_result") | .type == "string"' \
    "${COMPILE_OUT}" >/dev/null; then
    ok "compiled graph serializes a string-typed review_result output"
else
    fail "compiled graph has no string review_result output — the develop tree's evidence copy would read null"
fi

echo "==> Checking the output declaration on the workflow source..."
output_block="$(sed -n '/^output "review_result" {/,/^}/p' "${TREE_ROOT}/main.chcl")"
if printf '%s' "$output_block" | grep -q 'type  = string' \
    && printf '%s' "$output_block" | grep -q 'value = data.internal.review_result.value'; then
    ok "output review_result is typed and sourced from data.internal.review_result.value"
else
    fail "output review_result missing or not sourced from data.internal.review_result.value"
fi

if [ "$FAILED" -eq 0 ]; then
    echo "ALL PASS: review_result output wiring (KB-51)"
    exit 0
fi
exit 1