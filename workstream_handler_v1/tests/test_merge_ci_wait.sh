#!/usr/bin/env bash
# KB-272 acceptance: the merge-commit CI gate closes inside the workflow —
# after the squash merge, wait_merge_ci polls the squash commit's own Actions
# check-runs until a bounded verdict, and the handler routes green -> the done
# arm, red/timeout -> the assistance lane (typed failure_reason, NO Done move).
#
# Script-level scenarios run end-to-end through the rendered script (gh_retry
# shim spliced at its marker, same as Criteria injects):
#
#   - all check runs completed green                     => merge_ci=green, single poll
#   - runs still in_progress then green                  => merge_ci=green after polling
#   - a completed failing run                            => merge_ci=failed with failed_check evidence
#   - runs never complete within the deadline            => merge_ci=timeout (bounded, terminates)
#   - commit has no check runs, repo has NO workflows    => merge_ci=green (CRI-90 semantics)
#   - merge_pr emitted no sha; mergeCommit appears later => sha refreshed bounded, then green
#   - sha never materializes within the deadline         => merge_ci=timeout (fail closed)
#   - merge_pr output lost its PR number (wiring bug)    => step fails (rc != 0), fail closed
#   - transport blip on the checks poll                  => gh_retry absorbs it, verdict still green
#
# Compile-level pins on the handler workflow: merge_pr's success arm goes to
# wait_merge_ci (not straight to teardown), the route switch sends only
# merge_ci=green to teardown_worktree, the default arm routes the assistance
# lane to step.fail_merge_ci which writes the typed merge_ci_failed reason on
# BOTH of its outcome arms (the parent trees park the ticket in Review), and
# merge_pr.sh emits the squash sha on both of its success arms.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BASE="${SCRIPT_DIR}/.."
TEMPLATE="${BASE}/scripts/wait_merge_ci.sh.tftpl"
WRAPPER="${BASE}/scripts/gh_retry.sh.tftpl"
MERGE_TEMPLATE="${BASE}/scripts/merge_pr.sh.tftpl"
MAIN="${BASE}/main.chcl"
WORK_DIR="$(mktemp -d)"
trap 'rm -rf "${WORK_DIR}"' EXIT

FAKE_GH="${WORK_DIR}/gh"
SCRIPT="${WORK_DIR}/wait_merge_ci.sh"
OUT="${WORK_DIR}/stdout"
ERR="${WORK_DIR}/stderr"

# Render the template into an executable script: splice the wrapper body at
# its marker and substitute the shellquoted criteria value (merge_pr's stdout
# line: pr number and — when merge_pr could resolve it — the squash sha).
gh_retry_line="$(grep -n '{{ *\.gh_retry *}}' "${TEMPLATE}" | head -1 | cut -d: -f1)"
{
    head -n $((gh_retry_line - 1)) "${TEMPLATE}"
    cat "${WRAPPER}"
    tail -n "+$((gh_retry_line + 1))" "${TEMPLATE}"
} > "${WORK_DIR}/body.pre"
render_case() {
    local criteria_value="$1"
    # KB-328: the merge-CI budget renders from the tree var as a plain
    # go-template action (no shell expansion — the default lands via the
    # max_seconds assignment); MERGE_CI_MAX_TEMPLATE lets scenarios verify a
    # non-default rendered budget.
    sed -e "s#{{ *\\.criteria_value_1 *| *shellquote *}}#\"${criteria_value}\"#g" \
        -e "s#{{ *\\.merge_ci_max *}}#${MERGE_CI_MAX_TEMPLATE:-600}#g" \
        "${WORK_DIR}/body.pre" > "${SCRIPT}"
    chmod +x "${SCRIPT}"
}

# Sequence-bearing fake gh: per-class response lists, '|' separated, last one
# repeats (index >= list length). Transport-failure simulation is per class
# too: fail the first MOCK_FAIL_FIRST calls of MOCK_FAIL_CLASS with
# MOCK_FAIL_TEXT on stderr (gh_retry sees a transient TLS-shaped text).
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

seq_pick() {
    # $1 = env var holding a '|' separated sequence; $2 = 1-based index.
    # Items beyond the end repeat the last item. The here-string already
    # carries a trailing '|', so the residue after the final delimiter is
    # EOF, not an (empty) item — only a non-empty tail read past the last
    # delimiter is a real item.
    local list="" i=1 doc="" last=""
    [ -n "${!1:-}" ] && list="${!1}"
    [ -z "$list" ] && { printf ''; return; }
    while IFS='|' read -r -d '|' doc; do
        [ "$i" -eq "$2" ] && { printf '%s' "$doc"; return; }
        last="$doc"
        i=$((i + 1))
    done <<< "${list}|"
    printf '%s' "$last"
}

