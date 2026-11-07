# pair_loop_parity fixtures — SELF-TEST ARMS ONLY

Every file in this directory is a **SELF-TEST ARM** of
`workstream_handler_v1/tests/test_pair_loop_parity.sh`.

**These are NOT a real v1 baseline.** The durations in them are SYNTHETIC: they
exist only to exercise the comparator's arithmetic (a regressed slot must fail,
an in-threshold candidate must pass, a broken record must error). KB-386 is
**UN-ARMED** (no `k8s-run`), so **no real-card timing record exists** anywhere in
this repo. No number in this directory is a measurement, and none of them gate
KB-386.

The real records are collected by the operator's post-merge fleet lane, per
`workstream_handler_v1/workflows/pair_programming_loop_v2/PARITY.md` (collection
steps C1 → C4).

| Fixture | Arm |
|---------|-----|
| `self_test_v1_baseline.tsv` | the synthetic "v1 baseline" record of the self-test (NOT a real v1 baseline) |
| `self_test_v2_candidate_pass.tsv` | the in-threshold candidate — must PASS |
| `self_test_v2_candidate_regressed_delivery.tsv` | `delivery_leg` regressed past its threshold — must FAIL |
| `self_test_v2_candidate_regressed_gate.tsv` | `gate_leg` regressed — must FAIL |
| `self_test_v2_candidate_regressed_review.tsv` | `review_leg` regressed — must FAIL |
| `self_test_v2_candidate_regressed_plan_budget.tsv` | `plan_leg` over its budget — must FAIL |
| `self_test_v2_candidate_regressed_log.tsv` | `log_leg` regressed — must FAIL |
| `self_test_v2_candidate_unknown_step.tsv` | an unmapped step id — must ERROR |
| `self_test_v2_candidate_malformed_ms.tsv` | a non-numeric duration — must ERROR |
| `self_test_v2_candidate_negative_ms.tsv` | a negative duration — must ERROR |
| `self_test_v2_candidate_empty.tsv` | an empty record — must ERROR |
| `self_test_v2_candidate_missing_mandatory.tsv` | a mandatory slot missing — must ERROR |
| `self_test_v2_candidate_v1_step.tsv` | a v1-only step id in a v2 record — must ERROR |
