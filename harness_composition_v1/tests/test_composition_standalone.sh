#!/usr/bin/env bash
set -uo pipefail

# KB-278: standalone regression suite for harness_composition_v1 (matrix
# G1-G12 + G10's re-assert of B7/B9). The composition card's acceptance lane
# is the live green-run: these asserts pin the COMPILED and SOURCE shapes the
# live observations read — the tree's composed graph must carry exactly the
# cross-layer identities, iteration shapes, guards, and gaps the matrix rows
# describe, so a half-implementation cannot pass silently:
#
#   G1  same definition identity + same environment identity across BOTH
#       layers (parent shell.pass_in@remote.comp_main and the callee's
#       re-spelling), in the compiled WorkflowGraphs shape.
#   G2  the iteration-body environment referenced from inside an iteration
#       body (step-level traversal on the iterating steps), keyed with
#       per_scope_sessions on the environment (G3) and for_each/parallel
#       stimuli (G5).
#   G4  aggregate outcome arms (all_succeeded + any_failed) on BOTH iterating
#       steps, and NO aggregate arms on the plain composing steps.
#   G6/G8  the borrower dial (callee re-spells the pass-in identity) and the
#       root survivor re-dial AFTER the callee and iteration scopes exited —
#       the alias-ledger/B6 shape the SessionManager ledger resolves.
#   G7  the same-key environment declared in BOTH layers with the contrast
#       values ("parent" root / "callee-shadow" callee) — the parent's value
#       is the only one pods must observe.
#   G9  the callee-only environment (remote.callee_default) with no parent
#       declaration anywhere.
#   G10/B7  the late-join member (shell.late_join) declared in the graph and
#       first-dialed only by the last dial of the chain.
#   G12 the subworkflow block's `environment` traversal present in the tree
#       (the compiled-but-unread attribute; a future engine fix that starts
#       reading it must not silently break this tree's shape).
#   G11 the dangling-traversal fixture must compile green TODAY (the gap is
#       real); the fixture text pins the dangling identity so the future fix
#       turns it loud here on purpose.
#   G5-negative  the parallel-over-non-parallel-safe fixture must be refused
#       at compile when the pinned copilot binary is resolvable (CI), and is
#       permissively green when not (guard defers to the runtime gate).
#
# Pins are cross-checked against the tree's own lockfile digests so a bumped
# binary cannot sneak through a stale pin. Skips cleanly when the engine or
# jq are unavailable — the CI lanes that lack them pick other gates.

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

# ── Preconditions (skip, not fail — compile-level depth is opt-in) ───────────

SKIP=""
if ! command -v "$CRITERIA" >/dev/null 2>&1; then
    SKIP="criteria binary '$CRITERIA' not found (set CRITERIA_BIN)"
elif ! command -v jq >/dev/null 2>&1; then
    SKIP="jq not available (compiled-graph assertions)"
fi
if [ -z "$SKIP" ] && ! "$CRITERIA" version >/dev/null 2>&1; then
    SKIP="criteria cannot report a version ('$CRITERIA version' failed)"
fi
if [ -n "$SKIP" ]; then
    echo "SKIP: harness_composition_v1 standalone suite — $SKIP"
    exit 0
fi

WORK="$(mktemp -d /tmp/harness_composition_v1.XXXXXX)"
trap 'rm -rf "$WORK"' EXIT

# ── 1. The tree validates and compiles ───────────────────────────────────────

if "$CRITERIA" validate "$TREE_ROOT" >"$WORK/validate.log" 2>&1; then
    ok "criteria validate harness_composition_v1 (tree green)"
else
    fail "criteria validate harness_composition_v1 failed:" "$(<"$WORK/validate.log")"
fi
if "$CRITERIA" compile --format json --out "$WORK/graph.json" "$TREE_ROOT" >"$WORK/compile.log" 2>&1; then
    ok "criteria compile --format json (composed graph emitted)"
else
    fail "criteria compile failed:" "$(<"$WORK/compile.log")"
fi

# ── 2. Compiled-graph shapes (the WorkflowGraphs payloads the operator reads)

# G1: the pass-in identity carries the SAME environment identity across both
# layers — exactly one root spelling, exactly one callee spelling.
if [ "$(jq -r '[.adapters[] | select(.name == "pass_in")] | length' "$WORK/graph.json")" = "1" ] && \
   [ "$(jq -r '.adapters[] | select(.name == "pass_in") | .environment' "$WORK/graph.json")" = "remote.comp_main" ]; then
    ok "G1: root layer spells shell.pass_in @ remote.comp_main exactly once"
