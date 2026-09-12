#!/usr/bin/env bash
set -euo pipefail

# Regression test for the CRI-136 debug-only events-file default: the runner
# must not pass --events-file (or derive an events.ndjson path) unless an
# explicit EVENTS_FILE is set. The operator-side gating is covered by
# TestBuildRunnerJobEventsFileDebugOnly in criteria-k8s; this test pins the
# script/manifest layer that actually decides whether events.ndjson is
# written, so the retired dual-write cannot quietly come back.
#
# Checks are comment-insensitive: the runner legitimately mentions
# events.ndjson in explanatory comments, so code assertions run on
# comment-stripped copies.

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

RUNNER_CANONICAL="$REPO_ROOT/k8s/pod-adapter-runner.sh"
RUNNER_CHART="$REPO_ROOT/charts/criteria-k8s/scripts/runner.sh"
PREREQS="$REPO_ROOT/k8s/operator-prereqs.yaml"
TMPL="$REPO_ROOT/k8s/job-cri-27.yaml.tmpl"
EXAMPLE="$REPO_ROOT/k8s/examples/ticket-job.yaml"
LAUNCHER="$REPO_ROOT/k8s/launch-pod-adapter-job.sh"

fail() {
    echo "FAIL: $1" >&2
    exit 1
}

strip_comments() {
    sed 's/#.*$//'
}

for f in "$RUNNER_CANONICAL" "$RUNNER_CHART"; do
    [ -f "$f" ] || fail "missing runner script: $f"
done

# The canonical runner and its generated chart copy must stay in sync
# (k8s/generate-pod-adapter-manifest.sh regenerates the chart copy).
cmp -s "$RUNNER_CANONICAL" "$RUNNER_CHART" || \
    fail "charts/criteria-k8s/scripts/runner.sh drifted from k8s/pod-adapter-runner.sh (run k8s/generate-pod-adapter-manifest.sh)"

# Source-level guard on the runner scripts: no default events.ndjson path may
# be derived, and --events-file may only be passed inside the
# [ -n "$EVENTS_FILE" ] guard using the caller-supplied value verbatim.
for f in "$RUNNER_CANONICAL" "$RUNNER_CHART"; do
    code="$(strip_comments < "$f")"

    if grep -qF 'events.ndjson' <<<"$code"; then
        fail "$f references events.ndjson in code (only comments may mention it)"
    fi

    count=$(grep -cF -- '--events-file' <<<"$code" || true)
    [ "$count" -eq 1 ] || fail "$f passes --events-file $count times (expected exactly once, inside the EVENTS_FILE guard)"

    guard_block="$(awk '/if \[ -n "\$EVENTS_FILE" \]; then/{flag=1} flag{print} flag && /^fi$/{exit}' <<<"$code")"
    [ -n "$guard_block" ] || fail "$f is missing the 'if [ -n \"\$EVENTS_FILE\" ]; then' guard"
    grep -qF -- '--events-file' <<<"$guard_block" || \
        fail "$f passes --events-file outside the [ -n \"\$EVENTS_FILE\" ] guard"
    events_line=$(grep -F -- '--events-file' <<<"$code")
    grep -qF 'EVENTS_FILE' <<<"$events_line" || \
        fail "$f must pass the caller-supplied \$EVENTS_FILE verbatim to --events-file"
done

# Static manifests: no events.ndjson outside comments.
for f in "$PREREQS" "$TMPL" "$EXAMPLE"; do
    [ -f "$f" ] || fail "missing manifest: $f"
    if strip_comments < "$f" | grep -qF 'events.ndjson'; then
        fail "$f references events.ndjson outside comments"
    fi
done

# The example manifest must ship an empty default EVENTS_FILE.
example_value=$(grep -A1 'name: EVENTS_FILE' "$EXAMPLE" | sed -n 's/^[[:space:]]*value: //p' || true)
[ "$example_value" = '""' ] || fail "examples/ticket-job.yaml EVENTS_FILE default is not empty: '$example_value'"

# The rendered job manifest (launcher DRY_RUN, default env) must carry an
# empty default EVENTS_FILE and no events.ndjson in code lines. EVENTS_FILE is
# explicitly cleared so the assertion covers the default, not any ambient
# debug value exported by the calling shell.
manifest="$(mktemp)"
manifest_code="$(mktemp)"
trap 'rm -f "$manifest" "$manifest_code"' EXIT
env -u EVENTS_FILE TICKET_ID=CRI-136 REPO_URL=brokenbots/workflow-example DRY_RUN=1 \
    "$LAUNCHER" > "$manifest"

if ! grep -qF 'name: EVENTS_FILE' "$manifest"; then
    fail "rendered job manifest lost the EVENTS_FILE env wiring"
fi
manifest_value=$(grep -A1 'name: EVENTS_FILE' "$manifest" | sed -n 's/^[[:space:]]*value: //p' || true)
[ "$manifest_value" = '""' ] || fail "rendered job manifest EVENTS_FILE default is not empty: '$manifest_value'"

strip_comments < "$manifest" > "$manifest_code"
if grep -qF 'events.ndjson' "$manifest_code"; then
    fail "rendered job manifest writes events.ndjson by default"
fi

echo "PASS: runner events-file is debug-only (no default events.ndjson anywhere in script or manifests)"