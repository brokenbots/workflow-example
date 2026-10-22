#!/usr/bin/env bash
set -euo pipefail

# KB-313 (ADR 307): pincheck class E — the routes CM global run-config
# staleness gate. The engine is scripts/check_routes_stale.sh (pincheck
# delegates the class to it, example + live CM); this test drives the engine
# over the two-revision KB-313 fixture matrix:
#   1. post-cutover payload + stale surfaces    -> exit 1, names KB-313
#   2. coexistence payload + stale surfaces     -> exit 0, window-open note
#      (two cutover-done projects + three un-cut, the migration shape)
#   3. clean post-cutover payload               -> exit 0, clean note
#   4. no projectRegistry (pre-KB-311) + stale  -> exit 0, pre-registry note
#   5. invalid JSON                             -> exit 1
#   6. shipped example                          -> exit 0 (window open today)
#   7. pincheck wiring                          -> class E delegates to the engine

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
ENGINE="$REPO_ROOT/scripts/check_routes_stale.sh"
PINCHECK="$REPO_ROOT/scripts/pincheck.sh"

fail() {
    echo "FAIL: $1" >&2
    exit 1
}

ENGINE_RC=0

[ -x "$ENGINE" ] || fail "scripts/check_routes_stale.sh is missing or not executable"
[ -f "$PINCHECK" ] || fail "scripts/pincheck.sh is missing"
command -v jq >/dev/null 2>&1 || fail "jq is required"

# Build a routes payload with the given stale key set on the given entries:
# make_payload <registry-json> <wf-name>=<stale-keys[,..]> ...
make_payload() {
    local registry="$1" entry_name entry_keys
    shift
    local entries=""
    for spec in "$@"; do
        entry_name="${spec%%=*}"
        entry_keys="${spec#*=}"
        entries="$entries,\"$entry_name\":$(printf '%s' "$entry_keys" | jq -Rs '
            . as $keys | rtrimstr("\n") | split(",") as $set |
            {
                type: "url", url: "https://example.invalid/wf", ref: "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2",
                interface: {apiVersion: "criteria/v1", kind: "workflow-interface",
                    workflow: "wf", adapters: {shell: [], copilot: []},
                    mounts: [], secretEnv: [], env: [], configSlots: []}
            }
            | if ($set | contains(["env"])) then . + {env: {RUNNER_NOTE: "stale"}} else . end
            | if ($set | contains(["adapterImages"])) then . + {adapterImages: {shell: "localhost:5000/stale:1"}} else . end
            | if ($set | contains(["volumes"])) then . + {volumes: [{name: "v", kind: "tmp", mountPath: "/tmp/v"}]} else . end
            | if ($set | contains(["secrets"])) then . + {secrets: [{name: "s", secretProviderClass: "openbao", mountPath: "/tmp/s"}]} else . end
        ')"
    done
    printf '{"routes":[],"workflowLibrary":{%s},"projectRegistry":%s}' "${entries#,}" "$registry"
}

run_engine() { # $1=fixture-label, payload JSON on stdin -> rc in ENGINE_RC
    ENGINE_RC=0
    bash "$ENGINE" - "$1" >/tmp/engine_out 2>/tmp/engine_err || ENGINE_RC=$?
}

expect_fail() { # $1=fixture-label (payload on stdin), $2=must-contain
    run_engine "$1"
    [ "$ENGINE_RC" -eq 1 ] || fail "$1: expected exit 1, got $ENGINE_RC"
    grep -q "$2" /tmp/engine_err || fail "$1: expected '$2' in engine error: $(cat /tmp/engine_err)"
}

expect_pass_note() { # $1=fixture-label (payload on stdin), $2=must-contain (stdout)
    run_engine "$1"
    [ "$ENGINE_RC" -eq 0 ] || fail "$1: expected exit 0, got $ENGINE_RC: $(cat /tmp/engine_err)"
}

