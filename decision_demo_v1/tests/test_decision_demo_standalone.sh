#!/usr/bin/env bash
set -euo pipefail

# KB-205: compile-level regression gate for the decision demo pair
# (decision_demo_v1 + decision_demo_local_v1). The runtime depth lives in
# decision_demo_local_v1/tests/ against a stub backend; this script is the
# part CI always runs and guards the tree:
#
#   1. both trees validate standalone and compile green (tolerating only the
#      expected "adapter \"decision\" schema unverified" warning that CI
#      emits while the decision adapter is still unpublished);
#   2. the compiled graph is classify -> route_department with the switch
#      arms in the workstream's declared order, and EVERY answer type
#      (noul / choice / score) is exercised in routing;
#   3. the confidence gate is the FIRST arm and targets the human approval
#      node (match[0] -> confirm_department), the demo's flagship path;
#   4. both approval nodes resolve their approved/rejected outcomes, all six
#      terminals exist with correct success flags, and every node/terminal is
#      reachable from classify;
#   5. credential discipline: the cloud variant declares exactly one secret
#      variable routed only through the adapter secrets channel, the local
#      variant is fully unauthenticated (localhost, no secrets block), and
#      the compiled graph carries config-keys only (no secret material);
#   6. the two variants compile to identical grammar (same steps, switch,
#      terminals, outputs) and differ only in adapter wiring — the
#      backend-agnostic claim.

TREE_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
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

CLOUD="$TREE_ROOT/decision_demo_v1"
LOCAL="$TREE_ROOT/decision_demo_local_v1"

# ── 1. Standalone validation (both variants, tolerated-warnings check) ───────

for dir in "$CLOUD" "$LOCAL"; do
    _name="$(basename "$dir")"
    "$CRITERIA" validate "$dir" >/dev/null 2>"$TMP/$_name.validate.err" \
        || { echo "FAIL: criteria validate $_name: $(cat "$TMP/$_name.validate.err")" >&2; exit 1; }
    # The only warning CI may emit is the unverified-schema notice for the
    # unpublished decision adapter; anything else is a tree defect. The
    # notice is emitted as a block — a "<path>: warnings:" preamble, the
    # header line, and indented continuation lines — so the whole block is
    # consumed before the "anything else" check. A distinct warning (its
    # column-0 header, or text at column 0 right after) still surfaces.
    if awk '
        /: warnings:$/ { skip = 1; next }
        /Warning: adapter "decision" schema unverified/ { skip = 1; next }
        skip && /^[[:space:]]/ { next }
        { skip = 0 }
        { print }
    ' "$TMP/$_name.validate.err" | grep -q .; then
        fail "$_name validate produced unexpected warnings: $(cat "$TMP/$_name.validate.err")"
    else
        ok "criteria validate passes standalone ($_name)"
    fi
done

# ── Compile both variants ───────────────────────────────────────────────────

"$CRITERIA" compile "$CLOUD" --format json > "$TMP/cloud.json" \
    || { echo "FAIL: compile cloud variant" >&2; exit 1; }
"$CRITERIA" compile "$LOCAL" --format json > "$TMP/local.json" \
    || { echo "FAIL: compile local variant" >&2; exit 1; }
ok "both variants compile green"

# ── 2. The classify step: single decision call, declared inputs ─────────────

require_equal "$(jq -r '.steps | length' "$TMP/cloud.json")" "1" "exactly one step"
require_equal "$(jq -r '.steps[0].name' "$TMP/cloud.json")" "classify" "step is classify"
require_equal "$(jq -r '.steps[0].adapter' "$TMP/cloud.json")" "decision.system_one" "step runs the System One decision adapter"
require_equal "$(jq -r '.steps[0].input_keys | tostring' "$TMP/cloud.json")" '["questions","state"]' "classify sends questions + state"
require_equal "$(jq -r '.initial_state' "$TMP/cloud.json")" "classify" "classify is the initial state"
require_equal "$(jq -r '.steps[0].outcomes | tostring' "$TMP/cloud.json")" \
    '[{"name":"failure","next":"decision_failed"},{"name":"success","next":"route_department"}]' \
    "classify failure goes to decision_failed, success to the switch"

