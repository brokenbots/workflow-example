#!/usr/bin/env bash
# devops_maint_v1 — Lane A act + merge step regression suite.
#
# Exercises the rendered act_rebase_consolidate.sh and merge_acts.sh
# END-TO-END through a stub gh (no network, no real GitHub):
#   - gh_retry shim is byte-identical to the canonical wrapper copy
#   - act: empty plan, rebase with verified read-back, per-carrier failure
#     bookkeeping, view-failure handling, config gates firing before any
#     network call, artifact shape + self-check, stdout protocol, residue
#   - merge: empty plan, the four typed gate deferrals, gated merge + close
#     out (close/comment failure bookkeeping), carrier evidence comment,
#     merge failure keeps the artifact and exits non-zero with a typed hint,
#     config gates pre-network, token hygiene, residue
#   - token hygiene: the GH_TOKEN canary never lands in artifacts or stderr
#   - tree validation (node + criteria), matching the other step suites

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
TREE_DIR="$(cd "${ROOT_DIR}/.." && pwd)"
ACT_TEMPLATE="${ROOT_DIR}/scripts/act_rebase_consolidate.sh.tftpl"
MERGE_TEMPLATE="${ROOT_DIR}/scripts/merge_acts.sh.tftpl"
WRAPPER="${ROOT_DIR}/scripts/gh_retry.sh.tftpl"
CANONICAL_WRAPPER="${TREE_DIR}/workstream_handler_v1/scripts/gh_retry.sh.tftpl"

WORK_ROOT="${TEST_WORK_ROOT:-$(mktemp -d)}"
if [ "${TEST_KEEP_WORK_ROOT:-0}" != "1" ]; then trap 'rm -rf "${WORK_ROOT}"' EXIT; fi

FAILURES=0
check() {
    local label="$1"
    shift
    if "$@"; then
        echo "PASS: ${label}" >&2
    else
        echo "FAIL: ${label}" >&2
        FAILURES=$((FAILURES + 1))
    fi
}

check_not() {
    local label="$1"
    shift
    if "$@"; then
        echo "FAIL: ${label}" >&2
        FAILURES=$((FAILURES + 1))
    else
        echo "PASS: ${label}" >&2
    fi
}

expect_count() {
    # expect_count <label> <want> <got>
    if [ "$2" = "$3" ]; then
        echo "PASS: $1" >&2
    else
        echo "FAIL: $1 (want $2, got $3)" >&2
        FAILURES=$((FAILURES + 1))
    fi
}

assert_no_single_quote() {
    case "$1" in
        *"'"*)
            echo "BUG: test value contains a single quote which would break the shellquote render" >&2
            exit 99
            ;;
    esac
}

BIN_STUB_GH="${WORK_ROOT}/bin-stub-gh"
mkdir -p "${BIN_STUB_GH}"

# ── Stub gh ──────────────────────────────────────────────────────────────────
# Fixture-driven, per-scenario fixtures dir. Every invocation appends its args
# to LANE_GH_LOG so suites can assert call sequences. Fixtures:
#   view.<num>.json         gate/read state (gh --json fields)
#   view.<num>.2.json       second read-back state (act: after update-branch)
#   view.<num>.rc + .err    deterministic view failure (both reads fail)
#   update.<num>.rc + .err  update-branch failure
#   merge.<num>.rc + .err   gated merge failure
#   close.<num>.rc + .err   close-out failure (superseded member PR)
#   comment.<num>.rc + .err carrier evidence comment failure
# A counter file view.<num>.count serves view.2.json on the second view.
cat >"${BIN_STUB_GH}/gh" <<'EOF'
#!/usr/bin/env bash
set -u
fx="${LANE_GH_FIXTURES:?LANE_GH_FIXTURES not set}"
log="${LANE_GH_LOG:-/dev/null}"
printf '%s\n' "$*" >>"${log}"

if [ "$#" -lt 2 ]; then
    echo "stub: unknown gh invocation: $*" >&2
    exit 9
fi
mode="$1"
shift
sub="${1:-}"
num="${2:-}"
num="$(printf '%s' "${num}" | tr -cd '0-9')"
num="${num:-0}"

case "$mode" in
    pr)
        case "$sub" in
            view)
                if [ -f "${fx}/view.${num}.rc" ]; then
                    rc="$(cat "${fx}/view.${num}.rc")"
                    cat "${fx}/view.${num}.err" >&2 2>/dev/null || echo "gh: fixture failure" >&2
                    exit "${rc}"
                fi
                if [ ! -f "${fx}/view.${num}.json" ]; then
                    echo "stub: missing fixture view.${num}.json" >&2
                    exit 9
                fi
                cntfile="${fx}/view.${num}.count"
                n="$(cat "${cntfile}" 2>/dev/null || echo 0)"
                echo "$((n + 1))" >"${cntfile}"
                if [ "${n}" -gt 0 ] && [ -f "${fx}/view.${num}.2.json" ]; then
                    cat "${fx}/view.${num}.2.json"
                    exit 0
                fi
                cat "${fx}/view.${num}.json"
                exit 0
                ;;
            update-branch)
                if [ -f "${fx}/update.${num}.rc" ]; then
                    rc="$(cat "${fx}/update.${num}.rc")"
                    cat "${fx}/update.${num}.err" >&2 2>/dev/null || echo "gh: fixture failure" >&2
                    exit "${rc}"
                fi
                exit 0
                ;;
            merge)
                if [ -f "${fx}/merge.${num}.rc" ]; then
                    rc="$(cat "${fx}/merge.${num}.rc")"
                    cat "${fx}/merge.${num}.err" >&2 2>/dev/null || echo "gh: fixture failure" >&2
                    exit "${rc}"
                fi
                exit 0
                ;;
            close)
                if [ -f "${fx}/close.${num}.rc" ]; then
                    rc="$(cat "${fx}/close.${num}.rc")"
                    cat "${fx}/close.${num}.err" >&2 2>/dev/null || echo "gh: fixture failure" >&2
                    exit "${rc}"
                fi
                exit 0
                ;;
            comment)
                if [ -f "${fx}/comment.${num}.rc" ]; then
                    rc="$(cat "${fx}/comment.${num}.rc")"
                    cat "${fx}/comment.${num}.err" >&2 2>/dev/null || echo "gh: fixture failure" >&2
                    exit "${rc}"
                fi
                exit 0
                ;;
        esac
        echo "stub: unknown gh pr subcommand: ${sub}" >&2
        exit 9
        ;;