# Registry JSON fragments for the fixture matrix
REG_ALL_DONE='{"Go Dev":{"namespace":"criteria-go-dev","repos":["o/go"],"cutover":"done"},"TS SDK":{"namespace":"typescript-sdk","repos":["o/ts"],"cutover":"done"}}'
REG_MIGRATION='{"Go Dev":{"namespace":"criteria-go-dev","repos":["o/go"],"cutover":"done"},"TS SDK":{"namespace":"typescript-sdk","repos":["o/ts"],"cutover":"done"},"Runner":{"namespace":"criteria-jobs","repos":["o/runner"]},"Other":{"namespace":"other-jobs","repos":["o/other"]},"Kanboard Tickets":{"namespace":"criteria-jobs","repos":["o/kb"]}}'

# 1. post-cutover + stale surfaces (each leaving key) -> the CI error
for keys in env adapterImages volumes secrets env,volumes,secrets,adapterImages; do
    make_payload "$REG_ALL_DONE" "done-wf=$keys" | expect_fail "post-cutover stale ($keys)" "KB-313"
done

# 1b. one un-cut entry means the window is still open (allow-with-warning)
make_payload "$REG_MIGRATION" "kanboard-tree=env,adapterImages" | expect_pass_note "coexistence stale" "migration window open"

# 2. the migration coexistence shape: two cut-over + three un-cut projects,
#    stale surfaces only on the un-cut side -> warning, not error
make_payload "$REG_MIGRATION" "runner-tree=volumes,secrets,env,adapterImages" | expect_pass_note "two-cut+three-uncut stale" "allowed-with-warning"

# 3. clean post-cutover payload -> pass, clean note
make_payload "$REG_ALL_DONE" | expect_pass_note "clean post-cutover" "no stale global run-config surfaces"

# 4. pre-KB-311 payload (no projectRegistry) + stale surfaces -> window note
printf '{"routes":[],"workflowLibrary":{"legacy":{"type":"url","url":"u","ref":"r","env":{"E":"1"}}}}' \
    | expect_pass_note "pre-registry stale" "pre-KB-311 payload"

# 5. invalid JSON -> fail loudly
printf 'not json at all' > /tmp/engine_payload
run_engine "invalid-json" < /tmp/engine_payload
[ "$ENGINE_RC" -eq 1 ] || fail "invalid-json: expected exit 1, got $ENGINE_RC"
grep -q "not valid JSON" /tmp/engine_err || fail "invalid-json: expected 'not valid JSON' error: $(cat /tmp/engine_err)"

# 6. the shipped example passes today (window open, stale surfaces warned)
bash "$ENGINE" "$REPO_ROOT/k8s/examples/routes-configmap.yaml" >/tmp/engine_out 2>/tmp/engine_err
grep -q "migration window open" /tmp/engine_out \
    || fail "shipped example: expected the window-open note, got: $(cat /tmp/engine_out /tmp/engine_err)"

# 6b. ...and its post-cutover siblings validate: a payload equal to the
# example minus the two un-cut registry entries AND their stale surfaces
# must pass cleanly (the post-cutover target state is reachable).
jq -c '
    {
        routes: .routes,
        workflowLibrary:
            (.workflowLibrary | with_entries(
                .value |= del(.env, .adapterImages, .volumes, .secrets))),
        configLibrary: .configLibrary,
        projectRegistry: (.projectRegistry | with_entries(select(.value.cutover == "done")))
    }
' <(awk -v key="routes.json" '$0 ~ "^[ ]*" key ": \\|$" {b=1;next} b && (/^[ \t]/||$0==""){sub(/^[ \t]*/,"",$0);print;next} b{exit}' "$REPO_ROOT/k8s/examples/routes-configmap.yaml") \
    > /tmp/engine_payload
run_engine "post-cutover target shape" < /tmp/engine_payload
grep -q "no stale global run-config surfaces" /tmp/engine_out \
    || fail "post-cutover target shape: expected the clean note, got: $(cat /tmp/engine_out /tmp/engine_err)"

# 7. pincheck delegates class E to the engine (wiring, cheap)
grep -q "check_routes_stale.sh" "$PINCHECK" \
    || fail "pincheck does not delegate class E to scripts/check_routes_stale.sh"

rm -f /tmp/engine_out /tmp/engine_err

echo "PASS: pincheck class E (KB-313 routes CM staleness gate) — two-revision matrix"