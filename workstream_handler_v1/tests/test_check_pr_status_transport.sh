#!/usr/bin/env bash
# KB-235 acceptance: check_pr_status transient transport failures back off
# and classify, wired end-to-end through the rendered script (gh_retry shim
# spliced at its marker, same as Criteria injects).
#
#   - fails twice then succeeds (simulate)                 => script proceeds (status:approved, rc 0)
#   - persistent transport failure (exhausts 3 retries)    => named status:transport_failed verdict with
#                                                             pr_number/class/attempts/stage, rc 0
#   - HTTP 4xx (auth, not-found)                           => single call, honest rc 1, no verdict
#   - transport death mid-script at the checks poll        => verdict with transport_stage=checks poll
# Plus compile-level pins for the merge_pending_github wiring in main.chcl:
# terminal state, route arm before default, review_result preservation
# (KB-49: standing verdict survives the poll death), and the named
# "github transport failed after retries" failure reason.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BASE="${SCRIPT_DIR}/.."
CHILD="${BASE}/workflows/pr_reviewer_loop"
TEMPLATE="${CHILD}/scripts/check_pr_status.sh.tftpl"
WRAPPER="${CHILD}/scripts/gh_retry.sh.tftpl"
MAIN="${CHILD}/main.chcl"
WORK_DIR="$(mktemp -d)"
trap 'rm -rf "${WORK_DIR}"' EXIT

FAKE_GH="${WORK_DIR}/gh"
RENDER_TEMPLATE="${WORK_DIR}/template.rendered"
SCRIPT="${WORK_DIR}/check_pr_status.sh"
OUT="${WORK_DIR}/stdout"
ERR="${WORK_DIR}/stderr"

# Render the template into an executable script: splice the wrapper body at
# its marker and substitute the shellquoted PR number.
gh_retry_line="$(grep -n '{{ *\.gh_retry *}}' "${TEMPLATE}" | head -1 | cut -d: -f1)"
{
    head -n $((gh_retry_line - 1)) "${TEMPLATE}"
    cat "${WRAPPER}"
    tail -n "+$((gh_retry_line + 1))" "${TEMPLATE}"
} > "${RENDER_TEMPLATE}"
sed -e 's/{{ *\.criteria_value_1 *| *shellquote *}}/"42"/g' \
    "${RENDER_TEMPLATE}" > "${SCRIPT}"
chmod +x "${SCRIPT}"

cat > "${FAKE_GH}" <<'MOCK'
#!/usr/bin/env bash
set -uo pipefail

COUNTER_DIR="${MOCK_COUNTER_DIR:?}"

bump() {
    local f="${COUNTER_DIR}/$1"
    local n=0
    [ -f "$f" ] && n="$(cat "$f")"
    n=$((n + 1))
    printf '%s' "$n" > "$f"
    printf '%s' "$n"
}

args="$*"
case "$args" in
    *"pr view"*"--json state"*)             class=state ;;
    *"repo view"*"--json owner"*)           class=owner ;;
    *"repo view"*"--json name"*)            class=name ;;
    *"/check-runs"*)                        class=checks ;;
    *"graphql"*)                            class=threads ;;
    *"--json reviewDecision"*)              class=decision ;;
    *"--json headRefOid"*)                  class=head ;;
    *"pulls/42/reviews"*)                   class=reviews ;;
    *)                                      class=other ;;
esac
n="$(bump "$class")"

if [ -n "${MOCK_FAIL_CLASS:-}" ] && [ "$class" = "${MOCK_FAIL_CLASS}" ] && [ "$n" -le "${MOCK_FAIL_FIRST:-0}" ]; then
    printf '%s\n' "${MOCK_FAIL_TEXT:-net/http: TLS handshake timeout}" >&2
    exit 1
fi

case "$class" in
    state)    printf '%s\n' "${MOCK_PR_STATE:-OPEN}" ;;
    owner)    printf '%s\n' "octocat-org" ;;
    name)     printf '%s\n' "repo-under-test" ;;
    head)     printf '%s\n' "abc123" ;;
    decision) printf '%s\n' "${MOCK_REVIEW_DECISION:-APPROVED}" ;;
    checks)   printf '%s\n' "${MOCK_CHECKS_JSON:-[{"name":"ci","state":"COMPLETED","conclusion":"success"}]}" ;;
    threads)  printf '%s\n' '{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[]}}}}}' ;;
    reviews)  printf '%s\n' "[{\"state\":\"APPROVED\",\"commit_id\":\"abc123\"}]" ;;
    *)
        echo "unexpected gh invocation: $args" >&2
        exit 99
        ;;
