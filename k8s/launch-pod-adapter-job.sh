#!/usr/bin/env bash
set -euo pipefail

# Launch a Kubernetes Job that runs linear_intake_v1 with separate in-pod
# adapter sidecars. Secrets are mounted via CSI SecretProviderClass only; no
# container uses secret environment variables.
#
# Environment variables:
#   IMAGE                     Container image to run (default: localhost:5000/linear-intake-remote:dev)
#   NAMESPACE                 Target namespace (default: criteria-jobs)
#   JOB_NAME                  Explicit Job name; defaults to pod-adapter-<lowercase ticket>
#   DATA_PVC                  PVC for /data (default: criteria-data)
#
#   TICKET_ID                 Required Linear ticket identifier
#   REPO_URL                  Required GitHub repository (e.g. brokenbots/criteria)
#
#   All workflow settings use the same defaults as container-entrypoint.sh.

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TEMPLATE="$REPO_ROOT/k8s/job-cri-27.yaml"

tmpl_var() {
    local name="$1"
    local value="$2"
    export "__${name}__=$value"
}

tmpl_var IMAGE           "${IMAGE:-localhost:5000/linear-intake-remote:dev}"
tmpl_var DIGEST          "${DIGEST:-latest}"
tmpl_var NAMESPACE       "${NAMESPACE:-criteria-jobs}"
tmpl_var DATA_PVC        "${DATA_PVC:-criteria-data}"

: "${TICKET_ID:?TICKET_ID is required}"
: "${REPO_URL:?REPO_URL is required}"
tmpl_var TICKET_ID       "$TICKET_ID"
tmpl_var REPO_URL        "$REPO_URL"

DEFAULT_JOB_NAME="pod-adapter-$(printf '%s' "$TICKET_ID" | tr '[:upper:]' '[:lower:]')"
tmpl_var JOB_NAME        "${JOB_NAME:-$DEFAULT_JOB_NAME}"

tmpl_var LINEAR_REVIEW_STATE  "${LINEAR_REVIEW_STATE:-In Review}"
tmpl_var LINEAR_TRIAGE_STATE  "${LINEAR_TRIAGE_STATE:-Triage}"
tmpl_var LINEAR_WORK_STATE    "${LINEAR_WORK_STATE:-In Progress}"
tmpl_var LINEAR_DONE_STATE    "${LINEAR_DONE_STATE:-Done}"
tmpl_var BASE_BRANCH          "${BASE_BRANCH:-main}"
tmpl_var BUILD_CMD            "${BUILD_CMD:-}"
tmpl_var TEST_CMD             "${TEST_CMD:-}"
tmpl_var CI_GATE_CMD          "${CI_GATE_CMD:-}"
tmpl_var TEST_REFS            "${TEST_REFS:-both}"
tmpl_var MAIN_REF             "${MAIN_REF:-origin/main}"
tmpl_var STABLE_REF           "${STABLE_REF:-}"
tmpl_var DESIGN_INTENT_FILE   "${DESIGN_INTENT_FILE:-}"
tmpl_var REPRO_WORKFLOW_DIR   "${REPRO_WORKFLOW_DIR:-}"
tmpl_var ALLOW_DIRTY          "${ALLOW_DIRTY:-false}"
tmpl_var MAX_AGENT_VISITS     "${MAX_AGENT_VISITS:-2}"
tmpl_var PROVIDER_BASE_URL    "${PROVIDER_BASE_URL:-http://192.168.17.116:11434/v1}"
tmpl_var EVENTS_FILE          "${EVENTS_FILE:-}"

tmpl_var REPO_DIR        "${REPO_DIR:-/repo}"
tmpl_var INTAKE_ROOT     "${INTAKE_ROOT:-/data/intake}"
tmpl_var TRIAGE_ROOT     "${TRIAGE_ROOT:-/data/triage}"

render_template() {
    perl -pe 's/__([A-Z_][A-Z0-9_]*)__/exists $ENV{"__$1__"} ? $ENV{"__$1__"} : die "template placeholder $1 is missing\n"/ge' "$1"
}

if [ -n "${DRY_RUN:-}" ]; then
    render_template "$TEMPLATE"
    exit 0
fi

if ! command -v kubectl >/dev/null 2>&1; then
    echo "kubectl is required to apply the job (set DRY_RUN=1 to render the manifest)" >&2
    exit 1
fi

render_template "$TEMPLATE" | kubectl apply -f -
