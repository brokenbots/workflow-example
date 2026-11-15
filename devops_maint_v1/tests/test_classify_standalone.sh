#!/usr/bin/env bash
# devops_maint_v1 — classify-step regression suite (KB-407, ADR-0015 wave 2).
#
# Exercises the rendered classify template END-TO-END (no network, no gh —
# the classifier is pure jq over the inventory artifact):
#   - the full gate ladder: parse failure (grouped/retitled titles), opaque
#     version tokens, mixed int/dotted kinds, major bump, base drift
#     (BEHIND/DIRTY on confirmably green-or-none CI), red CI, pending/error
#     CI — and the clean base_ref/merge_state/CI fall-through that ends in
#     mechanical_bump
#   - title grammar (KB-413): conventional dependabot commit prefixes
#     (build/chore/fix(deps[-dev]):) stripped before capturing, "bump"
#     matched either case, grouped titles parked under the prefix too
#   - vuln_clear: advisory findings in the SAME repo path-matching the module
#     (exact or "/"-suffix), any version, on a version-only same-major bump
#   - report_hold: rows from a partially-collected repo (collected=false)
#     with the typed repo reason, stripped module fields, report lane entry
#   - consolidation: same-target bumps join ONE group keyed
#     (repo, module, to_version); carrier = non-rebase first, freshest
#     updated_at, higher number; members superseded into closes[]
#   - mixed base_refs park report-only ("consolidation blocked") and never
#     join groups
#   - summary block matches rows/groups/rebases/closes/report counts; the
#     structural self-check passes honest fixtures and the artifact carries
#     the inventory's sweep id/org/repos
#   - determinism: identical inputs classify byte-identically modulo the
#     started/finished timestamps
#   - fail-loud config validation: missing artifact, wrong schema, '..'-free
#     path enforcement — before anything is written
#   - hygiene: stdout carries ONLY the classification path; no temp files
#     survive in the run dir; stderr logs the evidence lane ("classify: ...")
#
# The tree still validates (criteria grammar + interface contract), matching
# the collect suite's structural gate.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
TREE_DIR="$(cd "${ROOT_DIR}/.." && pwd)"
TEMPLATE="${ROOT_DIR}/scripts/classify_prs.sh.tftpl"

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
    local label="$1" want="$2" got="$3"
    if [ "${got}" = "${want}" ]; then
        echo "PASS: ${label}" >&2
    else
        echo "FAIL: ${label} (want ${want}, got ${got})" >&2
        FAILURES=$((FAILURES + 1))
    fi
}

# ── Renderer ─────────────────────────────────────────────────────────────────
# The classify template consumes exactly one marker; test values must not
# contain single quotes and no marker may survive the render.
assert_no_single_quote() {
    local value="$1"
    case "${value}" in *"'"*) echo "BUG: test value contains a single quote: ${value}" >&2; exit 99 ;; esac
}

render_classify() { # <inventory_path> <out>
    local inventory_path="$1" out="$2"
    assert_no_single_quote "${inventory_path}"
    local inv_rep="'${inventory_path}'"
    local rendered
    rendered="$(cat "${TEMPLATE}")"
    rendered="${rendered//'{{ .inventory_file | shellquote }}'/"${inv_rep}"}"
    printf '%s' "${rendered}" >"${out}"
    if grep -q '{{' "${out}"; then
        echo "BUG: rendered classify still contains template markers" >&2
        exit 99
    fi
    chmod +x "${out}"
}

# ── Fixture builders (devops_maint_v1.inventory.v1 shapes) ───────────────────
dep_row() { # dep_row <number> <title> <updated_at> <merge_state> <ci_verdict> <base_ref>
    jq -cn --argjson num "$1" --arg title "$2" --arg up "$3" --arg ms "$4" --arg ci "$5" --arg base "$6" \
        '{number: $num, title: $title, author: "dependabot[bot]",
          head_ref: ("dep-" + ($num | tostring)), base_ref: $base, url: null,
          updated_at: $up, merge_state: (if $ms == "" then null else $ms end),
          ci: {verdict: $ci, check_count: 0, checks: [], error: null}}'
}

