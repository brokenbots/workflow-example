#!/usr/bin/env bash
# routes-sync.sh — re-render criteria-routes CM url pins from THIS repo's HEAD (#303).
# Aligns every workflowLibrary git url pin to the current commit, stamps peer adapter
# image tags from releases.chcl, and applies the CM. Never hand-edit the CM again.
#   scripts/routes-sync.sh --check   print the diff only (exit 1 if out of sync)
#   scripts/routes-sync.sh           apply
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
MODE="${1:-apply}"
export KUBECONFIG="${KUBECONFIG:-$HOME/.kube/config}"

HEAD="$(git -C "$ROOT" rev-parse HEAD)"
PEER_SHELL="$(sed -n 's/^peer_shell_tag *= *"\([^"]*\)".*/\1/p' "$ROOT/releases.chcl")"
PEER_COPILOT="$(sed -n 's/^peer_copilot_tag *= *"\([^"]*\)".*/\1/p' "$ROOT/releases.chcl")"

CMD="$(command -v kubectl || echo kubectl)"
CUR="$("$CMD" -n criteria-jobs get cm criteria-routes -o json)"
python3 - "$CUR" "$HEAD" "$PEER_SHELL" "$PEER_COPILOT" <<'PYEOF' > /tmp/routes_new.json
import json, re, sys
doc = json.loads(sys.argv[1])
head, peer_shell, peer_copilot = sys.argv[2], sys.argv[3], sys.argv[4]
routes = json.loads(doc["data"]["routes.json"])
hits = 0
for name, wf in routes["workflowLibrary"].items():
    url = wf.get("url")
    if not url:
        continue
    new = re.sub(r"ref=[0-9a-f]{40}", f"ref={head}", url)
    if new != url:
        hits += 1
        wf["url"] = new
    imgs = wf.get("adapterImages") or {}
    if "shell" in imgs:
        imgs["shell"] = re.sub(r":[^:]*$", f":{peer_shell}", imgs["shell"]); hits += 1
    if "copilot" in imgs:
        imgs["copilot"] = re.sub(r":[^:]*$", f":{peer_copilot}", imgs["copilot"]); hits += 1
routes.setdefault("metadata", {})["renderedFrom"] = head
import collections
if isinstance(routes.get("routes"), list):
    for r in routes["routes"]:
        pass
doc["data"]["routes.json"] = json.dumps(routes, indent=2)
print(json.dumps(doc))
PYEOF

if [ "$MODE" = "--check" ]; then
    "$CMD" -n criteria-jobs get cm criteria-routes -o json > /tmp/routes_cur.json
    if cmp -s /tmp/routes_cur.json /tmp/routes_new.json; then
        echo "routes-sync --check: CM in sync with HEAD ($HEAD)"
        exit 0
    fi
    echo "routes-sync --check: CM DRIFTED from HEAD ($HEAD) — run 'make routes-sync'" >&2
    diff <(python3 -m json.tool /tmp/routes_cur.json) <(python3 -m json.tool /tmp/routes_new.json) | head -40 >&2
    exit 1
fi
"$CMD" -n criteria-jobs apply -f /tmp/routes_new.json
echo "routes-sync: criteria-routes CM updated (all url pins -> $HEAD, peers -> shell:$PEER_SHELL copilot:$PEER_COPILOT)"
