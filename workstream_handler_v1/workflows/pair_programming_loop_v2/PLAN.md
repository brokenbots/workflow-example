# pair_programming_loop_v2 — WAVE-2 (body pass executed)

Status: **BODY WIRED — the part graph is fully authored; UNCOMMITTED (wave-2
body pass keeps the tree in the working set).** TODO-1 and TODO-7 are the
next lanes: the typed-verdict payload widening TODO-1 is dropped in this
pass (the per-step reason payloads carried the plan and findings verbatim),
and TODO-7 (the parent's dial in workstream_handler_v1's main.chcl) is the
parent handler's next change.

## What exists (this tree as wired)

- `main.chcl` — workflow block (version 1, criteria_version
  `">=0.5.9, >=0.5.52-rc1, <0.6.0"`, name pair_loop_v2, initial_state
  get_branch_name, target_state done, policy max_total_steps 100), the
  data.internal surface (plan_json/worktree_status added by this pass),
  the v1-verbatim body entry contract, the planning leg (plan_parts with
  the typed plan-JSON arm -> reduce_plan_parts' jq validation ->
  route_part_slots' flat always-enter-at-p1 routing with try()-wrapped
  total conditions — the skeleton's enter-at-slot-N defect closed), the
  per-slot chains (run_part_pN_fast/deep lanes by the plan's own effort
  -> pN_verify (shared deterministic verify) -> pN_refute -> pN_remediate
  (typed copilot remediation) -> pN_reverify (re-stamp, KB-151) ->
  route_slot_pN gated on the REDUCED count), the v1-verbatim checkpoint
  chain (push_checkpoint's widened inputs: planned slots part_ready before
  checkpoint_local -> preserve_pending -> sync_checkpoint_upstream ->
  resume_after_preservation), the whole-tree verdict composition
  (switch_verdict_route's deterministic aggregate over the reduced count),
  write_review_log/commit_review_log/push_review_checkpoint ->
  triage_review -> done/failed, the fail-* typed-fail bookkeeping steps
  (KB-374 shapes: both arms route + typed reasons), and the outputs
  (pair_verdict, failure_reason).
- `adapters.chcl` — remote worktree env (per_scope_sessions, v1
  verbatim), shell sh, copilot pair_part_planner, copilot
  pair_part_pN_dev (fast lane, x4) + pair_part_pN_dev_deep (deep lane,
  x4 NEW: planner model/reasoning/provider, developer max_turns),
  copilot pair_part_pN_remediator (x4, system_prompt developer.md).
- `variables.chcl` — the v1 pair-loop interface verbatim plus the v2
  knobs (max_pair_cycles, planner, remediator).
- `interface.json` — mounts + the ciGateCmd config slot (rides the
  per-part verify's gate); no secretEnv (LOCAL-ONLY).
- `.criteria.lock.hcl` — 13 copilot entries (planner + 4 fast + 4 deep +
  4 remediators) all pinned copilot 0.5.17 digest
  sha256:d4400abc... , plus shell sh 0.5.5.
- `agents/` — pair_part_planner.md (the plan-JSON charter),
  pair_plan_prompt.md.tftpl (the plan-parts brief), developer.md (the
  per-part developer charter for BOTH lanes), pair_part_brief.md.tftpl
  (per-part brief; strings pre-joined), pair_part_remediate.md.tftpl
  (mechanical remediation charter), pair_part_verdict_log.md.tftpl
  (log body), pair_not_ready.md.tftpl (typed failure report), plus the
  v1-copied supporting agents (missing_developer_prompt,
  develop_workstream, developer_with_worktree_status, develop_continue,
  review_workstream, checkpoint_recover).
- `scripts/` — reduce_plan_parts.sh.tftpl (jq plan gate), the SHARED
  pair_part_verify.sh.tftpl (branch/HEAD-vs-entry_sha/clean-tree/gate
  asserts; x8 call sites: 4 verify + 4 reverify),
  switch_verdict_route.sh.tftpl (aggregate), verify_checkpoint_pushed.sh.tftpl
  (widened: planned slots part_ready), and the v1-copied mechanical
  scripts (checkout_branch, capture_entry_sha, check_working_tree,
  commit_pending_changes, write_review_log, commit_review_log).

## The body flip (v2 vs v1)

v1: develop <-> review cycling inside ONE copilot session per role; the
verdict travels through the legacy reason lane; review triage over the
whole workstream.

v2: the pair loop SPLITS the workstream into PARTS (p1..p4 slot budget),
one fresh agent call per part (fast/deep lanes per the planned effort; a
copilot instance per lane; conversations never cross parts — the
KB-257/CRI-145 never-opened-instance pattern), each producing a typed
verdict the tree's OWN data.internal asserts (data.internal.pair_slot_pN_verdict,
re-stamped from the verify step's deterministic truth before every
consumer — KB-151). The tree — not the agent — tracks every verdict.

## Constraint register (non-negotiable)

- criteria_version `">=0.5.9, >=0.5.52-rc1, <0.6.0"` — LOCKED.
- NO bare contains(<string>) anywhere. Both stdout membership checks are
  split-list membership: push_checkpoint's resumed_no_work probe
  (`contains(split(output.stdout, "\n"), "resumed_no_work=1")`, v1
  verbatim), and the part brief's context probe.
- && does NOT short-circuit in this engine: every switch condition touching
  plan_json is try()-wrapped TOTAL.
- Every non-iterating step carries both outcome paths (KB-374) — verified
  by the compiler's outcome-completeness check.
- Scripts render POSIX sh (no bashisms, no `local`).
- Nested-dial subworkflow sources resolve relative to the tree dir
  (`source = "../upstream_sync"`).
- The copilot adapter does NOT expose a per-step outcome output — the
  tree tracks every verdict in data.internal itself; no template or step
  reads steps.<agent>.outcome anywhere.
- Contract mode is PER-STEP: every copilot outcome on a typed step
  carries `schema = object({ reason = string })` (no bare siblings); the
  shell steps stay legacy-mode (untyped) — never author a mixed step.
- KB-151 single source of truth: the tree's OWN data.internal is the
  loop's verdict register; downstream steps verify against it, never
  re-judge it. Every verdict hop RE-STAMPS its consumer read before the
  next hop.
- LOCAL-ONLY (remaster ruling 1): zero pushes/fetches/clones in this
  tree; the only network surface is the upstream_sync subworkflow.

## Remaining TODOs

- TODO-1 (typed-verdict payload widening): SKIPPED/settled — the
  per-step `object({ reason = string })` payload carried the plan JSON
  and the findings verbatim; the verdict material rides the tree's own
  slot verdicts/findings data.internal. No `{verdict, findings}` widening
  was needed this pass. Next wave decides whether the parent consumes a
  wider per-slot payload.
- TODO-7 (the parent dial): run_pair_programming_loop_v2 + inputs + the
  failure lane's try() in workstream_handler_v1's main.chcl — the parent
  handler's own change (carded lane), not this tree's surface.


## Wave-2 close state (2026-11-07)

- TODO-1 settled-not-needed: `object({ reason = string })` payloads carry the plan
  JSON and findings verbatim; no `{verdict, findings}` widening required.
- TODO-7 CLOSED: the handler's `pair_programming_loop` and
  `pair_programming_loop_feedback` subworkflow decls source-swap to
  `pair_programming_loop_v2`; log_file no longer passed (ruling 12). Round trees
  (`_round2`/`_round3`) stay on v1 until Stage 3 folds the round plumbing.
- Validates: parent handler tree 0 Errors (fleet stamp v0.5.58 container);
  v2 tree 0 Errors. harness_contracts_v1 failure verified pre-existing on clean
  HEAD (stash baseline) — untouched trees, out of this wave's lane.
- Tree tests: reduce_plan_parts jq gate + shared pair_part_verify exercised both
  ways (positive + negative arms) against synthetic repos; CI_GATE_CMD unset-arm
  defect caught in re-verification and fixed (`${CI_GATE_CMD:-}`).

## Stage-3 close state (2026-11-07 — KB-386): the fold and the round-tree deletion are DONE; the gate stays open on the operator's real-card parity run

Card: **KB-386** ("Stage 3 - fold round plumbing onto pair-loop v2"); plan
`.hermes/plans/2026-11-06_123002-pair-loop-remaster-local-upstream-plus-planner.md`
(Stage-2 section "Sequencing"). Rulings honoured: ruling 1 (LOCAL-ONLY — this part
adds no network surface; the tree's only one is the `upstream_sync` delegation),
and the house rule that deterministic shell truth outranks agent judgement.

**The referenced `.hermes` plan file is NOT PRESENT on this machine.** There is no
`.hermes/` directory at the repo root, no `.hermes` directory anywhere on the
filesystem, and no `.hermes` path tracked in git (`git ls-files | grep hermes` is
empty). The dispositions below were therefore taken from the card's own
acceptance text and from this plan's own record (plus `PARITY.md`) — never from
the plan file, and nothing here is attributed to a plan-file section that could
not be read.

### 3.1 Fold disposition (as delivered)

- **ALL FOUR handler dials point at this tree.** The handler's
  `pair_programming_loop`, `pair_programming_loop_feedback`,
  `pair_programming_loop_feedback_r2` and `pair_programming_loop_feedback_r3`
  subworkflow decls all carry `source = "./workflows/pair_programming_loop_v2"`
  — `workstream_handler_v1/main.chcl` lines 174/175, 192/193, 221/222 and
  236/237. (The r2/r3 source-swap landed in part 1; the base + feedback dials
  landed in wave 2.)
- **The r2/r3 hand-off re-entries RIDE this tree.** A hand-off re-enters
  `pair_programming_loop_v2` — the same tree the base and feedback dials run —
  so the tree splits the workstream into parts and every part is remediated
  INSIDE the tree (per-part develop -> verify -> refute -> remediate ->
  reverify). The round-suffixed copilot instances (`developer_r2`/`reviewer_r2`,
  `developer_r3`/`reviewer_r3`) and the round trees' ladders are retired.
- **The r2/r3 round trees are DELETED.** `workflows/pair_programming_loop_round2/`
  and `workflows/pair_programming_loop_round3/` are gone — deleted in this card
  (commit `7dcedc1`). `git ls-files` returns 0 paths under either tree and the
  directories are absent from the worktree. The fold makes them dead weight: no
  dial source, route binding, instance declaration or CI target points at them
  anywhere in the repo — `workstream_handler_v1/tests/test_pair_loop_fold_invariants.sh`
  asserts exactly that, repo-wide.
- **The v1 base tree `workflows/pair_programming_loop` is RETAINED** — it is the
  parity BASELINE, and the fold deleted only the round trees. The deletion does
  not impair the parity comparison: the baseline tree is what the comparison
  needs, it is untouched, and it can be re-run at any time to re-measure the
  candidate against it.

### 3.2 Observed validation sweep (run in this card's part 3)

Every leg below was RUN in this part and its exit code was OBSERVED. No leg is
reported that was not run, and no result is carried over from another part's
run. Compiler in use: `criteria` **v0.5.58**.

`criteria validate` over every tree the repo's `validate` surface covers:

| Leg | Exit | Observed result |
|------|------|-----------------|
| `ticket_cleanup_v1` | 0 | ok (warning: `shell` adapter schema unverified) |
| `decision_demo_v1` | 0 | ok (warning: `decision` adapter schema unverified) |
| `decision_demo_local_v1` | 0 | ok (warning: `decision` adapter schema unverified) |
| `harness_contracts_v1` | **1** | **compile failed — 4 outcome-completeness errors. PRE-EXISTING; see 3.3** |
| `harness_composition_v1` | 0 | ok |
| `harness_composition_v1/tests/fixtures/g11_dangling_env` | 0 | ok |
| `harness_failures_v1` | 0 | ok |
| `harness_failures_v1/tests/fixtures/e24_stall_wedge` | 0 | ok |
| `harness_failures_v1/tests/fixtures/e30_stale_pin` | 0 | ok |
| `harness_failures_v1/tests/fixtures/g12_silent_change` | 0 | ok |
| `harness_failures_v1/tests/fixtures/g12_silent_change/workflows/callee_b` | 0 | ok |
| `harness_fleet_smoke_v1` | 0 | ok |
| `harness_fleet_smoke_v1/tests/fixtures/digest_miss` | 0 | ok (warning: `shell` adapter schema unverified) |
| `harness_wire_v1` | 0 | ok |
| `workstream_handler_v1` | 0 | ok (3 informational same-step read/write warnings) |

`criteria validate` over the surviving subtrees:

| Leg | Exit | Observed result |
|------|------|-----------------|
| `workstream_handler_v1/workflows/pair_programming_loop` (v1 baseline, RETAINED) | 0 | ok |
| `workstream_handler_v1/workflows/pair_programming_loop_v2` (this tree) | 0 | ok |
| `workstream_handler_v1/workflows/branch_manager` | 0 | ok |
| `workstream_handler_v1/workflows/upstream_sync` | 0 | ok |
| `workstream_handler_v1/workflows/pr_reviewer_loop` | 0 | ok (1 informational warning) |

Repo-wide mechanical checks:

| Leg | Exit | Observed result |
|------|------|-----------------|
| `node scripts/tree-outcome-flow.mjs --root .` | 0 | `OK (29 tree files, 0 violations)` |
| `node scripts/interface_validate.mjs` | 0 | `PASS (15 trees validated, interface declarations + example routes payload consistent)` |
| `node scripts/harness_matrix.mjs` | 0 | `sound: 49/49 items (37 base + 12 G)` |
| `./scripts/pincheck.sh` | 0 | `ALL CLEAN` |
| `./k8s/tests/test_fleet_apply_safe.sh` | 0 | `SKIP: cluster unreachable from this environment (Class A passed)` |

**Honest note on how the `validate` surface was run:** `make` is NOT installed on
this machine, so `make validate` as a single command could not be run. Every
`criteria validate` leg and every `node` leg that the Makefile's `validate`
target lists WAS run individually above (the list is identical to the target's
body), and its two prerequisites (`pincheck`, `fleet-apply-safe`) were run as
the underlying scripts they invoke.

`workstream_handler_v1/tests/test_*.sh` — the whole suite, 31 files, run one by
one: **31/31 exit 0**, no failures. That includes the new guard test from part
1, `test_pair_loop_fold_invariants.sh` (exit 0), and the parity comparator's own
self-test `test_pair_loop_parity.sh` (exit 0).

### 3.3 The one red leg — `harness_contracts_v1` — is PRE-EXISTING, carried as a FORWARD-POINTER

`criteria validate harness_contracts_v1` exits **1** with four
outcome-completeness errors:

    Error: step "contract_comment": outcome completeness requires handling adapter failure/error paths: declare outcome "failure", outcome "error", outcome "default", or set fallback = true
    Error: step "contract_uncontracted": outcome completeness requires handling adapter failure/error paths: declare outcome "failure", outcome "error", outcome "default", or set fallback = true
    Error: step "contract_permissions": outcome completeness requires handling adapter failure/error paths: declare outcome "failure", outcome "error", outcome "default", or set fallback = true
    Error: step "contract_unmapped": outcome completeness requires handling adapter failure/error paths: declare outcome "failure", outcome "error", outcome "default", or set fallback = true

This failure is **PRE-EXISTING on clean HEAD**. This card neither caused it nor
touches it:

- **The branch diff for that tree is EMPTY.** `git diff --stat main --
  harness_contracts_v1` prints nothing, and `git ls-tree -r main --
  harness_contracts_v1` and `git ls-tree -r HEAD -- harness_contracts_v1` list the
  same 32 tracked files. The tree is byte-identical to `main`.
- **It reproduces on a clean HEAD.** Extracting `main`'s copy (`git archive main
  harness_contracts_v1`) into a scratch directory and validating it there yields
  the SAME four errors and the SAME exit code 1.
- **The compiler is in range.** The installed `criteria` is **v0.5.58** and the
  tree declares `criteria_version = ">=0.5.9, >=0.5.52-rc1, <0.6.0"`, so v0.5.58
  is INSIDE the tree's required range. The failure is a real source-level defect
  in that tree, not a version-skew artefact.

**FORWARD-POINTER:** the owning card fixes that tree. It is **out of this card's
lane** — this card must not widen its scope to repair `harness_contracts_v1` — so
the tree is left **UNTOUCHED** by this card (no edit, no commit; nothing under
`harness_contracts_v1/` appears in this card's diff). The red leg is reported
here exactly as observed, not repaired, and the repo is NOT green on that one
leg.

### 3.4 The one open item — the operator's real-card parity run

**Open item (the ONLY one):** the real-card parity run — the v1 baseline card
run plus the v2 candidate card run. KB-386 is **UN-ARMED** (no `k8s-run`), so
this card does not perform it and claims no timing.

- **Procedure (cross-referenced, not duplicated):** `PARITY.md` §2 (the
  per-step timing collection procedure C1 -> C4), §3 (the per-slot threshold rule
  table) and §4 (the evidence slots) are the collection procedure of record.
- **Closer — the comparator's own exit code:**

      sh workstream_handler_v1/tests/test_pair_loop_parity.sh --compare <v1.tsv> <v2.tsv>

  exit `0` = every gated slot within threshold -> the gate CLOSES; exit `1` = a
  gated slot REGRESSED (the report names the slot); exit `2` = a record/rule
  error (the comparison did not happen — not a pass).
- Every measured slot in `PARITY.md` §4 is **PENDING**. No timing was invented,
  estimated, interpolated or copied from a fixture; the comparator's fixtures
  are labelled SELF-TEST ARMS and are explicitly NOT a real v1 baseline.
- The parity RECORD itself (`PARITY.md`) and the deterministic comparator
  (`workstream_handler_v1/tests/test_pair_loop_parity.sh`) are delivered and
  green.

**There is no other open item.** The fold, the four-dial retarget, the round-tree
deletion and the repo-wide validation sweep are closed as of this section. The
operator's real-card parity run is a post-merge fleet action on this un-armed
card, and it is the single thing left to close the gate.