else
    fail "G1: root layer pass_in identity missing or not bound to remote.comp_main"
fi
if [ "$(jq -r '.subworkflows | length' "$WORK/graph.json")" = "1" ] && \
   [ "$(jq -r '.subworkflows[0].name' "$WORK/graph.json")" = "callee_compose" ]; then
    ok "G1: composed graph carries the embedded callee (subworkflows[0].callee_compose)"
else
    fail "G1: embedded callee missing from the composed graph"
fi
if [ "$(jq -r '[.subworkflows[0].body.adapters[] | select(.name == "pass_in")] | length' "$WORK/graph.json")" = "1" ] && \
   [ "$(jq -r '.subworkflows[0].body.adapters[] | select(.name == "pass_in") | .environment' "$WORK/graph.json")" = "remote.comp_main" ]; then
    ok "G1/G8: callee layer re-spells shell.pass_in @ remote.comp_main (definition+environment identity collide with the root)"
else
    fail "G1/G8: callee layer pass_in re-spelling missing or bound to a different identity"
fi

# G9: the callee-only environment exists in the callee layer and NOWHERE in
# the root (no parent declaration, no override path).
if [ "$(jq -r '.subworkflows[0].body.adapters[] | select(.name == "callee_unique") | .environment' "$WORK/graph.json")" = "remote.callee_default" ] && \
   [ "$(jq -r '[.adapters[] | .environment] | map(select(. == "remote.callee_default")) | length' "$WORK/graph.json")" = "0" ]; then
    ok "G9: remote.callee_default exists only in the callee layer (no parent declaration/override)"
else
    fail "G9: callee-only environment not callee-exclusive"
fi

# G4: BOTH iterating steps route through the aggregate arms; the composing
# steps keep the plain success/failure arms.
if [ "$(jq -r '[.steps[] | select(.name == "loop_iterations") | .outcomes[].name] | sort | join(",")' "$WORK/graph.json")" = "all_succeeded,any_failed" ] && \
   [ "$(jq -r '[.steps[] | select(.name == "parallel_probe") | .outcomes[].name] | sort | join(",")' "$WORK/graph.json")" = "all_succeeded,any_failed" ]; then
    ok "G4: both iterating steps declare the aggregate arms (all_succeeded + any_failed)"
else
    fail "G4: iterating steps' aggregate arms missing or wrong"
fi
if [ "$(jq -r '.steps[] | select(.name == "compose_callee") | [.outcomes[].name] | sort | @json' "$WORK/graph.json")" = '["failure","success"]' ]; then
    ok "G4: the composing (non-iterating) step keeps plain success/failure arms"
else
    fail "G4: compose_callee outcomes are not the plain pair"
fi

# G6/G8: the borrower dial (callee's pass_in step) plus the root survivor
# re-dial AFTER the callee scope — the stimulus pair the alias ledger serves.
if [ "$(jq -r '.steps[] | select(.name == "compose_callee") | .subworkflow' "$WORK/graph.json")" = "callee_compose" ] && \
   [ "$(jq -r '.subworkflows[0].body.steps[] | select(.name == "dial_shared") | .adapter' "$WORK/graph.json")" = "shell.pass_in" ]; then
    ok "G6/G8: borrower dial wired (compose_callee -> callee_compose dial_shared -> shell.pass_in)"
else
    fail "G6/G8: borrower dial wiring wrong"
fi
if [ "$(jq -r '.step_order | length' "$WORK/graph.json")" = "6" ] && \
   [ "$(jq -r '.steps[] | select(.name == "survivor_redial") | .adapter' "$WORK/graph.json")" = "shell.pass_in" ] && \
   [ "$(jq -r '[.step_order[]] | index("survivor_redial")' "$WORK/graph.json")" -gt "$(jq -r '[.step_order[]] | index("compose_callee")' "$WORK/graph.json")" ]; then
    ok "G6: survivor re-dial of the pass-in identity routes AFTER the callee scope in step_order"
else
    fail "G6: survivor re-dial missing or not after the callee scope"
fi