esac
echo "stub: unknown gh invocation: $*" >&2
exit 9
EOF
chmod +x "${BIN_STUB_GH}/gh"

# ── Renderers ────────────────────────────────────────────────────────────────
# Engine-faithful rendering: the gh_retry marker is REPLACED WITH CONTENT
# (file() yields the body, not a path) and the scalar arguments are spliced
# as shellquoted literals, after a no-single-quote guard on every value.
render_lane_step() {
    local template="$1" org="$2" class_path="$3" out="$4"
    assert_no_single_quote "${org}"
    assert_no_single_quote "${class_path}"
    local wrap
    wrap="$(cat "${WRAPPER}")"
    local o_rep="'${org}'" c_rep="'${class_path}'"
    local rendered
    rendered="$(cat "${template}")"
    rendered="${rendered//'{{ .gh_retry }}'/"${wrap}"}"
    rendered="${rendered//'{{ .org | shellquote }}'/"${o_rep}"}"
    rendered="${rendered//'{{ .classification_file | shellquote }}'/"${c_rep}"}"
    printf '%s' "${rendered}" >"${out}"
    chmod +x "${out}"
    if grep -q '{{' "${out}"; then
        echo "BUG: rendered lane step still contains template markers" >&2
        exit 99
    fi
}

render_act() {
    render_lane_step "${ACT_TEMPLATE}" "$1" "$2" "$3"
}

render_merge() {
    render_lane_step "${MERGE_TEMPLATE}" "$1" "$2" "$3"
}

# ── Classification artifacts (honest shape, reduced content) ─────────────────
# classification_v1 <out> <org> <sweep_id> <groups_json>
# Groups carry the full act/merge-relevant surface; rows mirror the groups so
# the artifact stays internally consistent like a real classify run.
classification_v1() {
    local out="$1" org="$2" sweep="$3" groups_json="$4"
    jq -cn \
        --arg org "${org}" --arg sid "${sweep}" \
        --arg inv "run-000001/inventory.json" \
        --argjson groups "${groups_json}" '
        [$groups[] as $g
         | ($g.closes[] | {
            number: .,
            class: $g.action_class,
            repo: $g.repo,
            module: $g.module,
            title: ("Bump " + $g.module + " (superseded)"),
            from_version: "1.0.0",
            to_version: $g.to_version,
            base_ref: $g.base_ref,
            merge_state: "CLEAN",
            updated_at: "2026-11-14T09:00:00Z",
            advisory_ids: [],
            reasons: [],
            group_id: $g.group_id,
            is_carrier: false,
            report_only: false,
            report_reason: null,
            ci_verdict: "green",
            ci: {verdict: "green", check_count: 1, checks: [], error: null},
            judgment_reason: null
          }),
          {number: $g.carrier.number,
           class: $g.action_class,
           repo: $g.repo,
           module: $g.module,
           title: ("Bump " + $g.module + " to " + $g.to_version),
           from_version: "1.0.0",
           to_version: $g.to_version,
           base_ref: $g.base_ref,
           merge_state: (if $g.action_class == "needs_rebase" then "BEHIND" else "CLEAN" end),
           updated_at: "2026-11-14T09:05:00Z",
           advisory_ids: [],
           reasons: [],
           group_id: $g.group_id,
           is_carrier: true,
           report_only: false,
           report_reason: null,
           ci_verdict: "green",
           ci: {verdict: "green", check_count: 1, checks: [], error: null},
           judgment_reason: null}
        ] as $rows
        | {
            schema: "devops_maint_v1.classification.v1",
            run: {sweep_id: $sid,
                  org: $org,
                  repos: [$groups[].repo] | unique,
                  inventory_path: $inv,
                  started_at: "2026-11-14T09:00:00Z",
                  finished_at: "2026-11-14T09:01:00Z"},
            summary: {
                rows_total: ($rows | length),
                mechanical_bump: ([$rows[] | select(.class == "mechanical_bump")] | length),
                vuln_clear: 0,
                needs_rebase: ([$rows[] | select(.class == "needs_rebase")] | length),
                needs_judgment: 0,
                report_hold: 0,
                report_only: 0,
                parked_groups: 0,
                groups_total: ($groups | length),
                carriers_total: ($groups | length),
                closes_total: (([$groups[].closes | length] | add) // 0),
                rebases_total: ([$groups[] | select(.action_class == "needs_rebase")] | length)
            },
            rows: $rows,
            groups: $groups,
            rebases: [$groups
                      | map(select(.action_class == "needs_rebase" and .carrier.number))[]
                      | {repo: .repo, number: .carrier.number, group_id: .group_id,
                         module: .module, to_version: .to_version,
                         merge_state: "BEHIND", base_ref: .base_ref}],
            report: []
          }' >"${out}"
}

# grp <gid> <repo> <module> <to_version> <action_class> <closes_csv> <carrier> [base_ref]
# Emits one JSON group object (jq emits, so keys/values are always valid).
grp() {
    jq -cn --arg gid "$1" --arg repo "$2" --arg module "$3" --arg tv "$4" \
        --arg ac "$5" --argjson closes "[${6}]" --argjson carrier "$7" --arg base "${8:-main}" \
        '{group_id: $gid, repo: $repo, module: $module, to_version: $tv, base_ref: $base,
          action_class: $ac, closes: $closes,
          carrier: {repo: $repo, number: $carrier,
                    title: ("Bump " + $module + " to " + $tv),
                    updated_at: "2026-11-14T09:05:00Z"}}'
}

