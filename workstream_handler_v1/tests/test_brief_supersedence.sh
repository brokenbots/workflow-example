#!/usr/bin/env bash
# Regression test for KB-151 (brief supersedence after reviewer-directed
# branch mutations), reproduced live 2026-11-05
# (castle run 3bdb62da-9526-4d95-9582-cb8d54a2492b).
#
# Defect: the review loop held TWO requirement interpreters with DIFFERENT
# requirement sets. Reviewer changes_requested ruled an out-of-scope commit
# (12fd86b, CI-infra hardening) must be stripped; develop stripped the branch
# (reset+force-push to the approved f55fca6); validate_developer_response
# approved the post-strip state; then push_and_respond judged the branch
# against the STILL-STANDING pre-strip brief (which still required the
# hardening carried on the branch) → needs_work → run terminated
# need_assistance. Reviewer-directed strips/resets never invalidated the
# stale brief items, and nothing arbitrated between the two requirement
# sets because it is ONE coordinator session: every brief ever written
# rides push_and_respond's context.
#
# Fix (uniform stamp + no-re-judgment): every arm that verdicts the branch
# state before push_and_respond re-stamps data.internal.developer_feedback
# with its own typed reason — the LATEST validated requirement set:
#
#   validate_developer_response approved → push_and_respond
#   triage_pr_feedback      no_work     → push_and_respond
#   branch_manager_final_say converge_on_state → push_and_respond
#
# push_and_respond's prompt then re-derives its requirement set from that
# stamp (rendered as <validated_state>) instead of its session context, is
# forbidden from re-judging requirements (needs_work is not a valid verdict
# on an already-validated state), and is taught the supersedence rule: a
# reviewer-directed strip/reset wins over earlier carry-requirements, and
# the stripped commit is recoverable from git history and surfaced for a
# separate card — never re-required.
#
# The prompt templates carry the same doctrine: triage assembles its brief
# from the latest ruling (never a brief requiring both carry and strip),
# and validate_developer_response never routes needs_work for a stripped
# commit's absence.
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

block_of() {
    sed -n "/^step \"$1\" {/,/^}/p" "${TREE_ROOT}/main.chcl"
}

echo "==> Compiling workstream_handler_v1..."
cd "${TREE_ROOT}"
criteria compile . --format json --out "${COMPILE_OUT}" 2>/dev/null \
    || { echo "FAIL: criteria compile workstream_handler_v1" >&2; exit 1; }

# ── Every verdict arm re-stamps the brief before push_and_respond (routing) ──

for pair in validate_developer_response:approved triage_pr_feedback:no_work branch_manager_final_say:converge_on_state; do
    step="${pair%%:*}"
    outcome="${pair##*:}"
    echo "==> Checking ${outcome} of ${step} routes through push_and_respond..."
    jq -e --arg step "${step}" --arg outcome "${outcome}" '
      .steps[] | select(.name == $step) |
      ([.outcomes[] | select(.name == $outcome)][0].next) == "push_and_respond"
    ' "${COMPILE_OUT}" >/dev/null \
        && ok "${outcome} → push_and_respond (the validated state always reaches the push step)" \
        || fail "${outcome} of ${step} no longer pushes the validated state"
done

echo "==> Checking push_and_respond still completes into the reviewer loop..."
jq -e '
  .steps[] | select(.name == "push_and_respond") |
  ([.outcomes[] | select(.name == "success")][0].next) == "run_pr_reviewer_loop"
' "${COMPILE_OUT}" >/dev/null \
    && ok "a completed push re-enters the reviewer loop (need_assistance-free completion path)" \
    || fail "push_and_respond success does not re-enter the reviewer loop"

# ── The stamp is written by every entry arm (source-level) ───────────────────

for pair in validate_developer_response:approved triage_pr_feedback:no_work branch_manager_final_say:converge_on_state; do
    step="${pair%%:*}"
    outcome="${pair##*:}"
    echo "==> Checking ${outcome} of ${step} stamps developer_feedback..."
    arm_block="$(block_of "${step}" | sed -n "/^    outcome \"${outcome}\" {/,/^    }/p")"
    if printf '%s' "$arm_block" | grep -q 'target = data.internal.developer_feedback.value' \
        && printf '%s' "$arm_block" | grep -q 'value  = output.reason'; then
        ok "${outcome} re-stamps the standing brief with its typed verdict (latest validated set)"
    else
        fail "${outcome} of ${step} does not stamp developer_feedback — a stale brief survives into push_and_respond"
    fi
done

