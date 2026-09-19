#!/usr/bin/env bash
# Regression test for k8s/create-ready-for-development-state.sh (CRI-241).
#
# The state creation is the deploy-time pairing half required before the
# criteria-develop route and the watcher rollout can go live: the triage
# workflow's re-arm step (CRI-240) moves tickets to "Ready for Development",
# which only works once the state exists on the Linear team.
#
# shellcheck disable=SC2016

set -euo pipefail

# The mock Linear API below emulates the LIVE schema (captured via
# introspection against api.linear.app), not the script's assumptions:
#   - WorkflowStateCreateInput exposes inputFields (an INPUT_OBJECT has no
#     "fields"), with name/type/color/teamId NON_NULL; a probe answering in
#     the fabricated "fields" shape must make the script fail loudly.
#   - There is NO WorkflowStateType enum (WorkflowState.type is String!).
#   - The Team type has NO workflowStates field -- a lookup written as
#     team(id:) { workflowStates } must be rejected with the live GraphQL
#     error, so only the ROOT workflowStates(filter:...) query can succeed.
#   - The create mutation must supply color (required String!, no default);
#     a mutation omitting it is rejected with the live required-field error.
#   - workflowStateCreate returns WorkflowStatePayload {success,
#     workflowState {id name type color}}.
#
# Cases: happy path (probe-first ordering, create input incl. color,
# root-form lookups, live-shape create response, verify-after-create);
# idempotency; terminal-state refusal; probe failure (required inputField
# absent); probe answering in the fabricated "fields" shape; create
# success=false; verify-after-create failure; HTTP 500; missing
# LINEAR_API_KEY with zero requests; and cross-file consistency of the
# "Ready for Development" state name (triage variable <-> script constant
# <-> route states).

# GraphQL query strings in the mock-contract probes below deliberately
# contain $variables that must NOT be expanded by the shell.
# shellcheck disable=SC2016

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SCRIPT="$REPO_ROOT/k8s/create-ready-for-development-state.sh"
TMP="$(mktemp -d)"

fail() {
    echo "FAIL: $1" >&2
    exit 1
}
ok() {
    echo "ok: $1"
}

command -v python3 >/dev/null 2>&1 || fail "python3 is required"
command -v jq >/dev/null 2>&1 || fail "jq is required"
[ -f "$SCRIPT" ] || fail "k8s/create-ready-for-development-state.sh is missing"

# ── mock Linear API (live-schema emulation) ────────────────────────────────
# Probe fixture mirrors the captured live introspection of
# WorkflowStateCreateInput (inputFields with NON_NULL wrappers and
# defaultValue) and the root query/mutation field lists.
MOCK="$TMP/mock_linear.py"
MOCK_CFG="$TMP/mock_config.json"
SEQ_FILE="$TMP/requests.jsonl"
MUT_FILE="$TMP/mutations.jsonl"
STATES_Q_FILE="$TMP/states_queries.jsonl"
MOCK_LOG="$TMP/mock.log"

cat > "$MOCK" <<'PY'
import json
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer

CFG_FILE, SEQ_FILE, MUT_FILE, STATES_Q_FILE = sys.argv[1:5]

NON_NULL_STRING = {"kind": "NON_NULL", "name": None,
                   "ofType": {"kind": "SCALAR", "name": "String"}}
# Captured live inputFields of WorkflowStateCreateInput.
INPUT_FIELDS = [
    {"name": "id", "type": {"kind": "SCALAR", "name": "String", "ofType": None},
     "defaultValue": None},
    {"name": "type", "type": NON_NULL_STRING, "defaultValue": None},
    {"name": "name", "type": NON_NULL_STRING, "defaultValue": None},
    {"name": "color", "type": NON_NULL_STRING, "defaultValue": None},
    {"name": "description", "type": {"kind": "SCALAR", "name": "String", "ofType": None},
     "defaultValue": None},
    {"name": "position", "type": {"kind": "SCALAR", "name": "Float", "ofType": None},
     "defaultValue": None},
    {"name": "teamId", "type": NON_NULL_STRING, "defaultValue": None},
]
# Required create input fields, per the live schema (all NON_NULL).
REQUIRED_INPUT_FIELDS = ("name", "type", "color", "teamId")


