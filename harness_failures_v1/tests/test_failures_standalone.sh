#!/usr/bin/env bash
set -uo pipefail

# KB-279: standalone regression suite for harness_failures_v1 (matrix
# E24-E32 + G12 — the engineered failure-path slice). The failures card's
# acceptance lane is the live green-run plus the engineered-fault runs, each
# landing in its DESIGNED CLASS marker; these asserts pin the COMPILED,
# SOURCE, FIXTURE, and PROBE shapes the live observations read, so a
# half-implementation cannot pass silently:
#
#   E24 provider stall → watchdog/timeout class: the wedge fixture compiles
#       with exactly ONE heartbeat-only step whose designed terminal is
#       `failed` (the StallWatchdog's classification), never a park.
#   E25 provider 5xx → bounded retry then awaiting_human: the retry engine
#       template is rendered and EXECUTED in both halves — the recovery half
#       (transient 503s, recovers on attempt 3, success lane) and the
#       exhaustion half (bounded attempts, clamped sleep budget, exhausted
#       record, exit != 0 — the park route); the tree's retry step routes its
#       failure arm to state.need_assistance.
#   E26 never-scheduled pod vs handshake budget: the registry record names
#       BOTH distinct markers (unscheduled_capacity,
#       handshake_budget_exhausted) — never a merged "failed" class.
#   E27 zero provision_wanted scope emission: the borrower callee dials ONLY
#       the owner-established pass-in identity in the compiled graph.
#   E28 heartbeat reaper discrimination: state.need_assistance is a distinct
#       park terminal (success=false) from state.failed — parks and failures
#       never fold together.
#   E29 restart resume-vs-replay: the probes' records are fixed-content
#       (byte-stable across re-writes at the same env) — a replay can never
#       fake a resume.
#   E30 stale pin refusal (expected-pin mismatch): OBSERVED, not pinned by
#       grep alone — the suite tars the e30 fixture archive, serves it over a
#       throwaway loopback TLS server, and runs the admission pin gate:
#       matched digest validates green; wrong expectation refuses fail-closed
#       naming pinned-vs-resolved verbatim; a byte-tampered archive changes
#       the RESOLVED digest and refuses with the pre-tamper expectation.
#   E31 BaseImageMismatch + E32 broken mirror: registry classes pin the
#       designed markers (base_image_mismatch; mirror_unreachable naming the
#       mirror) — engine/operator-owned surfaces aimed at, not re-implemented.
#   G12 silent env-change closeout: the silent_change fixture validates AND
#       compiles green as shipped (traversal remote.side_b) and a flipped
#       copy (remote.side_a) is also green — the change is silent in the
#       static layers; the compiled graph carries NO subworkflow-level
#       environment attribute (the compiled-but-unread shape).
#   Probe-exec: every record/failure template renders through the engine's
#       template contract and runs under env -i across the environment
#       marker permutations (guarded expansion, no truncation).
#
# Pins are cross-checked against the tree's own lockfile digests so a bumped
# binary cannot sneak through a stale pin. Skips cleanly when the engine or
# jq are unavailable — the CI lanes that lack them pick other gates. The E30
# leg needs `go` (the throwaway TLS archive server) and loud-skips without it.

TREE_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FIXTURES="$(cd "$(dirname "${BASH_SOURCE[0]}")/fixtures" && pwd)"
CRITERIA="${CRITERIA_BIN:-criteria}"

FAILED=0
WORK=""

fail() {
    echo "FAIL: $*" >&2
    FAILED=$((FAILED + 1))
}

ok() {
    echo "ok: $*"
}

SKIP=""

if ! command -v "$CRITERIA" >/dev/null 2>&1; then
    SKIP="criteria binary '$CRITERIA' not found (set CRITERIA_BIN)"
fi
if [ -z "$SKIP" ] && ! command -v jq >/dev/null 2>&1; then
    SKIP="jq not available (compiled-graph assertions)"
fi
if [ -z "$SKIP" ] && ! "$CRITERIA" version >/dev/null 2>&1; then
    SKIP="criteria cannot report a version ('$CRITERIA version' failed)"
fi

if [ -n "$SKIP" ]; then
    echo "SKIP: harness_failures_v1 standalone suite — $SKIP"
    exit 0
fi

WORK="$(mktemp -d /tmp/harness_failures_v1.XXXXXX)"
trap 'rm -rf "$WORK"' EXIT

# ── 1. E-matrix tree: validate green, compile green, graph shape ─────────────

if "$CRITERIA" validate "$TREE_ROOT" >"$WORK/validate.log" 2>&1; then
    ok "tree validates green (harness_failures_v1)"
else
    fail "tree validation failed:" "$(<"$WORK/validate.log")"
fi

"$CRITERIA" compile --format json "$TREE_ROOT" >"$WORK/graph.json" 2>"$WORK/compile.log" || {
    fail "tree compile failed:" "$(<"$WORK/compile.log")"
}

