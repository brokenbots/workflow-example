# validation-run-b (CRI-245, M10.3)

Validation run B (plan CRI-214 exit condition 2, dev leg) is EVIDENCE, not a
code change: prove that a Linear ticket sitting at **Ready for Development**
automatically fires `linear_develop_v1` through the `criteria-develop` dev
route — the split pattern's automatic handoff (M10.1) — and completes the dev
leg end to end (develop -> PR -> reviewer loop -> merge -> Done). Per the
validation design, the point is the ROUTE + WORKFLOW proof, not the change.

## How it is run

Unlike run A (`examples/validation-run-a/`, a purpose-built workflow
fixture), run B needs no repo fixture. A purpose-built carrier ticket on the
Criteria project, labelled `k8s-run`, sits at [Ready for Development]; the
linear-watcher's `criteria-develop` route (see
`k8s/examples/routes-configmap.yaml`) matches it on the next poll and creates
the CriteriaRun automatically — no manual kickoff of the develop workflow.
The carrier's change is trivial by design (its own run records
`carrier-note.md` in this directory); it exists only to carry the run.

## Evidence

Evidence of the run is recorded on the tickets (CRI-245 and the carrier):
the fired run id, the matched route, the develop workflow that executed
(`linear_develop_v1`, not `linear_intake_v1`), and the terminal Done state.
On the shared data PVC, the CriteriaRun name (`cri-<ticket>-<timestamp>` under
`/data/.criteria/runs/`) identifies the pod run, and the engine-side run
summary (`/data/criteria/runs/<run-id>.json`) names the workflow that
executed and the steps it visited.