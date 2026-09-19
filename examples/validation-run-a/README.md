# validation-run-a fixture (CRI-244, M10.2)

Minimal remote workflow + remote subworkflow fixture for **validation run A**
(plan CRI-214 exit condition 1): a live run in which the criteria-base image
loads a REMOTE root workflow by git URL, pinned per ADR-0005 D7, whose
subworkflow is ALSO loaded remotely (cascade per CRI-227), with the nested
cache layout, lockfile pins, and run-metadata provenance demonstrated.

## Layout

- `root/` — the root workflow module. It declares the child as a REMOTE
  subworkflow (git URL, `ref` pinned per D7) and reads the child's exported
  evidence output from a final shell step. Its `.criteria.lock.hcl` carries
  the shell adapter pin and the child's `workflow_ref` pin.
- `child/` — the subworkflow module, fetched remotely at its own git URL. It
  executes one deterministic shell step on the pinned shell adapter and posts
  the run's own evidence comment on the ticket under test (no-op when the
  Linear key is not supplied). Its `.criteria.lock.hcl` pins the shell
  adapter.

The child's git URL is pinned by commit SHA in `root/main.chcl` — the root's
tree contains no child copy, so the fetch cascades into
`cache/workflows/<root-slug>/subworkflows/<child-slug>/<version>`.

## Running it

Via the criteria-base entrypoint contract (what the operator does for a
source-mode CriteriaRun; no DinD, the image is built and pushed on the host):

```sh
docker/podman build --build-arg TARGETARCH=amd64 \
    -f criteria-base/Dockerfile -t localhost:5000/criteria-base:<build-tag> criteria-base/
docker/podman push localhost:5000/criteria-base:<build-tag>
kubectl -n criteria-jobs set env deploy/criteria-k8s-operator \
    CRITERIA_BASE_IMAGE=localhost:5000/criteria-base:<build-tag>

docker/podman run --rm -e WORKFLOW_URL \
    "git::https://github.com/brokenbots/workflow-example.git//examples/validation-run-a/root?ref=<root-commit>" \
    -e WORKFLOW_REF=<root-commit> \
    localhost:5000/criteria-base:<build-tag> \
    --var ticket_id=CRI-244 --var linear_api_key=$LINEAR_API_KEY --output concise
```

Engine compatibility: the fixture requires the CRI-227 cascade machinery, so
both modules declare `criteria_version = ">=0.5.24-10, <0.6.0"` — the
prerelease lower bound matches the audited base-image build exactly
(`v0.5.24-10-geae0181`, the commit pinned in `criteria-base/Dockerfile`).

## What the run proves

- The base image fetches a remote root workflow by git URL and enforces the
  declared `WORKFLOW_REF` pin (D7, CRI-226) before execution.
- The root's remote subworkflow source cascades into the nested cache layout
  under the root slug (CRI-227), with the warm-cache index recording both
  fetches (D5).
- The run records provenance at `runs/<run-id>/run-metadata.json` (D6):
  redacted source, resolved ref, cache path, fetch time.
- Adapter digests are lockfile-pinned in both modules and the child's
  resolved commit is pinned in the root's lockfile (`workflow_ref`, D7).
- The run reaches a success terminal state; the child posts the run's own
  evidence comment on the ticket.