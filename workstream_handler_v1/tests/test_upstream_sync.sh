#!/usr/bin/env bash
set -euo pipefail

# upstream_sync regression: the mechanical push script + tree wiring
# (pair-loop remaster Wave 1, Task 6).
#
# Pinned:
#   1. upstream_sync validates standalone (compile-time resolvable inputs);
#   2. push creates the remote branch and prints the protocol lines;
#   3. idempotent re-push exits 0 with the same sha;
#   4. non-fast-forward shape exits 1 with non_fast_forward=1;
#   5. the mechanical path NEVER carries a force flag (ruling 9);
#   6. worktree .git/config stays credential-free (run-time helper only).

TREE_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CRITERIA="${CRITERIA_BIN:-criteria}"

FAILED=0

fail() {
    echo "FAIL: $1" >&2
    FAILED=$((FAILED + 1))
}

ok() {
    echo "ok: $1"
}

require_equal() {
    if [ "$1" = "$2" ]; then
        ok "$3"
    else
        fail "$3: got '$1', want '$2'"
    fi
}

# ── 1. validate standalone ────────────────────────────────────────────────────
# Compile-time resolvable inputs: branch is required-only when used, tree
# vars carry defaults; validate proves the tree resolves and the nested
# upstream_sync source (../upstream_sync) is engine-legal.
if "$CRITERIA" validate "$TREE_ROOT/workflows/upstream_sync" >/dev/null 2>&1; then
    ok "upstream_sync validates standalone"
else
    fail "criteria validate upstream_sync"
fi

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

CLONE="$TMP/clone"
ORIGIN="$TMP/origin.git"
WORK="$TMP/worktree"
git init --bare -q "$ORIGIN"
git init -q "$WORK"
git -C "$WORK" config user.email test@example.com
git -C "$WORK" config user.name test
git -C "$WORK" remote add origin "$ORIGIN"
printf 'seed\n' > "$WORK/seed.txt"
git -C "$WORK" add seed.txt
git -C "$WORK" commit -qm seed
git -C "$WORK" checkout -qb ticket-branch

# Seed the bare origin BEFORE the divergence clone, and point the bare
# repo's HEAD at the seeded branch: a bare init's HEAD (refs/heads/master)
# does not exist, so a clone comes out unborn and the fixture's divergent
# push fails silently.
git -C "$WORK" push -q origin ticket-branch 2>/dev/null
git -C "$ORIGIN" symbolic-ref HEAD refs/heads/ticket-branch

render() { # -> $TMP/push.sh
    sed -e "s@^criteria_value_1=.*@criteria_value_1='ticket-branch'@" \
        "$TREE_ROOT/workflows/upstream_sync/scripts/push_branch.sh.tftpl" > "$TMP/push.sh"
}

# ── 2. push creates the remote branch + protocol lines ───────────────────────
render
if out="$(cd "$WORK" && sh "$TMP/push.sh" 2>&1)"; then
    ok "mechanical push succeeds"
    printf '%s\n' "$out" | grep -q '^upstream_ref=refs/heads/ticket-branch$' \
        && ok "protocol line upstream_ref present" \
        || fail "protocol line upstream_ref missing: $out"
    sha="$(git -C "$WORK" rev-parse HEAD)"
    printf '%s\n' "$out" | grep -q "^remote_head=${sha}$" \
        && ok "protocol line remote_head matches local HEAD" \
        || fail "protocol line remote_head mismatch: $out"
else
    fail "mechanical push failed: $out"
fi

# ── 3. idempotent re-push ─────────────────────────────────────────────────────
if out2="$(cd "$WORK" && sh "$TMP/push.sh" 2>&1)"; then
    ok "re-push exits 0 (idempotent)"
    require_equal "$(printf '%s\n' "$out2" | sed -n 's/^remote_head=//p')" "$sha" "re-push reports the same sha"
else
    fail "idempotent re-push failed: $out2"
fi

# ── 4. non-fast-forward shape ─────────────────────────────────────────────────
# Diverge the worktree from the remote: a second clone pushes commits the
# worktree has never seen, then the worktree commits on its own old head —
# plain push must reject, the script must classify it.
if git clone -q "$ORIGIN" "$CLONE" 2>/dev/null \
    && git -C "$CLONE" config user.email test@example.com \
    && git -C "$CLONE" config user.name test \
    && printf 'remote-divergent\n' > "$CLONE/divergent.txt" \
    && git -C "$CLONE" add divergent.txt \
    && git -C "$CLONE" commit -qm divergent \
    && git -C "$CLONE" push -q origin HEAD:refs/heads/ticket-branch 2>/dev/null \
    && printf 'worktree-divergent\n' > "$WORK/divergent.txt" \
    && git -C "$WORK" add divergent.txt \
    && git -C "$WORK" commit -qm divergent-worktree; then
    if out3="$(cd "$WORK" && sh "$TMP/push.sh" 2>&1)"; then
        fail "non-fast-forward push unexpectedly succeeded: $out3"
    else
        printf '%s\n' "$out3" | grep -q '^non_fast_forward=1' \
            && ok "non-FF classified with non_fast_forward=1" \
            || fail "non-FF not classified (missing non_fast_forward=1): $out3"
    fi
else
    fail "non-FF fixture setup failed"
fi

# ── 5. the mechanical path never carries a force flag ────────────────────────
# Render is deterministic; assert on the template itself plus the render:
if grep -q -- '--force' "$TREE_ROOT/workflows/upstream_sync/scripts/push_branch.sh.tftpl" \
    || grep -q -- '--force' "$TMP/push.sh"; then
    fail "mechanical push path must never carry a force flag (ruling 9)"
else
    ok "no force flag in the mechanical path"
fi

# ── 6. worktree .git/config stays credential-free ─────────────────────────────
if grep -rq "WORKFLOW_UPSTREAM_TOKEN" "$WORK/.git/config" 2>/dev/null; then
    fail "token leaked into worktree .git/config (must be run-time helper only)"
else
    ok "no credential material at rest in worktree config"
fi

if [ "$FAILED" -ne 0 ]; then
    echo "test_upstream_sync: $FAILED failure(s)" >&2
    exit 1
fi
echo "test_upstream_sync: all pins hold"