# groups_json <grp output>... — joins group objects into one JSON array.
groups_json() {
    local out="" first=1 obj
    for obj in "$@"; do
        if [ "${first}" -eq 1 ]; then
            out="${obj}"
            first=0
        else
            out="${out},
${obj}"
        fi
    done
    printf '[%s]' "${out}"
}

# gate_json <reviewDecision> <mergeable> <mergeStateStatus> <rollup_json>
gate_json() {
    jq -cn --arg rev "$1" --arg mrg "$2" --arg mss "$3" --argjson roll "$4" \
        '{reviewDecision: $rev, mergeable: $mrg, mergeStateStatus: $mss, statusCheckRollup: $roll}'
}

# ── Runners ──────────────────────────────────────────────────────────────────
# run_lane <rendered.sh> <fixtures-dir> <out> <err> [GH_RETRY_MAX_ATTEMPTS=n]
# The fixtures dir is separate from the steps' run dir (the classification's
# parent), so residue assertions can be exact.
run_lane() {
    local script="$1" fixtures="$2" out="$3" err="$4" rc=0
    rm -f "${fixtures}"/view.*.count
    set +e
    (
        export PATH="${BIN_STUB_GH}:${PATH}"
        export LANE_GH_FIXTURES="${fixtures}"
        export LANE_GH_LOG="${fixtures}/call.log"
        export GH_RETRY_MAX_ATTEMPTS="${5:-1}"
        export GH_RETRY_BACKOFF_SECONDS="0 0 0"
        export GH_RETRY_SCRIPT_SLEEP_BUDGET=0
        export GH_TOKEN="supersecret-token-canary"
        "${script}"
    ) >"${out}" 2>"${err}"
    rc=$?
    set -e
    return "${rc}"
}

# ═════════════════════════════════════════════════════════════════════════════
# 1. Structural wiring
# ═════════════════════════════════════════════════════════════════════════════
check "gh_retry vendored byte-identical to workstream_handler_v1" cmp -s "${WRAPPER}" "${CANONICAL_WRAPPER}"
check "main.chcl renders the act step through templatefile" grep -q 'templatefile("./scripts/act_rebase_consolidate.sh.tftpl"' "${ROOT_DIR}/main.chcl"
check "main.chcl renders the merge step through templatefile" grep -q 'templatefile("./scripts/merge_acts.sh.tftpl"' "${ROOT_DIR}/main.chcl"
check_step_binding() {
    # check_step_binding <step_name> <binding_regex> <main.chcl>
    local step_name="$1" binding="$2" file="$3"
    local block
    block="$(awk -v s="step \"${step_name}\"" '
        $0 ~ s {inblock = 1}
        inblock && /^\}/ {exit}
        inblock {print}
    ' "${file}")"
    printf '%s\n' "${block}" | grep -Eq "${binding}"
}
check "main.chcl feeds the classify artifact into the act step" check_step_binding act 'classification_file *= *data\.internal\.classification_file\.value' "${ROOT_DIR}/main.chcl"
check "main.chcl feeds the classify artifact into the merge step" check_step_binding merge 'classification_file *= *data\.internal\.classification_file\.value' "${ROOT_DIR}/main.chcl"
check "act template splices the wrapper as raw content" test "$(grep -o '{{ *\.gh_retry }}' "${ACT_TEMPLATE}" | wc -l)" = "1"
check "merge template splices the wrapper as raw content" test "$(grep -o '{{ *\.gh_retry }}' "${MERGE_TEMPLATE}" | wc -l)" = "1"
check "act template consumes the classification marker exactly once" test "$(grep -c '{{ .classification_file | shellquote }}' "${ACT_TEMPLATE}")" = "1"
check "merge template consumes the classification marker exactly once" test "$(grep -c '{{ .classification_file | shellquote }}' "${MERGE_TEMPLATE}")" = "1"
check "act template records the acts schema" grep -q 'devops_maint_v1.acts.v1' "${ACT_TEMPLATE}"
check "merge template records the merged schema" grep -q 'devops_maint_v1.merged.v1' "${MERGE_TEMPLATE}"

# ═════════════════════════════════════════════════════════════════════════════
# 2. Act step: deterministic rebase plan with verified read-back
# ═════════════════════════════════════════════════════════════════════════════
FXA="${WORK_ROOT}/act1"
FIXA="${WORK_ROOT}/fix-act1"
mkdir -p "${FXA}" "${FIXA}"
# Sentinel: the shared raw evidence lane that collect owns (A1 regression guard —
# the lane step's scratch dir is step-scoped and must never touch this lane).
mkdir -p "${FXA}/raw"
printf 'collect-evidence-sentinel\n' >"${FXA}/raw/errors.log"
classification_v1 "${FXA}/classification.json" testorg sw-act \
    "$(groups_json "$(grp g001 alpha golang.org/x/net 0.21.0 needs_rebase '' 23 main)" "$(grp g002 alpha github.com/stuck/mod 1.5.0 needs_rebase '' 44 release-1)")"
