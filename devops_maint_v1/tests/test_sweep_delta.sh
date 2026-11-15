#!/usr/bin/env bash
# devops_maint_v1 — Lane B delta-report-step regression suite (KB-410,
# ADR-0015 D6).
#
# Exercises the rendered sweep_delta.sh END-TO-END through a stub gh (no
# network, no real GitHub):
#   - wiring: main.chcl splices the template from the real template + wrapper
#   - config gates fire BEFORE any network call (bad org/dup or empty repos/
#     bad pack/out-of-scope plan rows -> typed failure, zero gh invocations)
#   - before-inventory gates: unreadable file, wrong schema, org mismatch
#   - delta arithmetic per repo and per surface, both pr-flatten and the
#     multi-paginate code-scanning flatten (same shape as collect)
#   - delta_class precedence: partial_error > regressed > improved > clean >
#     unchanged, recomputed independently by the artifact self-check
#   - rulesets transitions: applied 0 -> 1 is improved, 1 -> 0 is regressed
#   - CodeQL disabled (HTTP 404 on the alerts API) is a zero-alert state, not
#     an error; a transport failure is an honest partial_error row
#   - all-repos-failed re-enumeration -> typed failure with the artifact kept
#   - before-row-missing -> zeroed-before computed delta flagged
#     before_inventory, inventory_collected_before false
#   - unplanned repos stay report-only (pack null) but are still enumerated
#   - advisories: carried from the before inventory, never re-scanned
#   - stdout protocol: the artifact path and nothing else; log() on stderr
#   - token hygiene: the GH_TOKEN canary never lands in artifacts or stderr
#   - scratch (.delta.raw.*) is cleaned up; the artifact survives beside the
#     before inventory
#   - tree validation (node + criteria), matching the other step suites

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
TREE_DIR="$(cd "${ROOT_DIR}/.." && pwd)"
TEMPLATE="${ROOT_DIR}/scripts/sweep_delta.sh.tftpl"
WRAPPER="${ROOT_DIR}/scripts/gh_retry.sh.tftpl"
MAIN="${ROOT_DIR}/main.chcl"
ADAPTERS="${ROOT_DIR}/adapters.chcl"

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

expect_count() {
    if [ "$2" = "$3" ]; then
        echo "PASS: $1" >&2
    else
        echo "FAIL: $1 (want $2, got $3)" >&2
        FAILURES=$((FAILURES + 1))
    fi
}

assert_no_single_quote() {
    local value="$1"
    case "${value}" in *"'"*) echo "BUG: test value contains a single quote: ${value}" >&2; exit 99 ;; esac
}

# ── Renderer ─────────────────────────────────────────────────────────────────
# Replays the templatefile splice faithfully, exactly like the other step
# suites: the gh_retry wrapper is content-substituted raw; shellquote markers
# render as single-quoted literals (test values must not contain single
# quotes); no {{ marker may survive the render.
render_sweep() { # render_sweep <org> <repos_json> <plan_json> <inventory_file> <out>
    local org="$1" repos_json="$2" plan_json="$3" inventory="$4" out="$5"
    assert_no_single_quote "${org}"
    assert_no_single_quote "${repos_json}"
    assert_no_single_quote "${plan_json}"
    assert_no_single_quote "${inventory}"
    local wrap
    wrap="$(cat "${WRAPPER}")"
    local rendered
    rendered="$(cat "${TEMPLATE}")"
    rendered="${rendered//'{{ .gh_retry }}'/"${wrap}"}"
    rendered="${rendered//'{{ .org | shellquote }}'/"'${org}'"}"
    rendered="${rendered//'{{ .repos_json | shellquote }}'/"'${repos_json}'"}"
    rendered="${rendered//'{{ .plan_json | shellquote }}'/"'${plan_json}'"}"
    rendered="${rendered//'{{ .inventory_file | shellquote }}'/"'${inventory}'"}"
    printf '%s' "${rendered}" >"${out}"
    if grep -q '{{' "${out}"; then
        echo "BUG: rendered sweep template still contains template markers" >&2
        exit 99
    fi
    chmod +x "${out}"
}

# ── Stub gh ──────────────────────────────────────────────────────────────────
# Fixture-driven; every invocation appends its full argv to SWD_GH_LOG.
#   gh pr list -R ORG/REPO --state open --limit 200 --json number,author
#     -> <fixtures>/<repo>/pr.json (default: empty page), pr.rc/pr.err for rc
#   gh api repos/ORG/REPO/code-scanning/alerts?... --paginate
#     -> <fixtures>/<repo>/cq.json (page documents in sequence); cq.rc=1
#        (canned HTTP 404 -> CodeQL disabled lane), cq.rc=2 (transport lane)
#   gh api repos/ORG/REPO/rulesets?per_page=100 --paginate
#     -> <fixtures>/<repo>/rs.json (default: no rulesets); rs.rc/rs.err
cat >"${BIN_STUB_GH}/gh" <<'EOF'
#!/usr/bin/env bash
set -u
fx="${SWD_GH_FIXTURES:?SWD_GH_FIXTURES not set}"
log="${SWD_GH_LOG:-/dev/null}"
printf '%s\n' "$*" >>"${log}"

fxr_of() { # <repo> -> fixture dir or die (wiring mistakes must be loud)
    if [ -d "${fx}/$1" ]; then
        printf '%s/%s' "${fx}" "$1"
        return 0
    fi
    echo "stub: no fixture dir for repo $1" >&2
    exit 9
}