repo_rec() { # repo_rec <name> <collected> <reason> <osv_findings_json> <gov_findings_json> <dep_rows_json>
    jq -cn --arg repo "$1" --argjson collected "$2" --arg reason "$3" \
        --argjson osv "$4" --argjson gov "$5" --argjson deps "$6" \
        '{repo: $repo, collected: $collected, collected_reason: $reason,
          prs: {prs_skipped: false, open_total: ($deps | length),
                dependabot_total: ($deps | length), other_open_total: 0,
                pr_checks_truncated: false, pr_list_truncated: false,
                dependabot: $deps},
          advisories: {govulncheck: {status: "ok", reason: "", findings: $gov},
                       osv: {status: "ok", reason: "", findings: $osv}},
          codeql: {status: "ok", reason: "", open_alerts: 0, by_severity: {}},
          rulesets: {status: "ok", reason: "", applied: true, rules: []},
          errors: []}'
}

dep_rows() { # dep_rows <num> <title> <up> <ms> <ci> <base> [<num> <title> ...]
    local rows=""
    while [ "$#" -ge 6 ]; do
        rows="${rows}${rows:+
,}$(dep_row "$1" "$2" "$3" "$4" "$5" "$6")"
        shift 6
    done
    printf '[%s]' "${rows}"
}

make_inventory() { # make_inventory <org> <repos_json> <out>
    jq -cn --arg org "$1" --argjson recs "$2" \
        '{schema: "devops_maint_v1.inventory.v1",
          sweep: {sweep_id: "sw-classify", started_at: "2026-11-15T04:00:00Z",
                  finished_at: "2026-11-15T04:05:00Z", org: $org,
                  repos: ([$recs[].repo]), repos_total: ($recs | length),
                  repos_collected: ([$recs[] | select(.collected)] | length),
                  repos_failed: ([$recs[] | select(.collected | not)] | length),
                  scan_govulncheck: true, scan_osv: true},
          repos: $recs,
          summary: {rows_total: ([$recs[].prs.dependabot[]] | length),
                    repos_total: ($recs | length),
                    repos_collected: ([$recs[] | select(.collected)] | length),
                    repos_failed: ([$recs[] | select(.collected | not)] | length)}}' >"$3"
}

# run_classify <rendered.sh> <inventory_path> <out> <err>
run_classify() {
    local script="$1" out="$2" err="$3"
    local rc=0
    set +e
    "${script}" >"${out}" 2>"${err}"
    rc=$?
    set -e
    return "${rc}"
}

# ═════════════════════════════════════════════════════════════════════════════
# 1. Structural wiring
# ═════════════════════════════════════════════════════════════════════════════
check "main.chcl renders classify through templatefile" grep -q 'templatefile("./scripts/classify_prs.sh.tftpl"' "${ROOT_DIR}/main.chcl"
check "main.chcl feeds the collect artifact into classify" grep -q 'inventory_file *= *data\.internal\.inventory_file\.value' "${ROOT_DIR}/main.chcl"
check "template consumes the inventory marker exactly once" test "$(grep -c '{{ .inventory_file | shellquote }}' "${TEMPLATE}")" = "1"
check "template self-checks the artifact structurally" grep -q 'classification artifact failed its structural self-check' "${TEMPLATE}"

# ═════════════════════════════════════════════════════════════════════════════
# 2. Happy path: every bucket, consolidation + carrier/supersede, report lane
# ═════════════════════════════════════════════════════════════════════════════
FX1="${WORK_ROOT}/fx1"
RUN1="${FX1}"
mkdir -p "${RUN1}"
OSV_LEFTPAD='[{"id": "GHSA-a1b2", "package": "npm/left-pad", "version": "1.3.0"}]'
RECS_ALPHA="$(repo_rec alpha true "" "${OSV_LEFTPAD}" '[]' "$(dep_rows \
    21 "Bump actions/checkout from 4.1.2 to 4.2.0" "2026-11-14T09:00:00Z" "CLEAN" "green" "main" \
    22 "Bump npm/left-pad from 1.3.0 to 1.3.1" "2026-11-14T09:05:00Z" "CLEAN" "green" "main" \
    23 "Bump golang.org/x/net from 0.20.0 to 0.21.0" "2026-11-14T08:00:00Z" "BEHIND" "green" "main" \
    24 "Bump github.com/foo/bar from 2.3.1 to 3.0.0" "2026-11-14T08:10:00Z" "CLEAN" "green" "main" \
    25 "Bump actions/setup-node from abc to def" "2026-11-14T08:20:00Z" "CLEAN" "green" "main" \
    26 "Bump the go_modules group across 2 directories with 2 updates" "2026-11-14T08:30:00Z" "CLEAN" "green" "main" \
    27 "Bump github.com/red/mod from 1.0.0 to 1.1.0" "2026-11-14T08:40:00Z" "CLEAN" "red" "main" \
    28 "Bump github.com/pend/mod from 1.0.0 to 1.0.5" "2026-11-14T08:50:00Z" "CLEAN" "pending" "main" \
    31 "Bump github.com/consul/mod from 1.2.0 to 1.2.5" "2026-11-01T00:00:00Z" "CLEAN" "green" "main" \
    32 "Bump github.com/consul/mod from 1.2.0 to 1.2.5" "2026-11-02T00:00:00Z" "CLEAN" "green" "main")")"
