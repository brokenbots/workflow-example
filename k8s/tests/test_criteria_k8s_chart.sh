#!/usr/bin/env bash
set -euo pipefail

# Regression test for the criteria-k8s Helm chart (CRI-129). The chart
# packages the hand-applied k8s/ manifests as a single install/upgrade unit.
# This test verifies the chart structure, the scripts ConfigMap sync, the
# rendered resource inventory, and that values actually drive the templates.
#
# When helm is available the chart is linted and templated; without helm the
# structural checks still run and template rendering is skipped.

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
CHART="$REPO_ROOT/charts/criteria-k8s"

fail() {
    echo "FAIL: $1" >&2
    exit 1
}

# ---------------------------------------------------------------- structure
[ -f "$CHART/Chart.yaml" ] || fail "charts/criteria-k8s/Chart.yaml is missing"
[ -f "$CHART/values.yaml" ] || fail "charts/criteria-k8s/values.yaml is missing"
[ -d "$CHART/templates" ] || fail "charts/criteria-k8s/templates is missing"
[ -f "$CHART/crds/criteria.brokenbots.dev_criteriaruns.yaml" ] || fail "CriteriaRun CRD is missing from crds/"

# The CRD must match the canonical schema in criteria-k8s/config.
cmp -s "$CHART/crds/criteria.brokenbots.dev_criteriaruns.yaml" \
    "$REPO_ROOT/criteria-k8s/config/crd/bases/criteria.brokenbots.dev_criteriaruns.yaml" || \
    fail "chart CRD has drifted from criteria-k8s/config/crd/bases"

# install.yaml carries a third copy of the CriteriaRun CRD (a hand-maintained
# structural schema with status subresource and an enumerated
# status.properties map, so unknown fields are pruned by the API server).
# It must expose the same status.properties key set as the chart CRD: a
# missing key there means the operator's status writes of that field are
# silently dropped on clusters installed from install.yaml.
crd_status_props() {
    awk '/^            status:$/{on = 1; next}
         on && /^            [a-z]/{on = 0}
         on && /^                [a-zA-Z]+:$/{gsub(/:$/, "", $0); print $1}' "$1" | sort
}
cmp -s <(crd_status_props "$CHART/crds/criteria.brokenbots.dev_criteriaruns.yaml") \
    <(crd_status_props "$REPO_ROOT/criteria-k8s/config/install.yaml") || \
    fail "install.yaml CRD status.properties drifted from the chart CRD (e.g. missing castleTerminalObserved)"

# Chart-embedded wrapper scripts must be byte-identical to the canonical
# k8s/ sources (generate-pod-adapter-manifest.sh keeps them in sync).
cmp -s "$CHART/scripts/runner.sh" "$REPO_ROOT/k8s/pod-adapter-runner.sh" || \
    fail "charts/criteria-k8s/scripts/runner.sh drifted from k8s/pod-adapter-runner.sh (run ./k8s/generate-pod-adapter-manifest.sh)"
cmp -s "$CHART/scripts/adapter.sh" "$REPO_ROOT/k8s/pod-adapter-adapter.sh" || \
    fail "charts/criteria-k8s/scripts/adapter.sh drifted from k8s/pod-adapter-adapter.sh (run ./k8s/generate-pod-adapter-manifest.sh)"

# No templated secrets: values and templates must never carry credentials.
if grep -rniE 'stringData|secretKeyRef|^kind: Secret$' "$CHART/values.yaml" "$CHART/templates" > /dev/null 2>&1; then
    fail "chart templates or values reference templated secrets"
fi

