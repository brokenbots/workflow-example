#!/usr/bin/env bash
set -euo pipefail

# KB-281 successor of the CRI-27 example-job checks: k8s/operator-prereqs.yaml
# is the only operator-side manifest generated from the trimmed
# k8s/operator-prereqs.yaml.tmpl template. The linear-spc SecretProviderClass
# and the job-cri-27 Job-group example went with the retired linear_* workflow
# trees, so this test must FAIL if linear-spc ever comes back, and it pins the
# kept documents: the copilot-spc SPC (live OpenBao path criteria/data/linear)
# and the pod-adapter-scripts ConfigMap whose embedded wrapper scripts must
# stay byte-identical to their canonical sources.

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

MANIFEST="$REPO_ROOT/k8s/operator-prereqs.yaml"
GENERATOR="$REPO_ROOT/k8s/generate-pod-adapter-manifest.sh"
RUNNER_CANONICAL="$REPO_ROOT/k8s/pod-adapter-runner.sh"
ADAPTER_CANONICAL="$REPO_ROOT/k8s/pod-adapter-adapter.sh"

fail() {
    echo "FAIL: $1" >&2
    exit 1
}

has() {
    if ! grep -qF -- "$1" "$MANIFEST"; then
        fail "missing '$1' in operator-prereqs.yaml"
    fi
}

lacks() {
    if grep -qF -- "$1" "$MANIFEST"; then
        fail "operator-prereqs.yaml must not contain '$1'"
    fi
}

for f in "$MANIFEST" "$GENERATOR" "$RUNNER_CANONICAL" "$ADAPTER_CANONICAL"; do
    [ -f "$f" ] || fail "missing file: $f"
done

# 1. The committed manifest is exactly what the generator renders now.
prerender="$(mktemp)"
trap 'rm -f "$prerender"' EXIT
cp "$MANIFEST" "$prerender"
"$GENERATOR" >/dev/null
cmp -s "$prerender" "$MANIFEST" || \
    fail "k8s/operator-prereqs.yaml drifted from its template/generator (run k8s/generate-pod-adapter-manifest.sh)"
if [ -n "$(git -C "$REPO_ROOT" status --porcelain -- k8s/operator-prereqs.yaml charts/criteria-k8s/scripts/)" ]; then
    fail "regenerating operator-prereqs.yaml or the chart scripts changed tracked files (run k8s/generate-pod-adapter-manifest.sh and commit)"
fi

# 2. Unsubstituted template placeholders may not ship.
if grep -Eq -- '__(RUNNER_SCRIPT|ADAPTER_SCRIPT)__|namespace: __[A-Za-z]+' "$MANIFEST"; then
    fail "operator-prereqs.yaml still carries an unsubstituted template placeholder"
fi

# 3. KB-281 absence regression: the retired linear-spc SPC must not return,
#    and the retired Job-group example must not be resurrected here.
lacks 'name: linear-spc'
lacks 'secretProviderClass: linear-spc'
if grep -q '^kind: Job' "$MANIFEST"; then
    fail "operator-prereqs.yaml must carry only prerequisites, not example Jobs"
fi

# 4. The kept copilot-spc SPC: openbao provider, live secret path, no
#    key references or vault-legacy fields.
has 'name: copilot-spc'
has 'provider: openbao'
has 'secretPath: criteria/data/linear'
has 'objectName: workflow_github_token'
has 'objectName: reviewer_github_token'
lacks 'name: linear_api_key'
lacks 'vaultAddress'
lacks 'secretKeyRef'
lacks 'envFrom'

# 5. The kept pod-adapter-scripts ConfigMap embeds the same scripts as the
#    canonical sources and the chart copies.
has 'name: pod-adapter-scripts'
cmp_embedded() {
    local key="$1" canonical="$2" block
    block="$(awk -v key="$key: |" 'index($0, key) {flag=1; next} flag && /^  [^ ]/ {flag=0} flag {gsub(/^    /,""); print}' "$MANIFEST")"
    [ -n "$block" ] || fail "no embedded $key document in pod-adapter-scripts ConfigMap"
    diff <(printf '%s\n' "$block") "$canonical" >/dev/null || \
        fail "embedded $key drifted from $canonical (run k8s/generate-pod-adapter-manifest.sh)"
}
cmp_embedded "runner.sh" "$RUNNER_CANONICAL"
cmp_embedded "adapter.sh" "$ADAPTER_CANONICAL"

# 5b. The embedded runner.sh must carry the KB-344 run-event proxy identity
#     block (--name run attribution + --server-bootstrap-token enrollment):
#     the byte-compare above already catches drift from the canonical copy,
#     but pinning the block by name turns any future "simplification" of
#     the proxy handoff into a named failure here, same as the other copies.
has 'set -- "$@" --name "$CRITERIA_RUN_NAME"'
has ': "${CRITERIA_PROXY_ENROLLMENT_TOKEN:=}"'
has 'if [ -n "$CRITERIA_PROXY_ENROLLMENT_TOKEN" ]'
has 'set -- "$@" --server-bootstrap-token "$CRITERIA_PROXY_ENROLLMENT_TOKEN"'

# 6. Secret hygiene: credential-shaped literals never ship in the manifest.
if grep -qE 'ghp_|gho_|github_pat_|hvs\.' "$MANIFEST"; then
    fail "credential-shaped literal found in operator-prereqs.yaml"
fi

echo "PASS: operator-prereqs is up to date, linear-spc-free, and keeps only copilot-spc + pod-adapter-scripts"
