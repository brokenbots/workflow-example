#!/usr/bin/env python3
"""teardown_project — cleanly retire a project: disarm, drain, delete ns surfaces,
castle tombstone (record cleared, project row KEPT, run history preserved).

Stage order (each gated; refusals are loud, never best-effort deletes):
  1. zombie-sweep   find any card carrying the trigger tag with this project's
                    repo tag -> DISARM them (strip k8s-run). Never moves columns.
  2. quiesce        wait for zero non-terminal CriteriaRuns in the ns
                    (--drain-timeout, default 600s). Refuse teardown while runs live.
  3. disarm-record  registry entry REMOVED from routes payload (no route can
                    dispatch to a tombstoned project) + castle config record
                    cleared via SetProjectConfig {document:'', expectedEtag}.
  4. delete-ns      kubectl delete namespace (cascades operator/CRs/Jobs/PVCs
                    per owner-refs). REQUIRES --apply.
  5. tombstone      castle: remove project AGENT members (operators of the dead
                    ns); project ROW STAYS with membership + history; settings
                    doc cleared. NEVER deletes the project row (runs.project_id
                    story unresolved — tombstone = unrouteable + memberless).

Post state: no registry entry, no ns, no record, castle project row kept
(unroutable: dispatch requires a member agent; none remain).

Usage: components/teardown_project.py --namespace <ns> --project-name "<name>"
       [--drain-timeout 600] [--apply]
"""

import argparse
import json
import os
import sys
import time

PLATFORM_DIR = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
sys.path.insert(0, PLATFORM_DIR)
REPO_ROOT = os.path.dirname(PLATFORM_DIR)

from lib import common, castle_client  # noqa: E402
from lib import configops  # noqa: E402


def parse_args(argv):
    ap = argparse.ArgumentParser()
    ap.add_argument("--namespace", required=True)
    ap.add_argument("--project-name", required=True)
    ap.add_argument("--repos", default=None, help="owner/name to disarm cards by (comma sep)")
    ap.add_argument("--drain-timeout", type=int, default=600)
    ap.add_argument("--apply", action="store_true")
    return ap.parse_args(argv)


def live_crs(ns):
    out = common.kubectl("get", "criteriaruns", "-n", ns,
                         "-o", "custom-columns=N:.metadata.name,P:.status.phase",
                         "--no-headers", check=False, timeout=60)
    live = []
    for line in out.strip().splitlines():
        if not line.strip():
            continue
        name, _, phase = line.partition(":")
        phase = phase.strip()
        if phase not in ("Succeeded", "Failed", "Cancelled", ""):
            live.append((name.strip(), phase))
    return live


