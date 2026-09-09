# workflow-example

Repository for Criteria-based triage and workstream-handler workflows used by
BrokenBots.

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
