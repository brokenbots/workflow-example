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
#
#   4. every terminal route reachable from a live session (deterministic shell
#      failures, agent escalation/failure without a reported reason) writes a
#      non-empty failure_reason — an empty reason reaching the parent means
#      engine-synthesized session loss and nothing else, which is what keeps
#      the re-provision routes from firing on ordinary failures.

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

# ── Live-session terminal routes write a non-empty failure reason (B1) ───────
#
# Any terminal route reachable while the session was live must reach the
# parent with a non-empty failure_reason; otherwise the parent's
# route_*_scope_reprovision switch misclassifies the failure as a lost scope
# session and wastefully re-runs the child. This scan walks every `outcome`
# block in the subworkflow sources that routes directly to a terminal state
# and requires a failure_reason write whose value is guaranteed non-empty
# (a quoted literal with content, or the empty-guarded try() ternary).

scan_terminal_routes() {
    local file="$1" label="$2" expected="$3" reason_target="$4"
    awk -v expected="$expected" -v label="$label" -v target="$reason_target" '
        function eval_block(block,    n, i, j, lines, saw_target, val_line, ok_val) {
            if (block !~ /next *= *state[.](failed|needs_human)/) return
            total++
            saw_target = 0
            val_line = ""
            n = split(block, lines, "\n")
            for (i = 1; i <= n; i++) {
                if (index(lines[i], "target = " target) > 0) {
                    saw_target = 1
                    for (j = i + 1; j <= n; j++) {
                        if (lines[j] ~ /^[[:space:]]*value/) { val_line = lines[j]; break }
                    }
                    break
                }
            }
            if (!saw_target) { print label ": terminal outcome without a failure_reason write: " cur; bad = 1; return }
            if (val_line == "") { print label ": failure_reason write without a value: " cur; bad = 1; return }
            ok_val = 0
            if (val_line ~ /^[[:space:]]*value[[:space:]]*=[[:space:]]*"[^"]*[A-Za-z][^"]*"/) ok_val = 1
            if (val_line ~ /try[(]output[.]reason, ""[)] != "" [?] try[(]output[.]reason, ""[)] : "[^"]*[A-Za-z][^"]*"/) ok_val = 1
            if (!ok_val) { print label ": failure_reason value not guaranteed non-empty: " val_line; bad = 1 }
        }
        BEGIN { total = 0; bad = 0; in_block = 0 }
        /^    outcome "/ {
            if (in_block) { print label ": outcome block not closed before: " cur; bad = 1; in_block = 0 }
            cur = $0
            if ($0 ~ /[{].*[}]$/) { eval_block($0 "\n"); cur = "" }
            else { in_block = 1; buf = $0 "\n" }
            next
        }
        in_block { buf = buf $0 "\n" }
        in_block && /^    [}]$/ { eval_block(buf); in_block = 0; next }
        END {
            if (in_block) { print label ": unterminated outcome block"; bad = 1 }
            if (bad) exit 1
            if (total != expected) {
                printf "%s: expected %s direct terminal routes, found %s\n", label, expected, total
                exit 1
            }
        }
    ' "$file"
}

echo "==> Scanning branch_manager for reasonless terminal routes..."
scan_terminal_routes "${TREE_ROOT}/workflows/branch_manager/main.chcl" "branch_manager" 4 "data.internal.failure_reason.value"

echo "==> Scanning pr_reviewer_loop for reasonless terminal routes..."
scan_terminal_routes "${TREE_ROOT}/workflows/pr_reviewer_loop/main.chcl" "pr_reviewer_loop" 4 "data.internal._failure_reason.value"

echo "==> Checking the deterministic failure routes carry typed reasons..."
grep -F 'value  = "branch resolution failed (KB-24)"' "${TREE_ROOT}/workflows/branch_manager/main.chcl" >/dev/null
grep -F 'value  = "worktree setup failed (KB-24)"' "${TREE_ROOT}/workflows/branch_manager/main.chcl" >/dev/null
grep -F 'value  = "PR number resolution failed (KB-24)"' "${TREE_ROOT}/workflows/pr_reviewer_loop/main.chcl" >/dev/null
grep -F 'value  = "PR status check failed (KB-24)"' "${TREE_ROOT}/workflows/pr_reviewer_loop/main.chcl" >/dev/null
grep -F 'value  = "posting the approval review failed (KB-24)"' "${TREE_ROOT}/workflows/pr_reviewer_loop/main.chcl" >/dev/null
grep -F 'value  = "posting the changes-requested review failed (KB-24)"' "${TREE_ROOT}/workflows/pr_reviewer_loop/main.chcl" >/dev/null

echo "==> Checking agent-reported reasons are guarded against the empty case..."
grep -cF 'try(output.reason, "") != "" ? try(output.reason, "") :' "${TREE_ROOT}/workflows/branch_manager/main.chcl" | grep -qx '2'
grep -cF 'try(output.reason, "") != "" ? try(output.reason, "") :' "${TREE_ROOT}/workflows/pr_reviewer_loop/main.chcl" | grep -qx '1'
grep -F 'try(output.reason, "") != "" ? try(output.reason, "") : "branch triage agent escalated to a human without a reason"' "${TREE_ROOT}/workflows/branch_manager/main.chcl" >/dev/null
grep -F 'try(output.reason, "") != "" ? try(output.reason, "") : "triage agent reported failure without a reason"' "${TREE_ROOT}/workflows/branch_manager/main.chcl" >/dev/null
grep -F 'try(output.reason, "") != "" ? try(output.reason, "") : "malformed reviewer response (no valid outcome)"' "${TREE_ROOT}/workflows/pr_reviewer_loop/main.chcl" >/dev/null

echo "==> Checking an unrecognized PR status carries its output as the reason..."
grep -F 'unrecognized PR status output: ${output.stdout}' "${TREE_ROOT}/workflows/pr_reviewer_loop/main.chcl" >/dev/null

echo "==> Checking the designed scope-loss signals stay intentionally empty..."
# Exactly one bare try(output.reason, "") reason write per subworkflow: the
# engine-synthesized failure outcomes whose empty value marks the loss for
# the reprovision switches. The empty-guarded ternaries above must not be
# counted (this anchor pins the whole line).
[ "$(grep -cE '^            value  = try\(output\.reason, ""\)$' "${TREE_ROOT}/workflows/branch_manager/main.chcl")" -eq 1 ]
[ "$(grep -cE '^            value  = try\(output\.reason, ""\)$' "${TREE_ROOT}/workflows/pr_reviewer_loop/main.chcl")" -eq 1 ]

echo "==> All checks passed."