args="$*"
case "$args" in
    *"repo view"*"--json owner"*)            class=owner ;;
    *"repo view"*"--json name"*)             class=name ;;
    *"pr view"*"--json mergeCommit"*)        class=merge_commit ;;
    *"/check-runs"*)                         class=checks ;;
    *"actions/workflows"*)                   class=workflows ;;
    *)                                       class=other ;;
esac
n="$(bump "$class")"

if [ -n "${MOCK_FAIL_CLASS:-}" ] && [ "$class" = "${MOCK_FAIL_CLASS}" ] && [ "$n" -le "${MOCK_FAIL_FIRST:-0}" ]; then
    printf '%s\n' "${MOCK_FAIL_TEXT:-net/http: TLS handshake timeout}" >&2
    exit 1
fi

case "$class" in
    owner)         printf '%s\n' "octocat-org" ;;
    name)          printf '%s\n' "repo-under-test" ;;
    merge_commit)  printf '%s\n' "$(seq_pick MOCK_SHA_SEQ "$n")" ;;
    checks)        printf '%s' "$(seq_pick MOCK_CHECKS_SEQ "$n")" ;;
    workflows)     printf '{\"total_count\":%s}\n' "${MOCK_WF_COUNT:-2}" ;;
    *)
        echo "unexpected gh invocation: $args" >&2
        exit 99
        ;;
esac
MOCK
chmod +x "${FAKE_GH}"

# Short deadline/cadence for timeout scenarios: the bound itself (10 min,
# 30s cadence) is pinned in the compile-level checks, not slept through here.
run_case() {
    local name="$1"
    shift
    echo "==> ${name}"
    : > "${OUT}"
    : > "${ERR}"
    local cdir="${WORK_DIR}/counters_$(printf '%s' "$name" | tr -c 'a-zA-Z0-9' '_')"
    mkdir -p "$cdir"
    rm -f "${TMPDIR:-/tmp}/gh_retry-last.$$" 2>/dev/null || true
    # Every case runs at test cadence; timeout cases below override the bound.
    if env MERGE_CI_POLL_SECONDS=0 MERGE_CI_MAX_SECONDS=180 "$@" \
        "MOCK_COUNTER_DIR=${cdir}" GH_RETRY_BACKOFF_SECONDS="0 0 0" \
        PATH="${WORK_DIR}:${PATH}" bash "${SCRIPT}" >"${OUT}" 2>"${ERR}"; then
        rc=0
    else
        rc=$?
    fi
    counters="$cdir"
}

counter() { cat "${counters}/$1" 2>/dev/null || echo 0; }

fail() {
    echo "FAIL: $1" >&2
    echo "--- stdout ---" >&2
    cat "${OUT}" >&2 || true
    echo "--- stderr ---" >&2
    cat "${ERR}" >&2 || true
    exit 1
}

SHA="abc123def456abc123def456abc123def456ab12"
GREEN='{"total_count":2,"check_runs":[{"name":"ci","status":"completed","conclusion":"success"},{"name":"lint","status":"completed","conclusion":"success"}]}'
IN_PROGRESS='{"total_count":2,"check_runs":[{"name":"ci","status":"completed","conclusion":"success"},{"name":"lint","status":"in_progress"}]}'
ONE_FAILED='{"total_count":2,"check_runs":[{"name":"unit tests","status":"completed","conclusion":"failure"},{"name":"lint","status":"completed","conclusion":"success"}]}'
EMPTY_RUNS='{"total_count":0,"check_runs":[]}'

# ── Scenario A (acceptance): completed-green on the first poll => done arm ────
render_case "merged=true pr=42 merge_sha=${SHA}"
run_case "green on first poll" MOCK_CHECKS_SEQ="${GREEN}"
[ "$rc" -eq 0 ] || fail "expected rc 0 on a green merge-commit rollup, got rc=$rc"
[ "$(head -n 1 "${OUT}")" = "merge_ci=green" ] || fail "first stdout line must be the green verdict"
grep -q "^sha=${SHA}$" "${OUT}" || fail "the verdict must carry the squash sha"
[ "$(counter checks)" -eq 1 ] || fail "a completed-green rollup must stop after one poll, got $(counter checks)"
[ "$(counter workflows)" -eq 0 ] || fail "the no-CI fallback must not run when check runs exist, got $(counter workflows) calls"

# ── Scenario B (acceptance): runs still in_progress -> poll, then green ───────
run_case "pending then green" MOCK_CHECKS_SEQ="${IN_PROGRESS}|${GREEN}"
[ "$rc" -eq 0 ] || fail "expected rc 0, got rc=$rc"
[ "$(head -n 1 "${OUT}")" = "merge_ci=green" ] || fail "expected merge_ci=green once the last run completed"
[ "$(counter checks)" -eq 2 ] || fail "expected 2 checks polls (pending + green), got $(counter checks)"

