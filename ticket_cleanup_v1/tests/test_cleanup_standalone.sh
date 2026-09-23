#!/usr/bin/env bash
set -euo pipefail

# CRI-310: ticket_cleanup_v1 is the dirty-label cleanup workflow (carrier for
# CRI-246, plan CRI-214 M10.4, validation run C). Fired through the
# criteria-cleanup route on a ticket carrying the sticky "criteria-dirty"
# label, it proves the dirty label is a routing surface. This test guards the
# tree:
#
#   1. the tree validates standalone and compiles;
#   2. the graph is exactly fetch_ticket -> post_cleanup_comment -> done,
#      with both failure outcomes ending in the failed terminal (CRI-275:
#      the run must not lie about its evidence) and no subworkflows;
#   3. it is secret-free beyond the Linear API key — no GitHub tokens, no
#      repo operations, no references into the sibling trees;
#   4. the scripts are deterministic on the real ticket.json shape
#      (the enveloped GraphQL response): fetch validates the ticket id,
#      stores the ticket, and the comment step posts the exact evidence
#      comment "cleanup pass: dirty label verified as a routing surface
#      (validation run C)" with the issue UUID read through .data.issue.id;
#   5. linear_triage_v1 and linear_develop_v1 are untouched (the D7 pins for
#      those subtrees stay valid).

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
ok "criteria validate passes standalone"

# The tree contributes zero validation warnings.
if grep '^Warning:' "$TMP/validate.err" | grep "ticket_cleanup_v1" >/dev/null 2>&1; then
    grep '^Warning:' "$TMP/validate.err" | grep "ticket_cleanup_v1" >&2
    fail "validation reports warnings originating in the cleanup tree"
else
    ok "cleanup tree contributes no validation warnings"
fi

# ── 2. Graph structure ───────────────────────────────────────────────────────

"$CRITERIA" compile "$TREE_ROOT" --format json --out "$TMP/graph.json" 2>"$TMP/compile.err" \
    || { echo "FAIL: criteria compile: $(cat "$TMP/compile.err")" >&2; exit 1; }
GRAPH="$TMP/graph.json"

# Exactly two terminals: the success terminal (the evidence comment landed)
# and the failed terminal (fetch or comment failed — the watcher re-raises
# the dirty label for a human).
terminal_names="$(jq -r '[.states[] | select(.terminal) | .name] | sort | join(" ")' "$GRAPH")"
require_equal "$terminal_names" "done failed" "terminal states are exactly done and failed"

jq -e '.states[] | select(.terminal and .name == "done" and .success)' "$GRAPH" >/dev/null \
    || fail "terminal done must be success=true"
jq -e '.states[] | select(.terminal and .name == "failed" and (.success | not))' "$GRAPH" >/dev/null \
    || fail "terminal failed must be success=false: evidence failures must be recorded as run failures"
ok "terminals carry the right success flags"

# No subworkflows, no routing switches: a single linear shell flow.
subwf_count="$(jq -r '(.subworkflows // []) | length' "$GRAPH")"
[ "$subwf_count" -eq 0 ] || fail "expected 0 subworkflows, got $subwf_count"
switch_count="$(jq -r '(.switches // []) | length' "$GRAPH")"
[ "$switch_count" -eq 0 ] || fail "expected 0 switches (no routing gates), got $switch_count"
ok "no subworkflows and no switches"

# The step set is exactly the cleanup path.
step_names="$(jq -r '[.steps[] | .name] | sort | join(" ")' "$GRAPH")"
require_equal "$step_names" "fetch_ticket post_cleanup_comment" "step set is exactly the cleanup path"

assert_edge() {
    local from="$1" outcome="$2" to="$3"
    local got
    got="$(jq -r --arg from "$from" --arg outcome "$outcome" \
        '.steps[] | select(.name == $from) | .outcomes[] | select(.name == $outcome) | .next' "$GRAPH")"
    require_equal "$got" "$to" "edge $from.$outcome -> $to"
}

assert_edge "fetch_ticket" "success" "post_cleanup_comment"
assert_edge "fetch_ticket" "failure" "failed"
assert_edge "post_cleanup_comment" "success" "done"
assert_edge "post_cleanup_comment" "failure" "failed"

