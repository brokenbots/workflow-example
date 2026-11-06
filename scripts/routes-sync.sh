#!/usr/bin/env bash
# routes-sync.sh — re-render the routes-pinned CM url pins from THIS repo's HEAD (#303).
# KB-352: the sync owns BOTH routes pin sites of record —
#   -- criteria-routes: the watcher's routing CM (workflow trees, interface
#      refs, projectRegistry, route rules),
#   -- criteria-agent-routes: the operator's assignment-agent routes payload
#      (KB-338 — the lane that stamps CriteriaRuns from dispatch).
# Both carry the same routes.json shape and live or die on the SAME per-entry
# pin sites: the workflow url's `ref=` query parameter and the separate
# top-level `ref` key. Aligning only criteria-routes leaves the agent payload
# stale and every CR the assignment lane stamps bakes the OLD workflow ref
# (live 2026-11-04: kb-131/132/133/135/136 + kb-136 first leg all died
# wait_merge_ci at 600s because criteria-agent-routes still pinned
# kanboard_develop_v1 while criteria-routes carried that day's #239 repin).
# Aligns every workflowLibrary git url pin (the `ref=` query parameter and the
# separate top-level `ref` key) to the current commit, and applies both CMs.
# Never hand-edit the CMs again.
# KB-313 (ADR 307): the CMs' run-config surfaces are retired — the sync touches
# url/ref pins only, never adapterImages/env/volumes/secrets; adapter image tags
# are per-project binding concerns (binding CMs), not pins this script owns.
# KB-338 (P0.5 dual-read): per-project dispatch records migrate out of the
# routes array project by project — --export-dispatch renders the records
# fragment from the live array so the operator can stage, review, and land them
# (the criteria-k8s dual-read serves record-carrying projects from their record
# alone; the 322 follow-up deletes the array once every live lane carries one).
#   scripts/routes-sync.sh --check   report BOTH CMs' drift (exit 1 if either is out of sync)
#   scripts/routes-sync.sh           align + apply BOTH CMs, print the operator reload checklist
#   scripts/routes-sync.sh --export-dispatch   print {dispatch} fragment (stdout) + summary (stderr); never applies; criteria-routes scope (the routes array lives there)
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
MODE="${1:-apply}"
export KUBECONFIG="${KUBECONFIG:-$HOME/.kube/config}"

HEAD="$(git -C "$ROOT" rev-parse HEAD)"

CMD="$(command -v kubectl || echo kubectl)"
NAMESPACE="criteria-jobs"
# KB-352: both routes pin sites of record, in sync order
ROUTE_CMS=(criteria-routes criteria-agent-routes)

# Fail-closed on a mistyped mode: an unknown flag silently falling through to
# apply would mutate the live CMs instead of the dry operation the caller
# intended.
case "$MODE" in
    apply|--check|--export-dispatch) ;;
    *) echo "usage: routes-sync.sh [apply|--check|--export-dispatch] (unknown mode: $MODE)" >&2; exit 2 ;;
esac

SCRATCH="$(mktemp -d /tmp/routes-sync.XXXXXX)"
trap 'rm -rf "$SCRATCH"' EXIT

fetch_cm() { # $1 = cm name -> "$SCRATCH/$1.cur.json"; fail-closed when unreadable
    if ! "$CMD" -n "$NAMESPACE" get cm "$1" -o json > "$SCRATCH/$1.cur.json" 2> "$SCRATCH/$1.get.err"; then
        echo "routes-sync: cannot read ConfigMap $NAMESPACE/$1 — every routes CM of record must exist, or its pin sites cannot be verified at HEAD (a missing $1 is exactly the silent-second-pin-site hole KB-352 closed)" >&2
        sed 's/^/  /' "$SCRATCH/$1.get.err" >&2
        return 1
    fi
}

render_cm() { # $1 = cm name -> "$SCRATCH/$1.new.json" (pins rendered at HEAD)
    python3 - "$SCRATCH/$1.cur.json" "$HEAD" "$1" > "$SCRATCH/$1.new.json" <<'PYEOF'
import json, re, sys
cur_path, head, cm = sys.argv[1], sys.argv[2], sys.argv[3]
doc = json.load(open(cur_path))
try:
    payload = json.loads(doc["data"]["routes.json"])
    workflow_library = payload["workflowLibrary"]
except (KeyError, ValueError) as err:
    sys.exit(f"routes-sync: CM {cm} does not carry a routes.json payload with workflowLibrary ({err}) — cannot sync pins")
for name, wf in workflow_library.items():
    url = wf.get("url")
    if url:
        new = re.sub(r"ref=[0-9a-f]{40}", f"ref={head}", url)
        if new != url:
            wf["url"] = new
    # the CM carries a separate top-level `ref` key per entry (a second pin site:
    # b9355ab/b2bc937 era). Align it too — an unaligned ref silently overrides
    # the aligned url depending on which consumer reads which key (found live
    # 2026-10-19: CM held url@HEAD + ref@b9355ab simultaneously). Same rule on
    # both CMs: the agent payload stamps spec.workflowSource.ref from this key
    # verbatim, so a stale agent ref bakes a stale pin into every CR it stamps.
    if isinstance(wf.get("ref"), str) and re.fullmatch(r"[0-9a-f]{40}", wf["ref"]):
        if wf["ref"] != head:
            wf["ref"] = head
payload.setdefault("metadata", {})["renderedFrom"] = head
doc["data"]["routes.json"] = json.dumps(payload, indent=2)
print(json.dumps(doc))
PYEOF
}

