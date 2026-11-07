# sim_lib.sh — shared mechanics for the pair_loop_v2 routing simulator.
# Sourced by workstream_handler_v1/tests/test_pair_loop_v2_routing.sh and the
# router gate suite. Runs under bash (the other workstream suites' convention).

# Marker prefixes the stub dispatches on. They ride the tree COPY's prompt
# templates only (appended at setup time); the shipped tree stays marker-free.
PAIR_V2_MARKER_PLANNER="${PAIR_V2_MARKER_PLANNER:-PAIR-V2 SIM planner probe}"
PAIR_V2_MARKER_PART="${PAIR_V2_MARKER_PART:-PAIR-V2 SIM part brief p}"
PAIR_V2_MARKER_REM="${PAIR_V2_MARKER_REM:-PAIR-V2 SIM remediation probe p}"

pair_v2_marker_line() { # $1=file-ABSENT-ok: prepend the matching marker line
    :
}

# pair_v2_prep_tree DEST: COPY the real v2 tree; neutralize the network dial;
# prepend the dispatch markers; leave the shipped files untouched on disk.
pair_v2_prep_tree() { # $1=dest  (dest is a PARENT dir; the tree lands at
    # dest/workflows/pair_programming_loop_v2 so the REAL "../upstream_sync"
    # source resolves exactly as it does in the repo)
    dest="$1/workflows/pair_programming_loop_v2"
    rm -rf "$dest"
    mkdir -p "$(dirname "$dest")"
    cp -r "${TREE_ROOT}/." "${dest}"
    # (tests/ is workstream-level, not tree-level, in the real layout; nothing
    #  to remove here. The interface/lockfile ride the copy.)

    # The shimmed subworkflow: materialize the neutralized twin at the REAL
    # resolution target ("../upstream_sync" from the tree -> workflows/).
    mkdir -p "${dest}/../upstream_sync"
    updir="${dest}/../upstream_sync"
    cat > "${updir}/main.chcl" <<'____SIM_UP____'
workflow {
    version          = "1"
    criteria_version = ">=0.5.9, >=0.5.52-rc1, <0.6.0"
    name             = "upstream_sync_sim"
    initial_state    = "push_branch"
    target_state     = "done"
    policy { max_total_steps = 4 }
}
variable "worktree_dir" {
    type    = string
    default = ""
}
variable "branch" {
    type    = string
    default = ""
}
variable "workstream_file" {
    type    = string
    default = ""
}
adapter "shell" "sh" {
    source  = "ghcr.io/brokenbots/criteria-adapter-shell"
    version = "0.5.5"
    environment = shell.sim_root_env
}
environment "shell" "sim_root_env" {
    working_directory = "__SIMROOT__/worktree"
}
step "push_branch" {
    target = adapter.shell.sh
    input {
        command = "echo 'SIM: upstream push elided (network neutralized by the routing simulator)'"
    }
    outcome "success" { next = state.done }
    outcome "failure" { next = state.failed }
}
state "done" {
    terminal = true
    success  = true
}
state "failed" {
    terminal = true
    success  = false
}
____SIM_UP____
    sed -i "s|__SIMROOT__|${simroot}|g" "${updir}/main.chcl"

    # 2) Marker lines into the copy's prompt templates (the stub's dispatch
    #    key; LAST marker occurrence in serialized input = active call).
    f="${dest}/agents/pair_plan_prompt.md.tftpl"
    { printf '%s\n' "// ${PAIR_V2_MARKER_PLANNER} (sim dispatch)"; cat "$f"; } > "$f.new" && mv "$f.new" "$f"

    f="${dest}/agents/pair_part_brief.md.tftpl"
    # The template carries the part_id as a template var rendered per slot —
    # the marker needs the rendered id. Insert a marker line that references
    # the SAME var: {{ .part_id }} renders inside the prompt, so the serialized
    # input carries the concrete pN and the stub matches "[marker]pN".
    sed_1='// ${PAIR_V2_MARKER_PART}{{ .part_id }} (sim dispatch)'
    { printf '// %s\n' "${PAIR_V2_MARKER_PART}{{ .part_id }} (sim dispatch)"; cat "$f"; } > "$f.new" && mv "$f.new" "$f"

    f="${dest}/agents/pair_part_remediate.md.tftpl"
    { printf '// %s (sim dispatch)\n' "${PAIR_V2_MARKER_REM}"; cat "$f"; } > "$f.new" && mv "$f.new" "$f"

    # 3) Neutralize the review-log write's log_file var (ruling 12: the
    #    verdict log is a /tmp path in sim; nothing rides on it).
    :
}

