#!/usr/bin/env bash
# CRI-241: create the "Ready for Development" workflow state on the CRI team
# via the Linear API.
#
# The criteria-develop route (k8s/examples/routes-configmap.yaml) routes any
# ticket in the "Ready for Development" state to the linear_develop_v1
# workflow, and the triage workflow's re-arm step (CRI-240, 9db68c3) moves
# re-armed tickets to that state via set_ready_state — but none of that can
# fire until the state EXISTS on the Linear team. This script is the
# deploy-time half of that pairing: run it BEFORE applying the routes
# ConfigMap and rolling the operator/watcher (make apply-routes), so the
# re-arm step's state name exists when it fires.
#
# Every GraphQL shape below was introspected from the LIVE Linear schema
# (not assumed):
#   - WorkflowStateCreateInput exposes inputFields (NOT fields): name, type,
#     color and teamId are all NON_NULL with no default — color is easy to
#     miss and required.
#   - WorkflowState.type is String!; there is NO WorkflowStateType enum, so
#     the state type constant is validated against Linear's documented
#     workflow state type set instead of an introspected enum.
#   - The Team type has NO workflowStates field; the state lookup uses the
#     ROOT `workflowStates(filter: {team: {id: {eq: $team}}, name:
#     {eq: $name}})` query — the same idiom the CRI-240 re-arm step
#     (linear_triage_v1/scripts/set_review_state.sh.tftpl) uses in
#     production.
#   - workflowStateCreate returns WorkflowStatePayload {success,
#     workflowState {id name type color ...}}.
#
# Behavior:
#   1. PROBE the surfaces above via introspection BEFORE mutating anything
#      ("probe the mutation shape first").
#   2. Resolve the team by key (LINEAR_TEAM_KEY, default "CRI") or take
#      LINEAR_TEAM_ID directly when set.
#   3. Idempotency: an existing state is verified (non-terminal type) and
#      left alone — no create mutation is sent.
#   4. Create the state with type=unstarted (refined, not yet started —
#      development itself moves a ticket to a started state later) and a
#      valid hex color.
#   5. VERIFY the state resolves by name in a FRESH query, so set_ready_state
#      will find it.
#
# LINEAR_API_KEY must be set in the environment; it is only ever sent as the
# Authorization header and never logged, echoed, or written to disk. The
# endpoint literal below is replaced with a mock server URL by the
# regression test (k8s/tests/test_create_ready_state.sh).

# GraphQL query strings deliberately contain $variables that must NOT be
# expanded by the shell -- they are passed to jq via --arg/--argjson, so the
# SC2016 warnings on those single-quoted strings are intentional.
# shellcheck disable=SC2016

set -euo pipefail

readonly STATE_NAME="Ready for Development"
readonly STATE_TYPE="unstarted"
# Linear's documented workflow state type set (WorkflowState.type is String!
# in the live schema -- there is no introspectable enum to validate against).
readonly STATE_TYPES="backlog unstarted started completed canceled"
# color is a required String! create input with no default; Linear renders
# states in hex colors, and #5e6ad2 is Linear's blue.
readonly STATE_COLOR="#5e6ad2"
readonly LINEAR_GRAPHQL_ENDPOINT="https://api.linear.app/graphql"

fail() {
    echo "error: $*" >&2
    exit 1
}

if [ -z "${LINEAR_API_KEY:-}" ]; then
    fail "LINEAR_API_KEY is not set -- export it before creating the '$STATE_NAME' state"
fi

# linear_post QUERY VARIABLES_JSON -> response JSON on stdout, with loud
# failure on transport errors and GraphQL application errors (which Linear
# reports inside a 200 response).
linear_post() {
    local query="$1" variables="$2" response
    response="$(curl -sS -f \
        -H "Authorization: $LINEAR_API_KEY" \
        -H "Content-Type: application/json" \
        -d "$(jq -n --arg q "$query" --argjson v "$variables" '{query: $q, variables: $v}')" \
        "$LINEAR_GRAPHQL_ENDPOINT")" || fail "Linear request failed (transport error)"
    [ -n "$response" ] || fail "Linear returned an empty response"
    if jq -e '.errors != null and (.errors | length > 0)' >/dev/null 2>&1 <<<"$response"; then
        fail "Linear returned GraphQL errors: $(jq -c '.errors' <<<"$response")"
    fi
    jq -e '.data != null' >/dev/null 2>&1 <<<"$response" || \
        fail "Linear response has no data envelope: ${response:-<empty>}"
    printf '%s\n' "$response"
}

# ── 1. Probe the real schema surface before touching anything ─────────────
probe="$(linear_post \
    'query { input: __type(name: "WorkflowStateCreateInput") { name inputFields { name type { kind name ofType { kind name } } defaultValue } } rootQuery: __schema { queryType { fields { name } } } rootMutation: __schema { mutationType { fields { name } } } }' \
    '{}')"

# The create input's required fields (NON_NULL, no default). color is the
# easy one to miss: String! with no default.
for field in name type color teamId; do
    if ! jq -e --arg f "$field" \
        '[(.data.input.inputFields // [])[].name] | index($f) != null' \
        >/dev/null <<<"$probe"; then
        fail "Linear's WorkflowStateCreateInput does not expose a required '$field' inputField -- adapt the create mutation before proceeding"
    fi
