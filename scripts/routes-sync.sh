#!/usr/bin/env bash
# routes-sync.sh — re-render criteria-routes CM url pins from THIS repo's HEAD (#303).
# Aligns every workflowLibrary git url pin (the `ref=` query parameter and the
# separate top-level `ref` key) to the current commit, and applies the CM. Never
# hand-edit the CM again.
# KB-313 (ADR 307): the CM's run-config surfaces are retired — the sync touches
# url/ref pins only, never adapterImages/env/volumes/secrets; adapter image tags
# are per-project binding concerns (binding CMs), not pins this script owns.
#   scripts/routes-sync.sh --check   print the diff only (exit 1 if out of sync)
#   scripts/routes-sync.sh           apply
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
MODE="${1:-apply}"
export KUBECONFIG="${KUBECONFIG:-$HOME/.kube/config}"

HEAD="$(git -C "$ROOT" rev-parse HEAD)"

CMD="$(command -v kubectl || echo kubectl)"
CUR="$("$CMD" -n criteria-jobs get cm criteria-routes -o json)"
python3 - "$CUR" "$HEAD" <<'PYEOF' > /tmp/routes_new.json
import json, re, sys
doc = json.loads(sys.argv[1])
head = sys.argv[2]
routes = json.loads(doc["data"]["routes.json"])
for name, wf in routes["workflowLibrary"].items():
    url = wf.get("url")
    if url:
        new = re.sub(r"ref=[0-9a-f]{40}", f"ref={head}", url)
        if new != url:
            wf["url"] = new
    # the CM carries a separate top-level `ref` key per entry (a second pin site:
    # b9355ab/b2bc937 era). Align it too — an unaligned ref silently overrides
    # the aligned url depending on which consumer reads which key (found live
    # 2026-10-19: CM held url@HEAD + ref@b9355ab simultaneously).
    if isinstance(wf.get("ref"), str) and re.fullmatch(r"[0-9a-f]{40}", wf["ref"]):
        if wf["ref"] != head:
            wf["ref"] = head
routes.setdefault("metadata", {})["renderedFrom"] = head
doc["data"]["routes.json"] = json.dumps(routes, indent=2)
print(json.dumps(doc))
PYEOF

if [ "$MODE" = "--check" ]; then
    # compare ONLY the rendered routes.json payload (CM metadata churns by itself:
    # resourceVersion/managedFields are not ours to pin)
    "$CMD" -n criteria-jobs get cm criteria-routes -o json > /tmp/routes_cur.json
    cat > /tmp/routes_cmp.py <<'PYCHK'
import json, sys
cur = json.load(open("/tmp/routes_cur.json"))
new = json.load(open("/tmp/routes_new.json"))
live = json.loads(cur["data"]["routes.json"])
mine = json.loads(new["data"]["routes.json"])
same = (live.get("metadata", {}).get("renderedFrom") == mine.get("metadata", {}).get("renderedFrom")
        and live["workflowLibrary"] == mine["workflowLibrary"])
sys.exit(0 if same else 1)
PYCHK
    if python3 /tmp/routes_cmp.py; then
        echo "routes-sync --check: CM in sync with HEAD ($HEAD)"
        exit 0
    fi
    echo "routes-sync --check: CM DRIFTED from HEAD ($HEAD) — run 'make routes-sync'" >&2
    exit 1
fi
"$CMD" -n criteria-jobs apply -f /tmp/routes_new.json
echo "routes-sync: criteria-routes CM updated (all url/ref pins -> $HEAD)"
