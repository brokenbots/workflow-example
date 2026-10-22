#!/usr/bin/env bash
# Per-project namespace lifecycle tool (KB-312, ADR 307): one operator per
# project namespace, shared castle+registry (criteria-jobs stays the platform
# namespace — it is only shrunk, never renamed).
#
# The template renders the COMPLETE per-project target state:
#   ns (project labels) -> SAs (the project operator identity plus
#   criteria-runner, the OpenBao-bound job identity the binding's CSI mounts
#   authenticate through); RBAC (namespaced Role + RoleBindings — the
#   project operator's SA plus a cross-namespace RoleBinding for the platform
#   kanboard-watcher SA in criteria-jobs, the intake write path for
#   post-cutover runs); SPC wiring per project (OpenBao roleName <ns> +
#   the project paths scoped in the OpenBao policy; per-repo scoping lives
#   INSIDE the OpenBao paths; no new credentials and no secret schema
#   changes); operator Deployment (namespaced cache, source-mode base image,
#   the shared adapter registry, castle dialed cross-namespace by service
#   FQDN, resources per the card: 64Mi/256Mi memory, 50m/500m CPU; retention
#   rendered inert — flip it on with 'retention-on' only after the archive
#   round-trip is proven); project PVCs (criteria-data + criteria-repo,
#   local-path so the host-affinity same-host rule still applies,
#   docs/pod-topology.md); the project-owned binding ConfigMap spliced
#   verbatim from k8s/examples/binding-<ns>.yaml (the KB-311 fail-closed
#   binding schema of record).
#
# Usage:
#   project-lifecycle.sh render [--namespace NS] [--binding-file FILE] [--output FILE]
#   project-lifecycle.sh binding-checksum [--namespace NS]
#   project-lifecycle.sh apply [--namespace NS] [--file FILE]
#   project-lifecycle.sh openbao-wire [--namespace NS]
#   project-lifecycle.sh verify [--namespace NS]
#   project-lifecycle.sh triage [--namespace NS]
#   project-lifecycle.sh retention-on/off [--namespace NS]
#   project-lifecycle.sh retention-plan on|off [--namespace NS]
#       (offline helper: prints the exact kubectl env mutation the toggle runs)
#
# Cluster-bound commands (apply/verify/triage/retention-on|off) need kubectl;
# render does not. The go-domain namespace criteria-go-dev is the default (the
# KB-312 acceptance leg); other domains' cutovers are separate cards.
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
platform_ns="criteria-jobs" # platform namespace: kept, only shrunk (KB-312)

ns="criteria-go-dev"
binding_file=""
output_file=""
run_id=""

err() { echo "project-lifecycle: ${*}" >&2; exit 1; }
need_kubectl() { command -v kubectl >/dev/null 2>&1 || err "kubectl is required for this command (not on PATH)"; }

parse_args() {
  while [ $# -gt 0 ]; do
    case "$1" in
      --namespace) [ $# -ge 2 ] || err "--namespace needs a value"; ns="$2"; shift 2 ;;
      --binding-file) [ $# -ge 2 ] || err "--binding-file needs a value"; binding_file="$2"; shift 2 ;;
      --output) [ $# -ge 2 ] || err "--output needs a value"; output_file="$2"; shift 2 ;;
      --file) [ $# -ge 2 ] || err "--file needs a value"; output_file="$2"; shift 2 ;;
      --run) [ $# -ge 2 ] || err "--run needs a value"; run_id="$2"; shift 2 ;;
      *) err "unknown argument: $1" ;;
    esac
  done
}

default_binding_file() { echo "${repo_root}/k8s/examples/binding-${ns}.yaml"; }
default_render_path() { echo "${repo_root}/k8s/projects/${ns}.yaml"; }

resolve_binding_file() {
  binding_file="$(default_binding_file)"
  [ -f "$binding_file" ] || err "no binding example for namespace ${ns}: missing ${binding_file} (cutovers are binding-first, KB-311)"
}

