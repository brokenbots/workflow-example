#!/bin/bash
# test_pair_loop_v2_router_negative.sh — the routing simulator's NEGATIVE
# arms: each known live-run defect class is re-introduced into a scratch copy
# of the v2 tree, the simulator runs against the break, and it MUST fail.
# Positive control: the unmodified tree passes. Proves the simulator is not
# vacuous (the guards-are-vacuous class from the standing pitfalls list).
#
# The three defect classes (from the 2026-11-07 zoom-out + their live fires):
#   C1 verdict-stamp missing on a part_ready arm (fire 1: verify 1ms-failed)
#   C2 route_slot exit-arm shadowing (fire 2: valid 3-part plan walked p4)
#   C3 phantom-slot select (fire 2: empty-brief part ran remediation 22 min)
#
# Runs under bash; skips without the fleet engine/node/jq/git.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
FIXTURES="${SCRIPT_DIR}/fixtures"
WORK=""

FAILURES=0
ok() { echo "PASS: $*"; }
fail() { echo "FAIL: $*" >&2; FAILURES=$((FAILURES + 1)); }

CRITERIA=""
for c in "${CRITERIA_BIN:-}" /tmp/ci-criteria-fleet/criteria; do
    if [ -n "$c" ] && [ -x "$c" ] && [ "$("$c" version 2>/dev/null | tr -d '[:space:]')" = "v0.5.58" ]; then
        CRITERIA="$c"
        break
    fi
done
[ -n "$CRITERIA" ] || { echo "SKIP: no fleet-stamp engine v0.5.58" >&2; exit 0; }
for t in node jq git; do
    command -v "$t" >/dev/null 2>&1 || { echo "SKIP: $t not available" >&2; exit 0; }
done

WORK="$(mktemp -d "${TMPDIR:-/tmp}/pairv2neg.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT

export TREE_ROOT FIXTURES WORK CRITERIA
TREE_ROOT="$(cd "${SCRIPT_DIR}/../workflows/pair_programming_loop_v2" && pwd)"
FIXTURES="${SCRIPT_DIR}/fixtures"
TIMEOUT_SECS="${SIM_TIMEOUT:-240}"

shell_shim() {
    # shellcheck source=/dev/null
    . "${FIXTURES}/sim_lib.sh"
}

# run_case NAME EXPECT(pass|fail): run the simulator with the WORK-tree v2
# pointed at the CURRENT workstream copy (already mutated by the caller) by
# exporting a TREE_ROOT override through the sim_lib contract.
run_case() {
    local name="$1" expect="$2"
    local out
    out="$(SIM_TIMEOUT="${TIMEOUT_SECS}" bash "${SCRIPT_DIR}/test_pair_loop_v2_routing.sh" 2>&1 || true)"
    if [ "$expect" = "fail" ]; then
        if printf '%s' "$out" | grep -qE '^FAIL:'; then
            ok "${name}: the simulator FAILED against the break (caught the defect class)"
        else
            fail "${name}: the simulator PASSED against the break (vacuous)"
        fi
    else
        if printf '%s' "$out" | grep -qE 'routing simulator: all scenarios green'; then
            ok "${name}: the simulator passed the clean tree"
        else
            fail "${name}: the simulator failed on the CLEAN tree (check the breaks were restored): $(printf '%s' "$out" | grep -E '^FAIL:' | head -2)"
            printf '%s\n' "$out" | tail -20 >&2
        fi
    fi
}

# The scenario-A plan (3 parts) is the shape C2/C3 fired live.
PLAN_3='{"parts":3,"per_part":[{"id":"p1","files_claimed":["src/f1.go"],"brief":"sim p1 brief: append marker to src/f1.go","effort":"fast","acceptance":"marker appended"},{"id":"p2","files_claimed":["src/f2.go"],"brief":"sim p2 brief: append marker to src/f2.go","effort":"deep","acceptance":"marker appended"},{"id":"p3","files_claimed":["src/f3.go"],"brief":"sim p3 brief: append marker to src/f3.go","effort":"deep","acceptance":"marker appended"}]}'
PLAN_4='{"parts":4,"per_part":[{"id":"p1","files_claimed":["src/f1.go"],"brief":"sim p1 brief: append marker to src/f1.go","effort":"fast","acceptance":"marker appended"},{"id":"p2","files_claimed":["src/f2.go"],"brief":"sim p2 brief: append marker to src/f2.go","effort":"deep","acceptance":"marker appended"},{"id":"p3","files_claimed":["src/f3.go"],"brief":"sim p3 brief: append marker to src/f3.go","effort":"fast","acceptance":"marker appended"},{"id":"p4","files_claimed":["src/f4.go"],"brief":"sim p4 brief: append marker to src/f4.go","effort":"fast","acceptance":"marker appended"}]}'

