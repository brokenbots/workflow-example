#!/usr/bin/env bash
# Execution test for the KB-219 store_pr_url step command.
#
# store_pr_url.sh.tftpl renders into a step's `command` on the happy path of
# every workstream_handler_v1 run, so the rendered artifact is exercised the
# same way the create_pr/post_review harnesses do: splice the shared gh_retry
# wrapper at its marker, execute against stub gh/git binaries, and assert the
# emitted URL, the exit status, and a clean stderr channel. This is what would
# have caught the non-comment `//` head regression (reviewer finding 1): as
# rendered, those lines executed as a command named `//`, poisoning the step's
# stderr with "Permission denied"/"not found" before the real body ran.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TEMPLATE="${SCRIPT_DIR}/../scripts/store_pr_url.sh.tftpl"
RETRY_TEMPLATE="${SCRIPT_DIR}/../scripts/gh_retry.sh.tftpl"
WORK_ROOT="$(mktemp -d)"
trap 'rm -rf "${WORK_ROOT}"' EXIT

FAILED=0
ok() { echo "PASS: $1"; }
fail() {
    FAILED=$((FAILED + 1))
    echo "FAIL: $1" >&2
}

# Render: splice the wrapper raw at the marker (same body Criteria injects).
RENDERED="${WORK_ROOT}/store_pr_url.rendered"
gh_retry_line="$(grep -n '{{ *\.gh_retry *}}' "${TEMPLATE}" | head -1 | cut -d: -f1)"
{
    head -n $((gh_retry_line - 1)) "${TEMPLATE}"
    cat "${RETRY_TEMPLATE}"
    tail -n "+$((gh_retry_line + 1))" "${TEMPLATE}"
} >"${RENDERED}"
chmod +x "${RENDERED}"

# Stub gh/git: stable happy path (PR URL) and a current-branch answer.
mkdir -p "${WORK_ROOT}/bin"
cat >"${WORK_ROOT}/bin/gh" <<'STUB'
#!/bin/sh
if [ "$(printf '%s ' "$@" | grep -c '\-\-json url')" -eq 1 ]; then
    echo 'https://github.com/org/repo/pull/42'
    exit 0
fi
echo 'unexpected gh invocation: gh '"$*" >&2
exit 1
STUB
cat >"${WORK_ROOT}/bin/git" <<'STUB'
#!/bin/sh
[ "$1" = "branch" ] && [ "$2" = "--show-current" ] && echo 'KB-219' && exit 0
exec /usr/bin/env git "$@"
STUB
chmod +x "${WORK_ROOT}/bin/gh" "${WORK_ROOT}/bin/git"

# Rendered-command pin: the first non-empty line of the file Criteria sends to
# the shell adapter must be a shell comment (no `//` command heads).
first_line="$(grep -m1 -v '^[[:space:]]*$' "${RENDERED}")"
case "${first_line}" in
\#*) ok "rendered command's first non-empty line is a comment (got: ${first_line##'#'})" ;;
*)   fail "rendered command's first non-empty line is a comment (got: [${first_line}])" ;;
esac

run_rendered() { # run_rendered <out-file> <err-file> <extra-path...>
    local out_file=$1 err_file=$2
    ( export PATH="${WORK_ROOT}/bin:$PATH"
      export HOME="${WORK_ROOT}"
      export GH_RETRY_BACKOFF_SECONDS="${GH_RETRY_BACKOFF_SECONDS:-"15 30 60"}"
      "${RENDERED}" >"${out_file}" 2>"${err_file}"
    )
    echo $?
}

echo "==> Happy path: one wrapped gh call emits the URL on stdout, clean stderr"
out="${WORK_ROOT}/out"; err="${WORK_ROOT}/err"
rc=$(run_rendered "${out}" "${err}")
[ "$rc" -eq 0 ] && ok "rendered store_pr_url exits 0" || fail "rendered store_pr_url exits 0 (rc=${rc})"
[ "$(cat "${out}")" = 'https://github.com/org/repo/pull/42' ] && ok \
    "stdout is the PR URL" || fail "stdout is the PR URL (got [$(cat "${out}")])"
if [ ! -s "${err}" ]; then
    ok "stderr is empty"
else
    fail "stderr is empty (got: $(cat "${err}" | head -2))"
fi

echo "==> Failure path: a persistently-failing gh call fails the step honestly"
# The wrapper retries transient failures with the default 15/30/60s backoff —
# collapsed here so the suite stays fast; real-budget behavior has its own
# scenario in test_gh_retry.sh.
export GH_RETRY_BACKOFF_SECONDS='0 0 0'
printf 'reject' >"${WORK_ROOT}/deny"
cat >"${WORK_ROOT}/bin/gh" <<'STUB'
#!/bin/sh
echo 'fatal: unable to access .api.github.com/: TLS handshake timeout' >&2
exit 1
STUB
chmod +x "${WORK_ROOT}/bin/gh"
out2="${WORK_ROOT}/out2"; err2="${WORK_ROOT}/err2"
rc=$(run_rendered "${out2}" "${err2}")
[ "$rc" -ne 0 ] && ok "failed gh call keeps the step failing (rc=${rc})" \
    || fail "failed gh call keeps the step failing (rc=${rc})"
grep -q 'attempt=' "${err2}" && ok "failure path carries retry evidence on stderr" \
    || fail "failure path carries retry evidence on stderr"

if [ "${FAILED}" -eq 0 ]; then
    echo "PASS: store_pr_url rendered artifact verified"
    exit 0
fi
echo "FAILED: ${FAILED} assertion(s)" >&2
exit 1