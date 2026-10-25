#!/usr/bin/env bash
# KB-328 gap-4 durable fix (2026-10-25): wait_merge_ci verdicts on the repo's
# CANONICAL push CI only (actions-runs API, event=push), never on check-runs
# rollups — Dependabot's dynamic graph-update runs carry repo identity
# markers but are not CI (observed: one failed dynamic run turned the gate
# red while canonical CI was green; 14-pending mass false-timed the budget).
# Hermetic: mocked gh api; bare-sh compatible.
set -u
TREE_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TPL="$TREE_ROOT/scripts/wait_merge_ci.sh.tftpl"
WRAP="$TREE_ROOT/scripts/gh_retry.sh.tftpl"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

fail() { echo "FAIL: $1"; exit 1; }
pass() { echo "PASS: $1"; }

SHA='8909c4097948685230f0793c92d266b3c78d2acf'

# Mock gh: gh_retry calls `gh api <url>` / `gh pr view` / `gh repo view`.
cat > "$TMP/gh" <<'MOCK'
#!/bin/sh
case "$*" in
    *"actions/runs?head_sha="*)
        # Payload chosen by MOCK_RUNS_FILE so scenarios can vary the SHA's runs.
        cat "${MOCK_RUNS_FILE:?}" ;;
    *"actions/workflows"*)
        printf '{"total_count":5}' ;;
    *"commits/$SHA"*)
        # sha-refresh path: unused here (SHA preset) but answer sanely
        printf '{"mergeCommit":{"oid":"'"$SHA"'"}}' ;;
    *"repo view"*)
        printf '{"owner":{"login":"brokenbots"},"name":"criteria"}' ;;
    *) exit 64 ;;
esac
MOCK
chmod +x "$TMP/gh"
printf '#!/bin/sh\n"$@"\n' > "$TMP/gh_retry"; chmod +x "$TMP/gh_retry"

# Render: splice the wrapper at its marker (as the engine does), substitute
# the slot, keep budget tiny for fast timeouts.
gh_retry_line="$(grep -n '{{ *\.gh_retry *}}' "$TPL" | head -1 | cut -d: -f1)"
cat > "$TMP/slots.awk" <<'AWK'
index($0, "criteria_value_1=") == 1 && index($0, "{{") > 0 { sub(/=.*/, "=\"$1\""); print; next }
index($0, "max_seconds=") == 1 && index($0, "{{") > 0 { sub(/={{ *\.merge_ci_max *}}/, "=600"); print; next }
{ print }
AWK
{
    head -n $((gh_retry_line - 1)) "$TPL"
    cat "$WRAP"
    tail -n "+$((gh_retry_line + 1))" "$TPL"
} | awk -f "$TMP/slots.awk" > "$TMP/rendered.sh"
chmod +x "$TMP/rendered.sh"

run_gate() {
    # $1 = runs payload file; output captured; budget 2s, cadence 0.
    MOCK_RUNS_FILE="$1" MERGE_CI_MAX_SECONDS=2 MERGE_CI_POLL_SECONDS=0 \
        PATH="$TMP:$PATH" bash "$TMP/rendered.sh" "pr=543
merge_sha=$SHA"
}

# Fixture A: canonical push CI pending + dependabot dynamic runs (the poison).
cat > "$TMP/runs_A.json" <<'JSON'
{"total_count":9,"workflow_runs":[
 {"name":"CI","event":"push","status":"in_progress","conclusion":null,"created_at":"2026-10-24T23:23:10Z"},
 {"name":"Code Quality: Push on main","event":"push","status":"completed","conclusion":"success","created_at":"2026-10-24T23:23:05Z"},
 {"name":"graph commit for go_modules","event":"dynamic","status":"completed","conclusion":"success","created_at":"2026-10-24T23:24:00Z"},
 {"name":"Dependabot","event":"dynamic","status":"completed","conclusion":"failure","created_at":"2026-10-24T23:24:10Z"}
]}
JSON

