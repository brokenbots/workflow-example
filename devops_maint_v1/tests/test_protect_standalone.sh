#!/usr/bin/env bash
# devops_maint_v1 — Lane B protection-step regression suite (KB-409, ADR-0015 wave 4).
#
# Exercises the rendered protect_rulesets.sh END-TO-END through a stub gh
# (no network, no real GitHub):
#   - gh_retry shim is byte-identical to the canonical wrapper copy
#   - config gates fire BEFORE any network call (empty/dup/bad-pack/
#     out-of-scope plans -> typed failure, zero gh invocations)
#   - green signal -> ruleset CREATED exactly matching the pack context
#     contract (POST), verified by API read-back
#   - idempotent re-run: contract-matching ruleset -> NO write, verified
#   - NEVER protection-without-signal: red/missing contexts + no canonical
#     ruleset -> deferred row AND zero ruleset-mutating calls
#   - maintain-on-red: existing canonical ruleset verified under a red
#     signal with no write (maintenance is allowed, creation is not)
#   - drift heal on green -> PUT (not POST) with the canonical contract
#   - drift heal on red -> PUT + honest heal-on-red note
#   - apply failure -> per-repo error row, sweep-style bookkeeping, rc 0
#   - read-back mismatch -> typed step failure AFTER artifact preservation
#   - signal enumeration failure -> error row, no ruleset calls
#   - unplanned repos are report-only rows
#   - pagination flatten (rulesets past the first page are still honored)
#   - read-back transport retry (bounded) still verifies
#   - stdout protocol: the artifact path and nothing else; log() on stderr
#   - token hygiene: the GH_TOKEN canary never lands in artifacts or stderr
#   - pack context table cross-checked against ci_templates (KB-408)
#   - tree validation (node + criteria), matching the other step suites

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
TREE_DIR="$(cd "${ROOT_DIR}/.." && pwd)"
TEMPLATE="${ROOT_DIR}/scripts/protect_rulesets.sh.tftpl"
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
# suites: the gh_retry wrapper is content-substituted raw; shellquote
# markers render as single-quoted literals (test values must not contain
# single quotes); no {{ marker may survive the render.
render_protect() { # render_protect <org> <repos_json> <plan_json> <classification_file> <out>
    local org="$1" repos_json="$2" plan_json="$3" anchor="$4" out="$5"
    assert_no_single_quote "${org}"
    assert_no_single_quote "${repos_json}"
    assert_no_single_quote "${plan_json}"
    assert_no_single_quote "${anchor}"
    local wrap
    wrap="$(cat "${WRAPPER}")"
    local rendered
    rendered="$(cat "${TEMPLATE}")"
    rendered="${rendered//'{{ .gh_retry }}'/"${wrap}"}"
    rendered="${rendered//'{{ .org | shellquote }}'/"'${org}'"}"
    rendered="${rendered//'{{ .repos_json | shellquote }}'/"'${repos_json}'"}"
    rendered="${rendered//'{{ .plan_json | shellquote }}'/"'${plan_json}'"}"
    rendered="${rendered//'{{ .classification_file | shellquote }}'/"'${anchor}'"}"
    printf '%s' "${rendered}" >"${out}"
    if grep -q '{{' "${out}"; then
        echo "BUG: rendered protect template still contains template markers" >&2
        exit 99
    fi
    chmod +x "${out}"
}

# ── Stub gh ──────────────────────────────────────────────────────────────────
# Fixture-driven; every invocation appends "METHOD URL" (and, for mutating
# calls, a "BODY <json>" line) to PROTECT_GH_LOG. Mutations mark a flag file
# so read-back of a ruleset can change after a POST/PUT (API-faithful).
cat >"${BIN_STUB_GH}/gh" <<'EOF'
#!/usr/bin/env bash
set -u
fx="${PROTECT_GH_FIXTURES:?PROTECT_GH_FIXTURES not set}"
log="${PROTECT_GH_LOG:-/dev/null}"

url=""
method="GET"
input=""
args=("$@")
# gh api <url> ...: the subcommand token precedes the URL.
if [ "${args[0]:-}" = "api" ]; then
    args=("${args[@]:1}")
