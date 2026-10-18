#!/usr/bin/env bash
set -euo pipefail

# KB-276: harness_wire_v1 is the KB-276 wire/transport harness tree
# (matrix slice A1-A6 wire/transport + B7-B12 operator composition;
# matrix of record docs/harness-expectations.md, series card 276). This
# suite guards the tree's deterministic stimulus set:
#
#   1. the tree validates standalone (with the callee subworkflow) and
#      compiles into the exact probe graph;
#   2. the graph carries the two-environment two-kind shape: three shell
#      definition identities in remote.probe_shell and the graph-only
#      copilot member in remote.probe_copilot (A3 identity grammar end to
#      end in the compiled graph, B7/B8 graph-carrier boot member);
#   3. every adapter is digest-pinned and the pin EQUALS its lockfile
#      entry (A2 pinned-present shape; accept_digest_from="lockfile" in
#      both environments);
#   4. NO on_crash is declared anywhere and the compiled graph still
#      carries the language-default fail (A5's stimulus stays in force);
#   5. steps are the exact five-probe chain, boot_probe -> scope_probe_one
#      -> scope_probe_two -> late_join_probe -> redial_probe_a -> done,
#      every failure ending in failed (the run must not lie about its
#      evidence), and both subworkflow layers share ONE callee source and
#      ONE definition identity (B8 dedup stimulus);
#   6. the record-write scripts render with the engine's shellquote
#      idiom, write fixed-content records byte-identically across runs,
#      and treat hostile probe roots as opaque single paths;
#   7. trees are independent: no reference into a sibling tree, and the
#      production trees stay untouched (harness changes never modify
#      production trees).

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

"$CRITERIA" validate "$TREE_ROOT" >/dev/null 2>"$TMP/validate.err" \
    || { echo "FAIL: criteria validate: $(cat "$TMP/validate.err")" >&2; exit 1; }
ok "criteria validate passes standalone (root + callee)"

if grep '^Warning:' "$TMP/validate.err" | grep "harness_wire" >/dev/null 2>&1; then
    grep '^Warning:' "$TMP/validate.err" | grep "harness_wire" >&2
    fail "validation reports warnings originating in the wire harness tree"
else
    ok "wire harness tree contributes no validation warnings"
fi

"$CRITERIA" compile "$TREE_ROOT" --format json --out "$TMP/graph.json" 2>"$TMP/compile.err" \
    || { echo "FAIL: criteria compile: $(cat "$TMP/compile.err")" >&2; exit 1; }
GRAPH="$TMP/graph.json"

# ── 2. The deterministic probe graph ─────────────────────────────────────────

# Two environment identities, exactly as named: the shell scope-set env and
# the copilot boot-member env (the 2-env shape; the identities are what the
# operator's env grouping keys on, A3/A4).
env_names="$(jq -r '[.adapters[].environment] | join(" ")' "$GRAPH")"
require_equal "$(echo "$env_names" | tr ' ' '\n' | LC_ALL=C sort -u | tr '\n' ' ' | sed 's/ $//')" \
    "remote.probe_copilot remote.probe_shell" \
    "environment identities are exactly probe_shell and probe_copilot"

# The definition-identity grammar end to end (A3): <type>.<name> identities
# in the compiled adapters + step targets, exactly the four members.
adapter_identities="$(jq -r '[.adapters[] | "\(.type).\(.name)"] | sort | join(" ")' "$GRAPH")"
require_equal "$adapter_identities" "copilot.boot_member shell.probe_a shell.probe_b" \
    "root definition identities use the <type>.<name> grammar (A3) and are exactly the three members"
callee_identities="$(jq -r '[.subworkflows[] | .body.adapters[] | "\(.type).\(.name)"] | unique | join(" ")' "$GRAPH")"
require_equal "$callee_identities" "shell.probe_dialer" \
    "callee definition identity is shell.probe_dialer (A3) in every subworkflow layer"