# ── Scenario C (acceptance): a completed failing run => assistance lane ───────
run_case "red merge commit" MOCK_CHECKS_SEQ="${ONE_FAILED}"
[ "$rc" -eq 0 ] || fail "the failed verdict must exit 0 so the route switch owns the lane, got rc=$rc"
[ "$(head -n 1 "${OUT}")" = "merge_ci=failed" ] || fail "first stdout line must be the failed verdict"
grep -q "^sha=${SHA}$" "${OUT}" || fail "the failed verdict must carry the sha"
grep -q "^failed_check=unit tests=failure$" "${OUT}" || fail "the failed verdict must name the failing check"
[ "$(counter checks)" -eq 1 ] || fail "a completed-failed rollup must stop after one poll, got $(counter checks)"

# ── Scenario D (acceptance): CI never completes within the deadline ───────────
run_case "timeout bounded" MOCK_CHECKS_SEQ="${IN_PROGRESS}" \
    MERGE_CI_POLL_SECONDS=0 MERGE_CI_MAX_SECONDS=2
[ "$rc" -eq 0 ] || fail "the timeout verdict must exit 0, got rc=$rc"
[ "$(head -n 1 "${OUT}")" = "merge_ci=timeout" ] || fail "first stdout line must be the timeout verdict"
grep -q "^sha=${SHA}$" "${OUT}" || fail "the timeout verdict must carry the sha"
[ "$(counter checks)" -ge 2 ] || fail "expected multiple bounded polls before the deadline, got $(counter checks)"

# ── Scenario E: commit has no check runs and the repo has no CI (CRI-90) ──────
run_case "no check runs, no workflows" MOCK_CHECKS_SEQ="${EMPTY_RUNS}" MOCK_WF_COUNT=0
[ "$rc" -eq 0 ] || fail "expected rc 0, got rc=$rc"
[ "$(head -n 1 "${OUT}")" = "merge_ci=green" ] || fail "a repo with no Actions workflows must not hang the gate"
grep -q "no GitHub Actions workflows" "${OUT}" || fail "the green verdict must record the no-CI rationale as evidence"
[ "$(counter workflows)" -eq 1 ] || fail "expected exactly one workflows lookup, got $(counter workflows)"

# ── Scenario F: merge_pr emitted no sha; mergeCommit appears on refresh ───────
render_case "merged=true pr=42"
run_case "empty sha refreshed" MOCK_CHECKS_SEQ="${GREEN}" \
    MOCK_SHA_SEQ="|${SHA}"
grep -q "^sha=${SHA}$" "${OUT}" || fail "the verdict must carry the refreshed sha"
[ "$(head -n 1 "${OUT}")" = "merge_ci=green" ] || fail "expected green after the refreshed sha's CI came back green"
[ "$(counter merge_commit)" -eq 2 ] || fail "the sha refresh must poll mergeCommit until it materializes (empty first, sha second), got $(counter merge_commit)"

# ── Scenario G: sha never materializes => timeout, fail closed ────────────────
run_case "sha never appears" MOCK_SHA_SEQ="" \
    MERGE_CI_POLL_SECONDS=0 MERGE_CI_MAX_SECONDS=1
[ "$rc" -eq 0 ] || fail "the timeout verdict must exit 0, got rc=$rc"
[ "$(head -n 1 "${OUT}")" = "merge_ci=timeout" ] || fail "an unresolvable sha must never verdict green"
[ "$(counter checks)" -eq 0 ] || fail "no checks poll may run without a sha, got $(counter checks)"

# ── Scenario H: merge_pr output without a pr number => step failure ───────────
render_case "merged=true (pr number lost)"
run_case "pr number missing" MOCK_CHECKS_SEQ="${GREEN}"
[ "$rc" -ne 0 ] || fail "losing the PR number must fail the step (fail closed), got rc=0"
! grep -q "^merge_ci=" "${OUT}" || fail "no typed verdict may be emitted on a wiring failure"
grep -q "could not parse the pull request number" "${ERR}" || fail "stderr must name the wiring failure"

# ── Scenario I: transport blip on the checks poll => gh_retry absorbs it ──────
render_case "merged=true pr=42 merge_sha=${SHA}"
run_case "transport blip on the checks poll" MOCK_FAIL_CLASS=checks MOCK_FAIL_FIRST=1 MOCK_CHECKS_SEQ="${GREEN}"
[ "$rc" -eq 0 ] || fail "expected rc 0 after the transports cleared, got rc=$rc"
[ "$(head -n 1 "${OUT}")" = "merge_ci=green" ] || fail "expected the green verdict after the blip"
[ "$(counter checks)" -eq 2 ] || fail "expected 2 checks calls (1 failed + 1 success), got $(counter checks)"

