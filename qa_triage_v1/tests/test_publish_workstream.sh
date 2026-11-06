#!/usr/bin/env bash
set -euo pipefail

# qa_triage_v1 publish_workstream (KB-355 round 2): the published workstream
# must carry exactly one canonical scope-generation marker derived from the
# ticket scope, and stamp-or-preserve it across re-publishes — the
# delivery-fingerprint classifier uses the marker to suppress a rewritten
# ticket's predecessor-scope merged PRs (live: castle #54 re-closed card 322).
#
# Pinned:
#   1. fresh publish stamps the canonical sha256(title+"\n"+desc) digest;
#   2. unchanged-scope re-publish preserves the stamp byte-identically;
#   3. changed-scope publish over a marked file advances hash and stamp;
#   4. standalone (no scope source) publishes without a marker (legacy);
#   5. marker-shaped lines in the agent draft (with a forged 2099 stamp) are
#      stripped and can never arm or preserve a generation.

TREE_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
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

# ── validate standalone, as every workflow tree must ─────────────────────────
if "$CRITERIA" validate "$TREE_ROOT" >/dev/null 2>&1; then
    ok "qa_triage_v1 validates standalone (compile-time resolvable inputs)"
else
    fail "criteria validate qa_triage_v1"
fi

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

run_dir="$TMP/run"
out_dir="$run_dir/streams"
out="$out_dir/KB-77.md"
mkdir -p "$run_dir/evidence" "$run_dir/report" "$TMP/qa/intake/KB-77"

printf '# Workstream: approved draft body\n\n## Task description\n\ndraft reproduction\n' > "$run_dir/evidence/workstream-draft.md"

render() { # $1 = scope_ticket_json ('' for standalone)
    sed \
        -e "s@^criteria_value_1=.*@criteria_value_1='$run_dir'@" \
        -e "s@^criteria_value_2=.*@criteria_value_2='$out_dir'@" \
        -e "s@^criteria_value_3=.*@criteria_value_3='$run_dir/report/KB-77.md'@" \
        -e "s@^scope_ticket_json=.*@scope_ticket_json='$1'@" \
        "$TREE_ROOT/scripts/publish_workstream.sh.tftpl" > "$TMP/publish.sh"
    chmod +x "$TMP/publish.sh"
}

scope_write() { # $1 title, $2 description
    jq -n --arg t "$1" --arg d "$2" \
        '{task: {id: 77, title: $t, description: $d, column_id: 2}, tags: [], comments: []}' \
        > "$TMP/qa/intake/KB-77/ticket.json"
}

marker_of() { # $1 file -> anchored canonical marker line (empty = none)
    awk '/^## /{exit} {print}' "$1" 2>/dev/null \
        | grep -E -m1 '^<!-- scope-generation: [0-9a-f]{12} stamped: [0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z -->$' \
        || true
}

marker_hash() { # $1 file -> digest from the anchored marker
    printf '%s' "$(marker_of "$1")" | sed -n 's/.*scope-generation: \([0-9a-f]\{12\}\) stamped:.*/\1/p'
}

marker_stamp() { # $1 file -> stamp from the anchored marker
    printf '%s' "$(marker_of "$1")" | sed -n 's/.*stamped: \([0-9T:-]*Z\).*/\1/p'
}

scope_json="$TMP/qa/intake/KB-77/ticket.json"

# ── 1. fresh publish stamps the canonical digest ─────────────────────────────
scope_write "gate races teardown" "reproduction text for the fresh publish"
render "$scope_json"
if pub_out="$("$TMP/publish.sh")"; then
    require_equal "$pub_out" "$out" "publisher prints the published path stdout"
    if grep -Eq '^<!-- scope-generation: [0-9a-f]{12} stamped: [0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z -->$' "$out"; then
        ok "fresh publish stamps a scope-generation marker"
    else
        fail "fresh publish must carry '<!-- scope-generation: <hash> stamped: <RFC3339Z> -->' in the header region"
    fi
    scope_hash=$(printf '%s\n%s' "gate races teardown" "reproduction text for the fresh publish" | sha256sum | cut -c1-12)
    require_equal "$(marker_hash "$out")" "$scope_hash" "published hash is the canonical scope digest (title + description)"
    if grep -q '^# Workstream: approved draft body' "$out" \
        && [ "$(grep -cE '^<!-- scope-generation: [0-9a-f]{12} stamped: [0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z -->$' "$out" || true)" = "1" ] \
        && [ "$(awk '/^## /{exit} /^# Workstream/{print NR}' "$out")" = "1" ] \
        && [ "$(grep -n '^<!-- scope-generation' "$out" | cut -d: -f1 || true)" = "2" ]; then
        ok "marker sits in the header region right after the title heading; exactly one"
    else
        fail "marker must appear exactly once in the header region right after the title heading"
    fi
