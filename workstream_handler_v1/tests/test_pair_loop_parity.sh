#!/bin/sh
# test_pair_loop_parity.sh — KB-386 (Stage-3 fold, part 2 of 3): the pair-loop
# v1 <-> v2 per-step parity COMPARATOR, plus the self-test harness that proves
# the comparator's own logic.
#
# TWO MODES, ONE FILE:
#
#   test_pair_loop_parity.sh --compare <baseline.tsv> <candidate.tsv>
#       OPERATOR LANE. Compares a v1 BASELINE per-step timing record against a
#       v2 CANDIDATE per-step timing record, slot by slot, against the
#       documented thresholds (workflows/pair_programming_loop_v2/PARITY.md).
#         exit 0  every gated slot is within its threshold
#         exit 1  a gated slot REGRESSED (the report names the slot)
#         exit 2  a record/collection error (unmapped step, malformed
#                 duration, empty record, missing mandatory slot)
#
#   test_pair_loop_parity.sh
#       SELF-TEST LANE. Negative arms FIRST (the comparator must be able to
#       FAIL), then the positive arm, then the self-tests that prove the
#       comparator's own logic.
#
# THE FIXTURES ARE SELF-TEST ARMS - NOT A BASELINE:
#   tests/fixtures/pair_loop_parity/ holds SYNTHETIC records. KB-386 is
#   UN-ARMED (no k8s-run), so NO real-card timing record exists anywhere in
#   this repo. Every fixture says so in its own header. Nothing in this script
#   invents a measurement: the numbers below are the documented THRESHOLDS.
#
# THE GATE:
#   KB-386's gate is a parity run on a REAL card measured against the v1
#   baseline tree (workstream_handler_v1/workflows/pair_programming_loop,
#   RETAINED - the fold deletes only the round trees). This card does NOT run
#   it: KB-386 is UN-ARMED, so the real-card run is the operator's post-merge
#   fleet action. See workflows/pair_programming_loop_v2/PARITY.md.
#
# Rulings honoured: LOCAL-ONLY (ruling 1 - no network surface in this tree),
# POSIX sh (no bashisms, no `local`), deterministic shell truth over agent
# judgement, no fabricated evidence.

set -eu

SCRIPT_PATH="$(cd "$(dirname "$0")" && pwd)/$(basename "$0")"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
TREE_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT INT TERM

NS="plp"

# ─────────────────────────────────────────────────────────────────────────────
# The rule table - THE comparator's single source of truth. The same table is
# reproduced in PARITY.md; the self-test compares the two byte-for-byte (the
# drift tripwire: the documented spec and the enforced spec must never
# diverge silently).
#
#   slot|side|metric|mode|pct|ref|mandatory|patterns
#
#   side       v1 = the baseline record, v2 = the candidate record
#   metric     total    = the slot's summed duration
#              max_unit = the slot's single largest invocation
#   mode       delta    = candidate <= floor(baseline * (100 + pct) / 100)
#              budget   = candidate <= floor(ref_slot's v1 TOTAL * (100+pct)/100)
#              one_sided= reported only, never gated
#   pct        the documented threshold in percent (delta/budget)
#   ref        the baseline slot a budget is measured against (slot@side)
#   mandatory  yes = the record MUST exercise the slot (else exit 2)
#   patterns   space-separated step ids (shell globs) that belong to the slot
# ─────────────────────────────────────────────────────────────────────────────
RULES=$(cat <<'PARITY_RULES'
# --- BEGIN PARITY RULE TABLE ---
slot|side|metric|mode|pct|ref|mandatory|patterns
prelude_leg|v1|total|delta|25||yes|get_branch_name read_workstream checkout_branch capture_entry_sha check_working_tree
prelude_leg|v2|total|delta|25||yes|get_branch_name read_workstream checkout_branch capture_entry_sha check_working_tree
plan_leg|v2|total|budget|0|delivery_leg@v1|yes|plan_parts reduce_plan_parts
delivery_leg|v1|max_unit|delta|25||yes|develop
delivery_leg|v2|max_unit|delta|25||yes|run_part_p*_fast run_part_p*_deep
gate_leg|v1|max_unit|delta|25||yes|verify_committed ci_gate
gate_leg|v2|max_unit|delta|25||yes|p*_verify p*_refute p*_reverify
review_leg|v1|max_unit|delta|25||yes|review
review_leg|v2|max_unit|delta|25||no|pair_part_p*_remediate
checkpoint_leg|v1|total|delta|50||no|push_checkpoint preserve_pending sync_checkpoint_upstream resume_after_preservation
checkpoint_leg|v2|total|delta|50||no|push_checkpoint preserve_pending sync_checkpoint_upstream resume_after_preservation
log_leg|v1|total|delta|25||yes|write_review_log commit_review_log push_review_checkpoint
log_leg|v2|total|delta|25||yes|write_review_log commit_review_log push_review_checkpoint
wip_leg|v1|total|one_sided|||no|write_work_log commit_work_log push_wip_checkpoint
verdict_leg|v2|total|one_sided|||no|switch_verdict_route
failure_leg|v2|total|one_sided|||no|fail_no_parts_attempted fail_parts_overflow fail_verdict_mismatch fail_pair_verdict_parking
# --- END PARITY RULE TABLE ---
PARITY_RULES
)

