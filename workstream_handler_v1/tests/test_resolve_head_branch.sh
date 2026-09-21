#!/usr/bin/env bash
# Regression test for CRI-273: create_pr must operate on the branch that
# actually carries the work, even when the developer worked in a second
# clone it created itself (the worktree then sits on the empty ticket
# branch and "gh pr create" fails with "No commits between main and <ticket>").
#
# Renders resolve_head_branch.sh.tftpl and drives it against a real git
# fixture (a bare origin, the operator's primary tree, and a per-run worktree
# on the ticket branch, with an "agent clone" simulating the developer's
# self-made checkout):
#   - happy path: worktree branch has commits ahead of base -> unchanged
#   - recovery: the agent pushed a descriptive branch naming the ticket
#   - recovery: agent branch carries no ticket id but is ahead of base
#   - same-branch push: agent pushed to the ticket branch from its clone
#   - no candidates: worktree branch stays checked out
#   - hijack guard: a branch naming another ticket is never picked
#   - preference: a name-matching branch wins over a newer generic one
#   - recency among ticket-name matches: most recently pushed wins even
#     when it sorts last alphabetically
#   - recency among generic candidates: most recently pushed wins even
#     when it sorts last alphabetically
#   - empty name-match: a name-matching branch with no commits is skipped
#   - loud failure: the agent branch held by another worktree cannot be
#     checked out -> non-zero exit instead of a silent wrong-branch PR

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TEMPLATE="${SCRIPT_DIR}/../scripts/resolve_head_branch.sh.tftpl"
WORK_DIR="$(mktemp -d)"
trap 'rm -rf "${WORK_DIR}"' EXIT

ORIGIN="${WORK_DIR}/origin.git"
REPO="${WORK_DIR}/repo"
WT="${WORK_DIR}/worktree"
AGENT="${WORK_DIR}/agent-clone"
TICKET="CRI-9"
WORKSTREAM_FILE="${WORK_DIR}/${TICKET}.md"
SCRIPT="${WORK_DIR}/resolve.sh"

fail() {
    echo "FAIL: $1" >&2
    exit 1
}

# Render the template with fixed test values. shellquote renders string
# literals safely; for the harness simple quoted values are enough. The
# credential-helper snippet is replaced by a no-op: the fixture's origin is
# a local bare repo and needs no token.
sed -e "s|^criteria_value_1=.*|criteria_value_1='main'|" \
    -e "s|^criteria_value_2=.*|criteria_value_2='${WORKSTREAM_FILE}'|" \
    -e "s|^{{ *\.helper *}}\$|. \"${SCRIPT_DIR}/../scripts/_github_token_git_credentials.sh.tftpl\"|" \
    "${TEMPLATE}" > "${SCRIPT}"
chmod +x "${SCRIPT}"

git_user() {
    git -C "$1" config user.email "test@example.com"
    git -C "$1" config user.name "test"
}

# Commit at a deterministic time so "most recently pushed" ordering is stable.
commit_file() { # repo_dir branch file content timestamp
    local dir="$1" branch="$2" file="$3" content="$4" ts="$5"
    git -C "$dir" checkout -q "$branch" 2>/dev/null || git -C "$dir" checkout -q -b "$branch" origin/main
    printf '%s\n' "$content" > "${dir}/${file}"
    git -C "$dir" add -A
    GIT_AUTHOR_DATE="${ts}" GIT_COMMITTER_DATE="${ts}" git -C "$dir" commit -qm "add ${file}"
}

git init --bare -q -b main "${ORIGIN}"
git init -q -b main "${REPO}"
git_user "${REPO}"
printf 'base\n' > "${REPO}/base.txt"
git -C "${REPO}" add -A
git -C "${REPO}" commit -qm "base"
git -C "${REPO}" remote add origin "${ORIGIN}"
git -C "${REPO}" push -q -u origin main

# Prune origin back to main, drop the run worktree, and refresh the agent
# clone, so every scenario starts from the same state: worktree on the
# empty ticket branch, origin without agent branches.
reset_fixture() {
    git -C "${REPO}" worktree remove --force "${WT}" >/dev/null 2>&1 || true
    git -C "${REPO}" worktree remove --force "${WORK_DIR}/other-wt" >/dev/null 2>&1 || true
    git -C "${REPO}" worktree prune
    for branch in $(git -C "${REPO}" ls-remote --heads origin | awk '{print $2}' | sed 's|refs/heads/||' | grep -v '^main$'); do
        git -C "${REPO}" push -q origin --delete "${branch}"
    done
    git -C "${REPO}" branch -D "${TICKET}" >/dev/null 2>&1 || true
    git -C "${REPO}" worktree add -q -b "${TICKET}" "${WT}" origin/main
    rm -rf "${AGENT}"
    git clone -q "${ORIGIN}" "${AGENT}"
    git_user "${AGENT}"
}

