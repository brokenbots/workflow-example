# pair_programming_loop_v2 — real-card parity record (KB-386)

Card: **KB-386** (Kanboard) — "Stage 3 - fold round plumbing onto pair-loop v2".
Plan: `.hermes/plans/2026-11-06_123002-pair-loop-remaster-local-upstream-plus-planner.md`
(Stage-2 section "Sequencing"; Stage-3 = fold the r2/r3 round plumbing onto this
tree's shape, retarget the `_feedback_r2`/`_feedback_r3` dials, and delete the round
trees **after** parity on a real card).
Rulings honoured: ruling 1 (LOCAL-ONLY — this tree performs zero pushes/fetches/
clones; the only network surface is the `upstream_sync` subworkflow), ruling 12 (the
handler dial stops passing `log_file`). House rule: deterministic shell truth over
agent judgement; no fabricated evidence.

This file is the **parity RECORD** — the baseline definition, the collection
procedure, the thresholds, the evidence slots and the gate status. The deterministic
comparator that enforces the thresholds is
`workstream_handler_v1/tests/test_pair_loop_parity.sh`; the rule table below is the
single source of truth for BOTH (the comparator's self-test fails if the two drift).

**KB-386 is UN-ARMED (no `k8s-run`).** Nothing in this repo is a measurement of a
real card. Every measured slot below is **PENDING**. No invented, estimated or
interpolated timing appears anywhere in this record.

## 1. The v1 baseline IS the surviving `workflows/pair_programming_loop` tree

The baseline is the surviving base loop tree:

    workstream_handler_v1/workflows/pair_programming_loop/

— the tree the Stage-3 fold replaced **at the dial level** (the handler's
`pair_programming_loop` dial now sources `./workflows/pair_programming_loop_v2`).
That base tree **STAYS: it is RETAINED**, not deleted. The Stage-3 deletion removes
ONLY the round trees (`workflows/pair_programming_loop_round2/` and
`workflows/pair_programming_loop_round3/` — part 3 of 3), and it never touches the
base tree. Keeping the baseline tree in the repo is what makes the parity run
REPEATABLE: the operator can re-run the baseline card at any time and re-measure the
candidate against it.

The baseline card is the card whose workflow sources
`workstream_handler_v1/workflows/pair_programming_loop`; the candidate card is the
card whose workflow sources `workstream_handler_v1/workflows/pair_programming_loop_v2`.

## 2. Per-step timing collection procedure (real card)

### 2.1 The two runs

Same workstream, same repository, same fleet lane, same card shape — one variable
changed (the sourced loop tree):

| Run | Workflow sourced | Purpose |
|-----|------------------|---------|
| **V1 baseline** | `workflows/pair_programming_loop` | the surviving baseline tree's per-step timings |
| **V2 candidate** | `workflows/pair_programming_loop_v2` | the folded tree's per-step timings |

Both run ids are recorded in the card's evidence (they are unknown until the
operator runs the lane — **PENDING**).

### 2.2 Where each step's timing is read

The per-step timings come from **castle's run event stream**, not from a log scrape:

- Endpoint: `POST <castle>/criteria.v1.ServerService/ListRunEvents` with body
  `{"runId":"<run-uuid>","sinceSeq":0,"limit":500}`; the response body is
  `{"events":[...],"nextSinceSeq":N}`.
- Every event is an envelope carrying `seq`, `ts` and a typed payload:
  `stepEntered{step,adapter}` (the step entered) and `stepOutcome{step,outcome,error}`
  (the step exited). The stream carries **no per-step duration field** — the duration
  is DERIVED from the envelope timestamps:
  `duration_ms(step) = ts(stepOutcome for step) - ts(stepEntered for step)`.
- Paging: page with `sinceSeq` until a page returns fewer events than `limit`;
  `nextSinceSeq` is the cursor for the next page (`seq` is the per-envelope fallback).
