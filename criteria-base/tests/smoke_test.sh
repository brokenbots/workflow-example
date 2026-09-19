#!/usr/bin/env bash
set -euo pipefail

# Local smoke test for the minimal criteria base image (CRI-230, M6.1).
#
# Builds criteria-base/Dockerfile, publishes it to the local registry as
# localhost:5000/criteria-base:<tag>, then proves the CRI-230 exit criteria:
#
#   1. Image contents: criteria binary from the pinned criteria main commit,
#      git and ca-certs present, no baked /workflows tree, no node/gh/jq.
#   2. Restricted securityContext: runs as uid 10001 with --cap-drop=ALL.
#   3. CRITERIA_HOME=/data/criteria on the (mounted) data volume, writable.
#   4. Entrypoint e2e: a git source URL is fetched through the merged fetcher
#      and a trivial workflow executes end to end on the cached tree.
#   5. Pin enforcement (CRI-226): a WORKFLOW_REF mismatch is not applied.
#
# Requires docker (or podman) and a running registry (default localhost:5000).

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
IMAGE="${CRITERIA_BASE_IMAGE:-localhost:5000/criteria-base}"
# Repository path inside the registry (e.g. criteria-base), used for the
# registry API tag check.
image_repo="${IMAGE#*/}"
CONTAINER_TOOL="${CONTAINER_TOOL:-$(command -v docker || command -v podman || true)}"

fail() {
    echo "FAIL: $1" >&2
    exit 1
}

[ -n "$CONTAINER_TOOL" ] || fail "no container tool found (docker/podman); this smoke test must run on a host with one"

# Pinned criteria main commit built into the image.
pinned_sha="$(awk -F= '/^ARG CRITERIA_COMMIT=/ {print $2}' "$REPO_ROOT/criteria-base/Dockerfile" | tr -d '[:space:]')"
[ "$pinned_sha" = "eae01816c4a2da88449833a233d1b3f6a099bef1" ] || \
    fail "criteria-base/Dockerfile pins unexpected commit: $pinned_sha"
pinned_short="${pinned_sha:0:7}"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# Registry must be up: the tag check later depends on it.
registry_host="$(printf '%s' "$IMAGE" | cut -d/ -f1)"
curl -fsS "http://$registry_host/v2/" >/dev/null || \
    fail "registry not reachable at http://$registry_host/v2/ (start the local registry first)"

# Trivial workflow fixture in a git source repo; the container fetches it via
# the merged fetcher (git::file:// URL) and applies it from the cached tree.
src="$tmp/smoke-src"
git init -q -b main "$src"
cp "$REPO_ROOT/criteria-base/tests/fixtures/smoke_workflow/main.hcl" "$src/"
cp "$REPO_ROOT/criteria-base/tests/fixtures/smoke_workflow/.criteria.lock.hcl" "$src/"
git -C "$src" add -A
git -C "$src" -c user.name=smoke -c user.email=smoke@example.invalid \
    commit -q -m "criteria-base smoke workflow"
head_sha="$(git -C "$src" rev-parse HEAD)"

# Build + publish with a unique tag (the kubelet never re-resolves reused tags).
tag="$(date +%Y%m%d-%H%M%S)-$(git -C "$REPO_ROOT" rev-parse --short HEAD 2>/dev/null || echo nosha)"
echo "==> Building $IMAGE:$tag"
"$CONTAINER_TOOL" build --build-arg TARGETARCH=amd64 \
    -f "$REPO_ROOT/criteria-base/Dockerfile" -t "$IMAGE:$tag" "$REPO_ROOT/criteria-base/"
echo "==> Publishing $IMAGE:$tag"
"$CONTAINER_TOOL" push "$IMAGE:$tag"

registry_tags="$(curl -fsS "http://$registry_host/v2/$image_repo/tags/list")"
printf '%s' "$registry_tags" | grep -q "\"$tag\"" || \
    fail "tag $tag not present in registry catalog: $registry_tags"

image_user="$("$CONTAINER_TOOL" image inspect --format '{{.Config.User}}' "$IMAGE:$tag")"
[ "$image_user" = "10001" ] || \
    fail "image Config.User is '$image_user', want 10001 (non-root)"

