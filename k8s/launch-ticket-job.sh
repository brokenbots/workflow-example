#!/usr/bin/env bash
set -euo pipefail

# Launch a Kubernetes Job for linear_intake_v1 using the remote-mode image.
#
# Environment variables:
#   IMAGE                     Container image to run (default: localhost:5000/linear-intake-remote:dev)
#   NAMESPACE                 Target namespace (default: default)
#   JOB_NAME                  Explicit Job name; defaults to linear-intake-<lowercase ticket>
#   DATA_PVC                  PVC for /data (default: criteria-data)
#   REPO_PVC                  PVC for /repo (default: criteria-repo)
#   CREATE_SECRET             Create/update linear-intake-credentials Secret from env vars (default: true)
#   DRY_RUN                   Render the Job manifest to stdout instead of applying it (default: unset)
#
#   TICKET_ID                 Required Linear ticket identifier
#   REPO_URL                  Required GitHub repository (e.g. brokenbots/criteria)
#
#   LINEAR_API_KEY            Optional; used when CREATE_SECRET=true
#   WORKFLOW_GITHUB_TOKEN     Optional; used when CREATE_SECRET=true
#   REVIEWER_GITHUB_TOKEN     Optional; used when CREATE_SECRET=true
#
#   All other workflow settings use the same defaults as container-entrypoint.sh.

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TEMPLATE="$REPO_ROOT/k8s/05-job-template.yaml"

export IMAGE="${IMAGE:-localhost:5000/linear-intake-remote:dev}"
export NAMESPACE="${NAMESPACE:-criteria-jobs}"
export DATA_PVC="${DATA_PVC:-criteria-data}"
export REPO_PVC="${REPO_PVC:-criteria-repo}"
export CREATE_SECRET="${CREATE_SECRET:-true}"

export LINEAR_REVIEW_STATE="${LINEAR_REVIEW_STATE:-In Review}"
export LINEAR_TRIAGE_STATE="${LINEAR_TRIAGE_STATE:-Triage}"
export LINEAR_WORK_STATE="${LINEAR_WORK_STATE:-In Progress}"
export LINEAR_DONE_STATE="${LINEAR_DONE_STATE:-Done}"
export BASE_BRANCH="${BASE_BRANCH:-main}"
export BUILD_CMD="${BUILD_CMD:-}"
export TEST_CMD="${TEST_CMD:-}"
export CI_GATE_CMD="${CI_GATE_CMD:-}"
export TEST_REFS="${TEST_REFS:-both}"
export MAIN_REF="${MAIN_REF:-origin/main}"
export STABLE_REF="${STABLE_REF:-}"
export DESIGN_INTENT_FILE="${DESIGN_INTENT_FILE:-}"
export REPRO_WORKFLOW_DIR="${REPRO_WORKFLOW_DIR:-}"
export ALLOW_DIRTY="${ALLOW_DIRTY:-false}"
export MAX_AGENT_VISITS="${MAX_AGENT_VISITS:-2}"
export PROVIDER_BASE_URL="${PROVIDER_BASE_URL:-http://192.168.17.116:11434/v1}"

: "${TICKET_ID:?TICKET_ID is required}"
: "${REPO_URL:?REPO_URL is required}"
export TICKET_ID
export REPO_URL

# Lowercase job naming logic: default name is derived from the ticket id.
DEFAULT_JOB_NAME="linear-intake-$(printf '%s' "$TICKET_ID" | tr '[:upper:]' '[:lower:]')"
export JOB_NAME="${JOB_NAME:-$DEFAULT_JOB_NAME}"

render_template() {
    perl -pe 's/\$\{(\w+)\}/exists $ENV{$1} ? $ENV{$1} : die "template placeholder $1 is missing\n"/ge' "$1"
}

if [ -n "${DRY_RUN:-}" ]; then
    render_template "$TEMPLATE"
    exit 0
fi

if [ "$CREATE_SECRET" = "true" ] && command -v kubectl >/dev/null 2>&1; then
    secret_args=()
    if [ -n "${LINEAR_API_KEY:-}" ]; then
        secret_args+=("--from-literal=LINEAR_API_KEY=$LINEAR_API_KEY")
    fi
    if [ -n "${WORKFLOW_GITHUB_TOKEN:-}" ]; then
        secret_args+=("--from-literal=WORKFLOW_GITHUB_TOKEN=$WORKFLOW_GITHUB_TOKEN")
    fi
    if [ -n "${REVIEWER_GITHUB_TOKEN:-}" ]; then
        secret_args+=("--from-literal=REVIEWER_GITHUB_TOKEN=$REVIEWER_GITHUB_TOKEN")
    fi

    if [ "${#secret_args[@]}" -gt 0 ]; then
        kubectl create secret generic linear-intake-credentials \
            --namespace "$NAMESPACE" \
            "${secret_args[@]}" \
            --dry-run=client -o yaml | kubectl apply -f -
    fi
fi

if ! command -v kubectl >/dev/null 2>&1; then
    echo "kubectl is required to apply the job (set DRY_RUN=1 to render the manifest)" >&2
    exit 1
fi

render_template "$TEMPLATE" | kubectl apply -f -