# Reachability from initial_state: both steps and both terminals reachable.
initial="$(jq -r '.initial_state' "$GRAPH")"
[ "$initial" = "fetch_ticket" ] || fail "initial_state must be fetch_ticket, got $initial"

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
for node in fetch_ticket post_cleanup_comment done failed; do
    [ "${reachable[$node]+set}" = "set" ] || fail "node not reachable from initial_state: $node"
done
ok "every step and terminal is reachable from initial_state"

# ── 3. Minimal, secret-free shape ────────────────────────────────────────────

# Single shell adapter, pinned to the locked version, no environment block.
adapter_blocks="$(grep -c '^adapter "shell"' "$TREE_ROOT/main.chcl")"
[ "$adapter_blocks" -eq 1 ] || fail "expected exactly 1 shell adapter, got $adapter_blocks"
if grep -q 'source[[:space:]]* = "ghcr.io/brokenbots/criteria-adapter-shell"' "$TREE_ROOT/main.chcl" \
    && grep -q 'version[[:space:]]* = "0.5.3"' "$TREE_ROOT/main.chcl"; then
    ok "single shell adapter is criteria-adapter-shell:0.5.3"
else
    fail "adapter must pin ghcr.io/brokenbots/criteria-adapter-shell:0.5.3"
fi

# No environment block (the child-fixture minimal shape: everything happens
# in the process working directory under intake_root).
if grep -Eq '^environment ' "$TREE_ROOT/main.chcl"; then
    fail "cleanup tree must not declare an environment block (minimal tree)"
else
    ok "no environment block"
fi

# The only secret is the Linear API key — the cleanup pass does zero git
# operations, so no GitHub tokens are bound (plan section 3.8: secrets via
# the existing SecretProviderClass references only).
secret_vars="$(grep -c 'secret[[:space:]]* = true' "$TREE_ROOT/main.chcl")"
[ "$secret_vars" -eq 1 ] || fail "expected exactly 1 secret variable (linear_api_key), got $secret_vars"
grep -q 'variable "linear_api_key"' "$TREE_ROOT/main.chcl" \
    || fail "linear_api_key secret variable missing"
if grep -Eq 'github_token|GITHUB_TOKEN' "$TREE_ROOT/main.chcl"; then
    fail "cleanup tree must not reference GitHub tokens"
else
    ok "no GitHub tokens in the cleanup tree"
fi
ok "exactly one secret variable: linear_api_key"

# No repo operations and no references into the sibling trees: the cleanup
# pass only reads the ticket and posts a comment.
if grep -rnE '(^|[^[:alnum:]_])git([^[:alnum:]_-]|$)' "$TREE_ROOT/scripts" >/dev/null 2>&1; then
    grep -rnE '(^|[^[:alnum:]_])git([^[:alnum:]_-]|$)' "$TREE_ROOT/scripts" >&2
    fail "cleanup scripts must not run git operations"
else
    ok "no git operations in the cleanup scripts"
fi
if grep -rn -e "linear_intake_v1/" -e "linear_triage_v1/" -e "linear_develop_v1/" \
    -e "workstream_handler_v1/" \
    "$TREE_ROOT/main.chcl" "$TREE_ROOT/scripts" >/dev/null 2>&1; then
    grep -rn -e "linear_intake_v1/" -e "linear_triage_v1/" -e "linear_develop_v1/" \
        -e "workstream_handler_v1/" \
        "$TREE_ROOT/main.chcl" "$TREE_ROOT/scripts" >&2
    fail "cleanup tree references a sibling workflow's files — trees must be independently runnable"
else
    ok "no file path into the sibling trees"
fi

# The evidence comment body is the exact string the validation run C
# shepherd greps the ticket for.
grep -q 'cleanup pass: dirty label verified as a routing surface (validation run C)' \
    "$TREE_ROOT/scripts/post_cleanup_comment.sh.tftpl" \
    || fail "post_cleanup_comment must post the exact evidence comment body"

# ── 4. Script behavior on the real ticket.json shape ─────────────────────────

