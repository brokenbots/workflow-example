#!/usr/bin/env bash
# check_routes_stale.sh — KB-313 (ADR 307) global run-config staleness gate.
#
# The routes CM is a thin catalog: workflow trees + interface refs +
# projectRegistry + route rules. The four global run-config surfaces on
# workflowLibrary entries (volumes, secrets, env, adapterImages) are
# deprecated: a POST-CUTOVER payload (projectRegistry non-empty, every entry
# cutover: done) carrying them is a CI error — the surfaces died with the
# binding cutover (KB-311). While the migration window is open (any entry
# un-cut, or no projectRegistry at all — pre-KB-311 payloads) the surfaces
# are allowed-with-warning; criteria-k8s/internal/routes carries the matching
# two-revision Go gate.
#
# Usage:
#   check_routes_stale.sh [input] [label]
#     input        — a routes.json payload file, or a ConfigMap manifest
#                    carrying routes.json as its sole data key ('-' = stdin,
#                    no input = the shipped k8s/examples/routes-configmap.yaml).
#     label        — human-readable name for report lines.
#
# Exit codes:
#   0  clean, or migration-window stale (warning note on stdout)
#   1  post-cutover stale payload, invalid JSON, missing workflowLibrary, or
#      an empty/missing input (drift must fail loudly)
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
EXAMPLE="$REPO_ROOT/k8s/examples/routes-configmap.yaml"

payload_file=""
label="routes payload"
tmp_made=""
extracted_made=""
report=""
TMP=""
EXTRACTED=""

case "${1:-}" in
    "") ;; # default: shipped example
    -) payload_file="-" ;;
    *) payload_file="$1" ;;
esac
[ -n "${2:-}" ] && label="$2"

fail() { echo "check_routes_stale FAIL: $*" >&2; exit 1; }
cleanup() {
    [ -n "$tmp_made" ] && rm -f "$TMP"
    [ -n "$extracted_made" ] && rm -f "$EXTRACTED"
    [ -n "$report" ] && rm -f "$report"
    return 0
}
trap cleanup EXIT

# Extract the routes.json data key from a ConfigMap manifest into $2.
extract_from_manifest() {
    awk -v key="routes.json" '
        $0 ~ "^[ ]*" key ": \\\|$" { inblock = 1; next }
        inblock && (/^[ \t]/ || $0 == "") { sub(/^[ \t]*/, "", $0); print; next }
        inblock { exit }
    ' "$1" > "$2"
}

command -v jq >/dev/null 2>&1 || fail "jq is required"

if [ -z "$payload_file" ]; then
    TMP="$(mktemp)" && tmp_made=1
    extract_from_manifest "$EXAMPLE" "$TMP"
    label="k8s/examples/routes-configmap.yaml"
elif [ "$payload_file" = "-" ]; then
    TMP="$(mktemp)" && tmp_made=1
    cat > "$TMP"
else
    TMP="$payload_file"
fi
[ -s "$TMP" ] || fail "$label: could not extract a routes.json payload"

# Raw routes.json or a manifest carrying routes.json — manifests are extracted.
if ! jq -e . "$TMP" >/dev/null 2>&1; then
    EXTRACTED="$(mktemp)" && extracted_made=1
    extract_from_manifest "$TMP" "$EXTRACTED"
    TMP="$EXTRACTED"
fi
jq -e . "$TMP" >/dev/null 2>&1 || fail "$label: routes.json is not valid JSON"

# workflowLibrary is required; a missing projectRegistry is a pre-KB-311
# payload — the Go gate treats it as the migration window open, and so does
# this check: no registry means no cutover to judge the payload by.
jq -e '.workflowLibrary?' "$TMP" >/dev/null 2>&1 \
    || fail "$label: routes.json does not carry workflowLibrary"

report="$(mktemp)"
jq -c '
    {
        preRegistry: (.projectRegistry? == null),
        postCutover: (((.projectRegistry // {}) | length > 0)
            and ([(.projectRegistry // {})[]
                | select((.cutover // "") != "done")] | length == 0)),
        stale: [(.workflowLibrary // {}) | to_entries[]
            | select(.value | has("adapterImages") or has("volumes")
                or has("secrets") or has("env")) | .key]
    }
' "$TMP" > "$report"

stale="$(jq -r '.stale | join(", ")' "$report")"
if jq -e '.postCutover and (.stale | length > 0)' "$report" >/dev/null; then
    fail "$label is post-cutover but workflowLibrary still carries global run-config (binding cutovers done, so delete volumes/secrets/env/adapterImages — KB-313): $stale"
elif jq -e '.preRegistry' "$report" >/dev/null; then
    echo "check_routes_stale: $label has no projectRegistry (pre-KB-311 payload) — migration window open; stale surfaces allowed-with-warning on: $stale"
elif jq -e '.stale | length > 0' "$report" >/dev/null; then
    echo "check_routes_stale: $label migration window open — stale global run-config surfaces allowed-with-warning on: $stale"
else
    echo "check_routes_stale: $label carries no stale global run-config surfaces"
fi