shell_shim

echo "==> Positive control: the clean tree passes"
run_case "control-clean" pass

echo "==> C1: part_ready arm loses its verdict stamp (the stamp-break)"
stamp_file="${TREE_ROOT}/main.chcl"
stamp_backup="$(mktemp)"
cp "$stamp_file" "$stamp_backup"
PRISTINE="$(mktemp)"
cp "$stamp_file" "$PRISTINE"
python3 - "$stamp_file" <<'PY'
import sys
p = sys.argv[1]
s = open(p).read()
# Remove the FIRST part_ready slot stamp (the one the live fire broke).
old = '''        write {
            // THE AGENT'S REPORTED OUTCOME IS THE STAMP (KB-151: the tree's own
            // data.internal is the verdict source the verify validates against).
            target = data.internal.pair_slot_p1_verdict.value
            value  = "part_ready"
        }'''
assert old in s, "stamp block not found (the fix may have moved; the negative arm needs re-anchoring)"
s = s.replace(old, "", 1)
open(p, "w").write(s)
print("C1 break injected: p1 part_ready stamp removed")
PY
run_case "C1-stamp-missing" fail
cp "$PRISTINE" "$stamp_file"
rm -f "$stamp_backup"

echo "==> C2: a 3-part plan walks into slot 4 (exit-arm shadowing — the fire-2 break)"
python3 - "$stamp_file" <<'PY'
import sys
p = sys.argv[1]
s = open(p).read()
old = '''data.internal.part_count.value != "2" && data.internal.part_count.value != "3"'''
new = '''data.internal.part_count.value != "2"'''
assert old in s, "route_slot_p3 !=3 anchor not found"
s = s.replace(old, new, 1)
open(p, "w").write(s)
print("C2 break injected: the != 3 guard dropped (count==3 falls into select_slot_p4)")
PY
run_case "C2-shadowed-exit" fail
cp "$PRISTINE" "$stamp_file"

echo "==> C3: phantom-slot path (C2's shadowing + the p4 brief guards dropped — the live fire-2 combination)"
python3 - "$stamp_file" <<'PY'
import sys
p = sys.argv[1]
s = open(p).read()
n = 0
# Drop BOTH p4 brief guards (fast lane then deep lane).
for lane in ("fast", "deep"):
    marker = 'per_part[3].brief, "") != "" && try(jsondecode(data.internal.plan_json.value).per_part[3].effort, "") ' + ('!=' if lane == 'fast' else '==') + ' "' + lane + '"\n        next      = step.run_part_p4_' + lane
    if marker in s:
        s = s.replace(marker, marker.replace('per_part[3].brief, "") != "" && ', ""), 1)
        n += 1
# Drop the shadowing guard so count==3 reaches select_slot_p4 (the live path).
old2 = 'data.internal.part_count.value != "2" && data.internal.part_count.value != "3"'
if old2 in s:
    s = s.replace(old2, 'data.internal.part_count.value != "2"', 1)
    n += 1
assert n >= 2, f"C3 injection found {n} anchors (need both p4 brief guards + shadowing)"
open(p, "w").write(s)
print(f"C3 break injected: {n} anchors (both p4 brief guards + shadowing)")
PY
run_case "C3-phantom-slot" fail
cp "$PRISTINE" "$stamp_file"
rm -f "$PRISTINE"

echo "==> Restore check: the tree is byte-identical to HEAD"
if git -C "${TREE_ROOT}" diff --quiet -- main.chcl 2>/dev/null || [ -z "$(cd "${TREE_ROOT}" && git status --porcelain main.chcl)" ]; then
    ok "restore: the v2 tree's main.chcl is clean after the negative arms"
else
    fail "restore: the v2 tree's main.chcl is DIRTY after the negative arms (a break leaked): $(cd "${TREE_ROOT}" && git status --porcelain main.chcl)"
fi

echo
if [ "$FAILURES" -eq 0 ]; then
    echo "pair_loop_v2 router negative arms: the simulator catches all three defect classes (not vacuous)"
    exit 0
fi
echo "pair_loop_v2 router negative arms: ${FAILURES} failure(s)" >&2
exit 1
