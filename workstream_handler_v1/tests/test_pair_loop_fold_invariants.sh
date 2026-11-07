#!/usr/bin/env bash
# KB-386 Stage 3 fold invariants — durable guard for the fold of the r2/r3
# round plumbing onto pair_programming_loop_v2.
#
# The Stage-3 fold deletes the retired round-suffixed workflow trees (r2/r3)
# and retargets every pair-loop handler dial at the v2 tree. The existing
# suite covers parts of this: test_branch_manager_final_say.sh pins the r2/r3
# hand-off dial SOURCES and the retired-tree absence under
# workstream_handler_v1/workflows only; test_pair_loop_local_isolation.sh
# scans the surviving loop trees for upstream surfaces. This guard pins the
# five fold invariants no existing test covers end-to-end:
#
#   INVARIANT 1 — ALL FOUR pair-loop dial blocks in main.chcl
#     (pair_programming_loop, pair_programming_loop_feedback,
#     pair_programming_loop_feedback_r2, pair_programming_loop_feedback_r3)
#     carry source = "./workflows/pair_programming_loop_v2", asserted by dial
#     name. A base/feedback dial drifting off the v2 tree is a regression the
#     final-say test does not see (it asserts only the r2/r3 sources).
#
#   INVARIANT 2 — the retired round trees are absent repo-wide: zero live
#     path, dial source, route binding or CI reference anywhere in the repo,
#     and no directory of that name may exist anywhere.
#
#   INVARIANT 3 — the v1 base tree workflows/pair_programming_loop/ still
#     exists: it is the retained parity baseline (the retained parity baseline) and
#     must survive any later cleanup.
#
#   INVARIANT 4 — no round-suffixed copilot instance names remain anywhere in
#     the repo (developer_rN / reviewer_rN adapter instances died with the
#     round trees; remediation is per-part INSIDE the v2 tree).
#
# Conventions: suite style (set -euo pipefail, ok/fail helpers, non-zero exit
# on failure). CI and the Makefile run every workstream_handler_v1/tests/
# test_*.sh via their existing glob, so this file needs no Makefile or
# .github edit.
#
# Negative arms FIRST: every scanner is probed against a scratch tree so the
# guard can demonstrably fail, plus a clean probe proving the scanners do not
# false-positive on the honest dial names and v2 sources. The retired-tree prefix and every probe line are
# assembled AT RUNTIME — the prefix spelled in pieces, as
# test_branch_manager_final_say.sh spells it — so this guard's own source
# never becomes a live repo-wide match of the reference it scans for.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TREE_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
REPO_ROOT="$(cd "${TREE_ROOT}/.." && pwd)"

FAILED=0
ok() {
    echo "PASS: $1"
}
fail() {
    FAILED=$((FAILED + 1))
    echo "FAIL: $1" >&2
}

# ── the retired-tree prefix, spelled in pieces ───────────────────────────────
# The scan key is the retired prefix "pair_programming_loop_" + "round";
# assembled only at runtime so no line of this guard's own source (or of any
# test file) carries it contiguously.
loop_tree_prefix="pair_programming_loop_"
retired_tree_prefix="${loop_tree_prefix}round"

# Live round-suffixed copilot instance shapes (INVARIANT 4): the adapter
# declaration line and the route-binding target. Prose that merely DESCRIBES
# the retired names (unquoted, like main.chcl's fold commentary) matches
# neither shape — this is the same precedent
# test_branch_manager_final_say.sh uses for its handlers-only check.
instance_decl_ere='adapter "copilot" "[A-Za-z0-9_]+_r[0-9]+"'
instance_target_ere='adapter\.copilot\.[A-Za-z0-9_]+_r[0-9]+'

