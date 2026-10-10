#!/usr/bin/env bash
# Behavioral test for KB-219: the shared bounded-backoff retry wrapper, as
# extended by KB-235 (backoff that spans a 60-90s transport blackout, HTTP 4xx
# surfaces the named failure instead of being retried, and a cross-subshell
# exhaustion signal that lets capture sites re-emit a named transport verdict).
#
# Backdrop (dave ruling 2026-10-08): two 60m+ run cycles were lost because
# check_pr_status and its single 10s retry both landed inside the same ~30s
# api.github.com TLS-handshake-timeout window (KB-48/KB-225: the immediate
# retry refired inside the same blackout window and failed identically). The
# wrapper is the transport layer under every GitHub API / gh call: bounded
# retries (4 attempts, 30/60/120s, per-call overridable) for transient network
# classes only — never for verdict-shaped results, never for HTTP 4xx.
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
#   8. the shadow retries transient classes;
#   9. a literal `gh_retry gh ...` invocation wraps exactly once;
#  10. structural consumer coverage of every gh/API call site; and
#  11. a REAL multi-second deny window (the KB-213/214 shape): the engine
#      converges while the window is open, with clock-checked backoff.
#  12. a PATH with no gh left fails honestly in one attempt (no shadow
#      recursion into the bare-`gh` fallback); and
#  13. a bare `gh_retry` (no command) is rejected with the friendly rc 2
#      evidence line instead of dying on the engine's post-shift `$1` read
#      under set -u.
#  14. the KB-235 default schedule (30/60/120s) and sleep budget (210s) are
#      pinned in the canonical wrapper source;
#  15. HTTP 4xx texts never retry — curl -f "(22) ... returned error: 404",
#      gh's HTTP 404 and Bad-credentials pass through in one call — while
#      429/secondary-rate-limit (transient despite the 4xx shape), 5xx and
#      TLS classes still retry;
#  16. after a capture-site schedule exhaustion, gh_retry_transport_dead sees
#      the exhaustion across the $(...) subshell boundary via the state file
#      and gh_retry_transport_verdict re-emits the named
#      status:transport_failed verdict and exits 0; a non-transport failure
#      exits the ORIGINAL rc instead; and
#  17. a converged success REWRITES the state file, so a stale exhaustion
#      never survives a fresh win.

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
make_stub "${WORK_ROOT}/bin/curl"

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


# Scenario 10 pins the wrapper's CONSUMER coverage structurally: every call
# surface that hits api.github.com / gh CLI must reference gh_retry in its
# step or script file, so removing a wrapper line regresses loudly even
# without an egress-deny live test. Counts are the mechanical scan numbers.
echo "==> Scenario 10: structural consumer coverage of every gh/API call site"
REPO_ROOT="${SCRIPT_DIR}/../.."
assert_grep_count() {
    local desc=$1 want=$2 file=$3
    if [ ! -f "${file}" ]; then
        fail "${desc}: missing file ${file}"
        return
    fi
    local got
    got=$(grep -c "${4:-gh_retry }" "${file}" 2>/dev/null || true)
    if [ "${got}" -eq "${want}" ]; then
        ok "${desc}"
    else
        fail "${desc}: expected ${want}, got ${got} in ${file}"
    fi
}
assert_grep_count "workstream_handler_v1 root steps wrapped" 8 \
    "${REPO_ROOT}/workstream_handler_v1/main.chcl"
assert_grep_count "pr_reviewer_loop steps wrapped" 6 \
    "${REPO_ROOT}/workstream_handler_v1/workflows/pr_reviewer_loop/main.chcl"
assert_grep_count "devops_triage root steps wrapped" 2 \
    "${REPO_ROOT}/devops_triage_v1/main.chcl"
assert_grep_count "devops_triage open_pr script wrapped" 2 \
    "${REPO_ROOT}/devops_triage_v1/scripts/open_pr.sh.tftpl"
assert_grep_count "linear intake entrypoint sites wrapped" 3 \
    "${REPO_ROOT}/linear_intake_v1/container-entrypoint.sh"
for f in criteria-k8s/internal/jobbuilder/jobbuilder.go k8s/job-cri-27.yaml \
         k8s/job-cri-27.yaml.tmpl k8s/examples/ticket-job.yaml; do
    assert_grep_count "jobbuilder fixture wrapped: ${f}" 1 "${REPO_ROOT}/${f}" \
        "gh_retry gh repo clone"
done
assert_grep_count "CI registers the linear intake retry test" 1 \
    "${REPO_ROOT}/.github/workflows/ci.yml" "test_container_entrypoint_gh_retry"
