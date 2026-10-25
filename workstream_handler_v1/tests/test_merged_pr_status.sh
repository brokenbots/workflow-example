#!/usr/bin/env bash
# Regression tests for workstream_handler_v1/scripts/merged_pr_status.sh.tftpl
# (KB-328 merged-PR recovery). Bare-sh compatible: no bashisms, no `local`.
# Hermetic: gh is mocked in PATH, so the suite runs green in fleet CI with no
# GitHub auth. The live shape (gh pr view KB-328 -> MERGED #543 with a deleted
# head branch) was verified once by hand on the host; the mock pins that shape.
# Usage: ./workstream_handler_v1/tests/test_merged_pr_status.sh
set -u
TREE_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TPL="$TREE_ROOT/scripts/merged_pr_status.sh.tftpl"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

fail() { echo "FAIL: $1"; exit 1; }
pass() { echo "PASS: $1"; }

# Mock gh: handles exactly the one call the template makes.
cat > "$TMP/gh" <<'MOCK'
#!/usr/bin/env bash
case "$*" in
    *"--json number,state,url,mergeCommit"*)
        case "$*" in
            *"KB-328"*)
                printf '{"number":543,"state":"MERGED","url":"https://github.com/brokenbots/criteria/pull/543","mergeCommit":{"oid":"8909c4097948685230f0793c92d266b3c78d2acf"}}'
                ;;
            *"KB-open"*)
                printf '{"number":534,"state":"OPEN","url":"https://github.com/brokenbots/criteria/pull/534","mergeCommit":null}'
                ;;
            *)
                exit 1
                ;;
        esac
        ;;
    *) exit 64 ;;
esac
MOCK
chmod +x "$TMP/gh"

RENDERED="$TMP/merged_pr_status.sh"
# Render: substitute the templatefile slots by hand (the fleet test cannot
# compile HCL). The slot lines are matched with fixed-string probes and the
# branches rewritten under a different delimiter to survive the '|' inside
# the {{ .criteria_value_1 | shellquote }} slot itself.
awk '
    { gsub(/\r$/, "") }
    /^criteria_value_1=\{\{ \.criteria_value_1 \| shellquote \}\}$/ { print "criteria_value_1=\"$1\""; next }
    /^\{\{ \.gh_retry \}\}$/ { next }
    { print }
' "$TPL" > "$RENDERED" || fail "render template"
chmod +x "$RENDERED"

# 1. Positive arm: MERGED PR -> rc=0, marker + status + protocol fields.
# bash, not sh: the template carries `set -o pipefail` like every sibling —
# under dash (CI's /bin/sh) that line exits 2 before any gh call.
out=$(PATH="$TMP:$PATH" bash "$RENDERED" KB-328 2>/dev/null); rc=$?
[ "$rc" = "0" ] || fail "rc=$rc on MERGED PR, want 0"
printf '%s\n' "$out" | grep -q '^marker=merged_recovery$' || fail "missing marker line"
printf '%s\n' "$out" | grep -q '^status:merged$' || fail "missing status:merged"
printf '%s\n' "$out" | grep -q '^pr_number=543 pr_url=https://github.com/brokenbots/criteria/pull/543 merge_sha=8909c4097948685230f0793c92d266b3c78d2acf$' \
    || fail "field line malformed: got: $out"
pass "MERGED PR emits marker + status + full field line"

# 2. Negative arm: OPEN PR -> rc=2, no marker (caller falls through).
uout=$(PATH="$TMP:$PATH" bash "$RENDERED" KB-open 2>/dev/null); urc=$?
[ "$urc" = "2" ] || fail "rc=$urc on OPEN PR, want 2"
printf '%s\n' "$uout" | grep -q 'marker=' && fail "marker leaked on OPEN PR"
pass "OPEN PR exits 2 without marker"

# 3. Negative arm: gh finds nothing -> rc=2, no marker.
nout=$(PATH="$TMP:$PATH" bash "$RENDERED" KB-ghost 2>/dev/null); nrc=$?
[ "$nrc" = "2" ] || fail "rc=$nrc on unknown branch, want 2"
printf '%s\n' "$nout" | grep -q 'marker=' && fail "marker leaked on unknown branch"
pass "unknown branch exits 2 without marker"

# 4. Template renders with the required slots intact.
grep -q 'gh pr view "$branch" --json number,state,url,mergeCommit' "$RENDERED" \
    || fail "rendered script lost its gh invocation"
grep -q 'branch="$criteria_value_1"' "$RENDERED" || fail "render lost the branch binding"
pass "template renders with required slots"

echo "all merged_pr_status tests green"