# ── Compile-level pins: gate wiring in main.chcl ──────────────────────────────
echo "==> Checking merge gate wiring (KB-272)"

merge_block="$(sed -n '/^step "merge_pr" {/,/^}/p' "${MAIN}")"
printf '%s\n' "$merge_block" | grep -q "next = step.wait_merge_ci" || \
    fail "merge_pr's success arm must route to wait_merge_ci"
! printf '%s\n' "$merge_block" | grep -q "teardown_worktree" || \
    fail "merge_pr must not reach teardown_worktree directly anymore"

wait_block="$(sed -n '/^step "wait_merge_ci" {/,/^}/p' "${MAIN}")"
printf '%s\n' "$wait_block" | grep -q 'timeout = "3600s"' || \
    fail "wait_merge_ci step timeout must be the 3600s envelope above any sane budget (KB-328)"
printf '%s\n' "$wait_block" | grep -q "next = switch.route_merge_ci" || \
    fail "wait_merge_ci success must route through the verdict switch"
printf '%s\n' "$wait_block" | grep -q "data.internal.merge_ci_verdict.value" || \
    fail "wait_merge_ci must capture its verdict for the assistance lane"
printf '%s\n' "$wait_block" | grep -q "merge_ci_failed" || \
    fail "wait_merge_ci's step-failure arm must type the merge_ci_failed reason"

route_block="$(sed -n '/^switch "route_merge_ci" {/,/^}/p' "${MAIN}")"
green_line="$(printf '%s\n' "$route_block" | grep -n 'startswith(steps.wait_merge_ci.stdout, "merge_ci=green")' | head -1 | cut -d: -f1)"
[ -n "$green_line" ] || fail "route_merge_ci must key on the green verdict"
default_line="$(printf '%s\n' "$route_block" | grep -n 'default { next = step.fail_merge_ci }' | head -1 | cut -d: -f1)"
[ -n "$default_line" ] || fail "route_merge_ci must fail closed to the assistance lane"
[ "$green_line" -lt "$default_line" ] || fail "the green arm must precede the default arm"
teardown_next="$(awk -v arm="$green_line" 'NR == arm + 1 { print; exit }' <<< "$route_block")"
printf '%s\n' "$teardown_next" | grep -q "step.teardown_worktree" || \
    fail "the green arm must proceed to the existing done arm (teardown_worktree)"

fail_block="$(sed -n '/^step "fail_merge_ci" {/,/^}/p' "${MAIN}")"
[ "$(printf '%s\n' "$fail_block" | grep -c 'next = state.failed')" -eq 2 ] || \
    fail "both fail_merge_ci outcome arms must fail the run"
[ "$(printf '%s\n' "$fail_block" | grep -c 'merge_ci_failed: ')" -eq 2 ] || \
    fail "both fail_merge_ci outcome arms must write the typed merge_ci_failed reason"
printf '%s\n' "$fail_block" | grep -q "data.internal.pr_url.value" || \
    fail "the typed reason must carry the PR URL"

# merge_pr.sh emits the squash sha on both success arms (fresh + already-merged).
[ "$(grep -c 'echo "merge_sha=' "${MERGE_TEMPLATE}")" -eq 2 ] || \
    fail "merge_pr.sh must emit merge_sha on both success arms (fresh and already merged)"
[ "$(grep -c -- '--json mergeCommit' "${MERGE_TEMPLATE}")" -eq 2 ] || \
    fail "merge_pr.sh must resolve the sha via gh pr view --json mergeCommit"

# The bound is project-configurable (KB-328): the script's deadline default
# renders from the tree var merge_ci_max_seconds; env MERGE_CI_MAX_SECONDS
# remains the test/ops override. Main.chcl must bind the rendered var.
grep -q 'MERGE_CI_POLL_SECONDS:-30' "${SCRIPT}" || fail "poll cadence must default to 30s"
grep -Eq '^max_seconds=[0-9]+' "${SCRIPT}" || fail "wait deadline default must render from the tree var (KB-328 project config)"
grep -Eq 'merge_ci_max[[:space:]]+= format\("%d", var\.merge_ci_max_seconds\)' "${MAIN}" || \
    fail "wait_merge_ci must render the budget via format(%d) from var.merge_ci_max_seconds (numeric vars die in templatefile at runtime)"
grep -q 'criteria_value_1' "${SCRIPT}" || fail "the sha source must be merge_pr's output, not a re-derivation"
MERGE_CI_MAX_TEMPLATE=1200 render_case "merged=true pr=42 merge_sha=${SHA}"
grep -Eq '^max_seconds=1200' "${SCRIPT}" || fail "a non-default tree budget must render into the script default (1200 expected)"

echo "==> All checks passed."