#!/usr/bin/env bash
# Regression test for KB-258: pair-programming feedback bodies must re-assert
# their worktree base at entry.
#
# Observed live failure (kb-246-1791952191, v0.5.53 fleet): a triage leg moved
# the shared /repo worktree onto 'dependabot/go_modules/minor-and-patch-91744db657'
# during its review work; the feedback body's checkout_branch step then failed
# in 2ms ("expected branch KB-246 but worktree is on dependabot/...") and the
# whole pair_loop body routed failed, parking the run awaiting_human after a
# ~40min GREEN develop leg.
#
# Validates the body-entry branch contract on the SHARED multi-body worktree:
#   - body A moves HEAD onto another branch and exits; body B's
#     checkout_branch re-asserts the expected branch (fetch + checkout),
#     carrying untracked scratch safely and preserving the branch's local
#     (incl. unpushed) commits, and the loop completes both bodies;
#   - the fast path (worktree already on the expected branch) is unchanged;
#   - the re-assert is best-effort about origin: an unreachable/missing remote
#     is reported (fetch_skipped=true) and the re-assert proceeds from the
#     local ref;
#   - a TRUE conflict (an untracked scratch file the expected branch tracks)
#     still fails, with the observed mismatch signature preserved and a typed
#     checkout_conflict=true reason carrying the unexpected ref — and the
#     worktree deterministically stays on the drifted ref;
#   - a worktree left in detached HEAD re-asserts too.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORKFLOW_DIR="${SCRIPT_DIR}/../workflows/pair_programming_loop"
MAIN_CHCL="${WORKFLOW_DIR}/main.chcl"
TEMPLATE="${WORKFLOW_DIR}/scripts/checkout_branch.sh.tftpl"
WORK_DIR="$(mktemp -d)"
trap 'rm -rf "${WORK_DIR}"' EXIT

fail() {
    echo "FAIL: $1" >&2
    exit 1
}

command -v git >/dev/null || fail "git required"
command -v criteria >/dev/null 2>&1 || fail "criteria required (compile check)"

echo "==> Compiling pair_programming_loop..."
(cd "${SCRIPT_DIR}/.." && criteria compile "${WORKFLOW_DIR}" >/dev/null 2>&1) \
    || fail "pair_programming_loop compiles on a stock engine"

echo "==> Checking the entry step still routes a failure to the terminal state..."
(cd "${WORKFLOW_DIR}" && grep -A9 'step "checkout_branch"' main.chcl | grep -q 'outcome "failure" { next = state.failed }') \
    || fail "checkout_branch failure must keep routing to state.failed (typed route)"

# KB-258 repro names: the drifted ref is the exact dependabot branch observed
# in the live failure.
TICKET="KB-258"
WS_FILE="/repo/workstreams/${TICKET}.md"
DRIFTED="dependabot/go_modules/minor-and-patch-91744db657"

# The template's first (only) template line is the shellquoted workstream-file
# assignment; render it by hand exactly as the step passes var.workstream_file.
render_script() { # out_dir
    local out="${1}/checkout_body.sh"
    {
        printf "criteria_value_1='%s'\n" "${WS_FILE}"
        tail -n +2 "${TEMPLATE}"
    } > "${out}"
    chmod +x "${out}"
}

branch_of() {
    git -C "$1" branch --show-current
}

# Fixture = origin (bare) + the repo primary tree, as the shell adapter sees
# the worktree: branch_manager attached the worktree to ${TICKET}, which
# carries the loop's durably-pushed checkpoint plus one local unpushed commit
# (mid-loop WIP), and origin also holds a drifted ${DRIFTED} branch body A
# moved HEAD onto.
make_fixture() { # dir
    local dir="$1"
    rm -rf "${dir}"
    git init --bare -q -b main "${dir}/origin.git"
    git init -q -b main "${dir}/repo"
    git -C "${dir}/repo" config user.email "test@example.com"
    git -C "${dir}/repo" config user.name "test"
    git -C "${dir}/repo" remote add origin "${dir}/origin.git"
    printf 'base\n' > "${dir}/repo/base.txt"
    git -C "${dir}/repo" add -A
    git -C "${dir}/repo" commit -qm "base"
    git -C "${dir}/repo" push -q origin main
    git -C "${dir}/repo" checkout -q -b "${TICKET}"
    printf 'checkpoint work\n' > "${dir}/repo/work.txt"
    git -C "${dir}/repo" add -A
    git -C "${dir}/repo" commit -qm "pushed checkpoint"
    git -C "${dir}/repo" push -q origin "${TICKET}"
    printf 'unpushed wip\n' > "${dir}/repo/wip.txt"
    git -C "${dir}/repo" add -A
    git -C "${dir}/repo" commit -qm "unpushed wip"
    git -C "${dir}/repo" checkout -q -b "${DRIFTED}" main
    git -C "${dir}/repo" commit -q --allow-empty -m "drifted body A work"
}

TICKET_SHA() {
    git -C "$1" rev-parse refs/heads/"${TICKET}"
}

echo "==> Scenario 1: acceptance repro — body A left the worktree on a dependabot branch"
FIX="${WORK_DIR}/s1"
make_fixture "${FIX}"
REPO="${FIX}/repo"
printf 'body A untracked scratch\n' > "${REPO}/scratch_body_a.txt"
render_script "${FIX}"
[ "$(branch_of "${REPO}")" = "${DRIFTED}" ] || fail "fixture broken: worktree is not on the drifted branch"

out=$(cd "${REPO}" && bash "${FIX}/checkout_body.sh" 2>&1) \
    || fail "body B checkout_branch failed instead of re-asserting (KB-258): ${out}"
[ "$(branch_of "${REPO}")" = "${TICKET}" ] \
    || fail "re-assert did not put the worktree back on ${TICKET}"
