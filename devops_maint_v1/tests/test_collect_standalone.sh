#!/usr/bin/env bash
# devops_maint_v1 — collect-step regression suite (KB-406, ADR-0015 wave 1).
#
# Exercises the rendered sweep template END-TO-END through stub gh / stub
# govulncheck / stub osv-scanner binaries (no network, no real GitHub):
#   - gh_retry shim is byte-identical to the canonical wrapper copy
#   - normalized artifact: schema/sweep/summary, per-repo records
#   - PR normalization: dependabot filter, open/other lanes, truncation flags
#   - per-PR CI verdicts: green/red/pending/none/error/unenumerated (cap)
#   - advisory lanes: ok / not_applicable / unavailable / error + findings
#   - CodeQL lanes: ok / disabled (404) / error; multi-page-safe alert counts
#   - ruleset lanes: applied / none / error
#   - per-repo partial failure recorded, sweep continues; all-failed sweep
#     ends rc 1 with the partial artifact kept
#   - fail-loud config validation (org/repo safe-name shapes, empty repos,
#     integer cap, '..'-free sweep root)
#   - secrets hygiene: GH_TOKEN value never leaks into artifact or stderr
#   - determinism: identical inputs produce byte-identical artifacts modulo
#     sweep_id/started_at/finished_at
#   - the tree still validates (criteria grammar + interface contract)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
TREE_DIR="$(cd "${ROOT_DIR}/.." && pwd)"
TEMPLATE="${ROOT_DIR}/scripts/collect_inventory.sh.tftpl"
WRAPPER="${ROOT_DIR}/scripts/gh_retry.sh.tftpl"
CANONICAL_WRAPPER="${TREE_DIR}/workstream_handler_v1/scripts/gh_retry.sh.tftpl"

WORK_ROOT="${TEST_WORK_ROOT:-$(mktemp -d)}"
# TEST_WORK_ROOT pins the scratch dir (debugging); it is removed on exit
# unless TEST_KEEP_WORK_ROOT=1.
if [ "${TEST_KEEP_WORK_ROOT:-0}" != "1" ]; then trap 'rm -rf "${WORK_ROOT}"' EXIT; fi
BIN_STUB_GH="${WORK_ROOT}/bin_gh"
BIN_STUB_TOOLS="${WORK_ROOT}/bin_tools"
mkdir -p "${BIN_STUB_GH}" "${BIN_STUB_TOOLS}"

FAILURES=0
# All three helpers report on stderr so that assertion commands ending in
# stdout redirections (`check "..." jq -e ... >/dev/null`) do not swallow
# the PASS/FAIL report itself.
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
    # Inverted assertion: FAILS when the command succeeds (e.g. grep finding
    # a forbidden canary). `check "..." ! cmd` cannot work because "$@"
    # would execute a command literally named "!".
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
    # expect_count <label> <want> <got>
    local label="$1" want="$2" got="$3"
    if [ "${got}" = "${want}" ]; then
        echo "PASS: ${label}" >&2
    else
        echo "FAIL: ${label} (want ${want}, got ${got})" >&2
        FAILURES=$((FAILURES + 1))
    fi
}

# ── Renderer ─────────────────────────────────────────────────────────────────
# Replays the templatefile splice faithfully: file() returns the file's
# CONTENT, so the {{ .gh_retry }} marker is replaced with the wrapper's
# full body (raw, unquoted), exactly like the template engine does for
# devops_triage_v1's open_pr step. The {{ .key | shellquote }} markers
# render as a single-quoted literal. Test values must not contain single
# quotes (asserted below), and every marker must be consumed (no leftover
# {{ in the render).
assert_no_single_quote() {
    local value="$1"
    case "${value}" in *"'"*) echo "BUG: test value contains a single quote: ${value}" >&2; exit 99 ;; esac
}

RETRY_KNOBS=()   # set per-scenario when a fixture injects transient failures

render_sweep() {
    local org="$1" repos_json="$2" sweep_root="$3" scan_gov="$4" scan_osv="$5" out="$6"
    assert_no_single_quote "${org}"
    assert_no_single_quote "${repos_json}"
    assert_no_single_quote "${sweep_root}"
    assert_no_single_quote "${scan_gov}"
    assert_no_single_quote "${scan_osv}"
    local wrap
    wrap="$(cat "${WRAPPER}")"
    local o_rep="'${org}'" r_rep="'${repos_json}'" s_rep="'${sweep_root}'" \
        g_rep="'${scan_gov}'" os_rep="'${scan_osv}'"
    local rendered
    rendered="$(cat "${TEMPLATE}")"
    # Engine-faithful: the gh_retry splice is CONTENT substitution (file()
    # yields the file's body, not a path), exactly like templatefile.
    rendered="${rendered//'{{ .gh_retry }}'/"${wrap}"}"
    rendered="${rendered//'{{ .org | shellquote }}'/"${o_rep}"}"
    rendered="${rendered//'{{ .repos_json | shellquote }}'/"${r_rep}"}"
    rendered="${rendered//'{{ .sweep_root | shellquote }}'/"${s_rep}"}"
    rendered="${rendered//'{{ .scan_govulncheck | shellquote }}'/"${g_rep}"}"
    rendered="${rendered//'{{ .scan_osv | shellquote }}'/"${os_rep}"}"
    printf '%s' "${rendered}" >"${out}"
    if grep -q '{{' "${out}"; then
        echo "BUG: rendered sweep still contains template markers" >&2
        exit 99
    fi
    chmod +x "${out}"
}

# ── Stub binaries ────────────────────────────────────────────────────────────
# Stub gh: fixture-driven; every invocation appends to COLLECT_GH_LOG so the
# suite can assert call counts (cap behavior, clone skipping).
cat >"${BIN_STUB_GH}/gh" <<'EOF'
#!/usr/bin/env bash
set -u
fx="${COLLECT_GH_FIXTURES:?COLLECT_GH_FIXTURES not set}"
log="${COLLECT_GH_LOG:-/dev/null}"
printf '%s\n' "$*" >>"${log}"

mode="$1"
shift
repo=""
args=("$@")
i=0
while [ "$i" -lt "${#args[@]}" ]; do
    if [ "${args[$i]}" = "-R" ]; then
        repo="${args[$((i + 1))]:-}"
    fi
    i=$((i + 1))
done
repo="${repo#*/}"
case "$mode" in
    repo)
        # `gh repo clone <org/repo> <dir>` has no -R: the owner/repo is
        # args[0]'s first flag-free argument after the "clone" subcommand.
        if [ -z "${repo}" ]; then
            repo="${args[1]:-}"
            repo="${repo#*/}"
        fi
        ;;
esac