for probe in k8s/pod-adapter-runner.sh k8s/pod-adapter-adapter.sh; do
    if grep -q 'api\.github\.com\|gh pr\|gh api\|gh repo' "${REPO_ROOT}/${probe}" 2>/dev/null; then
        assert_grep_count "probe-only script unexpectedly calls gh: ${probe}" 0 \
            "${REPO_ROOT}/${probe}" "gh_retry "
    else
        ok "probe-only script makes no GitHub calls: ${probe}"
    fi
done

# Reviewer follow-ups on the structural pin, derived mechanically so a new
# call site cannot slip in silently:
#   (a) the four vendored copies of the wrapper must stay byte-identical;
#   (b) EVERY script under the consumer trees that reaches for gh/api.github.com
#       must reference the wrapper (the shadow covers bare calls only inside
#       spliced files, so "no gh_retry reference" means "unwrapped call site").
#
# The scan is `find`-based so it behaves identically under GNU grep (CI) and
# busybox grep (local shells that do not implement --include). Paths under a
# `*/agents/*` directory are excluded: those are AI-reviewer prompt
# templates (agent instructions like `gh pr diff`), not scripts the
# wrapper's shell step gates.
for copy in workstream_handler_v1/scripts/gh_retry.sh.tftpl \
            workstream_handler_v1/workflows/pr_reviewer_loop/scripts/gh_retry.sh.tftpl \
            devops_triage_v1/scripts/gh_retry.sh.tftpl \
            linear_intake_v1/scripts/gh_retry.sh; do
    if cmp -s "${REPO_ROOT}/workstream_handler_v1/scripts/gh_retry.sh.tftpl" "${REPO_ROOT}/${copy}"; then
        ok "vendored copy byte-identical: ${copy}"
    else
        fail "vendored copy diverged from the canonical wrapper: ${copy}"
    fi
done
found_unwrapped=0
while IFS= read -r script; do
    # An empty substitution leaves one blank heredoc line — skip it.
    [ -z "${script}" ] && continue
    if ! grep -q 'gh_retry' "${REPO_ROOT}/${script}"; then
        found_unwrapped=$((found_unwrapped + 1))
        fail "mechanically-scanned gh call site without the wrapper: ${script}"
    fi
done <<EOF
$(find "${REPO_ROOT}/workstream_handler_v1" "${REPO_ROOT}/devops_triage_v1" \
       "${REPO_ROOT}/linear_intake_v1" "${REPO_ROOT}/criteria-k8s" \
       \( -name '*.sh' -o -name '*.tftpl' \) ! -path '*/agents/*' \
    -exec grep -lE 'gh (api|pr|repo)|api\.github\.com' {} + 2>/dev/null \
    | sed "s|^${REPO_ROOT}/||" | grep -v 'gh_retry\|_test\|/tests/')
EOF
[ "${found_unwrapped}" -eq 0 ] && ok "mechanical scan: every gh-call script references the wrapper"


# Scenario 11 is the KB-213/214 acceptance shape end to end: a REAL wall-clock
# deny window (like the scripted 20-30s egress deny, compressed to ~3s) stays
# open across the first two engine attempts, and the engine converges on the
# third with attempt# + class evidence. Real sleeps (1s × 2+) are asserted
# with a clock check so the test cannot silently degrade to the
# instant-pass-through path. The deny window is 2.5s; backoff is 1s.
echo "==> Scenario 11: real-time deny window — wrapper retries and converges"
printf 'reject' >"${WORK_ROOT}/s11.deny"
cat >"${WORK_ROOT}/bin/gh" <<'STUB'
#!/bin/sh
n=$(cat "$GH_RETRY_STUB_COUNT" 2>/dev/null || echo 0)
n=$((n + 1))
echo "$n" >"$GH_RETRY_STUB_COUNT"
if [ -f "$GH_RETRY_DENY_FILE" ]; then
    echo 'fatal: unable to access .api.github.com/: TLS handshake timeout' >&2
    exit 1
fi
echo '{"state":"OPEN"}'
STUB
chmod +x "${WORK_ROOT}/bin/gh"
: >"${WORK_ROOT}/s11.count"
s11_rc=$( ( export PATH="${WORK_ROOT}/bin:${PATH}"
            export GH_RETRY_STUB_COUNT="${WORK_ROOT}/s11.count"
            export GH_RETRY_STUB_LOG="${WORK_ROOT}/s11.stderr"
            export GH_RETRY_DENY_FILE="${WORK_ROOT}/s11.deny"
            set +e
            : >"${GH_RETRY_STUB_LOG}"
            ( sleep 2.5 && rm -f "${GH_RETRY_DENY_FILE}" ) &
            source "${ROOT_DIR}/scripts/gh_retry.sh.tftpl"
            s11_start=$(date +%s)
            GH_RETRY_BACKOFF_SECONDS="1 1 1" gh_retry gh api /user >/dev/null 2>"${GH_RETRY_STUB_LOG}"
            s11_rc=$?
            s11_elapsed=$(( $(date +%s) - s11_start ))
            printf 'RESULT=%s ELAPSED=%s' "$s11_rc" "$s11_elapsed"
          ) 2>/dev/null )
