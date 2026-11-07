#!/usr/bin/env bash
set -euo pipefail

# KB-385 probe regression: the --var secret binding contract between the
# operator's CRITERIA_SECRET_VARS pairs and the tree's lowercase secret
# variables.
#
# The operator contract (pinned in secretVarBindings and
# docs/jobs-setup.md): a secret's env map keys are SCREAMING binding env
# lane names (e.g. WORKFLOW_UPSTREAM_TOKEN), the values are the rendered
# secret file keys inside the CSI mount, and the derived --var pair is
# lower(lane)=file:<mountPath>/<fileKey>. The engine binds --var overrides
# by exact name, so the pair name must be the tree's lowercase secret var
# (KB-304 name parity) — a screaming-keyed pair never resolves.
#
# Pinned here against the live engine, via the probe fixture tree
# (fixtures/probe_secret_binding — resolution-only, no adapter needed):
#   1. the probe tree validates standalone;
#   2. the interface parity (KB-304) holds: the lane is declared in the
#      handler interface's secretEnv, the tree declares the lowercase var,
#      and docs/jobs-setup.md carries the contract as written;
#   3. lowering a screaming lane binding a file under a DECLARED secret
#      root RESOLVES: the run completes green and the engine reports the
#      var as an override;
#   4. a file: origin OUTSIDE every declared root fails LOUDLY with a typed
#      reason that names the bound variable (typed-reason convention);
#   5. a SCREAMING-keyed pair NEVER resolves into the lowercase tree var.
# The derivation itself (CRITERIA_SECRET_VARS -> --var argv) is pinned by
# the jobbuilder contract tests (criteria-k8s/internal/jobbuilder).
#
# Engine floor: legs 3-5 pin KB-369 engine behavior (multi-root secret-file
# containment, the CRITERIA_SECRET_FILE_ROOTS flag, and the typed
# `variable "<name>" (origin file:` failure reason) that criteria only
# carries from v0.5.58. The CI engine pin (v0.5.52-rc4,
# .github/workflows/ci.yml) predates that floor: there, legs 3-5 SKIP with
# a typed skip line (suite convention: k8s/tests/test_workflow_graphs_flat.sh)
# while the version-free legs (1, 2, 2b) still run and stay green. The skip
# is fail-closed: an engine that reports no version skips the live-engine
# legs too.

TREE_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
FIXTURE="$TREE_ROOT/tests/fixtures/probe_secret_binding"
CRITERIA="${CRITERIA_BIN:-criteria}"

FAILED=0

fail() {
    echo "FAIL: $1" >&2
    FAILED=$((FAILED + 1))
}

ok() {
    echo "ok: $1"
}

require_contains() {
    if grep -q -- "$2" <<<"$1"; then
        ok "$3"
    else
        fail "$3: missing '$2'"
    fi
}

# ── 1. the probe tree validates standalone ───────────────────────────────────
if "$CRITERIA" validate "$FIXTURE" >/dev/null 2>&1; then
    ok "probe_secret_binding validates standalone"
else
    fail "criteria validate probe_secret_binding"
fi

# ── 2. interface parity (KB-304) ─────────────────────────────────────────────
# The workflow_upstream_token tree var is the interface-parity carrier of
# the WORKFLOW_UPSTREAM_TOKEN secretEnv lane; the contract binds it the
# WORKFLOW_GITHUB_TOKEN lane's rendered file.
grep -q 'variable "workflow_upstream_token"' "$FIXTURE/variables.chcl" \
    && ok "probe tree declares the lowercase workflow_upstream_token var" \
    || fail "probe tree must declare the lowercase workflow_upstream_token var"
jq -e --arg lane "WORKFLOW_UPSTREAM_TOKEN" 'any(.secretEnv[]; . == $lane)' \
    "$TREE_ROOT/interface.json" >/dev/null 2>&1 \
    && ok "interface declares the WORKFLOW_UPSTREAM_TOKEN secretEnv lane" \
    || fail "interface.json must declare the WORKFLOW_UPSTREAM_TOKEN secretEnv lane"

# ── 2b. the contract is documented ───────────────────────────────────────────
DOCS="$(cd "$TREE_ROOT/.." && pwd)/docs/jobs-setup.md"
grep -q "lower(env lane)=file:<mountPath>/<fileKey>" "$DOCS" \
    && ok "docs/jobs-setup.md pins the KB-385 binding contract" \
    || fail "docs/jobs-setup.md must pin the KB-385 binding contract"

# ── engine-version gate (legs 3-5) ───────────────────────────────────────────
# The gate compares `criteria version` against the KB-369 floor (v0.5.58)
# with sort -V, so pre-release pins (v0.5.52-rc4) order below it.
ENGINE_FLOOR="v0.5.58"
if command -v "$CRITERIA" >/dev/null 2>&1; then
    ENGINE_VERSION="$("$CRITERIA" version 2>/dev/null | head -n 1 | tr -d '[:space:]' || true)"
