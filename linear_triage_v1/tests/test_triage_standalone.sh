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
#   5. linear_intake_v1 itself is untouched and keeps its develop path;
#   6. the ready_for_development terminal path re-arms the development route
#      (CRI-240): the re-arm step is reached only from the confirmed-finding
#      gates (no awaiting_human path reaches it), its merged label write
#      preserves every existing label against a mock Linear API, and a
#      missing arming label or failed write fails loudly.

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

# ── 5. Ready-for-development re-arm wiring (CRI-240) ─────────────────────────

# The re-arm steps exist in the compiled graph.
for step_name in rearm_k8s_run comment_rearm_failed; do
    jq -e --arg s "$step_name" '.steps[] | select(.name == $s)' "$GRAPH" >/dev/null \
        || fail "re-arm step missing from the compiled graph: $step_name"
done
ok "re-arm steps present in the compiled graph"

assert_edge() {
    local from="$1" outcome="$2" to="$3"
    local got
    got="$(jq -r --arg from "$from" --arg outcome "$outcome" \
        '.steps[] | select(.name == $from) | .outcomes[] | select(.name == $outcome) | .next' "$GRAPH")"
    require_equal "$got" "$to" "edge $from.$outcome -> $to"
}

assert_edge "set_confirmed_bug_label" "success" "rearm_k8s_run"
assert_edge "rearm_k8s_run" "success" "set_ready_state"
assert_edge "rearm_k8s_run" "failure" "comment_rearm_failed"
assert_edge "comment_rearm_failed" "success" "set_review_state"
assert_edge "comment_rearm_failed" "failure" "set_review_state"
assert_edge "set_ready_state" "success" "ready_for_development"

route_ready_next="$(jq -r '.switches[] | select(.name == "route_ready") | .conditions[0].next' "$GRAPH")"
require_equal "$route_ready_next" "rearm_k8s_run" "route_ready match routes to the re-arm step"

# Every edge into a node, for reachability-style assertions below.
predecessors() {
    jq -r --arg to "$1" '
        [ (.steps[] | .name as $s | [.outcomes[]? | {from: $s, to: .next}])
        , (.switches[] | .name as $s
            | [.conditions[]? | {from: $s, to: .next}]
            + [(.default_next // empty) as $d | {from: $s, to: $d}])
        ]
        | flatten[]
        | select(.to == $to) | .from
    ' "$GRAPH" | sort
}

# The re-arm fires on the ready_for_development outcome only:
#   - set_ready_state's ONLY predecessor is the re-arm, so every run that
#     terminates ready_for_development re-arms first;
#   - the re-arm's ONLY predecessors are the two confirmed-finding gates —
#     no awaiting_human path (comment steps, set_review_state) can reach it,
#     so In Review tickets are never re-armed: the human IS the signal.
require_equal "$(predecessors set_ready_state)" "rearm_k8s_run" \
    "every ready_for_development run passes through the re-arm step"
require_equal "$(predecessors rearm_k8s_run)" "$(printf 'route_ready\nset_confirmed_bug_label')" \
    "re-arm is reached only from the confirmed-finding gates (never from the In Review path)"

# ── 6. rearm_k8s_run: merged label write against a mock Linear API (CRI-240) ─

# The re-arm must leave the ticket carrying k8s-run IN ADDITION to every
# label it already has, so the arming write is a merge over the ticket's
# CURRENT label ids (read fresh from the API, not from the fetch-time
# ticket.json snapshot — set_confirmed_bug_label may rewrite labels during
# the run) applied with REPLACE semantics via issueUpdate(labelIds). The
# mock Linear server records every issueUpdate payload for assertion.

export LINEAR_API_KEY="mock-key"

MOCK="$TMP/mock_linear.py"
cat > "$MOCK" <<'PY'
import json
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer

CFG_FILE, MUT_FILE = sys.argv[1], sys.argv[2]


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_POST(self):
        cfg = json.load(open(CFG_FILE))
        body = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))))
        query = body.get("query", "")
        if "issueUpdate" in query:
            with open(MUT_FILE, "a") as f:
                f.write(json.dumps(body["variables"]["labelIds"]) + "\n")
            if cfg.get("mutation_status", 200) != 200:
                self.send_response(cfg["mutation_status"])
                self.end_headers()
                return
            resp = {"data": {"issueUpdate": {"success": cfg.get("mutation_success", True)}}}
        elif "issueLabels" in query:
            issue = cfg["issue"]
            resp = {"data": {
                "issue": {"labels": {"nodes": [{"id": i} for i in issue["label_ids"]]}},
                "issueLabels": {"nodes": ([{"id": cfg["run_label_id"]}]
                                          if cfg.get("run_label_present", True) else [])},
            }}
        else:
            resp = {"errors": [{"message": "unexpected query"}]}
        data = json.dumps(resp).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)


server = HTTPServer(("127.0.0.1", 0), Handler)
print(server.server_address[1], flush=True)
server.serve_forever()
PY

MOCK_CFG="$TMP/mock_config.json"
MUT_FILE="$TMP/mutations.jsonl"
MOCK_LOG="$TMP/mock.log"

python3 "$MOCK" "$MOCK_CFG" "$MUT_FILE" >"$MOCK_LOG" 2>&1 &
MOCK_PID=$!
trap 'kill "$MOCK_PID" 2>/dev/null; rm -rf "$TMP"' EXIT

