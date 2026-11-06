#!/usr/bin/env bash
set -euo pipefail

# KB-352 regression test: routes-sync.sh must align BOTH routes pin sites of
# record — criteria-routes (the watcher's routing CM) AND criteria-agent-routes
# (the operator's assignment-agent routes payload, stamped verbatim into
# spec.workflowSource.ref). A repin wave that touches only criteria-routes
# leaves the agent payload stale and every CR the assignment lane stamps bakes
# the OLD pin (live 2026-11-04: kb-131/132/133/135/136 + kb-136 first leg all
# died wait_merge_ci at the old 600s merge-CI budget while criteria-routes had
# moved to the 1200s repin) — the second pin site must no longer survive a
# repin silently.
#
# The script drives a fake kubectl (state kept in a temp dir), so the suite
# runs with no cluster access. Asserts:
#   apply     — a HEAD move re-pins url ref= AND the top-level ref key on BOTH
#               CMs; the operator reload checklist prints (the lane resolves
#               its routes payload once at operator startup: zero Running CRs
#               check + rollout restart before re-arming);
#   --check   — exit 0 when both CMs align at HEAD; exit 1 listing BOTH CMs
#               when both are stale; exit 1 naming criteria-agent-routes when
#               only that CM drifted (the silent-stale agent payload);
#   missing/malformed CM — fail closed BEFORE any apply (no partial sync);
#   --export-dispatch — keeps its criteria-routes scope: no routes array in
#               the agent CM, and export must not require it.
#
# The pin rule under test is the existing one (url `ref=` regex + 40-hex
# top-level ref), applied unchanged on the second CM.

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SYNC="$REPO_ROOT/scripts/routes-sync.sh"

fail() {
    echo "FAIL: $1" >&2
    exit 1
}

command -v python3 >/dev/null 2>&1 || fail "python3 is required"
command -v git >/dev/null 2>&1 || fail "git is required"
[ -f "$SYNC" ] || fail "scripts/routes-sync.sh is missing"

WORK="$(mktemp -d /tmp/routes-sync-test.XXXXXX)"
trap 'rm -rf "$WORK"' EXIT
FAKEBIN="$WORK/bin"
STATE="$WORK/cluster"
LOG="$WORK/sync.log"
mkdir -p "$FAKEBIN" "$STATE"

HEAD="$(git -C "$REPO_ROOT" rev-parse HEAD)"
HEAD_SHORT="${HEAD:0:7}"
OLD_SHA="aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" # 40-hex, must differ from HEAD
[ "$HEAD" != "$OLD_SHA" ] || OLD_SHA="bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

ENTRIES="kanboard-triage-url kanboard-develop-url kanboard-ticket-cleanup-url"

# fake kubectl: routes-sync's exact call surface, backed by one JSON file per CM
cat > "$FAKEBIN/kubectl" <<'FAKEEOF'
#!/usr/bin/env bash
set -euo pipefail
STATE="${FAKE_KUBE_STATE:?FAKE_KUBE_STATE must be set}"
sub=""
name=""
file=""
while [ $# -gt 0 ]; do
    case "$1" in
        -n) shift 2 ;;
        -o) shift ;;
        -f) file="$2"; shift 2 ;;
        get|apply) sub="$1"; shift ;;
        cm) shift ;;
        *)
            if [ -z "$name" ]; then
                name="$1"
            fi
            shift
            ;;
    esac
done
case "$sub" in
    get)
        store="$STATE/$name.json"
        if [ ! -f "$store" ]; then
            echo "Error from server (NotFound): configmaps \"$name\" not found" >&2
            exit 1
        fi
        cat "$store"
        ;;
    apply)
        name="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["metadata"]["name"])' "$file")"
        cp -f "$file" "$STATE/$name.json"
        echo "configmap/$name configured"
        ;;
    *)
        echo "fake kubectl: unsupported subcommand: $sub" >&2
        exit 2
        ;;
esac
FAKEEOF
chmod +x "$FAKEBIN/kubectl"

write_cm() { # $1=cm name $2=ref pin value $3=payload mode (plain|routed)
    python3 - "$STATE" "$1" "$2" "$3" <<'PYEOF'
import json, sys
state, cm, ref, mode = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4]
payload = {
    "apiVersion": "criteria.brokenbots.dev/v1",
    "kind": "Routes",
    "workflowLibrary": {
        entry: {
            "type": "url",
            "url": f"https://github.com/brokenbots/workflow-example.git//kanboard_develop_v1?ref={ref}",
            "ref": ref,
        }
        for entry in ("kanboard-triage-url", "kanboard-develop-url", "kanboard-ticket-cleanup-url")
    },
    "routes": [],
    # routes-sync stamps this watermark into the payload on every render, so a
    # CM "at ref" models the post-apply state with the same watermark
    "metadata": {"renderedFrom": ref},
}
if mode == "routed":
    payload["routes"] = [{
        "name": "kanboard-develop",
        "workflow": "kanboard-develop-url",
        "project": "Kanboard Tickets",
        "tags": ["Ready for Development"],
        "states": ["Ready"],
    }]
    payload["projectRegistry"] = {
        "Kanboard Tickets": {"namespace": "criteria-jobs", "repos": ["brokenbots/workflow-example"]},
    }