s11_status=${s11_rc%% *}
s11_elapsed=${s11_rc##*ELAPSED=}
check "deny-window run converged rc 0" 'RESULT=0' "$s11_status"
s11_calls=$(call_count s11)
if [ "${s11_calls}" -eq 4 ]; then
    ok "deny-window run retried inside the window and converged (calls: ${s11_calls})"
else
    fail "deny-window run retried inside the window and converged (calls: ${s11_calls})"
fi
if [ "$s11_elapsed" -ge 3 ]; then
    ok "wrapper executed real backoff sleeps (elapsed ${s11_elapsed}s >= deny window)"
else
    fail "wrapper executed real backoff sleeps (elapsed ${s11_elapsed}s, expected >= 3)"
fi
err_has "attempt=1/4 result=retry class=tls_handshake_timeout wait=1s" s11 "first attempt logged its class"
err_has "result=success class=none" s11 "convergence after the window logged"
# Scenario 11 swapped bin/gh for its deny-window stub, which ignores
# GH_RETRY_STUB_POLICY; later scenarios need the policy-driven stub back.
make_stub "${WORK_ROOT}/bin/gh"

# Scenario 12 is a reviewer follow-up: with NO gh anywhere on PATH, the bare
# `gh` fallback must fail the call honestly in one attempt. Before the
# `command`-rebuild guard, the bare fallback re-resolved onto the gh() shadow
# and recursed until process exhaustion; with the guard the engine resolves
# through `command` (which suppresses function lookup) and returns the exec
# failure rc. `timeout` bounds the regression: an engine that recurses again
# dies at the bound with rc 124 and the rc assertion fails loudly.
echo "==> Scenario 12: no gh on PATH — engine fails honestly, no shadow recursion"
mkdir -p "${WORK_ROOT}/minimal"
for tool in cat rm sleep; do
    ln -sf "$(command -v "${tool}")" "${WORK_ROOT}/minimal/${tool}"
done
cat >"${WORK_ROOT}/s12.body" <<BODY
export PATH="${WORK_ROOT}/minimal"
set +e
# POSIX dot-source (not bash's "source" keyword): this probe runs under
# /bin/sh, which is dash on CI and has no source builtin.
# NOTE: this heredoc is unquoted so ROOT_DIR expands; keep backticks out of
# its comments -- an unquoted heredoc executes them as command substitutions.
. "${ROOT_DIR}/scripts/gh_retry.sh.tftpl"
GH_RETRY_BACKOFF_SECONDS="0 0 0" gh_retry gh api /user >/dev/null 2>"${WORK_ROOT}/s12.stderr"
echo "rc=\$?"
BODY
s12_rc=$(timeout 10 sh "${WORK_ROOT}/s12.body" 2>/dev/null)
s12_status=${s12_rc##*rc=}
# `timeout` bounds the regression probe: a recursing engine returns 124 here,
# which fails the assertion instead of hanging the suite.
check "missing gh failed the call in one honest attempt (rc 127)" 'rc=127' "${s12_rc}"
err_has "result=pass_through class=non_transient" s12 "exec failure logged as a non-transient pass-through"

# Scenario 13 pins the degenerate-argument guard: `gh_retry` with no
# command reaches the engine as a lone mode arg; without the `[ $# -ge 2 ]`
# guard the engine shifted past the mode and died at the post-shift `$1`
# read under set -u, so the subshell produced no RC line and no evidence
# at all.
echo "==> Scenario 13: bare gh_retry — friendly degenerate-arg rejection"
cat >"${WORK_ROOT}/s13.policy" <<'EOF'
0|stub-must-not-run|
EOF
out=$(scenario s13 "${WORK_ROOT}/s13.policy" 'gh_retry')
check "bare gh_retry rejected with rc 2, nothing executed" 'RC=2' "$out"
check "no stub call on a degenerate invocation" 0 "$(call_count s13)"
err_has "attempt=1/0 result=pass_through class=non_transient label=none (no command given)" s13 \
    "degenerate call logged the honest pass-through evidence line"

if [ "${FAILED}" -gt 0 ]; then
    echo "FAILED: ${FAILED} assertion(s)" >&2
    exit 1
fi

# ── KB-235 scenarios ─────────────────────────────────────────────────────────

# Scenario 14 pins the KB-235 schedule structurally: the contract schedule is
# 30s/60s/120s with a 210s cumulative script budget. It cannot be exercised
# with real default sleeps inside a CI-fast test (30+60+120 = 210s), so the
# default VALUES are pinned here while scenario 2/4/6/11 exercise the same
# engine paths with compressed backoff and real clocks.
echo "==> Scenario 14: KB-235 default backoff schedule and budget pinned"
grep -qF 'GH_RETRY_BACKOFF_SECONDS:-"30 60 120"' "${ROOT_DIR}/scripts/gh_retry.sh.tftpl" \
    && ok "default backoff schedule pinned at 30/60/120" \
    || fail "default backoff schedule is not the KB-235 contract 30/60/120"
grep -qF '_gr_budget=${GH_RETRY_SCRIPT_SLEEP_BUDGET:-210}' "${ROOT_DIR}/scripts/gh_retry.sh.tftpl" \
    && ok "default sleep budget pinned at 210s" \
    || fail "default sleep budget is not the KB-235 contract 210s"
grep -qF '_gr_budget=210 ;; esac' "${ROOT_DIR}/scripts/gh_retry.sh.tftpl" \
    && ok "invalid budget fallback pinned at 210s" \
    || fail "invalid budget fallback no longer the KB-235 budget"

echo "==> Scenario 15: HTTP 4xx never retry; 429 and 5xx still do"
printf '1||HTTP 404: Not Found|\n' >"${WORK_ROOT}/p15a.policy"
out=$(scenario s15a "${WORK_ROOT}/p15a.policy" 'gh_retry gh api repos/o/r/pulls/42')
check "gh HTTP 404 passed through in ONE call, no retry" 'RC=1' "$out"
check "gh HTTP 404 made exactly one call" 1 "$(call_count s15a)"
err_has "result=pass_through class=non_transient" s15a "4xx surfaced the named failure immediately"

printf '1||curl: (22) The requested URL returned error: 404|\n' >"${WORK_ROOT}/p15b.policy"
out=$(scenario s15b "${WORK_ROOT}/p15b.policy" 'gh_retry curl -sS -f -X POST -d x https://kanboard.example/jsonrpc.php')
check "curl -f 4xx error text passed through in ONE call" 'RC=1' "$out"
check "curl -f 4xx made exactly one call" 1 "$(call_count s15b)"
err_has "result=pass_through class=non_transient" s15b "curl 4xx text classified non-transient (KB-235 fix)"

printf '1||HTTP 401: Bad credentials|\n' >"${WORK_ROOT}/p15c.policy"
out=$(scenario s15c "${WORK_ROOT}/p15c.policy" 'GH_RETRY_BACKOFF_SECONDS="0 0 0" gh_retry gh api /user')
check "auth failure (401/Bad credentials) passed through immediately" 'RC=1' "$out"
check "auth failure made exactly one call" 1 "$(call_count s15c)"

printf '1||curl: (22) The requested URL returned error: 429|\n' >"${WORK_ROOT}/p15d.policy"
out=$(scenario s15d "${WORK_ROOT}/p15d.policy" 'GH_RETRY_MAX_ATTEMPTS="3" GH_RETRY_BACKOFF_SECONDS="0 0 0" gh_retry curl -sS -f -X POST -d x https://kanboard.example/jsonrpc.php')
check "429 retried despite the 4xx shape" 3 "$(call_count s15d)"
check "429 exhaustion returned the last rc" 'RC=1' "$out"
err_has "result=exhausted class=rate_limit" s15d "429 classified as rate_limit"

printf '1||API rate limit exceeded for 1.2.3.4. Please wait|\n' >"${WORK_ROOT}/p15e.policy"
out=$(scenario s15e "${WORK_ROOT}/p15e.policy" 'GH_RETRY_MAX_ATTEMPTS="2" GH_RETRY_BACKOFF_SECONDS="0 0 0" gh_retry gh api /user')
check "gh secondary rate limit text retried" 2 "$(call_count s15e)"
err_has "result=retry class=rate_limit" s15e "gh rate-limit text classified as rate_limit"

printf '1||HTTP 502: Bad Gateway|\n' >"${WORK_ROOT}/p15f.policy"
out=$(scenario s15f "${WORK_ROOT}/p15f.policy" 'GH_RETRY_MAX_ATTEMPTS="2" GH_RETRY_BACKOFF_SECONDS="0 0 0" gh_retry gh api /user')
check "5xx still retried" 2 "$(call_count s15f)"
err_has "result=retry class=http_5xx" s15f "5xx classified as http_5xx"

# Scenario 16 is the KB-235 cross-subshell exhaustion signal: a capture-site
# call exhausts inside $(...) where in-process env vars cannot survive, then
# gh_retry_transport_dead reads the terminal state file and
# gh_retry_transport_verdict re-emits the named protocol verdict and exits 0.
echo "==> Scenario 16: transport verdict helpers across the capture subshell"
printf '1||fatal: unable to access .api.github.com/: TLS handshake timeout|\n' >"${WORK_ROOT}/p16.policy"
out=$(scenario s16 "${WORK_ROOT}/p16.policy" '
GH_RETRY_BACKOFF_SECONDS="0 0 0"
if v=$(gh_retry gh api /user 2>/dev/null); then
    echo "BUG capture succeeded"
elif gh_retry_transport_dead; then
    echo "DEAD=${GH_RETRY_DEAD_CLASS}/${GH_RETRY_DEAD_ATTEMPTS}"
else
    echo "BUG state file does not report the exhaustion"
fi')
check "exhaustion seen across the subshell with class and attempts" \
    'DEAD=tls_handshake_timeout/4' "$(printf '%s' "$out" | head -n 1)"

out=$(scenario s16b "${WORK_ROOT}/p16.policy" '
set +e
( GH_RETRY_BACKOFF_SECONDS="0 0 0" v=$(gh_retry gh api /user 2>/dev/null); gh_retry_transport_verdict "$?" 42 "pr state lookup" )
og=$?
if [ "$og" -eq 0 ]; then
    echo "guard rc=0 (exit inside the guard)"
else
    echo "BUG transport guard did not exit 0 for the exhaustion: rc=$og"
fi')
lines16b=$(printf '%s' "$out" | sed '$d')
check "transport guard re-emitted the named verdict before its exit" \
    'status:transport_failedpr_number=42transport_class=tls_handshake_timeouttransport_attempts=4transport_stage=pr state lookup' \
    "$(printf '%s' "$out" | sed '$d' | head -n 5 | tr -d '\n')"
check "transport guard exited 0 and nothing after it ran" \
    'guard rc=0 (exit inside the guard)' "$(printf '%s' "$out" | sed '$d' | tail -n 1)"
check "scenario body returned rc 0 after the guard's exit 0" 'RC=0' "$(printf '%s' "$out" | tail -n 1)"

# Scenario 17: the guard's non-transport path exits the ORIGINAL rc (KB-24's
# honest failure route stays in charge), and a converged success REWRITES the
# state file so a stale exhaustion never survives a fresh win.
echo "==> Scenario 17: guard honors non-transport failures; successes clear the signal"
printf '1||HTTP 404: Not Found|\n' >"${WORK_ROOT}/p17a.policy"
out=$(scenario s17a "${WORK_ROOT}/p17a.policy" '
set +e
( v=$(gh_retry gh api repos/o/r 2>/dev/null); gh_retry_transport_verdict "$?" 42 "pr state lookup" )
og=$?
if [ "$og" -eq 1 ]; then
    echo "original rc propagated: $og"
else
    echo "BUG non-transport guard rc: $og"
fi')
check "non-transport failure exits the ORIGINAL rc, no transport verdict" \
    'original rc propagated: 1' "$(printf '%s' "$out" | sed '$d')"
check "scenario body returned rc 0 after the guard's rc-1 exit" 'RC=0' "$(printf '%s' "$out" | tail -n 1)"

printf '1||fatal: unable to access .api.github.com/: TLS handshake timeout|\n0|{"state":"OPEN"}|\n' >"${WORK_ROOT}/p17b.policy"
out=$(scenario s17b "${WORK_ROOT}/p17b.policy" '
set +e
GH_RETRY_BACKOFF_SECONDS="0 0 0"
if v=$(gh_retry gh api /user 2>/dev/null); then
    echo "capture value: $v"
fi
if gh_retry_transport_dead; then
    echo "BUG stale exhaustion survived a fresh success"
else
    echo "signal cleared after convergence"
fi')
check "converged capture returned its value" 'capture value: {"state":"OPEN"}' "$(printf '%s' "$out" | head -n 1)"
check "fresh success rewrote the state file (stale exhaustion cleared)" \
    'signal cleared after convergence' "$(printf '%s' "$out" | sed -n 2p)"

if [ "${FAILED}" -gt 0 ]; then
    echo "FAILED: ${FAILED} assertion(s)" >&2
    exit 1
fi
echo "PASS: gh_retry wrapper behavior verified"