#!/usr/bin/env bash
set -euo pipefail

# CRI-238: linear_triage_v1 is the triage phase of linear_intake_v1 extracted
# into an independently runnable workflow. This test guards the extraction:
#
#   1. the tree validates standalone and references no linear_intake_v1 asset;
#   2. the graph has exactly two terminal states, both success, both
#      reachable, and no develop steps (no handler subworkflow, no work/done
#      states, no reviewer token) — the develop path stays in
#      linear_intake_v1 until CRI-239 extracts it;
#   3. the internal-reproduced gate is deterministic on the real ticket.json
#      shape (the enveloped GraphQL response fetch_ticket stores) — the same
#      input always yields the same routing token;
#   4. the mechanical report/workstream assembly scripts produce non-empty
#      artifacts from a fetched ticket and fail loudly without one;
#   5. linear_intake_v1 itself is untouched and keeps its develop path.

TREE_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
REPO_ROOT="$(cd "$TREE_ROOT/.." && pwd)"
CRITERIA="${CRITERIA_BIN:-criteria}"

FAILED=0

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

command -v "$CRITERIA" >/dev/null 2>&1 \
    || { echo "FAIL: criteria binary '$CRITERIA' not found (set CRITERIA_BIN)" >&2; exit 1; }
command -v jq >/dev/null 2>&1 \
    || { echo "FAIL: jq not found" >&2; exit 1; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# ── 1. Standalone validation ─────────────────────────────────────────────────

"$CRITERIA" validate "$TREE_ROOT" --warnings-as-errors >/dev/null 2>"$TMP/validate.err" \
    || { echo "FAIL: criteria validate --warnings-as-errors: $(cat "$TMP/validate.err")" >&2; exit 1; }
ok "criteria validate passes standalone (warnings as errors)"

# No runtime dependency on the intake tree: no path into linear_intake_v1
# anywhere (prose mentions in comments are fine; structural embedding is
# already guarded by the compiled-graph grep below).
if grep -rn "linear_intake_v1/" --exclude="test_triage_standalone.sh" "$TREE_ROOT"; then
    fail "triage tree references linear_intake_v1 files — trees must be independently runnable"
else
    ok "no file path into the linear_intake_v1 tree"
fi

# ── 2. Graph structure ───────────────────────────────────────────────────────

"$CRITERIA" compile "$TREE_ROOT" --format json --out "$TMP/graph.json" 2>"$TMP/compile.err" \
    || { echo "FAIL: criteria compile: $(cat "$TMP/compile.err")" >&2; exit 1; }
GRAPH="$TMP/graph.json"

terminals="$(jq -r '[.states[] | select(.terminal)] | length' "$GRAPH")"
[ "$terminals" -eq 2 ] || fail "expected exactly 2 terminal states, got $terminals"

terminal_names="$(jq -r '[.states[] | select(.terminal) | .name] | sort | join(" ")' "$GRAPH")"
require_equal "$terminal_names" "awaiting_human ready_for_development" "terminal states are exactly awaiting_human and ready_for_development"

jq -e '[.states[] | select(.terminal) | .success] | all' "$GRAPH" >/dev/null \
    || fail "terminal states must both be success=true: every run ends by handing the ticket to a human or declaring it ready"

# Every step, switch and terminal reachable from initial_state; both terminal
# outcomes reachable in tests is asserted by membership here.
jq -r '
  [ (.steps[] | .name as $s | [.outcomes[]? | {from: $s, to: .next}])
  , (.switches[] | .name as $s
      | [.conditions[]? | {from: $s, to: .next}]
      + [(.default_next // empty) as $d | {from: $s, to: $d}])
  ]
  | flatten[]
  | [.from, .to] | @tsv
' "$GRAPH" > "$TMP/edges.tsv"

# Breadth-first search over the edge list from the initial state.
initial="$(jq -r '.initial_state' "$GRAPH")"
declare -A reachable
reachable["$initial"]=1
frontier=("$initial")

while [ "${#frontier[@]}" -gt 0 ]; do
    next_frontier=()
    for node in "${frontier[@]}"; do
        while IFS=$'\t' read -r from to; do
            [ "$from" = "$node" ] || continue
            [ -n "$to" ] || continue
            [ "${reachable[$to]+set}" = "set" ] && continue
            reachable["$to"]=1
            next_frontier+=("$to")
        done < "$TMP/edges.tsv"
    done
    frontier=("${next_frontier[@]}")
done

unreachable_steps="$(jq -r '.steps[] | .name' "$GRAPH" | while read -r s; do
    [ "${reachable[$s]+set}" = "set" ] || echo "$s"
done)"
[ -z "$unreachable_steps" ] || fail "steps not reachable from initial_state: $unreachable_steps"

for state_name in awaiting_human ready_for_development; do
    if [ "${reachable[$state_name]+set}" = "set" ]; then
        ok "terminal outcome reachable: $state_name"
    else
        fail "terminal outcome not reachable from initial_state: $state_name"
    fi
done

# Only cross-tree dependency allowed: the qa_triage_v1 subworkflow.
subwf_count="$(jq -r '.subworkflows | length' "$GRAPH")"
[ "$subwf_count" -eq 1 ] || fail "expected exactly 1 subworkflow (qa_triage), got $subwf_count"
subwf_name="$(jq -r '.subworkflows[0].body.name // ""' "$GRAPH")"
require_equal "$subwf_name" "qa_triage_v1" "subworkflow is qa_triage_v1"

# No develop coupling anywhere in the compiled graph. "run_handler" is quoted
# exactly so the documented reviewer outcome token valid_run_handler does not
# match.
if grep -Eq 'workstream_handler_v1|"run_handler"|comment_handler|set_work_state|set_done_state|linear_work_state|linear_done_state|reviewer_github_token|classify_ticket|check_ticket_state|linear_intake_v1' "$GRAPH"; then
    grep -En 'workstream_handler_v1|"run_handler"|comment_handler|set_work_state|set_done_state|linear_work_state|linear_done_state|reviewer_github_token|classify_ticket|check_ticket_state|linear_intake_v1' "$GRAPH" >&2
    fail "compiled graph contains develop-path symbols"
else
    ok "no develop-path symbols in the compiled graph"
fi

# The routing gate keeps intake's semantics: labeled -> workstream path,
# default -> QA triage.
gate_match="$(jq -r '.switches[] | select(.name == "route_internal_label") | .conditions[0].match' "$GRAPH")"
require_equal "$gate_match" "data.internal.internal_reproduced.value" "route_internal_label matches the internal-reproduced data write"
gate_next="$(jq -r '.switches[] | select(.name == "route_internal_label") | .conditions[0].next' "$GRAPH")"
require_equal "$gate_next" "write_confirmed_workstream" "internal-reproduced routes to the confirmed-workstream path"

# ── 3. Gate determinism on real fixture shapes ───────────────────────────────

# fetch_ticket stores the enveloped GraphQL response; the gate must read
# through .data.issue. Rendering mimics templatefile for the two variables
# these scripts consume.
SLUG="CRI-99"
render() {
    sed -e "s|{{ .intake_root }}|$1|g" -e "s|{{ .ticket_id }}|$SLUG|g" "$2" > "$3"
}
run_dir="$TMP/intake/$SLUG"
mkdir -p "$run_dir"

gate_script="$TMP/check_internal_label.sh"
render "$TMP/intake" "$TREE_ROOT/scripts/check_internal_label.sh.tftpl" "$gate_script"
chmod +x "$gate_script"

assert_gate() {
    local fixture="$1" expected="$2" label="$3" out1 out2
    cp "$TREE_ROOT/tests/fixtures/$fixture" "$run_dir/ticket.json"
    out1="$("$gate_script")"
    out2="$("$gate_script")"
    if [ "$out1" = "$expected" ] && [ "$out2" = "$expected" ]; then
        ok "$label (deterministic across repeated runs)"
    else
        fail "$label: got '$out1' then '$out2', want '$expected' both times"
    fi
}

assert_gate "ticket_labeled.json" "skip_triage" "labeled ticket routes skip_triage"
assert_gate "ticket_unlabeled.json" "run_triage" "unlabeled ticket routes run_triage"

rm -f "$run_dir/ticket.json"
if out="$(cd "$TMP" && "$gate_script")" && [ "$out" = "run_triage" ]; then
    ok "missing ticket.json fails safe to run_triage"
else
    fail "missing ticket.json: gate returned '${out:-<no output>}'"
fi

# ── 4. Report and workstream assembly ────────────────────────────────────────

report_script="$TMP/write_bug_report.sh"
render "$TMP/intake" "$TREE_ROOT/scripts/write_bug_report.sh.tftpl" "$report_script"
chmod +x "$report_script"

cp "$TREE_ROOT/tests/fixtures/ticket_labeled.json" "$run_dir/ticket.json"
if "$report_script" > "$TMP/report.out" 2>&1; then
    ok "write_bug_report assembles a report"
else
    fail "write_bug_report failed: $(cat "$TMP/report.out")"
fi
report="$run_dir/$SLUG.md"
[ -s "$report" ] || fail "bug report missing or empty: $report"
if [ -s "$report" ] \
    && grep -q "# Bug report: Broken gate on empty payload" "$report" \
    && grep -q "nil map read" "$report" \
    && grep -q "panic: assignment to entry in nil map" "$report"; then
    ok "bug report carries title, description and comment evidence"
else
    fail "bug report is missing ticket content"
fi

cp "$TREE_ROOT/tests/fixtures/ticket_notitle.json" "$run_dir/ticket.json"
if "$report_script" >/dev/null 2>&1; then
    fail "write_bug_report must fail loudly when the ticket has no title"
else
    ok "write_bug_report fails loudly on a title-less ticket"
fi

ws_script="$TMP/write_confirmed_workstream.sh"
render "$TMP/intake" "$TREE_ROOT/scripts/write_confirmed_workstream.sh.tftpl" "$ws_script"
chmod +x "$ws_script"

cp "$TREE_ROOT/tests/fixtures/ticket_labeled.json" "$run_dir/ticket.json"
if "$ws_script" > "$TMP/ws.out" 2>&1; then
    ok "write_confirmed_workstream assembles the workstream"
else
    fail "write_confirmed_workstream failed: $(cat "$TMP/ws.out")"
fi
workstream="$run_dir/workstreams/$SLUG.md"
[ -s "$workstream" ] || fail "workstream missing or empty: $workstream"
if [ -s "$workstream" ] \
    && grep -q "# Workstream: Broken gate on empty payload" "$workstream" \
    && grep -q "https://linear.app/brokenbots/issue/CRI-99" "$workstream" \
    && grep -q "nil map read" "$workstream" \
    && grep -q "rr-4821" "$workstream" \
    && grep -q "## Required behavior" "$workstream"; then
    ok "workstream carries title, source, description and comment evidence"
else
    fail "workstream is missing ticket content"
fi

# ── 5. linear_intake_v1 untouched, develop path intact ───────────────────────

untouched="$(git -C "$REPO_ROOT" status --porcelain -- linear_intake_v1 qa_triage_v1)"
if [ -z "$untouched" ]; then
    ok "linear_intake_v1 and qa_triage_v1 are untouched (no uncommitted changes)"
else
    fail "linear_intake_v1/qa_triage_v1 have uncommitted changes: $untouched"
fi

intake_main="$REPO_ROOT/linear_intake_v1/main.chcl"
develop_markers=("subworkflow \"handler\"" "workstream_handler_v1" "linear_work_state" "linear_done_state" "state \"handler_complete\"")
for marker in "${develop_markers[@]}"; do
    if grep -q "$marker" "$intake_main"; then
        ok "intake develop path intact: $marker"
    else
        fail "intake develop path marker missing: $marker (CRI-239 owns its removal)"
    fi
done

if [ "$FAILED" -gt 0 ]; then
    echo "FAILED: $FAILED assertion(s)" >&2
    exit 1
fi
echo "PASS: linear_triage_v1 standalone extraction verified"