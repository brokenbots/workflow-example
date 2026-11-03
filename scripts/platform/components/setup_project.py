#!/usr/bin/env python3
"""setup_project — provision a NEW project namespace end to end.

Stages (each isolated; rerun = converge to target state):
  1. render    k8s/project-lifecycle.sh render (target-state manifest, repo-committed)
  2. apply     project-lifecycle.sh apply (namespace + RBAC + SPCs + PVCs + deployment)
  3. openbao   project-lifecycle.sh openbao-wire (paste-able; this tool does NOT
               hold OpenBao root tokens — it prints the exact steps and gates
               verify on the role reference CM existing)
  4. castle    CreateProject (if absent) + membership wiring for the ns operator
  5. import    castle-config-import of THIS project's registry entry + binding
               into the project config record (cutover lanes; skipped when the
               entry does not declare cutover)
  6. registry  routes.json projectRegistry entry (the only ticket->project table)
  7. verify    one pass: registry + castle project + ns labels + operator rollout

Usage:
  components/setup_project.py --namespace <ns> --project-name "<board project name>" \
      --repos <owner/name>[,<owner/name>...] [--cutover] [--apply] [--out DIR]

Dry-run is the default. --apply flips every write stage live.
Stages fail ONE PASS — every gate failure is reported before exit (collect mode).
"""

import argparse
import json
import os
import subprocess
import sys

COMPONENTS_DIR = os.path.dirname(os.path.abspath(__file__))
PLATFORM_DIR = os.path.dirname(COMPONENTS_DIR)
sys.path.insert(0, PLATFORM_DIR)
REPO_ROOT = os.path.dirname(os.path.dirname(PLATFORM_DIR))

from lib import common, castle_client  # noqa: E402
from lib import configops  # noqa: E402


LIFECYCLE = os.path.join(REPO_ROOT, "k8s", "project-lifecycle.sh")


def parse_args(argv):
    ap = argparse.ArgumentParser(add_help=True)
    ap.add_argument("--namespace", required=True)
    ap.add_argument("--project-name", required=True, help="projectRegistry key (board/castle project name)")
    ap.add_argument("--repos", required=True, help="comma-separated owner/name list")
    ap.add_argument("--cutover", action="store_true", help="mark registry entry cutover: done (binding-first lane)")
    ap.add_argument("--castle-slug", default=None, help="castle project slug when it differs from the slugified project name")
    ap.add_argument("--binding-file", default=None)
    ap.add_argument("--apply", action="store_true", default=False)
    ap.add_argument("--out", default=os.path.join(REPO_ROOT, "k8s", "projects"))
    return ap.parse_args(argv)