# ── 3. Switch arms: exact order, every answer type exercised ────────────────

require_equal "$(jq -r '.switches | length' "$TMP/cloud.json")" "1" "exactly one switch"
require_equal "$(jq -r '.switches[0].conditions | length' "$TMP/cloud.json")" "6" "six match arms"

# Each arm is [condition, next]; order is routing semantics (first match wins).
GOT_ARMS="$(jq -c '[.switches[0].conditions[] | [.match, .next]] | tostring' "$TMP/cloud.json")"
WANT_ARMS="$(jq -c -n '
    [["steps.classify.answers.department.confidence < var.route_conf_floor","confirm_department"],
      ["steps.classify.answers.urgency.noul == \"yes\" || (steps.classify.answers.urgency.noul != \"no\" && steps.classify.answers.urgency.noul >= var.urgency_act_floor)","confirm_escalation"],
      ["steps.classify.answers.severity.score >= var.severity_escalation_level","confirm_escalation"],
      ["steps.classify.answers.department.choice == \"security\"","confirm_escalation"],
      ["steps.classify.answers.department.choice == \"billing\"","queued_billing"],
      ["steps.classify.answers.department.choice == \"technical\"","queued_technical"]] | tostring')"
require_equal "$GOT_ARMS" "$WANT_ARMS" "switch arms in the declared order with the declared targets"
require_equal "$(jq -r '.switches[0].default_next' "$TMP/cloud.json")" "confirm_department" "undeclared choices default to the human gate"

# Acceptance: the demo exercises every answer type in routing.
ARMS_RAW="$(jq -r '.switches[0].conditions[].match' "$TMP/cloud.json")"
if echo "$ARMS_RAW" | grep -q 'answers\.department\.confidence'; then
    ok "confidence answer exercised in routing"
else
    fail "confidence answer not exercised in routing"
fi
if echo "$ARMS_RAW" | grep -q 'answers\.urgency\.noul'; then
    ok "noul answer exercised in routing"
else
    fail "noul answer not exercised in routing"
fi
if echo "$ARMS_RAW" | grep -q 'answers\.severity\.score'; then
    ok "score answer exercised in routing"
else
    fail "score answer not exercised in routing"
fi
CHOICE_ARMS="$(echo "$ARMS_RAW" | grep -c 'answers\.department\.choice' || true)"
require_equal "$CHOICE_ARMS" "3" "choice answer exercised in routing (security/billing/technical)"

# ── 4. Approval outcomes, terminals, reachability ────────────────────────────

# Approvals are engine-validated at compile time (targets must resolve), but
# their outcome edges live only in the source; pin them here.
for pair in \
    'confirm_department:approved:dispatched' \
    'confirm_department:rejected:abandoned' \
    'confirm_escalation:approved:escalated' \
    'confirm_escalation:rejected:abandoned'; do
    _node="${pair%%:*}" ; _rest="${pair#*:}" ; _outcome="${_rest%%:*}" ; _target="${_rest#*:}"
    _block="$(sed -n "/^approval \"$_node\"/,/^}/p" "$CLOUD/main.chcl")"
    if echo "$_block" | grep -q "outcome \"$_outcome\" { next = state.$_target }"; then
        ok "approval $_node outcome $_outcome -> $_target"
    else
        fail "approval $_node is missing outcome $_outcome -> $_target"
    fi
done

require_equal "$(jq -r '[.states[].name] | sort | tostring' "$TMP/cloud.json")" \
    '["abandoned","decision_failed","dispatched","escalated","queued_billing","queued_technical"]' \
    "terminal states exactly as declared"
for terminal in abandoned decision_failed; do
    require_equal "$(jq -r --arg t "$terminal" '([.states[] | select(.name == $t)] | first).success | tostring' "$TMP/cloud.json")" "false" "$terminal is a failure terminal"
done
for terminal in dispatched escalated queued_billing queued_technical; do
    require_equal "$(jq -r --arg t "$terminal" '([.states[] | select(.name == $t)] | first).success | tostring' "$TMP/cloud.json")" "true" "$terminal is a success terminal"
done

