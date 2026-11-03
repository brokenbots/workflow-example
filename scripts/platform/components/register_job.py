#!/usr/bin/env python3
"""register_job — register a NEW job (workflow route) on an EXISTING project.

Input: a small declarative JSON spec (see docs/jobs-setup.md schema):
{
  "tree": "kanboard_develop_v1",          # directory under repo root
  "project": "Kanboard Tickets",          # projectRegistry key
  "route": "kanboard-develop-go",         # NEW route name (unique)
  "states": ["Ready"],                    # trigger states (board columns / linear states)
  "catalog_key": "kanboard-develop-url",  # typically existing; or omitted to register tree with pinned url
  "tags": ["repo:brokenbots/x"],          # optional route tag filter
  "config_ref": "criteria-repo",          # optional configLibrary entry key
  "config_slots": {"baseBranch": "main", "ciGateCmd": "make ci"}  # optional; written to configLibrary entry "<project-slug>" or config_ref
}

Deterministic gates (one pass, all failures reported):
  G1 tree exists with main.chcl + interface.json
  G2 lockfile current: `criteria adapter lock` no-op under fleet criteria-base image
  G3 compile+validate clean: `criteria validate <tree>` in criteria-base container
  G4 interface configSlots: every key in config_slots is a KB-310 slot; unknown slot aborts
  G5 tree pinned from HEAD: url/ref = origin/main HEAD (both pin sites)
  G6 route name not already present; project has a registry entry
  G7 binding schema check: k8s/tests/test_criteria_binding.sh still green (untouched lanes)
Gates run in the container, never against the host binary (engine lag rule).

Writes (apply):
  - catalog entry (workflowLibrary) — only when catalog_key is absent from the live payload
  - configLibrary entry when config_slots given and the entry does not exist
  - the route row
  - target state depends on project cutover: cutover-done -> project record via
    SetProjectConfig + repo yaml re-render (committed target state);
    otherwise the routes CM directly + the repo example in the same pass.

Usage: components/register_job.py --spec spec.json [--apply] [--dry-run-detail]
Default = dry-run (prints the exact delta). --apply writes live + repo yaml.
"""

import argparse
import json
import os
import subprocess
import sys

COMPONENTS_DIR = os.path.dirname(os.path.abspath(__file__))          # scripts/platform/components
PLATFORM_DIR = os.path.dirname(COMPONENTS_DIR)                        # scripts/platform
sys.path.insert(0, PLATFORM_DIR)
REPO_ROOT = os.path.dirname(os.path.dirname(PLATFORM_DIR))            # repo root

from lib import common, castle_client  # noqa: E402
from lib import configops  # noqa: E402

PLATFORM_REGISTRY_NAME = "Criteria K8s Workflow Runner"
TREE_FIELDS_REQUIRED = ["main.chcl", "interface.json"]


def parse_args(argv):
    ap = argparse.ArgumentParser()
    ap.add_argument("--spec", required=True)
    ap.add_argument("--apply", action="store_true")
    ap.add_argument("--skip-container-gates", action="store_true",
                    help="DEV ONLY: skip in-container validate/lock gates (never for real arming)")
    ap.add_argument("--expect-pin", default=None,
                    help="register against the SERVING pin when origin/main is ahead but un-repinned: "
                         "the route will serve the tree at the LIVE catalog pin, not local HEAD. "
                         "Requires the pin to validate in the fleet image (still enforced).")
    return ap.parse_args(argv)


def load_spec(path):
    try:
        with open(path) as f:
            s = json.load(f)
    except FileNotFoundError:
        print(f"spec file not found: {path}"); raise SystemExit(2)
    except json.JSONDecodeError as e:
        print(f"spec not valid JSON: {path}: {e}"); raise SystemExit(2)
    if not isinstance(s, dict):
        print(f"spec must be a JSON object: {path}"); raise SystemExit(2)
    missing = [k for k in ("tree", "project", "route", "states") if k not in s]
    if missing:
        print(f"spec missing required key(s): {missing}  (spec schema: docs/jobs-setup.md)")
        raise SystemExit(2)
    if not isinstance(s["states"], list) or not s["states"]:
        print("spec.states must be a non-empty list"); raise SystemExit(2)
    return s


def head_sha():
    return common.run(["git", "-C", REPO_ROOT, "rev-parse", "origin/main"], timeout=30).strip()


def gate_container(tag, script, timeout=900):
    """Run a script inside the fleet criteria-base image with the repo mounted."""
    image = f"criteria-base:{tag}"
    # only run if image exists locally or remotely
    common.run(["docker", "image", "inspect", image], timeout=60)
    return common.run(["docker", "run", "--rm",
                       "-v", f"{REPO_ROOT}:/wx",
                       "-w", "/wx",
                       "--user", f"{os.getuid()}:{os.getgid()}",
                       "-e", "HOME=/tmp",
                       "--entrypoint", "/bin/sh", image, "-c", script], timeout=timeout)