if [ -s "$WORK/graph.json" ]; then
    got_order="$(jq -r '.steps | map(.name) | join(",")' "$WORK/graph.json")"
    want_order="owner_dial,class_registry,fail_class_lane,mark_step_failure,retry_bounded_probe,borrower_callee,loop_fail_probe,mark_aggregate,g12_probe,redial_pass_in"
    if [ "$got_order" = "$want_order" ]; then
        ok "compiled step order is the designed E-lane chain (10 steps)"
    else
        fail "compiled step order = [$got_order], want [$want_order]"
    fi

    second_step="$(jq -r '.steps[1].name' "$WORK/graph.json")"
    if [ "$second_step" = "class_registry" ]; then
        ok "class_registry rides second (the designed-class map live observations read)"
    else
        fail "steps[1] = $second_step, want class_registry"
    fi

    # E25: the retry step's designed failure arm is the awaiting-human PARK.
    park_next="$(jq -r '.steps[] | select(.name=="retry_bounded_probe") | .outcomes[] | select(.name=="failure") | .next' "$WORK/graph.json")"
    if [ "$park_next" = "need_assistance" ]; then
        ok "E25: retry_bounded_probe failure arm parks at state.need_assistance"
    else
        fail "E25: retry failure arm next = $park_next, want need_assistance"
    fi

    # E28: parks and failures are DISTINCT terminals in the compiled states.
    states_names="$(jq -r '.states | map(.name) | join(",")' "$WORK/graph.json")"
    states_success="$(jq -r '.states | map("\( .name )=\( .success )") | join(",")' "$WORK/graph.json")"
    if [ "$states_names" = "done,failed,need_assistance" ] && [ "$states_success" = "done=true,failed=false,need_assistance=false" ]; then
        ok "E28: done/failed/need_assistance terminals with distinct success spellings (park != failed)"
    else
        fail "E28: states = [$states_names] [$states_success], want done,failed,need_assistance with success true/false/false"
    fi

    # The aggregate lane: any_failed routes onward exactly once, post-loop;
    # all_succeeded is the loud design-bug arm.
    agg="$(jq -r '.steps[] | select(.name=="loop_fail_probe") | .outcomes | map("\( .name )=\( .next )") | sort | join(",")' "$WORK/graph.json")"
    if [ "$agg" = "all_succeeded=failed,any_failed=mark_aggregate" ]; then
        ok "aggregate: any_failed routes to mark_aggregate; all_succeeded is the loud design-bug arm"
    else
        fail "aggregate outcomes = [$agg], want all_succeeded=failed,any_failed=mark_aggregate"
    fi

    # The loud design-bug arm on the engineered failure lane: a stimulus that
    # DID NOT fail must land on state.failed, never pass silently.
    bug_arm="$(jq -r '.steps[] | select(.name=="fail_class_lane") | .outcomes[] | select(.name=="success") | .next' "$WORK/graph.json")"
    if [ "$bug_arm" = "failed" ]; then
        ok "loud arms: fail_class_lane success routes to state.failed (a stimulus that did not fail is a design bug)"
    else
        fail "fail_class_lane success arm next = $bug_arm, want failed"
    fi

    # Adapter identities: the three shell members on their designed
    # environments, each carrying the pinned recorded digest.
    pin="sha256:41288ce9ce5d3d46841f40b85208c6208c596f36993a898c23002acfa9100a41"
    adaps="$(jq -r '.adapters | map("\( .type ).\( .name )@\( .environment )@\( .pinned_digest )") | sort | join(",")' "$WORK/graph.json")"
    want_adaps="shell.fail_dialer@remote.iter_body@${pin},shell.g12_declared@remote.g12_side@${pin},shell.pass_in@remote.fail_main@${pin}"
    if [ "$adaps" = "$want_adaps" ]; then
        ok "adapters: the three shell identities on their designed environments with the pinned digest"
    else
        fail "adapters = [$adaps], want [$want_adaps]"
    fi
    if grep -q "$pin" "$TREE_ROOT/.criteria.lock.hcl"; then
        ok "lockfile cross-check: the pinned digest is the recorded fleet pin"
    else
        fail "lockfile does not carry the pinned digest (a bumped binary cannot sneak through)"
    fi

    # E27: the borrower callee dials ONLY the owner-established pass-in
    # identity — a scope with nothing new to wire.
    borrower_dials="$(jq -r '.subworkflows[] | select(.name=="borrower_scope") | .body.steps | map(.adapter) | join(",")' "$WORK/graph.json")"
    if [ "$borrower_dials" = "shell.pass_in" ]; then
        ok "E27: borrower callee dials exactly one identity — the pass-in session (zero provision_wanted scope)"
    else
        fail "E27: borrower dials = [$borrower_dials], want shell.pass_in (only)"
    fi

    # G12: the g12 callee resolves the same-key identity (the SUBSTITUTED
    # side), and NO subworkflow-level environment attribute survives in the
    # compiled graph (the compiled-but-unread shape).
    g12_env="$(jq -r '.subworkflows[] | select(.name=="g12_compose") | .body.adapters[0].environment' "$WORK/graph.json")"
    if [ "$g12_env" = "remote.g12_side" ]; then
        ok "G12: g12 callee's identity resolves to remote.g12_side (same-key substitution)"
    else
        fail "G12: g12 callee adapter environment = $g12_env, want remote.g12_side"
    fi
    sub_envs="$(jq -r '[.subworkflows[] | (keys | any(. == "environment"))] | any' "$WORK/graph.json")"
    if [ "$sub_envs" = "false" ]; then
        ok "G12: no subworkflow-level environment attribute in the compiled graph (compiled-but-unread)"
    else
        fail "G12: .subworkflows[].environment present in the compiled graph — the attribute shape regressed"
    fi
else
    fail "no compiled graph to assert on"
fi

# ── 2. Source pins: the lanes the live observations read ─────────────────────

MAIN="$TREE_ROOT/main.chcl"
if grep -q 'park route (need_assistance)' "$TREE_ROOT/scripts/retry_probe.sh.tftpl" && \
   grep -q 'need_assistance' "$MAIN"; then
    ok "source: the park spelling is thread-woven (retry evidence names the park route, tree declares it)"
else
    fail "source: need_assistance park spelling missing from tree/retry evidence"
fi

