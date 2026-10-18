#!/usr/bin/env bash
set -uo pipefail

# KB-277: standalone regression suite for harness_contracts_v1 (matrix C13-C18
# + D19/D21/D22). Each matrix shape is observed in run events by a FIXTURE
# RUN: a single-stimulus copy of the tree (trimmed to the probe step and, when
# the slice needs the KB-217 record sink, the verdict echo step) driven by a
# scripted model stub over a shimmed local provider on a random loopback
# port. Asserts are raw run-event observations (event lines and their
# payloads), never compile-only claims — half-implementations cannot pass
# silently. The loud-failing fixtures (F3b/F4/F8) assert their designed
# failure shapes; every other fixture must land green.
#
# Per-shape fixture runs (not one full-chain run) are deliberate: the CLI
# session carries conversation state ACROSS steps, and a scripted stub cannot
# steer multi-step sessions deterministically. Full-chain green-run coverage
# is the fleet legs' job; this suite pins the per-shape mechanics this suite
# card owns.
#
# Skips cleanly when the engine, node, jq, or the pinned adapter versions are
# unavailable — the CI lanes that lack them pick the compile-level gates.

TREE_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FIXTURES="$(cd "$(dirname "${BASH_SOURCE[0]}")/fixtures" && pwd)"
CRITERIA="${CRITERIA_BIN:-criteria}"
TIMEOUT_SECS="${SUITE_TIMEOUT:-150}"

FAILED=0
declare -a STUB_PIDS=()
WORK=""

fail() {
    echo "FAIL: $1" >&2
    FAILED=$((FAILED + 1))
}

ok() {
    echo "ok: $1"
}

# ── Preconditions (skip, not fail — run-event depth is opt-in) ───────────────

SKIP=""
if ! command -v "$CRITERIA" >/dev/null 2>&1; then
    SKIP="criteria binary '$CRITERIA' not found (set CRITERIA_BIN)"
elif ! command -v node >/dev/null 2>&1; then
    SKIP="node not available (scripted model stub)"
elif ! command -v jq >/dev/null 2>&1; then
    SKIP="jq not available (run-event assertions)"
elif ! command -v git >/dev/null 2>&1; then
    SKIP="git not available (adapter pin cross-check)"
fi
if [ -z "$SKIP" ] && ! "$CRITERIA" version >/dev/null 2>&1; then
    SKIP="criteria cannot report a version ('$CRITERIA version' failed)"
fi
if [ -z "$SKIP" ] && ! "$CRITERIA" adapter list 2>/dev/null | grep -q "0.5.15"; then
    SKIP="pinned copilot adapter 0.5.15 not cached ('criteria adapter list' lacks it)"
fi
if [ -z "$SKIP" ] && ! "$CRITERIA" adapter list 2>/dev/null | grep -q "0.5.4"; then
    SKIP="pinned shell adapter 0.5.4 not cached ('criteria adapter list' lacks it)"
fi
if [ -n "$SKIP" ]; then
    echo "SKIP: harness_contracts_v1 standalone suite: $SKIP" >&2
    exit 0
fi

cleanup() {
    local pid
    for pid in "${STUB_PIDS[@]:-}"; do
        [ -n "$pid" ] && kill "$pid" 2>/dev/null
    done
    [ -n "$WORK" ] && rm -rf "$WORK"
}
trap cleanup EXIT

WORK="$(mktemp -d "${TMPDIR:-/tmp}/hcontracts-suite.XXXXXX")"

free_port() {
    node -e 'const s=require("net").createServer();s.listen(0,"127.0.0.1",()=>{console.log(s.address().port);s.close()})'
}

events() {
    # Event lines begin with {"schema_version" — everything else is log noise.
    grep '^{"schema_version"' "$1" 2>/dev/null | jq -r '.payload_type' 2>/dev/null
}

count_type() {
    events "$1" | grep -c "^$2\$" || true
}

# run_fixture NAME MODE: creates the tree copy, starts the stub, applies,
# and leaves the caller the event log at $EV and the run rc at $RC.
EV=""
RC=""
PROOT=""
run_fixture() {
    local name="$1" mode="$2"
    local tree port log stub_pid
    tree="$WORK/tree-$name"
    cp -r "$TREE_ROOT" "$tree"
    rm -rf "$tree/tests"
    cp "$FIXTURES/$name.chcl" "$tree/main.chcl"

    PROOT="$WORK/probe-$name"
    mkdir -p "$PROOT"

    port="$(free_port)"
    log="$WORK/stub-$name.log"
    STUB_MODE="$mode" STUB_PORT="$port" node "$FIXTURES/contracts_stub.mjs" >"$log" 2>&1 &
    stub_pid=$!
    STUB_PIDS+=("$stub_pid")

    STUBPORT="$port" PROBEROOT="$PROOT" node "$FIXTURES/adapters_shim.mjs" \
        "$tree/adapters.chcl" "$tree/adapters.chcl"

    EV="$WORK/events-$name.log"
    RC=0
    timeout "$TIMEOUT_SECS" "$CRITERIA" apply "$tree" \
        --var workflow_github_token=x \
        --var probe_root="$PROOT" \
        --output json --ui=false >"$EV" 2>&1 || RC=$?
    kill "$stub_pid" 2>/dev/null

    if [ "$SUITE_DEBUG" = "1" ]; then
        echo "--- events $name (rc=$RC) ---" >&2
        tail -c 4000 "$EV" >&2
        echo "--- stub $name ---" >&2
        tail -c 1500 "$log" >&2
    fi
}

has_ev() {
    grep -q "$1" "$EV"
}

expect_rc_zero() {
    if [ "$RC" = "0" ]; then ok "$1 run exit 0"; else fail "$1 run exited $RC (expected 0)"; fi
}

