#!/bin/sh
# Entrypoint for the minimal criteria base image (CRI-230, ADR-0005 D1/D3/D4).
#
# Fetches the CriteriaRun workflow source through the merged fetcher (git or
# archive URL) and applies it from the CRITERIA_HOME cache with fail-closed
# pin enforcement (CRI-226). Expected behavior:
#
#   WORKFLOW_URL   required. The workflow source declared on the CriteriaRun
#                  (git::<repo>?ref=..., https://.../workflow.tar.gz, .zip).
#                  Empty or whitespace-only fails closed: never fall back to a
#                  baked tree, the image ships none (D2 declared-not-inferred).
#   WORKFLOW_REF   optional. The route's expected ref (git commit SHA or
#                  sha256:<digest>); passed to `criteria apply --workflow-ref`
#                  verbatim so the CRI-226 machinery enforces it before the run
#                  executes. Empty or whitespace-only means "no pin declared".
#   Remaining argv is forwarded to `criteria apply` (--var, --var-file,
#   --server, --events-file, ...).
#
# CRITERIA_HOME defaults to /data/criteria (the data PVC) and must be writable
# by the runtime uid; the workflow cache and its index.json live there.

set -eu

criteria_bin="${CRITERIA_BIN:-/usr/local/bin/criteria}"

fail() {
    echo "criteria-base-entrypoint: $1" >&2
    exit "$2"
}

# Fail closed on an undeclared workflow source: this image ships no baked
# /workflows tree, so there is nothing to run without one.
workflow_url="${WORKFLOW_URL-}"
if [ "$(printf '%s' "$workflow_url" | tr -d '[:space:]')" = "" ]; then
    fail "WORKFLOW_URL is not set: declare the CriteriaRun workflow source (git or archive URL); this image has no baked /workflows tree" 64
fi

# CRITERIA_HOME must be creatable and writable by the runtime uid; the
# fetcher caches the fetched tree and its index.json there. On a fresh data
# PVC mount the directory does not exist yet, so create it before checking.
criteria_home="${CRITERIA_HOME:-/data/criteria}"
export CRITERIA_HOME="$criteria_home"
if ! mkdir -p "$criteria_home" 2>/dev/null || [ ! -w "$criteria_home" ]; then
    fail "CRITERIA_HOME '$criteria_home' is not a directory writable by uid $(id -u): mount the data PVC at /data" 70
fi

set -- apply "$workflow_url" "$@"

workflow_ref="${WORKFLOW_REF-}"
if [ "$(printf '%s' "$workflow_ref" | tr -d '[:space:]')" != "" ]; then
    # CRI-226: the caller-declared pin is enforced by criteria itself at
    # resolve time; a mismatch fails closed before any step runs.
    set -- "$@" --workflow-ref "$workflow_ref"
fi

exec "$criteria_bin" "$@"