RECS_DELTA="$(repo_rec delta false "partial (see errors)" '[]' '[]' "$(dep_rows \
    41 "Bump actions/cache from 4 to 4.2" "2026-11-14T07:00:00Z" "CLEAN" "green" "main")")"
make_inventory testorg "[${RECS_ALPHA},
${RECS_DELTA}]" "${FX1}/inventory.json"

render_classify "${FX1}/inventory.json" "${WORK_ROOT}/classify1.sh"
OUT1="${WORK_ROOT}/s1.out"
ERR1="${WORK_ROOT}/s1.err"
CLS_RC1=0
run_classify "${WORK_ROOT}/classify1.sh" "${OUT1}" "${ERR1}" || CLS_RC1=$?
expect_count "S1 classify exits 0 on the mixed inventory" 0 "${CLS_RC1}"
CLS1="$(cat "${OUT1}")"
check "S1 stdout carries one line, the classification path" test "$(wc -l <"${OUT1}")" = "1"
check "S1 stdout carries exactly the classification path" test "${CLS1}" = "${FX1}/classification.json"
check "S1 classification artifact is a sibling of the inventory" jq -e '.schema == "devops_maint_v1.classification.v1"' "${CLS1}" >/dev/null

check "S1 run block mirrors the inventory sweep identity" jq -e '.run.sweep_id == "sw-classify" and .run.org == "testorg" and .run.repos == ["alpha", "delta"]' "${CLS1}" >/dev/null
check "S1 summary counts every bucket" jq -e '.summary == {rows_total: 11, mechanical_bump: 3, vuln_clear: 1, needs_rebase: 1, needs_judgment: 5, report_hold: 1, report_only: 1, parked_groups: 0, groups_total: 4, carriers_total: 4, closes_total: 1, rebases_total: 1}' "${CLS1}" >/dev/null

# Gate ladder — first-fired gate wins, every triggered gate recorded.
check "S1 same-major bump -> mechanical_bump" jq -e '.rows[] | select(.number == 21) | .class == "mechanical_bump" and (.reasons | length == 0) and .is_carrier' "${CLS1}" >/dev/null
check "S1 path-matched advisory -> vuln_clear with the finding id" jq -e '.rows[] | select(.number == 22) | .class == "vuln_clear" and .advisory_ids == ["GHSA-a1b2"]' "${CLS1}" >/dev/null
check "S1 drifted base on green CI -> needs_rebase with BEHIND recorded" jq -e '.rows[] | select(.number == 23) | .class == "needs_rebase" and .merge_state == "BEHIND" and (.reasons[0] | test("base drifted"))' "${CLS1}" >/dev/null
check "S1 semver major bump -> needs_judgment" jq -e '.rows[] | select(.number == 24) | .class == "needs_judgment" and (.reasons[0] | test("semver major bump"))' "${CLS1}" >/dev/null
check "S1 opaque version tokens -> needs_judgment" jq -e '.rows[] | select(.number == 25) | .class == "needs_judgment" and (.reasons[0] | test("not comparable"))' "${CLS1}" >/dev/null
check "S1 grouped/retitled title -> needs_judgment" jq -e '.rows[] | select(.number == 26) | .class == "needs_judgment" and (.reasons[0] | test("does not parse")) and .module == null' "${CLS1}" >/dev/null
check "S1 red CI on clean base -> needs_judgment" jq -e '.rows[] | select(.number == 27) | .class == "needs_judgment" and (.reasons[0] | test("CI verdict red"))' "${CLS1}" >/dev/null
check "S1 pending CI -> needs_judgment" jq -e '.rows[] | select(.number == 28) | .class == "needs_judgment" and (.reasons[0] | test("pending"))' "${CLS1}" >/dev/null
check "S1 judgment rows never join groups" jq -e '[.rows[] | select(.class == "needs_judgment") | select(.group_id != null)] | length == 0' "${CLS1}" >/dev/null

