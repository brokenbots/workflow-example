# workflow-example

Repository for Criteria-based triage and workstream-handler workflows used by
BrokenBots.

## Workflow trees

- `linear_intake_v1`, `linear_triage_v1`, `linear_develop_v1`, `ticket_cleanup_v1`:
  the Linear ticket pipelines routed via the `criteria-routes` ConfigMap
  (`k8s/examples/routes-configmap.yaml`).
- `decision_demo_v1` (KB-205, ADR-0013): the runnable proof of the decision
  adapter's composition shape. One decision step asks a System One model for
  a noul answer (urgency), a choice (department), and a score (severity)
  about a ticket; a switch routes on the answers — confident models dispatch
  on the department choice, while a department confidence under
  `route_conf_floor` pauses the run on a human approval node (matching arm
  is evaluated to the approval first). A second human gate handles
  urgencies, high-severity scores, and security department choices.
  Endings cover both queue terminals, the escalation terminal, the approval
  abandonment, and the model-failure terminal, with a routing summary
  output capturing the model's facts.
- `decision_demo_local_v1`: the same grammar pointed at
  `http://localhost:11434` (an Ollama System One backend, v0.35.1 or newer)
  with no credentials and no secrets block, proving the composition is
  backend-agnostic; its adapter config carries the only difference. The
  validated test bed on the GPU-light demo host is model `tev1:0.8b`
  (KB-209); `clef-flash` is the original pinned example model name, and any
  Ollama System One model can be substituted.
- Both trees validate and compile with `make validate`, and
  `decision_demo_v1/tests/` pins the compiled graph (arm order, every answer
  type exercised in routing, the confidence-gate approval path,
  credential discipline). `decision_demo_local_v1/tests/` runs a 7-scenario
  end-to-end regression against a stub System One backend
  (`stub_systemone.mjs`) where the unpublished decision adapter binary is
  available; `k8s/examples/routes-configmap.yaml` carries the
  `decision-demo-url` object prepared for the KB-207 routing run.

## Kubernetes deployment

The `k8s/` directory contains a Kubernetes Job template and launcher for running
`linear_intake_v1` against a ticket. The Job uses the remote-mode image:

```text
localhost:5000/linear-intake-remote:dev
```

Remote mode runs unprivileged: the pod uses a non-root security context and the
Criteria adapters connect in-pod to `127.0.0.1:7778`. Legacy sandbox-era
privileges are not required.

### Prerequisites

- A Kubernetes cluster with amd64 catch nodes. The template uses
  `nodeSelector: kubernetes.io/arch=amd64` and tolerates the `catch` taint.
- PVCs named `criteria-data` (mounted at `/data`) and `criteria-repo` (mounted
  at `/repo`). Override the names with `DATA_PVC` and `REPO_PVC` if needed.
- A Kubernetes Secret named `linear-intake-credentials` in the target namespace
  with the keys `LINEAR_API_KEY`, `WORKFLOW_GITHUB_TOKEN`, and
  `REVIEWER_GITHUB_TOKEN`. Alternatively, set `CREATE_SECRET=true` when running
  the launcher to create the Secret from the local environment.

### Launch a job

```sh
export TICKET_ID=CRI-110
export REPO_URL=brokenbots/workflow-example
export LINEAR_API_KEY=...
export WORKFLOW_GITHUB_TOKEN=...
export REVIEWER_GITHUB_TOKEN=...

./k8s/launch-ticket-job.sh
```

The launcher defaults `IMAGE` to `localhost:5000/linear-intake-remote:dev`,
renders the template, optionally creates the credentials Secret, and applies the
Job. The default Job name lowercases the ticket id: `linear-intake-cri-110`.

To preview the rendered manifest without applying it:

```sh
DRY_RUN=1 ./k8s/launch-ticket-job.sh
```

### Credential handling

The container entrypoint reads the three required tokens from environment
variables first. When a variable is absent, it falls back to reading the
corresponding file from a `/secrets` CSI mount (`/secrets/linear_api_key`,
`/secrets/workflow_github_token`, `/secrets/reviewer_github_token`). The Job
template supplies credentials through a Kubernetes Secret by default, but a
CSI-backed `/secrets` volume can be used instead.