# Rendering mimics templatefile for the variables the scripts consume,
# including the engine's shellquote (single-quote wrapping with '\'' escaping).
SLUG="CRI-310"
shquote() {
    printf "'%s'" "${1//\'/\'\\\'\'}"
}
render() {
    sed -e "s@{{ .intake_root | shellquote }}@$(shquote "$1")@g" \
        -e "s@{{ .ticket_id | shellquote }}@$(shquote "$SLUG")@g" "$2" > "$3"
}

run_dir="$TMP/intake/$SLUG"

# Stub wget: records every --post-data body to $CURL_STUB_LOG, serves the
# canned ticket response for the issue query, and the canned commentCreate
# response for the mutation — the same canned behavior the original curl
# stub provided, over the base image's actual HTTP client (validation run C
# corrected the script toolset from curl+jq to BusyBox wget).
mkdir -p "$TMP/stub"
cat > "$TMP/stub/wget" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
body=""
out="/dev/stdout"
args=("$@")
for i in "${!args[@]}"; do
    case "${args[$i]}" in
        --post-data) body="${args[$((i + 1))]}" ;;
        -O) out="${args[$((i + 1))]}" ;;
    esac
done
printf '%s' "$body" >> "$CURL_STUB_LOG"
printf '\n' >> "$CURL_STUB_LOG"
case "$body" in
    *commentCreate*)
        if [ "${CURL_STUB_COMMENT:-ok}" != "ok" ]; then
            exit 8
        fi
        printf '{"data":{"commentCreate":{"success":true}}}'
        ;;
    *)
        printf '%s' "$CURL_STUB_TICKET" > "$out"
        if [ "${CURL_STUB_HTTP:-200}" != "200" ]; then
            exit 8
        fi
        ;;
esac
STUB
chmod +x "$TMP/stub/wget"

export CURL_STUB_LOG="$TMP/requests.log"
: > "$CURL_STUB_LOG"
export CURL_STUB_TICKET='{"data":{"issue":{"id":"9f3c1a2b-4c4d-4e5f-8a9b-0c1d2e3f4a5b","identifier":"CRI-310","title":"M10.4 carrier: dirty label cleanup workflow + route","url":"https://linear.app/brokenbots/issue/CRI-310","state":{"name":"Ready for Development"}}}}'

fetch_script="$TMP/fetch_ticket.sh"
render "$TMP/intake" "$TREE_ROOT/scripts/fetch_ticket.sh.tftpl" "$fetch_script"
chmod +x "$fetch_script"

comment_script="$TMP/post_cleanup_comment.sh"
render "$TMP/intake" "$TREE_ROOT/scripts/post_cleanup_comment.sh.tftpl" "$comment_script"
chmod +x "$comment_script"

# Stub curl is put on PATH ahead of the real one: the scripts under test
# must be exercised against the canned Linear responses, never the live API.
export PATH="$TMP/stub:$PATH"

export LINEAR_API_KEY="lina_test_key_000000"

# Happy path: fetch stores the enveloped ticket and reports it.
if out="$("$fetch_script")"; then
    ok "fetch_ticket fetches the ticket"
else
    fail "fetch_ticket failed: $out"
fi
[ -s "$run_dir/ticket.json" ] || fail "ticket.json missing or empty: $run_dir/ticket.json"
grep -q "fetched CRI-310" <<<"$out" || fail "fetch_ticket must report the fetched ticket: got '$out'"

# The fetch query carried the ticket id as the GraphQL variable.
# (jq reads the multi-line pretty-printed request bodies as a JSON stream.)
if [ "$(jq -r '.variables.id' "$CURL_STUB_LOG")" = "$SLUG" ]; then
    ok "fetch query carries the ticket id as a GraphQL variable"
else
    fail "fetch query variables.id is not the ticket id"
fi
if grep -q 'query(\$id: String!)' "$CURL_STUB_LOG"; then
    ok "fetch uses the issue(id:) query"
else
    fail "fetch must use the issue(id:) query"
fi

# The comment step posts the exact evidence body against the ticket's UUID.
: > "$CURL_STUB_LOG"
if out="$("$comment_script")"; then
    ok "post_cleanup_comment posts the evidence comment"