# Consolidation: same-target pair -> ONE group g002; carrier = freshest.
check "S1 consul pair forms group g002 with carrier 32" jq -e '.groups[] | select(.group_id == "g002") | .repo == "alpha" and .module == "github.com/consul/mod" and .to_version == "1.2.5" and .carrier.number == 32 and .members == [31, 32] and .closes == [31] and .action_class == "mechanical_bump"' "${CLS1}" >/dev/null
check "S1 superseded member 31 keyed to the group, not the carrier" jq -e '.rows[] | select(.number == 31) | .class == "mechanical_bump" and .group_id == "g002" and (.is_carrier | not)' "${CLS1}" >/dev/null
check "S1 groups are id-ordered over sorted (repo, module, to_version)" jq -e '([.groups[].group_id] | sort) == [.groups[].group_id] and .groups[0].group_id == "g001" and .groups[3].group_id == "g004"' "${CLS1}" >/dev/null
check "S1 rebase plan carries the drifted carrier with its base" jq -e '.rebases == [{repo: "alpha", number: 23, group_id: "g003", module: "golang.org/x/net", to_version: "0.21.0", merge_state: "BEHIND", base_ref: "main"}]' "${CLS1}" >/dev/null
check "S1 rebase carrier group carries needs_rebase action" jq -e '.groups[] | select(.group_id == "g003") | .action_class == "needs_rebase" and .carrier.number == 23' "${CLS1}" >/dev/null
check "S1 exactly one carrier per non-parked group" jq -e '[.rows[] | select(.is_carrier)] | length == 4' "${CLS1}" >/dev/null
check "S1 carriers are 21, 22, 23 and 32" jq -e '[.rows[] | select(.is_carrier) | .number] | sort == [21, 22, 23, 32]' "${CLS1}" >/dev/null

# Report-only lane: the partial repo's row parks before any grouping.
check "S1 partial-repo row parks with the typed repo reason" jq -e '.rows[] | select(.number == 41) | .class == "report_hold" and .report_only and .module == null and .from_version == null and .to_version == null and .advisory_ids == [] and (.report_reason | test("repo inventory partially collected"))' "${CLS1}" >/dev/null
check "S1 report lane mirrors parked rows exactly" jq -e '.report == [{repo: "delta", number: 41, title: "Bump actions/cache from 4 to 4.2", class: "report_hold", reason: "repo inventory partially collected: partial (see errors)"}]' "${CLS1}" >/dev/null

# ═════════════════════════════════════════════════════════════════════════════
# 2p. Title grammar (KB-413): conventional dependabot prefixes + bump case
# ═════════════════════════════════════════════════════════════════════════════
# Real dependabot titles carry a conventional-commit prefix ("build(deps):",
# "chore(deps):", "fix(deps):", optionally "-dev") and often a lowercase
# "bump". Those are THE mechanical bumps: the parse strips the prefix before
# capturing and matches either case, while grouped titles stay unparseable
# under the prefix too.
FXP="${WORK_ROOT}/fxp"
RUNP="${FXP}"
mkdir -p "${RUNP}"
make_inventory testorg "[$(repo_rec prefixed true "" '[]' '[]' "$(dep_rows \
    61 "build(deps): Bump github.com/brokenbots/criteria-go-adapter-sdk from 0.5.2 to 0.5.3" "2026-11-14T09:00:00Z" "CLEAN" "green" "main" \
    62 "chore(deps): bump npm/case-var from 1.2.0 to 1.2.1" "2026-11-14T09:10:00Z" "CLEAN" "green" "main" \
    63 "fix(deps): Bump registry.example/pkg from 0.9.0 to 0.9.1" "2026-11-14T09:20:00Z" "CLEAN" "green" "main" \
    64 "build(deps-dev): Bump registry.example/dev from 2.0.0 to 2.0.1" "2026-11-14T09:30:00Z" "CLEAN" "green" "main" \
    65 "build(deps): Bump the go_modules group across 3 directories with 3 updates" "2026-11-14T09:40:00Z" "CLEAN" "green" "main")")]" "${FXP}/inventory.json"

