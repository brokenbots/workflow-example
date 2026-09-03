#!/usr/bin/env bash
# Regression test for CRI-92: post_review is resilient to a pre-existing
# pending review by the reviewer identity.
#
# Mocks gh(1) and exercises:
#   - no pending review -> creates a new review
#   - pending review exists -> submits the existing review
#   - create fails (422) + pending review found -> submits the existing review
#   - pending review exists but submit fails -> exits with diagnostic naming it
#   - reviewer == author -> keeps the existing guard and exits 1

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TEMPLATE="${SCRIPT_DIR}/../workflows/pr_reviewer_loop/scripts/post_review.sh.tftpl"
WORK_DIR="$(mktemp -d)"
trap 'rm -rf "${WORK_DIR}"' EXIT

FAKE_GH="${WORK_DIR}/gh"
SCRIPT="${WORK_DIR}/post_review.sh"
OUT="${WORK_DIR}/stdout"
ERR="${WORK_DIR}/stderr"

# Render the template into an executable script with fixed test values.
# shellquote in Criteria renders string literals safely; for the harness we
# use simple quoted values.
sed -e 's/{{ *\.criteria_value_1 *| *shellquote *}}/"42"/g' \
    -e 's/{{ *\.criteria_value_2 *| *shellquote *}}/"APPROVE"/g' \
    -e 's/{{ *\.criteria_value_3 *| *shellquote *}}/"looks good"/g' \
    "${TEMPLATE}" > "${SCRIPT}"
chmod +x "${SCRIPT}"

# Generate a fake gh that reads scenario state from the environment.
cat > "${FAKE_GH}" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail

COUNTER="${MOCK_COUNTER_FILE:-/dev/null}"

bump_counter() {
    local n=0
    if [[ -f "$COUNTER" ]]; then
        n=$(cat "$COUNTER")
    fi
    n=$((n + 1))
    printf '%d\n' "$n" > "$COUNTER"
    printf '%d\n' "$n"
}

# Flatten arguments for matching.
args="$*"

if [[ "$args" == *"api user"*"--jq .login"* ]]; then
    printf '%s\n' "${MOCK_REVIEWER:-reviewer-bot}"
    exit 0
fi

if [[ "$args" == *"pr view"*"--json author"*"--jq .author.login"* ]]; then
    printf '%s\n' "${MOCK_AUTHOR:-human-dev}"
    exit 0
fi

if [[ "$args" == *"pulls/42/reviews"*"--jq"* ]]; then
    count=$(bump_counter)
    # Recovery case: empty on first lookup, then configured id on later lookups.
    if [[ -n "${MOCK_RECOVERY_PENDING_ID:-}" ]]; then
        if [[ "$count" -eq 1 ]]; then
            exit 0
        fi
        printf '%s\n' "$MOCK_RECOVERY_PENDING_ID"
        exit 0
    fi
    # Normal case: return the configured pending review id, or nothing if unset.
    if [[ -n "${MOCK_PENDING_ID:-}" ]]; then
        printf '%s\n' "$MOCK_PENDING_ID"
    fi
    exit 0
fi

if [[ "$args" == *"pulls/42/reviews/"*"/events"*"-X POST"* ]]; then
    if [[ "${MOCK_SUBMIT_FAIL:-0}" == "1" ]]; then
        echo "HTTP 422: cannot submit pending review" >&2
        exit 1
    fi
    exit 0
fi

if [[ "$args" == *"pulls/42/reviews"*"-X POST"*"event="* ]]; then
    touch "${MOCK_CREATE_SENTINEL:-/dev/null}"
    if [[ "${MOCK_CREATE_FAIL:-0}" == "1" ]]; then
        echo "HTTP 422: review cannot be submitted, pending review exists" >&2
        exit 1
    fi
    exit 0
fi

echo "unexpected gh invocation: $args" >&2
exit 99
MOCK
chmod +x "${FAKE_GH}"