# Commit in a clone and push the branch to origin, as the self-made agent
# checkout would. Timestamps are explicit so "most recently pushed" ordering
# is deterministic.
commit_push() { # clone_dir branch file content timestamp
    local dir="$1" branch="$2" file="$3" content="$4" ts="$5"
    git -C "$dir" checkout -q -b "$branch" origin/main
    printf '%s\n' "$content" > "${dir}/${file}"
    git -C "$dir" add -A
    GIT_AUTHOR_DATE="${ts}" GIT_COMMITTER_DATE="${ts}" git -C "$dir" commit -qm "add ${file}"
    git -C "$dir" push -q origin "$branch"
}

# Run the script from inside the worktree, as adapter.shell.sh does.
run_resolve() {
    git -C "${WT}" fetch -q origin 2>/dev/null || true
    (cd "${WT}" && GIT_CONFIG_GLOBAL="${WORK_DIR}/gitconfig" bash "${SCRIPT}")
}

wt_branch() {
    git -C "${WT}" branch --show-current
}

wt_head() {
    git -C "${WT}" rev-parse HEAD
}

reset_fixture
touch "${WORKSTREAM_FILE}"

echo "==> Scenario: happy path — worktree branch has commits ahead of base"
printf 'work\n' > "${WT}/wip.txt"
git -C "${WT}" add -A
GIT_AUTHOR_DATE="2026-01-01T00:00:00Z" GIT_COMMITTER_DATE="2026-01-01T00:00:00Z" \
    git -C "${WT}" commit -qm "add wip.txt"
out=$(run_resolve)
[ "$(printf '%s' "$out")" = "${TICKET}" ] || fail "happy path resolved '${out}', expected ${TICKET}"
[ "$(wt_branch)" = "${TICKET}" ] || fail "happy path changed the checked-out branch to $(wt_branch)"
echo "==> OK"

echo "==> Scenario: recovery — agent pushed a descriptive branch naming the ticket"
reset_fixture
commit_push "${AGENT}" "cri-9-provider-retry" "fix.txt" "fix" "2026-01-01T01:00:00Z"
out=$(run_resolve)
[ "$(printf '%s' "$out")" = "cri-9-provider-retry" ] || fail "expected the agent branch, got '${out}'"
[ "$(wt_branch)" = "cri-9-provider-retry" ] || fail "worktree not moved to the agent branch (on $(wt_branch))"
echo "==> OK"

echo "==> Scenario: same-branch push — agent pushed to the ticket branch from its clone"
reset_fixture
commit_push "${AGENT}" "${TICKET}" "same-branch.txt" "same branch" "2026-01-01T02:00:00Z"
out=$(run_resolve)
[ "$(printf '%s' "$out")" = "${TICKET}" ] || fail "expected ${TICKET}, got '${out}'"
[ "$(wt_head)" = "$(git -C "${REPO}" rev-parse "origin/${TICKET}")" ] || \
    fail "worktree not fast-forwarded to origin/${TICKET}"
echo "==> OK"

echo "==> Scenario: no candidates — worktree branch stays checked out"
reset_fixture
out=$(run_resolve)
[ "$(printf '%s' "$out")" = "${TICKET}" ] || fail "expected ${TICKET}, got '${out}'"
[ "$(wt_branch)" = "${TICKET}" ] || fail "branch changed to $(wt_branch)"
echo "==> OK"

echo "==> Scenario: hijack guard — another ticket's branch is never picked"
reset_fixture
commit_push "${AGENT}" "cri-8-other-fix" "other.txt" "other ticket" "2026-01-01T03:00:00Z"
out=$(run_resolve)
[ "$(printf '%s' "$out")" = "${TICKET}" ] || fail "expected ${TICKET}, got '${out}'"
[ "$(wt_branch)" = "${TICKET}" ] || fail "hijacked another ticket's branch (on $(wt_branch))"
echo "==> OK"

