#!/usr/bin/env bash
# Behavioral test for KB-219: the shared bounded-backoff retry wrapper.
#
# Backdrop (dave ruling 2026-10-08): two 60m+ run cycles were lost because
# check_pr_status and its single 10s retry both landed inside the same ~30s
# api.github.com TLS-handshake-timeout window. The wrapper is the transport
# layer under every GitHub API / gh call: bounded retries (4 attempts,
# 15/30/60s, per-call overridable) for transient network classes only — never
# for verdict-shaped results.
#
# Verified against a scenario-driven stub gh/git:
#   1. a verdict body (rc 0) passes through with exactly one call;
#   2. a transient outage window is retried and the call converges;
#   3. gh rc 8 "no checks" (pending verdict) is NOT retried;
#   4. exhaustion returns the last rc and stops calling (stub git);
#   5. terminal stderr folds back into captures that previously used 2>&1,
#      and the step's own stderr channel stays free of captured text;
#   6. the cumulative sleep budget keeps the wall clock bounded;
#   7. the drop-in `gh` shadow engages the same engine;
#   8. the shadow retries transient classes; and
#   9. a literal `gh_retry gh ...` invocation wraps exactly once.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="${SCRIPT_DIR}/.."
WORK_ROOT="$(mktemp -d)"
trap 'rm -rf "${WORK_ROOT}"' EXIT

FAILED=0
ok() { echo "PASS: $1"; }
fail() {
    FAILED=$((FAILED + 1))
    echo "FAIL: $1" >&2
}

# Scenario-driven stub: consecutive numbered lines "<rc>|<stdout>|<stderr>".
# Calls beyond the last line repeat the last line, so an "always fail"
# scenario is a one-line policy. One copy answers as gh, one as git; both
# count into the scenario's own count file.
make_stub() {
    local path=$1
    cat >"${path}" <<'STUB'
#!/bin/sh
n=$(cat "$GH_RETRY_STUB_COUNT" 2>/dev/null || echo 0)
n=$((n + 1))
echo "$n" >"$GH_RETRY_STUB_COUNT"
line=""
[ -n "${GH_RETRY_STUB_POLICY:-}" ] && line=$(sed -n "${n}p" "${GH_RETRY_STUB_POLICY}" 2>/dev/null)
[ -n "$line" ] || line=$(tail -n 1 "${GH_RETRY_STUB_POLICY}" 2>/dev/null)
[ -n "$line" ] || line="0|default-stub-ok|"
rc=$(printf '%s' "$line" | cut -d'|' -f1)
out=$(printf '%s' "$line" | cut -d'|' -f2)
err=$(printf '%s' "$line" | cut -d'|' -f3 || true)
[ -n "$out" ] && printf '%s\n' "$out"
[ -n "$err" ] && printf '%s\n' "$err" >&2
exit "$rc"
STUB
    chmod +x "${path}"
}
mkdir -p "${WORK_ROOT}/bin"
make_stub "${WORK_ROOT}/bin/gh"
make_stub "${WORK_ROOT}/bin/git"

# Policy files: each line is a scripted gh/git answer for one call.
printf '0|{"state":"APPROVED"}|\n' >"${WORK_ROOT}/p1.policy"
{
    printf '1||fatal: unable to access .api.github.com/: TLS handshake timeout\n'
    printf '1||fatal: unable to access .api.github.com/: TLS handshake timeout\n'
    printf '0|{"total_count":3}|\n'
} >"${WORK_ROOT}/p2.policy"
printf '8||no checks reported the merge commit\n' >"${WORK_ROOT}/p3.policy"
printf '1||fatal: unable to access .github.com/: Connection reset by peer\n' >"${WORK_ROOT}/p4.policy"
printf '1||fatal: no such remote|\n' >"${WORK_ROOT}/p5.policy"
printf '1||fatal: unable to access .api.github.com/: TLS handshake timeout\n' >"${WORK_ROOT}/p6.policy"
printf '0|{"state":"OPEN"}|\n' >"${WORK_ROOT}/p7.policy"
{
    printf '1||fatal: unable to access .api.github.com/: Could not resolve host\n'
    printf '0|{"state":"OPEN"}|\n'
} >"${WORK_ROOT}/p8.policy"
s9_policy="${WORK_ROOT}/p9.policy"
{
    printf '1||fatal: unable to access .api.github.com/: Connection reset by peer\n'
    printf '0|{"bucket":"success"}|\n'
} >"${s9_policy}"

