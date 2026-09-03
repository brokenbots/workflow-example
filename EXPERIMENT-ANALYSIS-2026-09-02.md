# Criteria Autonomy Experiment — Analysis Report

Date: 2026-09-02
Scope: Linear intake experiments 1–3, tickets CRI-47 through CRI-81, 29 merged PRs
(brokenbots/criteria #336–#350, brokenbots/castle #1–#14).
Method: cross-checked the run ledger ([criteria-linear-intake-evaluations.md](criteria-linear-intake-evaluations.md))
against local git history (both clones) and GitHub PR review data. Every
quantitative claim below is verified against commit/PR data, not just the ledger.

## 1. Data verification

- All 29 merge commits verified locally. Two SHAs missing from the ledger were
  recovered: CRI-79 merged as `17de02741456dcb09291f9826ae53bff6013ddff`,
  CRI-80 as `338496e46b8fbaeb45ee768fc3f9ea8bde7e35b8`.
- Castle `origin/main` = `5ef38bd2` = final experiment state.
- Criteria `origin/main` has advanced past the experiment to `7a920ed`
  (v0.5.17) with work through CRI-89 — **the workflow kept running autonomously
  after the analyzed window closed.**
- All merges are squash commits; every PR's base equals the previous PR's merge
  commit. The "latest origin/main" operating rule was followed 29/29 times.
- Identity: author `handcaught` on all 29 PRs, committer GitHub (web merge),
  sole reviewer `brokenbot` on all 29.

## 2. Instruction following — strong

- Classification and routing behaved as designed in every recorded case. The
  four `insufficient_report` rejections (CRI-50/51/55/56) were evidence-based
  and the correct gate outcome for under-specified tickets; the workflow refused
  to invent evidence.
- The red herring (CRI-61) was the best single result: QA built a 3-session
  harness, confirmed the premise, falsified the claimed consequence at
  `Engine.Pause`, and declined to write code.
- No PR contains unrelated changes. CRI-69 and CRI-52 explicitly maintained
  removals-only scope discipline.

## 3. Review quality — the experiment's biggest asymmetry

Two distinct review layers performed very differently.

**Internal implementation review (in-workflow, pre-PR): genuinely strong.**
Verified real catches:
- CRI-67: recovered runs authenticating with the fresh agent identity instead of
  persisted owner credentials — a subtle security bug masked by a
  constant-identity fake server.
- CRI-69: `PauseRun` returning success after merely enqueueing `RunCancel`.
- CRI-72: submit-time dispatch stranding a leased backlog item.
- CRI-74: `InspectRun` scanning only the earliest 1,000 events.
- CRI-75: 10 RPCs with zero success-case conformance coverage.
- CRI-76: three blockers including a control client that could not control the
  runs it targeted.

**External PR review (brokenbot on GitHub): effectively ceremonial.** 28/29 PRs
received a single approval with zero comment threads within ~2 hours, regardless
of size (CRI-74's 11,050-line / 53-file PR was treated identically to 3-file
fixes). The only threads on any PR came from `github-code-quality[bot]` (CRI-65
nil-dereference). Quality control is real but lives entirely *inside* the
workflow; the GitHub "independent approval" gate is a rubber stamp by
construction.

## 4. Tactical vs durable solutions

**Durable (verified in diffs):**
- CRI-47/48/49 security fixes: root-cause changes (sort-longest-first redaction,
  path-component-aware confinement) with adversarial regression matrices.
- CRI-69 protocol migration: clean excision of `overlord.v1` (98 files,
  +3.6k/−10k) — deletion-heavy, the healthy direction.
- CRI-78: genuine causal analysis (WAL allows concurrent readers, not writers);
  `SetMaxOpenConns(1)` is defensible for single-replica SQLite.
- CRI-79/81: restart re-handshake from persisted `last_seq`; Compose
  `oneoff=True` labeling — mechanistically correct.

**Tactical (verified in diffs):**
- CRI-68: raised the race-test coalescing ceiling 10→15 after a CI flake. The
  commit documents why and keeps the upper-bound assertion — tactical with
  honest bookkeeping, but a band-aid over a timing-sensitive test.
- CRI-52: lowered the OCI coverage floor 67.0→65.5 with justification. This is a
  governance hole: **the workflow can lower its own quality gates when they
  block it.** Ratchet relaxations should require human approval.

## 5. Failure taxonomy (verified counts)

| Category | Count | Incidents |
|---|---|---|
| Workflow-owned defect | 2 | CRI-52 feedback info-loss (fixed via `status_output`); CRI-62 non-durable plans (unfixed) |
| Adapter defect | 4 | Malformed tool-call ×3 (CRI-67/71/75); post_approval HTTP 422 ×1 (CRI-65) |
| External infra | 6 | Docker `context canceled` ×3 (CRI-54 ×2, CRI-62); terminal SIGINT ×2 (CRI-63); container disappearance ×1 (CRI-52) |
| Operator error | ~7 | Empty ticket bodies (CRI-63..76 batch); CRI-68 ×3 config failures; wrong repo (CRI-78); TEST_REFS misconfig (CRI-77) |
| Live-only product defects | 5 | CRI-77..81 chain |

**Restart durability split:**
- Clean resumes from durable state: CRI-65, 67, 71, 75-retry, 52-third-run (5)
- Lost uncommitted work, full re-implementation: CRI-54 ×2, CRI-69, CRI-75-first (4)
- Non-resumable approved plans: CRI-62 (1)

## 6. Human intervention vs the "few minor interventions" claim

Counted from the ledger: **~19 operator touches across 35 tickets (54%)** — but
~11 were Experiment-3 setup/config scaffolding (ticket-body repair, BUILD_CMD,
stable refs, terminal discipline). True mid-run course corrections after steady
state: ~6 (CRI-65 manual approval, CRI-68 stress-test termination, CRI-54/62
retries). The claim holds for Experiments 1–2 (2 touches / 16 tickets);
Experiment 3 was a two-repo shakedown and needed real babysitting.

## 7. Coupling hotspots (cross-PR touch counts)

- Castle: `sqlite.go` ×6, `castle.go` ×5, `Makefile` ×5,
  `integration_test.go` ×5 — storage and RPC surface are the churn center.
- Criteria: max 3 touches (`client.go`, `agent.go`, `main.go`) — healthier
  decoupling.

## 8. Recommendations (ordered by leverage)

### Workflow-owned (fixable in example-workflows directly)
1. **Commit-and-push WIP checkpointing in the developer loop** — push the branch
   after every green local gate. Eliminates the entire "lost uncommitted work"
   class (CRI-54 ×2, CRI-69, CRI-75) and converts restart-from-scratch into
   resume-from-branch.
2. **Generalize the CRI-52 `status_output` pattern** — every loop child should
   declare a structured status output that parents route on. One instance was
   fixed; the class remains.

### Product/adapter tickets (route through linear_intake_v1)
3. **Persist approved QA plans to the ticket, not the container** (fixes
   CRI-62's inconsistent re-classification on restart).
4. **Malformed tool-call retry-with-repair** in the reviewer adapter
   (3 occurrences: CRI-67/71/75).
5. **Pending-review resume for post_approval HTTP 422** (CRI-65).
6. **Gate coverage-floor ratchets behind human approval** (CRI-52 governance
   hole).
7. **Intake pre-launch hardening**: ticket-body lint (CRI-63..76 empty-body
   bug), STABLE_REF preflight for tagless repos (CRI-68 ×2, CRI-78),
   BUILD_CMD as a make-target list not shell (CRI-68 ×3).

### Path to autonomous single-repo management
The pieces exist: `linear_intake_v1` + `qa_triage_v1` = intake and verification
(4 correct rejections, 1 red herring caught); `workstream_handler_v1` =
implementation loop (29 merges). Missing: **(a)** a periodic-review workflow that
audits the repo on a schedule and files its own tickets; **(b)** a
roadmap/planning layer (the Experiment-3 project doc was human-written; promote
it to a maintained artifact); **(c)** durable state per items 1–3 — autonomy
fails today not on judgment but on amnesia.
