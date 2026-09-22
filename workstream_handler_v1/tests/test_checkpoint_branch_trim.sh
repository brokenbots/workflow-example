#!/usr/bin/env bash
# Regression test for CRI-295: the pair_programming_loop branch value carried a
# trailing newline. get_branch_name runs `basename <workstream_file> .md`, whose
# stdout ends with a newline; read_workstream wrote it into
# data.internal.branch untrimmed, and push_checkpoint rendered
# `git ls-remote origin "refs/heads/CRI-287\n"` — no match — so a timed-out
# develop turn failed push_checkpoint on a branch that was verifiably pushed
# and parked at In Review (run 0062ee06, castle event stream #880).
#
# Validates:
#   - the write site trims steps.get_branch_name.stdout before storing the
#     branch, so every consumer of data.internal.branch gets a clean value;
#   - the sibling _log_file write keeps its existing trimspace;
#   - verify_checkpoint_pushed.sh.tftpl verifies a branch value carrying the
#     observed trailing-newline (defense in depth at the consumer);
#   - the normal clean-branch path still passes verification;
#   - the loud failure signatures survive: a branch missing on the remote and
#     a diverged local/remote head still fail the step.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORKFLOW_DIR="${SCRIPT_DIR}/../workflows/pair_programming_loop"
MAIN_CHCL="${WORKFLOW_DIR}/main.chcl"
TEMPLATE="${WORKFLOW_DIR}/scripts/verify_checkpoint_pushed.sh.tftpl"
COMPILE_OUT="$(mktemp)"
WORK_DIR="$(mktemp -d)"
trap 'rm -f "${COMPILE_OUT}"; rm -rf "${WORK_DIR}"' EXIT

fail() {
    echo "FAIL: $1" >&2
    exit 1
}

echo "==> Compiling pair_programming_loop..."
cd "${SCRIPT_DIR}/.."
criteria compile "${WORKFLOW_DIR}" >"${COMPILE_OUT}" 2>/dev/null

# The compiled JSON carries no write expressions, so the write-site assertions
# are source-level (same approach test_develop_turn_timeout.sh uses for
# compiler-dropped default edges).
echo "==> Checking read_workstream trims the branch name it writes..."
branch_write="$(grep -A7 'target = data\.internal\.branch\.value' "${MAIN_CHCL}" || true)"
printf '%s' "${branch_write}" | grep -q 'value[[:space:]]*= trimspace(steps\.get_branch_name\.stdout)' \
    || fail "data.internal.branch must be written from trimspace(steps.get_branch_name.stdout)"

echo "==> Checking no untrimmed get_branch_name.stdout write remains..."
if grep -Eq 'value[[:space:]]*=[[:space:]]*steps\.get_branch_name\.stdout' "${MAIN_CHCL}"; then
    fail "steps.get_branch_name.stdout is still written untrimmed"
fi

echo "==> Checking the sibling log-file write keeps its trimspace..."
printf '%s' "$(grep -A2 'target = data\.internal\._log_file\.value' "${MAIN_CHCL}")" \
    | grep -q 'trimspace(steps\.get_branch_name\.stdout)' \
    || fail "data.internal._log_file lost its trimspace guard"

# ── Behavioral: drive the rendered verify_checkpoint_pushed.sh.tftpl ─────────
#
# The template's first line is the shellquoted criteria_value_1 assignment. The
# harness swaps that line for a hand-rendered one, so a poisoned value can be
# written exactly as shellquote would emit it — a single-quoted string with the
# literal newline inside.

ORIGIN="${WORK_DIR}/origin.git"
REPO="${WORK_DIR}/repo"
TICKET="CRI-295"

# Recreate origin + clone from scratch; the script under test runs inside the
# clone (cwd of adapter.shell.sh) with origin pointing at the bare repo.
reset_fixture() {
    rm -rf "${ORIGIN}" "${REPO}"
    git init --bare -q -b main "${ORIGIN}"
    git init -q -b main "${REPO}"
    git -C "${REPO}" config user.email "test@example.com"
    git -C "${REPO}" config user.name "test"
    git -C "${REPO}" remote add origin "${ORIGIN}"
    printf 'base\n' > "${REPO}/base.txt"
    git -C "${REPO}" add -A
    git -C "${REPO}" commit -qm "base"
    git -C "${REPO}" push -q -u origin main
    git -C "${REPO}" checkout -q -b "${TICKET}"
    printf 'checkpoint work\n' > "${REPO}/work.txt"
    git -C "${REPO}" add -A
    git -C "${REPO}" commit -qm "checkpoint work"
    git -C "${REPO}" push -q origin "${TICKET}"
}

