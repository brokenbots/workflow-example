#!/usr/bin/env bash
set -euo pipefail

# Regression test for KB-240: deployed remote envs must declare per-env
# port-0 listen addresses, not a shared fixed port.
#
# Every deployed remote env across the workflow trees used to declare
# listen_address "0.0.0.0:7778". Pre-KB-238 only the root env got a listener,
# so the shared declaration was latent; engine v0.5.48 (KB-238, #526) correctly
# hosts a listener per declared env, and the shared port triple-bound —
# deterministic "bind: address already in use" at run init (canary
# kb-228-1791669373 crash-loop on a v0.5.48 base, KB-228 comment id 745).
#
# The fix binds 0.0.0.0:0 per env: the engine resolves a distinct OS-assigned
# wildcard port per env and publishes each env's resolved address per-env
# (CRI-293 + KB-238 wiring), which the operator bakes into each peer pod.
#
# The KB-238 engine suite missed this class because its fixture (variant C)
# declared DISTINCT listen addresses across envs — not the deployed shape.
# This suite mirrors the REAL tree shape:
#
#   1. structural: every remote env in every deployed tree (root +
#      subworkflow bodies) declares exactly `listen_address = "0.0.0.0:0"`,
#      with the multi-env trees keeping their exact env inventories — all envs
#      with IDENTICAL declared addresses across root and subworkflow bodies;
#   2. runtime positive: applying a root+subworkflow fixture that declares
#      multiple remote envs with IDENTICAL port-0 addresses ends in a
#      successful run with one wildcard shim per env, pairwise-distinct
#      resolved ports, and none of the CRI-293 loopback-isolation fallbacks;
#   3. runtime negative control: the same fixture shape with a shared FIXED
#      port (0.0.0.0:7778 — the shipped-before-fix declaration) is flagged by
#      the engine: shim-era engines isolate the colliding shims onto LOOPBACK
#      random ports (not dialable from separate pods), while KB-238-line
#      engines fail the run outright ("bind: address already in use").
#      Either signal proves the shared-port shape is still detected — the
#      positive probe above cannot pass vacuously.

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
FIXTURES="$REPO_ROOT/k8s/tests/fixtures"
CRITERIA="${CRITERIA_BIN:-criteria}"

fail() {
    echo "FAIL: $1" >&2
    exit 1
}

command -v "$CRITERIA" >/dev/null 2>&1 \
    || fail "criteria binary '$CRITERIA' not found (set CRITERIA_BIN); the suite runs the pinned engine offline"
command -v python3 >/dev/null 2>&1 || fail "python3 not found"
command -v jq >/dev/null 2>&1 || fail "jq not found"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# ── 1. Structural invariant: deployed remote envs bind per-env 0.0.0.0:0 ─────

python3 - "$REPO_ROOT" <<'PYSTRUCT'
import re
import sys

repo_root = sys.argv[1]

# Every deployed remote env declaration in the repo, with the env names that
# must exist per file. workflow dirs contribute their subworkflow bodies, so
# the counts ARE the deployed tree shape this suite pins.
deployed = {
    "kanboard_develop_v1/adapters.chcl": ["develop"],
    "kanboard_triage_v1/adapters.chcl": ["triage"],
    "review_triage_v1/adapters.chcl": ["review"],
    "qa_triage_v1/adapters.chcl": ["primary", "scratch"],
    "qa_triage_v1/workflows/repro_runner/adapters.chcl": ["primary", "worktree"],
    "workstream_handler_v1/adapters.chcl": ["worktree", "primary"],
    "workstream_handler_v1/workflows/pair_programming_loop/adapters.chcl": ["worktree"],
    "workstream_handler_v1/workflows/branch_manager/adapters.chcl": ["worktree", "primary"],
    "workstream_handler_v1/workflows/pr_reviewer_loop/adapters.chcl": ["worktree"],
}

