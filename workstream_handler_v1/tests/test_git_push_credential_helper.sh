#!/usr/bin/env bash
# Regression test for CRI-95: git push credential helper reads the injected
# environment token and never writes it to disk or logs.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
HELPER="${SCRIPT_DIR}/../scripts/_github_token_git_credentials.sh.tftpl"
WORK_DIR="$(mktemp -d)"
trap 'rm -rf "${WORK_DIR}"' EXIT

export HOME="${WORK_DIR}"
export GIT_CONFIG_GLOBAL="${WORK_DIR}/.gitconfig"

# Load the helper under test.
# shellcheck source=../scripts/_github_token_git_credentials.sh.tftpl
. "${HELPER}"

# Install the credential helper in the isolated git config before any test runs.
setup_gh_token_git_credentials

fail() {
    echo "FAIL: $1" >&2
    exit 1
}

run_fill() {
    printf 'protocol=https\nhost=github.com\n\n' | git credential fill
}

# GH_TOKEN is the primary source.
export GH_TOKEN="test-gh-token"
out=$(run_fill)
[[ "$out" == *"username=x-access-token"* ]] || fail "missing username for GH_TOKEN"
[[ "$out" == *"password=test-gh-token"* ]] || fail "missing password for GH_TOKEN"
echo "==> GH_TOKEN works"

# GITHUB_TOKEN fallback.
unset GH_TOKEN
export GITHUB_TOKEN="test-github-token"
out=$(run_fill)
[[ "$out" == *"password=test-github-token"* ]] || fail "GITHUB_TOKEN fallback failed"
echo "==> GITHUB_TOKEN fallback works"

# WORKFLOW_GITHUB_TOKEN fallback.
unset GITHUB_TOKEN
export WORKFLOW_GITHUB_TOKEN="test-workflow-token"
out=$(run_fill)
[[ "$out" == *"password=test-workflow-token"* ]] || fail "WORKFLOW_GITHUB_TOKEN fallback failed"
echo "==> WORKFLOW_GITHUB_TOKEN fallback works"

# No token present -> the helper reports an error and git credential fill fails.
unset WORKFLOW_GITHUB_TOKEN
if run_fill 2>/dev/null; then
    fail "expected failure when no token is present"
fi
echo "==> Missing-token error works"

# The helper should ignore store/erase operations instead of erroring.
export GH_TOKEN="test-gh-token"
if out=$(printf 'protocol=https\nhost=github.com\n\n' | git credential approve); then
    [ -z "$out" ] || fail "approve (store) operation produced unexpected output: $out"
    echo "==> approve (store) operation is a no-op"
else
    fail "approve (store) operation should exit 0"
fi

# Confirm the on-disk git config does not contain the literal token.
if [ -f "${GIT_CONFIG_GLOBAL}" ]; then
    if grep -q "test-gh-token\|test-github-token\|test-workflow-token" "${GIT_CONFIG_GLOBAL}"; then
        fail "token was written to git config"
    fi
    echo "==> Token not persisted in git config"
fi

echo "==> All checks passed."
