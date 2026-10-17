# Harness expectations matrix — workflow test harnesses (KB-254, series anchor)

This document is the **validation surface of record** for the workflow test
harness series (KB-254, the series anchor): 37 base items (`A1`–`F37`) plus
**12** subworkflow/composition items (`G1`–`G12`). It is traceability, not
narrative: every item states its harness assertion, the designed stimulus, the
run-event carrier the expectation is observed in, and the series card that owns
it.

Two rules are structural:

1. **A card's acceptance = its expectation slice observed in run events.** A
   series card is done only when each of its items' carrier assertions has been
   observed in an actual run (operator/peer/runner logs, castle stream events,
   or run-state files). Half-implementations cannot pass silently: an item
   whose carrier never fired is not "covered", it is unobserved.
2. **Defects file back to their own carriers.** A harness card changes
   *workflow-path* surfaces only — new trees, harness routes entries, their
   regression tests. No adapter/engine code change ever lands from a harness
   card; a defect the harness exposes files back to the engine, operator, or
   adapter carrier that owns it (a carrier defect never gets fixed inside the
   harness tree).

## Status and arming

- The series was filed and ARMED on dave's 2026-10-17 ruling: all six legs
  (this anchor plus the five cards below) fire within the standing green-run
  plan. The workflow-example dev lane self-serializes them FIFO: anchor first,
  then wire → contracts → composition → failures → fleet-smoke, one leg at a
  time.
- The original planning numbers (`KB-255` … `KB-260`) collided with live cards
  and are RETIRED: the series is identified by the `harness/*` titles and the
  card ids fixed below.
- The optional failure-injection recipes card is **unfiled** (optional); it
  stays a library candidate for the failures card's live-fault items.

## Series map

| Card | Harness title | Tree | Items (this matrix) |
|---|---|---|---|
| 276 | `harness/wire` | `harness_wire_v1` | `A1`–`A6`, `B7`–`B12` |
| 277 | `harness/contracts` | `harness_contracts_v1` | `C13`–`C18`, `D19`, `D21`, `D22` |
| 278 | `harness/composition` | `harness_composition_v1` | `G1`–`G12` (+ the live-operator composition asserts, `G10`) |
| 279 | `harness/failures` | `harness_failures_v1` | `E24`–`E32`, `G12` |
| 280 | `harness/fleet-smoke` | `harness_fleet_smoke_v1` | canary slice: `A1`, `A2`, `F33`, `F34`, `F36` |

- **harness/fleet-smoke (280) is the FAST-FEEDBACK CANARY**: a ~1-minute,
  deterministic, no-agent tree that must run BEFORE any develop leg on every
  roll/pin change; it is the first candidate for cron-smoke treatment once its
  tree lands. It gates a branch cheaply; the full KB-246-class develop leg
  REMAINS the acceptance gate — harnesses narrow what that leg burns, they do
  not replace it.
- **Deliberately unassigned harness items:** `D20` (checkpoint discipline) and
  `D23` (session resume after adapter death) are exercised by the *standing*
  develop leg and handler loop on live runs — they are validated on the
  pair-programming-loop carrier, not by a harness tree. They stay in the
  matrix so the numbering never has a hole.

## Conventions of record (shared surface for every leg)

Each leg lands in workflow-example: **new trees** (never a production tree),
plus its **harness routes entries** in the shipped example
`k8s/examples/routes-configmap.yaml`, in the same PR as its trees.

- **Tree names:** `harness_wire_v1` (276), `harness_contracts_v1` (277),
  `harness_composition_v1` (278), `harness_failures_v1` (279),
  `harness_fleet_smoke_v1` (280). A tree is validated standalone by its own
  `tests/` suite (repo convention) and by `make validate`.
- **Routes keys:** entry name `harness-<leg>-url` under `workflowLibrary`
  (url-only, `git::https://github.com/brokenbots/workflow-example.git//<tree>`,
  namespace `criteria-jobs`), route name `harness-<leg>`. Subtree-change
  re-pin rule applies unchanged (ADR-0005 D7): objects ship `ref`-pinless at
  merge time and a follow-up change re-pins them to the squashed merge commit.
- **Validation tag convention:** harness runs fire from tickets carrying BOTH
  the category tag `harness-validation` AND the dispatch tag
  `harness-<leg>` (route `tags` carries both; tag-subset matching selects).
  Production routes never match these tags, and harness tickets are raised in
  the Criteria project where the only production route requires
  `criteria-dirty` — a harness ticket without it matches nothing (fail-closed).
- **Queue class:** every harness workflow object stamps `class = "triage"` —
  harness runs are read-only against the repo and may run concurrently with
  any run; nothing in a harness tree writes the repository under test.