else
    fail "post_cleanup_comment failed: $out"
fi
require_equal "$out" "cleanup comment posted" "post_cleanup_comment reports success"
# The jq -r gate passes; the recorded request (a multi-line pretty-printed
# JSON value — jq reads it as a stream) carries the exact evidence body
# against the ticket's UUID.
require_equal "$(jq -r '.variables.body' "$CURL_STUB_LOG")" \
    "cleanup pass: dirty label verified as a routing surface (validation run C)" \
    "comment body is the exact evidence string"
require_equal "$(jq -r '.variables.id' "$CURL_STUB_LOG")" \
    "9f3c1a2b-4c4d-4e5f-8a9b-0c1d2e3f4a5b" \
    "comment targets the ticket's issue UUID (read through .data.issue.id)"
if jq -e '.query | contains("commentCreate") and contains("issueId: $id")' "$CURL_STUB_LOG" >/dev/null; then
    ok "comment uses the commentCreate mutation"
else
    fail "comment must use the commentCreate mutation on issueId"
fi

# The jq -e gate rejects an in-band failure: success=false must fail the step.
cat > "$TMP/stub/comment_fail.json" <<'EOF'
{"data":{"commentCreate":{"success":false}}}
EOF
if env CURL_STUB_COMMENT="no" "$comment_script" >/dev/null 2>&1; then
    fail "commentCreate success=false must fail the step"
else
    ok "commentCreate success=false fails the step"
fi

# A missing Linear key fails loudly before any request is made.
: > "$CURL_STUB_LOG"
if env -u LINEAR_API_KEY "$comment_script" >/dev/null 2>&1; then
    fail "missing LINEAR_API_KEY must fail loudly"
else
    ok "missing LINEAR_API_KEY fails loudly"
fi
# No request was made without the credential.
[ ! -s "$CURL_STUB_LOG" ] || fail "a request was attempted without the credential"

# An HTTP error from Linear fails the fetch (the ticket is unfetched, so no
# comment is possible and the workflow's failed terminal records it).
rm -rf "$run_dir"
if env CURL_STUB_HTTP="500" "$fetch_script" >/dev/null 2>&1; then
    fail "HTTP 500 from Linear must fail the fetch"
else
    ok "HTTP error from Linear fails the fetch"
fi

# An in-band GraphQL error (unknown ticket) fails too.
rm -rf "$run_dir"
CURL_STUB_TICKET='{"errors":[{"message":"Issue not found"}]}' "$fetch_script" >/dev/null 2>&1 \
    && { fail "in-band GraphQL error must fail the fetch"; } \
    || ok "in-band GraphQL error fails the fetch"

# A ticket id outside Linear's identifier shape is rejected before any
# request is made.
: > "$CURL_STUB_LOG"
bad_render="$TMP/fetch_bad.sh"
sed -e "s@{{ .intake_root | shellquote }}@$(shquote "$TMP/intake")@g" \
    -e "s@{{ .ticket_id | shellquote }}@$(shquote 'CRI-32; rm -rf /tmp/x')@g" \
    "$TREE_ROOT/scripts/fetch_ticket.sh.tftpl" > "$bad_render"
chmod +x "$bad_render"
if "$bad_render" >/dev/null 2>&1; then
    fail "a ticket id with non-identifier characters must be rejected"
else
    ok "ticket id outside the identifier shape is rejected"
fi
[ ! -s "$CURL_STUB_LOG" ] || fail "a request was attempted with an invalid ticket id"

# ── 5. Sibling trees untouched ───────────────────────────────────────────────

untouched="$(git -C "$REPO_ROOT" status --porcelain -- linear_intake_v1 linear_triage_v1 linear_develop_v1 qa_triage_v1)"
if [ -z "$untouched" ]; then
    ok "linear_triage_v1 and linear_develop_v1 (and the other sibling trees) are untouched — the D7 pins stay valid"
else
    fail "sibling trees have uncommitted changes (would invalidate the D7 pins): $untouched"
fi

if [ "$FAILED" -gt 0 ]; then
    echo "FAILED: $FAILED assertion(s)" >&2
    exit 1
fi
echo "PASS: ticket_cleanup_v1 standalone verified"