# Two kinds, two environments (the 2-env 2-kind census; A4's env-wide kind
# check resolves the image by adapter type).
root_kinds="$(jq -r '[.adapters[] | .type] | join("\n")' "$GRAPH")"
require_equal "$(echo "$root_kinds" | LC_ALL=C sort | tr '\n' ' ' | sed 's/ $//')" \
    "copilot shell shell" \
    "root graph carries the shell probes and the copilot boot member (2 kinds)"

# The copilot boot member is a graph member only (B7/B8): it exists in the
# compiled graph and pins its digest, but NO step or subworkflow targets it.
if jq -e '.steps[] | select(.adapter == "copilot.boot_member")' "$GRAPH" >/dev/null 2>&1; then
    fail "no step may target copilot.boot_member (graph-only boot member, B7/B8)"
else
    ok "no step targets the copilot boot member (dial identity stays absent, B8)"
fi
boot_member_pin="$(jq -r '.adapters[] | select(.name == "boot_member") | .pinned_digest' "$GRAPH")"
case "$boot_member_pin" in sha256:*) ok "copilot boot member carries a pinned digest (B8)" ;;
    *) fail "copilot boot member must carry a pinned digest, got '$boot_member_pin'" ;;
esac

# ── 3. Digest pins == lockfile (A2) ──────────────────────────────────────────

lockfile_digest() {
    local file="$1" type="$2" name="$3"
    awk -v block="adapter \"$type\" \"$name\"" '
        index($0, block " {") == 1 { inb = 1 }
        inb && /resolved_digest/ { gsub(/.*= *"|"/, ""); print $0; exit }
    ' "$file"
}
for def in shell=probe_a shell=probe_b copilot=boot_member; do
    kind="${def%%=*}"; name="${def##*=}"
    pinned="$(jq -r --arg type "$kind" --arg name "$name" \
        '.adapters[] | select(.type == $type and .name == $name) | .pinned_digest' "$GRAPH")"
    locked="$(lockfile_digest "$TREE_ROOT/.criteria.lock.hcl" "$kind" "$name")"
    require_equal "$pinned" "$locked" "$kind.$name pinned digest equals the lockfile entry (A2)"
done
callee_pinned="$(jq -r '[.subworkflows[] | .body.adapters[] | select(.name == "probe_dialer") | .pinned_digest] | .[0]' "$GRAPH")"
callee_locked="$(lockfile_digest "$TREE_ROOT/workflows/scope_probe/.criteria.lock.hcl" shell probe_dialer)"
require_equal "$callee_pinned" "$callee_locked" \
    "callee probe_dialer pinned digest equals its lockfile entry (A2)"

# The environments resolve digests from the lockfile (the A2 wiring shape);
# the pinned-miss stimulus rides the same binding on the rig.
env_blocks="$(grep -c '^environment' "$TREE_ROOT/adapters.chcl")"
[ "$env_blocks" -eq 2 ] || fail "expected exactly 2 environment blocks, got $env_blocks"
accept_digest_from="$(grep -c 'accept_digest_from = "lockfile"' "$TREE_ROOT/adapters.chcl")"
[ "$accept_digest_from" -eq 2 ] || fail "expected accept_digest_from=\"lockfile\" in both environments, got $accept_digest_from"
ok "both environments accept digests from the lockfile (A2)"

# The callee declares the SAME environment identity (A4's shared scope set):
# remote.probe_shell by type+name, mirroring the parent's declaration.
if grep -q '^environment "remote" "probe_shell" {' "$TREE_ROOT/workflows/scope_probe/adapters.chcl"; then
    ok "callee re-declares the same environment identity (A4)"
else
    fail "callee must declare environment remote.probe_shell (the shared scope set)"
fi

# ── 4. Lifecycle stimulus stays in force (A5/A6) ─────────────────────────────