- **Volumes/secrets/adapterImages:** fork the kanboard object shape: `data`
  and `repo` PVCs (with `CRITERIA_RUN_DIR_ROOT` and
  `CRITERIA_SHELL_ALLOWED_PATHS` env), `scratch` tmp volume, and both peer
  adapter images (shell + copilot) — minus Kanboard secrets, which harness
  trees never need. Shipped configs stay secret-free (CRI-250 gate) — a
  harness config carries no secret material and its secret block is either
  empty or name-references only.
- **Run-event carriers** (what "observed in run events" means — these are the
  concrete surfaces harness assertions read):
  - runner lifecycle stream: flat `{"event": ..., ...}` lines or nested
    `adapter.lifecycle.*` envelopes — `provision_wanted` (carries `adapter`,
    `adapter_type`, `digest`, `scope_name`, `scope_instance_id`,
    `environment_type`, `environment_name`, `image_reference`, `accept_token`,
    `shim_listen_address`, `token_ref`) and `release` per (scope, environment);
  - CriteriaRun status: `status.conditions` (`ConfigRenderDrift`,
    `BaseImageMismatch`, stall-watchdog condition), `status.Queue.Position`;
  - castle stream: step-progress envelopes, `CriteriaHeartbeat`,
    `CriteriaRuns`, WorkflowGraphs (compiled graph member list);
  - pod-adapter manifests + peer logs: boot manifest membership, OpenSession
    identity handshake lines, dial/refusal records;
  - run-state files under `<run_dir>/` where a step's declared output is
    persisted.

## A. Wire/transport probes (tree `harness_wire_v1`, card 276)

Deterministic two-environment, two-kind tree (shell + copilot environment
pods) with pinned digests and NO agent steps: every probe is a structural or
transport observation, not development.

| ID | Harness assertion | Stimulus / carrier |
|---|---|---|
| **A1** | Identity handshake: the adapter pod presents the type-presented name (`CRITERIA_ADAPTER_NAME` = adapter **type**, never the node name) and the scope handshake binds `<scope>/<scope_instance_id>`; an empty scope in per-scope mode is rejected by the shim. | Peer logs (OpenSession identity handshake) + `provision_wanted` envelope fields; wrong-presented-name stimulus refused. |
| **A2** | Digest-pin resolve/miss: a lockfile-pinned digest resolves and the session opens with that OCI digest; a mismatching digest is refused BEFORE any session opens (D7 fail-closed) and no pod spawns. | `provision_wanted` `digest` vs lockfile pin in the runner stream; refusal record in runner events, zero pod-create events for the miss. |
| **A3** | Definition-identity grammar end-to-end: the workflow definition identity (name + version + criteria bound) resolves from compile through admission to the peer-composition identity `<type>.<name>` per environment. | WorkflowGraphs member list + peer composition grouping (definition identity + environment identity) in peer logs. |
| **A4** | Scope-set dialing: ONE environment pod serves MANY scopes of one run (distinct (scope, adapter) pairs), and the env-wide kind check resolves the adapter image from `adapter_type` (never the node name). | Number of `provision_wanted` emissions vs pod creates; `adapter_type` carried verbatim; per-scope peer-builder manifest shapes. |
| **A5** | Child lifecycle: a clean-exit child never respawns (verbatim spawn spec observed); a crashed child spawns/respawns only per its declared `on_crash`; `kill_child` is never emitted on the harness's healthy path; at most one live pod per definition name (per-definition mutex). | Peer logs + operator pod create/delete counts for the run. |
| **A6** | Rotation/release tombstones: `release` per (scope, environment) tears the corresponding pod down promptly; a rotated `accept_token` on an already-released pair never resurrects it (tombstone). | `release` events + operator pod deletion events; absence of a second `provision_wanted` after tombstone. |

## B. Operator composition, graph-carrier shape (tree `harness_wire_v1`, card 276)