def record(path, payload):
    with open(path, "a") as f:
        f.write(payload + "\n")


def graphqLError(message):
    return {"errors": [{"message": message}]}


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_POST(self):
        cfg = json.load(open(CFG_FILE))
        body = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))))
        query = body.get("query", "")

        if "workflowStateCreate" in query:
            record(SEQ_FILE, "create")
            inp = body.get("variables", {}).get("input", {})
            missing = [f for f in REQUIRED_INPUT_FIELDS
                       if not isinstance(inp.get(f), str) or not inp[f]]
            if missing:
                # The live API rejects a mutation omitting a required input
                # field (reproduced: Field "color" of required type
                # "String!" was not provided.).
                resp = graphqLError(
                    'Field "%s" of required type "String!" was not provided.' % missing[0])
            else:
                record(MUT_FILE, json.dumps(inp))
                if cfg.get("create_status", 200) != 200:
                    self.send_response(cfg["create_status"])
                    self.end_headers()
                    return
                if cfg.get("create_success", True):
                    created = {"id": "state-new", "name": inp["name"],
                               "type": inp["type"], "color": inp["color"]}
                    # Post-create verification uses a fresh workflowStates
                    # query; the script must fail when the created state
                    # cannot be resolved.
                    cfg["existing_state"] = created if cfg.get("verify_resolvable", True) else None
                    json.dump(cfg, open(CFG_FILE, "w"))
                    resp = {"data": {"workflowStateCreate": {
                        "success": True, "workflowState": created}}}
                else:
                    resp = {"data": {"workflowStateCreate": {
                        "success": False, "workflowState": None}}}
        elif "__type" in query or "__schema" in query:
            record(SEQ_FILE, "probe")
            resp = {"data": {
                "input": {"name": "WorkflowStateCreateInput"},
                # __schema results nest under queryType/mutationType in the
                # live introspection response.
                "rootQuery": {"queryType": {"fields": [{"name": "issues"},
                                                       {"name": "teams"},
                                                       {"name": "workflowStates"}]}},
                "rootMutation": {"mutationType": {"fields": [{"name": "issueUpdate"},
                                                             {"name": "workflowStateCreate"}]}},
            }}
            if cfg.get("probe_shape", "inputFields") == "inputFields":
                skip = cfg.get("probe_missing_fields", [])
                resp["data"]["input"]["inputFields"] = [
                    f for f in INPUT_FIELDS if f["name"] not in skip]
            else:
                # The fabricated shape the first draft assumed: INPUT_OBJECT
                # introspection answered via "fields" (always null live).
                resp["data"]["input"]["fields"] = None
        elif "teams(filter" in query:
            record(SEQ_FILE, "teams")
            resp = {"data": {"teams": {"nodes": [
                {"id": "team-1", "key": "CRI", "name": "Criteria"}]}}}
        elif "team(id:" in query:
            # The live Team type has no workflowStates field; this is the
            # exact error the live API returns for that query shape.
            resp = graphqLError('Cannot query field "workflowStates" on type "Team". '
                                'Did you mean "draftWorkflowState", "mergeWorkflowState", '
                                'or "startWorkflowState"?')
        elif "workflowStates(filter" in query:
            record(SEQ_FILE, "states")
            record(STATES_Q_FILE, query)
            state = cfg.get("existing_state")
            nodes = ([{"id": state["id"], "name": state["name"], "type": state["type"]}]
                     if state else [])
            resp = {"data": {"workflowStates": {"nodes": nodes}}}
        else:
            resp = graphqLError("unexpected query")

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

python3 "$MOCK" "$MOCK_CFG" "$SEQ_FILE" "$MUT_FILE" "$STATES_Q_FILE" >"$MOCK_LOG" 2>&1 &
MOCK_PID=$!
trap 'kill "$MOCK_PID" 2>/dev/null; rm -rf "$TMP"' EXIT

