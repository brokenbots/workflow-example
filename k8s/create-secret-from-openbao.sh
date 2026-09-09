#!/bin/bash
# create-secret-from-openbao.sh
#
# Pulls the criteria secrets from OpenBao and writes them as a Kubernetes
# Secret in the criteria-jobs namespace. This is the bridge between OpenBao
# (the source of truth) and the Job manifest (which references the K8s Secret
# via envFrom/secretKeyRef).
#
# Usage:
#   KUBECONFIG=~/.kube/config ./k8s/create-secret-from-openbao.sh
#
# Requires: kubectl, and OpenBao unsealed and reachable via the openbao-0 pod.
#
# After updating secret values in OpenBao, re-run this script to refresh the
# K8s Secret.
set -eu

NAMESPACE="${NAMESPACE:-criteria-jobs}"
SECRET_NAME="${SECRET_NAME:-criteria-secrets}"
OPENBAO_NS="${OPENBAO_NS:-default}"
OPENBAO_POD="${OPENBAO_POD:-openbao-0}"

# Root token from the openbao.keys file or environment
if [ -z "${VAULT_TOKEN:-}" ]; then
  KEYS_FILE="${KEYS_FILE:-$HOME/k3s-dynamo/openbao.keys}"
  if [ ! -f "$KEYS_FILE" ]; then
    echo "Error: set VAULT_TOKEN or point KEYS_FILE at your openbao.keys file" >&2
    exit 1
  fi
  VAULT_TOKEN=$(grep "Initial Root Token:" "$KEYS_FILE" | awk '{print $NF}')
fi

echo "Reading secrets from OpenBao at criteria/linear..."
LINEAR_JSON=$(kubectl exec -n "$OPENBAO_NS" "$OPENBAO_POD" -- \
  env VAULT_TOKEN="$VAULT_TOKEN" vault kv get -format=json criteria/linear)

LINEAR_API_KEY=$(echo "$LINEAR_JSON" | jq -r '.data.data.linear_api_key')
WORKFLOW_GITHUB_TOKEN=$(echo "$LINEAR_JSON" | jq -r '.data.data.workflow_github_token')
REVIEWER_GITHUB_TOKEN=$(echo "$LINEAR_JSON" | jq -r '.data.data.reviewer_github_token')

echo "Reading castle config from OpenBao at criteria/castle..."
CASTLE_JSON=$(kubectl exec -n "$OPENBAO_NS" "$OPENBAO_POD" -- \
  env VAULT_TOKEN="$VAULT_TOKEN" vault kv get -format=json criteria/castle)

CASTLE_ADDR=$(echo "$CASTLE_JSON" | jq -r '.data.data.castle_addr')
CASTLE_BOOTSTRAP_TOKEN=$(echo "$CASTLE_JSON" | jq -r '.data.data.castle_bootstrap_token // ""')

echo "Creating Kubernetes Secret $SECRET_NAME in $NAMESPACE..."

kubectl delete secret "$SECRET_NAME" -n "$NAMESPACE" 2>/dev/null || true

kubectl create secret generic "$SECRET_NAME" \
  -n "$NAMESPACE" \
  --from-literal=linear_api_key="$LINEAR_API_KEY" \
  --from-literal=workflow_github_token="$WORKFLOW_GITHUB_TOKEN" \
  --from-literal=reviewer_github_token="$REVIEWER_GITHUB_TOKEN" \
  --from-literal=castle_addr="$CASTLE_ADDR" \
  --from-literal=castle_bootstrap_token="$CASTLE_BOOTSTRAP_TOKEN"

echo "Done. Secret $SECRET_NAME created in $NAMESPACE."
echo "  LINEAR_API_KEY: ${LINEAR_API_KEY:0:8}..."
echo "  WORKFLOW_GITHUB_TOKEN: ${WORKFLOW_GITHUB_TOKEN:0:8}..."
echo "  REVIEWER_GITHUB_TOKEN: ${REVIEWER_GITHUB_TOKEN:0:8}..."
echo "  CASTLE_ADDR: $CASTLE_ADDR"