render_classify "${FXP}/inventory.json" "${WORK_ROOT}/classify_prefixed.sh"
CLS_RCP=0
run_classify "${WORK_ROOT}/classify_prefixed.sh" "${WORK_ROOT}/s1p.out" "${WORK_ROOT}/s1p.err" || CLS_RCP=$?
expect_count "S1p classify exits 0 on prefixed/case titles" 0 "${CLS_RCP}"
CLS_P="$(cat "${WORK_ROOT}/s1p.out")"
check "S1p prefixed/case titles land in the mechanical buckets" jq -e '.summary == {rows_total: 5, mechanical_bump: 4, vuln_clear: 0, needs_rebase: 0, needs_judgment: 1, report_hold: 0, report_only: 0, parked_groups: 0, groups_total: 4, carriers_total: 4, closes_total: 0, rebases_total: 0}' "${CLS_P}" >/dev/null
check "S1p canonical build(deps) title parses and acts mechanically" jq -e '.rows[] | select(.number == 61) | .class == "mechanical_bump" and .module == "github.com/brokenbots/criteria-go-adapter-sdk" and .from_version == "0.5.2" and .to_version == "0.5.3" and (.reasons | length == 0) and .is_carrier' "${CLS_P}" >/dev/null
check "S1p chore(deps) + lowercase bump parses and acts mechanically" jq -e '.rows[] | select(.number == 62) | .class == "mechanical_bump" and .module == "npm/case-var" and .from_version == "1.2.0" and .to_version == "1.2.1"' "${CLS_P}" >/dev/null
check "S1p fix(deps) prefix parses and acts mechanically" jq -e '.rows[] | select(.number == 63) | .class == "mechanical_bump" and .module == "registry.example/pkg"' "${CLS_P}" >/dev/null
check "S1p build(deps-dev) prefix parses and acts mechanically" jq -e '.rows[] | select(.number == 64) | .class == "mechanical_bump" and .module == "registry.example/dev"' "${CLS_P}" >/dev/null
check "S1p grouped title stays needs_judgment under the prefix" jq -e '.rows[] | select(.number == 65) | .class == "needs_judgment" and (.reasons[0] | test("does not parse")) and .module == null' "${CLS_P}" >/dev/null
check "S1p prefixed rows group and carry like bare bumps" jq -e '[.groups[].module] | sort == (["github.com/brokenbots/criteria-go-adapter-sdk", "npm/case-var", "registry.example/dev", "registry.example/pkg"] | sort)' "${CLS_P}" >/dev/null

# ═════════════════════════════════════════════════════════════════════════════
# 3. Consolidation parked: mixed base_refs stay report-only
# ═════════════════════════════════════════════════════════════════════════════
FX2="${WORK_ROOT}/fx2"
RUN2="${FX2}"
mkdir -p "${RUN2}"
make_inventory testorg "[$(repo_rec alpha true "" '[]' '[]' "$(dep_rows \
    51 "Bump github.com/park/mod from 1.0.0 to 1.2.0" "2026-11-14T09:00:00Z" "CLEAN" "green" "main" \
    52 "Bump github.com/park/mod from 1.0.0 to 1.2.0" "2026-11-13T09:00:00Z" "CLEAN" "green" "release-1.0" \
    53 "Bump github.com/zzz/mod from 9.0.0 to 9.0.5" "2026-11-14T09:30:00Z" "CLEAN" "green" "main")")]" "${FX2}/inventory.json"

