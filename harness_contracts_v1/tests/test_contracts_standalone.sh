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
# Single-stimulus fixture runs pin each shape mechanically: the CLI session
# carries conversation state ACROSS steps, and a scripted stub cannot steer
# multi-step sessions deterministically, so per-shape fixtures keep the
# engineered call chains short and exact. F9 then runs the SHIPPED
# main.chcl verbatim end-to-end (all nine steps, loud-terminal by design)
# so the routed tree itself is under test, not only its fixture copies.
#
# Skips cleanly when the engine, node, jq, or the pinned adapter versions are
# unavailable — the CI lanes that lack them pick the compile-level gates.

TREE_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FIXTURES="$(cd "$(dirname "${BASH_SOURCE[0]}")/fixtures" && pwd)"
CRITERIA="${CRITERIA_BIN:-criteria}"
TIMEOUT_SECS="${SUITE_TIMEOUT:-150}"
# The shipped-chain run (F9) drives all nine steps plus turn-budget and
# loud-terminal stimuli in one session; give it its own generous ceiling.
TIMEOUT_FULL="${SUITE_TIMEOUT_FULL:-750}"

FAILED=0
declare -a STUB_PIDS=()
WORK=""

fail() {
    echo "FAIL: $*" >&2
    FAILED=$((FAILED + 1))
}

ok() {
    echo "ok: $*"
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
# pin_installed VERSION: true when the harness tree's lockfile-referenced
# adapter pinned at this version is present in the local OCI cache. The
# digest join runs against the tree's own lockfile ('list --referenced'
# resolves pins relative to the caller's cwd) and matches it against the
# installed cache by digest — plain list output renders cached entries
# unattributed, so a cwd like the repo root would never see the version.
pin_installed() {
    local refdig
    refdig="$(cd "$TREE_ROOT" && "$CRITERIA" adapter list --referenced 2>/dev/null \
        | awk -v v=":$1" '$2 ~ v"$" {print $3}')"
    [ -n "$refdig" ] || return 1
    # No -q in pipelines: with pipefail a left grep killed by SIGPIPE turns a
    # found match into a spurious failure — drain the stream instead.
    "$CRITERIA" adapter list --installed 2>/dev/null | grep "$refdig" >/dev/null
}
if [ -z "$SKIP" ] && ! pin_installed 0.5.15; then
    SKIP="pinned copilot adapter 0.5.15 not cached ('criteria adapter pull ghcr.io/brokenbots/criteria-adapter-copilot:0.5.15')"
fi
if [ -z "$SKIP" ] && ! pin_installed 0.5.4; then
    SKIP="pinned shell adapter 0.5.4 not cached ('criteria adapter pull ghcr.io/brokenbots/criteria-adapter-shell:0.5.4')"
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

# kind_count FILE KIND: the loud shapes (payload_invalid, comment_missing,
# finalize_exhausted, permission.*) are AdapterEvent KINDS, not payload
# types — counting them requires the kind member, never the payload_type.
kind_count() {
    grep -o "\"kind\":\"$2\"" "$1" 2>/dev/null | grep -c . || true
}

# run_shipped NAME MODE [TIMEOUT]: like run_fixture but does NOT replace
# main.chcl — the SHIPPED flow runs verbatim, so every assertion below
# exercises the tree the routes entry actually deploys, not a fixture
# copy. The stub still supplies the engineered call chain for the whole
# fixed-prompt chain (stub mode full_chain: per-step marker dispatch;
# committed finalize tails answer plain end-of-turn so the CLI's stray
# post-commit model call cannot drift the next step's dispatch).
run_shipped() {
    local name="$1" mode="$2"
    local tmo="${3:-$TIMEOUT_SECS}"
    local tree port log stub_pid
    tree="$WORK/tree-$name"
    cp -r "$TREE_ROOT" "$tree"
    rm -rf "$tree/tests"

    PROOT="$WORK/probe-$name"
    mkdir -p "$PROOT"

    port="$(free_port)"
    log="$WORK/stub-$name.log"
    STUB_MODE="$mode" STUB_PORT="$port" PROBEROOT="$PROOT" node "$FIXTURES/contracts_stub.mjs" >"$log" 2>&1 &
    stub_pid=$!
    STUB_PIDS+=("$stub_pid")

    STUBPORT="$port" PROBEROOT="$PROOT" node "$FIXTURES/adapters_shim.mjs" \
        "$tree/adapters.chcl" "$tree/adapters.chcl"

    EV="$WORK/events-$name.log"
    RC=0
    timeout "$tmo" "$CRITERIA" apply "$tree" \
        --var workflow_github_token=x \
        --var probe_root="$PROOT" \
        --output json --ui=false >"$EV" 2>&1 || RC=$?
    kill "$stub_pid" 2>/dev/null

    if [ "${SUITE_DEBUG:-}" = "1" ]; then
        echo "--- events $name (rc=$RC) ---" >&2
        tail -c 4000 "$EV" >&2
        echo "--- stub $name ---" >&2
        tail -c 1500 "$log" >&2
    fi
}
EV=""
RC=""
PROOT=""
# run_fixture NAME MODE: creates the tree copy, replaces main.chcl with the
# fixture, starts the stub, applies, and leaves the caller the event log at
# $EV and the run rc at $RC.
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

    if [ "${SUITE_DEBUG:-}" = "1" ]; then
        echo "--- events $name (rc=$RC) ---" >&2
        tail -c 4000 "$EV" >&2
        echo "--- stub $name ---" >&2
        tail -c 1500 "$log" >&2
    fi
}