esac
MOCK
chmod +x "${FAKE_GH}"

# Backoffs are compressed to zero: the schedule itself is pinned by
# test_gh_retry.sh (scenario 14); here we exercise script-level dynamics, so
# no scenario sleeps through the real 30/60/120s.
run_case() {
    local name="$1"
    shift
    echo "==> ${name}"
    : > "${OUT}"
    : > "${ERR}"
    local cdir="${WORK_DIR}/counters_$(printf '%s' "$name" | tr -c 'a-zA-Z0-9' '_')"
    mkdir -p "$cdir"
    : > "${cdir}/state"
    if env "$@" "MOCK_COUNTER_DIR=${cdir}" GH_RETRY_BACKOFF_SECONDS="0 0 0" \
        PATH="${WORK_DIR}:${PATH}" bash "${SCRIPT}" >"${OUT}" 2>"${ERR}"; then
        rc=0
    else
        rc=$?
    fi
    counters="$cdir"
}

counter() { cat "${counters}/$1"; }

fail() {
    echo "FAIL: $1" >&2
    echo "--- stdout ---" >&2
    cat "${OUT}" >&2 || true
    echo "--- stderr ---" >&2
    cat "${ERR}" >&2 || true
    exit 1
}

# ── Scenario A (acceptance): fails twice then succeeds => run proceeds ────────
run_case "transport fails twice then succeeds" \
    MOCK_FAIL_CLASS=state MOCK_FAIL_FIRST=2
[ "$rc" -eq 0 ] || fail "expected the step to succeed after two transient transport failures, got rc=$rc"
grep -q "^status:approved$" "${OUT}" || fail "expected status:approved after transient failures cleared"
grep -q "^pr_number=42$" "${OUT}" || fail "expected pr_number=42 in the success verdict"
[ "$(counter state)" -eq 3 ] || fail "expected 3 state-poll attempts (2 failed + 1 success), got $(counter state)"
! grep -q "status:transport_failed" "${OUT}" || fail "transport verdict must not fire when the call eventually succeeds"

# ── Scenario B (acceptance): persistent transport failure => named verdict ────
run_case "transport failure exhausts the schedule" \
    MOCK_FAIL_CLASS=state MOCK_FAIL_FIRST=99
[ "$rc" -eq 0 ] || fail "the transport verdict must exit 0 so the loop can park the run, got rc=$rc"
[ "$(head -n 1 "${OUT}")" = "status:transport_failed" ] || fail "first stdout line must be the named transport verdict"
grep -q "^pr_number=42$" "${OUT}" || fail "verdict must carry pr_number"
grep -q "^transport_class=tls_handshake_timeout$" "${OUT}" || fail "verdict must carry the classified transport class"
grep -q "^transport_attempts=4$" "${OUT}" || fail "verdict must carry 4 attempts (1 initial + 3 retries)"
grep -q "^transport_stage=pr state lookup$" "${OUT}" || fail "verdict must carry the named transport stage"
[ "$(counter state)" -eq 4 ] || fail "expected 1 initial attempt + 3 backoff retries, got $(counter state)"

# ── Scenario C: HTTP 4xx never retries, surfaces the honest failure ───────────
run_case "HTTP 404 passes through without retry" \
    MOCK_FAIL_CLASS=state MOCK_FAIL_FIRST=99 \
    MOCK_FAIL_TEXT="HTTP 404: Not Found (https://api.github.com/repos/octocat-org/repo-under-test/pulls/42)"
[ "$rc" -eq 1 ] || fail "a non-transient HTTP 4xx must exit the original rc 1, got rc=$rc"
grep -q "HTTP 404" "${ERR}" || fail "stderr must surface the named HTTP 4xx failure"
[ "$(counter state)" -eq 1 ] || fail "HTTP 4xx must not be retried (single call), got $(counter state) attempts"
! grep -q "status:transport_failed" "${OUT}" || fail "no transport verdict for a non-transport failure"
[ ! -s "${OUT}" ] || fail "stdout must stay empty on the pass-through failure path"

# ── Scenario D: transport death mid-script at the checks poll ─────────────────
run_case "checks poll transport death reaches the verdict" \
    MOCK_FAIL_CLASS=checks MOCK_FAIL_FIRST=99
