#!/usr/bin/env bash
# repin.sh — rewrite every tree adapters.chcl version pin from releases.chcl (#303).
# Usage:
#   scripts/repin.sh              apply manifest versions to all trees
#   scripts/repin.sh --check      exit 1 if any pin differs (CI drift gate)
# Managed adapter types: keys of releases.chcl that look like adapter repo names
# (currently: copilot, shell). Everything else in adapters.chcl is left alone.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
MODE="${1:-apply}"

# manifest: type=version lines (copilot=, shell=; peer_* and tree_ref ignored)
MANIFEST="$(sed -n 's/^\([a-z][a-z_]*\) *= *"\([0-9][0-9.]*\)".*/\1=\2/p' "$ROOT/releases.chcl" | grep -Ev '^(peer_|tree_ref)')"
TYPES="$(printf '%s\n' "$MANIFEST" | cut -d= -f1 | paste -sd'|' -)"
if [ -z "$TYPES" ]; then
    echo "repin: no managed adapter versions parsed from releases.chcl" >&2
    exit 2
fi
export MANIFEST

# One awk pass per file: track the enclosing adapter "<type>" block, rewrite its
# `version = "..."` literal when the type is managed. Prints diff actions.
rewrite() {
    awk -v types="$TYPES" -v mode="$MODE" -v file="$1" '
        BEGIN { n = split(types, T, "|"); for (i = 1; i <= n; i++) want[T[i]] = "" }
        # harvest manifest from env
        function load_manifest(   arr, i, kv) {
            split(ENVIRON["MANIFEST"], arr, "\n")
            for (i in arr) { split(arr[i], kv, "="); if (kv[1] in want) want[kv[1]] = kv[2] }
        }
        BEGIN { load_manifest() }
        /^[[:space:]]*adapter[[:space:]]*"[^"]+"[[:space:]]*"[^"]+"/ {
            cur = $0
            if (match(cur, /adapter[[:space:]]*"[^"]+"/)) {
                t = substr(cur, RSTART, RLENGTH)
                gsub(/adapter[[:space:]]*"?|"$/, "", t)
                gsub(/"/, "", t)
                adapter_type = t
            }
        }
        /^[[:space:]]*version[[:space:]]*=/ && adapter_type in want {
            cur = $0
            if (match(cur, /"([0-9][0-9.]*)"/)) {
                cur_v = substr(cur, RSTART+1, RLENGTH-2)
                if (cur_v != want[adapter_type]) {
                    if (mode == "--check") {
                        printf "repin --check: %s: adapter %s pinned %s, manifest wants %s\n", file, adapter_type, cur_v, want[adapter_type] > "/dev/stderr"
                        drift = 1
                    } else {
                        new = cur
                        sub(/"([0-9][0-9.]*)"/, "\"" want[adapter_type] "\"", new)
                        printf "repin: %s: %s %s -> %s\n", file, adapter_type, cur_v, want[adapter_type] > "/dev/stderr"
                        print new
                        next
                    }
                }
            }
        }
        { print }
        END { exit (mode == "--check" && drift) ? 1 : 0 }
    ' "$1"
}

cd "$ROOT"
mapfile -t FILES < <(git ls-files '*adapters.chcl')
DRIFT_EXIT=0
for f in "${FILES[@]}"; do
    [ -f "$f" ] || continue
    if [ "$MODE" = "--check" ]; then
        rewrite "$f" > /dev/null || DRIFT_EXIT=1
    else
        if rewrite "$f" > "/tmp/repin.$$.out"; then
            :
        else
            echo "repin: awk failed on $f" >&2
            rm -f "/tmp/repin.$$.out"
            exit 2
        fi
        if ! cmp -s "$f" "/tmp/repin.$$.out"; then
            cp "/tmp/repin.$$.out" "$f"
        fi
        rm -f "/tmp/repin.$$.out"
    fi
done

if [ "$MODE" = "--check" ]; then
    if [ "$DRIFT_EXIT" != 0 ]; then
        echo "repin --check: DRIFT DETECTED — run 'make repin' and commit" >&2
        exit 1
    fi
    echo "repin --check: all managed adapter pins match releases.chcl"
    exit 0
fi

# Lock refresh for touched trees (best effort, per tree, never hides others).
ST=0
while IFS= read -r rel; do
    [ -n "$rel" ] || continue
    tree_dir="$ROOT/$(dirname "$rel")"
    echo "repin: criteria adapter lock $tree_dir" >&2
    if command -v criteria >/dev/null 2>&1; then
        criteria adapter lock "$tree_dir" || ST=$?
    else
        # house fallback: LAN criteria-base container, host uid for writable lockfiles
        docker run --rm --user "$(id -u):$(id -g)" --entrypoint /bin/sh \
            -e HOME=/tmp -e CRITERIA_HOME=/tmp/.criteria \
            -v "$ROOT:$ROOT" -w "$ROOT" \
            localhost:5000/criteria-base:v0.1-v0.5.56 \
            criteria adapter lock "$tree_dir" || ST=$?
    fi
done < <(git -C "$ROOT" diff --name-only -- '*adapters.chcl')
exit $ST