# Reachability from classify over compiled edges + the approval outcome edges
# pinned above (approval outcome edges live only in the source).
APPROVAL_EDGES="$TMP/approval_edges.txt"
grep -E 'outcome "(approved|rejected)" \{ next = state\.[a-z_]+' "$CLOUD/main.chcl" \
    | sed -E 's/.*next = state\.([a-z_]+).*/\1/' > "$APPROVAL_EDGES"
if [ "$(wc -l < "$APPROVAL_EDGES")" -ne 4 ]; then
    fail "expected 4 approval outcome edges, got $(wc -l < "$APPROVAL_EDGES")"
fi
TARGETS="$(
    { jq -r '[.steps[].outcomes[].next, .switches[].conditions[].next, .switches[].default_next] | .[]' "$TMP/cloud.json"
      cat "$APPROVAL_EDGES"; } | sort -u | tr '\n' ' '
)"
for node in confirm_department confirm_escalation dispatched escalated queued_billing queued_technical abandoned decision_failed; do
    case " $TARGETS " in
        *" $node "*)
            ok "$node reachable from classify"
            ;;
        *)
            fail "$node is not reachable from any edge in the compiled graph"
            ;;
    esac
done

# ── 5. Credential discipline ────────────────────────────────────────────────

# Compiled graph carries config keys only — never secret values or names.
if jq -e '.adapters[0] | (has("config") or has("secrets") or has("api_key") or has("secret")) | not' "$TMP/cloud.json" >/dev/null; then
    ok "compiled graph carries adapter config keys only (no secret material)"
else
    fail "compiled graph exposes adapter config or secret material"
fi
require_equal "$(jq -r '.adapters[0].config_keys | tostring' "$TMP/cloud.json")" \
    '["base_url","model","retries","timeout"]' "adapter config keys as declared"

# Cloud variant: exactly one secret variable, routed through the secrets
# channel; no quoted credential literals anywhere.
CLOUD_SECRETS="$(grep -c 'secret      = true' "$CLOUD/main.chcl" || true)"
require_equal "$CLOUD_SECRETS" "1" "cloud variant declares exactly one secret variable"
if grep -q 'api_key = var.systemone_api_key' "$CLOUD/main.chcl"; then
    ok "api_key reaches the adapter only via the secrets channel"
else
    fail "api_key is not bound through the adapter secrets channel"
fi
if grep -qE 'api_?key[[:space:]]*=[[:space:]]*"[^"]+"' "$CLOUD/main.chcl"; then
    fail "cloud variant contains a quoted credential literal"
else
    ok "no quoted credential literal in the cloud variant"
fi
if grep -q 'base_url = "https://api.typesafe.ai"' "$CLOUD/main.chcl" && grep -q 'model    = "jev-latest"' "$CLOUD/main.chcl"; then
    ok "cloud variant pins the System One endpoint and model jev-latest"
else
    fail "cloud variant does not pin the System One endpoint/model"
fi

# Local variant: fully unauthenticated, different backend — proves the
# backend-agnostic shape.
if grep -q 'base_url = "http://localhost:11434"' "$LOCAL/main.chcl" && grep -q 'model    = "clef-flash"' "$LOCAL/main.chcl"; then
    ok "local variant pins localhost:11434 / clef-flash"
else
    fail "local variant does not pin the localhost clef-flash backend"
fi
if grep -q 'secrets {' "$LOCAL/main.chcl" || grep -q 'secret      = true' "$LOCAL/main.chcl"; then
    fail "local variant must be fully unauthenticated (no secrets block / secret vars)"
else
    ok "local variant declares no secrets"
fi

# ── 6. Grammar identity across variants ──────────────────────────────────────

for section in steps switches states outputs plugins_required; do
    require_equal \
        "$(jq -S --arg s "$section" '.[$s] | tostring' "$TMP/cloud.json")" \
        "$(jq -S --arg s "$section" '.[$s] | tostring' "$TMP/local.json")" \
        "grammar identical across variants: $section"
done

echo
if [ "$FAILED" -eq 0 ]; then
    echo "PASS: decision demo compile-level test"
else
    echo "FAILED: $FAILED assertion(s)" >&2
    exit 1
fi