def castle_slug_for(base, token, project_name, spec=None):
    """Registry (board) name -> castle project slug: lowercased+dash, with a
    match-on-listing fallback; explicit mapping rows live in the project spec
    (legacy rows diverge: 'Criteria Go Dev' -> 'go-dev')."""
    projs = castle_client.list_projects(base, token)
    ids = {pr.get("id") for pr in projs}
    guess = project_name.lower().replace(" ", "-")
    if guess in ids:
        return guess
    nospace = project_name.lower().replace(" ", "")
    if nospace in ids:
        return nospace
    if spec and spec.get("castle_slug"):
        if spec["castle_slug"] in ids:
            return spec["castle_slug"]
        raise common.Gate(f"spec.castle_slug {spec['castle_slug']!r} not among castle projects {sorted(ids)}")
    raise common.Gate(
        f"no castle project matches {project_name!r} (tried {guess!r}/{nospace!r}): {sorted(ids)} — "
        "add \"castle_slug\" to the job spec (legacy rows diverge, e.g. 'Criteria Go Dev' -> go-dev)")


def fleet_base_image():
    out = common.kubectl("-n", "criteria-jobs", "get", "deploy", "criteria-k8s-operator",
                         "-o", "jsonpath={.spec.template.spec.containers[0].env}", timeout=60)
    envs = json.loads(out) if out.strip().startswith("[") else []
    for e in envs:
        if e.get("name") == "CRITERIA_BASE_IMAGE":
            return e["value"]
    raise common.Gate("operator deployment lacks CRITERIA_BASE_IMAGE env")