echo "==> Inspecting image contents"
# The inspection runs inside the container; keep it in a file so it is not
# expanded by the host shell.
cat >"$tmp/inspect.sh" <<'EOF'
echo "uid=$(id -u)"
echo "git=$(command -v git || echo missing)"
echo "ca=$([ -f /etc/ssl/certs/ca-certificates.crt ] && echo present || echo missing)"
echo "workflows=$([ -e /workflows ] && echo present || echo absent)"
echo "node=$(command -v node || echo absent)"
echo "gh=$(command -v gh || echo absent)"
echo "jq=$(command -v jq || echo absent)"
echo "criteria-home=$CRITERIA_HOME"
echo "criteria-version=$(criteria version)"
EOF
chmod 0644 "$tmp/inspect.sh"
inspect="$("$CONTAINER_TOOL" run --rm --entrypoint /bin/sh \
    -v "$tmp/inspect.sh:/inspect.sh:ro" "$IMAGE:$tag" /inspect.sh)"
printf '%s\n' "$inspect"
printf '%s\n' "$inspect" | grep -qx "uid=10001" || fail "image does not run as uid 10001"
printf '%s\n' "$inspect" | grep -qx "criteria-home=/data/criteria" || fail "CRITERIA_HOME is not /data/criteria"
printf '%s\n' "$inspect" | grep -qE "^criteria-version=.*g$pinned_short\$" || \
    fail "criteria binary is not from pinned criteria main ($pinned_short): got $(printf '%s' "$inspect" | grep '^criteria-version=')"
printf '%s\n' "$inspect" | grep -qE '^git=/' || fail "git missing from image"
printf '%s\n' "$inspect" | grep -qx "ca=present" || fail "ca-certificates missing from image"
printf '%s\n' "$inspect" | grep -qx "workflows=absent" || fail "image must not ship a baked /workflows tree"
printf '%s\n' "$inspect" | grep -qx "node=absent" || fail "node must not be in the image"
printf '%s\n' "$inspect" | grep -qx "gh=absent" || fail "gh must not be in the image"
printf '%s\n' "$inspect" | grep -qx "jq=absent" || fail "jq must not be in the image"

# The data volume must be writable by the runtime uid regardless of which host
# user created it.
data="$tmp/data"
mkdir -p "$data"
chmod 0777 "$data"

run_smoke() {
    # Restricted posture mirroring the operator's restrictedContainerSecurity
    # context (uid 10001, all caps dropped, no privilege escalation). The
    # container runtime's default seccomp profile (RuntimeDefault, matching
    # the pod spec) applies because no --security-opt seccomp overrides it.
    "$CONTAINER_TOOL" run --rm \
        --user 10001:10001 \
        --cap-drop=ALL \
        --security-opt no-new-privileges \
        -e WORKFLOW_URL="$1" \
        -e WORKFLOW_REF="${2-}" \
        -v "$src:/smoke-src:ro" \
        -v "$data:/data" \
        "$IMAGE:$tag"
}

echo "==> Running smoke workflow end to end (git source, matching pin)"
out="$(run_smoke "git::file:///smoke-src?ref=main" "$head_sha")" || \
    fail "smoke run failed: $out"
printf '%s\n' "$out"
printf '%s' "$out" | grep -q "criteria-base smoke ok" || \
    fail "smoke workflow did not execute: 'criteria-base smoke ok' not in output"
[ -d "$data/criteria/cache/workflows" ] || \
    fail "fetched workflow cache not found under CRITERIA_HOME on /data"
found_tree="$(find "$data/criteria/cache/workflows" -name main.hcl | head -1)"
[ -n "$found_tree" ] || fail "cached tree does not contain main.hcl"
[ -f "$data/criteria/cache/workflows/index.json" ] || \
    fail "cache index.json not written at CRITERIA_HOME/cache/workflows/index.json"

echo "==> Verifying fail-closed pin enforcement (CRI-226)"
mismatch="0000000000000000000000000000000000000000"
if out="$(run_smoke "git::file:///smoke-src?ref=main" "$mismatch" 2>&1)"; then
    fail "mismatched WORKFLOW_REF was applied instead of failing closed"
fi
printf '%s\n' "$out" | grep -q "expected-pin mismatch" || \
    fail "mismatch failure did not report expected-pin mismatch: $out"
if printf '%s\n' "$out" | grep -q "criteria-base smoke ok"; then
    fail "smoke workflow ran despite mismatched pin"
fi
echo "==> Verifying fail-closed missing WORKFLOW_URL (D2)"
if out="$("$CONTAINER_TOOL" run --rm --user 10001:10001 --cap-drop=ALL \
        -v "$data:/data" "$IMAGE:$tag" 2>&1)"; then
    fail "missing WORKFLOW_URL did not fail closed"
fi
printf '%s\n' "$out" | grep -q "WORKFLOW_URL is not set" || \
    fail "missing WORKFLOW_URL failure not reported: $out"

echo "PASS: criteria-base image smoke test ($IMAGE:$tag)"