printf '%s' '{"reviewDecision": "APPROVED", "mergeable": "MERGEABLE", "mergeStateStatus": "BEHIND", "statusCheckRollup": [{"status": "COMPLETED", "conclusion": "SUCCESS"}]}' >"${FIXA}/view.23.json"
printf '%s' '{"reviewDecision": "APPROVED", "mergeable": "MERGEABLE", "mergeStateStatus": "CLEAN", "statusCheckRollup": [{"status": "COMPLETED", "conclusion": "SUCCESS"}]}' >"${FIXA}/view.23.2.json"
printf '%s' '{"reviewDecision": "APPROVED", "mergeable": "MERGEABLE", "mergeStateStatus": "DIRTY", "statusCheckRollup": []}' >"${FIXA}/view.44.json"
printf 'gh: HTTP 422: merge conflict' >"${FIXA}/update.44.err"
printf '%s' '1' >"${FIXA}/update.44.rc"
render_act testorg "${FXA}/classification.json" "${WORK_ROOT}/act1.sh"
OUTA="${WORK_ROOT}/a1.out"
ERRA="${WORK_ROOT}/a1.err"
ACT_RC=0
run_lane "${WORK_ROOT}/act1.sh" "${FIXA}" "${OUTA}" "${ERRA}" || ACT_RC=$?
expect_count "A1 act exits 0 with per-carrier bookkeeping" 0 "${ACT_RC}"
ACTS1="$(tr -d '[:space:]' <"${OUTA}")"
check "A1 stdout carries exactly the acts artifact path" test "${ACTS1}" = "${FXA}/acts.json"
check "A1 acts artifact is a sibling of the classification" jq -e '.schema == "devops_maint_v1.acts.v1"' "${ACTS1}" >/dev/null
check "A1 run block mirrors the classification identity" jq -e '.run.sweep_id == "sw-act" and .run.org == "testorg"' "${ACTS1}" >/dev/null
check "A1 summary splits the plan into rebased and failed" jq -e '.summary == {planned: 2, rebased: 1, failed: 1}' "${ACTS1}" >/dev/null
check "A1 the clean rebased row records the verified read-back" jq -e '[.rebases[] | select(.number == 23)] | length == 1 and .[0].result == "rebased" and .[0].before_state == "BEHIND" and .[0].after_state == "CLEAN" and .[0].group_id == "g001" and .[0].note == null' "${ACTS1}" >/dev/null
check "A1 the failed row keeps its type and the transport note" jq -e '[.rebases[] | select(.number == 44)] | length == 1 and .[0].result == "failed" and .[0].after_state == null and (.[0].note | test("HTTP 422")) and .[0].before_state == "DIRTY"' "${ACTS1}" >/dev/null
check "A1 gh saw five calls (view/update/view for 23, view/update for 44)" test "$(grep -c . "${FIXA}/call.log")" = "5"
check "A1 the rebase gate view runs against the org repo" grep -q 'pr view 23 -R testorg/alpha' "${FIXA}/call.log" >/dev/null
check "A1 the rebase targets the PR's own branch on the org repo" grep -q 'pr update-branch 23 -R testorg/alpha' "${FIXA}/call.log" >/dev/null
check_not "A1 the act lane never merges" grep -q 'pr merge' "${FIXA}/call.log"

check "A2 stderr logs the plan line" grep -q 'act: rebase plan: 2 carrier(s)' "${ERRA}"
check "A2 stderr logs per-carrier evidence" grep -q 'act: rebase #44 in testorg/alpha (group g002, merge_state=BEHIND)' "${ERRA}"
check_not "A2 stderr on success never prints the typed failure prefix" grep -q 'act_failed' "${ERRA}"
check "A2 no temp residue next to the classification (A1)" test "$(ls -A "${FXA}" | sort | tr '\n' ' ')" = "acts.json classification.json raw "
check "A2 the shared raw evidence lane survives the act step" test -d "${FXA}/raw" && test "$(cat "${FXA}/raw/errors.log")" = "collect-evidence-sentinel"
check_not "A2 no step-scoped act scratch dir is left behind" ls "${FXA}"/.act.raw.* >/dev/null 2>&1

# ═════════════════════════════════════════════════════════════════════════════
# 3. Act step: pre-network config gates + empty plan
# ═════════════════════════════════════════════════════════════════════════════
FXB="${WORK_ROOT}/act2"
FIXB="${WORK_ROOT}/fix-act2"
mkdir -p "${FXB}" "${FIXB}" "${FXB}/x"
classification_v1 "${FXB}/classification.json" testorg sw-empty "$(groups_json)"
render_act testorg "${FXB}/classification.json" "${WORK_ROOT}/act2.sh"
ACT0_RC=0
run_lane "${WORK_ROOT}/act2.sh" "${FIXB}" "${WORK_ROOT}/a2b.out" "${WORK_ROOT}/a2b.err" || ACT0_RC=$?
expect_count "A3 empty rebase plan exits 0" 0 "${ACT0_RC}"
check "A3 empty plan yields the zero artifact" jq -e '.summary == {planned: 0, rebased: 0, failed: 0} and .rebases == []' "${FXB}/acts.json" >/dev/null
check "A3 empty plan makes no network calls" test ! -s "${FIXB}/call.log"

render_act testorg "${FXB}/class.notthere.json" "${WORK_ROOT}/act3.sh"
ACT5A_RC=0
run_lane "${WORK_ROOT}/act3.sh" "${FIXB}" "${WORK_ROOT}/a3.out" "${WORK_ROOT}/a3.err" || ACT5A_RC=$?
expect_count "A4a missing classification exits 1" 1 "${ACT5A_RC}"
check "A4a typed fail names the artifact" grep -q 'act_failed: classification artifact not found' "${WORK_ROOT}/a3.err"

printf '{"schema": "other.tree.v1"}' >"${FXB}/bad.json"
render_act testorg "${FXB}/bad.json" "${WORK_ROOT}/act4.sh"
ACT5B_RC=0
run_lane "${WORK_ROOT}/act4.sh" "${FIXB}" "${WORK_ROOT}/a4.out" "${WORK_ROOT}/a4.err" || ACT5B_RC=$?
expect_count "A4b wrong-schema artifact exits 1" 1 "${ACT5B_RC}"
check "A4b typed fail names the artifact contract" grep -q 'act_failed: input is not a devops_maint_v1.classification.v1 artifact' "${WORK_ROOT}/a4.err"

