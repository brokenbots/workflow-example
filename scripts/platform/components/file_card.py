#!/usr/bin/env python3
"""file_card — file a kanboard card for a registered job, DORMANT (never armed).

Dormant posture is the SAFE default: no k8s-run trigger tag, description
carries repo URL line one + acceptance, sits in the trigger column but
CANNOT fire (trigger gate = k8s-run tag absent).
Arming stays the deliberate move: platformctl card arm <task_id> (a separate
component op that composes the exempt-shaped tag set + fires on column entry).

Usage:
  components/file_card.py --project-id 1 --title "..." --description "..."
  components/file_card.py arm --task-id 123 --repo owner/name [--classification p0|p1|p2]
  components/file_card.py disarm --task-id 123
  components/file_card.py show --task-id 123

Tag-set rules (the watcher's exempt shape; ANY other tag fails route
resolution closed — dual-ticket-watchers pitfall):
  armed = [k8s-run, internal-reproduced?, repo:<owner>/<name>, + at most one
  classification tag in KANBOARD_ROUTING_TAGS ('criteria','operator','p0'...)]
"""

import argparse
import json
import os
import sys

PLATFORM_DIR = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
sys.path.insert(0, PLATFORM_DIR)

from lib import common  # noqa: E402

TRIGGER_TAG = "k8s-run"
DEFAULT_ROUTING_TAGS = ["criteria", "operator", "p0", "p1", "p2"]


def kanboard():
    tok = common.kanboard_token_from_pod()
    url = os.environ.get("KANBOARD_RPC_URL", common.KANBOARD_RPC_FALLBACK)

    def call(method, **params):
        return common.kanboard_rpc(method, params, token=tok, url=url)
    return call


def get_columns(kb, project_id):
    cols = kb("getColumns", project_id=project_id)
    return {c["title"]: c["id"] for c in cols}


def read_tags(kb, task_id):
    tags = kb("getTaskTags", task_id=task_id)
    return sorted(set(tags.values()))


def main(argv):
    ap = argparse.ArgumentParser()
    ap.add_argument("op", nargs="?", default="file",
                    help="one of: file (default), arm, disarm, show")
    ap.add_argument("--project-id", type=int, default=1)
    ap.add_argument("--title")
    ap.add_argument("--description")
    ap.add_argument("--task-id", type=int)
    ap.add_argument("--repo", help="owner/name for the repo routing tag")
    ap.add_argument("--classification", default=None, help="p0|p1|p2 (routing tag, optional)")
    ap.add_argument("--internal-reproduced", action="store_true")
    ap.add_argument("--column", default=None, help="target column title for file (default: project's first trigger col 'Backlog')")
    args = ap.parse_args(argv)

    kb = kanboard()

    if args.op == "file":
        if not args.title or not args.description:
            print("file needs --title and --description"); return 2
        if args.repo and args.repo.count("/") != 1:
            print("--repo must be owner/name"); return 2
        cols = get_columns(kb, args.project_id)
        target = args.column or "Backlog"
        if target not in cols:
            print(f"unknown column {target!r}; columns={sorted(cols)}"); return 2
        tid = kb("createTask",
                 project_id=args.project_id,
                 title=args.title,
                 description=args.description,
                 column_id=cols[target],
                 color="cyan")
        if not isinstance(tid, int):
            print(f"createTask returned non-int {tid!r} — check card actually created"); return 1
        # dormant: NO tags written at all. A no-tag card cannot fire (skips on
        # missing repo URL/trigger tag) but IS visible for review/arming.
        print(f"created DORMANT card KB-{tid} (project {args.project_id}, column {target}, no tags)")
        print("arming is deliberate: platformctl card arm --task-id", tid, "--repo <owner/name>")
        return 0

    if args.op == "arm":
        if not args.task_id or not args.repo:
            print("arm needs --task-id and --repo"); return 2
        tags = [TRIGGER_TAG, f"repo:{args.repo}"]
        if args.internal_reproduced:
            tags.append("internal-reproduced")
        if args.classification:
            if args.classification not in DEFAULT_ROUTING_TAGS:
                print(f"classification tag {args.classification!r} not in routing tags {DEFAULT_ROUTING_TAGS}")
                return 2
            tags.append(args.classification)
        cur = kb("getTask", task_id=args.task_id)
        if not cur:
            print(f"task {args.task_id} not found"); return 1
        col = get_columns(kb, cur["project_id"])
        inv = {v: k for k, v in col.items()}
        cur_col = inv.get(cur.get("column_id"), "?")
        if cur_col == "Done":
            print(f"refusing to arm KB-{args.task_id}: it sits in Done (terminal history, not an "
                  "arming surface). A Done card must stay un-runnable (dave rule: finished cards "
                  "un-runnable). File a NEW card for new work.")
            return 1
        target_col = "Ready" if "Ready" in col else args.column
        # write tags with REPLACE semantics + explicit read-back
        ok = kb("setTaskTags", project_id=cur["project_id"], task_id=args.task_id, tags=tags)
        got = read_tags(kb, args.task_id)
        if sorted(got) != sorted(tags):
            print(f"tag read-back mismatch: wrote {tags} read {got} — arming aborted"); return 1
        moved = kb("moveTaskPosition", project_id=cur["project_id"], task_id=args.task_id,
                   column_id=col[target_col], position=1, swimlane_id=1)
        now = kb("getTask", task_id=args.task_id)
        print(f"armed KB-{args.task_id}: tags={got} column={now.get('column_id')} (was {cur.get('column_id')})")
        print("verify fire within 2 polls: kubectl -n criteria-jobs logs deploy/criteria-kanboard-watcher --since=3m | grep", args.task_id)
        return 0

    if args.op == "disarm":
        if not args.task_id:
            print("disarm needs --task-id"); return 2
        cur = kb("getTask", task_id=args.task_id)
        if not cur:
            print(f"task {args.task_id} not found"); return 1
        tags = [t for t in read_tags(kb, args.task_id) if t != TRIGGER_TAG]
        kb("setTaskTags", project_id=cur["project_id"], task_id=args.task_id, tags=tags)
        got = read_tags(kb, args.task_id)
        print(f"disarmed KB-{args.task_id}: tags now {got}")
        return 0

    if args.op == "show":
        if not args.task_id:
            print("show needs --task-id"); return 2
        cur = kb("getTask", task_id=args.task_id)
        cols = get_columns(kb, cur["project_id"])
        inv = {v: k for k, v in cols.items()}
        print(f"KB-{args.task_id}: {cur.get('title')}")
        print(f"  column: {inv.get(cur.get('column_id'))} tags: {read_tags(kb, args.task_id)}")
        print(f"  active: {cur.get('is_active')} (closed tasks: is_active=0)")
        return 0

    print(f"unknown op {args.op!r}"); return 2


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