# The multi-body deployed trees: root and subworkflow bodies must keep
# multiple remote envs with IDENTICAL declared listen addresses (the shape
# the KB-238 variant-C fixture never covered).
identical_shape = {
    "workstream_handler_v1": [
        "workstream_handler_v1/adapters.chcl",
        "workstream_handler_v1/workflows/pair_programming_loop/adapters.chcl",
        "workstream_handler_v1/workflows/branch_manager/adapters.chcl",
        "workstream_handler_v1/workflows/pr_reviewer_loop/adapters.chcl",
    ],
    "qa_triage_v1": [
        "qa_triage_v1/adapters.chcl",
        "qa_triage_v1/workflows/repro_runner/adapters.chcl",
    ],
}

env_decl = re.compile(r'environment\s+"remote"\s+"([^"]+)"')


def env_blocks(src, path):
    """Balanced-scan each `environment "remote" "name" { ... }` block."""
    blocks = []
    for match in env_decl.finditer(src):
        idx = src.index("{", match.end())
        depth = 0
        for pos in range(idx, len(src)):
            if src[pos] == "{":
                depth += 1
            elif src[pos] == "}":
                depth -= 1
                if depth == 0:
                    blocks.append((match.group(1), src[idx + 1:pos]))
                    break
        else:
            sys.exit(f"FAIL: unbalanced env block for '{match.group(1)}' in {path}")
    return blocks


errors = []

for rel_path, expected_envs in deployed.items():
    path = f"{repo_root}/{rel_path}"
    try:
        with open(path, encoding="utf-8") as f:
            src = f.read()
    except FileNotFoundError:
        errors.append(f"{rel_path} is missing")
        continue
    blocks = env_blocks(src, rel_path)
    names = [name for name, _ in blocks]
    if sorted(names) != sorted(expected_envs):
        errors.append(
            f"{rel_path}: envs {sorted(names)} != deployed shape {sorted(expected_envs)}"
        )
    for name, body in blocks:
        addrs = re.findall(r'listen_address\s*=\s*"([^"]*)"', body)
        if len(addrs) != 1:
            errors.append(f"{rel_path} env '{name}': {len(addrs)} listen_address declarations")
            continue
        if addrs[0] != "0.0.0.0:0":
            errors.append(
                f"{rel_path} env '{name}': listen_address '{addrs[0]}' is not 0.0.0.0:0 "
                "(KB-240: two envs sharing a fixed bind address triple-bind at run init)"
            )

for tree, files in identical_shape.items():
    addrs = []
    for rel_path in files:
        with open(f"{repo_root}/{rel_path}", encoding="utf-8") as f:
            src = f.read()
        addrs.extend(a for _, body in env_blocks(src, rel_path) for a in re.findall(r'listen_address\s*=\s*"([^"]*)"', body))
    if len(addrs) < 2:
        errors.append(f"{tree}: deployed shape lost its multi-remote-env fixture surface")
    if len(set(addrs)) != 1:
        errors.append(
            f"{tree}: root + subworkflow bodies no longer declare IDENTICAL listen addresses {addrs}"
        )

if errors:
    for err in errors:
        print(f"FAIL: {err}", file=sys.stderr)
    sys.exit(1)
print("ok: structural invariant (every deployed remote env binds 0.0.0.0:0)")
PYSTRUCT

# ── 2. Runtime positive: port-0 envs resolve to distinct wildcard shims ──────

POSITIVE="$FIXTURES/env_listen_ports"
"$CRITERIA" validate "$POSITIVE" --warnings-as-errors >/dev/null 2>"$TMP/positive_validate.err" \
    || fail "positive fixture failed validate --warnings-as-errors: $(tail -5 <"$TMP/positive_validate.err")"

rc=0
(cd "$POSITIVE" && timeout 120 "$CRITERIA" apply . \
    --events-file "$TMP/positive_events.ndjson" \
    >"$TMP/positive.out.log" 2>"$TMP/positive.err.log") || rc=$?
[ "$rc" -eq 0 ] || fail "positive fixture apply failed (exit $rc): $(cat "$TMP/positive.out.log" "$TMP/positive.err.log" | tail -5)"

