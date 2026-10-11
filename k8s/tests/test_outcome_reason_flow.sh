#!/usr/bin/env bash
# Regression test for the KB-217 mechanical outcome-reason flow rule (inverted
# by the dave 2026-10-11 require_comment removal ruling: cards KB-247/248/249).
#
# Rule (validate-time, enforced by scripts/tree-outcome-flow.mjs): any outcome
# arm whose adapter reason (output.reason / steps.<S>.reason) transitively
# renders into another step's input prompt, a subworkflow call input, or a
# workflow output export must declare a typed payload SCHEMA covering reason —
# schema = object({ reason = string }) or any schema attr naming reason.
# `require_comment` is DEPRECATED (warn-only in v0.5.x, removed at v0.6.0):
# the comment concept is adapter-family-specific and the shared evaluator no
# longer gates on it.
#
# Covered here:
#   1. repo-wide pass: the committed trees produce zero violations;
#   2. positive: a compliant reason-feeding arm (var flow + direct
#      steps.<S>.reason read, schema declared) passes;
#   3. negative: the same arm WITHOUT the schema fails validation with the
#      file, step, outcome arm, every reachable sink, and the fix hint;
#   4. over-fire guard: structured adapter output (output.stdout) flowing
#      through a variable does NOT trip the rule (payload fields other than
#      reason are not gated).
set -euo pipefail

SCRIPT_DIR="$(cd "${BASH_SOURCE[0]%/*}" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
FIXTURES="${SCRIPT_DIR}/fixtures/outcome-flow"
VALIDATOR="${REPO_ROOT}/scripts/tree-outcome-flow.mjs"

fail() { echo "FAIL: $1" >&2; exit 1; }

command -v node >/dev/null 2>&1 || fail "node is required to run the tree-outcome-flow validator"
[ -f "${VALIDATOR}" ] || fail "validator missing: ${VALIDATOR}"

run_validator() { # <root> -> (stdout: output, exit via globals)
    node "${VALIDATOR}" --root "$1" --quiet
}

# ── 1. Repo-wide pass: committed trees satisfy the rule ──────────────────────
if run_validator "${REPO_ROOT}" >/tmp/kb217-repowide.log 2>&1; then
    echo "PASS: repo-wide outcome-reason flow rule (0 violations)"
else
    echo "FAIL: repo-wide outcome-reason flow rule" >&2
    cat /tmp/kb217-repowide.log >&2
    fail "committed trees violate the KB-217 rule (schema form) — add a typed reason schema to every flagged arm"
fi

# ── 2. Positive: compliant fixture passes ────────────────────────────────────
if run_validator "${FIXTURES}/compliant" >/dev/null 2>&1; then
    echo "PASS: compliant reason-feeding arm (typed reason schema present)"
else
    fail "compliant fixture should pass: $(node "${VALIDATOR}" --root "${FIXTURES}/compliant" 2>&1 | tail -3)"
fi

# ── 3. Negative: violating fixture fails with file/step/arm + fix hint ───────
out="$(run_validator "${FIXTURES}/violating" 2>&1)" && fail "violating fixture should be rejected"
echo "${out}" | grep -q 'fixtures/outcome-flow/violating/main.chcl:28' || fail "report must attribute findings to the file (repo-root-relative path shape): got: ${out}"
echo "${out}" | grep -q 'outcome "work_done" of step "agent_work"' || fail "report must name step and arm: got: ${out}"
echo "${out}" | grep -q 'renders its adapter reason into input of step "report"' || fail "report must name the step input sink: got: ${out}"
echo "${out}" | grep -q 'renders its adapter reason into input of subworkflow call "rollup"' || fail "report must name the subworkflow input sink: got: ${out}"
echo "${out}" | grep -q 'schema = object({ reason = string })' || fail "report must carry the schema fix hint: got: ${out}"
echo "PASS: violating fixture rejected with file/step/arm and fix hint"
