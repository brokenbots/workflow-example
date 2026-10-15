#!/usr/bin/env bash
# Regression test for KB-265: the review-feedback cycle terminates.
#
# Defect: the pair-feedback loop had NO termination for "work complete,
# reviewer approves, validator keeps needs_work'ing for process reasons" —
# nine byte-identical approvals then need_help, run parked. The fix bounds
# the cycle by rounds: after each completed cycle, the round check either
# re-enters validation with an incremented count or hands the FINAL SAY to
# the branch manager (dave rule):
#
#   run_pair_programming_loop_with_feedback
#   └─ validate_round_check (shell)
#      ├─ success + "budget_spent" stdout → branch_manager_final_say
#      │   ├─ converge_on_state → push_and_respond  (normal completion → merge)
#      │   ├─ blocked / failure / default → state.failed
#      │   │   with output.reason → data.internal.failure_reason (KB-262)
#      │   └─ (the coordinator verifies ACTUAL branch/PR/CI state; it never
#      │       opens another dev round)
#      └─ success + "<n>" stdout → validate_developer_response (counter <n>)
#
# Blocked and failure arms must write the typed reason so the parent's
# handler_error write and the parked-ticket failure comment carry the
# coordinator's precision, not generic prose (KB-262 boundary writes).
#
# Routing assertions run against the compiled graph; write and prompt
# semantics are not serialized, so they are asserted on the workflow source.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TREE_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
COMPILE_OUT="$(mktemp)"
trap 'rm -f "${COMPILE_OUT}"' EXIT

FAILED=0
ok() {
    echo "PASS: $1"
}
fail() {
    FAILED=$((FAILED + 1))
    echo "FAIL: $1" >&2
}

# Source blocks asserted below (writes and default outcomes are not serialized
# into the compiled graph).
step_block="$(sed -n '/^step "branch_manager_final_say" {/,/^}/p' "${TREE_ROOT}/main.chcl")"

echo "==> Compiling workstream_handler_v1..."
cd "${TREE_ROOT}"
criteria compile . --format json --out "${COMPILE_OUT}" 2>/dev/null \
    || { echo "FAIL: criteria compile workstream_handler_v1" >&2; exit 1; }

# ── Round budget wiring (compiled routing) ───────────────────────────────────

echo "==> Checking the feedback cycle terminates in the round check..."
jq -e '
  .steps[] | select(.name == "run_pair_programming_loop_with_feedback") |
  ([.outcomes[] | select(.name == "success")][0].next) == "validate_round_check"
  and ([.outcomes[] | select(.name == "failure")][0].next) == "failed"
' "${COMPILE_OUT}" >/dev/null \
    && ok "the feedback loop routes every completion through validate_round_check" \
    || fail "the feedback loop no longer terminates in the round check"

echo "==> Checking the round check routes through the two-outcome switch..."
jq -e '
  .steps[] | select(.name == "validate_round_check") |
  ([.outcomes[] | select(.name == "success")][0].next) == "validate_round_check_route"
  and ([.outcomes[] | select(.name == "failure")][0].next) == "failed"
' "${COMPILE_OUT}" >/dev/null \
    && ok "shell outcome pair (success/failure) routes through the budget switch" \
    || fail "validate_round_check does not use the shell adapter's real success/failure pair"

echo "==> Checking budget spend hands the final say to the branch manager..."
jq -e '
  .switches[] | select(.name == "validate_round_check_route") |
  ([.conditions[].next] | join(",")) == "branch_manager_final_say"
  and .default_next == "validate_developer_response"
' "${COMPILE_OUT}" >/dev/null \
    && ok "budget_spent → branch manager, otherwise the coordinator validates" \
    || fail "the budget route does not reach branch_manager_final_say"

echo "==> Checking the budget-spent match keys on the script's stdout marker..."
jq -e '
  .switches[] | select(.name == "validate_round_check_route") |
  .conditions[0].match == "trimspace(steps.validate_round_check.stdout) == \"budget_spent\""
' "${COMPILE_OUT}" >/dev/null \
    && ok "the switch matches the exact stdout marker the script emits" \
    || fail "switch condition drifted from the script's stdout contract"

