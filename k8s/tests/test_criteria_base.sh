#!/usr/bin/env bash
set -euo pipefail

# Static structural checks for the minimal criteria base image (CRI-230, M6.1).
#
# These run in CI without a container tool; the docker-run smoke test
# (criteria-base/tests/smoke_test.sh) proves the same criteria on a host with
# docker. Checks here guard the image contract so it cannot silently drift:
#
#   - criteria main pinned at 28777aa+ with a build that fails closed on a
#     checkout mismatch;
#   - runtime ships only git + ca-certificates (no node/gh/jq, no baked
#     /workflows tree);
#   - uid 10001, CRITERIA_HOME=/data/criteria, restricted entrypoint;
#   - the operator is NOT wired to the new image (CRI-231 owns that).

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
DOCKERFILE="$REPO_ROOT/criteria-base/Dockerfile"
ENTRYPOINT="$REPO_ROOT/criteria-base/entrypoint.sh"
MAKEFILE="$REPO_ROOT/Makefile"

fail() {
    echo "FAIL: $1" >&2
    exit 1
}

[ -f "$DOCKERFILE" ] || fail "criteria-base/Dockerfile missing"
[ -f "$ENTRYPOINT" ] || fail "criteria-base/entrypoint.sh missing"

have() {
    grep -qF -- "$1" "$2"
}

# --- pinned criteria main commit (merged CRI-215..229 chain) ---------------
have 'ARG CRITERIA_COMMIT=28777aacc3cfbe85005ddb27f548116e692c0eb4' "$DOCKERFILE" || \
    fail "Dockerfile does not pin criteria main at 28777aa"
have 'git clone' "$DOCKERFILE" || fail "Dockerfile does not clone criteria"
grep -Eq 'test "\$\(git rev-parse HEAD\)" = "\$\{CRITERIA_COMMIT\}"' "$DOCKERFILE" || \
    fail "Dockerfile build does not fail closed if the checkout is not the pinned commit"

# --- minimal runtime: git + ca-certs only ----------------------------------
have 'FROM alpine:' "$DOCKERFILE" || fail "runtime stage must build on alpine"
grep -Eq 'apk add --no-cache ca-certificates git$' "$DOCKERFILE" || \
    fail "runtime stage must install exactly ca-certificates and git"
for tool in nodejs node npm gh jq; do
    if grep -Eq "apk add.*\b$tool\b" "$DOCKERFILE"; then
        fail "image must not install $tool"
    fi
done
have 'COPY --from=builder /out/criteria /usr/local/bin/criteria' "$DOCKERFILE" || \
    fail "Dockerfile must install the criteria binary from the builder stage"
if grep -Eq 'COPY [^/]*\S+[[:space:]]+/workflows' "$DOCKERFILE"; then
    fail "image must not ship a baked /workflows tree"
fi

# --- restricted securityContext posture ------------------------------------
have 'adduser -S -u 10001 -G criteria' "$DOCKERFILE" || \
    fail "Dockerfile must create a non-root uid 10001 user"
have 'USER 10001' "$DOCKERFILE" || fail "Dockerfile must run as USER 10001 (non-root)"
have 'ENV HOME=/home/criteria' "$DOCKERFILE" || fail "Dockerfile must set HOME"
have 'CRITERIA_HOME=/data/criteria' "$DOCKERFILE" || \
    fail "Dockerfile must set CRITERIA_HOME=/data/criteria (data PVC)"
have 'mkdir -p /data/criteria' "$DOCKERFILE" || \
    fail "Dockerfile must create /data/criteria for the data PVC"
have 'ENTRYPOINT ["/usr/local/bin/criteria-base-entrypoint"]' "$DOCKERFILE" || \
    fail "Dockerfile must use the criteria-base entrypoint"

# --- entrypoint contract ----------------------------------------------------
[ -x "$ENTRYPOINT" ] || fail "entrypoint.sh must be executable"
have 'WORKFLOW_URL' "$ENTRYPOINT" || fail "entrypoint must read WORKFLOW_URL"
have 'WORKFLOW_REF' "$ENTRYPOINT" || fail "entrypoint must read WORKFLOW_REF"
have '--workflow-ref' "$ENTRYPOINT" || \
    fail "entrypoint must enforce the route ref via criteria apply --workflow-ref (CRI-226)"
grep -Eq 'no baked /workflows tree" 64$' "$ENTRYPOINT" || \
    fail "entrypoint must fail closed (exit 64) when WORKFLOW_URL is undeclared"
have 'CRITERIA_HOME:-/data/criteria' "$ENTRYPOINT" || \
    fail "entrypoint must default CRITERIA_HOME to /data/criteria"
grep -Eq 'mount the data PVC at /data" 70$' "$ENTRYPOINT" || \
    fail "entrypoint must fail (exit 70) when CRITERIA_HOME is not writable"
grep -Eq '^exec .*criteria_bin' "$ENTRYPOINT" || \
    fail "entrypoint must exec the criteria binary"
at_arg="\"\$@\""  # the literal "$@" forwarded-args token
grep -qF "$at_arg" "$ENTRYPOINT" || \
    fail "entrypoint must exec criteria apply with forwarded arguments"

# --- Makefile wiring: build/publish targets, tests, lint -------------------
have 'CRITERIA_BASE_IMAGE ?= localhost:5000/criteria-base' "$MAKEFILE" || \
    fail "Makefile must define CRITERIA_BASE_IMAGE ?= localhost:5000/criteria-base"
have 'build-criteria-base:' "$MAKEFILE" || \
    fail "Makefile must provide a build-criteria-base target"
have 'build-criteria-base-push:' "$MAKEFILE" || \
    fail "Makefile must provide a build-criteria-base-push target"
grep -q 'criteria-base/Dockerfile' "$MAKEFILE" || \
    fail "Makefile build-criteria-base must build criteria-base/Dockerfile"
# Both regression tests must be wired into `make test` and shellchecked in lint.
for t in test_criteria_base.sh test_criteria_base_entrypoint.sh; do
    grep -Eq "\./k8s/tests/$t$" "$MAKEFILE" || \
        fail "Makefile test target must run k8s/tests/$t"
    grep -Eq "^\s+k8s/tests/$t( \\\\)?$" "$MAKEFILE" || \
        fail "Makefile lint must shellcheck k8s/tests/$t"
done

# --- operator stays on the baked image (wiring deferred to CRI-231) --------
if grep -rq 'criteria-base' "$REPO_ROOT/criteria-k8s/"; then
    fail "operator (criteria-k8s/) must not reference criteria-base: wiring is CRI-231"
fi
grep -q 'localhost:5000/linear-intake-remote:dev' "$REPO_ROOT/criteria-k8s/cmd/operator/main.go" || \
    fail "DEFAULT_CRITERIA_IMAGE must remain the baked workflow image"

echo "PASS: criteria-base image structure (CRI-230)"