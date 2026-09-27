#!/usr/bin/env bash
# Regression test for KB-24: reviewer/branch scope re-provision on session
# loss. A lost adapter-scope session used to leave the reviewer loop and
# branch_manager unconcluded and the develop run hung forever. The handler
# now:
#
#   1. re-provisions the lost scope (fresh rotate + re-dial) via dedicated
#      reprovision steps, bounded by max_reviewer_scope_retries /
#      max_branch_scope_retries;
#   2. fails with a typed failure_reason once the budget is exhausted, so
#      the parent's failure path (comment + Review column) engages;
#   3. does NOT re-provision when the failure carries a reported reason
#      (agent-decided) — that fails the run with the reason directly.
#
# Routing assertions run against the compiled graphs; write and variable
# semantics are not serialized into the compiled graph, so they are asserted
# on the workflow source.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TREE_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
COMPILE_OUT="$(mktemp)"
SUB_COMPILE_OUT="$(mktemp)"
trap 'rm -f "${COMPILE_OUT}" "${SUB_COMPILE_OUT}"' EXIT

command -v jq >/dev/null 2>&1 || { echo "FAIL: jq not found" >&2; exit 1; }

echo "==> Compiling workstream_handler_v1..."
cd "${TREE_ROOT}"
criteria compile . --format json --out "${COMPILE_OUT}" 2>/dev/null

echo "==> Compiling the reviewer subworkflow standalone..."
criteria compile "${TREE_ROOT}/workflows/pr_reviewer_loop" --format json --out "${SUB_COMPILE_OUT}" 2>/dev/null

# ── Reviewer scope reprovision (compiled routing) ────────────────────────────

echo "==> Checking the reviewer verdict routes keep their order..."
jq -e '
  .switches[] | select(.name == "route_pr_reviewer_loop") |
  ([.conditions[].next] | join(",")) == "merge_pr,triage_pr_feedback,triage_pr_feedback,failed,reprovision_reviewer_scope"
  and .default_next == "exhausted_reviewer_scope"
' "${COMPILE_OUT}" >/dev/null

echo "==> Checking the reviewer reprovision match is bounded by the retry budget..."
jq -e '
  .switches[] | select(.name == "route_pr_reviewer_loop") |
  .conditions[4].match == "data.internal.reviewer_scope_retries.value < var.max_reviewer_scope_retries"
' "${COMPILE_OUT}" >/dev/null

echo "==> Checking a reported reviewer failure reason fails the run (true reason preserved)..."
jq -e '
  .switches[] | select(.name == "route_pr_reviewer_loop") |
  .conditions[3].match == "data.internal.failure_reason.value != \"\""
  and .conditions[3].next == "failed"
' "${COMPILE_OUT}" >/dev/null

echo "==> Checking the reviewer reprovision step re-enters the reviewer loop..."
jq -e '
  .steps[] | select(.name == "reprovision_reviewer_scope") |
  ([.outcomes[] | select(.name == "success")][0].next) == "run_pr_reviewer_loop"
  and ([.outcomes[] | select(.name == "failure")][0].next) == "failed"
' "${COMPILE_OUT}" >/dev/null

echo "==> Checking budget exhaustion ends in the failed terminal..."
jq -e '
  .steps[] | select(.name == "exhausted_reviewer_scope") |
  ([.outcomes[] | select(.name == "success")][0].next) == "failed"
' "${COMPILE_OUT}" >/dev/null

# ── Branch scope reprovision (compiled routing) ──────────────────────────────

echo "==> Checking the branch failure routes through the scope-loss switch..."
jq -e '
  .steps[] | select(.name == "run_branch_manager") |
  ([.outcomes[] | select(.name == "failure")][0].next) == "route_branch_scope_reprovision"
' "${COMPILE_OUT}" >/dev/null

echo "==> Checking the branch scope-loss switch prefers reported reasons and bounded reprovision..."
jq -e '
  .switches[] | select(.name == "route_branch_scope_reprovision") |
  ([.conditions[].next] | join(",")) == "failed,reprovision_branch_scope"
  and .conditions[0].match == "data.internal.failure_reason.value != \"\""
  and .conditions[1].match == "data.internal.branch_scope_retries.value < var.max_branch_scope_retries"
  and .default_next == "exhausted_branch_scope"
