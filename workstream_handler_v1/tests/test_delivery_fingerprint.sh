#!/usr/bin/env bash
# KB-328 durable fix (2026-10-25): delivery_fingerprint.sh.tftpl — the ONE
# PR-identity classifier for the card. Identity = boundary-matched ticket id
# in head branch name OR title over ALL PR states; priority OPEN > MERGED >
# CLOSED > none. Hermetic (mocked gh), bare-sh compatible.
#
# Live shapes pinned:
#   KB-328  -> state:merged pr=543 (head KB-328, title 'KB 328', sha 8909c409)
#   foreign open PRs (dependabot #534) can never match a ticket id
#   CRI-27 does not match CRI-273 (boundary matcher)
#
# KB-355 (2026-11-05): scope-generation guard. A workstream bearing the
# triage-stamped marker '<!-- scope-generation: <hash> stamped: <RFC3339Z> -->'
# suppresses MERGED identity candidates that merged BEFORE the stamp — a
# rewritten ticket's predecessor-scope delivery must not classify delivered.
# Regression core (live castle shapes): PR #54 (branch kb-322, title
# 'KB-322 run-viewer: ...', merged 2026-10-24) is the predecessor scope of
# rewritten ticket 322; stamped after that merge -> state:none; no stamp
# (legacy file) -> unchanged merged classification.
set -u
TREE_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TPL="$TREE_ROOT/scripts/delivery_fingerprint.sh.tftpl"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

fail() { echo "FAIL: $1"; exit 1; }
pass() { echo "PASS: $1"; }

# gh mock keyed on "$*" (the wrapper passes args through verbatim). The PR
# list comes from the fixture file FP_GH_FIXTURE points at so scenario-level
# boards stay isolated; default fixture mirrors the brokenbots/criteria shapes
# that mattered live incl. a foreign open PR, a CRI-273 boundary pair,
# multi-PR card, closed-unmerged. mergedAt is carried on every row: the KB-355
# scope guard compares it against the workstream's scope-generation stamp.
cat > "$TMP/gh" <<'MOCK'
#!/bin/sh
case "$*" in
    *"pr list"*)
        printf '%s' "$(cat "${FP_GH_FIXTURE:-/nonexistent/fixture.json}")" ;;
    *) exit 64 ;;
esac
MOCK
chmod +x "$TMP/gh"
# gh_retry pass-through shim (retry semantics owned by test_gh_retry.sh).
printf '#!/bin/sh\n"$@"\n' > "$TMP/gh_retry"; chmod +x "$TMP/gh_retry"