PORT=""
tries=0
until [ -n "$PORT" ] || [ "$tries" -ge 50 ]; do
    PORT="$(head -n1 "$MOCK_LOG" 2>/dev/null || true)"
    tries=$((tries + 1))
    [ -n "$PORT" ] || sleep 0.1
done
[ -n "$PORT" ] || fail "mock Linear server did not start: $(cat "$MOCK_LOG")"

STATE_SCRIPT="$TMP/create_state.sh"
sed "s@https://api.linear.app/graphql@http://127.0.0.1:$PORT/graphql@g" \
    "$SCRIPT" > "$STATE_SCRIPT"
chmod +x "$STATE_SCRIPT"

reset() {
    echo '{}' > "$MOCK_CFG"
    : > "$SEQ_FILE"
    : > "$MUT_FILE"
    : > "$STATES_Q_FILE"
}

seq_kinds() {
    cat "$SEQ_FILE" 2>/dev/null || true
}

# run_case EXPECTED_EXIT DESC -- runs the script with LINEAR_API_KEY set and
# captures output + exit code.
run_case() {
    local expected="$1" desc="$2"
    OUT_FILE="$TMP/out.txt"
    set +e
    LINEAR_API_KEY="test-key" LINEAR_TEAM_KEY="CRI" "$STATE_SCRIPT" >"$OUT_FILE" 2>&1
    rc=$?
    set -e
    if [ "$rc" -ne "$expected" ]; then
        cat "$OUT_FILE" >&2
        fail "$desc: exit code $rc, expected $expected"
    fi
}

# ── 1. happy path: probe first, create with color, verify resolves ────────
reset
run_case 0 "happy path"
run_out="$(cat "$OUT_FILE")"
printf '%s' "$run_out" | grep -q "probe ok" || fail "happy path: probe message missing"
printf '%s' "$run_out" | grep -q "created state 'Ready for Development' (state-new, type=unstarted, color=#5e6ad2)" || \
    fail "happy path: creation message missing or wrong type/color/id: $run_out"
printf '%s' "$run_out" | grep -q "verified" || fail "happy path: verification message missing"

# probe must precede the teams lookup and every mutation
if [ "$(seq_kinds)" != "$(printf 'probe\nteams\nstates\ncreate\nstates')" ]; then
    fail "happy path: request order was [$(seq_kinds | tr '\n' ' ')], expected probe before teams and create"
fi
ok "happy path: probe-first ordering enforced"

create_input="$(head -n1 "$MUT_FILE")"
if ! jq -e --arg name "Ready for Development" --arg ty "unstarted" --arg team "team-1" \
    --arg color "#5e6ad2" \
    '.name == $name and .type == $ty and .teamId == $team and .color == $color' \
    >/dev/null <<<"$create_input"; then
    fail "happy path: create input wrong (must supply name/type/color/teamId): $create_input"
fi
ok "happy path: create input is {name, type: unstarted, color: #5e6ad2, teamId} -- every required inputField supplied"

# The state lookup must use the ROOT workflowStates(filter:) query; a
# team(id:) { workflowStates } lookup is invalid against the live schema.
n_states_queries="$(wc -l <"$STATES_Q_FILE")"
[ "$n_states_queries" -eq 2 ] || fail "happy path: expected 2 states lookups, got $n_states_queries"
grep -q 'workflowStates(filter:' "$STATES_Q_FILE" || \
    fail "happy path: states lookup does not use the root workflowStates(filter:) query"
if grep -q 'team(id:' "$STATES_Q_FILE"; then
    fail "happy path: states lookup uses the invalid team(id:) form"
fi
ok "happy path: state lookups use the root workflowStates(filter:) form (live-schema valid)"

# The mock itself must reject the invalid team(id:) lookup shape, mirroring
# the live API (proves the mock emulates the live contract, not the script).
mock_reject="$(curl -sS -H "Authorization: test-key" -H "Content-Type: application/json" \
    -d '{"query":"query($team: ID!, $name: String!) { team(id: $team) { workflowStates(filter: {name: {eq: $name}}) { nodes { id } } } }","variables":{"team":"team-1","name":"Ready for Development"}}' \
    "http://127.0.0.1:$PORT/graphql")"
