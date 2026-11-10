# Setting up projects and jobs on the criteria platform

The operator manual for turning a need into a running job on the criteria
workflow platform: a new project namespace, a new job (route) on an existing
project, and the kanboard card that drives it. The tooling is deterministic
Python under `scripts/platform/`; this document is the contract those tools
serve. When this document and a tool disagree, the tool is the bug.

Audience: any agent or human setting up platform work. Nothing here requires
reading the skills; links at the bottom are for deeper diagnosis.

Standing rule (applies to every section):

- This document records MECHANICS and CONTRACTS only — commands, schemas,
  gates, invariants. Never live state (which cards are armed, what is
  deployed, current images). Live state lives on kanboard cards and castle
  records, which is what keeps this document from rotting.

- A card describes what to do; this doc describes how the platform does it.

Every tool is DRY-RUN by default. `--apply` (or nothing) is the only switch
between printing and writing. Every write component is idempotent: rerunning
a completed setup converges (reports "ok/exists", changes nothing).

---

## 1. The pieces

```
scripts/platform/
  platformctl                     dispatch-only wrapper (no logic)
  lib/common.py                   subprocess + kanboard RPC + openbao + reporting
  lib/castle_client.py            castle Connect-RPC (ProjectService, console login)
  lib/configops.py                routes-payload + ProjectWorkflowConfig record ops
  components/setup_project.py     new project namespace, end to end
  components/verify_project.py    one-pass drift + health check per project
  components/teardown_project.py  disarm, drain, delete ns, castle tombstone
  components/register_job.py      register a new job (route) — gated + deterministic
  components/file_card.py         kanboard cards: file dormant / arm / disarm / show
```

Each component runs standalone (`python3 scripts/platform/components/<x>.py
--help`) or through the wrapper. Components never import each other's logic;
they share only the lib/ layer. Adding a component = one file + one wrapper
case row.

The two surfaces a job exists on, decided by the project's `cutover` state
(NOT a flag):

- `cutover: done` project — the project's config record lives IN castle
  (castle.v1 ProjectService Get/SetProjectConfig; console-session writes).
  The routes ConfigMap is not written for these projects.
- registry entry without cutover — the shared `criteria-jobs` routes
  ConfigMap stays the surface (merge-patch `data.routes.json` + watcher
  restart).

The kanboard-watcher is the only CR creator and the operator reconciles CRs
into Jobs; neither is touched by any setup flow here.

## 2. Layer 1 — a new project (one namespace)

Goal state: namespace with labels + project operator + PVCs + binding
ConfigMap + OpenBao role, a castle project row with members, and the
registry entry that routes cards to it.

Prereqs you create by hand first (they are project DECISIONS, not mechanics):

1. `k8s/examples/binding-<ns>.yaml` — the project binding payload
   (images, env, volumes, secrets, config). Schema of record:
   `k8s/criteria-binding.schema.json`; validated fail-closed by
   `k8s/tests/test_criteria_binding.sh`. Copy an existing example and edit
   the project-specific values.
2. The repo list (owner/name) the project owns.
3. The castle project slug (defaults to the lowercased board name; legacy
   rows diverge — pass `--castle-slug`).

Then:

```
scripts/platform/platformctl project setup \
  --namespace <ns> --project-name "<board project name>" \
  --repos <owner/name>[,...] [--cutover] [--castle-slug <slug>] [--apply]
```

Stages, in order, each individually re-runnable:

- render — k8s/project-lifecycle.sh render (target-state manifest into
  k8s/projects/<ns>.yaml; re-render + diff is the drift check)
- apply — project-lifecycle.sh apply (ns, SAs, namespaced RBAC, SPCs, PVCs,
  binding CM spliced verbatim from the example, operator deployment)
- openbao — prints the openbao-wire recipe (this tooling never holds OpenBao
  root tokens; paste the printed block with a root token to wire the role)
- castle — CreateProject if absent (console-session credential class)
- import — cutover lanes only: the castle-config-import run that turns the
  registry entry + binding + routes into the project's config record
- registry — the routes.json projectRegistry entry (the only
  ticket-to-project routing table)

Verification in one pass:

```
scripts/platform/platformctl project verify --namespace <ns> \
  --project-name "<board project name>" [--castle-slug <slug>]
```

Checks (each failure listed, all reported, exit 1 on any): namespace labels,
operator rollout + image, PVC Bound-or-lazily-pending (local-path PVCs bind
at first consumer — Pending with no volumeName is healthy), binding CM
parses, registry entry + castle record identity (cutover lanes: record
registryName/namespace must match, ListWorkflowCatalog must return entries),
kanboard-watcher pod running, armed-zombie sweep (no CLOSED card still
carrying the trigger tag), openbao role reference CM.

First real run on a new project: fire ONE smoke card end to end before
arming anything else (armed-card hygiene depends on the whole chain being
true; verify alone is necessary, not sufficient).

## 3. Layer 2 — a new job on an existing project

A "job" = a workflow route: a tree (directory under repo root), the project
it belongs to, a route name, and its trigger states (board columns for
kanboard projects). Registering is gated, deterministic, and dry-run first.

Write the job spec (JSON, self-contained; schemas below):

```
{
  "tree": "kanboard_develop_v1",
  "project": "Kanboard Tickets",          // projectRegistry key
  "route": "kanboard-develop-go",         // NEW, unique route name
  "states": ["Ready"],                    // trigger states
  "catalog_key": "kanboard-develop-url",  // existing catalog entry; omit to register the tree afresh (pinned at HEAD)
  "tags": ["repo:brokenbots/x"],          // optional: route fires only for cards carrying these
  "config_ref": "criteria-repo",          // optional: configLibrary entry key
  "config_slots": {"baseBranch": "main", "ciGateCmd": "make ci"},  // optional; only KB-310 slots
  "castle_slug": "go-dev"                 // optional: when the board name is not the castle slug
}
```

KB-310 slot vocabulary (the only values castle/interpreter accepts):
`buildCmd`, `testCmd`, `ciGateCmd`, `baseBranch`.

```
scripts/platform/platformctl job register --spec job.json [--apply]
   [--expect-pin <full-serving-sha>]
```

Gates (ALL run; every failure reported in one pass before any write):

- registry entry exists for the project (setup_project first, otherwise)
- tree exists with main.chcl + interface.json
- pins current: an existing catalog entry must pin origin/main HEAD at BOTH
  pin sites (url `?ref=` + top-level `ref`) — a stale pin replays an old
  tree; repin via `scripts/routes-sync.sh` first, or register against the
  serving pin deliberately with `--expect-pin` (the route then serves the
  tree at the LIVE pin, not local HEAD)
- compile-clean `criteria validate <tree>` under the fleet's
  CRITERIA_BASE_IMAGE (read from the operator deployment env, never the
  host binary). Compile warnings are printed, not fatal (the serving trees
  carry known warnings, and the engine accepts them); strict schema
  verification happens at admission
- config slots: every requested slot is a KB-310 slot AND declared by the
  tree's interface.json
- route name unused
- binding schema test green (registering a job must never touch the binding)

Write lane by cutover state: cutover-done projects get the route in their
castle config record (SetProjectConfig, etag-checked; castle re-renders the
namespace CMs from the record); other projects get the routes CM
merge-patch + watcher restart in the same pass. Dry-run prints the exact
delta for both lanes.

`job verify` = the same component in dry-run: it re-checks every gate
without writing.

### Secret variable binding (the --var contract, KB-385)

A job's workflow object declares its secrets as env lanes: each entry maps
a SCREAMING binding env lane name (e.g. `WORKFLOW_UPSTREAM_TOKEN`) to the
secret file key rendered inside the CSI mount — usually that lane's own
lowercase file, but lanes may reuse another lane's file (the remaster's
upstream lane binds the `WORKFLOW_GITHUB_TOKEN` file). The watcher derives
the run's `CRITERIA_SECRET_VARS` pair set from those entries; the source
runner turns it into `criteria apply --var` args; the lane that never
reaches a pair dies the tree's `missing required input(s)` failure.

The contract, pinned in `criteria-k8s/internal/jobbuilder/secretvars.go`:

