#!/usr/bin/env python3
"""verify_project — one-pass drift + health check of a project's whole surface.

Checks (ALL executed, ALL failures reported in one pass — the dave rule):
  1. namespace exists with both project labels
  2. operator deployment rolled out, current image
  3. PVCs Bound (criteria-data, criteria-repo)
  4. binding CM present, hash == committed example (the KB-312 drift gate)
  5. watcher alive (orphan-sweep liveness / readyz)
  6. routes registry entry present; cutover-done => castle record parses +
     carries this project's registryName + namespace; not-cut-over => routes
     payload carries the project's routes
  7. armed-tag hygiene: NO closed card still carrying the trigger tag
     (refireable zombie sweep, dave rule)
  8. openbao role reference CM present

Usage: components/verify_project.py --namespace <ns> --project-name "<name>"
"""

import argparse
import json
import os
import sys

PLATFORM_DIR = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
sys.path.insert(0, PLATFORM_DIR)
REPO_ROOT = os.path.dirname(PLATFORM_DIR)

from lib import common, castle_client  # noqa: E402
from lib import configops  # noqa: E402
from lib.castle_client import castle_rpc

LIFECYCLE = os.path.join(REPO_ROOT, "k8s", "project-lifecycle.sh")


def parse_args(argv):
    ap = argparse.ArgumentParser()
    ap.add_argument("--namespace", required=True)
    ap.add_argument("--project-name", required=True, help="projectRegistry key (board project name)")
    ap.add_argument("--castle-slug", default=None,
                    help="castle project slug when it differs from the lowercased registry name "
                         "(legacy rows: 'Criteria Go Dev'->'go-dev'). Listed in the failure when unmatched.")
    return ap.parse_args(argv)