printf '%s\n' "$RULES" > "$WORKDIR/rules.tsv"

# ─────────────────────────────────────────────────────────────────────────────
# comparator internals
# ─────────────────────────────────────────────────────────────────────────────

parity_error() {
    printf 'PARITY error=%s\n' "$*" >&2
    exit 2
}

# rule_row <slot> <side> <field> - print one field of a rule row ("" if absent)
rule_row() {
    awk -F'|' -v want_slot="$1" -v want_side="$2" -v want_field="$3" '
        /^#/ { next }
        $1 == "slot" { next }
        $1 == want_slot && $2 == want_side {
            if (want_field == "metric")    { print $3; exit }
            if (want_field == "mode")      { print $4; exit }
            if (want_field == "pct")       { print $5; exit }
            if (want_field == "ref")       { print $6; exit }
            if (want_field == "mandatory") { print $7; exit }
            if (want_field == "patterns") { print $8; exit }
        }
    ' "$WORKDIR/rules.tsv"
}

# rules_slot_ids - every slot id in the table, once, in table order
rules_slot_ids() {
    awk -F'|' '
        /^#/ { next }
        $1 == "slot" { next }
        !seen[$1]++ { print $1 }
    ' "$WORKDIR/rules.tsv"
}

# slot_of_step <side> <step> - the slot the step id belongs to ("" if unmapped)
slot_of_step() {
    awk -F'|' -v want_side="$1" -v want_step="$2" '
        function glob_to_regex(p,   out, i, c) {
            out = "^"
            n = length(p)
            for (i = 1; i <= n; i++) {
                c = substr(p, i, 1)
                if (c == "*") out = out ".*"
                else if (c == "?") out = out "."
                else if (c ~ /[.^$+(){}|\[\]\\]/) out = out "\\" c
                else out = out c
            }
            return out "$"
        }
        /^#/ { next }
        $1 == "slot" { next }
        $2 == want_side {
            n = split($8, pats, " ")
            for (i = 1; i <= n; i++) {
                if (want_step ~ glob_to_regex(pats[i])) { print $1; exit }
            }
        }
    ' "$WORKDIR/rules.tsv"
}

# ─────────────────────────────────────────────────────────────────────────────
# ingest <record> <side> - parse one record file into the per-slot accumulators
# ─────────────────────────────────────────────────────────────────────────────
ingest() {
    rec="$1"
    side="$2"
    lineno=0
    datalines=0

    while IFS= read -r line || [ -n "$line" ]; do
        lineno=$((lineno + 1))
        line="$(printf '%s' "$line" | tr -d '\r')"

        case "$line" in
            '' ) continue ;;
            '#'*) continue ;;
        esac
        case "$line" in
            *[![:space:]]*) ;;
            * ) continue ;;
        esac

        set -f
        # shellcheck disable=SC2086
        set -- $line
        set +f
        step="${1-}"
        ms="${2-}"
        extra="${3-}"

        if [ -z "$step" ] || [ -n "$extra" ]; then
            parity_error "malformed record line $lineno in the $side record: $line"
        fi
        case "$step" in
            *[!A-Za-z0-9_.-]*)
                parity_error "malformed record line $lineno in the $side record: $step"
                ;;
        esac
        case "$ms" in
            ''|*[!0-9]*)
                parity_error "malformed duration '$ms' for step '$step' ($side record line $lineno)"
                ;;
        esac
        if [ "${#ms}" -gt 15 ]; then
            parity_error "implausible duration '$ms' for step '$step' ($side record line $lineno)"
        fi

        slot="$(slot_of_step "$side" "$step")"
        if [ -z "$slot" ]; then
            parity_error "unmapped step '$step' in the $side record (line $lineno) - the parity map is stale"
        fi

        eval "a_cnt=\${${NS}_cnt_${slot}_${side}:-0}"
        eval "a_sum=\${${NS}_sum_${slot}_${side}:-0}"
        eval "a_max=\${${NS}_max_${slot}_${side}:-0}"
        a_cnt=$((a_cnt + 1))
        a_sum=$((a_sum + ms))
        if [ "$ms" -gt "$a_max" ]; then a_max=$ms; fi
        eval "${NS}_cnt_${slot}_${side}=\$a_cnt"
        eval "${NS}_sum_${slot}_${side}=\$a_sum"
        eval "${NS}_max_${slot}_${side}=\$a_max"

        datalines=$((datalines + 1))
    done < "$rec"

    if [ "$datalines" -eq 0 ]; then
        parity_error "empty $side record: $rec carries no step timings"
    fi
}

