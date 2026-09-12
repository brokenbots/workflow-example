#!/usr/bin/env bash
set -euo pipefail

# Regression test for CRI-140: the runner must publish per-run adapter digest
# files keyed by adapter INSTANCE name (digest-intake) in addition to adapter
# kind (digest-shell/digest-copilot), and a per-scope adapter pod must resolve
# its pinned digest from the instance-keyed file. Before the fix a per-scope
# pod for instance "intake" polled digest-intake forever (or resolved the
# wrong image) and the shim handshake never completed.
#
# Part 1 exercises the runner's lockfile parsing against the real workflow
# lockfile plus a synthetic multi-entry lockfile. Part 2 runs pod-adapter-
# adapter.sh end-to-end with a stubbed criteria-adapter-remote-runner binary
# and a seeded discovery directory, asserting the resolved connection env and
# that the poll loop terminates.

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
RUNNER="$REPO_ROOT/k8s/pod-adapter-runner.sh"
ADAPTER="$REPO_ROOT/k8s/pod-adapter-adapter.sh"
LOCKFILE="$REPO_ROOT/linear_intake_v1/.criteria.lock.hcl"

fail() {
    echo "FAIL: $1" >&2
    exit 1
}

[ -f "$LOCKFILE" ] || fail "workflow lockfile is missing"
[ -f "$RUNNER" ] || fail "k8s/pod-adapter-runner.sh is missing"
[ -f "$ADAPTER" ] || fail "k8s/pod-adapter-adapter.sh is missing"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# ---------------------------------------------------------------- part 1
# Extract the runner's digest-resolution functions and publish digests into a
# temp discovery directory, exactly as the runner script does at startup.

eval "$(awk '/^adapter_digest\(\) \{/,/^\}/' "$RUNNER")" || fail "cannot extract adapter_digest from the runner script"
eval "$(awk '/^adapter_entry_digests\(\) \{/,/^\}/' "$RUNNER")" || fail "cannot extract adapter_entry_digests from the runner script"
declare -f adapter_digest > /dev/null || fail "adapter_digest extraction from k8s/pod-adapter-runner.sh failed"
declare -f adapter_entry_digests > /dev/null || fail "adapter_entry_digests extraction from k8s/pod-adapter-runner.sh failed"

lockfile="$LOCKFILE"
run_dir="$tmp/runs/cri-140"
mkdir -p "$run_dir"

for kind in shell copilot; do
    printf 'sha256:%s' "$(adapter_digest "$kind")" > "$run_dir/digest-$kind"
done
adapter_entry_digests | while read -r entry_instance entry_digest; do
    printf 'sha256:%s' "$entry_digest" > "$run_dir/digest-$entry_instance"
done

digest_of() {
    [ -f "$run_dir/digest-$1" ] || fail "discovery directory is missing digest-$1"
    cat "$run_dir/digest-$1"
}

