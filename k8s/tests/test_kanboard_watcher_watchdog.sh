#!/usr/bin/env bash
set -euo pipefail

# KB-22 regression: the kanboard-watcher deployment must wire the watcher's
# watchdog health endpoints so a poll stall renders the pod unready instead
# of a silent Running 1/1. Asserts the readiness/liveness probes point at
# /readyz + /livez on the health port, that the port matches the binary's
# default health address, and that the per-poll deadline env is configured.

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
MANIFEST="$REPO_ROOT/k8s/kanboard-watcher.yaml"
WATCHER_SRC="$REPO_ROOT/criteria-k8s/cmd/kanboard-watcher/main.go"

fail() {
    echo "FAIL: $1" >&2
    exit 1
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

echo "OK: kanboard-watcher watchdog wiring (KB-22)"