| ID | Harness assertion | Stimulus / carrier |
|---|---|---|
| **B7** | Graph-complete boot manifests: an adapter that appears in the compiled graph but whose `provision_wanted` arrives late (e.g. `branch_repair` at boot) is boot-manifested — the peer pod manifest contains EVERY graph member's environment at boot, so the late provision joins an existing pod instead of recreating it. | Castle WorkflowGraphs + boot manifest membership in peer logs; late-join stimulus. |
| **B8** | Synthetic members: a late provisioner joins the pod group as a synthetic member — it carries the pinned digest (pins yes), holds no dial identity until the engine actually dials (dial identity no), and is deduplicated against the boot manifest member names (never listed twice). | Peer composition grouping records (`syntheticManifestMember` shapes) in operator/peer logs. |
| **B9** | Drift-recreate ABSENT both directions: neither a late join (B7) nor a post-boot re-provision of a same-key member recreates the pod — both directions produce ZERO recreate-shaped events. | Operator pod create counts == 1 per (scope, environment) for the whole run. |
| **B10** | Stamp-timing: a run admitted BEFORE a ConfigMap update executes its stamped (pinned) config — the drift is fail-closed (`ConfigRenderDrift`), never a silent switch; a run admitted AFTER the update renders the new values. | `ConfigRenderDrift` condition on a CM-update-mid-run stimulus; run stamped values in the CR. |
| **B11** | Deleted-CR reconcile parking: deleting a CriteriaRun mid-flight parks cleanly — reconcile releases the queue slot, sweeps child Jobs, and leaves no orphan pod. | CR deletion stimulus; queue slot release + child Job sweep in operator events. |
| **B12** | Boot gate: a run whose config/base-image render disagrees at boot REFUSES fast (render/base-image conditions) instead of starting mismatched children; the queue slot is released for the watcher to refire on the current pin. | `ConfigRenderDrift` / `BaseImageMismatch` conditions at boot; zero child Jobs for the refused run. |

## C. Typed outcomes/contracts (tree `harness_contracts_v1`, card 277)

| ID | Harness assertion | Stimulus / carrier |
|---|---|---|
| **C13** | Contract-mode activation per-step: declaring ANY payload schema on a step contracts ALL of that step's outcomes (the uncontracted arm becomes invalid); the uncontracted shape must fail LOUD within seconds of the step — never the silent no-observation class (this was the KB-246 `no_work` class). | Harness contract-mode step with one declared schema + one deliberately undeclared arm; the undeclared arm fails loudly with a schema contract diagnostic in seconds. |
| **C14** | Valid payload accepted: a payload satisfying the outcome's `type.<name>` (object subset) routes through the declared outcome with its projected outputs intact. | Fixed-prompt single-session contract step; green route + outputs in run-state. |
| **C15** | Malformed payload → payload_schema repair loop: a payload violating the declared schema is rejected and the run re-prompts the adapter (bounded repair loop) instead of dropping the observation silently. | Malformed-payload stimulus; repair-loop iterations visible in castle stream, then finalized verdict. |
| **C16** | Rejected attempt commits NOTHING: a schema-rejected attempt writes no `data.*` value, publishes no output, and advances no node — the commit point is only a schema-valid finalization. | Run-state diff across a rejected attempt: data values and outputs unchanged. |
| **C17** | Require-comment: an outcome arm whose adapter reason renders into a downstream step's input declares `require_comment = true` (KB-217 mechanical rule); an empty comment on such an arm is rejected. | Contract step's downstream-rendering arm; empty-reason stimulus rejected loudly. |
| **C18** | Fallback lane vs default mapping: the per-step `fallback` arm (adapter produced no finalized result, max one) routes to the lane it declares, while plain unmapped outcome names are mapped only via an explicit `outcome "default"` (18b) — an UNMAPPED outcome name with no default fails LOUD with a routing error instead of being swallowed. | No-finalization stimulus (fixed low max_turns session) → fallback lane; unknown-outcome-name stimulus → routing error in run events. |

## D. Light agent behavior, single fixed session (tree `harness_contracts_v1`, card 277 — `D19`, `D21`, `D22`)

One copilot session: fixed prompt, fixed model, low max-turns. NO real
development happens — these probes exercise agent-mechanics shapes only, and a
step that lands in its EXPECTED rejection shape routes green by design.

| ID | Harness assertion | Stimulus / carrier |
|---|---|---|
| **D19** | Payload submission + EXPECTED rejection shape: the session submits a payload shaped to REJECT (wrong field set, empty comment); the run routes this engineered rejection to its designed green lane, with the rejection reason carried verbatim in the run-state. | Malformed submission; the step's declared rejection outcome routes green, reason recorded in run-state files. |
| **D20** *(standing develop leg, not a harness tree)* | Checkpoint discipline: a mid-turn commit+push is the unit of progress; a run resuming from a checkpoint resumable state resumes rather than dying (pair-programming-loop checkpoint semantics). | Held by the standing develop leg / workstream_handler_v1 loop on live runs (handler regression suite pins the shapes). |
| **D21** | Permission policy: compound segmentation (tool targets segmented by adapter/kind and arguments) and trailing-`*` bands behave per the first-match-wins glob contract; a recovered permission denial keeps the turn's success (deny → recover → success) instead of poisoning the outcome. | Permission-gate stimulus with a scoped allow band; denial + recovery recorded in castle stream permission events; final outcome success. |
| **D22** | Turn budgets + missing-finalize class: a session exhausted at its max-turn budget ends in the designed missing-finalize class — routed through the step's fallback lane, loud and class-marked (never an ambiguous FSM error). | Low max-turn budget; budget-exhaustion event + fallback route observed. |
| **D23** *(standing develop leg, not a harness tree)* | Session resume after adapter death: a killed adapter child's session resumes with context restored (peer respawn + engine resume path). | Held by the standing develop leg on live runs (peer respawn observations belong to `A5`'s carriers). |

