#!/usr/bin/env bash
# Regression test for CRI-91: reviewer loop bounded retry on malformed
# tool-call / missing-outcome failure.
#
# Validates that the pr_reviewer_loop workflow compiles and that its compiled
# form contains the bounded retry switch (route_review_retry) wired back to
# pr_review, guarded by the _review_attempts counter and max_review_visits
# variable.

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

echo "==> Checking retry switch routes back to pr_review while under budget..."
jq -e '.switches[] | select(.name == "route_review_retry") | .conditions[0] | .match == "data.internal._review_attempts.value < var.max_review_visits" and .next == "pr_review"' "${COMPILE_OUT}" >/dev/null

echo "==> Checking retry switch falls through to failed when budget exhausted..."
jq -e '.switches[] | select(.name == "route_review_retry") | .default_next == "failed"' "${COMPILE_OUT}" >/dev/null

echo "==> Checking pr_review failure/default outcomes route to retry switch..."
jq -e '.steps[] | select(.name == "pr_review") | .outcomes[] | select(.next == "route_review_retry")' "${COMPILE_OUT}" >/dev/null

echo "==> All checks passed."