# Values contract (CRI-129): image repo+tag, namespace, PVC names+sizes,
# retention period/interval, provider base URL, Linear project/state, poll
# interval must all be configurable without editing templates.
grep -q 'repository: localhost:5000/criteria-k8s' "$CHART/values.yaml" || fail "values.yaml missing operator image repository"
grep -q 'tag: dev' "$CHART/values.yaml" || fail "values.yaml missing image tag"
grep -q 'namespace: criteria-jobs' "$CHART/values.yaml" || fail "values.yaml missing namespace"
grep -q 'name: criteria-data' "$CHART/values.yaml" || fail "values.yaml missing data PVC name"
grep -q 'size: 10Gi' "$CHART/values.yaml" || fail "values.yaml missing PVC size"
grep -q 'retentionPeriod: 168h' "$CHART/values.yaml" || fail "values.yaml missing retention period"
grep -q 'retentionInterval: 1h' "$CHART/values.yaml" || fail "values.yaml missing retention interval"
grep -q 'providerBaseUrl:' "$CHART/values.yaml" || fail "values.yaml missing provider base URL"
grep -q 'linearProjectName:' "$CHART/values.yaml" || fail "values.yaml missing Linear project"
grep -q 'linearTriageState:' "$CHART/values.yaml" || fail "values.yaml missing Linear triage state"
grep -q 'pollInterval: 60s' "$CHART/values.yaml" || fail "values.yaml missing poll interval"

if ! command -v helm > /dev/null 2>&1; then
    echo "helm not found; chart structure checks passed, skipping template rendering"
    echo "PASS: criteria-k8s chart structure is valid"
    exit 0
fi

# ------------------------------------------------------------- helm lint
helm lint "$CHART" > /dev/null 2>&1 || fail "helm lint failed"

# ---------------------------------------------------- default-value rendering
RENDERED="$(mktemp)"
NS_RENDERED="$(mktemp)"
CRD_RENDERED="$(mktemp)"
cleanup() {
    rm -f "$RENDERED" "$NS_RENDERED" "$CRD_RENDERED"
}
trap cleanup EXIT

helm template criteria-k8s "$CHART" > "$RENDERED" || fail "helm template failed with default values"

HAVE_PY_YAML=0
if python3 -c "import yaml" 2> /dev/null; then
    HAVE_PY_YAML=1
fi

# Count rendered documents of a kind in the criteria-jobs namespace.
count_kind() { # kind
    local kind="$1"
    if [ "$HAVE_PY_YAML" -eq 1 ]; then
        python3 - "$RENDERED" "$kind" <<'PY'
import sys, yaml
docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
kind = sys.argv[2]
print(sum(1 for d in docs if d.get("kind") == kind))
PY
    else
        grep -c "^kind: $kind\$" "$RENDERED" || true
    fi
}

# Resource inventory with default values (diff-reviewable against k8s/).
[ "$(count_kind Deployment)" -eq 3 ] || fail "expected 3 Deployments (operator, watcher, castle), got $(count_kind Deployment)"
grep -q 'name: castle$' "$RENDERED" || fail "castle Deployment missing"
[ "$(count_kind Service)" -eq 1 ] || fail "expected 1 Service (castle), got $(count_kind Service)"
[ "$(count_kind PersistentVolumeClaim)" -eq 2 ] || fail "expected 2 PVCs, got $(count_kind PersistentVolumeClaim)"
[ "$(count_kind SecretProviderClass)" -eq 2 ] || fail "expected 2 SecretProviderClasses, got $(count_kind SecretProviderClass)"
[ "$(count_kind Role)" -eq 2 ] || fail "expected 2 Roles (operator, runner), got $(count_kind Role)"
[ "$(count_kind RoleBinding)" -eq 2 ] || fail "expected 2 RoleBindings, got $(count_kind RoleBinding)"
[ "$(count_kind ServiceAccount)" -eq 2 ] || fail "expected 2 ServiceAccounts, got $(count_kind ServiceAccount)"
[ "$(count_kind ConfigMap)" -eq 1 ] || fail "expected 1 ConfigMap (pod-adapter-scripts), got $(count_kind ConfigMap)"
[ "$(count_kind Secret)" -eq 0 ] || fail "chart must not template Secret resources"
[ "$(count_kind Namespace)" -eq 0 ] || fail "namespace must not be rendered by default (Helm skips ns; use --create-namespace)"

# Namespaced RBAC for the operator (k8s/operator-rbac.yaml shape).
grep -q 'criteriaruns/status' "$RENDERED" || fail "operator Role does not cover criteriaruns/status"
grep -q 'resources: \["pods", "pods/exec", "pods/log", "events"\]' "$RENDERED" || fail "operator Role does not cover pods/exec/log/events"
grep -q 'resourceNames: \["criteria-secrets"\]' "$RENDERED" || fail "runner Role does not scope to criteria-secrets"