doc = {
    "apiVersion": "v1",
    "kind": "ConfigMap",
    "metadata": {"name": cm, "namespace": "criteria-jobs", "resourceVersion": "1"},
    "data": {"routes.json": json.dumps(payload, indent=2)},
}
with open(f"{state}/{cm}.json", "w") as fh:
    json.dump(doc, fh)
PYEOF
}

read_pin() { # $1=cm $2=entry $3=url|ref -> pin value from the fake cluster state
    python3 - "$STATE/$1.json" "$2" "$3" <<'PYEOF'
import json, sys
doc = json.load(open(sys.argv[1]))
print(json.loads(doc["data"]["routes.json"])["workflowLibrary"][sys.argv[2]][sys.argv[3]])
PYEOF
}

assert_pin() { # $1=cm $2=entry $3=url|ref $4=expected
    got="$(read_pin "$1" "$2" "$3")"
    [ "$got" = "$4" ] || fail "$1/$2 $3 pin = $got, want $4"
}

read_url_ref() { # $1=cm $2=entry -> the ref= parameter embedded in the entry's url
    read_pin "$1" "$2" url | sed -n 's/.*ref=\([0-9a-f]*\)$/\1/p'
}

assert_pin_url_ref() { # $1=cm $2=entry $3=expected ref= parameter
    got="$(read_url_ref "$1" "$2")"
    [ "$got" = "$3" ] || fail "$1/$2 url ref= parameter = '$got', want $3"
}

run_sync() { # $1=mode argument ("" = apply); rc -> SYNC_RC, output -> $LOG
    SYNC_RC=0
    if [ "$1" = "" ]; then
        FAKE_KUBE_STATE="$STATE" PATH="$FAKEBIN:$PATH" bash "$SYNC" > "$LOG" 2>&1 || SYNC_RC=$?
    else
        FAKE_KUBE_STATE="$STATE" PATH="$FAKEBIN:$PATH" bash "$SYNC" "$1" > "$LOG" 2>&1 || SYNC_RC=$?
    fi
}

assert_log_has() {
    grep -qF -- "$1" "$LOG" || fail "sync log missing '$1':
$(cat "$LOG")"
}

assert_log_lacks() {
    if grep -qF -- "$1" "$LOG"; then
        fail "sync log unexpectedly contains '$1':
$(cat "$LOG")"
    fi
}

snapshot_cm() { # $1=cm -> byte copy in $WORK/$1.before
    cp -f "$STATE/$1.json" "$WORK/$1.before"
}

assert_cm_unchanged() { # $1=cm — the fake cluster state must be byte-identical
    cmp -s "$WORK/$1.before" "$STATE/$1.json" || fail "$1 was mutated when the sync must have failed closed first"
}

# 1. APPLY: a HEAD move re-pins BOTH CMs (the core regression — the agent CM
#    is no longer left behind by a repin wave), and the reload checklist prints.
write_cm criteria-routes "$OLD_SHA" plain
write_cm criteria-agent-routes "$OLD_SHA" plain
run_sync ""
[ "$SYNC_RC" -eq 0 ] || fail "apply exited $SYNC_RC (log follows):
$(cat "$LOG")"
for cm in criteria-routes criteria-agent-routes; do
    for entry in $ENTRIES; do
        assert_pin_url_ref "$cm" "$entry" "$HEAD"
        assert_pin "$cm" "$entry" ref "$HEAD"
    done
done
assert_log_has "criteria-routes + criteria-agent-routes CMs updated"
assert_log_has "kubectl -n criteria-jobs get criteriaruns"
assert_log_has "kubectl -n criteria-jobs rollout restart deploy/criteria-k8s-operator"
echo "PASS: apply re-pins both CMs at HEAD and prints the reload checklist"

# 2. --check exits 0 and reports both CMs when both align at HEAD.
write_cm criteria-routes "$HEAD" plain
write_cm criteria-agent-routes "$HEAD" plain
run_sync "--check"
[ "$SYNC_RC" -eq 0 ] || fail "--check exited $SYNC_RC on aligned CMs (log follows):
$(cat "$LOG")"
assert_log_has "criteria-routes in sync with HEAD"
assert_log_has "criteria-agent-routes in sync with HEAD"
assert_log_has "both CMs aligned with HEAD"
echo "PASS: --check exits 0 when both CMs align at HEAD"

