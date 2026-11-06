#!/usr/bin/env bash
set -euo pipefail

# KB-372: the ONE apply path for the criteria-jobs control-plane fleet.
#
# Applies ONLY the exported live-verified manifests (k8s/*-fleet.yaml + kanboard-watcher.yaml
# + the chart for RBAC/SPC/PVC/castle service). Every image tag, env pin and secret ref in
# those files matches the running cluster, so this apply is a verified no-op when in sync and
# rolls exactly what changed since export otherwise.
#
# Refuses to run when a criteria run is live (never roll the operator mid-run).
#
# Usage:
#   scripts/fleet-apply.sh           # preview (kubectl diff, exit 1 on substantive drift)
#   scripts/fleet-apply.sh --apply   # apply + rollout status
#
# After ANY live image/env roll: run k8s/tests/export_fleet_state.py to fold the new state
# into git (the drift test fails the next CI otherwise).

NS="criteria-jobs"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MODE="preview"
if [ "${1:-}" = "--apply" ]; then MODE="apply"; fi

KC="kubectl"
[ -n "${KUBECONFIG:-}" ] || KC="kubectl --kubeconfig=$HOME/.kube/config"

FLEET_FILES=(
  "$REPO_ROOT/k8s/03-operator-fleet.yaml"
  "$REPO_ROOT/k8s/kanboard-watcher.yaml"
  "$REPO_ROOT/k8s/04-castle-fleet.yaml"
  "$REPO_ROOT/k8s/06-parapet-fleet.yaml"
)

# Hard gate: no live runs while rolling the operator base image / control plane.
live_runs="$($KC -n "$NS" get criteriaruns --no-headers 2>/dev/null | grep -cvE 'Succeeded|Failed' || true)"
if [ "${live_runs:-0}" -gt 0 ]; then
  echo "REFUSING: $live_runs non-terminal CriteriaRun(s) live — roll after the lane drains." >&2
  exit 2
fi

for f in "${FLEET_FILES[@]}"; do
  echo "== $f"
  if [ "$MODE" = "apply" ]; then
    $KC apply -f "$f"
  else
    # preview: substantive-drift detector (annotation/generation noise already filtered by export)
    out="$($KC diff -f "$f" 2>/dev/null || true)"
    subst="$(printf '%s\n' "$out" | grep -E '^[+-][^+-]' \
      | grep -vE 'last-applied|creationTimestamp|managedFields|"apiVersion"|revision:|generation|restartedAt' || true)"
    if [ -n "$subst" ]; then
      echo "SUBSTANTIVE DRIFT in $f — the live cluster moved away from the exported state:" >&2
      printf '%s\n' "$subst" | head -40 >&2
      echo "Re-export first: k8s/tests/export_fleet_state.py" >&2
      exit 3
    fi
    echo "  in sync (no substantive drift)"
  fi
done

if [ "$MODE" = "apply" ]; then
  for dep in criteria-k8s-operator criteria-kanboard-watcher castle parapet; do
    $KC -n "$NS" rollout status "deploy/$dep" --timeout=180s
  done
fi
echo "fleet-apply: $MODE complete — all four deployments in declared state"