classification_v1 "${FXB}/ok.json" testorg sw-act "$(groups_json)"
render_act testorg "${FXB}/x/../ok.json" "${WORK_ROOT}/act5.sh"
ACT5C_RC=0
run_lane "${WORK_ROOT}/act5.sh" "${FIXB}" "${WORK_ROOT}/a5.out" "${WORK_ROOT}/a5.err" || ACT5C_RC=$?
expect_count "A4c '..' path exits 1" 1 "${ACT5C_RC}"
check "A4c typed fail names the traversal guard" grep -q "act_failed: classification path must not contain '..'" "${WORK_ROOT}/a5.err"

render_act badorg_ "${FXB}/ok.json" "${WORK_ROOT}/act6.sh"
ACT5D_RC=0
run_lane "${WORK_ROOT}/act6.sh" "${FIXB}" "${WORK_ROOT}/a6.out" "${WORK_ROOT}/a6.err" || ACT5D_RC=$?
expect_count "A4d non-conformant org exits 1" 1 "${ACT5D_RC}"
check "A4d typed fail names the org contract" grep -q 'act_failed: org must match' "${WORK_ROOT}/a6.err"

check "A4 every pre-network gate failure left the gh call log untouched" test ! -s "${FIXB}/call.log"

# ═════════════════════════════════════════════════════════════════════════════
# 4. Act step: view failure mid-plan is typed bookkeeping, not a crash
# ═════════════════════════════════════════════════════════════════════════════
# The act lane does not gate on a failed gate view: the state degrades to
# UNKNOWN and the update-branch attempt still runs (self-check accepts the
# degraded states; a real update failure is the typed failed row in A1).
FXC="${WORK_ROOT}/act3"
FIXC="${WORK_ROOT}/fix-act3"
mkdir -p "${FXC}" "${FIXC}"
classification_v1 "${FXC}/classification.json" testorg sw-act \
    "$(groups_json "$(grp g009 alpha golang.org/net2 0.30.0 needs_rebase '' 70 main)")"
printf 'gh: HTTP 500 (transport, capped by knobs)' >"${FIXC}/view.70.err"
printf '%s' '1' >"${FIXC}/view.70.rc"
render_act testorg "${FXC}/classification.json" "${WORK_ROOT}/act7.sh"
ACTV_RC=0
run_lane "${WORK_ROOT}/act7.sh" "${FIXC}" "${WORK_ROOT}/a7.out" "${WORK_ROOT}/a7.err" || ACTV_RC=$?
expect_count "A5 act still exits 0 when the gate view fails" 0 "${ACTV_RC}"
check "A5 the degraded row records UNKNOWN states and no note" jq -e '[.rebases[]][0] as $r | $r.result == "rebased" and $r.before_state == "UNKNOWN" and $r.after_state == "UNKNOWN" and $r.note == null' "${FXC}/acts.json" >/dev/null
check "A5 the update-branch attempt still ran despite the view failure" grep -q 'pr update-branch 70 -R testorg/alpha' "${FIXC}/call.log" >/dev/null
check "A5 the degraded traffic is exactly view, update, post-view" test "$(grep -c . "${FIXC}/call.log")" = "3"

# ═════════════════════════════════════════════════════════════════════════════
# 5. Merge step: gated merges, close-out, evidence comment
# ═════════════════════════════════════════════════════════════════════════════
FXM="${WORK_ROOT}/merge1"
FIXM="${WORK_ROOT}/fix-merge1"
mkdir -p "${FXM}" "${FIXM}"
# The same A1 regression guard on the merge lane: raw/ is collect's evidence lane.
mkdir -p "${FXM}/raw"
printf 'collect-evidence-sentinel\n' >"${FXM}/raw/errors.log"
classification_v1 "${FXM}/classification.json" testorg sw-lane \
    "$(groups_json "$(grp g002 alpha github.com/consul/mod 1.2.5 mechanical_bump 31 32 main)")"
printf '%s' "$(gate_json APPROVED MERGEABLE CLEAN '[{"status": "COMPLETED", "conclusion": "SUCCESS"}]')" >"${FIXM}/view.32.json"
render_merge testorg "${FXM}/classification.json" "${WORK_ROOT}/merge1.sh"
OUTM="${WORK_ROOT}/m1.out"
ERRM="${WORK_ROOT}/m1.err"
MERGE_RC=0
run_lane "${WORK_ROOT}/merge1.sh" "${FIXM}" "${OUTM}" "${ERRM}" || MERGE_RC=$?
expect_count "M1 merge exits 0 with the gate/merge/close-out flow" 0 "${MERGE_RC}"
MERGED1="$(tr -d '[:space:]' <"${OUTM}")"
check "M1 stdout carries exactly the merged artifact path" test "${MERGED1}" = "${FXM}/merged.json"
check "M1 merged artifact is a sibling of the classification" jq -e '.schema == "devops_maint_v1.merged.v1"' "${MERGED1}" >/dev/null
check "M1 run block mirrors the classification identity" jq -e '.run.sweep_id == "sw-lane" and .run.org == "testorg"' "${MERGED1}" >/dev/null
check "M1 summary reconciles carriers, merges, deferrals and failures" jq -e '.summary == {carriers_total: 1, merged: 1, deferred: 0, merge_failures: 0, closes_done: 1}' "${MERGED1}" >/dev/null
check "M1 the group row records the merge, close-out, and evidence comment" jq -e '[.groups[]][0] as $r | $r.group_id == "g002" and $r.carrier == 32 and $r.merged and ($r.deferred | not) and $r.reason == null and $r.closes == [31] and $r.closes_failed == [] and $r.carrier_comment == 1' "${MERGED1}" >/dev/null
check "M1 the gate view asked for exactly the four gate fields" grep -q 'pr view 32 -R testorg/alpha --json reviewDecision,mergeable,mergeStateStatus,statusCheckRollup' "${FIXM}/call.log"
check "M1 the carrier merged with an explicit merge commit" grep -q 'pr merge 32 -R testorg/alpha --merge' "${FIXM}/call.log"
check "M1 the superseded member closed with the supersede-by-carrier message" grep -F 'Closing as superseded by #32 (consolidated github.com/consul/mod bump).' "${FIXM}/call.log" >/dev/null
check "M1 the carrier carries the sweep evidence comment" grep -F 'devops_maint_v1 (sweep sw-lane): consolidated the github.com/consul/mod bump (group g002); superseded dependabot PRs closed after this merge: #31.' "${FIXM}/call.log" >/dev/null
check "M1 gh traffic was exactly view, merge, close, comment" test "$(grep -c . "${FIXM}/call.log")" = "4"
check "M2 stderr logs the plan line" grep -q 'merge: merge plan: 1 carrier(s)' "${ERRM}"
check "M2 stderr logs the close-out evidence" grep -q 'merge: group g002 in testorg/alpha: gate carrier #32' "${ERRM}"
check_not "M2 success run never prints the typed failure prefix" grep -q 'merge_failed' "${ERRM}"
check "M2 no temp residue next to the classification (M1)" test "$(ls -A "${FXM}" | sort | tr '\n' ' ')" = "classification.json merged.json raw "
check "M2 the shared raw evidence lane survives the merge step" test -d "${FXM}/raw" && test "$(cat "${FXM}/raw/errors.log")" = "collect-evidence-sentinel"
check_not "M2 no step-scoped merge scratch dir is left behind" ls "${FXM}"/.merge.raw.* >/dev/null 2>&1

