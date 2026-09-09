#!/usr/bin/env bash
set -euo pipefail

# Install the Secrets Store CSI driver and the OpenBao CSI provider on the
# k3s catch node. Both are deployed as DaemonSets so they run wherever
# workflow pods are scheduled; the values files pin them to amd64 and add
# tolerations for the catch/control-plane taints.
#
# Environment variables:
#   CSI_NAMESPACE   Namespace for the driver/provider (default: csi)
#   EXTERNAL_BAO_ADDR  OpenBao address for the provider (default:
#                      http://openbao-0.default.svc.cluster.local:8200)

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CSI_NAMESPACE="${CSI_NAMESPACE:-csi}"
EXTERNAL_BAO_ADDR="${EXTERNAL_BAO_ADDR:-http://openbao-0.default.svc.cluster.local:8200}"

if ! command -v helm >/dev/null 2>&1; then
    echo "helm is required to install the Secrets Store CSI driver" >&2
    exit 1
fi

helm repo add secrets-store-csi-driver https://kubernetes-sigs.github.io/secrets-store-csi-driver/charts
helm repo add openbao https://openbao.github.io/openbao-helm
helm repo update

echo "Installing secrets-store-csi-driver into namespace ${CSI_NAMESPACE}..."
helm upgrade --install secrets-store-csi-driver secrets-store-csi-driver/secrets-store-csi-driver \
    --namespace "${CSI_NAMESPACE}" \
    --create-namespace \
    --values "${REPO_ROOT}/k8s/values-secrets-store-csi-driver.yaml" \
    --wait

echo "Installing openbao-csi-provider into namespace ${CSI_NAMESPACE}..."
helm upgrade --install openbao openbao/openbao \
    --namespace "${CSI_NAMESPACE}" \
    --create-namespace \
    --set "global.externalBaoAddr=${EXTERNAL_BAO_ADDR}" \
    --values "${REPO_ROOT}/k8s/values-openbao-csi-provider.yaml" \
    --wait

echo "Secrets Store CSI driver and OpenBao provider installed."
