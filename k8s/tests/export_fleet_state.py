"""KB-372: regenerate k8s/kanboard-watcher.yaml + k8s/06-parapet.yaml + castle/operator pins
from LIVE cluster state so the checked-in manifests are apply-safe ground truth."""
import json
import subprocess

env = {"KUBECONFIG": "/home/dave/.kube/config", "PATH": "/usr/bin:/bin:/usr/local/bin", "HOME": "/home/dave"}
WF = "/home/dave/Projects/workflow-example"


def runb(cmd, timeout=120):
    r = subprocess.run(["bash", "-c", cmd], capture_output=True, text=True, timeout=timeout, env=env)
    return (r.stdout + r.stderr).strip()


def clean_deploy(d, strip_env=(), drop_meta=("managedFields", "resourceVersion", "uid", "creationTimestamp",
                                              "generation", "annotations")):
    md = {k: v for k, v in d["metadata"].items() if k in ("name", "namespace", "labels")}
    # keep the last-applied-free: we intentionally DROP annotations (incl. kubectl last-applied)
    td = d["spec"]["template"]
    t_ann = {k: v for k, v in td["metadata"].get("annotations", {}).items()
             if not k.startswith("kubectl.kubernetes.io/last-applied")}
    # keep operational restartedAt so the rendered template-hash matches the live spec (apply = no-op)
    template = {
        "metadata": {"labels": td["metadata"].get("labels", {}), **({"annotations": t_ann} if t_ann else {})},
        "spec": {},
    }
    ts = td["spec"]
    keep = {}
    for k in ("serviceAccountName", "terminationGracePeriodSeconds", "nodeSelector", "tolerations",
              "securityContext", "affinity", "volumes", "automountServiceAccountToken"):
        if k in ts:
            keep[k] = ts[k]
    template["spec"] = keep
    conts = []
    for c in ts["containers"]:
        cc = {"name": c["name"], "image": c["image"], "imagePullPolicy": c.get("imagePullPolicy", "IfNotPresent")}
        for k in ("name", "command", "args", "ports", "livenessProbe", "startupProbe", "readinessProbe", "resources",
                  "volumeMounts", "securityContext", "terminationMessagePath", "terminationMessagePolicy", "workingDir"):
            if k in c:
                cc[k] = c[k]
        envs = []
        for e in c.get("env", []):
            if e["name"] in strip_env:
                continue
            if "value" in e:
                envs.append({"name": e["name"], "value": e["value"]})
            elif "valueFrom" in e:
                envs.append({"name": e["name"], "valueFrom": e["valueFrom"]})
        cc["env"] = envs
        # canonical key order inside probes/ports so text-grepping tests (watchdog KB-22) see
        # `name:` before `containerPort:`; pyyaml sorts scalar keys alphabetically otherwise.
        for p in cc.get("ports", []) or []:
            if "name" in p:
                cc2 = {"name": p["name"]}
                for k, v in p.items():
                    if k != "name":
                        cc2[k] = v
                p.clear()
                p.update(cc2)
        conts.append(cc)
    template["spec"]["containers"] = conts
    out = {
        "apiVersion": "apps/v1",
        "kind": "Deployment",
        "metadata": md,
        "spec": {"replicas": d["spec"].get("replicas", 1), "selector": d["spec"]["selector"],
                 "template": template},
    }
    if d["spec"].get("strategy"):
        out["spec"]["strategy"] = d["spec"]["strategy"]
    return out


strip_common = ("RESTART_TS",)

results = {}
for dep in ("criteria-k8s-operator", "criteria-kanboard-watcher", "castle", "parapet"):
    raw = json.loads(runb(f"kubectl -n criteria-jobs get deploy {dep} -o json"))
    results[dep] = clean_deploy(raw, strip_env=strip_common)

# header comment to prepend
HEADER = """# KB-372: THIS FILE IS GENERATED GROUND TRUTH — exported from the live cluster
# (2026-11-06, post KB-369 roll) by k8s/tests/export_fleet_state.py. Do not hand-edit image
# tags or env here: roll live first, then re-export. The apply-safety drift test
# (k8s/tests/test_fleet_apply_safe.sh) diffs THIS file against the live cluster and fails
# closed on any divergence, so a stale apply can never regress the fleet.
# Secrets stay secretKeyRef'd (nothing inline); RESTART_TS (operational restart marker)
# intentionally stripped — it is set at deploy time, not config.
"""

for dep, fn in (("criteria-k8s-operator", "k8s/03-operator-fleet.yaml"), ("criteria-kanboard-watcher", "k8s/kanboard-watcher.yaml"),
                ("castle", "k8s/04-castle-fleet.yaml"), ("parapet", "k8s/06-parapet-fleet.yaml")):
    if fn:
        path = f"{WF}/{fn}"
        old = open(path).read() if __import__("os").path.exists(path) else "# (new file)"
        with open(path, "w") as f:
            f.write(HEADER + __import__("yaml").safe_dump(results[dep], sort_keys=False, default_flow_style=False))
        print(f"wrote {fn}")

# operator: lives in the CHART not a yaml — emit a values overlay instead:
vals = {
    "namespace": "criteria-jobs",
    "images": {
        "operator": {"repository": "localhost:5000/criteria-k8s", "tag": "20261106-kb369-1"},
        "workflow": {"repository": "localhost:5000/linear-intake-remote", "tag": "20260919-021702-07f2a29"},
        "criteriaBase": {"repository": "localhost:5000/criteria-base", "tag": "v0.1-v0.5.58"},
        "adapter": {"registry": "localhost:5000", "tag": "k8s-0.5.58-1"},
        "castle": {"repository": "localhost:5000/castle", "tag": "20261026-kb343-2141"},
    },
}
with open(f"{WF}/k8s/fleet/values-live.yaml", "w") as f:
    f.write("# KB-372: LIVE-verified chart values for criteria-jobs (exported 2026-11-06).\n"
            "# Feed to the deploy wrapper: helm template -f k8s/fleet/values-live.yaml.\n"
            "# Regenerate via k8s/tests/export_fleet_state.py after any live roll.\n"
            + __import__("yaml").safe_dump(vals, sort_keys=False))
print("wrote k8s/fleet/values-live.yaml")

# keep a machine-readable full-state export for the drift test
json.dump(results, open(f"{WF}/k8s/fleet/fleet-live-state.json", "w"), indent=1)
print("wrote k8s/fleet/fleet-live-state.json")