iter_lane="$(sed -n '/^step "loop_fail_probe"/,/^}/p' "$MAIN")"
if printf '%s\n' "$iter_lane" | grep -q 'fail_label   = "two"' && \
   printf '%s\n' "$iter_lane" | grep -q 'designed_rc  = "7"' && \
   printf '%s\n' "$iter_lane" | grep -q 'for_each    = var.iter_labels'; then
    ok "source: loop_fail_probe engineered stimulus pinned (label two, rc 7, for_each labels)"
else
    fail "source: loop_fail_probe engineered stimulus pins missing (fail_label two / designed_rc 7 / for_each)"
fi

if grep -q 'templatefile("./scripts/registry_class.sh.tftpl"' "$MAIN"; then
    ok "source: class_registry renders the registry template"
else
    fail "source: class_registry does not render registry_class.sh.tftpl"
fi

g12_block="$(sed -n '/^subworkflow "g12_compose"/,/^}/p' "$MAIN")"
if printf '%s\n' "$g12_block" | grep -q 'environment = remote.g12_side'; then
    ok "G12: parent block traversal pinned (environment = remote.g12_side, source of the inert attribute)"
else
    fail "G12: parent block traversal missing (environment = remote.g12_side)"
fi

# The 4xx pass-through half of the retry contract is source-pinned (a
# verdict-shaped 4xx is never retried; pass-through exits the original rc).
if grep -q '"http 4"\|"returned error: 4"' "$TREE_ROOT/scripts/retry_probe.sh.tftpl" && \
   grep -q 'Non-transient: never retried' "$TREE_ROOT/scripts/retry_probe.sh.tftpl"; then
    ok "E25: retry contract source pins the 4xx pass-through (non-transient) route"
else
    fail "E25: retry contract lost the 4xx pass-through branch"
fi

# ── 3. Probe renders + exec: the lanes the live run executes ─────────────────

shquote() {
    printf "'%s'" "$(printf '%s' "$1" | sed "s/'/'\\\\''/g")"
}

render_probe() {
    # render_probe <template> <out> <field>=<value>...
    local tpl="$1" out="$2"
    shift 2
    local sed_args=() pair field value
    for pair in "$@"; do
        field="${pair%%=*}"
        value="${pair#*=}"
        sed_args+=(-e "s@{{ .${field} | shellquote }}@$(shquote "$value")@")
    done
    sed "${sed_args[@]}" "$tpl" > "$out" || return 1
    ! grep -q '{{' "$out"
}

# (a) record_write: the three guarded env markers under env -i permutations.
# Each spec is <ENVVAR>=<record_field>=<expected-or-<unset>>: the record lines
# are fixed by the template (failure_main_env_marker / failure_iter_env_marker
# / failure_g12_env_marker); <unset> means the pod identity never declared it
# and the ${VAR:-} guard expands it empty.
run_record_case() {
    # run_record_case <case> <ENVVAR>=<field>=<wanted|<unset>>...
    local case_name="$1"
    shift
    local rendered="$WORK/record_${case_name}.sh"
    local rec_root="$WORK/records_${case_name}"
    local env_cmd=(env -i PATH=/usr/bin:/bin)
    local rec_ok=1 spec var_name rest field wanted rec out rc
    for spec in "$@"; do
        var_name="${spec%%=*}"
        rest="${spec#*=}"
        field="${rest%%=*}"
        wanted="${rest#*=}"
        if [ "$wanted" = "<unset>" ]; then
            wanted=""
        else
            env_cmd+=("${var_name}=${wanted}")
        fi
        marker_names+=("${field}|${wanted}")
    done
    if ! render_probe "$TREE_ROOT/scripts/record_write.sh.tftpl" "$rendered" \
        "probe_root=$rec_root" "record_name=rec_${case_name}" "record_value=value_${case_name}"; then
        fail "record render ($case_name): leftover template markers"
        return
    fi
    rec="$rec_root/failure_probe_records/rec_${case_name}"
    out="$("${env_cmd[@]}" sh "$rendered" 2>&1)"
    rc=$?
    if [ "$rc" -ne 0 ]; then
        fail "record exec ($case_name): exit $rc" "${out:-<no output>}"
        return
    fi
    local pair w_name w_val
    for pair in "${marker_names[@]}"; do
        w_name="${pair%%|*}"
        w_val="${pair#*|}"
        if ! grep -Fxq "failure_${w_name}_env_marker=$w_val" "$rec"; then
            fail "record exec ($case_name): line failure_${w_name}_env_marker=$w_val missing in $rec"
            rec_ok=0
        fi
    done
    if [ "$rec_ok" -eq 0 ]; then
        return
    fi
    ok "record exec ($case_name): guarded markers expand exactly (empty = identity never declared the marker)"
}

marker_names=()
run_record_case none "FAILURE_MAIN_ENV_MARKER=main=<unset>" "FAILURE_ITER_ENV_MARKER=iter=<unset>" "FAILURE_G12_ENV_MARKER=g12=<unset>"
marker_names=()
run_record_case owner "FAILURE_MAIN_ENV_MARKER=main=owner" "FAILURE_ITER_ENV_MARKER=iter=<unset>" "FAILURE_G12_ENV_MARKER=g12=<unset>"
marker_names=()
run_record_case all "FAILURE_MAIN_ENV_MARKER=main=owner" "FAILURE_ITER_ENV_MARKER=iter=iter_body" "FAILURE_G12_ENV_MARKER=g12=parent"

