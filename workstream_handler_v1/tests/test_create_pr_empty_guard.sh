#!/usr/bin/env bash
# Regression test for KB-49: the PR step must fail loudly when create_pr
# reports success without producing a pull request URL, instead of handing
# the workflow an empty pr_url to spend as success.
#
# Two layers:
#   - behavioural: the rendered create_pr script exits 1 with
#     pr_create_failed=true / names the missing URL when `gh pr create`
#     returns 0 with no pull request URL in its output (the documented
#     silent-empty mechanism), still succeeds for a real URL (plain or
#     embedded in banner text) and for reusing an existing OPEN PR;
#   - structural: the compiled workflow cannot route an empty pr_url into
#     the reviewer loop or the reconcile path — create_pr and store_pr_url
#     are guarded by route_created_pr / route_stored_pr which fail the run
#     after the retry budget is exhausted.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="${SCRIPT_DIR}/.."
WORK_ROOT="$(mktemp -d)"
trap 'rm -rf "${WORK_ROOT}"' EXIT

command -v criteria >/dev/null 2>&1 || {
    echo "FAIL: criteria is not installed" >&2
    exit 1
}
command -v jq >/dev/null 2>&1 || {
    echo "FAIL: jq not found" >&2
    exit 1
}

FAILED=0
ok() {
    echo "PASS: $1"
}
fail() {
    FAILED=$((FAILED + 1))
    echo "FAIL: $1" >&2
}

echo "==> Rendering create_pr.sh.tftpl with fixed inputs"
# shellquote in Criteria renders string literals safely; for the harness we
# substitute plain, shell-safe test values (same trick as
# test_post_review_pending.sh). The credential helper is stubbed to a no-op:
# local file:// remotes never consult it and it has its own dedicated test.
RENDERED="${WORK_ROOT}/create_pr.sh"
# The gh_retry shim (KB-219) is spliced in raw at its marker — the same
# wrapper body Criteria injects today — before the sed substitutions.
RENDER_TEMPLATE="${WORK_ROOT}/create_pr.template"
gh_retry_line="$(grep -n '{{ *\.gh_retry *}}' "${ROOT_DIR}/scripts/create_pr.sh.tftpl" | head -1 | cut -d: -f1)"
{
    head -n $((gh_retry_line - 1)) "${ROOT_DIR}/scripts/create_pr.sh.tftpl"
    cat "${ROOT_DIR}/scripts/gh_retry.sh.tftpl"
    tail -n "+$((gh_retry_line + 1))" "${ROOT_DIR}/scripts/create_pr.sh.tftpl"
} >"${RENDER_TEMPLATE}"
sed -e 's/{{ *\.criteria_value_1 *| *shellquote *}}/develop/g' \
    -e "s@{{ *\\.criteria_value_2 *| *shellquote *}}@${WORK_ROOT}/workstream.md@g" \
    -e "s@{{ *\\.helper *}}@setup_gh_token_git_credentials() { :; }@" \
    "${RENDER_TEMPLATE}" >"${RENDERED}"
chmod +x "${RENDERED}"
printf '# KB-49 regression body\n\nThe PR body content is irrelevant here.\n' >"${WORK_ROOT}/workstream.md"

# The fake gh reads its scenario from env vars, so the heredoc must stay
# unexpanded at generation time.
cat >"${WORK_ROOT}/fake-gh.sh" <<'EOF'
#!/usr/bin/env bash
case "$1 $2" in
    "pr view")
        if [ -n "${FAKE_PR_OPEN_JSON:-}" ]; then
            printf '%s' "${FAKE_PR_OPEN_JSON}"
            exit 0
        fi
        exit 1
        ;;
    "pr create")
        case "${FAKE_CREATE_MODE:-}" in
            url) printf 'https://github.com/example-org/example-repo/pull/123'; exit 0 ;;
            mixed) printf 'Creating a pull request for example-org/example-repo:\nhttps://github.com/example-org/example-repo/pull/123'; exit 0 ;;
            error) printf 'No commits between develop and KB-49' >&2; exit 1 ;;
            empty) exit 0 ;;
        esac
        ;;
    *)
        echo "unexpected gh invocation: $*" >&2
        exit 64
        ;;
