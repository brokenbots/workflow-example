# Compile expectations matrix — trees vs engine version bands

This document is the **compile-expectations surface of record**: it states,
per tree, what compiling the tree is expected to yield on the engine, and the
engine version band the expectation applies inside. It is deliberately
separate from `docs/harness-expectations.md` (the runtime expectations
matrix, whose item-id space is bounded at 49 slots): compile expectations
have no item ids of their own — a tree is one row, machine-checked by its
guard (`node scripts/compile_matrix.mjs`), wired into `make validate` and
into the CI `k8s-shell-tests` job beside the engine install.

Two rules are structural:

1. **A row's expectation is asserted live, not recited.** The guard runs the
   installed engine's `validate` for every row and fails closed when the
   observed compile result contradicts the declared expectation, when a tree
   row is missing/duplicated/malformed, or when the installed engine sits
   outside the row's stated version band. Crossing a band's ceiling is a
   re-derivation, never an edit in place.
2. **Compile expectations and runtime observations are separate surfaces.**
   A compile row says nothing about run-time behavior; the tree's standalone
   suite (for harness trees, its `tests/` leg) remains the runtime assertion
   carrier, pinning the run-event classes each compile-compliant shape
   actually emits.
3. **Band-banded trees only carry rows.** A tree whose compile expectation
   is version-independent stays off this matrix — `make validate` already
   compiles every tree it deploys; a row exists only where the expectation
   is a function of the engine's version band, and the guard fails closed if
   a row's band does not bracket the installed engine exactly.

## Matrix

| Tree | Expectation | Engine band | Compiled shape | Runtime carrier |
|---|---|---|---|---|
| `harness_contracts_v1` | `ok` | `>=0.5.57, <0.6.0` | the probe steps' every outcome arm resolves through a declared arm under the absolute 0.5.57+ outcome-completeness verifier (no engine escape hatch exists): the never-finalize C18b probe (`contract_unmapped`) ships its declared `finalize_exhausted` fallback arm beside the schema-bearing `approved` arm, its no-finalize lane caught by name and routed to the `failed` terminal | `harness_contracts_v1/tests/test_contracts_standalone.sh` — F8 pins the fallback commit plus the loud AND routed terminal (`RunCompleted` on the failed terminal, the retired `outcome_not_allowed` class asserted ABSENT), F7/F8b pin budget exhaustion and default-lane routing, F9 pins the shipped chain end-to-end |