## E. Engineered failure paths (tree `harness_failures_v1`, card 279)

Every step is engineered to fail on purpose; **green = each failure landed in
its designed class marker**, observed in the named carrier.

| ID | Harness assertion | Stimulus / carrier |
|---|---|---|
| **E24** | Provider stall → watchdog/timeout class: a wedged adapter session (heartbeat-only stream, no step progress) trips the stall watchdog — the run is failed into the stall class, child Jobs are deleted, the queue slot is released, and the ticket is never parked silently. | Stall stimulus (session emits only heartbeats); watchdog condition on the CR + zero parked-without-error outcomes. |
| **E25** | Provider 5xx → bounded retry then `awaiting_human`: transport failures retry within a bounded budget and finalize the run parked at `awaiting_human` (no restart) — bounded, designed, human-facing. | 5xx stimulus; retry count observed bounded, terminal park at awaiting_human in run-state. |
| **E26** | Never-scheduled pod (capacity) distinguishable from handshake budget: a pod that NEVER SCHEDULES (capacity/starvation) produces a distinct, named class marker — never collapsed into a generic handshake-timeout observation. | Starved-pod stimulus; unscheduled class marker distinct from the handshake-budget marker in operator events. |
| **E27** | Zero `provision_wanted` scope emission: a scope with nothing to wire emits ZERO `provision_wanted` events (no spurious pods for an unwired scope). | Scope-present-but-unwired stimulus; absence asserted in the runner stream. |
| **E28** | Heartbeat reaper discrimination (parked ≠ failed): heartbeat and non-activity adapter chatter NEVER fold into step progress (a heartbeat-only stream folds a zero-progress baseline), so a parked-but-alive run is never reaped as failed. | Heartbeat-only stimulus while parked; zero step-progress fold + survival in castle progress observation. |
| **E29** | Container restart resume-vs-replay: a runner-container restart RESUMES from the stored checkpoint (source-mode state under `/data/criteria-home/<runner-job>/`) and never replays the run fresh. | Kill+restart stimulus; resumed-run event trail with no fresh-start emission. |
| **E30** | Stale pin refusal (expected-pin mismatch): workflow content resolving to a stale/expected-pin-mismatch SHA is REFUSED fail-closed at admission — with the pin duality (url `?ref=` vs the `ref` key) agreement asserted per entry. | Stale-pin stimulus; admission refusal with the pinned-vs-resolved values named. |
| **E31** | `BaseImageMismatch` refusal: child Jobs whose runner image disagrees with the CR's pinned base image fail fast with the `BaseImageMismatch` condition, delete the children, and release the queue slot. | Pinned-vs-current image drift stimulus; condition + slot release observed. |
| **E32** | Broken image mirror identification: an unreachable/broken registry mirror is NAMED in the failure (which mirror, which adapter) — never a generic pull error. | Broken-mirror stimulus; failure text identifies the mirror. |

## F. Fleet invariants (canary slice in `harness_fleet_smoke_v1`, card 280)

| ID | Harness assertion | Stimulus / carrier |
|---|---|---|
| **F33** | Zero recreates: across the whole harness run pod creates == 1 per (scope, adapter/environment) — no recreate-shaped events anywhere, including after late joins and post-boot re-provisions. | Whole-run operator event census. |
| **F34** | Zero refused dials: every dial attempt meets its handshake — zero handshake refusals unless the refusal IS the item's designed stimulus (in which case the stimulus's class marker is the green observation). | Whole-run peer-log census. |
| **F35** | Queue Position semantics: a queued run's `Position` counts down to 0, and Position == 0 means admitted-and-running — semantics stable across class shapes (dev serializes per repoURL, triage admits concurrently). | Queued + admitted run observation in CR `status.Queue.Position`. |
| **F36** | Secret-free configs: every shipped harness config is secret-free — no literal secret material anywhere in shipped payloads (CRI-250 discipline), secrets exist only as SecretProviderClass name-references. | Secret-free grep gate + per-object review of harness entries. |
| **F37** | Memory-ceiling parity across BOTH adapter builders: the peer-pod builder and the adapter-pod builder derive identical memory ceilings for the same shape (engine/runtime container resources), so neither builder can drift its fleet envelope silently. | Built-manifest diff across the two builders for identical declarations. |

