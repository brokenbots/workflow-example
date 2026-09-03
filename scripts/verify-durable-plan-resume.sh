#!/usr/bin/env bash
set -euo pipefail

# Manual verification script for CRI-90: durable QA plan persistence and resume.
# Exercises the file-detection and copy logic used by qa_triage_v1 and
# linear_intake_v1 without requiring a full Criteria apply run.

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT

INTAKE_ROOT="$TMPDIR/intake"
TRIAGE_ROOT="$TMPDIR/triage"
TICKET_ID="CRI-TEST"
mkdir -p "$INTAKE_ROOT/$TICKET_ID/workstreams"
mkdir -p "$TRIAGE_ROOT/$TICKET_ID/evidence"

# Render a Criteria template file by substituting the simple placeholders used
# by these scripts. Test values are chosen to contain no shell metacharacters,
# so direct substitution is safe for this verification.
render_template() {
    local src="$1" intake_root="$2" ticket_id="$3" run_dir="$4" approved_plan_file="$5"
    # Use '~' as the sed delimiter because the template patterns contain '|'.
    sed \
        -e "s~{{ .intake_root | shellquote }}~$intake_root~g" \
        -e "s~{{ .ticket_id | shellquote }}~$ticket_id~g" \
        -e "s~{{ .run_dir | shellquote }}~$run_dir~g" \
        -e "s~{{ .approved_plan_file | shellquote }}~$approved_plan_file~g" \
        "$src"
}

echo "==> Compile both workflows"
criteria compile "$REPO_ROOT/qa_triage_v1" >/dev/null
criteria compile "$REPO_ROOT/linear_intake_v1" >/dev/null
echo "    ok: both workflows compile"

echo "==> linear_intake_v1 check: no ticket.json -> unknown"
rendered="$TMPDIR/check-state-unknown.sh"
render_template "$REPO_ROOT/linear_intake_v1/scripts/check_ticket_state.sh.tftpl" "$INTAKE_ROOT" "$TICKET_ID" "" "" > "$rendered"
chmod +x "$rendered"
result=$("$rendered")
[ "$result" = "unknown" ]
echo "    ok: returns unknown when ticket.json is missing"

echo "==> linear_intake_v1 check: completed ticket -> terminal"
jq -n '{data: {issue: {state: {type: "completed"}}}}' > "$INTAKE_ROOT/$TICKET_ID/ticket.json"
rendered="$TMPDIR/check-state-terminal.sh"
render_template "$REPO_ROOT/linear_intake_v1/scripts/check_ticket_state.sh.tftpl" "$INTAKE_ROOT" "$TICKET_ID" "" "" > "$rendered"
chmod +x "$rendered"
result=$("$rendered")
[ "$result" = "terminal" ]
echo "    ok: returns terminal for completed state"

echo "==> linear_intake_v1 check: started ticket -> open"
jq -n '{data: {issue: {state: {type: "started"}}}}' > "$INTAKE_ROOT/$TICKET_ID/ticket.json"
rendered="$TMPDIR/check-state-open.sh"
render_template "$REPO_ROOT/linear_intake_v1/scripts/check_ticket_state.sh.tftpl" "$INTAKE_ROOT" "$TICKET_ID" "" "" > "$rendered"
chmod +x "$rendered"
result=$("$rendered")
[ "$result" = "open" ]
echo "    ok: returns open for nonterminal state"

echo "==> linear_intake_v1 check: no approved plan -> fetch"
rendered="$TMPDIR/check-intake-fetch.sh"
render_template "$REPO_ROOT/linear_intake_v1/scripts/check_approved_plan.sh.tftpl" "$INTAKE_ROOT" "$TICKET_ID" "" "" > "$rendered"
chmod +x "$rendered"
result=$("$rendered")
[ "$result" = "fetch" ]
echo "    ok: returns fetch when no plan exists"

echo "==> qa_triage_v1 check: no approved plan -> fresh"
rendered="$TMPDIR/check-qa-fresh.sh"
render_template "$REPO_ROOT/qa_triage_v1/scripts/check_approved_plan.sh.tftpl" "" "" "$TRIAGE_ROOT/$TICKET_ID" "$INTAKE_ROOT/$TICKET_ID/approved-plan.md" > "$rendered"
chmod +x "$rendered"
result=$("$rendered")
[ "$result" = "fresh" ]
echo "    ok: returns fresh when no plan exists"

echo "==> qa_triage_v1 persist approved plan"
echo "fake approved plan" > "$TRIAGE_ROOT/$TICKET_ID/evidence/plan.md"
rendered="$TMPDIR/persist-qa.sh"
render_template "$REPO_ROOT/qa_triage_v1/scripts/persist_approved_plan.sh.tftpl" "" "" "$TRIAGE_ROOT/$TICKET_ID" "$INTAKE_ROOT/$TICKET_ID/approved-plan.md" > "$rendered"
chmod +x "$rendered"
"$rendered" >/dev/null
[ -f "$INTAKE_ROOT/$TICKET_ID/approved-plan.md" ]
[ -f "$INTAKE_ROOT/$TICKET_ID/approved-plan.json" ]
diff -q "$TRIAGE_ROOT/$TICKET_ID/evidence/plan.md" "$INTAKE_ROOT/$TICKET_ID/approved-plan.md" >/dev/null
echo "    ok: plan and metadata persisted"

echo "==> linear_intake_v1 check: approved plan + report -> resume"
echo "fake bug report" > "$INTAKE_ROOT/$TICKET_ID/$TICKET_ID.md"
rendered="$TMPDIR/check-intake-resume.sh"
render_template "$REPO_ROOT/linear_intake_v1/scripts/check_approved_plan.sh.tftpl" "$INTAKE_ROOT" "$TICKET_ID" "" "" > "$rendered"
chmod +x "$rendered"
result=$("$rendered")
[ "$result" = "resume" ]
echo "    ok: returns resume when approved plan and report exist"

echo "==> qa_triage_v1 check: approved plan exists -> restored"
rm -f "$TRIAGE_ROOT/$TICKET_ID/evidence/plan.md"
rendered="$TMPDIR/check-qa-restored.sh"
render_template "$REPO_ROOT/qa_triage_v1/scripts/check_approved_plan.sh.tftpl" "" "" "$TRIAGE_ROOT/$TICKET_ID" "$INTAKE_ROOT/$TICKET_ID/approved-plan.md" > "$rendered"
chmod +x "$rendered"
result=$("$rendered")
[ "$result" = "restored" ]
[ -f "$TRIAGE_ROOT/$TICKET_ID/evidence/plan.md" ]
diff -q "$INTAKE_ROOT/$TICKET_ID/approved-plan.md" "$TRIAGE_ROOT/$TICKET_ID/evidence/plan.md" >/dev/null
echo "    ok: returns restored and copies plan into evidence"

echo "==> linear_intake_v1 check: published workstream blocks resume"
echo "fake workstream" > "$INTAKE_ROOT/$TICKET_ID/workstreams/$TICKET_ID.md"
rendered="$TMPDIR/check-intake-workstream.sh"
render_template "$REPO_ROOT/linear_intake_v1/scripts/check_approved_plan.sh.tftpl" "$INTAKE_ROOT" "$TICKET_ID" "" "" > "$rendered"
chmod +x "$rendered"
result=$("$rendered")
[ "$result" = "fetch" ]
echo "    ok: returns fetch when a workstream already exists"

echo ""
echo "All durable-plan persistence/resume checks passed."
