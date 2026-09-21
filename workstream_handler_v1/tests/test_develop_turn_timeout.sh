#!/usr/bin/env bash
# Regression test for CRI-275: the develop step had no step-level timeout, so
# a turn wedged on a provider request that never answers ran unbounded (run
# cri-274-1790005670: 51+ min silent before a manual kill).
#
# Validates the pair_programming_loop develop turn ceiling and its no-verdict
# routing:
#   - develop declares a step-level timeout (60m) sized to the checkpoint
#     discipline's turn limit, and review declares the same ceiling for the
#     same wedge class;
#   - a timed-out step produces outcome "failure" (engine emits
#     "context deadline exceeded" as failure — verified against criteria
#     v0.5.22), so develop routes "failure" into the checkpoint loop
#     (push_checkpoint verifies the pushed commit, the next turn resumes);
#   - develop routes unmatched agent verdicts ("default") the same way, and
#     keeps need_help terminal (explicit agent decision).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORKFLOW_DIR="${SCRIPT_DIR}/../workflows/pair_programming_loop"
COMPILE_OUT="$(mktemp)"
trap 'rm -f "${COMPILE_OUT}"' EXIT

echo "==> Compiling pair_programming_loop..."
cd "${SCRIPT_DIR}/.."
criteria compile "${WORKFLOW_DIR}" >"${COMPILE_OUT}" 2>/dev/null

echo "==> Checking develop declares a 60m step-level timeout..."
jq -e '.steps[] | select(.name == "develop") | .timeout == "1h0m0s"' "${COMPILE_OUT}" >/dev/null

echo "==> Checking review declares the same 60m ceiling..."
jq -e '.steps[] | select(.name == "review") | .timeout == "1h0m0s"' "${COMPILE_OUT}" >/dev/null

echo "==> Checking develop routes no-verdict turns (failure) into the checkpoint loop..."
jq -e '.steps[] | select(.name == "develop") | .outcomes[] | select(.name == "failure") | .next == "push_checkpoint"' "${COMPILE_OUT}" >/dev/null

echo "==> Checking develop keeps its agent-verdict routing..."
jq -e '.steps[] | select(.name == "develop") | .outcomes[] | select(.name == "checkpoint") | .next == "push_checkpoint"' "${COMPILE_OUT}" >/dev/null
jq -e '.steps[] | select(.name == "develop") | .outcomes[] | select(.name == "need_help") | .next == "failed"' "${COMPILE_OUT}" >/dev/null
jq -e '.steps[] | select(.name == "develop") | .outcomes[] | select(.name == "ready_for_review") | .next == "write_work_log"' "${COMPILE_OUT}" >/dev/null

echo "==> Checking push_checkpoint still fails the run when the push never landed..."
jq -e '.steps[] | select(.name == "push_checkpoint") | .outcomes[] | select(.name == "failure") | .next == "failed"' "${COMPILE_OUT}" >/dev/null

echo "==> Checking develop keeps its visit bound and default catch-all..."
# The compiler drops step-level "default" outcomes from its JSON (they are not
# part of the named-outcome set), and its static reachability check does not
# follow default edges — but the engine honors them at runtime (verified with
# a local probe: an unmatched outcome transitions viaOutcome="default").
# Both edges are therefore asserted at source level.
grep -Eq 'outcome "default"[[:space:]]*\{[[:space:]]*next = step\.push_checkpoint[[:space:]]*\}' "${WORKFLOW_DIR}/main.chcl"
grep -Eq 'max_visits = var\.max_agent_visits' "${WORKFLOW_DIR}/main.chcl"

echo "==> All checks passed."
