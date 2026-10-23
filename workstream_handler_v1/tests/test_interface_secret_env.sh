#!/usr/bin/env bash
set -euo pipefail

# Regression tests for the machine-readable interface contract (ADR 307,
# KB-310) after KB-304 removed the Linear estate from this tree. The
# interface is the admission surface of record: a secretEnv lane the
# workflow's variables do not declare (declared, secret) makes the operator
# demand a credential nothing provides, and an orphaned lockfile pin for an
# adapter the tree cannot use survives every doc-level check. Both drift
# classes slipped through test_notes_pins.sh, which only compares the lock
# against NOTES.md. This test ties the three sources of truth together:
#
#   1. every interface.json secretEnv entry maps to a variable in
#      variables.chcl declared with secret = true, and every secret-typed
#      variable is surfaced as a secretEnv lane;
#   2. every interface.json adapter kind has at least one adapter block of
#      that kind in adapters.chcl;
#   3. the .criteria.lock.hcl pin set (adapter kind/name) is exactly the
#      adapters.chcl block set — no orphaned pins, no unpinned adapters.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TREE_ROOT="${SCRIPT_DIR}/.."
FAILED=0

ok() {
    echo "ok: $1"
}

fail() {
    FAILED=$((FAILED + 1))
    echo "FAIL: $1" >&2
}

command -v node >/dev/null 2>&1 || { echo "FAIL: node is required" >&2; exit 1; }
command -v awk >/dev/null 2>&1 || { echo "FAIL: awk is required" >&2; exit 1; }

INTERFACE="${TREE_ROOT}/interface.json"
VARIABLES="${TREE_ROOT}/variables.chcl"
ADAPTERS="${TREE_ROOT}/adapters.chcl"
LOCK="${TREE_ROOT}/.criteria.lock.hcl"

for f in "$INTERFACE" "$VARIABLES" "$ADAPTERS" "$LOCK"; do
    [ -f "$f" ] || { echo "FAIL: missing $f" >&2; exit 1; }
done

# interface.json must parse and name this tree.
node -e "
const fs = require('fs');
const i = JSON.parse(fs.readFileSync(process.argv[1], 'utf8'));
if (i.kind !== 'WorkflowInterface') { console.error('interface kind = ' + i.kind); process.exit(1); }
if (i.workflow !== 'workstream_handler_v1') { console.error('interface workflow = ' + i.workflow); process.exit(1); }
for (const e of (i.secretEnv || [])) {
    if (!/^[A-Z][A-Z0-9_]*\$/.test(e)) { console.error('secretEnv entry ' + e + ' violates the interface grammar'); process.exit(1); }
}
console.log((i.secretEnv || []).join('\n'));
" "$INTERFACE" > /tmp/kb304_secret_env.$$
[ $? -eq 0 ] || { rm -f /tmp/kb304_secret_env.$$; fail "interface.json is not a valid workstream_handler_v1 interface"; exit 1; }
secret_env="$(cat /tmp/kb304_secret_env.$$)"
rm -f /tmp/kb304_secret_env.$$
ok "interface.json parses (secretEnv: $(echo "$secret_env" | tr '\n' ' '))"

# variable "\t> is_declared "name" in the variables root with secret = true. Prints 1/0.
is_secret_variable() {
    awk -F'"' -v want="$1" '
        /^variable "/ { inb = 1; name = $2; secret = 0 }
        inb && /^[ \t]*secret[ \t]*=[ \t]*true[ \t]*$/ { secret = 1 }
        /^\}/ && inb { if (name == want) { print secret; exit } inb = 0 }
    ' "$VARIABLES"
}

if [ -z "$secret_env" ]; then
    fail "interface.json declares no secretEnv lanes (the GitHub identities went missing?)" || true
fi
for entry in $secret_env; do
    var="$(echo "$entry" | tr 'A-Z' 'a-z')"
    secret="$(is_secret_variable "$var" || true)"
    if [ "$secret" = "1" ]; then
        ok "secretEnv $entry maps to secret-typed variable $var"
    else
        fail "secretEnv lane $entry has no variable \"$var\" declared with secret = true in variables.chcl (admission would demand a credential nothing provides, KB-304 regression class)"
    fi
done

# Reverse direction: every secret-typed variable must be surfaced as a lane.
secret_vars="$(awk -F'"' '
    /^variable "/ { inb = 1; name = $2; secret = 0 }
    inb && /^[ \t]*secret[ \t]*=[ \t]*true[ \t]*$/ { secret = 1 }
    /^\}/ && inb { if (secret == 1) print name; inb = 0 }
' "$VARIABLES")"
for var in $secret_vars; do
    env_name="$(echo "$var" | tr 'a-z' 'A-Z')"
    if printf '%s\n' "$secret_env" | grep -Fxq "$env_name"; then
        ok "secret variable $var surfaced as secretEnv $env_name"
    else
        fail "variables.chcl declares secret variable \"$var\" but interface.json has no $env_name lane (the credential would reach the tree without admission declaring it)"
    fi
done

# ── 2. interface adapters vs live adapter blocks ────────────────────────────
interface_kinds="$(node -e "
const fs = require('fs');
const i = JSON.parse(fs.readFileSync(process.argv[1], 'utf8'));
console.log((i.adapters || []).join('\n'));
" "$INTERFACE")"
if [ -z "$interface_kinds" ]; then
    fail "interface.json declares no adapters"
fi
for kind in $interface_kinds; do
    if grep -Eq "^adapter \"$kind\" " "$ADAPTERS"; then
        ok "interface adapter kind $kind has an adapters.chcl block"
    else
        fail "interface.json adapter kind $kind has no adapter block in adapters.chcl"
    fi
done

# ── 3. lockfile pins vs adapters.chcl blocks (kind/name pairs) ──────────────
lock_pairs="$(awk -F'"' '/^adapter "/ { print $2 "\t" $4 }' "$LOCK" | sort)"
live_pairs="$(awk -F'"' '/^adapter "/ { print $2 "\t" $4 }' "$ADAPTERS" | sort)"
[ -n "$lock_pairs" ] || fail "could not parse any adapter block from $LOCK"
[ -n "$live_pairs" ] || fail "could not parse any adapter block from $ADAPTERS"

orphans="$(comm -23 <(printf '%s\n' "$lock_pairs") <(printf '%s\n' "$live_pairs") || true)"
if [ -z "$orphans" ]; then
    ok "no orphaned lockfile pins (every lock adapter exists in adapters.chcl)"
else
    fail "lockfile pins for adapters the tree does not declare (KB-304 zombie class): $(echo "$orphans" | tr '\t' '/')"
fi
unpinned="$(comm -13 <(printf '%s\n' "$lock_pairs") <(printf '%s\n' "$live_pairs") || true)"
if [ -z "$unpinned" ]; then
    ok "every adapter block is pinned in the lockfile"
else
    fail "adapters.chcl blocks without a lockfile pin: $(echo "$unpinned" | tr '\t' '/')"
fi

if [ "$FAILED" -ne 0 ]; then
    echo "test_interface_secret_env: $FAILED failure(s)" >&2
    exit 1
fi
echo "test_interface_secret_env: all checks passed"