# expect_step LABEL STEP OUTCOME: asserts the StepOutcome payload pair
# ({"step":...,"outcome":...} — engine serialization order) against the
# current event log.
expect_step() {
    if has_ev "\"step\":\"$2\",\"outcome\":\"$3\""; then
        ok "$1: $2 outcome $3 observed in run events"
    else
        fail "$1: $2 did not route outcome $3"
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
    # expect_run_green LABEL [FINALSTATE]: the green terminal of a fixture is
    # its declared final state — single-probe flows end in done, flows that
    # chain the verdict echo end in echo_done.
    expect_event "$1" RunCompleted
    local want="${2:-done}"
    if grep '"payload_type":"RunCompleted"' "$EV" | grep "\"finalState\":\"$want\"" >/dev/null; then
        ok "$1 run completed with finalState=$want"
    else
        fail "$1: RunCompleted payload was not finalState=$want"
    fi
}

# ── F1: C14 — valid payload accepted, projected outputs in run state ────────

run_fixture f1_valid f1_valid
expect_rc_zero F1
expect_event F1 StepOutcome
expect_run_green F1
# The lane-discriminated projection observation: the record is written from
# data.internal (run state) — it must carry the one lane the engine projects
# (top-level reason) VERBATIM, and must NOT carry the payload lane text (the
# engine drops payload body fields — the pinned projection gap). Exact
# content, not a whole-log grep: a broken projection (empty output.reason) or
# a payload-text projection fails both checks.
C14_REC="$PROOT/contracts_probe_records/c14_reason_record"
if grep -qxF 'c14_reason=C14 projected outputs: the top-level reason lane is the one lane the engine projects — these characters flow verbatim through output.reason into the c14_reason run-state record' "$C14_REC" 2>/dev/null; then
    ok "F1 c14_reason record carries the projected reason-lane text exactly (run-state projection observed)"
else
    fail "F1: c14_reason record missing or does not carry the projected lane text exactly"
fi
if ! grep -qF 'C14 payload lane:' "$C14_REC" 2>/dev/null; then
    ok "F1 payload lane text absent from run state (projection gap pinned: payload fields dropped)"
else
    fail F1: the payload lane text leaked into the projected run state
fi

# ── F2: C15/C16/D19 — repair loop, no commit, engineered rejection green ────

run_fixture f2_reject_echo f2_reject
expect_rc_zero F2
if [ "$(kind_count "$EV" outcome.payload_invalid)" = "2" ]; then
    ok "F2 repair loop bounded: exactly 2 payload_invalid rejections then success"
else
    fail "F2: expected exactly 2 payload_invalid repair-loop rejections, saw $(kind_count "$EV" outcome.payload_invalid)"
fi
expect_run_green F2 echo_done

# C16: rejected attempts commit NOTHING. Both rejected submissions carry
# probe strings; the record file is written once by the final valid
# submission, so its content must equal that reason alone — a write-before-
# validate defect would leak a probe string or an extra verdict line here.
# Exactly one commit exists per chain node: the engine emits one StepOutcome
# per committed step, so a rejected submission that committed would surface
# a second StepOutcome for the step. (Record content is carried by the
# dual-lane submit: the payload satisfies the schema and the identical
# top-level reason is what the engine projects into output.reason.)
D19_REC="$PROOT/contracts_probe_records/d19_verdict_record"
if [ -f "$D19_REC" ]; then
    ok "F2 d19 verdict record written to run-state files"