# Operator deployment shape: retention env, namespace flag, /data mount.
grep -q 'name: RETENTION_PERIOD' "$RENDERED" || fail "operator deployment missing RETENTION_PERIOD"
grep -q 'value: "168h"' "$RENDERED" || fail "operator RETENTION_PERIOD default is not 168h"
grep -q 'name: RETENTION_INTERVAL' "$RENDERED" || fail "operator deployment missing RETENTION_INTERVAL"
grep -q 'name: DEFAULT_CRITERIA_IMAGE' "$RENDERED" || fail "operator deployment missing DEFAULT_CRITERIA_IMAGE"
grep -q 'command: \["/manager"\]' "$RENDERED" || fail "operator container command missing"
grep -q -- '--namespace' "$RENDERED" || fail "operator deployment missing --namespace flag"
grep -q 'mountPath: /data$' "$RENDERED" || fail "operator does not mount /data"

# Watcher deployment shape: Linear polling config through CSI only.
grep -q 'name: criteria-linear-watcher' "$RENDERED" || fail "watcher deployment missing"
grep -q 'command: \["/linear-watcher"\]' "$RENDERED" || fail "watcher container command missing"
grep -q 'name: LINEAR_PROJECT_NAME' "$RENDERED" || fail "watcher missing LINEAR_PROJECT_NAME"
grep -q 'name: LINEAR_TRIAGE_STATE' "$RENDERED" || fail "watcher missing LINEAR_TRIAGE_STATE"
grep -q 'name: POLL_INTERVAL' "$RENDERED" || fail "watcher missing POLL_INTERVAL"
grep -q 'secretProviderClass: linear-spc' "$RENDERED" || fail "watcher CSI volume does not reference linear-spc"
grep -q 'mountPath: /secrets/linear_api_key' "$RENDERED" || fail "watcher does not mount the Linear API key file"

# Scripts ConfigMap contents must equal the canonical wrapper scripts.
extract_configmap_file() { # key -> content on stdout
    local key="$1" stop
    if [ "$key" = "runner.sh" ]; then stop="adapter.sh"; else stop=""; fi
    awk -v start="  $key: |" -v stop="  $stop: |" '
        !flag && index($0, start) == 1 { flag = 1; next }
        stop != "" && flag && index($0, stop) == 1 { flag = 0; next }
        flag && $0 == "---" { flag = 0; next }
        flag { sub(/^    /, ""); print }
    ' "$RENDERED"
}

if [ "$HAVE_PY_YAML" -eq 1 ]; then
    python3 - "$RENDERED" "$REPO_ROOT/k8s/pod-adapter-runner.sh" "$REPO_ROOT/k8s/pod-adapter-adapter.sh" <<'PY' || fail "rendered pod-adapter-scripts ConfigMap differs from k8s source scripts"
import sys, yaml
cm = next(d for d in yaml.safe_load_all(open(sys.argv[1])) if d and d.get("kind") == "ConfigMap")
assert cm["metadata"]["name"] == "pod-adapter-scripts", "wrong ConfigMap name"
for key, path in (("runner.sh", sys.argv[2]), ("adapter.sh", sys.argv[3])):
    want = open(path).read()
    got = cm["data"][key]
    if got != want:
        print(f"FAIL: ConfigMap {key} differs from {path}", file=sys.stderr)
        sys.exit(1)
print("rendered pod-adapter-scripts ConfigMap matches source scripts")
PY
else
    extract_configmap_file runner.sh > "$RENDERED.runner"
    cmp -s "$RENDERED.runner" "$REPO_ROOT/k8s/pod-adapter-runner.sh" || \
        fail "rendered pod-adapter-scripts/runner.sh differs from k8s/pod-adapter-runner.sh"
    extract_configmap_file adapter.sh > "$RENDERED.adapter"
    cmp -s "$RENDERED.adapter" "$REPO_ROOT/k8s/pod-adapter-adapter.sh" || \
        fail "rendered pod-adapter-scripts/adapter.sh differs from k8s/pod-adapter-adapter.sh"
    rm -f "$RENDERED.runner" "$RENDERED.adapter"
