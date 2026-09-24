#!/usr/bin/env bash
set -euo pipefail

# kanboard_develop_v1 is the development phase of the Kanboard intake split: a
# port of linear_develop_v1 (CRI-239) to the Kanboard JSON-RPC API, fired on a
# TRIAGED (Ready-column) task. This test guards the port:
#
#   1. the tree validates standalone and references no linear_* tree asset;
#   2. the graph has exactly three terminal states — two success
#      (handler_complete, awaiting_human) and one failed bookkeeping
#      terminal (CRI-275 semantics: a parking comment or column move that
#      does not land records the run failed so the watcher can refire/alert)
#      — all reachable, its only subworkflow is workstream_handler_v1, and no
#      triage symbol appears anywhere in it — no triage step is reachable,
#      the confirmed workstream is the workflow's input;
#   3. the confirmed-workstream write-or-reuse gate is deterministic on the
#      real ticket.json shape (the {task, tags, comments} envelope
#      fetch_ticket stores): an existing published workstream is reused
#      byte-for-byte untouched, an absent/empty one is assembled from the
#      task and its comments, and a title-less task fails loudly;
#   4. the develop edge wiring is exactly the linear develop path's shape
#      (fetch -> workstream gate -> started comment -> work column ->
#      handler -> done comment -> done column | failure comments -> review
#      column), with CRI-275's bookkeeping-failure edges ending in the
#      failed terminal;
#   5. the linear_* source trees are untouched.

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

# Plain validate, matching the Makefile gate: the composed handler subtree
# carries known informational warnings (see below). The tree must contribute
# none of them itself.
"$CRITERIA" validate "$TREE_ROOT" >/dev/null 2>"$TMP/validate.err" \
    || { echo "FAIL: criteria validate: $(cat "$TMP/validate.err")" >&2; exit 1; }
ok "criteria validate passes standalone"

# The develop tree itself contributes zero warnings: any warning must point
# into workstream_handler_v1 (pre-existing, informational, shared with
# linear_intake_v1 — a write that re-reads its own step-entry snapshot).
if grep '^Warning:' "$TMP/validate.err" | grep "kanboard_develop_v1" >/dev/null 2>&1; then
    grep '^Warning:' "$TMP/validate.err" grep "kanboard_develop_v1" >&2
    fail "validation reports warnings originating in the develop tree"
else
    ok "develop tree contributes no validation warnings"
fi

# No runtime dependency on the sibling trees: no path into either anywhere
# (prose mentions in comments are fine; structural embedding is already
# guarded by the compiled-graph grep below).
if grep -rn -e "linear_intake_v1/" -e "linear_triage_v1/" -e "linear_develop_v1/" \
    --include="*.chcl" --include="*.sh.tftpl" --include="*.md.tftpl" "$TREE_ROOT"; then
    fail "develop tree references a linear_* tree's files — trees must be independently runnable"
else
    ok "no file path into any linear_* tree"
fi

# ── 2. Graph structure ───────────────────────────────────────────────────────

"$CRITERIA" compile "$TREE_ROOT" --format json --out "$TMP/graph.json" 2>"$TMP/compile.err" \
    || { echo "FAIL: criteria compile: $(cat "$TMP/compile.err")" >&2; exit 1; }
GRAPH="$TMP/graph.json"

terminals="$(jq -r '[.states[] | select(.terminal)] | length' "$GRAPH")"
[ "$terminals" -eq 3 ] || fail "expected exactly 3 terminal states, got $terminals"

terminal_names="$(jq -r '[.states[] | select(.terminal) | .name] | sort | join(" ")' "$GRAPH")"
require_equal "$terminal_names" "awaiting_human failed handler_complete" "terminal states are exactly awaiting_human, failed and handler_complete"

