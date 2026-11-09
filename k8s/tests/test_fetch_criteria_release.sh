#!/usr/bin/env bash
set -euo pipefail

# Regression test for KB-392 / CI run 42571727491: the engine-install step in
# .github/workflows/ci.yml died to an HTTP 504 window — release-assets
# returned 504 on every curl-level --retry attempt (initial + 5 retries at
# --retry-delay 5, ~36 s budget) and the whole k8s-shell-tests job was skipped
# because the step exited 22. scripts/fetch-criteria-release.sh wraps that
# fetch with a longer bounded retry+backoff loop AND closes a fail-open hole
# in the old inline verification: `grep " ${archive}$" SHA256SUMS |
# sha256sum -c -` exits 0 on a SHA256SUMS that LACKS the archive line (empty
# stdin ⇒ nothing to check), silently installing an unverified binary.
#
# Four deterministic arms against a throwaway local node HTTP server:
#   1. happy path              → exit 0, dest bytes == served payload
#   2. transient 504 arm       → archive 504s for its first 2 requests, then
#                                200 → exit 0 (retry survives the outage);
#                                SHA256SUMS answers 200 throughout
#   3. missing SHA256SUMS line → exit non-zero, dest NOT created
#   4. wrong digest            → exit non-zero, dest NOT created

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
FETCH="$REPO_ROOT/scripts/fetch-criteria-release.sh"
VERSION="v0.5.59"
ARCHIVE="criteria-${VERSION}-linux-amd64.tar.gz"
DECOY_ARCHIVE="criteria-v0.5.58-linux-amd64.tar.gz"
DECOY_SUM="0000000000000000000000000000000000000000000000000000000000000000"

fail() {
    echo "FAIL: $*" >&2
    exit 1
}

command -v node >/dev/null 2>&1 || fail "node not found (test serves fixtures over a local HTTP server)"
command -v curl >/dev/null 2>&1 || fail "curl not found"
command -v sha256sum >/dev/null 2>&1 || fail "sha256sum not found"
[ -f "$FETCH" ] || fail "fetch script missing: $FETCH"

TMP="$(mktemp -d)"
PIDS=()
cleanup() {
    for pid in "${PIDS[@]:-}"; do kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; done
    rm -rf "$TMP"
}
trap cleanup EXIT

# ── local fixture server ─────────────────────────────────────────────────────
# Serves <root>/SHA256SUMS and <root>/<archive>; the first <flaky> archive
# requests answer 504 Gateway Timeout (server-side CDN-window simulation).
cat > "$TMP/server.js" <<'EOF'
const http = require('http');
const fs = require('fs');
const path = require('path');
const [root, archive, flakyRaw] = process.argv.slice(2);
const flaky = parseInt(flakyRaw, 10) || 0;
let archiveRequests = 0;
function sendFile(res, file) {
    fs.readFile(file, (err, data) => {
        if (err) { res.writeHead(404); res.end('not found'); return; }
        res.writeHead(200, { 'content-length': data.length });
        res.end(data);
    });
}
const server = http.createServer((req, res) => {
    const name = path.basename(decodeURIComponent(req.url));
    if (name === archive) {
        archiveRequests += 1;
        if (archiveRequests <= flaky) {
            res.writeHead(504, { 'content-type': 'text/plain' });
            res.end('Gateway Timeout (fixture)');
            return;
        }
        sendFile(res, path.join(root, archive));
        return;
    }
    if (name === 'SHA256SUMS') { sendFile(res, path.join(root, 'SHA256SUMS')); return; }
    res.writeHead(404); res.end('not found');
});
server.listen(0, '127.0.0.1', () => console.log(`PORT=${server.address().port}`));
EOF

start_server() {
    local root="$1" flaky="$2" out="$3" pid port i
    node "$TMP/server.js" "$root" "$ARCHIVE" "$flaky" >"$out" 2>&1 &
    pid=$!
    PIDS+=("$pid")
    for i in $(seq 1 100); do
        port="$(sed -n 's/^PORT=\([0-9][0-9]*\)$/\1/p' "$out" | head -1)"
        [ -n "$port" ] && curl -fs "http://127.0.0.1:${port}/SHA256SUMS" -o /dev/null 2>/dev/null && break
        sleep 0.1
    done
    [ -n "${port:-}" ] || fail "fixture server never became ready ($(head -3 "$out"))"
    echo "$port"
}

