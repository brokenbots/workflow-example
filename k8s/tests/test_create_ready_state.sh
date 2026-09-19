#!/usr/bin/env bash
set -euo pipefail

# Regression test for k8s/create-ready-for-development-state.sh (CRI-241).
#
# The state creation is the deploy-time pairing half required before the
# criteria-develop route and the watcher rollout can go live: the triage
# workflow's re-arm step (CRI-240) moves tickets to "Ready for Development",
# which only works once the state exists on the Linear team. The script is
# exercised against a mock Linear API that records every request, covering:
#
#   1. probe-first ordering: WorkflowStateCreateInput introspection happens
#      BEFORE the teams query and any mutation;
#   2. the create mutation's input (name=Ready for Development, type=unstarted,
#      teamId of the resolved CRI team);
#   3. verify-after-create: a fresh states query must resolve the new state;
#   4. idempotency: an existing state produces NO create mutation and exits 0;
#   5. probe failure (mutation shape missing a required field) is a loud
#      failure with zero mutations;
#   6. create success=false is a loud failure;
#   7. verify-after-create failure (state absent after a "successful" create)
#      is a loud failure;
#   8. missing LINEAR_API_KEY fails loudly with ZERO requests.

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

# ── mock Linear API ────────────────────────────────────────────────────────
# Serves the four requests the script makes: the introspection probe
# (__type), the teams lookup (teams(filter:), the workflowStates lookups
# (team(id:) { workflowStates }), and the workflowStateCreate mutation.
# Behavior is driven by MOCK_CFG (a JSON file rewritten between cases);
# every request's kind is appended to SEQ_FILE and mutation inputs to
# MUT_FILE so ordering and payloads can be asserted.

MOCK="$TMP/mock_linear.py"
MOCK_CFG="$TMP/mock_config.json"
SEQ_FILE="$TMP/requests.jsonl"
MUT_FILE="$TMP/mutations.jsonl"
MOCK_LOG="$TMP/mock.log"

cat > "$MOCK" <<'PY'
import json
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer

CFG_FILE, SEQ_FILE, MUT_FILE = sys.argv[1], sys.argv[2], sys.argv[3]

INPUT_FIELDS = [
    {"name": "name", "type": {"kind": "NON_NULL", "name": None}},
    {"name": "type", "type": {"kind": "NON_NULL", "name": None}},
    {"name": "teamId", "type": {"kind": "NON_NULL", "name": None}},
    {"name": "description", "type": {"kind": "SCALAR", "name": "String"}},
]
ENUM_VALUES = [{"name": "backlog"}, {"name": "unstarted"},
               {"name": "started"}, {"name": "completed"}, {"name": "canceled"}]


def record(path, payload):
    with open(path, "a") as f:
        f.write(payload + "\n")


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_POST(self):
        cfg = json.load(open(CFG_FILE))
        body = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))))
        query = body.get("query", "")

        if "__type" in query:
            record(SEQ_FILE, "probe")
            skip = cfg.get("probe_missing_fields", [])
            resp = {"data": {
                "input": {"name": "WorkflowStateCreateInput",
                          "fields": [f for f in INPUT_FIELDS if f["name"] not in skip]},
                "enum": {"name": "WorkflowStateType", "enumValues": ENUM_VALUES},
            }}
        elif "teams(filter" in query:
            record(SEQ_FILE, "teams")
            resp = {"data": {"teams": {"nodes": [
                {"id": "team-1", "key": "CRI", "name": "Criteria"}]}}}
        elif "workflowStates" in query:
            record(SEQ_FILE, "states")
            state = cfg.get("existing_state")
            nodes = ([{"id": state["id"], "name": state["name"], "type": state["type"]}]
                     if state else [])
            resp = {"data": {"team": {"workflowStates": {"nodes": nodes}}}}
        elif "workflowStateCreate" in query:
            record(SEQ_FILE, "create")
            record(MUT_FILE, json.dumps(body["variables"]["input"]))
            if cfg.get("create_status", 200) != 200:
                self.send_response(cfg["create_status"])
                self.end_headers()
                return
            if cfg.get("create_success", True):
                created = {"id": "state-new", "name": body["variables"]["input"]["name"],
                           "type": body["variables"]["input"]["type"]}
                # Post-create verification uses a fresh states query; the
                # script must fail when the created state cannot be resolved.
                resolvable = cfg.get("verify_resolvable", True)
                cfg["existing_state"] = created if resolvable else None
                json.dump(cfg, open(CFG_FILE, "w"))
                resp = {"data": {"workflowStateCreate": {"success": True,
                                                         "workflowState": created}}}
            else:
                resp = {"data": {"workflowStateCreate": {"success": False,
                                                         "workflowState": None}}}
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