case "${1:-}" in
    pr)
        if [ "${2:-}" != "list" ] || [ "${3:-}" != "-R" ]; then
            echo "stub: unexpected gh pr invocation: $*" >&2
            exit 9
        fi
        org_repo="${4:-}"
        case "${org_repo}" in */*) ;; *) echo "stub: bad -R value: ${org_repo}" >&2; exit 9 ;; esac
        repo="${org_repo#*/}"
        fxr="$(fxr_of "${repo}")"
        if [ -f "${fxr}/pr.rc" ]; then
            rc="$(cat "${fxr}/pr.rc")"
            [ -f "${fxr}/pr.err" ] && cat "${fxr}/pr.err" >&2
            exit "$rc"
        fi
        if [ -f "${fxr}/pr.json" ]; then cat "${fxr}/pr.json"; else echo "[]"; fi
        ;;
    api)
        url="${2:-}"
        case "${url}" in
            repos/*/code-scanning/alerts*|repos/*/rulesets*) ;;
            *) echo "stub: unknown gh api URL: ${url}" >&2; exit 9 ;;
        esac
        repo="$(printf '%s' "${url}" | cut -d/ -f3)"
        fxr="$(fxr_of "${repo}")"
        case "${url}" in
            *code-scanning/alerts*)
                if [ -f "${fxr}/cq.rc" ]; then
                    rc="$(cat "${fxr}/cq.rc")"
                    if [ -f "${fxr}/cq.err" ]; then cat "${fxr}/cq.err" >&2
                    elif [ "${rc}" = "1" ]; then echo 'gh: CodeQL was not found (HTTP 404)' >&2
                    else echo 'gh: transport broken (connection reset by peer)' >&2; fi
                    exit "$rc"
                fi
                if [ -f "${fxr}/cq.json" ]; then cat "${fxr}/cq.json"; else echo "[]"; fi
                ;;
            *rulesets*)
                if [ -f "${fxr}/rs.rc" ]; then
                    rc="$(cat "${fxr}/rs.rc")"
                    if [ -f "${fxr}/rs.err" ]; then cat "${fxr}/rs.err" >&2
                    else echo 'gh: transport broken (connection reset by peer)' >&2; fi
                    exit "$rc"
                fi
                if [ -f "${fxr}/rs.json" ]; then cat "${fxr}/rs.json"; else echo "[]"; fi
                ;;
        esac
        ;;
    *)
        echo "stub: unknown gh subcommand: ${1:-<none>}" >&2
        exit 9
        ;;
esac
EOF
chmod +x "${BIN_STUB_GH}/gh"

# ── Fixtures ─────────────────────────────────────────────────────────────────
mk_inventory() { # mk_inventory <path> <org> <scope_csv> <rows_json>
    printf '{"schema":"devops_maint_v1.inventory.v1","sweep":{"org":"%s","repos":[%s]},"repos":%s}\n' "$2" "$3" "$4" >"$1"
}

mk_fixtures() { # mk_fixtures <dir>
    local fx="$1"
    mkdir -p "${fx}"
    : >"${fx}/call.log"
}

# ── Run helper ───────────────────────────────────────────────────────────────
run_sweep() { # run_sweep <idx> <org> <repos_json> <plan_json> <inventory_file>
    local idx="$1" org="$2" repos_json="$3" plan_json="$4" inventory="$5"
    local fx="${WORK_ROOT}/fx${idx}"
    mkdir -p "${fx}"
    render_sweep "${org}" "${repos_json}" "${plan_json}" "${inventory}" "${WORK_ROOT}/sweep${idx}.sh"
    bash -n "${WORK_ROOT}/sweep${idx}.sh"
    SWD_GH_FIXTURES="${fx}" \
        SWD_GH_LOG="${fx}/call.log" \
        GH_RETRY_MAX_ATTEMPTS=1 \
        GH_RETRY_BACKOFF_SECONDS="0 0 0" \
        GH_RETRY_SCRIPT_SLEEP_BUDGET=0 \
        GH_TOKEN='supersecret-token-canary' \
        PATH="${BIN_STUB_GH}:${PATH}" \
        bash "${WORK_ROOT}/sweep${idx}.sh" \
        >"${WORK_ROOT}/sweep${idx}.out" 2>"${WORK_ROOT}/sweep${idx}.err"
}

# ═══════════════════════════════════════════════════════════════════════════
# S1 — wiring: main.chcl splices the delta template with the real wrapper
# ═══════════════════════════════════════════════════════════════════════════
check "S1 main.chcl targets the delta_report adapter" grep -qF 'target  = adapter.shell.delta_report' "${MAIN}"
check "S1 main.chcl renders sweep_delta.sh.tftpl" grep -qF 'templatefile("./scripts/sweep_delta.sh.tftpl"' "${MAIN}"
check "S1 main.chcl feeds the inventory artifact as the before" grep -qF 'inventory_file  = data.internal.inventory_file.value' "${MAIN}"
check "S1 main.chcl scopes the sweep by repos_json" grep -qF 'repos_json      = jsonencode(var.repos)' "${MAIN}"
check "S1 main.chcl carries the protection plan" grep -qF 'plan_json       = jsonencode(var.protection_plan)' "${MAIN}"
check "S1 adapters.chcl declares the delta_report shell adapter" grep -qF 'adapter "shell" "delta_report"' "${ADAPTERS}"
check "S1 wrapper copy is byte-identical to the canonical vendor" cmp -s "${WRAPPER}" "${TREE_DIR}/workstream_handler_v1/scripts/gh_retry.sh.tftpl"

FX1="${WORK_ROOT}/fx1"
mk_fixtures "${FX1}"
mkdir -p "${FX1}/a1"
mk_inventory "${FX1}/inv.json" testorg '"a1"' \
    '[{"repo":"a1","collected":true,"prs":{"open_total":0,"dependabot_total":0,"other_open_total":0},"codeql":{"open_alerts":0,"status":"ok"},"rulesets":{"applied":false},"advisories":null}]'
render_sweep testorg '["a1"]' '[{"repo":"a1","pack":"go"}]' "${FX1}/inv.json" "${WORK_ROOT}/sweep1.sh"
check "S1 rendered script parses" bash -n "${WORK_ROOT}/sweep1.sh"
check "S1 rendered script splices the retry wrapper" grep -qF "$(head -1 "${WRAPPER}")" "${WORK_ROOT}/sweep1.sh"

