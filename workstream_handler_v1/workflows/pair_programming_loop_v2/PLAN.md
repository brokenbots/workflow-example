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