esac
EOF
chmod +x "${WORK_ROOT}/fake-gh.sh"

# Real git: create_pr reads the worktree's branch and pushes it to origin.
# Every scenario starts from its own throwaway workspace so pushes are
# genuinely exercised against a local bare remote.
new_workspace() {
    scenario="$1"
    ws="${WORK_ROOT}/${scenario}"
    rm -rf "${ws}"
    mkdir -p "${ws}/repo" "${ws}/bin"
    cp "${WORK_ROOT}/fake-gh.sh" "${ws}/bin/gh"
    git -C "${ws}/repo" init -q -b KB-49 2>/dev/null || git -C "${ws}/repo" init -q
    git -C "${ws}/repo" symbolic-ref HEAD refs/heads/KB-49
    git -C "${ws}/repo" -c user.email=t@e.example -c user.name=test commit -q --allow-empty -m init
    if [ "${scenario}" = "push_fails" ]; then
        git -C "${ws}/repo" remote add origin "${ws}/no-such-upstream.git"
    else
        git init -q --bare "${ws}/upstream.git"
        git -C "${ws}/repo" remote add origin "${ws}/upstream.git"
        # A second, unpushed commit makes each scenario exercise a real push.
        git -C "${ws}/repo" -c user.email=t@e.example -c user.name=test \
            commit -q --allow-empty -m "work for ${scenario}"
    fi
    printf '%s\n' "${ws}"
}

# run_scenario <name> <env-assignments> <expected rc> <expected exact stdout
# or '-' to skip the exact compare> <space-separated markers expected in the
# captured output>
run_scenario() {
    ws="$(new_workspace "$1")"
    read -ra env_parts <<<"$2"
    set +e
    (cd "${ws}/repo" && env "${env_parts[@]}" PATH="${ws}/bin:${PATH}" \
        bash "${RENDERED}" >"${ws}/stdout.txt" 2>"${ws}/stderr.txt")
    rc=$?
    set -e
    want_rc="$3"
    if [ "$rc" -ne "$want_rc" ]; then
        fail "$1: exit code ${rc}, want ${want_rc} (stderr: $(tr '\n' ' ' <"${ws}/stderr.txt"))"
    else
        ok "$1: create_pr exits ${want_rc}"
    fi
    if [ "$4" != "-" ]; then
        printf '%s' "$4" >"${ws}/want.txt"
        # Normalize a missing trailing newline on either side (the script's
        # URL output is newline-free by contract; failure reports end with
        # echo's newline).
        awk '1' "${ws}/want.txt" >"${ws}/want.norm"
        awk '1' "${ws}/stdout.txt" >"${ws}/got.norm"
        if diff -u "${ws}/want.norm" "${ws}/got.norm" >/dev/null 2>&1; then
            ok "$1: stdout matches exactly"
        else
            diff -u "${ws}/want.norm" "${ws}/got.norm" | sed 's/^/  /' >&2
            fail "$1: stdout does not match the expected output"
        fi
    fi
    for marker in $5; do
        if grep -q "$marker" "${ws}/stdout.txt" || grep -q "$marker" "${ws}/stderr.txt"; then
            ok "$1: output carries marker $marker"
        else
            fail "$1: output missing marker $marker (stderr: $(tr '\n' ' ' <"${ws}/stderr.txt"))"
        fi
    done
}

echo "==> Behavioural scenarios"

# The documented KB-49 signature: gh pr create reports success (exit 0) but
# its output carries no pull request URL — the old script printed an empty
# pr_url and the run continued as success. Now it must fail loudly. The
# failing echos land on stdout by design (they are the step's report), so the
# exact stdout is pinned for this scenario too.
run_scenario empty_create_output "FAKE_CREATE_MODE=empty" 1 \
    "pr_create_failed=true branch=KB-49
pr_create_error=no pull request URL in gh pr create output: " \
    "pr_create_failed=true" "no pull request URL in gh pr create output"

