#!/usr/bin/env bash
set -euo pipefail

# Regression test for CRI-140: the runner must publish per-run adapter digest
# files keyed by adapter INSTANCE name (digest-triage) in addition to adapter
# kind (digest-shell/digest-copilot), and a per-scope adapter pod must resolve
# its pinned digest from the instance-keyed file. Before the fix a per-scope
# pod for instance "triage" polled digest-triage forever (or resolved the
# wrong image) and the shim handshake never completed.
#
# Part 1 exercises the runner's lockfile parsing against the real workflow
# lockfiles (the both-kinds qa_triage_v1 exemplar and the shell-only
# kanboard_triage_v1 triage tree) plus a synthetic multi-entry lockfile.
# Part 2 runs pod-adapter-adapter.sh end-to-end with a stubbed
# criteria-adapter-remote-runner binary and a seeded discovery directory,
# asserting the resolved connection env and that the poll loop terminates.

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
RUNNER="$REPO_ROOT/k8s/pod-adapter-runner.sh"
ADAPTER="$REPO_ROOT/k8s/pod-adapter-adapter.sh"
TRIAGE_LOCKFILE="$REPO_ROOT/kanboard_triage_v1/.criteria.lock.hcl"
EXEMPLAR_LOCKFILE="$REPO_ROOT/qa_triage_v1/.criteria.lock.hcl"

fail() {
    echo "FAIL: $1" >&2
    exit 1
}

[ -f "$TRIAGE_LOCKFILE" ] || fail "kanboard_triage_v1 workflow lockfile is missing"
[ -f "$EXEMPLAR_LOCKFILE" ] || fail "qa_triage_v1 workflow lockfile is missing"
[ -f "$RUNNER" ] || fail "k8s/pod-adapter-runner.sh is missing"
[ -f "$ADAPTER" ] || fail "k8s/pod-adapter-adapter.sh is missing"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# ---------------------------------------------------------------- part 1
# Extract the runner's digest-resolution functions and its actual digest-
# publication statements, then execute the runner's own block against the
# real workflow lockfile. The publication block is eval'd verbatim — never
# re-typed here — so a regression in k8s/pod-adapter-runner.sh's publication
# (a deleted or renamed instance-keyed write, or a deleted kind-keyed loop)
# fails this test instead of passing while per-scope pods wedge.

eval "$(awk '/^adapter_digest\(\) \{/,/^\}/' "$RUNNER")" || fail "cannot extract adapter_digest from the runner script"
eval "$(awk '/^adapter_entry_digests\(\) \{/,/^\}/' "$RUNNER")" || fail "cannot extract adapter_entry_digests from the runner script"
declare -f adapter_digest > /dev/null || fail "adapter_digest extraction from k8s/pod-adapter-runner.sh failed"
declare -f adapter_entry_digests > /dev/null || fail "adapter_entry_digests extraction from k8s/pod-adapter-runner.sh failed"

# The runner's publication call site, extracted from the "Publish the
# digests keyed by kind" comment through the instance-keyed loop's closing
# "done". The guards below make marker drift fail loudly instead of
# silently eval'ing a truncated block.
publication_block="$(awk '
    /^# Publish the digests keyed by kind/ { f = 1 }
    /^# Publish the runner dial address/    { f = 0 }
    f { print }
' "$RUNNER")"
printf '%s\n' "$publication_block" | grep -q '^for kind in shell copilot; do$' || \
    fail "cannot extract the kind-keyed publication loop from k8s/pod-adapter-runner.sh"
printf '%s\n' "$publication_block" | grep -q '^adapter_entry_digests | while read -r entry_instance entry_digest; do$' || \
    fail "cannot extract the instance-keyed publication loop from k8s/pod-adapter-runner.sh"
[ "$(printf '%s\n' "$publication_block" | grep -c '^done$')" -eq 2 ] || \
    fail "digest publication block extraction is truncated (expected both loops closed)"

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

# kanboard_triage_v1 keeps only its shell/triage pin now: the QA-output
# reviewer copilot moved to review_triage_v1 (KB-284). The runner's copilot
# kind leg still fires against this shell-only lockfile, emitting its
# expected "criteria adapter copilot not found" stderr and an empty
# digest-copilot that the both-kinds pass below repins.
# Both published passes share one run dir because the triage and exemplar
# trees pin the identical shell adapter digest - assert that pairing instead
# of just claiming it.
[ "sha256:$(lockfile="$TRIAGE_LOCKFILE" entry_digest shell triage)" = "sha256:$(lockfile="$EXEMPLAR_LOCKFILE" entry_digest shell repo)" ] || \
    fail "triage and exemplar lockfiles must pin the identical shell digest for the shared run dir to stay unambiguous"
