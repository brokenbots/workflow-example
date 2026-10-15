#!/usr/bin/env bash
# Regression test for KB-265: the pair-feedback round budget check script.
#
# validate_round_check.sh.tftpl is the terminal bound on the
# review-feedback cycle: every validation entry either
#   - prints the incremented round count (validation re-enters with it), or
#   - prints "budget_spent" (the branch manager decides convergence — dave
#     rule: branch manager gets the final say when a reviewer-approved,
#     CI-green diff keeps getting needs_work'd for process reasons).
#
# The original KB-265 hand implementation referenced this script without
# shipping it, so the step died at template render before any routing ever
# happened. This test pins the script's observable behavior — the exact
# stdout contract that switch.validate_round_check_route (startswith/
# trimspace match on "budget_spent") and the success-outcome counter write
# (trimspace'd value) depend on:
#
#   - rounds < budget   → "<rounds+1>", exit 0
#   - rounds >= budget  → "budget_spent", exit 0
#   - empty/garbage counters count as 0 (bounded, never unbounded)
#   - an empty or zero budget spends immediately: the branch manager gets
#     the say after the first completed cycle, never an infinite loop.
#
# Rendered inputs substitute through shellquote; these test values are pure
# digits so a plain sed substitution is safe and matches what criteria
# renders for var.max_pair_cycles (number) and data.internal.validate_rounds
# (string counter).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TREE_ROOT="${SCRIPT_DIR}/.."
SCRIPT="$TREE_ROOT/scripts/validate_round_check.sh.tftpl"
RENDERED="${TMPDIR:-/tmp}/validate_round_check_test.$$.sh"
trap 'rm -f "${RENDERED}"' EXIT

FAILED=0
ok() {
    echo "PASS: $1"
}
fail() {
    FAILED=$((FAILED + 1))
    echo "FAIL: $1" >&2
}

test -f "$SCRIPT" || {
    echo "FAIL: missing $SCRIPT — the round check renders a nonexistent template at runtime" >&2
    exit 1
}

render() {
    # $1 = budget (criteria_value_1 = var.max_pair_cycles)
    # $2 = rounds (criteria_value_2 = data.internal.validate_rounds.value)
    # Substitutions emulate the tree's `| shellquote` filter (single-quote
    # wrapping); all test values are shellquote-safe literals.
    sed -e "s@{{ \.criteria_value_1 | shellquote }}@'$1'@g" \
        -e "s@{{ \.criteria_value_2 | shellquote }}@'$2'@g" \
        "$SCRIPT" >"$RENDERED"
}

check() {
    # $1 = budget, $2 = rounds, $3 = expected stdout, $4 = description
    render "$1" "$2"
    out="$(sh "$RENDERED" 2>/tmp/validate_round_check_test.$$.err || true)"
    rc=$?
    if [ "$out" = "$3" ] && [ "$rc" = "0" ]; then
        ok "$4"
    else
        fail "$4 — got stdout='$out' rc=$rc (stderr: $(cat /tmp/validate_round_check_test.$$.err))"
    fi
    rm -f /tmp/validate_round_check_test.$$.err
}

echo "==> Checking the continue path increments and re-enters validation..."
check 5 0 "1" "first entry (0 rounds spent, budget 5) prints 1 and exits 0"
check 5 3 "4" "mid-run entry (3 rounds spent) prints 4 and exits 0"
check 5 4 "5" "last remaining entry (4 rounds spent) prints 5 and exits 0"

echo "==> Checking the budget-spent path hands the say to the branch manager..."
check 5 5 "budget_spent" "budget exhausted (rounds == budget) prints budget_spent and exits 0"
check 5 9 "budget_spent" "an already-over counter stays spent (bounded, no wraparound)"
check 2 2 "budget_spent" "budget 2 with 2 rounds spent routes to the branch manager"

echo "==> Checking a zero/empty budget stays terminated..."
check 0 0 "budget_spent" "budget 0 spends immediately — branch manager after the first cycle"
check "" 0 "budget_spent" "empty budget renders as 0 and spends immediately"

echo "==> Checking garbage counters are bounded, not bypassed..."
check 5 "" "1" "empty rounds counter counts as 0"
check 5 "  7 " "budget_spent" "non-canonical rounds counter is digit-trimmed before compare"
check 5 "abc" "1" "garbage rounds counter counts as 0"

echo "==> Checking the script is strict about its own failures..."
render 5 0
if grep -q 'set -eu' "$RENDERED"; then
    ok "script fails loud on shell errors (set -eu), never loops on a broken counter"
else
    fail "script lost its set -eu strictness"
fi

if [ "$FAILED" -eq 0 ]; then
    echo "test_validate_round_check: all checks passed"
    exit 0
fi
echo "test_validate_round_check: $FAILED check(s) failed" >&2
exit 1