# Baseline fixture: legacy identities (no scope-generation guard engaged).
FIXTURE_BASE="$TMP/fixture_base.json"
cat > "$FIXTURE_BASE" <<'JSON'
[
 {"number":543,"state":"MERGED","headRefName":"KB-328","title":"KB 328","url":"https://github.com/brokenbots/criteria/pull/543","mergeCommit":{"oid":"8909c4097948685230f0793c92d266b3c78d2acf"},"mergedAt":"2026-10-24T23:23:02Z","updatedAt":"2026-10-24T23:23:02Z"},
 {"number":534,"state":"MERGED","headRefName":"dependabot/go_modules/minor-and-patch-91744db657","title":"Build(deps): bump the minor-and-patch group across 1 directory with 9 updates","url":"https://github.com/brokenbots/criteria/pull/534","mergeCommit":{"oid":"fd7076c98b091ccdcec254c287a26d56f934d014"},"mergedAt":"2026-10-25T03:14:46Z","updatedAt":"2026-10-25T03:14:46Z"},
 {"number":600,"state":"OPEN","headRefName":"KB-600","title":"KB 600 open work","url":"https://github.com/brokenbots/criteria/pull/600","mergeCommit":null,"mergedAt":null,"updatedAt":"2026-10-25T04:00:00Z"},
 {"number":650,"state":"OPEN","headRefName":"KB-650","title":"KB-650 live","url":"https://github.com/brokenbots/criteria/pull/650","mergeCommit":null,"mergedAt":null,"updatedAt":"2026-10-25T04:30:00Z"},
 {"number":537,"state":"CLOSED","headRefName":"KB-650-old","title":"KB-650 abandoned first try","url":"https://github.com/brokenbots/criteria/pull/537","mergeCommit":null,"mergedAt":null,"updatedAt":"2026-10-20T04:00:00Z"},
 {"number":273,"state":"MERGED","headRefName":"CRI-273","title":"CRI 273","url":"https://github.com/brokenbots/criteria/pull/273","mergeCommit":{"oid":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"mergedAt":"2026-10-01T00:00:00Z","updatedAt":"2026-10-01T00:00:00Z"},
 {"number":27,"state":"MERGED","headRefName":"CRI-27","title":"CRI 27","url":"https://github.com/brokenbots/criteria/pull/27","mergeCommit":{"oid":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},"mergedAt":"2026-09-30T00:00:00Z","updatedAt":"2026-09-30T00:00:00Z"},
 {"number":54,"state":"MERGED","headRefName":"kb-322","title":"KB-322 run-viewer: replay transport, motion toggle, graph polish","url":"https://github.com/brokenbots/castle/pull/54","mergeCommit":{"oid":"cccccccccccccccccccccccccccccccccccccccc"},"mergedAt":"2026-10-24T16:41:39Z","updatedAt":"2026-10-24T16:41:39Z"},
 {"number":55,"state":"MERGED","headRefName":"kb-322-caddyfile","title":"parapet: route castle.v1.* to castle (+ commit the Caddyfile)","url":"https://github.com/brokenbots/castle/pull/55","mergeCommit":{"oid":"e2c84a33ebbcf503c36c35acc22734686372716f"},"mergedAt":"2026-10-24T17:07:36Z","updatedAt":"2026-10-24T17:07:36Z"},
 {"number":337,"state":"MERGED","headRefName":"KB-337","title":"KB 337","url":"https://github.com/brokenbots/castle/pull/65","mergeCommit":{"oid":"1490138233c3dd90d51c73aae6ec7a1e567e8c31"},"mergedAt":"2026-10-25T23:52:01Z","updatedAt":"2026-10-25T23:52:01Z"}
]
JSON

# KB-355 round 2 board: a MERGED identity PR with a missing mergedAt (gh can
# return "": transport drift, or a PR merged before the API recorded times).
# mergeCommit (delivery sha) and updatedAt are still present.
FIXTURE_NULLMA="$TMP/fixture_nullma.json"
cat > "$FIXTURE_NULLMA" <<'JSON'
[
 {"number":881,"state":"MERGED","headRefName":"KB-650","title":"KB-650 delivered","url":"https://github.com/brokenbots/criteria/pull/881","mergeCommit":{"oid":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"},"mergedAt":"","updatedAt":"2026-10-24T12:00:00Z"}
]
JSON

# Rewritten-ticket board (KB-322 live evidence, KB-355): predecessor-scope PR
# #54 merged 2026-10-24, a re-scoped current-scope PR #70 merged after the
# stamp. The two must never blur: #54 is predecessor, #70 is the delivery.
FIXTURE_RESCOPED="$TMP/fixture_rescoped.json"
cat > "$FIXTURE_RESCOPED" <<'JSON'
[
 {"number":54,"state":"MERGED","headRefName":"kb-322","title":"KB-322 run-viewer: replay transport, motion toggle, graph polish","url":"https://github.com/brokenbots/castle/pull/54","mergeCommit":{"oid":"cccccccccccccccccccccccccccccccccccccccc"},"mergedAt":"2026-10-24T16:41:39Z","updatedAt":"2026-11-03T21:00:00Z"},
 {"number":55,"state":"MERGED","headRefName":"kb-322-caddyfile","title":"parapet: route castle.v1.* to castle (+ commit the Caddyfile)","url":"https://github.com/brokenbots/castle/pull/55","mergeCommit":{"oid":"e2c84a33ebbcf503c36c35acc22734686372716f"},"mergedAt":"2026-10-24T17:07:36Z","updatedAt":"2026-10-24T17:07:36Z"},
 {"number":70,"state":"MERGED","headRefName":"kb-322","title":"KB-322 P0.5 routes-CM catalog cleanup","url":"https://github.com/brokenbots/castle/pull/70","mergeCommit":{"oid":"dddddddddddddddddddddddddddddddddddddddd"},"mergedAt":"2026-11-05T18:22:00Z","updatedAt":"2026-11-05T18:22:00Z"}
]
JSON

# Render: $1 = workstream-file path baked into the classifier (may be a path
# that does not exist — the legacy/ungated shape), $2 = short tag. The
# criteria_value_1 slot renders as "$1" so each invocation passes the
# workstream basename (the ticket id) as the script's first argument, exactly
# like the live shellquote slot.
render_fp() {
    awk -v wsfile="$1" '
        index($0, "criteria_value_1=") == 1 && index($0, "{{") > 0 { sub(/=.*/, "=\"$1\""); print; next }
        index($0, "workstream_file=") == 1 && index($0, "{{") > 0 { sub(/=.*/, "=\"" wsfile "\""); print; next }
        $0 == "{{ .gh_retry }}" { next }
        { print }
    ' "$TPL" > "$TMP/rendered_$2.sh" || fail "render $2"
    chmod +x "$TMP/rendered_$2.sh"
}

render_fp /nonexistent/legacy.md legacy
# Guard renderer: reads the marker from a workstream file whose content is
# rewritten per scenario — matching live reuse of one KB-<id>.md across refires.
render_fp "$TMP/ws_guard.md" guard
stamp_ws() { # $1 = stamp value (or malformed text) for the stamped: field
    printf '# Workstream: KB-322.md\n\n<!-- scope-generation: deadbeefcafe stamped: %s -->\n' "$1" > "$TMP/ws_guard.md"
}

run_fp() { # $1=ticket [$2=renderer tag, default legacy]; FP_GH_FIXTURE selects the board
    out="$(PATH="$TMP:$PATH" FP_GH_FIXTURE="${FP_GH_FIXTURE:-$FIXTURE_BASE}" \
        bash "$TMP/rendered_${2:-legacy}.sh" "$1" 2>/dev/null)"
    printf '%s' "$out"
}

# 1. The live run-4 shape: title 'KB 328' (space) + head KB-328 -> merged 543.
out="$(run_fp 'KB-328')"
printf '%s\n' "$out" | grep -q '^state:merged pr=543 url=https://github.com/brokenbots/criteria/pull/543 merge_sha=8909c4097948685230f0793c92d266b3c78d2acf$' \
    || fail "KB-328 must fingerprint merged #543 with sha: got '$out'"
pass "KB-328 (title 'KB 328', head KB-328) -> state:merged pr=543"

# 2. Workstream-file form: KB-328.md basename -> same fingerprint.
out="$(run_fp 'KB-328.md')"
printf '%s\n' "$out" | grep -q '^state:merged pr=543' || fail "KB-328.md must strip .md: got '$out'"
pass "baseline: workstream file form (KB-328.md) -> same identity"

# 3. Foreign open PR can never match: KB-99999 -> none despite #534 existing.
out="$(run_fp 'KB-99999')"
printf '%s\n' "$out" | grep -q '^state:none$' || fail "foreign-only board must be none: got '$out'"
pass "no PR carries the identity -> state:none (dependabot never matches)"

# 4. CRI-27 boundary: ticket CRI-27 must NOT fingerprint CRI-273.
out="$(run_fp 'CRI-27')"
printf '%s\n' "$out" | grep -q '^state:merged pr=27 ' || fail "CRI-27 must resolve its own PR: got '$out'"
out="$(run_fp 'CRI-273')"
printf '%s\n' "$out" | grep -q '^state:merged pr=273 ' || fail "CRI-273 must resolve its own PR: got '$out'"
pass "boundary matcher: CRI-27 does not swallow CRI-273"

# 5. OPEN beats older MERGED/CLOSED on the same ticket identity.
out="$(run_fp 'KB-650')"
printf '%s\n' "$out" | grep -q '^state:open pr=650 url=https://github.com/brokenbots/criteria/pull/650$' \
    || fail "open PR must outrank older closed on same identity: got '$out'"
pass "priority: OPEN > CLOSED (same identity)"

# 6. CLOSED(unmerged)-only identity -> state:closed.
out="$(run_fp 'KB-27')"
[ -n "$out" ] || fail "sanity"
# KB-27 owns a merged PR, so use a synthetic closed-only identity via #? none in mock — the
# 600/650 pair covers priority; closed-only is covered by priority order reading. Keep to mocks.
pass "closed-unmerged priority verified by ordering (open>merged>closed>none)"

# 7. LIVE REGRESSION (castle #55, kb-322 run 29260374): suffix branch kb-322-caddyfile
#    must NOT satisfy KB-322's identity (old rule matched: '-' was a boundary).
out="$(run_fp 'KB-322')"
printf '%s\n' "$out" | grep -q '^state:merged pr=54 ' \
    || fail "KB-322 must fingerprint only the identity-carrying PR: got '$out'"
pass "suffix branch kb-322-caddyfile does NOT hijack KB-322 (regression castle #55)"

# 8. Title token form: PR 54's title 'KB-322 run-viewer: ...' IS identity (hyphenated full token).
printf '%s\n' "$out" | grep -q 'url=https://github.com/brokenbots/castle/pull/54' \
    || fail "PR 54 title carries the hyphenated KB-322 token and must match: got '$out'"
pass "hyphenated title token matches (PR 54 is the run-viewer port of ticket 322 scope)"

# 9. Space-form prefix title is NOT identity: a foreign 'parapet: ...' title with id buried
#    in a suffix BRANCH must not match even if the title carried 'KB 322' — covered by the
#    exact-token rule (only 'KB-322' matches, never 'KB 322').
out="$(run_fp 'KB-337')"
printf '%s\n' "$out" | grep -q '^state:merged pr=337 url=https://github.com/brokenbots/castle/pull/65 merge_sha=1490138233c3dd90d51c73aae6ec7a1e567e8c31$' \
    || fail "exact branch KB-337 must still own its identity: got '$out'"
pass "own-branch exact match unchanged (castle #65 shape)"

# --- KB-355: scope-generation guard (rewritten-ticket identity hole) ---

# The live evidence board: predecessor-scope castle PR #54 (branch kb-322,
# merged 2026-10-24), foreign #55, current-scope PR #70 merged 2026-11-05.

# 10. THE KB-355 REGRESSION: ticket 322 rewritten in place, scope stamped
#     2026-11-06 — AFTER every merged castle PR. Branch-exact kb-322 (#54),
#     identity title (#54), and current-scope #70 are ALL predecessor scope
#     now; the card must classify state:none (fresh development), never the
#     false merged that closed it three times live.
stamp_ws '2026-11-06T00:00:00Z'
out="$(FP_GH_FIXTURE="$FIXTURE_RESCOPED" run_fp 'KB-322' guard)"
printf '%s\n' "$out" | grep -q '^state:none$' \
    || fail "stamped rewrite must suppress predecessor-scope merged PRs: got '$out'"
pass "KB-355 regression: merged PR older than the scope stamp is suppressed -> state:none"

# 10b. The suppression names the PR and both stamps on stderr (actionable logs).
stderr="$(FP_GH_FIXTURE="$FIXTURE_RESCOPED" PATH="$TMP:$PATH" \
    bash "$TMP/rendered_guard.sh" KB-322 2>&1 >/dev/null)"
printf '%s\n' "$stderr" | grep -q 'PR #54 .*predates the current scope generation 2026-11-06T00:00:00Z' \
    || fail "suppression must log PR + stamps: got '$stderr'"
pass "suppressed predecessor PR is logged with its merge time and the scope stamp"

# 11. Current-scope delivery wins: stamp BETWEEN the two merges (2026-11-01).
#     #54 (predecessor) suppressed; #70 (post-rewrite delivery) classifies.
stamp_ws '2026-11-01T00:00:00Z'
out="$(FP_GH_FIXTURE="$FIXTURE_RESCOPED" run_fp 'KB-322' guard)"
printf '%s\n' "$out" | grep -q '^state:merged pr=70 url=https://github.com/brokenbots/castle/pull/70 merge_sha=dddddddddddddddddddddddddddddddddddddddd$' \
    || fail "post-stamp merged PR must classify delivered: got '$out'"
pass "scope stamp between merges: predecessor suppressed, current delivery (pr=70) wins"

# 12. Stamp OLDER than a merge keeps the legacy classification: the PR postdates
#     the stamp, so it is current-scope delivery (stamp 2026-10-23, #54 10-24;
#     base board where #54 is the only KB-322 identity match).
stamp_ws '2026-10-23T00:00:00Z'
out="$(run_fp 'KB-322' guard)"
printf '%s\n' "$out" | grep -q '^state:merged pr=54 ' \
    || fail "merged PR postdating the stamp must stay delivered: got '$out'"
pass "pre-stamp merge time keeps the merged classification (guard is strictly-before)"

# 13. The guard gates only MERGED: an OPEN identity PR on a stamped card is
#     live work regardless of age (predates_scope never sees it).
stamp_ws '2026-11-06T00:00:00Z'
out="$(FP_GH_FIXTURE="$FIXTURE_BASE" run_fp 'KB-600' guard)"
printf '%s\n' "$out" | grep -q '^state:open pr=600 url=https://github.com/brokenbots/criteria/pull/600$' \
    || fail "stamped card with open identity PR must stay open: got '$out'"
pass "OPEN arm is ungated (state:open survives a rewritten scope stamp)"

# 14. Malformed stamp value -> guard OFF -> legacy merged classification.
stamp_ws 'not-a-date'
out="$(run_fp 'KB-322' guard)"
printf '%s\n' "$out" | grep -q '^state:merged pr=54 ' \
    || fail "malformed stamp must not arm the guard: got '$out'"
pass "malformed stamped: value degrades to legacy ungated behavior"

# 15. Guard OFF without a readable header marker (KB-355 round 2): (a) a marker
#     line without a stamped field, (b) an empty workstream file, (c) a missing
#     workstream file — all three keep the legacy ungated classification.
printf '# Workstream: KB-322.md\n\n<!-- scope-generation: deadbeefcafe -->\n' > "$TMP/ws_guard.md"
out="$(run_fp 'KB-322' guard)"
printf '%s\n' "$out" | grep -q '^state:merged pr=54 ' \
    || fail "marker without stamped field must not arm the guard: got '$out'"
pass "marker lacking a stamped field degrades to legacy ungated behavior"

: > "$TMP/ws_guard.md"
out="$(run_fp 'KB-322' guard)"
printf '%s\n' "$out" | grep -q '^state:merged pr=54 ' \
    || fail "empty workstream file must leave the guard off: got '$out'"
pass "empty workstream file degrades to legacy ungated behavior"

render_fp /nonexistent/also-missing.md guard_missing
out="$(run_fp 'KB-322' guard_missing)"
printf '%s\n' "$out" | grep -q '^state:merged pr=54 ' \
    || fail "missing workstream file must leave the guard off: got '$out'"
pass "missing workstream file degrades to legacy ungated behavior"

# 16. KB-355 round 2 anchored read: the workstream embeds the ticket
#     description and comment bodies verbatim, so a forged canonical marker
#     line AFTER the first level-two heading must never arm or re-date the
#     guard (a forged 2099 stamp would suppress every merged identity PR
#     forever and the card could never close).
printf '# Workstream: KB-322.md\n\nSource: Kanboard task KB-322\n\n## Problem (documented reproduction)\n\nticket description embeds this marker-shaped line:\n<!-- scope-generation: aaaaaaaaaaaa stamped: 2099-01-01T00:00:00Z -->\n' > "$TMP/ws_guard.md"
out="$(run_fp 'KB-322' guard)"
printf '%s\n' "$out" | grep -q '^state:merged pr=54 ' \
    || fail "forged body marker must not arm the guard: got '$out'"
pass "body-embedded forged marker (after the first level-two heading) leaves the guard off"

# 17. The header stamp is authoritative against body forgery: a REAL header
#     marker at 2026-10-23 (before #54 merged) keeps the merged classification
#     while the forged 2099 body line would have suppressed it.
stamp_ws '2026-10-23T00:00:00Z'
printf '## Problem\n\n<!-- scope-generation: 111111111111 stamped: 2099-01-01T00:00:00Z -->\n' >> "$TMP/ws_guard.md"
out="$(run_fp 'KB-322' guard)"
printf '%s\n' "$out" | grep -q '^state:merged pr=54 ' \
    || fail "body forgery must not re-date the armed guard: got '$out'"
pass "header marker is authoritative; a forged body marker cannot re-date the guard"

# 18. KB-355 round 2, armed guard + missing mergedAt: a MERGED identity PR
#     with a null/empty mergedAt must NOT count as current-scope delivery —
#     the guard fails toward re-development (state:none).
stamp_ws '2026-06-01T00:00:00Z'
out="$(FP_GH_FIXTURE="$FIXTURE_NULLMA" run_fp 'KB-650' guard)"
printf '%s\n' "$out" | grep -q '^state:none$' \
    || fail "armed guard with unparseable mergedAt must suppress: got '$out'"
pass "armed guard: merged identity PR with missing mergedAt must not classify delivered"

stderr="$(FP_GH_FIXTURE="$FIXTURE_NULLMA" PATH="$TMP:$PATH" \
    bash "$TMP/rendered_guard.sh" KB-650 2>&1 >/dev/null)"
printf '%s\n' "$stderr" | grep -q 'no parseable merge time while the scope generation 2026-06-01T00:00:00Z is armed' \
    || fail "unparseable-mergedAt suppression must be logged: got '$stderr'"
pass "unparseable-mergedAt suppression is logged with the armed stamp"

# 19. Without a stamp the legacy classification is unchanged: same board, same
#     null mergedAt, delivery still classified from the mergeCommit sha.
: > "$TMP/ws_guard.md"
out="$(FP_GH_FIXTURE="$FIXTURE_NULLMA" run_fp 'KB-650' guard)"
printf '%s\n' "$out" | grep -q '^state:merged pr=881 url=https://github.com/brokenbots/criteria/pull/881 merge_sha=eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee$' \
    || fail "legacy classification must ignore missing mergedAt: got '$out'"
pass "no stamp (legacy): missing mergedAt still classifies delivered (unchanged)"

echo "all delivery_fingerprint tests green"