render_classify "${FX2}/inventory.json" "${WORK_ROOT}/classify2.sh"
CLS_RC2=0
run_classify "${WORK_ROOT}/classify2.sh" "${WORK_ROOT}/s2.out" "${WORK_ROOT}/s2.err" || CLS_RC2=$?
expect_count "S2 classify exits 0 with a parked group" 0 "${CLS_RC2}"
CLS2="$(tr -d '[:space:]' <"${WORK_ROOT}/s2.out")"
# Parked groups keep their place in the deterministic id order, so the active
# group after them takes g002 (an id gap where a later sort key parked).
check "S2 mixed base refs park the group report-only" jq -e '.summary == {rows_total: 3, mechanical_bump: 1, vuln_clear: 0, needs_rebase: 0, needs_judgment: 0, report_hold: 2, report_only: 2, parked_groups: 1, groups_total: 1, carriers_total: 1, closes_total: 0, rebases_total: 0}' "${CLS2}" >/dev/null
check "S2 parked members keep class report_hold with the blocker reason" jq -e '[.rows[] | select((.report_reason // "") | test("consolidation blocked: mixed base refs \\(main, release-1.0\\)"))] | length == 2' "${CLS2}" >/dev/null
check "S2 parked rows never carry group ids" jq -e '[.rows[] | select(.class == "report_hold") | select(.group_id != null)] | length == 0' "${CLS2}" >/dev/null
check "S2 both parked rows land in report_hold" jq -e '[.rows[] | select(.class == "report_hold")] | length == 2' "${CLS2}" >/dev/null
check "S2 the active group takes the id after the parked group" jq -e '.groups[0].group_id == "g002" and .groups[0].module == "github.com/zzz/mod"' "${CLS2}" >/dev/null
check "S2 the zzz bump groups and carries" jq -e '.rows[] | select(.number == 53) | .class == "mechanical_bump" and .group_id == "g002" and .is_carrier' "${CLS2}" >/dev/null
check "S2 parked group surfaces nothing in groups/rebases" jq -e '(.rebases == []) and (.report | length == 2) and ([.groups[].group_id] == ["g002"])' "${CLS2}" >/dev/null

# ═════════════════════════════════════════════════════════════════════════════
# 4. Empty inventory classifies to an empty-bucket artifact
# ═════════════════════════════════════════════════════════════════════════════
FX3="${WORK_ROOT}/fx3"
RUN3="${FX3}"
mkdir -p "${RUN3}"
make_inventory testorg "[$(repo_rec empty true "" '[]' '[]' "$(dep_rows)")]" "${FX3}/inventory.json"
render_classify "${FX3}/inventory.json" "${WORK_ROOT}/classify3.sh"
CLS_RC3=0
run_classify "${WORK_ROOT}/classify3.sh" "${WORK_ROOT}/s3.out" "${WORK_ROOT}/s3.err" || CLS_RC3=$?
expect_count "S3 classify exits 0 on an empty inventory" 0 "${CLS_RC3}"
CLS3="$(tr -d '[:space:]' <"${WORK_ROOT}/s3.out")"
check "S3 empty inventory yields the zero artifact" jq -e '.rows == [] and .groups == [] and .rebases == [] and .report == [] and .summary.rows_total == 0 and .summary.groups_total == 0' "${CLS3}" >/dev/null

# ═════════════════════════════════════════════════════════════════════════════
# 5. Determinism: byte-identical artifacts modulo the two timestamps
# ═════════════════════════════════════════════════════════════════════════════
RUN4="${FX1}/runs/000002"
mkdir -p "${RUN4}"
cp "${FX1}/inventory.json" "${RUN4}/inventory.json"
render_classify "${RUN4}/inventory.json" "${WORK_ROOT}/classify4.sh"
run_classify "${WORK_ROOT}/classify4.sh" "${WORK_ROOT}/s4.out" "${WORK_ROOT}/s4.err"
CLS4="$(tr -d '[:space:]' <"${WORK_ROOT}/s4.out")"
check "S4 determinism: only run timestamps and the input path differ" \
    jq -n -e --slurpfile a "${FX1}/classification.json" --slurpfile b "${RUN4}/classification.json" \
    '($a[0] | del(.run.started_at, .run.finished_at, .run.inventory_path)) == ($b[0] | del(.run.started_at, .run.finished_at, .run.inventory_path))' >/dev/null
check "S4 the two runs name their own inventories" jq -e -n --slurpfile a "${FX1}/classification.json" --slurpfile b "${RUN4}/classification.json" '$a[0].run.inventory_path != $b[0].run.inventory_path' >/dev/null
check "S4 timestamps still populated" jq -e '.run.started_at | length > 0' "${CLS4}" >/dev/null