jq -e 'select(.payload_type == "RunCompleted") | .payload.success' \
    <"$TMP/positive_events.ndjson" | grep -q true \
    || fail "positive fixture run did not complete successfully"

shim_logs="$(cat "$TMP/positive.out.log" "$TMP/positive.err.log" | grep -c 'remote shim listening addr=' || true)"
[ "$shim_logs" -ge 2 ] || \
    fail "expected >=2 per-env shim listeners at run init, got $shim_logs (the two root bodies must each bind)"

printf '%s' "$(cat "$TMP/positive.out.log" "$TMP/positive.err.log")" \
    | grep -q 'isolating colliding remote environment shim' && \
    fail "positive fixture hit the CRI-293 loopback isolation substitute — port-0 envs must bind their own wildcard shims"

# Every shim resolves wildcard (dialable from separate pods, unlike the
# CRI-293 loopback substitute) with a pairwise-distinct, non-fixed port.
addrs="$(cat "$TMP/positive.out.log" "$TMP/positive.err.log" \
    | grep -o 'remote shim listening addr=[^ ]*' | sed 's/.*addr=//')"
[ -n "$addrs" ] || fail "no resolved shim addresses found in the positive fixture run"

loop_count="$(printf '%s\n' "$addrs" | grep -c '^\(127\.0\.0\.1\|\[::1\]\|localhost\):' || true)"
[ "$loop_count" -eq 0 ] \
    || fail "shim resolved to loopback — must resolve wildcard for cross-pod dialing"

dupes="$(printf '%s\n' "$addrs" | grep -o '[0-9][0-9]*$' | sort -n | uniq -d)"
[ -z "$dupes" ] || fail "shims resolved to duplicate ports: $dupes"
echo "ok: runtime positive ($(echo "$addrs" | wc -l) envs resolved distinct wildcard shims: $(echo "$addrs" | tr '\n' ' '))"

# ── 3. Negative control: the shared FIXED port shape stays flagged ───────────

SHARED="$FIXTURES/env_listen_ports_shared"
rc=0
(cd "$SHARED" && timeout 120 "$CRITERIA" apply . \
    --events-file "$TMP/shared_events.ndjson" \
    >"$TMP/shared.out.log" 2>"$TMP/shared.err.log") || rc=$?

logs="$(cat "$TMP/shared.out.log" "$TMP/shared.err.log")"
bound_fail="$(printf '%s' "$logs" | grep -c 'address already in use' || true)"
isolations="$(printf '%s' "$logs" | grep -c 'isolating colliding remote environment shim' || true)"

if [ "$bound_fail" -ge 1 ]; then
    # KB-238-line engine: the shared port triple-binds at run init. This IS
    # the crash the deployed fleet hit on v0.5.48.
    [ "$rc" -ne 0 ] || fail "engine logged a bind collision but the run did not fail"
    echo "ok: negative control (engine refuses the shared fixed port at run init)"
elif [ "$isolations" -ge 2 ]; then
    # Shim-era engine (v0.5.44, CRI-293 wiring): colliding shims are isolated
    # onto loopback randoms — the not-dialable substitute this card refuses.
    [ "$rc" -eq 0 ] || fail "engine isolated the colliding shims but the run failed unexpectedly: $(cat "$TMP/shared.out.log" "$TMP/shared.err.log" | tail -5)"
    loopback_shims="$(printf '%s\n' "$logs" | grep 'remote shim listening addr=' | grep -c 'addr=127.0.0.1\|addr=\[::1\]\|addr=localhost' || true)"
    [ "$loopback_shims" -ge 2 ] || \
        fail "expected the isolated colliding shims to fall back to loopback, got: $(printf '%s\n' "$logs" | grep 'remote shim listening addr=' | sed 's/.*addr=//')"
    echo "ok: negative control (shim-era engine isolated the colliding shims onto nondialable loopback)"
else
    fail "shared fixed-port fixture produced no collision signal (neither bind failure nor isolation) — the fixture no longer detects the KB-240 class"
fi

echo "PASS: KB-240 — deployed remote envs declare per-env port-0 addresses; the engine resolves and publishes a distinct wildcard address per env"