# ═══════════════════════════════════════════════════════════════════════════
# S2 — config gates fire before any network call
# ═══════════════════════════════════════════════════════════════════════════
FX2="${WORK_ROOT}/fx2"
mk_fixtures "${FX2}"
mkdir -p "${FX2}/a1"
mk_inventory "${FX2}/inv.json" testorg '"a1"' \
    '[{"repo":"a1","collected":true,"prs":{"open_total":0,"dependabot_total":0,"other_open_total":0},"codeql":{"open_alerts":0,"status":"ok"},"rulesets":{"applied":false},"advisories":null}]'

gated_run() { # gated_run <tag> <org> <repos> <plan>
    local tag="$1" org="$2" repos="$3" plan="$4"
    render_sweep "${org}" "${repos}" "${plan}" "${FX2}/inv.json" "${WORK_ROOT}/sweep2${tag}.sh"
    SWD_GH_FIXTURES="${FX2}" SWD_GH_LOG="${FX2}/call.log" GH_RETRY_MAX_ATTEMPTS=1 \
        GH_RETRY_BACKOFF_SECONDS="0 0 0" GH_RETRY_SCRIPT_SLEEP_BUDGET=0 \
        GH_TOKEN='supersecret-token-canary' PATH="${BIN_STUB_GH}:${PATH}" \
        bash "${WORK_ROOT}/sweep2${tag}.sh" >/dev/null 2>"${WORK_ROOT}/sweep2${tag}.err" || true
}

gated_run a 'test_org' '["a1"]' '[{"repo":"a1","pack":"go"}]'
check "S2a bad org fails typed" grep -qF 'delta_failed: org failed its shape check' "${WORK_ROOT}/sweep2a.err"
expect_count "S2a no network before the org gate" 0 "$(wc -l <"${FX2}/call.log")"

gated_run b testorg '["a1","a1"]' '[{"repo":"a1","pack":"go"}]'
check "S2b duplicate repos fail typed" grep -qF 'delta_failed: repos failed their shape check' "${WORK_ROOT}/sweep2b.err"

gated_run c testorg '[]' '[{"repo":"a1","pack":"go"}]'
check "S2c empty sweep scope fails typed" grep -qF 'delta_failed: repos failed their shape check' "${WORK_ROOT}/sweep2c.err"

gated_run d testorg '["a1"]' '[{"repo":"a1","pack":"node"}]'
check "S2d unknown pack fails typed" grep -qF 'delta_failed: delta plan failed its shape check' "${WORK_ROOT}/sweep2d.err"

gated_run e testorg '["a1"]' '[{"repo":"outside","pack":"go"}]'
check "S2e out-of-scope plan row fails typed" grep -qF 'delta_failed: plan row targets repo outside the sweep scope' "${WORK_ROOT}/sweep2e.err"

gated_run f testorg '["a1"]' '[{"repo":"a1","pack":"go","starter_language":"ts"}]'
check "S2f starter_language on a non-starter row fails typed" grep -qF 'delta_failed: delta plan failed its shape check' "${WORK_ROOT}/sweep2f.err"

gated_run g testorg '["a1"]' '[]'
check "S2g empty plan fails typed" grep -qF 'delta_failed: delta plan failed its shape check' "${WORK_ROOT}/sweep2g.err"

gated_run h testorg '["a1"]' '[{"repo":"a1","pack":"starter"}]'
check "S2h starter row without starter_language fails typed" grep -qF 'delta_failed: delta plan failed its shape check' "${WORK_ROOT}/sweep2h.err"

gated_run i testorg '["bad name"]' '[{"repo":"bad name","pack":"go"}]'
check "S2i repo name with whitespace fails typed" grep -qF 'delta_failed: repo name failed its shape check' "${WORK_ROOT}/sweep2i.err"
check "S2i gate names the offending value" grep -qF 'got: bad name)' "${WORK_ROOT}/sweep2i.err"
expect_count "S2i no network before the repo-name gate" 0 "$(wc -l <"${FX2}/call.log")"

expect_count "S2 no sweep run reached the network" 0 "$(wc -l <"${FX2}/call.log")"
check_not "S2 no artifact beside the gate-failed inventory" test -e "${FX2}/delta.json"

# ═══════════════════════════════════════════════════════════════════════════
# S3 — before-inventory gates
# ═══════════════════════════════════════════════════════════════════════════
FX3="${WORK_ROOT}/fx3"
mk_fixtures "${FX3}"

render_sweep testorg '["a1"]' '[{"repo":"a1","pack":"go"}]' "${FX3}/missing.json" "${WORK_ROOT}/sweep3a.sh"
SWD_GH_FIXTURES="${FX3}" SWD_GH_LOG="${FX3}/call.log" GH_RETRY_MAX_ATTEMPTS=1 \
    GH_RETRY_BACKOFF_SECONDS="0 0 0" GH_RETRY_SCRIPT_SLEEP_BUDGET=0 \
    GH_TOKEN='supersecret-token-canary' PATH="${BIN_STUB_GH}:${PATH}" \
    bash "${WORK_ROOT}/sweep3a.sh" >/dev/null 2>"${WORK_ROOT}/sweep3a.err" || true
check "S3a unreadable inventory fails typed" grep -qF 'delta_failed: before inventory not readable' "${WORK_ROOT}/sweep3a.err"

printf '{"schema":"devops_maint_v1.inventory.v9","sweep":{"org":"testorg"},"repos":[]}\n' >"${FX3}/inv3b.json"
render_sweep testorg '["a1"]' '[{"repo":"a1","pack":"go"}]' "${FX3}/inv3b.json" "${WORK_ROOT}/sweep3b.sh"
SWD_GH_FIXTURES="${FX3}" SWD_GH_LOG="${FX3}/call.log" GH_RETRY_MAX_ATTEMPTS=1 \
    GH_RETRY_BACKOFF_SECONDS="0 0 0" GH_RETRY_SCRIPT_SLEEP_BUDGET=0 \
    GH_TOKEN='supersecret-token-canary' PATH="${BIN_STUB_GH}:${PATH}" \
    bash "${WORK_ROOT}/sweep3b.sh" >/dev/null 2>"${WORK_ROOT}/sweep3b.err" || true