# Run one scenario: sources the wrapper in a subshell with the stub PATH, then
# evaluates the body. Stdout and rc are emitted on stdout (body first, then a
# "RC=<n>" line); stderr is isolated to the scenario evidence file so the test
# can assert the run-log lines and prove nothing bleeds into stdout.
scenario() { # scenario <name> <policy path> <body>
    local name=$1 policy_path=$2 body=$3
    : >"${WORK_ROOT}/${name}.count"
    ( export PATH="${WORK_ROOT}/bin:${PATH}"
      export GH_RETRY_STUB_COUNT="${WORK_ROOT}/${name}.count"
      export GH_RETRY_STUB_POLICY="${policy_path}"
      export GH_RETRY_STUB_LOG="${WORK_ROOT}/${name}.stderr"
      set +e
      source "${ROOT_DIR}/scripts/gh_retry.sh.tftpl"
      eval "${body}" 2>"${GH_RETRY_STUB_LOG}"; echo "RC=$?" ) 2>/dev/null
}
call_count() { [ -s "${WORK_ROOT}/$1.count" ] && cat "${WORK_ROOT}/$1.count" || echo 0; }

check() { # check <desc> <expected> <actual>
    if [ "$2" = "$3" ]; then
        ok "$1"
    else
        fail "$1 (expected [$(printf '%s' "$2" | tr '\n' '~')], got [$(printf '%s' "$3" | tr '\n' '~')])"
    fi
}
err_has() { # err_has <fixed string> <scenario> <desc>
    grep -qF -- "$1" "${WORK_ROOT}/$2.stderr" && ok "$3" || fail "$3 (missing [$1] in evidence: $(cat "${WORK_ROOT}/$2.stderr" 2>/dev/null | tr '\n' '~'))"
}
err_not_has() { # err_not_has <fixed string> <scenario> <desc>
    ! grep -qF -- "$1" "${WORK_ROOT}/$2.stderr" && ok "$3" || fail "$3 (found [$1] in evidence: $(cat "${WORK_ROOT}/$2.stderr" 2>/dev/null | tr '\n' '~'))"
}

echo "==> Scenario 1: verdict body passes through on the first call"
out=$(scenario s1 "${WORK_ROOT}/p1.policy" 'gh_retry gh pr view 42 --json state')
check "verdict body passed through unchanged, rc 0" '{"state":"APPROVED"}RC=0' "$out"
check "exactly one call for a verdict body" 1 "$(call_count s1)"
err_not_has "gh_retry:" s1 "no wrapper evidence on a clean first-attempt success"

echo "==> Scenario 2: transient outage window is retried and converges"
out=$(scenario s2 "${WORK_ROOT}/p2.policy" 'GH_RETRY_BACKOFF_SECONDS="1 1 1" gh_retry gh api repos/o/r/check-runs')
check "transient outage retried to convergence (3 calls)" 3 "$(call_count s2)"
check "converged output passed through, rc 0" '{"total_count":3}RC=0' "$out"
err_has "attempt=1/4 result=retry class=tls_handshake_timeout wait=1s" s2 "attempt# + error class logged as run evidence"
err_has "attempt=2/4 result=retry class=tls_handshake_timeout wait=1s" s2 "second attempt retried with backoff"
err_has "attempt=3/4 result=success class=none" s2 "terminal success logged once"
err_not_has "cat: can" s2 "no capture plumbing noise in evidence"

echo "==> Scenario 3: gh rc 8 (pending verdict) is NOT retried"
out=$(scenario s3 "${WORK_ROOT}/p3.policy" 'gh_retry gh pr checks 42 --json bucket')
check "rc 8 verdict passed through immediately" 'RC=8' "$out"
check "rc 8 made exactly one call" 1 "$(call_count s3)"
err_has "result=pass_through class=non_transient" s3 "non-transient verdict logged as pass_through"

echo "==> Scenario 4: exhaustion returns the last rc and stops (git over https)"
out=$(scenario s4 "${WORK_ROOT}/p4.policy" 'GH_RETRY_MAX_ATTEMPTS="3" GH_RETRY_BACKOFF_SECONDS="1" gh_retry git push -u origin topic')
check "bounded at GH_RETRY_MAX_ATTEMPTS=3" 3 "$(call_count s4)"
check "exhaustion returned the last rc" 'RC=1' "$out"
err_has "attempt=3/3 result=exhausted class=connection_reset" s4 "exhaustion logged with its error class"
err_not_has "attempt=4/" s4 "no calls past the attempt bound"