# Scan helpers are scan-root-parameterized so the negative arms reuse the
# exact implementations that assert against the real repo.
#
# content_matches: file CONTENT containing the search key. On a git checkout
# the scan is tracked-files-only (deterministic: no .git blobs, no untracked
# scratch noise; a physically re-added tree is still caught by the directory
# scan below). Paths print relative to the scan root in both arms.
content_matches() { # $1=scan root, $2=search key -> relative paths of hits
    if git -C "$1" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
        git -C "$1" grep -l -F -- "$2" 2>/dev/null || true
    else
        (cd "$1" && find . -name .git -prune -o -type f -print \
            | xargs grep -l -F -- "$2" 2>/dev/null) || true
    fi
}

retired_dirs() { # $1=scan root, $2=dir-name prefix -> relative paths of hit dirs
    (cd "$1" && find . -name .git -prune -o -type d -name "${2}*" -print) | sort
}

instance_name_files() { # $1=scan root -> relative paths carrying a live shape
    if git -C "$1" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
        git -C "$1" grep -l -E -e "${instance_decl_ere}" -e "${instance_target_ere}" 2>/dev/null || true
    else
        (cd "$1" && find . -name .git -prune -o -type f -print \
            | xargs grep -l -E -e "${instance_decl_ere}" -e "${instance_target_ere}" 2>/dev/null) || true
    fi
}

# ── negative arms: the scanners must be able to fail ─────────────────────────

echo "==> Probing the scanners against scratch trees (the guard must fail)..."
TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT

probe_root="${TMP}/probe"
mkdir -p "${probe_root}/workflows"
# A pretend retired dial source, assembled at runtime:
printf 'subworkflow "probe_retired_dial" {\n    source = "./workflows/%s2"\n}\n' \
    "${retired_tree_prefix}" > "${probe_root}/dial_probe.chcl"
if [ -n "$(content_matches "${probe_root}" "${retired_tree_prefix}")" ]; then
    ok "negative arm: a live retired-tree dial source is flagged by the content scan"
else
    fail "negative arm: the content scan does not flag a retired-tree dial source"
fi

# A physically re-added retired tree directory (untracked, non-git scratch):
mkdir -p "${probe_root}/workflows/${retired_tree_prefix}2"
if [ -n "$(retired_dirs "${probe_root}" "${retired_tree_prefix}")" ]; then
    ok "negative arm: a re-added retired round tree directory is flagged by the dir scan"
else
    fail "negative arm: the dir scan does not flag a re-added retired round tree"
fi

# A live round-suffixed copilot instance, assembled at runtime (declaration +
# route-binding shapes):
printf 'adapter "copilot" "developer_r%s" {\n}\n    target = adapter.copilot.reviewer_r%s\n' \
    2 3 > "${probe_root}/instance_probe.chcl"
if [ -n "$(instance_name_files "${probe_root}")" ]; then
    ok "negative arm: a round-suffixed copilot instance declaration/binding is flagged"
else
    fail "negative arm: the instance scan does not flag a round-suffixed copilot instance"
fi

# A git-tracked probe exercises the git-grep arm of the content scan (the
# scratch probes above fall back to the find-and-grep arm):
git_root="${TMP}/tracked"
mkdir -p "${git_root}"
printf 'source = "./workflows/%s3"\n' "${retired_tree_prefix}" > "${git_root}/tracked_probe.chcl"
if (cd "${git_root}" && git init -q . && git add tracked_probe.chcl) && \
    [ -n "$(content_matches "${git_root}" "${retired_tree_prefix}")" ]; then
    ok "negative arm: a tracked retired-tree reference is flagged by the tracked-files content scan"
else
    fail "negative arm: the tracked-files content scan does not flag a retired-tree reference"
fi

# Anti-false-positive arm: honest fold-era shapes must match NOTHING.
clean_root="${TMP}/clean"
mkdir -p "${clean_root}"
printf 'subworkflow "pair_programming_loop_feedback_r2" {\n    source = "./workflows/pair_programming_loop_v2"\n}\nadapter "copilot" "pair_part_p1_dev" {\n}\n    target = adapter.copilot.coordinator\n' \
    > "${clean_root}/clean_probe.chcl"
if [ -z "$(content_matches "${clean_root}" "${retired_tree_prefix}")" ] \
    && [ -z "$(instance_name_files "${clean_root}")" ]; then
    ok "negative arm: honest v2 sources, dial names and instance names match nothing (no false positives)"