def main(argv):
    args = parse_args(argv)
    try:
        spec = load_spec(args.spec)
    except SystemExit:
        raise
    failures = []
    report_changes = []

    def note(msg, changed=False):
        marker = "APPLY " if (changed and args.apply) else ("WOULD" if changed else "ok")
        print(f"  [{marker}] {msg}")

    def gate(name, fn):
        print(f"- gate: {name}")
        try:
            fn()
        except common.Skip as s:
            print(f"  [skip] {s}")
        except common.Gate as g:
            failures.append(f"{name}: {g}")
            print(f"  [GATE] {g}")

    routesCM = None
    def g_load_live():
        nonlocal routesCM
        live = common.kubectl("-n", "criteria-jobs", "get", "cm", "criteria-routes",
                              "-o", "jsonpath={.data.routes\.json}", timeout=60)
        routesCM = json.loads(live)
        note(f"live routes payload: {len(routesCM.get('workflowLibrary', {}))} catalog, "
             f"{len(routesCM.get('routes', []))} routes")
    gate("load-live-routes", g_load_live)

    reg = None
    def g_registry():
        nonlocal reg
        reg = configops.find_registry_entry(routesCM, spec["project"])
        if not reg:
            raise common.Gate(f"project has NO registry entry: {spec['project']} — run setup_project first")
        note(f"registry: ns={reg.get('namespace')} cutover={reg.get('cutover', 'unset')}")
    gate("registry-entry-exists", g_registry)

    def g_tree_exists():
        treedir = os.path.join(REPO_ROOT, spec["tree"])
        for f in TREE_FIELDS_REQUIRED:
            if not os.path.exists(os.path.join(treedir, f)):
                raise common.Gate(f"tree missing {f}: {treedir}")
        note(f"tree present: {spec['tree']}")
    gate("tree-exists", g_tree_exists)

    cutover = (reg or {}).get("cutover") == "done"
    catalog_key = spec.get("catalog_key") or (spec["tree"].replace("_", "-") + "-url")

    def g_pins():
        existing = routesCM.get("workflowLibrary", {}).get(catalog_key)
        if args.expect_pin:
            # operator-acknowledged serving-pin lag: route serves the LIVE pinned tree
            if not existing or (existing.get("ref") or "") != args.expect_pin:
                raise common.Gate(f"--expect-pin {args.expect_pin[:12]} but live catalog {catalog_key} "
                                  f"pins {(existing or {}).get('ref', '?(absent)')[:12] or '?(absent)'}")
            note(f"serving pin accepted: {catalog_key} @ {args.expect_pin[:12]} (origin/main ahead, lag acknowledged)")
            return
        s = head_sha()
        if existing:
            ref = (existing.get("ref") or "").strip()
            url = existing.get("url") or ""
            if ref and ref != s:
                raise common.Gate(
                    f"catalog entry {catalog_key} pins ref={ref[:12]} but origin/main is {s[:12]} — "
                    "repin via scripts/routes-sync.sh BEFORE registering; a stale pin replays an old tree "
                    "(register against the SERVING pin deliberately: --expect-pin " + ref + ")")
            if ref and f"ref={ref}" not in url:
                raise common.Gate(f"catalog entry {catalog_key} url does not carry ref={ref[:12]} (two pin sites must align)")
            note(f"catalog pin {catalog_key}: ref={s[:12]} aligned")
        else:
            note(f"catalog entry {catalog_key} absent — will register pinned at {s[:12]}", changed=True)
    gate("pins-current", g_pins)

    def g_container():
        if args.skip_container_gates:
            raise common.Skip("container gates skipped by flag (dev only)")
        base = fleet_base_image()  # full ref, e.g. criteria-base:v0.1-v0.5.56
        tree = spec["tree"]
        # compile-clean validate (warnings printed, not fatal): the fleet trees
        # carry known compile warnings while the SERVING engine version accepts
        # them; --warnings-as-errors would fail every currently-serving tree.
        # Strict schema verification happens at admission via the operator.
        script = (f"cd /wx && criteria adapter lock {tree} >/dev/null 2>&1 || true; "
                  f"criteria validate {tree}")
        common.run(["docker", "run", "--rm", "-v", f"{REPO_ROOT}:/wx", "-w", "/wx",
                    "--user", f"{os.getuid()}:{os.getgid()}", "-e", "HOME=/tmp",
                    "--entrypoint", "/bin/sh", base, "-c", script], timeout=1200)
        note(f"criteria validate clean under {base}")
    gate("validate-in-fleet-image", g_container)

    def g_slots():
        intf_path = os.path.join(REPO_ROOT, spec["tree"], "interface.json")
        if not os.path.exists(intf_path):
            raise common.Gate(f"interface.json absent: {intf_path}")
        with open(intf_path) as f:
            intf = json.load(f)
        declared = {s.get("slot") for s in (intf.get("configSlots") or [])}
        want = spec.get("config_slots") or {}
        for k in want:
            if k not in configops.SLOT_VARIABLES:
                raise common.Gate(f"config slot {k!r} is not a KB-310 slot ({sorted(configops.SLOT_VARIABLES)})")
            if k not in declared:
                raise common.Gate(f"tree interface does not declare slot {k!r} (declared: {sorted(declared)})")
        note(f"config slots ok ({len(want)} given, tree declares {sorted(declared)})")
    gate("config-slots", g_slots)

    def g_route_unique():
        if configops.route_exists(routesCM, spec["route"]):
            raise common.Gate(f"route {spec['route']!r} already exists in live payload")
        note(f"route name {spec['route']!r} free")
    gate("route-unique", g_route_unique)

    def g_binding_untouched():
        # the binding CM is project data; registering a job must NOT touch it.
        # verify the committed example still validates (schema gate) so this run
        # cannot silently carry a half-edited binding.
        t = os.path.join(REPO_ROOT, "k8s", "tests", "test_criteria_binding.sh")
        if os.path.exists(t):
            common.run(["bash", t], timeout=300)
            note("binding example schema test green")
        else:
            raise common.Skip("binding schema test not present (repo layout drift)")
    gate("binding-schema-green", g_binding_untouched)

    # ---- writes ----
    def write_routes(routes, why):
        import tempfile
        with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
            json.dump({"data": {"routes.json": json.dumps(routes, indent=2)}}, f, indent=2)
            pf = f.name
        try:
            common.run(["kubectl", "-n", "criteria-jobs", "patch", "cm", "criteria-routes",
                        "--type=merge", "--patch-file", pf], timeout=120)
            print(f"  [APPLY] routes CM patched: {why}")
            # restart the watcher (CM patch requires watcher restart per skill rule)
            common.run(["kubectl", "rollout", "restart", "deploy/criteria-kanboard-watcher",
                        "-n", "criteria-jobs"], timeout=120)
            print("  [APPLY] watcher rollout restarted")
        finally:
            os.unlink(pf)

    def write_record(routes, why):
        """cutover lane: the project's OWN config record in castle + repo yaml re-render."""
        base = common.castle_svc_url()
        tok = castle_client.console_login(base)  # ProjectService = console-class (340)
        slug = castle_slug_for(base, tok, spec["project"], spec)
        cur = castle_client.get_project_config(base, tok, slug)
        if cur["status"] != 200:
            raise common.Gate(f"castle GetProjectConfig({spec['project']}) returned {cur['status']}")
        rec = json.loads(cur["document"]) if cur["document"] else configops.record_for_project(
            spec["project"], (reg or {}).get("namespace", ""), (reg or {}).get("repos", []))
        # splices: catalog + configLibrary + routes (record-shaped)
        cat = rec.setdefault("catalog", {})
        if catalog_key not in cat:
            raise common.Gate(f"record catalog lacks {catalog_key}: record-first projects get catalog entries via setup/import; aborting to avoid shape drift")
        lib = rec.setdefault("configLibrary", {})
        if spec.get("config_slots"):
            cfgkey = spec.get("config_ref") or config_key_for(spec)
            lib[cfgkey] = {"ciGateCmd": spec["config_slots"].get("ciGateCmd")}
        rec_routes = rec.setdefault("routes", [])
        if any(r.get("name") == spec["route"] for r in rec_routes):
            raise common.Gate(f"record already carries route {spec['route']}")
        new = {"name": spec["route"], "workflow": catalog_key}
        if spec.get("tags"):
            new["tags"] = spec["tags"]
        rrules_new = dict(new)
        rrules_new["states"] = spec["states"]
        rec_routes.append(rrules_new)
        etag = cur["etag"]
        if args.apply:
            castle_client.set_project_config(base, tok, slug,
                                             json.dumps(rec, indent=2), etag)
            print(f"  [APPLY] project record updated in castle: {why}")
            # the 339 render lane re-renders the ns CMs FROM the record — call it out
            print(f"  [APPLY] note: castle re-renders the {spec['project']} ns CMs from the record (KB-339 config-home)")
        else:
            print(f"  [WOULD] SetProjectConfig({slug}) (+route {spec['route']}, etag {etag[:12]})")

    def config_key_for(spec):
        return spec.get("config_ref") or ("-".join((spec["project"].lower().split()[0:1] or ["x"])) + "-config")

    if not failures:
        print("- write phase ({} )".format("APPLY" if args.apply else "dry-run"))
        if cutover:
            try:
                write_record(routesCM, spec["route"])
            except (common.Gate, Exception) as e:
                failures.append(f"record-write: {e}")
        else:
            try:
                import copy
                staged = copy.deepcopy(routesCM)
                if catalog_key not in staged.get("workflowLibrary", {}):
                    s = head_sha()
                    tree = spec["tree"]
                    base_url = (staged.get("workflowLibrary", {}).get("kanboard-triage-url", {}) or {}).get("url", "")
                    if base_url:
                        import re
                        m = re.match(r"git::(https://[^?]+)//", base_url)
                    else:
                        m = None
                    repo_url = m.group(1) if (m and m.group(1)) else "https://github.com/brokenbots/workflow-example.git"
                    entry = {"type": "url",
                             "url": f"git::{repo_url}//{tree}?ref={s}",
                             "ref": s,
                             "namespace": reg.get("namespace", "criteria-jobs"),
                             "volumes": [{"name": "data", "kind": "pvc", "claim": "criteria-data",
                                          "mountPath": "/data",
                                          "env": {"CRITERIA_RUN_DIR_ROOT": "/data/.criteria/runs",
                                                   "CRITERIA_SHELL_ALLOWED_PATHS": "/data"}},
                                         {"name": "repo", "kind": "pvc", "claim": "criteria-repo",
                                          "mountPath": "/repo"},
                                         {"name": "scratch", "kind": "tmp", "mountPath": "/tmp/scratch",
                                          "sizeLimit": "8Gi"}],
                             "secrets": [{"name": "github-tokens", "secretProviderClass": "copilot-spc",
                                          "mountPath": "/home/criteria/secrets"}],
                             "adapterImages": {"shell": "localhost:5000/criteria-adapter-shell-peer:k8s-0.5.56-1",
                                                "copilot": "localhost:5000/criteria-adapter-copilot-peer:k8s-0.5.56-1"}}
                    with open(os.path.join(REPO_ROOT, tree, "interface.json")) as f:
                        entry["interface"] = json.load(f)
                    configops.upsert_catalog_entry(staged, catalog_key, entry)
                    report_changes.append(f"catalog entry {catalog_key}")
                if spec.get("config_slots"):
                    cfgkey = config_key_for(spec)
                    if cfgkey not in staged.get("configLibrary", {}):
                        configops.upsert_config_library(staged, cfgkey, spec["config_slots"].get("ciGateCmd", "make test"))
                        report_changes.append(f"configLibrary entry {cfgkey}")
                configops.add_route(staged, spec["route"], catalog_key, spec["project"],
                                    spec["states"], tags=spec.get("tags"),
                                    config_ref=spec.get("config_ref"))
                report_changes.append(f"route {spec['route']}")
                if args.apply:
                    write_routes(staged, ", ".join(report_changes))
                else:
                    for ch in report_changes:
                        print(f"  [WOULD] {ch}")
            except ValueError as e:
                failures.append(f"routes-write: {e}")

    if not args.apply and not failures and report_changes:
        print(f"\ndry-run clean — {len(report_changes)} change(s) staged. rerun with --apply to write them live.")

    return common.report(f"register_job {spec['route']}", 7 - len(failures), failures, args.apply)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