# CRI-275: terminal success flags reflect delivery state. The delivery and
# handoff terminals stay success (the PR merged, or the ticket was parked
# In Review with an accurate comment); a bookkeeping failure — a parking
# comment or a state move that did not land — records the run failed so the
# watcher raises the dirty label and can refire the ticket.
for t in handler_complete awaiting_human; do
    jq -e --arg t "$t" '.states[] | select(.terminal and .name == $t and .success)' "$GRAPH" >/dev/null \
        || fail "terminal $t must be success=true"
done
ok "delivery/handoff terminals are success=true"
jq -e '.states[] | select(.terminal and .name == "failed" and (.success | not))' "$GRAPH" >/dev/null \
    || fail "terminal failed must be success=false: bookkeeping failures must be recorded as run failures"
ok "bookkeeping-failure terminal is success=false"

# Every step and terminal reachable from initial_state; both terminal
# outcomes reachable is asserted by membership here.
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

for state_name in awaiting_human failed handler_complete; do
    if [ "${reachable[$state_name]+set}" = "set" ]; then
        ok "terminal outcome reachable: $state_name"
    else
        fail "terminal outcome not reachable from initial_state: $state_name"
    fi
done

# Only cross-tree dependency allowed: the workstream_handler_v1 subworkflow
# (develop -> PR -> reviewer loop -> merge lives there).
subwf_count="$(jq -r '.subworkflows | length' "$GRAPH")"
[ "$subwf_count" -eq 1 ] || fail "expected exactly 1 subworkflow (workstream_handler_v1), got $subwf_count"
subwf_name="$(jq -r '.subworkflows[0].body.name // ""' "$GRAPH")"
require_equal "$subwf_name" "workstream_handler_v1" "subworkflow is workstream_handler_v1"

# No triage coupling anywhere in the compiled graph: no intake classification,
# no internal-reproduced gate, no QA triage subworkflow, no triage reviewer,
# no classification labels, no path into the sibling trees. The confirmed
# workstream is this workflow's input, not something it re-derives.
forbidden='qa_triage_v1|write_bug_report|classify_ticket|check_internal_label|review_qa_output|triage_reviewer|set_triage_state|linear_triage_state|linear_bug_label|linear_feature_label|set_classification_label|comment_triage_failed|linear_intake_v1|linear_triage_v1'
if grep -Eq "$forbidden" "$GRAPH"; then
    grep -En "$forbidden" "$GRAPH" >&2
    fail "compiled graph contains triage-path symbols — no triage step may be reachable"
else
    ok "no triage-path symbols in the compiled graph"
fi

# No routing switches at all: nothing to classify, nothing to gate on labels.
switch_count="$(jq -r '.switches | length' "$GRAPH")"
[ "$switch_count" -eq 0 ] || fail "expected 0 switches (no triage gates), got $switch_count"

# The step set is exactly the develop path — no intake/triage steps.
step_names="$(jq -r '[.steps[] | .name] | sort | join(" ")' "$GRAPH")"
expected_steps="comment_develop_failed comment_done_move_failed comment_handler_done comment_handler_failed comment_handler_started ensure_confirmed_workstream fetch_ticket park_after_done_move_failed run_handler set_done_state set_review_state set_work_state"
require_equal "$step_names" "$expected_steps" "step set is exactly the develop path"

# The confirmed-workstream gate is deterministic shell — no model tools.
gate_tools="$(jq -r '.steps[] | select(.name == "ensure_confirmed_workstream") | .allow_tools | length' "$GRAPH")"
[ "$gate_tools" -eq 0 ] || fail "ensure_confirmed_workstream must not allow model tools, got $gate_tools"

# The handler's reviewer identity must be threaded through (independent
# approval gate): reviewer_github_token wired from a dedicated reviewer
# variable into the subworkflow input. Input bindings are not serialized
# into the compiled graph, so this is asserted on the workflow source.
if grep -q 'reviewer_github_token = var.reviewer_github_token' "$TREE_ROOT/main.chcl" \
    && grep -q 'variable "reviewer_github_token"' "$TREE_ROOT/variables.chcl"; then
    ok "reviewer_github_token threaded into the handler wiring"