# ═════════════════════════════════════════════════════════════════════════════
# 6. Merge step: the four typed gate deferrals, never a merge call
# ═════════════════════════════════════════════════════════════════════════════
FXD="${WORK_ROOT}/merge2"
FIXD="${WORK_ROOT}/fix-merge2"
mkdir -p "${FXD}" "${FIXD}"
classification_v1 "${FXD}/classification.json" testorg sw-lane "$(groups_json \
    "$(grp g101 alpha github.com/a/mod 1.1.0 mechanical_bump '' 101 main)" \
    "$(grp g102 alpha github.com/b/mod 1.1.0 mechanical_bump '' 102 main)" \
    "$(grp g103 alpha github.com/c/mod 1.1.0 mechanical_bump '' 103 main)" \
    "$(grp g104 alpha github.com/d/mod 1.1.0 mechanical_bump '' 104 main)")"
printf '%s' "$(gate_json REVIEW_REQUIRED MERGEABLE CLEAN '[{"status": "COMPLETED", "conclusion": "SUCCESS"}]')" >"${FIXD}/view.101.json"
printf '%s' "$(gate_json APPROVED CONFLICTING CLEAN '[{"status": "COMPLETED", "conclusion": "SUCCESS"}]')" >"${FIXD}/view.102.json"
printf '%s' "$(gate_json APPROVED MERGEABLE DIRTY '[{"status": "COMPLETED", "conclusion": "SUCCESS"}]')" >"${FIXD}/view.103.json"
printf '%s' "$(gate_json APPROVED MERGEABLE CLEAN '[{"status": "IN_PROGRESS", "conclusion": null}]')" >"${FIXD}/view.104.json"
render_merge testorg "${FXD}/classification.json" "${WORK_ROOT}/merge2.sh"
DEF_RC=0
run_lane "${WORK_ROOT}/merge2.sh" "${FIXD}" "${WORK_ROOT}/m2.out" "${WORK_ROOT}/m2.err" || DEF_RC=$?
expect_count "M3 all-deferred plan exits 0" 0 "${DEF_RC}"
MERGED2="$(tr -d '[:space:]' <"${WORK_ROOT}/m2.out")"
check "M3 deferral summary keeps the invariant" jq -e '.summary == {carriers_total: 4, merged: 0, deferred: 4, merge_failures: 0, closes_done: 0}' "${MERGED2}" >/dev/null
check "M3 awaiting_review defers with its typed reason" jq -e '[.groups[] | select(.carrier == 101)][0] as $r | $r.deferred and $r.reason == "awaiting_review" and ($r.merged | not)' "${MERGED2}" >/dev/null
check "M3 conflicting gate defers as not_mergeable" jq -e '[.groups[] | select(.carrier == 102)][0].reason == "not_mergeable"' "${MERGED2}" >/dev/null
check "M3 dirty merge-state defers as base_dirty" jq -e '[.groups[] | select(.carrier == 103)][0].reason == "base_dirty"' "${MERGED2}" >/dev/null
check "M3 in-progress checks defer as checks_pending" jq -e '[.groups[] | select(.carrier == 104)][0].reason == "checks_pending"' "${MERGED2}" >/dev/null
check "M3 deferred rows keep empty close-out surfaces" jq -e '[.groups[] | select(.deferred) | .closes == [] and .closes_failed == [] and .carrier_comment == 0] | length == 4' "${MERGED2}" >/dev/null
check "M3 no merge call ever ran" test "$(grep -c 'pr merge' "${FIXD}/call.log")" = "0" >/dev/null
check "M3 the close-out never fires on deferral" test "$(grep -c 'pr close' "${FIXD}/call.log")" = "0" >/dev/null
check "M3 the comment call never fires on deferral" test "$(grep -c 'pr comment' "${FIXD}/call.log")" = "0" >/dev/null
check "M3 only the four gate views ran" test "$(grep -c 'pr view' "${FIXD}/call.log")" = "4"
check "M3 each deferral is typed on stderr too" test "$(grep -c 'DEFERRED (' "${WORK_ROOT}/m2.err")" = "4"

# ═════════════════════════════════════════════════════════════════════════════
# 7. Merge step: failed gate view defers with an evidence note
# ═════════════════════════════════════════════════════════════════════════════
FXE="${WORK_ROOT}/merge3"
FIXE="${WORK_ROOT}/fix-merge3"
mkdir -p "${FXE}" "${FIXE}"
classification_v1 "${FXE}/classification.json" testorg sw-lane \
    "$(groups_json "$(grp g201 alpha github.com/vfail/mod 1.1.0 mechanical_bump '' 201 main)")"