check_cm() { # $1 = cm name -> prints verdict; rc 0 aligned, 1 drifted
    local cm="$1"
    # re-read at check time: another actor may have applied since the render fetch
    "$CMD" -n "$NAMESPACE" get cm "$cm" -o json > "$SCRATCH/$cm.check.json"
    if python3 - "$cm" "$SCRATCH/$cm.check.json" "$SCRATCH/$cm.new.json" "$HEAD" <<'PYCHK'
import json, re, sys
cm, live_path, mine_path, head = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4]

def short(v):
    # display form: 7-hex like the ticket shorthand, taken from a url ?ref= or a bare ref value
    if not v:
        return "(none)"
    m = re.search(r"ref=([0-9a-f]{7})", str(v))
    return m.group(1) if m else str(v)[:7]

live = json.loads(json.load(open(live_path))["data"]["routes.json"])
mine = json.loads(json.load(open(mine_path))["data"]["routes.json"])
drift = []
if (live.get("metadata") or {}).get("renderedFrom") != (mine.get("metadata") or {}).get("renderedFrom"):
    drift.append(f"renderedFrom {(live.get('metadata') or {}).get('renderedFrom')} -> {head}")
for name, wf in mine["workflowLibrary"].items():
    lwf = live["workflowLibrary"].get(name) or {}
    for key in ("url", "ref"):
        old, new = lwf.get(key), wf.get(key)
        if old != new:
            drift.append(f"{name} {key} pin: {short(old)} -> {short(new)}")
if not drift:
    sys.exit(0)
print(f"routes-sync --check: {cm} DRIFTED from HEAD ({head}):")
for line in drift:
    print(f"  {line}")
sys.exit(1)
PYCHK
    then
        echo "routes-sync --check: $cm in sync with HEAD ($HEAD)"
        return 0
    fi
    return 1
}

fetch_cm criteria-routes

if [ "$MODE" = "--export-dispatch" ]; then
    # criteria-routes scope: the dispatch records migrate out of THIS CM's
    # routes array; the agent CM carries no routes array.
    python3 - "$SCRATCH/criteria-routes.cur.json" <<'PYEXP'
import json, sys
doc = json.load(open(sys.argv[1]))
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

# The agent CM is a hard requirement for check + apply: silently syncing only
# criteria-routes is the exact defect this script exists to prevent.
fetch_cm criteria-agent-routes
render_cm criteria-routes
render_cm criteria-agent-routes

if [ "$MODE" = "--check" ]; then
    # compare ONLY the rendered routes.json payload (CM metadata churns by itself:
    # resourceVersion/managedFields are not ours to pin). Both CMs are always
    # reported; exit 1 iff EITHER pin site of record drifted.
    rc=0
    for cm in "${ROUTE_CMS[@]}"; do
        check_cm "$cm" || rc=1
    done
    if [ "$rc" -eq 0 ]; then
        echo "routes-sync --check: both CMs aligned with HEAD ($HEAD)"
    else
        echo "routes-sync --check: drifted CMs above — run 'make routes-sync'" >&2
    fi
    exit "$rc"
fi

"$CMD" -n "$NAMESPACE" apply -f "$SCRATCH/criteria-routes.new.json"
"$CMD" -n "$NAMESPACE" apply -f "$SCRATCH/criteria-agent-routes.new.json"
echo "routes-sync: criteria-routes + criteria-agent-routes CMs updated (all url/ref pins -> $HEAD)"
cat <<'CHECKLIST'
routes-sync RELOAD CHECKLIST (KB-352) — the operator's assignment-agent lane
resolves its routes payload ONCE at operator startup (in-memory); the CM apply
above never re-reads it, so the pins only take effect after the operator reloads:
  1. Confirm zero Running CriteriaRuns (drain any live run first):
       kubectl -n criteria-jobs get criteriaruns
  2. Restart the operator at zero Running CRs:
       kubectl -n criteria-jobs rollout restart deploy/criteria-k8s-operator
  3. Re-arm / refire only once the restart is healthy:
       kubectl -n criteria-jobs rollout status deploy/criteria-k8s-operator
(this script cannot safely restart the operator itself while runs are live — the restart is the caller's)
CHECKLIST
