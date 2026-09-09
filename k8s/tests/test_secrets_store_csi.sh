#!/usr/bin/env bash
set -euo pipefail

# Regression test for the Secrets Store CSI driver / OpenBao provider
# installation artifacts. When helm is available the test also templates both
# charts to confirm the catch-node scheduling values are accepted.

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
INSTALL_SCRIPT="${REPO_ROOT}/k8s/install-secrets-store-csi.sh"
DRIVER_VALUES="${REPO_ROOT}/k8s/values-secrets-store-csi-driver.yaml"
BAO_VALUES="${REPO_ROOT}/k8s/values-openbao-csi-provider.yaml"

fail() {
    echo "FAIL: $1" >&2
    exit 1
}

[ -x "${INSTALL_SCRIPT}" ] || fail "install-secrets-store-csi.sh is missing or not executable"
[ -f "${DRIVER_VALUES}" ] || fail "values-secrets-store-csi-driver.yaml is missing"
[ -f "${BAO_VALUES}" ] || fail "values-openbao-csi-provider.yaml is missing"

# The driver values must require amd64 and tolerate catch/control-plane.
grep -q 'kubernetes.io/arch: amd64' "${DRIVER_VALUES}" || \
    fail "driver values do not select amd64 nodes"
grep -q 'key: catch' "${DRIVER_VALUES}" || \
    fail "driver values do not tolerate the catch taint"
grep -q 'key: node-role.kubernetes.io/control-plane' "${DRIVER_VALUES}" || \
    fail "driver values do not tolerate the control-plane taint"
grep -q 'windows:' "${DRIVER_VALUES}" || \
    fail "driver values are missing the Windows block"
grep -q 'enabled: false' "${DRIVER_VALUES}" || \
    fail "driver values do not disable the Windows daemonset"

# The OpenBao provider values must disable server/injector, enable only the
# CSI provider, and point at the existing OpenBao instance.
grep -q 'externalBaoAddr:' "${BAO_VALUES}" || \
    fail "openbao provider values do not set externalBaoAddr"
grep -q 'server:' "${BAO_VALUES}" || fail "openbao provider values are missing server block"
grep -q 'injector:' "${BAO_VALUES}" || fail "openbao provider values are missing injector block"
grep -q 'csi:' "${BAO_VALUES}" || fail "openbao provider values are missing csi block"
grep -q 'enabled: true' "${BAO_VALUES}" || \
    fail "openbao provider values do not enable CSI"
grep -q 'agent:' "${BAO_VALUES}" || fail "openbao provider values are missing csi agent block"
grep -q 'enabled: false' "${BAO_VALUES}" || \
    fail "openbao provider values do not disable the agent sidecar"
grep -q 'kubernetes.io/arch: amd64' "${BAO_VALUES}" || \
    fail "openbao provider values do not select amd64 nodes"
grep -q 'key: catch' "${BAO_VALUES}" || \
    fail "openbao provider values do not tolerate the catch taint"
grep -q 'key: node-role.kubernetes.io/control-plane' "${BAO_VALUES}" || \
    fail "openbao provider values do not tolerate the control-plane taint"

if command -v helm >/dev/null 2>&1; then
    echo "helm found; validating chart templates..."
    helm repo add secrets-store-csi-driver https://kubernetes-sigs.github.io/secrets-store-csi-driver/charts >/dev/null 2>&1 || true
    helm repo add openbao https://openbao.github.io/openbao-helm >/dev/null 2>&1 || true
    helm repo update >/dev/null 2>&1 || true

    driver_manifest=$(mktemp)
    bao_manifest=$(mktemp)
    trap 'rm -f "${driver_manifest}" "${bao_manifest}"' EXIT

    helm template secrets-store-csi-driver secrets-store-csi-driver/secrets-store-csi-driver \
        --values "${DRIVER_VALUES}" --kube-version 1.30.0 > "${driver_manifest}" || \
        fail "failed to template secrets-store-csi-driver"
    grep -q 'kind: DaemonSet' "${driver_manifest}" || \
        fail "secrets-store-csi-driver template produced no DaemonSet"

    helm template openbao openbao/openbao \
        --values "${BAO_VALUES}" --kube-version 1.30.0 > "${bao_manifest}" || \
        fail "failed to template openbao provider"
    grep -q 'kind: DaemonSet' "${bao_manifest}" || \
        fail "openbao provider template produced no DaemonSet"
    grep -q 'app.kubernetes.io/name: openbao-csi-provider' "${bao_manifest}" || \
        fail "openbao provider template does not label pods as openbao-csi-provider"
else
    echo "helm not found; skipping chart template validation"
fi

echo "PASS: Secrets Store CSI driver / OpenBao provider installation artifacts are valid"