# E29: fixed-content records — the SAME render run twice at the SAME env
# writes byte-identical records (a replay can never fake a resume).
e29_render="$WORK/e29_render.sh"
e29_root="$WORK/e29_root"
if render_probe "$TREE_ROOT/scripts/record_write.sh.tftpl" "$e29_render" \
    "probe_root=$e29_root" "record_name=e29_stability" "record_value=byte-stable probe record"; then
    (env -i PATH=/usr/bin:/bin FAILURE_MAIN_ENV_MARKER=owner sh "$e29_render") >/dev/null 2>&1
    cp "$e29_root/failure_probe_records/e29_stability" "$WORK/e29_first.bytes"
    (env -i PATH=/usr/bin:/bin FAILURE_MAIN_ENV_MARKER=owner sh "$e29_render") >/dev/null 2>&1
    if cmp -s "$WORK/e29_first.bytes" "$e29_root/failure_probe_records/e29_stability"; then
        ok "E29: re-written probe record is byte-stable (resume evidence cannot drift into a replay)"
    else
        fail "E29: re-written probe record drifted (resume-vs-replay discrimination is compromised)"
    fi
else
    fail "E29: stability render leaked template markers"
fi

# (b) fail_class: the engineered stimulus fails LOUDLY in its designed class.
fail_class_tpl="$WORK/fail_class.sh"
if ! render_probe "$TREE_ROOT/scripts/fail_class.sh.tftpl" "$fail_class_tpl" \
    "probe_root=$WORK" "stimulus_class=engineered_probe_fail" "stimulus_rc=3" "stimulus_step=fail_class_lane"; then
    fail "fail_class render: leftover template markers"
else
    fail_class_out="$(env -i PATH=/usr/bin:/bin sh "$fail_class_tpl" 2>&1)"
    fail_class_rc=$?
    if [ "$fail_class_rc" -eq 3 ] && printf '%s\n' "$fail_class_out" | grep -q 'class=engineered_probe_fail' && \
       printf '%s\n' "$fail_class_out" | grep -q 'designed_rc=3'; then
        ok "fail_class exec: exits the designed rc with the class marker on the evidence stream"
    else
        fail "fail_class exec: rc=$fail_class_rc (want 3) or class markers missing" "$fail_class_out"
    fi
fi

# (c) iter_fail_write: the engineered iteration failure + the passing labels.
run_iter_case() {
    # run_iter_case <case> <iter_label>
    local case_name="$1" iter_label="$2"
    local rendered="$WORK/iter_${case_name}.sh"
    local rec_root="$WORK/iters_${case_name}"
    if ! render_probe "$TREE_ROOT/scripts/iter_fail_write.sh.tftpl" "$rendered" \
        "probe_root=$rec_root" "record_name=iter_${case_name}" \
        "record_value=iteration label=${iter_label} idx=0 of 1" \
        "iter_label=$iter_label" "fail_label=two" "designed_rc=7"; then
        fail "iter render ($case_name): leftover template markers"
        return
    fi
    local rec="$rec_root/failure_probe_records/iter_${case_name}" out rc
    out="$(env -i PATH=/usr/bin:/bin sh "$rendered" 2>&1)"
    rc=$?
    if [ "$case_name" = "fail" ]; then
        if [ "$rc" -eq 7 ] && grep -Fxq 'engineered_iteration_failure=1' "$rec" && \
           grep -Fxq 'engineered_iteration_class=engineered_iteration_failure' "$rec" && \
           printf '%s\n' "$out" | grep -q 'class=engineered_iteration_failure'; then
            ok "iter exec (fail): label two exits the designed rc 7 with the class record + stderr marker"
        else
            fail "iter exec (fail): rc=$rc or engineered markers missing" "$out"
        fi
    else
        if [ "$rc" -eq 0 ] && grep -Fxq 'engineered_iteration_failure=0' "$rec"; then
            ok "iter exec (pass): label one records normally (the loop's designed split)"
        else
            fail "iter exec (pass): rc=$rc or normal record missing" "$out"
        fi
    fi
}

run_iter_case fail "two"
run_iter_case pass "one"

# (d) retry engine — RECOVERY half: transient 503s, recovered on attempt 3.
retry_rec_root="$WORK/retry_recovered"
retry_tpl="$WORK/retry_recovered.sh"
if ! render_probe "$TREE_ROOT/scripts/retry_probe.sh.tftpl" "$retry_tpl" \
    "probe_root=$retry_rec_root" "record_name=retry_recovered" \
    "record_value=bounded-retry recovery half" \
    "max_attempts=4" "backoff_seconds=1 2 3" "sleep_budget=10" "fail_all=0"; then
    fail "retry render (recovered): leftover template markers"
else
    retry_out="$(env -i PATH=/usr/bin:/bin sh "$retry_tpl" 2>&1)"
    retry_rc=$?
    retry_rec="$retry_rec_root/failure_probe_records/retry_recovered"
    if [ "$retry_rc" -eq 0 ] && grep -Fxq 'retry_recovered=1' "$retry_rec" && \
       grep -Fxq 'retry_exhausted=0' "$retry_rec" && \
       grep -Fxq 'retry_attempts=3' "$retry_rec" && \
       grep -Fxq 'retry_err_class=none' "$retry_rec" && \
       grep -Fxq 'retry_sleep_used_seconds=3' "$retry_rec"; then
        ok "E25 exec (recovered): 2 transient http_5xx attempts then recovery on attempt 3 — success lane"
    else
        fail "E25 exec (recovered): rc=$retry_rc or recovery record fields missing" "$retry_out"
    fi
fi

# (e) retry engine — EXHAUSTION half: every attempt transient, bounded
# schedule, sleep budget clamps the wall clock, exit != 0 (the park route).
retry2_root="$WORK/retry_exhausted"
retry2_tpl="$WORK/retry_exhausted.sh"
if ! render_probe "$TREE_ROOT/scripts/retry_probe.sh.tftpl" "$retry2_tpl" \
    "probe_root=$retry2_root" "record_name=retry_exhausted" \
    "record_value=bounded-retry exhaustion half" \
    "max_attempts=3" "backoff_seconds=2 2 2" "sleep_budget=3" "fail_all=1"; then
    fail "retry render (exhausted): leftover template markers"
