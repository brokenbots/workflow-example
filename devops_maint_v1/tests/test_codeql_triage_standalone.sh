#!/usr/bin/env bash
# devops_maint_v1 — Lane B CodeQL triage-step regression suite (KB-409, ADR-0015 waves 5+6).
#
# Exercises the rendered codeql_triage.sh END-TO-END through a stub gh
# (no network, no real GitHub):
#   - gh_retry shim is byte-identical to the canonical wrapper copy
#   - config gates fire BEFORE any network call (budget 0/empty/negative,
#     out-of-scope plan rows, duplicate (repo, alert_number) rows, bad
#     disposition, suppress-without-dismissed_reason, bad/missing rule_id,
#     fix-with-dismissed_reason -> typed failure, zero gh calls, no artifact)
#   - suppress happy path: typed dismissal PATCH with
#     {state: dismissed, dismissed_reason, dismissed_comment} carrying the
#     typed suppression + rationale, READ BACK as dismissed -> budget_used 1,
#     row result dismissed, held_at_zero honored
#   - the dismissed_comment carries rule, alert number, dismissed_reason and
#     the rationale verbatim (typed suppression-with-rationale)
#   - budget cap: only BUDGET suppression executions per run; excess rows are
#     out_of_budget bookkeeping (retried next run), never over-executed
#   - fix rows: fix_pending, NEVER act, zero budget consumption
#   - rule_mismatch: stale plan evidence never dismissed, no PATCH
#   - already_resolved: alert no longer open in the fresh enumeration, no PATCH
#   - unplanned open alerts: report-only rows with severity
#   - read-back that still shows open -> readback_mismatch, budget NOT consumed
#   - dismissal PATCH failure -> dismiss_failed bookkeeping, step still rc 0
#   - read-back transport failure -> readback_failed bookkeeping
#   - CodeQL disabled (404) -> disabled repo lane, rows recorded but never acted
#   - enumeration failure -> error lane, error row, held_at_zero cannot be claimed
#   - post-run re-enumeration failure -> repo flips to error, held_at_zero FALSE
#   - empty plan verify-only run stays SUCCESS
#   - stdout protocol: artifact path only; diagnostics on stderr
#   - token hygiene: the GH_TOKEN canary never lands in artifacts or stderr
#   - tree validation (node + criteria), matching the other step suites

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
TREE_DIR="$(cd "${ROOT_DIR}/.." && pwd)"
TEMPLATE="${ROOT_DIR}/scripts/codeql_triage.sh.tftpl"
WRAPPER="${ROOT_DIR}/scripts/gh_retry.sh.tftpl"
CANONICAL_WRAPPER="${TREE_DIR}/workstream_handler_v1/scripts/gh_retry.sh.tftpl"

WORK_ROOT="${TEST_WORK_ROOT:-$(mktemp -d)}"
if [ "${TEST_KEEP_WORK_ROOT:-0}" != "1" ]; then trap 'rm -rf "${WORK_ROOT}"' EXIT; fi
BIN_STUB_GH="${WORK_ROOT}/bin_gh"
mkdir -p "${BIN_STUB_GH}"

FAILURES=0
check() {
    local label="$1"
    shift
    if "$@"; then
        echo "PASS: ${label}" >&2
    else
        echo "FAIL: ${label}" >&2
        FAILURES=$((FAILURES + 1))
    fi
}

check_not() {
    local label="$1"
    shift
    if "$@"; then
        echo "FAIL: ${label}" >&2
        FAILURES=$((FAILURES + 1))
    else
        echo "PASS: ${label}" >&2
    fi
}

expect_equal() {
    if [ "$2" = "$3" ]; then
        echo "PASS: $1" >&2
    else
        echo "FAIL: $1 (want [$2], got [$3])" >&2
        FAILURES=$((FAILURES + 1))
    fi
}

assert_no_single_quote() {
    local value="$1"
    case "${value}" in *"'"*) echo "BUG: test value contains a single quote: ${value}" >&2; exit 99 ;; esac
}

# ── Renderer ─────────────────────────────────────────────────────────────────
# Replays the templatefile splice faithfully: gh_retry content substituted raw;
# shellquote markers render as single-quoted literals; no {{ marker survives.
render_triage() { # render_triage <org> <repos_json> <plan_json> <budget> <classification_file> <out>
    local org="$1" repos_json="$2" plan_json="$3" budget="$4" anchor="$5" out="$6"
    assert_no_single_quote "${org}"
    assert_no_single_quote "${repos_json}"
    assert_no_single_quote "${plan_json}"
    assert_no_single_quote "${budget}"
    assert_no_single_quote "${anchor}"
    local wrap
    wrap="$(cat "${WRAPPER}")"
    local rendered
    rendered="$(cat "${TEMPLATE}")"
    rendered="${rendered//'{{ .gh_retry }}'/"${wrap}"}"
    rendered="${rendered//'{{ .org | shellquote }}'/"'${org}'"}"
    rendered="${rendered//'{{ .repos_json | shellquote }}'/"'${repos_json}'"}"
    rendered="${rendered//'{{ .plan_json | shellquote }}'/"'${plan_json}'"}"
    rendered="${rendered//'{{ .budget | shellquote }}'/"'${budget}'"}"
    rendered="${rendered//'{{ .classification_file | shellquote }}'/"'${anchor}'"}"
    printf '%s' "${rendered}" >"${out}"
    if grep -q '{{' "${out}"; then
        echo "BUG: rendered triage template still contains template markers" >&2
        exit 99
    fi
    chmod +x "${out}"
}

# ── Stub gh ──────────────────────────────────────────────────────────────────
# Fixture-driven; every invocation appends "METHOD URL" (plus "BODY <json>"
# for mutating calls) to TRIAGE_GH_LOG. Mutation counters mirror the API:
#   - alerts enumeration (alerts?state=open...): per-repo counter; call 1
#     serves enum1 hooks / alerts.json; later calls serve enum2 hooks /
#     explicit enum2.json, then .mutated + alerts.after.json, else alerts.json.
#   - PATCH alerts/<N>: dismiss.rc hook -> fail; else .mutated + lastbody.json
#   - GET alerts/<N>: rb.rc hook -> fail; else rb.json, or rb.after.json once
#     .mutated (the dismissal is actually observable afterwards).
cat >"${BIN_STUB_GH}/gh" <<'EOF'
#!/usr/bin/env bash
set -u
fx="${TRIAGE_GH_FIXTURES:?TRIAGE_GH_FIXTURES not set}"
log="${TRIAGE_GH_LOG:-/dev/null}"