check "S3b wrong inventory schema fails typed" grep -qF 'delta_failed: before inventory failed its schema check' "${WORK_ROOT}/sweep3b.err"

mk_inventory "${FX3}/inv3c.json" otherorg '"a1"' \
    '[{"repo":"a1","collected":true,"prs":{"open_total":0,"dependabot_total":0,"other_open_total":0},"codeql":{"open_alerts":0,"status":"ok"},"rulesets":{"applied":false},"advisories":null}]'
render_sweep testorg '["a1"]' '[{"repo":"a1","pack":"go"}]' "${FX3}/inv3c.json" "${WORK_ROOT}/sweep3c.sh"
SWD_GH_FIXTURES="${FX3}" SWD_GH_LOG="${FX3}/call.log" GH_RETRY_MAX_ATTEMPTS=1 \
    GH_RETRY_BACKOFF_SECONDS="0 0 0" GH_RETRY_SCRIPT_SLEEP_BUDGET=0 \
    GH_TOKEN='supersecret-token-canary' PATH="${BIN_STUB_GH}:${PATH}" \
    bash "${WORK_ROOT}/sweep3c.sh" >/dev/null 2>"${WORK_ROOT}/sweep3c.err" || true
check "S3c org mismatch fails typed before any gh call" grep -qF 'delta_failed: before inventory org does not match the sweep org' "${WORK_ROOT}/sweep3c.err"
expect_count "S3 no sweep run reached the network" 0 "$(wc -l <"${FX3}/call.log")"

# ═══════════════════════════════════════════════════════════════════════════
# S4 — happy sweep: three delta classes + the exact collect-predicate gh calls
# ═══════════════════════════════════════════════════════════════════════════
FX4="${WORK_ROOT}/fx4"
mk_fixtures "${FX4}"
for r in g1 g2 g3; do mkdir -p "${FX4}/${r}"; done
mk_inventory "${FX4}/inv.json" testorg '"g1","g2","g3"' '
[{"repo":"g1","collected":true,
  "prs":{"open_total":2,"dependabot_total":1,"other_open_total":1},
  "codeql":{"open_alerts":23,"status":"ok"},
  "rulesets":{"applied":false},
  "advisories":[{"id":"GHSA-test-1","severity":"critical"}]},
 {"repo":"g2","collected":true,
  "prs":{"open_total":10,"dependabot_total":6,"other_open_total":4},
  "codeql":{"open_alerts":23,"status":"ok"},
  "rulesets":{"applied":false},
  "advisories":[{"id":"GHSA-test-2"}]},
 {"repo":"g3","collected":true,
  "prs":{"open_total":0,"dependabot_total":0,"other_open_total":0},
  "codeql":{"open_alerts":0,"status":"disabled"},
  "rulesets":{"applied":false},
  "advisories":null}]'
# g1 regressed: one new dependabot PR after the sweep; the CodeQL after-state
# mirrors the before-state so the regression is attributable to PRs alone.
cat >"${FX4}/g1/pr.json" <<'EOF'
[{"number":607,"author":{"login":"dependabot[bot]"}},
 {"number":608,"author":{"login":"dependabot[bot]"}},
 {"number":391,"author":{"login":"mona"}}]
EOF
jq -n '[range(21; 44) | {number: .}]' >"${FX4}/g1/cq.json"
# g2 improved: consolidated carrier merges consumed the backlog; the CodeQL
# after-snapshot arrives as TWO page documents to exercise --paginate flatten.
cat >"${FX4}/g2/pr.json" <<'EOF'
[{"number":546,"author":{"login":"dependabot[bot]"}},
 {"number":438,"author":{"login":"dependabot[bot]"}},
 {"number":29,"author":{"login":"mona"}},
 {"number":31,"author":{"login":"hubot"}}]
