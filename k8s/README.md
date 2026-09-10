# Kubernetes workflow runner deployment

This directory contains the Kubernetes manifests and launchers for running a
Criteria workflow (currently `linear_intake_v1`) against a Linear ticket inside
a cluster. The recommended deployment is operator-based and autonomous:

* The `criteria-k8s-operator` reconciles a `CriteriaRun` custom resource into
  a runner `Job` plus one adapter `Job` per adapter type.
* The `criteria-linear-watcher` polls Linear and creates a `CriteriaRun` for each
  ticket that reaches the configured Triage state.
* A manual path lets you create `CriteriaRun` objects by hand for testing.
* The host-based launcher scripts remain available for local testing only.

---

## Table of contents

1. [Architecture](#architecture)
2. [Autonomous flow](#autonomous-flow)
3. [Setup](#setup)
4. [Running a workflow](#running-a-workflow)
5. [Reading run status](#reading-run-status)
6. [Debugging](#debugging)
7. [Security](#security)
8. [Files in this directory](#files-in-this-directory)

---

## Architecture

An operator-based run is driven by the `criteria-k8s-operator` and scheduled as
a group of batch/v1 Jobs in the `criteria-jobs` namespace.

```text
Linear ticket in Triage
        |
        v
+---------------------------+
| criteria-linear-watcher |  (polls Linear, creates CriteriaRun)
+---------------------------+
        |
        v
+---------------------------+
|  criteria-k8s-operator    |  (reconciles CriteriaRun)
+---------------------------+
        |            |            |
        v            v            v
   +---------+ +-------------+ +-------------+
   | runner  | | adapter Job | | adapter Job |
   | Job     | |   shell     | |   copilot   |
   +---------+ +-------------+ +-------------+
```

| Resource | Role |
|----------|------|
| `CriteriaRun` | Custom resource that describes the ticket to process, repository, image, and runtime settings. |
| `criteria-linear-watcher` | Polls Linear for tickets in the Triage state and creates a `CriteriaRun` for each one. |
| `criteria-k8s-operator` | Watches `CriteriaRun` resources and reconciles them into child Jobs. |
| Runner Job | `repo-clone` init container clones the repository; `workflow-runner` runs `criteria apply` and hosts the remote shim. |
| Adapter Job | One Job per adapter type (`shell`, `copilot`). Adapters phone home to the runner shim over the pod network. |

The runner Job mounts the `linear-spc` CSI volume for the Linear API key and the
`copilot-spc` CSI volume for the two GitHub tokens. Adapter Jobs mount only the
shared `/data` and `/repo` PVCs and have no CSI volumes, no Kubernetes
Secrets, and no service account token auto-mount.

The runner starts the Criteria shim on `0.0.0.0:7778` using the pod IP and
writes per-run connection metadata to `/data/.criteria/runs/<job-name>/`:

| Discovery file | Contents |
|----------------|----------|
| `host` | Runner pod IP and port (`<pod-ip>:7778`). |
| `token` | Per-run `accept_token` bearer token. |
| `digest-shell` | Pinned SHA256 digest of the `shell` adapter. |
| `digest-copilot` | Pinned SHA256 digest of the `copilot` adapter. |

Adapters poll those files and then connect to the shim. The adapter's identity
is verified against the pinned digest and the `accept_token` bearer token.

---

## Autonomous flow

When a Linear ticket in the watched project is moved to the `Triage` state:

1. The `criteria-linear-watcher` reads the ticket, extracts a repository URL
   from the description, and checks whether a `CriteriaRun` for that ticket is
   already active.
2. If no active run exists, the watcher creates a `CriteriaRun` in the
   `criteria-jobs` namespace. The `CriteriaRun` carries the ticket ID,
   repository URL, image, provider URL, and default gate settings.
3. The `criteria-k8s-operator` sees the new `CriteriaRun` and creates the child
   Jobs: one runner Job and one adapter Job for each adapter type
   (`shell`, `copilot`).
4. The runner Job clones the repository and runs `criteria apply`. The engine
   resolves workflow secrets from the runner's CSI mounts and delivers them to
   adapters over the OpenSession SDK channel.
5. Adapter Jobs start, read the runner's dial address, bearer token, and pinned
   digest from the shared `/data` PVC, and phone home to the runner shim.
6. When the runner Job finishes, the operator reads the run's `events.ndjson`
   file and updates `CriteriaRun` status with the final phase, PR number, and
   ticket state.

---

## Setup

### Prerequisites

* A Kubernetes cluster with amd64 worker nodes. The manifests use
  `nodeSelector: kubernetes.io/arch: amd64` and tolerate the `catch` and
  `node-role.kubernetes.io/control-plane` taints.
* Container images built and available to the cluster. The workflow image is
  `localhost:5000/linear-intake-remote:dev` and the operator image is
  `localhost:5000/criteria-k8s:dev`. Build them locally and push to a registry
  the cluster can reach.
* A local insecure registry mirror if you use `localhost:5000`. Copy
  `k8s/registries.yaml` to `/etc/rancher/k3s/registries.yaml` and restart k3s:

  ```sh
  sudo cp k8s/registries.yaml /etc/rancher/k3s/registries.yaml
  sudo systemctl restart k3s
  ```
* `kubectl` and `helm` (for CSI driver installation) on your workstation.

### 1. Create the namespace and RBAC

```sh
kubectl apply -f k8s/00-namespace.yaml
kubectl apply -f k8s/02-serviceaccount.yaml
```

### 2. Install the Secrets Store CSI driver and OpenBao CSI provider

The helper script installs both charts with the tuning values in this directory:

```sh
./k8s/install-secrets-store-csi.sh
```

To point the provider at a different OpenBao instance:

```sh
EXTERNAL_BAO_ADDR=http://openbao-0.default.svc.cluster.local:8200 \
  ./k8s/install-secrets-store-csi.sh
```

Verify the driver and provider pods are ready:

```sh
./k8s/verify-secrets-store-csi.sh
```

### 3. Configure OpenBao

Enable the Kubernetes auth method, write the `criteria-jobs` policy, and create
a role bound to the `criteria-runner` service account:

```sh
export VAULT_TOKEN=<root-token>

kubectl exec -n default openbao-0 -- \
  vault auth enable kubernetes || true

kubectl exec -n default openbao-0 -- \
  vault policy write criteria-jobs - < k8s/criteria-jobs-policy.hcl

kubectl exec -n default openbao-0 -- \
  vault write auth/kubernetes/role/criteria-jobs \
    bound_service_account_names=criteria-runner \
    bound_service_account_namespaces=criteria-jobs \
    policies=criteria-jobs \
    ttl=1h
```

Store the credentials in OpenBao:

```sh
kubectl exec -n default openbao-0 -- \
  env VAULT_TOKEN=$VAULT_TOKEN vault kv put criteria/linear \
    linear_api_key="$LINEAR_API_KEY" \
    workflow_github_token="$WORKFLOW_GITHUB_TOKEN" \
    reviewer_github_token="$REVIEWER_GITHUB_TOKEN"
```

### 4. Create the PVCs

```sh
kubectl apply -f k8s/03-pvc.yaml
```

### 5. Apply the operator prerequisites

The operator and its adapter Jobs need the `linear-spc` and `copilot-spc`
`SecretProviderClass` objects and the `pod-adapter-scripts` ConfigMap. Apply
all three with the dedicated prerequisite manifest:

```sh
kubectl apply -f k8s/operator-prereqs.yaml
```

### 6. Build and push the operator image

```sh
make build-criteria-k8s
docker push localhost:5000/criteria-k8s:dev
```

Use `podman push` instead of `docker push` if your build tool is podman.

### 7. Install the operator

```sh
kubectl apply -f criteria-k8s/config/install.yaml
```

This creates the `criteria-jobs` namespace, the `CriteriaRun` CRD, the
operator's RBAC, and the `criteria-k8s-operator` and `criteria-linear-watcher`
Deployments.

### 8. Verify the watcher picks up Linear tickets

Wait for the operator and watcher to be ready:

```sh
kubectl wait -n criteria-jobs deployment/criteria-k8s-operator \
  --for=condition=Available --timeout=120s
kubectl wait -n criteria-jobs deployment/criteria-linear-watcher \
  --for=condition=Available --timeout=120s
```

Tail the watcher logs:

```sh
kubectl logs -n criteria-jobs deployment/criteria-linear-watcher -f
```

Move a Linear ticket in the watched project to the `Triage` state and include a
repository URL in the ticket. The watcher should log that it created a
`CriteriaRun` for the ticket.

---

## Running a workflow

### Autonomous path

Once the watcher is running, move any Linear ticket in the watched project to
`Triage`. The watcher creates a `CriteriaRun`; the operator reconciles it into
Jobs automatically.

### Manual path

Create a `CriteriaRun` by hand for testing. Edit the example manifest to set the
right ticket, repository, image, and provider URL:

```sh
kubectl apply -f criteria-k8s/config/examples/criteriarun.yaml
```

You can also write a run from scratch:

```yaml
apiVersion: criteria.brokenbots.dev/v1
kind: CriteriaRun
metadata:
  name: cri-42
  namespace: criteria-jobs
spec:
  ticketId: "CRI-42"
  repoUrl: "https://github.com/brokenbots/workflow-example.git"
  image: "localhost:5000/linear-intake-remote:dev"
  buildCmd: "make build"
  testCmd: "make test"
  ciGateCmd: "make ci-gate"
  maxAgentVisits: 2
  providerBaseUrl: "http://192.168.17.116:11434/v1"
```

After creating it, the operator creates the runner and adapter Jobs.

### Host-based launchers (local testing only)

The launcher scripts are still available for local testing but they bypass
the operator and the `CriteriaRun` CRD:

* `./k8s/launch-pod-adapter-job.sh` renders and applies a runner Job plus one
  adapter Job per adapter type directly.
* `./k8s/launch-ticket-job.sh` renders and applies the legacy single-container
  Job template (`05-job-template.yaml`).

These launchers are intended for local debugging only; production runs should
use the operator and watcher.

### Watch the run

List the child Jobs for a run:

```sh
kubectl get jobs -n criteria-jobs -l criteria.brokenbots.dev/run=cri-42
```

Follow the runner logs:

```sh
kubectl logs -n criteria-jobs job/cri-42 -c workflow-runner -f
```

Wait for completion:

```sh
kubectl wait -n criteria-jobs job/cri-42 --for=condition=complete --timeout=60m
```

---

## Reading run status

`kubectl get criteriarun` shows the run summary:

```sh
kubectl get criteriarun -n criteria-jobs
```

Output columns include `Ticket`, `Phase`, `Job`, `PR`, and `Age`. The `Phase`
reflects the lifecycle of the child runner Job (`Pending`, `Running`,
`Succeeded`, or `Failed`). The `PR` column shows the pull request number produced
by the run when one was created.

For the full status, including the events file path:

```sh
kubectl describe criteriarun <run-name> -n criteria-jobs
```

The run's events file lives at `/data/intake/<ticket-id>/events.ndjson` on the
`criteria-data` PVC. The same path is recorded in the `CriteriaRun` status as
`eventsPath`. After the runner Job finishes, the operator reads this file to
populate `prNumber` and `ticketState`.

---

## Debugging

### Read container logs

The runner Job has a `repo-clone` init container and a `workflow-runner`
container. Each adapter Job has a single `adapter-shell` or `adapter-copilot`
container. Read each one separately:

```sh
kubectl logs -n criteria-jobs job/<run-name> -c workflow-runner
kubectl logs -n criteria-jobs job/<run-name>-adapter-shell -c adapter-shell
kubectl logs -n criteria-jobs job/<run-name>-adapter-copilot -c adapter-copilot
kubectl logs -n criteria-jobs job/<run-name> -c repo-clone
```

Typical failures:

* `repo-clone` exits non-zero when the workflow GitHub token cannot read the
  repository, or when `REPO_URL` is missing.
* `workflow-runner` exits if the Linear API key is missing, `ALLOW_DIRTY` is
  invalid, or workflow validation fails.
* Adapter containers loop or crash when they cannot reach the runner shim,
  when the per-run discovery files are missing, or when the pinned adapter
  digest does not match the binary in the image.

### Check the Linear issue state

Look at the ticket in Linear. The workflow moves the issue through the
configured states:

1. `Triage` when a bug is classified
2. `In Progress` before the handler starts
3. `Done` after the PR merges
4. `In Review` when the run cannot proceed autonomously

A failure comment is posted to the issue when a step exits non-zero or when a
gate rejects its input.

### Inspect events and artifacts

Per-ticket artifacts live on the `criteria-data` PVC at
`/data/intake/<ticket_id>/`:

```sh
# Find the node/PV path if using local-path storage, or run a debug pod.
kubectl run -n criteria-jobs debug --rm -i --tty \
  --image=busybox --overrides='{"spec":{"volumes":[{"name":"data","persistentVolumeClaim":{"claimName":"criteria-data"}}],"containers":[{"name":"debug","image":"busybox","volumeMounts":[{"name":"data","mountPath":"/data"}],"stdin":true,"tty":true}]}}'
```

Key files:

| Path | Contents |
|------|----------|
| `/data/intake/<ticket_id>/events.ndjson` | One JSON object per workflow event. This is the primary trace. |
| `/data/intake/<ticket_id>/ticket.json` | Raw Linear issue payload. |
| `/data/intake/<ticket_id>/<ticket_id>.md` | Classified bug report. |
| `/data/intake/<ticket_id>/workstreams/<ticket_id>.md` | Feature workstream or QA-confirmed bug workstream. |
| `/data/intake/<ticket_id>/worktree/` | Handler's isolated worktree. |
| `/data/intake/<ticket_id>/review-notes.md` | Triage reviewer's reasoning. |
| `/data/intake/<ticket_id>/intake-notes.md` | Questions posted to Linear. |

### Verify CSI secret mounts

If the runner fails with "required via /secrets/...", check that the CSI
volume mounted correctly:

```sh
kubectl exec -n criteria-jobs job/<run-name> -c workflow-runner -- \
  ls -la /secrets/

kubectl exec -n criteria-jobs job/<run-name> -c workflow-runner -- \
  cat /secrets/linear_api_key | head -c 8
```

You should see a single file with mode `0600` owned by the `fsGroup` user
(`10001`).

### Regenerate the prerequisite manifest

`k8s/operator-prereqs.yaml` and `k8s/job-cri-27.yaml` are generated from
`k8s/job-cri-27.yaml.tmpl` by embedding the runner and adapter wrapper scripts.
After editing either wrapper script, regenerate both files:

```sh
./k8s/generate-pod-adapter-manifest.sh
```

---

## Security

### Secret isolation

The runner Job is the only pod that mounts credential volumes:

* `workflow-runner` mounts only the Linear API key at `/secrets/linear_api_key`
  and the two GitHub tokens at `/home/criteria/secrets`.
* `repo-clone` mounts only the workflow GitHub token at `/home/criteria/secrets`.
* Adapter Jobs do not mount any CSI volume, Kubernetes Secret, or ConfigMap
  containing credentials.

This limits the blast radius of a container compromise: a vulnerability in an
adapter Job cannot leak a credential that was never present in its filesystem or
environment.

### Secrets Store CSI driver

Credentials are stored in OpenBao and injected by the CSI driver as files
inside the runner's containers. They are never written to the pod spec, never
exposed as environment variables, and never persisted in the container image.
The driver mounts each volume read-only with `0600` permissions. The OpenBao
role is bound to the `criteria-runner` service account and namespace and has a
short TTL (one hour by default).

### OpenSession secret delivery

The runner passes the GitHub tokens to the engine as `file:` variable origin
references (`file:/home/criteria/secrets/workflow_github_token`). The engine's
secret provider stack resolves those files at startup and delivers the values
to adapters over the OpenSession SDK contract. Adapters never read workflow
credentials from their own process environment or filesystem.

### Remote bridge authentication

Adapters authenticate to the runner shim with two mechanisms:

1. **Per-run `accept_token`**: the runner generates a 32-byte alphanumeric token
   per run and writes it to the shared `/data` PVC. The runner and adapter Jobs
   agree on the token; nothing outside the runner's Job can read it.
2. **Pinned digest**: each adapter resolves its SHA256 digest from the workflow
   lockfile and presents it during the OpenSession handshake. The shim rejects
   connections whose identity does not match the lockfile pin.

The default deployment uses the pod network (`<runner-pod-ip>:7778`) because the
runner and adapters run in separate pods. `accept_token` is required for
non-loopback listen addresses.

### mTLS between pods (if cross-pod)

If you split the runner and adapters across separate network zones, the Criteria
remote bridge crosses pod boundaries. In that configuration you must:

* Issue a client certificate for each adapter and a server certificate for the
  shim.
* Mount the certificates through the CSI driver or cert-manager, not as plain
  Secret env vars.
* Configure the adapter environment to use TLS (`https://` or the equivalent
  Criteria transport option) and set `accept_token` to a cryptographically
  random value generated per run and stored only in the shared PVC.
* Restrict network policy so only adapter pods can reach the runner pod on the
  remote port.

The manifests in this directory use the in-cluster pod network and do not
configure cross-pod mTLS.

### Pod security

* `runAsNonRoot: true`, `runAsUser: 10001`, `fsGroup: 10001`.
* `seccompProfile: RuntimeDefault` for the operator and watcher.
* No `privileged: true`, no `--security-opt`.
* Adapter Jobs disable service account token auto-mount.
* The namespace enforces the `restricted` Pod Security Standard.
* Only the `csi` driver DaemonSets require host-level access; the workflow Jobs
  do not.

---

## Files in this directory

| File | Purpose |
|------|---------|
| `00-namespace.yaml` | `criteria-jobs` namespace with restricted pod security. |
| `01-openbao-config.yaml` | Reference ConfigMap for the OpenBao Kubernetes auth role and policy. |
| `02-serviceaccount.yaml` | `criteria-runner` ServiceAccount and RBAC. |
| `03-pvc.yaml` | `criteria-data` and `criteria-repo` PVCs. |
| `04-castle.yaml` | Optional Castle control-plane Deployment and Service. |
| `05-job-template.yaml` | Legacy single-container Job template. |
| `operator-prereqs.yaml` | `SecretProviderClass` and `ConfigMap` objects required by the operator. |
| `job-cri-27.yaml.tmpl` | Pod-adapter Job group template with embedded script placeholders. |
| `job-cri-27.yaml` | Generated pod-adapter manifest used by the local launcher. |
| `pod-adapter-runner.sh` | Runner wrapper script; becomes the `runner.sh` entry in the `pod-adapter-scripts` ConfigMap. |
| `pod-adapter-adapter.sh` | Adapter wrapper script; becomes the `adapter.sh` entry in the `pod-adapter-scripts` ConfigMap. |
| `generate-pod-adapter-manifest.sh` | Embeds the wrapper scripts into `job-cri-27.yaml` and `operator-prereqs.yaml`. |
| `launch-pod-adapter-job.sh` | Renders and applies the pod-adapter Job group directly. |
| `launch-ticket-job.sh` | Renders and applies the legacy `05-job-template.yaml`. |
| `install-secrets-store-csi.sh` | Helm installer for the CSI driver and OpenBao provider. |
| `verify-secrets-store-csi.sh` | Waits for the CSI driver/provider DaemonSets and SPCs. |
| `create-secret-from-openbao.sh` | Pulls credentials from OpenBao into a Kubernetes Secret for the legacy path. |
| `criteria-jobs-policy.hcl` | OpenBao policy granting read access to `criteria/data/*`. |
| `registries.yaml` | k3s containerd mirror for insecure local registries. |
| `values-secrets-store-csi-driver.yaml` | Helm values for the upstream CSI driver chart. |
| `values-openbao-csi-provider.yaml` | Helm values for the OpenBao CSI provider chart. |
| `examples/ticket-job.yaml` | Apply-ready example Job for the local launcher path. |
| `tests/` | Regression tests for the manifests and launchers. |
| `criteria-k8s/config/install.yaml` | Operator install manifest (CRD, RBAC, operator and watcher deployments). |
| `criteria-k8s/config/examples/criteriarun.yaml` | Example `CriteriaRun` for the manual path. |
