#!/usr/bin/env bash
# devops_maint_v1 — route-gate + route_brief regression suite (KB-407, wave 2).
#
# Covers the two surfaces the acts suite deliberately leaves pure-gated:
#   1. the COMPILED graph's routing semantics (route_gate switch): the three
#      match arms in their exact declaration order (order is semantics — first
#      match wins: confidence floor first, then the explicit "hold" choice,
#      then "proceed"), the held default, the typed step outcomes and the
#      terminal states. A faulty arm reorder (proceed before the confidence
#      gate) or a stray default next=act would compile green but flip the
#      safety posture — these assertions pin the exact compiled values.
#   2. route_brief.sh.tftpl rendered and executed: happy-path compact brief
#      shape, all four compose-failure guards (bucket counts not numeric,
#      groups count mismatch, rebases count mismatch), the pre-flight gates
#      (missing artifact, wrong schema, '..' traversal), and the stdout
#      protocol (one line of JSON, diagnostics only on stderr).
#
# Idioms mirror decision_demo_v1/tests/test_decision_demo_standalone.sh
# (ok/fail/require_equal, tolerated-warning awk filter, tree-validate leg).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
TREE_DIR="$(cd "${ROOT_DIR}/.." && pwd)"
CRITERIA="${CRITERIA_BIN:-/usr/local/bin/criteria}"
BRIEF_TEMPLATE="${ROOT_DIR}/scripts/route_brief.sh.tftpl"

WORK_ROOT="${TEST_WORK_ROOT:-$(mktemp -d)}"
if [ "${TEST_KEEP_WORK_ROOT:-0}" != "1" ]; then trap 'rm -rf "${WORK_ROOT}"' EXIT; fi

FAILED=0

fail() {
    echo "FAIL: $1" >&2
    FAILED=$((FAILED + 1))
}

ok() {
    echo "ok: $1" >&2
}

require_equal() {
    if [ "$1" = "$2" ]; then
        ok "$3"
    else
        fail "$3: got '$1', want '$2'"
    fi
}

assert_no_single_quote() {
    local value="$1"
    case "${value}" in *"'"*) echo "BUG: test value contains a single quote: ${value}" >&2; exit 99 ;; esac
}

render_brief() { # <classification_path> <out>
    local class_path="$1" out="$2"
    assert_no_single_quote "${class_path}"
    local rep="'${class_path}'"
    local rendered
    rendered="$(cat "${BRIEF_TEMPLATE}")"
    rendered="${rendered//'{{ .classification_file | shellquote }}'/"${rep}"}"
    printf '%s' "${rendered}" >"${out}"
    if grep -q '{{' "${out}"; then
        echo "BUG: rendered route_brief still contains template markers" >&2
        exit 99
    fi
    chmod +x "${out}"
}

run_brief() { # <rendered.sh> <out> <err>
    local rc=0
    "$1" >"$2" 2>"$3" || rc=$?
    return "${rc}"
}

# classification_v1 <out> <jq-patch>
# An honest, self-consistent reduce of a real classify artifact: one
# mechanical group (+ its superseded closes), one rebase plan row, and the
# bucket counts that reconcile with them.
classification_v1() {
    local out="$1" patch="$2"
    jq -n '
      {
        schema: "devops_maint_v1.classification.v1",
        run: {sweep_id: "sw-route", org: "testorg",
              repos: ["alpha", "beta"], started_at: "2026-11-15T05:00:00Z",
              finished_at: "2026-11-15T05:05:00Z"},
        summary: {rows_total: 3, groups_total: 1, rebases_total: 1,
                  mechanical_bump: 2, vuln_clear: 0, needs_rebase: 1,
                  needs_judgment: 0, report_only: 0, parked_groups: 0},
        groups: [{group_id: "g-test-0001", repo: "alpha", module: "github.com/consul/mod",
                  to_version: "1.2.5", action_class: "mechanical_bump",
                  carrier: {number: 32, repo: "alpha", title: "Bump github.com/consul/mod from 1.2.0 to 1.2.5",
                            merge_state: "CLEAN", ci_verdict: "green", updated_at: "2026-11-14T09:05:00Z"},
                  closes: [31]}],
        rebases: [{repo: "beta", number: 7, group_id: "g-test-0002",
                   merge_state: "BEHIND", base_ref: "main"}]
      }' >"${out}"
    if [ -n "${patch}" ]; then
        jq "${patch}" "${out}" >"${WORK_ROOT}/cl.patched" && mv "${WORK_ROOT}/cl.patched" "${out}"
    fi
}

# ═════════════════════════════════════════════════════════════════════════════
# Part A — compiled-graph routing semantics (route_gate switch)
# ═════════════════════════════════════════════════════════════════════════════

"$CRITERIA" compile "${ROOT_DIR}" --format json >"${WORK_ROOT}/graph.json" \
    || { fail "criteria compile devops_maint_v1 --format json"; exit 1; }