EOF
cat >"${FX4}/g2/cq.json" <<'EOF'
[{"number":1,"state":"open"},{"number":2,"state":"open"},{"number":3,"state":"open"}]
[{"number":4,"state":"open"},{"number":5,"state":"open"}]
EOF
# g3 stays clean: no PRs, CodeQL still disabled (404 -> jq 1), no rulesets.
printf '1\n' >"${FX4}/g3/cq.rc"
run_sweep 4 testorg '["g1","g2","g3"]' '[{"repo":"g1","pack":"go"},{"repo":"g2","pack":"ts"},{"repo":"g3","pack":"py"}]' "${FX4}/inv.json" || {
    echo "BUG: S4 sweep failed"; cat "${WORK_ROOT}/sweep4.err" >&2; exit 99;
}
ART4="${FX4}/delta.json"
OUT4="${WORK_ROOT}/sweep4.out"
check "S4 sweep exits rc 0" test -f "${ART4}"
expect_count "S4 stdout carries exactly one line" 1 "$(wc -l <"${OUT4}")"
expect_count "S4 stdout is the artifact path alone" 1 "$(grep -cF "${ART4}" "${OUT4}")"
check "S4 artifact schema is delta v1" jq -e '(.schema == "devops_maint_v1.delta.v1") and (.sweep.org == "testorg") and (.sweep.repos_total == 3)' "${ART4}" >/dev/null
check "S4 artifact anchors the before inventory" jq -e --arg p "${FX4}/inv.json" '.sweep.before_inventory == $p' "${ART4}" >/dev/null
check "S4 artifact is keyed to its sweep id" jq -e --arg sid "$(basename "$(dirname "${FX4}/inv.json")")" '.sweep.sweep_id == $sid' "${ART4}" >/dev/null
check "S4 g1 regressed with honest PR arithmetic" jq -e '[.repos[] | select(.repo == "g1")] | (.[0].delta_class == "regressed") and (.[0].deltas.prs_total == 1) and (.[0].deltas.prs_dependabot == 1) and (.[0].deltas.prs_other == 0) and (.[0].deltas.codeql_alerts == 0) and (.[0].deltas.rulesets_applied == 0)' "${ART4}" >/dev/null
check "S4 g1 before counts copied from the inventory" jq -e '[.repos[] | select(.repo == "g1")] | (.[0].before.prs_total == 2) and (.[0].before.prs_dependabot == 1) and (.[0].before.codeql_alerts == 23) and (.[0].before.rulesets_applied == false)' "${ART4}" >/dev/null
check "S4 g2 improved across PR and CodeQL surfaces" jq -e '[.repos[] | select(.repo == "g2")] | (.[0].delta_class == "improved") and (.[0].deltas.prs_total == -6) and (.[0].deltas.prs_dependabot == -4) and (.[0].deltas.prs_other == -2) and (.[0].deltas.codeql_alerts == -18)' "${ART4}" >/dev/null
check "S4 g2 after snapshot flattened both paginate pages" jq -e '[.repos[] | select(.repo == "g2")] | (.[0].after.codeql_alerts == 5) and (.[0].after.prs_total == 4) and (.[0].after.prs_dependabot == 2) and (.[0].after.codeql_status == "ok")' "${ART4}" >/dev/null
check "S4 g3 clean with a disabled CodeQL lane" jq -e '[.repos[] | select(.repo == "g3")] | (.[0].delta_class == "clean") and (.[0].after.codeql_status == "disabled") and (.[0].after.codeql_alerts == 0) and (.[0].after.rulesets_applied == false)' "${ART4}" >/dev/null
check "S4 advisories carried for rows that had them" jq -e '[.repos[] | select(.repo == "g1")] | (.[0].advisories.carried == true) and (.[0].advisories.carried_from[0].id == "GHSA-test-1") and (.[0].advisories.note | test("not re-scanned by the delta pass"))' "${ART4}" >/dev/null
check "S4 advisories null rows reported as uncarried" jq -e '[.repos[] | select(.repo == "g3")] | (.[0].advisories.carried == false) and (.[0].advisories.carried_from == null) and (.[0].advisories.note == "no advisory data in the before inventory")' "${ART4}" >/dev/null
check "S4 summary recounts all classes" jq -e '(.summary.repos_planned == 3) and (.summary.repos_clean == 1) and (.summary.repos_improved == 1) and (.summary.repos_regressed == 1) and (.summary.repos_unchanged == 0) and (.summary.repos_partial_error == 0) and (.summary.repos_failed_rows == 0)' "${ART4}" >/dev/null
check "S4 summary totals the deltas" jq -e '(.summary.prs_dependabot_delta_total == -3) and (.summary.prs_other_delta_total == -2) and (.summary.codeql_alerts_delta_total == -18) and (.summary.rulesets_applied_net == 0)' "${ART4}" >/dev/null
check "S4 PR enumeration uses the EXACT collect predicate" grep -qF 'pr list -R testorg/g2 --state open --limit 200 --json number,author' "${FX4}/call.log"
check "S4 CodeQL enumeration pins tool_name=CodeQL" grep -qF 'repos/testorg/g2/code-scanning/alerts?state=open&per_page=100&tool_name=CodeQL' "${FX4}/call.log"
check "S4 rulesets enumeration paginates" grep -qF 'repos/testorg/g3/rulesets?per_page=100 --paginate' "${FX4}/call.log"

# ═══════════════════════════════════════════════════════════════════════════
# S5 — unchanged: equal, non-zero backlog stays honestly unchanged
# ═══════════════════════════════════════════════════════════════════════════
FX5="${WORK_ROOT}/fx5"
mk_fixtures "${FX5}"
mkdir -p "${FX5}/u1"
mk_inventory "${FX5}/inv.json" testorg '"u1"' \
    '[{"repo":"u1","collected":true,"prs":{"open_total":3,"dependabot_total":0,"other_open_total":3},"codeql":{"open_alerts":2,"status":"ok"},"rulesets":{"applied":true},"advisories":null}]'
cat >"${FX5}/u1/pr.json" <<'EOF'
[{"number":7,"author":{"login":"mona"}},{"number":8,"author":{"login":"hubot"}},{"number":9,"author":{"login":"pat"}}]
EOF
printf '[{"number":10},{"number":11}]\n' >"${FX5}/u1/cq.json"
printf '[{"id":1,"enforcement":"active"}]\n' >"${FX5}/u1/rs.json"
run_sweep 5 testorg '["u1"]' '[{"repo":"u1","pack":"go"}]' "${FX5}/inv.json" || { echo "BUG: S5 sweep failed" >&2; exit 99; }
check "S5 equal non-zero backlog classifies unchanged" jq -e '[.repos[] | select(.repo == "u1")] | (.[0].delta_class == "unchanged") and (.[0].deltas.prs_total == 0) and (.[0].deltas.prs_dependabot == 0) and (.[0].deltas.prs_other == 0) and (.[0].deltas.codeql_alerts == 0) and (.[0].deltas.rulesets_applied == 0)' "${FX5}/delta.json" >/dev/null
check "S5 applied rulesets stay applied after the delta" jq -e '[.repos[] | select(.repo == "u1")] | (.[0].after.rulesets_applied == true) and (.[0].before.rulesets_applied == true)' "${FX5}/delta.json" >/dev/null

# ═══════════════════════════════════════════════════════════════════════════
# S6 — protection-surface classification: applied 0 -> 1 is improved,
# 1 -> 0 is regressed (the raw signed deltas still keep their polarity)
# ═══════════════════════════════════════════════════════════════════════════
FX6="${WORK_ROOT}/fx6"
mk_fixtures "${FX6}"
for r in p1 p2; do mkdir -p "${FX6}/${r}"; done
mk_inventory "${FX6}/inv.json" testorg '"p1","p2"' '
[{"repo":"p1","collected":true,"prs":{"open_total":0,"dependabot_total":0,"other_open_total":0},"codeql":{"open_alerts":0,"status":"ok"},"rulesets":{"applied":false},"advisories":null},
 {"repo":"p2","collected":true,"prs":{"open_total":0,"dependabot_total":0,"other_open_total":0},"codeql":{"open_alerts":0,"status":"ok"},"rulesets":{"applied":true},"advisories":null}]'