# ═════════════════════════════════════════════════════════════════════════════
# 6. Fail-loud config validation, before anything is written
# ═════════════════════════════════════════════════════════════════════════════
# 6a. missing artifact
FXMISSING="${WORK_ROOT}/missing"
mkdir -p "${FXMISSING}/runs/000001"
render_classify "${FXMISSING}/runs/000001/notthere.json" "${WORK_ROOT}/classify_missing.sh"
CLS_RC5A=0
run_classify "${WORK_ROOT}/classify_missing.sh" "${WORK_ROOT}/s5a.out" "${WORK_ROOT}/s5a.err" || CLS_RC5A=$?
expect_count "S5a missing artifact exits 1" 1 "${CLS_RC5A}"
check "S5a typed fail on stderr" grep -q 'classify_failed: inventory artifact not found' "${WORK_ROOT}/s5a.err"
check "S5a nothing written (stdout empty)" test ! -s "${WORK_ROOT}/s5a.out"

# 6b. wrong schema
FXBAD="${WORK_ROOT}/badschema"
mkdir -p "${FXBAD}"
printf '{"schema": "other.tree.v1", "repos": []}' >"${FXBAD}/inv.json"
render_classify "${FXBAD}/inv.json" "${WORK_ROOT}/classify_bad.sh"
CLS_RC5B=0
run_classify "${WORK_ROOT}/classify_bad.sh" "${WORK_ROOT}/s5b.out" "${WORK_ROOT}/s5b.err" || CLS_RC5B=$?
expect_count "S5b wrong-schema artifact exits 1" 1 "${CLS_RC5B}"
check "S5b typed fail names the artifact contract" grep -q 'classify_failed: input is not a devops_maint_v1.inventory.v1 artifact' "${WORK_ROOT}/s5b.err"
check "S5b no classification emitted next to the bad input" test ! -e "${FXBAD}/classification.json"

# 6c. '..' traversal in the path is refused
FXDOT="${WORK_ROOT}/dots"
mkdir -p "${FXDOT}/x"
printf '{"schema": "devops_maint_v1.inventory.v1", "repos": []}' >"${FXDOT}/inv.json"
render_classify "${FXDOT}/x/../inv.json" "${WORK_ROOT}/classify_dots.sh"
CLS_RC5C=0
run_classify "${WORK_ROOT}/classify_dots.sh" "${WORK_ROOT}/s5c.out" "${WORK_ROOT}/s5c.err" || CLS_RC5C=$?
expect_count "S5c '..' path exits 1" 1 "${CLS_RC5C}"
check "S5c typed fail names the traversal guard" grep -q "classify_failed: inventory path must not contain '..'" "${WORK_ROOT}/s5c.err"

# 6d. unwritable run dir (non-root only)
if [ "$(id -u)" != "0" ]; then
    FXRO="${WORK_ROOT}/readolny"
    mkdir -p "${FXRO}"
    printf '{"schema": "devops_maint_v1.inventory.v1", "repos": []}' >"${FXRO}/inv.json"
    chmod 555 "${FXRO}"
    render_classify "${FXRO}/inv.json" "${WORK_ROOT}/classify_ro.sh"
    CLS_RC5D=0
    run_classify "${WORK_ROOT}/classify_ro.sh" "${WORK_ROOT}/s5d.out" "${WORK_ROOT}/s5d.err" || CLS_RC5D=$?
    chmod 755 "${FXRO}"
    expect_count "S5d unwritable run dir exits 1" 1 "${CLS_RC5D}"
    check "S5d typed fail names the run directory" grep -q 'classify_failed: run directory not writable' "${WORK_ROOT}/s5d.err"
fi

# ═════════════════════════════════════════════════════════════════════════════
# 7. Hygiene: stderr evidence lane, stdout protocol purity, no temp residue
# ═════════════════════════════════════════════════════════════════════════════
check "S7 stderr logs the inventory input line" grep -q 'classify: input inventory' "${ERR1}"
check "S7 stderr logs the deterministic bucket counts" grep -q 'classify: buckets: mechanical=3 vuln=1 rebase=1 judgment=5 report_only=1' "${ERR1}"
check_not "S7 success run never prints the typed failure prefix" grep -q 'classify_failed' "${ERR1}"
check "S7 no temp residue next to the mixed inventory (S1)" test "$(ls -A "${RUN1}" | grep -v '^runs$' | sort | tr '\n' ' ')" = "classification.json inventory.json "
check "S7 no temp residue next to the parked inventory (S2)" test "$(ls -A "${RUN2}" | sort | tr '\n' ' ')" = "classification.json inventory.json "
check "S7 no temp residue for the empty-inventory run (S3)" test "$(ls -A "${RUN3}" | sort | tr '\n' ' ')" = "classification.json inventory.json "

