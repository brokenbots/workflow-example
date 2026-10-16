#!/usr/bin/env bash
# Regression test for KB-265 (bounded feedback cycle) as amended by KB-267
# (branch-manager hand-off wipes developer context).
#
# KB-265 defect: the pair-feedback loop had NO termination for "work complete,
# reviewer approves, validator keeps needs_work'ing for process reasons" —
# nine byte-identical approvals then need_help, run parked. The fix bounds
# the cycle by rounds: after each completed cycle, the round check either
# re-enters validation with an incremented count or hands the FINAL SAY to
# the branch manager (dave rule).
#
# KB-267 finding (kb-264-1792110869): a developer-actionable blocker parked
# the run at the final say even though the manager had typed a remediation
# brief. The final say now supports THREE usable verdicts (dave ruling,
# live kb-264-1792110869):
#
#   branch_manager_final_say
#   ├─ converge_on_state → push_and_respond (normal completion → merge)
#   ├─ hand_off          → increment_hand_off_count (shell) → hand_off_route
#   │   ├─ count 1 → run_pair_programming_loop_with_feedback_r2 (round-fresh
#   │   │            instances developer_r2/reviewer_r2 in
#   │   │            workflows/pair_programming_loop_round2; the brief rides
#   │   │            developer_initial_prompt, the fresh session's first
#   │   │            prompt; KB-262 developer_feedback boundary write)
#   │   ├─ count 2 → run_pair_programming_loop_with_feedback_r3 (r3 instances)
#   │   └─ (default: defensive terminus; the manager template forbids a
#   │        third hand-off)
#   ├─ blocked / failure / default → state.failed with output.reason →
#   │   data.internal.failure_reason (KB-262)
#   └─ blocked is the verdict for non-actionable blockers (outside
#       dependency, human decision, immovable repo state) AND for the
#       hand-off budget exhausted / same finding with no state change —
#       the hand-off is bounded, exactly as KB-265 intended, and the
#       manager NEVER reopens the base (context-inheriting) developer dial.
#
# Both fresh-round dials complete into validate_round_check, which prints
# budget_spent again (the counter is spent for real), so each hand-off round
# returns to the manager for a fresh converge/hand_off/blocked call.
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

echo "==> Checking the branch manager's bounded hand-off routes through a fresh round..."
jq -e '
  .steps[] | select(.name == "branch_manager_final_say") |
  ([.outcomes[] | select(.name == "hand_off")][0].next) == "increment_hand_off_count"
  and ([.outcomes[] | select(.name == "converge_on_state")][0].next) == "push_and_respond"
  and ([.outcomes[] | select(.name == "blocked")][0].next) == "failed"
  and ([.outcomes[] | select(.name == "failure")][0].next) == "failed"
' "${COMPILE_OUT}" >/dev/null \
    && ok "hand_off increments deterministically; converge completes; explicit verdicts fail loud" \
    || fail "branch_manager_final_say verdict routes drifted from the KB-267 machine"

echo "==> Checking the branch manager can never reopen the base (context-inheriting) dial..."
jq -e '
  .steps[] | select(.name == "branch_manager_final_say") |
  ([.outcomes[].next] | index("run_pair_programming_loop_with_feedback") | not)
' "${COMPILE_OUT}" >/dev/null \
    && ok "branch_manager_final_say never re-enters the base developer dial (re-entry reuses the session; only round-fresh dials may follow a hand-off)" \
    || fail "branch_manager_final_say re-enters the base dial — the developer context would be inherited, the KB-267 wipe defeated"

echo "==> Checking hand-offs route deterministically to the two round-fresh dials..."
jq -e '
  .switches[] | select(.name == "hand_off_route") |
  ([.conditions[].match] | join(",")) == "trimspace(steps.increment_hand_off_count.stdout) == \"1\",trimspace(steps.increment_hand_off_count.stdout) == \"2\""
  and ([.conditions[].next] | join(",")) == "run_pair_programming_loop_with_feedback_r2,run_pair_programming_loop_with_feedback_r3"
  and .default_next == "failed"
' "${COMPILE_OUT}" >/dev/null \
    && ok "hand_off_route keys on the incrementer's stdout: 1 → r2 dial, 2 → r3 dial" \
    || fail "hand_off_route does not key the two round-fresh dials on the incrementer's stdout"

echo "==> Checking the round-fresh dials complete into the same round check..."
for dial in run_pair_programming_loop_with_feedback_r2 run_pair_programming_loop_with_feedback_r3; do
    jq -e --arg dial "${dial}" '
      .steps[] | select(.name == $dial) |
      ([.outcomes[] | select(.name == "success")][0].next) == "validate_round_check"
      and ([.outcomes[] | select(.name == "failure")][0].next) == "failed"
    ' "${COMPILE_OUT}" >/dev/null \
        && ok "${dial} returns to validate_round_check (budget stays spent → the manager decides again)" \
        || fail "${dial} does not complete into validate_round_check"
done