- the derived pair is `lower(env lane)=file:<mountPath>/<fileKey>` — the
  varname comes from the lane name lowercased, NEVER from the mount file
  key (which is another lane's file in the reuse case);
- the engine binds `--var` overrides by exact name, so the pair name must
  be the tree's lowercase secret-typed variable (KB-304 name parity: every
  binding env lane SCREAMING name ↔ one lowercase tree var declared in
  `variables.chcl` and the interface's `secretEnv`);
- a tree variable an operator pair never feeds stays unbound — the run
  fails loudly at run_handler (`missing required input(s)`), never silently;
- `file:` origins must sit under a declared secret root
  (`CRITERIA_SECRET_FILE_ROOTS`, derived from the same secret entries); the
  engine resolves them eagerly and fails with a typed reason naming the
  variable on any escape. The multi-root containment and this typed reason
  are KB-369 engine behavior from criteria v0.5.58 — older engines confine
  `file:` origins to the single default secrets dir and cannot express
  them, so the probe leg skips its live-engine legs below that floor.

Regression surface: the jobbuilder contract tests
(`criteria-k8s/internal/jobbuilder/secretvars_test.go`) and the live-engine
probe leg (`workstream_handler_v1/tests/test_secret_binding.sh`). Tree
authoring side: see section 8 (criteria-workflow-trees skill).


## 4. Layer 3 — the card: filing, arming, the everyday loop

The card is how a job instance gets work. `file_card` handles the card
mechanics with the arming rules enforced in code (the watcher's fail-closed
tag resolution is NOT something to learn by experiment):

- file: creates a DORMANT card (correct column, NO tags). A dormant card
  cannot fire (trigger gate = k8s-run tag) but is fully visible for review.
- arm: composes the ONLY legal armed shape — `[k8s-run, repo:<owner>/<name>,
  internal-reproduced?, + at most one classification tag from
  KANBOARD_ROUTING_TAGS]` — verifies the tag read-back, then makes the
  trigger-column move. Column ENTRY is the fire action; the watcher creates
  the CriteriaRun within one poll (60s).
- disarm: strips the trigger tag (KEEPs everything else) with read-back.
- show: card + column + tags + closed state.

Hard guards enforced by the tool (all from live incidents, do not bypass):

- arm REFUSES a card sitting in Done — terminal cards are history, never an
  arming surface; finished work must stay un-runnable. File a new card.
- arm aborts on any tag read-back mismatch (the silent no-fire class:
  tags written as ids, digits-as-values, partial sets).
- arm runs the arm-time compile gate before it arms: the route's pinned trees
  must compile under the engine stamp carried by the criteria-base image the
  run will fire on (the last `vX.Y.Z` token in the tag,
  `criteria-base:v0.1-v0.5.56` → engine v0.5.56). The base-roll gate (no roll
  while a run is RUNNING) is orthogonal to compile exposure: a tree is
  compiled only when the card fires, so a card armed before a base roll
  inherits the rolled image on its first fire. A gate failure refuses the arm
  and the card stays disarmed. Gate: `tools/armgate/arm-gate.sh` in the
  criteria repo ([github.com/brokenbots/criteria]
  (https://github.com/brokenbots/criteria)), one `--tree` per pinned tree,
  run with a criteria binary at/above that engine stamp (a tag-exact local
  build suffices when the published release lags); exit 0 = safe to arm,
  1 = gate failed closed, 2 = usage error (nothing was checked).
- file writes NO tag; a "filed-looking" card that never fires is the
  missing-arm state, visible via `card show`.

The everyday loop for "run this ticket through the workflow":

1. `platformctl job register --spec job.json` (dry) — inspect the delta
2. `platformctl job register --spec job.json --apply` — write the route
3. `platformctl card file --project-id <p> --title "..." --description "..."`
   — the description carries repo URL line one + acceptance (the workflow
   agents see ONLY this body)
4. review the dormant card on the board (it is the human checkpoint)
5. run the arm-time compile gate (the third arm guard above): gate the
   route's pinned trees under the engine stamp carried by the
   CRITERIA_BASE_IMAGE the run will fire on — registration compile-checked
   the image as it stood at registration time, so a gate that passed then
   does not cover a base roll that landed while the card sat dormant
6. `platformctl card arm --task-id <id> --repo <owner/name>
   [--classification p1] [--internal-reproduced]`
7. verify fire: the watcher log greps `created CriteriaRun` with the ticket
   (`kubectl -n criteria-jobs logs deploy/criteria-kanboard-watcher
   --since=3m | grep <id>`) — the card flips WIP itself within one poll
8. verify done: the PR merged in the bound repo, THEN the card Done. A green
   run without a merged PR is not done.

## 5. Retiring a project

```
scripts/platform/platformctl project teardown --namespace <ns> \
  --project-name "<board project name>" [--repos <owner/name,...>]
  [--drain-timeout 600] [--apply]
```

Stage order, each refuse-loud (never best-effort deletes):

- zombie-sweep — disarm any card carrying the trigger tag for this
  project's repos (tag strip only; no column moves)
- quiesce — wait for zero non-terminal CriteriaRuns in the ns (refuses
  teardown under live runs; drain-timeout bounds the wait)
- disarm-record — registry entry + routes removed from the dispatch
  surface; castle config record cleared (SetProjectConfig document='')
- delete-ns — the namespace (cascades operator/CRs/Jobs/PVCs per
  owner-refs)
- tombstone — castle agent members removed, settings cleared, project ROW
  KEPT with its run history: unrouteable (no registry entry, no members)
  and unrunnable, but every past run stays queryable. Full project-row
  deletion stays out until the runs.project_id story exists.

Dry-run prints every WOULD. Only `--apply` deletes; there is no undo
past delete-ns except re-running setup_project (which is exactly why the
tombstone keeps the castle row).

## 6. Invariants (violations are gate failures, never workarounds)

- Armed tag set is EXEMPT-SHAPED ONLY. Any other tag on an armed card fails
  route resolution closed on every poll (ErrAmbiguousWorkflow). Board
  metadata tags go on cards via the board UI lanes, never onto armed cards.
- Arming is a deliberate act (tag pass + trigger-column move), never a side
  effect of setup. Files that "just want to run" arm deliberately.
- One write surface per project: cutover-done => castle record;
  pre-cutover => routes CM. Never both.
- Pins have BOTH sites (url ?ref= + top-level ref) and must agree. After
  any tree change: routes-sync (repins) BEFORE registering, else
  --expect-pin with the acknowledged serving sha.
- The binding is project data: setup never edits a live binding; verify
  compares hashes.
- Card descriptions are self-contained (repo URL line one): the workflow
  agents see nothing else.
- Dry-run default everywhere; --apply is the only write switch.

## 7. When something is off

- `project verify` is the entry point: one pass, all failures listed. Fix
  and rerun; the second pass shows only what remains.
- Run/agent failures during a JOB are a DIFFERENT discipline (castle event
  tails, CR liveness, refire policy) — see the criteria incident skills for
  diagnosis depth; this document's job setup stops at "the run fired".
- Route/tag resolution refusals print the offending tag list in the watcher
  log before anything else; read that line before touching routes.

## 8. Pointers

- Stack architecture + component fates: `k8s/README.md`, Option B scope
  doc (~/.hermes/plans/option-b-castle-orchestrator-scope.md)
- Pod topology: docs/pod-topology.md
- Run ops (arm/refire/serialize/close-out discipline) + kanboard JSON-RPC
  quirks: the criteria-k8s-runner + criteria-intake-workflow skills
  (hermes profile skills — agent-diagnosis depth, not setup mechanics)
- Binding schema: k8s/criteria-binding.schema.json; routes schema:
  k8s/routes.schema.json; interface schema: k8s/workflow-interface.schema.json
- Tree authoring: the criteria-workflow-trees skill; repo conventions in
  the repos' AGENTS.md. KB-385 lesson for that skill: a secret-typed tree
  variable is fed by the operator's CRITERIA_SECRET_VARS pair whose varname
  is the lane name lowercased — declare the lowercase var
  (`variables.chcl` + interface `secretEnv` lane), never a screaming-named
  variant, and remember the upstream lane binds the WORKFLOW_GITHUB_TOKEN
  file while contributing no file of its own.