# ═════════════════════════════════════════════════════════════════════════════
# 8. Regression: duplicate PR numbers across repos must not trip the self-check
# ═════════════════════════════════════════════════════════════════════════════
# PR numbers are per-repo; two repos can both carry a PR #5. The artifact
# self-check must key uniqueness on (repo, number), not on number alone.
FXDUP="${WORK_ROOT}/dupnum"
RUNdup="${FXDUP}"
mkdir -p "${RUNdup}"
RECS_DUP_ALPHA="$(repo_rec alpha true "" '[]' '[]' "$(dep_rows \
    5 "Bump github.com/one/mod from 1.0.0 to 1.0.1" "2026-11-14T09:00:00Z" "CLEAN" "green" "main")")"
RECS_DUP_BETA="$(repo_rec beta true "" '[]' '[]' "$(dep_rows \
    5 "Bump github.com/two/mod from 1.0.0 to 1.0.1" "2026-11-14T09:05:00Z" "CLEAN" "green" "main")")"
make_inventory testorg "[${RECS_DUP_ALPHA},
${RECS_DUP_BETA}]" "${FXDUP}/inventory.json"
render_classify "${FXDUP}/inventory.json" "${WORK_ROOT}/classify_dup.sh"
OUTdup="${WORK_ROOT}/dup.out"
ERRdup="${WORK_ROOT}/dup.err"
CLS_DUP_RC=0
run_classify "${WORK_ROOT}/classify_dup.sh" "${OUTdup}" "${ERRdup}" || CLS_DUP_RC=$?
expect_count "S8 duplicate number across repos classifies fine" 0 "${CLS_DUP_RC}"
CLSDUP="$(cat "${OUTdup}")"
check "S8 both duplicate-number rows are classified and carried" jq -e '[.rows[] | select(.number == 5)] | length == 2' "${CLSDUP}" >/dev/null
check "S8 alpha #5 keeps its own repo-scoped group and carrier flag" jq -e '[.rows[] | select(.repo == "alpha" and .number == 5)][0] as $r | $r.is_carrier and ($r.group_id | type == "string" and length > 0) and $r.class == "mechanical_bump"' "${CLSDUP}" >/dev/null
check "S8 beta #5 keeps its own repo-scoped group and carrier flag" jq -e '[.rows[] | select(.repo == "beta" and .number == 5)][0] as $r | $r.is_carrier and ($r.group_id | type == "string" and length > 0) and $r.class == "mechanical_bump"' "${CLSDUP}" >/dev/null
check "S8 the two rows land in distinct per-repo groups" jq -e '[.groups[]] | length == 2 and ([.[].carrier.repo] | sort == ["alpha", "beta"]) and (.[0].group_id != .[1].group_id)' "${CLSDUP}" >/dev/null
check "S8 each group carriers its own repo's PR number" jq -e '[.groups[] | select(.carrier.repo == "beta")][0].carrier.number == 5' "${CLSDUP}" >/dev/null
check "S8 per-(repo, number) uniqueness holds" jq -e '([.rows[] | [.repo, .number]] | unique | length) == (.rows | length)' "${CLSDUP}" >/dev/null
if grep -q 'classification artifact failed its structural self-check' "${ERRdup}"; then
    check "S8 self-check accepts per-repo duplicate numbers" false
else
    check "S8 self-check accepts per-repo duplicate numbers" true
fi

# ═════════════════════════════════════════════════════════════════════════════
# 9. The tree still validates (grammar + interface contract)
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
check "criteria reports devops_maint_v1 ok" grep -q 'devops_maint_v1: ok' "${WORK_ROOT}/validate.out"

if [ "${FAILURES}" -gt 0 ]; then
    echo "FAILED: ${FAILURES} assertion(s)" >&2
    exit 1
fi
check "suite leaves no scratch leak beside its fixtures" test ! -e "${SCRIPT_DIR}/testorg"
check "suite leaves no scratch leak at the tree root" test ! -e "${ROOT_DIR}/../testorg"
echo "PASS: devops_maint_v1 classify-step regression suite verified"