fi
i=0
while [ "$i" -lt "${#args[@]}" ]; do
    case "${args[$i]}" in
        --method) i=$((i + 1)); method="${args[$i]:-GET}" ;;
        --input)  i=$((i + 1)); input="${args[$i]:-}" ;;
        /*|"-"*) ;;
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
    "repos/${org}/${repo}/commits/"*/check-runs*)
        [ -d "$fxr" ] || { echo "stub: no fixture dir for ${repo}" >&2; exit 9; }
        erc="${fxr}/checks.rc"; [ -f "$erc" ] && { rc="$(cat "$erc")";
            [ -f "${fxr}/checks.err" ] && cat "${fxr}/checks.err" >&2; exit "$rc"; }
        if [ -f "${fxr}/checks.pages.json" ]; then cat "${fxr}/checks.pages.json"
        elif [ -f "${fxr}/checks.json" ]; then cat "${fxr}/checks.json"
        else echo "stub: missing fixture checks.json for ${repo}" >&2; exit 9; fi
        ;;
    "repos/${org}/${repo}/commits/"*)
        [ -f "${fxr}/head.json" ] || { echo "stub: missing fixture head.json for ${repo}" >&2; exit 9; }
        cat "${fxr}/head.json"
        ;;
    "repos/${org}/${repo}/rulesets/"*)
        # Mutating an existing ruleset (PUT) lands on the same URL shape as
        # reading it back; dispatch on method. A successful mutation marks a
        # flag so a later read-back can serve readback.after.json (if the
        # fixture provides one), API-faithfully.
        [ -d "$fxr" ] || { echo "stub: no fixture dir for ${repo}" >&2; exit 9; }
        if [ "$method" != "GET" ]; then
            erc="${fxr}/put.rc"
            [ -f "$erc" ] && { rc="$(cat "$erc")";
                [ -f "${fxr}/put.err" ] && cat "${fxr}/put.err" >&2; exit "$rc"; }
            touch "${fxr}/.mutated"
            [ -n "${input}" ] && cp "${input}" "${fxr}/lastbody.json"
            if [ -f "${fxr}/put.json" ]; then cat "${fxr}/put.json"
            else echo "stub: missing fixture put.json for ${repo}" >&2; exit 9; fi
            exit 0
        fi
        if [ -f "${fxr}/readback.tr" ]; then
            n="$(cat "${fxr}/readback.tr")"
        elif [ -f "${fxr}/readback.tr.count" ]; then
            n=0
        fi
        if [ -f "${fxr}/readback.tr.count" ]; then
            c="$(cat "${fxr}/readback.tr.count")"
            if [ "$c" -lt "$n" ]; then
                echo "$((c + 1))" >"${fxr}/readback.tr.count"
                echo 'gh: connection reset by peer' >&2
                exit 1
            fi
        fi
        if [ -f "${fxr}/.mutated" ] && [ -f "${fxr}/readback.after.json" ]; then
            cat "${fxr}/readback.after.json"
        elif [ -f "${fxr}/readback.json" ]; then
            cat "${fxr}/readback.json"
        else
            echo "stub: missing fixture readback.json for ${repo}" >&2
            exit 9
        fi
        ;;
    "repos/${org}/${repo}/rulesets" | "repos/${org}/${repo}/rulesets"?*)
        [ -d "$fxr" ] || { echo "stub: no fixture dir for ${repo}" >&2; exit 9; }
        if [ "$method" = "POST" ]; then
            erc="${fxr}/post.rc"
            [ -f "$erc" ] && { rc="$(cat "$erc")";
                [ -f "${fxr}/post.err" ] && cat "${fxr}/post.err" >&2; exit "$rc"; }
            touch "${fxr}/.mutated"
            [ -n "${input}" ] && cp "${input}" "${fxr}/lastbody.json"
            if [ -f "${fxr}/post.json" ]; then cat "${fxr}/post.json"
            else echo "stub: missing fixture post.json for ${repo}" >&2; exit 9; fi
        else
            erc="${fxr}/rulesets.rc"; [ -f "$erc" ] && { rc="$(cat "$erc")";
                [ -f "${fxr}/rulesets.err" ] && cat "${fxr}/rulesets.err" >&2; exit "$rc"; }
            if [ -f "${fxr}/rulesets.pages.json" ]; then cat "${fxr}/rulesets.pages.json"
            elif [ -f "${fxr}/rulesets.json" ]; then cat "${fxr}/rulesets.json"
            else echo "stub: missing fixture rulesets.json for ${repo}" >&2; exit 9; fi
        fi
        ;;
    "repos/${org}/${repo}")
        [ -d "$fxr" ] || { echo "stub: no fixture dir for ${repo}" >&2; exit 9; }
        if [ -f "${fxr}/repo.rc" ]; then
            rc="$(cat "${fxr}/repo.rc")"
            [ -f "${fxr}/repo.err" ] && cat "${fxr}/repo.err" >&2
            exit "$rc"
        fi
        [ -f "${fxr}/repo.json" ] || { echo "stub: missing fixture repo.json for ${repo}" >&2; exit 9; }
        cat "${fxr}/repo.json"
        ;;
    *)
        echo "stub: unknown gh api path: ${url}" >&2
        exit 9
        ;;
esac
EOF
chmod +x "${BIN_STUB_GH}/gh"

canon_ruleset() { # canon_ruleset <contexts-csv> [id] -> canonical read-back shape
    local ctx="$1" rid="${2:-901}"
    printf '{"id": %s, "name": "devops-maintenance-protection", "target": "branch", "enforcement": "active",
  "conditions": {"ref_name": {"include": ["~DEFAULT_BRANCH"], "exclude": []}}, "bypass": [],
  "rules": [
    {"type": "pull_request", "parameters": {
       "required_approving_review_count": 1, "dismiss_stale_reviews_on_push": false,
       "require_code_owner_review": false, "require_last_push_approval": false,
       "required_review_thread_resolution": false, "allowed_merge_methods": ["merge","squash","rebase"],
       "dismissal_restrictions": {"users": [], "teams": []}}},
    {"type": "required_status_checks", "parameters": {
       "strict_required_status_checks_policy": false,
       "required_status_checks": [%s]}}]}' "${rid}" "${ctx}"
}

make_fixtures() { # make_fixtures <dir>
    local fx="$1"
    mkdir -p "${fx}"
    : >"${fx}/call.log"
}

green_checks() { # green_checks <dir> <contexts-csv> -> all-success check-runs fixture
    local fx="$1" ctx="$2"
    local rows=""
    local c
    for c in $(printf '%s' "${ctx}" | tr ',' ' '); do
        rows="${rows}{\"name\": \"${c}\", \"conclusion\": \"success\"},"
    done
    printf '{"total_count": %s, "check_runs": [%s]}' \
        "$(printf '%s' "$ctx" | tr ',' '\n' | wc -l | tr -d ' ')" "${rows%,}" >"${fx}/checks.json"
}

red_checks() { # red_checks <dir> <contexts-csv> <missing-context> -> fixture with one failing/absent context
    local fx="$1" ctx="$2" bad="$3"
    local rows=""
    local c
    for c in $(printf '%s' "${ctx}" | tr ',' ' '); do
        case "$c" in
            "$bad") rows="${rows}{\"name\": \"${c}\", \"conclusion\": \"failure\"}," ;;
            *)      rows="${rows}{\"name\": \"${c}\", \"conclusion\": \"success\"}," ;;
        esac
    done
    printf '{"total_count": %s, "check_runs": [%s]}' \
        "$(printf '%s' "$ctx" | tr ',' '\n' | wc -l | tr -d ' ')" "${rows%,}" >"${fx}/checks.json"
}