# Independent oracle: the resolved digest of one specific lockfile entry.
entry_digest() {
    awk -v kind="$1" -v instance="$2" '
        $1 == "adapter" && $2 == "\""kind"\"" && $3 == "\""instance"\"" { on = 1; next }
        on && /^[[:space:]]*}/ { exit }
        on && /resolved_digest/ {
            gsub(/[ "]/, "")
            sub(/^resolved_digest=sha256:/, "")
            print
            exit
        }
    ' "$lockfile"
}

expect_intake="sha256:$(entry_digest shell intake)"
expect_copilot="sha256:$(entry_digest copilot intake_classifier)"
[ -n "$expect_intake" ] || fail "oracle could not read the shell/intake entry from the lockfile"
[ -n "$expect_copilot" ] || fail "oracle could not read the copilot/intake_classifier entry from the lockfile"

[ "$(digest_of intake)" = "$expect_intake" ] || fail "digest-intake must be published keyed by adapter instance (CRI-140)"
[ "$(digest_of shell)" = "$expect_intake" ] || fail "digest-shell must keep the kind-keyed shell digest"
[ "$(digest_of copilot)" = "$expect_copilot" ] || fail "digest-copilot must keep the kind-keyed copilot digest"
[ -f "$run_dir/digest-intake_classifier" ] || fail "missing instance-keyed digest for intake_classifier"
[ -f "$run_dir/digest-triage_reviewer" ] || fail "missing instance-keyed digest for triage_reviewer"

[ -z "$(adapter_digest nonexistentkind 2> /dev/null)" ] || \
    fail "adapter_digest must not resolve an unknown kind"

# A lockfile may pin several instances of one kind to different versions:
# instance keying disambiguates, kind keying keeps the first entry.
lockfile="$tmp/two-shell.hcl"
cat > "$lockfile" <<'EOF'
schema_version = 1
adapter "shell" "alpha" {
  reference            = "ghcr.io/brokenbots/criteria-adapter-shell:0.5.3"
  version              = "0.5.3"
  resolved_digest      = "sha256:aaaa"
}
adapter "shell" "beta" {
  reference            = "ghcr.io/brokenbots/criteria-adapter-shell:0.5.4"
  version              = "0.5.4"
  resolved_digest      = "sha256:bbbb"
}
adapter "copilot" "gamma" {
  reference            = "ghcr.io/brokenbots/criteria-adapter-copilot:0.5.6"
  version              = "0.5.6"
  resolved_digest      = "sha256:cccc"
}
EOF

out="$(adapter_entry_digests)"
printf '%s' "$out" | grep -qx 'alpha aaaa' || fail "instance digest resolution lost shell/alpha"
printf '%s' "$out" | grep -qx 'beta bbbb' || fail "instance digest resolution lost shell/beta (same-kind instances must not collide)"
printf '%s' "$out" | grep -qx 'gamma cccc' || fail "instance digest resolution lost copilot/gamma"
[ "$(adapter_digest shell)" = "aaaa" ] || fail "kind-keyed digest lookup must keep returning the first pinned entry"

# ---------------------------------------------------------------- part 2
stubbin="$tmp/bin"
mkdir -p "$stubbin"
cat > "$stubbin/criteria-adapter-remote-runner" <<'EOF'
#!/usr/bin/env sh
env | grep '^CRITERIA_' | sort > "$CRITERIA_STUB_OUT"
EOF
chmod +x "$stubbin/criteria-adapter-remote-runner"

printf '10.0.0.9:7778' > "$run_dir/host"
printf 'per-run-token' > "$run_dir/token"
# Distinct from the kind digest so the assertion proves the instance-keyed
# file was used (the real lockfile currently pins intake and shell to the
# same digest, which would make the resolution indistinguishable).
printf 'sha256:intake-instance-pin' > "$run_dir/digest-intake"

run_adapter() {
    local out="$1"
    shift
    # A 20s timeout turns the CRI-139 wedge symptom (a poll_file loop that
    # never terminates) into a hard failure.
    env -i PATH="$stubbin:/usr/bin:/bin" HOME="$tmp" CRITERIA_STUB_OUT="$out" "$@" \
        timeout 20 "$ADAPTER" || fail "pod-adapter-adapter.sh did not terminate within 20s (poll loop wedged)"
    [ -s "$out" ] || fail "pod-adapter-adapter.sh never exec'd the adapter binary"
}

# The CRI-140 per-scope intake pod: kind shell, instance intake, no direct
# env for host/token/digest — everything resolves from the discovery files,
# and the digest comes from the instance-keyed file.
run_adapter "$tmp/adapter-env" \
    ADAPTER_KIND=shell \
    CRITERIA_ADAPTER_NAME=intake \
    CRITERIA_RUN_JOB_NAME=cri-140 \
    CRITERIA_RUN_DIR_ROOT="$tmp/runs"
resolved="$(cat "$tmp/adapter-env")"
printf '%s' "$resolved" | grep -qx 'CRITERIA_REMOTE_HOST=10.0.0.9:7778' || \
    fail "adapter.sh must dial the discovery host file (POD_IP:7778), never the shim bind address"
printf '%s' "$resolved" | grep -qx 'CRITERIA_REMOTE_TOKEN=per-run-token' || \
    fail "adapter.sh must resolve the per-run bearer token from discovery"
printf '%s' "$resolved" | grep -qx 'CRITERIA_REMOTE_DIGEST=sha256:intake-instance-pin' || \
    fail "adapter.sh must resolve the instance-keyed digest-intake over the kind-keyed file"

# An instance with no instance-keyed file falls back to the kind digest, so
# the poll loop always terminates.
run_adapter "$tmp/adapter-env-kind" \
    ADAPTER_KIND=shell \
    CRITERIA_ADAPTER_NAME=unknown-instance \
    CRITERIA_RUN_JOB_NAME=cri-140 \
    CRITERIA_RUN_DIR_ROOT="$tmp/runs"
printf '%s' "$(cat "$tmp/adapter-env-kind")" | grep -qx "$(digest_of shell | sed 's/^/CRITERIA_REMOTE_DIGEST=/')" || \
    fail "adapter.sh must fall back to the kind-keyed digest when the instance has no lockfile entry"

# Without an instance name (run-duration adapter Jobs) the behavior is
# unchanged.
run_adapter "$tmp/adapter-env-legacy" \
    ADAPTER_KIND=copilot \
    CRITERIA_RUN_JOB_NAME=cri-140 \
    CRITERIA_RUN_DIR_ROOT="$tmp/runs"
printf '%s' "$(cat "$tmp/adapter-env-legacy")" | grep -qx "CRITERIA_REMOTE_DIGEST=$expect_copilot" || \
    fail "adapter.sh without CRITERIA_ADAPTER_NAME must keep the kind-keyed digest fallback"

echo "PASS"