url=""
method="GET"
input=""
args=("$@")
if [ "${args[0]:-}" = "api" ]; then
    args=("${args[@]:1}")
fi
i=0
while [ "$i" -lt "${#args[@]}" ]; do
    case "${args[$i]}" in
        --method) i=$((i + 1)); method="${args[$i]:-GET}" ;;
        --input)  i=$((i + 1)); input="${args[$i]:-}" ;;
        --paginate|-H|/*|"-"*) ;;
        repos/*) [ -z "${url}" ] && url="${args[$i]}" ;;
        *) [ -z "${url}" ] && url="${args[$i]}" ;;
    esac
    i=$((i + 1))
done
printf '%s %s\n' "${method}" "${url}" >>"${log}"
if [ -n "${input}" ] && [ -f "${input}" ]; then
    printf 'BODY %s\n' "$(cat "${input}")" >>"${log}"
fi

org="$(printf '%s' "${url}" | cut -d/ -f2)"
repo="$(printf '%s' "${url}" | cut -d/ -f3)"
fxr="${fx}/${repo}"

case "$url" in
    "repos/${org}/${repo}/code-scanning/alerts?"*)
        [ -d "$fxr" ] || { echo "stub: no fixture dir for ${repo}" >&2; exit 9; }
        c=0
        [ -f "${fxr}/.enum.count" ] && c="$(cat "${fxr}/.enum.count")"
        c=$((c + 1))
        echo "$c" >"${fxr}/.enum.count"
        if [ "$c" -eq 1 ] && [ -f "${fxr}/enum1.rc" ]; then
            rc="$(cat "${fxr}/enum1.rc")"
            [ -f "${fxr}/enum1.err" ] && cat "${fxr}/enum1.err" >&2
            exit "$rc"
        fi
        if [ "$c" -ge 2 ]; then
            if [ -f "${fxr}/enum2.rc" ]; then
                rc="$(cat "${fxr}/enum2.rc")"
                [ -f "${fxr}/enum2.err" ] && cat "${fxr}/enum2.err" >&2
                exit "$rc"
            fi
            if [ -f "${fxr}/enum2.json" ]; then cat "${fxr}/enum2.json"; exit 0; fi
        fi
        if [ -f "${fxr}/.mutated" ] && [ -f "${fxr}/alerts.after.json" ]; then
            cat "${fxr}/alerts.after.json"
        elif [ -f "${fxr}/alerts.json" ]; then
            cat "${fxr}/alerts.json"
        else
            echo "stub: missing fixture alerts.json for ${repo}" >&2
            exit 9
        fi
        ;;
    "repos/${org}/${repo}/code-scanning/alerts/"*)
        [ -d "$fxr" ] || { echo "stub: no fixture dir for ${repo}" >&2; exit 9; }
        if [ "$method" != "GET" ]; then
            erc="${fxr}/dismiss.rc"
            [ -f "$erc" ] && { rc="$(cat "$erc")";
                [ -f "${fxr}/dismiss.err" ] && cat "${fxr}/dismiss.err" >&2; exit "$rc"; }
            touch "${fxr}/.mutated"
            [ -n "${input}" ] && cp "${input}" "${fxr}/lastbody.json"
            if [ -f "${fxr}/dismiss.json" ]; then cat "${fxr}/dismiss.json"; else echo '{"state": "dismissed"}'; fi
            exit 0
        fi
        erc="${fxr}/rb.rc"
        if [ -f "$erc" ]; then
            rc="$(cat "$erc")"
            [ -f "${fxr}/rb.err" ] && cat "${fxr}/rb.err" >&2
            exit "$rc"
        fi
        if [ -f "${fxr}/.mutated" ] && [ -f "${fxr}/rb.after.json" ]; then
            cat "${fxr}/rb.after.json"
        elif [ -f "${fxr}/rb.json" ]; then
            cat "${fxr}/rb.json"
        else
            echo "stub: missing fixture rb.json for ${repo}" >&2
            exit 9
        fi
        ;;
    *)
        echo "stub: unknown gh api path: ${url}" >&2
        exit 9
        ;;
esac
EOF
chmod +x "${BIN_STUB_GH}/gh"

# codeql_enumerate shapes alerts as {number, rule_id: .rule.id,
# severity: .rule.security_severity_level} via jq -s over the (paginated) doc
# stream; fixtures are one-doc-per-line JSON (the --paginate shape).
alert() { # alert <number> <rule_id> <severity> -> prints one one-alert page doc (API returns arrays per page)
    printf '[{"number": %s, "rule": {"id": "%s", "security_severity_level": "%s"}, "state": "open"}]\n' "$1" "$2" "$3"
}

run_sub() { # run_sub <script> <fixtures-dir> <out> <err> -> rc
    local script="$1" fixtures="$2" out="$3" err="$4"
    local rc=0
    set +e
    (
        export PATH="${BIN_STUB_GH}:${PATH}"
        export TRIAGE_GH_FIXTURES="${fixtures}"
        export TRIAGE_GH_LOG="${fixtures}/call.log"
        export GH_RETRY_MAX_ATTEMPTS=1
        export GH_RETRY_BACKOFF_SECONDS="0 0 0"
        export GH_RETRY_SCRIPT_SLEEP_BUDGET=0
        export GH_TOKEN="supersecret-token-canary"
        "${script}"
    ) >"${out}" 2>"${err}"
    rc=$?
    set -e
    return "${rc}"
}

# Per-scenario run anchors: each anchor lives in its own run dir (the artifact
# is always codeql_triage.json in dirname(anchor)).
mk_anchor() { # mk_anchor <name> -> <path>; creates an existing anchor file.
    local name="$1"
    mkdir -p "${WORK_ROOT}/runs/${name}"
    printf '{}' >"${WORK_ROOT}/runs/${name}/classification.json"
    printf '%s' "${WORK_ROOT}/runs/${name}/classification.json"
}

# ═════════════════════════════════════════════════════════════════════════════
# 1. Vendored wrapper identity + structural wiring
# ═════════════════════════════════════════════════════════════════════════════
check "gh_retry vendored byte-identical to workstream_handler_v1" cmp -s "${WRAPPER}" "${CANONICAL_WRAPPER}"
check "main.chcl renders the triage loop through templatefile" grep -q 'templatefile("./scripts/codeql_triage.sh.tftpl"' "${ROOT_DIR}/main.chcl"
check "main.chcl renders the protection gate through templatefile" grep -q 'templatefile("./scripts/protect_rulesets.sh.tftpl"' "${ROOT_DIR}/main.chcl"
check "main.chcl binds the triage plan variable" grep -q 'jsonencode(var.codeql_triage_plan)' "${ROOT_DIR}/main.chcl"
check "main.chcl formats the budget as an integer" grep -q 'format("%d", var.codeql_triage_budget)' "${ROOT_DIR}/main.chcl"
check "triage template splices the wrapper as raw content" test "$(grep -o '{{ *\.gh_retry }}' "${TEMPLATE}" | wc -l)" = "1"
check "triage template does not treat the wrapper as a path" test "$(grep -c '$_TPL_DIR' "${TEMPLATE}")" = "0"
check "triage template consumes the classification anchor" grep -q 'ANCHOR_PATH={{ .classification_file | shellquote }}' "${TEMPLATE}"
check "triage template consumes the typed plan" grep -q 'PLAN_JSON={{ .plan_json | shellquote }}' "${TEMPLATE}"
check "triage template consumes the budget" grep -q 'BUDGET={{ .budget | shellquote }}' "${TEMPLATE}"

NODE_OK=1
set +e
(cd "${TREE_DIR}" && node scripts/interface_validate.mjs) >/dev/null 2>&1
NODE_OK=$?
CRIT_OK=1
(cd "${TREE_DIR}" && /usr/local/bin/criteria validate devops_maint_v1) >"${WORK_ROOT}/validate.out" 2>&1
CRIT_OK=$?
set -e
expect_equal "interface contract validates (all trees)" 0 "${NODE_OK}"
expect_equal "criteria accepts the devops_maint_v1 tree" 0 "${CRIT_OK}"
check "criteria reports devops_maint_v1 ok" grep -q 'devops_maint_v1: ok' "${WORK_ROOT}/validate.out"

# ═════════════════════════════════════════════════════════════════════════════
# 2. Config gates: typed, fail-loud, BEFORE any network call
# ═════════════════════════════════════════════════════════════════════════════
FXC="${WORK_ROOT}/fxconfig"
mkdir -p "${FXC}/r1"
: >"${FXC}/call.log"

gate_run() { # gate_run <name> — runs g_<name>.sh, asserts typed failure, no traffic, no artifact
    local name="$1" out err rc=0
    out="${WORK_ROOT}/gate_${name}.out"
    err="${WORK_ROOT}/gate_${name}.err"
    : >"${FXC}/call.log"
    set +e
    (
        export PATH="${BIN_STUB_GH}:${PATH}"
        export TRIAGE_GH_FIXTURES="${FXC}"
        export TRIAGE_GH_LOG="${FXC}/call.log"
        export GH_TOKEN="supersecret-token-canary"
        "${WORK_ROOT}/scripts/g_${name}.sh"
    ) >"${out}" 2>"${err}" || rc=$?
    set -e
    check "S2 ${name}: rc != 0" test "${rc}" -ne 0
    check "S2 ${name}: typed failure line on stderr" grep -q '^codeql_triage_failed:' "${err}"
    check "S2 ${name}: zero gh traffic before the gate" test ! -s "${FXC}/call.log"
    check "S2 ${name}: no artifact produced" test ! -e "${WORK_ROOT}/runs/g_${name}/codeql_triage.json"
}

mkdir -p "${WORK_ROOT}/scripts"
A_G0="$(mk_anchor "g_budget0")"
render_triage testorg '["r1"]' '[{"repo":"r1","alert_number":1,"rule_id":"go/unsafe-quoting","disposition":"suppress","rationale":"x","dismissed_reason":"false_positive"}]' 0 "${A_G0}" "${WORK_ROOT}/scripts/g_budget0.sh"
gate_run budget0
A_G1="$(mk_anchor "g_budgetempty")"
render_triage testorg '["r1"]' '[{"repo":"r1","alert_number":1,"rule_id":"go/unsafe-quoting","disposition":"suppress","rationale":"x","dismissed_reason":"false_positive"}]' '' "${A_G1}" "${WORK_ROOT}/scripts/g_budgetempty.sh"
gate_run budgetempty
A_G2="$(mk_anchor "g_budneg")"
render_triage testorg '["r1"]' '[{"repo":"r1","alert_number":1,"rule_id":"go/unsafe-quoting","disposition":"suppress","rationale":"x","dismissed_reason":"false_positive"}]' -7 "${A_G2}" "${WORK_ROOT}/scripts/g_budneg.sh"
gate_run budneg
A_G3="$(mk_anchor "g_scope")"
render_triage testorg '["r1"]' '[{"repo":"elsewhere","alert_number":1,"rule_id":"go/unsafe-quoting","disposition":"suppress","rationale":"x","dismissed_reason":"false_positive"}]' 1 "${A_G3}" "${WORK_ROOT}/scripts/g_scope.sh"
gate_run scope
A_G4="$(mk_anchor "g_dup")"
render_triage testorg '["r1"]' '[{"repo":"r1","alert_number":1,"rule_id":"a","disposition":"fix","rationale":"x"},{"repo":"r1","alert_number":1,"rule_id":"a","disposition":"fix","rationale":"y"}]' 1 "${A_G4}" "${WORK_ROOT}/scripts/g_dup.sh"
gate_run dup
A_G5="$(mk_anchor "g_disp")"
render_triage testorg '["r1"]' '[{"repo":"r1","alert_number":1,"rule_id":"go/unsafe-quoting","disposition":"revert","rationale":"x"}]' 1 "${A_G5}" "${WORK_ROOT}/scripts/g_disp.sh"
gate_run disp
A_G6="$(mk_anchor "g_noreason")"
render_triage testorg '["r1"]' '[{"repo":"r1","alert_number":1,"rule_id":"go/unsafe-quoting","disposition":"suppress","rationale":"x","dismissed_reason":""}]' 1 "${A_G6}" "${WORK_ROOT}/scripts/g_noreason.sh"
gate_run noreason
A_G7="$(mk_anchor "g_badreason")"
render_triage testorg '["r1"]' '[{"repo":"r1","alert_number":1,"rule_id":"go/unsafe-quoting","disposition":"suppress","rationale":"x","dismissed_reason":"whatever"}]' 1 "${A_G7}" "${WORK_ROOT}/scripts/g_badreason.sh"
gate_run badreason
A_G8="$(mk_anchor "g_norule")"
render_triage testorg '["r1"]' '[{"repo":"r1","alert_number":1,"disposition":"fix","rationale":"x"}]' 1 "${A_G8}" "${WORK_ROOT}/scripts/g_norule.sh"
gate_run norule
A_G9="$(mk_anchor "g_fixreason")"
render_triage testorg '["r1"]' '[{"repo":"r1","alert_number":1,"rule_id":"go/unsafe-quoting","disposition":"fix","rationale":"x","dismissed_reason":"false_positive"}]' 1 "${A_G9}" "${WORK_ROOT}/scripts/g_fixreason.sh"
gate_run fixreason

# ═════════════════════════════════════════════════════════════════════════════
# 3. Suppress happy path: typed dismissal PATCH, read-back dismissed,
#    budget_used == 1, held_at_zero
# ═════════════════════════════════════════════════════════════════════════════
FX1="${WORK_ROOT}/fx1"
mkdir -p "${FX1}/go1"
: >"${FX1}/call.log"
alert 1 go/unsafe-quoting critical >"${FX1}/go1/alerts.json"
printf '[]\n' >"${FX1}/go1/alerts.after.json"
printf '{"state": "open"}' >"${FX1}/go1/rb.json"
printf '{"state": "dismissed"}' >"${FX1}/go1/rb.after.json"

S3_PLAN='[{"repo":"go1","alert_number":1,"rule_id":"go/unsafe-quoting","disposition":"suppress","rationale":"fixture: quoting in test scaffolding is intentional","dismissed_reason":"false_positive"}]'
A1="$(mk_anchor "t3")"
render_triage testorg '["go1"]' "${S3_PLAN}" 1 "${A1}" "${WORK_ROOT}/triage1.sh"
T3_RC=0
run_sub "${WORK_ROOT}/triage1.sh" "${FX1}" "${WORK_ROOT}/t3.out" "${WORK_ROOT}/t3.err" || T3_RC=$?
expect_equal "S3 run exits 0" 0 "${T3_RC}"
TRIAGE1="$(tr -d '[:space:]' <"${WORK_ROOT}/t3.out")"
check "S3 stdout is exactly the artifact path (one line, file exists)" test "$(printf '%s\n' "${TRIAGE1}" | wc -l)" = "1" -a -f "${TRIAGE1}"
check "S3 artifact schema is the triage v1 shape" jq -e '.schema == "devops_maint_v1.codeql_triage.v1"' "${TRIAGE1}" >/dev/null
check "S3 row dismissed with read-back" jq -e '.repos[0].dispositions[0].result == "dismissed" and .repos[0].dispositions[0].readback_state == "dismissed"' "${TRIAGE1}" >/dev/null
check "S3 row carries the typed dismissed_reason and rationale" jq -e '.repos[0].dispositions[0].dismissed_reason == "false_positive" and .repos[0].dispositions[0].rationale == "fixture: quoting in test scaffolding is intentional"' "${TRIAGE1}" >/dev/null
check "S3 alerts open before=1 after=0" jq -e '.repos[0].alerts_before == 1 and .repos[0].alerts_after == 0' "${TRIAGE1}" >/dev/null
check "S3 held_at_zero" jq -e '.summary.held_at_zero' "${TRIAGE1}" >/dev/null
check "S3 budget consumed exactly once" jq -e '.summary.budget_used == 1 and .summary.budget == 1 and .summary.suppressions_executed == 1' "${TRIAGE1}" >/dev/null
check "S3 PATCH hit the alert endpoint" grep -q '^PATCH repos/testorg/go1/code-scanning/alerts/1$' "${FX1}/call.log"

# ═════════════════════════════════════════════════════════════════════════════
# 4. Typed suppression comment: the PATCH body carries rule, alert, reason,
#    rationale verbatim (auditable suppression-with-rationale)
# ═════════════════════════════════════════════════════════════════════════════
check "S4 lastbody is the typed dismissal body" jq -e '.state == "dismissed" and .dismissed_reason == "false_positive"' "${FX1}/go1/lastbody.json" >/dev/null
check "S4 dismissed_comment names the rule" jq -r '.dismissed_comment' "${FX1}/go1/lastbody.json" | grep -q "rule: go/unsafe-quoting"
check "S4 dismissed_comment names the alert" jq -r '.dismissed_comment' "${FX1}/go1/lastbody.json" | grep -q "alert: #1"
check "S4 dismissed_comment records the dismissed_reason" jq -r '.dismissed_comment' "${FX1}/go1/lastbody.json" | grep -q "dismissed_reason: false_positive"
check "S4 dismissed_comment carries the rationale verbatim" jq -r '.dismissed_comment' "${FX1}/go1/lastbody.json" | grep -q "rationale: fixture: quoting in test scaffolding is intentional"
check "S4 dismissed_comment is the devops_maint_v1 typed marker" jq -r '.dismissed_comment' "${FX1}/go1/lastbody.json" | grep -q "devops_maint_v1 codeql-triage"

# ═════════════════════════════════════════════════════════════════════════════
# 5. Budget cap: one suppression per run, next row waits as out_of_budget
# ═════════════════════════════════════════════════════════════════════════════
FX2="${WORK_ROOT}/fx2"
mkdir -p "${FX2}/go2"
: >"${FX2}/call.log"
{ alert 1 go/unsafe-quoting critical; alert 2 go/unsafe-quoting high; } >"${FX2}/go2/alerts.json"
alert 2 go/unsafe-quoting high >"${FX2}/go2/alerts.after.json"
printf '{"state": "open"}' >"${FX2}/go2/rb.json"
printf '{"state": "dismissed"}' >"${FX2}/go2/rb.after.json"

A2="$(mk_anchor "t5")"
render_triage testorg '["go2"]' '[{"repo":"go2","alert_number":1,"rule_id":"go/unsafe-quoting","disposition":"suppress","rationale":"one","dismissed_reason":"false_positive"},{"repo":"go2","alert_number":2,"rule_id":"go/unsafe-quoting","disposition":"suppress","rationale":"two","dismissed_reason":"wont_fix"}]' 1 "${A2}" "${WORK_ROOT}/triage2.sh"
T5_RC=0
run_sub "${WORK_ROOT}/triage2.sh" "${FX2}" "${WORK_ROOT}/t5.out" "${WORK_ROOT}/t5.err" || T5_RC=$?
expect_equal "S5 budgeted run exits 0" 0 "${T5_RC}"
TRIAGE2="$(tr -d '[:space:]' <"${WORK_ROOT}/t5.out")"
check "S5 first suppression executed, second held" jq -e '.repos[0].dispositions[0].result == "dismissed" and .repos[0].dispositions[1].result == "out_of_budget"' "${TRIAGE2}" >/dev/null
check "S5 out-of-budget row stays open and names the retry" jq -e '.repos[0].dispositions[1].readback_state == "open" and (.repos[0].dispositions[1].note | contains("budget exhausted"))' "${TRIAGE2}" >/dev/null
check "S5 budget used is exactly the cap" jq -e '.summary.budget_used == 1 and .summary.out_of_budget_total == 1' "${TRIAGE2}" >/dev/null
expect_equal "S5 only ONE PATCH left the stub" 1 "$(grep -c '^PATCH repos/testorg/go2/code-scanning/alerts/1$' "${FX2}/call.log")"
expect_equal "S5 no PATCH for the second alert" 0 "$(grep -c 'PATCH repos/testorg/go2/code-scanning/alerts/2$' "${FX2}/call.log")"

# ═════════════════════════════════════════════════════════════════════════════
# 6. Fix rows: recorded as fix_pending, NEVER acted on, budget untouched
# ═════════════════════════════════════════════════════════════════════════════
FX3="${WORK_ROOT}/fx3"
mkdir -p "${FX3}/go3"
: >"${FX3}/call.log"
alert 1 go/unsafe-quoting critical >"${FX3}/go3/alerts.json"
printf '{"state": "open"}' >"${FX3}/go3/rb.json"

A3="$(mk_anchor "t6")"
render_triage testorg '["go3"]' '[{"repo":"go3","alert_number":1,"rule_id":"go/unsafe-quoting","disposition":"fix","rationale":"fix lands via the standard pipeline"}]' 5 "${A3}" "${WORK_ROOT}/triage3.sh"
T6_RC=0
run_sub "${WORK_ROOT}/triage3.sh" "${FX3}" "${WORK_ROOT}/t6.out" "${WORK_ROOT}/t6.err" || T6_RC=$?
expect_equal "S6 fix run exits 0" 0 "${T6_RC}"
TRIAGE3="$(tr -d '[:space:]' <"${WORK_ROOT}/t6.out")"
check "S6 fix row is fix_pending with the alert open" jq -e '.repos[0].dispositions[0].result == "fix_pending" and .repos[0].dispositions[0].readback_state == "open"' "${TRIAGE3}" >/dev/null
check "S6 no budget consumed" jq -e '.summary.budget_used == 0 and .summary.fix_pending_total == 1' "${TRIAGE3}" >/dev/null
check_not "S6 zero mutating calls" grep -q '^PATCH ' "${FX3}/call.log"
check "S6 alerts_after still 1 (held_at_zero false)" jq -e '.repos[0].alerts_after == 1 and .summary.held_at_zero == false' "${TRIAGE3}" >/dev/null

# ═════════════════════════════════════════════════════════════════════════════
# 7. rule_mismatch: a stale plan never dismisses evidence it was not written
#    against
# ═════════════════════════════════════════════════════════════════════════════
FX4="${WORK_ROOT}/fx4"
mkdir -p "${FX4}/go4"
: >"${FX4}/call.log"
alert 1 go/unsafe-quoting critical >"${FX4}/go4/alerts.json"
printf '{"state": "open"}' >"${FX4}/go4/rb.json"

A4="$(mk_anchor "t7")"
render_triage testorg '["go4"]' '[{"repo":"go4","alert_number":1,"rule_id":"go/some-other-rule","disposition":"suppress","rationale":"stale plan row","dismissed_reason":"false_positive"}]' 1 "${A4}" "${WORK_ROOT}/triage4.sh"
T7_RC=0
run_sub "${WORK_ROOT}/triage4.sh" "${FX4}" "${WORK_ROOT}/t7.out" "${WORK_ROOT}/t7.err" || T7_RC=$?
expect_equal "S7 rule-mismatch run exits 0" 0 "${T7_RC}"
TRIAGE4="$(tr -d '[:space:]' <"${WORK_ROOT}/t7.out")"
check "S7 stale row is rule_mismatch" jq -e '.repos[0].dispositions[0].result == "rule_mismatch" and .repos[0].dispositions[0].readback_state == "open"' "${TRIAGE4}" >/dev/null
check "S7 mismatch note names both rules" jq -e '.repos[0].dispositions[0].note | contains("go/some-other-rule") and contains("go/unsafe-quoting")' "${TRIAGE4}" >/dev/null
check_not "S7 no PATCH for stale evidence" grep -q '^PATCH ' "${FX4}/call.log"
check "S7 budget untouched" jq -e '.summary.budget_used == 0 and .summary.rule_mismatch_total == 1' "${TRIAGE4}" >/dev/null

# ═════════════════════════════════════════════════════════════════════════════
# 8. already_resolved: an alert no longer open resolves without action
# ═════════════════════════════════════════════════════════════════════════════
FX5="${WORK_ROOT}/fx5"
mkdir -p "${FX5}/go5"
: >"${FX5}/call.log"
alert 1 go/unsafe-quoting critical >"${FX5}/go5/alerts.json"
printf '{"state": "open"}' >"${FX5}/go5/rb.json"

A5="$(mk_anchor "t8")"
render_triage testorg '["go5"]' '[{"repo":"go5","alert_number":7,"rule_id":"go/unsafe-quoting","disposition":"suppress","rationale":"already fixed upstream","dismissed_reason":"false_positive"}]' 1 "${A5}" "${WORK_ROOT}/triage5.sh"
T8_RC=0
run_sub "${WORK_ROOT}/triage5.sh" "${FX5}" "${WORK_ROOT}/t8.out" "${WORK_ROOT}/t8.err" || T8_RC=$?
expect_equal "S8 already-resolved run exits 0" 0 "${T8_RC}"
TRIAGE5="$(tr -d '[:space:]' <"${WORK_ROOT}/t8.out")"
check "S8 row already_resolved" jq -e '.repos[0].dispositions[0].result == "already_resolved"' "${TRIAGE5}" >/dev/null
check_not "S8 no PATCH fired" grep -q '^PATCH ' "${FX5}/call.log"
check "S8 budget untouched" jq -e '.summary.budget_used == 0 and .summary.already_resolved_total == 1' "${TRIAGE5}" >/dev/null

# ═════════════════════════════════════════════════════════════════════════════
# 9. Unplanned open alerts: report-only rows, never widened
# ═════════════════════════════════════════════════════════════════════════════
FX6="${WORK_ROOT}/fx6"
mkdir -p "${FX6}/go6"
: >"${FX6}/call.log"
{ alert 1 go/unsafe-quoting critical; alert 2 go/sql-injection high; } >"${FX6}/go6/alerts.json"
printf '{"state": "open"}' >"${FX6}/go6/rb.json"

A6="$(mk_anchor "t9")"
render_triage testorg '["go6"]' '[]' 1 "${A6}" "${WORK_ROOT}/triage6.sh"
T9_RC=0
run_sub "${WORK_ROOT}/triage6.sh" "${FX6}" "${WORK_ROOT}/t9.out" "${WORK_ROOT}/t9.err" || T9_RC=$?
expect_equal "S9 unplanned run exits 0" 0 "${T9_RC}"
TRIAGE6="$(tr -d '[:space:]' <"${WORK_ROOT}/t9.out")"
check "S9 both open alerts are unplanned report-only rows" jq -e '.repos[0].dispositions[0].result == "unplanned" and .repos[0].dispositions[1].result == "unplanned"' "${TRIAGE6}" >/dev/null
check "S9 unplanned rows carry severity and open state" jq -e '.repos[0].dispositions[0].severity == "critical" and .repos[0].dispositions[1].severity == "high" and .repos[0].dispositions[0].readback_state == "open"' "${TRIAGE6}" >/dev/null
check "S9 no PATCH, budget untouched" jq -e '.summary.budget_used == 0 and .summary.unplanned_total == 2' "${TRIAGE6}" >/dev/null
check "S9 unplanned note is report-only" jq -e '.repos[0].dispositions[0].note | contains("report-only")' "${TRIAGE6}" >/dev/null

# ═════════════════════════════════════════════════════════════════════════════
# 10. readback_mismatch: dismissal PATCH landed but read-back still open ->
#     budget NOT consumed (a row the next run retries)
# ═════════════════════════════════════════════════════════════════════════════
FX7="${WORK_ROOT}/fx7"
mkdir -p "${FX7}/go7"
: >"${FX7}/call.log"
alert 1 go/unsafe-quoting critical >"${FX7}/go7/alerts.json"
alert 1 go/unsafe-quoting critical >"${FX7}/go7/alerts.after.json"
printf '{"state": "open"}' >"${FX7}/go7/rb.json"
printf '{"state": "open"}' >"${FX7}/go7/rb.after.json"

A7="$(mk_anchor "t10")"
render_triage testorg '["go7"]' '[{"repo":"go7","alert_number":1,"rule_id":"go/unsafe-quoting","disposition":"suppress","rationale":"read-back race fixture","dismissed_reason":"false_positive"}]' 3 "${A7}" "${WORK_ROOT}/triage7.sh"
T10_RC=0
run_sub "${WORK_ROOT}/triage7.sh" "${FX7}" "${WORK_ROOT}/t10.out" "${WORK_ROOT}/t10.err" || T10_RC=$?
expect_equal "S10 readback-mismatch run exits 0" 0 "${T10_RC}"
TRIAGE7="$(tr -d '[:space:]' <"${WORK_ROOT}/t10.out")"
check "S10 row readback_mismatch with open state" jq -e '.repos[0].dispositions[0].result == "readback_mismatch" and .repos[0].dispositions[0].readback_state == "open"' "${TRIAGE7}" >/dev/null
check "S10 budget NOT consumed" jq -e '.summary.budget_used == 0 and .summary.suppressions_failed == 1' "${TRIAGE7}" >/dev/null
check "S10 held_at_zero false (alert still open)" jq -e '.summary.held_at_zero == false' "${TRIAGE7}" >/dev/null

# ═════════════════════════════════════════════════════════════════════════════
# 11. dismiss_failed: a rejected PATCH is bookkeeping, step still rc 0
# ═════════════════════════════════════════════════════════════════════════════
FX8="${WORK_ROOT}/fx8"
mkdir -p "${FX8}/go8"
: >"${FX8}/call.log"
alert 1 go/unsafe-quoting critical >"${FX8}/go8/alerts.json"
printf '{"state": "open"}' >"${FX8}/go8/rb.json"
printf '1' >"${FX8}/go8/dismiss.rc"
printf 'gh: HTTP 422: dismissed_reason is invalid' >"${FX8}/go8/dismiss.err"

A8="$(mk_anchor "t11")"
render_triage testorg '["go8"]' '[{"repo":"go8","alert_number":1,"rule_id":"go/unsafe-quoting","disposition":"suppress","rationale":"rejection fixture","dismissed_reason":"false_positive"}]' 3 "${A8}" "${WORK_ROOT}/triage8.sh"
T11_RC=0
run_sub "${WORK_ROOT}/triage8.sh" "${FX8}" "${WORK_ROOT}/t11.out" "${WORK_ROOT}/t11.err" || T11_RC=$?
expect_equal "S11 dismissal-failure run exits 0" 0 "${T11_RC}"
TRIAGE8="$(tr -d '[:space:]' <"${WORK_ROOT}/t11.out")"
check "S11 row dismiss_failed with the 422 detail" jq -e '.repos[0].dispositions[0].result == "dismiss_failed" and (.repos[0].dispositions[0].note | contains("422"))' "${TRIAGE8}" >/dev/null
check "S11 budget NOT consumed" jq -e '.summary.budget_used == 0 and .summary.suppressions_failed == 1' "${TRIAGE8}" >/dev/null

# ═════════════════════════════════════════════════════════════════════════════
# 12. readback_failed: the read-back itself failed transport-wise
# ═════════════════════════════════════════════════════════════════════════════
FX9="${WORK_ROOT}/fx9"
mkdir -p "${FX9}/go9"
: >"${FX9}/call.log"
alert 1 go/unsafe-quoting critical >"${FX9}/go9/alerts.json"
printf '{"state": "open"}' >"${FX9}/go9/rb.json"
printf '1' >"${FX9}/go9/rb.rc"
printf 'gh: connection reset by peer' >"${FX9}/go9/rb.err"

A9="$(mk_anchor "t12")"
render_triage testorg '["go9"]' '[{"repo":"go9","alert_number":1,"rule_id":"go/unsafe-quoting","disposition":"suppress","rationale":"readback transport fixture","dismissed_reason":"false_positive"}]' 3 "${A9}" "${WORK_ROOT}/triage9.sh"
T12_RC=0
run_sub "${WORK_ROOT}/triage9.sh" "${FX9}" "${WORK_ROOT}/t12.out" "${WORK_ROOT}/t12.err" || T12_RC=$?
expect_equal "S12 readback-failure run exits 0" 0 "${T12_RC}"
TRIAGE9="$(tr -d '[:space:]' <"${WORK_ROOT}/t12.out")"
check "S12 row readback_failed without a state" jq -e '.repos[0].dispositions[0].result == "readback_failed" and .repos[0].dispositions[0].readback_state == null' "${TRIAGE9}" >/dev/null
check "S12 budget NOT consumed" jq -e '.summary.budget_used == 0 and .summary.suppressions_failed == 1' "${TRIAGE9}" >/dev/null

# ═════════════════════════════════════════════════════════════════════════════
# 13. CodeQL disabled (404 lane): disabled repo lane, rows recorded, never acted
# ═════════════════════════════════════════════════════════════════════════════
FX10="${WORK_ROOT}/fx10"
mkdir -p "${FX10}/go10"
: >"${FX10}/call.log"
printf 'gh: HTTP 404: CodeQL is not enabled for this repo' >"${FX10}/go10/enum1.err"
printf '1' >"${FX10}/go10/enum1.rc"

A10="$(mk_anchor "t13")"
render_triage testorg '["go10"]' '[{"repo":"go10","alert_number":1,"rule_id":"go/unsafe-quoting","disposition":"suppress","rationale":"disabled-lane fixture","dismissed_reason":"false_positive"}]' 3 "${A10}" "${WORK_ROOT}/triage10.sh"
T13_RC=0
run_sub "${WORK_ROOT}/triage10.sh" "${FX10}" "${WORK_ROOT}/t13.out" "${WORK_ROOT}/t13.err" || T13_RC=$?
expect_equal "S13 disabled run exits 0" 0 "${T13_RC}"
TRIAGE10="$(tr -d '[:space:]' <"${WORK_ROOT}/t13.out")"
check "S13 repo lane is disabled (not error)" jq -e '.repos[0].codeql_status == "disabled" and .summary.repos_disabled == 1 and .summary.repos_error == 0' "${TRIAGE10}" >/dev/null
check "S13 row repo_codeql_disabled" jq -e '.repos[0].dispositions[0].result == "repo_codeql_disabled"' "${TRIAGE10}" >/dev/null
check_not "S13 no PATCH on a disabled repo" grep -q '^PATCH ' "${FX10}/call.log"
check "S13 held_at_zero still true (disabled is not an error)" jq -e '.summary.held_at_zero == true' "${TRIAGE10}" >/dev/null

# ═════════════════════════════════════════════════════════════════════════════
# 14. Enumeration failure (non-404): error lane + honest held_at_zero=false
# ═════════════════════════════════════════════════════════════════════════════
FX11="${WORK_ROOT}/fx11"
mkdir -p "${FX11}/go11"
: >"${FX11}/call.log"
printf 'gh: HTTP 502: upstream error' >"${FX11}/go11/enum1.err"
printf '1' >"${FX11}/go11/enum1.rc"

A11="$(mk_anchor "t14")"
render_triage testorg '["go11"]' '[{"repo":"go11","alert_number":1,"rule_id":"go/unsafe-quoting","disposition":"suppress","rationale":"enum-failure fixture","dismissed_reason":"false_positive"}]' 3 "${A11}" "${WORK_ROOT}/triage11.sh"
T14_RC=0
run_sub "${WORK_ROOT}/triage11.sh" "${FX11}" "${WORK_ROOT}/t14.out" "${WORK_ROOT}/t14.err" || T14_RC=$?
expect_equal "S14 enum-failure run exits 0" 0 "${T14_RC}"
TRIAGE11="$(tr -d '[:space:]' <"${WORK_ROOT}/t14.out")"
check "S14 repo lane is error" jq -e '.repos[0].codeql_status == "error" and .summary.repos_error == 1' "${TRIAGE11}" >/dev/null
check "S14 row repo_enumeration_failed + structured error surface" jq -e '.repos[0].dispositions[0].result == "repo_enumeration_failed" and .repos[0].errors[0].surface == "codeql"' "${TRIAGE11}" >/dev/null
check "S14 held_at_zero can NOT be claimed" jq -e '.summary.held_at_zero == false' "${TRIAGE11}" >/dev/null
check_not "S14 no PATCH fired on enum failure" grep -q '^PATCH ' "${FX11}/call.log"

# ═════════════════════════════════════════════════════════════════════════════
# 15. Post-run re-enumeration failure: dismissal counts, held_at_zero FALSE
# ═════════════════════════════════════════════════════════════════════════════
FX12="${WORK_ROOT}/fx12"
mkdir -p "${FX12}/go15"
: >"${FX12}/call.log"
alert 1 go/unsafe-quoting critical >"${FX12}/go15/alerts.json"
printf '{"state": "open"}' >"${FX12}/go15/rb.json"
printf '{"state": "dismissed"}' >"${FX12}/go15/rb.after.json"
printf 'gh: HTTP 500: flake' >"${FX12}/go15/enum2.err"
printf '1' >"${FX12}/go15/enum2.rc"

A12="$(mk_anchor "t15")"
render_triage testorg '["go15"]' '[{"repo":"go15","alert_number":1,"rule_id":"go/unsafe-quoting","disposition":"suppress","rationale":"re-enum-failure fixture","dismissed_reason":"false_positive"}]' 3 "${A12}" "${WORK_ROOT}/triage12.sh"
T15_RC=0
run_sub "${WORK_ROOT}/triage12.sh" "${FX12}" "${WORK_ROOT}/t15.out" "${WORK_ROOT}/t15.err" || T15_RC=$?
expect_equal "S15 re-enum-failure run exits 0" 0 "${T15_RC}"
TRIAGE12="$(tr -d '[:space:]' <"${WORK_ROOT}/t15.out")"
check "S15 repo flipped to error" jq -e '.repos[0].codeql_status == "error" and .summary.repos_error == 1' "${TRIAGE12}" >/dev/null
check "S15 dismissal still recorded as executed" jq -e '.summary.budget_used == 1' "${TRIAGE12}" >/dev/null
check "S15 held_at_zero can NOT be claimed" jq -e '.summary.held_at_zero == false' "${TRIAGE12}" >/dev/null
check "S15 structured error explains the hold" jq -e '.repos[0].errors[0].detail | contains("held_at_zero")' "${TRIAGE12}" >/dev/null

# ═════════════════════════════════════════════════════════════════════════════
# 16. Empty plan verify-only run: SUCCESS with no rows and zero budget
# ═════════════════════════════════════════════════════════════════════════════
FX13="${WORK_ROOT}/fx13"
mkdir -p "${FX13}/go16"
: >"${FX13}/call.log"
printf '[]\n' >"${FX13}/go16/alerts.json"

A13="$(mk_anchor "t16")"
render_triage testorg '["go16"]' '[]' 1 "${A13}" "${WORK_ROOT}/triage13.sh"
T16_RC=0
run_sub "${WORK_ROOT}/triage13.sh" "${FX13}" "${WORK_ROOT}/t16.out" "${WORK_ROOT}/t16.err" || T16_RC=$?
expect_equal "S16 verify-only run exits 0" 0 "${T16_RC}"
TRIAGE13="$(tr -d '[:space:]' <"${WORK_ROOT}/t16.out")"
check "S16 empty plan + no alerts = held_at_zero" jq -e '.summary.held_at_zero == true and .summary.budget_used == 0 and .summary.unplanned_total == 0' "${TRIAGE13}" >/dev/null
check "S16 no dispositions at all" jq -e '.repos[0].dispositions == []' "${TRIAGE13}" >/dev/null

# ═════════════════════════════════════════════════════════════════════════════
# 17. Per-run budget across repos: two repos x one suppress row, budget=1 —
#     exactly ONE dismissal lands, the second repo's row is out_of_budget,
#     rc 0, and the structural self-check stays consistent (the budget is
#     one counter for the WHOLE sweep, never per repo)
# ═════════════════════════════════════════════════════════════════════════════
FX14="${WORK_ROOT}/fx14"
mkdir -p "${FX14}/goA" "${FX14}/goB"
: >"${FX14}/call.log"
alert 1 go/unsafe-quoting critical >"${FX14}/goA/alerts.json"
printf '{"state": "open"}' >"${FX14}/goA/rb.json"
printf '{"state": "dismissed"}' >"${FX14}/goA/rb.after.json"
alert 1 go/unsafe-quoting critical >"${FX14}/goB/alerts.json"
printf '{"state": "open"}' >"${FX14}/goB/rb.json"

A14="$(mk_anchor "t17")"
render_triage testorg '["goA","goB"]' '[{"repo":"goA","alert_number":1,"rule_id":"go/unsafe-quoting","disposition":"suppress","rationale":"first repo row","dismissed_reason":"false_positive"},{"repo":"goB","alert_number":1,"rule_id":"go/unsafe-quoting","disposition":"suppress","rationale":"second repo row","dismissed_reason":"wont_fix"}]' 1 "${A14}" "${WORK_ROOT}/triage14.sh"
T17_RC=0
run_sub "${WORK_ROOT}/triage14.sh" "${FX14}" "${WORK_ROOT}/t17.out" "${WORK_ROOT}/t17.err" || T17_RC=$?
expect_equal "S17 cross-repo budgeted run exits 0" 0 "${T17_RC}"
TRIAGE14="$(tr -d '[:space:]' <"${WORK_ROOT}/t17.out")"
check "S17 budget_used hits the cap exactly once" jq -e '.summary.budget_used == 1 and .summary.budget == 1 and .summary.suppressions_executed == 1' "${TRIAGE14}" >/dev/null
check "S17 the second repo's row is typed out_of_budget" jq -e '.summary.out_of_budget_total == 1 and .repos[1].dispositions[0].result == "out_of_budget"' "${TRIAGE14}" >/dev/null
check "S17 first repo's row dismissed with read-back" jq -e '.repos[0].repo == "goA" and .repos[0].dispositions[0].result == "dismissed"' "${TRIAGE14}" >/dev/null
check "S17 out-of-budget row leaves the evidence open" jq -e '.repos[1].repo == "goB" and .repos[1].dispositions[0].readback_state == "open"' "${TRIAGE14}" >/dev/null
check "S17 exactly ONE PATCH across the whole sweep" test "$(grep -c '^PATCH ' "${FX14}/call.log")" -eq 1
check_not "S17 no PATCH ever landed on repo B" grep -q '^PATCH repos/testorg/goB/' "${FX14}/call.log"
check "S17 held_at_zero stays false with goB still open" jq -e '.summary.held_at_zero == false and .repos[1].alerts_after == 1' "${TRIAGE14}" >/dev/null

# ═════════════════════════════════════════════════════════════════════════════
# 18. Protocol + hygiene: stdout path only, stderr diagnostics, token canary
# ═════════════════════════════════════════════════════════════════════════════
check "S18 triage diagnostics go to stderr as triage: lines" grep -q '^triage: triage plan: ' "${WORK_ROOT}/t3.err"
check_not "S18 stderr carries no artifact JSON" grep -q '"dispositions"' "${WORK_ROOT}/t3.err"
check_not "S18 token canary absent from the S3 artifact" grep -q "supersecret-token-canary" "${TRIAGE1}"
check_not "S18 token canary absent from S3 stderr" grep -q "supersecret-token-canary" "${WORK_ROOT}/t3.err"
check_not "S18 token canary absent from the S5 artifact" grep -q "supersecret-token-canary" "${TRIAGE2}"
TR3DIR="$(dirname "$(printf '%s\n' "${TRIAGE1}")")"
check "S18 no .triage scratch files left in the run dir" test "$(ls -A "${TR3DIR}" 2>/dev/null | grep -c '^\.triage')" = "0"

if [ "${FAILURES}" -gt 0 ]; then
    echo "FAILED: ${FAILURES} assertion(s)" >&2
    exit 1
fi
check "suite leaves no scratch leak beside its fixtures" test ! -e "${SCRIPT_DIR}/testorg"
check "suite leaves no scratch leak at the tree root" test ! -e "${ROOT_DIR}/../testorg"
echo "PASS: devops_maint_v1 codeql-triage-step regression suite verified"
