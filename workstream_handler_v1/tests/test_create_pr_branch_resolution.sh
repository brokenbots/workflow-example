#!/usr/bin/env bash
# Regression test for CRI-273: create_pr must operate on the branch that
# actually carries the work.
#
# Structural checks on the compiled workstream_handler_v1 workflow:
#   - the sync_head_branch step sits between run_pair_programming_loop and
#     create_pr on the success path, and fails the run on error;
#   - create_pr still derives the branch from the worktree itself;
#   - the developer prompt forbids self-cloning (the anchor for fix part 1);
#   - the resolve script targets stdout at the branch name only.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="${SCRIPT_DIR}/.."
COMPILE_OUT="$(mktemp)"
trap 'rm -f "${COMPILE_OUT}"' EXIT

command -v criteria >/dev/null 2>&1 || {
    echo "FAIL: criteria is not installed" >&2
    exit 1
}

echo "==> Compiling workstream_handler_v1..."
cd "${ROOT_DIR}"
criteria compile . >"${COMPILE_OUT}" 2>/dev/null

echo "==> Checking sync_head_branch exists on the shell adapter..."
jq -e '.steps[] | select(.name == "sync_head_branch") | .adapter == "shell.sh"' "${COMPILE_OUT}" >/dev/null

echo "==> Checking the develop loop hands off to sync_head_branch..."
jq -e '.steps[] | select(.name == "run_pair_programming_loop") | .outcomes[] | select(.name == "success" and .next == "sync_head_branch")' "${COMPILE_OUT}" >/dev/null

echo "==> Checking sync_head_branch hands off to create_pr and fails loudly..."
jq -e '.steps[] | select(.name == "sync_head_branch") | .outcomes[] | select(.name == "success" and .next == "create_pr")' "${COMPILE_OUT}" >/dev/null
jq -e '.steps[] | select(.name == "sync_head_branch") | .outcomes[] | select(.name == "failure" and .next == "failed")' "${COMPILE_OUT}" >/dev/null

echo "==> Checking create_pr still derives the branch from the worktree..."
grep -q 'git branch --show-current' "${ROOT_DIR}/scripts/create_pr.sh.tftpl" \
    || { echo "FAIL: create_pr.sh.tftpl no longer reads the worktree's branch" >&2; exit 1; }

echo "==> Checking the developer prompt forbids working outside the worktree..."
grep -q 'NEVER.*git clone' "${ROOT_DIR}/workflows/pair_programming_loop/agents/developer.md" \
    || { echo "FAIL: developer.md lost the no-self-clone anchor" >&2; exit 1; }
grep -qi 'work only inside the provided worktree' "${ROOT_DIR}/workflows/pair_programming_loop/agents/developer.md" \
    || { echo "FAIL: developer.md lost the worktree anchor" >&2; exit 1; }

echo "==> Checking the resolve script reports diagnostics on stderr only..."
grep -q 'stdout is exactly the resolved branch name' "${ROOT_DIR}/scripts/resolve_head_branch.sh.tftpl" \
    || { echo "FAIL: resolve_head_branch.sh.tftpl stdout contract missing" >&2; exit 1; }
grep -q '1>&2' "${ROOT_DIR}/scripts/resolve_head_branch.sh.tftpl" \
    || { echo "FAIL: resolve_head_branch.sh.tftpl diagnostics must go to stderr" >&2; exit 1; }

echo "==> All checks passed."