printf '[{"id":1,"enforcement":"active"}]\n' >"${FX6}/p1/rs.json"          # 0 -> 1
printf '[{"id":1,"enforcement":"evaluate"}]\n' >"${FX6}/p2/rs.json"        # 1 -> 0
run_sweep 6 testorg '["p1","p2"]' '[{"repo":"p1","pack":"go"}]' "${FX6}/inv.json" || { echo "BUG: S6 sweep failed" >&2; exit 99; }
check "S6 protection newly applied is improved" jq -e '[.repos[] | select(.repo == "p1")] | first | (.delta_class == "improved") and (.deltas.rulesets_applied == 1) and (.after.rulesets_applied == true)' "${FX6}/delta.json" >/dev/null
check "S6 protection dropped is regressed" jq -e '[.repos[] | select(.repo == "p2")] | first | (.delta_class == "regressed") and (.deltas.rulesets_applied == -1) and (.after.rulesets_applied == false)' "${FX6}/delta.json" >/dev/null
check "S6 protection gained with every other surface flat is improved end-to-end" jq -e '[.repos[] | select(.repo == "p1")] | first | (.before.rulesets_applied == false) and (.after.rulesets_applied == true) and (.before.prs_total == 0) and (.after.prs_total == 0) and (.before.prs_dependabot == 0) and (.after.prs_dependabot == 0) and (.before.prs_other == 0) and (.after.prs_other == 0) and (.before.codeql_alerts == 0) and (.after.codeql_alerts == 0) and (.delta_class == "improved")' "${FX6}/delta.json" >/dev/null
check "S6 summary classifies both transitions and nets them" jq -e '(.summary.rulesets_applied_net == 0) and (.summary.repos_improved == 1) and (.summary.repos_regressed == 1) and (.summary.repos_clean == 0) and (.summary.repos_unchanged == 0) and (.summary.repos_partial_error == 0)' "${FX6}/delta.json" >/dev/null
check "S6 unplanned row stays report-only (pack null)" jq -e '[.repos[] | select(.repo == "p2")] | (.[0].pack == null) and (.[0].starter_language == null)' "${FX6}/delta.json" >/dev/null
check "S6 summary counts the planned rows only" jq -e '(.summary.repos_planned == 1) and (.sweep.repos_total == 2)' "${FX6}/delta.json" >/dev/null

# ═══════════════════════════════════════════════════════════════════════════
# S7 — partial_error precedence: failed surfaces keep the row honest even
# when the remaining arithmetic looks better
# ═══════════════════════════════════════════════════════════════════════════
FX7="${WORK_ROOT}/fx7"
mk_fixtures "${FX7}"
for r in e1 e2 e3; do mkdir -p "${FX7}/${r}"; done
mk_inventory "${FX7}/inv.json" testorg '"e1","e2","e3"' '
[{"repo":"e1","collected":true,"prs":{"open_total":0,"dependabot_total":0,"other_open_total":0},"codeql":{"open_alerts":2,"status":"ok"},"rulesets":{"applied":false},"advisories":null},
 {"repo":"e2","collected":true,"prs":{"open_total":5,"dependabot_total":2,"other_open_total":3},"codeql":{"open_alerts":0,"status":"ok"},"rulesets":{"applied":false},"advisories":null},
 {"repo":"e3","collected":true,"prs":{"open_total":2,"dependabot_total":1,"other_open_total":1},"codeql":{"open_alerts":0,"status":"ok"},"rulesets":{"applied":false},"advisories":null}]'
printf '2\n' >"${FX7}/e1/cq.rc"
printf '2\n' >"${FX7}/e2/pr.rc"
cat >"${FX7}/e3/pr.json" <<'EOF'
[{"number":1,"author":{"login":"dependabot[bot]"}},{"number":2,"author":{"login":"mona"}}]
EOF
run_sweep 7 testorg '["e1","e2","e3"]' '[{"repo":"e1","pack":"go"},{"repo":"e2","pack":"ts"},{"repo":"e3","pack":"py"}]' "${FX7}/inv.json" || { echo "BUG: S7 sweep failed" >&2; exit 99; }
ART7="${FX7}/delta.json"
check "S7 codeql transport failure is a typed bookkeeping row" jq -e '[.repos[] | select(.repo == "e1")] | (.[0].delta_class == "partial_error") and (.[0].errors[0].surface == "codeql") and (.[0].errors[0].detail | test("CodeQL alerts enumeration failed")) and (.[0].inventory_collected_before == true)' "${ART7}" >/dev/null
check "S7 pr transport failure is a typed bookkeeping row" jq -e '[.repos[] | select(.repo == "e2")] | (.[0].delta_class == "partial_error") and (.[0].errors[0].surface == "pull_requests") and (.[0].errors[0].detail | test("open-PR enumeration failed"))' "${ART7}" >/dev/null
check "S7 partial rows still carry their raw deltas" jq -e '[.repos[] | select(.repo == "e2")] | (.[0].deltas.prs_total == -5) and (.[0].after.prs_total == 0)' "${ART7}" >/dev/null
check "S7 surviving row completes without a transport error" jq -e '[.repos[] | select(.repo == "e3")] | first | (.delta_class == "unchanged") and ((.errors | length) == 0)' "${ART7}" >/dev/null
check "S7 summary separates partial from failed rows" jq -e '(.summary.repos_partial_error == 2) and (.summary.repos_failed_rows == 2) and (.summary.repos_unchanged == 1) and (.summary.repos_planned == 3) and (.summary.prs_dependabot_delta_total == -2) and (.summary.prs_other_delta_total == -3) and (.summary.codeql_alerts_delta_total == -2)' "${ART7}" >/dev/null

