#!/usr/bin/env bash
set -euo pipefail

# KB-205: runtime regression test for the decision demo (decision_demo_v1 /
# decision_demo_local_v1). decision_demo_v1 targets the System One cloud
# endpoint, which CI cannot reach, so this test exercises the IDENTICAL
# grammar of decision_demo_local_v1 against a deterministic System One stub
# (tests/stub_systemone.mjs) on a random loopback port. It proves the
# compiled graph routes each answer type (noul / choice / score) end-to-end,
# that a department confidence below route_conf_floor pauses the run on the
# approval node (human-in-the-loop confidence gating), and that approving or
# rejecting that pause lands on the corresponding terminal. Skips cleanly
# when the engine, the decision adapter binary, or node is unavailable: the
# decision adapter is still unpublished, so CI runs skip this test by design
# and the compile-level test (decision_demo_v1/tests/) is the CI gate.
# The live-ollama validation leg (real engine, model tev1:0.8b on the
# GPU-light test host) is executed out-of-band per KB-209; the deterministic
# stub below speaks the same map-shaped wire contract.

TREE_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CRITERIA="${CRITERIA_BIN:-criteria}"

FAILED=0
STUB_PID=""

fail() {
    echo "FAIL: $1" >&2
    FAILED=$((FAILED + 1))
}

ok() {
    echo "ok: $1"
}

require_equal() {
    if [ "$1" = "$2" ]; then
        ok "$3"
    else
        fail "$3: got '$1', want '$2'"
    fi
}

# ── Preconditions (skip, not fail — this is an opt-in runtime depth) ─────────

SKIP=""
[ -x "$(command -v "$CRITERIA")" ] || SKIP="criteria binary '$CRITERIA' not found (set CRITERIA_BIN)"
if [ -z "$SKIP" ] && ! command -v node >/dev/null 2>&1; then
    SKIP="node not available (stub System One backend)"
fi
if [ -z "$SKIP" ] && ! command -v jq >/dev/null 2>&1; then
    SKIP="jq not available"
fi
if [ -z "$SKIP" ]; then
    # The decision adapter binary must resolve by name: the caller's
    # CRITERIA_ADAPTERS, CRITERIA_HOME, or the default install dir.
    ADAPTER_SRC=""
    for candidate in \
        "${CRITERIA_ADAPTERS:-}/criteria-adapter-decision" \
        "${CRITERIA_HOME:-}/adapters/criteria-adapter-decision" \
        "$HOME/.local/criteria/adapters/criteria-adapter-decision"; do
        [ -n "$candidate" ] && [ -x "$candidate" ] && { ADAPTER_SRC="$candidate"; break; }
    done
    [ -n "$ADAPTER_SRC" ] || SKIP="criteria-adapter-decision binary not installed (compile-level test is the CI gate)"
fi
if [ -n "$SKIP" ]; then
    echo "SKIP: decision demo runtime test: $SKIP" >&2
    exit 0
fi

cleanup() {
    if [ -n "$STUB_PID" ]; then
        kill "$STUB_PID" 2>/dev/null || true
    fi
}
trap cleanup EXIT

TMP="$(mktemp -d)"

# Isolated engine home so run state and adapter discovery never touch the
# operator's environment.
export CRITERIA_HOME="$TMP/cri-home"
mkdir -p "$CRITERIA_HOME/adapters"
cp "$ADAPTER_SRC" "$CRITERIA_HOME/adapters/criteria-adapter-decision"

# Copy the workflow tree and re-point its pinned endpoint at the stub port
# (random, 20000-29999, collision-retried): proving that the adapter block's
# base_url/model config drives the backend target is part of the demo's
# backend-agnostic claim.
STUB_UP=""
for _ in 1 2 3; do
    PORT=$((20000 + ($$ % 9000)))
    sed "s|http://localhost:11434|http://localhost:$PORT|" "$TREE_ROOT/main.chcl" > "$TMP/main.chcl"
    node "$TREE_ROOT/tests/stub_systemone.mjs" "$PORT" > "$TMP/stub.log" 2>&1 &
    STUB_PID=$!
    for _ in 1 2 3 4 5 6 7 8 9 10; do
        if nc -z 127.0.0.1 "$PORT" 2>/dev/null; then STUB_UP=1; break; fi
        sleep 0.5
    done
    [ -n "$STUB_UP" ] && break
    kill "$STUB_PID" 2>/dev/null || true
    STUB_PID=""
done
if [ -z "$STUB_UP" ]; then
    echo "FAIL: stub System One backend did not come up on any candidate port" >&2
    cat "$TMP/stub.log" >&2 || true
    exit 1
fi
ok "stub System One backend listening on port $PORT"

run_case() {
    # run_case <case_id> <subject> [answers_file]
    _case_id="$1"; _subject="$2"; _answers="${3:-}"
    _args=(apply "$TMP" --var "ticket_subject=$_subject" --var "ticket_body=n/a" --output json --ui=false)
    [ -n "$_answers" ] && _args+=(--answers "$_answers")
    # RunCompleted carries no success flag on non-failure terminals; the
    # engine exits 1 when the run lands on a failure-success terminal
    # (abandoned / decision_failed), so the caller asserts exit codes per
    # scenario rather than uniformly.
    set +e
    "$CRITERIA" "${_args[@]}" > "$TMP/$_case_id.log" 2>&1
    RC_EXIT=$?
    set -e
}

