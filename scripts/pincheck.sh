#!/usr/bin/env bash
# pincheck.sh — ONE-SHOT validation of every pin consumer in the repo (#303, dave:
# stop the CI ping-pong; a developer validation pass must surface ALL stale pins
# at once, not fail one literal per CI round).
#
# Checks (each reports every offender, none stops the others):
#   A. adapters.chcl version literals == releases.chcl manifest
#   B. no stale hardcoded adapter refs in CI/test guards
#   C. lockfile freshness: every tree compiles strict against its current lockfile
#   D. routes CM alignment (delegates to routes-sync.sh --check when reachable)
#   E. routes CM global run-config staleness (KB-313, ADR 307): a post-cutover
#      payload (every projectRegistry entry cutover:done) that still carries
#      volumes/secrets/env/adapterImages on workflowLibrary entries is a CI
#      error; while the migration window is open the stale surfaces are noted.
# Exit: 0 only when every check passes. Honors CRITERIA_BIN; container fallback.
set -uo pipefail
export KUBECONFIG="${KUBECONFIG:-$HOME/.kube/config}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
FAILED=0
note() { echo "pincheck: $*"; }
bad()  { echo "pincheck FAIL: $*" >&2; FAILED=$((FAILED+1)); }

# -- engine selection: host criteria if present, else the LAN container binary
CRIT="$(command -v criteria || true)"
CRIT_ARGS=()
if [ -n "${CRITERIA_BIN:-}" ]; then CRIT="$CRITERIA_BIN"; fi
run_crit() {
    # runs `criteria ...` with a 0.5.56 engine; container fallback keeps the lock
    # contract identical to CI.
    if [ -n "$CRIT" ] && [ -x "$CRIT" ]; then
        "$CRIT" "$@"
    else
        docker run --rm --user "$(id -u):$(id -g)" --entrypoint /bin/sh \
            -e HOME=/tmp -e CRITERIA_HOME=/tmp/.criteria \
            -v "$ROOT:$ROOT" -w "$ROOT" \
            localhost:5000/criteria-base:v0.1-v0.5.56 criteria "$@"
    fi
}

