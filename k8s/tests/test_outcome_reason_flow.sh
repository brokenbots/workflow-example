#!/usr/bin/env bash
# Regression test for the KB-217 mechanical outcome-reason flow rule.
#
# Rule (validate-time, enforced by scripts/tree-outcome-flow.mjs): any outcome
# arm whose adapter reason (output.reason / steps.<S>.reason) transitively
# renders into another step's input prompt, a subworkflow call input, or a
# workflow output export must declare require_comment = true — otherwise an
# empty adapter finalize comment would render an empty findings prompt into
# the next step. The rule is mechanical: trees are auto-discovered under a
# root and there are NO per-tree allowlists to rot.
#
# Covered here:
#   1. repo-wide pass: the committed trees produce zero violations;
#   2. positive: a compliant reason-feeding arm (var flow + direct
#      steps.<S>.reason read) passes;
#   3. negative: the same arm WITHOUT require_comment fails validation with
#      the file, step, outcome arm, every reachable sink, and the fix hint;
#   4. over-fire guard: structured adapter output (output.stdout) flowing
#      through a variable does NOT trip the rule (require_comment gates the
#      comment, not payload fields).
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
    fail "committed trees violate the KB-217 rule — add require_comment to every flagged arm"
fi

# ── 2. Positive: compliant fixture passes ────────────────────────────────────
if run_validator "${FIXTURES}/compliant" >/dev/null 2>&1; then
    echo "PASS: compliant reason-feeding arm (require_comment present)"
else
    fail "compliant fixture should pass: $(node "${VALIDATOR}" --root "${FIXTURES}/compliant" 2>&1 | tail -3)"
fi

# ── 3. Negative: violating fixture fails with file/step/arm + fix hint ───────
out="$(run_validator "${FIXTURES}/violating" 2>&1)" && fail "violating fixture should be rejected"
echo "${out}" | grep -q 'fixtures/outcome-flow/violating/main.chcl:28' || fail "report must attribute findings to the file (repo-root-relative path shape): got: ${out}"
echo "${out}" | grep -q 'outcome "work_done" of step "agent_work"' || fail "report must name step and arm: got: ${out}"
echo "${out}" | grep -q 'renders its adapter reason into input of step "report"' || fail "report must name the step input sink: got: ${out}"
echo "${out}" | grep -q 'renders its adapter reason into input of subworkflow call "rollup"' || fail "report must name the subworkflow input sink: got: ${out}"
echo "${out}" | grep -q 'require_comment' || fail "report must carry the require_comment fix hint: got: ${out}"
echo "PASS: violating fixture rejected with file/step/arm and fix hint"

# ── 4. Over-fire guard: shell output.stdout plumbing is not the reason ───────
if run_validator "${FIXTURES}/shell_plumbing" >/dev/null 2>&1; then
    echo "PASS: structured output plumbing does not trip the rule"
else
    fail "shell_plumbing fixture should pass: $(node "${VALIDATOR}" --root "${FIXTURES}/shell_plumbing" 2>&1 | tail -3)"
fi

echo "ALL: outcome-reason flow rule regression test passed"