echo "==> Scenario: hijack guard — a branch whose ticket id is a prefix of ${TICKET} is never picked"
reset_fixture
# CRI-9 shares a prefix with CRI-91/92/95/97/98; their cri-9X-* branches must
# never be adopted, even when they are the only ones ahead of base. CRI-9 is
# also a prefix of CRI-90/99 — only whole-id equality should pass.
commit_push "${AGENT}" "cri-91-other" "p91.txt" "other prefix ticket" "2026-01-01T03:00:00Z"
commit_push "${AGENT}" "cri-95-other" "p95.txt" "other prefix ticket" "2026-01-01T03:01:00Z"
commit_push "${AGENT}" "cri-98-other" "p98.txt" "other prefix ticket" "2026-01-01T03:02:00Z"
commit_push "${AGENT}" "cri-99-other" "p99.txt" "other prefix ticket" "2026-01-01T03:03:00Z"
out=$(run_resolve)
[ "$(printf '%s' "$out")" = "${TICKET}" ] || fail "expected ${TICKET} (no prefix-collision hijack), got '${out}'"
[ "$(wt_branch)" = "${TICKET}" ] || fail "worktree moved to a prefix-collision branch (on $(wt_branch))"
echo "==> OK"

echo "==> Scenario: preference — name-matching branch wins over a newer generic one"
reset_fixture
commit_push "${AGENT}" "cri-9-older-fix" "older.txt" "older" "2026-01-01T04:00:00Z"
commit_push "${AGENT}" "zz-generic-newer" "newer.txt" "newer" "2026-01-01T05:00:00Z"
out=$(run_resolve)
[ "$(printf '%s' "$out")" = "cri-9-older-fix" ] || fail "expected the name-matching branch, got '${out}'"
[ "$(wt_branch)" = "cri-9-older-fix" ] || fail "worktree not on the name-matching branch (on $(wt_branch))"
echo "==> OK"

echo "==> Scenario: recency among ticket-name matches — most recently pushed wins"
reset_fixture
# cri-9-alpha is pushed first (older) and sorts first alphabetically; the
# selector must still pick the most recently pushed cri-9-zeta.
commit_push "${AGENT}" "cri-9-alpha" "alpha.txt" "alpha" "2026-01-01T04:00:00Z"
commit_push "${AGENT}" "cri-9-zeta" "zeta.txt" "zeta" "2026-01-01T05:00:00Z"
out=$(run_resolve)
[ "$(printf '%s' "$out")" = "cri-9-zeta" ] || fail "expected the most recently pushed name match, got '${out}'"
[ "$(wt_branch)" = "cri-9-zeta" ] || fail "worktree not on the most recently pushed name match (on $(wt_branch))"
[ "$(wt_head)" = "$(git -C "${REPO}" rev-parse "origin/cri-9-zeta")" ] || \
    fail "worktree HEAD is not origin/cri-9-zeta"
echo "==> OK"

echo "==> Scenario: recency among generic candidates — most recently pushed wins"
reset_fixture
commit_push "${AGENT}" "aa-generic-older" "old.txt" "old" "2026-01-01T04:00:00Z"
commit_push "${AGENT}" "zz-generic2" "new.txt" "new" "2026-01-01T05:00:00Z"
out=$(run_resolve)
[ "$(printf '%s' "$out")" = "zz-generic2" ] || fail "expected the most recently pushed generic candidate, got '${out}'"
[ "$(wt_branch)" = "zz-generic2" ] || fail "worktree not on the most recently pushed generic candidate (on $(wt_branch))"
[ "$(wt_head)" = "$(git -C "${REPO}" rev-parse "origin/zz-generic2")" ] || \
    fail "worktree HEAD is not origin/zz-generic2"
echo "==> OK"

echo "==> Scenario: empty name-match is skipped — a branch with no commits cannot back a PR"
reset_fixture
git -C "${AGENT}" push -q origin origin/main:refs/heads/cri-9-empty
commit_push "${AGENT}" "zz-generic-newer" "newer.txt" "newer" "2026-01-01T05:00:00Z"
out=$(run_resolve)
[ "$(printf '%s' "$out")" = "zz-generic-newer" ] || fail "expected the generic branch, got '${out}'"
echo "==> OK"

echo "==> Scenario: loud failure when the agent branch cannot be checked out"
reset_fixture
commit_push "${AGENT}" "cri-9-provider-retry" "fix.txt" "fix" "2026-01-01T06:00:00Z"
# Hold the agent branch in a second worktree so checkout in the run worktree
# must fail; the script must exit non-zero instead of silently staying put.
git -C "${REPO}" worktree add -q "${WORK_DIR}/other-wt" "cri-9-provider-retry"
if out=$(run_resolve 2>/dev/null); then
    fail "expected failure when the agent branch is checked out in another worktree, got '${out}'"
fi
echo "==> OK"

echo "PASS: resolve_head_branch verified"