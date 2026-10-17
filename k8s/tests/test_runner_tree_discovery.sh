#!/usr/bin/env bash
set -euo pipefail

# Regression test for the KB-281 runner genericization (review R1/R2 on this
# branch). The image-mode runner resolves its workflow tree and binds $lockfile
# by scanning the mount; that resolution MUST happen before the first
# adapter_digest/adapter_entry_digests call. The genericization commit briefly
# moved the resolution after the digest publication loop, so under `set -eu`
# every `$lockfile` dereference hit an unbound variable: the digest files were
# written as a bare "sha256:" and the pipeline form masked the failure, letting
# the run continue with no usable adapter digests. This test extracts the
# runner's token-publication through digest-publication span VERBATIM IN
# SOURCE ORDER and replays it against fixture mount layouts, so a reordering
# regression (or any discovery/lockfile-binding breakage) fails here.
#
# Extraction mirrors k8s/tests/test_per_scope_digest_files.sh (CRI-140).

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
RUNNER="$REPO_ROOT/k8s/pod-adapter-runner.sh"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }
pass() { echo "PASS: $*"; }

# --- extract the token->workflow substitution span from the runner -------------
# Span: from the bearer-token publication through tree resolution and the
# digest publication loops, stopping at the workflow_tmp token substitution.
# Taken in source order so a resolution block placed AFTER the digest calls
# reproduces the bug here, not just in production.
# Start the span AFTER the token generation (the runner computes $token on its
# own line first); the token machinery is out of scope for tree discovery and
# everything in the span only writes into $run_dir.
SPAN="$(awk '
    /^# Substitute the placeholders/ { f=0 }
    /printf .* > "\$run_dir\/token"$/ { f=1; next }
    f { print }
' "$RUNNER")"

if ! printf '%s' "$SPAN" | grep -q '^workflow_src=/workflows$'; then
    fail "span extraction lost the workflow_src resolution (KB-281 R1: the resolution block must stay inside the span between token and digest publication)"
fi
if [ ! "$(printf '%s' "$SPAN" | grep -c 'for kind in shell copilot')" -eq 1 ]; then
    fail "span extraction lost the kind-keyed publication loop"
fi
if [ ! "$(printf '%s' "$SPAN" | grep -c 'adapter_entry_digests | while')" -eq 1 ]; then
    fail "span extraction lost the instance-keyed publication pipeline"
fi
if ! printf '%s' "$SPAN" | grep -q 'expected exactly one workflow tree'; then
    fail "span extraction lost the tree_count fail-closed branch"
fi
if printf '%s' "$SPAN" | grep -q 'Publish the runner dial address'; then
    : # the dial publication legitimately sits inside this window
else
    fail "span extraction lost the dial-address publication (window end drifted)"
fi
if ! printf '%s' "$SPAN" | grep -q '> "\$run_dir/host"'; then
    fail "span extraction lost the dial-address host publication"
fi

# R1 ordering assertion: $lockfile must be bound BEFORE the first digest read.
kind_line="$(printf '%s' "$SPAN" | grep -n 'for kind in shell copilot' | cut -d: -f1)"
lock_line="$(printf '%s' "$SPAN" | grep -n '^lockfile=' | cut -d: -f1)"
[ -n "$kind_line" ] || fail "kind publication loop not found in span"
[ -n "$lock_line" ] || fail "lockfile binding not found in span"
[ "$lock_line" -lt "$kind_line" ] \
    || fail "R1 regression: \$lockfile is bound (line $lock_line) AFTER its first digest read (line $kind_line) - the resolution block must sit before the digest publication under set -eu, else the digest files are written as a bare 'sha256:'"

# Repoint the hardcoded /workflows//tmp/workflows assignments at fixture mount
# paths; everything else is replayed verbatim.
SPAN="${SPAN//workflow_src=\/workflows/workflow_src=\"\$FIXTURE_SRC\"}"
SPAN="${SPAN//workflow_tmp=\/tmp\/workflows/workflow_tmp=\"\$FIXTURE_TMP\"}"
if printf '%s' "$SPAN" | grep -q 'workflow_src=/workflows\|workflow_tmp=/tmp/workflows'; then
    fail "fixture repointing left a hardcoded mount assignment in place"
fi

# --- fixture lockfile (same grammar the runner awk consumes) ------------------
SHELL_DIGEST=1111111111111111111111111111111111111111111111111111111111111111
COPILOT_DIGEST=2222222222222222222222222222222222222222222222222222222222222222

write_lockfile() {
    local dir="$1" shell_entry="$2" copilot_entry="$3"
    cat > "$dir/.criteria.lock.hcl" <<EOF
schema_version = 1

adapter "shell" "$shell_entry" {
    reference            = "ghcr.io/brokenbots/criteria-adapter-shell:0.5.3"
    version              = "0.5.3"
    resolved_digest      = "sha256:$SHELL_DIGEST"
}

adapter "copilot" "$copilot_entry" {
    reference            = "ghcr.io/brokenbots/criteria-adapter-copilot:0.5.6"
    version              = "0.5.6"
    resolved_digest      = "sha256:$COPILOT_DIGEST"
}
EOF
}