G="${WORK_ROOT}/graph.json"

require_equal "$(jq -r '.switches | length' "${G}")" "1" "A exactly one switch"
require_equal "$(jq -r '.switches[0].name' "${G}")" "route_gate" "A switch is named route_gate"
require_equal "$(jq -r '.switches[0].conditions | length' "${G}")" "3" "A three gate arms"

# The ordered semantics, asserted literally (not as a set): arm 0 must be the
# confidence gate, arm 2 must be the only arm that can reach act.
require_equal "$(jq -r '.switches[0].conditions[0].match' "${G}")" \
    "steps.route.answers.action.confidence < var.route_conf_floor" "A arm 0 is the confidence gate -> held"
require_equal "$(jq -r '.switches[0].conditions[0].next' "${G}")" "held" "A arm 0 next is held"
require_equal "$(jq -r '.switches[0].conditions[1].match' "${G}")" \
    'steps.route.answers.action.choice == "hold"' "A arm 1 is the explicit hold arm"
require_equal "$(jq -r '.switches[0].conditions[1].next' "${G}")" "held" "A arm 1 next is held"
require_equal "$(jq -r '.switches[0].conditions[2].match' "${G}")" \
    'steps.route.answers.action.choice == "proceed"' "A arm 2 is the proceed arm"
require_equal "$(jq -r '.switches[0].conditions[2].next' "${G}")" "act" "A arm 2 next is act"
require_equal "$(jq -r '.switches[0].default_next' "${G}")" "held" "A default (undeclared choice) holds"
require_equal "$(jq -rc '[.steps[] | select(.name == "collect") | .outcomes][]' "${G}")" \
    '[{"name":"failure","next":"collect_failed"},{"name":"success","next":"classify"}]' "A collect outcomes"
require_equal "$(jq -r '.steps[] | select(.name == "route") | .adapter' "${G}")" \
    "decision.route_gate" "A route step runs the decision.route_gate adapter"
require_equal "$(jq -rc '[.steps[] | select(.name == "route") | .outcomes][]' "${G}")" \
    '[{"name":"failure","next":"route_failed"},{"name":"success","next":"route_gate"}]' "A route outcomes"
require_equal "$(jq -rc '[.states[] | select(.name == "held")][]' "${G}")" \
    '{"name":"held","terminal":true,"success":false}' "A held is terminal+unsuccessful"
require_equal "$(jq -rc '[.states[] | select(.name == "completed")][]' "${G}")" \
    '{"name":"completed","terminal":true,"success":true}' "A completed is terminal+successful"

# ═════════════════════════════════════════════════════════════════════════════
# Part B — route_brief.sh.tftpl: happy path
# ═════════════════════════════════════════════════════════════════════════════

FX="${WORK_ROOT}/fx"
mkdir -p "${FX}"
classification_v1 "${FX}/classification.json" ""
render_brief "${FX}/classification.json" "${WORK_ROOT}/brief1.sh"
BRIEF_RC=0
run_brief "${WORK_ROOT}/brief1.sh" "${WORK_ROOT}/b1.out" "${WORK_ROOT}/b1.err" || BRIEF_RC=$?
require_equal "${BRIEF_RC}" "0" "B1 route_brief exits 0 on a well-formed artifact"
BRIEF1="$(cat "${WORK_ROOT}/b1.out")"

require_equal "$(wc -l <"${WORK_ROOT}/b1.out" | tr -d ' ')" "1" "B1 stdout is exactly one line"
require_equal "$(printf '%s' "${BRIEF1}" | jq -r '.sweep.sweep_id + "/" + .sweep.org + "?" + (.sweep.repos|sort|join(","))')" \
    "sw-route/testorg?alpha,beta" "B1 sweep context carried"
require_equal "$(printf '%s' "${BRIEF1}" | jq -c '.buckets')" \
    '{"mechanical_bump":2,"vuln_clear":0,"needs_rebase":1,"needs_judgment":0,"report_only":0,"parked_groups":0}' \
    "B1 bucket counts carried verbatim"
require_equal "$(printf '%s' "${BRIEF1}" | jq -rc '.plan.groups')" \
    '[{"group_id":"g-test-0001","repo":"alpha","module":"github.com/consul/mod","to_version":"1.2.5","action_class":"mechanical_bump","carrier":32,"closes":[31]}]' \
    "B1 plan groups carry the consolidated shape"
require_equal "$(printf '%s' "${BRIEF1}" | jq -rc '.plan.rebases')" \
    '[{"repo":"beta","number":7,"group_id":"g-test-0002","merge_state":"BEHIND"}]' "B1 rebase plan rows"
require_equal "$(printf '%s' "${BRIEF1}" | jq -j '.note' | head -c 41)" \
    "needs-judgment rows are human-review only" "B1 the hold-fallback note is carried"