[ "$rc" -eq 0 ] || fail "the transport verdict must exit 0, got rc=$rc"
grep -q "^status:transport_failed$" "${OUT}" || fail "expected the named transport verdict"
grep -q "^transport_stage=checks poll$" "${OUT}" || fail "verdict must name the checks poll stage"
grep -q "^transport_attempts=4$" "${OUT}" || fail "verdict must carry 4 attempts"
grep -q "^transport_class=tls_handshake_timeout$" "${OUT}" || fail "verdict must carry the classified class"

# ── Scenario E: the hoisted head-ref poll fails twice then succeeds ──────────
run_case "head ref transport fails twice then succeeds" \
    MOCK_FAIL_CLASS=head MOCK_FAIL_FIRST=2
[ "$rc" -eq 0 ] || fail "expected the step to succeed after two transient head-ref failures, got rc=$rc"
grep -q "^status:approved$" "${OUT}" || fail "expected status:approved after the head-ref poll recovered"
[ "$(counter head)" -eq 3 ] || fail "expected 3 head-ref attempts (2 failed + 1 success), got $(counter head)"
! grep -q "status:transport_failed" "${OUT}" || fail "no transport verdict when the head-ref poll recovers"

# ── Scenario F: head-ref poll death reaches the named verdict ────────────────
run_case "head ref transport exhaustion reaches the verdict" \
    MOCK_FAIL_CLASS=head MOCK_FAIL_FIRST=99
[ "$rc" -eq 0 ] || fail "the transport verdict must exit 0, got rc=$rc"
grep -q "^status:transport_failed$" "${OUT}" || fail "expected the named transport verdict"
grep -q "^transport_stage=head ref lookup$" "${OUT}" || fail "verdict must name the head ref lookup stage"
grep -q "^transport_attempts=4$" "${OUT}" || fail "verdict must carry 4 attempts (1 initial + 3 retries)"
grep -q "^transport_class=tls_handshake_timeout$" "${OUT}" || fail "verdict must carry the classified transport class"
[ "$(counter head)" -eq 4 ] || fail "expected 1 initial attempt + 3 backoff retries on the head-ref poll, got $(counter head)"
checks_runs="$(cat "${counters}/checks" 2>/dev/null || echo 0)"
[ "${checks_runs:-0}" -eq 0 ] || fail "the checks poll must not run when the head-ref surface dies first, got ${checks_runs} attempts"

# ── Compile-level pins: merge_pending_github wiring in main.chcl ──────────────
echo "==> Checking merge_pending_github wiring (KB-235)"
grep -q '^state "merge_pending_github" {' "${MAIN}" || fail "missing merge_pending_github terminal state"
merge_block="$(sed -n '/^state "merge_pending_github" {/,/^}/p' "${MAIN}")"
printf '%s\n' "$merge_block" | grep -q "terminal = true" || fail "merge_pending_github must be terminal"
printf '%s\n' "$merge_block" | grep -q "success  = false" || fail "merge_pending_github must report success=false"

arm_line="$(grep -n 'startswith(steps.check_pr_status.stdout, "status:transport_failed")' "${MAIN}" | head -1 | cut -d: -f1)" || fail "missing transport route arm in route_pr_status"
[ -n "$arm_line" ] || fail "missing transport route arm in route_pr_status"
arm_next="$(awk -v arm="$arm_line" 'NR == arm + 1 { print; exit }' "${MAIN}")"
printf '%s\n' "$arm_next" | grep -q "state.merge_pending_github" || fail "transport route arm must target state.merge_pending_github"
default_line="$(awk -v arm="$arm_line" 'NR > arm && /default \{ next = state.failed \}/ { print NR; exit }' "${MAIN}")"
[ -n "$default_line" ] || fail "missing default arm after the transport arm in route_pr_status"
[ "$arm_line" -lt "$default_line" ] || fail "transport route arm must precede the default failure arm in route_pr_status"

[ "$(grep -c 'startswith(output.stdout, "status:transport_failed") ? data.internal.review_result.value' "${MAIN}")" -eq 2 ] || \
    fail "both status steps must preserve the standing review_result on a transport verdict (KB-49)"
[ "$(grep -c 'github transport failed after retries (merge pending github): ' "${MAIN}")" -eq 2 ] || \
    fail "both status steps must write the named failure reason"
[ "$(grep -c 'timeout    = "600s"' "${MAIN}")" -eq 5 ] || \
    fail "expected the 5 gh_retry-consuming child steps on 600s timeouts (KB-235 budget)"

echo "==> All checks passed."