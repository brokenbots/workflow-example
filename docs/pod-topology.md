# Pod topology & the network-only data contract

Scope: the k8s runner topology (criteria-k8s operator + per-scope adapter
pods) and the data-isolation contract it enforces. Normative for every
workflow executed on the cluster; the plan-of-record is CRI-214 (pod
topology section), CRI-237 (network-only data), CRI-236 (token handoff).

## Topology per run

    CriteriaRun (CR, watcher-stamped)
      └─ runner Job
           ├─ repo-clone (init container)   — per-ticket clone on the data PVC
           └─ workflow-runner                — criteria binary, fetches/applies
                                                the workflow source
                ├─ (per scope, per adapter) adp-<digest>-<scope> pods
                └─ remote shim on the runner (listen 0.0.0.0:7778)

- **Runner pod**: one per CriteriaRun. Mounts the data PVC (`/data`), the
  per-ticket repo clone, declared workflow volumes/secrets. Runs the engine
  in server mode (`CASTLE_ADDR`) streaming all lifecycle events to castle.
- **Adapter pods**: operator-spawned per **(subworkflow scope, adapter
  environment)** from `provision_wanted` events. A multi-scope run holds
  ~10 concurrent adapter pods by design — one per distinct (scope, adapter)
  pair; the same adapter name re-provisioned *inside one scope* is the leak
  class (fixed, CRI-253/302 era). Released at scope exit (`released` event →
  pod teardown). Adapter pods mount **no shared volumes by default** and
  receive no token files: they dial the runner over the network.

## Co-location rules (CRI-234/235)

- Adapters bound to the same **environment** within one scope share one pod
  (separate containers): one accept_token, one identity per environment
  instance.
- Pods holding mounts of the same **host-affinity PV** are scheduled onto the
  same host (NFS exempt — network-attached, no locality constraint).
- The engine **never shares volumes with adapters**: all engine↔adapter data
  flows over the network (shim channel), never a mounted PVC.

## Network-only data contract (CRI-237)

1. **Token handoff is on the wire.** The engine mints a per-scope
   `accept_token`, carried on the `provision_wanted` event wire; the operator
   injects it into the adapter pod env (`CRITERIA_REMOTE_TOKEN`), along with
   the routable `CRITERIA_REMOTE_HOST` (runner pod IP + shim port). Adapter
   pods hold no rotated-token files and read no discovery files — the shared
   PVC token-file surface exists only for pre-wire engines (legacy path).
2. **Adapter pod mounts.** A wire-shaped adapter pod mounts: the workflow
   object's declared volumes (re-sourcing `/data` when the workflow itself
   declares it — the declaration, not the delivery path, is what mounts
   data), declared CSI secrets (name references only; values ride
   OpenSession at identity handshake), and the pod-adapter scripts
   ConfigMap. It does not mount the run-state data volume implicitly.
3. **Working directory.** The engine injects the resolved environment
   working directory per session; per-scope shell adapters accept paths
   under `$HOME` or `CRITERIA_SHELL_ALLOWED_PATHS` — the k8s routes ship
   `CRITERIA_SHELL_ALLOWED_PATHS=/data` on every `/data` volume env map so
   the operator-injected `/data/...` working directories pass confinement.
4. **Secrets.** Declared `secrets` entries are SecretProviderClass
   name-references rendered to files by the CSI driver; the values reach the
   adapter over the OpenSession contract only — they are never in env vars,
   argv, or on the PVC.

## Identity & digest verification

Adapter pods present `CRITERIA_ADAPTER_NAME` (the adapter **type** — the
lockfile verifier keys by type, not node name) and the event's OCI
`digest`; the runner's lockfile check refuses a mismatching digest before
any session opens. The scope handshake binds `<scope>/<scope_instance_id>`;
an empty scope in per-scope mode is rejected by the shim.

## Run state (CRI-303)

Source-mode engine state lives under `/data/criteria-home/<runner-job>/`
(per-run, on the data PVC) so a runner-container restart resumes from
CRI-125 checkpoints instead of replaying fresh. Image-mode keeps its own
`/data/criteria` root; the two roots never share state.

## Teardown

Operator-mediated: per-scope adapter pods are deleted when the engine emits
`released` for the (scope, environment) pair; finished-run children are
swept by the operator's retention loop; the runner Job is owned by the CR
(owner-ref cascade). The operator never sits inside the engine↔adapter
communication path — it actuates k8s objects only (castle is the control
plane; see CRI-133/134/237).