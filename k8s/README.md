# Kubernetes workflow runner deployment

This directory contains the Kubernetes manifests and launchers for running a
Criteria workflow (currently `linear_intake_v1`) against a Linear ticket inside
a cluster. The deployment is split into two paths:

* **Pod-adapter Job (recommended)** — `job-cri-27.yaml` / `launch-pod-adapter-job.sh`.
  The workflow engine and each adapter run as separate containers in one pod.
  Secrets are mounted with the Secrets Store CSI driver and OpenBao; no
  credential is passed as a pod environment variable.
* **Legacy single-container Job** — `05-job-template.yaml` /
  `launch-ticket-job.sh`. All adapters run in one container and credentials are
  supplied through a Kubernetes Secret via `envFrom`.

The pod-adapter path is the default for new work. This README focuses on it,
but the legacy launcher is still documented below for existing environments.

---

## Table of contents

1. [Architecture](#architecture)
2. [Secret flow](#secret-flow)
3. [Setup](#setup)
4. [Running a workflow](#running-a-workflow)
5. [Debugging](#debugging)
6. [Security](#security)
7. [Files in this directory](#files-in-this-directory)

---

## Architecture

A pod-adapter run is a Kubernetes `Job` with three long-lived containers plus an
init container:

| Container | Role |
|-----------|------|
| `repo-clone` (init) | Clones the ticket's GitHub repository into an `emptyDir` volume shared with the other containers. It reads only the workflow GitHub token from its own CSI mount. |
| `workflow-runner` | Runs `/usr/local/bin/criteria apply`. It owns the Criteria shim that listens on `127.0.0.1:7778` and drives the workflow. It mounts only the Linear API key. |
| `adapter-copilot` | Sidecar that runs the pinned `criteria-adapter-copilot` binary. It connects back to the shim on the pod loopback interface. It mounts both GitHub tokens. |
| `adapter-shell` | Sidecar that runs the pinned `criteria-adapter-shell` binary. It mounts only the workflow GitHub token. |

```text
┌─────────────────────────────────────────────────────────────────────┐
│ Pod: pod-adapter-<ticket>                                            │
│  ┌──────────────┐    ┌──────────────┐    ┌──────────────┐           │
│  │ repo-clone   │    │ workflow-    │    │ adapter-     │           │
│  │ (init)       │    │ runner       │    │ copilot      │           │
│  │              │    │ 127.0.0.1:7778│◄───│              │           │
│  │ /repo emptyDir│──►│ /data PVC    │◄───│ /data PVC    │           │
│  │ shell-spc    │    │ linear-spc   │    │ copilot-spc  │           │
│  └──────────────┘    └──────────────┘    └──────────────┘           │
│                                         ┌──────────────┐            │
│                                         │ adapter-shell│            │
│                                         │ 127.0.0.1:7778◄───────────│
│                                         │ shell-spc    │            │
│                                         └──────────────┘            │
└─────────────────────────────────────────────────────────────────────┘
```

The `workflow-runner` starts the Criteria engine against a copied workflow tree
(`/tmp/workflows`). The engine opens a remote listener on the pod loopback
address. The two adapter sidecars start after the init container finishes,
resolve their pinned digests from the workflow lockfile, and run the adapter
binaries in "phone-home" mode: each adapter opens a TCP connection to
`127.0.0.1:7778` and calls `OpenSession` over TCP. The session handshake uses a
per-run bearer token (`accept_token`) and the pinned adapter digest as identity.

This repository does not use a CustomResourceDefinition; workflows are scheduled
as Kubernetes Jobs. A separate optional `castle` Deployment provides a control
plane for agent registration and event buffering, but the intake run itself does
not require it.

---

## Secret flow

1. OpenBao stores the three credentials under `criteria/data/linear`:
   * `linear_api_key`
   * `workflow_github_token`
   * `reviewer_github_token`
2. The Secrets Store CSI driver and OpenBao CSI provider are installed as
   DaemonSets in namespace `csi`.
3. Three `SecretProviderClass` objects (`linear-spc`, `copilot-spc`, `shell-spc`)
   declare which OpenBao paths and keys each pod volume exposes. Each file is
   mounted with permission `0600`.
4. The Job mounts one CSI volume per container:
   * `workflow-runner` mounts only `linear-spc` → `/secrets/linear_api_key`
   * `adapter-copilot` mounts `copilot-spc` → `/secrets/workflow_github_token`
     and `/secrets/reviewer_github_token`
   * `adapter-shell` (and the `repo-clone` init container) mounts only
     `shell-spc` → `/secrets/workflow_github_token`
5. At startup each container reads its own credential files with `IFS= read -r`.
6. The runner generates a per-run bearer token, writes it to
   `/data/.criteria-remote-token` on the shared PVC, and rewrites the workflow's
   `adapters.chcl` so GitHub token references become `env:WORKFLOW_GITHUB_TOKEN`
   / `env:REVIEWER_GITHUB_TOKEN`. The adapters therefore read the token files
   directly instead of receiving the value from the runner over the secret
   channel.
7. Each adapter sidecar exports `CRITERIA_REMOTE_HOST=127.0.0.1:7778` and
   `CRITERIA_REMOTE_DIGEST=sha256:<pin>` and execs its binary. The adapter
   connects over TCP and calls `OpenSession`, authenticating with the bearer
   token configured in the remote environment's `accept_token` attribute.

No container sees all three credentials, and none of them are exposed as pod
environment variables.

---

## Setup

### Prerequisites

* A Kubernetes cluster with amd64 worker nodes. The manifests use
  `nodeSelector: kubernetes.io/arch: amd64` and tolerate the `catch` and
  `node-role.kubernetes.io/control-plane` taints.
* Container images built and available to the cluster. The default image is
  `localhost:5000/linear-intake-remote:dev`. Build it locally (see the root
  `README.md` and `Makefile`) and push to a registry the cluster can reach.
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

### 4. Create the PVCs (if they do not already exist)

```sh
kubectl apply -f k8s/03-pvc.yaml
```

### 5. Apply the SecretProviderClasses

```sh
kubectl apply -f k8s/job-cri-27.yaml
```

This creates `linear-spc`, `copilot-spc`, and `shell-spc` in the
`criteria-jobs` namespace. It also creates the `pod-adapter-scripts`
ConfigMap that embeds the runner and sidecar shell scripts.

### Optional: deploy the Castle control plane

If you want the `castle` control plane for agent registration and event
buffering, apply its Deployment and Service:

```sh
kubectl apply -f k8s/04-castle.yaml
```

Castle is not required for a standalone intake run.

---

## Running a workflow

### With the launcher (recommended)

The launcher renders the template and applies the Job:

```sh
export TICKET_ID=CRI-105
export REPO_URL=brokenbots/workflow-example
export IMAGE=localhost:5000/linear-intake-remote:dev

# Optional overrides
export NAMESPACE=criteria-jobs
export DATA_PVC=criteria-data
export PROVIDER_BASE_URL=http://host.docker.internal:11434/v1

./k8s/launch-pod-adapter-job.sh
```

To preview the rendered manifest without applying it:

```sh
DRY_RUN=1 ./k8s/launch-pod-adapter-job.sh
```

### Without the launcher

A fully rendered example Job is available at `k8s/examples/ticket-job.yaml`.
Edit the image, ticket, repository, provider URL, and PVC names to match your
cluster, then apply it directly:

```sh
kubectl apply -f k8s/examples/ticket-job.yaml
```

### With the legacy single-container launcher

For environments that have not migrated to the pod-adapter path:

```sh
export TICKET_ID=CRI-105
export REPO_URL=brokenbots/workflow-example
export LINEAR_API_KEY=...
export WORKFLOW_GITHUB_TOKEN=...
export REVIEWER_GITHUB_TOKEN=...

./k8s/launch-ticket-job.sh
```

This path creates a Kubernetes Secret named `linear-intake-credentials` from
the local environment and applies `05-job-template.yaml`.

### Watch the run

```sh
kubectl logs -n criteria-jobs job/pod-adapter-cri-105 -f --all-containers
kubectl wait -n criteria-jobs job/pod-adapter-cri-105 --for=condition=complete --timeout=60m
```

---

## Debugging

### Read container logs

The Job has three main containers. Read each one separately:

```sh
kubectl logs -n criteria-jobs job/pod-adapter-cri-105 -c workflow-runner
kubectl logs -n criteria-jobs job/pod-adapter-cri-105 -c adapter-copilot
kubectl logs -n criteria-jobs job/pod-adapter-cri-105 -c adapter-shell
kubectl logs -n criteria-jobs job/pod-adapter-cri-105 -c repo-clone
```

Typical failures:

* `repo-clone` exits non-zero when the workflow GitHub token cannot read the
  repository, or when `REPO_URL` is missing.
* `workflow-runner` exits if the Linear API key is missing, `ALLOW_DIRTY` is
  invalid, or the workflow validation fails.
* `adapter-copilot` / `adapter-shell` loop or crash when they cannot reach
  `127.0.0.1:7778`, when the bearer token file is missing, or when the pinned
  adapter digest does not match the binary in the image.

### Check the Linear issue state

Look at the ticket in Linear. The workflow moves the issue through the
configured states:

1. `linear_triage_state` when a bug is classified
2. `linear_work_state` before the handler starts
3. `linear_done_state` after the PR merges
4. `linear_review_state` when the run cannot proceed autonomously

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
| `/data/.criteria-remote-token` | Per-run bearer token shared between the runner and adapters. |

### Verify CSI secret mounts

If a container fails with "required via /secrets/...", check that the CSI
volume mounted correctly:

```sh
kubectl exec -n criteria-jobs job/pod-adapter-cri-105 -c workflow-runner -- \
  ls -la /secrets/

kubectl exec -n criteria-jobs job/pod-adapter-cri-105 -c workflow-runner -- \
  cat /secrets/linear_api_key | head -c 8
```

You should see a single file with mode `0600` owned by the `fsGroup` user
(`10001`).

### Regenerate the pod-adapter manifest

`k8s/job-cri-27.yaml` is generated from `job-cri-27.yaml.tmpl` by embedding the
runner and sidecar scripts. After editing either script, regenerate it:

```sh
./k8s/generate-pod-adapter-manifest.sh
```

---

## Security

### Secret isolation

The pod-adapter design isolates credentials by container:

* The `workflow-runner` only needs the Linear API key. It never mounts either
  GitHub token.
* The `adapter-copilot` mounts both GitHub tokens because it performs GitHub
  work and PR review under separate identities.
* The `adapter-shell` and `repo-clone` init container mount only the workflow
  GitHub token.

This limits the blast radius of a container compromise: a vulnerability in the
runner cannot leak the reviewer's GitHub token because that file is never
present in the runner's filesystem namespace.

### Secrets Store CSI driver

Credentials are stored in OpenBao and injected by the CSI driver as files
inside the container. They are never written to the pod spec, never exposed as
environment variables, and never persisted in the container image. The driver
mounts each volume read-only with `0600` permissions. The OpenBao role is
bound to the `criteria-runner` service account and namespace and has a short
TTL (one hour by default).

### Remote bridge authentication

Adapters authenticate to the in-pod Criteria shim with two mechanisms:

1. **Bearer token (`accept_token`)**: the runner generates a 32-byte
   alphanumeric token per run and writes it to the shared PVC. The runner and
   sidecars agree on the token; nothing outside the pod can read it.
2. **Pinned digest**: each sidecar resolves the adapter's SHA256 digest from the
   workflow lockfile and exports it as `CRITERIA_REMOTE_DIGEST`. The shim rejects
   connections whose presented identity does not match the lockfile pin.

The default deployment uses the pod loopback interface (`127.0.0.1:7778`), so
the remote bridge does not cross a network boundary and mTLS is not required.

### mTLS between pods (if cross-pod)

If you split the runner and adapters across separate pods, the Criteria remote
bridge leaves the loopback interface. In that configuration you must:

* Issue a client certificate for each adapter and a server certificate for the
  shim.
* Mount the certificates through the CSI driver or cert-manager, not as plain
  Secret env vars.
* Configure the adapter environment to use TLS (`https://` or the equivalent
  Criteria transport option) and set `accept_token` to a cryptographically
  random value generated per run and stored only in memory or a CSI mount.
* Restrict network policy so only adapter pods can reach the runner pod on the
  remote port.

The manifests in this directory use the single-pod loopback model and do not
configure cross-pod mTLS.

### Pod security

* `runAsNonRoot: true`, `runAsUser: 10001`, `fsGroup: 10001`.
* No `privileged: true`, no `seccompProfile`, no `--security-opt`.
* The namespace enforces the `baseline` Pod Security Standard.
* Only the `csi` driver DaemonSets require host-level access; the workflow Job
  does not.

### accept_token authentication

The `accept_token` value in each `environment "remote"` block is the shared
secret that allows an adapter to join the shim. It is generated at runtime,
not committed to the repository. The runner replaces the committed placeholder
(`CRITERIA_REMOTE_TOKEN_PLACEHOLDER`) in a temporary copy of the workflow tree
before `criteria apply` starts, so the token never appears in the image or on
the persistent workflow source volume.

---

## Files in this directory

| File | Purpose |
|------|---------|
| `00-namespace.yaml` | `criteria-jobs` namespace with baseline pod security. |
| `01-openbao-config.yaml` | Reference ConfigMap for the OpenBao Kubernetes auth role and policy. |
| `02-serviceaccount.yaml` | `criteria-runner` ServiceAccount and RBAC. |
| `03-pvc.yaml` | `criteria-data` and `criteria-repo` PVCs. |
| `04-castle.yaml` | Optional Castle control-plane Deployment and Service. |
| `05-job-template.yaml` | Legacy single-container Job template. |
| `job-cri-27.yaml.tmpl` | Pod-adapter Job template with embedded script placeholders. |
| `job-cri-27.yaml` | Generated pod-adapter manifest (created by `generate-pod-adapter-manifest.sh`). |
| `pod-adapter-runner.sh` | Script for the `workflow-runner` container. |
| `pod-adapter-sidecar.sh` | Script for the `adapter-copilot` and `adapter-shell` containers. |
| `generate-pod-adapter-manifest.sh` | Embeds the scripts into `job-cri-27.yaml`. |
| `launch-pod-adapter-job.sh` | Renders and applies `job-cri-27.yaml`. |
| `launch-ticket-job.sh` | Renders and applies the legacy `05-job-template.yaml`. |
| `install-secrets-store-csi.sh` | Helm installer for the CSI driver and OpenBao provider. |
| `verify-secrets-store-csi.sh` | Waits for the CSI driver/provider DaemonSets and SPCs. |
| `create-secret-from-openbao.sh` | Pulls credentials from OpenBao into a Kubernetes Secret for the legacy path. |
| `criteria-jobs-policy.hcl` | OpenBao policy granting read access to `criteria/data/*`. |
| `registries.yaml` | k3s containerd mirror for insecure local registries. |
| `values-secrets-store-csi-driver.yaml` | Helm values for the upstream CSI driver chart. |
| `values-openbao-csi-provider.yaml` | Helm values for the OpenBao CSI provider chart. |
| `examples/ticket-job.yaml` | Apply-ready example Job for the pod-adapter path. |
| `tests/` | Regression tests for the manifests and launchers. |