else
    retry2_out="$(env -i PATH=/usr/bin:/bin sh "$retry2_tpl" 2>&1)"
    retry2_rc=$?
    retry2_rec="$retry2_root/failure_probe_records/retry_exhausted"
    if [ "$retry2_rc" -ne 0 ] && grep -Fxq 'retry_exhausted=1' "$retry2_rec" && \
       grep -Fxq 'retry_recovered=0' "$retry2_rec" && \
       grep -Fxq 'retry_attempts=3' "$retry2_rec" && \
       grep -Fxq 'retry_err_class=http_5xx' "$retry2_rec" && \
       grep -Fxq 'retry_sleep_used_seconds=3' "$retry2_rec"; then
        ok "E25 exec (exhausted): bounded at 3 attempts, sleep clamped to the budget (3s), honest exit != 0"
    else
        fail "E25 exec (exhausted): rc=$retry2_rc or exhaustion record fields missing" "$retry2_out"
    fi
    if printf '%s\n' "$retry2_out" | grep -q 'result=retry class=http_5xx' && \
       printf '%s\n' "$retry2_out" | grep -q 'result=exhausted class=http_5xx'; then
        ok "E25 exec (exhausted): per-attempt evidence on the stderr channel (attempt# + class)"
    else
        fail "E25 exec (exhausted): attempt evidence missing from the stderr stream" "$retry2_out"
    fi
fi

# (f) registry exec: the designed-class vocabulary record, written live.
registry_root="$WORK/registry"
registry_tpl="$WORK/registry.sh"
if ! render_probe "$TREE_ROOT/scripts/registry_class.sh.tftpl" "$registry_tpl" \
    "probe_root=$registry_root"; then
    fail "registry render: leftover template markers"
else
    rec="$registry_root/failure_probe_records/failure_class_registry"
    if env -i PATH=/usr/bin:/bin sh "$registry_tpl" >/dev/null 2>&1 && [ -f "$rec" ]; then
        reg_ok=1
        for cline in \
            'class=E24 name=stall_watchdog' \
            'class=E25 name=bounded_retry_park' \
            'class=E26 name=unscheduled_capacity' \
            'class=E26 name=handshake_budget_exhausted' \
            'class=E27 name=zero_provision_wanted_borrower' \
            'class=E28 name=heartbeat_park' \
            'class=E29 name=restart_resume' \
            'class=E30 name=stale_pin_refusal' \
            'class=E31 name=base_image_mismatch' \
            'class=E32 name=mirror_unreachable' \
            'class=G12 name=silent_env_change'; do
            if ! grep -q "^${cline}" "$rec"; then
                fail "registry record: designed class line missing: $cline"
                reg_ok=0
            fi
        done
        # E26's discrimination is the WHOLE point: the two markers are distinct
        # lines naming distinct carriers, never a merged "failed" class.
        if grep -q 'class=E26 name=unscheduled_capacity' "$rec" && \
           grep -q 'class=E26 name=handshake_budget_exhausted' "$rec"; then
            ok "E26: registry names unscheduled_capacity AND handshake_budget_exhausted as distinct carriers"
        fi
        if [ "$reg_ok" -eq 1 ]; then
            ok "registry exec: all designed class markers (E24-E32 + G12) land in the registry record"
        fi
    else
        fail "registry exec: record not written"
    fi
fi

# ── 4. Fixture render contract: templatefile knobs must interpolate ──────────
# Every knob a parent .chcl passes to templatefile must reach the rendered
# script through a {{ .key | shellquote }} placeholder — a template that only
# carries shell-default fallbacks would silently IGNORE its declared knobs
# and bake in the defaults instead (review item 2). One assertion per
# fixture: placeholders present in the template, configured values present
# in the render.
render_fixture_script() {
    # render_fixture_script <template> <out> <key>=<value>...
    # Bash-native literal substitution: knob values may contain any character
    # (e.g. '@' in identity strings) that would collide with a sed delimiter.
    local tpl="$1" out="$2"
    shift 2
    local content pair key value ph
    content="$(cat "$tpl")" || return 1
    for pair in "$@"; do
        key="${pair%%=*}"
        value="${pair#*=}"
        ph="{{ .$key | shellquote }}"
        content="${content//"$ph"/"$(shquote "$value")"}"
    done
    printf '%s\n' "$content" > "$out"
    ! grep -q '{{' "$out"
}

E24_FIXTURE="$FIXTURES/e24_stall_wedge"
e24_wedge_tpl="$E24_FIXTURE/scripts/wedge.sh.tftpl"
if [ "$(grep -cF '{{ .wedge_seconds | shellquote }}' "$e24_wedge_tpl")" = "1" ] && \
   [ "$(grep -cF '{{ .probe_root | shellquote }}' "$e24_wedge_tpl")" = "1" ]; then
    if render_fixture_script "$e24_wedge_tpl" "$WORK/e24_wedge_rendered.sh" \
        "wedge_seconds=900" "probe_root=/home/criteria/criteria-harness-failures" && \
       grep -Fq "wedge_seconds=$(shquote 900)" "$WORK/e24_wedge_rendered.sh" && \
       grep -Fq "probe_root=$(shquote /home/criteria/criteria-harness-failures)" "$WORK/e24_wedge_rendered.sh"; then
        ok "E24 fixture render contract: the configured wedge window and probe root carry through to the rendered script (defaults can never silently win)"
    else
        fail "E24 fixture render contract: configured knobs missing from the rendered script"
    fi
else
    fail "E24 fixture render contract: template lost a {{ .knob | shellquote }} placeholder (knobs would be silently ignored)"