else
    ENGINE_VERSION=""
fi
if [ -n "$ENGINE_VERSION" ] \
    && [ "$(printf '%s\n%s\n' "$ENGINE_VERSION" "$ENGINE_FLOOR" | sort -V | head -n 1)" = "$ENGINE_FLOOR" ]; then
    echo "engine: criteria $ENGINE_VERSION (>= $ENGINE_FLOOR: KB-369 legs 3-5 run)"
else
    # Fail-closed skip: an engine below the floor cannot express the
    # multi-root containment these legs pin, and an unparseable version
    # must not unskip them by accident.
    if [ "$FAILED" -ne 0 ]; then
        echo "test_secret_binding: $FAILED leg(s) failed" >&2
        exit 1
    fi
    echo "skip: legs 3-5 (live-engine KB-369 legs) need criteria >= $ENGINE_FLOOR; engine is '${ENGINE_VERSION:-unknown}'"
    echo "test_secret_binding: legs 1-2b green; legs 3-5 skipped (engine floor $ENGINE_FLOOR)"
    exit 0
fi

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

SECRETS="$TMP/secrets"
mkdir -p "$SECRETS" "$TMP/home"
# Sentinel values must not overlap any varname: the engine redacts the
# printed var name where it matches the value, and the probe asserts the
# literal var override lines.
printf 'a3f58c2e91b7d640\n' > "$SECRETS/workflow_github_token"
printf '8d1e47c03b56a2f9\n' > "$SECRETS/reviewer_github_token"

apply_probe() { # (args...) -> stdout+stderr on stdout; sets APPLY_EXIT
    local out
    if out="$(CRITERIA_HOME="$TMP/home" \
        CRITERIA_SECRET_FILE_ROOTS="$SECRETS" \
        "$CRITERIA" apply "$FIXTURE" --ui=false --output concise "$@" 2>&1)"; then
        APPLY_EXIT=0
    else
        APPLY_EXIT=$?
    fi
    APPLY_OUT="$out"
    return 0
}

# ── 3. lowered binding resolves green ────────────────────────────────────────
# The pair set is exactly what the fixed CRITERIA_SECRET_VARS derivation
# emits for the live triple-lane binding: classic lanes plus the upstream
# lane reusing the workflow_github_token file.
apply_probe \
    --var reviewer_github_token="file:$SECRETS/reviewer_github_token" \
    --var workflow_github_token="file:$SECRETS/workflow_github_token" \
    --var workflow_upstream_token="file:$SECRETS/workflow_github_token"
[ "$APPLY_EXIT" -eq 0 ] || fail "lowered binding must resolve green (exit $APPLY_EXIT)"
require_contains "$APPLY_OUT" "run completed" "lowered binding run completes"
require_contains "$APPLY_OUT" "var workflow_upstream_token=(sensitive) (override)" \
    "the engine resolves the lowercase workflow_upstream_token var"

# ── 4. a file: origin outside every declared root fails loudly ──────────────
# KB-369 confinement is the engine's typed gate: the failure names the
# variable the pair bound INTO, so a missing/escaping origin can never
# fail silently.
apply_probe --var workflow_upstream_token="file:$TMP/outside/wf_token"
if [ "$APPLY_EXIT" -eq 0 ]; then
    fail "out-of-root file: origin must fail loudly"
else
    ok "out-of-root file: origin fails with a non-zero exit"
fi
require_contains "$APPLY_OUT" 'variable "workflow_upstream_token" (origin file:' \
    "the typed failure names the bound variable and its origin"

# ── 5. a screaming-keyed pair never resolves ────────────────────────────────
# The ignored pair is not a crash: unbound secret vars do not gate
# apply-time resolution on a resolution-only tree, so the run still
# completes green — it just never gains the override. Asserting the green
# completion keeps this leg meaningful (it would catch an engine that
# crashes on unknown overrides) while the absence of the override line
# pins the exact-name lookup.
apply_probe --var WORKFLOW_UPSTREAM_TOKEN="file:$SECRETS/workflow_github_token"
[ "$APPLY_EXIT" -eq 0 ] \
    || fail "the screaming-keyed run must complete (unknown overrides are ignored, not fatal; exit $APPLY_EXIT)"
require_contains "$APPLY_OUT" "run completed" "the screaming-keyed run completes green"
if grep -q "var workflow_upstream_token=(sensitive) (override)" <<<"$APPLY_OUT"; then
    fail "a screaming-keyed pair must never bind into the lowercase tree var"
else
    ok "the screaming-keyed pair does not resolve into the tree var"
fi

if [ "$FAILED" -eq 0 ]; then
    echo "test_secret_binding: all legs green"
else
    echo "test_secret_binding: $FAILED leg(s) failed" >&2
    exit 1
fi