echo "==> Checking the branch manager's final say converges or fails loud..."
# NOTE: the compiler elides `outcome "default"` from the serialized graph its
# catch-all is enforced at runtime — its routing is asserted on source below.
jq -e '
  .steps[] | select(.name == "branch_manager_final_say") |
  ([.outcomes[] | select(.name == "converge_on_state")][0].next) == "push_and_respond"
  and ([.outcomes[] | select(.name == "blocked")][0].next) == "failed"
  and ([.outcomes[] | select(.name == "failure")][0].next) == "failed"
' "${COMPILE_OUT}" >/dev/null \
    && ok "converge_on_state takes the normal completion path; explicit verdicts fail loud" \
    || fail "branch_manager_final_say verdict routes are not terminate-or-converge"

echo "==> Checking the branch manager's catch-all verdict also terminates..."
if printf '%s' "$step_block" | tr -s ' ' | grep -A1 ' outcome "default"' | grep -q 'next = state.failed'; then
    ok "unrecognized verdicts hit state.failed, never another dev round"
else
    fail "outcome default on branch_manager_final_say no longer routes to state.failed"
fi

echo "==> Checking the branch manager cannot reopen a dev round..."
jq -e '
  .steps[] | select(.name == "branch_manager_final_say") |
  ([.outcomes[].next] | index("run_pair_programming_loop_with_feedback") | not)
' "${COMPILE_OUT}" >/dev/null \
    && ok "branch_manager_final_say never routes back into the developer loop (final say)" \
    || fail "branch_manager_final_say can reopen the developer loop — the cycle is unbounded again"

# ── Boundary writes and prompt wiring (source-level) ─────────────────────────

echo "==> Checking the typed-reason boundary writes on the final-say arms..."
write_count="$(printf '%s' "$step_block" | grep -c 'target = data.internal.failure_reason.value')"
if [ "$write_count" -eq 3 ]; then
    ok "blocked, failure and default arms all copy output.reason into failure_reason (KB-262)"
else
    fail "expected failure_reason writes on blocked/failure/default, found $write_count"
fi

echo "==> Checking the counter write rejects the budget_spent marker..."
check_block="$(sed -n '/^step "validate_round_check" {/,/^}/p' "${TREE_ROOT}/main.chcl")"
if printf '%s' "$check_block" | grep -q 'trimspace(output.stdout) == "budget_spent" ? data.internal.validate_rounds.value : trimspace(output.stdout)'; then
    ok "the success arm preserves the step-entry counter when budget is spent"
else
    fail "the counter write would store 'budget_spent' — it feeds branch_manager_final_say's prompt"
fi

echo "==> Checking the round check renders a shipped script template..."
if printf '%s' "$check_block" | grep -q 'templatefile("./scripts/validate_round_check.sh.tftpl"' \
    && test -f "${TREE_ROOT}/scripts/validate_round_check.sh.tftpl"; then
    ok "the rendered template exists — no render-time death of the round check"
else
    fail "validate_round_check renders ./scripts/validate_round_check.sh.tftpl but the file is absent"
fi

echo "==> Checking the round check feeds the branch manager's prompt..."
bm_block="$(sed -n '/^step "branch_manager_final_say" {/,/^}/p' "${TREE_ROOT}/main.chcl")"
if printf '%s' "$bm_block" | grep -q 'rounds             = data.internal.validate_rounds.value' \
    && printf '%s' "$bm_block" | grep -q 'target      = adapter.copilot.coordinator'; then
    ok "branch_manager_final_say is a coordinator verdict that receives the spent rounds"
else
    fail "branch_manager_final_say prompt wiring drifted (rounds/adapter target)"
fi

echo "==> Checking the counter starts deterministic..."
datablock="$(sed -n '/^data "internal" "validate_rounds" {/,/^}/p' "${TREE_ROOT}/main.chcl")"
if printf '%s' "$datablock" | grep -q 'value = "0"'; then
    ok "validate_rounds starts at 0 — the first entry increments, no phantom spend"
else
    fail "validate_rounds lacks the deterministic 0 seed"
fi

echo "==> Checking the convergence doctrine is taught to the validator..."
if grep -q "Convergence (KB-265)" "${TREE_ROOT}/agents/validate_developer_response.md.tftpl"; then
    ok "validator prompt carries the reviewer-approved + CI-green doctrine"
else
    fail "validate_developer_response lost the KB-265 convergence doctrine"
fi

if [ "$FAILED" -eq 0 ]; then
    echo "test_branch_manager_final_say: all checks passed"
    exit 0
fi
echo "test_branch_manager_final_say: $FAILED check(s) failed" >&2
exit 1