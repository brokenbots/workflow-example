#!/usr/bin/env bash
set -euo pipefail

# kanboard_develop_v1 is the development phase of the Kanboard intake split: a
# port of linear_develop_v1 (CRI-239) to the Kanboard JSON-RPC API, fired on a
# TRIAGED (Ready-column) task. This test guards the port:
#
#   1. the tree validates standalone and references no linear_* tree asset;
#   2. the graph has exactly three terminal states — two success
#      (handler_complete, awaiting_human) and one failed bookkeeping
#      terminal (CRI-275 semantics: a parking comment or column move that
#      does not land records the run failed so the watcher can refire/alert)
#      — all reachable, its only subworkflow is workstream_handler_v1, and no
#      triage symbol appears anywhere in it — no triage step is reachable,
#      the confirmed workstream is the workflow's input;
#   3. the confirmed-workstream write-or-reuse gate is deterministic on the
#      real ticket.json shape (the {task, tags, comments} envelope
#      fetch_ticket stores): an existing published workstream is reused
#      byte-for-byte untouched, an absent/empty one is assembled from the
#      task and its comments, and a title-less task fails loudly;
#   4. the develop edge wiring is exactly the linear develop path's shape
#      (fetch -> workstream gate -> started comment -> work column ->
#      handler -> done comment -> done column | failure comments -> review
#      column), with CRI-275's bookkeeping-failure edges ending in the
#      failed terminal;
#   5. fetch_ticket requests task_id-only getTaskTags against a mock Kanboard
#      API and stores the {task, tags, comments} envelope with tags
#      normalized to the [{name}] shape the fixtures pin — real Kanboard
#      rejects extra params with -32602 "Invalid params: Too many arguments",
#      and the pre-fix port sent project_id and stored that error body as
#      ticket.json's tags (KB-10 smoke regression);
#   6. the linear_* source trees are untouched;
#   7. every comment script posts a createComment carrying the numeric task
#      id, user_id 0 and a non-empty body; the done-comment carries the full
#      evidence trail (verdict, workstream path, PR url, commit range) per
#      KB-51's acceptance, and the old `.result == true` success check on
#      createComment (KB-51) is treated as dead: Kanboard resolves the call
#      to a numeric comment_id, so the scripts must treat a numeric result
#      as success and a JSON-RPC error body as loud failure. The wiring that
#      supplies the evidence fields is asserted block-scoped on the source:
#      run_handler's success outcome copies subworkflow.review_result and
#      subworkflow.branch into their internal channels and
#      comment_handler_done spends them as criteria_value_3..7 (the
#      reviewer-loop output itself is pinned in workstream_handler_v1's
#      tests, where CI runs it).

TREE_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
REPO_ROOT="$(cd "$TREE_ROOT/.." && pwd)"
CRITERIA="${CRITERIA_BIN:-criteria}"

FAILED=0

fail() {
    echo "FAIL: $1" >&2
    FAILED=$((FAILED + 1))
}

ok() {
    echo "ok: $1"
}

require_equal() {
    if [ "$1" = "$2" ]; then
        ok "$3"
    else
        fail "$3: got '$1', want '$2'"
    fi
}

command -v "$CRITERIA" >/dev/null 2>&1 \
    || { echo "FAIL: criteria binary '$CRITERIA' not found (set CRITERIA_BIN)" >&2; exit 1; }
