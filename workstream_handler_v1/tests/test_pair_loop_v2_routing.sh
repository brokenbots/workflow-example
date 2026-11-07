#!/bin/bash
# test_pair_loop_v2_routing.sh — the pair_programming_loop_v2 ROUTING
# simulator: the standing gate's middle tier. The REAL v2 tree runs WHOLE on
# the fleet-stamp engine with its copilot adapters shimmed to a scripted
# provider stub (KB-277's proven pattern) and its remote worktree env shimmed
# to a local git repo; the network dial is neutralized and LOCAL-ONLY is
# asserted over every executed command.
#
# Routing properties (the live-run defect classes as permanent arms live in
# the router gate suite — test_pair_loop_v2_router.gate.sh):
#   R1 3-part plan -> exactly p1..p3, no phantom p4, checkpoint, pair_ready, done
#   R2 1-part plan -> exactly p1, done
#   R3 4-part mixed effort -> per-part fast/deep lane selection observed
#   R4 every slot's verdict stamp -> deterministic verify passes on first try
#
# Skips (never fails) when the fleet engine / node / jq / git are absent.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
TREE_ROOT="$(cd "${SCRIPT_DIR}/../workflows/pair_programming_loop_v2" && pwd)"
FIXTURES="${SCRIPT_DIR}/fixtures"

FAILURES=0
ok() { echo "PASS: $*"; }
fail() { echo "FAIL: $*" >&2; FAILURES=$((FAILURES + 1)); }

# ── Preconditions ─────────────────────────────────────────────────────────────
CRITERIA=""
for c in "${CRITERIA_BIN:-}" /tmp/ci-criteria-fleet/criteria; do
    if [ -n "$c" ] && [ -x "$c" ] && [ "$("$c" version 2>/dev/null | tr -d '[:space:]')" = "v0.5.58" ]; then
        CRITERIA="$c"
        break
    fi
done
if [ -z "$CRITERIA" ]; then
    echo "SKIP: no fleet-stamp engine v0.5.58 (set CRITERIA_BIN or extract /tmp/ci-criteria-fleet/criteria)" >&2
    exit 0
fi
for t in node jq git; do
    command -v "$t" >/dev/null 2>&1 || { echo "SKIP: $t not available" >&2; exit 0; }
done

WORK="$(mktemp -d "${TMPDIR:-/tmp}/pairv2-sim.XXXXXX")"
STUB_PIDS=""
RC=0
EV=""
cleanup() {
    # shellcheck disable=SC2086 disable=SC2154
    for pid in ${STUB_PIDS:-}; do
        kill "$pid" 2>/dev/null || true
    done
    :
}
trap cleanup EXIT

TIMEOUT_SECS="${SIM_TIMEOUT:-240}"
export TREE_ROOT FIXTURES WORK CRITERIA TIMEOUT_SECS

# shellcheck source=/dev/null
. "${FIXTURES}/sim_lib.sh"

# ── Plans ──────────────────────────────────────────────────────────────────────
PLAN_A='{"parts":3,"per_part":[{"id":"p1","files_claimed":["src/f1.go"],"brief":"sim p1 brief: append marker to src/f1.go","effort":"fast","acceptance":"marker appended"},{"id":"p2","files_claimed":["src/f2.go"],"brief":"sim p2 brief: append marker to src/f2.go","effort":"deep","acceptance":"marker appended"},{"id":"p3","files_claimed":["src/f3.go"],"brief":"sim p3 brief: append marker to src/f3.go","effort":"deep","acceptance":"marker appended"}]}'
PLAN_B='{"parts":1,"per_part":[{"id":"p1","files_claimed":["src/f1.go"],"brief":"sim p1 brief: append marker to src/f1.go","effort":"fast","acceptance":"marker appended"}]}'
PLAN_C='{"parts":4,"per_part":[{"id":"p1","files_claimed":["src/f1.go"],"brief":"sim p1 brief: append marker to src/f1.go","effort":"fast","acceptance":"marker appended"},{"id":"p2","files_claimed":["src/f2.go"],"brief":"sim p2 brief: append marker to src/f2.go","effort":"deep","acceptance":"marker appended"},{"id":"p3","files_claimed":["src/f3.go"],"brief":"sim p3 brief: append marker to src/f3.go","effort":"fast","acceptance":"marker appended"},{"id":"p4","files_claimed":["src/f4.go"],"brief":"sim p4 brief: append marker to src/f4.go","effort":"fast","acceptance":"marker appended"}]}'

# ── Scenario A: 3 parts (the phantom-slot class's live shape) ─────────────────
echo "==> Scenario A: 3-part plan"
run_pair_v2 planA "$PLAN_A"
assert_step_outcome planA plan_parts parts_ready
assert_slot_chain_exact planA 3
assert_no_step_entered planA run_part_p4_fast
assert_no_step_entered planA run_part_p4_deep
assert_no_step_entered planA pair_part_p4_remediate
assert_step_outcome planA push_checkpoint success
assert_pair_verdict planA pair_ready
assert_terminal planA RunCompleted done
assert_no_network_ops planA

# ── Scenario B: 1 part ────────────────────────────────────────────────────────
echo "==> Scenario B: 1-part plan"
run_pair_v2 planB "$PLAN_B"
assert_step_outcome planB plan_parts parts_ready
assert_slot_chain_exact planB 1
assert_no_step_entered planB run_part_p2_fast
assert_step_outcome planB push_checkpoint success
assert_terminal planB RunCompleted done
assert_no_network_ops planB

# ── Scenario C: 4 parts mixed effort ──────────────────────────────────────────
echo "==> Scenario C: 4-part mixed effort"
run_pair_v2 planC "$PLAN_C"
assert_slot_chain_exact planC 4
assert_slot_lane planC 1 fast
assert_slot_lane planC 2 deep
assert_slot_lane planC 3 fast
assert_slot_lane planC 4 fast
assert_step_outcome planC push_checkpoint success
assert_terminal planC RunCompleted done
assert_no_network_ops planC

echo
if [ "$FAILURES" -eq 0 ]; then
    echo "pair_loop_v2 routing simulator: all scenarios green (plans 3/1/4; lanes; stamps->verify; checkpoint; aggregate; LOCAL-ONLY)"
    exit 0
fi
echo "pair_loop_v2 routing simulator: ${FAILURES} failure(s)" >&2
exit 1