PORT=""
tries=0
until [ -n "$PORT" ] || [ "$tries" -ge 50 ]; do
    PORT="$(head -n1 "$MOCK_LOG" 2>/dev/null || true)"
    tries=$((tries + 1))
    [ -n "$PORT" ] || sleep 0.1
done
if [ -z "$PORT" ]; then
    echo "FAIL: mock Linear server did not start: $(cat "$MOCK_LOG")" >&2
    exit 1
fi

# Render mimics templatefile for the shellquoted criteria_value_N variables
# and points the script's Linear endpoint at the mock server.
shquote() {
    printf "'%s'" "${1//\'/\'\\\'\'}"
}
RUN_LABEL="k8s-run"
rearm_script="$TMP/rearm_k8s_run.sh"
sed -e "s@{{ .criteria_value_1 | shellquote }}@$(shquote "$TMP/intake")@g" \
    -e "s@{{ .criteria_value_2 | shellquote }}@$(shquote "$SLUG")@g" \
    -e "s@{{ .criteria_value_3 | shellquote }}@$(shquote "$RUN_LABEL")@g" \
    -e "s@https://api.linear.app/graphql@http://127.0.0.1:$PORT/graphql@g" \
    "$TREE_ROOT/scripts/rearm_k8s_run.sh.tftpl" > "$rearm_script"
chmod +x "$rearm_script"

cp "$TREE_ROOT/tests/fixtures/ticket_labeled.json" "$run_dir/ticket.json"

cfg() {
    jq -n \
        --argjson ids "$1" \
        --arg run_id "$2" \
        --argjson ok "${3:-true}" \
        --argjson status "${4:-200}" \
        --argjson present "${5:-true}" \
        '{issue: {label_ids: $ids}, run_label_id: $run_id,
          mutation_success: $ok, mutation_status: $status, run_label_present: $present}' \
        > "$MOCK_CFG"
}

mutations() {
    cat "$MUT_FILE" 2>/dev/null || true
}

# The written label set must be exactly the union: every pre-existing label
# preserved, the arming label added, nothing else.
assert_merge() {
    local existing="$1" written="$2" desc="$3" run_label_id="$4"
    # The jq variable is deliberately not named $label: "label" is a jq >= 1.6
    # keyword (the same pitfall check_internal_label.sh.tftpl documents).
    # -n: inputs arrive only via --argjson/--arg; without it jq would read the
    # ambient stdin, which is EOF under CI runners, and jq >= 1.7 then exits 4
    # ("no valid result") without ever evaluating the comparison.
    if jq -en --argjson existing "$existing" --argjson written "$written" --arg lid "$run_label_id" '
        ($existing + [$lid] | unique) as $want
        | ($written | unique) == $want
          and ($written | length) == ($written | unique | length)
    ' >/dev/null; then
        ok "$desc"
    else
        fail "$desc: existing=$existing written=$written"
    fi
}

# Merge semantics: the mock's current label ids include l3, which is NOT in
# the fixture ticket.json snapshot — the write including l3 proves the merge
# builds on the fresh API read, not the fetch-time snapshot.
cfg '["l1", "l2", "l3"]' "l-k8s"
rm -f "$MUT_FILE"
if out="$("$rearm_script")" && [ "$out" = "ticket re-armed with $RUN_LABEL" ]; then
    ok "re-arm writes the merged label set"
else
    fail "re-arm: got '${out:-<no output>}', want 'ticket re-armed with $RUN_LABEL'"
fi
written="$(mutations)"
if [ -z "$written" ]; then
    fail "re-arm performed no label write"
else
    assert_merge '["l1", "l2", "l3"]' "$written" \
        "merged write preserves every existing label and adds $RUN_LABEL" "l-k8s"
fi
require_equal "$(mutations | grep -c .)" "1" "re-arm performs exactly one issueUpdate write"

# Idempotency: a ticket already carrying the arming label is not rewritten.
cfg '["l1", "l-k8s"]' "l-k8s"
rm -f "$MUT_FILE"
if out="$("$rearm_script")" \
    && [ "$out" = "ticket already carries $RUN_LABEL; no re-arm write needed" ]; then
    ok "already-armed ticket is a no-op"
else
    fail "already-armed ticket: got '${out:-<no output>}'"
fi
[ -z "$(mutations)" ] || fail "already-armed ticket must not be rewritten"

# The arming label missing from the team means the dev route could never
# fire: fail loudly (the workflow routes this to a human), never silently.
cfg '["l1", "l2"]' "l-k8s" true 200 false
rm -f "$MUT_FILE"
if "$rearm_script" >/dev/null 2>&1; then
    fail "missing $RUN_LABEL label on the team must fail loudly"
else
    ok "missing $RUN_LABEL label on the team fails loudly"
fi
[ -z "$(mutations)" ] || fail "missing $RUN_LABEL label must not write"

# A rejected mutation (success=false) fails the re-arm loudly.
cfg '["l1", "l2"]' "l-k8s" false
if "$rearm_script" >/dev/null 2>&1; then
    fail "issueUpdate success=false must fail the re-arm"
else
    ok "issueUpdate success=false fails the re-arm"
fi

# An HTTP error from the mutation fails the re-arm loudly.
cfg '["l1", "l2"]' "l-k8s" true 500
if "$rearm_script" >/dev/null 2>&1; then
    fail "issueUpdate HTTP error must fail the re-arm"
else
    ok "issueUpdate HTTP error fails the re-arm"
fi

# ── 7. linear_intake_v1 untouched, develop path intact ───────────────────────

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