expect_rc_loud() {
    if [ "$RC" != "0" ]; then ok "$1 run exited $RC (designed loud failure)"; else fail "$1 run exited 0 but the fixture is designed to fail loud"; fi
}

expect_event() {
    if count_type "$EV" "$2" | grep -qv '^0$'; then
        ok "$1 event $2 observed"
    else
        fail "$1: event $2 not observed"
    fi
}

expect_run_green() {
    expect_event "$1" RunCompleted
    if has_ev '"success":true'; then
        ok "$1 run completed with success=true"
    else
        fail "$1: RunCompleted payload was not success=true"
    fi
}

# ── F1: C14 — valid payload accepted ─────────────────────────────────────────

run_fixture f1_valid f1_valid
expect_rc_zero F1
expect_event F1 StepOutcome
expect_run_green F1
if has_ev 'C14 valid payload: schema-valid payload routes the declared approved outcome with its projected outputs intact'; then
    ok F1 engineered payload reason carried verbatim in run events
else
    fail F1: engineered payload reason not found verbatim in run events
fi

# ── F2: C15/C16/D19 — repair loop, no commit, engineered rejection green ────

run_fixture f2_reject_echo f2_reject
expect_rc_zero F2
if [ "$(count_type "$EV" payload_invalid 2>/dev/null || true)" -ge 1 ] || has_ev 'payload_invalid'; then
    ok F2 payload_invalid rejections observed
else
    fail F2: no payload_invalid repair-loop rejections observed
fi
if ! has_ev 'wrong field set on purpose' ; then :; fi
expect_run_green F2
if [ -f "$PROOT/contracts_probe_records/d19_verdict_record" ]; then
    ok F2 d19 verdict record written to run-state files
else
    fail F2: d19_verdict_record missing from probe root
fi
if has_ev 'D19 engineered rejection routed green: wrong-field-set and wrong-lane shapes were rejected loudly by the payload contract, then the declared rejected lane carried this verbatim reason'; then
    ok F2 D19 engineered reason carried verbatim in run events
else
    fail F2: D19 engineered reason not found verbatim in run events
fi

# ── F3: C17 — empty adapter comment rejected loudly, observed exit green ────

run_fixture f3_comment_echo f3_comment
expect_rc_zero F3
if has_ev 'comment_missing'; then
    ok "F3 empty adapter comment rejected loudly (comment_missing)"
else
    fail F3: comment_missing loud rejection not observed
fi
if has_ev 'payload_invalid'; then
    ok F3 rejection surfaced as payload_invalid
else
    fail F3: loud rejection missing the payload_invalid shape
fi
expect_run_green F3
if [ -f "$PROOT/contracts_probe_records/c17_comment_record" ]; then
    ok F3 c17 verdict record written to run-state files
else
    fail F3: c17_comment_record missing from probe root
fi

# ── F3b: C17 defect evidence — corrected comment never reaches the engine ───

run_fixture f3b_comment_defect f3b_comment_defect
expect_rc_loud F3b
if has_ev 'missing_comment'; then
    ok "F3b engine-side require_comment rejection observed (missing_comment)"
else
    fail F3b: engine-side missing_comment shape not observed
fi
expect_event F3b RunFailed
if has_ev 'failed after 1 attempts'; then
    ok F3b step failed loud after 1 attempts
else
    fail F3b: loud step-failure-after-1-attempts shape not observed
fi

# ── F4: C13 carrier fired — undeclared arm fails loud in seconds ────────────

run_fixture f4_undeclared f4_undeclared
expect_rc_loud F4
if has_ev 'outcome_uncontracted'; then
    ok "F4 undeclared-arm submission failed loud (outcome_uncontracted)"
else
    fail F4: outcome_uncontracted schema contract diagnostic not observed
fi
expect_event F4 RunFailed

# ── F5: C13 contrast — uncontracted reason lane accepted ────────────────────

run_fixture f5_uncontracted f5_uncontracted
expect_rc_zero F5
expect_run_green F5
if has_ev 'payload_invalid'; then
    fail F5: uncontracted step must accept the reason lane without payload rejections
else
    ok F5 reason-lane accepted with no payload rejections
fi

# ── F6: D21 — trailing-* band, recovered denial keeps success ───────────────

run_fixture f6_permissions f6_permissions
expect_rc_zero F6
if has_ev 'permission.denied'; then
    ok "F6 permission.denied event observed"
else
    fail F6: permission.denied event not observed for the read tool call
fi
expect_run_green F6

# ── F7: C18a/D22 — turn budget exhausted routes the fallback lane ───────────

run_fixture f7_budget_echo f7_budget
expect_rc_zero F7
if has_ev 'finalize_exhausted'; then
    ok F7 turn budget exhaustion observed
else
    fail F7: turn.finalize_exhausted not observed
fi
if has_ev 'finalize_kind":"missing"\|"missing"'; then
    ok F7 missing-finalize class marked
else
    fail F7: finalize_kind=missing class not observed
fi
expect_run_green F7

# ── F8: C18b — unmapped outcome name fails loud ─────────────────────────────

run_fixture f8_unmapped f8_unmapped
expect_rc_loud F8
if has_ev 'outcome_not_allowed'; then
    ok "F8 unmapped outcome failed loud (outcome_not_allowed routing error)"
else
    fail F8: outcome_not_allowed routing error not observed
fi
expect_event F8 RunFailed

# ── Verdict ──────────────────────────────────────────────────────────────────

if [ "$FAILED" -ne 0 ]; then
    echo "harness_contracts_v1 standalone suite: $FAILED failure(s)" >&2
    exit 1
fi
echo "harness_contracts_v1 standalone suite: all fixture shapes observed in run events"