# --- run_case: replay the span against a fixture mount ------------------------
# $1 = case tag, $2 = fixture workflow_src directory.
run_case() {
    local tag="$1" src="$2"
    mkdir -p "$tmp/workflow_tmp-$tag" "$tmp/discovery-$tag"
    (
        set -eu
        FIXTURE_SRC="$src"
        FIXTURE_TMP="$tmp/workflow_tmp-$tag"
        run_dir="$tmp/discovery-$tag"
        POD_IP="127.0.0.1"
        eval "$SPAN"
        printf 'resolved_tree=%s\nlockfile=%s\n' "$workflow_tree" "$lockfile" \
            > "$tmp/resolved-$tag"
    ) > "$tmp/out-$tag" 2> "$tmp/err-$tag" || echo "$?" > "$tmp/rc-$tag"
}

expect_rc() {
    local tag="$1" want="$2"
    if [ "$want" = "0" ]; then
        [ ! -f "$tmp/rc-$tag" ] || fail "case $tag: expected exit 0, got $(cat "$tmp/rc-$tag")"
    else
        [ -f "$tmp/rc-$tag" ] || fail "case $tag: expected non-zero exit, got success"
        [ "$(cat "$tmp/rc-$tag")" != "0" ] || fail "case $tag: expected non-zero exit, got success"
    fi
}

# --- case 1: happy path (regression guard for R1) -----------------------------
# One qualifying tree (plus a non-qualifying decoy dir without a lockfile).
HAPPY="$tmp/one-tree"
mkdir -p "$HAPPY/tree_a" "$HAPPY/not-a-tree"
write_lockfile "$HAPPY/tree_a" triage triage_reviewer
printf 'not a workflow tree\n' > "$HAPPY/not-a-tree/README"

run_case happy "$HAPPY"
expect_rc happy 0

for kind_digest in "shell:$SHELL_DIGEST" "copilot:$COPILOT_DIGEST"; do
    kind="${kind_digest%%:*}"
    want="sha256:${kind_digest#*:}"
    got="$(cat "$tmp/discovery-happy/digest-$kind" 2>/dev/null || true)"
    [ -n "$got" ] || fail "case happy: digest-$kind is empty or missing (an unbound \$lockfile in the runner reads make this test fail - see review R1)"
    [ "$got" = "$want" ] || fail "case happy: digest-$kind = '$got', want '$want'"
    pass "case happy: digest-$kind matches the lockfile digest"
done

# Entry-keyed publication runs from the same span against the same lockfile.
[ "$(cat "$tmp/discovery-happy/digest-triage")" = "sha256:$SHELL_DIGEST" ] \
    || fail "case happy: digest-triage does not match the lockfile shell entry"
[ "$(cat "$tmp/discovery-happy/digest-triage_reviewer")" = "sha256:$COPILOT_DIGEST" ] \
    || fail "case happy: digest-triage_reviewer does not match the lockfile copilot entry"

grep -q 'resolved_tree=tree_a' "$tmp/resolved-happy" \
    || fail "case happy: workflow_tree did not resolve to the single qualifying tree"
resolved_lock="$(sed -n 's/^lockfile=//p' "$tmp/resolved-happy")"
[ "$resolved_lock" = "$HAPPY/tree_a/.criteria.lock.hcl" ] \
    || fail "case happy: lockfile resolved to '$resolved_lock', want the fixture lockfile path"
[ -f "$resolved_lock" ] || fail "case happy: resolved lockfile path does not exist"
if grep -q 'unbound variable' "$tmp/err-happy"; then
    fail "case happy: runner span produced an unbound-variable error: $(cat "$tmp/err-happy")"
fi
pass "case happy: tree resolution + lockfile binding + digest publication correct"

# --- case 2: zero qualifying trees fails closed --------------------------------
ZERO="$tmp/no-tree"
mkdir -p "$ZERO/empty_tree"
printf 'junk\n' > "$ZERO/empty_tree/README"

run_case zero "$ZERO"
expect_rc zero 1
[ ! -f "$tmp/discovery-zero/digest-shell" ] \
    || fail "case zero: digest files must not be published when no tree qualifies"
grep -q 'expected exactly one workflow tree' "$tmp/err-zero" \
    || fail "case zero: missing fail-closed diagnostic on stderr"
pass "case zero: exits non-zero with the diagnostic on stderr, no digests published"

# --- case 3: two qualifying trees fails closed ---------------------------------
TWO="$tmp/two-tree"
mkdir -p "$TWO/tree_a" "$TWO/tree_b"
write_lockfile "$TWO/tree_a" triage triage_reviewer
write_lockfile "$TWO/tree_b" develop develop_reviewer

run_case two "$TWO"
expect_rc two 1
[ ! -f "$tmp/discovery-two/digest-shell" ] \
    || fail "case two: digest files must not be published when the tree count is ambiguous"
grep -q 'expected exactly one workflow tree' "$tmp/err-two" \
    || fail "case two: missing fail-closed diagnostic on stderr"
pass "case two: exits non-zero with the diagnostic on stderr, no digests published"

echo "test_runner_tree_discovery: all cases passed"