# run_protect <script> <fixtures-dir> <out> <err>; echoes artifact stdout.
run_protect() {
    local script="$1" fixtures="$2" out="$3" err="$4"
    local rc=0
    set +e
    (
        export PATH="${BIN_STUB_GH}:${PATH}"
        export PROTECT_GH_FIXTURES="${fixtures}"
        export PROTECT_GH_LOG="${fixtures}/call.log"
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

# ═════════════════════════════════════════════════════════════════════════════
# 1. Vendored wrapper identity + structural wiring
# ═════════════════════════════════════════════════════════════════════════════
check "gh_retry vendored byte-identical to workstream_handler_v1" cmp -s "${WRAPPER}" "${CANONICAL_WRAPPER}"
check "main.chcl renders the protection gate through templatefile" grep -q 'templatefile("./scripts/protect_rulesets.sh.tftpl"' "${ROOT_DIR}/main.chcl"
check "main.chcl renders the triage loop through templatefile" grep -q 'templatefile("./scripts/codeql_triage.sh.tftpl"' "${ROOT_DIR}/main.chcl"
check "main.chcl passes the gh_retry shim to the protection gate" grep -Eq 'gh_retry +[[:space:]]*= *file\("./scripts/gh_retry.sh.tftpl"\)' "${ROOT_DIR}/main.chcl"
check "protect template splices the wrapper as raw content" test "$(grep -o '{{ *\.gh_retry }}' "${TEMPLATE}" | wc -l)" = "1"
check "protect template does not treat the wrapper as a path" test "$(grep -c '$_TPL_DIR' "${TEMPLATE}")" = "0"
check "protect template consumes the classification anchor" grep -q 'ANCHOR_PATH={{ .classification_file | shellquote }}' "${TEMPLATE}"
check "protect template consumes the protection plan" grep -q 'PLAN_JSON={{ .plan_json | shellquote }}' "${TEMPLATE}"

NODE_OK=1
set +e
(cd "${TREE_DIR}" && node scripts/interface_validate.mjs) >/dev/null 2>&1
NODE_OK=$?
CRIT_OK=1
(cd "${TREE_DIR}" && /usr/local/bin/criteria validate devops_maint_v1) >"${WORK_ROOT}/validate.out" 2>&1
CRIT_OK=$?
set -e
expect_count "interface contract validates (all trees)" 0 "${NODE_OK}"
expect_count "criteria accepts the devops_maint_v1 tree" 0 "${CRIT_OK}"
check "criteria reports devops_maint_v1 ok" grep -q 'devops_maint_v1: ok' "${WORK_ROOT}/validate.out"

GO_CTX="changes,lint,test,govulncheck,osv"

# ═════════════════════════════════════════════════════════════════════════════
# 2. Config gates: typed, fail-loud, BEFORE any network call
# ═════════════════════════════════════════════════════════════════════════════
FXC="${WORK_ROOT}/fxconfig"
make_fixtures "${FXC}"
mkdir -p "${FXC}/r1"
printf '{"default_branch": "main"}' >"${FXC}/r1/repo.json"
printf '{"sha": "abc123"}' >"${FXC}/r1/head.json"
green_checks "${FXC}/r1" "${GO_CTX}"
printf '[]' >"${FXC}/r1/rulesets.json"

mk_anchor() { # mk_anchor <name> -> <path>; creates an existing anchor file.
    # Each anchor lives in its own run dir so per-scenario artifacts (always
    # named protection.json / codeql_triage.json in dirname(anchor)) cannot
    # overwrite each other.
    local name="$1"
    mkdir -p "${WORK_ROOT}/runs/${name}"
    printf '{}' >"${WORK_ROOT}/runs/${name}/classification.json"
    printf '%s' "${WORK_ROOT}/runs/${name}/classification.json"
}

render_and_run_config() { # render_and_run_config <label> <plan_json> <out> <err>
    local rendered="${WORK_ROOT}/protect.${1}.sh"
    render_protect testorg '["r1"]' "$2" "$(mk_anchor "c-${1}.json")" "${rendered}"
    run_protect "${rendered}" "${FXC}" "$3" "$4" >/dev/null
}

FAIL_RC=0
render_and_run_config empty '[]' "${WORK_ROOT}/c-empty.out" "${WORK_ROOT}/c-empty.err" || FAIL_RC=$?
check_not "config: empty plan fails" test "${FAIL_RC}" -eq 0
check "config: empty plan -> typed failure" grep -q '^protect_failed:' "${WORK_ROOT}/c-empty.err"
FAIL_RC=0
render_and_run_config dup '[{"repo":"r1","pack":"go"},{"repo":"r1","pack":"go"}]' "${WORK_ROOT}/c-dup.out" "${WORK_ROOT}/c-dup.err" || FAIL_RC=$?
check_not "config: duplicate repo rows fail" test "${FAIL_RC}" -eq 0
FAIL_RC=0
render_and_run_config badpack '[{"repo":"r1","pack":"ruby"}]' "${WORK_ROOT}/c-bad.out" "${WORK_ROOT}/c-bad.err" || FAIL_RC=$?
check_not "config: pack outside the language set fails" test "${FAIL_RC}" -eq 0
FAIL_RC=0
render_and_run_config oos '[{"repo":"elsewhere","pack":"go"}]' "${WORK_ROOT}/c-oos.out" "${WORK_ROOT}/c-oos.err" || FAIL_RC=$?
check_not "config: plan repo outside the sweep scope fails" test "${FAIL_RC}" -eq 0
check "config: all four gates fired before ANY gh call" test ! -s "${FXC}/call.log"

# ═════════════════════════════════════════════════════════════════════════════
# 3. Happy path: green signal -> ruleset created + verified by read-back
# ═════════════════════════════════════════════════════════════════════════════
FX1="${WORK_ROOT}/fx1"
make_fixtures "${FX1}"
mkdir -p "${FX1}/go1"
printf '{"default_branch": "main"}' >"${FX1}/go1/repo.json"
printf '{"sha": "abc123"}' >"${FX1}/go1/head.json"
green_checks "${FX1}/go1" "${GO_CTX}"
printf '[]' >"${FX1}/go1/rulesets.json"
canon_ruleset '{"context": "changes", "integration_id": null}, {"context": "lint", "integration_id": null}, {"context": "test", "integration_id": null}, {"context": "govulncheck", "integration_id": null}, {"context": "osv", "integration_id": null}' >"${FX1}/go1/post.json"
canon_ruleset '{"context": "changes", "integration_id": null}, {"context": "lint", "integration_id": null}, {"context": "test", "integration_id": null}, {"context": "govulncheck", "integration_id": null}, {"context": "osv", "integration_id": null}' >"${FX1}/go1/readback.json"

P1_RENDER="${WORK_ROOT}/protect1.sh"
render_protect testorg '["go1"]' '[{"repo":"go1","pack":"go"}]' "$(mk_anchor "p-main.json")" "${P1_RENDER}"
P1_RC=0
run_protect "${P1_RENDER}" "${FX1}" "${WORK_ROOT}/p1.out" "${WORK_ROOT}/p1.err" || P1_RC=$?
expect_count "S3 protect exits 0 on a green signal" 0 "${P1_RC}"
ART1="$(tr -d '[:space:]' <"${WORK_ROOT}/p1.out")"
check "S3 stdout is exactly the artifact path" test -f "${ART1}"
check "S3 stdout has no second line" test "$(wc -l <"${WORK_ROOT}/p1.out")" -eq 1
check "S3 artifact schema" jq -e '.schema == "devops_maint_v1.protection.v1"' "${ART1}" >/dev/null
check "S3 row is verified" jq -e '.repos[0].status == "verified"' "${ART1}" >/dev/null
check "S3 row records the created action" jq -e '.repos[0].protection.action == "created" and .repos[0].protection.readback_verified == true' "${ART1}" >/dev/null
check "S3 row carries the pack contexts" jq -e '.repos[0].protection.required_contexts | sort == ["changes","govulncheck","lint","osv","test"]' "${ART1}" >/dev/null
check "S3 signal observed green for the default branch" jq -e '.repos[0].signal.green == true and .repos[0].signal.default_branch == "main"' "${ART1}" >/dev/null
check "S3 summary counts" jq -e '.summary.repos_verified_total == 1 and .summary.rulesets_created_total == 1 and .summary.verify_mismatch_total == 0' "${ART1}" >/dev/null
check "S3 log: repo metadata enumerated" grep -q "GET repos/testorg/go1$" "${FX1}/call.log"
check "S3 log: check-runs enumerated" grep -q "GET repos/testorg/go1/commits/abc123/check-runs" "${FX1}/call.log"
check "S3 log: ruleset created with POST exactly once" test "$(grep -c "^POST repos/testorg/go1/rulesets$" "${FX1}/call.log")" -eq 1
check "S3 log: no PUT call (nothing to update)" test "$(grep -c "^PUT " "${FX1}/call.log")" -eq 0
check "S3 create body carries the exact context contract" test "$(jq -r '.rules[] | select(.type == "required_status_checks") | .parameters.required_status_checks[].context' "${FX1}/go1/lastbody.json" | sort | tr '\n' ',' | sed 's/,$//')" = "changes,govulncheck,lint,osv,test"
check "S3 create body enforces to admins (no bypass)" test "$(jq -r '.bypass | length' "${FX1}/go1/lastbody.json")" -eq "0"
check "S3 create body requires PR approval" test "$(jq -r '.rules[] | select(.type == "pull_request") | .parameters.required_approving_review_count' "${FX1}/go1/lastbody.json")" -eq "1"
check "S3 log: read-back GET on the created ruleset" grep -q "GET repos/testorg/go1/rulesets/901$" "${FX1}/call.log"

CTX_CONTRACT='{"context": "changes", "integration_id": null}, {"context": "lint", "integration_id": null}, {"context": "test", "integration_id": null}, {"context": "govulncheck", "integration_id": null}, {"context": "osv", "integration_id": null}'

# ═════════════════════════════════════════════════════════════════════════════
# 4. Idempotency: contract-matching ruleset + green -> verify-only, no write
# ═════════════════════════════════════════════════════════════════════════════
FX2="${WORK_ROOT}/fx2"
make_fixtures "${FX2}"
mkdir -p "${FX2}/go2"
printf '{"default_branch": "main"}' >"${FX2}/go2/repo.json"
printf '{"sha": "abc123"}' >"${FX2}/go2/head.json"
green_checks "${FX2}/go2" "${GO_CTX}"
printf '[{"id": 901, "name": "devops-maintenance-protection", "target": "branch", "enforcement": "active"}]' >"${FX2}/go2/rulesets.json"
canon_ruleset "${CTX_CONTRACT}" >"${FX2}/go2/readback.json"

P2="${WORK_ROOT}/protect2.sh"
render_protect testorg '["go2"]' '[{"repo":"go2","pack":"go"}]' "$(mk_anchor "p2.json")" "${P2}"
P2_RC=0
run_protect "${P2}" "${FX2}" "${WORK_ROOT}/p2.out" "${WORK_ROOT}/p2.err" || P2_RC=$?
expect_count "S4 idempotent run exits 0" 0 "${P2_RC}"
ART2="$(tr -d '[:space:]' <"${WORK_ROOT}/p2.out")"
check "S4 verified with no write" jq -e '.repos[0].status == "verified" and .repos[0].protection.action == "none" and .repos[0].protection.ruleset_id == 901' "${ART2}" >/dev/null
check "S4 summary records an unchanged ruleset" jq -e '.summary.rulesets_unchanged_total == 1 and .summary.rulesets_created_total == 0 and .summary.rulesets_updated_total == 0' "${ART2}" >/dev/null
check_not "S4 no POST issued" grep -q "^POST " "${FX2}/call.log"
check_not "S4 no PUT issued" grep -q "^PUT " "${FX2}/call.log"

# ═════════════════════════════════════════════════════════════════════════════
# 5. NEVER protection-without-signal: red + no canonical ruleset -> deferred,
#    zero mutating calls
# ═════════════════════════════════════════════════════════════════════════════
FX3="${WORK_ROOT}/fx3"
make_fixtures "${FX3}"
mkdir -p "${FX3}/go3"
printf '{"default_branch": "main"}' >"${FX3}/go3/repo.json"
printf '{"sha": "abc123"}' >"${FX3}/go3/head.json"
red_checks "${FX3}/go3" "${GO_CTX}" "test"
printf '[]' >"${FX3}/go3/rulesets.json"

P3="${WORK_ROOT}/protect3.sh"
render_protect testorg '["go3"]' '[{"repo":"go3","pack":"go"}]' "$(mk_anchor "p3.json")" "${P3}"
P3_RC=0
run_protect "${P3}" "${FX3}" "${WORK_ROOT}/p3.out" "${WORK_ROOT}/p3.err" || P3_RC=$?
expect_count "S5 defer (never protect without signal) exits 0" 0 "${P3_RC}"
ART3="$(tr -d '[:space:]' <"${WORK_ROOT}/p3.out")"
check "S5 row is deferred, protection null" jq -e '.repos[0].status == "deferred" and .repos[0].protection == null' "${ART3}" >/dev/null
check "S5 missing context is named" jq -e '.repos[0].missing_contexts == ["test"] and .repos[0].signal.green == false' "${ART3}" >/dev/null
check "S5 summary deferral count" jq -e '.summary.repos_deferred_total == 1 and .summary.rulesets_created_total == 0' "${ART3}" >/dev/null
check_not "S5 no ruleset creation on a red signal" grep -q "^POST " "${FX3}/call.log"
check_not "S5 no ruleset mutation on a red signal" grep -q "^PUT " "${FX3}/call.log"

# ═════════════════════════════════════════════════════════════════════════════
# 6. Maintain-on-red: existing canonical ruleset + red signal -> verify-only
# ═════════════════════════════════════════════════════════════════════════════
FX4="${WORK_ROOT}/fx4"
make_fixtures "${FX4}"
mkdir -p "${FX4}/go4"
printf '{"default_branch": "main"}' >"${FX4}/go4/repo.json"
printf '{"sha": "abc123"}' >"${FX4}/go4/head.json"
red_checks "${FX4}/go4" "${GO_CTX}" "govulncheck"
printf '[{"id": 901, "name": "devops-maintenance-protection", "target": "branch", "enforcement": "active"}]' >"${FX4}/go4/rulesets.json"
canon_ruleset "${CTX_CONTRACT}" >"${FX4}/go4/readback.json"

P4="${WORK_ROOT}/protect4.sh"
render_protect testorg '["go4"]' '[{"repo":"go4","pack":"go"}]' "$(mk_anchor "p4.json")" "${P4}"
P4_RC=0
run_protect "${P4}" "${FX4}" "${WORK_ROOT}/p4.out" "${WORK_ROOT}/p4.err" || P4_RC=$?
expect_count "S6 maintain-on-red exits 0" 0 "${P4_RC}"
ART4="$(tr -d '[:space:]' <"${WORK_ROOT}/p4.out")"
check "S6 existing ruleset stays verified (maintained)" jq -e '.repos[0].status == "verified" and .repos[0].protection.action == "none"' "${ART4}" >/dev/null
check "S6 existing protection is NOT torn down" jq -e '.repos[0].missing_contexts == ["govulncheck"]' "${ART4}" >/dev/null
check_not "S6 no write while verifying (maintain-only)" grep -q "^POST \|^PUT " "${FX4}/call.log"

# ═════════════════════════════════════════════════════════════════════════════
# 7. Drift heal on green: drifted canonical ruleset -> PUT with the contract
# ═════════════════════════════════════════════════════════════════════════════
FX5="${WORK_ROOT}/fx5"
make_fixtures "${FX5}"
mkdir -p "${FX5}/go5"
printf '{"default_branch": "main"}' >"${FX5}/go5/repo.json"
printf '{"sha": "abc123"}' >"${FX5}/go5/head.json"
green_checks "${FX5}/go5" "${GO_CTX}"
printf '[{"id": 880, "name": "devops-maintenance-protection", "target": "branch", "enforcement": "active"}]' >"${FX5}/go5/rulesets.json"
canon_ruleset '{"context": "changes", "integration_id": null}, {"context": "lint", "integration_id": null}' 880 >"${FX5}/go5/readback.json"
canon_ruleset "${CTX_CONTRACT}" 880 >"${FX5}/go5/readback.after.json"
canon_ruleset "${CTX_CONTRACT}" 880 >"${FX5}/go5/put.json"

P5="${WORK_ROOT}/protect5.sh"
render_protect testorg '["go5"]' '[{"repo":"go5","pack":"go"}]' "$(mk_anchor "p5.json")" "${P5}"
P5_RC=0
run_protect "${P5}" "${FX5}" "${WORK_ROOT}/p5.out" "${WORK_ROOT}/p5.err" || P5_RC=$?
expect_count "S7 drift heal exits 0" 0 "${P5_RC}"
ART5="$(tr -d '[:space:]' <"${WORK_ROOT}/p5.out")"
check "S7 drifted ruleset healed via PUT" jq -e '.repos[0].status == "verified" and .repos[0].protection.action == "updated" and .repos[0].protection.ruleset_id == 880' "${ART5}" >/dev/null
check "S7 log: exactly one PUT on the existing ruleset id" test "$(grep -c "^PUT repos/testorg/go5/rulesets/880$" "${FX5}/call.log")" -eq 1
check_not "S7 heal used PUT, never POST" grep -q "^POST " "${FX5}/call.log"
check "S7 healed body carries the full contract" test "$(jq -r '.rules[] | select(.type == "required_status_checks") | .parameters.required_status_checks[].context' "${FX5}/go5/lastbody.json" | sort | tr '\n' ',' | sed 's/,$//')" = "changes,govulncheck,lint,osv,test"

# ═════════════════════════════════════════════════════════════════════════════
# 8. Drift heal on red: mutation happens, with the honest heal-on-red note
# ═════════════════════════════════════════════════════════════════════════════
FX6="${WORK_ROOT}/fx6"
make_fixtures "${FX6}"
mkdir -p "${FX6}/go6"
printf '{"default_branch": "main"}' >"${FX6}/go6/repo.json"
printf '{"sha": "abc123"}' >"${FX6}/go6/head.json"
red_checks "${FX6}/go6" "${GO_CTX}" "lint"
printf '[{"id": 880, "name": "devops-maintenance-protection", "target": "branch", "enforcement": "active"}]' >"${FX6}/go6/rulesets.json"
canon_ruleset '{"context": "changes", "integration_id": null}, {"context": "lint", "integration_id": null}' 880 >"${FX6}/go6/readback.json"
canon_ruleset "${CTX_CONTRACT}" 880 >"${FX6}/go6/readback.after.json"
canon_ruleset "${CTX_CONTRACT}" 880 >"${FX6}/go6/put.json"

P6="${WORK_ROOT}/protect6.sh"
render_protect testorg '["go6"]' '[{"repo":"go6","pack":"go"}]' "$(mk_anchor "p6.json")" "${P6}"
P6_RC=0
run_protect "${P6}" "${FX6}" "${WORK_ROOT}/p6.out" "${WORK_ROOT}/p6.err" || P6_RC=$?
expect_count "S8 heal-on-red exits 0" 0 "${P6_RC}"
ART6="$(tr -d '[:space:]' <"${WORK_ROOT}/p6.out")"
check "S8 heal-on-red verified with updated action" jq -e '.repos[0].status == "verified" and .repos[0].protection.action == "updated"' "${ART6}" >/dev/null
check "S8 honest heal-on-red note is recorded" jq -r '.repos[0].note' "${ART6}" | grep -q "drift healed on an existing ruleset while signal red"
check_not "S8 heal-on-red never CREATES protection" grep -q "^POST " "${FX6}/call.log"

# ═════════════════════════════════════════════════════════════════════════════
# 9. Apply failure: per-repo error row, bookkeeping, run still succeeds
# ═════════════════════════════════════════════════════════════════════════════
FX7="${WORK_ROOT}/fx7"
make_fixtures "${FX7}"
mkdir -p "${FX7}/go7"
printf '{"default_branch": "main"}' >"${FX7}/go7/repo.json"
printf '{"sha": "abc123"}' >"${FX7}/go7/head.json"
green_checks "${FX7}/go7" "${GO_CTX}"
printf '[]' >"${FX7}/go7/rulesets.json"
printf '1' >"${FX7}/go7/post.rc"
printf 'gh: HTTP 500: Internal Server Error' >"${FX7}/go7/post.err"

P7="${WORK_ROOT}/protect7.sh"
render_protect testorg '["go7"]' '[{"repo":"go7","pack":"go"}]' "$(mk_anchor "p7.json")" "${P7}"
P7_RC=0
run_protect "${P7}" "${FX7}" "${WORK_ROOT}/p7.out" "${WORK_ROOT}/p7.err" || P7_RC=$?
expect_count "S9 apply failure keeps rc 0 (bookkeeping)" 0 "${P7_RC}"
ART7="$(tr -d '[:space:]' <"${WORK_ROOT}/p7.out")"
check "S7 row is error with the typed code" jq -e '.repos[0].status == "error" and .repos[0].errors[0].surface == "ruleset_apply"' "${ART7}" >/dev/null
check "S9 error row carries the gh stderr" jq -e '.repos[0].errors[0].detail | test("HTTP 500")' "${ART7}" >/dev/null
check "S9 summary error count" jq -e '.summary.repos_error_total == 1 and .summary.rulesets_created_total == 0' "${ART7}" >/dev/null

# ═════════════════════════════════════════════════════════════════════════════
# 10. Read-back mismatch: typed step failure, artifact preserved
# ═════════════════════════════════════════════════════════════════════════════
FX8="${WORK_ROOT}/fx8"
make_fixtures "${FX8}"
mkdir -p "${FX8}/go8"
printf '{"default_branch": "main"}' >"${FX8}/go8/repo.json"
printf '{"sha": "abc123"}' >"${FX8}/go8/head.json"
green_checks "${FX8}/go8" "${GO_CTX}"
printf '[]' >"${FX8}/go8/rulesets.json"
canon_ruleset '{"context": "changes", "integration_id": null}, {"context": "lint", "integration_id": null}, {"context": "test", "integration_id": null}, {"context": "govulncheck", "integration_id": null}' >"${FX8}/go8/post.json"
canon_ruleset '{"context": "changes", "integration_id": null}, {"context": "lint", "integration_id": null}, {"context": "test", "integration_id": null}, {"context": "govulncheck", "integration_id": null}' >"${FX8}/go8/readback.json"

mkdir -p "${WORK_ROOT}/runs8"
printf '{}' >"${WORK_ROOT}/runs8/p8.json"
P8="${WORK_ROOT}/protect8.sh"
render_protect testorg '["go8"]' '[{"repo":"go8","pack":"go"}]' "${WORK_ROOT}/runs8/p8.json" "${P8}"
P8_RC=0
run_protect "${P8}" "${FX8}" "${WORK_ROOT}/p8.out" "${WORK_ROOT}/p8.err" || P8_RC=$?
expect_count "S10 read-back mismatch exits NON-ZERO" 1 "${P8_RC}"
check "S10 typed failure names the gate" grep -qi 'protect_failed' "${WORK_ROOT}/p8.err"
check "S10 typed failure names the artifact path" grep -qF "${WORK_ROOT}/runs8/protection.json" "${WORK_ROOT}/p8.err"
check "S10 artifact was still written and preserved" test -f "${WORK_ROOT}/runs8/protection.json"
check "S10 artifact records the mismatch" jq -e '.summary.verify_mismatch_total == 1 and .repos[0].status != "verified"' "${WORK_ROOT}/runs8/protection.json" >/dev/null

# ═════════════════════════════════════════════════════════════════════════════
# 11. Signal enumeration failure -> error row, zero ruleset calls
# ═════════════════════════════════════════════════════════════════════════════
FX9="${WORK_ROOT}/fx9"
make_fixtures "${FX9}"
mkdir -p "${FX9}/go9"
printf '1' >"${FX9}/go9/repo.rc"
printf 'gh: HTTP 404: Not Found' >"${FX9}/go9/repo.err"
printf '[]' >"${FX9}/go9/rulesets.json"

P9="${WORK_ROOT}/protect9.sh"
render_protect testorg '["go9"]' '[{"repo":"go9","pack":"go"}]' "$(mk_anchor "p9.json")" "${P9}"
P9_RC=0
run_protect "${P9}" "${FX9}" "${WORK_ROOT}/p9.out" "${WORK_ROOT}/p9.err" || P9_RC=$?
expect_count "S11 enumeration failure keeps rc 0 (bookkeeping)" 0 "${P9_RC}"
ART9="$(tr -d '[:space:]' <"${WORK_ROOT}/p9.out")"
check "S11 error row typed as signal_enumerate" jq -e '.repos[0].status == "error" and .repos[0].errors[0].surface == "signal" and .repos[0].signal == null' "${ART9}" >/dev/null
check_not "S11 no ruleset calls without signal" grep -qE "^POST |^PUT |^GET repos/testorg/go9/(rulesets|901)" "${FX9}/call.log"

# ═════════════════════════════════════════════════════════════════════════════
# 12. Unplanned repos are report-only rows
# ═════════════════════════════════════════════════════════════════════════════
FX10="${WORK_ROOT}/fx10"
make_fixtures "${FX10}"
mkdir -p "${FX10}/go10" "${FX10}/extra"
printf '{"default_branch": "main"}' >"${FX10}/go10/repo.json"
printf '{"sha": "abc123"}' >"${FX10}/go10/head.json"
green_checks "${FX10}/go10" "${GO_CTX}"
printf '[]' >"${FX10}/go10/rulesets.json"
canon_ruleset "${CTX_CONTRACT}" >"${FX10}/go10/post.json"
canon_ruleset "${CTX_CONTRACT}" >"${FX10}/go10/readback.json"

P10="${WORK_ROOT}/protect10.sh"
render_protect testorg '["go10","extra"]' '[{"repo":"go10","pack":"go"}]' "$(mk_anchor "p10.json")" "${P10}"
P10_RC=0
run_protect "${P10}" "${FX10}" "${WORK_ROOT}/p10.out" "${WORK_ROOT}/p10.err" || P10_RC=$?
expect_count "S12 unplanned-repo run exits 0" 0 "${P10_RC}"
ART10="$(tr -d '[:space:]' <"${WORK_ROOT}/p10.out")"
check "S12 unplanned row is report-only" jq -e '.repos[1].status == "unplanned" and .repos[1].protection == null and .repos[1].signal == null' "${ART10}" >/dev/null
check "S12 summary counts the unplanned row" jq -e '.summary.repos_unplanned_total == 1 and .summary.repos_total == 2' "${ART10}" >/dev/null
check_not "S12 no enumeration for unplanned repos" grep -q "repos/testorg/extra" "${FX10}/call.log"
check "S12 planned repo still applied" jq -e '.repos[0].status == "verified" and .repos[0].protection.action == "created"' "${ART10}" >/dev/null

# ═════════════════════════════════════════════════════════════════════════════
# 13. Pagination: canonical ruleset on page 2 is still honored (verify-only)
# ═════════════════════════════════════════════════════════════════════════════
FX11="${WORK_ROOT}/fx11"
make_fixtures "${FX11}"
mkdir -p "${FX11}/go11"
printf '{"default_branch": "main"}' >"${FX11}/go11/repo.json"
printf '{"sha": "abc123"}' >"${FX11}/go11/head.json"
green_checks "${FX11}/go11" "${GO_CTX}"
cat >"${FX11}/go11/rulesets.pages.json" <<'EOF'
[{"id": 700, "name": "unrelated-ruleset", "target": "branch", "enforcement": "active"}]
[{"id": 901, "name": "devops-maintenance-protection", "target": "branch", "enforcement": "active"}]
EOF
canon_ruleset "${CTX_CONTRACT}" >"${FX11}/go11/readback.json"

P11="${WORK_ROOT}/protect11.sh"
render_protect testorg '["go11"]' '[{"repo":"go11","pack":"go"}]' "$(mk_anchor "p11.json")" "${P11}"
P11_RC=0
run_protect "${P11}" "${FX11}" "${WORK_ROOT}/p11.out" "${WORK_ROOT}/p11.err" || P11_RC=$?
expect_count "S13 paginated ruleset run exits 0" 0 "${P11_RC}"
ART11="$(tr -d '[:space:]' <"${WORK_ROOT}/p11.out")"
check "S13 page-2 canonical ruleset honored, no duplicate write" jq -e '.repos[0].status == "verified" and .repos[0].protection.action == "none" and .repos[0].protection.ruleset_id == 901' "${ART11}" >/dev/null
check_not "S13 no second ruleset was created" grep -q "^POST " "${FX11}/call.log"

# ═════════════════════════════════════════════════════════════════════════════
# 14. Read-back transport race: bounded retry still verifies
# ═════════════════════════════════════════════════════════════════════════════
FX12="${WORK_ROOT}/fx12"
make_fixtures "${FX12}"
mkdir -p "${FX12}/go12"
printf '{"default_branch": "main"}' >"${FX12}/go12/repo.json"
printf '{"sha": "abc123"}' >"${FX12}/go12/head.json"
green_checks "${FX12}/go12" "${GO_CTX}"
printf '[]' >"${FX12}/go12/rulesets.json"
canon_ruleset "${CTX_CONTRACT}" 901 >"${FX12}/go12/post.json"
canon_ruleset "${CTX_CONTRACT}" 901 >"${FX12}/go12/readback.json"
printf '2' >"${FX12}/go12/readback.tr"
printf '0' >"${FX12}/go12/readback.tr.count"

P12="${WORK_ROOT}/protect12.sh"
P12_RC=0
render_protect testorg '["go12"]' '[{"repo":"go12","pack":"go"}]' "$(mk_anchor "p12.json")" "${P12}"
READBACK_SLEEP=0 run_protect "${P12}" "${FX12}" "${WORK_ROOT}/p12.out" "${WORK_ROOT}/p12.err" || P12_RC=$?
expect_count "S14 transport-race retry run exits 0" 0 "${P12_RC}"
ART12="$(tr -d '[:space:]' <"${WORK_ROOT}/p12.out")"
check "S14 verified after transient read-back failures" jq -e '.repos[0].status == "verified" and .repos[0].protection.action == "created"' "${ART12}" >/dev/null
check "S14 both transient failures hit the stub" test "$(grep -c "^GET repos/testorg/go12/rulesets/901$" "${FX12}/call.log")" -eq 3

# ═════════════════════════════════════════════════════════════════════════════
# 15. Token hygiene: the canary never lands in the artifact or stderr
# ═════════════════════════════════════════════════════════════════════════════
check_not "S15 token canary absent from the S3 artifact" grep -q "supersecret-token-canary" "${ART1}"
check_not "S15 token canary absent from S3 stderr" grep -q "supersecret-token-canary" "${WORK_ROOT}/p1.err"
check_not "S15 token canary absent from the S5 artifact" grep -q "supersecret-token-canary" "${ART3}"

# ═════════════════════════════════════════════════════════════════════════════
# 16. Pack context contract stays in lockstep with ci_templates (KB-408:
#     "the job name values are the KB-409 protection contract")
# ═════════════════════════════════════════════════════════════════════════════
top_jobs() { # <file> -> top-level job keys in declaration order
    awk '/^jobs:/ { f=1; next }
         f && /^[^[:space:]#]/ { f=0 }
         f && /^  [A-Za-z][A-Za-z0-9_-]*:/ {
             s = $0; sub(/^  /, "", s)
             print substr(s, 1, index(s, ":") - 1)
         }' "$1"
}
pack_contexts_tpl() { # <pack> -> sorted contexts from PACK_CONTEXTS in the template
    grep '^PACK_CONTEXTS=' "${TEMPLATE}" | sed 's/^PACK_CONTEXTS=//' | tr -d "'" \
        | jq -r --arg p "$1" '.[$p][]' | sort | tr '\n' ',' | sed 's/,$//'
}
require_equal() {
    if [ "$2" = "$3" ]; then
        echo "PASS: $1" >&2
    else
        echo "FAIL: $1 (want [$2], got [$3])" >&2
        FAILURES=$((FAILURES + 1))
    fi
}
for pack in go ts py starter; do
    tpl="${ROOT_DIR}/ci_templates/github-ci-${pack}.yml"
    jobs_yml="$(top_jobs "${tpl}" | sort | tr '\n' ',' | sed 's/,$//')"
    contexts="$(pack_contexts_tpl "${pack}")"
    require_equal "S16 ${pack}: PACK_CONTEXTS matches the template job set" "${contexts}" "${jobs_yml}"
done
check "S16 all four pack templates exist" test -f "${ROOT_DIR}/ci_templates/github-ci-go.yml" -a -f "${ROOT_DIR}/ci_templates/github-ci-ts.yml" -a -f "${ROOT_DIR}/ci_templates/github-ci-py.yml" -a -f "${ROOT_DIR}/ci_templates/github-ci-starter.yml"
check "S16 README records KB-409 as the required-check authority" grep -q "KB-409" "${ROOT_DIR}/ci_templates/README.md"

if [ "${FAILURES}" -gt 0 ]; then
    echo "FAILED: ${FAILURES} assertion(s)" >&2
    exit 1
fi
check "suite leaves no scratch leak beside its fixtures" test ! -e "${SCRIPT_DIR}/testorg"
check "suite leaves no scratch leak at the tree root" test ! -e "${ROOT_DIR}/../testorg"
echo "PASS: devops_maint_v1 protection-step regression suite verified"