else
    fail F2: d19_verdict_record missing from probe root
fi
if [ "$(count_type "$EV" StepOutcome)" = "2" ]; then
    ok "F2 exactly one committed outcome per step (reject + echo; no rejected commit)"
else
    fail "F2: expected exactly 2 StepOutcome events, saw $(count_type "$EV" StepOutcome)"
fi
if [ "$(wc -l <"$D19_REC" 2>/dev/null | tr -d ' ')" = "1" ]; then
    ok "F2 record holds exactly one verdict line (no rejected writes)"
else
    fail "F2: d19_verdict_record is not a single verdict line (rejected attempt(s) wrote state)"
fi
if grep -qF 'wrong field set on purpose' "$D19_REC" 2>/dev/null \
    || grep -qF 'wrong lane on purpose' "$D19_REC" 2>/dev/null; then
    fail "F2: rejected probe strings leaked into run-state record (C16 violated)"
else
    ok "F2 rejected attempts committed no run-state content (C16)"
fi
if grep -qF 'D19 engineered rejection routed green: wrong-field-set and wrong-lane shapes were rejected loudly by the payload contract, then the declared rejected lane carried this verbatim reason' "$D19_REC" 2>/dev/null; then
    ok "F2 record content equals the final engineered reason only"
else
    fail "F2: d19_verdict_record content != final engineered reason"
fi
if has_ev 'D19 engineered rejection routed green: wrong-field-set and wrong-lane shapes were rejected loudly by the payload contract, then the declared rejected lane carried this verbatim reason'; then
    ok "F2 D19 engineered reason carried verbatim in run events"
else
    fail F2: D19 engineered reason not found verbatim in run events
fi

# ── F3: C17 — empty adapter comment rejected loudly, observed exit green ────

run_fixture f3_comment_echo f3_comment
expect_rc_zero F3
if kind_count "$EV" comment_missing | grep -qv '^0$'; then
    ok "F3 empty adapter comment rejected loudly (comment_missing)"
else
    fail F3: comment_missing loud rejection not observed
fi
if [ "$(kind_count "$EV" outcome.payload_invalid)" -ge 1 ] 2>/dev/null; then
    ok "F3 rejection surfaced as payload_invalid"
else
    fail F3: loud rejection missing the payload_invalid shape
fi
expect_run_green F3 echo_done
if [ -f "$PROOT/contracts_probe_records/c17_comment_record" ]; then
    ok "F3 c17 verdict record written to run-state files"
else
    fail F3: c17_comment_record missing from probe root
fi
if grep -qF 'C17 observed: the empty adapter comment on the require_comment arm was rejected loudly by the payload contract (payload_invalid, comment_missing)' "$PROOT/contracts_probe_records/c17_comment_record" 2>/dev/null; then
    ok "F3 c17 record carries the engineered observed verdict"
else
    fail "F3: c17_comment_record content != the engineered observed verdict"
fi
if has_ev 'C17 observed: the empty adapter comment on the require_comment arm was rejected loudly by the payload contract (payload_invalid, comment_missing)'; then
    ok "F3 engineered observed verdict carried verbatim in run events"
else
    fail F3: engineered observed verdict not found verbatim in run events
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
    ok "F3b step failed loud after 1 attempts"
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
    ok "F5 reason-lane accepted with no payload rejections"
fi

# ── F6: D21 — trailing-* band first-match-wins, recovered denial ─────────────

run_fixture f6_permissions f6_permissions
expect_rc_zero F6

# Grant attribution: the trailing-* band matched the shell tool call
# FIRST-MATCH-WINS — the permission.granted payload echoes the winning
# pattern (s*) and the policy tool name (shell).
if grep 'permission\.granted' "$EV" 2>/dev/null | grep -F '"pattern":"s*"' >/dev/null; then
    ok "F6 permission.granted echoes the winning trailing-* pattern (s*) — first match wins"
else
    fail "F6: permission.granted does not echo pattern=s* (grant not attributable to the trailing-* band)"
fi
if grep 'permission\.granted' "$EV" 2>/dev/null | grep -F '"tool":"shell"' >/dev/null; then
    ok "F6 allowed tool call attributed: shell"