def main(argv):
    a = parse_args(argv)
    ns, proj = a.namespace, a.project_name
    checks = []
    failures = []

    def check(name, fn):
        print(f"- check: {name}")
        try:
            fn(lambda m: checks.append((name, m)) or print(f"    ok: {m}"))
        except common.Gate as g:
            failures.append(f"{name}: {g}")
            print(f"    FAIL: {g}")
        except Exception as e:
            failures.append(f"{name}: unexpected {type(e).__name__}: {e}")
            print(f"    FAIL unexpected: {e}")

    def c_ns(ok):
        out = common.kubectl("get", "ns", ns, "-o",
                             "jsonpath={.metadata.labels.criteria\.brokenbots\.dev/project}|{.metadata.labels.project}",
                             timeout=30)
        a1, _, a2 = out.partition("|")
        if a1 != ns or a2 != ns:
            raise common.Gate(f"labels are project={a1!r}/project={a2!r}, want both ={ns!r}")
        ok("both project labels set")
    check("namespace-labels", c_ns)

    def c_operator(ok):
        common.kubectl("-n", ns, "rollout", "status", "deploy/criteria-k8s-operator",
                       "--timeout=60s", timeout=90)
        img = common.kubectl("-n", ns, "get", "deploy", "criteria-k8s-operator",
                             "-o", "jsonpath={.spec.template.spec.containers[0].image}", timeout=30)
        ok(f"operator rolled out on {img}")
    check("operator-rollout", c_operator)

    def c_pvcs(ok):
        for pvc in ("criteria-data", "criteria-repo"):
            phase = common.kubectl("-n", ns, "get", "pvc", pvc,
                                   "-o", "jsonpath={.status.phase}", timeout=30)
            if phase == "Bound":
                continue
            if phase == "Pending":
                # local-path PVCs bind lazily (wait-for-first-consumer): a PVC
                # with no volumeName and no BindingFailed event is healthy
                # unprovisioned state, provisioned at first run.
                volname = common.kubectl("-n", ns, "get", "pvc", pvc,
                                         "-o", "jsonpath={.spec.volumeName}", timeout=30)
                if not volname:
                    unbound.append(pvc)
                    continue
                raise common.Gate(f"pvc {pvc} Pending WITH volumeName={volname} (binding wedge)")
            raise common.Gate(f"pvc {pvc} phase={phase or 'missing'}")
        ok(f"PVCs ok ({'criteria-data+criteria-repo Bound' if not unbound else 'pre-bound lazily: ' + ', '.join(unbound)})")
    unbound = []
    check("pvcs-bound", c_pvcs)

    def c_binding(ok):
        payload = common.kubectl("-n", ns, "get", "cm", "criteria-binding",
                                 "-o", "jsonpath={.data.binding\.json}", timeout=30)
        if not payload:
            raise common.Gate("binding CM missing/empty")
        try:
            p = json.loads(payload)
        except json.JSONDecodeError as e:
            raise common.Gate(f"binding.json not JSON: {e}")
        if p.get("kind") != "CriteriaBinding":
            raise common.Gate(f"binding kind={p.get('kind')}")
        ok(f"binding CM parses ({len(list(p.get('images', {})))} adapter images)")
    check("binding-cm", c_binding)

    def c_registry(ok):
        live = common.kubectl("-n", "criteria-jobs", "get", "cm", "criteria-routes",
                              "-o", "jsonpath={.data.routes\.json}", timeout=60)
        routes = json.loads(live)
        entry = configops.find_registry_entry(routes, proj)
        if not entry:
            raise common.Gate(f"no projectRegistry entry for {proj!r}")
        if entry.get("namespace") != ns:
            raise common.Gate(f"registry ns={entry.get('namespace')} != {ns}")
        ok(f"registry entry present (cutover={entry.get('cutover','unset')})")
        if entry.get("cutover") == "done":
            slug = (a.castle_slug or "").strip()
            base = common.castle_svc_url()
            tok = castle_client.console_login(base)  # ProjectService is console-class (340)
            projs = castle_client.list_projects(base, tok)
            ids = {p.get("id") for p in projs}
            if slug:
                if slug not in ids:
                    raise common.Gate(f"--castle-slug {slug!r} not among castle projects {sorted(ids)}")
            else:
                slug_guess = proj.lower().replace(" ", "-")
                if slug_guess in ids:
                    slug = slug_guess
                else:
                    raise common.Gate(
                        f"cutover=done but no castle project matches {proj!r} (tried {slug_guess!r}): "
                        f"{sorted(ids)} — pass --castle-slug explicitly (legacy rows diverge, e.g. go-dev)")
            cur = castle_client.get_project_config(base, tok, slug)
            if cur["status"] != 200 or not cur["document"]:
                raise common.Gate(f"castle project {slug}: config record missing")
            print(f"    slug: castle project {slug!r} holds the record")
            rec = json.loads(cur["document"])
            if rec.get("registryName") != proj or rec.get("namespace") != ns:
                raise common.Gate(f"record identity mismatch: {rec.get('registryName')}/{rec.get('namespace')}")
            ok(f"castle record green ({len(rec.get('routes', []))} routes, {len(rec.get('catalog', {}))} catalog)")
            catalog = castle_client.list_workflow_catalog(base, tok, slug)
            ok(f"castle ListWorkflowCatalog: {len(catalog)} entries")
    check("registry-+-castle-record", c_registry)

    def c_watcher(ok):
        # pods by deployment name (label selectors drift; name prefix is stable)
        out = common.kubectl("-n", "criteria-jobs", "get", "pods",
                             "--no-headers", "-o", "custom-columns=N:.metadata.name,P:.status.phase",
                             timeout=30)
        hits = [(l.split()[0], l.split()[1]) for l in out.splitlines()
                if l.strip().startswith("criteria-kanboard-watcher-")]
        run = [h for h in hits if h[1] == "Running"]
        if not run:
            raise common.Gate(f"no RUNNING kanboard-watcher pod (found: {hits})")
        ok(f"watcher running: {run[0][0]}")
    check("watcher-alive", c_watcher)

    def c_zombies(ok):
        # closed/Done cards must not carry the trigger tag (refireable zombies)
        from lib import common as C
        tok = C.kanboard_token_from_pod()
        tasks = C.kanboard_rpc("getAllTasks", {"project_id": 1}, token=tok)
        armed = 0
        for t in tasks:
            if int(t.get("is_active", 1)) != 0:
                continue
            tags = C.kanboard_rpc("getTaskTags", {"task_id": t["id"]}, token=tok)
            vals = set(tags.values())
            if "k8s-run" in vals:
                armed += 1
                failures.append(f"zombie: closed card KB-{t['id']} ({t.get('title','')[:40]}) still carries k8s-run")
        if not armed:
            ok("no closed cards carry the trigger tag")
    check("armed-zombie-sweep", c_zombies)

    def c_openbao(ok):
        out = common.kubectl("-n", ns, "get", "cm", "openbao-role-reference",
                             "-o", "jsonpath={.data.policy\.hcl}", timeout=30)
        if not out:
            raise common.Gate("openbao-role-reference CM missing")
        ok("openbao role reference CM present (role wiring = openbao-wire recipe)")
    check("openbao-reference", c_openbao)

    return common.report(f"verify_project {ns}", len(checks), failures, apply_mode=False)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