# Fixture B: canonical push CI green + failed dynamic (the live false-red).
cat > "$TMP/runs_B.json" <<'JSON'
{"total_count":9,"workflow_runs":[
 {"name":"CI","event":"push","status":"completed","conclusion":"success","created_at":"2026-10-24T23:23:10Z"},
 {"name":"Code Quality: Push on main","event":"push","status":"completed","conclusion":"success","created_at":"2026-10-24T23:23:05Z"},
 {"name":"Dependabot","event":"dynamic","status":"completed","conclusion":"failure","created_at":"2026-10-24T23:24:10Z"},
 {"name":"graph commit for go_modules","event":"dynamic","status":"completed","conclusion":"success","created_at":"2026-10-24T23:24:00Z"}
]}
JSON

# Fixture C: canonical push RED (real failure must fail the gate).
cat > "$TMP/runs_C.json" <<'JSON'
{"total_count":9,"workflow_runs":[
 {"name":"CI","event":"push","status":"completed","conclusion":"failure","created_at":"2026-10-24T23:23:10Z"},
 {"name":"Code Quality: Push on main","event":"push","status":"completed","conclusion":"success","created_at":"2026-10-24T23:23:05Z"}
]}
JSON

# Fixture D: dynamic-only presence (canonical has not registered yet).
cat > "$TMP/runs_D.json" <<'JSON'
{"total_count":2,"workflow_runs":[
 {"name":"Dependabot","event":"dynamic","status":"completed","conclusion":"failure","created_at":"2026-10-24T23:24:10Z"}
]}
JSON

# Fixture E: zero push runs, repo HAS workflows -> timeout (budget 2s).
cat > "$TMP/runs_E.json" <<'JSON'
{"total_count":0,"workflow_runs":[]}
JSON

# A: pending canonical + failing dynamic -> NOT red, NOT green: timeout verdict
run_gate "$TMP/runs_A.json" > "$TMP/out_A" 2>/dev/null
head -1 "$TMP/out_A" | grep -q '^merge_ci=timeout$' || fail "A: pending canonical + failed dynamic must never verdict red/green early: got '$(head -1 "$TMP/out_A")'"
pass "A: dynamic runs cannot force a verdict while canonical is pending (timeout on the tiny budget)"

# B: green canonical + failed dynamic -> GREEN (the live false-red, fixed)
run_gate "$TMP/runs_B.json" > "$TMP/out_B" 2>/dev/null
head -1 "$TMP/out_B" | grep -q '^merge_ci=green$' || fail "B: canonical green must be green despite dynamic failure: got '$(head -1 "$TMP/out_B")'"
pass "B: canonical push CI green -> green (dependabot dynamic failure ignored)"

# C: canonical red -> failed with the canonical name in the evidence
run_gate "$TMP/runs_C.json" > "$TMP/out_C" 2>/dev/null
head -1 "$TMP/out_C" | grep -q '^merge_ci=failed$' || fail "C: canonical red must fail the gate: got '$(head -1 "$TMP/out_C")'"
grep -q 'failed_check=CI=failure' "$TMP/out_C" || fail "C: failed evidence must name the canonical run"
pass "C: canonical push CI red -> failed with the run's own name"

# D: dynamic-only -> keep polling to timeout, never red
run_gate "$TMP/runs_D.json" > "$TMP/out_D" 2>/dev/null
head -1 "$TMP/out_D" | grep -q '^merge_ci=timeout$' || fail "D: dynamic-only sha must poll (timeout), never fail: got '$(head -1 "$TMP/out_D")'"
pass "D: dynamic-only presence = not-yet-CI (kept polling to the bounded timeout)"

# E: dynamic-only with zero canonical + workflows exist -> same timeout path
run_gate "$TMP/runs_E.json" > "$TMP/out_E" 2>/dev/null
head -1 "$TMP/out_E" | grep -q '^merge_ci=timeout$' || fail "E: no-runs sha must poll to timeout: got '$(head -1 "$TMP/out_E")'"
pass "E: empty runs + registered workflows -> bounded timeout (CRI-90 zero-workflows green is a repo-level case)"

echo "all wait_merge_ci canonical-CI tests green"