else
    fail "reviewer_github_token missing from the handler wiring — the handler would review with the author identity"
fi

# Develop edge wiring: success and failure paths, matching the intake
# develop path's shape.
assert_edge() {
    local from="$1" outcome="$2" to="$3"
    local got
    got="$(jq -r --arg from "$from" --arg outcome "$outcome" \
        '.steps[] | select(.name == $from) | .outcomes[] | select(.name == $outcome) | .next' "$GRAPH")"
    require_equal "$got" "$to" "edge $from.$outcome -> $to"
}

assert_edge "fetch_ticket" "success" "ensure_confirmed_workstream"
assert_edge "fetch_ticket" "failure" "awaiting_human"
assert_edge "ensure_confirmed_workstream" "success" "comment_handler_started"
assert_edge "ensure_confirmed_workstream" "failure" "comment_develop_failed"
assert_edge "comment_handler_started" "success" "set_work_state"
assert_edge "comment_handler_started" "failure" "set_work_state"
assert_edge "set_work_state" "success" "run_handler"
assert_edge "set_work_state" "failure" "comment_develop_failed"
assert_edge "run_handler" "success" "comment_handler_done"
assert_edge "run_handler" "failure" "comment_handler_failed"
# CRI-275: bookkeeping-failure routing. A parking comment or state move that
# fails ends the run in the failed terminal (success=false), mirroring
# linear_intake_v1's bookkeeping-failure semantics — including the closing
# comment on the post-merge path (comment_handler_done), whose failure skips
# the Done move entirely. The Done-move-failed route keeps its accurate
# fallback comment, parks In Review, and still ends failed — the merged-PR
# bookkeeping failed even when the parking worked.
assert_edge "comment_handler_done" "success" "set_done_state"
assert_edge "comment_handler_done" "failure" "failed"
assert_edge "set_done_state" "success" "handler_complete"
assert_edge "set_done_state" "failure" "comment_done_move_failed"
assert_edge "comment_handler_failed" "success" "set_review_state"
assert_edge "comment_handler_failed" "failure" "failed"
assert_edge "comment_develop_failed" "success" "set_review_state"
assert_edge "comment_develop_failed" "failure" "failed"
assert_edge "comment_done_move_failed" "success" "park_after_done_move_failed"
assert_edge "comment_done_move_failed" "failure" "failed"
assert_edge "park_after_done_move_failed" "success" "failed"
assert_edge "park_after_done_move_failed" "failure" "failed"
assert_edge "set_review_state" "success" "awaiting_human"
assert_edge "set_review_state" "failure" "failed"

# ── 3. Confirmed-workstream gate on real fixture shapes ──────────────────────

# fetch_ticket stores the enveloped GraphQL response; the gate must read
# through .data.issue. Rendering mimics templatefile for the two variables
# the script consumes, including the engine's shellquote (single-quote
# wrapping with '\'' escaping).
SLUG="KB-140"
shquote() {
    printf "'%s'" "${1//\'/\'\\\'\'}"
}
render() {
    sed -e "s@{{ .criteria_value_1 | shellquote }}@$(shquote "$1")@g" \
        -e "s@{{ .criteria_value_2 | shellquote }}@$(shquote "$SLUG")@g" "$2" > "$3"
}
run_dir="$TMP/intake/$SLUG"
mkdir -p "$run_dir"

ws_script="$TMP/ensure_confirmed_workstream.sh"
render "$TMP/intake" "$TREE_ROOT/scripts/ensure_confirmed_workstream.sh.tftpl" "$ws_script"
chmod +x "$ws_script"

workstream="$run_dir/workstreams/$SLUG.md"