## G. Subworkflow / for_each / parallel composition (tree `harness_composition_v1`, card 278)

The defining card: a mini-tree with an embedded callee environment, a shared
pass-in adapter, and a 3-iteration `for_each` with an iteration-body
environment; asserts pod/manifest shapes from operator + peer logs.

| ID | Harness assertion | Stimulus / carrier |
|---|---|---|
| **G1** | Embedded callee env members grouped across ALL layers: adapter members of an embedded callee environment join the composition groups with the parent's members across every subworkflow layer (grouping key = definition identity + environment identity `<type>.<name>`). | Composition grouping records for parent + embedded callee layers in peer logs. |
| **G2** | Iteration-body env pod keyed per iteration: an environment referenced from inside an iteration body resolves per iteration — pods are keyed by the iteration's scope identity, not a single shared pod. | Pod key census: one distinct (iteration scope, environment) pod per iteration. |
| **G3** | Fresh pod per iteration + teardown scoping: each iteration provisions a FRESH pod, and teardown releases exactly that iteration's pods (per (scope-instance, environment)) — an iteration's exit never releases another iteration's pods. | `provision_wanted`/`release` sequences per iteration; pod create/delete census. |
| **G4** | Aggregates post-loop: after the last iteration completes, the aggregate outcome (`all_succeeded` / `any_failed`) routes exactly once — never per-iteration. | Castle stream: single aggregate transition after the final iteration event. |
| **G5** | Parallel concurrency guard: only instances declaring the parallel-safe capability may be `parallel` targets (guard is a compile-time error otherwise); `parallel_max` bounds concurrency (default GOMAXPROCS). | Non-parallel-safe target refuses at compile; bounded concurrency observed at run. |
| **G6** | Borrow-not-owned: a borrower of a pass-in (shared) adapter never provisions, tears down, or respawns it — the owner does; an iteration's exit NEVER kills a borrowed session, and the borrowed session outlives the borrower. | Borrow stimulus: owner-provision event only, borrower teardown absence, session survival after borrower exit. |
| **G7** | ADR-0009 substitution: the parent pins/shadows the callee's same-key environment (one instance wins — substitution replaces, never appends); conformance post-substitution is asserted on the PASSED adapter only — the substitute instance carries the conformance check, the callee's original shadowed instance is not observed. | Shadow-key stimulus; single provisioning of the substituted env; post-substitution conformance observed on the passed instance. |
| **G8** | SessionManager alias ledger: the alias ledger resolves multiple identity spellings of one session to the SAME canonical instance — an alias never provisions a second session. | Alias-addressed calls against one live instance; ledger records in peer logs. |
| **G9** | Callee-default env without override: a callee environment the parent does NOT override provisions from the callee's own declaration — identical identity to the callee's standalone shape (no parent leakage either direction). | No-override stimulus; identity comparison callee-standalone vs composed-run. |
| **G10** | Live-operator composition asserts: on a live operator, the composition tree re-asserts the graph-carrier shape with an embedded callee — boot manifests contain every member (`B7`) and drift-recreate is absent both directions (`B9`) even though the callee's members arrive only from the composed graph. | Peer logs + operator pod census on a live operator. |
| **G11** | KNOWN GAP (pinned, engine carrier): `subworkflow environment=` is NOT validated to exist in the parent — a dangling environment traversal on a subworkflow compiles and fails only at runtime. The composition harness pins the gap's shape deliberately; a fix files back to the engine carrier, never to a harness card. | Dangling-environment stimulus on the composition tree; documented runtime (not compile) failure shape. |
| **G12** | KNOWN GAP (pinned, engine carrier): `SubworkflowNode.Environment` is compiled-but-unread — the attribute travels through compilation and is inert at runtime (subworkflow-level environment declarations are silently ignored). The composition card pins its compiled-but-unread shape and the failures card (279) pins the failure shape: a runtime composition change that silently does nothing. | Compiled node carrying the attribute + runtime inertness observed; documented as a known gap in run notes. |

## Guard

`scripts/harness_matrix.mjs` (wired into `make validate`) fails closed when
this matrix is incomplete or self-inconsistent: missing/duplicate item ids,
wrong count (must be 37 + 12), a slice assignment missing from the series map,
a KNOWN GAP without its pinned marker, or a conventions block missing one of
the of-record facts above. `k8s/tests/test_harness_expectations.sh` runs the
same guard for the CI shell-test loop.