- Precedents in-repo: `scripts/castle_verify.mjs` (the `ListRunEvents` call, the
  paging loop and the page budget), `criteria-k8s/internal/castle/castle.go`
  (`drain()` — the cursor/`nextSinceSeq`/page-length loop), and the operator's
  event-tail reads in `arc_shepherd_v1/scripts/classify_events.sh.tftpl` and
  `arc_shepherd_v1/scripts/prepare_consult.sh.tftpl`.
- Only a `stepOutcome` that matches a `stepEntered` of the same step id yields a
  duration; an unpaired event is a collection error (never a guessed duration).

### 2.3 Subworkflow attribution (mandatory)

A nested subworkflow's inner steps are attributed to the **OUTER** step id. The
`upstream_sync` subworkflow's inner steps (`push_branch`, `upstream_triage_agent`)
ride `sync_checkpoint_upstream`; the inner ids never appear as their own rows.
The same holds for every nested dial in both trees.

### 2.4 The record format

One TSV file per side, one line per step invocation:

    <step_id>\t<duration_ms>

- `#` starts a comment line (ignored); blank lines are ignored.
- A step invoked more than once contributes one line per invocation.
- Step ids are the tree's own step ids (`main.chcl` `step "<id>"`).
- The files are the comparator's inputs:
  `sh workstream_handler_v1/tests/test_pair_loop_parity.sh --compare <v1.tsv> <v2.tsv>`
  — exit `0` = every gated slot is within threshold, exit `1` = a gated slot
  REGRESSED (the report names the slot), exit `2` = record/rule error (the
  comparison did not happen).

## 3. The per-slot thresholds

The gate is **per step**: for a `max_unit` slot the slowest single invocation must
be within the threshold, which means every invocation in that slot is (the maximum
is the binding one); for a `total` slot the slot's summed duration must be within
the threshold.

| Mode | Rule |
|------|------|
| `delta` | `candidate ≤ floor(baseline × (100 + pct) / 100)` |
| `budget` | `candidate ≤ floor(reference slot's v1 TOTAL (the sum of that slot's steps) × (100 + pct) / 100)` |
| `max_unit` | the metric is the slot's largest single invocation |
| `total` | the metric is the slot's summed duration |
| `one_sided` | REPORTED, never gated (a leg with no counterpart) |
| absent slot | `NOT_COMPARABLE` when optional; a RECORD ERROR when mandatory |

The percentages are **the documented threshold** — a design parameter of this gate,
not a measurement, and not derived from any observed timing (no real-card timing
exists). Their rationale: the prelude/log legs are the same shell steps in both
trees and may only drift with fleet jitter (`+25%`); the split legs (`delivery`,
`gate`, `review`) may cost a little more per part than the monolithic v1 step
(fresh instance, narrower context — `+25%`, no more); the v2 planning leg must fit
INSIDE the v1 development budget (`budget`, `0%` of the reference slot — the fold
may not buy its planning overhead with the delivery budget); the checkpoint leg is
dominated by the `upstream_sync` delegation and the fleet's push latency (`+50%`,
optional because a run with nothing to checkpoint simply does not exercise it).

The machine-readable table (the comparator's own table, byte-identical — the
self-test compares the two and fails on drift):