# -- manifest versions
SHELL_V="$(sed -n 's/^shell *= *"\([^"]*\)".*/\1/p' releases.chcl)"
COPILOT_V="$(sed -n 's/^copilot *= *"\([^"]*\)".*/\1/p' releases.chcl)"
[ -n "$SHELL_V" ] && [ -n "$COPILOT_V" ] || { bad "cannot parse releases.chcl"; exit 2; }
note "manifest: copilot=$COPILOT_V shell=$SHELL_V"

# -- A: every adapters.chcl literal matches the manifest (report ALL drift)
TMP="$(mktemp)"
while IFS= read -r f; do
    awk -v file="$f" -v sv="$SHELL_V" -v cv="$COPILOT_V" '
        /^[[:space:]]*adapter[[:space:]]*"[^"]+"/ {
            if (match($0, /adapter[[:space:]]*"[^"]+"/)) {
                t = substr($0, RSTART, RLENGTH); gsub(/adapter[[:space:]]*"?|"$/, "", t); gsub(/"/, "", t); at = t
            }
        }
        /^[[:space:]]*version[[:space:]]*=/ && (at == "copilot" || at == "shell") {
            if (match($0, /"([0-9][0-9.]*)"/)) {
                v = substr($0, RSTART+1, RLENGTH-2)
                want = (at == "copilot") ? cv : sv
                if (v != want) printf "%s: %s pinned %s, manifest wants %s\n", file, at, v, want
            }
        }
    ' "$f" >> "$TMP"
done < <(git ls-files '*adapters.chcl')
if [ -s "$TMP" ]; then bad "adapter pin drift (run 'make repin'):"; cat "$TMP" >&2
else note "A: all adapters.chcl pins match the manifest"; fi
rm -f "$TMP"

# -- B: stale hardcoded adapter refs in CI / tests
TMP="$(mktemp)"
A_BEFORE=$FAILED
grep -rn --include='*.yml' --include='*.yaml' --include='*.sh' \
    -E 'criteria-adapter-(copilot|shell):[0-9]+\.[0-9]+\.[0-9]+|pin_installed [0-9]' \
    .github 2>/dev/null | grep -vE 'releases\.chcl' > "$TMP" || true
while IFS= read -r line; do
    case "$line" in
        *":$COPILOT_V"*|*":$SHELL_V"*|*"pin_installed $COPILOT_V"*|*"pin_installed $SHELL_V"*) ;;
        *) bad "hardcoded adapter ref not from the manifest: $line" ;;
    esac
done < "$TMP"
[ "$FAILED" = "$A_BEFORE" ] && note "B: no stale hardcoded adapter refs"
rm -f "$TMP"

# -- C: lockfile freshness per tree (strict compile; EVERY failing tree listed)
C_BEFORE=$FAILED
ERRF="$(mktemp)"
while IFS= read -r tree; do
    [ -d "$tree" ] || continue
    if ! run_crit validate "$ROOT/$tree" --warnings-as-errors >/dev/null 2>"$ERRF"; then
        # classify: lockfile-lag errors are pincheck's business; other compile
        # errors are pre-existing tree defects (already tracked on cards) —
        # still report, as a WARN class that does not mask pin drift.
        if grep -qE 'lockfile does not match|version changed' "$ERRF"; then
            bad "lockfile lag in $tree (run 'make repin')"
        else
            note "WARN: $tree fails strict validate (pre-existing, tracked separately): $(grep -m1 -E 'Error:' "$ERRF" | cut -c1-140)"
        fi
    fi
done < <(git ls-files | grep -E '/$|^[^/]+$' | grep -E '^[a-z_]+_v[0-9]+$' | sort -u)
rm -f "$ERRF"
if [ "$FAILED" = "$C_BEFORE" ]; then note "C: no lockfile lag in any tree"; fi

# -- D: routes CM (skip cleanly off-cluster)
D_BEFORE=$FAILED
if command -v kubectl >/dev/null 2>&1 && kubectl -n criteria-jobs get cm criteria-routes >/dev/null 2>&1; then
    if ./scripts/routes-sync.sh --check >/dev/null 2>&1; then
        note "D: routes CM in sync with HEAD"
    else
        bad "routes CM drifted from HEAD (run 'make routes-sync')"
    fi
else
    note "D: routes CM check skipped (no cluster access)"
fi

# -- E: routes CM global run-config staleness (KB-313, ADR 307): delegated to
# scripts/check_routes_stale.sh — a post-cutover payload (every projectRegistry
# entry cutover: done) that still carries volumes/secrets/env/adapterImages on
# workflowLibrary entries is a CI error; while the migration window is open the
# stale surfaces are noted. Checked on the shipped example, plus the live CM
# when kubectl can reach it.
E_BEFORE=$FAILED
TMP_PAYLOAD="$(mktemp)"
if ./scripts/check_routes_stale.sh k8s/examples/routes-configmap.yaml; then
    note "E: shipped routes CM passes the KB-313 staleness gate"
else
    bad "E: shipped routes CM fails the KB-313 staleness gate"
fi
if [ "$FAILED" = "$E_BEFORE" ] && command -v kubectl >/dev/null 2>&1 \
    && kubectl -n criteria-jobs get cm criteria-routes -o jsonpath='{.data.routes\.json}' > "$TMP_PAYLOAD" 2>/dev/null; then
    if ./scripts/check_routes_stale.sh - "live criteria-routes CM" < "$TMP_PAYLOAD"; then
        note "E: live routes CM passes the KB-313 staleness gate"
    else
        bad "E: live routes CM fails the KB-313 staleness gate"
    fi
fi
rm -f "$TMP_PAYLOAD"

if [ "$FAILED" -gt 0 ]; then
    echo "pincheck: $FAILED check class(es) failed — fix ALL above in one pass" >&2
    exit 1
fi
echo "pincheck: ALL CLEAN"