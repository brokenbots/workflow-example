#!/usr/bin/env bash
# tests/test_platform_setup.sh — offline assertions for scripts/platform tooling.
# Cluster/registry/dockder-dependent gates are integration territory (live ops);
# these cover what CI can prove without the cluster: tooling compiles, the
# wrapper dispatches, and the deterministic refusals fire (bad specs, done-card
# arm refusal shape) without any write.
set -u
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

fail=0
say() { printf '%s\n' "$*"; }
t_pass() { say "ok - $1"; }
t_fail() { say "FAIL - $1"; fail=1; }

# --- components compile
for py in scripts/platform/components/*.py scripts/platform/lib/*.py; do
  if python3 -m py_compile "$py" 2>/dev/null; then t_pass "compiles: $py"; else t_fail "compiles: $py"; fi
done

# --- platformctl dispatch table (help + unknown command), no cluster needed
if bash scripts/platform/platformctl help >/dev/null 2>&1; then
  t_pass "platformctl help exits 0"
else
  t_fail "platformctl help"
fi
if bash scripts/platform/platformctl bogus 2>/dev/null; then
  t_fail "platformctl rejects unknown command"
else
  t_pass "platformctl rejects unknown command"
fi

# --- configops: extract the REAL shipped binding payload + parse it
if python3 - <<'PY'
import sys, json
sys.path.insert(0, "scripts/platform")
from lib import configops
payload = configops.extract_data_json_block(
    open("k8s/examples/binding-criteria-go-dev.yaml").read(), "binding.json")
p = json.loads(payload)
assert p["kind"] == "CriteriaBinding", p.get("kind")
assert "images" in p and "shell" in p["images"]
routes = json.loads(configops.extract_data_json_block(
    open("k8s/examples/routes-configmap.yaml").read(), "routes.json"))
assert routes["kind"] == "Routes"
assert "kanboard-develop-url" in routes["workflowLibrary"]
print("extract+parse ok")
PY
then t_pass "configops extract/parse (repo manifests)"; else t_fail "configops extract/parse"; fi

# --- configops: add-route / registry determinism + refusal to overwrite
if python3 - <<'PY'
import sys, json
sys.path.insert(0, "scripts/platform")
from lib import configops
r = configops.empty_routes_payload()
configops.add_registry_entry(r, "Proj X", "ns-x", ["owner/repo"], cutover="done")
configops.add_route(r, "route-a", "wf-a", "Proj X", ["Ready"])
try:
    configops.add_route(r, "route-a", "wf-a", "Proj X", ["Ready"])
    raise SystemExit("dup route accepted")
except ValueError:
    pass
configops.upsert_catalog_entry(r, "wf-a", {"type": "url", "url": "x", "ref": "y"})
try:
    configops.upsert_catalog_entry(r, "wf-a", {"type": "url"})
    raise SystemExit("catalog overwrite accepted")
except ValueError:
    pass
assert len(r["routes"]) == 1
print("determinism+refusals ok")
PY
then t_pass "configops route/registry semantics"; else t_fail "configops route/registry semantics"; fi

# --- register_job: bad spec refusal exits 2 WITHOUT cluster calls
spec="$(mktemp /tmp/spec-bad-XXXX.json)"
printf '%s' '{"tree":"t","project":"p"}' > "$spec"
if python3 scripts/platform/components/register_job.py --spec "$spec" >/dev/null 2>&1; then
  t_fail "register_job accepts an invalid spec"
else
  t_pass "register_job refuses spec missing keys (exit 2)"
fi
rm -f "$spec"

# --- file_card: arm needs repo tag argument (no cluster call before the check)
if python3 scripts/platform/components/file_card.py arm --task-id 1 >/dev/null 2>&1; then
  t_fail "file_card arm without --repo"
else
  t_pass "file_card arm without --repo refused"
fi

exit "$fail"