' "${COMPILE_OUT}" >/dev/null

echo "==> Checking the branch reprovision step re-enters branch_manager..."
jq -e '
  .steps[] | select(.name == "reprovision_branch_scope") |
  ([.outcomes[] | select(.name == "success")][0].next) == "run_branch_manager"
  and ([.outcomes[] | select(.name == "failure")][0].next) == "failed"
' "${COMPILE_OUT}" >/dev/null

echo "==> Checking the branch budget exhaustion ends in the failed terminal..."
jq -e '
  .steps[] | select(.name == "exhausted_branch_scope") |
  ([.outcomes[] | select(.name == "success")][0].next) == "failed"
' "${COMPILE_OUT}" >/dev/null

echo "==> Checking the typed failure reason is surfaced to the parent..."
jq -e '.outputs[] | select(.name == "failure_reason")' "${COMPILE_OUT}" >/dev/null

# ── Write and variable semantics (source-level) ──────────────────────────────

echo "==> Checking the reprovision steps count their attempts..."
grep -F 'value  = data.internal.reviewer_scope_retries.value + 1' main.chcl >/dev/null
grep -F 'value  = data.internal.branch_scope_retries.value + 1' main.chcl >/dev/null

echo "==> Checking budget exhaustion writes a typed failure reason..."
grep -F 'value  = "reviewer scope reprovision budget exhausted (${var.max_reviewer_scope_retries} retries) without a review verdict (KB-24)"' main.chcl >/dev/null
grep -F 'value  = "branch scope reprovision budget exhausted (${var.max_branch_scope_retries} retries) without a reported reason (KB-24)"' main.chcl >/dev/null

echo "==> Checking helper step failures carry typed reasons for the parent..."
grep -F 'value  = "reviewer scope reprovision step failed before the review re-run (KB-24)"' main.chcl >/dev/null
grep -F 'value  = "reviewer scope reprovision exhaustion step failed while failing the run (KB-24)"' main.chcl >/dev/null
grep -F 'value  = "branch scope reprovision step failed before the branch re-run (KB-24)"' main.chcl >/dev/null
grep -F 'value  = "branch scope reprovision exhaustion step failed while failing the run (KB-24)"' main.chcl >/dev/null

echo "==> Checking a concluded review round renews the reviewer budget..."
grep -F 'value  = 0' main.chcl >/dev/null

echo "==> Checking the failure outcomes capture the child's typed reason..."
grep -F 'value  = try(subworkflow.failure_reason, "")' main.chcl >/dev/null

echo "==> Checking the retry budgets default to 2..."
[ "$(grep -c 'variable "max_reviewer_scope_retries"' variables.chcl)" -eq 1 ]
[ "$(grep -c 'variable "max_branch_scope_retries"' variables.chcl)" -eq 1 ]
grep -A4 'variable "max_reviewer_scope_retries"' variables.chcl | grep -F 'default     = 2' >/dev/null
grep -A4 'variable "max_branch_scope_retries"' variables.chcl | grep -F 'default     = 2' >/dev/null

# ── Subworkflow scope-loss signals ───────────────────────────────────────────

echo "==> Checking pr_reviewer_loop gates the in-place retry on a reported reason..."
jq -e '
  .switches[] | select(.name == "route_review_retry") |
  .conditions[0].match == "data.internal._failure_reason.value == \"\""
  and .conditions[0].next == "failed"
  and .conditions[1].next == "pr_review"
' "${SUB_COMPILE_OUT}" >/dev/null

echo "==> Checking branch_manager routes scope loss for the parent..."
jq -e '
  .subworkflows[] | select(.body.name == "branch_manager") |
  .body.switches[] | select(.name == "route_triage_scope_loss") |
  .conditions[0].match == "data.internal.failure_reason.value != \"\""
  and .conditions[0].next == "failed"
  and .default_next == "needs_human"
' "${COMPILE_OUT}" >/dev/null
jq -e '
  .subworkflows[] | select(.body.name == "branch_manager") |
  .body.steps[] | select(.name == "branch_triage_agent") | .timeout == "1h0m0s"
' "${COMPILE_OUT}" >/dev/null

echo "==> All checks passed."