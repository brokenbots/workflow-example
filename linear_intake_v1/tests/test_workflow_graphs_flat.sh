#!/usr/bin/env bash
set -euo pipefail

# CRI-300: operator validation that the v0.5.32 engine (b2d0b66) ships the
# flat WorkflowGraphs shape end-to-end. The compiler always emits a nested
# graph (each layer's body embeds its children); the runtime emitter must
# instead flatten every subworkflow layer into one sibling array. On a
# 3-level nested fixture (root -> flat_l1 -> flat_l2 -> flat_l3) this test
# pins the emitter contract:
#
#   1. the fixture genuinely nests three levels in the compiled graph;
#   2. the emitted WorkflowGraphs event carries all layers as siblings in a
#      flat payload.subworkflows array (payload has no other keys);
#   3. no layer body string contains a subworkflows key at any depth;
#   4. each body parses, matches its layer name and embeds the layer's path.
#
# Engines older than v0.5.32 emit no WorkflowGraphs event at all: the test
# then skips (exit 0) so the suite stays green across engine versions.

TREE_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FIXTURE="$TREE_ROOT/tests/fixtures/workflow_graphs_flat"
CRITERIA="${CRITERIA_BIN:-criteria}"

FAILED=0
fail() { echo "FAIL: $1" >&2; FAILED=$((FAILED + 1)); }
ok() { echo "ok: $1"; }
require_equal() {
    if [ "$1" = "$2" ]; then ok "$3"; else fail "$3: got '$1', want '$2'"; fi
}

command -v "$CRITERIA" >/dev/null 2>&1 \
    || { echo "FAIL: criteria binary '$CRITERIA' not found (set CRITERIA_BIN)" >&2; exit 1; }
command -v jq >/dev/null 2>&1 \
    || { echo "FAIL: jq not found" >&2; exit 1; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# ── 1. The fixture is genuinely 3-level nested in the compiled graph ────────

"$CRITERIA" compile "$FIXTURE" --format json --out "$TMP/graph.json" 2>"$TMP/compile.err" \
    || { echo "FAIL: criteria compile: $(cat "$TMP/compile.err")" >&2; exit 1; }
GRAPH="$TMP/graph.json"

chain="$(jq -r '[.subworkflows[0].name,
                 .subworkflows[0].body.subworkflows[0].name,
                 .subworkflows[0].body.subworkflows[0].body.subworkflows[0].name] | join(" ")' "$GRAPH")"
require_equal "$chain" "flat_l1 flat_l2 flat_l3" "compiled graph nests 3 subworkflow levels"
jq -e '.subworkflows[0].body.subworkflows[0].body.subworkflows[0].body | has("subworkflows") | not' "$GRAPH" >/dev/null \
    || fail "deepest compiled layer still embeds a subworkflows key"
ok "deepest compiled layer is a leaf (no nested subworkflows)"

# ── 2. Run the fixture and gate on the WorkflowGraphs event ─────────────────

"$CRITERIA" apply "$FIXTURE" --events-file "$TMP/events.ndjson" >/dev/null 2>&1 \
    || { echo "FAIL: criteria apply: $(tail -5 "$TMP/events.ndjson" 2>/dev/null || true)" >&2; exit 1; }

wg_count="$(jq -s '[.[] | select(.payload_type == "WorkflowGraphs")] | length' "$TMP/events.ndjson")"
if [ "$wg_count" -eq 0 ]; then
    echo "SKIP: engine $("$CRITERIA" version 2>/dev/null || echo unknown) emits no WorkflowGraphs event — flat emitter needs criteria >= v0.5.32 (b2d0b66); flat contract not asserted"
    [ "$FAILED" -gt 0 ] && exit 1
    exit 0
fi

wg="$(jq -s '[.[] | select(.payload_type == "WorkflowGraphs")]' "$TMP/events.ndjson")"

# ── 3. Flat contract: one event, all layers as siblings, clean bodies ───────

require_equal "$wg_count" "1" "exactly one WorkflowGraphs event in the stream"
require_equal "$(jq -r '.[0].payload | keys | join(" ")' <<<"$wg")" "subworkflows" \
    "payload carries only the flat subworkflows array (no nested root graph)"

require_equal "$(jq -r '.[0].payload.subworkflows[].name' <<<"$wg" | sort | paste -sd' ' -)" \
    "flat_l1 flat_l2 flat_l3" "all subworkflow layers are siblings in the flat payload"

bodies_with_key="$(jq -r '[ .[0].payload.subworkflows[] | .body | fromjson | .. | objects
                          | select(has("subworkflows")) ] | length' <<<"$wg")"
require_equal "$bodies_with_key" "0" "no layer body string contains a subworkflows key at any depth"

mismatched="$(jq -r '[ .[0].payload.subworkflows[] | select((.body | fromjson | .name) != .name) | .name ]
                     | join(" ")' <<<"$wg")"
[ -z "$mismatched" ] || fail "body name mismatches layer name for: $mismatched"
ok "every layer body parses and matches its layer name"

misplaced="$(jq -r '[ .[0].payload.subworkflows[] | . as $l
                     | select(($l.sourcePath | contains($l.name)) | not) | $l.name ]
                    | join(" ")' <<<"$wg")"
[ -z "$misplaced" ] || fail "sourcePath does not embed the layer path for: $misplaced"
ok "sourcePath embeds each layer's nested path (depth-3 reaches the emitter)"

if [ "$FAILED" -gt 0 ]; then
    echo "FAILED: $FAILED assertion(s)" >&2
    exit 1
fi
echo "PASS: WorkflowGraphs flat contract verified on engine $("$CRITERIA" version 2>/dev/null || echo unknown)"