# Absent workstream + triaged ticket with triage outputs in comments:
# assembled mechanically from the ticket and its comments.
cp "$TREE_ROOT/tests/fixtures/task_ready.json" "$run_dir/ticket.json"
if "$ws_script" > "$TMP/ws.out" 2>&1; then
    ok "ensure_confirmed_workstream assembles the workstream"
else
    fail "ensure_confirmed_workstream failed: $(cat "$TMP/ws.out")"
fi
[ -s "$workstream" ] || fail "workstream missing or empty: $workstream"
if [ -s "$workstream" ] \
    && grep -q "# Workstream: Deploy job races the cache warm step" "$workstream" \
    && grep -q "Kanboard task KB-140" "$workstream" \
    && grep -q "Board column at development start: 6" "$workstream" \
    && grep -q "partially warmed cache" "$workstream" \
    && grep -q "cache_warm_wait_gate" "$workstream" \
    && grep -q "rr-5173" "$workstream" \
    && grep -q "## Required behavior" "$workstream"; then
    ok "workstream carries title, source, state, description and comment triage evidence"
else
    fail "workstream is missing ticket content or triage evidence from comments"
fi

# Reuse: an existing published workstream is the triage run's conclusion and
# must pass through byte-for-byte untouched. (CRI-239 exit criterion: written
# when absent, reused when present, never overwriting triage conclusions.)
sentinel="# TRIAGE CONCLUSION — DO NOT OVERWRITE
cached verdict: reproduced"
printf '%s\n' "$sentinel" > "$workstream"
before="$(md5sum "$workstream" | cut -d' ' -f1)"
if out="$("$ws_script")" && [ "$out" = "workstream reused: $workstream" ]; then
    ok "existing workstream is reused (semantic source line)"
else
    fail "existing workstream: got '${out:-<no output>}', want 'workstream reused: $workstream'"
fi
after="$(md5sum "$workstream" | cut -d' ' -f1)"
require_equal "$before" "$after" "reused workstream is byte-for-byte untouched"

# An empty existing file is not a conclusion: assembled.
: > "$workstream"
if out="$("$ws_script")" && [ "$out" = "workstream written: $workstream" ]; then
    ok "empty workstream file is assembled (written)"
else
    fail "empty workstream file: got '${out:-<no output>}', want 'workstream written: $workstream'"
fi
[ -s "$workstream" ] || fail "workstream missing or empty after assembly: $workstream"

# Determinism: the same input yields the same file.
assembled="$(md5sum "$workstream" | cut -d' ' -f1)"
"$ws_script" >/dev/null
require_equal "$assembled" "$(md5sum "$workstream" | cut -d' ' -f1)" "assembly is deterministic across repeated runs"

# Missing ticket.json and no workstream: fail loudly (the workflow routes the
# failure to the develop-failed comment and the review state).
rm -f "$run_dir/ticket.json" "$workstream"
if "$ws_script" >/dev/null 2>&1; then
    fail "missing ticket.json must fail loudly"
else
    ok "missing ticket.json fails loudly"
fi

# A title-less ticket cannot carry a workstream header: fail loudly.
cp "$TREE_ROOT/tests/fixtures/task_notitle.json" "$run_dir/ticket.json"
if "$ws_script" >/dev/null 2>&1; then
    fail "title-less ticket must fail loudly"
else
    ok "title-less ticket fails loudly"
fi

# ── 4. Sibling trees untouched ───────────────────────────────────────────────

untouched="$(git -C "$REPO_ROOT" status --porcelain -- linear_intake_v1 linear_triage_v1 linear_develop_v1 qa_triage_v1 workstream_handler_v1)"
if [ -z "$untouched" ]; then
    ok "linear_* and shared subworkflow trees are untouched (no uncommitted changes)"
else
    fail "linear_*/shared trees have uncommitted changes: $untouched"
fi

if [ "$FAILED" -gt 0 ]; then
    echo "FAILED: $FAILED assertion(s)" >&2
    exit 1
fi

echo "all develop standalone tests passed"
