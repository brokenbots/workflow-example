#!/usr/bin/env bash
# fetch-criteria-release.sh — resilient, FAIL-CLOSED fetch of a criteria engine
# release archive. Usage:
#   scripts/fetch-criteria-release.sh <version> <dest-path>
#
# Environment overrides (test hooks, defaults reproduce the CI fetch):
#   CRITERIA_RELEASE_BASE     default https://github.com/brokenbots/criteria/releases/download/<version>
#   CRITERIA_FETCH_ATTEMPTS    default 6 outer attempts
#   CRITERIA_FETCH_RETRY_SLEEP default 15 s outer backoff (doubles per attempt)
#
# Why this exists: CI run 42571727491 (PR #269) died at the engine-install step —
# release-assets.githubusercontent.com answered HTTP 504 on all six of curl's
# --retry attempts (initial + 5 retries at --retry-delay 5, a ~36 s budget) and
# the whole k8s-shell-tests job was skipped. The asset itself was healthy; the
# window just outlived the curl-level budget. House precedent 74eceef (PR #255)
# added that curl-level retry for exit-35 transport resets; this wrapper extends
# the same step to server-side 5xx windows with a longer bounded retry-and-
# backoff loop around it.
#
# Fail-closed guarantees:
#   - SHA256SUMS must carry EXACTLY ONE line for the archive. The previous
#     inline check (`grep " ${archive}$" ... | sha256sum -c -`) was fail-OPEN:
#     a SHA256SUMS lacking the archive line piped empty stdin to sha256sum,
#     which exits 0 on nothing-to-check — installing an unverified binary.
#   - The digest line is verified with sha256sum -c; any mismatch fails.
#   - <dest-path> is written only AFTER verification succeeds. The script
#     never installs anything and never leaves an unverified archive there.
set -euo pipefail

fail() {
    echo "fetch-criteria-release: $*" >&2
    exit 1
}

[ "$#" -eq 2 ] || fail "usage: scripts/fetch-criteria-release.sh <version> <dest-path>"
VERSION="$1"
DEST="$2"
[ -n "$VERSION" ] && [ -n "$DEST" ] || fail "version and dest-path must be non-empty"

ARCHIVE="criteria-${VERSION}-linux-amd64.tar.gz"
BASE="${CRITERIA_RELEASE_BASE:-https://github.com/brokenbots/criteria/releases/download/${VERSION}}"
ATTEMPTS="${CRITERIA_FETCH_ATTEMPTS:-6}"
RETRY_SLEEP="${CRITERIA_FETCH_RETRY_SLEEP:-15}"
case "$ATTEMPTS" in (*[!0-9]*|'') fail "CRITERIA_FETCH_ATTEMPTS must be a positive integer" ;; esac
case "$RETRY_SLEEP" in (*[!0-9]*|'') fail "CRITERIA_FETCH_RETRY_SLEEP must be a non-negative integer" ;; esac
[ "$ATTEMPTS" -ge 1 ] || fail "CRITERIA_FETCH_ATTEMPTS must be >= 1"

command -v curl >/dev/null 2>&1 || fail "curl not found"
command -v sha256sum >/dev/null 2>&1 || fail "sha256sum not found"

# fetch per attempt via the house curl-level retry budget (74eceef): transport
# resets and short 5xx blips are absorbed here, longer windows by the outer loop.
CURL_RETRY=(curl -fsSL --retry 5 --retry-all-errors --retry-delay 5 --retry-max-time 120)

STAGE="$(mktemp -d)"
trap 'rm -rf "$STAGE"' EXIT

for ((attempt = 1; attempt <= ATTEMPTS; attempt++)); do
    echo "fetch-criteria-release: attempt ${attempt}/${ATTEMPTS}: ${BASE}/${ARCHIVE}"
    last_err=""
    stage_ok=0
    if "${CURL_RETRY[@]}" "${BASE}/${ARCHIVE}" -o "${STAGE}/${ARCHIVE}"; then
        if "${CURL_RETRY[@]}" "${BASE}/SHA256SUMS" -o "${STAGE}/SHA256SUMS"; then
            # Exactly one line whose second whitespace-separated field is the archive.
            lines="$(awk -v a="$ARCHIVE" '$2 == a' "${STAGE}/SHA256SUMS" || true)"
            count="$(printf '%s\n' "$lines" | sed '/^$/d' | wc -l | tr -d ' ')"
            if [ "$count" -ne 1 ]; then
                last_err="expected exactly one '<sum>  ${ARCHIVE}' line in SHA256SUMS, found ${count}"
                if [ "$count" -gt 0 ]; then
                    last_err="${last_err} ($(printf '%s\n' "$lines" | head -3 | tr '\n' '|'))"
                fi
            elif (cd "$STAGE" && printf '%s\n' "$lines" | sha256sum -c - >"$STAGE/verify.out" 2>&1); then
                stage_ok=1
            else
                last_err="checksum mismatch for ${ARCHIVE} — $(tail -1 "$STAGE/verify.out" | tr '\n' ' ')"
            fi
        else
            last_err="curl exit $? fetching ${BASE}/SHA256SUMS"
        fi
    else
        last_err="curl exit $? fetching ${BASE}/${ARCHIVE}"
    fi

    if [ "$stage_ok" -eq 1 ]; then
        mkdir -p "$(dirname "$DEST")"
        # Temp-file + rename: a mid-copy failure must never leave a truncated DEST.
        tmp_dest="${DEST}.tmp.$$"
        cp -- "${STAGE}/${ARCHIVE}" "${tmp_dest}"
        mv -f -- "${tmp_dest}" "${DEST}"
        echo "fetch-criteria-release: verified ${ARCHIVE}, wrote ${DEST}"
        exit 0
    fi

    # Fail-closed hygiene: drop the partial archive + checksums before sleeping.
    rm -f "${STAGE}/${ARCHIVE}" "${STAGE}/SHA256SUMS" "${STAGE}/verify.out"
    if [ "$attempt" -eq "$ATTEMPTS" ]; then
        fail "all ${ATTEMPTS} attempts failed; last error: ${last_err}"
    fi
    echo "fetch-criteria-release: attempt failed (${last_err}); retrying in ${RETRY_SLEEP}s" >&2
    sleep "$RETRY_SLEEP"
    RETRY_SLEEP=$((RETRY_SLEEP * 2))
done

# Unreachable (loop exits either 0 or via fail), kept for the fail-closed shape.
exit 1