run_case() {
    local name="$1"
    shift
    local -a env_vars=("$@")
    echo "==> ${name}"
    : > "${OUT}"
    : > "${ERR}"
    local counter="${WORK_DIR}/counter_$(printf '%s' "$name" | tr -c 'a-zA-Z0-9' '_')"
    local sentinel="${WORK_DIR}/create_$(printf '%s' "$name" | tr -c 'a-zA-Z0-9' '_')"
    rm -f "$counter" "$sentinel"
    if env "${env_vars[@]}" "MOCK_COUNTER_FILE=$counter" "MOCK_CREATE_SENTINEL=$sentinel" PATH="${WORK_DIR}:${PATH}" bash "${SCRIPT}" >"${OUT}" 2>"${ERR}"; then
        rc=0
    else
        rc=$?
    fi
}

fail() {
    echo "FAIL: $1" >&2
    echo "--- stdout ---" >&2
    cat "${OUT}" >&2 || true
    echo "--- stderr ---" >&2
    cat "${ERR}" >&2 || true
    exit 1
}

# 1. No pending review -> create succeeds.
run_case "create when no pending review exists" \
    MOCK_REVIEWER=reviewer-bot MOCK_AUTHOR=human-dev MOCK_PENDING_ID="" MOCK_CREATE_FAIL=0
[ "$rc" -eq 0 ] || fail "expected success when creating a new review, got rc=$rc"
[ -f "${WORK_DIR}/create_create_when_no_pending_review_exists" ] || fail "create POST was not invoked"
grep -q "review posted by reviewer-bot" "${OUT}" || fail "missing 'review posted' confirmation"

# 2. Pending review exists -> submit it instead of creating.
run_case "submit existing pending review" \
    MOCK_REVIEWER=reviewer-bot MOCK_AUTHOR=human-dev MOCK_PENDING_ID=12345 MOCK_CREATE_FAIL=0
[ "$rc" -eq 0 ] || fail "expected success submitting pending review, got rc=$rc"
[ ! -f "${WORK_DIR}/create_submit_existing_pending_review" ] || fail "create POST should not be invoked when pending review exists"
grep -q "submitting existing pending review 12345 for reviewer-bot" "${ERR}" || fail "missing pending-review diagnostic"
grep -q "review submitted by reviewer-bot" "${OUT}" || fail "missing 'review submitted' confirmation"

# 3. Create fails with 422 + pending review found -> recover and submit it.
run_case "recover from 422 by submitting pending review" \
    MOCK_REVIEWER=reviewer-bot MOCK_AUTHOR=human-dev MOCK_RECOVERY_PENDING_ID=67890 MOCK_CREATE_FAIL=1
[ "$rc" -eq 0 ] || fail "expected success after 422 recovery, got rc=$rc"
[ -f "${WORK_DIR}/create_recover_from_422_by_submitting_pending_review" ] || fail "create POST was not invoked during recovery"
grep -q "submitting existing pending review 67890 for reviewer-bot" "${ERR}" || fail "missing recovery diagnostic"
grep -q "review submitted by reviewer-bot" "${OUT}" || fail "missing 'review submitted' confirmation"

# 4. Pending review exists but submit fails -> clear diagnostic naming the review.
run_case "fail clearly when pending review cannot be submitted" \
    MOCK_REVIEWER=reviewer-bot MOCK_AUTHOR=human-dev MOCK_PENDING_ID=11111 MOCK_CREATE_FAIL=0 MOCK_SUBMIT_FAIL=1
[ "$rc" -ne 0 ] || fail "expected failure when pending review submit fails"
grep -q "submitting existing pending review 11111 for reviewer-bot" "${ERR}" || fail "missing named pending-review diagnostic"

# 5. Reviewer == author -> existing guard still fails.
run_case "guard against reviewer matching author" \
    MOCK_REVIEWER=human-dev MOCK_AUTHOR=human-dev
[ "$rc" -ne 0 ] || fail "expected failure when reviewer equals author"
grep -q "reviewer identity matches PR author: human-dev" "${ERR}" || fail "missing author-match diagnostic"

echo "==> All checks passed."
