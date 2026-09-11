# criteria-k8s Helm chart

Single declarative install/upgrade unit for the Criteria workflow stack in the
`criteria-jobs` namespace, replacing the hand-applied manifests in `k8s/` and
the live-mutated deployment env. The chart packages:

* `criteria-k8s-operator` Deployment (retention sweep env, `--namespace` flag)
* `criteria-linear-watcher` Deployment (Linear polling config)
* Namespaced `Role`/`RoleBinding` for the operator plus both service accounts
  (`criteria-k8s-operator`, `criteria-runner`) and the runner RBAC
* `CriteriaRun` CRD (`crds/`, installed by helm on install; see the [upgrade
  caveat](#upgrades) below)
* `criteria-data`/`criteria-repo` PVCs
* Castle Deployment + Service
* `linear-spc`/`copilot-spc` SecretProviderClasses for the OpenBao CSI provider
* `pod-adapter-scripts` ConfigMap (runner/adapter wrapper scripts)

## Install

```sh
# From the repo root (external prerequisites below must already exist)
helm install criteria-k8s charts/criteria-k8s \
  -n criteria-jobs --create-namespace
```

Helm creates the namespace via `--create-namespace` but it is a bare
namespace (no pod-security labels). To get the `pod-security.kubernetes.io`
labels from the chart instead of `--create-namespace`, set
`namespaceCreate=true` (or label the namespace manually).

Upgrade:

```sh
helm upgrade criteria-k8s charts/criteria-k8s -n criteria-jobs
```

To preview the rendered output:

```sh
helm template criteria-k8s charts/criteria-k8s
```

## Values

| Key | Default | Description |
|-----|---------|-------------|
| `namespace` | `criteria-jobs` | Target namespace for every namespaced resource. Empty string falls back to the release namespace (`-n`). |
| `namespaceCreate` | `false` | Render a `Namespace` object (with pod-security labels) from the chart. Leave `false` and use helm's `--create-namespace`. |
| `podSecurity.enforce` | `baseline` | Pod Security Standard label applied to the rendered namespace. |
| `images.operator.repository` | `localhost:5000/criteria-k8s` | Operator/watcher image repository. |
| `images.operator.tag` | `dev` | Operator/watcher image tag. |
| `images.operator.pullPolicy` | `IfNotPresent` | Operator/watcher image pull policy. |
| `images.workflow.repository` | `localhost:5000/linear-intake-remote` | Criteria workflow image repository (runner Jobs). |
| `images.workflow.tag` | `dev` | Criteria workflow image tag. |
| `operator.enabled` | `true` | Deploy the operator. |
| `operator.replicaCount` | `1` | Operator replicas. |
| `operator.retentionPeriod` | `168h` | Keep per-ticket intake/triage artifacts this long after the last write (`0s` disables sweeping). Go duration. |
| `operator.retentionInterval` | `1h` | How often the retention sweep runs. Go duration. |
| `operator.providerBaseUrl` | `http://192.168.17.116:11434/v1` | Ollama-compatible provider endpoint handed to adapter workflows. |
| `operator.resources` | `128Mi/100m` requests, `512Mi/500m` limits | Operator pod resources. |
| `watcher.enabled` | `true` | Deploy the Linear watcher. |
| `watcher.replicaCount` | `1` | Watcher replicas. |
| `watcher.linearProjectName` | `Criteria K8s Workflow Runner` | Linear project the watcher polls. |
| `watcher.linearTriageState` | `Triage` | Linear state that triggers a run. |
| `watcher.pollInterval` | `60s` | How often the watcher polls Linear. Go duration. |
| `watcher.maxAgentVisits` | `2` | Default max agent visits recorded in created `CriteriaRun`s. |
| `watcher.providerBaseUrl` | `http://192.168.17.116:11434/v1` | Provider endpoint recorded in created `CriteriaRun`s. |
| `watcher.resources` | `64Mi/50m` requests, `256Mi/200m` limits | Watcher pod resources. |
| `castle.enabled` | `true` | Deploy the Castle control plane. |
| `castle.image.repository` | `localhost:5000/castle` | Castle image repository. |
| `castle.image.tag` | `dev` | Castle image tag. |
| `castle.image.pullPolicy` | `Always` | Castle image pull policy. |
| `castle.port` | `8080` | h2c port for the Castle Deployment and Service. |
| `castle.tolerations` | control-plane taint | Castle tolerations. |
| `pvc.data.name` | `criteria-data` | Intake/triage artifacts PVC. Passed to the operator (`CRITERIA_DATA_PVC`) and mounted by runner Jobs. |
| `pvc.data.size` | `10Gi` | Data PVC size. |
| `pvc.data.storageClass` | `local-path` | Data PVC storage class (k3s local-path provisioner). |
| `pvc.repo.name` | `criteria-repo` | Shared clone workspace PVC for adapter Jobs. |
| `pvc.repo.size` | `10Gi` | Repo PVC size. |
| `pvc.repo.storageClass` | `local-path` | Repo PVC storage class. |
| `openbao.provider` | `openbao` | CSI provider name for the SecretProviderClasses. |
| `openbao.roleName` | `criteria-jobs` | OpenBao kubernetes auth role the CSI provider authenticates as. |
| `openbao.secretPath` | `criteria/data/linear` | OpenBao KV path the SPC objects read. |
| `podSecurityContext` | non-root uid/gid 10001, fsGroup 10001, RuntimeDefault | Shared pod security context for operator/watcher/castle. |
| `nodeSelector` | `kubernetes.io/arch: amd64` | Node selector for operator/watcher pods. |
| `tolerations` | `catch` taint | Tolerations for operator/watcher pods. |

## External prerequisites

The chart intentionally does **not** template secrets or the OpenBao control
plane. Before installing:

1. **Secrets Store CSI driver and OpenBao provider** — install with
   `./k8s/install-secrets-store-csi.sh` and verify with
   `./k8s/verify-secrets-store-csi.sh`.
2. **OpenBao kubernetes auth role and policy** — configure OpenBao directly
   (see `k8s/01-openbao-config.yaml` for the reference configuration):

   ```sh
   kubectl exec -n default openbao-0 -- \
     vault policy write criteria-jobs - < k8s/criteria-jobs-policy.hcl
   kubectl exec -n default openbao-0 -- \
     vault write auth/kubernetes/role/criteria-jobs \
       bound_service_account_names=criteria-runner \
       bound_service_account_namespaces=criteria-jobs \
       policies=criteria-jobs ttl=1h
   kubectl exec -n default openbao-0 -- \
     env VAULT_TOKEN=$VAULT_TOKEN vault kv put criteria/linear \
       linear_api_key="$LINEAR_API_KEY" \
       workflow_github_token="$WORKFLOW_GITHUB_TOKEN" \
       reviewer_github_token="$REVIEWER_GITHUB_TOKEN"
   ```

   No Kubernetes Secret is templated: credentials only ever reach pods through
   the CSI mounts.
3. **Container images** in the registry the cluster can reach:
   `make images-push`.
4. **Namespace with pod-security labels** — `--create-namespace` creates a
   bare namespace; either set `namespaceCreate=true` or apply
   `pod-security.kubernetes.io/enforce: baseline` manually when you use it.

## Upgrades

The `CriteriaRun` CRD lives in `crds/`. Helm installs CRDs from that directory
on install but **does not upgrade them**. If the CRD schema changes, apply the
new CRD manually:

```sh
kubectl apply -f charts/criteria-k8s/crds/criteria.brokenbots.dev_criteriaruns.yaml
```

## Superseded legacy files

The chart supersedes the manual launch paths. `k8s/05-job-template.yaml`,
`k8s/job-cri-27.yaml*` and `k8s/launch-*.sh` are legacy/manual launch paths
that should be deleted (the operator reconciles `CriteriaRun`s into Jobs).

## Keeping the scripts ConfigMap in sync

`templates/configmap-scripts.yaml` embeds `scripts/runner.sh` and
`scripts/adapter.sh`. After editing `k8s/pod-adapter-runner.sh` or
`k8s/pod-adapter-adapter.sh`, regenerate with:

```sh
./k8s/generate-pod-adapter-manifest.sh
```

which re-renders the generated manifests and copies the scripts into the
chart. `k8s/tests/test_criteria_k8s_chart.sh` fails when the copies drift.