fi

pin_tpl="$FIXTURES/e30_stale_pin/scripts/pin_write.sh.tftpl"
if [ "$(grep -cF '{{ .probe_root | shellquote }}' "$pin_tpl")" = "1" ] && \
   [ "$(grep -cF '{{ .record_name | shellquote }}' "$pin_tpl")" = "1" ] && \
   [ "$(grep -cF '{{ .record_value | shellquote }}' "$pin_tpl")" = "1" ]; then
    if render_fixture_script "$pin_tpl" "$WORK/pin_write_rendered.sh" \
        "probe_root=/home/criteria/criteria-harness-failures" "record_name=pin_probe" \
        "record_value=pin-probe identity=shell.pass_in@remote.pin_env (admission pin gate exercised at archive level)" && \
       grep -Fq "record_name=$(shquote pin_probe)" "$WORK/pin_write_rendered.sh" && \
       grep -Fq "record_value=$(shquote 'pin-probe identity=shell.pass_in@remote.pin_env (admission pin gate exercised at archive level)')" "$WORK/pin_write_rendered.sh"; then
        ok "E30 fixture render contract: the declared identity values carry through to the rendered pin probe"
    else
        fail "E30 fixture render contract: configured record knobs missing from the rendered script"
    fi
else
    fail "E30 fixture render contract: template lost a {{ .knob | shellquote }} placeholder (knobs would be silently ignored)"
fi

g12_parent_tpl="$FIXTURES/g12_silent_change/scripts/parent_write.sh.tftpl"
if [ "$(grep -cF '{{ .probe_root | shellquote }}' "$g12_parent_tpl")" = "1" ] && \
   [ "$(grep -cF '{{ .record_name | shellquote }}' "$g12_parent_tpl")" = "1" ] && \
   [ "$(grep -cF '{{ .record_value | shellquote }}' "$g12_parent_tpl")" = "1" ]; then
    if render_fixture_script "$g12_parent_tpl" "$WORK/parent_write_rendered.sh" \
        "probe_root=/home/criteria/criteria-harness-failures" "record_name=g12_silent_parent" \
        "record_value=g12-silent scope=root identity=shell.pass_in@remote.side_a (block traversal below is the inert attribute)" && \
       grep -Fq "record_name=$(shquote g12_silent_parent)" "$WORK/parent_write_rendered.sh" && \
       grep -Fq "record_value=$(shquote 'g12-silent scope=root identity=shell.pass_in@remote.side_a (block traversal below is the inert attribute)')" "$WORK/parent_write_rendered.sh"; then
        ok "G12 fixture render contract (parent): the declared root identity values carry through to the rendered script"
    else
        fail "G12 fixture render contract (parent): configured record knobs missing from the rendered script"
    fi
else
    fail "G12 fixture render contract (parent): template lost a {{ .knob | shellquote }} placeholder (knobs would be silently ignored)"
fi

g12_callee_tpl="$FIXTURES/g12_silent_change/workflows/callee_b/scripts/g12_write.sh.tftpl"
if [ "$(grep -cF '{{ .probe_root | shellquote }}' "$g12_callee_tpl")" = "1" ] && \
   [ "$(grep -cF '{{ .record_name | shellquote }}' "$g12_callee_tpl")" = "1" ] && \
   [ "$(grep -cF '{{ .record_value | shellquote }}' "$g12_callee_tpl")" = "1" ]; then
    if render_fixture_script "$g12_callee_tpl" "$WORK/g12_write_rendered.sh" \
        "probe_root=/home/criteria/criteria-harness-failures" "record_name=g12_silent_callee" \
        "record_value=g12-silent scope=callee identity=shell.callee_declared@remote.callee_side (pod marker would read callee-shadow, never a block-attribute value)" && \
       grep -Fq "record_name=$(shquote g12_silent_callee)" "$WORK/g12_write_rendered.sh" && \
       grep -Fq "record_value=$(shquote 'g12-silent scope=callee identity=shell.callee_declared@remote.callee_side (pod marker would read callee-shadow, never a block-attribute value)')" "$WORK/g12_write_rendered.sh" && \
       grep -Fq "probe_root=$(shquote /home/criteria/criteria-harness-failures)" "$WORK/g12_write_rendered.sh"; then
        ok "G12 fixture render contract (callee): the required probe_root + declared callee identity values carry through to the rendered script"
    else
        fail "G12 fixture render contract (callee): configured record knobs missing from the rendered script"
    fi
else
    fail "G12 fixture render contract (callee): template lost a {{ .knob | shellquote }} placeholder (knobs would be silently ignored)"
fi

# ── 5. E24 fixture: the heartbeat-only wedge (shape-only, never run) ──────────

if "$CRITERIA" validate "$E24_FIXTURE" >"$WORK/e24_validate.log" 2>&1; then
    ok "E24: wedge fixture validates green"
else
    fail "E24: wedge fixture validation failed:" "$(<"$WORK/e24_validate.log")"
fi