# 3. --check exits 1 and lists drift on BOTH CMs when both are stale.
write_cm criteria-routes "$OLD_SHA" plain
write_cm criteria-agent-routes "$OLD_SHA" plain
run_sync "--check"
[ "$SYNC_RC" -eq 1 ] || fail "--check exited $SYNC_RC on double-stale CMs, want 1"
assert_log_has "criteria-routes DRIFTED"
assert_log_has "criteria-agent-routes DRIFTED"
assert_log_has "kanboard-develop-url url pin"
assert_log_has "kanboard-develop-url ref pin"
assert_log_has "aaaaaaa" # OLD_SHA display form (7-hex, as reported in drift detail)
assert_log_has "$HEAD_SHORT"
echo "PASS: --check lists drift on both CMs when both are stale"

# 4. --check catches the agent CM drifting ALONE: a criteria-routes-only repin
#    wave must not be able to survive --check silently.
write_cm criteria-routes "$HEAD" plain
write_cm criteria-agent-routes "$OLD_SHA" plain
run_sync "--check"
[ "$SYNC_RC" -eq 1 ] || fail "--check exited $SYNC_RC with only the agent CM stale, want 1"
assert_log_has "criteria-routes in sync with HEAD"
assert_log_has "criteria-agent-routes DRIFTED"
echo "PASS: --check names criteria-agent-routes when only that CM drifted"

# 5. APPLY on the same shape repairs the agent CM: the apply covers both CMs.
run_sync ""
[ "$SYNC_RC" -eq 0 ] || fail "apply exited $SYNC_RC with only the agent CM stale"
for cm in criteria-routes criteria-agent-routes; do
    for entry in $ENTRIES; do
        assert_pin_url_ref "$cm" "$entry" "$HEAD"
        assert_pin "$cm" "$entry" ref "$HEAD"
    done
done
echo "PASS: apply heals the stale agent CM back to HEAD"

# 6. Missing agent CM fails closed BEFORE any mutation — no partial apply, and
#    --check refuses to certify "in sync" on an unverifiable pin site.
rm -f "$STATE"/*.json
write_cm criteria-routes "$OLD_SHA" plain
snapshot_cm criteria-routes
run_sync ""
[ "$SYNC_RC" -ne 0 ] || fail "apply exited 0 with criteria-agent-routes missing"
assert_log_has "criteria-agent-routes"
assert_log_lacks "CMs updated" # the failure must precede any mutation
assert_cm_unchanged criteria-routes
run_sync "--check"
[ "$SYNC_RC" -ne 0 ] || fail "--check exited 0 with criteria-agent-routes missing"
assert_log_has "criteria-agent-routes"
echo "PASS: missing agent CM fails closed (both modes, no partial apply)"

# 7. A CM whose routes.json payload cannot be parsed fails closed too.
write_cm criteria-agent-routes "$OLD_SHA" plain
python3 - "$STATE" <<'PYEOF'
import json, sys
state = sys.argv[1]
path = f"{state}/criteria-agent-routes.json"
doc = json.load(open(path))
doc["data"]["routes.json"] = json.dumps({"kind": "Routes"}, indent=2)  # no workflowLibrary
with open(path, "w") as fh:
    json.dump(doc, fh)
PYEOF
snapshot_cm criteria-routes
run_sync ""
[ "$SYNC_RC" -ne 0 ] || fail "apply exited 0 on a payload without workflowLibrary"
assert_log_has "criteria-agent-routes"
assert_log_has "workflowLibrary"
assert_log_lacks "CMs updated" # the payload error must precede any mutation
assert_cm_unchanged criteria-routes
echo "PASS: malformed agent payload fails closed before any apply"

# 8. --export-dispatch keeps its criteria-routes scope: the routes array lives
#    in criteria-routes, and export must not require the agent CM to exist.
rm -f "$STATE"/*.json
write_cm criteria-routes "$HEAD" routed
run_sync "--export-dispatch"
[ "$SYNC_RC" -eq 0 ] || fail "--export-dispatch exited $SYNC_RC (log follows):
$(cat "$LOG")"
assert_log_has '"dispatch"'
assert_log_has "exported record 'Kanboard Tickets'"
echo "PASS: --export-dispatch stays criteria-routes-scoped"

echo "PASS: routes-sync dual-CM pin alignment (KB-352) — apply/--check cover criteria-routes + criteria-agent-routes"