<!-- BEGIN PARITY RULE TABLE -->
slot|side|metric|mode|pct|ref|mandatory|patterns
prelude_leg|v1|total|delta|25||yes|get_branch_name read_workstream checkout_branch capture_entry_sha check_working_tree
prelude_leg|v2|total|delta|25||yes|get_branch_name read_workstream checkout_branch capture_entry_sha check_working_tree
plan_leg|v2|total|budget|0|delivery_leg@v1|yes|plan_parts reduce_plan_parts
delivery_leg|v1|max_unit|delta|25||yes|develop
delivery_leg|v2|max_unit|delta|25||yes|run_part_p*_fast run_part_p*_deep
gate_leg|v1|max_unit|delta|25||yes|verify_committed ci_gate
gate_leg|v2|max_unit|delta|25||yes|p*_verify p*_refute p*_reverify
review_leg|v1|max_unit|delta|25||yes|review
review_leg|v2|max_unit|delta|25||no|pair_part_p*_remediate
checkpoint_leg|v1|total|delta|50||no|push_checkpoint preserve_pending sync_checkpoint_upstream resume_after_preservation
checkpoint_leg|v2|total|delta|50||no|push_checkpoint preserve_pending sync_checkpoint_upstream resume_after_preservation
log_leg|v1|total|delta|25||yes|write_review_log commit_review_log push_review_checkpoint
log_leg|v2|total|delta|25||yes|write_review_log commit_review_log push_review_checkpoint
wip_leg|v1|total|one_sided|||no|write_work_log commit_work_log push_wip_checkpoint
verdict_leg|v2|total|one_sided|||no|switch_verdict_route
failure_leg|v2|total|one_sided|||no|fail_no_parts_attempted fail_parts_overflow fail_verdict_mismatch fail_pair_verdict_parking
<!-- END PARITY RULE TABLE -->

Slot notes:

- `prelude_leg` — the five shared shell steps (branch, workstream, checkout, entry
  sha, clean tree). Identical step ids on both sides.
- `plan_leg` — v2-only (the planner + the jq plan validation). Budgeted against the
  v1 `delivery_leg` sum: the planner may not cost more than the development leg it
  replaces. Mandatory on the v2 side.
- `delivery_leg` — v1 `develop` vs v2 `run_part_pN_fast|deep` (the part split).
  The unit is one part, not the whole run.
- `gate_leg` — v1 `verify_committed`/`ci_gate` vs v2 `pN_verify`/`pN_refute`/
  `pN_reverify` (the per-part deterministic verify + the refute/re-verify cycle).
- `review_leg` — v1 `review` vs v2 `pair_part_pN_remediate` (the in-tree
  remediation that replaces the round ladders). Optional on the v2 side: a clean run
  never remediates, so the slot may be absent (reported, not gated).
- `checkpoint_leg` — the shared checkpoint/preserve/upstream-sync chain. Optional on
  both sides (a run with nothing to checkpoint does not exercise it).
- `log_leg` — the shared review-log write/commit/push tail.
- `wip_leg` — v1-only (`write_work_log`/`commit_work_log`/`push_wip_checkpoint`):
  the v2 tree folds the work log into its per-part verdict log. Reported, never gated.
- `verdict_leg` — v2-only (`switch_verdict_route`): the tree's deterministic
  whole-tree verdict aggregate. Reported, never gated.
- `failure_leg` — v2-only (`fail_*`): the typed-failure bookkeeping steps. Reported,
  never gated.

## 4. The evidence slots (all PENDING)

Every measured slot is **PENDING** with its collection step. The "v1 value"/"v2
value" cells stay empty until the operator's real-card run fills them — a slot is
closed only by a value derived from the real event stream, never by an estimate.

| Slot | v1 baseline value | v2 candidate value | Threshold | Status | Collection step |
|------|-------------------|-------------------|-----------|--------|-----------------|
| `prelude_leg` | PENDING | PENDING | `total`, delta `+25%` | PENDING | C1 → C2 → C3 → C4 |
| `plan_leg` | n/a (v2-only leg; budget reference = `delivery_leg@v1`) | PENDING | `total`, budget `0%` of `delivery_leg@v1` | PENDING | C1 → C2 → C3 → C4 |
| `delivery_leg` | PENDING | PENDING | `max_unit`, delta `+25%` | PENDING | C1 → C2 → C3 → C4 |
| `gate_leg` | PENDING | PENDING | `max_unit`, delta `+25%` | PENDING | C1 → C2 → C3 → C4 |
| `review_leg` | PENDING | PENDING | `max_unit`, delta `+25%` | PENDING | C1 → C2 → C3 → C4 |
| `checkpoint_leg` | PENDING | PENDING | `total`, delta `+50%` | PENDING | C1 → C2 → C3 → C4 |
| `log_leg` | PENDING | PENDING | `total`, delta `+25%` | PENDING | C1 → C2 → C3 → C4 |
| `wip_leg` | PENDING | n/a (v1-only leg) | REPORTED, never gated | PENDING | C1 → C2 → C3 |
| `verdict_leg` | n/a (v2-only leg) | PENDING | REPORTED, never gated | PENDING | C1 → C2 → C3 |
| `failure_leg` | n/a (v2-only leg) | PENDING | REPORTED, never gated | PENDING | C1 → C2 → C3 |