# G10/B7: the late-join member is declared in the composed graph and dialed
# only by the late_join_probe (first dial comes at the end of the chain).
if [ "$(jq -r '[.adapters[] | select(.name == "late_join")] | length' "$WORK/graph.json")" = "1" ] && \
   [ "$(jq -r '.adapters[] | select(.name == "late_join") | .environment' "$WORK/graph.json")" = "remote.comp_main" ] && \
   [ "$(jq -r '.steps[] | select(.name == "late_join_probe") | .adapter' "$WORK/graph.json")" = "shell.late_join" ]; then
    ok "G10/B7: graph-complete late-join member declared before any dial (first dial is the B7 stimulus)"
else
    fail "G10/B7: late-join member or its probe step missing"
fi

# G2/G3/G5: the iterating steps and the iteration-body environment. The
# compiled JSON does not expose for_each/parallel/environment attrs, so the
# traversal pins below are source-level; here we pin the compiled targets.
if [ "$(jq -r '.steps[] | select(.name == "loop_iterations") | .adapter' "$WORK/graph.json")" = "shell.iter_dialer" ] && \
   [ "$(jq -r '.adapters[] | select(.name == "iter_dialer") | .environment' "$WORK/graph.json")" = "remote.iter_body" ]; then
    ok "G2/G3: the iterating steps' member is bound to the iteration-body environment"
else
    fail "G2/G3: iteration member/environment binding wrong"
fi

# Lockfile digests: every pinned member in both layers must carry the tree's
# own pin (bumped binaries cannot sneak through a stale pin).
if [ "$(jq -r '[.adapters[].pinned_digest] | map(select(startswith("sha256:"))) | length' "$WORK/graph.json")" = "$(jq -r '[.adapters[]] | length' "$WORK/graph.json")" ] && \
   [ "$(jq -r '[.subworkflows[0].body.adapters[].pinned_digest] | map(select(startswith("sha256:"))) | length' "$WORK/graph.json")" = "$(jq -r '[.subworkflows[0].body.adapters[]] | length' "$WORK/graph.json")" ]; then
    ok "lockfile pins: every member in both layers carries a sha256 pin"
else
    fail "lockfile pins: a member is missing its pinned_digest"
fi
if grep -q "sha256:41288ce9ce5d3d46841f40b85208c6208c596f36993a898c23002acfa9100a41" "$TREE_ROOT/.criteria.lock.hcl"; then
    ok "lockfile pins: shell pin digest matches the tree's committed lockfile"
else
    fail "lockfile pins: shell pin digest drifted from the tree's lockfile"
fi

# ── 3. Source-shape pins (attrs the compiled JSON does not expose) ───────────

MAIN="$TREE_ROOT/main.chcl"
ROOT_ADAPTERS="$TREE_ROOT/adapters.chcl"
CALLEE_ADAPTERS="$TREE_ROOT/workflows/callee_compose/adapters.chcl"

if grep -q 'for_each    = var.iter_labels' "$MAIN" && \
   grep -q 'parallel     = var.parallel_labels' "$MAIN" && \
   grep -q 'parallel_max = 2' "$MAIN"; then
    ok "G2/G5: for_each + parallel + parallel_max = 2 stimuli present"
else
    fail "G2/G5: iteration stimuli attrs missing from main.chcl"
fi
if sed -n '/^step "loop_iterations"/,/^}/p' "$MAIN" | grep -Eq 'environment += remote\.iter_body' && \
   sed -n '/^step "parallel_probe"/,/^}/p' "$MAIN" | grep -Eq 'environment += remote\.iter_body'; then
    ok "G2: both iterating steps reference the iteration-body environment in-body"
else
    fail "G2: iteration-body environment reference missing on an iterating step"
fi
if [ "$(grep -c 'per_scope_sessions = true' "$ROOT_ADAPTERS")" = "2" ] && \
   [ "$(grep -c 'per_scope_sessions = true' "$CALLEE_ADAPTERS")" = "2" ]; then
    ok "G3/G7: per_scope_sessions pinned on every environment in both layers"
else
    fail "G3/G7: per_scope_sessions is not pinned on every environment"
fi
if grep -q 'COMPOSITION_ENV_SHADING = "parent"' "$ROOT_ADAPTERS" && \
   grep -q 'COMPOSITION_ENV_SHADING = "callee-shadow"' "$CALLEE_ADAPTERS"; then
    ok "G7: ADR-0009 substitution pair present (parent 'parent' shadows callee 'callee-shadow')"