else
    fail "F6: no permission.granted attributable to the shell tool call"
fi

# Denial attribution: every denied permission event names the read tool —
# a band that denied the shell call too would surface a shell-named denial.
if grep 'permission\.denied' "$EV" 2>/dev/null | grep -F '"tool":"read"' >/dev/null; then
    ok "F6 permission.denied attributed to the read tool call"
else
    fail "F6: permission.denied not attributable to the read tool call"
fi
if grep 'permission\.denied' "$EV" 2>/dev/null | grep -F '"tool":"shell"' >/dev/null; then
    fail "F6: the band policy denied the allowed shell tool call"
else
    ok "F6 the allowed shell tool call was never denied"
fi

# Execution proof: the granted bash call actually ran — its side-effect
# record exists with the engineered content under the probe root.
if [ "$(cat "$PROOT/d21_shell_band_record" 2>/dev/null || true)" = "d21-shell-band-ok" ]; then
    ok "F6 band-matched shell call executed (side-effect record content)"
else
    fail "F6: d21_shell_band_record missing or wrong — the granted shell call did not run"
fi
# The submitted reason (top-level lane — the one the engine projects) flows
# into run state via the step's write of output.reason; the captured value
# equals the submitted reason (fault-sensitive: a broken projection or a
# dropped lane yields an empty/different captured reason).
if grep 'StepOutputCaptured' "$EV" 2>/dev/null | grep -F '"reason":"D21 recovered denial keeps success: the trailing-* band granted the shell tool call first-match-wins (pattern s* echoed in the permission.granted event), the read tool call was denied with a permission.denied event naming the tool, and the step still finalized approved after recovering"' >/dev/null; then
    ok "F6 projected reason captured in run state (StepOutputCaptured content)"
else
    fail "F6: projected reason missing from run state — output.reason was not captured"
fi
expect_run_green F6

# ── F6c: D21 — compound per-segment matching, every-segment refusal ─────────

run_fixture f6c_compound f6c_compound
expect_rc_zero F6c

# The denied compound is refused with the EXACT every-segment refusal: the
# matcher split printf && ls and required EVERY segment to match — segment
# one matched the kind-prefixed argument band, segment two matched nothing.
if grep 'permission\.denied' "$EV" 2>/dev/null | grep -F '"reason":"no matching allow_tools entry for every segment of the compound command"' >/dev/null; then
    ok "F6c compound denial carries the exact every-segment refusal"
else
    fail "F6c: compound refusal not observed verbatim (per-segment matcher unobserved)"
fi
if grep 'permission\.denied' "$EV" 2>/dev/null | grep -F '"tool":"shell"' >/dev/null; then
    ok "F6c denial attributed to the shell tool call"
else
    fail "F6c: no denial attributable to the shell tool call"
fi

# The granted all-matching compound is attributed to the winning
# kind-prefixed argument band (shell kind + printf * argument pattern).
if grep 'permission\.granted' "$EV" 2>/dev/null | grep -F '"pattern":"shell:printf *"' >/dev/null; then
    ok "F6c compound grant attributed to the kind-prefixed argument band (shell:printf *)"
else
    fail "F6c: no grant attributable to the kind-prefixed argument band"
fi

# The denied compound executed NOTHING (no seg1 record) while the granted
# compound executed BOTH segments (seg2 + seg3 records with engineered
# content).
if [ ! -e "$PROOT/d21c_seg1_record" ]; then
    ok "F6c denied compound committed nothing (seg1 record absent)"
else
    fail "F6c: d21c_seg1_record exists — the refused compound was not fully denied"
fi
if [ "$(cat "$PROOT/d21c_seg2_record" 2>/dev/null || true)" = "d21c-segment-a" ] \
    && [ "$(cat "$PROOT/d21c_seg3_record" 2>/dev/null || true)" = "d21c-segment-b" ]; then
    ok "F6c granted compound executed both segments (side-effect records content)"
else
    fail "F6c: d21c_seg2_record/d21c_seg3_record missing or wrong — the granted compound did not run"
fi
expect_run_green F6c

# ── F6d: D21 — kind-prefixed + adapter-namespace bands match nothing ────────

run_fixture f6d_kindprefix f6d_kindprefix
expect_rc_zero F6d

