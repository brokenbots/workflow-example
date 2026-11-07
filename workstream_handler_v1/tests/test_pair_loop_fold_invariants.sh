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
#     path, dial source, route binding or CI reference anywhere in the repo.
#     The only repo-wide mentions allowed are the honest prose in the v2
#     tree's PARITY.md and PLAN.md, and no directory of that name may exist
#     anywhere (final-say checks only under workflows/, not the whole repo).
#
#   INVARIANT 3 — the v1 base tree workflows/pair_programming_loop/ still
#     exists: it is the retained parity baseline (PARITY.md section 1) and
#     must survive any later cleanup.
#
#   INVARIANT 4 — no round-suffixed copilot instance names remain anywhere in
#     the repo (developer_rN / reviewer_rN adapter instances died with the
#     round trees; remediation is per-part INSIDE the v2 tree).
#
#   INVARIANT 5 — the record's restore path is REAL. The stage-3 determination
#     (PARITY.md section 5 and the plan's Stage-3 close state) promises the
#     round-tree deletion stays restorable and carries a concrete restore
#     command; a stale or wrong commit or path in that command would leave the
#     promise false while invariants 1-4 still held. The guard parses the
#     deletion commit and the two retired tree paths out of the record
#     documents (never hard-coded) and asserts: the commit exists; the
#     commit's parent actually holds both retired round trees; the recorded
#     restore command names EXACTLY those two paths; and the record's
#     comparator invocation uses the comparator's real --compare flag
#     (live-proven on the comparator's self-test arms).
#
# Conventions: suite style (set -euo pipefail, ok/fail helpers, non-zero exit
# on failure). CI and the Makefile run every workstream_handler_v1/tests/
# test_*.sh via their existing glob, so this file needs no Makefile or
# .github edit.
#
# Negative arms FIRST: every scanner is probed against a scratch tree so the
# guard can demonstrably fail, plus a clean probe proving the scanners do not
# false-positive on the honest dial names and v2 sources; the restore-path
# check is probed against a scratch record naming a commit whose parent does
# not hold the retired trees. The retired-tree prefix and every probe line are
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

# ── INVARIANT 5 plumbing: the record's restore path ──────────────────────────
# Everything the check needs comes out of the record documents at runtime:
# the deletion commit (prose mention + restore command must agree), the two
# retired tree paths (assembled in pieces like every other probe line) and
# the comparator's gate-closing invocation. No sha is hard-coded in this file.
retired_rel2="workstream_handler_v1/workflows/${retired_tree_prefix}2"
retired_rel3="workstream_handler_v1/workflows/${retired_tree_prefix}3"
comparator_rel="workstream_handler_v1/tests/test_pair_loop_parity.sh"
comparator="${REPO_ROOT}/${comparator_rel}"
comparator_v1_fixture="${REPO_ROOT}/workstream_handler_v1/tests/fixtures/pair_loop_parity/self_test_v1_baseline.tsv"
comparator_pass_fixture="${REPO_ROOT}/workstream_handler_v1/tests/fixtures/pair_loop_parity/self_test_v2_candidate_pass.tsv"

# EREs for the record shapes: a restore-command line (the code span carries
# the retired prefix, spelled in pieces), the prose deletion-commit mention,
# and the record's comparator invocation with the real flag.
restore_line_ere="git +checkout +[0-9a-f]{7,40}"'\^'" +-- +.*${retired_tree_prefix}"
prose_commit_ere='commit +`[0-9a-f]{7,40}`'
comparator_inv_ere='workstream_handler_v1/tests/test_pair_loop_parity\.sh +--compare'