# A real URL (plain, or embedded in gh's banner text) prints exactly the URL
# on stdout — stdout is the pr_url the workflow consumes.
run_scenario url_create_output "FAKE_CREATE_MODE=url" 0 \
    "https://github.com/example-org/example-repo/pull/123" ""
run_scenario url_create_banner_text "FAKE_CREATE_MODE=mixed" 0 \
    "https://github.com/example-org/example-repo/pull/123" ""

# An existing OPEN PR is reused without pushing.
run_scenario reuse_open_pr \
    "FAKE_PR_OPEN_JSON={\"url\":\"https://github.com/example-org/example-repo/pull/7\",\"state\":\"OPEN\"}" 0 \
    "https://github.com/example-org/example-repo/pull/7" ""
# The reuse branch takes gh pr view as its evidence: origin must never have
# received the workstream commit.
if [ -n "$(git -C "${ws}/repo" rev-list KB-49 --not --remotes=origin 2>/dev/null)" ]; then
    ok "reuse path leaves the workstream commit unpushed"
else
    fail "reuse path must not push the workstream commit to origin"
fi

# A failing gh pr create still fails loudly with its diagnostics.
run_scenario create_error "FAKE_CREATE_MODE=error" 1 "-" \
    "pr_create_failed=true" "pr_create_error=No commits between"

# A rejected push fails loudly and never reaches gh pr create.
run_scenario push_fails "FAKE_CREATE_MODE=url" 1 "-" \
    "push_failed=true"

echo "==> Compiling workstream_handler_v1 for structural checks"
COMPILE_OUT="${WORK_ROOT}/handler.json"
(cd "${ROOT_DIR}" && criteria compile . >"${COMPILE_OUT}" 2>/dev/null)

echo "==> create_pr (both outcomes) goes through the empty-pr_url guard"
jq -e '.steps[] | select(.name == "create_pr") | ([.outcomes[] | select(.name == "success" and .next == "route_created_pr")] | length) == 1' "${COMPILE_OUT}" >/dev/null \
    || fail "create_pr success must route through route_created_pr"
jq -e '.steps[] | select(.name == "create_pr") | ([.outcomes[] | select(.name == "failure" and .next == "route_created_pr")] | length) == 1' "${COMPILE_OUT}" >/dev/null \
    || fail "create_pr failure must route through route_created_pr"

echo "==> route_created_pr: empty pr_url retries (bounded), then fails loudly"
jq -e '.switches[] | select(.name == "route_created_pr") | ([.conditions[] | select(.match == "data.internal.pr_url.value != \"\"" and .next == "route_reviewer_creds")] | length) == 1' "${COMPILE_OUT}" >/dev/null \
    || fail "route_created_pr must hand a non-empty pr_url to the reviewer routing"
jq -e '.switches[] | select(.name == "route_created_pr") | ([.conditions[] | select((.match | contains("pr_url_retries")) and .next == "retry_create_pr")] | length) == 1' "${COMPILE_OUT}" >/dev/null \
    || fail "route_created_pr must retry create_pr while the retry budget lasts"
jq -e '.switches[] | select(.name == "route_created_pr") | .default_next == "fail_missing_pr_url"' "${COMPILE_OUT}" >/dev/null \
    || fail "route_created_pr must fail the run loudly once the budget is exhausted"
jq -e '.steps[] | select(.name == "retry_create_pr") | ([.outcomes[] | select(.name == "success" and .next == "create_pr")] | length) == 1 and ([.outcomes[] | select(.name == "failure" and .next == "fail_missing_pr_url")] | length) == 1' "${COMPILE_OUT}" >/dev/null \
    || fail "retry_create_pr must loop back to create_pr and bail out on failure"

echo "==> fail_missing_pr_url terminally fails the run"
jq -e '.steps[] | select(.name == "fail_missing_pr_url") | ([.outcomes[] | select(.next == "failed")] | length) == 2' "${COMPILE_OUT}" >/dev/null \
    || fail "fail_missing_pr_url must fail the run on both outcomes"

echo "==> store_pr_url (the gh-empty-stdout masking path) is guarded identically"
jq -e '.steps[] | select(.name == "store_pr_url") | ([.outcomes[] | select(.name == "success" and .next == "route_stored_pr")] | length) == 1' "${COMPILE_OUT}" >/dev/null \
    || fail "store_pr_url success must route through route_stored_pr"