# make_payload ROOT: deterministic ~256 B archive body + a well-formed
# SHA256SUMS carrying a decoy line and the real archive line.
make_payload() {
    local root="$1" sum
    printf 'criteria-engine-payload KB-392\n%.0s' {1..16} >"$root/$ARCHIVE"
    sum="$(sha256sum "$root/$ARCHIVE" | cut -d' ' -f1)"
    printf '%s  %s\n' "$DECOY_SUM" "$DECOY_ARCHIVE" >"$root/SHA256SUMS"
    printf '%s  %s\n' "$sum" "$ARCHIVE" >>"$root/SHA256SUMS"
}

# Shared knobs: small attempt count + 1 s base backoff keep the whole test fast.
export CRITERIA_FETCH_ATTEMPTS=2
export CRITERIA_FETCH_RETRY_SLEEP=1

run_arm() {
    local name="$1" flaky="$2" dest="$3" root="$4" port rc=0
    local log="arm${name}.fetch.log"
    echo "--- arm ${name}: fetch-criteria-release ${VERSION} (flaky=${flaky}) -> ${dest}" >&2
    port="$(start_server "$root" "$flaky" "$TMP/arm${name}.server.out")"
    CRITERIA_RELEASE_BASE="http://127.0.0.1:${port}" \
        bash "$FETCH" "$VERSION" "$dest" >"$TMP/$log" 2>&1 || rc=$?
    cat "$TMP/$log" >&2
    echo "$rc"
}

# ── arm 1: happy path ────────────────────────────────────────────────────────
mkdir -p "$TMP/arm1"
make_payload "$TMP/arm1"
rc="$(run_arm 1 0 "$TMP/arm1/dest.tar.gz" "$TMP/arm1")"
[ "$rc" -eq 0 ] || fail "arm 1: script exited ${rc}, expected 0 (log above)"
cmp "$TMP/arm1/$ARCHIVE" "$TMP/arm1/dest.tar.gz" || fail "arm 1: dest bytes differ from served payload"

# ── arm 2: transient 504 window, then 200 ────────────────────────────────────
mkdir -p "$TMP/arm2"
cp "$TMP/arm1/$ARCHIVE" "$TMP/arm2/$ARCHIVE"
cp "$TMP/arm1/SHA256SUMS" "$TMP/arm2/SHA256SUMS"
rc="$(run_arm 2 2 "$TMP/arm2/dest.tar.gz" "$TMP/arm2")"
[ "$rc" -eq 0 ] || fail "arm 2: script exited ${rc}, expected 0 — retry must survive a 504 window curl-level --retry alone cannot outlive (log above)"

# ── arm 3: SHA256SUMS lacks the archive line (the old fail-open hole) ───────
mkdir -p "$TMP/arm3"
make_payload "$TMP/arm3"
grep -v " ${ARCHIVE}\$" "$TMP/arm3/SHA256SUMS" >"$TMP/arm3/SHA256SUMS.new" && mv "$TMP/arm3/SHA256SUMS.new" "$TMP/arm3/SHA256SUMS"
rc="$(run_arm 3 0 "$TMP/arm3/dest.tar.gz" "$TMP/arm3")"
[ "$rc" -ne 0 ] || fail "arm 3: script exited 0 on a SHA256SUMS missing the archive line — this was exactly the old fail-open hole"
[ ! -e "$TMP/arm3/dest.tar.gz" ] || fail "arm 3: dest was created without a verified checksum"

# ── arm 4: SHA256SUMS lists a wrong digest ───────────────────────────────────
mkdir -p "$TMP/arm4"
make_payload "$TMP/arm4"
sed -i "s/^$(sha256sum "$TMP/arm4/$ARCHIVE" | cut -d' ' -f1)/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/" "$TMP/arm4/SHA256SUMS"
rc="$(run_arm 4 0 "$TMP/arm4/dest.tar.gz" "$TMP/arm4")"
[ "$rc" -ne 0 ] || fail "arm 4: script exited 0 despite a digest mismatch — verification must fail closed"
[ ! -e "$TMP/arm4/dest.tar.gz" ] || fail "arm 4: dest was created despite a failed checksum verification"

echo "fetch-criteria-release: all arms asserted"
