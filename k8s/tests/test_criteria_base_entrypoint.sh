#!/usr/bin/env bash
set -euo pipefail

# Behavioral regression test for the criteria-base entrypoint (CRI-230, M6.1).
#
# Runs criteria-base/entrypoint.sh against a stub criteria binary and proves:
#
#   - WORKFLOW_URL + forwarded argv are passed to `criteria apply` in order;
#   - WORKFLOW_REF is enforced via --workflow-ref (CRI-226) only when declared,
#     and whitespace-only refs are treated as absent;
#   - empty/whitespace WORKFLOW_URL fails closed (exit 64) without invoking
#     criteria: the image ships no baked /workflows tree (D2);
#   - an unusable CRITERIA_HOME fails closed (exit 70) without invoking
#     criteria;
#   - the criteria exit status propagates (exec semantics).

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
ENTRYPOINT="$REPO_ROOT/criteria-base/entrypoint.sh"

fail() {
    echo "FAIL: $1" >&2
    exit 1
}

[ -f "$ENTRYPOINT" ] || fail "criteria-base/entrypoint.sh missing"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# Stub criteria binary: records its argv and the effective CRITERIA_HOME, then
# exits with STUB_EXIT (default 0).
cat >"$tmp/stub-criteria" <<'EOF'
#!/usr/bin/env bash
{
    printf 'argv:'
    for a in "$@"; do
        printf ' [%s]' "$a"
    done
    printf '\n'
    printf 'criteria_home=%s\n' "${CRITERIA_HOME-}"
} >>"$STUB_LOG"
exit "${STUB_EXIT:-0}"
EOF
chmod +x "$tmp/stub-criteria"

log="$tmp/stub.log"
: >"$log"

url="git::https://example.com/org/workflows.git?ref=main"
ref="0123456789abcdef0123456789abcdef01234567"

# run_entrypoint CRITERIA_HOME WORKFLOW_URL [WORKFLOW_REF] [forwarded args...]
run_entrypoint() {
    local home="$1" wf_url="$2" wf_ref="${3-}"
    (
        export WORKFLOW_URL="$wf_url" WORKFLOW_REF="$wf_ref"
        export STUB_LOG="$log" CRITERIA_BIN="$tmp/stub-criteria" CRITERIA_HOME="$home"
        exec "$ENTRYPOINT" "${@:4}"
    )
}

last_argv() {
    grep '^argv:' "$log" | tail -1
}

# --- happy path: URL, forwarded args, route ref -----------------------------
: >"$log"
run_entrypoint "$tmp/home" "$url" "$ref" --var x=1
last_argv | grep -qF "argv: [apply] [$url] [--var] [x=1] [--workflow-ref] [$ref]" || \
    fail "apply invoked with wrong arguments: $(last_argv)"
grep -qF "criteria_home=$tmp/home" "$log" || \
    fail "criteria must run with CRITERIA_HOME exported to the entrypoint value"

# --- WORKFLOW_REF unset: no pin declared ------------------------------------
: >"$log"
run_entrypoint "$tmp/home" "$url"
last_argv | grep -qF "argv: [apply] [$url]" || \
    fail "apply invoked with wrong arguments: $(last_argv)"
if grep -q -- '--workflow-ref' "$log"; then
    fail "--workflow-ref must not be passed when WORKFLOW_REF is undeclared"
fi

# --- WORKFLOW_REF whitespace-only: treated as absent ------------------------
: >"$log"
run_entrypoint "$tmp/home" "$url" "   "
last_argv | grep -qF "argv: [apply] [$url]" || \
    fail "whitespace WORKFLOW_REF must be treated as absent: $(last_argv)"
if grep -q -- '--workflow-ref' "$log"; then
    fail "--workflow-ref must not be passed for whitespace-only WORKFLOW_REF"
fi

# --- empty WORKFLOW_URL: fail closed, criteria never invoked ----------------
for bad_url in "" "   " "$(printf '\t')"; do
    : >"$log"
    if out="$(run_entrypoint "$tmp/home" "$bad_url" 2>&1)"; then
        fail "empty WORKFLOW_URL must fail closed, got success: $out"
    fi
    if [ -s "$log" ]; then
        fail "criteria must not be invoked without WORKFLOW_URL: $(last_argv)"
    fi
    printf '%s' "$out" | grep -q "WORKFLOW_URL is not set" || \
        fail "empty WORKFLOW_URL failure not reported: $out"
    printf '%s' "$out" | grep -q "no baked /workflows tree" || \
        fail "failure must explain there is no baked tree to fall back to: $out"
done

# --- unusable CRITERIA_HOME: fail closed before any fetch -------------------
# A path whose parent is a regular file cannot be created by any uid.
bad_home="$tmp/not-a-dir/sub"
echo data >"$tmp/not-a-dir"
: >"$log"
if out="$(WORKFLOW_URL="$url" CRITERIA_HOME="$bad_home" \
        STUB_LOG="$log" CRITERIA_BIN="$tmp/stub-criteria" \
        "$ENTRYPOINT" 2>&1)"; then
    fail "non-directory CRITERIA_HOME must fail closed, got success: $out"
fi
if [ -s "$log" ]; then
    fail "criteria must not be invoked when CRITERIA_HOME is unusable: $(last_argv)"
fi
printf '%s' "$out" | grep -q "not a directory writable" || \
    fail "unusable CRITERIA_HOME failure not reported: $out"

: >"$log"
if out="$(WORKFLOW_URL="$url" CRITERIA_HOME="$tmp/not-a-dir/other-sub" \
        STUB_LOG="$log" CRITERIA_BIN="$tmp/stub-criteria" \
        "$ENTRYPOINT" 2>&1)"; then
    fail "uncreatable CRITERIA_HOME must fail closed, got success: $out"
fi
printf '%s' "$out" | grep -q "not a directory writable" || \
    fail "uncreatable CRITERIA_HOME failure not reported: $out"

# --- creatable CRITERIA_HOME: entrypoint creates it (fresh PVC semantics) ---
: >"$log"
mkdir_parent="$tmp/created"
if out="$(WORKFLOW_URL="$url" CRITERIA_HOME="$mkdir_parent/deep/home" \
        STUB_LOG="$log" CRITERIA_BIN="$tmp/stub-criteria" \
        "$ENTRYPOINT" 2>&1)"; then
    grep -qF "criteria_home=$mkdir_parent/deep/home" "$log" || \
        fail "entrypoint must create and export CRITERIA_HOME: $(tail -2 "$log")"
else
    fail "entrypoint must create a missing but creatable CRITERIA_HOME: $out"
fi

# --- criteria exit status propagates (exec) ---------------------------------
status="$(WORKFLOW_URL="$url" CRITERIA_HOME="$tmp/home" STUB_EXIT=42 \
    STUB_LOG="$log" CRITERIA_BIN="$tmp/stub-criteria" \
    "$ENTRYPOINT" >/dev/null 2>&1; echo $?)"
[ "$status" -eq 42 ] || fail "expected criteria exit 42 to propagate, got $status"

echo "PASS: criteria-base entrypoint behavior (CRI-230)"