render_with_value() { # rendered_script assignment_lines
    local out="$1" value="$2"
    {
        printf '%b\n' "${value}"
        tail -n +2 "${TEMPLATE}"
    } > "${out}"
    chmod +x "${out}"
}

local_head() {
    git -C "${REPO}" rev-parse refs/heads/${TICKET}
}

reset_fixture

echo "==> Scenario: CRI-295 regression — branch value carries a trailing newline"
SCRIPT="${WORK_DIR}/verify_newline.sh"
# Exact rendering of a value that is "CRI-295\n": the quote closes on the next
# line, which is how the live failure embedded a line break into the error.
render_with_value "${SCRIPT}" "criteria_value_1='${TICKET}\n'"
out=$(cd "${REPO}" && bash "${SCRIPT}") \
    || fail "newline-carrying branch value failed verification (CRI-295 false negative): ${out}"
[[ "${out}" == *"checkpoint_pushed=$(local_head)"* ]] \
    || fail "newline-carrying branch value did not report checkpoint_pushed: ${out}"
echo "==> OK"

echo "==> Scenario: CRI-295 regression — branch value carries leading whitespace"
SCRIPT="${WORK_DIR}/verify_lead_ws.sh"
render_with_value "${SCRIPT}" "criteria_value_1='  ${TICKET}'"
out=$(cd "${REPO}" && bash "${SCRIPT}") \
    || fail "whitespace-padded branch value failed verification: ${out}"
[[ "${out}" == *"checkpoint_pushed=$(local_head)"* ]] \
    || fail "whitespace-padded branch value did not report checkpoint_pushed: ${out}"
echo "==> OK"

echo "==> Scenario: normal path — clean branch value still verifies"
SCRIPT="${WORK_DIR}/verify_clean.sh"
render_with_value "${SCRIPT}" "criteria_value_1='${TICKET}'"
out=$(cd "${REPO}" && bash "${SCRIPT}") \
    || fail "clean branch value failed verification: ${out}"
[[ "${out}" == *"checkpoint_pushed=$(local_head)"* ]] \
    || fail "clean branch value did not report checkpoint_pushed: ${out}"
echo "==> OK"

echo "==> Scenario: loud failure preserved — branch absent from the remote"
git -C "${REPO}" push -q origin --delete "${TICKET}"
if out=$(cd "${REPO}" && bash "${SCRIPT}" 2>&1); then
    fail "expected failure for a branch missing on the remote, got: ${out}"
fi
[[ "${out}" == *"checkpoint push verification: remote branch ${TICKET} does not exist"* ]] \
    || fail "expected the does-not-exist signature, got: ${out}"
# A match spanning the whole sentence is only possible when the branch name is
# clean: a newline-carrying value would break the line between the branch and
# "does not exist" (the signature observed in the live failure).
git -C "${REPO}" push -q origin "${TICKET}"
echo "==> OK"

echo "==> Scenario: loud failure preserved — diverged local and remote heads"
# Amend by changing the tree: an empty amend inside the same second as the
# prior commit can reproduce an identical sha (committer timestamp has
# 1s granularity), which would not diverge at all.
printf 'post-checkpoint note\n' > "${REPO}/note.txt"
git -C "${REPO}" add -A
git -C "${REPO}" commit -q --amend --no-edit
if out=$(cd "${REPO}" && bash "${SCRIPT}" 2>&1); then
    fail "expected failure for a diverged local/remote head, got: ${out}"
fi
[[ "${out}" == *"checkpoint push verification: local $(local_head) != remote"* ]] \
    || fail "expected the local != remote signature, got: ${out}"
echo "==> OK"

echo "PASS: checkpoint branch trim verified"