echo "==> Checking push_and_respond's prompt carries the validated state..."
push_block="$(block_of "push_and_respond")"
if printf '%s' "$push_block" | grep -q 'validated_state = data.internal.developer_feedback.value'; then
    ok "push_and_respond renders its requirement set from the stamped validated state (not session context)"
else
    fail "push_and_respond does not receive the validated state"
fi

# ── push_and_respond template: re-derivation, no re-judgment, supersedence ───

pr_tpl="${TREE_ROOT}/agents/push_and_respond.md.tftpl"

echo "==> Checking the push template renders the validated state..."
if grep -q '{{ .validated_state }}' "${pr_tpl}"; then
    ok "the push prompt renders the latest validated requirement set inline"
else
    fail "the push prompt never renders the validated state — the stale brief still wins"
fi

echo "==> Checking the push template forbids requirement re-judgment..."
if grep -q 'Do NOT judge the branch against the workstream file' "${pr_tpl}" \
    && grep -q 'earlier brief' "${pr_tpl}" \
    && grep -q 'Requirement interpretation is owned by the' "${pr_tpl}"; then
    ok "the push step is scoped to push + respond — it may not re-interpret requirements"
else
    fail "the push template no longer forbids re-judging requirements against stale context"
fi

echo "==> Checking the push template teaches the strip supersedence rule..."
if grep -q 'Supersedence (KB-151)' "${pr_tpl}" \
    && grep -q 'Never re-require the stripped' "${pr_tpl}" \
    && grep -q 'never revert the strip' "${pr_tpl}" \
    && grep -q 'separate card' "${pr_tpl}"; then
    ok "reviewer strip rulings supersede earlier carry-requirements; the stub is tracked, not re-required"
else
    fail "the push template lost part of the KB-151 supersedence doctrine"
fi

echo "==> Checking the push template answers contradicting threads without re-opening judgment..."
if grep -q 'contradicts the validated state' "${pr_tpl}" \
    && grep -q 'supersedence decision' "${pr_tpl}" \
    && grep -q 'Do not re-open the judgment' "${pr_tpl}"; then
    ok "threads that demand a stripped commit back are answered and resolved, not obeyed"
else
    fail "the push template lacks the contradicting-thread arm"
fi

echo "==> Checking the push template's verdict vocabulary excludes needs_work..."
if grep -q 'outcome "success"' "${pr_tpl}" \
    && grep -q 'outcome "failed"' "${pr_tpl}" \
    && grep -q 'verdicts like "needs_work" are invalid here' "${pr_tpl}"; then
    ok "the push step verdicts only success/failed — needs_work (the old KB-151 terminal) is invalid"
else
    fail "the push template's verdict vocabulary drifted from the two-outcome contract"
fi

# ── triage template: assemble the brief from the latest ruling ───────────────

triage_tpl="${TREE_ROOT}/agents/triage_pr_feedback.md.tftpl"

triage_text="$(tr 'A-Z' 'a-z' < "${triage_tpl}")"

echo "==> Checking the triage template teaches brief-assembly supersedence..."
if grep -qi 'SUPERSEDES' "${triage_tpl}" \
    && grep -q 'Do not carry a stripped commit' "${triage_tpl}" \
    && printf '%s' "$triage_text" | grep -q 'never assemble a brief that both' \
    && grep -q 'surfaced for a separate card' "${triage_tpl}" \
    && grep -q 'latest validated branch state' "${triage_tpl}"; then
    ok "the brief is assembled from the latest ruling — carry/strip conflicts can never co-exist"
else
    fail "the triage template lost part of the brief-assembly supersedence clause"
fi

# ── validate template: a stripped commit's absence is not a gap ──────────────

validate_tpl="${TREE_ROOT}/agents/validate_developer_response.md.tftpl"

echo "==> Checking the validator template carries the post-strip rule..."
if grep -q 'Post-strip supersedence (KB-151)' "${validate_tpl}" \
    && grep -q 'is NOT a missing requirement' "${validate_tpl}" \
    && grep -q 'Do NOT route needs_work for a stripped commit' "${validate_tpl}" \
    && grep -q 'Verify the' "${validate_tpl}" \
    && grep -q 'separate card' "${validate_tpl}"; then
    ok "the validator verifies the strip itself and never needs_works the stripped absence"
else
    fail "the validator template lost part of the post-strip supersedence doctrine"
fi

if [ "$FAILED" -eq 0 ]; then
    echo "test_brief_supersedence: all checks passed"
    exit 0
fi
echo "test_brief_supersedence: $FAILED check(s) failed" >&2
exit 1