# Extract a JSON payload from the YAML 'data: <key>: |' block of a shipped
# manifest (block scalars hold pure JSON; de-indent textually). Keeps
# render/verify free of python/yaml tooling dependencies.
extract_data_json() {
  local file="$1" key="$2"
  awk -v key="$key" '
    $0 ~ "^[ ]*" key ": \\|$" { inblock = 1; next }
    inblock && (/^[ \t]/ || $0 == "") { sub(/^[ \t]*/, "", $0); print; next }
    inblock { exit }
  ' "$file"
}

# Mirrors Go's json.Marshal of the binding.Payload STRUCT — the operator's
# checksum input (internal/binding: payload.Checksum(), the digest stamped into
# status.launchManifest.bindingChecksum by the project-binding admission path).
# Three properties jq -Sc does NOT have: struct-field ORDER (the struct emits
# fields in declaration order, not sorted; nested Volume/Secret reuse the
# routes structs' order), omitempty semantics on the optional
# Volume/Secret fields, and Go's HTML string escaping (& < > become \u0026
# \u003c \u003e — the shipped go binding carries "&&" in config.ciGateCmd, so
# this divergence is real, not theoretical). Map keys sort (encoding/json
# sorts all maps; jq's codepoint sort matches Go's UTF-8 byte sort).
# Known mirror limit: a string carrying a literal escape-sequence TEXT
# (e.g. the six characters \u003c) escapes the sed pass — the Go gate
# TestShippedGoBindingChecksumMatchesRenderedCM stays the algorithm's
# authority; no shipped binding carries such content.
canonical_json_binding() {
  jq -j -c '
    def srt: to_entries | sort_by(.key) | from_entries;
    def vol:
      . as $v
      | (reduce (["subPath", $v.subPath], ["readOnly", $v.readOnly], ["claim", $v.claim],
                 ["server", $v.server], ["path", $v.path], ["sizeLimit", $v.sizeLimit],
                 ["secretName", $v.secretName]) as $kv ({name: $v.name, kind: $v.kind, mountPath: $v.mountPath};
           if ($kv[1] == null or $kv[1] == false or $kv[1] == "") then . else . + {($kv[0]): $kv[1]} end))
      | . + (if (($v.env // {}) == {}) then {} else {env: ($v.env | srt)} end);
    def sec:
      . as $s
      | {name: $s.name, secretProviderClass: $s.secretProviderClass, mountPath: $s.mountPath}
      | . + (if (($s.env // {}) == {}) then {} else {env: ($s.env | srt)} end);
    {
      apiVersion: .apiVersion,
      kind: .kind,
      images: (if .images == null then null else (.images | srt) end),
      env: (if .env == null then null else (.env | srt) end),
      volumes: (if .volumes == null then null else (.volumes | map(vol)) end),
      secrets: (if .secrets == null then null else (.secrets | map(sec)) end),
      config: (if .config == null then null else (.config | srt) end)
    }
  ' | sed 's/</\\u003c/g; s/>/\\u003e/g; s/&/\\u0026/g'
}
canonical_sha256() { canonical_json_binding | sha256sum | awk '{print $1}'; }

# The routes example is the routing table of record; a project must be
# carried by its projectRegistry. Echoes the entry as {key,value} JSON.
registry_entry() {
  local routes_json entry
  routes_json="$(extract_data_json "${repo_root}/k8s/examples/routes-configmap.yaml" "routes.json")"
  [ -n "$routes_json" ] || err "could not extract routes.json from k8s/examples/routes-configmap.yaml"
  entry="$(printf '%s' "$routes_json" | jq -ec --arg ns "$ns" \
    '.projectRegistry | to_entries[] | select(.value.namespace == $ns)')" \
    || err "projectRegistry does not carry a project bound to namespace ${ns}; a cutover renders its project binding first, then flips the marker (binding-first, KB-311)"
  printf '%s' "$entry"
}

binding_checksum_for() {
  local payload
  payload="$(extract_data_json "$1" "binding.json")"
  [ -n "$payload" ] || err "no binding.json payload in $1"
  printf '%s' "$payload" | jq -e . >/dev/null 2>&1 || err "binding payload in $1 is not valid JSON"
  printf '%s' "$payload" | canonical_sha256
}

emit_doc() { # emit between-doc separator lazily: first doc has none
  if [ -s "${doc_file}" ]; then
    printf '\n---\n' >> "$doc_file"
  fi
  cat >> "$doc_file"
}

render_common() {
  resolve_binding_file
  [ "$ns" != "$platform_ns" ] || err "refusing to render the platform namespace ${platform_ns} as a project namespace (it is kept and only shrunk, KB-312)"
  command -v jq >/dev/null 2>&1 || err "jq is required (not on PATH)"

  registry_json="$(registry_entry)"
  project_name="$(printf '%s' "$registry_json" | jq -r '.key')"
  cutover_state="$(printf '%s' "$registry_json" | jq -r '.value.cutover // ""')"
  checksum="$(binding_checksum_for "$binding_file")"
}

render() {
  render_common
  doc_file="$(mktemp)"
  trap 'rm -f "$doc_file"' EXIT

  emit_doc <<EOF
# == KB-312 per-project namespace cutover (ADR 307) — rendered target state for
# project ${project_name} (ns ${ns}, registry cutover: ${cutover_state:-unset}) ==
#
# Rendered by k8s/project-lifecycle.sh; kubectl is not consulted at render
# time, so re-render + diff is the drift check:
#   ./k8s/project-lifecycle.sh render --namespace ${ns}
# The binding ConfigMap below is spliced verbatim from
# k8s/examples/binding-${ns}.yaml — the KB-311 fail-closed binding schema
# (k8s/criteria-binding.schema.json) is its contract.
# Binding checksum (canonical JSON, operator algorithm): ${checksum}
apiVersion: v1
kind: Namespace
metadata:
  name: ${ns}
  labels:
    criteria.brokenbots.dev/project: ${ns}
    project: ${ns}
EOF

  emit_doc <<EOF
apiVersion: v1
kind: ServiceAccount
metadata:
  name: criteria-k8s-operator
  namespace: ${ns}
  labels:
    criteria.brokenbots.dev/project: ${ns}
EOF

  emit_doc <<EOF
# The OpenBao-bound job identity: the operator's runner Jobs carry it and the
# OpenBao kubernetes auth role for this ns is bounded to it ('openbao-wire').
apiVersion: v1
kind: ServiceAccount
metadata:
  name: criteria-runner
  namespace: ${ns}
  labels:
    criteria.brokenbots.dev/project: ${ns}
EOF

  emit_doc <<EOF
# Namespaced ONLY (ADR 307): no ClusterRole/ClusterRoleBinding exists here.
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: criteria-k8s-operator
  namespace: ${ns}
  labels:
    criteria.brokenbots.dev/project: ${ns}
rules:
- apiGroups: ["criteria.brokenbots.dev"]
  resources: ["criteriaruns"]
  verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
- apiGroups: ["criteria.brokenbots.dev"]
  resources: ["criteriaruns/status"]
  verbs: ["get", "update", "patch"]
- apiGroups: ["criteria.brokenbots.dev"]
  resources: ["criteriaruns/finalizers"]
  verbs: ["update"]
- apiGroups: ["batch"]
  resources: ["jobs"]
  verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
- apiGroups: ["apps"]
  resources: ["deployments"]
  verbs: ["get"]
- apiGroups: [""]
  resources: ["pods", "pods/exec", "pods/log", "events"]
  verbs: ["get", "list", "watch", "create", "update", "delete"]
- apiGroups: [""]
  resources: ["configmaps"]
  verbs: ["get", "list", "watch"]
EOF

  emit_doc <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: criteria-k8s-operator
  namespace: ${ns}
  labels:
    criteria.brokenbots.dev/project: ${ns}
subjects:
- kind: ServiceAccount
  name: criteria-k8s-operator
  namespace: ${ns}
roleRef:
  kind: Role
  name: criteria-k8s-operator
  apiGroup: rbac.authorization.k8s.io
EOF

  emit_doc <<EOF
# The platform intake write path: the kanboard-watcher runs with the platform
# operator identity in ${platform_ns} and stamps post-cutover CriteriaRuns
# (binding references only, KB-311) into THIS namespace, so the platform SA
# gets a RoleBinding here. Namespaced grant; nothing cluster-scoped.
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: criteria-kanboard-watcher-intake
  namespace: ${ns}
  labels:
    criteria.brokenbots.dev/project: ${ns}
subjects:
- kind: ServiceAccount
  name: criteria-k8s-operator
  namespace: ${platform_ns}
roleRef:
  kind: Role
  name: criteria-k8s-operator
  apiGroup: rbac.authorization.k8s.io
EOF

  emit_doc <<EOF
# SPC wiring per project: OpenBao roleName bound to this ns (wire it live
# with 'openbao-wire'). The objects reuse the EXISTING platform secret paths —
# no new credentials, no secret schema change. Per-repo scoping lives INSIDE
# the OpenBao paths (criteria/data/${ns}/...); future project-owned keys land
# there, today's keys stay criteria/data/linear.
apiVersion: secrets-store.csi.x-k8s.io/v1
kind: SecretProviderClass
metadata:
  name: kanboard-spc
  namespace: ${ns}
  labels:
    criteria.brokenbots.dev/project: ${ns}
spec:
  provider: openbao
  parameters:
    roleName: ${ns}
    objects: |
      - objectName: kanboard_app_token
        secretPath: criteria/data/linear
        secretKey: kanboard_app_token
        filePermission: 0444
      - objectName: kanboard_url
        secretPath: criteria/data/linear
        secretKey: kanboard_url
        filePermission: 0444
EOF

  emit_doc <<EOF
apiVersion: secrets-store.csi.x-k8s.io/v1
kind: SecretProviderClass
metadata:
  name: copilot-spc
  namespace: ${ns}
  labels:
    criteria.brokenbots.dev/project: ${ns}
spec:
  provider: openbao
  parameters:
    roleName: ${ns}
    objects: |
      - objectName: workflow_github_token
        secretPath: criteria/data/linear
        secretKey: workflow_github_token
        filePermission: 0444
      - objectName: reviewer_github_token
        secretPath: criteria/data/linear
        secretKey: reviewer_github_token
        filePermission: 0444
EOF

  emit_doc <<EOF
# KB-314 shape: the castle token for the run-archive lane (#308), served from
# the same OpenBao role — the per-ns troubleshooting contract's archive leg.
apiVersion: secrets-store.csi.x-k8s.io/v1
kind: SecretProviderClass
metadata:
  name: castle-token-spc
  namespace: ${ns}
  labels:
    criteria.brokenbots.dev/project: ${ns}
spec:
  provider: openbao
  parameters:
    roleName: ${ns}
    objects: |
      - objectName: castle_criteria_token
        secretPath: criteria/data/linear
        secretKey: castle_criteria_token
        filePermission: 0444
EOF

  emit_doc <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: criteria-data
  namespace: ${ns}
  labels:
    criteria.brokenbots.dev/project: ${ns}
spec:
  accessModes:
  - ReadWriteOnce
  resources:
    requests:
      storage: 10Gi
  storageClassName: local-path
EOF

  emit_doc <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: criteria-repo
  namespace: ${ns}
  labels:
    criteria.brokenbots.dev/project: ${ns}
spec:
  accessModes:
  - ReadWriteOnce
  resources:
    requests:
      storage: 10Gi
  storageClassName: local-path
EOF

  emit_doc <<EOF
# OpenBao role/policy reference for this project ns; the LIVE wiring is a
# kubectl exec against the shared OpenBao pod ('openbao-wire' prints it).
apiVersion: v1
kind: ConfigMap
metadata:
  name: openbao-role-reference
  namespace: ${ns}
  labels:
    criteria.brokenbots.dev/project: ${ns}
data:
  role-config.txt: |
    auth/kubernetes/role/${ns}:
      bound_service_account_names: criteria-runner
      bound_service_account_namespaces: ${ns}
      policies: ${ns}
      ttl: 1h

  policy.hcl: |
    # Project-scoped paths: future per-project/per-repo secret reads land here.
    path "criteria/data/${ns}/*" {
      capabilities = ["read", "list"]
    }
    path "criteria/metadata/${ns}/*" {
      capabilities = ["read", "list"]
    }
    # The shared legacy platform path holds today's credentials (KB-312: no
    # new credentials, no secret schema change at cutover).
    path "criteria/data/linear" {
      capabilities = ["read", "list"]
    }
    path "criteria/metadata/linear" {
      capabilities = ["read", "list"]
    }
EOF

  emit_doc <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: criteria-k8s-operator
  namespace: ${ns}
  labels:
    criteria.brokenbots.dev/project: ${ns}
    app.kubernetes.io/name: criteria-k8s-operator
    app.kubernetes.io/component: operator
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: criteria-k8s-operator
  template:
    metadata:
      labels:
        criteria.brokenbots.dev/project: ${ns}
        app.kubernetes.io/name: criteria-k8s-operator
        app.kubernetes.io/component: operator
    spec:
      serviceAccountName: criteria-k8s-operator
      nodeSelector:
        kubernetes.io/arch: amd64
      tolerations:
      - key: catch
        operator: Exists
        effect: NoSchedule
      securityContext:
        runAsNonRoot: true
        runAsUser: 10001
        runAsGroup: 10001
        fsGroup: 10001
        seccompProfile:
          type: RuntimeDefault
      containers:
      - name: operator
        image: localhost:5000/criteria-k8s:dev
        imagePullPolicy: IfNotPresent
        command: ["/manager"]
        env:
        # Per-project operator (ADR 307): the manager's caches and queue
        # recovery scope to this namespace only.
        - name: CRITERIA_NAMESPACE
          value: ${ns}
        # Source-mode base image (CRI-231): url-only runs execute here.
        - name: CRITERIA_BASE_IMAGE
          value: localhost:5000/criteria-base:dev
        # Adapter image fallback (CRI-214 M14): the SHARED registry — the
        # registry is platform-owned per ADR 307; the dev-class run executes
        # the SAME workflow tree via the project binding's own images.
        - name: ADAPTER_REGISTRY
          value: localhost:5000
        - name: ADAPTER_TAG
          value: k8s-3
        - name: CRITERIA_DATA_PVC
          value: criteria-data
        # Artifact retention sweep: off — per-ticket artifacts stay until the
        # KB-309 toggle below is flipped (deliberately inert at render).
        - name: RETENTION_PERIOD
          value: 0s
        # KB-309 retention: inert by default (no namespace allowlist on the
        # sweep). Flip on AFTER the castle archive round-trip is proven:
        #   ./k8s/project-lifecycle.sh retention-on --namespace ${ns}
        - name: CR_RETENTION_DAYS
          value: 0
        - name: CR_RETENTION_KEEP_LAST
          value: 0
        # Castle is shared and stays in ${platform_ns}: cross-namespace dial.
        - name: CASTLE_ADDR
          value: http://castle.${platform_ns}.svc.cluster.local:8080
        - name: CASTLE_TOKEN_FILE
          value: /var/run/secrets/criteria/castle/castle_criteria_token
        - name: CRITERIA_JOB_ARCH
          value: amd64
        - name: CRITERIA_STALL_WINDOW
          value: 30m
        # KB-312 resource shape: observed 43-84Mi per operator; NO quota
        # change on catch at 6-9 namespaces.
        resources:
          requests:
            memory: 64Mi
            cpu: 50m
          limits:
            memory: 256Mi
            cpu: 500m
        volumeMounts:
        - name: criteria-data
          mountPath: /data
        - name: castle-token
          mountPath: /var/run/secrets/criteria/castle
          readOnly: true
      volumes:
      - name: criteria-data
        persistentVolumeClaim:
          claimName: criteria-data
      - name: castle-token
        csi:
          driver: secrets-store.csi.k8s.io
          readOnly: true
          volumeAttributes:
            secretProviderClass: castle-token-spc
EOF

  # The project-owned binding CM: spliced verbatim from the binding example —
  # from its first apiVersion line (the example's header comments stay in the
  # example) — stamped with the project label like every other doc here.
  printf '\n---\n' >> "$doc_file"
  sed -n '/^apiVersion:/,$p' "$binding_file" >> "$doc_file.tmpsplice" 2>/dev/null || true
  if [ ! -s "$doc_file.tmpsplice" ]; then
    err "binding example ${binding_file} carries no apiVersion line to splice"
  fi
  awk -v ns="$ns" '
    /^  namespace: [^ ]/ && !labeled && $0 == "  namespace: " ns {
      print; print "  labels:"; print "    criteria.brokenbots.dev/project: " ns; labeled = 1; next
    }
    { print }
    END { exit labeled ? 0 : 1 }
  ' "$doc_file.tmpsplice" >> "$doc_file" \
    || err "binding example ${binding_file} is not a ${ns}-namespaced ConfigMap doc"
  rm -f "$doc_file.tmpsplice"

  if [ -n "$output_file" ]; then
    mkdir -p "$(dirname "$output_file")"
    cp "$doc_file" "$output_file"
    echo "project-lifecycle: rendered ${project_name} (ns ${ns}, cutover ${cutover_state:-unset}, binding checksum ${checksum}) -> ${output_file}"
  else
    echo "project-lifecycle: ${project_name} (ns ${ns}, cutover ${cutover_state:-unset}, binding checksum ${checksum})" >&2
    cat "$doc_file"
  fi
}

apply() {
  parse_args "$@"; resolve_binding_file
  need_kubectl
  local file
  file="${output_file:-$(default_render_path)}"
  [ -f "$file" ] || err "no rendered manifest at ${file}; run: $0 render --namespace ${ns} --output ${file}"
  kubectl apply -f "$file"
  kubectl -n "$ns" rollout status deployment/criteria-k8s-operator --timeout=300s
  echo "project-lifecycle: ${ns} cutover target state applied; operator available"
}

openbao_wire() {
  parse_args "$@"; resolve_binding_file
  # Emits a directly paste-able, parse-able command sequence following the
  # documented shared-OpenBao pattern (k8s/README.md 'OpenBao wiring',
  # k8s/01-openbao-config.yaml): the pod's CLI binary is vault. The policy is
  # written from STDIN (no temp file left in the pod) and the token is passed
  # the same way the README pattern does — via env on the kubectl exec.
  cat <<EOF
# Wire the project namespace's OpenBao role + policy against the SHARED
# OpenBao pod (k8s/01-openbao-config.yaml pattern; roleName ${ns}; pod CLI
# binary: vault). Sets V (root token) and pipes the policy inline:
V='<operator-root-token>'
kubectl exec -n default openbao-0 -- env VAULT_TOKEN="\${V}" vault policy write ${ns} - <<'POLICY_EOF'
path "criteria/data/${ns}/*" {
  capabilities = ["read", "list"]
}
path "criteria/metadata/${ns}/*" {
  capabilities = ["read", "list"]
}
path "criteria/data/linear" {
  capabilities = ["read", "list"]
}
path "criteria/metadata/linear" {
  capabilities = ["read", "list"]
}
POLICY_EOF
kubectl exec -n default openbao-0 -- env VAULT_TOKEN="\${V}" vault write auth/kubernetes/role/${ns} \\
  bound_service_account_names=criteria-runner \\
  bound_service_account_namespaces=${ns} \\
  policies=${ns} ttl=1h
EOF
  echo "# Reference copy lives in the rendered manifest: ConfigMap openbao-role-reference (${ns})."
}

verify() {
  parse_args "$@"; resolve_binding_file
  need_kubectl
  local stamped payload local_digest live_digest stamped_hex

  echo "== verify ${ns}: namespace labels =="
  kubectl get ns "$ns" -o json | jq -e --arg ns "$ns" \
    '.metadata.labels["criteria.brokenbots.dev/project"] == $ns and .metadata.labels["project"] == $ns' >/dev/null \
    || err "namespace ${ns} is not both project-labeled"
  echo "labels: ok"

  echo "== verify ${ns}: operator workload, PVCs, binding CM =="
  kubectl -n "$ns" rollout status deployment/criteria-k8s-operator --timeout=300s
  for pvc in criteria-data criteria-repo; do
    phase="$(kubectl -n "$ns" get pvc "$pvc" -o jsonpath='{.status.phase}')"
    [ "$phase" = "Bound" ] || err "pvc ${pvc} not Bound (phase: ${phase:-?})"
  done
  if ! payload="$(kubectl -n "$ns" get configmap criteria-binding -o jsonpath='{.data.binding\.json}')"; then
    err "binding ConfigMap criteria-binding not found in ${ns}"
  fi
  [ -n "$payload" ] || err "binding ConfigMap criteria-binding has an empty binding.json in ${ns}"
  # The deployed CM must carry the committed target state (drift between the
  # cluster and the repo is caught here, not attributed to the CR).
  local_digest="$(binding_checksum_for "$binding_file")"
  live_digest="$(printf '%s' "$payload" | canonical_sha256)"
  [ "$live_digest" = "$local_digest" ] \
    || err "deployed binding CM is not the committed target state: live ${ns}/criteria-binding binding.json hashes ${live_digest}, committed target state (${binding_file}) hashes ${local_digest}"
  echo "operator rollout, PVCs, binding CM: ok"

  if [ -n "$run_id" ]; then
    echo "== verify ${ns}: run ${run_id} =="
    if ! stamped="$(kubectl -n "$ns" get criteriarun "$run_id" -o jsonpath='{.status.launchManifest.bindingChecksum}')"; then
      err "run ${run_id} not found in ${ns}"
    fi
    [ -n "$stamped" ] || err "run ${run_id} carries no status.launchManifest.bindingChecksum"
    # The project-binding admission path stamps "sha256:<hex>" of the payload
    # it read AT ADMITTANCE; the acceptance leg is that this digest matches the
    # LIVE CM content (not the repo file), so hash the fetched payload here.
    stamped_hex="${stamped#sha256:}"
    [ "$stamped_hex" = "$live_digest" ] \
      || err "binding checksum mismatch for ${run_id}: CR status.launchManifest stamps ${stamped}, live ${ns}/criteria-binding binding.json hashes ${live_digest}"
    echo "run ${run_id}: CR stamp ${stamped} == live CM binding.json digest ${live_digest}"
  fi

  echo "== verify ${ns}: registry entry + global CM shrink =="
  local entry project_name cutover_state global_json
  entry="$(registry_entry)"
  project_name="$(printf '%s' "$entry" | jq -r '.key')"
  cutover_state="$(printf '%s' "$entry" | jq -r '.value.cutover // ""')"
  [ "$cutover_state" = "done" ] || err "registry cutover marker for ${project_name} is ${cutover_state:-unset}, not done"
  global_json="$(kubectl -n "$platform_ns" get configmap criteria-routes -o jsonpath='{.data.routes\.json}')"
  [ -n "$global_json" ] || err "${platform_ns}/criteria-routes carries no routes.json"
  printf '%s' "$global_json" | jq -e --arg project "$project_name" \
    '.projectRegistry[$project] != null' >/dev/null \
    || err "routes.json projectRegistry dropped ${project_name} (the registry entry is routing metadata and stays)"
  printf '%s' "$global_json" | jq -e --arg ns "$ns" \
    '[.workflowLibrary | tostring | index($ns)] == null' >/dev/null \
    || err "${ns}-domain binding data still present in the global workflowLibrary (deleted only after cutover-done)"
  echo "${project_name}: registry entry intact; workflowLibrary carries no ${ns}-domain binding data"
  echo "project-lifecycle: ${ns} cutover verification green"
}

triage() {
  parse_args "$@"; resolve_binding_file
  need_kubectl
  echo "== triage ${ns}: operator logs =="
  kubectl -n "$ns" logs deployment/criteria-k8s-operator --tail=100 || true
  echo "== triage ${ns}: events =="
  kubectl -n "$ns" get events --sort-by=.lastTimestamp || true
  echo "== triage ${ns}: binding checksum in CR status (status.launchManifest.bindingChecksum) =="
  if [ -n "$run_id" ]; then
    kubectl -n "$ns" get criteriarun "$run_id" \
      -o jsonpath='{.status.launchManifest.bindingChecksum}{"\n"}' 2>/dev/null || true
  else
    printf 'kubectl -n %s get criteriaruns -o jsonpath=\x27{range .items[*]}{.metadata.name}{"\\t"}{.status.launchManifest.bindingChecksum}{"\\n"}{end}\x27\n' "$ns"
  fi
  echo "== triage ${ns}: castle archive records (#308) =="
  echo "kubectl -n ${platform_ns} exec deploy/castle -c castle -- castle archives list --namespace ${ns}"
  echo "  (the operator's archive leg needs CASTLE_TOKEN_FILE mounted; this ns's"
  echo "   deployment carries it via castle-token-spc)"
}

# Pure toggle data: the exact env mutations (one `name=value` token per line)
# for the KB-309 retention switch, isolated from kubectl so tests can assert
# the armed/inert shapes offline.
retention_env_lines() {
  local onoff="$1" ns="$2"
  if [ "$onoff" = "on" ]; then
    # The operator arms the KB-309 sweeper ONLY when cr-retention-days > 0 AND
    # the allowlist is non-empty (criteria-k8s/cmd/operator/main.go); setting
    # namespace/keep-last alone arms nothing, so days MUST go positive too.
    # 7 is controller.DefaultCRRetentionDays (internal/controller/crretention.go);
    # keep in step if that default moves.
    printf '%s\n' "CR_RETENTION_NAMESPACES=$ns" CR_RETENTION_DAYS=7 CR_RETENTION_KEEP_LAST=1
  else
    # Restore the fully inert rendered triple: days=0 disarms the sweep at the
    # flag level (days<=0 never starts it), the allowlist and keep-last are
    # removed (kubectl - suffix), matching the shape the render emits.
    printf '%s\n' CR_RETENTION_DAYS=0 CR_RETENTION_KEEP_LAST=0 CR_RETENTION_NAMESPACES-
  fi
}

# Offline-readable form of the toggle's cluster action (the exact kubectl line
# a retention-on|off would run). No cluster access needed.
retention_plan() {
  local onoff="$1"; shift
  parse_args "$@"
  printf 'kubectl -n %s set env deployment/criteria-k8s-operator' "$ns"
  local kv
  while IFS= read -r kv; do printf ' %s' "$kv"; done < <(retention_env_lines "$onoff" "$ns")
  printf '\n'
}

retention_toggle() {
  local onoff="$1"; shift
  parse_args "$@"; resolve_binding_file
  need_kubectl
  local env_args=() kv
  while IFS= read -r kv; do env_args+=("$kv"); done < <(retention_env_lines "$onoff" "$ns")
  kubectl -n "$ns" set env deployment/criteria-k8s-operator "${env_args[@]}"
  if [ "$onoff" = "on" ]; then
    echo "project-lifecycle: KB-309 retention ON for ${ns} (days 7, allowlist ${ns}, keep-last 1)"
    echo "project-lifecycle: confirm the castle archive round-trip was proven BEFORE this toggle"
  else
    echo "project-lifecycle: KB-309 retention OFF for ${ns} (inert again)"
  fi
}

usage() { awk '/^# Usage:/,/separate cards\.$/' "$0" | sed 's/^# \{0,1\}//'; }

main() {
  local cmd="${1:-}"; [ -n "$cmd" ] || { usage >&2; exit 2; }
  shift
  case "$cmd" in
    render) parse_args "$@"; render ;;
    binding-checksum) parse_args "$@"; render_common; echo "${checksum}" ;;
    apply) apply "$@" ;;
    openbao-wire) openbao_wire "$@" ;;
    verify) verify "$@" ;;
    triage) triage "$@" ;;
    retention-on) retention_toggle on "$@" ;;
    retention-off) retention_toggle off "$@" ;;
    retention-plan) retention_plan "$@" ;;
    help|-h|--help) usage ;;
    *) err "unknown command '${cmd}'; see '$0 help'" ;;
  esac
}

main "$@"