# ─────────────────────────────────────────────────────────────────────────────
# compare_records <baseline> <candidate> - the comparator proper
# ─────────────────────────────────────────────────────────────────────────────
compare_records() {
    baseline="$1"
    candidate="$2"

    [ -f "$baseline" ] || parity_error "baseline record not found: $baseline"
    [ -f "$candidate" ] || parity_error "candidate record not found: $candidate"

    ingest "$baseline" v1
    ingest "$candidate" v2

    compared=0
    reported=0
    failed=0
    failed_slots=""

    for slot in $(set -f; rules_slot_ids); do
        v1_metric="$(rule_row "$slot" v1 metric)"
        v1_mode="$(rule_row "$slot" v1 mode)"
        v1_pct="$(rule_row "$slot" v1 pct)"
        v1_ref="$(rule_row "$slot" v1 ref)"
        v1_mand="$(rule_row "$slot" v1 mandatory)"
        v2_metric="$(rule_row "$slot" v2 metric)"
        v2_mode="$(rule_row "$slot" v2 mode)"
        v2_pct="$(rule_row "$slot" v2 pct)"
        v2_ref="$(rule_row "$slot" v2 ref)"
        v2_mand="$(rule_row "$slot" v2 mandatory)"

        eval "v1_cnt=\${${NS}_cnt_${slot}_v1:-0}"
        eval "v1_sum=\${${NS}_sum_${slot}_v1:-0}"
        eval "v1_max=\${${NS}_max_${slot}_v1:-0}"
        eval "v2_cnt=\${${NS}_cnt_${slot}_v2:-0}"
        eval "v2_sum=\${${NS}_sum_${slot}_v2:-0}"
        eval "v2_max=\${${NS}_max_${slot}_v2:-0}"

        metric="$v2_metric"
        mode="$v2_mode"
        pct="$v2_pct"
        ref="$v2_ref"
        [ -n "$metric" ] || metric="$v1_metric"
        [ -n "$mode" ] || mode="$v1_mode"
        [ -n "$pct" ] || pct="$v1_pct"
        [ -n "$ref" ] || ref="$v1_ref"

        # the two sides of a slot must agree on the rule itself
        if [ -n "$v1_metric" ] && [ -n "$v2_metric" ] && [ "$v1_metric" != "$v2_metric" ]; then
            parity_error "rule table error: slot $slot declares two metrics ($v1_metric / $v2_metric)"
        fi
        if [ -n "$v1_mode" ] && [ -n "$v2_mode" ] && [ "$v1_mode" != "$v2_mode" ]; then
            parity_error "rule table error: slot $slot declares two modes ($v1_mode / $v2_mode)"
        fi

        base_val=0
        cand_val=0
        if [ "$metric" = "total" ]; then
            base_val="$v1_sum"
            cand_val="$v2_sum"
        else
            base_val="$v1_max"
            cand_val="$v2_max"
        fi

        case "$mode" in
            one_sided)
                # reported, never gated: the v1-only and v2-only legs have no
                # counterpart to compare against
                if [ "$v1_cnt" -gt 0 ] && [ "$v2_cnt" -gt 0 ]; then
                    b_ms="$v1_sum"
                    c_ms="$v2_sum"
                elif [ "$v1_cnt" -gt 0 ]; then
                    b_ms="$v1_sum"
                    c_ms="-"
                elif [ "$v2_cnt" -gt 0 ]; then
                    b_ms="-"
                    c_ms="$v2_sum"
                else
                    b_ms="-"
                    c_ms="-"
                fi
                reported=$((reported + 1))
                printf 'PARITY slot=%s metric=%s mode=%s baseline_ms=%s candidate_ms=%s allowed_ms=- verdict=REPORTED\n' \
                    "$slot" "$metric" "$mode" "$b_ms" "$c_ms"
                ;;
            delta|budget)
                if [ "$mode" = "budget" ]; then
                    ref_slot="${ref%@*}"
                    ref_side="${ref##*@}"
                    [ -n "$ref_slot" ] || parity_error "rule table error: budget slot $slot has no reference slot"
                    eval "ref_cnt=\${${NS}_cnt_${ref_slot}_${ref_side}:-0}"
                    eval "ref_sum=\${${NS}_sum_${ref_slot}_${ref_side}:-0}"
                    if [ "$ref_cnt" -eq 0 ]; then
                        parity_error "budget reference slot $ref is not exercised by the baseline record"
                    fi
                    base_val="$ref_sum"
                    allowed=$((ref_sum * (100 + pct) / 100))
                else
                    if [ -z "$v1_metric" ]; then
                        parity_error "rule table error: delta slot $slot has no v1 baseline row"
                    fi
                    allowed=$((base_val * (100 + pct) / 100))
                fi

                if [ "$v2_cnt" -eq 0 ]; then
                    if [ "$v2_mand" = "yes" ]; then
                        parity_error "missing mandatory slot $slot in the candidate record"
                    fi
                    reported=$((reported + 1))
                    printf 'PARITY slot=%s metric=%s mode=%s baseline_ms=- candidate_ms=- allowed_ms=- verdict=NOT_COMPARABLE\n' \
                        "$slot" "$metric" "$mode"
                    continue
                fi
                if [ "$mode" = "delta" ] && [ "$v1_cnt" -eq 0 ]; then
                    if [ "$v1_mand" = "yes" ]; then
                        parity_error "missing mandatory slot $slot in the baseline record"
                    fi
                    reported=$((reported + 1))
                    printf 'PARITY slot=%s metric=%s mode=%s baseline_ms=- candidate_ms=- allowed_ms=- verdict=NOT_COMPARABLE\n' \
                        "$slot" "$metric" "$mode"
                    continue
                fi

                compared=$((compared + 1))
                if [ "$cand_val" -le "$allowed" ]; then
                    verdict=PASS
                else
                    verdict=FAIL
                    failed=$((failed + 1))
                    if [ -z "$failed_slots" ]; then
                        failed_slots="$slot"
                    else
                        failed_slots="$failed_slots,$slot"
                    fi
                fi
                printf 'PARITY slot=%s metric=%s mode=%s baseline_ms=%s candidate_ms=%s allowed_ms=%s verdict=%s\n' \
                    "$slot" "$metric" "$mode" "$base_val" "$cand_val" "$allowed" "$verdict"
                ;;
            *)
                parity_error "rule table error: unknown mode '$mode' for slot $slot"
                ;;
        esac
    done

    if [ "$failed" -eq 0 ]; then
        summary_verdict=PASS
    else
        summary_verdict=FAIL
    fi
    printf 'PARITY summary slots_compared=%s slots_reported=%s slots_failed=%s failed_slots=%s verdict=%s\n' \
        "$compared" "$reported" "$failed" "${failed_slots:--}" "$summary_verdict"

    [ "$failed" -eq 0 ] && return 0
    return 1
}