done
# __schema results nest under queryType/mutationType in the live response.
jq -e '[(.data.rootQuery.queryType.fields // [])[].name] | index("workflowStates") != null' >/dev/null <<<"$probe" || \
    fail "Linear's root query does not expose the 'workflowStates' field -- the state lookup cannot run"
jq -e '[(.data.rootMutation.mutationType.fields // [])[].name] | index("workflowStateCreate") != null' >/dev/null <<<"$probe" || \
    fail "Linear's root mutation does not expose the 'workflowStateCreate' field -- the state cannot be created"
case " $STATE_TYPES " in
    *" $STATE_TYPE "*) ;;
    *) fail "state type '$STATE_TYPE' is not one of Linear's documented workflow state types ($STATE_TYPES)" ;;
esac
echo "probe ok: WorkflowStateCreateInput requires name/type/color/teamId; root workflowStates and workflowStateCreate exist; type '$STATE_TYPE' is valid"

# ── 2. Resolve the team ───────────────────────────────────────────────────
team_id="${LINEAR_TEAM_ID:-}"
if [ -z "$team_id" ]; then
    team_key="${LINEAR_TEAM_KEY:-CRI}"
    teams="$(linear_post \
        'query($key: String!) { teams(filter: {key: {eq: $key}}) { nodes { id key name } } }' \
        "$(jq -cn --arg k "$team_key" '{key: $k}')")"
    team_id="$(jq -r '(.data.teams.nodes // [])[0].id // empty' <<<"$teams")"
    [ -n "$team_id" ] || fail "no Linear team with key '$team_key'"
    echo "resolved team '$team_key' -> $team_id ($(jq -r '(.data.teams.nodes // [])[0].name' <<<"$teams"))"
fi

# ── 3. Idempotency: an existing state is verified and left alone ──────────
# The lookup uses the ROOT workflowStates query: the live Team type has no
# workflowStates field.
readonly STATES_QUERY='query($team: ID!, $name: String!) { workflowStates(filter: {team: {id: {eq: $team}}, name: {eq: $name}}) { nodes { id name type } } }'
states_vars="$(jq -cn --arg t "$team_id" --arg n "$STATE_NAME" '{team: $t, name: $n}')"
existing="$(linear_post "$STATES_QUERY" "$states_vars")"
existing_id="$(jq -r '(.data.workflowStates.nodes // [])[0].id // empty' <<<"$existing")"
if [ -n "$existing_id" ]; then
    existing_type="$(jq -r '(.data.workflowStates.nodes // [])[0].type' <<<"$existing")"
    case "$existing_type" in
        backlog | unstarted | started)
            echo "state '$STATE_NAME' already exists ($existing_id, type=$existing_type); nothing to create"
            printf '%s\n' "$existing_id"
            exit 0
            ;;
        *)
            fail "state '$STATE_NAME' already exists ($existing_id) with terminal type '$existing_type' -- refusing to recreate it"
            ;;
    esac
fi

# ── 4. Create the state ───────────────────────────────────────────────────
created="$(linear_post \
    'mutation($input: WorkflowStateCreateInput!) { workflowStateCreate(input: $input) { success workflowState { id name type color } } }' \
    "$(jq -cn --arg t "$team_id" --arg n "$STATE_NAME" --arg ty "$STATE_TYPE" --arg c "$STATE_COLOR" '{input: {teamId: $t, name: $n, type: $ty, color: $c}}')")"
if ! jq -e '.data.workflowStateCreate.success == true and .data.workflowStateCreate.workflowState != null' >/dev/null <<<"$created"; then
    fail "workflowStateCreate did not succeed: $(jq -c '.data.workflowStateCreate' <<<"$created")"
fi
state_id="$(jq -r '.data.workflowStateCreate.workflowState.id' <<<"$created")"
created_type="$(jq -r '.data.workflowStateCreate.workflowState.type' <<<"$created")"
created_color="$(jq -r '.data.workflowStateCreate.workflowState.color' <<<"$created")"
[ "$created_type" = "$STATE_TYPE" ] || fail "created state came back with type '$created_type', expected '$STATE_TYPE'"
[ "$created_color" = "$STATE_COLOR" ] || fail "created state came back with color '$created_color', expected '$STATE_COLOR'"
echo "created state '$STATE_NAME' ($state_id, type=$created_type, color=$created_color)"

# ── 5. Verify the state resolves by name in a fresh query ─────────────────
verified="$(linear_post "$STATES_QUERY" "$states_vars")"
resolved_id="$(jq -r '(.data.workflowStates.nodes // [])[0].id // empty' <<<"$verified")"
[ -n "$resolved_id" ] || fail "state '$STATE_NAME' does not resolve by name after creation -- set_ready_state would fail"
[ "$resolved_id" = "$state_id" ] || fail "state '$STATE_NAME' resolved to $resolved_id but create returned $state_id"
echo "verified: '$STATE_NAME' resolves as $resolved_id (type=$(jq -r '(.data.workflowStates.nodes // [])[0].type' <<<"$verified"))"