evs() {
    # evs <case_id> <payload_type>
    # The apply stream mixes JSON-lines events with a trailing human footer
    # (set -e turns jq's parse error into an abort), so feed jq only lines
    # that start a JSON object.
    grep '^{' "$TMP/$1.log" | jq -c --arg t "$2" 'select(.payload_type == $t) | .payload'
}

first_target() {
    # First BranchEvaluated target for a case (switch arms evaluate in order
    # but only the winning arm emits BranchEvaluated per node evaluation).
    evs "$1" BranchEvaluated | head -n 1 | jq -r '.target'
}

first_arm() {
    evs "$1" BranchEvaluated | head -n 1 | jq -r '.matchedArm'
}

final_state() {
    evs "$1" RunCompleted | tail -n 1 | jq -r '.finalState'
}

approval_decision() {
    evs "$1" ApprovalDecision | head -n 1
}

APPROVE_GATE="$TMP/approve_gate.json"
cat > "$APPROVE_GATE" <<EOF
{"confirm_department": {"decision": "approved", "reason": "manual review confirmed"}}
EOF
REJECT_GATE="$TMP/reject_gate.json"
cat > "$REJECT_GATE" <<EOF
{"confirm_department": {"decision": "rejected", "reason": "cannot confirm"}}
EOF
APPROVE_ESC="$TMP/approve_esc.json"
cat > "$APPROVE_ESC" <<EOF
{"confirm_escalation": {"decision": "approved", "reason": "outage confirmed"}}
EOF

# ── Scenario 1: technical subject auto-routes (no human step) ────────────────

run_case technical "server returns 500 on checkout"
require_equal "$(first_target technical)" "queued_technical" "technical routes to queued_technical"
require_equal "$(final_state technical)" "queued_technical" "technical run completes at queued_technical"
[ "$RC_EXIT" -eq 0 ] || fail "technical run exit code: got $RC_EXIT, want 0"
[ "$(evs technical ApprovalDecision | wc -l)" -eq 0 ] || fail "auto-routed run must not pause on an approval"
if evs technical BranchEvaluated | head -n 1 | grep -q '"matchedArm":"match\[5\]"'; then
    ok "technical routed via match[5] (department choice arm)"
else
    fail "technical did not route via the match[5] department arm"
fi
if grep -q 'department\\\\":\\\\"technical' "$TMP/technical.log" || \
   grep '^{' "$TMP/technical.log" | jq -r 'select(.payload_type == "run.outputs") | .payload.outputs[0].value' | grep -q technical; then
    ok "routing_summary output captured department=technical"
else
    fail "routing_summary output did not capture the department"
fi

# ── Scenario 2: billing subject routes on the choice answer ──────────────────

run_case billing "billing dispute on invoice"
require_equal "$(final_state billing)" "queued_billing" "billing routes to queued_billing"
[ "$RC_EXIT" -eq 0 ] || fail "billing run exit code: got $RC_EXIT, want 0"

# ── Scenario 3: low confidence triggers the human gate, approve -> dispatch ──

run_case lowconf_approved "lowconf portal issue" "$APPROVE_GATE"
require_equal "$(first_target lowconf_approved)" "confirm_department" "low confidence routes to the approval node"
approval_decision_low="$(approval_decision lowconf_approved | jq -r '.decision')"
require_equal "$approval_decision_low" "approved" "approval recorded as approved"
require_equal "$(final_state lowconf_approved)" "dispatched" "approved gate lands on dispatched"

# ── Scenario 4: rejecting the gate abandons the run ─────────────────────────

run_case lowconf_rejected "lowconf portal issue" "$REJECT_GATE"
approval_decision_rej="$(approval_decision lowconf_rejected | jq -r '.decision')"
require_equal "$approval_decision_rej" "rejected" "gate rejection recorded"
require_equal "$(final_state lowconf_rejected)" "abandoned" "rejected gate lands on abandoned"

# ── Scenario 5: noul answer (urgency yes) routes to the escalation gate ──────

run_case urgent "urgent production outage" "$APPROVE_ESC"
require_equal "$(first_arm urgent)" "match[1]" "noul urgency arm (match[1]) fires before the choice arms"
require_equal "$(final_state urgent)" "escalated" "approved escalation lands on escalated"

# ── Scenario 6: score arm (severity >= escalation level) routes to the gate ──

run_case critical "critical production problem" "$APPROVE_ESC"
require_equal "$(first_arm critical)" "match[2]" "score severity arm (match[2]) fires"
require_equal "$(final_state critical)" "escalated" "approved critical escalation lands on escalated"

# ── Scenario 7: backend unreachable -> failure outcome -> decision_failed ────

kill "$STUB_PID" 2>/dev/null || true
STUB_PID=""
sleep 1
run_case backend_down "server returns 500 on checkout"
require_equal "$(final_state backend_down)" "decision_failed" "backend failure routes to decision_failed"
[ "$RC_EXIT" -ne 0 ] || fail "backend-failure run should exit non-zero (failure terminal)"

echo
if [ "$FAILED" -eq 0 ]; then
    echo "PASS: decision demo runtime test (7 scenarios)"
else
    echo "FAILED: $FAILED assertion(s)" >&2
    exit 1
fi