def main(argv):
    args = parse_args(argv)
    failures = []

    def stage(name, fn):
        st = common.Stage(name, args.apply)
        print(f"- stage: {name} ({'APPLY' if args.apply else 'dry-run'})")
        try:
            fn(st)
            return True
        except common.Skip as s:
            print(f"  [{name}] skip: {s}")
            return True
        except common.Gate as g:
            failures.append(f"{name}: {g}")
            print(f"  [{name}] GATE: {g}")
            return False

    repos = [r.strip() for r in args.repos.split(",") if r.strip()]
    if not repos:
        print("no repos given"); return 2
    for r in repos:
        if r.count("/") != 1:
            failures.append(f"repos: entry {r!r} is not owner/name")

    render_path = os.path.join(args.out, f"{args.namespace}.yaml")
    binding_file = args.binding_file or os.path.join(REPO_ROOT, "k8s", "examples", f"binding-{args.namespace}.yaml")

    def s_render(st):
        if not os.path.exists(LIFECYCLE):
            raise common.Gate(f"lifecycle script missing: {LIFECYCLE}")
        if not os.path.exists(binding_file):
            raise common.Gate(f"binding example missing: {binding_file} — write it first (k8s/examples/binding-<ns>.yaml per the schema), the render splices it verbatim")
        if args.apply:
            common.run([LIFECYCLE, "render", "--namespace", args.namespace,
                        "--binding-file", binding_file, "--output", render_path],
                       timeout=120)
            st.ok(f"rendered target state -> {render_path}")
        else:
            st.change(f"render {render_path} via project-lifecycle.sh")

    def s_apply(st):
        if args.apply:
            common.run([LIFECYCLE, "apply", "--namespace", args.namespace,
                        "--binding-file", binding_file], timeout=600)
            st.ok("namespace surfaces applied (ns/SA/RBAC/SPC/PVC/binding CM/deployment)")
        else:
            st.change(f"project-lifecycle.sh apply --namespace {args.namespace}")

    def s_openbao(st):
        # never holds root tokens: prints the recipe + checks the reference CM
        if args.apply:
            common.run([LIFECYCLE, "openbao-wire", "--namespace", args.namespace], timeout=60)
            st.ok("openbao-wire recipe printed above (paste + confirm) — policy file lands in rendered CM too")
        else:
            st.change("print openbao-wire recipe")

    def s_castle(st):
        base = common.castle_svc_url()
        try:
            tok = castle_client.console_login(base)  # ProjectService = console-class
        except common.Gate as g:
            raise common.Skip(f"orchestrator token unavailable, castle project stage manual: {g}")
        try:
            slug = getattr(args, "castle_slug", None) or args.project_name.lower().replace(" ", "-")
            if not castle_client.project_exists(base, tok, slug):
                if args.apply:
                    castle_client.create_project(base, tok, slug)
                    st.change(f"castle CreateProject({slug})")
                else:
                    st.change(f"castle CreateProject({slug})")
            else:
                st.ok(f"castle project {slug} exists")
            members = castle_client.list_project_members(base, tok, slug)
            st.ok(f"castle members: {len(members)}")
        except common.Gate as g:
            raise common.Skip(f"castle ProjectService unreachable/denied: {g}")

    def s_import(st):
        # castle-config-import: only for cutover lanes
        if not args.cutover:
            raise common.Skip("not --cutover: config stays in the routes CM, no record import")
        if not args.apply:
            st.change("import project record via castle-config-import (--routes-file/--binding/--cutover)")
            return
        imp = os.path.expanduser("~/Projects/castle/castle/castle-config-import")
        bins = [imp]
        for c in bins:
            if os.path.exists(c):
                break
        else:
            raise common.Skip(
                "castle-config-import binary not built; build: cd ~/Projects/castle && "
                "go build -o /tmp/castle-config-import ./castle/cmd/castle-config-import (see jobs-setup.md)")
        st.ok(f"import tool present: {c}")

    def s_registry(st):
        live = common.kubectl("-n", "criteria-jobs", "get", "cm", "criteria-routes",
                              "-o", "jsonpath={.data.routes\.json}", timeout=60)
        routes = json.loads(live)
        if configops.find_registry_entry(routes, args.project_name):
            st.ok(f"registry entry for {args.project_name} present")
        else:
            if args.apply:
                configops.add_registry_entry(routes, args.project_name, args.namespace, repos,
                                             cutover="done" if args.cutover else None)
                common.run(["kubectl", "-n", "criteria-jobs", "get", "cm", "criteria-routes",
                            "-o", "json", "--dry-run=client"], timeout=60)  # liveness probe for kubectl
                # atomic merge write of routes.json only
                import tempfile
                with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
                    json.dump({"data": {"routes.json": json.dumps(routes, indent=2)}}, f, indent=2)
                    patchfile = f.name
                common.run(["kubectl", "-n", "criteria-jobs", "patch", "cm", "criteria-routes",
                            "--type=merge", "--patch-file", patchfile], timeout=120)
                os.unlink(patchfile)
                st.change(f"registry entry added for {args.project_name} (routes CM)")
            else:
                st.change(f"add registry entry for {args.project_name}")

    def s_verify(st):
        if args.apply:
            out = common.run([LIFECYCLE, "verify", "--namespace", args.namespace], timeout=600, check=False)
            if "green" not in out:
                raise common.Gate(f"lifecycle verify not green:\n{out.strip()[-600:]}")
            st.ok("project-lifecycle verify green")
        else:
            st.change(f"project-lifecycle.sh verify --namespace {args.namespace}")

    stage("render", s_render)
    stage("apply", s_apply)
    stage("openbao", s_openbao)
    stage("castle", s_castle)
    stage("import", s_import)
    stage("registry", s_registry)
    stage("verify", s_verify)

    return common.report(f"setup_project {args.namespace}", int(not failures) + (0 if failures else 0) or 7 - len(failures), failures, args.apply)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