"$CRITERIA" compile --format json "$E24_FIXTURE" >"$WORK/e24_graph.json" 2>"$WORK/e24_compile.log" || {
    fail "E24: wedge fixture compile failed:" "$(<"$WORK/e24_compile.log")"
}
if [ -s "$WORK/e24_graph.json" ]; then
    e24_steps="$(jq -r '.steps | map(.name) | join(",")' "$WORK/e24_graph.json")"
    e24_states="$(jq -r '.states | map("\( .name )=\( .success )") | join(",")' "$WORK/e24_graph.json")"
    e24_arms="$(jq -r '.steps[] | .outcomes | map("\( .name )=\( .next )") | sort | join(",")' "$WORK/e24_graph.json")"
    if [ "$e24_steps" = "wedge_poll" ] && [ "$e24_states" = "failed=false" ] && [ "$e24_arms" = "failure=failed,success=failed" ]; then
        ok "E24: exactly one heartbeat-only step; 'failed' is the designed terminal on BOTH arms (the watchdog's classification)"
    else
        fail "E24: wedge shape regressed: steps=[$e24_steps] states=[$e24_states] arms=[$e24_arms]"
    fi
    if grep -q 'heartbeat' "$E24_FIXTURE/scripts/wedge.sh.tftpl" && \
       grep -q 'sleep' "$E24_FIXTURE/scripts/wedge.sh.tftpl"; then
        ok "E24: wedge stimulus emits heartbeat chatter with zero step progress (sleep-past-the-window shape)"
    else
        fail "E24: wedge stimulus lost its heartbeat/sleep shape"
    fi
else
    fail "E24: no compiled wedge graph to assert on"
fi

# ── 6. G12 fixture: the silent env-change experiment ─────────────────────────

G12_FIXTURE="$FIXTURES/g12_silent_change"
if "$CRITERIA" validate "$G12_FIXTURE" >"$WORK/g12_validate.log" 2>&1; then
    ok "G12: silent-change fixture validates green as shipped (traversal remote.side_b)"
else
    fail "G12: silent-change fixture validation failed:" "$(<"$WORK/g12_validate.log")"
fi
"$CRITERIA" compile --format json "$G12_FIXTURE" >"$WORK/g12_graph.json" 2>/dev/null || true
if [ -s "$WORK/g12_graph.json" ]; then
    g12_steps="$(jq -r '.steps | map(.name) | join(",")' "$WORK/g12_graph.json")"
    if [ "$g12_steps" = "parent_dial,silent_env" ]; then
        ok "G12: parent graph compiles (parent_dial -> silent_env callee)"
    else
        fail "G12: parent graph steps = [$g12_steps], want parent_dial,silent_env"
    fi
fi

# The experiment: flip the traversal to the OTHER valid parent-declared
# identity (remote.side_a) in a copy. The change must be SILENT in the static
# layers: validate stays green, compile stays green — no diagnostic, no
# refusal. Only run events could ever tell the difference.
G12_FLIP="$WORK/g12_flip"
cp -r "$G12_FIXTURE" "$G12_FLIP"
sed -i 's/environment = remote.side_b/environment = remote.side_a/' "$G12_FLIP/main.chcl"
if [ "$(grep -cE '^[[:space:]]*environment = remote.side_a' "$G12_FLIP/main.chcl")" = "1" ] && \
   [ "$(grep -cE '^[[:space:]]*environment = remote.side_b' "$G12_FLIP/main.chcl")" = "0" ]; then
    if "$CRITERIA" validate "$G12_FLIP" >"$WORK/g12_flip_validate.log" 2>&1; then
        ok "G12 flipped copy validates green — the traversal change is silent at admission"
    else
        fail "G12: flipping the traversal no longer validates green — the static layers became loud:" "$(<"$WORK/g12_flip_validate.log")"
    fi
    if "$CRITERIA" compile --format json "$G12_FLIP" >"$WORK/g12_flip_graph.json" 2>"$WORK/g12_flip_compile.log"; then
        ok "G12 flipped copy compiles green — the traversal change is silent in the graph shape"
    else
        fail "G12: flipping the traversal no longer compiles green:" "$(<"$WORK/g12_flip_compile.log")"
    fi
else
    fail "G12: traversal flip did not apply cleanly to the copy"
fi

if grep -q 'environment "remote" "side_a"' "$G12_FIXTURE/adapters.chcl" && grep -q 'environment "remote" "side_b"' "$G12_FIXTURE/adapters.chcl"; then
    ok "G12: parent declares BOTH traversable identities (side_a live / side_b the block-attribute experiment)"
else
    fail "G12: parent identities missing (the flip experiment needs both spellings valid)"
fi

# ── 7. E30 admission pin observation (REQUIRES go — an unobserved E30
#    fails the suite; coverage that silently skips is not coverage) ───────────

E30_FIXTURE="$FIXTURES/e30_stale_pin"
E30_OBSERVED=0
e30_server_pid=""
e30_leg() { echo "$1"; }
if ! command -v go >/dev/null 2>&1; then
    e30_leg "SKIP (leg): E30 admission pin observation needs go (the throwaway TLS archive server) — recorded UNOBSERVED; the verdict will exit non-zero"
