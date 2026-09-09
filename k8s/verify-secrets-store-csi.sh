#!/usr/bin/env bash
set -euo pipefail

# Verify that the Secrets Store CSI driver and OpenBao CSI provider are
# running on the catch node and that the per-adapter SecretProviderClasses
# are present.
#
# Environment variables:
#   CSI_NAMESPACE   Namespace for the driver/provider (default: csi)
#   JOB_NAMESPACE   Namespace for workflow jobs (default: criteria-jobs)

CSI_NAMESPACE="${CSI_NAMESPACE:-csi}"
JOB_NAMESPACE="${JOB_NAMESPACE:-criteria-jobs}"

if ! command -v kubectl >/dev/null 2>&1; then
    echo "kubectl is required to verify the Secrets Store CSI driver" >&2
    exit 1
fi

wait_for_daemonset() {
    local ns="$1"
    local name="$2"
    echo "Waiting for DaemonSet ${ns}/${name} to be ready..."
    kubectl rollout status daemonset "${name}" -n "${ns}" --timeout=120s
}

echo "Checking Secrets Store CSI driver pods..."
wait_for_daemonset "${CSI_NAMESPACE}" "secrets-store-csi-driver"

echo "Checking OpenBao CSI provider pods..."
wait_for_daemonset "${CSI_NAMESPACE}" "openbao-csi-provider"

echo "Checking per-adapter SecretProviderClasses..."
for spc in linear-spc copilot-spc shell-spc; do
    if ! kubectl get secretproviderclass "${spc}" -n "${JOB_NAMESPACE}" >/dev/null 2>&1; then
        echo "SecretProviderClass ${JOB_NAMESPACE}/${spc} is missing" >&2
        exit 1
    fi
    echo "  ${spc}: present"
done

echo "All Secrets Store CSI driver, OpenBao provider, and SecretProviderClass checks passed."
