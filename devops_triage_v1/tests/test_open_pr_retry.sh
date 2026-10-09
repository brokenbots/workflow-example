#!/usr/bin/env bash
# Regression test for KB-219: open_pr's GitHub API surface (gh pr view,
# gh pr create) is wrapped by the shared gh_retry shim — transient transport
# errors (TLS handshake timeout, connection reset, 5xx) are retried with
# bounded backoff, while verdict-shaped results and non-transient failures
# pass through unchanged.
#
# Scenarios:
#   - reuse: gh pr view returns an OPEN PR -> one call, URL on stdout
#   - create + TLS blips: two transient failures then success -> URL out,
#     retry evidence on stderr (attempt/class lines)
#   - create + TLS blips exhausted -> rc 1 with pr_create_failed=true
#   - create + non-transient (422) -> pass_through on first failure, one try

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
TEMPLATE="${ROOT_DIR}/scripts/open_pr.sh.tftpl"
WRAPPER="${ROOT_DIR}/scripts/gh_retry.sh.tftpl"
WORK_ROOT="$(mktemp -d)"
trap 'rm -rf "${WORK_ROOT}"' EXIT

FAILURES=0
check() {
    local label="$1"; shift
    if "$@"; then
        echo "PASS: ${label}"
    else
        echo "FAIL: ${label}"
        FAILURES=$((FAILURES + 1))
    fi
}

# Render open_pr with the wrapper spliced in raw at its marker and the
# criteria values substituted the way shellquote renders literals.
gh_retry_line="$(grep -n '{{ *\.gh_retry *}}' "${TEMPLATE}" | head -1 | cut -d: -f1)"
{
    head -n $((gh_retry_line - 1)) "${TEMPLATE}"
    cat "${WRAPPER}"
    tail -n "+$((gh_retry_line + 1))" "${TEMPLATE}"
} | sed -e 's/{{ *\.criteria_value_1 *| *shellquote *}}/kb219-branch/g' \
      -e 's/{{ *\.criteria_value_2 *| *shellquote *}}/title/g' \
      -e "s@{{ *\\.criteria_value_3 *| *shellquote *}}@${WORK_ROOT}/run@g" \
      -e 's/{{ *\.criteria_value_4 *| *shellquote *}}/main/g' \
      > "${WORK_ROOT}/open_pr.sh"
chmod +x "${WORK_ROOT}/open_pr.sh"

# Fake gh: reads a scenario policy line by call number ("<rc>|<stdout>|<stderr>")
# so transient sequences (TLS blip, then success) are expressible without
# flaky timing; the last line repeats on overflow.
mk_gh() {
    local policy="$1"
    cat > "${WORK_ROOT}/gh" <<EOF
#!/usr/bin/env bash
n=\$(cat "${WORK_ROOT}/policy.idx" 2>/dev/null || echo 0)
n=\$((n + 1))
echo "\$n" > "${WORK_ROOT}/policy.idx"
line="\$(tail -n +"\$n" "${policy}" | head -1)"
[ -n "\$line" ] || line="\$(tail -n 1 "${policy}")"
rc="\${line%%|*}"; rest="\${line#*|}"; out="\${rest%%|*}"; err="\${rest#*|}"
[ -n "\$out" ] && printf '%s\\n' "\$out"
[ -n "\$err" ] && printf '%s\\n' "\$err" >&2
exit "\$rc"
EOF
    chmod +x "${WORK_ROOT}/gh"
}

# open_pr takes criteria_value_3 = run dir for --body-file; stage a stub so
# the harness gets into the gh paths without the template's cat failing.
mkdir -p "${WORK_ROOT}/run/evidence"
echo "report" > "${WORK_ROOT}/run/evidence/report.md"

run_scenario() {
    local dir="$1"
    (
        export GH_RETRY_BACKOFF_SECONDS=0
        export PATH="${WORK_ROOT}:${PATH}"
        cd "${WORK_ROOT}/${dir}" || exit 9
        # The test script itself runs set -e; the step under test is allowed
        # to fail — capture its rc explicitly instead of inheriting it.
        set +e
        rm -f ../policy.idx
        bash "${WORK_ROOT}/open_pr.sh" >out 2>err
        echo $? >rc
    )
}