# ═══════════════════════════════════════════════════════════════════════════
# S8 — CodeQL disabled (HTTP 404): a zero-alert state, honestly zeroed, never
# an error row (the collect predicate on the 404 text)
# ═══════════════════════════════════════════════════════════════════════════
FX8="${WORK_ROOT}/fx8"
mk_fixtures "${FX8}"
mkdir -p "${FX8}/d1"
mk_inventory "${FX8}/inv.json" testorg '"d1"' \
    '[{"repo":"d1","collected":true,"prs":{"open_total":1,"dependabot_total":1,"other_open_total":0},"codeql":{"open_alerts":3,"status":"ok"},"rulesets":{"applied":false},"advisories":null}]'
cat >"${FX8}/d1/pr.json" <<'EOF'
[{"number":5,"author":{"login":"dependabot[bot]"}}]
EOF
printf '1\n' >"${FX8}/d1/cq.rc"
run_sweep 8 testorg '["d1"]' '[{"repo":"d1","pack":"go"}]' "${FX8}/inv.json" || { echo "BUG: S8 sweep failed" >&2; exit 99; }
check "S8 404 on the alerts API is a disabled state, not an error" jq -e '[.repos[] | select(.repo == "d1")] | first | (.after.codeql_status == "disabled") and (.after.codeql_alerts == 0) and ((.errors | length) == 0)' "${FX8}/delta.json" >/dev/null
check "S8 CodeQL state change is reported as a delta" jq -e '[.repos[] | select(.repo == "d1")] | (.[0].delta_class == "improved") and (.[0].deltas.codeql_alerts == -3)' "${FX8}/delta.json" >/dev/null

# ═══════════════════════════════════════════════════════════════════════════
# S9 — every repo failed its re-enumeration: typed failure, artifact kept
# ═══════════════════════════════════════════════════════════════════════════
FX9="${WORK_ROOT}/fx9"
mk_fixtures "${FX9}"
for r in f1 f2; do mkdir -p "${FX9}/${r}"; printf '2\n' >"${FX9}/${r}/pr.rc"; done
mk_inventory "${FX9}/inv.json" testorg '"f1","f2"' \
    '[{"repo":"f1","collected":true,"prs":{"open_total":1,"dependabot_total":1,"other_open_total":0},"codeql":{"open_alerts":0,"status":"ok"},"rulesets":{"applied":false},"advisories":null},
 {"repo":"f2","collected":true,"prs":{"open_total":0,"dependabot_total":0,"other_open_total":0},"codeql":{"open_alerts":0,"status":"ok"},"rulesets":{"applied":false},"advisories":null}]'
run_sweep 9 testorg '["f1","f2"]' '[{"repo":"f1","pack":"go"},{"repo":"f2","pack":"ts"}]' "${FX9}/inv.json" >/dev/null 2>&1 || true
rc9=0
SWD_GH_FIXTURES="${FX9}" SWD_GH_LOG="${FX9}/call.log" GH_RETRY_MAX_ATTEMPTS=1 \
    GH_RETRY_BACKOFF_SECONDS="0 0 0" GH_RETRY_SCRIPT_SLEEP_BUDGET=0 \
    GH_TOKEN='supersecret-token-canary' PATH="${BIN_STUB_GH}:${PATH}" \
    bash "${WORK_ROOT}/sweep9.sh" >/dev/null 2>"${WORK_ROOT}/sweep9.err" || rc9=$?
expect_count "S9 sweep exits rc 1 on a total re-enumeration failure" 1 "${rc9}"
check "S9 typed failure names the all-repos-failed gate" grep -qF 'delta_failed: every repo failed its delta re-enumeration' "${WORK_ROOT}/sweep9.err"
check "S9 failure line preserves the artifact path" grep -qF "${FX9}/delta.json" "${WORK_ROOT}/sweep9.err"
check "S9 failed sweep still keeps its evidence artifact" jq -e '(.schema == "devops_maint_v1.delta.v1") and (.summary.repos_failed_rows == 2) and (.summary.repos_planned == 2)' "${FX9}/delta.json" >/dev/null
check "S9 all-failed rows are partial_error" jq -e '[.repos[] | (.delta_class == "partial_error") and ((.errors | length) > 0)] | all' "${FX9}/delta.json" >/dev/null

# ═══════════════════════════════════════════════════════════════════════════
# S10 — repo missing from the before inventory: computed against zeroed
# surfaces and flagged before_inventory, collected-before false
# ═══════════════════════════════════════════════════════════════════════════
FX10="${WORK_ROOT}/fx10"
mk_fixtures "${FX10}"
for r in m1 p1; do mkdir -p "${FX10}/${r}"; done
mk_inventory "${FX10}/inv.json" testorg '"p1"' \
    '[{"repo":"p1","collected":true,"prs":{"open_total":0,"dependabot_total":0,"other_open_total":0},"codeql":{"open_alerts":0,"status":"ok"},"rulesets":{"applied":false},"advisories":null}]'
cat >"${FX10}/m1/pr.json" <<'EOF'
[{"number":4,"author":{"login":"mona"}}]
EOF
run_sweep 10 testorg '["m1","p1"]' '[{"repo":"p1","pack":"go"}]' "${FX10}/inv.json" || { echo "BUG: S10 sweep failed" >&2; exit 99; }
check "S10 missing before row is a typed bookkeeping row" jq -e '[.repos[] | select(.repo == "m1")] | (.[0].errors[0].surface == "before_inventory") and (.[0].errors[0].detail | test("not present in the before inventory")) and (.[0].inventory_collected_before == false)' "${FX10}/delta.json" >/dev/null
check "S10 missing-before row computes against zeroed surfaces" jq -e '[.repos[] | select(.repo == "m1")] | (.[0].before.prs_total == 0) and (.[0].after.prs_total == 1) and (.[0].deltas.prs_total == 1)' "${FX10}/delta.json" >/dev/null
check "S10 collected-before false always pairs with an error row" jq -e 'all(.repos[]; if .inventory_collected_before == false then ((.errors | length) > 0) else true end)' "${FX10}/delta.json" >/dev/null
check "S10 planned row keeps its pack" jq -e '[.repos[] | select(.repo == "p1")] | (.[0].pack == "go") and (.[0].inventory_collected_before == true)' "${FX10}/delta.json" >/dev/null

