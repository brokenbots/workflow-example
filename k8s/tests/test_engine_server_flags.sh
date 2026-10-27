#!/usr/bin/env bash
set -euo pipefail

# Engine contract for the KB-344 run-event proxy handoff: the runner scripts
# pass --name (run attribution for the operator's proxy) and
# --server-bootstrap-token (the CR-stamped enrollment credential, presented
# as the X-Server-Bootstrap header on Register in server mode). This pins
# the flags and the header wording against the installed `criteria` binary
# so an engine upgrade cannot silently rename or drop either half of the
# handoff.

fail() {
    echo "FAIL: $1" >&2
    exit 1
}

if ! command -v criteria > /dev/null 2>&1; then
    echo "SKIP: criteria not on PATH (CI installs the pinned v0.5.52-rc4 build; the contract runs there)"
    exit 0
fi

help="$(criteria apply --help 2>&1)" || fail "criteria apply --help failed; cannot verify the server-flag contract"

# --name: run attribution. The proxy keys per-run streams on it, so the
# flag (and its 'server mode' role in the help) must exist.
if ! grep -Eq -- '^[[:space:]]*--name[[:space:]]+string[[:space:]]+.*server mode' <<<"$help"; then
    fail "criteria apply no longer exposes --name <agent> (server mode, defaults to hostname) — the KB-344 proxy attribution would carry no run identity"
fi

# --server-bootstrap-token: the enrollment credential path. The help line
# must name the X-Server-Bootstrap header it becomes.
if ! grep -Eq -- '^[[:space:]]*--server-bootstrap-token[[:space:]]' <<<"$help"; then
    fail "criteria apply no longer exposes --server-bootstrap-token — the CR-stamped enrollment credential would never reach the operator's run-event proxy"
fi
token_line="$(grep -- '--server-bootstrap-token' <<<"$help" | head -n 1)"
grep -qF 'X-Server-Bootstrap' <<<"$token_line" || \
    fail "the --server-bootstrap-token help text no longer mentions X-Server-Bootstrap: $token_line"

version="$(criteria version 2>/dev/null || true)"
echo "PASS: criteria engine ($version) exposes --name and --server-bootstrap-token (X-Server-Bootstrap) matching the runner scripts"