# pair_v2_seed REPO_DIR: fresh LOCAL git repo (worktree) + the workstream file
# + seeded part files. NO origin — any network op would die loudly.
pair_v2_seed() { # $1=simroot
    r="$1/worktree"
    git init -q -b main "$r"
    git -C "$r" config user.email sim@pairv2.invalid
    git -C "$r" config user.name "pair-v2-sim"
    mkdir -p "$r/src"
    i=1
    while [ "$i" -le 4 ]; do
        printf 'seed p%d\n' "$i" > "$r/src/f${i}.go"
        i=$((i + 1))
    done
    git -C "$r" add -A
    git -C "$r" commit -qm seed
    git -C "$r" checkout -q -b KB-386SIM
    mkdir -p "$1/intake"
    cat > "$1/intake/KB-386SIM.md" <<'____WS____'
# Sim workstream (pair loop v2 routing simulator)
Part 1: append the sim marker to src/f1.go and commit.
Part 2: append the sim marker to src/f2.go and commit.
Part 3: append the sim marker to src/f3.go and commit.
Part 4: append the sim marker to src/f4.go and commit.
____WS____
}

# run_pair_v2 NAME PLAN_JSON: start the stub, prep the tree, seed the repo,
# apply with the fleet engine, leave EV/RC for the assertion helpers.
run_pair_v2() { # $1=name $2=plan_json
    name="$1"; plan="$2"
    trace() { echo "[sim-trace $name] $1" >&2; }
    treeparent="${WORK}/tree-${name}"
    tree="${treeparent}/workflows/pair_programming_loop_v2"
    simroot="${WORK}/sim-${name}"
    mkdir -p "${simroot}"
    pair_v2_prep_tree "$treeparent"

    port="$(node -e 'const s=require("net").createServer();s.listen(0,"127.0.0.1",()=>{console.log(s.address().port);s.close()})')"
    slog="${WORK}/stub-${name}.log"
    STUB_PORT="$port" PLAN_JSON="$plan" SIM_SEED="$simroot" \
        MARKER_PLANNER="$PAIR_V2_MARKER_PLANNER" \
        MARKER_PART="$PAIR_V2_MARKER_PART" \
        MARKER_REM="$PAIR_V2_MARKER_REM" \
        node "${FIXTURES}/pair_v2_stub.mjs" >"$slog" 2>&1 &
    stub_pid=$!
    trace "stub up pid=$stub_pid port=$port"
    STUB_PIDS="${STUB_PIDS:-} +$stub_pid"

    trace "shim begin"
    STUB_PORT="$port" SIMROOT="$simroot" node "${FIXTURES}/pair_v2_shim.mjs" \
        "$tree/adapters.chcl" "$tree/adapters.chcl" || { trace "SHIM FAILED rc=$?"; return 1; }
    trace "shim done"


    # Seed AFTER the shim, asserted: the shimmed shell adapter binds to the
    # worktree at apply time (live defect: bind died with "chdir no such file
    # or directory" when the seed raced the bind).
    trace "seed begin"
    pair_v2_seed "${simroot}" || { trace "SEED FAILED rc=$?"; return 1; }
    trace "seed done"
    if [ ! -d "${simroot}/worktree/.git" ]; then
        echo "seed failed: worktree missing under ${simroot}" >&2
        ls -la "${simroot}" >&2 || true
        ls -la "${simroot}/worktree" >&2 || true
        return 1
    fi
    EV="${WORK}/events-${name}.log"
    RC=0
    trace "apply begin (CRITERIA_HOME=${CRITERIA_HOME:-unset})"
    timeout "${TIMEOUT_SECS}" "$CRITERIA" apply "$tree" \
        --var workstream_file="${simroot}/intake/KB-386SIM.md" \
        --var worktree_dir="${simroot}/worktree" \
        --var provider_base_url="" \
        --var log_file="" \
        --var developer_initial_prompt="" \
        --var branch_notes="" \
        --output json --ui=false >"$EV" 2>&1 || RC=$?
    kill "$stub_pid" 2>/dev/null

    if [ "${SUITE_DEBUG:-}" = "1" ]; then
        echo "--- events $name (rc=$RC) ---" >&2
        tail -c 3500 "$EV" >&2
        echo "--- stub $name ---" >&2
        tail -c 2000 "$slog" >&2
    fi
}