# grep only the declaration sources: the suite's own comments must not match.
if grep -n 'on_crash' "$TREE_ROOT/main.chcl" "$TREE_ROOT/adapters.chcl" \
    "$TREE_ROOT/workflows/scope_probe/adapters.chcl" \
    "$TREE_ROOT/workflows/scope_probe/main.chcl" >/dev/null 2>&1; then
    grep -rn 'on_crash' "$TREE_ROOT" >&2
    fail "on_crash must not be declared anywhere in the harness tree (language default stays in force)"
else
    ok "no on_crash declared anywhere in the tree (A5 language default)"
fi
default_on_crash="$(jq -r '[.adapters[].on_crash] | unique | join(" ")' "$GRAPH")"
require_equal "$default_on_crash" "fail" \
    "compiled adapters carry the language-default on_crash=fail (A5: crash-never-respawn)"

# Per-scope sessions on both environments (the scope-set + release shapes
# need per-scope lifecycles, and the peer serves scopes of one env from one
# pod — the operator composition the tree aims at).
per_scope="$(grep -c 'per_scope_sessions = true' "$TREE_ROOT/adapters.chcl")"
[ "$per_scope" -eq 2 ] || fail "expected per_scope_sessions=true on both environments, got $per_scope"
grep -q 'per_scope_sessions = true' "$TREE_ROOT/workflows/scope_probe/adapters.chcl" \
    || fail "callee environment must carry per_scope_sessions=true (per-scope participant)"
ok "per-scope sessions on both environments (A4/A5/A6 lifecycles)"

# ── 5. Step graph ────────────────────────────────────────────────────────────

terminal_names="$(jq -r '[.states[] | select(.terminal) | .name] | sort | join(" ")' "$GRAPH")"
require_equal "$terminal_names" "done failed" "terminal states are exactly done and failed"
jq -e '.states[] | select(.terminal and .name == "done" and .success)' "$GRAPH" >/dev/null \
    || fail "terminal done must be success=true"
jq -e '.states[] | select(.terminal and .name == "failed" and (.success | not))' "$GRAPH" >/dev/null \
    || fail "terminal failed must be success=false: probe failures must be recorded as run failures"
ok "terminals carry the right success flags"

step_names="$(jq -r '[.steps[] | .name] | sort | join(" ")' "$GRAPH")"
require_equal "$step_names" "boot_probe late_join_probe redial_probe_a scope_probe_one scope_probe_two" \
    "step set is exactly the five-probe chain"

assert_edge() {
    local from="$1" outcome="$2" to="$3"
    local got
    got="$(jq -r --arg from "$from" --arg outcome "$outcome" \
        '.steps[] | select(.name == $from) | .outcomes[] | select(.name == $outcome) | .next' "$GRAPH")"
    require_equal "$got" "$to" "edge $from.$outcome -> $to"
}

assert_edge "boot_probe" "success" "scope_probe_one"
assert_edge "scope_probe_one" "success" "scope_probe_two"
assert_edge "scope_probe_two" "success" "late_join_probe"
assert_edge "late_join_probe" "success" "redial_probe_a"
assert_edge "redial_probe_a" "success" "done"
assert_edge "boot_probe" "failure" "failed"
assert_edge "scope_probe_one" "failure" "failed"
assert_edge "scope_probe_two" "failure" "failed"
assert_edge "late_join_probe" "failure" "failed"
assert_edge "redial_probe_a" "failure" "failed"
ok "probe chain edges and failure paths pinned"

# Initial state and reachability (the whole chain, not a half-graph).
initial="$(jq -r '.initial_state' "$GRAPH")"
require_equal "$initial" "boot_probe" "initial_state is boot_probe"

jq -r '
  .steps[]
  | .name as $s
  | [.outcomes[]? | {from: $s, to: .next}]
  | flatten[]
  | [.from, .to] | @tsv
' "$GRAPH" > "$TMP/edges.tsv"

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
for node in boot_probe scope_probe_one scope_probe_two late_join_probe redial_probe_a "done" failed; do
    [ "${reachable[$node]+set}" = "set" ] || fail "node not reachable from initial_state: $node"
done
ok "every step and terminal is reachable from initial_state"

