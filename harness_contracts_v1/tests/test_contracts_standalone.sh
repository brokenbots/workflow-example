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
# #303: derive expected versions from releases.chcl (repo root) — hardcoded
# literals here went stale on the first repin and silently dropped the gate.
RELEASES="$TREE_ROOT/../releases.chcl"
COPILOT_V="$(sed -n 's/^copilot *= *"\([^"]*\)".*/\1/p' "$RELEASES" 2>/dev/null)"
SHELL_V="$(sed -n 's/^shell *= *"\([^"]*\)".*/\1/p' "$RELEASES" 2>/dev/null)"
if [ -z "$SKIP" ] && [ -n "$COPILOT_V" ] && ! pin_installed "$COPILOT_V"; then
    SKIP="pinned copilot adapter $COPILOT_V not cached ('criteria adapter pull ghcr.io/brokenbots/criteria-adapter-copilot:$COPILOT_V')"
fi
if [ -z "$SKIP" ] && [ -n "$SHELL_V" ] && ! pin_installed "$SHELL_V"; then
    SKIP="pinned shell adapter $SHELL_V not cached ('criteria adapter pull ghcr.io/brokenbots/criteria-adapter-shell:$SHELL_V')"
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

    # The redacting sink masks the registered --var secret values inside
    # payload text, which fragments the probe outcome names mid-string; the
    # single-char probe secret is restored so the assertions below pin exact
    # payload text. No payload legitimately contains a [REDACTED] literal.
    sed 's/\[REDACTED\]/x/g' "$EV" >"$EV.norm" && mv "$EV.norm" "$EV"

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

    # Same un-redaction as run_shipped: the runner's sink masks the probe
    # secret mid-string, and the assertions pin exact payload text.
    sed 's/\[REDACTED\]/x/g' "$EV" >"$EV.norm" && mv "$EV.norm" "$EV"

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
# The typed-lane projection observation (0.5.17 semantics, KB-305 re-proof):
# the record is written from data.internal (run state) — it must carry the
# schema-validated payload reason VERBATIM (the engine projects the validated
# payload lane's reason into output.reason). Exact content, not a whole-log
# grep: a broken projection (empty output.reason) fails the content check.
C14_REC="$PROOT/contracts_probe_records/c14_reason_record"
if grep -qxF 'c14_reason=C14 payload lane projected: the schema-validated payload reason is the submission surface under 0.5.17 typed lanes, and its characters flow verbatim through output.reason into the c14_reason run-state record' "$C14_REC" 2>/dev/null; then
    ok "F1 c14_reason record carries the projected payload-lane reason exactly (run-state projection observed)"
else
    fail "F1: c14_reason record missing or does not carry the projected lane text exactly"
fi

# ── F2: C15/C16/D19 — repair loop, no commit, engineered rejection green ────

run_fixture f2_reject_echo f2_reject
expect_rc_zero F2
# 0.5.19 wiring (KB-388 fold): the wrong-FIELD-SET rejection is payload_invalid
# (the engine schema validator sees it); the fold-REFUSED wrong-lane submission
# (the payload supplies its own reason while a different one rides the
# top-level lane) is rejected in-turn by the ADAPTER (typedReasonNotConsumed —
# silent to castle events, the model repairs from the ToolResult) — so the
# engine stream carries exactly 1 payload_invalid. A fold-CONSUMED top-level
# reason finalizes accepted (the fold delivers on the tool doc's promise);
# that acceptance half is its own leg F2b below.
if [ "$(kind_count "$EV" outcome.payload_invalid)" = "1" ]; then
    ok "F2 repair loop bounded: 1 engine payload_invalid (wrong-field) + silent in-turn fold-refusal, then success"
else
    fail "F2: expected exactly 1 engine payload_invalid rejections (0.5.19 fold semantics), saw $(kind_count "$EV" outcome.payload_invalid)"
fi
expect_run_green F2 echo_done

# C16: rejected attempts commit NOTHING. Both rejected submissions carry
# probe strings; the record file is written once by the final valid
# submission, so its content must equal that reason alone — a write-before-
# validate defect would leak a probe string or an extra verdict line here.
# Exactly one commit exists per chain node: the engine emits one StepOutcome
# per committed step, so a rejected submission that committed would surface
# a second StepOutcome for the step. (Record content is carried by the
# typed payload lane: the schema-validated payload reason is what the engine
# projects into output.reason — 0.5.17 semantics.)
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

# ── F2b: KB-388 — the fold-consumed top-level reason ACCEPTED on a typed step
# (0.5.19 fold semantics: the payload-reason acceptance half pinned separately
# from F2's rejection matrix, because an accepted finalize ends the step and
# cannot follow the rejection probes in one sequence).

run_fixture f2b_foldaccept f2b_foldaccept
expect_rc_zero F2b
expect_run_green F2b echo_done
if [ "$(kind_count "$EV" outcome.payload_invalid)" = "0" ]; then
    ok "F2b: the fold-consumed top-level reason finalized accepted with zero payload_invalid (the fold delivers on the tool doc's promise)"
else
    fail "F2b: expected 0 payload_invalid on the folded acceptance, saw $(kind_count "$EV" outcome.payload_invalid)"
fi
F2B_REC="$PROOT/contracts_probe_records/d19_verdict_record"
if grep -qF 'f2b fold-accept probe: this top-level reason must fold into the payload and finalize accepted' "$F2B_REC" 2>/dev/null; then
    ok "F2b: the record carries the folded top-level prose verbatim (the fold lane projected into output.reason)"
else
    fail "F2b: record does not carry the folded reason — the fold acceptance did not project into the typed lane"
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
if grep '"payload_type":"RunCompleted"' "$EV" | grep '"finalState":"failed"' >/dev/null; then
    ok "F4 run completed with finalState=failed (adapter pre-flight refused the undeclared arm)"
else
    fail "F4: no RunCompleted payload with finalState=failed"
fi

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

# ── F10: KB-387 — the rolled adapter's fold-boundary guard (rejection +
# typed-lane finalize under the 0.5.18 pin)

run_fixture f10_reasonfold f10_reasonfold
expect_rc_zero F10
expect_run_green F10
expect_step F10 contract_fold approved
# The leg finalizes with ZERO payload_invalid: call (1) the wrong-lane probe
# is the in-turn lane rejection (silent in events, re-prompted) and call (2)
# the typed-lane finalize commits first try. The KB-370 fold itself serves
# the funnel hops and pins upstream + on the live CR row (see fixture header);
# what the roll must never regress on the compiled surface is exactly this:
# wrong-lane rejection persistence + plain typed finalize green.
if [ "$(kind_count "$EV" outcome.payload_invalid)" = "0" ]; then
    ok "F10 finalized with zero payload_invalid on the rolled adapter (roll is behavior-preserving on the compiled surface)"
else
    fail "F10: expected 0 payload_invalid on the rolled adapter, saw $(kind_count "$EV" outcome.payload_invalid)"
fi
# Commit noise must stay bounded EXACTLY as F2 pins it for rejected
# attempts: exactly one StepOutcome (the finalize — the wrong-lane attempt
# wrote none) wired through exactly two engineered submit_outcome calls
# (the silent rejection re-prompt, then the commit). The probe/reason TEXTS
# surface only inside tool.invocation argument logs (the adapter logs every
# model call verbatim, rejected or not — the same reality F2's record pins
# cover); run-state carries the finalize alone.
if [ "$(count_type "$EV" StepOutcome)" = "1" ]; then
    ok "F10 exactly one StepOutcome (the finalize; the wrong-lane attempt committed nothing)"
else
    fail "F10: expected exactly 1 StepOutcome event, saw $(count_type "$EV" StepOutcome)"
fi
n_inv="$(kind_count "$EV" tool.invocation)"
if [ "$n_inv" = "2" ]; then
    ok "F10 exactly two submit_outcome calls (silent rejection + finalize)"
else
    fail "F10: expected exactly 2 tool.invocation events, saw $n_inv"
fi

# ── F7: C18a/D22 — turn budget exhausted routes the fallback lane ───────────
# The fixture mirrors main.chcl's contract_budget step: the adapter burns the
# turn budget without finalize, the pinned evaluator finalizes the step with
# the declared fallback arm by name, and the run lands green on echo_done.

run_fixture f7_budget_echo f7_budget
expect_rc_zero F7
expect_step F7 contract_budget failure
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

# ── F8: C18b — the never-finalize stimulus routes the declared fallback lane ─
# Compile-compliant recomposition of the retired unmapped-loud probe: the
# session never calls submit_outcome, the turn budget exhausts, the pinned
# evaluator finalizes the step with the declared fallback arm BY NAME, and the
# commit lands on the failed terminal — loud AND routed (a committed
# StepOutcome carrying the typed name), not an engine routing error. The
# retired unmapped-loud class is a compile-verified impossibility; its absence
# is asserted explicitly so a regression to it fails the leg.

run_fixture f8_unmapped f8_unmapped
expect_rc_loud F8
expect_step F8 contract_unmapped finalize_exhausted
if [ "$(kind_count "$EV" turn.finalize_exhausted)" -ge 1 ] 2>/dev/null; then
    ok "F8 turn budget exhaustion observed"
else
    fail F8: turn.finalize_exhausted not observed
fi
if has_ev '"finalize_kind":"missing"'; then
    ok "F8 missing-finalize class marked"
else
    fail F8: finalize_kind=missing class not observed
fi
expect_event F8 RunCompleted
if has_ev '"finalState":"failed"'; then
    ok "F8 the routed fallback reached the failed terminal through the standard completion path"
else
    fail F8: the run did not complete on the failed terminal
fi
if [ "$(count_type "$EV" RunFailed)" = "0" ]; then
    ok "F8 no engine-error path fired"
else
    fail F8: an engine-error path fired instead of the terminal completion
fi
if has_ev 'outcome_not_allowed'; then
    fail F8: the retired unmapped-loud class fired
else
    ok "F8 the retired unmapped-loud class is absent"
fi

# ── F8b: C18 default mapping — the synthetic unmapped name routes the default
#    lane ──────────────────────────────────────────────────────────────────────
# The mapped-by-default contrast to F8: identical never-finalize stimulus, but
# the fixture declares an explicit `outcome "default"` arm. The adapter's
# synthetic failure outcome name is unmapped on the step as a submission, and
# the engine maps it through the default arm — the run itself stays green.

run_fixture f8b_default f8b_default
expect_rc_zero F8b
if has_ev 'outcome_not_allowed'; then
    fail F8b: the retired unmapped-loud class fired on the default-routed step
else
    ok "F8b the retired unmapped-loud class is absent on the default-routed step"
fi
if has_ev '"to":"done","viaOutcome":"default"'; then
    ok "F8b unmapped synthetic failure name routed the explicit default arm (viaOutcome=default)"
else
    fail F8b: the default arm did not route the unmapped synthetic failure name
fi
# The 0.5.57+ engine rejects the synthetic name through the step's outcome
# contract surface (the observed notice on the invalid attempt) before the
# repair resolves the default lane — the same contract-rejection shape the
# shipped chain's default-routed step emits.
if has_ev 'outcome_uncontracted: outcome \\"failure\\" has no outcome_contracts entry'; then
    ok "F8b synthetic failure name rejected through the outcome-contract surface before the repair"
else
    fail F8b: the outcome-contract rejection notice for the synthetic name was not observed
fi
if [ "$(kind_count "$EV" turn.finalize_exhausted)" = "1" ] && has_ev '"finalize_kind":"missing"'; then
    ok "F8b burn bounded: exactly 1 finalize_exhausted turn before the default lane"
else
    fail F8b: expected exactly 1 finalize_exhausted turn saw "$(kind_count "$EV" turn.finalize_exhausted)" or finalize_kind=missing unobserved, before the default lane
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
# The default-routed step emits no committed StepOutcome: its synthetic name
# is rejected through the outcome-contract surface (StepOutcomeInvalid, the
# outcome_not_allowed issue on the failure submission) and the engine resolves
# the routing through the explicit default arm — visible only in the
# StepTransition viaOutcome. The mapped-by-default vs routed-terminal contrast
# with contract_unmapped: identical never-finalize stimulus, but the contract
# surface names the lane (a viaOutcome=default transition, green) where the
# terminal's declared fallback arm commits the lane BY NAME on state.failed.
expect_step F9 contract_default failure
expect_step F9 contract_unmapped finalize_exhausted
if has_ev '"from":"contract_default","to":"contract_unmapped","viaOutcome":"default"'; then
    ok "F9 default-lane routing observed (viaOutcome=default after the loud notice)"
else
    fail F9: the explicit default arm did not route the synthetic failure outcome
fi
expect_event F9 RunCompleted
# The routed run completes on the failed terminal through the standard
# completion path: a RunCompleted carrying finalState=failed (a failed
# terminal reports finalState; the engine-error path would emit RunFailed).
if has_ev '"finalState":"failed"'; then
    ok "F9 the routed chain completed on the failed terminal (finalState=failed)"
else
    fail F9: the run did not complete on the failed terminal
fi
if [ "$(count_type "$EV" RunFailed)" = "0" ]; then
    ok "F9 no engine-error path fired"
else
    fail F9: an engine-error path fired instead of the terminal completion
fi
if grep -q '"finalState":"done"' "$EV"; then
    fail "F9: the loud-terminal chain must not complete with finalState=done"
else
    ok "F9 no done completion on the loud-terminal chain"
fi
if [ "$(count_type "$EV" StepOutcomeInvalid)" = "1" ]; then
    ok "F9 exactly one invalid submission notice across the chain (the default-routed step)"
else
    fail "F9: expected exactly 1 StepOutcomeInvalid across the chain, saw $(count_type "$EV" StepOutcomeInvalid)"
fi
if has_ev 'outcome_not_allowed: outcome \\"failure\\" is not in allowed_outcomes'; then
    ok "F9 the lone invalid notice is the default-routed step's outcome_not_allowed rejection"
else
    fail F9: the outcome_not_allowed rejection notice on the default-routed step not observed
fi
if has_ev 'outcome_uncontracted'; then
    fail "F9: uncontracted contrast step must accept the reason lane green (see F4 for the loud shape)"
else
    ok "F9 uncontracted contrast step accepted the reason lane"
fi
# 0.5.17: contract_reject emits 1 payload_invalid (wrong-field-set; the
# wrong-lane rejection is silent) and contract_comment emits 1 (empty comment).
if [ "$(kind_count "$EV" outcome.payload_invalid)" = "2" ]; then
    ok "F9 payload rejection count bounded (1 schema rejection + 1 empty comment)"
else
    fail "F9: expected exactly 2 payload_invalid rejections, saw $(kind_count "$EV" outcome.payload_invalid)"
fi
if [ "$(kind_count "$EV" comment_missing)" = "1" ]; then
    ok "F9 empty-comment loud rejection observed once (comment_missing)"
else
    fail "F9: expected exactly 1 comment_missing, saw $(kind_count "$EV" comment_missing)"
fi
n_fin="$(kind_count "$EV" turn.finalize_exhausted)"
if [ "$n_fin" = "3" ]; then
    ok "F9 all three never-finalize steps exhausted their turn budget"
else
    fail F9: expected exactly 3 turn.finalize_exhausted saw "$n_fin" — budget + default + terminal
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
# finalized step, the d19 record equals the final engineered reason
# alone, and no rejected probe string reached run state (typed-lane submits
# carry the payload reason through the projection; rejected payloads never
# commit). 8 StepOutcome events = 5 green commits + the fallback-routed
# contract_budget failure + the echo_verdict success + the committed
# finalize_exhausted terminal lane; the default-routed step emits none (its
# routing is visible in the StepTransition viaOutcome).
if [ "$(count_type "$EV" StepOutcome)" = "8" ]; then
    ok "F9 exactly one outcome event per finalized step (7 commits incl. the terminal fallback lane + echo; the default-routed step emits none)"
else
    fail "F9: expected exactly 8 StepOutcome events across the chain, saw $(count_type "$EV" StepOutcome)"
fi
# C14 projection observed on the shipped chain too: the c14 record carries the
# schema-validated payload reason verbatim (typed-lane projection).
if grep -qxF 'c14_reason=C14 payload lane projected: the schema-validated payload reason is the submission surface under 0.5.17 typed lanes, and its characters flow verbatim through output.reason into the c14_reason run-state record' "$C14_REC" 2>/dev/null; then
    ok "F9 c14 record carries the projected payload-lane reason verbatim (typed-lane projection pin)"
else
    fail "F9: shipped-chain c14 record content wrong (projection assertion)"
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