command -v jq >/dev/null 2>&1 \
    || { echo "FAIL: jq not found" >&2; exit 1; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# ── 1. Standalone validation ─────────────────────────────────────────────────

# Plain validate, matching the Makefile gate: the composed handler subtree
# carries known informational warnings (see below). The tree must contribute
# none of them itself.
"$CRITERIA" validate "$TREE_ROOT" >/dev/null 2>"$TMP/validate.err" \
    || { echo "FAIL: criteria validate: $(cat "$TMP/validate.err")" >&2; exit 1; }
ok "criteria validate passes standalone"

# The develop tree itself contributes zero warnings: any warning must point
# into workstream_handler_v1 (pre-existing, informational, shared with
# linear_intake_v1 — a write that re-reads its own step-entry snapshot).
if grep '^Warning:' "$TMP/validate.err" | grep "kanboard_develop_v1" >/dev/null 2>&1; then
    grep '^Warning:' "$TMP/validate.err" | grep "kanboard_develop_v1" >&2
    fail "validation reports warnings originating in the develop tree"
else
    ok "develop tree contributes no validation warnings"
fi

# No runtime dependency on the sibling trees: no path into either anywhere
# (prose mentions in comments are fine; structural embedding is already
# guarded by the compiled-graph grep below).
if find "$TREE_ROOT" -type f \
    \( -name '*.chcl' -o -name '*.sh.tftpl' -o -name '*.md.tftpl' \) -print0 \
    | xargs -0 -r grep -l -e "linear_intake_v1/" -e "linear_triage_v1/" -e "linear_develop_v1/"; then
    fail "develop tree references a linear_* tree's files — trees must be independently runnable"
else
    ok "no file path into any linear_* tree"
fi

# ── 1b. KB-235: every Kanboard JSON-RPC surface retries with backoff ────────

# The vendored backoff shim is a byte copy of the workstream_handler_v1
# canonical wrapper (KB-235 ruling: ONE implementation, vendored per CRI-239
# for independent trees). Drift in either copy must fail here, not in a run.
if cmp -s "$TREE_ROOT/scripts/gh_retry.sh.tftpl" \
    "$TREE_ROOT/../workstream_handler_v1/scripts/gh_retry.sh.tftpl"; then
    ok "vendored gh_retry shim is byte-identical to the canonical wrapper"
else
    fail "kanboard_develop_v1/scripts/gh_retry.sh.tftpl drifted from workstream_handler_v1/scripts/gh_retry.sh.tftpl"
fi

# Splice + wrap coverage: the seven curl templates each carry the shim
# marker AND route their transport call through gh_retry; the parent wiring
# passes the file() arg to all ten templatefile calls; the non-network
# template stays unpolluted.
gh_retry_scripts="$(grep -l '{{ .gh_retry }}' "$TREE_ROOT"/scripts/*.sh.tftpl | wc -l)"
if [ "$gh_retry_scripts" -eq 7 ] \
    && [ "$(grep -l 'gh_retry curl' "$TREE_ROOT"/scripts/*.sh.tftpl | wc -l)" -eq 7 ] \
    && [ "$(grep -c '= file("./scripts/gh_retry.sh.tftpl")' "$TREE_ROOT/main.chcl")" -eq 10 ] \
    && ! grep -q '{{ .gh_retry }}' "$TREE_ROOT/scripts/ensure_confirmed_workstream.sh.tftpl"; then
    ok "gh_retry spliced into all 7 curl templates and wired at all 10 templatefile calls"
else
    fail "gh_retry vendoring incomplete: markers=$gh_retry_scripts (want 7), wrapped=$(grep -l 'gh_retry curl' "$TREE_ROOT"/scripts/*.sh.tftpl | wc -l), wired=$(grep -c '= file("./scripts/gh_retry.sh.tftpl")' "$TREE_ROOT/main.chcl")"
fi

# ── 2. Graph structure ───────────────────────────────────────────────────────

"$CRITERIA" compile "$TREE_ROOT" --format json --out "$TMP/graph.json" 2>"$TMP/compile.err" \
    || { echo "FAIL: criteria compile: $(cat "$TMP/compile.err")" >&2; exit 1; }
GRAPH="$TMP/graph.json"

terminals="$(jq -r '[.states[] | select(.terminal)] | length' "$GRAPH")"
[ "$terminals" -eq 3 ] || fail "expected exactly 3 terminal states, got $terminals"

terminal_names="$(jq -r '[.states[] | select(.terminal) | .name] | sort | join(" ")' "$GRAPH")"
require_equal "$terminal_names" "awaiting_human failed handler_complete" "terminal states are exactly awaiting_human, failed and handler_complete"

# 2026-10-02 dave ruling: waiting on a human is a FAILURE ending — humans
# are for broken things. The delivery terminal (handler_complete: PR merged)
# stays success; every human hand-off and every bookkeeping failure records
# the run failed so the watcher raises the dirty label and refires.
jq -e '.states[] | select(.terminal and .name == "handler_complete" and .success)' "$GRAPH" >/dev/null \
    || fail "terminal handler_complete must be success=true"
ok "delivery terminal is success=true"
jq -e '.states[] | select(.terminal and .name == "awaiting_human" and (.success | not))' "$GRAPH" >/dev/null \
    || fail "terminal awaiting_human must be success=false: a human hand-off is a failure ending"
ok "human hand-off terminal is success=false"
jq -e '.states[] | select(.terminal and .name == "failed" and (.success | not))' "$GRAPH" >/dev/null \
    || fail "terminal failed must be success=false: bookkeeping failures must be recorded as run failures"
ok "bookkeeping-failure terminal is success=false"

# Every step and terminal reachable from initial_state; both terminal
# outcomes reachable is asserted by membership here.
jq -r '
  [ (.steps[] | .name as $s | [.outcomes[]? | {from: $s, to: .next}])
  , (.switches[] | .name as $s
      | [.conditions[]? | {from: $s, to: .next}]
      + [(.default_next // empty) as $d | {from: $s, to: $d}])
  ]
  | flatten[]
  | [.from, .to] | @tsv
' "$GRAPH" > "$TMP/edges.tsv"

# Breadth-first search over the edge list from the initial state.
initial="$(jq -r '.initial_state' "$GRAPH")"
declare -A reachable
reachable["$initial"]=1
frontier=("$initial")

while [ "${#frontier[@]}" -gt 0 ]; do
    next_frontier=()
    for node in "${frontier[@]}"; do
        while IFS=$'\t' read -r from to; do
            [ "$from" = "$node" ] || continue
            [ -n "$to" ] || continue
            [ "${reachable[$to]+set}" = "set" ] && continue
            reachable["$to"]=1
            next_frontier+=("$to")
        done < "$TMP/edges.tsv"
    done
    frontier=("${next_frontier[@]}")
done

unreachable_steps="$(jq -r '.steps[] | .name' "$GRAPH" | while read -r s; do
    [ "${reachable[$s]+set}" = "set" ] || echo "$s"
done)"
[ -z "$unreachable_steps" ] || fail "steps not reachable from initial_state: $unreachable_steps"

for state_name in awaiting_human failed handler_complete; do
    if [ "${reachable[$state_name]+set}" = "set" ]; then
        ok "terminal outcome reachable: $state_name"
    else
        fail "terminal outcome not reachable from initial_state: $state_name"
    fi
done

# Only cross-tree dependency allowed: the workstream_handler_v1 subworkflow
# (develop -> PR -> reviewer loop -> merge lives there).
subwf_count="$(jq -r '.subworkflows | length' "$GRAPH")"
[ "$subwf_count" -eq 1 ] || fail "expected exactly 1 subworkflow (workstream_handler_v1), got $subwf_count"
subwf_name="$(jq -r '.subworkflows[0].body.name // ""' "$GRAPH")"
require_equal "$subwf_name" "workstream_handler_v1" "subworkflow is workstream_handler_v1"

# No triage coupling anywhere in the compiled graph: no intake classification,
# no internal-reproduced gate, no QA triage subworkflow, no triage reviewer,
# no classification labels, no path into the sibling trees. The confirmed
# workstream is this workflow's input, not something it re-derives.
forbidden='qa_triage_v1|write_bug_report|classify_ticket|check_internal_label|review_qa_output|triage_reviewer|set_triage_state|linear_triage_state|linear_bug_label|linear_feature_label|set_classification_label|comment_triage_failed|linear_intake_v1|linear_triage_v1'
if grep -Eq "$forbidden" "$GRAPH"; then
    grep -En "$forbidden" "$GRAPH" >&2
    fail "compiled graph contains triage-path symbols — no triage step may be reachable"
else
    ok "no triage-path symbols in the compiled graph"
fi

# No routing switches except the KB-49 empty-pr_url guard on the handler's
# success outcome: nothing to classify, nothing to gate on labels.
switch_count="$(jq -r '.switches | length' "$GRAPH")"
[ "$switch_count" -eq 1 ] || fail "expected exactly 1 switch (the KB-49 pr_url guard), got $switch_count"

# The step set is exactly the develop path plus the KB-49 guard steps — no
# intake/triage steps.
step_names="$(jq -r '[.steps[] | .name] | sort | join(" ")' "$GRAPH")"
expected_steps="comment_develop_failed comment_done_move_failed comment_handler_done comment_handler_failed comment_handler_started ensure_confirmed_workstream fetch_ticket flag_missing_pr_url park_after_done_move_failed run_handler set_done_state set_review_state set_work_state"
require_equal "$step_names" "$expected_steps" "step set is exactly the develop path"

# The confirmed-workstream gate is deterministic shell — no model tools.
gate_tools="$(jq -r '.steps[] | select(.name == "ensure_confirmed_workstream") | .allow_tools | length' "$GRAPH")"
[ "$gate_tools" -eq 0 ] || fail "ensure_confirmed_workstream must not allow model tools, got $gate_tools"

# The handler's reviewer identity must be threaded through (independent
# approval gate): reviewer_github_token wired from a dedicated reviewer
# variable into the subworkflow input. Input bindings are not serialized
# into the compiled graph, so this is asserted on the workflow source.
if grep -q 'reviewer_github_token = var.reviewer_github_token' "$TREE_ROOT/main.chcl" \
    && grep -q 'variable "reviewer_github_token"' "$TREE_ROOT/variables.chcl"; then
    ok "reviewer_github_token threaded into the handler wiring"
else
    fail "reviewer_github_token missing from the handler wiring — the handler would review with the author identity"
fi

# KB-24: the handler's typed failure reason is captured on failure and passed
# to the parking comment (criteria_value_5), which appends it when non-empty.
# Write bindings are not serialized into the compiled graph, so this is
# asserted on the workflow source and the comment script.
if grep -q 'value  = coalesce(try(subworkflow.failure_reason, ""), "")' "$TREE_ROOT/main.chcl" \
    && grep -q 'criteria_value_5 = data.internal.handler_error.value' "$TREE_ROOT/main.chcl" \
    && grep -q "Handler failure reason: \$criteria_value_5" "$TREE_ROOT/scripts/comment_handler_failed.sh.tftpl"; then
    ok "handler failure reason threaded into the parking comment"
else
    fail "handler failure reason not threaded into the parking comment wiring"
fi

# KB-51: the run's evidence thread. run_handler's success outcome copies the
# handler subworkflow's branch and verdict into the internal channels, and
# comment_handler_done spends them (verdict=3, workstream path=4, PR url=5,
# branch=6, base branch=7) on the closing comment. Write bindings are not
# serialized into the compiled graph and criteria compile does not validate
# subworkflow.<output> names, so this is asserted on the workflow source,
# scoped to the owning step block — a match elsewhere must not satisfy it.
run_handler_block="$(sed -n '/^step "run_handler" {/,/^}/p' "$TREE_ROOT/main.chcl")"
run_handler_success="$(printf '%s' "$run_handler_block" \
    | sed -n '/outcome "success" {/,/outcome "failure"/p')"
done_block="$(sed -n '/^step "comment_handler_done" {/,/^}/p' "$TREE_ROOT/main.chcl")"

if printf '%s' "$run_handler_success" \
        | grep -A1 'target = data.internal.branch.value' \
        | grep -q 'value  = subworkflow.branch'; then
    ok "run_handler success copies subworkflow.branch into data.internal.branch"
else
    fail "run_handler does not copy the handler branch into data.internal.branch — the done comment's commit range would be empty"
fi

if printf '%s' "$run_handler_success" \
        | grep -A1 'target = data.internal.review_result.value' \
        | grep -q 'value  = subworkflow.review_result'; then
    ok "run_handler success copies subworkflow.review_result into data.internal.review_result"
else
    fail "run_handler does not copy the handler verdict into data.internal.review_result — the done comment's verdict would be empty"
fi

if printf '%s' "$done_block" \
        | grep -q 'criteria_value_3 = data.internal.review_result.value' \
    && printf '%s' "$done_block" \
        | grep -q 'criteria_value_4 = data.internal.workstream_file.value' \
    && printf '%s' "$done_block" \
        | grep -q 'criteria_value_5 = data.internal.pr_url.value' \
    && printf '%s' "$done_block" \
        | grep -q 'criteria_value_6 = data.internal.branch.value' \
    && printf '%s' "$done_block" \
        | grep -q 'criteria_value_7 = var.base_branch'; then
    ok "comment_handler_done binds verdict, workstream path, PR url, branch and base branch"
else
    fail "comment_handler_done's evidence bindings incomplete — the closing comment would not carry the verdict, workstream path, PR url and commit range"
fi

# KB-24: every shell step's templatefile(...) input keys must be rendered by
# the referenced script, and every $criteria_value_N a script body references
# must have a render line in its header. Scripts run under set -euo pipefail,
# so an unbound body reference (the KB-24 parking-comment reason is empty on
# most handler failures) aborts the step before it posts anything and
# main.chcl routes the run to state.failed. Scan both directions.
bind_issues=0
for tpl in "$TREE_ROOT"/scripts/*.sh.tftpl; do
    for n in $(grep -oE '[$][{]?criteria_value_[0-9]+' "$tpl" | grep -oE '[0-9]+' | sort -u); do
        if ! grep -qE "^criteria_value_${n}=[{][{] [.]?criteria_value_${n}" "$tpl"; then
            fail "$(basename "$tpl") references \$criteria_value_$n but renders no {{ .criteria_value_$n }} binding"
            bind_issues=$((bind_issues + 1))
        fi
    done
done
sed -n '/templatefile(/,/^[[:space:]]*\}[)][[:space:]]*$/p' "$TREE_ROOT/main.chcl" \
    | sed -n 's/.*templatefile("\([^"]*\)".*/PATH \1/p; s/^[[:space:]]*\(criteria_value_[0-9][0-9]*\)[[:space:]]*= .*/KEY \1/p' \
    > "$TMP/tpl_keys.raw"
tpl_rel=""
key_render_issues=0
while read -r kind value; do
    case "$kind" in
        PATH) tpl_rel="$value" ;;
        KEY)
            if [ -n "$tpl_rel" ] \
                && ! grep -qE "^${value}=[{][{] [.]?${value}" "$TREE_ROOT/$tpl_rel"; then
                fail "$tpl_rel receives $value but renders no {{ .$value }} binding"
                key_render_issues=$((key_render_issues + 1))
            fi
            ;;
    esac
done < "$TMP/tpl_keys.raw"
if [ "$bind_issues" -eq 0 ] && [ "$key_render_issues" -eq 0 ]; then
    ok "every templatefile input key is rendered by its target script"
else
    fail "templatefile input keys and script render bindings out of sync"
fi

# KB-24/KB-51: execute the rendered handler-failure comment end-to-end. The engine
# binds criteria_value_N through the template header only and the script runs
# under set -euo pipefail, so a body reference without a render line aborts
# before anything is posted; and the createComment payload must actually carry
# the task id and the body (a jq -n call that forgets an --arg posts an empty
# payload). Exercise both with a stub curl.
#
# KB-51: the stub must speak the real Kanboard contract — createComment
# resolves to the new comment_id (a number), and E2E_STUB_ERROR (when set)
# replaces the body with a JSON-RPC error object to exercise the loud-failure
# path.
# KB-231: E2E_STUB_BODY adds the outage bodies the guard must name: "empty"
# (zero-length body) and "html302" (an HTML 302 page an auth-flapping proxy
# hands curl with rc 0).
STUBBIN="$TMP/stubbin"
mkdir -p "$STUBBIN"
cat > "$STUBBIN/curl" <<'EOF'
#!/bin/bash
while [ "$#" -gt 0 ]; do
    if [ "$1" = "-d" ] && [ "$#" -ge 2 ]; then
        printf '%s' "$2" > "$E2E_CAPTURE/payload.json"
        shift 2
        continue
    fi
    shift
done
if [ -n "${E2E_STUB_BODY:-}" ]; then
    if [ "$E2E_STUB_BODY" = "html302" ]; then
        printf '<html><body>302 Found. Object moved.</body></html>'
    fi
    # empty body: print nothing, exit 0
    exit 0
fi
if [ -n "${E2E_STUB_ERROR:-}" ]; then
    printf '{"jsonrpc":"2.0","id":1,"error":{"code":1,"message":"stub forced error"}}'
    exit 0
fi
    # jq -n emits pretty JSON, so match the method regardless of formatting.
    if [ "$(jq -r '.method // empty' "$E2E_CAPTURE/payload.json" 2>/dev/null)" = "createComment" ]; then
        printf '{"jsonrpc":"2.0","id":1,"result":101}'
    else
        printf '{"jsonrpc":"2.0","id":1,"result":true}'
    fi
EOF
chmod +x "$STUBBIN/curl"

# KB-235: rendered scripts carry the vendored backoff shim inline (the engine
# binds {{ .gh_retry }} to the shim source); splice it the same way here.
splice_gh_retry() {
    awk -v w="$TREE_ROOT/scripts/gh_retry.sh.tftpl" '
        /^\{\{ \.gh_retry \}\}$/ { while ((getline line < w) > 0) print line; next }
        { print }
    ' "$@"
}

# Render the template the engine way: bind the criteria_value_N header lines,
# leave the body untouched (its $refs resolve at runtime from the header).
render_failed_comment() {
    local reason="$1"
    sed -e "s,{{ .criteria_value_1 | shellquote }},'/tmp',g" \
        -e "s,{{ .criteria_value_2 | shellquote }},'KB-24',g" \
        -e "s,{{ .criteria_value_3 | shellquote }},'workstream',g" \
        -e "s,{{ .criteria_value_4 | shellquote }},'Review',g" \
        -e "s,{{ .criteria_value_5 | shellquote }},'${reason}',g" \
        "$TREE_ROOT/scripts/comment_handler_failed.sh.tftpl" \
        | splice_gh_retry > "$TMP/failed_comment_case.sh"
}

run_failed_comment() {
    render_failed_comment "$1"
    rm -f "$TMP/payload.json"
    KANBOARD_APP_TOKEN=stub-token KANBOARD_URL=http://127.0.0.1:1 \
        GH_RETRY_BACKOFF_SECONDS="0 0 0" \
        E2E_CAPTURE="$TMP" PATH="$STUBBIN:$PATH" \
        bash "$TMP/failed_comment_case.sh" >/dev/null 2>&1
}

if run_failed_comment "" && jq -e \
    '.params.task_id == 24 and (.params.content | contains("The implementation handler failed."))' \
    "$TMP/payload.json" >/dev/null 2>&1 \
    && ! grep -q "Handler failure reason" "$TMP/payload.json"; then
    ok "handler-failure comment posts with empty reason (no suffix, task carried)"
else
    fail "handler-failure comment aborted or payload wrong with empty criteria_value_5"
fi

if run_failed_comment "some reason" \
    && jq -r '.params.content' "$TMP/payload.json" 2>/dev/null | grep -q "Handler failure reason: some reason\."; then
    ok "handler-failure comment appends the failure reason when set"
else
    fail "handler-failure comment did not append the failure reason"
fi

# KB-51: every develop comment script must post a createComment whose params
# carry the numeric task id, user_id 0 and a non-empty body, and treat the
# numeric comment_id response as success (a `.result == true` check read every
# successful post as a step failure — comments landed while runs reported
# failure). Each script runs rendered the engine way; the done-comment must
# additionally carry the full evidence trail: verdict, workstream path, PR url
# and the commit range, per the acceptance criteria.
render_comment_case() {
    local tpl="$1"
    shift
    local out="$TMP/case_comment.sh"
    splice_gh_retry "$TREE_ROOT/scripts/$tpl" > "$out"
    local i=1
    local v quoted
    for v in "$@"; do
        # Engine shellquote semantics: single-quote the value, escape any
        # embedded single quotes.
        quoted="'${v//\'/\'\\\'\'}'"
        sed -i "s,{{ .criteria_value_$i | shellquote }},${quoted},g" "$out"
        i=$((i + 1))
    done
    printf '%s\n' "$out"
}

run_comment_case() {
    # $1=expected rc (0 on success, nonzero when the stub answers an error),
    # $2..=render args; assert the posted payload shape.
    local expect_rc="$1"
    shift
    render_comment_case "$@"
    rm -f "$TMP/payload.json"
    if [ "$expect_rc" = "0" ]; then
        KANBOARD_APP_TOKEN=stub-token KANBOARD_URL=http://127.0.0.1:1 \
            GH_RETRY_BACKOFF_SECONDS="0 0 0" \
            E2E_CAPTURE="$TMP" PATH="$STUBBIN:$PATH" \
            bash "$TMP/case_comment.sh" >/dev/null 2>&1
        return $?
    fi
    # Loud failure: the stub answers a JSON-RPC error body; the script must
    # exit non-zero.
    KANBOARD_APP_TOKEN=stub-token KANBOARD_URL=http://127.0.0.1:1 \
        GH_RETRY_BACKOFF_SECONDS="0 0 0" \
        E2E_CAPTURE="$TMP" PATH="$STUBBIN:$PATH" E2E_STUB_ERROR=1 \
        bash "$TMP/case_comment.sh" >/dev/null 2>&1
    [ $? -ne 0 ]
}

check_payload() {
    # $1=jq filter evaluated against the posted payload, plus content grep
    # tokens passed as $2.. (all must be substrings of .params.content).
    local probe="$1"
    shift
    if ! jq -e "$probe" "$TMP/payload.json" >/dev/null 2>&1; then
        return 1
    fi
    local token
    for token in "$@"; do
        jq -r '.params.content' "$TMP/payload.json" 2>/dev/null | grep -qF -- "$token" || return 1
    done
    return 0
}

# started (published workstream reused):
if run_comment_case 0 comment_handler_started.sh.tftpl /tmp KB-24 "workstreams/KB-24.md" reused \
    && check_payload '.params.task_id == 24 and .params.user_id == 0 and (.params.content | length > 0)' \
        "started on the triaged ticket" "workstreams/KB-24.md" "reused untouched"; then
    ok "started comment posts task+body and reads the numeric id as success (reused path)"
else
    fail "started comment failed on the reused path"
fi

# started (workstream reconstructed): the written variant flips the wording.
if run_comment_case 0 comment_handler_started.sh.tftpl /tmp KB-24 "workstreams/KB-24.md" written \
    && check_payload '.params.task_id == 24' "reconstructed from the ticket"; then
    ok "started comment says the workstream was reconstructed when nothing was reused"
else
    fail "started comment failed on the written path"
fi

# done: the evidence comment must carry verdict, workstream path, PR url and
# the commit range base..run-branch.
if run_comment_case 0 comment_handler_done.sh.tftpl /tmp KB-24 approved \
    "workstreams/KB-24.md" "https://example.org/org/repo/pull/9" "kb-51-9f24c1a" main \
    && check_payload '.params.task_id == 24' \
        "**Workstream complete." "Review verdict: approved" "Workstream path: workstreams/KB-24.md" \
        "PR: https://example.org/org/repo/pull/9" "Commit range: main..kb-51-9f24c1a"; then
    ok "done comment carries verdict, workstream path, PR url and commit range"
else
    fail "done comment misses part of the evidence trail"
fi

# done without a run branch: the commit-range sentence must be absent instead
# of printing an empty range.
if run_comment_case 0 comment_handler_done.sh.tftpl /tmp KB-24 approved \
    "workstreams/KB-24.md" "https://example.org/org/repo/pull/9" "" main \
    && check_payload '.params.task_id == 24' "Review verdict: approved" \
    && ! jq -r '.params.content' "$TMP/payload.json" 2>/dev/null | grep -q "Commit range"; then
    ok "done comment omits the commit range when no run branch exists"
else
    fail "done comment printed an empty commit range or lost verdict evidence"
fi

# develop-failed: carries the workstream context when assembly produced one.
if run_comment_case 0 comment_develop_failed.sh.tftpl /tmp KB-24 "workstreams/KB-24.md" Review \
    && check_payload '.params.task_id == 24' "Automated development did not start" "workstreams/KB-24.md"; then
    ok "develop-failed comment posts with the workstream path"
else
    fail "develop-failed comment failed"
fi

# develop-failed with no workstream: body says (none) instead of an empty path.
if run_comment_case 0 comment_develop_failed.sh.tftpl /tmp KB-24 "" Review \
    && check_payload '.params.task_id == 24' "Workstream: (none)."; then
    ok "develop-failed comment marks a missing workstream explicitly"
else
    fail "develop-failed comment did not mark the missing workstream"
fi

# done-move-failed keeps the accurate bookkeeping wording.
if run_comment_case 0 comment_done_move_failed.sh.tftpl /tmp KB-24 \
    "https://example.org/org/repo/pull/9" Done Review \
    && check_payload '.params.task_id == 24' \
        "**The implementation PR merged** (https://example.org/org/repo/pull/9)" \
        "**but moving the ticket to \"Done\" failed.**"; then
    ok "done-move-failed comment reports the merged PR and the failed move"
else
    fail "done-move-failed comment failed"
fi

# Loud failure: a JSON-RPC error body must fail the step (CRI-275 semantics —
# a comment that did not land must not read as success).
if run_comment_case x comment_handler_done.sh.tftpl /tmp KB-24 approved \
    "workstreams/KB-24.md" "https://example.org/org/repo/pull/9" "kb-51-9f24c1a" main; then
    ok "comment script fails loudly on a JSON-RPC error response"
else
    fail "comment script did not fail on a JSON-RPC error response"
fi

# Develop edge wiring: success and failure paths, matching the intake
# develop path's shape.
assert_edge() {
    local from="$1" outcome="$2" to="$3"
    local got
    got="$(jq -r --arg from "$from" --arg outcome "$outcome" \
        '.steps[] | select(.name == $from) | .outcomes[] | select(.name == $outcome) | .next' "$GRAPH")"
    require_equal "$got" "$to" "edge $from.$outcome -> $to"
}

assert_edge "fetch_ticket" "success" "ensure_confirmed_workstream"
assert_edge "fetch_ticket" "failure" "awaiting_human"
assert_edge "ensure_confirmed_workstream" "success" "comment_handler_started"
assert_edge "ensure_confirmed_workstream" "failure" "comment_develop_failed"
assert_edge "comment_handler_started" "success" "set_work_state"
assert_edge "comment_handler_started" "failure" "set_work_state"
assert_edge "set_work_state" "success" "run_handler"
assert_edge "set_work_state" "failure" "comment_develop_failed"
assert_edge "run_handler" "failure" "comment_handler_failed"
# KB-49: run_handler success is guarded by the empty-pr_url route, so the
# direct edge to the done-path comment is replaced by the guard switch.
# CRI-275: bookkeeping-failure routing. A parking comment or state move that
# fails ends the run in the failed terminal (success=false), mirroring
# linear_intake_v1's bookkeeping-failure semantics — including the closing
# comment on the post-merge path (comment_handler_done), whose failure skips
# the Done move entirely. The Done-move-failed route keeps its accurate
# fallback comment, parks In Review, and still ends failed — the merged-PR
# bookkeeping failed even when the parking worked.
assert_edge "comment_handler_done" "success" "set_done_state"
assert_edge "comment_handler_done" "failure" "failed"
assert_edge "set_done_state" "success" "handler_complete"
assert_edge "set_done_state" "failure" "comment_done_move_failed"
assert_edge "comment_handler_failed" "success" "set_review_state"
assert_edge "comment_handler_failed" "failure" "failed"
assert_edge "comment_develop_failed" "success" "set_review_state"
assert_edge "comment_develop_failed" "failure" "failed"
assert_edge "comment_done_move_failed" "success" "park_after_done_move_failed"
assert_edge "comment_done_move_failed" "failure" "failed"
assert_edge "park_after_done_move_failed" "success" "failed"
assert_edge "park_after_done_move_failed" "failure" "failed"
assert_edge "set_review_state" "success" "awaiting_human"
assert_edge "set_review_state" "failure" "failed"

# ── 3. Confirmed-workstream gate on real fixture shapes ──────────────────────

# fetch_ticket stores the enveloped GraphQL response; the gate must read
# through .data.issue. Rendering mimics templatefile for the two variables
# the script consumes, including the engine's shellquote (single-quote
# wrapping with '\'' escaping).
SLUG="KB-140"
shquote() {
    printf "'%s'" "${1//\'/\'\\\'\'}"
}
render() {
    sed -e "s@{{ .criteria_value_1 | shellquote }}@$(shquote "$1")@g" \
        -e "s@{{ .criteria_value_2 | shellquote }}@$(shquote "$SLUG")@g" "$2" > "$3"
}
run_dir="$TMP/intake/$SLUG"
mkdir -p "$run_dir"

ws_script="$TMP/ensure_confirmed_workstream.sh"
render "$TMP/intake" "$TREE_ROOT/scripts/ensure_confirmed_workstream.sh.tftpl" "$ws_script"
chmod +x "$ws_script"

workstream="$run_dir/workstreams/$SLUG.md"

# Absent workstream + triaged ticket with triage outputs in comments:
# assembled mechanically from the ticket and its comments.
cp "$TREE_ROOT/tests/fixtures/task_ready.json" "$run_dir/ticket.json"
if "$ws_script" > "$TMP/ws.out" 2>&1; then
    ok "ensure_confirmed_workstream assembles the workstream"
else
    fail "ensure_confirmed_workstream failed: $(cat "$TMP/ws.out")"
fi
[ -s "$workstream" ] || fail "workstream missing or empty: $workstream"
if [ -s "$workstream" ] \
    && grep -q "# Workstream: Deploy job races the cache warm step" "$workstream" \
    && grep -q "Kanboard task KB-140" "$workstream" \
    && grep -q "Board column at development start: 6" "$workstream" \
    && grep -q "partially warmed cache" "$workstream" \
    && grep -q "cache_warm_wait_gate" "$workstream" \
    && grep -q "rr-5173" "$workstream" \
    && grep -q "## Required behavior" "$workstream"; then
    ok "workstream carries title, source, state, description and comment triage evidence"
else
    fail "workstream is missing ticket content or triage evidence from comments"
fi

# Reuse: an existing published workstream is the triage run's conclusion and
# must pass through byte-for-byte untouched. (CRI-239 exit criterion: written
# when absent, reused when present, never overwriting triage conclusions.)
sentinel="# TRIAGE CONCLUSION — DO NOT OVERWRITE
cached verdict: reproduced"
printf '%s\n' "$sentinel" > "$workstream"
before="$(md5sum "$workstream" | cut -d' ' -f1)"
if out="$("$ws_script")" && [ "$out" = "workstream reused: $workstream" ]; then
    ok "existing workstream is reused (semantic source line)"
else
    fail "existing workstream: got '${out:-<no output>}', want 'workstream reused: $workstream'"
fi
after="$(md5sum "$workstream" | cut -d' ' -f1)"
require_equal "$before" "$after" "reused workstream is byte-for-byte untouched"

# An empty existing file is not a conclusion: assembled.
: > "$workstream"
if out="$("$ws_script")" && [ "$out" = "workstream written: $workstream" ]; then
    ok "empty workstream file is assembled (written)"
else
    fail "empty workstream file: got '${out:-<no output>}', want 'workstream written: $workstream'"
fi
[ -s "$workstream" ] || fail "workstream missing or empty after assembly: $workstream"

# Determinism: the same input yields the same file.
assembled="$(md5sum "$workstream" | cut -d' ' -f1)"
"$ws_script" >/dev/null
require_equal "$assembled" "$(md5sum "$workstream" | cut -d' ' -f1)" "assembly is deterministic across repeated runs"

# Missing ticket.json and no workstream: fail loudly (the workflow routes the
# failure to the develop-failed comment and the review state).
rm -f "$run_dir/ticket.json" "$workstream"
if "$ws_script" >/dev/null 2>&1; then
    fail "missing ticket.json must fail loudly"
else
    ok "missing ticket.json fails loudly"
fi

# A title-less ticket cannot carry a workstream header: fail loudly.
cp "$TREE_ROOT/tests/fixtures/task_notitle.json" "$run_dir/ticket.json"
if "$ws_script" >/dev/null 2>&1; then
    fail "title-less ticket must fail loudly"
else
    ok "title-less ticket fails loudly"
fi

# ── 4. fetch_ticket envelope against a mock Kanboard API ─────────────────────

# Regression (KB-10 smoke run): the port's fetch passed project_id alongside
# task_id to getTaskTags. Real Kanboard takes ONLY task_id — the extra
# project_id fails with -32602 "Invalid params: Too many arguments", and the
# raw RPC error body was stored as ticket.json's tags (the smoke run's
# operator ticket carried the error blob instead of the task's tags). The
# mock emulates the real API exactly: params beyond task_id return the real
# -32602 error body, and a correct call serves the {tag_link_id: name} map
# the real API returns. fetch_ticket must request task_id only, and store
# tags normalized to the [{name}] shape the fixtures pin.

command -v node >/dev/null 2>&1 \
    || { echo "FAIL: node not found (mock Kanboard server)" >&2; exit 1; }

export KANBOARD_APP_TOKEN="mock-token"

FETCH_DIR="$TMP/fetch-intake"
FETCH_RUN_DIR="$FETCH_DIR/$SLUG"
mkdir -p "$FETCH_RUN_DIR"

MOCK="$TMP/mock_kanboard_fetch.js"
cat > "$MOCK" <<'JS'
const http = require("http");
const fs = require("fs");
const [cfgFile, logFile, tasksFile, attemptsFile] = process.argv.slice(2);

const server = http.createServer((req, res) => {
    let body = "";
    req.on("data", (c) => {
        body += c;
    });
    req.on("end", () => {
        const rpc = JSON.parse(body || "{}");
        const method = rpc.method || "";
        const params = rpc.params || {};
        const cfg = JSON.parse(fs.readFileSync(cfgFile, "utf8"));
        const tasks = JSON.parse(fs.readFileSync(tasksFile, "utf8"));
        // KB-235: count every request that reaches the server (one line per
        // attempt — the backoff scenarios assert exact attempt counts).
        if (attemptsFile) fs.appendFileSync(attemptsFile, method + "\n");
        // KB-235: transport failure injection — destroy the socket before any
        // response is written, exactly like a connection reset from a flaky
        // proxy in front of the board. cfg.reset_methods maps method -> how
        // many incoming requests should still be killed; the state decays on
        // the cfg file so the same server serves fail-then-recover and
        // fail-forever scenarios.
        if ((cfg.reset_methods || {})[method] > 0) {
            cfg.reset_methods[method]--;
            fs.writeFileSync(cfgFile, JSON.stringify(cfg));
            req.socket.destroy();
            return;
        }
        let resp;
        // KB-231: real Kanboard answers JSON-RPC envelopes (jsonrpc + id); the
        // scripts' guard classifies any body without those keys as a
        // transport failure, so the mock must speak the real shape.
        if (method === "getTask") {
            resp = { jsonrpc: "2.0", id: 1, result: tasks.task };
        } else if (method === "getAllComments") {
            resp = { jsonrpc: "2.0", id: 1, result: tasks.comments };
        } else if (method === "getTaskTags") {
            // Record the exact request params: the regression asserts the
            // script sends {"task_id": N} and nothing else.
            fs.appendFileSync(logFile, JSON.stringify(params) + "\n");
            if (cfg.force_tags_error) {
                resp = { jsonrpc: "2.0", id: 1, error: { code: -32602, message: "Invalid params", data: "Too many arguments" } };
            } else if (Object.keys(params).length !== 1 || !("task_id" in params)) {
                // Real Kanboard: extra params fail with HTTP 200 + JSON-RPC error.
                resp = { jsonrpc: "2.0", id: 1, error: { code: -32602, message: "Invalid params", data: "Too many arguments" } };
            } else {
                // Real Kanboard shape: {tag_link_id: tag_name} map.
                resp = { jsonrpc: "2.0", id: 1, result: Object.fromEntries(tasks.tags.map((t, i) => [String(1000 + i), t.name])) };
            }
        } else {
            resp = { jsonrpc: "2.0", id: 1, error: { code: -32601, message: "unexpected method: " + method } };
        }
        // KB-231: outage modes — a dead board behind a proxy is what serves
        // curl a zero-length body or an HTML 302 page with HTTP 200; the
        // fetch must fail with the named stage, never leak an empty string
        // into a later jq.
        if (cfg.body_mode === "empty") {
            res.writeHead(200, { "Content-Type": "application/json", "Content-Length": 0 });
            res.end("");
            return;
        }
        if (cfg.body_mode === "html302") {
            const html = "<html><body>302 Found. Object moved.</body></html>";
            res.writeHead(200, { "Content-Type": "text/html", "Content-Length": Buffer.byteLength(html) });
            res.end(html);
            return;
        }
        const data = JSON.stringify(resp);
        res.writeHead(200, { "Content-Type": "application/json", "Content-Length": Buffer.byteLength(data) });
        res.end(data);
    });
});
server.listen(0, "127.0.0.1", () => { console.log(server.address().port); });
JS

FETCH_CFG="$TMP/fetch_mock_config.json"
FETCH_LOG="$TMP/fetch_rpc_params.jsonl"
FETCH_ATTEMPTS="$TMP/fetch_attempts.log"
TASKS_FILE="$TMP/fetch_tasks.json"
MOCK_LOG="$TMP/fetch_mock.log"

# Seed the mock's task/tag/comment state from the fixture envelope: getTask
# serves .task, getTaskTags serves the fixture tags as the real {tag_link_id:
# name} map, getAllComments serves .comments.
jq '{task: .task, tags: .tags, comments: (.comments // [])}' \
    "$TREE_ROOT/tests/fixtures/task_ready.json" > "$TASKS_FILE"
jq -n '{force_tags_error: false}' > "$FETCH_CFG"

node "$MOCK" "$FETCH_CFG" "$FETCH_LOG" "$TASKS_FILE" "$FETCH_ATTEMPTS" > "$MOCK_LOG" 2>&1 &
MOCK_PID=$!
trap 'kill "$MOCK_PID" 2>/dev/null; rm -rf "$TMP"' EXIT

PORT=""
tries=0
until [ -n "$PORT" ] || [ "$tries" -ge 50 ]; do
    PORT="$(head -n1 "$MOCK_LOG" 2>/dev/null || true)"
    tries=$((tries + 1))
    [ -n "$PORT" ] || sleep 0.1
done
if [ -z "$PORT" ]; then
    echo "FAIL: mock Kanboard server did not start: $(cat "$MOCK_LOG")" >&2
    exit 1
fi
KANBOARD_URL="http://127.0.0.1:$PORT"
export KANBOARD_URL

# Render mimics templatefile for the shellquoted variables.
fetch_script="$TMP/fetch_ticket.sh"
sed -e "s@{{ .intake_root | shellquote }}@$(shquote "$FETCH_DIR")@g" \
    -e "s@{{ .ticket_id | shellquote }}@$(shquote "$SLUG")@g" \
    "$TREE_ROOT/scripts/fetch_ticket.sh.tftpl" | splice_gh_retry > "$fetch_script"
chmod +x "$fetch_script"

rm -f "$FETCH_LOG"
if out="$("$fetch_script")"; then
    require_equal "$out" \
        "fetched KB-140: Deploy job races the cache warm step [column: 6]" \
        "fetch_ticket fetches and reports the task"
else
    fail "fetch_ticket failed: ${out:-<no output>}"
fi

# The getTaskTags request must carry task_id and nothing else: the extra
# project_id the pre-fix port sent is what real Kanboard rejects with
# -32602.
require_equal "$(cat "$FETCH_LOG")" '{"task_id":140}' \
    "getTaskTags is called with task_id only (no project_id: real Kanboard fails it with -32602)"

# The stored envelope must carry the task, the tags normalized to the
# [{name}] shape the fixtures pin, and the comment history.
ticket="$FETCH_RUN_DIR/ticket.json"
require_equal "$(jq -c '.tags' "$ticket")" \
    "$(jq -c '[.tags[]]' "$TREE_ROOT/tests/fixtures/task_ready.json")" \
    "ticket.json tags are normalized to the [{name}] envelope shape"
require_equal "$(jq -c '.task | {id, title, column_id}' "$ticket")" \
    '{"id":140,"title":"Deploy job races the cache warm step","column_id":6}' \
    "ticket.json carries the fetched task"
require_equal "$(jq -c '[.comments[].id]' "$ticket")" \
    "$(jq -c '[.comments[].id]' "$TREE_ROOT/tests/fixtures/task_ready.json")" \
    "ticket.json carries the comment history"

# An RPC error body must fail the fetch loudly, never be stored as tags —
# the exact defect the smoke run observed (the -32602 error blob stored as
# ticket.json's tags, the run reporting success). KB-231: the failure must
# also be a NAMED fatal carrying the failing method.
jq -n '{force_tags_error: true}' > "$FETCH_CFG"
rm -f "$ticket"
err_tags="$( "$fetch_script" 2>&1 >/dev/null )" && rc_tags=0 || rc_tags=$?
if [ "$rc_tags" -ne 0 ] && printf '%s' "$err_tags" | grep -q "kanboard rpc error: getTaskTags: Invalid params"; then
    ok "an RPC error body fails loudly with the named method (kanboard rpc error: getTaskTags)"
else
    fail "an RPC error body must fail with 'kanboard rpc error: getTaskTags: Invalid params' (rc=$rc_tags out=$err_tags)"
fi
[ ! -s "$ticket" ] \
    && ok "a failed fetch writes no ticket.json" \
    || fail "a failed fetch must not write ticket.json"

# ── 4a. KB-235: transport failures retry WITH BACKOFF, not single-shot ──────

# KB-48/KB-225: the pre-KB-235 transient-retry refired immediately, inside
# the same transport blackout, and failed identically. The curl transport is
# now wrapped in gh_retry (KB-235 contract, kanboard RPC surface): reset-style
# transport failures retry on the bounded exponential schedule — compressed
# to zero sleeps here via GH_RETRY_BACKOFF_SECONDS="0 0 0" — while the
# deterministic failures above still fail in a single attempt.

# Case 1 (the KB-235 acceptance case): the board transport-fails on getTask
# twice, then recovers — the fetch must ride the backoff through the outage
# and produce the full success result from exactly three server attempts.
jq -n '{reset_methods: {getTask: 2}}' > "$FETCH_CFG"
: > "$FETCH_ATTEMPTS"
rm -f "$FETCH_LOG"
rm -f "$ticket"
out_reset="$(GH_RETRY_BACKOFF_SECONDS="0 0 0" "$fetch_script" 2>/dev/null)" && rc_reset=0 || rc_reset=$?
attempts_reset="$(wc -l < "$FETCH_ATTEMPTS" | tr -d ' ')"
if [ "$rc_reset" -eq 0 ] \
    && [ "$out_reset" = "fetched KB-140: Deploy job races the cache warm step [column: 6]" ] \
    && [ "$attempts_reset" -eq 3 ] \
    && [ "$(grep -c '^getTask$' "$FETCH_ATTEMPTS")" -eq 3 ]; then
    ok "fetch rides the backoff through two transport resets (3 attempts, success output intact)"
else
    fail "fetch must ride the backoff through two resets: rc=$rc_reset out=${out_reset:-<none>} attempts=$attempts_reset"
fi

# Case 2 (the failure-injection case): the board kills getTask on every
# attempt — after the 1+3 bounded schedule the fetch must die with the KB-231
# named fatal carrying the method, from exactly four server attempts.
jq -n '{reset_methods: {getTask: 99}}' > "$FETCH_CFG"
: > "$FETCH_ATTEMPTS"
rm -f "$ticket"
err_exh="$(GH_RETRY_BACKOFF_SECONDS="0 0 0" "$fetch_script" 2>&1 >/dev/null)" && rc_exh=0 || rc_exh=$?
attempts_exh="$(wc -l < "$FETCH_ATTEMPTS" | tr -d ' ')"
if [ "$rc_exh" -ne 0 ] \
    && [ "$attempts_exh" -eq 4 ] \
    && printf '%s' "$err_exh" | grep -q "kanboard rpc transport: getTask"; then
    ok "exhausted transport retries fail with the named method after 4 attempts"
else
    fail "transport exhaustion must fail with 'kanboard rpc transport: getTask' after 4 attempts: rc=$rc_exh attempts=$attempts_exh err=${err_exh:-<none>}"
fi
[ ! -s "$ticket" ] \
    && ok "an exhausted transport writes no ticket.json" \
    || fail "an exhausted transport must not write ticket.json"

# Restore the happy-path config: the sections below share this mock.
jq -n '{force_tags_error: false}' > "$FETCH_CFG"
rm -f "$FETCH_LOG" "$FETCH_ATTEMPTS"

# ── 4b. Dead-board outage bodies fail with the named stage (KB-231) ─────────

# The 2026-10-09 outage: kanboard (behind a proxy) returned an empty body and
# an HTML 302 page with HTTP 200 while it was down. fetch_ticket used to turn
# that into "jq: invalid JSON text passed to --argjson" from a later call with
# NO failing stage named. The guard must catch both bodies at the call site
# and name the method that failed.
#
# Case 1+2: the mock serves body_mode empty / html302 for every RPC call, so
# getTask is the first hit and must die there.
for mode in empty html302; do
    jq -n --arg m "$mode" '{force_tags_error: false, body_mode: $m}' > "$FETCH_CFG"
    rm -f "$ticket"
    err="$( "$fetch_script" 2>&1 >/dev/null )" && rc_mode=0 || rc_mode=$?
    if [ "$mode" = empty ]; then want="kanboard rpc transport: getTask (empty response)"; else want="kanboard rpc transport: getTask (non-JSON-RPC body)"; fi
    if [ "$rc_mode" -ne 0 ] \
        && printf '%s' "$err" | grep -qF "$want" \
        && ! printf '%s' "$err" | grep -qF "invalid JSON text passed to --argjson"; then
        ok "an $mode body fails the fetch at getTask with its named fatal"
    else
        fail "an $mode body must fail with '$want', without any 'invalid JSON text' (rc=$rc_mode out=$err)"
    fi
    [ ! -s "$ticket" ] \
        && ok "an $mode body writes no ticket.json" \
        || fail "an $mode body must not write ticket.json"
done

# Case 3+4: the standalone comment path under the same outage bodies — a stub
# curl returns the body an outaged board hands curl (empty / HTML 302), and
# the createComment call site must name its stage.
run_guard_case() {
    # $1 = value for E2E_STUB_BODY; echoes the script's stderr, sets rc.
    render_comment_case comment_handler_done.sh.tftpl /tmp KB-24 approved \
        "workstreams/KB-24.md" "https://example.org/org/repo/pull/9" "kb-231-9f24c1a" main \
        >/dev/null
    KANBOARD_APP_TOKEN=stub-token KANBOARD_URL=http://127.0.0.1:1 \
        GH_RETRY_BACKOFF_SECONDS="0 0 0" \
        E2E_CAPTURE="$TMP" PATH="$STUBBIN:$PATH" E2E_STUB_BODY="$1" \
        bash "$TMP/case_comment.sh" 2>&1 >/dev/null
}
guard_err_empty="$(run_guard_case empty)" && rc_guard=0 || rc_guard=$?
if [ "$rc_guard" -ne 0 ] \
    && printf '%s' "$guard_err_empty" | grep -qF "kanboard rpc transport: createComment (empty response)" \
    && ! printf '%s' "$guard_err_empty" | grep -qF "invalid JSON text"; then
    ok "a comment post on an empty body names createComment as the failed stage"
else
    fail "an empty comment response must fail with 'kanboard rpc transport: createComment (empty response)' (rc=$rc_guard out=$guard_err_empty)"
fi
guard_err_html="$(run_guard_case html302)" && rc_guard=0 || rc_guard=$?
if [ "$rc_guard" -ne 0 ] \
    && printf '%s' "$guard_err_html" | grep -qF "kanboard rpc transport: createComment (non-JSON-RPC body)" \
    && ! printf '%s' "$guard_err_html" | grep -qF "invalid JSON text"; then
    ok "a comment post on an HTML 302 body names createComment as the failed stage"
else
    fail "an HTML 302 comment response must fail with 'kanboard rpc transport: createComment (non-JSON-RPC body)' (rc=$rc_guard out=$guard_err_html)"
fi

# ── 5. Handler success without a PR fails loudly (KB-49) ────────────────────

# A handler success carries pr_url. A success with an empty pr_url (commits
# pushed, no PR) must never reach the done-path bookkeeping — the ticket
# would be parked as done over work that was never merged under a PR that
# does not exist. The guard switch beneath the success outcome sends it down
# the failure-comment path with the KB-49 reason instead.
run_step=$(jq -c '.steps[] | select(.name == "run_handler")' "$GRAPH")
require_equal "$(printf '%s' "$run_step" | jq -r '.outcomes[] | select(.name == "success").next')" \
    "route_handler_pr" \
    "handler success routes through the empty-pr_url guard (KB-49)"
require_equal "$(printf '%s' "$run_step" | jq -r '.outcomes[] | select(.name == "failure").next')" \
    "comment_handler_failed" \
    "handler failure routing is unchanged by the PR guard"

guard_cond="$(jq -c '.switches[] | select(.name == "route_handler_pr")' "$GRAPH")"
require_equal "$(printf '%s' "$guard_cond" | jq -r '.conditions[0].match + " -> " + .conditions[0].next')" \
    'data.internal.pr_url.value != "" -> comment_handler_done' \
    "a non-empty pr_url keeps the done-path comment routing"
require_equal "$(printf '%s' "$guard_cond" | jq -r '.default_next')" \
    "flag_missing_pr_url" \
    "an empty pr_url routes to the KB-49 loud-failure step"

flag_step=$(sed -n '/^step "flag_missing_pr_url" {/,/^}/p' "$TREE_ROOT/main.chcl")
printf '%s' "$flag_step" | grep -q 'KB-49: the handler reported success with an empty pr_url' \
    && require_equal "$(printf '%s' "$flag_step" | grep -c 'target = data.internal.handler_error.value')" \
        "2" \
        "flag_missing_pr_url records the KB-49 reason on the handler_error channel"
require_equal "$(printf '%s' "$flag_step" | grep -c 'next = step.comment_handler_failed')" \
    "2" \
    "flag_missing_pr_url routes both outcomes to the failure comment path"

# ── 6. Sibling trees untouched ───────────────────────────────────────────────

untouched="$(git -C "$REPO_ROOT" status --porcelain -- linear_intake_v1 linear_triage_v1 linear_develop_v1 qa_triage_v1 workstream_handler_v1)"
if [ -z "$untouched" ]; then
    ok "linear_* and shared subworkflow trees are untouched (no uncommitted changes)"
else
    fail "linear_*/shared trees have uncommitted changes: $untouched"
fi

if [ "$FAILED" -gt 0 ]; then
    echo "FAILED: $FAILED assertion(s)" >&2
    exit 1
fi

echo "all develop standalone tests passed"