Collection steps:

- **C1 — arm the lane (operator, post-merge fleet lane).** Pick the two comparable
  cards (same workstream, same repo, same fleet lane): one sourcing
  `workflows/pair_programming_loop`, one sourcing `workflows/pair_programming_loop_v2`.
  Record both run ids on the card.
- **C2 — collect the per-step timings.** For each run: read the whole event stream
  via `POST <castle>/criteria.v1.ServerService/ListRunEvents` (paged), pair
  `stepEntered` ↔ `stepOutcome` per step id, derive `duration_ms` from the envelope
  `ts`, and attribute nested-subworkflow inner steps to the OUTER step id (§2.2,
  §2.3). An unpaired event is a collection error, not a duration.
- **C3 — write the two records.** One TSV per side (`<step_id>\t<duration_ms>`, one
  line per invocation) in the format of §2.4. The v1 record is the **baseline**; the
  v2 record is the **candidate**.
- **C4 — compare.** `sh workstream_handler_v1/tests/test_pair_loop_parity.sh
  --compare <v1.tsv> <v2.tsv>`. Exit `0` = every gated slot within threshold; exit
  `1` = a gated slot regressed (the report names the slot); exit `2` = record/rule
  error (no comparison happened). Attach the comparator's report to the card.

## 5. Gate status — OPEN, and the run is the operator's

**GATE: OPEN. The real-card parity run is NOT performed by this card.**

- KB-386 is **UN-ARMED** (no `k8s-run`): this card produces the parity RECORD (this
  file) plus the deterministic comparator, and nothing else. No card run happens
  here.
- The real-card run (the v1 baseline card run + the v2 candidate card run, §2.1) is
  the **operator's post-merge fleet action** (C1 → C4 above). It is the only thing
  that can close this gate, and it closes it with the comparator's own exit code.
- Every measured slot in §4 is **PENDING** until that run fills it. No timing in
  this record is invented, estimated or interpolated.
- The comparator's fixtures under
  `workstream_handler_v1/tests/fixtures/pair_loop_parity/` are **SELF-TEST ARMS**
  with synthetic values: they exercise the comparator's arithmetic and are
  explicitly **NOT a real v1 baseline** and never evidence for this gate.
- The round-tree deletion (`workflows/pair_programming_loop_round2/` +
  `_round3/`) is part 3 of 3 and stays **gated** on this comparison: the trees are
  deleted only after the comparator exits `0` on the REAL records. The base tree
  `workflows/pair_programming_loop` is RETAINED either way (§1).

## The comparator's own self-test (deterministic, no card)

`sh workstream_handler_v1/tests/test_pair_loop_parity.sh` (no arguments) runs the
comparator against its labelled self-test arms: the negative arms first (a candidate
that regresses a slot past the threshold MUST fail; a broken record MUST error),
then the positive arm (an in-threshold candidate MUST pass), then the self-test of
the comparator's own logic (the threshold boundary is exact — the allowed value
passes, allowed+1 fails; the reported threshold matches an independent
recomputation; a `one_sided` leg is reported and never gates; every step of both
trees maps into exactly one slot; the comparator's rule table still matches §3's
table; the script parses and runs as POSIX sh). The self-test proves the
COMPARATOR; it proves nothing about a real card.