# extract_section <file> <heading-ERE>: the heading's section, through the
# next top-level "## " heading (### subsections stay inside the section).
extract_section() { # $1=file, $2=heading ERE -> section text on stdout
    awk -v re="$2" '
        { if ($0 ~ re) { on = 1; next } if (on && $0 ~ /^## /) { on = 0 } if (on) print }
    ' "$1"
}

# restore_parse_section <section file>: parse the record's restore command
# line(s). Prints the deletion sha they name; fails unless at least one
# well-formed line exists, every line agrees on the commit, and the command's
# arguments after -- are EXACTLY the two retired tree paths. The git reality
# legs live separately (restore_reality) so the negative arm can fail on the
# reality leg alone.
restore_parse_section() { # $1=section file -> stdout "sha"
    local lines line tok p1 p2 n path_ok sha
    lines="$(grep -E -- "${restore_line_ere}" "$1" || true)"
    if [ -z "${lines}" ]; then
        return 1
    fi
    sha=""
    while IFS= read -r line; do
        [ -n "${line}" ] || continue
        # strip the markdown code backticks, then word-split the command
        line="$(printf '%s' "${line}" | tr -d '\140')"
        set -f
        set -- ${line}
        set +f
        if [ "$#" -lt 6 ] || [ "$1" != "git" ] || [ "$2" != "checkout" ] \
            || [ "$4" != "--" ]; then
            return 1
        fi
        case "$3" in
            *\^) tok="${3%\^}" ;;
            *) return 1 ;;
        esac
        case "${tok}" in
            *[!0-9a-f]* | "") return 1 ;;
        esac
        n=$(( $# - 4 ))
        if [ "${n}" -ne 2 ]; then
            return 1
        fi
        p1="${5%.}"
        p2="${6%.}"
        path_ok=0
        if [ "${p1}" = "${retired_rel2}" ] && [ "${p2}" = "${retired_rel3}" ]; then
            path_ok=1
        elif [ "${p1}" = "${retired_rel3}" ] && [ "${p2}" = "${retired_rel2}" ]; then
            path_ok=1
        fi
        if [ "${path_ok}" -ne 1 ]; then
            return 1
        fi
        if [ -z "${sha}" ]; then
            sha="${tok}"
        elif [ "${sha}" != "${tok}" ]; then
            return 1
        fi
    done <<< "${lines}"
    [ -n "${sha}" ] || return 1
    printf '%s\n' "${sha}"
}

# restore_reality <git root> <sha>: the record's promise must hold in git
# history — (i) the commit exists; (ii) the commit's parent actually holds
# BOTH retired round trees. Fails on the first untrue leg, named on stderr.
restore_reality() { # $1=git root, $2=sha -> rc 0 iff both legs true
    if ! git -C "$1" cat-file -e "${2}^{commit}" 2>/dev/null; then
        echo "    restore-reality: commit ${2} does not exist in ${1}" >&2
        return 1
    fi
    if ! git -C "$1" cat-file -e "${2}^:${retired_rel2}" 2>/dev/null; then
        echo "    restore-reality: the parent of ${2} does not hold ${retired_rel2}" >&2
        return 1
    fi
    if ! git -C "$1" cat-file -e "${2}^:${retired_rel3}" 2>/dev/null; then
        echo "    restore-reality: the parent of ${2} does not hold ${retired_rel3}" >&2
        return 1
    fi
    return 0
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

# INVARIANT 5's negative arm: a well-formed restore record naming a commit
# whose PARENT does not hold the retired trees. The parser must accept it
# (the command is syntactically exact) but the restore-reality leg must FAIL
# — the guard can catch a stale or wrong commit in the record. Assembled at
# runtime in a scratch git repo whose root commit holds nothing.
neg_root="${TMP}/negrecord"
if git init -q "${neg_root}" \
    && git -C "${neg_root}" -c user.name=probe -c user.email=probe@example.invalid commit --allow-empty -q -m "scratch root: empty tree" \
    && printf 'probe\n' > "${neg_root}/probe.txt" \
    && git -C "${neg_root}" add probe.txt \
    && git -C "${neg_root}" -c user.name=probe -c user.email=probe@example.invalid commit -q -m "scratch probe commit" \
    && neg_sha="$(git -C "${neg_root}" rev-parse HEAD)"; then
    neg_record="${TMP}/neg_record.md"
    {
        printf '## 5. scratch negative-arm probe\n\n'
        printf 'commit `%s` — pretends to be the deletion commit.\n\n' "${neg_sha}"
        printf 'git checkout %s^ -- %s %s\n' "${neg_sha}" "${retired_rel2}" "${retired_rel3}"
    } > "${neg_record}"
    neg_parsed=""
    if neg_parsed="$(restore_parse_section "${neg_record}")" \
        && [ "${neg_parsed}" = "${neg_sha}" ] \
        && ! restore_reality "${neg_root}" "${neg_sha}" >/dev/null 2>&1; then
        ok "negative arm: a record naming a commit whose parent does not hold the retired trees fails the restore-reality check (the parse alone accepts it)"
    else
        fail "negative arm: the restore check does not catch a record naming a commit whose parent lacks the retired trees"
    fi
else
    fail "negative arm: the scratch git repo for the restore check could not be built"
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
retired_v2_prose="workstream_handler_v1/workflows/pair_programming_loop_v2"
retired_hits="$(content_matches "${REPO_ROOT}" "${retired_tree_prefix}")"
live_refs=0
prose_refs=0
while IFS= read -r rel; do
    [ -z "${rel}" ] && continue
    rel="${rel#./}"
    case "${rel}" in
        "${retired_v2_prose}/PARITY.md" | "${retired_v2_prose}/PLAN.md")
            prose_refs=$((prose_refs + 1))
            ;;
        *)
            live_refs=$((live_refs + 1))
            echo "    live retired-tree reference: ${rel}" >&2
            ;;
    esac
done < <(printf '%s\n' "${retired_hits}")

retired_tree_dirs="$(retired_dirs "${REPO_ROOT}" "${retired_tree_prefix}")"
for d in ${retired_tree_dirs}; do
    live_refs=$((live_refs + 1))
    echo "    live retired-tree directory: ${d}" >&2
done

if [ "${live_refs}" -eq 0 ]; then
    ok "INVARIANT 2: zero live retired-tree references repo-wide (no path, dial source, route binding or CI target; ${prose_refs} prose-hit file(s), all inside ${retired_v2_prose}/); no retired-tree directory exists anywhere"
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
    ok "INVARIANT 3: workflows/pair_programming_loop/ survives intact (main/adapters/variables/agents/scripts) — the retained parity baseline (PARITY.md section 1) was not cleaned up"
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