# No band form in this set matches the policy tool naming (command tool maps
# to `shell`, view maps to `read`): the bash call denial is a plain
# no-matching-entry refusal, NOT the every-segment form.
if grep 'permission\.denied' "$EV" 2>/dev/null | grep -F '"reason":"no matching allow_tools entry"' >/dev/null \
    && ! grep -F 'for every segment of the compound command' "$EV" >/dev/null; then
    ok "F6d kind-prefixed bash band matches nothing (plain no-entry refusal, tool shell)"
else
    fail "F6d: expected the plain no-matching-entry refusal for the kind-prefixed band set"
fi
if grep 'permission\.denied' "$EV" 2>/dev/null | grep -F '"tool":"read"' >/dev/null; then
    ok "F6d the read call is denied too (adapter-namespace band matches nothing)"
else
    fail "F6d: the read call was not denied (no no-match observation)"
fi
if [ "$(kind_count "$EV" permission.granted)" != "0" ]; then
    fail "F6d: a grant was observed under a band set that matches nothing"
else
    ok "F6d no permission.granted under the no-match band set"
fi

# The denied bash call executed nothing.
if [ ! -e "$PROOT/d21d_record" ]; then
    ok "F6d denied call committed nothing (kind-prefix record absent)"
else
    fail "F6d: d21d_record exists — the denied call was not fully denied"
fi
expect_run_green F6d

# ── F7: C18a/D22 — turn budget exhausted routes the fallback lane ───────────

run_fixture f7_budget_echo f7_budget
expect_rc_zero F7
if [ "$(kind_count "$EV" turn.finalize_exhausted)" -ge 1 ] 2>/dev/null; then
    ok "F7 turn budget exhaustion observed"
else
    fail F7: turn.finalize_exhausted not observed
fi
if has_ev '"finalize_kind":"missing"'; then
    ok "F7 missing-finalize class marked"
else
    fail F7: finalize_kind=missing class not observed
fi
expect_run_green F7 echo_done

# ── F8: C18b — unmapped outcome name fails loud ─────────────────────────────

run_fixture f8_unmapped f8_unmapped
expect_rc_loud F8
if has_ev 'outcome_not_allowed'; then
    ok "F8 unmapped outcome failed loud (outcome_not_allowed routing error)"
else
    fail F8: outcome_not_allowed routing error not observed
fi
expect_event F8 RunFailed

# ── F8b: C18 default mapping — the synthetic unmapped name routes the default
#    lane ──────────────────────────────────────────────────────────────────────
# The mapped-by-default contrast to F8: identical never-finalize stimulus, but
# the fixture declares an explicit `outcome "default"` arm. The adapter's
# synthetic failure outcome name is unmapped on the step as a submission, and
# the engine maps it through the default arm — the run itself stays green.

run_fixture f8b_default f8b_default
expect_rc_zero F8b
if has_ev '"step":"contract_default","outcome":"failure"' \
   && has_ev 'outcome_not_allowed'; then
    ok "F8b synthetic failure name loudly unmapped even with the default arm (outcome_not_allowed)"
else
    fail F8b: the unmapped loud notice before the default routing was not observed
fi
if has_ev '"to":"done","viaOutcome":"default"'; then
    ok "F8b unmapped synthetic failure name routed the explicit default arm (viaOutcome=default)"
else
    fail F8b: the default arm did not route the unmapped synthetic failure name
fi
if [ "$(kind_count "$EV" turn.finalize_exhausted)" = "1" ]; then
    ok "F8b burn bounded: exactly 1 finalize_exhausted turn before the default lane"
else
    fail "F8b: expected exactly 1 finalize_exhausted (bounded turn budget), saw $(kind_count "$EV" turn.finalize_exhausted)"
fi
if has_ev '"finalize_kind":"missing"'; then
    ok "F8b missing-finalize class marked before the default lane routing"
else
    fail F8b: finalize_kind=missing not observed on the default-lane step
fi
expect_run_green F8b

# ── F9: shipped full chain — the SHIPPED main.chcl under test at runtime ────
#
# One run of the shipped tree, verbatim (run_shipped never replaces
# main.chcl): all nine steps execute against one stub process in chain
# order, so the card's expectation slice is observed in the events the
# routed tree actually emits — not only in fixture copies. The chain ends
# in state.failed BY DESIGN (the C18b unmapped step is loud-terminal), so
# the run rc is expected non-zero.

run_shipped f9_shipped full_chain "$TIMEOUT_FULL"
expect_rc_loud F9