fi

# ------------------------------------------------- namespaceCreate rendering
helm template criteria-k8s "$CHART" --set namespaceCreate=true > "$NS_RENDERED" || \
    fail "helm template failed with namespaceCreate=true"
grep -q '^kind: Namespace$' "$NS_RENDERED" || fail "namespaceCreate=true did not render a Namespace"
grep -q 'pod-security.kubernetes.io/enforce: baseline' "$NS_RENDERED" || fail "rendered Namespace lacks pod-security label"

# ------------------------------------------------------ value override wiring
check_override() { # description set-expr pattern
    local out
    out="$(helm template criteria-k8s "$CHART" --set "$2")" || fail "helm template failed for $1"
    printf '%s' "$out" | grep -q "$3" || fail "$1 not reflected in rendered output (--set $2)"
}

check_override "operator image tag override" "images.operator.tag=1.2.3" 'image: "localhost:5000/criteria-k8s:1.2.3"'
check_override "retention period override" "operator.retentionPeriod=48h" 'value: "48h"'
check_override "poll interval override" "watcher.pollInterval=30s" 'value: "30s"'
check_override "PVC size override" "pvc.data.size=20Gi" 'storage: 20Gi'
check_override "OpenBao secret path override" "openbao.secretPath=other/data/x" 'secretPath: other/data/x'
check_override "Linear project override" "watcher.linearProjectName=Other Project" 'value: "Other Project"'
check_override "Linear triage state override" "watcher.linearTriageState=Backlog" 'value: "Backlog"'
check_override "namespace override" "namespace=other-ns" 'namespace: other-ns'

out="$(helm template criteria-k8s "$CHART" --set "images.workflow.repository=ghcr.io/acme/runner" --set "images.workflow.tag=v5")" \
    || fail "helm template failed for workflow image"
printf '%s' "$out" | grep -q 'value: "ghcr.io/acme/runner:v5"' || fail "workflow image override not reflected in DEFAULT_CRITERIA_IMAGE"

# Empty namespace value falls back to the release namespace.
out="$(helm template criteria-k8s "$CHART" -n alt-ns --set namespace="")" || fail "helm template failed for namespace fallback"
printf '%s' "$out" | grep -q 'namespace: alt-ns' || fail "empty namespace does not fall back to the release namespace"

# -------------------------------------------------------------- toggles
out="$(helm template criteria-k8s "$CHART" --set castle.enabled=false)" || fail "helm template failed with castle disabled"
printf '%s' "$out" | grep -q '^kind: Service$' && fail "castle Service rendered despite castle.enabled=false"
printf '%s' "$out" | grep -q 'name: castle$' && fail "castle Deployment rendered despite castle.enabled=false"

out="$(helm template criteria-k8s "$CHART" --set operator.enabled=false --set watcher.enabled=false --set castle.enabled=false)" \
    || fail "helm template failed with all components disabled"
printf '%s' "$out" | grep -q '^kind: Deployment$' && fail "Deployment rendered with all components disabled"

# ------------------------------------------------------- CRD packaging
helm template criteria-k8s "$CHART" --include-crds > "$CRD_RENDERED" || fail "helm template --include-crds failed"
grep -q '^kind: CustomResourceDefinition$' "$CRD_RENDERED" || fail "CRD not packaged under crds/"
grep -q 'name: criteriaruns.criteria.brokenbots.dev' "$CRD_RENDERED" || fail "CRD has wrong name"

# ------------------------------------------ shape equivalence vs k8s sources
# The chart replaces live manifests. Deployment.spec.selector is immutable and
# Service selectors must keep matching pods, so rendered selectors, pod
# labels, and security contexts must be field-equal to the manifests being
# replaced (new chart labels may only be additive).