python3 "$MOCK" "$MOCK_CFG" "$SEQ_FILE" "$MUT_FILE" >"$MOCK_LOG" 2>&1 &
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

# ── 1. happy path: probe first, create type=unstarted, verify resolves ────
reset
echo '{"existing_state": null}' > "$MOCK_CFG"
run_case 0 "happy path"
run_out="$(cat "$OUT_FILE")"
printf '%s' "$run_out" | grep -q "probe ok" || fail "happy path: probe message missing"
printf '%s' "$run_out" | grep -q "created state 'Ready for Development' (state-new, type=unstarted)" || \
    fail "happy path: creation message missing or wrong type/id: $run_out"
printf '%s' "$run_out" | grep -q "verified" || fail "happy path: verification message missing"

# probe must precede the teams lookup and every mutation
if [ "$(seq_kinds)" != "$(printf 'probe\nteams\nstates\ncreate\nstates')" ]; then
    fail "happy path: request order was [$(seq_kinds | tr '\n' ' ')], expected probe before teams and create"
fi
ok "happy path: probe-first ordering enforced"

create_input="$(head -n1 "$MUT_FILE")"
if ! jq -e --arg name "Ready for Development" --arg ty "unstarted" --arg team "team-1" \
    '.name == $name and .type == $ty and .teamId == $team' >/dev/null <<<"$create_input"; then
    fail "happy path: create input wrong: $create_input"
fi
ok "happy path: create input is {name: Ready for Development, type: unstarted, teamId: team-1}"

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

# ── 4. probe failure: missing required input field -> no mutation ─────────
reset
echo '{"probe_missing_fields": ["teamId"]}' > "$MOCK_CFG"
run_case 1 "probe failure"
printf '%s' "$(cat "$OUT_FILE")" | grep -q "does not expose a 'teamId' field" || \
    fail "probe failure: missing-field message missing"
[ -z "$(seq_kinds | grep '^create$' || true)" ] || fail "probe failure sent a create mutation"
[ -z "$(seq_kinds | grep '^teams$' || true)" ] || fail "probe failure continued past the probe to team resolution"
ok "probe failure: loud failure before any team lookup or mutation"

# ── 5. create success=false is a loud failure ─────────────────────────────
reset
echo '{"create_success": false}' > "$MOCK_CFG"
run_case 1 "create failure"
printf '%s' "$(cat "$OUT_FILE")" | grep -q "workflowStateCreate did not succeed" || \
    fail "create failure: loud failure message missing"
ok "create success=false rejected with a loud failure"

# ── 6. verify-after-create failure is a loud failure ──────────────────────
reset
echo '{"verify_resolvable": false}' > "$MOCK_CFG"
run_case 1 "verify failure"
printf '%s' "$(cat "$OUT_FILE")" | grep -q "does not resolve by name after creation" || \
    fail "verify failure: loud failure message missing"
ok "verify-after-create failure rejected with a loud failure"

# ── 7. transport error (HTTP 500) is a loud failure ───────────────────────
reset
echo '{"create_status": 500}' > "$MOCK_CFG"
run_case 1 "transport failure"
printf '%s' "$(cat "$OUT_FILE")" | grep -q "Linear request failed" || \
    fail "transport failure: loud failure message missing"
ok "HTTP 500 from Linear rejected with a loud failure"

# ── 8. missing LINEAR_API_KEY fails loudly with zero requests ─────────────
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

echo "PASS: create-ready-for-development-state regression tests"