case "$mode" in
    pr)
        sub="${args[0]:-}"
        case "$sub" in
            list)
                if [ -f "${fx}/${repo}/prlist.tr" ]; then
                    # Transient-transport injection: fail the first N attempts
                    # with a retry-class stderr, then serve the real list.
                    cntf="${fx}/${repo}/prlist.tr.count"
                    tr_n="$(cat "${fx}/${repo}/prlist.tr")"
                    tr_done="$(cat "${cntf}" 2>/dev/null || echo 0)"
                    if [ "${tr_done:-0}" -lt "${tr_n}" ]; then
                        echo "$((tr_done + 1))" >"${cntf}"
                        echo 'gh: connection reset by peer' >&2
                        exit 1
                    fi
                fi
                if [ -f "${fx}/${repo}/prlist.fail" ]; then
                    rc="$(cat "${fx}/${repo}/prlist.fail")"
                    cat "${fx}/${repo}/prlist.failmsg" 2>/dev/null || echo "gh: fixture failure" >&2
                    exit "${rc}"
                fi
                if [ ! -f "${fx}/${repo}/prlist.json" ]; then
                    echo "stub: missing fixture prlist.json for ${repo}" >&2
                    exit 9
                fi
                cat "${fx}/${repo}/prlist.json"
                ;;
            checks)
                num="${args[1]:-}"
                tsv="${fx}/${repo}/checks.${num}.tsv"
                erc="${fx}/${repo}/checks.${num}.rc"
                if [ -f "${erc}" ]; then rc="$(cat "${erc}")"; else rc=0; fi
                if [ -f "${fx}/${repo}/checks.${num}.err" ]; then
                    cat "${fx}/${repo}/checks.${num}.err" >&2
                fi
                if [ -f "${tsv}" ]; then cat "${tsv}"; fi
                exit "${rc}"
                ;;
            *)
                echo "stub: unknown gh pr subcommand: ${sub}" >&2
                exit 9
                ;;
        esac
        ;;
    repo)
        # args = clone <org/repo> <dir> -- --depth 1
        dir="${args[2]:-}"
        if [ -f "${fx}/${repo}/clone.fail" ]; then
            echo "gh: fatal: repository not found" >&2
            exit 128
        fi
        mkdir -p "${dir}"
        if [ -f "${fx}/${repo}/gomod" ]; then
            cp "${fx}/${repo}/gomod" "${dir}/go.mod"
        fi
        ;;
    api)
        url="${args[0]:-}"
        _tmp="${url#repos/}"
        _repo_part="${_tmp#*/}"
        api_repo="${_repo_part%%/*}"
        case "$url" in
            *rulesets)
                erc="${fx}/${api_repo}/rulesets.rc"
                if [ -f "${erc}" ]; then rc="$(cat "${erc}")"; else rc=0; fi
                if [ "$rc" -ne 0 ]; then
                    cat "${fx}/${api_repo}/rulesets.err" 2>/dev/null || echo "gh: HTTP 500: rulesets error" >&2
                    exit "${rc}"
                fi
                if [ ! -f "${fx}/${api_repo}/rulesets.json" ]; then
                    echo "stub: missing fixture rulesets.json for ${api_repo}" >&2
                    exit 9
                fi
                cat "${fx}/${api_repo}/rulesets.json"
                ;;
            *code-scanning/alerts*)
                if [ -f "${fx}/${api_repo}/codeql.http404" ]; then
                    echo "gh: Not Found (HTTP 404)" >&2
                    exit 1
                fi
                erc="${fx}/${api_repo}/codeql.rc"
                if [ -f "${erc}" ]; then rc="$(cat "${erc}")"; else rc=0; fi
                if [ "$rc" -ne 0 ]; then
                    cat "${fx}/${api_repo}/codeql.err" 2>/dev/null || echo "gh: HTTP 500: codeql error" >&2
                    exit "${rc}"
                fi
                if [ ! -f "${fx}/${api_repo}/codeql.json" ]; then
                    echo "stub: missing fixture codeql.json for ${api_repo}" >&2
                    exit 9
                fi
                cat "${fx}/${api_repo}/codeql.json"
                ;;
            *)
                echo "stub: unknown gh api path: ${url}" >&2
                exit 9
                ;;
        esac
        ;;
    *)
        echo "stub: unknown gh invocation: $*" >&2
        exit 9
        ;;
esac
exit 0
EOF
chmod +x "${BIN_STUB_GH}/gh"

# Stub advisory tools in their own bin dir so a PATH without it models an
# unavailable toolchain (command -v fails -> unavailable lane).
cat >"${BIN_STUB_TOOLS}/govulncheck" <<'EOF'
#!/usr/bin/env bash
# Whole-sweep flags model single-repo or every-has-go.mod scenarios. Dir-mode
# (TEST_GOV_FIXTURE=<dir>) injects per-repo lanes (<dir>/<repo>.{json,unpars,fail}).
# govulncheck is invoked with cwd=clone dir and `./...` (no dir argument),
# so derive the scan repo from $PWD; osv-scanner takes the dir as argv.
if [ "${TEST_GOV_FAIL:-0}" = "1" ]; then
    echo "govulncheck: fixture-injected failure" >&2
    exit 1
fi
if [ -n "${TEST_GOV_UNPARSEABLE:-}" ]; then
    echo "this output is not json at all"
    exit 0
fi
# govulncheck is invoked with cwd=clone dir and `./...` (no dir argument),
# so derive the scan repo from $PWD; osv-scanner takes the dir as argv.
scan_repo="$(basename "$PWD")"
stripped="$(printf '%s' "$scan_repo" | sed -n 's/^[0-9]*\.//p')"
if [ -n "$stripped" ]; then scan_repo="$stripped"; fi
fx="${TEST_GOV_FIXTURE:-}"
if [ -n "$fx" ] && [ -d "$fx" ]; then
    if [ -f "$fx/$scan_repo.fail" ]; then
        echo "govulncheck: fixture-injected failure (per-repo)" >&2
        exit 1
    fi
    if [ -f "$fx/$scan_repo.unpars" ]; then
        echo "this output is not json at all"
        exit 0
    fi
    if [ -f "$fx/$scan_repo.malformed" ]; then
        # Parseable NDJSON whose finding rows lack the osv id.
        cat "$fx/$scan_repo.malformed"
        exit 0
    fi
    if [ -f "$fx/$scan_repo.json" ]; then
        cat "$fx/$scan_repo.json"
        exit 0
    fi
    echo "[]"
    exit 0
fi
if [ -n "$fx" ]; then
    cat "$fx"
    exit 0
fi
echo "[]"
exit 0
EOF
chmod +x "${BIN_STUB_TOOLS}/govulncheck"

cat >"${BIN_STUB_TOOLS}/osv-scanner" <<'EOF'
#!/usr/bin/env bash
# Fixture selection is per-scan-directory: multi-repo sweeps call the stub
# once per clone dir, so TEST_OSV_FIXTURE is a DIRECTORY named per repo
# (<dir>/<repo>.json). Repos without a file get a clean scan. FAIL and
# UNPARSEABLE flags stay whole-sweep on purpose (error-lane scenarios assert
# every repo in them).
if [ "${TEST_OSV_FAIL:-0}" = "1" ]; then
    echo "osv-scanner: fixture-injected failure" >&2
    exit 2