grep -q "reasserted_from=${DRIFTED}" <<<"${out}" \
    || fail "re-assert did not name the unexpected ref it moved from: ${out}"
grep -q "on_branch=${TICKET}" <<<"${out}" \
    || fail "output does not report the asserted branch: ${out}"
[ -f "${REPO}/scratch_body_a.txt" ] \
    || fail "untracked scratch was not carried across the re-assert"
[ "$(TICKET_SHA "${REPO}")" = "$(git -C "${REPO}" rev-parse HEAD)" ] \
    || fail "re-assert discarded the branch's local (unpushed) commits"
echo "==> OK"

echo "==> Scenario 2: fast path — body already on its branch"
FIX="${WORK_DIR}/s2"
make_fixture "${FIX}"
REPO="${FIX}/repo"
render_script "${FIX}"
git -C "${REPO}" checkout -q "${TICKET}"
out=$(cd "${REPO}" && bash "${FIX}/checkout_body.sh" 2>&1) \
    || fail "fast path failed on an unmoved worktree: ${out}"
grep -q "on_branch=${TICKET}" <<<"${out}" \
    || fail "fast path does not report the branch: ${out}"
if grep -q "reasserted_from" <<<"${out}"; then
    fail "fast path must not claim a re-assert"
fi
[ "$(branch_of "${REPO}")" = "${TICKET}" ] || fail "fast path moved HEAD unexpectedly"
echo "==> OK"

echo "==> Scenario 3: deterministic loop — body completes, next body's entry is a no-op"
# The KB-258 pair-loop shape: body B (feedback dev) re-asserts and completes;
# the next body (review feedback) re-enters through the same assertion and is
# a straight fast path.
FIX="${WORK_DIR}/s3"
make_fixture "${FIX}"
REPO="${FIX}/repo"
render_script "${FIX}"
out=$(cd "${REPO}" && bash "${FIX}/checkout_body.sh" 2>&1) \
    || fail "body B entry failed: ${out}"
out=$(cd "${REPO}" && bash "${FIX}/checkout_body.sh" 2>&1) \
    || fail "consecutive body entry failed on the re-asserted worktree: ${out}"
grep -q "on_branch=${TICKET}" <<<"${out}" \
    || fail "next body entry did not fast-path: ${out}"
echo "==> OK"

echo "==> Scenario 4: origin unreachable (best-effort fetch) — re-assert proceeds locally"
FIX="${WORK_DIR}/s4"
make_fixture "${FIX}"
REPO="${FIX}/repo"
render_script "${FIX}"
git -C "${REPO}" remote remove origin
printf 'scratch\n' > "${REPO}/scratch.txt"
out=$(cd "${REPO}" && bash "${FIX}/checkout_body.sh" 2>&1) \
    || fail "missing origin blocked the re-assert; the expected branch exists locally: ${out}"
grep -q "fetch_skipped=true" <<<"${out}" \
    || fail "missing origin was not reported: ${out}"
grep -q "reasserted_from=${DRIFTED}" <<<"${out}" \
    || fail "re-assert did not complete without origin: ${out}"
[ "$(branch_of "${REPO}")" = "${TICKET}" ] || fail "re-assert (no origin) did not restore ${TICKET}"
grep -q "unpushed wip" <(git -C "${REPO}" log -1 --format=%s) \
    || fail "re-assert (no origin) lost local history"
echo "==> OK"

echo "==> Scenario 5: detached HEAD — the branch contract still holds"
FIX="${WORK_DIR}/s5"
make_fixture "${FIX}"
REPO="${FIX}/repo"
render_script "${FIX}"
git -C "${REPO}" checkout -q --detach
out=$(cd "${REPO}" && bash "${FIX}/checkout_body.sh" 2>&1) \
    || fail "detached-HEAD entry failed instead of re-asserting: ${out}"
[ "$(branch_of "${REPO}")" = "${TICKET}" ] || fail "detached-HEAD re-assert did not restore ${TICKET}"
grep -q "reasserted_from=" <<<"${out}" \
    || fail "detached-HEAD re-assert did not report the moved ref: ${out}"
echo "==> OK"

echo "==> Scenario 6: TRUE conflict — typed failure, signature preserved, no half-switch"
# The expected branch tracks conflict.txt; the drifted branch does not; an
# untracked conflict.txt exists in the working tree, so git must refuse the
# switch rather than silently destroying scratch.
FIX="${WORK_DIR}/s6"
make_fixture "${FIX}"
REPO="${FIX}/repo"
render_script "${FIX}"
git -C "${REPO}" checkout -q "${TICKET}"
printf 'tracked on %s\n' "${TICKET}" > "${REPO}/conflict.txt"
git -C "${REPO}" add conflict.txt
git -C "${REPO}" commit -qm "tracks conflict.txt"
git -C "${REPO}" checkout -q "${DRIFTED}"
printf 'untracked scratch shadow\n' > "${REPO}/conflict.txt"
out=$(cd "${REPO}" && bash "${FIX}/checkout_body.sh" 2>&1) \
    && fail "expected a typed conflict failure, got exit 0: ${out}"
grep -q "expected branch '${TICKET}' but worktree is on '${DRIFTED}'" <<<"${out}" \
    || fail "typed failure lost the observed mismatch signature: ${out}"
grep -q "checkout_conflict=true" <<<"${out}" \
    || fail "typed failure does not carry the typed marker: ${out}"
[ "$(branch_of "${REPO}")" = "${DRIFTED}" ] \
    || fail "conflicted checkout left the worktree in a half-switched state"
[ -f "${REPO}/conflict.txt" ] \
    || fail "conflicted checkout destroyed the untracked scratch it refused to switch over"
echo "==> OK"

echo "PASS: KB-258 body-entry branch re-assert verified"