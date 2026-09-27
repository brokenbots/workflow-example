#!/usr/bin/env bash
# Regression test for CRI-91 + KB-24: reviewer loop bounded retry on malformed
# tool-call / missing-outcome failure, plus the KB-24 scope-loss gate.
#
# Validates that the pr_reviewer_loop workflow compiles and that its compiled
# form contains the bounded retry switch (route_review_retry) wired back to
# pr_review. KB-24: the switch's first condition routes an engine-synthesized
# failure (empty _failure_reason — timeout / crash / lost scope session, no
# output object) straight to failed so the parent re-provisions the scope;
# live-session failures retry in place under the _review_attempts counter and
# max_review_visits variable.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORKFLOW_DIR="${SCRIPT_DIR}/../workflows/pr_reviewer_loop"
COMPILE_OUT="$(mktemp)"
trap 'rm -f "${COMPILE_OUT}"' EXIT

echo "==> Compiling pr_reviewer_loop..."
cd "${SCRIPT_DIR}/.."
criteria compile "${WORKFLOW_DIR}" >"${COMPILE_OUT}" 2>/dev/null

echo "==> Checking retry switch exists..."
jq -e '.switches[] | select(.name == "route_review_retry")' "${COMPILE_OUT}" >/dev/null

echo "==> Checking scope-loss failures skip the in-place retry (KB-24)..."
jq -e '.switches[] | select(.name == "route_review_retry") | .conditions[0] | .match == "data.internal._failure_reason.value == \"\"" and .next == "failed"' "${COMPILE_OUT}" >/dev/null

echo "==> Checking retry switch routes back to pr_review while under budget..."
jq -e '.switches[] | select(.name == "route_review_retry") | .conditions[1] | .match == "data.internal._review_attempts.value < var.max_review_visits" and .next == "pr_review"' "${COMPILE_OUT}" >/dev/null

echo "==> Checking retry switch falls through to failed when budget exhausted..."
jq -e '.switches[] | select(.name == "route_review_retry") | .default_next == "failed"' "${COMPILE_OUT}" >/dev/null

echo "==> Checking pr_review failure/default outcomes route to retry switch..."
jq -e '.steps[] | select(.name == "pr_review") | .outcomes[] | select(.next == "route_review_retry")' "${COMPILE_OUT}" >/dev/null

echo "==> Checking pr_review carries the KB-24 attempt timeout..."
jq -e '.steps[] | select(.name == "pr_review") | .timeout == "1h0m0s"' "${COMPILE_OUT}" >/dev/null

echo "==> Checking the failure reason is surfaced to the parent (KB-24)..."
jq -e '.outputs[] | select(.name == "failure_reason")' "${COMPILE_OUT}" >/dev/null

echo "==> All checks passed."
