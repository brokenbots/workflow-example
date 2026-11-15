#!/usr/bin/env bash
# devops_maint_v1 — Lane B compliance-application-step regression suite
# (KB-410 guided sweep, ADR-0015 D6).
#
# Exercises the rendered apply_ci_pack.sh END-TO-END through a stub gh
# (no network, no real GitHub):
#   - gh_retry shim is byte-identical to the canonical wrapper copy
#   - config gates fire BEFORE any network call (missing anchor/empty/dup/
#     bad-pack/out-of-scope plans + bad starter_language + undecodable
#     template payload -> typed failure, zero gh invocations)
#   - absent files -> CREATED via PUT (no sha in the body), verified by
#     byte-exact read-back
#   - idempotent re-run: template-matching bytes -> NO write, verified
#   - drifted files -> UPDATED via PUT (body carries the fetched sha)
#   - per-pack template routing: go/ts/py self-describing; starter pack
#     routes its CI to the starter template and its dependabot file to the
#     STARTER_LANGUAGE's language template (checked against the real
#     ci_templates bytes, never test doubles)
#   - PUT body contract: exact commit message, exact branch, single-line
#     base64 content that decodes byte-identically to the template file
#   - read-back mismatch -> typed step failure AFTER artifact preservation,
#     with the mismatched file zeroed and the artifact path named in the line
#   - PUT failure -> per-file error row, sweep-style bookkeeping, rc 0
#   - repo-metadata failure -> error row with zero contents calls
#   - existing-file probe failure (non-404) -> error row, no write, rc 0
#   - unplanned repos are report-only rows (zero gh calls against them)
#   - stdout protocol: the artifact path and nothing else; log() on stderr
#   - token hygiene: the GH_TOKEN canary never lands in artifacts or stderr
#   - structural self-check recomputed independently from the artifact rows
#   - tree validation (node + criteria), matching the other step suites

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
TREE_DIR="$(cd "${ROOT_DIR}/.." && pwd)"
TEMPLATE="${ROOT_DIR}/scripts/apply_ci_pack.sh.tftpl"
WRAPPER="${ROOT_DIR}/scripts/gh_retry.sh.tftpl"
CANONICAL_WRAPPER="${TREE_DIR}/workstream_handler_v1/scripts/gh_retry.sh.tftpl"
CI_TPL_DIR="${ROOT_DIR}/ci_templates"

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

# Single-line base64 of a real KB-408 template file — exactly what the
# engine's base64encode(file(...)) splice carries at render time.
tpl_b64() { # tpl_b64 <template-file>
    base64 "$1" | tr -d '\n'
}

make_fixtures() { # make_fixtures <dir>
    local fx="$1"
    mkdir -p "${fx}"
    : >"${fx}/call.log"
}

# ── Renderer ─────────────────────────────────────────────────────────────────
# Replays the templatefile splice faithfully: the gh_retry wrapper is
# content-substituted raw; shellquote markers render as single-quoted
# literals (test values must not contain single quotes); the seven template
# payloads are spliced as base64 of the REAL ci_templates files; no {{ marker
# may survive the render.
render_apply() { # render_apply <org> <repos_json> <plan_json> <classification_file> <out> [force_b64_label:BAD]
    local org="$1" repos_json="$2" plan_json="$3" anchor="$4" out="$5" bad="${6:-}"
    assert_no_single_quote "${org}"
    assert_no_single_quote "${repos_json}"
    assert_no_single_quote "${plan_json}"
    assert_no_single_quote "${anchor}"
    local wrap
    wrap="$(cat "${WRAPPER}")"
    local rendered
    rendered="$(cat "${TEMPLATE}")"
    local payload
    for payload in go_ci ts_ci py_ci starter_ci go_db ts_db py_db; do
        local val
        if [ "${bad}" = "bad:${payload}" ]; then
            val="!!!not-base64!!!"
        else
            case "${payload}" in
                go_ci)      val="$(tpl_b64 "${CI_TPL_DIR}/github-ci-go.yml")" ;;
                ts_ci)      val="$(tpl_b64 "${CI_TPL_DIR}/github-ci-ts.yml")" ;;
                py_ci)      val="$(tpl_b64 "${CI_TPL_DIR}/github-ci-py.yml")" ;;
                starter_ci) val="$(tpl_b64 "${CI_TPL_DIR}/github-ci-starter.yml")" ;;
                go_db)      val="$(tpl_b64 "${CI_TPL_DIR}/dependabot-go.yml")" ;;
                ts_db)      val="$(tpl_b64 "${CI_TPL_DIR}/dependabot-ts.yml")" ;;
                py_db)      val="$(tpl_b64 "${CI_TPL_DIR}/dependabot-py.yml")" ;;
            esac
        fi
        assert_no_single_quote "${val}"
        rendered="${rendered//'{{ .'"${payload}"'_b64 | shellquote }}'/"'${val}'"}"
    done
    rendered="${rendered//'{{ .gh_retry }}'/"${wrap}"}"
    rendered="${rendered//'{{ .org | shellquote }}'/"'${org}'"}"
    rendered="${rendered//'{{ .repos_json | shellquote }}'/"'${repos_json}'"}"
    rendered="${rendered//'{{ .plan_json | shellquote }}'/"'${plan_json}'"}"
    rendered="${rendered//'{{ .classification_file | shellquote }}'/"'${anchor}'"}"
    printf '%s' "${rendered}" >"${out}"
    if grep -q '{{' "${out}"; then
        echo "BUG: rendered apply template still contains template markers" >&2
        exit 99
    fi
    chmod +x "${out}"
}