else
    fail "negative arm: a scan false-positives on honest fold-era shapes"
fi

# ── INVARIANT 1: all four pair-loop dial blocks re-enter the v2 tree ─────────

echo "==> Checking all four pair-loop dial blocks source pair_programming_loop_v2..."
for dial in pair_programming_loop pair_programming_loop_feedback \
    pair_programming_loop_feedback_r2 pair_programming_loop_feedback_r3; do
    block="$(sed -n "/^subworkflow \"${dial}\" {/,/^}/p" "${TREE_ROOT}/main.chcl")"
    if printf '%s' "${block}" | grep -qF 'source = "./workflows/pair_programming_loop_v2"'; then
        ok "INVARIANT 1: dial ${dial} carries source = ./workflows/pair_programming_loop_v2 (asserted by dial name)"
    else
        fail "INVARIANT 1: dial ${dial} does not source ./workflows/pair_programming_loop_v2"
    fi
done

# ── INVARIANT 2: the retired round trees are absent repo-wide ────────────────

echo "==> Scanning the whole repo for live references to the retired round trees..."
retired_hits="$(content_matches "${REPO_ROOT}" "${retired_tree_prefix}")"
live_refs=0
while IFS= read -r rel; do
    [ -z "${rel}" ] && continue
    rel="${rel#./}"
    live_refs=$((live_refs + 1))
    echo "    live retired-tree reference: ${rel}" >&2
done < <(printf '%s\n' "${retired_hits}")

retired_tree_dirs="$(retired_dirs "${REPO_ROOT}" "${retired_tree_prefix}")"
for d in ${retired_tree_dirs}; do
    live_refs=$((live_refs + 1))
    echo "    live retired-tree directory: ${d}" >&2
done

if [ "${live_refs}" -eq 0 ]; then
    ok "INVARIANT 2: zero live retired-tree references repo-wide (no path, dial source, route binding or CI target); no retired-tree directory exists anywhere"
else
    fail "INVARIANT 2: ${live_refs} live retired-tree reference(s)/directories remain (see above)"
fi

# ── INVARIANT 3: the v1 base tree survives as the retained parity baseline ───

echo "==> Checking the v1 base loop tree is still present (retained parity baseline)..."
v1_base="${TREE_ROOT}/workflows/pair_programming_loop"
v1_missing=0
for rel in main.chcl adapters.chcl variables.chcl agents scripts; do
    if [ ! -e "${v1_base}/${rel}" ]; then
        v1_missing=$((v1_missing + 1))
        echo "    missing: workflows/pair_programming_loop/${rel}" >&2
    fi
done
if [ "${v1_missing}" -eq 0 ]; then
    ok "INVARIANT 3: workflows/pair_programming_loop/ survives intact (main/adapters/variables/agents/scripts) — the retained parity baseline (the retained parity baseline) was not cleaned up"
else
    fail "INVARIANT 3: ${v1_missing} part(s) of the retained v1 parity baseline tree are gone"
fi

# ── INVARIANT 4: no round-suffixed copilot instance names anywhere ──────────

echo "==> Scanning the whole repo for live round-suffixed copilot instance names..."
instance_hits="$(instance_name_files "${REPO_ROOT}")"
if [ -z "${instance_hits}" ]; then
    ok "INVARIANT 4: zero round-suffixed copilot instance declarations/bindings repo-wide (developer_rN / reviewer_rN retired with the round trees; remediation is per-part inside the v2 tree)"
else
    fail "INVARIANT 4: round-suffixed copilot instance names still referenced in:"
    while IFS= read -r rel; do
        echo "    ${rel}" >&2
    done < <(printf '%s\n' "${instance_hits}")
fi

if [ "${FAILED}" -eq 0 ]; then
    echo "test_pair_loop_fold_invariants: all four fold invariants hold"
    exit 0
fi
echo "test_pair_loop_fold_invariants: ${FAILED} check(s) failed" >&2
exit 1
