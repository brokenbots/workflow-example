# ADR-0015 — Devops maintenance operations workflow family (devops_maint_v1)

**Status:** Proposed

**Date:** 2026-11-14

**Deciders:** Project lead (dave); rulings this session: all-repos scope with a compliance
mandate, one guided sweep before cadence, one consolidated PR per module.

---

## Context

Devops-level work — dependency hygiene, vulnerability clearing, CI upkeep, SAST triage,
repo compliance — is performed ad hoc inside other workflows' incident loops, and only
when a failure forces it. The backlog proves the posture is untenable: **42 open
dependabot PRs across 11 repos**, months stale in places, several carrying
vulnerability-clearing bumps that fail repo-state CI gates when main drifts past them
(the KB-83 fight was one instance of the class); **23 open CodeQL alerts on the core
repo** (including one CRITICAL `go/unsafe-quoting`) with no owner driving them down; and
**branch protection on exactly one of 16 product repos** (`criteria`'s "Main Protection"
ruleset) — every adapter, SDK, starter, and control-plane repo merges direct to main with
no ruleset at all.

The failure is structural, not habitual: the work has no deterministic loop. It recurs as
an identical command shape across sessions (enumerate PRs → read CI → classify → bump →
review → merge), which is the ops-workflow conversion signal: the graph should own the
loop and the agent should act only on the judgment branches.

## Decision

**D1 — One workflow family, two lanes.** `devops_maint_v1` in workflow-example (ops/
family, castle-connected) runs two lanes over every in-scope repo:

- **Lane A (dependency hygiene):** `collect` (per-repo dependabot PR enumeration + CI
  verdicts + govulncheck/osv advisory state, emitted as a normalized JSON inventory
  artifact) → `classify` (DETERMINISTIC rules — mechanical-bump: version-only diff in
  lockfiles/manifests; vuln-clear: bump closes an advisory present in scan output;
  needs-rebase: base drifted to DIRTY/UNKNOWN; needs-judgment: semver-major on a
  load-bearing dep, actions-major bumps, CI-red on the PR's own head) → `route`
  (decision adapter: mechanical and vuln-clear classes act automatically; judgment class
  is report-only) → `act` (one CONSOLIDATED PR per module closing N dependabot PRs;
  rebase drifted bases; compound commits where CI requires — e.g. a builder pin) →
  `review` → `merge` (brokenbot lane, green+complete checks on the merge commit) →
  close-out comments on the affected dependabot PRs.

- **Lane B (compliance baseline):** drive every repo to a compliant state, per language:
  (1) CI normalization — language-specific template (Go: lint + test + govulncheck +
  osv; TS: tsc + vitest container run + osv; PY: pytest + ruff + osv; starters: build
  parity + smoke) including the KB-404 design: repo-state heavy scans SKIP on docs-only
  PRs, push-to-main always full, mixed-diff full; (2) Dependabot config — per-repo
  `.github/dependabot.yml` pinned to org policy (weekly, grouped minor+patch, majors
  route to the judgment lane); (3) Branch protection — the protection ruleset
  (pull_request + required status checks matching that repo's CI contexts, enforced to
  admins) applied AFTER that repo's CI is green on main; NEVER protection-without-signal;
  (4) SAST ownership — the workflow owns the CodeQL backlog: budgeted triage each run
  (fix or typed suppression-with-rationale), driving open alerts to zero and holding.

**D2 — Deterministic steering, judgment carved out.** The graph owns the loop; model
turns exist only where classification leaves doubt. Decision-adapter confidence gates:
mechanical-consolidation and rebase acts are deterministic shell checklists; anything
matching the judgment class parks in the run's report artifact for a dave go.

**D3 — Review is required and SCOPED.** Every PR from this family goes through the
pipeline's review path like any develop run — no bypass, no merge without the standard
approval. The reviewer prompt for this family is DIFF-SCOPED: verify the diff closes the
named advisory/target and does not change behavior beyond it — not a full feature
review. Small targeted diffs should produce a mostly-approve verdict distribution; a
changes-requested on a mechanical bump is a real finding and parks with evidence.

**D4 — Secrets and identity.** GH tokens flow exclusively through the engine's secret
providers (workflow_github_token class); no raw token in env; brokenbot owns merges;
handcaught is the develop identity. All ruleset/config application acts are verified by
API read-back before the run marks the repo compliant.

**D5 — Scope.** 16 product repos per the 2026-11-14 survey: criteria, both SDKs (+ all
three starters, greeter, claude/codex-class adapters), proto, copilot, shell, noop,
decision, go-sdk, castle, workflow-example. EXCLUDED by class (dave-confirmed
"as surveyed"): homebrew-criteria, publish-adapter, smolcourse-sft-mlx, capture_the_mac.
Tiering inside scope: Tier A (criteria — bring CI gate-posture + CodeQL backlog to zero),
Tier B (10 adapters/SDKs — CI template + protection + dependabot), Tier C (3 starters +
greeter — minimal pack), with castle/workflow-example as Tier B (workflow-aware repos).

**D6 — Cadence and first run.** Hermes cron fires the weekly loop (the platform has no
native schedule; cron fires, the workflow owns the loop with checkpoints, not sleep
polls). The FIRST run is a guided sweep: it produces the full inventory artifact, burns
the 42-PR backlog (consolidated PRs per module, rebases, landings), applies the Lane-B
compliance pack repo by repo with read-back verification, and reports per-repo
before/after deltas. Subsequent runs are delta-driven.

## Consequences

- The dependency/CI/SAST backlog becomes owned: sweeps run on cadence, consolidations
  collapse same-target bumps (four otel PRs = one change), and stale-class PRs
  (actions 4→7) land or close with evidence instead of aging.
- Branch protection rolls out from one repo to sixteen WITHOUT locking a repo behind a
  red gate — protection lags CI-green per repo by construction.
- The repo-state-scan collision class (doc-only PRs tripping vuln gates) dies with the
  KB-404 design included in the CI templates rather than as a criteria-only patch.
- Rejected alternatives: fully automerging dependabot PRs without consolidation (keeps
  N PRs per change and drowns review); hand-run sweeps as cron scripts (already
  demonstrated they don't recur); extending the develop pipeline per-repo (develop lanes
  are already per-repo serialized and are the wrong shape for cross-repo sweeps).
- Sequencing: the series files AFTER the in-flight KB-83/KB-403/KB-404 legs (they resolve
  the originating instance); the guided sweep does not collide with a busy lane because
  it serializes per repo.

## Wave decomposition (filed as cards on the intake board)

1. **ADR commit** (this document) + per-repo inventory adapter step spec.
2. **Lane A graph** (collect/classify/route/act/merge/close-out) + example repo set.
3. **Guided sweep run** (disarmed until 1-2 land): full inventory + backlog burn-down +
   compliance application + delta report.
4. **Lane B CI templates** per language (Go/TS/PY/starter) carrying the docs-only skip.
5. **Protection + dependabot application** with read-back verification.
6. **CodeQL backlog triage loop** (budgeted per run, typed suppressions recorded).
7. **Weekly cadence + validation run** (disarmed): dry inventory, one consolidated merge,
   one protection application, delta report verified.