# Dial order encodes the wire probes: probe_a is the FIRST dial, both
# subworkflow scopes come next, probe_b joins late, probe_a redials last.
first_step_target="$(jq -r '.steps[] | select(.name == "boot_probe") | .adapter' "$GRAPH")"
require_equal "$first_step_target" "shell.probe_a" "the first dial is shell.probe_a (early provision)"
late_join_target="$(jq -r '.steps[] | select(.name == "late_join_probe") | .adapter' "$GRAPH")"
require_equal "$late_join_target" "shell.probe_b" "late_join_probe dials shell.probe_b (B7/B9 late join)"
redial_target="$(jq -r '.steps[] | select(.name == "redial_probe_a") | .adapter' "$GRAPH")"
require_equal "$redial_target" "shell.probe_a" "redial_probe_a dials shell.probe_a again (A5 clean-exit respawn)"
sub_step_targets="$(jq -r '[.steps[] | select(.name | startswith("scope_probe")) | .subworkflow] | join(" ")' "$GRAPH")"
require_equal "$sub_step_targets" "scope_probe_one scope_probe_two" \
    "the two scope probes target the two subworkflow arms (A4/A6)"

# Two subworkflow layers over ONE source with ONE definition identity (B8's
# dedup stimulus: the graph lists the member once regardless of layers).
wf_sources="$(jq -r '[.subworkflows[] | .source_path] | join(" ")' "$GRAPH")"
require_equal \
    "$(echo "$wf_sources" | xargs -n1 basename | LC_ALL=C sort -u | tr '\n' ' ' | sed 's/ $//')" \
    "scope_probe" "both subworkflow arms share one callee source tree"
wf_defs="$(jq -r '[.subworkflows[] | .body.adapters[] | "\(.type).\(.name)"] | join(" ")' "$GRAPH")"
require_equal "$(echo "$wf_defs" | tr ' ' '\n' | LC_ALL=C sort -u | tr '\n' ' ' | sed 's/ $//')" \
    "shell.probe_dialer" "the same definition identity appears across layers (B8 dedup stimulus)"

# The callee flow is the single scope-dial step over the same env identity.
callee_steps="$(jq -r '[.subworkflows[] | .body.steps[].name] | unique | join(" ")' "$GRAPH")"
require_equal "$callee_steps" "record_scope" "callee flow is exactly the one scope-dial step"

# ── 6. Secret-free + sibling-free shape ──────────────────────────────────────

# The only secret is the copilot boot member's GitHub token over the secret
# channel; the shell probes and the callee bind nothing.
shell_secrets="$(grep -c 'secrets {' "$TREE_ROOT/adapters.chcl")"
[ "$shell_secrets" -eq 1 ] || fail "expected secrets on exactly one root adapter (the copilot member), got $shell_secrets"
if grep -A3 'secrets {' "$TREE_ROOT/adapters.chcl" | grep -q 'GITHUB_TOKEN = var.workflow_github_token'; then
    ok "the copilot member binds its token over the secret channel (name-ref only)"
else
    fail "copilot member must bind GITHUB_TOKEN from the secret variable"
fi
if grep -Eq 'github_token|GITHUB_TOKEN' "$TREE_ROOT/workflows/scope_probe/adapters.chcl" \
    "$TREE_ROOT/workflows/scope_probe/variables.chcl"; then
    fail "the callee must stay secret-free (no token bindings)"
else
    ok "callee stays secret-free"
fi
if grep -rEn 'gh[pousr]_[A-Za-z0-9]{20,}' "$TREE_ROOT" >/dev/null 2>&1; then
    fail "a token-shaped literal appears in the tree (CRI-250)"
else
    ok "no token-shaped literals in the tree (CRI-250)"
fi

# The token placeholder is the runner-substituted wire token shape, never a
# literal credential.
if grep -c '__CRITERIA_REMOTE_TOKEN__' "$TREE_ROOT/adapters.chcl" | grep -q '^2$' \
    && grep -c '__CRITERIA_REMOTE_TOKEN__' "$TREE_ROOT/workflows/scope_probe/adapters.chcl" | grep -q '^1$'; then
    ok "wire accept tokens are runner-substituted placeholders (secret-free)"