lockfile="$TRIAGE_LOCKFILE"
run_dir="$tmp/runs/cri-140"
mkdir -p "$run_dir"
eval "$publication_block"

expect_shell="sha256:$(entry_digest shell triage)"
[ -n "$expect_shell" ] || fail "oracle could not read the shell/triage entry from the lockfile"
[ "$(digest_of triage)" = "$expect_shell" ] || fail "digest-triage must be published keyed by adapter instance (CRI-140)"
[ "$(digest_of shell)" = "$expect_shell" ] || fail "digest-shell must keep the kind-keyed shell digest"
[ "$(adapter_entry_digests)" = "triage $(entry_digest shell triage)" ] || \
    fail "the triage tree must publish exactly its shell/triage instance digest"

# qa_triage_v1 is the repo exemplar for a lockfile carrying BOTH kinds -
# copilot investigator/supervisor agents plus the shell repo adapter - so it
# exercises the kind-keyed copilot file and copilot instance keying in one
# pass. Its instance names never collide with the triage tree's and both
# trees pin the same shell 0.5.4 digest, so the exemplar publishes into the
# same run dir unambiguously; this pass must stay last so digest-copilot
# ends at the both-kinds value the part-2 kind fallback resolves.
lockfile="$EXEMPLAR_LOCKFILE"
eval "$publication_block"

expect_shell="sha256:$(entry_digest shell repo)"
[ -n "$expect_shell" ] || fail "oracle could not read the shell/repo entry from the lockfile"
[ "$(digest_of shell)" = "$expect_shell" ] || fail "digest-shell must keep the kind-keyed shell digest"
[ "$(digest_of repo)" = "$expect_shell" ] || fail "missing instance-keyed digest for repo"

expect_copilot="sha256:$(entry_digest copilot investigator)"
[ -n "$expect_copilot" ] || fail "oracle could not read the copilot/investigator entry from the lockfile"
[ "$(digest_of copilot)" = "$expect_copilot" ] || fail "digest-copilot must keep the kind-keyed copilot digest"
[ "$(digest_of investigator)" = "$expect_copilot" ] || fail "missing instance-keyed digest for investigator"
[ "$(digest_of supervisor)" = "sha256:$(entry_digest copilot supervisor)" ] || \
    fail "missing instance-keyed digest for supervisor"

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
# file was used (the real lockfile currently pins the triage instance and its shell kind to the
# same digest, which would make the resolution indistinguishable).
printf 'sha256:triage-instance-pin' > "$run_dir/digest-triage"

run_adapter() {
    local out="$1"
    shift
    # A 20s timeout turns the CRI-139 wedge symptom (a poll_file loop that
    # never terminates) into a hard failure.
    env -i PATH="$stubbin:/usr/bin:/bin" HOME="$tmp" CRITERIA_STUB_OUT="$out" "$@" \
        timeout 20 "$ADAPTER" || fail "pod-adapter-adapter.sh did not terminate within 20s (poll loop wedged)"
    [ -s "$out" ] || fail "pod-adapter-adapter.sh never exec'd the adapter binary"
}

# The CRI-140 per-scope triage pod: kind shell, instance triage, no direct
# env for host/token/digest — everything resolves from the discovery files,
# and the digest comes from the instance-keyed file.
run_adapter "$tmp/adapter-env" \
    ADAPTER_KIND=shell \
    CRITERIA_ADAPTER_NAME=triage \
    CRITERIA_RUN_JOB_NAME=cri-140 \
    CRITERIA_RUN_DIR_ROOT="$tmp/runs"
resolved="$(cat "$tmp/adapter-env")"
printf '%s' "$resolved" | grep -qx 'CRITERIA_REMOTE_HOST=10.0.0.9:7778' || \
    fail "adapter.sh must dial the discovery host file (POD_IP:7778), never the shim bind address"
printf '%s' "$resolved" | grep -qx 'CRITERIA_REMOTE_TOKEN=per-run-token' || \
    fail "adapter.sh must resolve the per-run bearer token from discovery"
printf '%s' "$resolved" | grep -qx 'CRITERIA_REMOTE_DIGEST=sha256:triage-instance-pin' || \
    fail "adapter.sh must resolve the instance-keyed digest-triage over the kind-keyed file"

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