# Print the document for kind/name from a multi-document YAML file.
doc_of() { # file kind name
    awk -v kind="$2" -v name="$3" '
        $0 == "---" {
            if (isdoc) { printf "%s", buf; exit }
            buf = ""
            seenname = 0
            isdoc = 0
            next
        }
        {
            if (!seenname && $0 ~ /^  name: /) {
                nm = $0
                sub(/^  name: */, "", nm)
                if (nm == name && buf ~ ("\nkind: " kind "\n")) {
                    isdoc = 1
                }
                seenname = 1
            }
            buf = buf $0 "\n"
        }
        END {
            if (isdoc) printf "%s", buf
        }
    ' "$1"
}

# Normalize a document for comparison: strip comments, blanks and indentation
# (quote stripping makes YAML quoting style irrelevant).
norm_doc() { # file kind name
    doc_of "$1" "$2" "$3" \
        | grep -v '^[[:space:]]*#' \
        | tr -d '"' \
        | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//' \
        | grep -v '^$'
}

# Sorted slice of a normalized document between exact keys (end exclusive).
sorted_block() { # start end (stdin: normalized doc)
    awk -v s="$1" -v e="$2" '$0 == s { f = 1; print; next } f && $0 == e { exit } f { print }' | sort
}

assert_blocks_equal() { # description rendered-block source-block
    [ "$2" = "$3" ] || fail "$1 differs from the k8s source shape"
}

castle_dep_r="$(norm_doc "$RENDERED" Deployment castle)"
castle_dep_s="$(norm_doc "$REPO_ROOT/k8s/04-castle.yaml" Deployment castle)"
castle_svc_r="$(norm_doc "$RENDERED" Service castle)"
castle_svc_s="$(norm_doc "$REPO_ROOT/k8s/04-castle.yaml" Service castle)"

# Castle Deployment selector and Service selector must equal the live
# manifest (immutable fields; a renamed selector would select no pods).
assert_blocks_equal "castle Deployment selector" \
    "$(printf '%s\n' "$castle_dep_r" | sorted_block 'selector:' 'template:')" \
    "$(printf '%s\n' "$castle_dep_s" | sorted_block 'selector:' 'template:')"
assert_blocks_equal "castle Service selector" \
    "$(printf '%s\n' "$castle_svc_r" | sorted_block 'selector:' 'ports:')" \
    "$(printf '%s\n' "$castle_svc_s" | sorted_block 'selector:' 'ports:')"

# Pod template labels must keep the live-manifest compat key; chart labels
# are additive on top of it.
printf '%s\n' "$castle_dep_r" \
    | awk '$0 == "template:" { f = 1; next } f && $0 == "spec:" { exit } f { print }' \
    | grep -qx 'app: castle' || fail "castle pod template dropped the live app: castle label"

# Castle container block (image, securityContext, env, command, probes,
# mounts) must equal the live manifest; the chart must not add runtime
# constraints the castle image was not tested with.
assert_blocks_equal "castle container securityContext/env/ports" \
    "$(printf '%s\n' "$castle_dep_r" | sed -n '/^- name: castle$/,/^volumes:/{/^volumes:/!p}' | sort)" \
    "$(printf '%s\n' "$castle_dep_s" | sed -n '/^- name: castle$/,/^volumes:/{/^volumes:/!p}' | sort)"
printf '%s\n' "$castle_dep_r" | grep -q 'readOnlyRootFilesystem' && \
    fail "castle container must not gain readOnlyRootFilesystem (deviates from k8s/04-castle.yaml)"

# Pod securityContext (restricted compliance) must match the live manifest.
assert_blocks_equal "castle pod securityContext" \
    "$(printf '%s\n' "$castle_dep_r" | sorted_block 'securityContext:' 'nodeSelector:')" \
    "$(printf '%s\n' "$castle_dep_s" | sorted_block 'securityContext:' 'nodeSelector:')"

# Operator/watcher selectors must equal criteria-k8s/config/install.yaml.
for comp in criteria-k8s-operator criteria-linear-watcher; do
    assert_blocks_equal "$comp Deployment selector" \
        "$(norm_doc "$RENDERED" Deployment "$comp" | sorted_block 'selector:' 'template:')" \
        "$(norm_doc "$REPO_ROOT/criteria-k8s/config/install.yaml" Deployment "$comp" | sorted_block 'selector:' 'template:')"
done

echo "PASS: criteria-k8s chart lint, inventory, values wiring and script sync are valid"