else
    fail "fresh publish failed"
fi

# ── 2. unchanged-scope re-publish preserves the stamp ────────────────────────
# Deterministic probe: seed the marker with the CANONICAL hash plus a fixed
# 2020 stamp, then re-publish. A publisher that re-stamps on every publish
# would move the stamp off 2020; a correct one carries 2020 forward.
scope_hash=$(printf '%s\n%s' "gate races teardown" "reproduction text for the fresh publish" | sha256sum | cut -c1-12)
printf '# Workstream: approved draft body\n\n<!-- scope-generation: %s stamped: 2020-01-01T00:00:00Z -->\n\n## Task description\n\ndraft reproduction\n' "$scope_hash" > "$out"
before_others="$(grep -v 'scope-generation' "$out" | md5sum | cut -d' ' -f1)"
if "$TMP/publish.sh" >/dev/null 2>&1 \
    && [ "$(marker_hash "$out")" = "$scope_hash" ] \
    && [ "$(marker_stamp "$out")" = "2020-01-01T00:00:00Z" ] \
    && [ "$(grep -v 'scope-generation' "$out" | md5sum | cut -d' ' -f1)" = "$before_others" ]; then
    ok "unchanged-scope re-publish preserves the seeded 2020 stamp (and everything else)"
else
    fail "unchanged-scope re-publish must preserve the seeded 2020 stamp (deterministic)"
fi

# ── 3. changed scope advances hash and stamp ─────────────────────────────────
scope_write "gate races teardown" "rewritten in place: new scope text"
if "$TMP/publish.sh" >/dev/null 2>&1; then
    new_scope_hash=$(printf '%s\n%s' "gate races teardown" "rewritten in place: new scope text" | sha256sum | cut -c1-12)
    require_equal "$(marker_hash "$out")" "$new_scope_hash" "changed scope advances the marker hash"
    if [ "$(marker_stamp "$out")" != "2020-01-01T00:00:00Z" ]; then
        ok "changed scope advances the marker stamp (past the seeded 2020 stamp)"
    else
        fail "changed scope must advance the marker stamp"
    fi
else
    fail "changed-scope publish failed"
fi

# ── 4. draft-borne marker-shaped lines are stripped ──────────────────────────
scope_write "draft forgery guard" "clean scope for the strip test"
printf '# Workstream: agent draft\n\n<!-- scope-generation: aaaaaaaaaaaa stamped: 2099-01-01T00:00:00Z -->\n\n## Task description\n\nbody\n' > "$run_dir/evidence/workstream-draft.md"
scope_hash_clean=$(printf '%s\n%s' "draft forgery guard" "clean scope for the strip test" | sha256sum | cut -c1-12)
if "$TMP/publish.sh" >/dev/null 2>&1 \
    && ! grep -qF 'aaaaaaaaaaaa' "$out" \
    && grep -qF "scope-generation: $scope_hash_clean stamped:" "$out" \
    && [ "$(grep -cE '^<!-- scope-generation: [0-9a-f]{12} stamped: [0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z -->$' "$out" || true)" = "1" ]; then
    ok "draft-borne forged marker line is stripped; exactly one canonical marker stands"
else
    fail "published file must drop the draft forgery and carry exactly one canonical marker"
fi

# ── 5. no scope source (standalone): legacy publish without a marker ─────────
rm -f "$out"
render ""
if "$TMP/publish.sh" >/dev/null 2>&1 \
    && [ ! -s "$out" ]; then
    fail "standalone publish must still publish the draft (got an empty file)"
elif [ "$(grep -c 'scope-generation' "$out" || true)" = "0" ] \
    && grep -q '^# Workstream: agent draft' "$out"; then
    ok "standalone publish (no scope source) carries no marker (legacy degradation)"
else
    fail "standalone publish must write the draft without arming a guard"
fi

# stdout stays the bare published path: the parent parses it with startswith.
clean_out="$("$TMP/publish.sh" 2>/dev/null)"
require_equal "$clean_out" "$out" "standalone publish stdout is still the bare published path"

if [ "$FAILED" -gt 0 ]; then
    echo "FAILED: $FAILED assertion(s)" >&2
    exit 1
fi
echo "all qa_triage publish_workstream tests passed"