printf '%s' "$mock_reject" | jq -e '.errors[0].message | contains("Cannot query field \"workflowStates\" on type \"Team\"")' >/dev/null || \
    fail "mock contract: team(id:) lookup was not rejected like the live API: $mock_reject"
ok "mock contract: team(id:) workflowStates lookup rejected like the live API"

# The mock must reject a create mutation omitting color with the live
# required-field error (proves a script regression on color fails here).
mock_no_color="$(curl -sS -H "Authorization: test-key" -H "Content-Type: application/json" \
    -d '{"query":"mutation($input: WorkflowStateCreateInput!) { workflowStateCreate(input: $input) { success workflowState { id name type color } } }","variables":{"input":{"teamId":"team-1","name":"Ready for Development","type":"unstarted"}}}' \
    "http://127.0.0.1:$PORT/graphql")"
printf '%s' "$mock_no_color" | jq -e '.errors[0].message | contains("Field \"color\" of required type \"String!\" was not provided.")' >/dev/null || \
    fail "mock contract: color-less create input was not rejected like the live API: $mock_no_color"
ok "mock contract: create input without color rejected with the live required-field error"

# The probe fixture must expose inputFields, not the fabricated "fields".
mock_probe="$(curl -sS -H "Authorization: test-key" -H "Content-Type: application/json" \
    -d '{"query":"query { __type(name: \"WorkflowStateCreateInput\") { name inputFields { name type { kind } } } }"}' \
    "http://127.0.0.1:$PORT/graphql")"
printf '%s' "$mock_probe" | jq -e '[(.data.input.inputFields // [])[].name] | (index("name") != null and index("type") != null and index("color") != null and index("teamId") != null)' >/dev/null || \
    fail "mock contract: probe fixture does not expose inputFields like the live schema: $mock_probe"
ok "mock contract: probe fixture exposes inputFields (live shape), not a fabricated fields shape"

# ── 2. idempotency: existing state -> no create mutation, exit 0 ──────────
reset
echo '{"existing_state": {"id": "state-old", "name": "Ready for Development", "type": "unstarted"}}' > "$MOCK_CFG"
run_case 0 "idempotent run"
run_out="$(cat "$OUT_FILE")"
[ -z "$(seq_kinds | grep '^create$' || true)" ] || fail "idempotent run sent a create mutation"
printf '%s' "$run_out" | grep -q "already exists" || fail "idempotent run: 'already exists' message missing"
if [ "$(seq_kinds)" != "$(printf 'probe\nteams\nstates')" ]; then
    fail "idempotent run: request order was [$(seq_kinds | tr '\n' ' ')]"
fi
ok "idempotency: existing state verified, no create mutation sent"

# ── 3. existing state with terminal type is refused ───────────────────────
reset
echo '{"existing_state": {"id": "state-done", "name": "Ready for Development", "type": "completed"}}' > "$MOCK_CFG"
run_case 1 "terminal state"
printf '%s' "$(cat "$OUT_FILE")" | grep -q "terminal type 'completed'" || \
    fail "terminal state: refusal message missing"
ok "terminal state refused with a loud failure"

# ── 4. probe failure: missing required inputField -> no mutation ──────────
reset
echo '{"probe_missing_fields": ["teamId"]}' > "$MOCK_CFG"
run_case 1 "probe failure"
printf '%s' "$(cat "$OUT_FILE")" | grep -q "does not expose a required 'teamId' inputField" || \
    fail "probe failure: missing-field message missing"
[ -z "$(seq_kinds | grep '^create$' || true)" ] || fail "probe failure sent a create mutation"
[ -z "$(seq_kinds | grep '^teams$' || true)" ] || fail "probe failure continued past the probe to team resolution"
ok "probe failure: loud failure before any team lookup or mutation"

# ── 5. probe answering in the fabricated "fields" shape fails loudly ──────
# The live schema answers INPUT_OBJECT introspection via inputFields; a
# response carrying only "fields" (the first-draft assumption) must not be
# mistaken for a passing probe.
reset
echo '{"probe_shape": "fields"}' > "$MOCK_CFG"
run_case 1 "probe fabricated shape"
printf '%s' "$(cat "$OUT_FILE")" | grep -q "does not expose a required 'name' inputField" || \
    fail "probe fabricated shape: loud failure message missing"
[ -z "$(seq_kinds | grep '^create$' || true)" ] || fail "probe fabricated shape sent a create mutation"
[ -z "$(seq_kinds | grep '^teams$' || true)" ] || fail "probe fabricated shape continued past the probe"
ok "probe 'fields'-shaped response (no inputFields) rejected with a loud failure"

# ── 6. create success=false is a loud failure ─────────────────────────────
reset
echo '{"create_success": false}' > "$MOCK_CFG"
run_case 1 "create failure"
printf '%s' "$(cat "$OUT_FILE")" | grep -q "workflowStateCreate did not succeed" || \
    fail "create failure: loud failure message missing"
ok "create success=false rejected with a loud failure"

# ── 7. verify-after-create failure is a loud failure ──────────────────────
reset
echo '{"verify_resolvable": false}' > "$MOCK_CFG"
run_case 1 "verify failure"
printf '%s' "$(cat "$OUT_FILE")" | grep -q "does not resolve by name after creation" || \
    fail "verify failure: loud failure message missing"
ok "verify-after-create failure rejected with a loud failure"

# ── 8. transport error (HTTP 500) is a loud failure ───────────────────────
reset
echo '{"create_status": 500}' > "$MOCK_CFG"
run_case 1 "transport failure"
printf '%s' "$(cat "$OUT_FILE")" | grep -q "Linear request failed" || \
    fail "transport failure: loud failure message missing"
ok "HTTP 500 from Linear rejected with a loud failure"

# ── 9. missing LINEAR_API_KEY fails loudly with zero requests ─────────────
reset
OUT_FILE="$TMP/out.txt"
set +e
LINEAR_API_KEY="" LINEAR_TEAM_KEY="CRI" "$STATE_SCRIPT" >"$OUT_FILE" 2>&1
rc=$?
set -e
[ "$rc" -eq 1 ] || fail "missing key: exit code $rc, expected 1"
printf '%s' "$(cat "$OUT_FILE")" | grep -q "LINEAR_API_KEY is not set" || \
    fail "missing key: loud failure message missing"
[ -z "$(seq_kinds)" ] || fail "missing key: script made requests without credentials"
ok "missing LINEAR_API_KEY: loud failure, zero requests"

# ── 10. cross-file consistency of the state name (CRI-241) ────────────────
# The state name is written in three places that must never drift apart:
# the triage workflow's linear_ready_state default (set_ready_state moves
# tickets there), the script's STATE_NAME constant (creates it), and the
# criteria-develop route's states list (fires on it).
STATE_NAME_EXPECTED="Ready for Development"

routes_json="$(awk '/^  routes.json: \|$/ {flag=1; next} flag && !/^    / {exit} flag {print substr($0, 5)}' \
    "$REPO_ROOT/k8s/examples/routes-configmap.yaml")"
[ -n "$routes_json" ] || fail "consistency: could not extract routes.json from the shipped example"
if ! jq -e --arg s "$STATE_NAME_EXPECTED" \
    '.routes[] | select(.name == "criteria-develop") | .states == [$s]' \
    >/dev/null <<<"$routes_json"; then
    fail "consistency: criteria-develop route states do not match '$STATE_NAME_EXPECTED'"
fi

if ! grep -Fq 'readonly STATE_NAME="Ready for Development"' "$SCRIPT"; then
    fail "consistency: script STATE_NAME constant drifted from '$STATE_NAME_EXPECTED'"
fi

if ! grep -A5 'variable "linear_ready_state"' "$REPO_ROOT/linear_triage_v1/variables.chcl" \
    | grep -Fq "default     = \"$STATE_NAME_EXPECTED\""; then
    fail "consistency: linear_triage_v1 linear_ready_state default drifted from '$STATE_NAME_EXPECTED'"
fi
ok "consistency: state name identical across triage variable, script constant, and route states"

echo "PASS: create-ready-for-development-state regression tests"