# ── 1. Reuse: existing OPEN PR — one call, verdict pass-through, no shim logs ─
mkdir -p "${WORK_ROOT}/s1"
printf '0|{"url":"https://github.com/o/r/pull/9","state":"OPEN"}|\n' > "${WORK_ROOT}/s1_policy"
mk_gh "${WORK_ROOT}/s1_policy"
run_scenario s1
check "s1 single gh call"     test "$(cat "${WORK_ROOT}/policy.idx" 2>/dev/null || echo 0)" = "1"
check "s1 URL on stdout"      grep -q 'pull/9$' "${WORK_ROOT}/s1/out"
check "s1 no shim evidence"   test ! -s "${WORK_ROOT}/s1/err"

# ── 2. View with two TLS blips then success: converges with evidence ─────────
mkdir -p "${WORK_ROOT}/s2"
printf '1||tls: handshake timeout\n1||tls: handshake timeout again\n0|{"url":"https://github.com/o/r/pull/10","state":"OPEN"}|\n' > "${WORK_ROOT}/s2_policy"
mk_gh "${WORK_ROOT}/s2_policy"
run_scenario s2
check "s2 converges rc=0"      test "$(cat "${WORK_ROOT}/s2/rc")" = "0"
check "s2 URL on stdout"       grep -q 'pull/10$' "${WORK_ROOT}/s2/out"
check "s2 two retry attempts"  test "$(grep -c 'result=retry' "${WORK_ROOT}/s2/err")" = "2"
check "s2 tls class logged"    grep -q 'class=tls_handshake_timeout' "${WORK_ROOT}/s2/err"
check "s2 success logged"      grep -q 'result=success' "${WORK_ROOT}/s2/err"

# ── 3. Persistent TLS blips on every call: exhaustion surfaces honestly ──────
mkdir -p "${WORK_ROOT}/s3"
printf '1||tls: handshake timeout forever\n' > "${WORK_ROOT}/s3_policy"
mk_gh "${WORK_ROOT}/s3_policy"
run_scenario s3
check "s3 exhausted rc=1"      test "$(cat "${WORK_ROOT}/s3/rc")" = "1"
check "s3 marker emitted"      grep -q 'pr_create_failed=true branch=kb219-branch' "${WORK_ROOT}/s3/out"
check "s3 exhaust logged"      grep -q 'result=exhausted' "${WORK_ROOT}/s3/err"
# gh_retry caps every single call: with backoff=0 neither call exhausts the
# sleep budget, so both the pr view and the create reach their full
# MAX_ATTEMPTS=4 — 8 gh invocations, bounded, then the honest rc 1.
check "s3 exhaust bounded"     test "$(cat "${WORK_ROOT}/policy.idx")" = "8"

# ── 4. Non-transient 422 on create: pass through untouched, single try ───────
mkdir -p "${WORK_ROOT}/s4"
printf '0|{"url":"https://github.com/o/r/pull/11","state":"CLOSED"}|\n422||422 Unprocessable Entity\n' > "${WORK_ROOT}/s4_policy"
mk_gh "${WORK_ROOT}/s4_policy"
run_scenario s4
check "s4 rc preserved"        test "$(cat "${WORK_ROOT}/s4/rc")" = "1"
check "s4 verdict not retried" test "$(grep -c 'result=retry' "${WORK_ROOT}/s4/err")" = "0"
check "s4 pass_through logged" grep -q 'result=pass_through class=non_transient' "${WORK_ROOT}/s4/err"

# ── 5. Structural: workflow renders open_pr with the wrapper ─────────────────
check "main.chcl renders open_pr"      grep -q 'templatefile("./scripts/open_pr.sh.tftpl"' "${ROOT_DIR}/main.chcl"
check "main.chcl passes gh_retry"      grep -q 'gh_retry         = file("./scripts/gh_retry.sh.tftpl")' "${ROOT_DIR}/main.chcl"
check "wrapper vendored"               test -f "${WRAPPER}"
check "create site uses merged mode"   grep -q 'gh_retry_merged gh pr create' "${ROOT_DIR}/scripts/open_pr.sh.tftpl"

if [ "${FAILURES}" -gt 0 ]; then
    echo "FAILED: ${FAILURES} assertion(s)" >&2
    exit 1
fi
echo "PASS: open_pr gh_retry integration verified"