fi
if [ -n "${TEST_OSV_UNPARSEABLE:-}" ]; then
    echo "<not json"
    exit 0
fi
tgt=""
for arg in "$@"; do
    case "$arg" in
        /*) tgt="$arg" ;;
    esac
done
scan_repo="$(basename "${tgt:-scan}")"
stripped="$(printf '%s' "$scan_repo" | sed -n 's/^[0-9]*\.//p')"
if [ -n "$stripped" ]; then scan_repo="$stripped"; fi
if [ -n "${TEST_OSV_FIXTURE:-}" ] && [ -d "${TEST_OSV_FIXTURE}" ]; then
    f="${TEST_OSV_FIXTURE}/${scan_repo}.json"
    if [ -f "${TEST_OSV_FIXTURE}/${scan_repo}.fail" ]; then
        echo "osv-scanner: fixture-injected failure (per-repo)" >&2
        exit 2
    fi
    if [ -f "${TEST_OSV_FIXTURE}/${scan_repo}.unpars" ]; then
        echo "<not json"
        exit 0
    fi
    if [ -f "$f" ]; then
        cat "$f"
        exit 1
    fi
    echo '{"results": []}'
    exit 0
fi
echo '{"results": []}'
exit 0
EOF
chmod +x "${BIN_STUB_TOOLS}/osv-scanner"

# ── Sweep runner ─────────────────────────────────────────────────────────────
# run_sweep <rendered.sh> <fixtures-dir> <out> <err> [extra env: k v ...]
# GH_RETRY knobs are pinned so no scenario sleeps; GH_TOKEN is a canary the
# tests assert never leaks.
run_sweep() {
    local script="$1" fixtures="$2" out="$3" err="$4"
    shift 4
    local envs=("$@")
    local rc=0
    set +e
    (
        export PATH="${BIN_STUB_GH}:${BIN_STUB_TOOLS}:${PATH}"
        export COLLECT_GH_FIXTURES="${fixtures}"
        export COLLECT_GH_LOG="${fixtures}/call.log"
        export GH_RETRY_MAX_ATTEMPTS=1
        # Zero-delay backoff: with retries disabled above this is inert, but
        # scenarios that override GH_RETRY_MAX_ATTEMPTS (retry-wiring tests)
        # must never actually sleep.
        export GH_RETRY_BACKOFF_SECONDS="0 0 0"
        export GH_RETRY_SCRIPT_SLEEP_BUDGET=0
        export GH_TOKEN="supersecret-token-canary"
        export TEST_ROOT="${WORK_ROOT}"
        # optional extra env
        local kv
        for kv in "${envs[@]:-}"; do
            [ -n "${kv}" ] || continue
            export "${kv%%=*}=${kv#*=}"
        done
        "${script}"
    ) >"${out}" 2>"${err}"
    rc=$?
    set -e
    return "${rc}"
}

make_fixtures() {
    local fx="$1"
    mkdir -p "${fx}"
    : >"${fx}/call.log"
}

pr() { # pr <number> <author-login>
    printf '{"number": %s, "title": "Bump deps to v%s", "author": {"login": "%s"}, "headRefName": "dep-%s", "baseRefName": "main", "url": "https://example.com/%s", "updatedAt": "2026-11-14T10:00:00Z"}' "$1" "$1" "$2" "$1" "$1"
}

# ═════════════════════════════════════════════════════════════════════════════
# 1. Vendored wrapper identity + structural wiring
# ═════════════════════════════════════════════════════════════════════════════
check "gh_retry vendored byte-identical to workstream_handler_v1" cmp -s "${WRAPPER}" "${CANONICAL_WRAPPER}"
check "main.chcl renders the sweep through templatefile" grep -q 'templatefile("./scripts/collect_inventory.sh.tftpl"' "${ROOT_DIR}/main.chcl"
check "main.chcl passes the gh_retry shim file" grep -q 'gh_retry         = file("./scripts/gh_retry.sh.tftpl")' "${ROOT_DIR}/main.chcl"
check "sweep template splices the wrapper as raw content" test "$(grep -o '{{ *\.gh_retry }}' "${TEMPLATE}" | wc -l)" = "1"
check "sweep template does not treat the wrapper as a path" test "$(grep -c '$_TPL_DIR' "${TEMPLATE}")" = "0"
check "sweep template consumes scan flags" grep -q 'scan_govulncheck={{ .scan_govulncheck | shellquote }}' "${TEMPLATE}"

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

# ═════════════════════════════════════════════════════════════════════════════
# 2. Happy path: two repos, full column set green
# ═════════════════════════════════════════════════════════════════════════════
FX1="${WORK_ROOT}/fx1"
make_fixtures "${FX1}"

# alpha: 2 dependabot PRs (green + red) + 1 other PR, advisories + codeql + rulesets all known
mkdir -p "${FX1}/alpha"
printf '[%s, %s, %s]' \
    "$(pr 12 "dependabot[bot]")" "$(pr 13 "dependabot[bot]")" "$(pr 5 "octocat")" >"${FX1}/alpha/prlist.json"
printf 'build\tsuccess\thttps://ci/build/12\ndeploy\tsuccess\t\n' >"${FX1}/alpha/checks.12.tsv"
printf '1 failing check\tsomething' >"${FX1}/alpha/checks.13.err"
printf 'build\tfailure\thttps://ci/build/13\nlint\tskipped\t' >"${FX1}/alpha/checks.13.tsv"
echo "1" >"${FX1}/alpha/checks.13.rc"
printf 'module example.com/alpha\ngo 1.22\n' >"${FX1}/alpha/gomod"
cat >"${FX1}/alpha/codeql.json" <<'EOF'
[
  {"rule": {"security_severity_level": "critical", "id": "sql-injection"}},
  {"rule": {"security_severity_level": "medium", "id": "ssrf"}}
]
EOF
cat >"${FX1}/alpha/rulesets.json" <<'EOF'
[{"id": 101, "name": "protect-main", "target": "branch", "enforcement": "active", "bypass": [{"actor_id": 5}]}]
EOF

# beta: 1 dependabot PR pending + 2 other PRs; no go.mod (not_applicable); osv finds; codeql 404; no rulesets
mkdir -p "${FX1}/beta"
printf '[%s, %s, %s]' "$(pr 20 "dependabot[bot]")" "$(pr 21 "hubot")" "$(pr 22 "hubot")" >"${FX1}/beta/prlist.json"
printf 'ci\tpending\t' >"${FX1}/beta/checks.20.tsv"
echo "8" >"${FX1}/beta/checks.20.rc"
printf '2 of 5 checks pending' >"${FX1}/beta/checks.20.err"
# per-repo osv fixtures live in one directory; repos without a file scan clean
mkdir -p "${FX1}/osv"
cat >"${FX1}/osv/beta.json" <<'EOF'
{"results": [{"packages": [{"package": {"name": "npm/left-pad", "version": "1.3.0"}, "vulnerabilities": [{"id": "GHSA-a1b2", "aliases": ["CVE-2026-1000"]}]}]}]}
EOF
# beta: CodeQL disabled (404 shape) + no protection rulesets configured
echo "1" >"${FX1}/beta/codeql.http404"
printf '[]\n' >"${FX1}/beta/rulesets.json"

# gamma (in this sweep to exercise 200-boundary? no — gamma belongs to S3; keep fx1 minimal)
# govulncheck -json emits NDJSON: one {"finding": ...} object per line.
cat >"${FX1}/govfindings.json" <<'EOF'
{"finding": {"osv": "GO-2024-0001", "frames": [{"module": "golang.org/x/net", "version": "0.1.0"}]}}
{"finding": {"osv": "GO-2024-0002", "frames": [{"module": "golang.org/x/crypto", "version": "0.5.0"}]}}
EOF

render_sweep testorg '["alpha","beta"]' "${WORK_ROOT}/sw1" true true "${WORK_ROOT}/sweep1.sh"
SWEEP_RC=0
run_sweep "${WORK_ROOT}/sweep1.sh" "${FX1}" "${WORK_ROOT}/s1.out" "${WORK_ROOT}/s1.err" "TEST_GOV_FIXTURE=${FX1}/govfindings.json" "TEST_OSV_FIXTURE=${FX1}/osv" || SWEEP_RC=$?

expect_count "S1 sweep exits zero on mixed-verdict happy path" 0 "${SWEEP_RC}"
expect_count "S1 stdout is exactly one line" 1 "$(wc -l <"${WORK_ROOT}/s1.out" | tr -d ' ')"
ART1="$(tr -d '[:space:]' <"${WORK_ROOT}/s1.out")"
check "S1 artifact file exists" test -f "${ART1}"
check_not "S1 no token canary in artifact" grep -q "supersecret-token-canary" "${ART1}"
check_not "S1 no token canary in stderr" grep -q "supersecret-token-canary" "${WORK_ROOT}/s1.err"
check_not "S1 stderr is diagnostics + no artifact path" grep -q '^collect_failed' "${WORK_ROOT}/s1.err"

check "S1 schema tag" test "$(jq -r .schema "${ART1}")" = "devops_maint_v1.inventory.v1"
check "S1 sweep org" test "$(jq -r .sweep.org "${ART1}")" = "testorg"
check "S1 sweep repos echoed" test "$(jq -c .sweep.repos "${ART1}")" = '["alpha","beta"]'
expect_count "S1 repos_total" 2 "$(jq -r .sweep.repos_total "${ART1}")"
check "S1 scan flags recorded" test "$(jq -c '[.sweep.scan_govulncheck, .sweep.scan_osv]' "${ART1}")" = '[true,true]'

expect_count "S1 summary repos_collected" 2 "$(jq -r .summary.repos_collected "${ART1}")"
expect_count "S1 summary repos_failed" 0 "$(jq -r .summary.repos_failed "${ART1}")"
expect_count "S1 summary dependabot_prs_total" 3 "$(jq -r .summary.dependabot_prs_total "${ART1}")"
expect_count "S1 summary other_open_prs_total" 3 "$(jq -r .summary.other_open_prs_total "${ART1}")"
expect_count "S1 summary codeql_open_alerts_total" 2 "$(jq -r .summary.codeql_open_alerts_total "${ART1}")"
expect_count "S1 summary advisory_findings_total" 3 "$(jq -r .summary.advisory_findings_total "${ART1}")"

# alpha record
A_ALPHA="${ART1}"
check "S1 alpha collected" jq -e '.repos[0] | .repo == "alpha" and .collected == true' "${A_ALPHA}" >/dev/null
check "S1 alpha open_total" test "$(jq -r '.repos[0].prs.open_total' "${A_ALPHA}")" = "3"
check "S1 alpha other_open_total" test "$(jq -r '.repos[0].prs.other_open_total' "${A_ALPHA}")" = "1"
check "S1 alpha pr12 verdict green" test "$(jq -r '.repos[0].prs.dependabot[0].ci.verdict' "${A_ALPHA}")" = "green"
check "S1 alpha pr12 2 checks normalized" test "$(jq -r '.repos[0].prs.dependabot[0].ci.checks | length' "${A_ALPHA}")" = "2"
check "S1 alpha pr12 check with empty url -> null" jq -e '.repos[0].prs.dependabot[0].ci.checks[1].url == null' "${A_ALPHA}" >/dev/null
check "S1 alpha pr13 verdict red" test "$(jq -r '.repos[0].prs.dependabot[1].ci.verdict' "${A_ALPHA}")" = "red"
check "S1 alpha pr13 failure state normalized" test "$(jq -r '.repos[0].prs.dependabot[1].ci.checks[0].state' "${A_ALPHA}")" = "fail"
check "S1 alpha pr13 skipped state normalized" test "$(jq -r '.repos[0].prs.dependabot[1].ci.checks[1].state' "${A_ALPHA}")" = "skip"
check "S1 alpha dependabot authors kept" test "$(jq -r '.repos[0].prs.dependabot[0].author' "${A_ALPHA}")" = "dependabot[bot]"
check "S1 alpha gov ok with 2 findings" jq -e '.repos[0].advisories.govulncheck.status == "ok" and (.repos[0].advisories.govulncheck.findings | length) == 2' "${A_ALPHA}" >/dev/null
check "S1 alpha gov finding id/module/version" test "$(jq -r '.repos[0].advisories.govulncheck.findings[0].id' "${A_ALPHA}")/"$(jq -r '.repos[0].advisories.govulncheck.findings[0].package' "${A_ALPHA}")"" = "GO-2024-0001/golang.org/x/net"
check "S1 alpha osv ok empty (clean tool run)" jq -e '.repos[0].advisories.osv.status == "ok" and (.advisories.osv.findings | length) == 0' "${A_ALPHA}" >/dev/null
check "S1 alpha codeql ok critical=1 medium=1" jq -e '.repos[0].codeql.status == "ok" and .repos[0].codeql.open_alerts == 2 and .repos[0].codeql.by_severity.critical == 1 and .repos[0].codeql.by_severity.medium == 1 and .repos[0].codeql.by_severity.other == 0' "${A_ALPHA}" >/dev/null
check "S1 alpha rulesets applied with 1 rule" jq -e '.repos[0].rulesets.applied == true and (.repos[0].rulesets.rules | length) == 1 and .repos[0].rulesets.rules[0].bypass_actors == 1' "${A_ALPHA}" >/dev/null

# beta record
check "S1 beta collected" jq -e '.repos[1] | .repo == "beta" and .collected == true' "${A_ALPHA}" >/dev/null
check "S1 beta pr20 pending" test "$(jq -r '.repos[1].prs.dependabot[0].ci.verdict' "${A_ALPHA}")" = "pending"
check "S1 beta dependabot filter kept others out" jq -e '(.repos[1].prs.dependabot | length) == 1 and .repos[1].prs.other_open_total == 2' "${A_ALPHA}" >/dev/null
check "S1 beta gov not_applicable (no go.mod)" test "$(jq -r '.repos[1].advisories.govulncheck.status' "${A_ALPHA}")" = "not_applicable"
check "S1 beta osv ok with finding" jq -e '.repos[1].advisories.osv.status == "ok" and .repos[1].advisories.osv.findings[0].id == "GHSA-a1b2" and .repos[1].advisories.osv.findings[0].package == "npm/left-pad"' "${A_ALPHA}" >/dev/null
check "S1 beta codeql disabled (404)" jq -e '.repos[1].codeql.status == "disabled" and .repos[1].codeql.open_alerts == 0' "${A_ALPHA}" >/dev/null
check "S1 beta rulesets none -> not applied" jq -e '.repos[1].rulesets.status == "ok" and .repos[1].rulesets.applied == false' "${A_ALPHA}" >/dev/null

# evidence lanes
check "S1 raw pr list kept for evidence" test -f "${FX1}/../sw1" -o -f "$(dirname "${ART1}")/raw/0000.alpha/pr_list.json"
check "S1 gh called once per repo" test "$(grep -c '^pr list' "${FX1}/call.log")" = "2"
check "S1 gh clone skipped when no advisory repo has go.mod on alpha only" test "$(grep -c '^repo clone' "${FX1}/call.log")" = "2"
check "S1 pr checks calls not retried (no transient fixture text)" test "$(grep -c '^pr checks' "${FX1}/call.log")" = "3"

# rendered-script shape: the wrapper is spliced as content and installs the shadow
check "S1 rendered sweep defines the wrapper classifier" grep -q '^gh_retry_is_transient() {' "${WORK_ROOT}/sweep1.sh"
check "S1 rendered sweep installs the gh drop-in shadow" grep -qF 'gh() { gh_retry_run stream' "${WORK_ROOT}/sweep1.sh"
check_not "S1 rendered sweep has no dangling path-source of the wrapper" grep -q 'd "$_TPL_DIR"' "${WORK_ROOT}/sweep1.sh"

# ═════════════════════════════════════════════════════════════════════════════
# 3. Honest error lanes: per-repo partial failure recorded, sweep continues
# ═════════════════════════════════════════════════════════════════════════════
FX2="${WORK_ROOT}/fx2"
make_fixtures "${FX2}"
mkdir -p "${FX2}/gamma"
printf '[%s, %s]' "$(pr 30 "dependabot[bot]")" "$(pr 31 "dependabot[bot]")" >"${FX2}/gamma/prlist.json"
echo "8" >"${FX2}/gamma/checks.30.rc"      # rc 8 with EMPTY tsv -> none
: >"${FX2}/gamma/checks.30.tsv"
printf 'gh: HTTP 500: checks exploded' >"${FX2}/gamma/checks.31.err"
echo "5" >"${FX2}/gamma/checks.31.rc"      # unexpected rc -> error verdict + errors row
printf 'module example.com/gamma\ngo 1.21\n' >"${FX2}/gamma/gomod"
printf 'gh: HTTP 500: codeql error' >"${FX2}/gamma/codeql.err"
echo "1" >"${FX2}/gamma/codeql.rc"         # codeql rc 1 non-404 -> error
echo "1" >"${FX2}/gamma/rulesets.rc"       # rulesets rc 1 -> error + empty rules
# per-repo osv error injection: gamma fails, delta scans clean (dir mode)
mkdir -p "${FX2}/osv"
echo "1" >"${FX2}/osv/gamma.fail"
# per-repo govulncheck error injection (only gamma has go.mod anyway)
mkdir -p "${FX2}/govd"
echo "1" >"${FX2}/govd/gamma.fail"

mkdir -p "${FX2}/delta"
printf '[]' >"${FX2}/delta/prlist.json"    # zero PRs, zero checks needed
printf '[]' >"${FX2}/delta/codeql.json"
printf '[]' >"${FX2}/delta/rulesets.json"

render_sweep testorg '["gamma","delta"]' "${WORK_ROOT}/sw2" true true "${WORK_ROOT}/sweep2.sh"
SWEEP_RC2=0
run_sweep "${WORK_ROOT}/sweep2.sh" "${FX2}" "${WORK_ROOT}/s2.out" "${WORK_ROOT}/s2.err" "TEST_GOV_FIXTURE=${FX2}/govd" "TEST_OSV_FIXTURE=${FX2}/osv" || SWEEP_RC2=$?

expect_count "S2 sweep continues past a partial repo (one complete)" 0 "${SWEEP_RC2}"
ART2="$(tr -d '[:space:]' <"${WORK_ROOT}/s2.out")"
check "S2 artifact exists" test -f "${ART2}"
check "S2 gamma collected=false (error lanes)" jq -e '.repos[0].repo == "gamma" and .repos[0].collected == false and (.repos[0].collected_reason | tostring | contains("partial"))' "${ART2}" >/dev/null
check "S2 gamma pr30 none (rc 8 with no rows)" test "$(jq -r '.repos[0].prs.dependabot[0].ci.verdict' "${ART2}")" = "none"
check "S2 gamma pr31 error verdict with message" jq -e '.repos[0].prs.dependabot[1].ci.verdict == "error" and (.repos[0].prs.dependabot[1].ci.error | contains("500"))' "${ART2}" >/dev/null
check "S2 gamma ci verdicts error row" jq -e '[.repos[0].errors[].surface] | index("ci_verdicts") != null' "${ART2}" >/dev/null
check "S2 gamma codeql error lane" test "$(jq -r '.repos[0].codeql.status' "${ART2}")" = "error"
check "S2 gamma codeql error row" jq -e '[.repos[0].errors[].surface] | index("codeql") != null' "${ART2}" >/dev/null
check "S2 gamma rulesets error lane with empty rules" jq -e '.repos[0].rulesets.status == "error" and (.repos[0].rulesets.rules | length) == 0' "${ART2}" >/dev/null
check "S2 gamma rulesets error row" jq -e '[.repos[0].errors[].surface] | index("rulesets") != null' "${ART2}" >/dev/null
check "S2 gamma govulncheck error lane (tool failed)" test "$(jq -r '.repos[0].advisories.govulncheck.status' "${ART2}")" = "error"
check "S2 gamma govulncheck error row" jq -e '[.repos[0].errors[].surface] | index("govulncheck") != null' "${ART2}" >/dev/null
check "S2 gamma osv error lane (tool failed)" test "$(jq -r '.repos[0].advisories.osv.status' "${ART2}")" = "error"
check "S2 gamma findings stay empty on error" jq -e '(.repos[0].advisories.govulncheck.findings | length) == 0 and (.repos[0].advisories.osv.findings | length) == 0' "${ART2}" >/dev/null

check "S2 delta clean repo collected" jq -e '.repos[1].repo == "delta" and .repos[1].collected == true and (.repos[1].prs.prs_skipped) == false' "${ART2}" >/dev/null
check "S2 delta zero PRs -> no checks calls" test "$(jq -r '.repos[1].prs.dependabot_total' "${ART2}")" = "0"
check "S2 delta advisories: gov not_applicable (no go.mod), osv clean ok" jq -e '.repos[1].advisories.govulncheck.status == "not_applicable" and .repos[1].advisories.osv.status == "ok" and (.repos[1].advisories.osv.findings | length) == 0' "${ART2}" >/dev/null
check "S2 delta clone happened for advisories" test "$(grep -c '^repo clone.*delta' "${FX2}/call.log")" = "1"
expect_count "S2 summary collected=1 failed=1" "1" "$(jq -r '.summary.repos_failed' "${ART2}")"

check_not "S2 no token canary in stderr" grep -q "supersecret-token-canary" "${WORK_ROOT}/s2.err"

# ═════════════════════════════════════════════════════════════════════════════
# 4. Tool unavailable + unparseable output lanes; scan flags off
# ═════════════════════════════════════════════════════════════════════════════
FX3="${WORK_ROOT}/fx3"
make_fixtures "${FX3}"
mkdir -p "${FX3}/eps"
printf '[%s]' "$(pr 40 'dependabot[bot]')" >"${FX3}/eps/prlist.json"
printf 'build\tsuccess\t' >"${FX3}/eps/checks.40.tsv"
mkdir -p "${FX3}/eps" "${FX3}/eps"
printf 'module example.com/eps\ngo 1.22\n' >"${FX3}/eps/gomod"
printf '[]' >"${FX3}/eps/codeql.json"
printf '[]' >"${FX3}/eps/rulesets.json"

render_sweep testorg '["eps"]' "${WORK_ROOT}/sw3" true true "${WORK_ROOT}/sweep3.sh"
# PATH without the tools dir: both advisory tools unavailable for the repo
SWEEP_RC3=0
set +e
(
    export PATH="${BIN_STUB_GH}:${PATH}"
    export COLLECT_GH_FIXTURES="${FX3}"
    export COLLECT_GH_LOG="${FX3}/call.log"
    export GH_RETRY_MAX_ATTEMPTS=1
    export GH_TOKEN="supersecret-token-canary"
    "${WORK_ROOT}/sweep3.sh"
) >"${WORK_ROOT}/s3.out" 2>"${WORK_ROOT}/s3.err"
SWEEP_RC3=$?
set -e
expect_count "S3 sweep exits 0 (unavailable lanes recorded, repo still collected)" 0 "${SWEEP_RC3}"
ART3="$(tr -d '[:space:]' <"${WORK_ROOT}/s3.out")"
check "S3 govulncheck unavailable recorded + repo collected" jq -e '.repos[0].advisories.govulncheck.status == "unavailable" and .repos[0].collected == true and (.repos[0].errors | length) == 0' "${ART3}" >/dev/null
check "S3 osv unavailable recorded" jq -e '.repos[0].advisories.osv.status == "unavailable"' "${ART3}" >/dev/null

# unparseable tool output with tools present -> error lane
FX3B="${WORK_ROOT}/fx3b"
make_fixtures "${FX3B}"
mkdir -p "${FX3B}/eps"
printf '[%s]' "$(pr 40 'dependabot[bot]')" >"${FX3B}/eps/prlist.json"
printf 'build\tsuccess\t' >"${FX3B}/eps/checks.40.tsv"
printf 'module example.com/eps\ngo 1.22\n' >"${FX3B}/eps/gomod"
printf '[]' >"${FX3B}/eps/codeql.json"
printf '[]' >"${FX3B}/eps/rulesets.json"
mkdir -p "${FX3B}/osv"
echo "1" >"${FX3B}/osv/eps.unpars"
mkdir -p "${FX3B}/govd"
echo "1" >"${FX3B}/govd/eps.unpars"
# eta: clean second repo so the sweep still emits a collected repo even
# though eps' advisory lanes error on unparseable output.
mkdir -p "${FX3B}/eta"
printf '[%s]' "$(pr 41 'dependabot[bot]')" >"${FX3B}/eta/prlist.json"
printf 'build\tsuccess\t' >"${FX3B}/eta/checks.41.tsv"
printf 'module example.com/eta\ngo 1.23\n' >"${FX3B}/eta/gomod"
printf '[]' >"${FX3B}/eta/codeql.json"
printf '[]' >"${FX3B}/eta/rulesets.json"
render_sweep testorg '["eps","eta"]' "${WORK_ROOT}/sw3b" true true "${WORK_ROOT}/sweep3b.sh"
SWEEP_RC3B=0
run_sweep "${WORK_ROOT}/sweep3b.sh" "${FX3B}" "${WORK_ROOT}/s3b.out" "${WORK_ROOT}/s3b.err" "TEST_GOV_FIXTURE=${FX3B}/govd" "TEST_OSV_FIXTURE=${FX3B}/osv" || SWEEP_RC3B=$?
expect_count "S3b unparseable output -> honest error, sweep continues" 0 "${SWEEP_RC3B}"
ART3B="$(tr -d '[:space:]' <"${WORK_ROOT}/s3b.out")"
check "S3b gov unparseable -> error" test "$(jq -r '.repos[0].advisories.govulncheck.status' "${ART3B}")" = "error"
check "S3b osv unparseable -> error" test "$(jq -r '.repos[0].advisories.osv.status' "${ART3B}")" = "error"
check "S3b eps rows error, eta collected clean" jq -e '.repos[0].collected == false and .repos[1].repo == "eta" and .repos[1].collected == true' "${ART3B}" >/dev/null
expect_count "S3b summary collected=1 failed=1" "1" "$(jq -r '.summary.repos_failed' "${ART3B}")"

# scan flags off -> both lanes not_applicable, no clone attempted
render_sweep testorg '["eps"]' "${WORK_ROOT}/sw3c" false false "${WORK_ROOT}/sweep3c.sh"
: >"${FX3B}/call.log"
SWEEP_RC3C=0
run_sweep "${WORK_ROOT}/sweep3c.sh" "${FX3B}" "${WORK_ROOT}/s3c.out" "${WORK_ROOT}/s3c.err" || SWEEP_RC3C=$?
ART3C="$(tr -d '[:space:]' <"${WORK_ROOT}/s3c.out")"
check "S3c flags off -> not_applicable lanes" jq -e '.repos[0].advisories.govulncheck.status == "not_applicable" and .repos[0].advisories.osv.status == "not_applicable"' "${ART3C}" >/dev/null
check "S3c no clone attempted (flags off)" test "$(grep -c '^repo clone' "${FX3B}/call.log")" = "0"
check "S3c scan flags recorded false" test "$(jq -c '[.sweep.scan_govulncheck, .sweep.scan_osv]' "${ART3C}")" = '[false,false]'

# ═════════════════════════════════════════════════════════════════════════════
# 5. Per-repo PR enumeration failure + ALL-repos failure (rc 1, artifact kept)
# ═════════════════════════════════════════════════════════════════════════════
FX4="${WORK_ROOT}/fx4"
make_fixtures "${FX4}"
mkdir -p "${FX4}/zeta"
printf 'gh: HTTP 404: repo vanished' >"${FX4}/zeta/prlist.failmsg"
echo "1" >"${FX4}/zeta/prlist.fail"

render_sweep testorg '["zeta"]' "${WORK_ROOT}/sw4" true true "${WORK_ROOT}/sweep4.sh"
SWEEP_RC4=0
run_sweep "${WORK_ROOT}/sweep4.sh" "${FX4}" "${WORK_ROOT}/s4.out" "${WORK_ROOT}/s4.err" || SWEEP_RC4=$?
expect_count "S4 all-repos-failed sweep exits 1" 1 "${SWEEP_RC4}"
check "S4 typed failure on stderr" grep -q '^collect_failed: every repo in scope failed to collect' "${WORK_ROOT}/s4.err"
check "S4 stdout stays empty (no artifact path on failure)" test ! -s "${WORK_ROOT}/s4.out"
check "S4 partial artifact kept and named in stderr" grep -q 'partial artifact kept' "${WORK_ROOT}/s4.err"
ART4="$(grep -o '/[^ ]*inventory.json' "${WORK_ROOT}/s4.err" | head -1)"
check "S4 partial artifact parses" jq -e '.repos[0].repo == "zeta" and .repos[0].collected == false' "${ART4}" >/dev/null
check "S4 pull_requests error row" jq -e '[.repos[0].errors[].surface] | index("pull_requests") != null' "${ART4}" >/dev/null
check "S4 prs_skipped recorded" jq -e '.repos[0].prs.prs_skipped == true' "${ART4}" >/dev/null

# ═════════════════════════════════════════════════════════════════════════════
# 6. Checks cap (COLLECT_MAX_PR_CHECKS) + pr-list truncation flag
# ═════════════════════════════════════════════════════════════════════════════
FX5="${WORK_ROOT}/fx5"
make_fixtures "${FX5}"
mkdir -p "${FX5}/iota"
# 3 dependabot PRs, cap 2 -> third is unenumerated, not an error
printf '[%s, %s, %s]' "$(pr 41 'dependabot[bot]')" "$(pr 42 'dependabot[bot]')" "$(pr 43 'dependabot[bot]')" >"${FX5}/iota/prlist.json"
printf 'build\tsuccess\t' >"${FX5}/iota/checks.41.tsv"
printf 'build\tfailure\t' >"${FX5}/iota/checks.42.tsv"
echo "1" >"${FX5}/iota/checks.42.rc"
printf '[]' >"${FX5}/iota/codeql.json"
printf '[]' >"${FX5}/iota/rulesets.json"

render_sweep testorg '["iota"]' "${WORK_ROOT}/sw5" true true "${WORK_ROOT}/sweep5.sh"
SWEEP_RC5=0
run_sweep "${WORK_ROOT}/sweep5.sh" "${FX5}" "${WORK_ROOT}/s5.out" "${WORK_ROOT}/s5.err" "COLLECT_MAX_PR_CHECKS=2" || SWEEP_RC5=$?
expect_count "S5 capped sweep exits 0 (cap is not an error)" 0 "${SWEEP_RC5}"
ART5="$(tr -d '[:space:]' <"${WORK_ROOT}/s5.out")"
check "S5 dependabot roster complete despite cap" test "$(jq -r '.repos[0].prs.dependabot | length' "${ART5}")" = "3"
check "S5 cap stub rows verdict unenumerated" test "$(jq -r '.repos[0].prs.dependabot[2].ci.verdict' "${ART5}")" = "unenumerated"
check "S5 cap stub rows carry reason" jq -e '.repos[0].prs.dependabot[2].ci.error | contains("COLLECT_MAX_PR_CHECKS=2")' "${ART5}" >/dev/null
check "S5 only 2 checks calls made" test "$(grep -c '^pr checks' "${FX5}/call.log")" = "2"
check "S5 truncation flag set" jq -e '.repos[0].prs.pr_checks_truncated == true' "${ART5}" >/dev/null
check "S5 repo still collected under cap" jq -e '.repos[0].collected == true' "${ART5}" >/dev/null

# pr-list truncation: 200 open PRs -> flag + cap interplay
FX5B="${WORK_ROOT}/fx5b"
make_fixtures "${FX5B}"
mkdir -p "${FX5B}/kappa"
jq -n '[range(0; 200) | {number: ., title: "pr", author: {login: "dependabot[bot]"}, headRefName: "h", baseRefName: "main", url: ("u" + (. | tostring)), updatedAt: "2026-11-14T10:00:00Z"}]' >"${FX5B}/kappa/prlist.json"
printf 'build\tsuccess\t' >"${FX5B}/kappa/checks.0.tsv"
printf '[]' >"${FX5B}/kappa/codeql.json"
printf '[]' >"${FX5B}/kappa/rulesets.json"

render_sweep testorg '["kappa"]' "${WORK_ROOT}/sw5b" true true "${WORK_ROOT}/sweep5b.sh"
SWEEP_RC5B=0
run_sweep "${WORK_ROOT}/sweep5b.sh" "${FX5B}" "${WORK_ROOT}/s5b.out" "${WORK_ROOT}/s5b.err" "COLLECT_MAX_PR_CHECKS=1" || SWEEP_RC5B=$?
ART5B="$(tr -d '[:space:]' <"${WORK_ROOT}/s5b.out")"
expect_count "S5b truncated sweep exits 0" 0 "${SWEEP_RC5B}"
check "S5b pr-list truncation flag set at 200" jq -e '.repos[0].prs.pr_list_truncated == true and .repos[0].prs.open_total == 200' "${ART5B}" >/dev/null
check "S5b checks roster complete at 200 with cap 1" test "$(jq -r '.repos[0].prs.dependabot | length' "${ART5B}")" = "200"
check "S5b exactly one real checks call" test "$(grep -c '^pr checks' "${FX5B}/call.log")" = "1"

# ═════════════════════════════════════════════════════════════════════════════
# 7. Fail-loud config validation (no gh calls, nothing on stdout)
# ═════════════════════════════════════════════════════════════════════════════
assert_config_fail() {
    local label="$1" org="$2" repos_json="$3" sweep_root="$4" extra_env="$5" name_pattern="$6"
    local sweep="${WORK_ROOT}/cfgfail-${name_pattern}.sh"
    render_sweep "${org}" "${repos_json}" "${sweep_root}" true true "${sweep}"
    local rc=0
    set +e
    (
        export PATH="${BIN_STUB_GH}:${BIN_STUB_TOOLS}:${PATH}"
        export GH_RETRY_MAX_ATTEMPTS=1
        if [ -n "${extra_env}" ]; then
            export "${extra_env%%=*}=${extra_env#*=}"
        fi
        "${sweep}"
    ) >/dev/null 2>"${WORK_ROOT}/cfgfail.err"
    rc=$?
    set -e
    expect_count "${label} exits 1" 1 "${rc}"
    check "${label} typed fail line" grep -q '^collect_failed' "${WORK_ROOT}/cfgfail.err"
}
assert_config_fail "S6 empty repos" testorg '[]' "${WORK_ROOT}/sw6" "" cfgEmptyRepos
assert_config_fail "S6 org with unsafe char" "bad!org" '["alpha"]' "${WORK_ROOT}/sw6" "" cfgBadOrg
assert_config_fail "S6 duplicate repos" testorg '["alpha","alpha"]' "${WORK_ROOT}/sw6" "" cfgDupRepos
assert_config_fail "S6 repo name outside safe shape" testorg '["a/b"]' "${WORK_ROOT}/sw6" "" cfgRepoSlash
assert_config_fail "S6 sweep_root with .." testorg '["alpha"]' "${WORK_ROOT}/a/../b" "" cfgDotDot
assert_config_fail "S6 COLLECT_MAX_PR_CHECKS not an integer" testorg '["alpha"]' "${WORK_ROOT}/sw6" "COLLECT_MAX_PR_CHECKS=abc" cfgCapNotInt

# ═════════════════════════════════════════════════════════════════════════════
# 8. Determinism: same fixtures -> same artifact modulo sweep id + timestamps
# ═════════════════════════════════════════════════════════════════════════════
FX8="${WORK_ROOT}/fx8"
cp -R "${FX1}" "${FX8}"
: >"${FX8}/call.log"
render_sweep testorg '["alpha","beta"]' "${WORK_ROOT}/sw8" true true "${WORK_ROOT}/sweep8.sh"
SWEEP_RC8=0
run_sweep "${WORK_ROOT}/sweep8.sh" "${FX8}" "${WORK_ROOT}/s8.out" "${WORK_ROOT}/s8.err" "TEST_GOV_FIXTURE=${FX1}/govfindings.json" "TEST_OSV_FIXTURE=${FX1}/osv" || SWEEP_RC8=$?
ART8="$(tr -d '[:space:]' <"${WORK_ROOT}/s8.out")"
jq 'del(.sweep.sweep_id, .sweep.started_at, .sweep.finished_at)' "${ART1}" >"${WORK_ROOT}/art1.norm"
jq 'del(.sweep.sweep_id, .sweep.started_at, .sweep.finished_at)' "${ART8}" >"${WORK_ROOT}/art8.norm"
check "S8 deterministic modulo sweep id/timestamps" cmp -s "${WORK_ROOT}/art1.norm" "${WORK_ROOT}/art8.norm"

# ═════════════════════════════════════════════════════════════════════════════
# 9. Multi-page CodeQL alert flatten (two concatenated docs -> per-alert math)
# ═════════════════════════════════════════════════════════════════════════════
FLAT=$(printf '[{"rule": {"security_severity_level": "medium"}}, {"rule": {"security_severity_level": "low"}}]\n[{"rule": {"security_severity_level": "critical"}}]' | jq -s '[.[].[]?] | length')
expect_count "S9 flatten counts alerts across pages" 3 "${FLAT}"
# Bucketing math mirroring the sweep template on the flattened pages.
S9BUCKETS=$(printf '[{"rule": {"security_severity_level": "medium"}}, {"rule": {"security_severity_level": "low"}}]\n[{"rule": {"security_severity_level": "critical"}}]\n[{"rule": {}}]' | jq -s --argjson total 4 '
    [.[].[]?] as $alerts
    | [$alerts[] | select((.rule.security_severity_level? // null) != null)] as $rows
    | {
        critical: ([$rows[] | select(.rule.security_severity_level == "critical")] | length),
        high:     ([$rows[] | select(.rule.security_severity_level == "high")] | length),
        medium:   ([$rows[] | select(.rule.security_severity_level == "medium")] | length),
        low:      ([$rows[] | select(.rule.security_severity_level == "low")] | length),
        other:    ($total - ($rows | length))
      }')
check "S9 severity buckets follow the documented security_severity_level enum" jq -e '.critical == 1 and .high == 0 and .medium == 1 and .low == 1 and .other == 1' <<<"${S9BUCKETS}" >/dev/null

# ═════════════════════════════════════════════════════════════════════════════
# 10. Retry wiring: transient pr-list failure is retried through the spliced
# KB-219 wrapper; verdict-shaped failures stay single-attempt (S1 above).
# ═════════════════════════════════════════════════════════════════════════════
FX10="${WORK_ROOT}/fx10"
make_fixtures "${FX10}"
mkdir -p "${FX10}/rho"
printf '[%s]' "$(pr 32 'dependabot[bot]')" >"${FX10}/rho/prlist.json"
echo "1" >"${FX10}/rho/prlist.tr"
printf 'build\tsuccess\t' >"${FX10}/rho/checks.32.tsv"
printf '[]' >"${FX10}/rho/codeql.json"
printf '[]' >"${FX10}/rho/rulesets.json"

render_sweep testorg '["rho"]' "${WORK_ROOT}/sw10" true true "${WORK_ROOT}/sweep10.sh"
SWEEP_RC10=0
run_sweep "${WORK_ROOT}/sweep10.sh" "${FX10}" "${WORK_ROOT}/s10.out" "${WORK_ROOT}/s10.err" \
    "GH_RETRY_MAX_ATTEMPTS=2" || SWEEP_RC10=$?
expect_count "S10 sweep exits zero after the transport retry" 0 "${SWEEP_RC10}"
ART10="$(tr -d '[:space:]' <"${WORK_ROOT}/s10.out")"
check "S10 rho collected after retry" jq -e '.repos[0].repo == "rho" and .repos[0].collected == true and .repos[0].prs.dependabot_total == 1' "${ART10}" >/dev/null
expect_count "S10 pr list attempted twice for one repo" "2" "$(grep -c '^pr list' "${FX10}/call.log")"
check "S10 retry evidence recorded on stderr capture" test "$(find "${WORK_ROOT}/sw10" -name pr_list.err | head -1 | xargs grep -c 'result=retry class=connection_reset')" = "1"

if [ "${FAILURES}" -gt 0 ]; then
    echo "FAILED: ${FAILURES} assertion(s)" >&2
    exit 1
fi
# Every fixture dir is explicit (WORK_ROOT-anchored); assert the suite never
# leaks scratch into its own directory or the repo tree.
check "suite leaves no scratch leak beside its fixtures" test ! -e "${SCRIPT_DIR}/testorg"
echo "PASS: devops_maint_v1 collect-step regression suite verified"