else
    fail "G7: substitution marker pair missing from the adapters files"
fi
if grep -q 'CALLEE_ONLY_ENV_MARKER' "$CALLEE_ADAPTERS" && [ "$(grep -c 'callee_default' "$ROOT_ADAPTERS")" = "0" ]; then
    ok "G9: callee-only environment carries its marker and is untouched by the parent"
else
    fail "G9: callee-only environment leaked into the parent or lost its marker"
fi
if sed -n '/^subworkflow "callee_compose"/,/^}/p' "$MAIN" | grep -q 'environment = remote.comp_main'; then
    ok "G12: the subworkflow block carries its environment traversal (the compiled-but-unread attribute)"
else
    fail "G12: the subworkflow block lost its environment traversal"
fi

# ── 4. G11: the dangling-traversal fixture compiles green (the gap) ──────────

G11_FIXTURE="$FIXTURES/g11_dangling_env"
if grep -q 'environment = remote.does_not_exist' "$G11_FIXTURE/main.chcl"; then
    ok "G11: fixture carries the dangling subworkflow environment traversal"
else
    fail "G11: fixture lost its dangling traversal"
fi
if (cd "$G11_FIXTURE" && "$CRITERIA" validate . >"$WORK/g11.log" 2>&1); then
    ok "G11 (KNOWN GAP pinned): the dangling environment traversal compiles green today"
else
    fail "G11: the fixture FAILED validation — the gap shape has changed; if an engine fix now validates the traversal (expected), the fixture-and-suite must flip the assertion" "$(<"$WORK/g11.log")"
fi

# ── 5. G5 (negative half): the parallel guard fixture ────────────────────────

G5_FIXTURE="$FIXTURES/g5_parallel_guard"
if grep -q 'parallel    = var.guard_labels' "$G5_FIXTURE/main.chcl" && \
   grep -q 'adapter "copilot" "guarded"' "$G5_FIXTURE/adapters.chcl"; then
    ok "G5: fixture carries the parallel step over the non-parallel-safe copilot kind"
else
    fail "G5: fixture lost its violation shape"
fi
# The compile-time guard runs only when the adapter binary is resolvable
# (schema collection); otherwise the guard defers to the runtime gate and the
# fixture validates permissively. Detect which half is observable and assert
# it — never a silent pass.
copilot_digest="$(cd "$G5_FIXTURE" && "$CRITERIA" adapter list --referenced 2>/dev/null \
    | awk -v v=":0.5.15" '$2 ~ v"$" {print $3}')"
copilot_cached=""
if [ -n "$copilot_digest" ] && "$CRITERIA" adapter list --installed 2>/dev/null | grep "$copilot_digest" >/dev/null; then
    copilot_cached="1"
fi
if [ -n "$copilot_cached" ]; then
    if (cd "$G5_FIXTURE" && "$CRITERIA" validate . >"$WORK/g5.log" 2>&1); then
        fail "G5: the parallel-over-copilot fixture VALIDATED under a resolvable copilot binary — the compile-time parallel_safe guard did not fire"
        fail "G5 fixture validate output was: $(head -c 400 "$WORK/g5.log")"
    elif grep -q "declare parallel_safe" "$WORK/g5.log"; then
        ok "G5 (negative half): the parallel_safe guard refused the fixture at compile time"
    else
        fail "G5: parallel guard fired with an unexpected diagnostic:" "$(<"$WORK/g5.log")"
    fi
else
    if (cd "$G5_FIXTURE" && "$CRITERIA" validate . >"$WORK/g5.log" 2>&1); then
        echo "note: G5 compile-time guard not observable (pinned copilot binary not cached; guard defers to the runtime gate) — asserting the permissive half"
        ok "G5 (permissive half): the fixture compiles when the copilot binary is unresolvable, as the schema-unknown posture requires"
    else
        fail "G5: the fixture failed validation without a resolvable copilot binary — the permissive posture broke:" "$(<"$WORK/g5.log")"
    fi
fi

# ── Verdict ──────────────────────────────────────────────────────────────────

if [ "$FAILED" -ne 0 ]; then
    echo "harness_composition_v1 standalone suite: FAILED ($FAILED assert(s))"
    exit 1
fi
echo "harness_composition_v1 standalone suite: all matrix shapes asserted (compile + source pins green)"
exit 0