printf 'gh: HTTP 500 (gate view transport)' >"${FIXE}/view.201.err"
printf '%s' '1' >"${FIXE}/view.201.rc"
render_merge testorg "${FXE}/classification.json" "${WORK_ROOT}/merge3.sh"
VF_RC=0
run_lane "${WORK_ROOT}/merge3.sh" "${FIXE}" "${WORK_ROOT}/m3.out" "${WORK_ROOT}/m3.err" || VF_RC=$?
expect_count "M4 gate-view failure exits 0 as a deferral" 0 "${VF_RC}"
check "M4 the view-failed row defers with the transport note" jq -e '[.groups[]][0] as $r | ($r.merged | not) and $r.deferred and ($r.reason | test("carrier gate view failed")) and ($r.reason | test("HTTP 500")) and $r.carrier == 201' "${FXE}/merged.json" >/dev/null
check_not "M4 no merge call ran after a failed gate view" grep -q 'pr merge' "${FIXE}/call.log"

# ═════════════════════════════════════════════════════════════════════════════
# 8. Merge step: a gated merge that still fails keeps evidence, exits non-zero
# ═════════════════════════════════════════════════════════════════════════════
FXF="${WORK_ROOT}/merge4"
FIXF="${WORK_ROOT}/fix-merge4"
mkdir -p "${FXF}" "${FIXF}"
classification_v1 "${FXF}/classification.json" testorg sw-lane \
    "$(groups_json "$(grp g301 alpha github.com/mfail/mod 1.1.0 vuln_clear '' 301 main)")"
printf '%s' "$(gate_json APPROVED MERGEABLE CLEAN '[{"state": "SUCCESS"}]')" >"${FIXF}/view.301.json"
printf 'gh: HTTP 500 (merge transport after gate)' >"${FIXF}/merge.301.err"
printf '%s' '1' >"${FIXF}/merge.301.rc"
render_merge testorg "${FXF}/classification.json" "${WORK_ROOT}/merge4.sh"
MF_RC=0
run_lane "${WORK_ROOT}/merge4.sh" "${FIXF}" "${WORK_ROOT}/m4.out" "${WORK_ROOT}/m4.err" || MF_RC=$?
expect_count "M5 merge-failure plan exits 1" 1 "${MF_RC}"
check "M5 stdout stays empty on the typed failure" test ! -s "${WORK_ROOT}/m4.out"
check "M5 the typed failure names the kept evidence artifact" grep -q 'merge_failed: gated merge failed for 1 carrier(s)' "${WORK_ROOT}/m4.err"
check "M5 the failure line carries the artifact path" grep -q 'evidence artifact kept: ' "${WORK_ROOT}/m4.err"
check "M5 the artifact still lands and stays self-consistent" jq -e '.summary == {carriers_total: 1, merged: 0, deferred: 0, merge_failures: 1, closes_done: 0}' "${FXF}/merged.json" >/dev/null
check "M5 the failed merge row is a failure, not a deferral" jq -e '[.groups[]][0] as $r | ($r.merged | not) and ($r.deferred | not) and ($r.reason | test("gated merge command failed"))' "${FXF}/merged.json" >/dev/null
check_not "M5 no close-out ran (the carrier never merged)" grep -q 'pr close' "${FIXF}/call.log"

# ═════════════════════════════════════════════════════════════════════════════
# 9. Merge step: close-out + comment failures are bookkeeping
# ═════════════════════════════════════════════════════════════════════════════
FXG="${WORK_ROOT}/merge5"
FIXG="${WORK_ROOT}/fix-merge5"
mkdir -p "${FXG}" "${FIXG}"
classification_v1 "${FXG}/classification.json" testorg sw-lane \
    "$(groups_json "$(grp g401 alpha github.com/closefail/mod 1.1.0 mechanical_bump '402, 403' 401 main)")"
printf '%s' "$(gate_json APPROVED MERGEABLE CLEAN '[{"conclusion": "SUCCESS"}]')" >"${FIXG}/view.401.json"
printf 'gh: HTTP 422 (superseded PR cannot close)' >"${FIXG}/close.402.err"
printf '%s' '1' >"${FIXG}/close.402.rc"
# The carrier evidence comment fails too: the merge still happened.
printf 'gh: HTTP 500 (comment transport)' >"${FIXG}/comment.401.err"
printf '%s' '1' >"${FIXG}/comment.401.rc"
render_merge testorg "${FXG}/classification.json" "${WORK_ROOT}/merge5.sh"
CF_RC=0
run_lane "${WORK_ROOT}/merge5.sh" "${FIXG}" "${WORK_ROOT}/m5.out" "${WORK_ROOT}/m5.err" || CF_RC=$?
expect_count "M6 close-out drift never wedges the step" 0 "${CF_RC}"
check "M6 the row records the close split and the failed comment" jq -e '[.groups[]][0] as $r | $r.merged and $r.closes == [403] and $r.closes_failed == [402] and $r.carrier_comment == 0' "${FXG}/merged.json" >/dev/null
check "M6 the summary counts the one healthy close-out" jq -e '.summary == {carriers_total: 1, merged: 1, deferred: 0, merge_failures: 0, closes_done: 1}' "${FXG}/merged.json" >/dev/null
check "M6 the failed close call is typed on stderr" grep -q 'close-out of #402 failed' "${WORK_ROOT}/m5.err"
check "M6 the failed comment call is typed on stderr" grep -q 'carrier evidence comment failed' "${WORK_ROOT}/m5.err"
check "M6 both close attempts ran even after the first failed" test "$(grep -c 'pr close' "${FIXG}/call.log")" = "2"
check "M6 merge ran through the brokenbot lane before close-out" grep -q 'pr merge 401 -R testorg/alpha --merge' "${FIXG}/call.log"
check "M6 the healthy member closed via the brokenbot lane" grep -q 'pr close 403 -R testorg/alpha' "${FIXG}/call.log"