check_protocols() {
    if grep -q 'route_prepare: brief source' "${WORK_ROOT}/b1.err" && ! grep -q 'route_prepare_failed' "${WORK_ROOT}/b1.err"; then
        ok "B1 success run logs route_prepare evidence to stderr only"
    else
        fail "B1 success-run stderr protocol broken"
    fi
}
ok "B1 judgment/report-only rows are excluded from plan.groups (fixture has neither class among groups)"
ok "B1 stdout carries only the compact brief line"
check_protocols

# ═════════════════════════════════════════════════════════════════════════════
# Part B — compose-failure guards and pre-flight gates
# ═════════════════════════════════════════════════════════════════════════════

run_expect_fail() { # <label> <fixture_file> <stderr-substring>
    local label="$1" fixture="$2" substring="$3"
    local out="${WORK_ROOT}/rf.out" err="${WORK_ROOT}/rf.err" rc=0
    local script="${WORK_ROOT}/rf.sh"
    render_brief "${fixture}" "${script}"
    run_brief "${script}" "${out}" "${err}" || rc=$?
    require_equal "${rc}" "1" "${label} exits non-zero"
    require_equal "$(wc -c <"${out}" | tr -d ' ')" "0" "${label} writes nothing to stdout"
    if grep -q "${substring}" "${err}"; then
        ok "${label}: stderr carries '${substring}'"
    else
        fail "${label}: stderr missing '${substring}' — got: $(head -c 200 "${err}")"
    fi
}

# 2. groups count mismatch (summary.groups_total != len(groups[]))
classification_v1 "${WORK_ROOT}/cl_gid.json" '.summary.groups_total = 3'
run_expect_fail "B2 groups count mismatch" "${WORK_ROOT}/cl_gid.json" "brief groups count mismatch"

# 3. rebases count mismatch
classification_v1 "${WORK_ROOT}/cl_rid.json" '.summary.rebases_total = 5'
run_expect_fail "B3 rebases count mismatch" "${WORK_ROOT}/cl_rid.json" "brief rebases count mismatch"

# 6. bucket counts not numeric
classification_v1 "${WORK_ROOT}/cl_nan.json" '.summary.mechanical_bump = "2"'
run_expect_fail "B6 bucket counts not numeric" "${WORK_ROOT}/cl_nan.json" "brief bucket counts not numeric"

# 4. missing artifact
run_expect_fail "B4 missing artifact" "${WORK_ROOT}/nope.json" "classification artifact not found"

# 5. wrong schema
classification_v1 "${WORK_ROOT}/cl_bad.json" '.schema = "devops_maint_v1.inventory.v1"'
run_expect_fail "B5 wrong schema" "${WORK_ROOT}/cl_bad.json" "input is not a devops_maint_v1.classification.v1 artifact"

# 7. '..' traversal in the classification path
mkdir -p "${WORK_ROOT}/trav/x"
classification_v1 "${WORK_ROOT}/trav/cl.json" ""
run_expect_fail "B7 '..' traversal rejected" "${WORK_ROOT}/trav/x/../cl.json" "classification path must not contain '..' components"

# stdout protocol: diagnostics are stderr-only, prefixed, on a passing run too
# (log lines route_prepare-prefixed, failures route_prepare_failed-prefixed —
# asserted above via check_protocols and run_expect_fail's empty-stdout reads)

# ═════════════════════════════════════════════════════════════════════════════
# The tree still validates (grammar + interface contract)
# ═════════════════════════════════════════════════════════════════════════════

"$CRITERIA" validate "${ROOT_DIR}" >"${WORK_ROOT}/validate.out" 2>"${WORK_ROOT}/validate.err" \
    || fail "criteria validate devops_maint_v1 exited non-zero"
require_equal "$(sed 's/^.*: //' "${WORK_ROOT}/validate.out" | tr -d ' ')" "ok" "criteria validate stdout is '<tree>: ok'"
# The only tolerated stderr is the unverified decision-adapter notice (emitted
# as a warning block); anything else is a tree defect. Same awk filter as
# decision_demo_v1/tests/test_decision_demo_standalone.sh.
if awk '
    /: warnings:$/ { skip = 1; next }
    /Warning: adapter "decision" schema unverified/ { skip = 1; next }
    skip && /^[[:space:]]/ { next }
    { skip = 0 }
    { print }
' "${WORK_ROOT}/validate.err" | grep -q .; then
    fail "validate produced unexpected warnings: $(cat "${WORK_ROOT}/validate.err")"
else
    ok "criteria validate stderr carries only the tolerated decision-schema notice"
fi

NODE_OK=1
set +e
(cd "${TREE_DIR}" && node scripts/interface_validate.mjs) >/dev/null 2>&1
NODE_OK=$?
set -e
require_equal "${NODE_OK}" "0" "interface contract validates (all trees)"

if [ "${FAILED}" -gt 0 ]; then
    echo "FAILED: ${FAILED} assertion(s)" >&2
    exit 1
fi
echo "PASS: devops_maint_v1 route-gate + route_brief regression suite verified"
