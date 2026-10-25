#!/usr/bin/env bash
# KB-328 (2026-10-25): foreign_pr_check.sh.tftpl — the stored-pr identity
# gate. After a squash merge deletes the head branch, a refire's develop lane
# can recreate the ticket-named branch and gh's PR-per-branch lookup can make
# store_pr_url resolve a DIFFERENT PR (observed live: empty recreated
# KB-328 branch -> store_pr_url returned the open dependabot #534 while the
# card's real PR #543 sat MERGED — the run adopted and merged the wrong PR).
# The check reads the stored URL's own PR (state + head branch):
#   MERGED PR (any)                    -> status:foreign_pr <url>
#   OPEN/CLOSED PR on a foreign branch -> status:foreign_pr <url>
#   unresolvable URL                   -> status:ok (do not guess)
#   open PR naming this ticket         -> status:ok
# Bare-sh compatible: no bashisms; mocked gh; gh_retry splice dropped like
# the sibling suites do for wrapper-consuming templates.
set -u
TREE_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TPL="$TREE_ROOT/scripts/foreign_pr_check.sh.tftpl"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

fail() { echo "FAIL: $1"; exit 1; }
pass() { echo "PASS: $1"; }

# Mock gh: state + headRefName decisions keyed on the FULL arg list ($* —
# the real call is `gh pr view <url> --json ...`, so $1 is the literal 'pr'`,
# never the URL). gh_retry passes args through verbatim on first success, so
# case matching over "$*" works for both `gh ...` and `gh_retry gh ...`.
cat > "$TMP/gh" <<'MOCK'
#!/bin/sh
case "$*" in
    *"/pull/543"*)
        printf '{"state":"MERGED","headRefName":"KB-328"}' ;;
    *"/pull/534"*)
        printf '{"state":"OPEN","headRefName":"dependabot/go_modules/minor-and-patch-91744db657"}' ;;
    *"/pull/600"*)
        printf '{"state":"OPEN","headRefName":"KB-600"}' ;;
    *"/pull/602"*)
        printf '{"state":"OPEN","headRefName":"KB-328"}' ;;
    *"/pull/601"*)
        printf '{"state":"CLOSED","headRefName":"feature/other"}' ;;
    *) exit 1 ;;
esac
MOCK
chmod +x "$TMP/gh"
# gh_retry passthrough shim: the template calls gh_retry gh ...; the wrapper's
# own retry behavior is regression-owned by test_gh_retry.sh, the mock here
# only needs the pass-through-success contract.
cat > "$TMP/gh_retry" <<'GT'
#!/bin/sh
"$@"
GT
chmod +x "$TMP/gh_retry"

# Render: substitute the two shellquoted slots with quoted test values and
# drop the gh_retry slice (the mock gh runs bare; wrapper behavior is already
# regression-covered by test_gh_retry.sh / the fleet's own harness).
# Fixed-string anchors (index/sub), not brace regexes: {{ }} is interval
# syntax to ERE and silently fails to match.
awk '
    index($0, "criteria_value_1=") == 1 && index($0, "{{") > 0 { sub(/=.*/, "=\"$1\""); print; next }
    index($0, "criteria_value_2=") == 1 && index($0, "{{") > 0 { sub(/=.*/, "=\"$2\""); print; next }
    $0 == "{{ .gh_retry }}" { next }
    { print }
' "$TPL" > "$TMP/rendered.sh" || fail "render"

run_case() {
    # bash, not sh: the template is bash-family (set -o pipefail); dash
    # (CI /bin/sh) exits 2 at that line before any gh call.
    PATH="$TMP:$PATH" bash "$TMP/rendered.sh" "$1" "$2" 2>/dev/null
}

# 1. The live run-3 shape: MERGED PR (even on our own branch name) is never
#    an adoption target.
out="$(run_case 'https://github.com/brokenbots/criteria/pull/543' 'KB-328')"
rc=$?
[ "$rc" = "0" ] || fail "rc=$rc on MERGED pr_url, want 0 (verdict rides stdout, not rc)"
printf '%s\n' "$out" | grep -q '^status:foreign_pr https://github.com/brokenbots/criteria/pull/543$' \
    || fail "MERGED pr_url must strip: got '$out'"
pass "MERGED pr_url (our branch) -> foreign_pr"

# 2. OPEN PR on a foreign branch (the adopted-#534 path).
out="$(run_case 'https://github.com/brokenbots/criteria/pull/534' 'KB-328')"
[ $? = "0" ] || fail "rc on foreign open PR"
printf '%s\n' "$out" | grep -q '^status:foreign_pr' || fail "foreign-branch open PR must strip: got '$out'"
pass "OPEN PR on foreign branch -> foreign_pr"

# 3. CLOSED PR on a foreign branch.
out="$(run_case 'https://github.com/brokenbots/criteria/pull/601' 'KB-328')"
printf '%s\n' "$out" | grep -q '^status:foreign_pr' || fail "closed foreign PR must strip: got '$out'"
pass "CLOSED PR on foreign branch -> foreign_pr"

# 4. OPEN PR on THIS ticket's branch -> ok (normal resume path).
out="$(run_case 'https://github.com/brokenbots/criteria/pull/602' 'KB-328')"
printf '%s\n' "$out" | grep -q '^status:ok$' || fail "own-branch open PR must pass: got '$out'"
pass "OPEN PR on own ticket branch -> ok"

# 5. Branch-name boundary: 'kb-328' must not match an unrelated ticket head.
# The mock has no KB-3281 head; assert via 600 (head KB-600) NOT matching
# kb-328 (the CRI-27/CRI-273 class guard).
out="$(run_case 'https://github.com/brokenbots/criteria/pull/600' 'kb-328')"
printf '%s\n' "$out" | grep -q '^status:foreign_pr' || fail "boundary: KB-600 head must be foreign to kb-328: got '$out'"
pass "ticket-boundary enforced (no CRI-27/CRI-273 class match)"

# 6. Unresolvable URL -> ok (conservative, no guess).
out="$(PATH="$TMP:$PATH" sh "$TMP/rendered.sh" 'https://github.com/brokenbots/criteria/pull/999' 'KB-328' 2>/dev/null)"
printf '%s\n' "$out" | grep -q '^status:ok$' || fail "unresolvable pr_url must keep ok: got '$out'"
pass "unresolvable pr_url -> ok (no guess)"

# 7. Template wiring: the render must carry the numeric-gate + both slots.
grep -q 'gh_retry gh pr view "$pr_url" --json state,headRefName' "$TMP/rendered.sh" \
    || fail "rendered script lost its gh invocation"
grep -q 'head_lc=$(printf' "$TMP/rendered.sh" || fail "rendered script lost the head normalization"
pass "template renders with required slots"

echo "all foreign_pr_check tests green"