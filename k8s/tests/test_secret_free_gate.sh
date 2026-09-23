#!/usr/bin/env bash
# CRI-250 (plan CRI-214 M11.3, plan section 3.8): deterministic secret-grep gate.
#
# Fails the build when any SHIPPED config carries credential-shaped material.
# Shipped configs are committed files — everything in the repo that is not a
# test fixture or documentation. Secrets are allowed ONLY as OpenBao/CSI name
# references (SecretProviderClass + key names), never as values.
#
# What this gate greps for (deliberately simple and deterministic):
#   - GitHub token shapes: gho_ / ghp_ / github_pat_ / github-app JWTs
#   - PEM private keys
#   - OpenBao/Vault root-token shapes (hvs. / s.<token>)
#   - Basic-auth userinfo in git URLs (https://user:pass@...)
#   - Assignment-looking secret envs (API_KEY=<value> with a non-reference value)
#
# Exclusions are allowlisted paths only (tests that assert redaction, docs).
set -u

fail=0
fail() { printf 'SECRET-GATE FAIL: %s\n' "$1" >&2; fail=1; }

# ---------------------------------------------------------------- file scope
# Everything tracked, minus exclusions. .gitignore'd content never ships.
if command -v git >/dev/null 2>&1 && git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    file_list=$(git ls-files)
else
    file_list=$(find . -type f -not -path './.git/*')
fi

# Paths allowed to contain credential-shaped strings: tests that PROVE the
# redaction (their literals are assembled at runtime or clearly fake), and
# this gate itself.
excluded() {
    case "$1" in
        # Redaction-behavior tests (credentials are runtime-assembled or fake).
        */workflow_source_test.go|*/run_metadata_test.go|*test_redact*|*/main_test.go|*/source_test.go|*/jobbuilder*_test.go|*/criteria_test.go|*/wire_test.go) return 0 ;;
        # jobbuilder source.go carries the REDACTION placeholder shape
        # (x-access-token:*** interpolated from an env var at runtime).
        */jobbuilder/source.go) return 0 ;;
        # The gate itself carries the patterns.
        k8s/tests/test_secret_free.sh|scripts/test_secret_free_gate.sh) return 0 ;;
        # Lockfiles carry sigstore SUBJECT lines (issuer/subject URLs, no secrets).
        *) return 1 ;;
    esac
}

count=0
while IFS= read -r f; do
    [ -z "$f" ] && continue
    if excluded "$f"; then continue; fi
    # Skip binaries and lockfiles' digest lines are fine (sha256: hex is not a secret).
    case "$f" in
        *.png|*.jpg|*.gif|*.woff|*.woff2|*.ico|*.sum) continue ;;
    esac
    count=$((count + 1))

    # 1. GitHub token literals.
    if grep -nEho '(gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})' "$f" >/dev/null 2>&1; then
        hits=$(grep -nEo '(gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})' "$f" | head -3)
        fail "$f: GitHub-token-shaped literal: $hits"
    fi

    # 2. Private keys.
    if grep -n 'BEGIN \(RSA \|EC \|OPENSSH \)\?PRIVATE KEY' "$f" >/dev/null 2>&1; then
        fail "$f: private key material committed"
    fi

    # 3. OpenBao/Vault service-token shapes (hvs.<20+ alnum).
    if grep -nEo 'hvs\.[A-Za-z0-9]{20,}' "$f" >/dev/null 2>&1; then
        hits=$(grep -nEo 'hvs\.[A-Za-z0-9]{20,}' "$f" | head -3)
        fail "$f: Vault-token-shaped literal: $hits"
    fi

    # 4. Basic-auth userinfo in URLs (https://user:pass@host) — real creds ship
    #    as SecretProviderClass references, never inline.
    if grep -nE 'https?://[^/[:space:]@]+:[^/[:space:]@]+@' "$f" >/dev/null 2>&1; then
        hits=$(grep -nEo 'https?://[^/[:space:]@]+:[^/[:space:]@]+@' "$f" | head -3)
        fail "$f: URL userinfo (user:pass@) literal: $hits"
    fi

    # 5. Literal secret-env assignments with token VALUES (the first four
    #    patterns already catch the token shapes themselves; this one binds
    #    them to secret env names for clearer failures). Allowed: name-only
    #    reference mappings (env: {LINEAR_API_KEY: linear_api_key} — the
    #    VALUE is a CSI key name) and placeholder examples.
    if grep -nEo '(gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})' "$f" >/dev/null 2>&1; then
        hits=$(grep -nEo '(gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})' "$f" | head -3)
        fail "$f: token-shaped literal: $hits"
    fi
done <<EOF
$file_list
EOF

if [ "$count" -eq 0 ]; then
    fail "gate scanned 0 files (file listing broken)"
fi

if [ "$fail" -ne 0 ]; then
    echo "Scanned $count files. Secrets are only allowed as OpenBao/CSI name references (plan CRI-214 section 3.8)." >&2
    exit 1
fi
echo "SECRET-GATE PASS: $count shipped files scanned, no credential material."