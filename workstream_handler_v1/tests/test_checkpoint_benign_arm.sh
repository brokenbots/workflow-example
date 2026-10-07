#!/usr/bin/env bash
# Regression test for the KB-96 run crash: push_checkpoint success-arm condition.
#
# The KB-211 benign-resume arm evaluated `contains(output.stdout,
# "resumed_no_work=1")`. HCL `contains()` requires a COLLECTION as its first
# argument; `output.stdout` is a string, so the expression compiled lazily but
# crashed AT RUNTIME the first time a checkpoint verdict succeeded:
#   main.chcl:205,21-30: Error in function call; Call to function "contains"
#   failed: argument must be list, tuple, or set.
# The crash is unconditional on the success arm - EVERY successful checkpoint
# kills the run after the checkpoint was already pushed (KB-96 lost nothing
# only because its branch was pushed first). The fix wraps the subject in
# `split(output.stdout, "\n")` so contains receives a list while keeping the
# token-present semantics.
#
# Validates the push_checkpoint outcome "success" arm:
#   1. contains() is never called directly on a bare string-typed subject
#      (output.stdout, steps.*.stdout, var.* of string type);
#   2. the benign-shape condition uses the safe split+contains form.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORKFLOW_DIR="${SCRIPT_DIR}/../workflows/pair_programming_loop"
MAIN_CHCL="${WORKFLOW_DIR}/main.chcl"

fail() { echo "FAIL: $1" >&2; exit 1; }

[ -f "$MAIN_CHCL" ] || fail "missing $MAIN_CHCL"

# 2) The safe split+contains form must be present (the KB-211 benign
#    discrimination is load-bearing - removing it regresses KB-211).
grep -q 'contains(split(output.stdout' "$MAIN_CHCL" || \
    fail "push_checkpoint success arm lost the split+contains benign-shape condition"

# 1) No direct contains(<string-typed subject>, ...) anywhere in the tree.
#    String-typed subjects observed in these trees: output.stdout,
#    steps.<name>.stdout, var.<name> string vars. A contains( whose first
#    argument starts with one of these WITHOUT an intervening split/list
#    constructor is the crash shape.
if grep -nE 'contains\((output\.stdout|steps\.[A-Za-z_][A-Za-z0-9_]*\.stdout|var\.[a-z_][a-z0-9_]*)[,)]' \
        "$MAIN_CHCL" "${WORKFLOW_DIR}"/../main.chcl >/dev/null 2>&1; then
    grep -nE 'contains\((output\.stdout|steps\.[A-Za-z_][A-Za-z0-9_]*\.stdout|var\.[a-z_][a-z0-9_]*)[,)]' \
        "$MAIN_CHCL" "${WORKFLOW_DIR}"/../main.chcl || true
    fail "direct contains() on a string-typed subject found (runtime crash: argument must be list, tuple, or set)"
fi

echo "PASS: push_checkpoint success arm uses split+contains on stdout " \
     "(no direct string-typed contains)"