else
    fail "accept_token must be the runner-substituted placeholder in every environment"
fi

# Trees are independent: no file path into a sibling tree, and the callee is
# in-tree.
if grep -rn -e 'kanboard_' -e 'workstream_handler_v1/' -e 'ticket_cleanup_v1/' \
    -e 'decision_demo' -e 'qa_triage_v1/' \
    "$TREE_ROOT/main.chcl" "$TREE_ROOT/adapters.chcl" "$TREE_ROOT/scripts" \
    >/dev/null 2>&1; then
    grep -rn -e 'kanboard_' -e 'workstream_handler_v1/' -e 'ticket_cleanup_v1/' \
        -e 'decision_demo' -e 'qa_triage_v1/' \
        "$TREE_ROOT/main.chcl" "$TREE_ROOT/adapters.chcl" "$TREE_ROOT/scripts" >&2
    fail "the tree references a sibling workflow's files — trees must be independently runnable"
else
    ok "no file path into the sibling trees"
fi

# No agent steps: every step targets a shell adapter or a subworkflow, and
# no copilot dial exists (the copilot member exists only in the graph).
if jq -e '[.steps[] | .adapter] | map(select(startswith("copilot."))) | length > 0' "$GRAPH" >/dev/null 2>&1; then
    fail "no step may target a copilot adapter (NO agent steps)"
else
    ok "no agent steps anywhere in the flow (NO agent steps)"
fi

# ── 7. Record-write script behavior ──────────────────────────────────────────

# Rendering mimics templatefile for the variables the scripts consume,
# including the engine's shellquote (single-quote wrapping with '\'' escaping).
shquote() {
    printf "'%s'" "${1//\'/\'\\\'\'}"
}
# busybox sed cannot take multi-line replacements, so tokens whose value has
# newlines are matched by exact whole line inside awk; the rest go through
# sed -e. Usage: render_tokens <src> <out> <token>=<replacement>...
render_tokens() {
    local src="$1" out="$2"
    shift 2
    local -a sed_args=() awk_args=()
    local pair token replacement
    for pair in "$@"; do
        token="${pair%%=*}"
        replacement="${pair#*=}"
        if [[ "$replacement" == *$'\n'* ]]; then
            awk_args+=(-v "tok=$token" -v "rep=$replacement")
        else
            replacement="${replacement//\\/\\\\}"       # \ -> \\ (sed emits \)
            replacement="${replacement//'&'/'\&'}"      # & -> \& (sed emits &)
            sed_args+=("-es@$token@$replacement@g")
        fi
    done
    sed "${sed_args[@]}" "$src" \
        | awk "${awk_args[@]}" \
        'length(tok) > 0 && index($0, tok) > 0 { i = index($0, tok); printf "%s%s%s\n", substr($0, 1, i - 1), rep, substr($0, i + length(tok)); next } { print }' \
        > "$out"
}
render_record() {
    render_tokens "$4" "$5" \
        "{{ .probe_root | shellquote }}=$(shquote "$1")" \
        "{{ .record_name | shellquote }}=$(shquote "$2")" \
        "{{ .record_lines | shellquote }}=$(shquote "$3")"
}
render_scope() {
    render_tokens "$4" "$5" \
        "{{ .probe_root | shellquote }}=$(shquote "$1")" \
        "{{ .scope_label | shellquote }}=$(shquote "$2")" \
        "{{ .record_lines | shellquote }}=$(shquote "$3")"
}

RENDERED_LINES_BOOT='definition-identity=shell.probe_a
scope-set-role=early-dial
wire-probe=harness_wire_v1/boot_probe'

probe_dir="$TMP/probe"
boot_render="$TMP/boot.sh"
render_record "$probe_dir" "boot_record" "$RENDERED_LINES_BOOT" \
    "$TREE_ROOT/scripts/record_write.sh.tftpl" "$boot_render"