# ── Stub gh ──────────────────────────────────────────────────────────────────
# Fixture-driven; every invocation appends "METHOD URL" (and, for mutating
# calls, a "BODY <json>" line) to APPLY_GH_LOG. Contents writes persist the
# body per path key (ci|db) so a later read-back GET serves the bytes that
# were PUT — API-faithful round-trip. Deliberate read-back overrides live in
# readback<key>.json.
cat >"${BIN_STUB_GH}/gh" <<'EOF'
#!/usr/bin/env bash
set -u
fx="${APPLY_GH_FIXTURES:?APPLY_GH_FIXTURES not set}"
log="${APPLY_GH_LOG:-/dev/null}"

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
    "repos/${org}/${repo}/contents/.github/workflows/ci.yml"*) key="ci" ;;
    "repos/${org}/${repo}/contents/.github/dependabot.yml"*)  key="db" ;;
    "repos/${org}/${repo}")
        [ -d "$fxr" ] || { echo "stub: no fixture dir for ${repo}" >&2; exit 9; }
        if [ -f "${fxr}/repo.rc" ]; then
            rc="$(cat "${fxr}/repo.rc")"
            [ -f "${fxr}/repo.err" ] && cat "${fxr}/repo.err" >&2
            exit "$rc"
        fi
        [ -f "${fxr}/repo.json" ] || { echo "stub: missing fixture repo.json for ${repo}" >&2; exit 9; }
        cat "${fxr}/repo.json"
        exit 0
        ;;
    *)
        echo "stub: unknown gh api path: ${url}" >&2
        exit 9
        ;;
esac

[ -d "$fxr" ] || { echo "stub: no fixture dir for ${repo}" >&2; exit 9; }

if [ "${method}" != "GET" ]; then
    erc="${fxr}/put.rc"
    [ -f "$erc" ] && { rc="$(cat "$erc")";
        [ -f "${fxr}/put.err" ] && cat "${fxr}/put.err" >&2; exit "$rc"; }
    [ -n "${input}" ] && cp "${input}" "${fxr}/lastbody.${key}.json"
    touch "${fxr}/.mutated"
    if [ -f "${fxr}/put.${key}.json" ]; then cat "${fxr}/put.${key}.json"
    else printf '{"content": {"sha": "postsha-%s"}}' "${key}"; fi
    exit 0
fi

# GET: after a successful PUT of this file, the read-back serves the stored
# bytes (deliberate mismatch/error fixtures win, API-faithfully).
if [ -f "${fxr}/readback.${key}.rc" ]; then
    rc="$(cat "${fxr}/readback.${key}.rc")"
    [ -f "${fxr}/readback.${key}.err" ] && cat "${fxr}/readback.${key}.err" >&2
    exit "$rc"
fi
if [ -f "${fxr}/readback.${key}.json" ]; then
    cat "${fxr}/readback.${key}.json"
    exit 0
fi
if [ -f "${fxr}/lastbody.${key}.json" ]; then
    content="$(jq -j '.content' "${fxr}/lastbody.${key}.json" 2>/dev/null)" || content=""
    printf '{"content": "%s", "sha": "postsha-%s"}' "${content}" "${key}"
    exit 0
fi
erc="${fxr}/get.${key}.rc"
[ -f "$erc" ] && { rc="$(cat "$erc")";
    [ -f "${fxr}/get.${key}.err" ] && cat "${fxr}/get.${key}.err" >&2; exit "$rc"; }
[ -f "${fxr}/get.${key}.json" ] && cat "${fxr}/get.${key}.json" && exit 0
echo 'gh: Not Found (HTTP 404)' >&2
exit 1
EOF

chmod +x "${BIN_STUB_GH}/gh"

# run_apply <script> <fixtures-dir> <out> <err>; echoes artifact stdout.
run_apply() {
    local script="$1" fixtures="$2" out="$3" err="$4"
    local rc=0
    set +e
    (
        export PATH="${BIN_STUB_GH}:${PATH}"
        export APPLY_GH_FIXTURES="${fixtures}"
        export APPLY_GH_LOG="${fixtures}/call.log"
        export GH_RETRY_MAX_ATTEMPTS=1
        export GH_RETRY_BACKOFF_SECONDS="0 0 0"
        export GH_RETRY_SCRIPT_SLEEP_BUDGET=0
        export READBACK_SLEEP=0
        export GH_TOKEN="supersecret-token-canary"
        "${script}"
    ) >"${out}" 2>"${err}"
    rc=$?
    set -e
    return "${rc}"
}

# mk_anchor <name> -> <path>; each anchor lives in its own run dir so
# per-scenario artifacts (always named compliance.json in dirname(anchor))
# cannot overwrite each other.
mk_anchor() {
    local name="$1"
    mkdir -p "${WORK_ROOT}/runs/${name}"
    printf '{}' >"${WORK_ROOT}/runs/${name}/classification.json"
    printf '%s' "${WORK_ROOT}/runs/${name}/classification.json"
}

# existing_file_fixture <repo-dir> <key> <template-file|other-file> [sha]
file_fixture() {
    local dir="$1" key="$2" src="$3" sha="${4:-csha-original}"
    printf '{"content": "%s", "sha": "%s"}' "$(tpl_b64 "${src}")" "${sha}" \
        >"${dir}/get.${key}.json"
}