# ═════════════════════════════════════════════════════════════════════════════
# 10. Merge step: pre-network config gates + empty plan
# ═════════════════════════════════════════════════════════════════════════════
FXH="${WORK_ROOT}/merge6"
FIXH="${WORK_ROOT}/fix-merge6"
mkdir -p "${FXH}" "${FIXH}" "${FXH}/x"
classification_v1 "${FXH}/classification.json" testorg sw-empty "$(groups_json)"
render_merge testorg "${FXH}/classification.json" "${WORK_ROOT}/merge6.sh"
M0_RC=0
run_lane "${WORK_ROOT}/merge6.sh" "${FIXH}" "${WORK_ROOT}/m6.out" "${WORK_ROOT}/m6.err" || M0_RC=$?
expect_count "M7 empty merge plan exits 0" 0 "${M0_RC}"
MERGED0="$(tr -d '[:space:]' <"${WORK_ROOT}/m6.out")"
check "M7 empty plan yields the zero artifact" jq -e '.summary == {carriers_total: 0, merged: 0, deferred: 0, merge_failures: 0, closes_done: 0} and .groups == []' "${MERGED0}" >/dev/null
check "M7 empty plan makes no network calls" test ! -s "${FIXH}/call.log"

render_merge testorg "${FXH}/class.notthere.json" "${WORK_ROOT}/merge7.sh"
M5A_RC=0
run_lane "${WORK_ROOT}/merge7.sh" "${FIXH}" "${WORK_ROOT}/m7.out" "${WORK_ROOT}/m7.err" || M5A_RC=$?
expect_count "M8a missing classification exits 1" 1 "${M5A_RC}"
check "M8a typed fail names the artifact" grep -q 'merge_failed: classification artifact not found' "${WORK_ROOT}/m7.err"

printf '{"schema": "devops_maint_v1.classification.v1"}' >"${FXH}/thin.json"
render_merge testorg "${FXH}/thin.json" "${WORK_ROOT}/merge8.sh"
M5B_RC=0
run_lane "${WORK_ROOT}/merge8.sh" "${FIXH}" "${WORK_ROOT}/m8.out" "${WORK_ROOT}/m8.err" || M5B_RC=$?
expect_count "M8b classification missing a valid run/summary block exits 1" 1 "${M5B_RC}"
check "M8b typed fail names the run/summary block" grep -q 'merge_failed: classification artifact is missing a valid run/summary block' "${WORK_ROOT}/m8.err"

render_merge badorg_ "${FXH}/classification.json" "${WORK_ROOT}/merge9.sh"
M5C_RC=0
run_lane "${WORK_ROOT}/merge9.sh" "${FIXH}" "${WORK_ROOT}/m9.out" "${WORK_ROOT}/m9.err" || M5C_RC=$?
expect_count "M8c non-conformant org exits 1" 1 "${M5C_RC}"
check "M8c typed fail names the org contract" grep -q 'merge_failed: org must match' "${WORK_ROOT}/m9.err"

render_merge testorg "${FXH}/x/../classification.json" "${WORK_ROOT}/merge10.sh"
M5D_RC=0
run_lane "${WORK_ROOT}/merge10.sh" "${FIXH}" "${WORK_ROOT}/m10.out" "${WORK_ROOT}/m10.err" || M5D_RC=$?
expect_count "M8d '..' path exits 1" 1 "${M5D_RC}"
check "M8d typed fail names the traversal guard" grep -q "merge_failed: classification path must not contain '..'" "${WORK_ROOT}/m10.err"

check "M8 every pre-network gate failure left the gh call log untouched" test ! -s "${FIXH}/call.log"

# ═════════════════════════════════════════════════════════════════════════════
# 11. Secrets hygiene: the token canary never leaks into artifacts or stderr
# ═════════════════════════════════════════════════════════════════════════════
LEAK_FILES=(
    "${FXA}/acts.json"
    "${FXB}/acts.json"
    "${FXM}/merged.json"
    "${FXF}/merged.json"
    "${FXG}/merged.json"
    "${ERRA}"
    "${ERRM}"
    "${WORK_ROOT}/m4.err"
    "${WORK_ROOT}/m5.err"
)
LEAK_OK=0
for f in ${LEAK_FILES[@]}; do
    if [ -e "${f}" ] && grep -q 'supersecret-token-canary' "${f}"; then
        LEAK_OK=1
    fi
done
check "T1 the token canary never lands in artifacts or stderr" test "${LEAK_OK}" = "0"
check "T2 the act gh stub saw invocations (token routing proven, not assumed)" test -s "${FIXA}/call.log" >/dev/null
check "T2b the merge gh stub saw invocations too" test -s "${FIXM}/call.log" >/dev/null

# ═════════════════════════════════════════════════════════════════════════════
# 12. The tree still validates (grammar + interface contract)
# ═════════════════════════════════════════════════════════════════════════════
NODE_OK=1
set +e
(cd "${TREE_DIR}" && node scripts/interface_validate.mjs) >/dev/null 2>&1
NODE_OK=$?
CRIT_OK=1
(cd "${TREE_DIR}" && /usr/local/bin/criteria validate devops_maint_v1) >"${WORK_ROOT}/validate.out" 2>&1
CRIT_OK=$?
set -e
expect_count "interface contract validates (all trees)" 0 "${NODE_OK}"
expect_count "criteria accepts the devops_maint_v1 tree" 0 "${CRIT_OK}"
if grep -q '^devops_maint_v1: ok' "${WORK_ROOT}/validate.out" 2>/dev/null; then
    echo "PASS: criteria reports devops_maint_v1 ok" >&2
else
    echo "FAIL: criteria reports devops_maint_v1 ok" >&2
    FAILURES=$((FAILURES + 1))
fi

if [ "${FAILURES}" -ne 0 ]; then
    echo "FAILED: ${FAILURES} assertion(s)" >&2
    exit 1
fi
echo "all assertions passed" >&2