# ─────────────────────────────────────────────────────────────────────────────
# self-test harness (no-argument mode)
# ─────────────────────────────────────────────────────────────────────────────
FAILED=0
ok() {
    echo "PASS: $1"
}
fail() {
    echo "FAIL: $1" >&2
    FAILED=$((FAILED + 1))
}

FIXTURES="$SCRIPT_DIR/fixtures/pair_loop_parity"
BASE_FIXTURE="$FIXTURES/self_test_v1_baseline.tsv"
PASS_FIXTURE="$FIXTURES/self_test_v2_candidate_pass.tsv"
V1_TREE="$TREE_ROOT/workflows/pair_programming_loop"
V2_TREE="$TREE_ROOT/workflows/pair_programming_loop_v2"
PARITY_MD="$TREE_ROOT/workflows/pair_programming_loop_v2/PARITY.md"
MD_RULES="$WORKDIR/md_rules.tsv"

RC=0
OUT=""

run_compare() {
    if OUT="$(sh "$SCRIPT_PATH" --compare "$1" "$2" 2>&1)"; then
        RC=0
    else
        RC=$?
    fi
}

# slot_detail <file> <step-list> - "<step> <ms>" for every step of the slot
slot_detail() {
    awk -v steps="$2" '
        BEGIN { n = split(steps, arr, " "); for (i = 1; i <= n; i++) want[arr[i]] = 1 }
        {
            line = $0
            if (line ~ /^[[:space:]]*#/ || line ~ /^[[:space:]]*$/) next
            split(line, f, /[ \t]+/)
            if (f[1] in want) print f[1] " " f[2]
        }
    ' "$1"
}

# override_record <in> <out> <step=ms>... - rewrite the durations of steps
override_record() {
    ovr="$WORKDIR/overrides.tsv"
    : > "$ovr"
    for kv in "$@"; do
        printf '%s\t%s\n' "${kv%%=*}" "${kv#*=}" >> "$ovr"
    done
    awk -v ovf="$ovr" '
        BEGIN { while ((getline l < ovf) > 0) { split(l, a, "\t"); ov[a[1]] = a[2] } }
        {
            line = $0
            if (line ~ /^[[:space:]]*#/ || line ~ /^[[:space:]]*$/) { print line; next }
            split(line, f, /[ \t]+/)
            if (f[1] in ov) print f[1] "\t" ov[f[1]]; else print line
        }
    ' "$1" > "$2"
}

# md_slot_field <slot> <side> <field>
md_slot_field() {
    awk -F'|' -v want_slot="$1" -v want_side="$2" -v want_field="$3" '
        /^#/ { next }
        $1 == "slot" { next }
        $1 == want_slot && $2 == want_side {
            if (want_field == "metric")    { print $3; exit }
            if (want_field == "mode")      { print $4; exit }
            if (want_field == "pct")       { print $5; exit }
            if (want_field == "ref")       { print $6; exit }
            if (want_field == "mandatory") { print $7; exit }
            if (want_field == "patterns") { print $8; exit }
        }
    ' "$MD_RULES"
}

# md_slot_steps <file> <slot> <side> - the slot's step ids present in a record
md_slot_steps() {
    patterns="$(md_slot_field "$2" "$3" patterns)"
    awk '
        { line = $0
          if (line ~ /^[[:space:]]*#/ || line ~ /^[[:space:]]*$/) next
          split(line, f, /[ \t]+/)
          print f[1] }
    ' "$1" | sort -u > "$WORKDIR/names.txt"
    set -f
    while IFS= read -r step; do
        [ -n "$step" ] || continue
        for pat in $patterns; do
            case "$step" in
                $pat)
                    printf '%s\n' "$step"
                    break
                    ;;
            esac
        done
    done < "$WORKDIR/names.txt"
    set +f
}

# md_stats <file> <steps> - "total max count"
md_stats() {
    awk -v steps="$2" '
        BEGIN { n = split(steps, arr, " "); for (i = 1; i <= n; i++) want[arr[i]] = 1 }
        {
            line = $0
            if (line ~ /^[[:space:]]*#/ || line ~ /^[[:space:]]*$/) next
            split(line, f, /[ \t]+/)
            if (f[1] in want) { total += f[2] + 0; count++; if (f[2] + 0 > max) max = f[2] + 0 }
        }
        END { printf "%d %d %d\n", total, max, count }
    ' "$1"
}

# md_allowed <slot> - the threshold recomputed from the documented spec
md_allowed() {
    slot="$1"
    mode="$(md_slot_field "$slot" v2 mode)"
    pct="$(md_slot_field "$slot" v2 pct)"
    metric="$(md_slot_field "$slot" v2 metric)"
    if [ "$mode" = "budget" ]; then
        ref="$(md_slot_field "$slot" v2 ref)"
        ref_slot="${ref%@*}"
        ref_steps="$(md_slot_steps "$BASE_FIXTURE" "$ref_slot" v1 | tr '\n' ' ')"
        set -- $(md_stats "$BASE_FIXTURE" "$ref_steps")
        base_total="$1"
        echo $((base_total * (100 + pct) / 100))
    else
        base_steps="$(md_slot_steps "$BASE_FIXTURE" "$slot" v1 | tr '\n' ' ')"
        set -- $(md_stats "$BASE_FIXTURE" "$base_steps")
        if [ "$metric" = "total" ]; then base="$1"; else base="$2"; fi
        echo $((base * (100 + pct) / 100))
    fi
}

# md_build_at <in> <out> <steps> <metric> <target> - a record whose slot metric
# equals the target exactly (the boundary sweep)
md_build_at() {
    slot_detail "$1" "$3" > "$WORKDIR/detail.txt"
    total=0
    max=0
    maxstep=""
    first=""
    firstval=0
    while read -r s v; do
        if [ -z "$first" ]; then
            first="$s"
            firstval="$v"
        fi
        total=$((total + v))
        if [ "$v" -gt "$max" ]; then
            max="$v"
            maxstep="$s"
        fi
    done < "$WORKDIR/detail.txt"
    if [ "$4" = "total" ]; then
        newv=$(( $5 - (total - firstval) ))
        override_record "$1" "$2" "$first=$newv"
    else
        override_record "$1" "$2" "$maxstep=$5"
    fi
}

# md_reported <slot> <field> - a field the comparator reported
md_reported() {
    printf '%s\n' "$OUT" | awk -v want_slot="$1" -v want_field="$2" '
        $1 == "PARITY" {
            slot = ""; value = ""
            for (i = 2; i <= NF; i++) {
                split($i, kv, "=")
                if (kv[1] == "slot") slot = kv[2]
                else if (kv[1] == want_field) value = kv[2]
            }
            if (slot == want_slot) { print value; exit }
        }
    '
}

# md_tree_steps <main.chcl> - the step ids of a tree
md_tree_steps() {
    awk '
        function flush() { if (name != "") { print name } }
        /^step "/ { flush(); name = $0; sub(/^step "/, "", name); sub(/".*/, "", name); next }
        END { flush() }
    ' "$1"
}

run_harness() {
    [ -f "$PARITY_MD" ] || {
        echo "FAIL: missing $PARITY_MD — the parity record ships with the comparator" >&2
        exit 1
    }
    [ -d "$FIXTURES" ] || {
        echo "FAIL: missing $FIXTURES — the self-test arms live there" >&2
        exit 1
    }

    # the documented spec (PARITY.md) is what the harness computes against
    awk '
        /^<!-- BEGIN PARITY RULE TABLE -->/ { f = 1; next }
        /^<!-- END PARITY RULE TABLE -->/ { f = 0 }
        f
    ' "$PARITY_MD" > "$MD_RULES"
    if [ ! -s "$MD_RULES" ]; then
        echo "FAIL: $PARITY_MD carries no machine-readable rule table" >&2
        exit 1
    fi

    echo "==> Negative arms: a regressed candidate must FAIL (exit 1)..."
    expect_slot_failure() {
        cand="$1"
        slot="$2"
        run_compare "$BASE_FIXTURE" "$cand"
        if [ "$RC" -ne 1 ]; then
            fail "$slot regression must exit 1 (got $RC) — $cand"
            return
        fi
        if ! printf '%s\n' "$OUT" | grep -q "slot=$slot .*verdict=FAIL"; then
            fail "$slot regression must be reported as a FAIL on slot $slot — $cand"
            return
        fi
        ok "$slot regression FAILS as designed (exit 1, slot named)"
    }
    expect_slot_failure "$FIXTURES/self_test_v2_candidate_regressed_delivery.tsv" delivery_leg
    expect_slot_failure "$FIXTURES/self_test_v2_candidate_regressed_gate.tsv" gate_leg
    expect_slot_failure "$FIXTURES/self_test_v2_candidate_regressed_review.tsv" review_leg
    expect_slot_failure "$FIXTURES/self_test_v2_candidate_regressed_plan_budget.tsv" plan_leg
    expect_slot_failure "$FIXTURES/self_test_v2_candidate_regressed_log.tsv" log_leg

    echo "==> Negative arms: a broken record must ERROR (exit 2)..."
    expect_record_error() {
        cand="$1"
        pattern="$2"
        run_compare "$BASE_FIXTURE" "$cand"
        if [ "$RC" -ne 2 ]; then
            fail "$pattern must exit 2 (got $RC) — $cand"
            return
        fi
        if ! printf '%s\n' "$OUT" | grep -q "$pattern"; then
            fail "$pattern must be reported by the comparator — $cand"
            return
        fi
        ok "$pattern is rejected with exit 2"
    }
    expect_record_error "$FIXTURES/self_test_v2_candidate_unknown_step.tsv" "unmapped step"
    expect_record_error "$FIXTURES/self_test_v2_candidate_malformed_ms.tsv" "malformed duration"
    expect_record_error "$FIXTURES/self_test_v2_candidate_negative_ms.tsv" "malformed duration"
    expect_record_error "$FIXTURES/self_test_v2_candidate_empty.tsv" "empty v2 record"
    expect_record_error "$FIXTURES/self_test_v2_candidate_missing_mandatory.tsv" "missing mandatory slot"
    expect_record_error "$FIXTURES/self_test_v2_candidate_v1_step.tsv" "unmapped step"

    echo "==> Positive arm: an in-threshold candidate must PASS (exit 0)..."
    run_compare "$BASE_FIXTURE" "$PASS_FIXTURE"
    if [ "$RC" -ne 0 ]; then
        fail "the in-threshold candidate must exit 0 (got $RC): $OUT"
    elif ! printf '%s\n' "$OUT" | grep -q "verdict=PASS"; then
        fail "the in-threshold candidate must report a PASS verdict: $OUT"
    else
        ok "the in-threshold candidate PASSES (exit 0)"
    fi
    if printf '%s\n' "$OUT" | grep -q "slots_failed=0"; then
        ok "the positive arm reports slots_failed=0"
    else
        fail "the positive arm must report slots_failed=0: $OUT"
    fi

    echo "==> Self-test: the threshold boundary is exact (allowed passes, allowed+1 fails)..."
    compared_slots=""
    for slot in $(set -f; awk -F'|' '
        /^#/ { next }
        $1 == "slot" { next }
        !seen[$1]++ { print $1 }
    ' "$MD_RULES"); do
        mode="$(md_slot_field "$slot" v2 mode)"
        [ -n "$mode" ] || continue
        [ "$mode" = "one_sided" ] && continue
        base_steps="$(md_slot_steps "$BASE_FIXTURE" "$slot" v1 | tr '\n' ' ')"
        if [ -n "$(md_slot_steps "$BASE_FIXTURE" "$slot" v1)" ] || [ "$mode" = "budget" ]; then
            compared_slots="$compared_slots $slot"
        fi
        if [ -z "$base_steps" ] && [ "$mode" != "budget" ]; then
            continue
        fi
        cand_steps="$(md_slot_steps "$PASS_FIXTURE" "$slot" v2 | tr '\n' ' ')"
        [ -n "$cand_steps" ] || continue
        metric="$(md_slot_field "$slot" v2 metric)"
        allowed="$(md_allowed "$slot")"

        md_build_at "$PASS_FIXTURE" "$WORKDIR/at_allowed.tsv" "$cand_steps" "$metric" "$allowed"
        run_compare "$BASE_FIXTURE" "$WORKDIR/at_allowed.tsv"
        if [ "$RC" -eq 0 ]; then
            ok "$slot at exactly the allowed ${allowed}ms still PASSES"
        else
            fail "$slot at exactly the allowed ${allowed}ms must PASS (got $RC): $OUT"
        fi

        md_build_at "$PASS_FIXTURE" "$WORKDIR/at_over.tsv" "$cand_steps" "$metric" "$((allowed + 1))"
        run_compare "$BASE_FIXTURE" "$WORKDIR/at_over.tsv"
        if [ "$RC" -eq 1 ] && printf '%s\n' "$OUT" | grep -q "slot=$slot .*verdict=FAIL"; then
            ok "$slot one ms past the allowed ${allowed}ms FAILS"
        else
            fail "$slot one ms past the allowed ${allowed}ms must FAIL (got $RC): $OUT"
        fi
    done

    echo "==> Self-test: the reported threshold matches an independent recomputation..."
    run_compare "$BASE_FIXTURE" "$PASS_FIXTURE"
    for slot in $compared_slots; do
        allowed="$(md_allowed "$slot")"
        reported="$(md_reported "$slot" allowed_ms)"
        if [ "$allowed" = "$reported" ]; then
            ok "$slot allowed_ms=$reported matches the independent recomputation"
        else
            fail "$slot allowed_ms=$reported but the independent recomputation says $allowed"
        fi
    done

    echo "==> Self-test: a one_sided leg is reported, never gated..."
    override_record "$BASE_FIXTURE" "$WORKDIR/wip_exploded.tsv" "write_work_log=99999999"
    run_compare "$WORKDIR/wip_exploded.tsv" "$PASS_FIXTURE"
    if [ "$RC" -eq 0 ] && printf '%s\n' "$OUT" | grep -q "slot=wip_leg .*verdict=REPORTED"; then
        ok "wip_leg (v1-only) is REPORTED, never gated"
    else
        fail "wip_leg must be REPORTED and never gate the comparison (rc=$RC): $OUT"
    fi

    echo "==> Self-test: every step of both trees maps into exactly one slot..."
    coverage_failures=0
    for side in v1 v2; do
        if [ "$side" = "v1" ]; then
            tree="$V1_TREE"
        else
            tree="$V2_TREE"
        fi
        md_tree_steps "$tree/main.chcl" > "$WORKDIR/tree_steps.txt"
        while IFS= read -r step; do
            [ -n "$step" ] || continue
            hits=0
            while IFS= read -r pat; do
                [ -n "$pat" ] || continue
                set -f
                case "$step" in
                    $pat) hits=$((hits + 1)) ;;
                esac
                set +f
            done <<EOF
$(awk -F'|' -v want_side="$side" '
    /^#/ { next }
    $1 == "slot" { next }
    $2 == want_side { print $8 }
' "$MD_RULES" | tr ' ' '\n' | sed '/^$/d')
EOF
            if [ "$hits" -ne 1 ]; then
                fail "step $step ($side) maps into $hits slots - it must map into exactly one"
                coverage_failures=$((coverage_failures + 1))
            fi
        done < "$WORKDIR/tree_steps.txt"
    done
    if [ "$coverage_failures" -eq 0 ]; then
        ok "every step of both trees maps into exactly one slot"
    fi

    echo "==> Self-test: the comparator's rule table still matches PARITY.md..."
    awk '
        /^# --- BEGIN PARITY RULE TABLE ---$/ { f = 1; next }
        /^# --- END PARITY RULE TABLE ---$/ { f = 0 }
        f
    ' "$SCRIPT_PATH" > "$WORKDIR/script_rules.tsv"
    if [ ! -s "$WORKDIR/script_rules.tsv" ]; then
        fail "the comparator carries no rule table"
    elif [ "$(cat "$WORKDIR/script_rules.tsv")" = "$(cat "$MD_RULES")" ]; then
        ok "the comparator's rule table and PARITY.md's rule table are identical"
    else
        fail "the comparator's rule table has drifted from PARITY.md's rule table"
    fi

    echo "==> Self-test: the comparator is POSIX sh..."
    if sh -n "$SCRIPT_PATH"; then
        ok "the comparator parses as POSIX sh (sh -n)"
    else
        fail "the comparator does not parse as POSIX sh"
    fi
    for shell_bin in dash; do
        if command -v "$shell_bin" > /dev/null 2>&1; then
            if "$shell_bin" -n "$SCRIPT_PATH" && "$shell_bin" "$SCRIPT_PATH" --compare "$BASE_FIXTURE" "$PASS_FIXTURE" > /dev/null 2>&1; then
                ok "the comparator runs green under $shell_bin"
            else
                fail "the comparator fails under $shell_bin"
            fi
        else
            ok "$shell_bin is not installed - the POSIX proof ran under sh only"
        fi
    done

    if [ "$FAILED" -eq 0 ]; then
        echo "test_pair_loop_parity: all checks passed"
        exit 0
    fi
    echo "test_pair_loop_parity: $FAILED check(s) failed" >&2
    exit 1
}

# ─────────────────────────────────────────────────────────────────────────────
# entry point
# ─────────────────────────────────────────────────────────────────────────────
if [ "$#" -eq 0 ]; then
    run_harness
    exit $?
elif [ "$#" -eq 3 ] && [ "$1" = "--compare" ]; then
    rc=0
    compare_records "$2" "$3" || rc=$?
    exit $rc
else
    echo "usage: $(basename "$0") [--compare <baseline.tsv> <candidate.tsv>]" >&2
    exit 2
fi
