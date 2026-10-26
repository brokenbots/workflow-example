#!/usr/bin/env bash
# routes-sync.sh — re-render criteria-routes CM url pins from THIS repo's HEAD (#303).
# Aligns every workflowLibrary git url pin (the `ref=` query parameter and the
# separate top-level `ref` key) to the current commit, and applies the CM. Never
# hand-edit the CM again.
# KB-313 (ADR 307): the CM's run-config surfaces are retired — the sync touches
# url/ref pins only, never adapterImages/env/volumes/secrets; adapter image tags
# are per-project binding concerns (binding CMs), not pins this script owns.
# KB-338 (P0.5 dual-read): per-project dispatch records migrate out of the
# routes array project by project — --export-dispatch renders the records
# fragment from the live array so the operator can stage, review, and land them
# (the criteria-k8s dual-read serves record-carrying projects from their record
# alone; the 322 follow-up deletes the array once every live lane carries one).
#   scripts/routes-sync.sh --check   print the diff only (exit 1 if out of sync)
#   scripts/routes-sync.sh           apply
#   scripts/routes-sync.sh --export-dispatch   print {dispatch} fragment (stdout) + summary (stderr); never applies
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

if [ "$MODE" = "--export-dispatch" ]; then
    python3 - "$CUR" <<'PYEXP'
import json, sys
doc = json.loads(sys.argv[1])
payload = json.loads(doc["data"]["routes.json"])
dispatch = {}
registered = set((payload.get("projectRegistry") or {}).keys())
for route in payload.get("routes") or []:
    project = route.get("project")
    rule = {k: v for k, v in route.items() if k != "project"}
    dispatch.setdefault(project, {"rules": []})["rules"].append(rule)
    if project not in registered:
        rule["__unregistered__"] = True
for project, record in list(dispatch.items()):
    bad = [r for r in record["rules"] if r.pop("__unregistered__", False)]
    if bad:
        print(f"WARNING: {len(bad)} route rule(s) for project {project!r} are not in projectRegistry — landing them as a record will fail validation (KB-311); add the registry entry first", file=sys.stderr)
if not dispatch:
    print("no routes to export — the routes array is empty or absent", file=sys.stderr)
    sys.exit(1)
print(json.dumps({"dispatch": dispatch}, indent=2))
for project, record in dispatch.items():
    names = ", ".join(r.get("name", "?") for r in record["rules"])
    print(f"exported record {project!r}: {len(record['rules'])} rule(s) [{names}]", file=sys.stderr)
print("next: review the fragment, merge it into the CM's routes.json payload, apply — the dual-read then serves this project from the record alone (KB-338 P0.5)", file=sys.stderr)
PYEXP
    exit 0
fi
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