chmod +x "$boot_render"

if out="$("$boot_render")"; then
    ok "record_write renders and runs (stub-free: fixed content only)"
else
    fail "record_write failed: $out"
fi
# The record path sits OUTSIDE the repository and carries the exact content.
[ -s "$probe_dir/wire_probe_records/boot_record" ] \
    || fail "boot record missing under the probe root's wire_probe_records"
require_equal "$(cat "$probe_dir/wire_probe_records/boot_record")" "$RENDERED_LINES_BOOT" \
    "boot record content is byte-exact"

# Determinism: a second write is byte-identical and does not accumulate.
"$boot_render" >/dev/null 2>&1
require_equal "$(cat "$probe_dir/wire_probe_records/boot_record")" "$RENDERED_LINES_BOOT" \
    "record writes are byte-identical across runs (no accumulation)"

# The scope record per arm (the two subworkflow arms).
scope_lines_arm1='definition-identity=shell.probe_dialer
scope-set-role=subscope-dial
wire-probe=harness_wire_v1/scope_probe_one'
scope_render="$TMP/scope.sh"
render_scope "$probe_dir" "one" "$scope_lines_arm1" \
    "$TREE_ROOT/workflows/scope_probe/scripts/scope_record.sh.tftpl" "$scope_render"
chmod +x "$scope_render"
if out="$("$scope_render")"; then
    ok "scope record script renders and runs"
else
    fail "scope record script failed: $out"
fi
require_equal "$(cat "$probe_dir/wire_probe_records/scope_record_one")" "$scope_lines_arm1" \
    "scope record for arm one is byte-exact"
grep -q "scope one" <<<"$out" || fail "scope record script must report the written scope"

# A hostile probe root is one opaque single path: shellquote keeps the whole
# value in one quoted word, so no command breaks out of the record write.
hostile="$TMP/hostile'; rm -f $TMP/marker; '"
hostile_render="$TMP/hostile.sh"
render_record "$hostile" "boot_record" "wire-probe=hostile" \
    "$TREE_ROOT/scripts/record_write.sh.tftpl" "$hostile_render"
chmod +x "$hostile_render"
if "$hostile_render" >/dev/null 2>&1; then
    ok "hostile probe_root renders without breaking out"
else
    fail "hostile probe_root must still write its record (opaque single path)"
fi
[ ! -e "$TMP/marker" ] || fail "a hostile probe_root must not execute interpolated commands"
require_equal "$(cat "$hostile/wire_probe_records/boot_record" 2>/dev/null)" "wire-probe=hostile" \
    "the hostile root's record lands at the ONE opaque path with exact content"

# The rendered scripts reference no network and no credential: they only
# mkdir/printf/echo, so the record carrier stays deterministic in airgapped
# runs.
if grep -En 'curl|wget|gh |git ' "$TREE_ROOT/scripts/record_write.sh.tftpl" \
    "$TREE_ROOT/workflows/scope_probe/scripts/scope_record.sh.tftpl" >/dev/null 2>&1; then
    fail "probe scripts must not touch the network or git"
else
    ok "probe scripts are offline-only (deterministic)"
fi

# ── 8. Production trees untouched ────────────────────────────────────────────

untouched="$(git -C "$REPO_ROOT" status --porcelain -- \
    ticket_cleanup_v1 kanboard_triage_v1 kanboard_develop_v1 qa_triage_v1 \
    decision_demo_v1 decision_demo_local_v1 workstream_handler_v1 devops_triage_v1 \
    criteria-k8s)"
if [ -z "$untouched" ]; then
    ok "production trees and the operator source are untouched (harness changes never modify them)"
else
    fail "production trees have uncommitted changes (out of harness-card scope): $untouched"
fi

if [ "$FAILED" -gt 0 ]; then
    echo "FAILED: $FAILED assertion(s)" >&2
    exit 1
fi
echo "PASS: harness_wire_v1 standalone verified"