else
    if "$CRITERIA" validate "$E30_FIXTURE" >"$WORK/e30_validate.log" 2>&1; then
        ok "E30: stale-pin fixture tree validates green (the refusal is exercised at ADMISSION, against the archive)"
    else
        fail "E30: stale-pin fixture tree failed to validate:" "$(<"$WORK/e30_validate.log")"
    fi

    if go build -o "$WORK/e30_tlsServe" "$E30_FIXTURE/tlsServe.go" 2>"$WORK/e30_build.log"; then
        ok "E30: throwaway TLS archive server builds (stdlib only)"
    else
        fail "E30: tlsServe build failed:" "$(<"$WORK/e30_build.log")"
    fi

    if [ -x "$WORK/e30_tlsServe" ]; then
        # Archive with the tree's .chcl at extraction ROOT (the observed
        # engine contract for archive sources).
        tar -C "$E30_FIXTURE" -czf "$WORK/e30_pristine.tgz" .
        tar -C "$E30_FIXTURE" -czf "$WORK/e30_tampered.tgz" .
        printf 'tamper' >> "$WORK/e30_tampered.tgz"
        e30_real="$(sha256sum "$WORK/e30_pristine.tgz" | cut -d' ' -f1)"
        e30_tam="$(sha256sum "$WORK/e30_tampered.tgz" | cut -d' ' -f1)"
        e30_zero="0000000000000000000000000000000000000000000000000000000000000000"
        e30_port=""

        stop_e30_server() {
            if [ -n "$e30_server_pid" ]; then
                kill "$e30_server_pid" 2>/dev/null || true
                wait "$e30_server_pid" 2>/dev/null || true
                e30_server_pid=""
            fi
        }

        start_e30_server() {
            # start_e30_server <file-to-serve>
            local file_to_serve="$1" port="$2" attempt=0
            "$WORK/e30_tlsServe" -port "$port" -file "$file_to_serve" -certout "$WORK/e30_cert.pem" \
                > "$WORK/e30_server.log" 2>&1 &
            e30_server_pid=$!
            while [ "$attempt" -lt 50 ]; do
                if curl -s --cacert "$WORK/e30_cert.pem" -o /dev/null \
                    "https://127.0.0.1:${port}/tree.tgz" >/dev/null 2>&1; then
                    return 0
                fi
                attempt=$((attempt + 1))
                sleep 0.1
            done
            return 1
        }

        # Find a free port by probing the curl reachability of a fresh server.
        e30_try=0
        while [ "$e30_try" -lt 10 ]; do
            e30_port="211$((RANDOM % 10))$((RANDOM % 10))"
            if start_e30_server "$WORK/e30_pristine.tgz" "$e30_port"; then
                break
            fi
            stop_e30_server
            e30_try=$((e30_try + 1))
        done

        if [ -n "$e30_port" ] && curl -s --cacert "$WORK/e30_cert.pem" -o /dev/null \
            "https://127.0.0.1:${e30_port}/tree.tgz" >/dev/null 2>&1; then
            e30_url="https://127.0.0.1:${e30_port}/tree.tgz"
            # The refusal assertions below EXECUTE from this point on — mark
            # the leg observed regardless of their pass/fail verdicts (those
            # outcomes are already counted through fail()).
            E30_OBSERVED=1

            # (1) matched expectation → the source resolves and validates GREEN.
            e30_out="$(SSL_CERT_FILE="$WORK/e30_cert.pem" "$CRITERIA" validate --workflow-ref "sha256:${e30_real}" "$e30_url" 2>&1)"
            e30_rc=$?
            if [ "$e30_rc" -eq 0 ]; then
                ok "E30 (admission): matched expected-pin — the archive source resolves and validates GREEN"
            else
                fail "E30 (admission): matched expectation should validate green (rc=$e30_rc):" "$e30_out"
            fi

            # (2) wrong expectation → fail-closed refusal naming pinned-vs-resolved.
            e30_out="$(SSL_CERT_FILE="$WORK/e30_cert.pem" "$CRITERIA" validate --workflow-ref "sha256:${e30_zero}" "$e30_url" 2>&1)"
            e30_rc=$?
            if [ "$e30_rc" -ne 0 ] && printf '%s\n' "$e30_out" | grep -q 'expected-pin mismatch' && \
               printf '%s\n' "$e30_out" | grep -q "expected \"sha256:${e30_zero}\"" && \
               printf '%s\n' "$e30_out" | grep -q "resolved \"sha256:${e30_real}\"" && \
               printf '%s\n' "$e30_out" | grep -q 'refusing to run'; then
                ok "E30 (admission): wrong expectation REFUSED fail-closed, pinned-vs-resolved named verbatim"
            else
                fail "E30 (admission): wrong-expectation refusal shape missing (rc=$e30_rc):" "$e30_out"
            fi

            # (3) tampered archive → the RESOLVED digest changes → refusal with
            # the PRE-TAMPER expectation (pin checked before content).
            stop_e30_server
            if start_e30_server "$WORK/e30_tampered.tgz" "$e30_port"; then
                e30_out="$(SSL_CERT_FILE="$WORK/e30_cert.pem" "$CRITERIA" validate --workflow-ref "sha256:${e30_real}" "$e30_url" 2>&1)"
                e30_rc=$?
                if [ "$e30_rc" -ne 0 ] && printf '%s\n' "$e30_out" | grep -q "expected \"sha256:${e30_real}\"" && \
                   printf '%s\n' "$e30_out" | grep -q "resolved \"sha256:${e30_tam}\"" && \
                   printf '%s\n' "$e30_out" | grep -q 'refusing to run'; then
                    ok "E30 (admission): tampered archive refuses with the REAL resolved digest named (stale pin, content-blind)"
                else
                    fail "E30 (admission): tampered-archive refusal shape missing (rc=$e30_rc):" "$e30_out"
                fi
            else
                fail "E30 (admission): the tampered-archive server leg failed to start"
            fi
        else
            fail "E30 (admission): could not start the throwaway TLS archive server" "$(<"$WORK/e30_server.log")"
        fi
        stop_e30_server
    fi
fi

# ── Verdict ──────────────────────────────────────────────────────────────────

if [ "$FAILED" -ne 0 ]; then
    echo "harness_failures_v1 standalone suite: FAILED ($FAILED assert(s))"
    exit 1
fi
# The green banner is conditional on EVERY leg being OBSERVED: an E30 that
# never executed (go absent) must fail the suite loudly — a skip is not a
# pass, and gate coverage that silently drops a leg is not coverage.
if [ "${E30_OBSERVED:-0}" -ne 1 ]; then
    echo "harness_failures_v1 standalone suite: all observed failure shapes asserted; E30 SKIPPED (tooling absent) — the designed-class admission observation did not execute; refusing the green banner"
    exit 1
fi
echo "harness_failures_v1 standalone suite: all failure shapes asserted (compile + probes + fixtures + admission pins green)"
exit 0