jq -e '.switches[] | select(.name == "route_stored_pr") | ([.conditions[] | select(.match == "data.internal.pr_url.value != \"\"" and .next == "route_pr_action")] | length) == 1' "${COMPILE_OUT}" >/dev/null \
    || fail "route_stored_pr must keep the valid-pr_url routing to route_pr_action"
jq -e '.switches[] | select(.name == "route_stored_pr") | ([.conditions[] | select((.match | contains("pr_url_retries")) and .next == "retry_store_pr_url")] | length) == 1' "${COMPILE_OUT}" >/dev/null \
    || fail "route_stored_pr must retry store_pr_url while the budget lasts"
jq -e '.switches[] | select(.name == "route_stored_pr") | .default_next == "fail_missing_pr_url"' "${COMPILE_OUT}" >/dev/null \
    || fail "route_stored_pr must fail the run loudly once the budget is exhausted"
jq -e '.steps[] | select(.name == "retry_store_pr_url") | ([.outcomes[] | select(.name == "success" and .next == "store_pr_url")] | length) == 1 and ([.outcomes[] | select(.name == "failure" and .next == "fail_missing_pr_url")] | length) == 1' "${COMPILE_OUT}" >/dev/null \
    || fail "retry_store_pr_url must loop back to store_pr_url and bail out on failure"

# criteria compile omits step writes, so pin them in the source, scoped to
# the step blocks.
echo "==> Source pins: writes, retry counters and the KB-49 diagnostics"
create_step=$(sed -n '/^step "create_pr" {/,/^}/p' "${ROOT_DIR}/main.chcl")
printf '%s' "${create_step}" | grep -q 'target = data.internal.pr_url.value' \
    && printf '%s' "${create_step}" | grep -q 'value  = output.stdout' \
    || fail "create_pr success must still write the produced pr_url"
retry_create=$(sed -n '/^step "retry_create_pr" {/,/^}/p' "${ROOT_DIR}/main.chcl")
printf '%s' "${retry_create}" | grep -q 'data.internal.pr_url_retries.value + 1' \
    || fail "retry_create_pr must consume the retry budget"
fail_step=$(sed -n '/^step "fail_missing_pr_url" {/,/^}/p' "${ROOT_DIR}/main.chcl")
printf '%s' "${fail_step}" | grep -q 'KB-49: the PR step completed with an empty pr_url' \
    || fail "fail_missing_pr_url must record the KB-49 failure reason for the run"
store_step=$(sed -n '/^step "store_pr_url" {/,/^}/p' "${ROOT_DIR}/main.chcl")
# KB-219: the gh pipeline moved into scripts/store_pr_url.sh.tftpl (rendered
# by the step body) so it can sit under the shared gh_retry wrapper; the
# pipefail contract moves with it.
printf '%s' "${store_step}" | grep -q 'store_pr_url.sh.tftpl' \
    || fail "store_pr_url must render scripts/store_pr_url.sh.tftpl (gh retry shim lives there)"
printf '%s' "${store_step}" | grep -q 'gh_retry' \
    || fail "store_pr_url render must pass the gh_retry shim into the template"
grep -q 'set -o pipefail; pr_url=$(gh pr view' "${ROOT_DIR}/scripts/store_pr_url.sh.tftpl" \
    || fail "store_pr_url script must run its gh pipeline under pipefail (a failed gh may not be masked as success)"

echo "==> The retry budget variable exists with a bounded default"
grep -q 'variable "max_pr_url_retries"' "${ROOT_DIR}/variables.chcl" \
    || fail "max_pr_url_retries must be declared in variables.chcl"
grep -Eq 'default[[:space:]]+=[[:space:]]+2' "${ROOT_DIR}/variables.chcl" \
    || fail "max_pr_url_retries must default to a small bounded number"

if [ "${FAILED}" -gt 0 ]; then
    echo "FAILED: ${FAILED} assertion(s)" >&2
    exit 1
fi
echo "PASS: create_pr empty-output guard verified"