# ── INVARIANT 5: the record's restore path is REAL ───────────────────────────

echo "==> Checking the record's restore path resolves (commit real, parent holds the retired trees, command exact, comparator flag real)..."
parity_doc="${TREE_ROOT}/workflows/pair_programming_loop_v2/PARITY.md"
plan_doc="${TREE_ROOT}/workflows/pair_programming_loop_v2/PLAN.md"
inv5_ok=1

# (iv) the comparator script really supports the flag the record invokes it
# with: the compare lane runs and exits 0 on the comparator's own self-test
# arms — they prove the COMPARATOR (never a real card, per the record).
if sh "${comparator}" --compare "${comparator_v1_fixture}" "${comparator_pass_fixture}" >/dev/null 2>&1; then
    :
else
    inv5_ok=0
    fail "INVARIANT 5: the comparator does not support --compare — the record's gate-closing invocation could not run (sh ${comparator_rel} --compare <v1.tsv> <v2.tsv> did not exit 0 on the comparator's self-test arms)"
fi

# Both record sections must: record a restore command that parses and names
# EXACTLY the two retired tree paths; name the deletion commit in prose as the
# SAME commit the restore command uses; carry a comparator invocation with the
# real --compare flag; and the commit must resolve in git history with both
# retired trees in its parent.
if [ -f "${parity_doc}" ] && [ -f "${plan_doc}" ]; then
    parity_sec="${TMP}/sec_parity5.md"
    plan_sec="${TMP}/sec_plan_stage3.md"
    extract_section "${parity_doc}" '^## 5\.' > "${parity_sec}"
    extract_section "${plan_doc}" '^## Stage-3' > "${plan_sec}"

    parity_sha=""
    plan_sha=""
    for doc in parity plan; do
        case "${doc}" in
            parity) sec="${parity_sec}"; label="PARITY.md section 5" ;;
            plan)   sec="${plan_sec}";  label="PLAN.md Stage-3 section" ;;
        esac

        # (iii) the restore command parses and names exactly the two paths
        if ! doc_sha="$(restore_parse_section "${sec}")"; then
            inv5_ok=0
            fail "INVARIANT 5: ${label}'s recorded restore command is missing, ambiguous, or does not name exactly the two retired round trees"
            continue
        fi

        # the deletion commit is also named in prose, as the SAME commit
        prose_shas="$(grep -oE -- "${prose_commit_ere}" "${sec}" \
            | sed -E 's/^commit[[:space:]]+`([0-9a-f]{7,40})`$/\1/' || true)"
        prose_n=0
        prose_bad=0
        while IFS= read -r ps; do
            [ -n "${ps}" ] || continue
            prose_n=$((prose_n + 1))
            if [ "${ps}" != "${doc_sha}" ]; then
                prose_bad=$((prose_bad + 1))
            fi
        done < <(printf '%s\n' "${prose_shas}")
        if [ "${prose_n}" -eq 0 ]; then
            inv5_ok=0
            fail "INVARIANT 5: ${label} names no prose deletion commit"
        elif [ "${prose_bad}" -gt 0 ]; then
            inv5_ok=0
            fail "INVARIANT 5: ${label}'s prose deletion commit disagrees with its restore command's commit"
        fi

        # (iv) the record invokes the comparator with the real --compare flag
        inv_n="$(grep -cE -- "${comparator_inv_ere}" "${sec}" || true)"
        if [ "${inv_n}" -eq 0 ]; then
            inv5_ok=0
            fail "INVARIANT 5: ${label} carries no comparator invocation with the real --compare flag"
        fi

        # (i) + (ii) the commit is real, and its parent holds both retired trees
        if ! restore_reality "${REPO_ROOT}" "${doc_sha}"; then
            inv5_ok=0
            fail "INVARIANT 5: ${label}'s restore path does not resolve against git history (see above)"
            continue
        fi

        case "${doc}" in
            parity) parity_sha="${doc_sha}" ;;
            plan)   plan_sha="${doc_sha}" ;;
        esac
    done

    if [ -n "${parity_sha}" ] && [ -n "${plan_sha}" ] \
        && [ "${parity_sha}" != "${plan_sha}" ]; then
        inv5_ok=0
        fail "INVARIANT 5: the two record sections disagree on the deletion commit (PARITY.md says ${parity_sha}, PLAN.md says ${plan_sha})"
    fi

    if [ "${inv5_ok}" -eq 1 ]; then
        ok "INVARIANT 5: the record's restore path is real — deletion commit ${parity_sha} exists and its parent holds both retired round trees; PARITY.md section 5 and the PLAN.md Stage-3 section record a restore command naming exactly those two paths; the record's comparator invocation uses the comparator's real --compare flag"
    fi
else
    inv5_ok=0
    fail "INVARIANT 5: the record documents are missing (expected ${parity_doc} and ${plan_doc})"
fi

if [ "${FAILED}" -eq 0 ]; then
    echo "test_pair_loop_fold_invariants: all five fold invariants hold"
    exit 0
fi
echo "test_pair_loop_fold_invariants: ${FAILED} check(s) failed" >&2
exit 1
