#!/usr/bin/env bash
# Local checkpoint verification (pair-loop remaster): the verifier takes the
# loop's LOCAL durability baseline (entry_sha, captured at checkout_branch)
# as criteria_value_2 and performs a LOCAL-only compare — zero remote surface.
#
# Old base_branch render contract retired by the remaster: remote-truth
# verification moved to upstream_sync; the loop's verifier answers exactly
# one question locally — did HEAD move since loop entry?
#
# Validates:
#   - the verifier renders with branch + entry_sha and exits 0 in both shapes:
#     HEAD == entry_sha (benign resume, resumed_no_work=1) and
#     HEAD != entry_sha (committed work, checkpoint_local line);
#   - the script text carries NO remote surface (ls-remote / fetch / push).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORKFLOW_DIR="${SCRIPT_DIR}/../workflows/pair_programming_loop"
TEMPLATE="${WORKFLOW_DIR}/scripts/verify_checkpoint_pushed.sh.tftpl"
WORK_DIR="$(mktemp -d)"
trap 'rm -rf "${WORK_DIR}"' EXIT
fail() { echo "FAIL: $1" >&2; exit 1; }

# The placeholder is rendered with sed — the same line-anchored shellquote
# substitution test_upstream_sync.sh uses for the push script (python3 is not
# guaranteed in every environment that runs this suite).
BRANCH="entrysha-sanity"
sed -e "s@^criteria_value_1=.*@criteria_value_1='${BRANCH}'@" \
    "$TEMPLATE" > "${WORK_DIR}/verify.sh"

# No remote surface in the verifier script — local-only contract (ruling 1).
if grep -qE 'ls-remote|git fetch|git push|origin/' "${WORK_DIR}/verify.sh"; then
    fail "verifier carries remote surface (remaster ruling 1: local-only)"
fi

# Fixture: a branch with a couple of local commits; NO remote at all.
git init -q "${WORK_DIR}/repo"
cd "${WORK_DIR}/repo"
git config user.email t@t
git config user.name t
git commit -q --allow-empty -m base
git checkout -q -b "${BRANCH}"
git commit -q --allow-empty -m wip
ENTRY_SHA="$(git rev-parse HEAD)"

render_with_entry() { # $1 = entry sha -> runs the verifier
    sed "s@^criteria_value_2=.*@criteria_value_2='$1'@" "${WORK_DIR}/verify.sh" > "${WORK_DIR}/verify_run.sh"
    chmod +x "${WORK_DIR}/verify_run.sh"
}

# Scenario 1: HEAD == entry_sha -> benign resume.
out="$(render_with_entry "${ENTRY_SHA}" && bash "${WORK_DIR}/verify_run.sh")" \
    || fail "benign-resume arm exited non-zero (stdout: ${out})"
printf '%s\n' "${out}" | grep -q '^resumed_no_work=1$' \
    || fail "benign-resume arm did not report resumed_no_work=1 (stdout: ${out})"

# Scenario 2: HEAD moved (committed work) -> checkpoint_local line, exit 0.
git commit -q --allow-empty -m "more work"
out="$(render_with_entry "${ENTRY_SHA}" && bash "${WORK_DIR}/verify_run.sh")" \
    || fail "committed-work arm exited non-zero (stdout: ${out})"
printf '%s\n' "${out}" | grep -q '^checkpoint_local=head:' \
    || fail "committed-work arm did not report checkpoint_local (stdout: ${out})"
printf '%s\n' "${out}" | grep -q "^checkpoint_local=head:$(git rev-parse HEAD) entry:${ENTRY_SHA}$" \
    || fail "checkpoint_local line lost sha fidelity (stdout: ${out})"

# Scenario 3: empty entry_sha -> loud failure (the guard must hold).
out="$(render_with_entry "" && bash "${WORK_DIR}/verify_run.sh" 2>&1)" \
    && fail "empty entry_sha must fail loudly (stdout: ${out})"
printf '%s\n' "${out}" | grep -q "entry_sha is empty" \
    || fail "empty entry_sha failure lost its typed reason (stdout: ${out})"

echo "PASS: entry-sha checkpoint verification verified (benign/committed/empty arms)"
