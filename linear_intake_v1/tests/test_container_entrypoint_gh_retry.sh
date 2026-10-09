#!/usr/bin/env bash
# Regression test for KB-219: the linear_intake container entrypoint routes
# its GitHub API surfaces (gh api user x2, gh repo clone) through the shared
# bounded-backoff gh_retry wrapper. The vendored copy is consumed by `source`
# in a /bin/sh entrypoint — not rendered by templatefile — so it must stay
# byte-identical to the canonical copy and run under plain POSIX sh.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TREE_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
ENTRYPOINT="${TREE_DIR}/container-entrypoint.sh"
WRAPPER="${TREE_DIR}/scripts/gh_retry.sh"
ROOT_WRAPPER="$(cd "${TREE_DIR}/.." && pwd)/workstream_handler_v1/scripts/gh_retry.sh.tftpl"

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

# ── Structural: entrypoint wiring matches the wrapper contract ───────────────
check "wrapper vendored (identical to canonical copy)" cmp -s "${WRAPPER}" "${ROOT_WRAPPER}"
check "wrapper is POSIX sh clean"    sh -n "${WRAPPER}"
check "entrypoint parses"            sh -n "${ENTRYPOINT}"
check "entrypoint sources wrapper"   grep -q 'scripts/gh_retry.sh' "${ENTRYPOINT}"
check "all gh sites wrapped"         test "$(grep -c 'gh_retry gh' "${ENTRYPOINT}")" = "3"
check "no unwrapped gh api/clone"    test "$(grep -n 'gh api\|gh repo clone' "${ENTRYPOINT}" | grep -cv 'gh_retry')" = "0"

# ── Behavioral: vendored wrapper + env-prefixed gh call under /bin/sh ────────
# Mirrors the entrypoint's call shape — `GH_TOKEN=... gh_retry gh api user
# --jq .login` inside a $( ) capture — with a stub gh driven by a retry
# policy. Backoff is zeroed so the suite stays fast; attempt caps still bind.
WORK_ROOT="$(mktemp -d)"
trap 'rm -rf "${WORK_ROOT}"' EXIT

cat > "${WORK_ROOT}/gh" <<'EOF'
#!/bin/sh
n=$(cat "${IDX_FILE}" 2>/dev/null || echo 0)
n=$((n + 1))
printf '%s\n' "$n" > "${IDX_FILE}"
line=$(tail -n +"$n" "${POLICY}" 2>/dev/null | head -1)
[ -n "$line" ] || line=$(tail -n 1 "${POLICY}")
rc=${line%%|*}; rest=${line#*|}; out=${rest%%|*}; err=${rest#*|}
[ -n "$out" ] && printf '%s\n' "$out"
[ -n "$err" ] && printf '%s\n' "$err" >&2
exit "$rc"
EOF
chmod +x "${WORK_ROOT}/gh"

cat > "${WORK_ROOT}/probe.sh" <<'EOF'
#!/bin/sh
set -eu
. "PROBE_WRAPPER"
scenario="$1"
# The entrypoint consumes these calls inside $( ) captures under set -e;
# mirror that, but record the rc instead of exiting so both verdicts are
# expressible.
set +e
u=$(GH_TOKEN="probe-token" gh_retry gh api user --jq .login 2>"${scenario}/err")
echo "$?" > "${scenario}/rc"
printf '%s' "$u" > "${scenario}/out"
EOF

cat > "${WORK_ROOT}/probe_run.sh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
scenario="$1"
cd "${scenario}"
export POLICY="${scenario}/policy"
export IDX_FILE="${scenario}/idx"
export PATH="@WORKROOT@:/usr/bin:/bin"
export GH_RETRY_BACKOFF_SECONDS=0
sh "@WORKROOT@/probe.sh" "${scenario}"
EOF
sed -i -e "s|PROBE_WRAPPER|${WRAPPER}|" -e "s|@WORKROOT@|${WORK_ROOT}|g" "${WORK_ROOT}/probe_run.sh"
sed -i "s|PROBE_WRAPPER|${WRAPPER}|" "${WORK_ROOT}/probe.sh"

probe() {
    local scenario="$1" policy="$2"
    mkdir -p "${WORK_ROOT}/${scenario}"
    printf '%s\n' "${policy}" > "${WORK_ROOT}/${scenario}/policy"
    bash "${WORK_ROOT}/probe_run.sh" "${WORK_ROOT}/${scenario}"
}

# TLS blips then success: bounded retries converge, stdout kept clean for the
# capture, attempt/class evidence on stderr.
probe blips '1||tls: handshake timeout
1||tls: handshake timeout again
0|octocat|'
check "blips converge rc=0"      test "$(cat "${WORK_ROOT}/blips/rc")" = "0"
check "blips login on stdout"    grep -qx 'octocat' "${WORK_ROOT}/blips/out"
check "blips retried twice"      test "$(grep -c 'result=retry' "${WORK_ROOT}/blips/err")" = "2"
check "blips class logged"       grep -q 'class=tls_handshake_timeout' "${WORK_ROOT}/blips/err"
check "blips success logged"     grep -q 'result=success' "${WORK_ROOT}/blips/err"

# Non-transient 422: pass through on the first failure — never retried.
probe e422 '1| |422 Unprocessable Entity'
check "e422 rc preserved"        test "$(cat "${WORK_ROOT}/e422/rc")" = "1"
check "e422 not retried"         test "$(grep -c 'result=retry' "${WORK_ROOT}/e422/err")" = "0"
check "e422 pass_through logged" grep -q 'result=pass_through class=non_transient' "${WORK_ROOT}/e422/err"

if [ "${FAILURES}" -gt 0 ]; then
    echo "FAILED: ${FAILURES} assertion(s)" >&2
    exit 1
fi
echo "PASS: linear_intake gh_retry wiring verified"