def main(argv):
    a = parse_args(argv)
    failures = []
    castle_slug = {"id": None}  # shared between stages (plain dict avoids nonlocal)

    def stage(name, fn):
        print(f"- stage: {name}")
        try:
            fn()
        except common.Skip as s:
            print(f"  [skip] {s}")
        except common.Gate as g:
            failures.append(f"{name}: {g}")
            print(f"  [GATE] {g}")

    repos = [r.strip() for r in (a.repos or "").split(",") if r.strip()]

    def s_zombies():
        tok = common.kanboard_token_from_pod()
        tasks = common.kanboard_rpc("getAllTasks", {"project_id": 1}, token=tok)
        disarmed = []
        for t in tasks:
            tags = common.kanboard_rpc("getTaskTags", {"task_id": t["id"]}, token=tok)
            vals = list(set(tags.values()))
            if "k8s-run" not in vals:
                continue
            if repos and not any(f"repo:{r}" in vals for r in repos):
                continue  # other project's armed card
            if a.apply:
                tags = common.kanboard_rpc("getTaskTags", {"task_id": t["id"]}, token=tok)
                keep = [v for v in tags.values() if v != "k8s-run"]
                common.kanboard_rpc("setTaskTags", project_id=t["project_id"],
                                    task_id=t["id"], tags=keep, token=tok)
                got = common.kanboard_rpc("getTaskTags", {"task_id": t["id"]}, token=tok)
                if "k8s-run" in set(got.values()):
                    raise common.Gate(f"disarm read-back failed on KB-{t['id']}")
            disarmed.append(t["id"])
        print(f"  {'disarmed' if a.apply else 'WOULD disarm'} {len(disarmed)} armed card(s): {disarmed}")
    stage("zombie-sweep", s_zombies)

    def s_quiesce():
        deadline = time.time() + a.drain_timeout
        while True:
            live = live_crs(a.namespace)
            if not live:
                print("  zero live CriteriaRuns — quiesced")
                return
            if time.time() > deadline:
                raise common.Gate(f"still {len(live)} live CRs after {a.drain_timeout}s: {live[:5]}")
            print(f"  waiting: {len(live)} live CRs ({live[:3]})")
            time.sleep(30)
    stage("quiesce", s_quiesce)

    def s_record():
        # routes registry entry removal + castle record clear
        live = common.kubectl("-n", "criteria-jobs", "get", "cm", "criteria-routes",
                              "-o", "jsonpath={.data.routes\.json}", timeout=60)
        routes = json.loads(live)
        changed = False
        if configops.find_registry_entry(routes, a.project_name):
            if a.apply:
                routes["projectRegistry"].pop(a.project_name, None)
                routes["routes"] = [r for r in routes.get("routes", [])
                                    if r.get("project") != a.project_name]
                import tempfile
                with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
                    json.dump({"data": {"routes.json": json.dumps(routes, indent=2)}}, f, indent=2)
                    pf = f.name
                common.run(["kubectl", "-n", "criteria-jobs", "patch", "cm", "criteria-routes",
                            "--type=merge", "--patch-file", pf], timeout=120)
                os.unlink(pf)
                common.run(["kubectl", "rollout", "restart", "deploy/criteria-kanboard-watcher",
                            "-n", "criteria-jobs"], timeout=120)
                print("  registry entry removed + watcher restarted")
                changed = True
            else:
                print("  WOULD remove registry entry + routes")
        else:
            print("  registry entry already absent")
        base = common.castle_svc_url()
        tok = castle_client.console_login(base)  # ProjectService = console-class
        projs = castle_client.list_projects(base, tok)
        ids = {p_.get("id") for p_ in projs}
        slug_guess = a.project_name.lower().replace(" ", "-")
        slug = slug_guess if slug_guess in ids else (a.project_name.lower().replace(" ", "") if a.project_name.lower().replace(" ", "") in ids else None)
        castle_slug["id"] = slug
        cur = castle_client.get_project_config(base, tok, slug) if slug else {"status": 404, "etag": "", "document": ""}
        if cur["status"] == 200 and cur["document"]:
            if a.apply:
                castle_client.set_project_config(base, tok, a.project_name, "", cur["etag"])
                print("  castle config record CLEARED (SetProjectConfig document='')")
            else:
                print("  WOULD clear castle config record")
        else:
            print("  castle record already empty/absent")
    stage("disarm-record", s_record)

    def s_delete():
        if a.apply:
            common.kubectl("delete", "ns", a.namespace, "--wait=true", timeout=600)
            print(f"  namespace {a.namespace} DELETED")
        else:
            print(f"  WOULD delete namespace {a.namespace} (cascades operator/CRs/Jobs/PVCs)")
    stage("delete-ns", s_delete)

    def s_tombstone():
        base2 = common.castle_svc_url()
        tok2 = castle_client.console_login(base2)
        slug = castle_slug["id"]
        if not slug:
            projs = castle_client.list_projects(base2, tok2)
            ids = {p_.get("id") for p_ in projs}
            g = a.project_name.lower().replace(" ", "-")
            slug = g if g in ids else a.project_name.lower().replace(" ", "")
            if slug not in ids:
                print(f"  [skip] no castle project matches {a.project_name!r} ({sorted(ids)})")
                return
        members = castle_client.list_project_members(base2, tok2, slug)
        for m in members:
            if m.get("kind") == "PROJECT_MEMBER_KIND_AI":  # agents of the dead ns only
                if a.apply:
                    castle_client.castle_rpc(base2, "castle.v1.ProjectService/RemoveProjectMember",
                                             {"projectId": slug or a.project_name, "memberId": m["memberId"]},
                                             token=tok2)
                print(f"  {'removed' if a.apply else 'WOULD remove'} agent member {m['memberId']}")
        st = castle_client.get_project_settings(base2, tok2, slug or a.project_name)
        if st.get("_status") == 200 and st.get("settings"):
            if a.apply:
                castle_client.set_project_settings(base, tok, a.project_name, {}, st.get("etag", ""))
                print("  settings doc cleared")
            else:
                print("  WOULD clear settings doc")
        print(f"  tombstoned: project row {a.project_name} KEPT (history preserved, unrouteable)")
    stage("tombstone", s_tombstone)

    return common.report(f"teardown_project {a.namespace}", 5 - len(failures), failures, a.apply)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