expect_step F9 contract_valid approved
expect_step F9 contract_reject rejected
expect_step F9 contract_comment observed
expect_step F9 contract_uncontracted approved
expect_step F9 contract_permissions approved
expect_step F9 contract_budget failure
expect_step F9 echo_verdict success
# The default-routed step emits no StepOutcome: the synthetic name is loudly
# un-mapped (StepOutcomeInvalid outcome_not_allowed) and the engine resolves
# the routing through the explicit default arm — visible only in the
# StepTransition viaOutcome. The mapped-by-default vs unmapped-loud contrast
# with the terminal contract_unmapped step is exactly: same loud notice, but
# a viaOutcome=default transition (green) instead of a dead end (RunFailed).
expect_step F9 contract_default failure
expect_step F9 contract_unmapped failure
if has_ev '"from":"contract_default","to":"contract_unmapped","viaOutcome":"default"'; then
    ok "F9 default-lane routing observed (viaOutcome=default after the loud notice)"
else
    fail F9: the explicit default arm did not route the synthetic failure outcome
fi
expect_event F9 RunFailed
# The unmapped routing error surfaces as a StepOutcomeInvalid issue string
# ("outcome_not_allowed: outcome ... not in allowed_outcomes") and an
# AdapterEvent kind outcome.failure — the same loud shape F8 asserts.
if has_ev 'outcome_not_allowed'; then
    ok "F9 unmapped-arm routing error observed loud (outcome_not_allowed)"
else
    fail F9: outcome_not_allowed routing error not observed
fi
if grep -q '"finalState":"done"' "$EV"; then
    fail "F9: the loud-terminal chain must not complete with finalState=done"
else
    ok "F9 no done completion on the loud-terminal chain"
fi
if has_ev 'outcome_uncontracted'; then
    fail "F9: uncontracted contrast step must accept the reason lane green (see F4 for the loud shape)"
else
    ok "F9 uncontracted contrast step accepted the reason lane"
fi
if [ "$(kind_count "$EV" outcome.payload_invalid)" = "3" ]; then
    ok "F9 payload rejection count bounded (2 repair-loop rejections + 1 empty comment)"
else
    fail "F9: expected exactly 3 payload_invalid rejections, saw $(kind_count "$EV" outcome.payload_invalid)"
fi
if [ "$(kind_count "$EV" comment_missing)" = "1" ]; then
    ok "F9 empty-comment loud rejection observed once (comment_missing)"
else
    fail "F9: expected exactly 1 comment_missing, saw $(kind_count "$EV" comment_missing)"
fi
if [ "$(kind_count "$EV" turn.finalize_exhausted)" = "3" ]; then
    ok "F9 all three never-finalize steps exhausted their turn budget"
else
    fail "F9: expected exactly 3 finalize_exhausted turns, saw $(kind_count "$EV" turn.finalize_exhausted)"
fi
if has_ev '"finalize_kind":"missing"'; then
    ok "F9 missing-finalize class marked on the exhausted turns"
else
    fail F9: finalize_kind=missing class not observed
fi

# D21 attribution on the shipped step: the granted/denied split and the
# executed side-effect record, exactly as in F6 but on the flown tree.
if grep 'permission\.granted' "$EV" 2>/dev/null | grep -F '"pattern":"s*"' >/dev/null; then
    ok "F9 trailing-* band granted first-match-wins (pattern s* echoed)"
else
    fail "F9: permission.granted does not echo pattern=s*"
fi
if grep 'permission\.denied' "$EV" 2>/dev/null | grep -F '"tool":"read"' >/dev/null; then
    ok "F9 D21 denial attributed to the read tool call"
else
    fail "F9: permission.denied not attributable to the read tool call"
fi
if grep 'permission\.denied' "$EV" 2>/dev/null | grep -F '"tool":"shell"' >/dev/null; then
    fail "F9: the shipped band policy denied the allowed shell tool call"
else
    ok "F9 the allowed shell tool call was never denied"
fi
if [ "$(cat "$PROOT/d21_shell_band_record" 2>/dev/null || true)" = "d21-shell-band-ok" ]; then
    ok "F9 shipped band-matched shell call executed"
else
    fail "F9: d21_shell_band_record missing or wrong — the shipped step's granted call did not run"
fi