# rendered fixture content for the PUT-body checks: `content` decodes to the
# template bytes exactly.
body_matches_template() { # body_matches_template <lastbody.json> <template-file>
    local body="$1" tpl="$2" tmp
    tmp="${WORK_ROOT}/bodydec.$$.$RANDOM.bin"
    jq -j '.content' "${body}" >"${tmp}.b64"
    tr -d '\n' <"${tmp}.b64" | base64 -d >"${tmp}" 2>/dev/null
    if cmp -s "${tmp}" "${tpl}"; then
        rm -f "${tmp}" "${tmp}.b64"
        printf 'ok'
    else
        rm -f "${tmp}" "${tmp}.b64"
        printf 'mismatch'
    fi
}

# Recomputed independent summary counts from the artifact rows (mirrors the
# script's own self-check but lives OUTSIDE the script, so a self-check bug
# that agrees with the composer still shows here).
row_stats() { # row_stats <artifact> -> newline-joined k=v facts on fd 1
    jq -r '
      def filecount(action):
        [.repos[] | .files? // {} | (.ci // {}), (.dependabot // {})]
        | [.[] | select(.action == action)] | length;
      def rbfalse:
        [.repos[] | .files? // {} | (.ci // {}), (.dependabot // {})]
        | [.[] | select(.readback_verified == false and .action != null)] | length;
      "rows=\(.repos | length)",
      "verified=\([.repos[] | select(.status == "verified")] | length)",
      "error=\([.repos[] | select(.status == "error")] | length)",
      "unplanned=\([.repos[] | select(.status == "unplanned")] | length)",
      "created=\(filecount("created"))",
      "updated=\(filecount("updated"))",
      "none=\(filecount("none"))",
      "rbfalse=\(rbfalse)"
    ' "$1"
}

# ═════════════════════════════════════════════════════════════════════════════
# 1. Vendored wrapper identity + structural wiring
# ═════════════════════════════════════════════════════════════════════════════
check "gh_retry vendored byte-identical to workstream_handler_v1" cmp -s "${WRAPPER}" "${CANONICAL_WRAPPER}"
check "main.chcl renders the compliance step through templatefile" grep -q 'templatefile("./scripts/apply_ci_pack.sh.tftpl"' "${ROOT_DIR}/main.chcl"
check "main.chcl renders the delta step through templatefile" grep -q 'templatefile("./scripts/sweep_delta.sh.tftpl"' "${ROOT_DIR}/main.chcl"
check "main.chcl splices the gh_retry shim into the compliance step" grep -Eq 'gh_retry +[[:space:]]*= *file\("./scripts/gh_retry.sh.tftpl"\)' "${ROOT_DIR}/main.chcl"
check "compliance template splices the wrapper as raw content" test "$(grep -o '{{ *\.gh_retry }}' "${TEMPLATE}" | wc -l)" = "1"
check "compliance template splices all seven template payloads" test "$(grep -o "_b64 | shellquote" "${TEMPLATE}" | wc -l)" = "7"
check "main.chcl passes every template payload by engine base64encode" test "$(grep -o 'base64encode(file("./ci_templates/' "${ROOT_DIR}/main.chcl" | wc -l)" = "7"
check "compliance template consumes the classification anchor" grep -q 'ANCHOR_PATH={{ .classification_file | shellquote }}' "${TEMPLATE}"
check "compliance template consumes the compliance plan" grep -q 'PLAN_JSON={{ .plan_json | shellquote }}' "${TEMPLATE}"
check "merged step hand-off goes to compliance_apply" grep -q 'next = step.compliance_apply' "${ROOT_DIR}/main.chcl"
check "compliance_apply hands off to protect" grep -Eq 'compliance_apply.+next = step.protect|next = step.protect' "${ROOT_DIR}/main.chcl"
check "codeql_triage hands off to delta_report" grep -q 'next = step.delta_report' "${ROOT_DIR}/main.chcl"
check "compliance_apply failure state is declared" grep -q 'state "compliance_apply_failed"' "${ROOT_DIR}/main.chcl"
check "delta_report failure state is declared" grep -q 'state "delta_report_failed"' "${ROOT_DIR}/main.chcl"
check "compliance artifacts declared as outputs" test "$(grep -c 'output "compliance_file"' "${ROOT_DIR}/main.chcl")" = "1" &&
    test "$(grep -c 'output "delta_file"' "${ROOT_DIR}/main.chcl")" = "1"

# ═════════════════════════════════════════════════════════════════════════════
# 2. Config gates: typed, fail-loud, BEFORE any network call
# ═════════════════════════════════════════════════════════════════════════════
FXC="${WORK_ROOT}/fxconfig"
make_fixtures "${FXC}"
mkdir -p "${FXC}/r1"
printf '{"default_branch": "main"}' >"${FXC}/r1/repo.json"

render_and_run_config() { # render_and_run_config <label> <plan_json> <out> <err> [bad_payload]
    local rendered="${WORK_ROOT}/apply.${1}.sh"
    render_apply testorg '["r1"]' "$2" "$(mk_anchor "c-${1}.json")" "${rendered}" "${5:-}"
    run_apply "${rendered}" "${FXC}" "$3" "$4" >/dev/null
}

FAIL_RC=0
render_and_run_config empty '[]' "${WORK_ROOT}/c-empty.out" "${WORK_ROOT}/c-empty.err" || FAIL_RC=$?
check_not "config: empty plan fails" test "${FAIL_RC}" -eq 0
FAIL_RC=0
render_and_run_config dup '[{"repo":"r1","pack":"go"},{"repo":"r1","pack":"go"}]' "${WORK_ROOT}/c-dup.out" "${WORK_ROOT}/c-dup.err" || FAIL_RC=$?
check_not "config: duplicate repo rows fail" test "${FAIL_RC}" -eq 0
FAIL_RC=0
render_and_run_config badpack '[{"repo":"r1","pack":"ruby"}]' "${WORK_ROOT}/c-bad.out" "${WORK_ROOT}/c-bad.err" || FAIL_RC=$?
check_not "config: pack outside the language set fails" test "${FAIL_RC}" -eq 0
FAIL_RC=0
render_and_run_config oos '[{"repo":"elsewhere","pack":"go"}]' "${WORK_ROOT}/c-oos.out" "${WORK_ROOT}/c-oos.err" || FAIL_RC=$?
check_not "config: plan repo outside the sweep scope fails" test "${FAIL_RC}" -eq 0
FAIL_RC=0
render_and_run_config starter_nolang '[{"repo":"r1","pack":"starter"}]' "${WORK_ROOT}/c-snl.out" "${WORK_ROOT}/c-snl.err" || FAIL_RC=$?
check_not "config: starter row without starter_language fails" test "${FAIL_RC}" -eq 0
FAIL_RC=0
render_and_run_config nonstarter_lang '[{"repo":"r1","pack":"go","starter_language":"py"}]' "${WORK_ROOT}/c-nsl.out" "${WORK_ROOT}/c-nsl.err" || FAIL_RC=$?
check_not "config: non-starter row with a starter_language fails" test "${FAIL_RC}" -eq 0
FAIL_RC=0
render_and_run_config badb64 '[{"repo":"r1","pack":"go"}]' "${WORK_ROOT}/c-bb64.out" "${WORK_ROOT}/c-bb64.err" bad:go_ci || FAIL_RC=$?
check_not "config: undecodable template payload fails before any call" test "${FAIL_RC}" -eq 0
FAIL_RC=0
render_and_run_config badb64db '[{"repo":"r1","pack":"go"}]' "${WORK_ROOT}/c-bb64db.out" "${WORK_ROOT}/c-bb64db.err" bad:ts_db || FAIL_RC=$?
check_not "config: undecodable ts dependabot payload fails before any call" test "${FAIL_RC}" -eq 0
check "config: all gates fired before ANY gh call" test ! -s "${FXC}/call.log"

# ═════════════════════════════════════════════════════════════════════════════
# 3. Happy path: absent files -> created, byte-verified by read-back
# ═════════════════════════════════════════════════════════════════════════════
FX1="${WORK_ROOT}/fx1"
make_fixtures "${FX1}"
mkdir -p "${FX1}/go1"
printf '{"default_branch": "main"}' >"${FX1}/go1/repo.json"

P1="${WORK_ROOT}/apply1.sh"
render_apply testorg '["go1"]' '[{"repo":"go1","pack":"go"}]' "$(mk_anchor "a1.json")" "${P1}"
P1_RC=0
run_apply "${P1}" "${FX1}" "${WORK_ROOT}/a1.out" "${WORK_ROOT}/a1.err" || P1_RC=$?
expect_count "S3 apply exits 0 on an absent-files repo" 0 "${P1_RC}"
ART1="$(tr -d '[:space:]' <"${WORK_ROOT}/a1.out")"
check "S3 stdout is exactly the artifact path" test -f "${ART1}"
check "S3 stdout has no second line" test "$(wc -l <"${WORK_ROOT}/a1.out")" -eq 1
check "S3 artifact schema" jq -e '.schema == "devops_maint_v1.compliance.v1"' "${ART1}" >/dev/null
check "S3 row is verified" jq -e '.repos[0].status == "verified"' "${ART1}" >/dev/null
check "S3 both files recorded as created and readback-verified" jq -e '.repos[0].files.ci.action == "created" and .repos[0].files.ci.readback_verified == true and .repos[0].files.dependabot.action == "created" and .repos[0].files.dependabot.readback_verified == true' "${ART1}" >/dev/null
check "S3 file rows carry the exact KB-408 target paths" jq -e '.repos[0].files.ci.target_path == ".github/workflows/ci.yml" and .repos[0].files.dependabot.target_path == ".github/dependabot.yml"' "${ART1}" >/dev/null
check "S3 summary counts" jq -e '.summary.repos_verified_total == 1 and .summary.repos_error_total == 0 and .summary.repos_unplanned_total == 0 and .summary.files_created_total == 2 and .summary.files_updated_total == 0 and .summary.files_unchanged_total == 0 and .summary.verify_mismatch_total == 0' "${ART1}" >/dev/null
check "S3 summary counts recomputed independently" test "$(row_stats "${ART1}" | tr '\n' '|' | sed 's/|$//')" = "$(printf 'rows=1|verified=1|error=0|unplanned=0|created=2|updated=0|none=0|rbfalse=0')"

check "S3 log: repo metadata enumerated" grep -q "GET repos/testorg/go1$" "${FX1}/call.log"
expect_count "S3 log: exactly two PUTs (ci + dependabot)" 2 "$(grep -c "^PUT repos/testorg/go1/contents/" "${FX1}/call.log")"
check "S3 log: read-back GETs after each PUT" test "$(grep -c "^GET repos/testorg/go1/contents/" "${FX1}/call.log")" -eq 4

check "S3 PUT ci body carries the exact commit message" test "$(jq -r '.message' "${FX1}/go1/lastbody.ci.json")" = "devops_maint_v1: apply Lane B compliance pack (KB-408 templates)"
check "S3 PUT dependabot body carries the exact commit message" test "$(jq -r '.message' "${FX1}/go1/lastbody.db.json")" = "devops_maint_v1: apply Lane B dependabot config (KB-408 template)"
check "S3 PUT bodies target the default branch" test "$(jq -r '.branch' "${FX1}/go1/lastbody.ci.json")" = "main" && test "$(jq -r '.branch' "${FX1}/go1/lastbody.db.json")" = "main"
check "S3 create bodies carry no sha key" test "$(jq 'has("sha")' "${FX1}/go1/lastbody.ci.json")" = "false" && test "$(jq 'has("sha")' "${FX1}/go1/lastbody.db.json")" = "false"
check "S3 PUT ci content decodes byte-identically to the real go template" test "$(body_matches_template "${FX1}/go1/lastbody.ci.json" "${CI_TPL_DIR}/github-ci-go.yml")" = "ok"
check "S3 PUT dependabot content decodes byte-identically to the real go dependabot template" test "$(body_matches_template "${FX1}/go1/lastbody.db.json" "${CI_TPL_DIR}/dependabot-go.yml")" = "ok"
check "S3 PUT ci content is single-line base64" test "$(jq -j '.content' "${FX1}/go1/lastbody.ci.json" | wc -l)" = "1"

# ═════════════════════════════════════════════════════════════════════════════
# 4. Idempotency: repo already carries the exact templates -> no write
# ═════════════════════════════════════════════════════════════════════════════
FX2="${WORK_ROOT}/fx2"
make_fixtures "${FX2}"
mkdir -p "${FX2}/go2"
printf '{"default_branch": "main"}' >"${FX2}/go2/repo.json"
file_fixture "${FX2}/go2" ci "${CI_TPL_DIR}/github-ci-go.yml" "existing-sha"
file_fixture "${FX2}/go2" db "${CI_TPL_DIR}/dependabot-go.yml" "existing-sha"

P2="${WORK_ROOT}/apply2.sh"
render_apply testorg '["go2"]' '[{"repo":"go2","pack":"go"}]' "$(mk_anchor "a2.json")" "${P2}"
P2_RC=0
run_apply "${P2}" "${FX2}" "${WORK_ROOT}/a2.out" "${WORK_ROOT}/a2.err" || P2_RC=$?
expect_count "S4 idempotent run exits 0" 0 "${P2_RC}"
ART2="$(tr -d '[:space:]' <"${WORK_ROOT}/a2.out")"
check "S4 verified with no write" jq -e '.repos[0].status == "verified" and .repos[0].files.ci.action == "none" and .repos[0].files.dependabot.action == "none" and .repos[0].files.ci.readback_verified == true and .repos[0].files.dependabot.readback_verified == true' "${ART2}" >/dev/null
check "S4 summary records unchanged files" jq -e '.summary.files_unchanged_total == 2 and .summary.files_created_total == 0 and .summary.files_updated_total == 0 and .summary.repos_verified_total == 1' "${ART2}" >/dev/null
check_not "S4 no PUT issued" grep -q "^PUT " "${FX2}/call.log"
check "S4 both files probed once (no read-back churn)" test "$(grep -c "^GET repos/testorg/go2/contents/" "${FX2}/call.log")" -eq 2

# ═════════════════════════════════════════════════════════════════════════════
# 5. Drift: existing bytes differ -> PUT with the fetched sha, verified
# ═════════════════════════════════════════════════════════════════════════════
FX3="${WORK_ROOT}/fx3"
make_fixtures "${FX3}"
mkdir -p "${FX3}/go3"
printf '{"default_branch": "trunk"}' >"${FX3}/go3/repo.json"
file_fixture "${FX3}/go3" ci "${CI_TPL_DIR}/github-ci-py.yml" "drifted-sha"   # wrong-template bytes = drift
file_fixture "${FX3}/go3" db "${CI_TPL_DIR}/dependabot-ts.yml" "drifted-sha" # likewise for dependabot

P3="${WORK_ROOT}/apply3.sh"
render_apply testorg '["go3"]' '[{"repo":"go3","pack":"go"}]' "$(mk_anchor "a3.json")" "${P3}"
P3_RC=0
run_apply "${P3}" "${FX3}" "${WORK_ROOT}/a3.out" "${WORK_ROOT}/a3.err" || P3_RC=$?
expect_count "S5 drift-heal run exits 0" 0 "${P3_RC}"
ART3="$(tr -d '[:space:]' <"${WORK_ROOT}/a3.out")"
check "S5 updated + readback-verified" jq -e '.repos[0].status == "verified" and .repos[0].files.ci.action == "updated" and .repos[0].files.ci.readback_verified == true and .repos[0].files.dependabot.action == "updated"' "${ART3}" >/dev/null
check "S5 summary records updates, not creates" jq -e '.summary.files_updated_total == 2 and .summary.files_created_total == 0 and .summary.files_unchanged_total == 0' "${ART3}" >/dev/null
expect_count "S5 exactly two PUTs" 2 "$(grep -c "^PUT repos/testorg/go3/contents/" "${FX3}/call.log")"
check "S5 PUT bodies carry the fetched drift sha" test "$(jq -r '.sha' "${FX3}/go3/lastbody.ci.json")" = "drifted-sha" && test "$(jq -r '.sha' "${FX3}/go3/lastbody.db.json")" = "drifted-sha"
check "S5 PUT lands on the declared default branch" test "$(jq -r '.branch' "${FX3}/go3/lastbody.ci.json")" = "trunk"
check "S5 healed bytes match the go pack templates" test "$(body_matches_template "${FX3}/go3/lastbody.ci.json" "${CI_TPL_DIR}/github-ci-go.yml")" = "ok" && test "$(body_matches_template "${FX3}/go3/lastbody.db.json" "${CI_TPL_DIR}/dependabot-go.yml")" = "ok"

# ═════════════════════════════════════════════════════════════════════════════
# 6. Pack routing: go/ts self-describing; starter routes dependabot by
#    starter_language (checked against the REAL template bytes)
# ═════════════════════════════════════════════════════════════════════════════
FX4="${WORK_ROOT}/fx4"
make_fixtures "${FX4}"
for r in g6 t6 s6; do
    mkdir -p "${FX4}/${r}"
    printf '{"default_branch": "main"}' >"${FX4}/${r}/repo.json"
done

P4="${WORK_ROOT}/apply4.sh"
render_apply testorg '["g6","t6","s6"]' \
    '[{"repo":"g6","pack":"go"},{"repo":"t6","pack":"ts"},{"repo":"s6","pack":"starter","starter_language":"py"}]' \
    "$(mk_anchor "a4.json")" "${P4}"
P4_RC=0
run_apply "${P4}" "${FX4}" "${WORK_ROOT}/a4.out" "${WORK_ROOT}/a4.err" || P4_RC=$?
expect_count "S6 multi-pack run exits 0" 0 "${P4_RC}"
ART4="$(tr -d '[:space:]' <"${WORK_ROOT}/a4.out")"
check "S6 all three repos verified" jq -e '[.repos[] | select(.status == "verified")] | length == 3 and .summary.files_created_total == 6' "${ART4}" >/dev/null
check "S6 starter row carries its declared language" jq -e '.repos[] | select(.repo == "s6") | .pack == "starter" and .starter_language == "py"' "${ART4}" >/dev/null
check "S6 g6 got the go ci template bytes" test "$(body_matches_template "${FX4}/g6/lastbody.ci.json" "${CI_TPL_DIR}/github-ci-go.yml")" = "ok"
check "S6 g6 got the go dependabot template bytes" test "$(body_matches_template "${FX4}/g6/lastbody.db.json" "${CI_TPL_DIR}/dependabot-go.yml")" = "ok"
check "S6 t6 got the ts ci template bytes" test "$(body_matches_template "${FX4}/t6/lastbody.ci.json" "${CI_TPL_DIR}/github-ci-ts.yml")" = "ok"
check "S6 t6 got the ts dependabot template bytes" test "$(body_matches_template "${FX4}/t6/lastbody.db.json" "${CI_TPL_DIR}/dependabot-ts.yml")" = "ok"
check "S6 s6 got the STARTER ci template bytes" test "$(body_matches_template "${FX4}/s6/lastbody.ci.json" "${CI_TPL_DIR}/github-ci-starter.yml")" = "ok"
check "S6 starter_language=py routed s6's dependabot to the py template" test "$(body_matches_template "${FX4}/s6/lastbody.db.json" "${CI_TPL_DIR}/dependabot-py.yml")" = "ok"

# ═════════════════════════════════════════════════════════════════════════════
# 7. Read-back mismatch: typed step failure AFTER artifact preservation
# ═════════════════════════════════════════════════════════════════════════════
FX5="${WORK_ROOT}/fx5"
make_fixtures "${FX5}"
mkdir -p "${FX5}/go5"
printf '{"default_branch": "main"}' >"${FX5}/go5/repo.json"
# The PUT succeeds but the later read-back GET serves DIFFERENT bytes (a
# non-404 error would be transport, not mismatch).
printf '{"content": "%s", "sha": "wrong-sha"}' "$(tpl_b64 "${CI_TPL_DIR}/github-ci-ts.yml")" >"${FX5}/go5/readback.ci.json"

P5="${WORK_ROOT}/apply5.sh"
render_apply testorg '["go5"]' '[{"repo":"go5","pack":"go"}]' "$(mk_anchor "a5.json")" "${P5}"
P5_RC=0
run_apply "${P5}" "${FX5}" "${WORK_ROOT}/a5.out" "${WORK_ROOT}/a5.err" || P5_RC=$?
expect_count "S7 read-back mismatch exits NON-ZERO" 1 "${P5_RC}"
check "S7 typed failure names the compliance gate" grep -q '^apply_ci_pack_failed:' "${WORK_ROOT}/a5.err"
check "S7 typed failure names the preserved artifact path" grep -qF "$(mk_anchor "a5.json" | sed 's|[^/]*$||')compliance.json" "${WORK_ROOT}/a5.err"
check "S7 artifact was still written and preserved" test -f "${WORK_ROOT}/runs/a5.json/compliance.json"
ART5="${WORK_ROOT}/runs/a5.json/compliance.json"
check "S7 row is error with the read-back error" jq -e '.repos[0].status == "error" and .repos[0].errors[0].surface == "ci_readback" and (.repos[0].errors[0].detail | test("read-back|template"))' "${ART5}" >/dev/null
check "S7 mismatched file zeroed from the row" jq -e '.repos[0].files.ci == null' "${ART5}" >/dev/null
check "S7 un-attempted dependabot file stays unverified" jq -e '.repos[0].files.dependabot != null and .repos[0].files.dependabot.action == "none" and .repos[0].files.dependabot.readback_verified == false' "${ART5}" >/dev/null
check "S7 summary counts one mismatch and zero verified" jq -e '.summary.verify_mismatch_total == 1 and .summary.repos_verified_total == 0 and .summary.repos_error_total == 1' "${ART5}" >/dev/null
check "S7 log has the PUT before the failed read-back" test "$(grep -c "^PUT repos/testorg/go5/contents/" "${FX5}/call.log")" = "1"

# ═════════════════════════════════════════════════════════════════════════════
# 8. PUT failure: per-file error row, sweep-style bookkeeping, rc 0
# ═════════════════════════════════════════════════════════════════════════════
FX6="${WORK_ROOT}/fx6"
make_fixtures "${FX6}"
mkdir -p "${FX6}/go6"
printf '{"default_branch": "main"}' >"${FX6}/go6/repo.json"
printf 1 >"${FX6}/go6/put.rc"
printf 'gh: server error (HTTP 500)' >"${FX6}/go6/put.err"

P6="${WORK_ROOT}/apply6.sh"
render_apply testorg '["go6"]' '[{"repo":"go6","pack":"go"}]' "$(mk_anchor "a6.json")" "${P6}"
P6_RC=0
run_apply "${P6}" "${FX6}" "${WORK_ROOT}/a6.out" "${WORK_ROOT}/a6.err" || P6_RC=$?
expect_count "S8 PUT failure keeps rc 0 (bookkeeping, not abort)" 0 "${P6_RC}"
ART6="$(tr -d '[:space:]' <"${WORK_ROOT}/a6.out")"
check "S8 row is error with the apply error" jq -e '.repos[0].status == "error" and .repos[0].errors[0].surface == "ci_apply" and (.repos[0].errors[0].detail | test("contents API put failed|500"))' "${ART6}" >/dev/null
check "S8 failed file zeroed; the second file stays honest" jq -e '.repos[0].files.ci == null and .repos[0].files.dependabot.action == "none"' "${ART6}" >/dev/null
check "S8 summary: zero verified, one errored, zero mismatch" jq -e '.summary.repos_verified_total == 0 and .summary.repos_error_total == 1 and .summary.verify_mismatch_total == 0' "${ART6}" >/dev/null
check_not "S8 no read-back after a failed PUT" grep -q "^GET repos/testorg/go6/contents/" "${FX6}/call.log"
check_not "S8 rc-0 run still emits NO typed failure" grep -q '^apply_ci_pack_failed:' "${WORK_ROOT}/a6.err"

# ═════════════════════════════════════════════════════════════════════════════
# 9. Repo-metadata failure: error row with ZERO contents calls
# ═════════════════════════════════════════════════════════════════════════════
FX7="${WORK_ROOT}/fx7"
make_fixtures "${FX7}"
mkdir -p "${FX7}/go7"
printf 1 >"${FX7}/go7/repo.rc"
printf 'gh: repository not found (HTTP 404)' >"${FX7}/go7/repo.err"

P7="${WORK_ROOT}/apply7.sh"
render_apply testorg '["go7"]' '[{"repo":"go7","pack":"go"}]' "$(mk_anchor "a7.json")" "${P7}"
P7_RC=0
run_apply "${P7}" "${FX7}" "${WORK_ROOT}/a7.out" "${WORK_ROOT}/a7.err" || P7_RC=$?
expect_count "S9 metadata failure keeps rc 0" 0 "${P7_RC}"
ART7="$(tr -d '[:space:]' <"${WORK_ROOT}/a7.out")"
check "S9 row is error with the metadata surface" jq -e '.repos[0].status == "error" and .repos[0].errors[0].surface == "repo_metadata" and .repos[0].files == null' "${ART7}" >/dev/null
check "S9 no contents call for the repo" test "$(grep -c "^GET repos/testorg/go7/contents/" "${FX7}/call.log")" -eq 0
check "S9 summary records one errored repo" jq -e '.summary.repos_error_total == 1 and .summary.repos_verified_total == 0' "${ART7}" >/dev/null

# ═════════════════════════════════════════════════════════════════════════════
# 10. Existing-file probe failure (non-404): error row, no write, rc 0
# ═════════════════════════════════════════════════════════════════════════════
FX8="${WORK_ROOT}/fx8"
make_fixtures "${FX8}"
mkdir -p "${FX8}/go8"
printf '{"default_branch": "main"}' >"${FX8}/go8/repo.json"
printf 1 >"${FX8}/go8/get.ci.rc"
printf 'gh: upstream timed out (HTTP 504)' >"${FX8}/go8/get.ci.err"

P8="${WORK_ROOT}/apply8.sh"
render_apply testorg '["go8"]' '[{"repo":"go8","pack":"go"}]' "$(mk_anchor "a8.json")" "${P8}"
P8_RC=0
run_apply "${P8}" "${FX8}" "${WORK_ROOT}/a8.out" "${WORK_ROOT}/a8.err" || P8_RC=$?
expect_count "S10 probe failure keeps rc 0" 0 "${P8_RC}"
ART8="$(tr -d '[:space:]' <"${WORK_ROOT}/a8.out")"
check "S10 row is error with the ci-apply surface" jq -e '.repos[0].status == "error" and .repos[0].errors[0].surface == "ci_apply"' "${ART8}" >/dev/null
check "S10 nothing was written to the repo" test "$(grep -c "^PUT repos/testorg/go8" "${FX8}/call.log")" -eq 0
check "S10 probe error names transport, never a mismatch" jq -e '.summary.verify_mismatch_total == 0' "${ART8}" >/dev/null

# ═════════════════════════════════════════════════════════════════════════════
# 11. Unplanned repos: report-only rows, zero gh calls against them
# ═════════════════════════════════════════════════════════════════════════════
FX9="${WORK_ROOT}/fx9"
make_fixtures "${FX9}"
for r in planned unpl ; do
    mkdir -p "${FX9}/${r}"
    printf '{"default_branch": "main"}' >"${FX9}/${r}/repo.json"
done

P9="${WORK_ROOT}/apply9.sh"
render_apply testorg '["planned","unpl"]' '[{"repo":"planned","pack":"go"}]' "$(mk_anchor "a9.json")" "${P9}"
P9_RC=0
run_apply "${P9}" "${FX9}" "${WORK_ROOT}/a9.out" "${WORK_ROOT}/a9.err" || P9_RC=$?
expect_count "S11 staged-rollout run exits 0" 0 "${P9_RC}"
ART9="$(tr -d '[:space:]' <"${WORK_ROOT}/a9.out")"
check "S11 planned row verified, unplanned row report-only" jq -e '[.repos[] | select(.repo == "planned")][0].status == "verified" and [.repos[] | select(.repo == "unpl")][0].status == "unplanned" and [.repos[] | select(.repo == "unpl")][0].files == null' "${ART9}" >/dev/null
check "S11 unplanned row carries no pack" jq -e '[.repos[] | select(.repo == "unpl")][0].pack == null' "${ART9}" >/dev/null
check "S11 summary counts one unplanned repo" jq -e '.summary.repos_unplanned_total == 1 and .summary.repos_verified_total == 1' "${ART9}" >/dev/null
check_not "S11 no gh call ever touched the unplanned repo" grep -qF "repos/testorg/unpl" "${FX9}/call.log"

# ═════════════════════════════════════════════════════════════════════════════
# 12. stdout/stderr protocol + token hygiene (replay of the happy-path run)
# ═════════════════════════════════════════════════════════════════════════════
check "S12 stdout carried no second line" test "$(wc -l <"${WORK_ROOT}/a1.out")" -eq 1
check "S12 stderr carries the per-repo log lines" grep -q "repo go1: applying compliance pack" "${WORK_ROOT}/a1.err"
check "S12 GH_TOKEN canary absent from the artifact" test "$(grep -cF 'supersecret-token-canary' "${ART1}")" = "0"
check_not "S12 GH_TOKEN canary absent from stdout+stderr" grep -qF 'supersecret-token-canary' "${WORK_ROOT}/a1.out" "${WORK_ROOT}/a1.err"
check "S12 typed prefix style reserved for step failures" test "$(grep -cE '^apply(_ci_pack)?_failed:' "${WORK_ROOT}/a1.err")" = "0"

# ═════════════════════════════════════════════════════════════════════════════
# 13. Multi-repo mixed-state sweep: error rows continue the sweep
# ═════════════════════════════════════════════════════════════════════════════
FX10="${WORK_ROOT}/fx10"
make_fixtures "${FX10}"
for r in ok bad; do
    mkdir -p "${FX10}/${r}"
    printf '{"default_branch": "main"}' >"${FX10}/${r}/repo.json"
done
printf 1 >"${FX10}/bad/get.db.rc"
printf 'gh: database unavailable (HTTP 503)' >"${FX10}/bad/get.db.err"

P10="${WORK_ROOT}/apply10.sh"
render_apply testorg '["ok","bad"]' '[{"repo":"ok","pack":"py"},{"repo":"bad","pack":"py"}]' "$(mk_anchor "a10.json")" "${P10}"
P10_RC=0
run_apply "${P10}" "${FX10}" "${WORK_ROOT}/a10.out" "${WORK_ROOT}/a10.err" || P10_RC=$?
expect_count "S13 mixed-state sweep keeps rc 0" 0 "${P10_RC}"
ART10="$(tr -d '[:space:]' <"${WORK_ROOT}/a10.out")"
check "S13 the good repo verified despite the bad one" jq -e '[.repos[] | select(.repo == "ok")][0].status == "verified"' "${ART10}" >/dev/null
check "S13 the bad repo error with the db file surface" jq -e '[.repos[] | select(.repo == "bad")][0].status == "error" and [.repos[] | select(.repo == "bad")][0].errors[0].surface == "dependabot_apply"' "${ART10}" >/dev/null
check "S13 summary: one verified, one error" jq -e '.summary.repos_verified_total == 1 and .summary.repos_error_total == 1' "${ART10}" >/dev/null
check "S13 py pack wrote the py templates" test "$(body_matches_template "${FX10}/ok/lastbody.ci.json" "${CI_TPL_DIR}/github-ci-py.yml")" = "ok" && test "$(body_matches_template "${FX10}/ok/lastbody.db.json" "${CI_TPL_DIR}/dependabot-py.yml")" = "ok"

# ═════════════════════════════════════════════════════════════════════════════
# 14. Tree validation (node + criteria), matching the other step suites
# ═════════════════════════════════════════════════════════════════════════════
NODE_OK=1
set +e
(cd "${TREE_DIR}" && node scripts/interface_validate.mjs) >/dev/null 2>&1
NODE_OK=$?
CRIT_OK=1
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
echo "PASS: devops_maint_v1 compliance-step regression suite verified"
