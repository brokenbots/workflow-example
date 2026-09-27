#!/usr/bin/env bash
set -euo pipefail

# KB-22 regression: the kanboard-watcher deployment must wire the watcher's
# watchdog health endpoints so a poll stall renders the pod unready instead
# of a silent Running 1/1. Asserts the readiness/liveness probes point at
# /readyz + /livez on the health port, that the port matches the binary's
# default health address, that the per-poll deadline env is configured, and
# that the startup probe budget exceeds POLL_TIMEOUT so a slow-but-healthy
# startup is never restart-looped while its work is still inside its
# deadline.

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
MANIFEST="$REPO_ROOT/k8s/kanboard-watcher.yaml"
WATCHER_SRC="$REPO_ROOT/criteria-k8s/cmd/kanboard-watcher/main.go"

fail() {
    echo "FAIL: $1" >&2
    exit 1
}

# duration_to_seconds converts a Go duration string ("300s", "5m", "1h")
# into whole seconds for the budget arithmetic below.
duration_to_seconds() {
    local d="$1" n u
    d=${d//\"/}
    n=$(sed -n 's/^\([0-9][0-9]*\)[smh]$/\1/p' <<<"$d")
    [ -n "$n" ] || { echo ""; return; }
    u=$(sed -n 's/^[0-9][0-9]*\([smh]\)$/\1/p' <<<"$d")
    case "$u" in
        s) echo "$n" ;;
        m) echo "$((n * 60))" ;;
        h) echo "$((n * 3600))" ;;
        *) echo "" ;;
    esac
}

# probe_value extracts a numeric key's value from a probe block.
probe_value() {
    sed -n "s/^.*$1: \([0-9][0-9]*\)$/\1/p" <<<"$2" | head -1
}

[ -f "$MANIFEST" ] || fail "k8s/kanboard-watcher.yaml is missing"
[ -f "$WATCHER_SRC" ] || fail "kanboard-watcher main.go is missing"

manifest=$(cat "$MANIFEST")
[ -n "$manifest" ] || fail "k8s/kanboard-watcher.yaml is empty"

# Health port the watcher serves /readyz and /livez on.
health_port_block=$(grep -B1 'containerPort: 8081' <<<"$manifest")
grep -q 'name: health' <<<"$health_port_block" || \
    fail "missing the named health port 8081 the probes must target"

# Readiness: fails once polls stop succeeding (stall detection, KB-22).
ready_block=$(grep -A4 'readinessProbe:' <<<"$manifest")
grep -q 'path: /readyz' <<<"$ready_block" || \
    fail "readinessProbe must call /readyz"
grep -q 'port: health' <<<"$ready_block" || \
    fail "readinessProbe must target the health port"

# Liveness: fails only when polls stop completing at all (wedge outside
# the per-poll deadline) so the kubelet restarts the pod.
live_block=$(grep -A6 'livenessProbe:' <<<"$manifest")
grep -q 'path: /livez' <<<"$live_block" || \
    fail "livenessProbe must call /livez"
grep -q 'port: health' <<<"$live_block" || \
    fail "livenessProbe must target the health port"

grep -q 'name: POLL_TIMEOUT' <<<"$manifest" || \
    fail "POLL_TIMEOUT must be configured on the watcher container"

# The binary's default health address must keep matching the manifest port.
grep -q 'defaultHealthAddr.*":8081"' "$WATCHER_SRC" || \
    fail "binary default health address no longer matches the manifest health port (8081)"

# And the binary must keep running polls under the per-poll deadline.
grep -q 'pollWithDeadline' "$WATCHER_SRC" || \
    fail "watcher no longer runs polls under the per-poll deadline (KB-22)"

# Startup budget (KB-22): until the startupProbe succeeds, readiness and
# liveness are disabled, and /livez reports "no poll has completed since
# startup" until the first poll finishes. The startup budget must exceed
# the worst-case bounded startup work — POLL_TIMEOUT for project
# resolution plus POLL_TIMEOUT for the first poll — or a slow-but-healthy
# startup would be restart-looped under exactly the degraded-upstream
# condition this workstream targets.
poll_timeout=$(duration_to_seconds "$(awk '/name: POLL_TIMEOUT/{getline; print $2}' <<<"$manifest")")
[ -n "$poll_timeout" ] || fail "POLL_TIMEOUT value is missing or unparseable"

startup_block=$(grep -A7 'startupProbe:' <<<"$manifest")
startup_period=$(probe_value 'periodSeconds' "$startup_block")
startup_threshold=$(probe_value 'failureThreshold' "$startup_block")
if [ -z "$startup_period" ] || [ -z "$startup_threshold" ]; then
    fail "startupProbe must configure periodSeconds and failureThreshold"
fi
startup_budget=$((startup_period * startup_threshold))
if [ "$startup_budget" -le "$((2 * poll_timeout))" ]; then
    fail "startupProbe budget ${startup_budget}s must exceed 2 * POLL_TIMEOUT ($((2 * poll_timeout))s): a deadline-bound startup plus first poll would be restart-looped"
fi

# The regular liveness fuse (post-startup wedge catcher) must stay wired.
live_period=$(probe_value 'periodSeconds' "$live_block")
live_threshold=$(probe_value 'failureThreshold' "$live_block")
if [ -z "$live_period" ] || [ -z "$live_threshold" ]; then
    fail "livenessProbe must configure periodSeconds and failureThreshold"
fi

echo "OK: kanboard-watcher watchdog wiring (KB-22)"