C14_REC="$PROOT/contracts_probe_records/c14_reason_record"
D19_REC="$PROOT/contracts_probe_records/d19_verdict_record"
C17_REC="$PROOT/contracts_probe_records/c17_comment_record"
# C16 on the shipped chain, same rule as F2: exactly one outcome event per
# non-unmapped step, the d19 record equals the final engineered reason
# alone, and no rejected probe string reached run state (dual-lane submits
# carry the top-level reason through the projection; rejected payloads never
# commit). 7 StepOutcome events = 5 finalize commits + the budget-exhausted
# contract_budget failure + the echo_verdict success; the unmapped loud
# terminal emits StepOutcomeInvalid (never a committed StepOutcome).
if [ "$(count_type "$EV" StepOutcome)" = "7" ]; then
    ok "F9 exactly one outcome event per finalized step (5 commits + 1 failure + 1 echo; the default-routed step emits none)"
else
    fail "F9: expected exactly 7 StepOutcome events across the chain, saw $(count_type "$EV" StepOutcome)"
fi
# C14 projection observed on the shipped chain too: the c14 record must carry
# the projected reason-lane text exactly and never the payload lane text.
if grep -qxF 'c14_reason=C14 projected outputs: the top-level reason lane is the one lane the engine projects — these characters flow verbatim through output.reason into the c14_reason run-state record' "$C14_REC" 2>/dev/null \
    && ! grep -qF 'C14 payload lane:' "$C14_REC" 2>/dev/null; then
    ok "F9 c14 record carries the projected lane verbatim, payload lane absent (projection pin)"
else
    fail "F9: shipped-chain c14 record content wrong (projection gap assertion)"
fi
if grep -qF 'D19 engineered rejection routed green: wrong-field-set and wrong-lane shapes were rejected loudly by the payload contract, then the declared rejected lane carried this verbatim reason' "$D19_REC" 2>/dev/null \
    && ! grep -qF 'wrong field set on purpose' "$D19_REC" 2>/dev/null \
    && ! grep -qF 'wrong lane on purpose' "$D19_REC" 2>/dev/null; then
    ok "F9 d19 record equals the final engineered reason only (C16)"
else
    fail "F9: shipped-chain d19 record content wrong (rejected writes?)"
fi
if grep -qF 'C17 observed: the empty adapter comment on the require_comment arm was rejected loudly by the payload contract (payload_invalid, comment_missing)' "$C17_REC" 2>/dev/null; then
    ok "F9 c17 record carries the engineered observed verdict"
else
    fail "F9: shipped-chain c17 record content wrong"
fi
# The D21 lane: the permission step's write lands output.reason (the lane
# the engine projects) — carried verbatim into the d21 record by the echo
# verdict rendering (same shellquote opaque-argument lane, no re-rendering).
D21_REC="$PROOT/contracts_probe_records/d21_reason_record"
if grep -qxF 'd21_reason=D21 recovered denial keeps success: the trailing-* band granted the shell tool call first-match-wins (pattern s* echoed in the permission.granted event), the read tool call was denied with a permission.denied event naming the tool, and the step still finalized approved after recovering' "$D21_REC" 2>/dev/null; then
    ok "F9 d21 record carries the projected outcome reason verbatim (outputs in run state)"
else
    fail "F9: d21_reason_record missing or does not carry the projected reason"
fi
# C13 complement: the positive content-carry proof for the schema-less
# reason lane. The contract_uncontracted approved arm (deliberately
# schema-less — ANY schema would contract all of the step's outcomes and
# destroy the stimulus) writes output.reason into run state; the captured
# write event carries the reason-only submission verbatim — the one lane the
# engine projects. The KB-247 flow rule keeps this reason OUT of downstream
# inputs on an untypeable arm, so the run-state write (not an echo record)
# is the content-carry observation surface.
if grep 'StepOutputCaptured' "$EV" 2>/dev/null | grep -F '"reason":"C13 uncontracted lane: the reason-form submission is accepted on a step with no declared schema — the same top-level reason shape is rejected loudly on all contracted steps"' >/dev/null; then
    ok "F9 c13 reason lane flows verbatim into run state (StepOutputCaptured content)"
else
    fail "F9: uncontracted-arm reason write missing from run state"
fi

# ── Verdict ──────────────────────────────────────────────────────────────────

if [ "$FAILED" -ne 0 ]; then
    echo "harness_contracts_v1 standalone suite: $FAILED failure(s)" >&2
    exit 1
fi
echo "harness_contracts_v1 standalone suite: all fixture shapes observed in run events"