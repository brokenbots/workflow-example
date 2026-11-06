#!/usr/bin/env bash
set -euo pipefail

# Pair-loop LOCAL-ONLY isolation guard (pair-loop remaster Wave 1, Task 6;
# settled ruling 1): the pair-programming loop touches ONLY the worktree and
# local git state. Anything network (fetch/push/ls-remote/gh/credential
# helpers/secrets) must live in the upstream_sync subworkflow, never in the
# loop trees. Remote-tracking refs already present are local data and stay
# legal (ruling 1, positive arm).
#
# Scope: ALL THREE live loop trees (base + round2 + round3 — full copies with
# their own scripts/adapters/vars, live until Stage 3 folds the rounds).
#
# Negative arms FIRST (the guard must be able to fail): probe leaks are
# injected into a scratch copy, the scanner must flag them, then the scratch
# copy is discarded. The working trees are never modified.

TREE_ROOT="$(cd "$(dirname "$0")/.." && pwd)"

FAILED=0

fail() {
    echo "FAIL: $1" >&2
    FAILED=$((FAILED + 1))
}

ok() {
    echo "ok: $1"
}

LOOP_TREES="pair_programming_loop pair_programming_loop_round2 pair_programming_loop_round3"

scan_file() { # $1=file  -> prints leaking lines (empty = clean)
    # Comment-only lines are contract commentary (ruling references); the
    # guard exists for EXECUTABLE surfaces, so strip them before scanning.
    grep -vE '^[[:space:]]*(//|#)' "$1" 2>/dev/null \
        | grep -nE 'git push|git fetch|git pull|ls-remote|(^|[^[:alnum:]_])gh |credential|GITHUB_TOKEN|GH_TOKEN|workflow_github_token' \
        || true
}

# ── negative arms: the guard must fail on injected leaks ─────────────────────
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
PROBE="$TMP/pair_programming_loop"
mkdir -p "$PROBE/scripts" "$PROBE/agents"

printf '# a step\ngit push origin HEAD\n' > "$PROBE/scripts/probe_push.sh.tftpl"
if [ -n "$(scan_file "$PROBE/scripts/probe_push.sh.tftpl")" ]; then
    ok "negative arm: git push line is flagged"
else
    fail "negative arm: scanner failed to flag a git push line"
fi

printf 'adapter "copilot" "developer" {\n    secrets {\n        GITHUB_TOKEN = var.workflow_github_token\n    }\n}\n' > "$PROBE/probe_adapters.chcl"
if [ -n "$(scan_file "$PROBE/probe_adapters.chcl")" ]; then
    ok "negative arm: token secrets block is flagged"
else
    fail "negative arm: scanner failed to flag a token secrets block"
fi

printf '# fetch checkpoint state\ngit fetch --quiet origin\n' > "$PROBE/scripts/probe_fetch.sh.tftpl"
if [ -n "$(scan_file "$PROBE/scripts/probe_fetch.sh.tftpl")" ]; then
    ok "negative arm: git fetch line is flagged"
else
    fail "negative arm: scanner failed to flag a git fetch line"
fi

# ── positive arms: the live trees are clean ──────────────────────────────────
# Comment lines that merely DESCRIBE the contract (ruling references) are
# filtered; executable surfaces must carry zero network verbs, zero credential
# surfaces, zero token vars.
LEAKS=0
for tree in $LOOP_TREES; do
    base="$TREE_ROOT/workflows/$tree"
    for f in "$base"/scripts/* "$base"/main.chcl "$base"/adapters.chcl "$base"/variables.chcl; do
        [ -f "$f" ] || continue
        hits="$(scan_file "$f")"
        if [ -n "$hits" ]; then
            LEAKS=$((LEAKS + 1))
            fail "upstream surface in $tree (${f#$TREE_ROOT/}):"
            printf '%s\n' "$hits" >&2
        fi
    done
done
if [ "$LEAKS" -eq 0 ]; then
    ok "all three loop trees carry zero upstream surface (scripts/adapters/vars/main)"
fi

# agents/*.md: the developer charter must not teach push or credential
# handling to the agent.
for tree in $LOOP_TREES; do
    f="$TREE_ROOT/workflows/$tree/agents/developer.md"
    if grep -qE 'credential helper|setup_gh_token_git_credentials|must be green before you call' "$f" 2>/dev/null; then
        fail "$tree agents/developer.md still teaches push/credentials/full-gate mandates"
    else
        ok "$tree agents/developer.md carries no push/credential/full-gate training"
    fi
done

# adapter pins unchanged (remaster must not drift pins silently):
for tree in $LOOP_TREES; do
    a="$TREE_ROOT/workflows/$tree/adapters.chcl"
    if grep -q 'version = "0.5.17"' "$a" && grep -q 'version = "0.5.5"' "$a"; then
        ok "$tree adapter pins intact (shell 0.5.5, copilot 0.5.17)"
    else
        fail "$tree adapter pins drifted"
    fi
done

if [ "$FAILED" -ne 0 ]; then
    echo "test_pair_loop_local_isolation: $FAILED failure(s)" >&2
    exit 1
fi
echo "test_pair_loop_local_isolation: all pins hold"