echo "==> Scenario 5: merged mode folds terminal stderr into the capture"
( export PATH="${WORK_ROOT}/bin:${PATH}"
  export GH_RETRY_STUB_COUNT="${WORK_ROOT}/s5.count"
  export GH_RETRY_STUB_POLICY="${WORK_ROOT}/p5.policy"
  export GH_RETRY_STUB_LOG="${WORK_ROOT}/s5.fd2"
  : >"${GH_RETRY_STUB_COUNT}"
  set +e
  source "${ROOT_DIR}/scripts/gh_retry.sh.tftpl"
  captured=$( { GH_RETRY_BACKOFF_SECONDS="1" gh_retry_merged gh pr merge 42 --squash; } 2>"${GH_RETRY_STUB_LOG}" )
  s5_rc=$?
  printf '%s' "$captured" >"${WORK_ROOT}/s5.capture"
  echo "$s5_rc" >"${WORK_ROOT}/s5.rc" ) 2>/dev/null
check "merged-mode non-transient passes through immediately" 1 "$(call_count s5)"
check "merged mode rc passed through" 1 "$(cat "${WORK_ROOT}/s5.rc")"
check "terminal stderr folded into the captured value" 'fatal: no such remote' "$(cat "${WORK_ROOT}/s5.capture")"
if [ "$(grep -vF 'gh_retry:' "${WORK_ROOT}/s5.fd2" 2>/dev/null | grep -vF 'gh_retry' | tr -d '\n')" = "" ]; then
    ok "merged mode kept the step stderr channel clean"
else
    fail "merged mode kept the step stderr channel clean ($(tr '\n' '~' <"${WORK_ROOT}/s5.fd2" 2>/dev/null))"
fi
if ! grep -qF 'fatal: no such remote' "${WORK_ROOT}/s5.fd2" 2>/dev/null; then
    ok "terminal stderr not duplicated on the stderr channel"
else
    fail "terminal stderr not duplicated on the stderr channel ($(tr '\n' '~' <"${WORK_ROOT}/s5.fd2" 2>/dev/null))"
fi

echo "==> Scenario 6: cumulative sleep budget keeps the wall clock bounded"
# Backoff would be 60/60/60 but the script-level budget only allows one
# second of sleep for the whole invocation, so the second wait clamps to 0.
out=$(scenario s6 "${WORK_ROOT}/p6.policy" 'GH_RETRY_BACKOFF_SECONDS="60 60 60" GH_RETRY_SCRIPT_SLEEP_BUDGET="1" gh_retry gh api /user')
check "budget-capped exhaustion still fails honestly" 'RC=1' "$out"
check "budget-capped exhaustion stopped calling" 4 "$(call_count s6)"
err_has "wait=1s" s6 "first wait consumed the remaining budget"
err_has "wait=0s" s6 "later waits clamped to the collapsed allowance"

echo "==> Scenario 7: the drop-in gh shadow engages the wrapper"
out=$(scenario s7 "${WORK_ROOT}/p7.policy" 'gh pr view 42 --json state')
check "shadowed gh passes a healthy verdict" '{"state":"OPEN"}RC=0' "$out"
check "shadowed gh made exactly one call" 1 "$(call_count s7)"

echo "==> Scenario 8: shadowed gh retries a transient failure too"
out=$(scenario s8 "${WORK_ROOT}/p8.policy" 'GH_RETRY_BACKOFF_SECONDS="1" gh pr view 42 --json state')
check "shadowed gh retried the DNS blip and converged" 2 "$(call_count s8)"
check "shadowed gh converged output" '{"state":"OPEN"}RC=0' "$out"
err_has "result=retry class=dns_failure" s8 "shadow retries logged with their class"

echo "==> Scenario 9: explicit gh_retry(gh) wraps exactly once"
out=$(scenario s9 "${s9_policy}" 'GH_RETRY_BACKOFF_SECONDS="1" gh_retry gh pr checks 42 --json bucket')
check "explicit gh_retry entry retried and converged" '{"bucket":"success"}RC=0' "$out"
check "explicit gh_retry entry made two calls" 2 "$(call_count s9)"
err_has "attempt=1/4 result=retry class=connection_reset" s9 "explicit entry logged the retry"
err_not_has "cat: can" s9 "no errfile collision noise from a nested engine"

if [ "${FAILED}" -gt 0 ]; then
    echo "FAILED: ${FAILED} assertion(s)" >&2
    exit 1
fi
echo "PASS: gh_retry wrapper behavior verified"