# has_ev REGEX
has_ev() { grep -q "$1" "$EV"; }
events_type() { grep '^{"schema_version"' "$EV" 2>/dev/null | jq -r '.payload_type' 2>/dev/null; }

# assert_step_outcome NAME STEP OUTCOME
assert_step_outcome() {
    if has_ev "\"step\":\"$2\",\"outcome\":\"$3\""; then
        ok "$1: $2 outcome $3 observed"
    else
        fail "$1: $2 did not route $3"
    fi
}

# assert_no_step_entered NAME STEP: the phantom-slot class — the step must
# NEVER have been entered (its adapter event line absent from the log).
assert_no_step_entered() {
    if has_ev "\"step\":\"$2\"" ; then
        fail "$1: $2 was ENTERED (must never run in this scenario)"
    else
        ok "$1: $2 never entered"
    fi
}

# assert_slot_chain_exact NAME N: slots p1..pN each entered exactly once;
# slots N+1..4 never entered.
assert_slot_chain_exact() {
    i=1
    while [ "$i" -le "$2" ]; do
        # fast or deep lane, exactly one entry per slot:
        n_fast=$(grep -c "\"payload_type\":\"StepEntered\",\"payload\":{\"step\":\"run_part_p${i}_fast\"," "$EV" 2>/dev/null || true)
        n_deep=$(grep -c "\"payload_type\":\"StepEntered\",\"payload\":{\"step\":\"run_part_p${i}_deep\"," "$EV" 2>/dev/null || true)
        total=$((n_fast + n_deep))
        if [ "$total" -eq 1 ]; then
            ok "$1: slot p${i} entered exactly once (fast=$n_fast deep=$n_deep)"
        else
            fail "$1: slot p${i} entered ${total}x (expected 1)"
        fi
        i=$((i + 1))
    done
    i=$((2 + 1))
    i="$2"; i=$((i + 1))
    while [ "$i" -le 4 ]; do
        assert_no_step_entered "$1" "run_part_p${i}_fast"
        assert_no_step_entered "$1" "run_part_p${i}_deep"
        i=$((i + 1))
    done
}

# assert_slot_lane NAME N FAST|DEEP: ruling-9's effort lane observed.
assert_slot_lane() {
    if has_ev "\"step\":\"run_part_p$2_$3\","; then
        ok "$1: slot p$2 ran the $3 lane"
    else
        fail "$1: slot p$2 did not run the $3 lane"
    fi
}

# assert_pair_verdict NAME EXPECT: the aggregate's stdout stamp.
assert_pair_verdict() {
    if has_ev "\"name\":\"pair_verdict\",\"value\":\"$2\"" || has_ev "\"pair_verdict\"[^}]*\"$2\""; then
        ok "$1: pair_verdict == $2"
    else
        fail "$1: pair_verdict != $2 (aggregate stamp missing or wrong)"
    fi
}

# assert_terminal NAME TYPE FINAL: the run's terminal event.
assert_terminal() {
    if has_ev "\"payload_type\":\"$2\".*\"finalState\":\"$3\"" || has_ev "\"payload_type\":\"$2\",\"payload\":{\"finalState\":\"$3\""; then
        ok "$1: $2 finalState=$3"
    else
        fail "$1: $2 with finalState=$3 not observed"
    fi
}

# assert_no_network_ops NAME: zero push/fetch/clone/ls-remote in ANY executed
# command text captured in the run events (LOCAL-ONLY, physics not policy).
assert_no_network_ops() {
    bad=$(grep -oE 'git (push|fetch|clone|ls-remote|remote add)[^"\\]*' "$EV" 2>/dev/null | grep -vc 'SIM: upstream push elided' || true)
    if [ "$bad" -eq 0 ]; then
        ok "$1: zero network operations in executed commands (LOCAL-ONLY)"
    else
        fail "$1: ${bad} network operation(s) observed in executed commands"
    fi
}