# ═══════════════════════════════════════════════════════════════════════════
# S11 — plan typing: starter packs carry starter_language; rows without a
# plan carry nulls
# ═══════════════════════════════════════════════════════════════════════════
FX11="${WORK_ROOT}/fx11"
mk_fixtures "${FX11}"
for r in s1 s2 u1; do mkdir -p "${FX11}/${r}"; done
mk_inventory "${FX11}/inv.json" testorg '"s1","s2","u1"' '
[{"repo":"s1","collected":true,"prs":{"open_total":0,"dependabot_total":0,"other_open_total":0},"codeql":{"open_alerts":0,"status":"ok"},"rulesets":{"applied":false},"advisories":null},
 {"repo":"s2","collected":true,"prs":{"open_total":0,"dependabot_total":0,"other_open_total":0},"codeql":{"open_alerts":0,"status":"ok"},"rulesets":{"applied":false},"advisories":null},
 {"repo":"u1","collected":true,"prs":{"open_total":0,"dependabot_total":0,"other_open_total":0},"codeql":{"open_alerts":0,"status":"ok"},"rulesets":{"applied":false},"advisories":null}]'
run_sweep 11 testorg '["s1","s2","u1"]' '[{"repo":"s1","pack":"starter","starter_language":"go"},{"repo":"s2","pack":"py"}]' "${FX11}/inv.json" || { echo "BUG: S11 sweep failed" >&2; exit 99; }
check "S11 starter pack rows carry their language" jq -e '[.repos[] | select(.repo == "s1")] | (.[0].pack == "starter") and (.[0].starter_language == "go")' "${FX11}/delta.json" >/dev/null
check "S11 non-starter pack rows carry null language" jq -e '[.repos[] | select(.repo == "s2")] | (.[0].pack == "py") and (.[0].starter_language == null)' "${FX11}/delta.json" >/dev/null
check "S11 summary counts both planned rows" jq -e '.summary.repos_planned == 2' "${FX11}/delta.json" >/dev/null

# ═══════════════════════════════════════════════════════════════════════════
# S12 — hygiene: scratch cleanup, stdout protocol, token never in evidence
# ═══════════════════════════════════════════════════════════════════════════
expect_count "S12 scratch lane cleaned up" 0 "$(find "${FX4}" -maxdepth 1 -name '.delta.raw.*' | wc -l)"
expect_count "S12 scratch lane cleaned up (partial-error run too)" 0 "$(find "${FX7}" -maxdepth 1 -name '.delta.raw.*' | wc -l)"
expect_count "S12 stdout line count is exactly one" 1 "$(wc -l <"${OUT4}")"
check "S12 token never lands in the artifact or stderr" bash -c "! grep -qF 'supersecret-token-canary' '${ART4}' '${WORK_ROOT}/sweep4.err' '${FX4}/call.log'" 2>/dev/null
check "S12 token never lands in the failed-run evidence" bash -c "! grep -qF 'supersecret-token-canary' '${FX9}/delta.json' '${WORK_ROOT}/sweep9.err' '${FX9}/call.log'" 2>/dev/null
check "S12 gh_retry banner stays on stderr, never stdout" bash -c "! grep -qF 'gh_retry:' '${OUT4}' 2>/dev/null" 2>/dev/null
check_not "S12 delta failures are prefixed delta_failed, stdout stays clean" grep -q 'delta_failed' "${OUT4}" 2>/dev/null

# ═══════════════════════════════════════════════════════════════════════════
# S13 — determinism (ADR-0015 D2): an identical sweep over the identical
# before inventory, re-run in the same run directory, produces a byte-identical
# delta artifact (no fields stripped: the artifact is keyed to its sweep —
# .sweep.before_inventory and .sweep.sweep_id are stable here by construction)
# ═══════════════════════════════════════════════════════════════════════════
FX13="${WORK_ROOT}/fx13"
cp -R "${FX4}" "${FX13}"
run_sweep 13 testorg '["g1","g2","g3"]' '[{"repo":"g1","pack":"go"},{"repo":"g2","pack":"ts"},{"repo":"g3","pack":"py"}]' "${FX13}/inv.json" || { echo "BUG: S13 sweep failed" >&2; exit 99; }
cp "${FX13}/delta.json" "${WORK_ROOT}/a.json"
run_sweep 13 testorg '["g1","g2","g3"]' '[{"repo":"g1","pack":"go"},{"repo":"g2","pack":"ts"},{"repo":"g3","pack":"py"}]' "${FX13}/inv.json" || { echo "BUG: S13 re-run failed" >&2; exit 99; }
cp "${FX13}/delta.json" "${WORK_ROOT}/b.json"
check "S13 identical sweeps produce byte-identical artifacts" cmp -s "${WORK_ROOT}/a.json" "${WORK_ROOT}/b.json"

# ═══════════════════════════════════════════════════════════════════════════
# S14 — tree validation, matching the other step suites
# ═══════════════════════════════════════════════════════════════════════════
set +e
(cd "${TREE_DIR}" && node scripts/interface_validate.mjs) >/dev/null 2>&1
NODE_OK=$?
(cd "${TREE_DIR}" && criteria validate devops_maint_v1) >"${WORK_ROOT}/validate.out" 2>&1
CRIT_OK=$?
set -e
expect_count "interface contract validates (all trees)" 0 "${NODE_OK}"
expect_count "criteria accepts the devops_maint_v1 tree" 0 "${CRIT_OK}"
check "criteria reports devops_maint_v1 ok" grep -q 'devops_maint_v1: ok' "${WORK_ROOT}/validate.out"

if [ "${FAILURES}" -gt 0 ]; then
    echo "FAILED: ${FAILURES} assertion(s)" >&2
    exit 1
fi
check "suite leaves no scratch leak beside its fixtures" test ! -e "${SCRIPT_DIR}/testorg"
check "suite leaves no scratch leak at the tree root" test ! -e "${ROOT_DIR}/../testorg"
echo "PASS: devops_maint_v1 delta-report-step regression suite verified"
