#!/usr/bin/env bash
set -euo pipefail

# KB-372 drift guard: the exported fleet manifests (k8s/*-fleet.yaml, kanboard-watcher.yaml)
# MUST equal the live cluster. Runs in CI when the cluster is reachable; skips cleanly when
# it is not (CI has no cluster). Fails closed: any substantive divergence = the exported
# state is stale after a live roll = re-export before the next apply.
#
# Also fails if an ACTIVE image tag in the exports is one of the known-dead tags (a stale
# pin that predates a roll class), regardless of cluster reachability.

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
KC="kubectl"
[ -n "${KUBECONFIG:-}" ] || KC="kubectl --kubeconfig=$HOME/.kube/config"

fail() { echo "FAIL: $1" >&2; exit 1; }

FLEET_FILES=(
  "$REPO_ROOT/k8s/03-operator-fleet.yaml"
  "$REPO_ROOT/k8s/kanboard-watcher.yaml"
  "$REPO_ROOT/k8s/04-castle-fleet.yaml"
  "$REPO_ROOT/k8s/06-parapet-fleet.yaml"
)

# Class A: known-dead image tags must never appear in the exports.
DEAD_TAGS="castle:dev|criteria-k8s:dev|criteria-k8s:kanboard-watcher-|parapet:v1[0-9]\b|criteria-base:v0.1-v0.5.5[0-7]"
for f in "${FLEET_FILES[@]}"; do
  [ -f "$f" ] || fail "missing fleet manifest $f (run k8s/tests/export_fleet_state.py)"
  if grep -Eq "image: localhost:5000/($DEAD_TAGS)" "$f"; then
    fail "$f carries a known-dead image tag (predates a roll class): $(grep -E 'image: localhost:5000/(castle|criteria-k8s|parapet):' "$f" | head -2)"
  fi
done

# Class A2: the operator base env must be the deployed release line (v0.5.58+).
op_base="$(grep -A1 'CRITERIA_BASE_IMAGE' "$REPO_ROOT/k8s/03-operator-fleet.yaml" | grep 'value:' | awk '{print $2}')"
case "$op_base" in
  localhost:5000/criteria-base:v0.1-v0.5.5[6-7]) fail "operator CRITERIA_BASE_IMAGE is a pre-KB-369 base ($op_base) — a deploy would resurrect the secrets-root crash";;
  localhost:5000/criteria-base:v0.1-v0.5.58) ;;
  localhost:5000/criteria-base:v0.1-v0.5.5[9]|localhost:5000/criteria-base:v0.1-v0.5.[6-9][0-9]) ;;
  *) fail "operator CRITERIA_BASE_IMAGE unparsable/unexpected: '$op_base'";;
esac

# Class A3: no plaintext tokens in fleet manifests (secretKeyRef only).
if grep -rnE 'value: (cap-|ctk-|[A-Za-z0-9_-]{32,})' "${FLEET_FILES[@]}" \
     | grep -vE 'value: ("?)(localhost|http|criteria|kanboard|castle|Kanboard|/etc|true|"?\$)'; then
  fail "fleet manifest carries a plaintext token-class value — move it to a secretKeyRef"
fi

# Class B: live cluster equality (skip when no cluster).
if ! $KC -n criteria-jobs get deploy criteria-k8s-operator >/dev/null 2>&1; then
  echo "SKIP: cluster unreachable from this environment (Class A passed)"
  exit 0
fi

for f in "${FLEET_FILES[@]}"; do
  out="$($KC diff -f "$f" 2>/dev/null || true)"
  subst="$(printf '%s\n' "$out" | grep -E '^[+-][^+-]' \
    | grep -vE 'last-applied|creationTimestamp|managedFields|"apiVersion"|revision:|generation|restartedAt' || true)"
  if [ -n "$subst" ]; then
    fail "fleet manifest $f diverges from live cluster:
$subst" | head -30
  fi
done

echo "PASS: fleet manifests == live cluster (apply-safety hold)"