echo "==> Checking each hand-off round runs round-fresh copilot instances..."
for n in 2 3; do
    tree="${TREE_ROOT}/workflows/pair_programming_loop_round${n}"
    if test -f "${tree}/main.chcl" \
        && printf '%s' "$(sed -n '/^step "develop" {/,/^}/p' "${tree}/main.chcl")" | grep -q "target     = adapter.copilot.developer_r${n}" \
        && printf '%s' "$(sed -n '/^step "review" {/,/^}/p' "${tree}/main.chcl")" | grep -q "target     = adapter.copilot.reviewer_r${n}" \
        && grep -q "adapter \"copilot\" \"developer_r${n}\"" "${tree}/adapters.chcl" \
        && grep -q "adapter \"copilot\" \"developer_r${n}\"" "${tree}/.criteria.lock.hcl" \
        && ! grep -q "^adapter \"copilot\" \"developer\" {" "${tree}/adapters.chcl" \
        && ! grep -q "^adapter \"copilot\" \"reviewer\" {" "${tree}/adapters.chcl"; then
        ok "round ${n} tree isolates conversation identity (developer_r${n}/reviewer_r${n}, no base-instance target)"
    else
        fail "round ${n} tree does not isolate the copilot conversation identity"
    fi
done

echo "==> Checking the brief travels as the fresh session's first prompt..."
for n in 2 3; do
    block="$(sed -n '/^subworkflow "pair_programming_loop_feedback_r'"${n}"'" {/,/^}/p' "${TREE_ROOT}/main.chcl")"
    if printf '%s' "$block" | grep -q 'developer_initial_prompt = data.internal.developer_feedback.value' \
        && printf '%s' "$block" | grep -q 'source = "./workflows/pair_programming_loop_round'"${n}"'"'; then
        ok "round ${n} passes the developer_feedback brief as developer_initial_prompt (fresh session's first prompt)"
    else
        fail "round ${n} subworkflow does not pass the brief as the fresh session's first prompt"
    fi
done

echo "==> Checking the hand-off counter increments deterministically and starts at zero..."
if test -f "${TREE_ROOT}/scripts/increment_hand_off_count.sh.tftpl"; then
    block="$(sed -n '/^step "increment_hand_off_count" {/,/^}/p' "${TREE_ROOT}/main.chcl")"
    if printf '%s' "$block" | grep -q 'templatefile("./scripts/increment_hand_off_count.sh.tftpl"' \
        && printf '%s' "$block" | grep -q 'value  = trimspace(output.stdout)' \
        && grep -qF '((count + 1))' "${TREE_ROOT}/scripts/increment_hand_off_count.sh.tftpl" \
        && printf '%s' "$(sed -n '/^data "internal" "hand_off_count" {/,/^}/p' "${TREE_ROOT}/main.chcl")" | grep -q 'value = "0"'; then
        ok "shell incrementer stores the rendered count; counter seeds at 0 (no phantom hand-offs)"
    else
        fail "increment_hand_off_count does not deterministically increment the stored counter"
    fi
else
    fail "scripts/increment_hand_off_count.sh.tftpl missing"
fi

# ── Boundary writes and prompt wiring (source-level) ─────────────────────────

echo "==> Checking the typed-reason boundary writes on the final-say arms..."
write_count="$(printf '%s' "$step_block" | grep -c 'target = data.internal.failure_reason.value')"
if [ "$write_count" -eq 3 ]; then
    ok "blocked, failure and default arms all copy output.reason into failure_reason (KB-262)"
else
    fail "expected failure_reason writes on blocked/failure/default, found $write_count"
fi

echo "==> Checking the hand-off brief travels the developer_feedback boundary..."
brief_count="$(printf '%s' "$step_block" | grep -c 'target = data.internal.developer_feedback.value')"
last_brief="$(printf '%s' "$step_block" | grep -c 'target = data.internal.last_hand_off_brief.value')"
if [ "$brief_count" -eq 1 ] && [ "$last_brief" -eq 1 ]; then
    ok "the hand_off arm re-briefs developer_feedback and records last_hand_off_brief (KB-262 pattern)"
else
    fail "hand_off arm does not write the brief to developer_feedback + last_hand_off_brief (found $brief_count/$last_brief)"
fi

echo "==> Checking the manager's default verdict stays terminal..."
if printf '%s' "$step_block" | tr -s ' ' | grep -A1 ' outcome "default"' | grep -q 'next = state.failed'; then
    ok "unrecognized manager verdicts hit state.failed, never another dev round"
else
    fail "outcome default on branch_manager_final_say no longer routes to state.failed"
fi

echo "==> Checking the manager's verdict vocabulary is taught (hand_off + bounded)..."
if grep -q '"hand_off"' "${TREE_ROOT}/agents/branch_manager_final_say.md.tftpl" \
    && grep -qi 'new session with NO memory' "${TREE_ROOT}/agents/branch_manager_final_say.md.tftpl" \
    && grep -qi 'never pushes code and never' "${TREE_ROOT}/agents/branch_manager_final_say.md.tftpl" \
    && grep -qi 'the same finding returned with no state change' "${TREE_ROOT}/agents/branch_manager_final_say.md.tftpl"; then
    ok "the manager template teaches hand_off, the fresh-session rule, state-verification only, and the same-finding-blocks rule"
else
    fail "the manager template lost part of the KB-267 verdict vocabulary"
fi

echo "==> Checking the manager prompt receives the hand-off context..."
bm_block="$(sed -n '/^step "branch_manager_final_say" {/,/^}/p' "${TREE_ROOT}/main.chcl")"
if printf '%s' "$bm_block" | grep -q 'hand_off_count     = data.internal.hand_off_count.value' \
    && printf '%s' "$bm_block" | grep -q 'last_hand_off      = data.internal.last_hand_off_brief.value'; then
    ok "the manager sees hand-offs spent and the prior brief"
else
    fail "branch_manager_final_say prompt lost the hand-off context plumbing"
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