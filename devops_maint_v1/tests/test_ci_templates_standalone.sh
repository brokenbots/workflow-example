#!/usr/bin/env bash
# devops_maint_v1 — Lane B CI template pack suite (KB-408, wave 3).
#
# Covers devops_maint_v1/ci_templates/ — four GitHub Actions workflow
# templates (go/ts/py/starter) with the KB-404 docs-only skip and three
# per-repo dependabot.yml policy variants:
#   1. file inventory + no accidental Go-template markers (the templates are
#      plain .yml applied byte-exact; GH Actions `${{ }}` and templatefile(3)
#      `{{ }}` collide, so a stray non-dollar `{{` anywhere is a red flag);
#   2. the KB-404 gate step is byte-identical across all four templates and
#      every heavy job carries `needs: changes` + the skip `if:` (push-to-main
#      always full; mixed diff full);
#   3. BEHAVIOURAL: the extracted docs-only gate script is executed against
#      real fixture git repos — docs-only PRs skip, mixed/push/empty-diff/
#      detection-failure stay full, `.github/` state files stay full;
#   4. language legs carry their ADR job set (go: vet/gofmt/test/
#      govulncheck/osv; ts: tsc + vitest container; py: ruff guarded pytest;
#      starter: exactly-one-language parity + smoke), stable `name:` values
#      for the KB-409 required-check contract, org action allowlist, pins;
#   5. dependabot policy shape incl. the negative: NO semver-major entry,
#      ever — majors must surface as judgment-lane PRs (KB-407 gate #3);
#   6. README mapping table vs the actual directory contents.
#   7. tree still validates (grammar + interface contract).
#
# Idioms mirror test_route_gate_standalone.sh (ok/fail/require_equal,
# WORK_ROOT overrides, tree-validate leg). The YAML-parse leg needs
# python3 + PyYAML and skips loudly when the local toolchain lacks them.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
TREE_DIR="$(cd "${ROOT_DIR}/.." && pwd)"
CRITERIA="${CRITERIA_BIN:-/usr/local/bin/criteria}"
TPL_DIR="${ROOT_DIR}/ci_templates"

WORK_ROOT="${TEST_WORK_ROOT:-$(mktemp -d)}"
if [ "${TEST_KEEP_WORK_ROOT:-0}" != "1" ]; then trap 'rm -rf "${WORK_ROOT}"' EXIT; fi

FAILED=0

fail() {
    echo "FAIL: $1" >&2
    FAILED=$((FAILED + 1))
}

ok() {
    echo "ok: $1" >&2
}

skip_loud() {
    echo "SKIP: $1" >&2
}

require_equal() {
    if [ "$1" = "$2" ]; then
        ok "$3"
    else
        fail "$3: got '$1', want '$2'"
    fi
}

CI_GO="${TPL_DIR}/github-ci-go.yml"
CI_TS="${TPL_DIR}/github-ci-ts.yml"
CI_PY="${TPL_DIR}/github-ci-py.yml"
CI_STARTER="${TPL_DIR}/github-ci-starter.yml"
DEP_GO="${TPL_DIR}/dependabot-go.yml"
DEP_TS="${TPL_DIR}/dependabot-ts.yml"
DEP_PY="${TPL_DIR}/dependabot-py.yml"
README="${TPL_DIR}/README.md"

# ═════════════════════════════════════════════════════════════════════════════
# 1. File inventory + no Go-template markers + YAML parse (when tools exist)
# ═════════════════════════════════════════════════════════════════════════════
ALL_TPL_FILES=("${CI_GO}" "${CI_TS}" "${CI_PY}" "${CI_STARTER}" "${DEP_GO}" "${DEP_TS}" "${DEP_PY}" "${README}")

for f in "${ALL_TPL_FILES[@]}"; do
    if [ -s "${f}" ]; then
        ok "template file present and non-empty: $(basename "${f}")"
    else
        fail "template file missing or empty: ${f}"
    fi
done

DISK_COUNT="$(find "${TPL_DIR}" -maxdepth 1 -type f | wc -l | tr -d ' ')"
require_equal "${DISK_COUNT}" "8" "no stray files in ci_templates (8 expected)"

TAB_OK=0
for f in "${ALL_TPL_FILES[@]}"; do
    # YAML forbids literal tabs; a tab anywhere in the pack is malformed.
    if [ "$(grep -c "$(printf '\t')" "${f}" || true)" != "0" ]; then
        TAB_OK=1
    fi
done
if [ "${TAB_OK}" = "0" ]; then
    ok "no literal tabs in any pack file (YAML would reject them)"
else
    fail "a literal tab snuck into the pack"
fi

# Stray Go-template markers: `${{` is Actions syntax, but a `{{` NOT preceded
# by `$` would mean someone rendered the file through templatefile(3) or
# copy/pasted template text — both mangle the workflow.
for f in "${CI_GO}" "${CI_TS}" "${CI_PY}" "${CI_STARTER}" "${DEP_GO}" "${DEP_TS}" "${DEP_PY}"; do
    if sed 's/\${{/[ACTIONS]/g' "${f}" | grep -q '{{'; then
        fail "stray Go-template marker ({{ not part of \${{) in $(basename "${f}")"
    else
        ok "no Go-template markers in $(basename "${f}")"
    fi
done

if command -v python3 >/dev/null 2>&1 && python3 -c 'import yaml' >/dev/null 2>&1; then
    for f in "${CI_GO}" "${CI_TS}" "${CI_PY}" "${CI_STARTER}" "${DEP_GO}" "${DEP_TS}" "${DEP_PY}"; do
        if python3 - <<PYEOF
import sys, yaml
try:
    yaml.safe_load(open("${f}"))
except Exception as exc:
    print(f"FAIL: YAML parse error in ${f}: {exc}", file=sys.stderr)
    sys.exit(1)
PYEOF
    then
        ok "YAML parses: $(basename "${f}")"
    else
        fail "YAML does not parse: $(basename "${f}")"
    fi
done
else
    skip_loud "YAML parse leg (python3+pyyaml unavailable here; the CI image covers it)"
fi

# ═════════════════════════════════════════════════════════════════════════════
# 2. Gate step extraction: byte-identical across all four CI templates
# ═════════════════════════════════════════════════════════════════════════════
# (ten-space dedent for the run script, six-space dedent for the whole step;
#  awk index() is used so the ── sentinels match literally without regex.)

gate_script_of() { # <file> -> KB-404 run script, dedented
    awk 'index($0, "── KB-404 docs-only detection") { f=1; next }
         index($0, "── end KB-404 docs-only detection") { f=0 }
         f { sub(/^          /, ""); print }' "$1"
}

gate_step_of() { # <file> -> the whole `- id: classify` step, dedented
    awk 'index($0, "      - id: classify") { f=1 }
         f { sub(/^      /, ""); print }
         index($0, "── end KB-404 docs-only detection") { f=0 }' "$1"
}

for f in "${CI_GO}" "${CI_TS}" "${CI_PY}" "${CI_STARTER}"; do
    if gate_script_of "${f}" >"${WORK_ROOT}/gate.$(basename "${f}").sh" && [ -s "${WORK_ROOT}/gate.$(basename "${f}").sh" ]; then
        ok "KB-404 gate script extractable: $(basename "${f}")"
    else
        fail "KB-404 gate script not extractable from $(basename "${f}") (sentinel drift)"
    fi
done

if cmp -s "${WORK_ROOT}/gate.github-ci-go.yml.sh" "${WORK_ROOT}/gate.github-ci-ts.yml.sh" &&
    cmp -s "${WORK_ROOT}/gate.github-ci-go.yml.sh" "${WORK_ROOT}/gate.github-ci-py.yml.sh" &&
    cmp -s "${WORK_ROOT}/gate.github-ci-go.yml.sh" "${WORK_ROOT}/gate.github-ci-starter.yml.sh"; then
    ok "gate run script byte-identical across all four CI templates"
else
    fail "gate run script drifted between templates (classification contract must not fork)"
fi

for f in "${CI_TS}" "${CI_PY}" "${CI_STARTER}"; do
    if cmp -s <(gate_step_of "${CI_GO}") <(gate_step_of "${f}"); then
        ok "gate step (env+run) byte-identical: github-ci-go.yml vs $(basename "${f}")"
    else
        fail "gate step drifted between github-ci-go.yml and $(basename "${f}")"
    fi
done

# ═════════════════════════════════════════════════════════════════════════════
# 3. Job sets + docs-only skip wiring + stable names (KB-409 contract)
# ═════════════════════════════════════════════════════════════════════════════
top_jobs() { # <file> -> top-level job keys in declaration order
    awk '/^jobs:/ { f=1; next }
         f && /^  [a-z][a-z0-9-]*:$/ { sub(/^  /, ""); sub(/:$/, ""); print; next }
         f && /^[a-z]/ { f=0 }' "$1"
}

require_equal "$(top_jobs "${CI_GO}" | tr '\n' ' ')" "changes lint test govulncheck osv " "go template job set + order"
require_equal "$(top_jobs "${CI_TS}" | tr '\n' ' ')" "changes typecheck test osv " "ts template job set + order"
require_equal "$(top_jobs "${CI_PY}" | tr '\n' ' ')" "changes lint test osv " "py template job set + order"
require_equal "$(top_jobs "${CI_STARTER}" | tr '\n' ' ')" "changes build-parity smoke " "starter template job set + order"

SKIP_IF="if: needs.changes.outputs.docs_only == 'false'"
job_block() { # <file> <job>
    awk -v key="$2" '$0 == "  " key ":" { f=1; print; next }
                     f && /^  [a-z][a-z0-9-]*:$/ && $0 != "  " key ":" { f=0 }
                     f { print }' "$1"
}

for pair in "${CI_GO}:lint test govulncheck osv" "${CI_TS}:typecheck test osv" "${CI_PY}:lint test osv" "${CI_STARTER}:build-parity smoke"; do    file="${pair%%:*}"
    for job in ${pair#*:}; do
        BLOCK="$(job_block "${file}" "${job}")"
        if printf '%s\n' "${BLOCK}" | grep -Eq 'needs: (\[?changes)'; then
            ok "$(basename "${file}")/${job}: needs the changes edge (docs gate)"
        else
            fail "$(basename "${file}")/${job}: missing needs: changes edge"
        fi
        if printf '%s\n' "${BLOCK}" | grep -Fq "${SKIP_IF}"; then
            ok "$(basename "${file}")/${job}: carries the docs-only skip if"
        else
            fail "$(basename "${file}")/${job}: missing the docs-only skip if (${SKIP_IF})"
        fi
        if printf '%s\n' "${BLOCK}" | grep -Fq "name: ${job}"; then
            ok "$(basename "${file}")/${job}: stable check name present"
        else
            fail "$(basename "${file}")/${job}: missing stable 'name:' for the KB-409 required-check contract"
        fi
    done
done

# The gate job itself must NOT gate on its own output.
GATE_BLOCK="$(job_block "${CI_GO}" "changes")"
if printf '%s\n' "${GATE_BLOCK}" | grep -Fq "${SKIP_IF}"; then
    fail "changes job wrongly self-gates on docs_only"
else
    ok "changes job never self-gates"
fi

# Starter: smoke also needs build-parity (skip cascades through the needs edge).
if printf '%s\n' "$(job_block "${CI_STARTER}" "smoke")" | grep -Fq 'needs: [changes, build-parity]'; then
    ok "starter/smoke: needs [changes, build-parity] (skip cascades on docs-only)"
else
    fail "starter/smoke: missing needs [changes, build-parity] cascade"
fi

# ═════════════════════════════════════════════════════════════════════════════
# 4. Triggers, permissions, concurrency (identical across all four)
# ═════════════════════════════════════════════════════════════════════════════
for f in "${CI_GO}" "${CI_TS}" "${CI_PY}" "${CI_STARTER}"; do
    base="$(basename "${f}")"
    if grep -q '^on:' "${f}" && grep -q '^  push:' "${f}" && grep -q 'branches: \[main\]' "${f}" && grep -q '^  pull_request:' "${f}"; then
        ok "${base}: push(main) + pull_request triggers"
    else
        fail "${base}: trigger shape wrong (need on: push{branches:[main]} + pull_request)"
    fi
    if grep -q '^permissions:' "${f}" && grep -q 'contents: read' "${f}" && ! grep -Eq 'contents: write|pull-requests: write' "${f}"; then
        ok "${base}: minimal permissions (contents: read, no write scopes)"
    else
        fail "${base}: permissions are not minimal read-only"
    fi
    if grep -q '^concurrency:' "${f}" && grep -Fq '${{ github.workflow }}-${{ github.ref }}' "${f}"; then
        ok "${base}: concurrency group keyed on workflow+ref"
    else
        fail "${base}: missing concurrency group"
    fi
done

# ═════════════════════════════════════════════════════════════════════════════
# 5. Language leg contents + pins
# ═════════════════════════════════════════════════════════════════════════════
# Go: vet + gofmt + test + govulncheck pin drift-checked against the Makefile.
for step in 'go vet ./...' 'gofmt -l' 'go test ./... -count=1' 'go-version-file: go.mod'; do
    if grep -Fq "${step}" "${CI_GO}"; then
        ok "go template carries: ${step}"
    else
        fail "go template missing: ${step}"
    fi
done
MK_PIN="$(grep -o 'govulncheck@v[0-9][0-9.]*' "${TREE_DIR}/Makefile" | head -1)"
TPL_PIN="$(grep -o 'govulncheck@v[0-9][0-9.]*' "${CI_GO}" | head -1)"
if [ -n "${MK_PIN}" ] && [ "${MK_PIN}" = "${TPL_PIN}" ]; then
    ok "govulncheck pin in lockstep with the org Makefile vuln-scan pin (${TPL_PIN})"
else
    fail "govulncheck pin drift: Makefile='${MK_PIN}' template='${TPL_PIN}'"
fi

# TS: tsc + vitest container + npm ci lockstep Node pins.
for step in 'npx tsc --noEmit' 'npx vitest run' 'image: node:24-bookworm' 'node-version: "24"' 'npm ci' 'cache: npm'; do
    if grep -Fq "${step}" "${CI_TS}"; then
        ok "ts template carries: ${step}"
    else
        fail "ts template missing: ${step}"
    fi
done
JOB_TS_TEST="$(job_block "${CI_TS}" "test")"
if printf '%s\n' "${JOB_TS_TEST}" | grep -Fq 'container:' && printf '%s\n' "${JOB_TS_TEST}" | grep -Fq 'image: node:24-bookworm'; then
    ok "ts test job runs vitest inside the pinned node container"
else
    fail "ts test job is not the pinned vitest container run"
fi

# Python: ruff pin + interpreter single-sourced via env + guarded legs.
for step in 'ruff==1.0.0' 'PYTHON_VERSION: "3.14"' "python-version: \${{ env.PYTHON_VERSION }}" 'python -m pytest -q' 'python -m pip install -e .'; do
    if grep -Fq "${step}" "${CI_PY}"; then
        ok "py template carries: ${step}"
    else
        fail "py template missing: ${step}"
    fi
done
JOB_PY_TEST="$(job_block "${CI_PY}" "test")"
if printf '%s\n' "${JOB_PY_TEST}" | grep -Fq "hashFiles('**/test_*.py') != '' || hashFiles('**/tests/**/*.py') != ''"; then
    ok "py pytest leg guarded on test-file presence"
else
    fail "py pytest leg not guarded on test-file presence"
fi

# Starter: exactly-one-language guard + make build + guarded language builds + smoke.
for step in 'exactly-one-language guard' 'make build' "hashFiles('go.mod') != ''" "hashFiles('package.json') != ''" "hashFiles('pyproject.toml') != ''" 'make -n smoke' 'make smoke' 'go build ./...' 'npm run build'; do
    if grep -Fq "${step}" "${CI_STARTER}"; then
        ok "starter carries: ${step}"
    else
        fail "starter missing: ${step}"
    fi
done

# osv scanner pin shared by the three language templates.
for f in "${CI_GO}" "${CI_TS}" "${CI_PY}"; do
    if grep -Fq 'ghcr.io/google/osv-scanner:v1.8.5' "${f}" && grep -Fq -- '--recursive /repo' "${f}"; then
        ok "$(basename "${f}"): pinned osv-scanner image over the whole tree"
    else
        fail "$(basename "${f}"): osv leg missing the pinned recursive scanner run"
    fi
done

# ═════════════════════════════════════════════════════════════════════════════
# 6. Action allowlist (org supply-chain discipline)
# ═════════════════════════════════════════════════════════════════════════════
ALLOWED_USES="actions/checkout@v7 actions/setup-go@v7 actions/setup-node@v4 actions/setup-python@v5 actions/cache@v4"
USES_OK=0
for f in "${CI_GO}" "${CI_TS}" "${CI_PY}" "${CI_STARTER}"; do
    while IFS= read -r use; do
        found=0
        for a in ${ALLOWED_USES}; do
            [ "${use}" = "${a}" ] && found=1
        done
        if [ "${found}" = "0" ]; then
            fail "$(basename "${f}"): uses '${use}' is outside the org action allowlist"
            USES_OK=1
        fi
    done < <(grep -o 'actions/[a-z-]*@v[0-9]*' "${f}" | sort -u)
done
if [ "${USES_OK}" = "0" ]; then
    ok "every uses: reference is within the org allowlist (${ALLOWED_USES})"
fi

# ═════════════════════════════════════════════════════════════════════════════
# 7. Dependabot policy variants
# ═════════════════════════════════════════════════════════════════════════════
for pair in "${DEP_GO}:go_modules" "${DEP_TS}:npm" "${DEP_PY}:pip"; do
    file="${pair%%:*}"
    eco="${pair#*:}"
    base="$(basename "${file}")"
    if grep -q '^version: 2' "${file}"; then
        ok "${base}: version 2"
    else
        fail "${base}: missing version: 2"
    fi
    if grep -qF 'package-ecosystem: "github-actions"' "${file}"; then
        ok "${base}: tracks github-actions (int maj action bumps land in the judgment lane)"
    else
        fail "${base}: missing github-actions ecosystem"
    fi
    if grep -qF "package-ecosystem: \"${eco}\"" "${file}"; then
        ok "${base}: language ecosystem ${eco}"
    else
        fail "${base}: missing language ecosystem ${eco}"
    fi
    ec="$(grep -c 'interval: "weekly"' "${file}" | tr -d ' ')"
    if [ "${ec}" -ge 2 ]; then
        ok "${base}: weekly schedule on every ecosystem (${ec} entries)"
    else
        fail "${base}: expected >=2 weekly entries, got ${ec}"
    fi
    ec="$(grep -cF 'version-update:semver-minor' "${file}" | tr -d ' ')"
    ec2="$(grep -cF 'version-update:semver-patch' "${file}" | tr -d ' ')"
    if [ "${ec}" -ge 2 ] && [ "${ec2}" -ge 2 ] && grep -qF 'minor-and-patch:' "${file}"; then
        ok "${base}: minor+patch grouped into one PR per ecosystem"
    else
        fail "${base}: minor-and-patch grouping miswired"
    fi
    # THE negative: majors must never be grouped/auto-acted — they surface as
    # individual judgment-lane PRs via the KB-407 classify gate #3.
    if grep -Fq 'version-update:semver-major' "${file}"; then
        fail "${base}: groups version-update:semver-major (majors must stay OUTSIDE every group)"
    else
        ok "${base}: no major grouping (majors -> judgment lane)"
    fi
done

if [ "$(grep -c '[^$-]{{' "${DEP_GO}" 2>/dev/null || true)" != "0" ]; then
    fail "dependabot-go.yml: unexpected template artifact"
fi

# Deeper policy validation when python3+yaml exist.
if command -v python3 >/dev/null 2>&1 && python3 -c 'import yaml' >/dev/null 2>&1; then
    python3 - "${DEP_GO}" "${DEP_TS}" "${DEP_PY}" <<'PYEOF' || fail "dependabot deep-shape validation failed"
import sys, yaml
for path in sys.argv[1:]:
    doc = yaml.safe_load(open(path))
    ups = doc.get("updates")
    assert doc.get("version") == 2, f"{path}: version != 2"
    assert len(ups) == 2, f"{path}: expected exactly 2 ecosystems, got {len(ups)}"
    ecos = [u["package-ecosystem"] for u in ups]
    assert "github-actions" in ecos, f"{path}: github-actions missing"
    for u in ups:
        assert u["schedule"]["interval"] == "weekly", f"{path}/{u['package-ecosystem']}: not weekly"
        g = u.get("groups", {}).get("minor-and-patch", {})
        assert sorted(g.get("update-types", [])) == sorted(
            ["version-update:semver-minor", "version-update:semver-patch"]
        ), f"{path}/{u['package-ecosystem']}: group types wrong"
        assert "commit-message" not in u, f"{path}/{u['package-ecosystem']}: unexpected commit-message override"
print("ok: dependabot deep shape (version/2 ecosystems/weekly/grouped minor+patch)")
PYEOF
else
    skip_loud "dependabot deep-shape leg (python3+pyyaml unavailable here; CI covers it)"
fi

# ═════════════════════════════════════════════════════════════════════════════
# 8. README mapping vs disk
# ═════════════════════════════════════════════════════════════════════════════
for f in "${CI_GO}" "${CI_TS}" "${CI_PY}" "${CI_STARTER}" "${DEP_GO}" "${DEP_TS}" "${DEP_PY}"; do
    if grep -qF "$(basename "${f}")" "${README}"; then
        ok "README maps: $(basename "${f}")"
    else
        fail "README missing mapping for $(basename "${f}")"
    fi
done
README_OK=1
for ctx in '`changes`, `lint`, `test`, `govulncheck`, `osv`' '`changes`, `typecheck`, `test`, `osv`' '`changes`, `lint`, `test`, `osv`' '`changes`, `build-parity`, `smoke`'; do
    if grep -qF "${ctx}" "${README}"; then :; else README_OK=0; fi
done
if [ "${README_OK}" = "1" ]; then
    ok "README carries all four required-check context rows"
else
    fail "README required-check rows drifted from the job sets"
fi

# ═════════════════════════════════════════════════════════════════════════════
# 9. BEHAVIOURAL: execute the extracted KB-404 gate against real fixture repos
# ═════════════════════════════════════════════════════════════════════════════
GATE_SH="${WORK_ROOT}/gate.github-ci-go.yml.sh"

fixture_repo() { # <dir> — bare origin + clone, main seeded with go.mod + README.md
    git init -q --bare -b main "$1/origin.git"
    git clone -q "$1/origin.git" "$1/repo"
    git -C "$1/repo" config user.email ci@example.invalid
    git -C "$1/repo" config user.name ci
    echo "module example.com/seed" >"$1/repo/go.mod"
    echo "seed repo" >"$1/repo/README.md"
    git -C "$1/repo" add -A
    git -C "$1/repo" commit -qm "seed"
    git -C "$1/repo" push -q origin main
}

branch_commit() { # <repo> <branch> <path> <content> -> head sha
    git -C "$1" checkout -q -b "$2" main
    mkdir -p "$(dirname "$1/$3")"
    printf '%s\n' "$4" >"$1/$3"
    git -C "$1" add -A
    git -C "$1" commit -qm "scenario: $3"
    git -C "$1" rev-parse HEAD
}

base_sha() {
    git -C "$1" rev-parse main
}

run_gate() { # <repo> <event> <head_sha> <outdir> -> echoes docs_only value
    local repo="$1" event="$2" sha="$3" out="$4"
    mkdir -p "${out}"
    (cd "${repo}" &&
        EVENT_NAME="${event}" PR_BASE_REF=main PR_HEAD_SHA="${sha}" \
            GITHUB_OUTPUT="${out}/out" GITHUB_STEP_SUMMARY="${out}/summary" \
            bash "${GATE_SH}") >"${out}/stdout" 2>"${out}/stderr"
    sed -n 's/^docs_only=//p' "${out}/out" | tail -1
}

# docs-only: only the fail-safe docs set changed -> heavy jobs SKIP.
FX="${WORK_ROOT}/fx-docs"
mkdir -p "${FX}"
fixture_repo "${FX}"
sha="$(branch_commit "${FX}/repo" pr "docs/guide.md" "# guide")"
sha="$(branch_commit "${FX}/repo" pr2 "notes.md" "# notes")"
require_equal "$(run_gate "${FX}/repo" pull_request "${sha}" "${WORK_ROOT}/out1")" "true" "docs-only PR (two .md files) -> heavy jobs SKIP"
require_equal "$(sed -n 's/^reason=//p' "${WORK_ROOT}/out1/out" | tail -1)" \
    "every changed file is a fail-safe docs path: skipping heavy jobs" \
    "docs-only reason is typed"

# mixed diff: docs + code -> full.
FX="${WORK_ROOT}/fx-mixed"
mkdir -p "${FX}"
fixture_repo "${FX}"
sha="$(branch_commit "${FX}/repo" pr "notes.md" "# notes")"
sha="$(branch_commit "${FX}/repo" pr2 "helper.go" "package main")"
require_equal "$(run_gate "${FX}/repo" pull_request "${sha}" "${WORK_ROOT}/out2")" "false" "mixed diff (md + go) -> full run"
if grep -qF "full run: helper.go is not a docs path" "${WORK_ROOT}/out2/out"; then
    ok "mixed-diff reason names the first non-docs path"
else
    fail "mixed-diff reason does not name the code path"
fi

# push event: ALWAYS full, even for an all-docs change.
FX="${WORK_ROOT}/fx-push"
mkdir -p "${FX}"
fixture_repo "${FX}"
sha="$(base_sha "${FX}/repo")"
require_equal "$(run_gate "${FX}/repo" push "${sha}" "${WORK_ROOT}/out3")" "false" "push event -> always full"

# repo-state files under .github/ are NEVER docs.
FX="${WORK_ROOT}/fx-dotgithub"
mkdir -p "${FX}"
fixture_repo "${FX}"
sha="$(branch_commit "${FX}/repo" pr ".github/dependabot.yml" "version: 2")"
require_equal "$(run_gate "${FX}/repo" pull_request "${sha}" "${WORK_ROOT}/out4")" "false" ".github/dependabot.yml change -> full run"
sha="$(branch_commit "${FX}/repo" pr2 ".github/workflows/other.yml" "on: push")"
require_equal "$(run_gate "${FX}/repo" pull_request "${sha}" "${WORK_ROOT}/out5")" "false" ".github/workflows change -> full run"

# fail-safe: ambiguous or code-bearing paths keep the full run.
FX="${WORK_ROOT}/fx-unsafe"
mkdir -p "${FX}"
fixture_repo "${FX}"
sha="$(branch_commit "${FX}/repo" pr "docs/generated.go" "package docs")"
require_equal "$(run_gate "${FX}/repo" pull_request "${sha}" "${WORK_ROOT}/out6")" "false" "code under docs/ -> full run (fail-safe)"
sha="$(branch_commit "${FX}/repo" pr2 ".gitignore" "tmp/")"
require_equal "$(run_gate "${FX}/repo" pull_request "${sha}" "${WORK_ROOT}/out7")" "false" ".gitignore change -> full run (fail-safe)"

# the rest of the fail-safe docs set: LICENSE*, CODEOWNERS, nested md, docs images, rst/mdx.
FX="${WORK_ROOT}/fx-set"
mkdir -p "${FX}"
fixture_repo "${FX}"
sha="$(branch_commit "${FX}/repo" pr "api/README.md" "# api")"
require_equal "$(run_gate "${FX}/repo" pull_request "${sha}" "${WORK_ROOT}/out8")" "true" "nested code-tree README -> SKIP"
sha="$(branch_commit "${FX}/repo" pr2 "docs/diagram.png" "binary-ish")"
require_equal "$(run_gate "${FX}/repo" pull_request "${sha}" "${WORK_ROOT}/out9")" "true" "docs/ image asset -> SKIP"
sha="$(branch_commit "${FX}/repo" pr3 "LICENSE" "MIT")"
require_equal "$(run_gate "${FX}/repo" pull_request "${sha}" "${WORK_ROOT}/out10")" "true" "LICENSE -> SKIP"
sha="$(branch_commit "${FX}/repo" pr5 "CODEOWNERS" "* @org/owners")"
require_equal "$(run_gate "${FX}/repo" pull_request "${sha}" "${WORK_ROOT}/out11")" "true" "CODEOWNERS -> SKIP"
sha="$(branch_commit "${FX}/repo" pr6 "design.rst" "heading")"
require_equal "$(run_gate "${FX}/repo" pull_request "${sha}" "${WORK_ROOT}/out12")" "true" "rst -> SKIP"
sha="$(branch_commit "${FX}/repo" pr7 "doc.mdx" "heading")"
require_equal "$(run_gate "${FX}/repo" pull_request "${sha}" "${WORK_ROOT}/out13")" "true" "mdx -> SKIP"

# degenerate inputs stay full and never red the gate.
FX="${WORK_ROOT}/fx-empty"
mkdir -p "${FX}"
fixture_repo "${FX}"
sha="$(base_sha "${FX}/repo")"
require_equal "$(run_gate "${FX}/repo" pull_request "${sha}" "${WORK_ROOT}/out14")" "false" "empty diff (head == base) -> full run"
require_equal "$(run_gate "${FX}/repo" pull_request "0000000000000000000000000000000000000000" "${WORK_ROOT}/out15")" "false" "dangling PR head sha -> fail-safe full, rc 0"
if grep -q "merge-base against" "${WORK_ROOT}/out15/out"; then
    ok "dangling-head reason names the merge-base failure"
else
    fail "dangling-head reason is untyped"
fi
for n in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15; do
    if [ -s "${WORK_ROOT}/out${n}/stderr" ] && grep -Eq 'Traceback|syntax error' "${WORK_ROOT}/out${n}/stderr"; then
        fail "gate run ${n} left unexpected tool output on stderr: $(cat "${WORK_ROOT}/out${n}/stderr")"
    fi
done
if grep -q "docs-only gate:" "${WORK_ROOT}/out1/summary"; then
    ok "gate writes the docs-only verdict into GITHUB_STEP_SUMMARY"
else
    fail "gate never wrote the step summary"
fi

# ═════════════════════════════════════════════════════════════════════════════
# 10. Secrets hygiene (CRI-250 patterns) over the whole pack
# ═════════════════════════════════════════════════════════════════════════════
LEAK=0
for f in "${ALL_TPL_FILES[@]}"; do
    if grep -Eq 'gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|-----BEGIN (RSA |EC |OPENSSH )?PRIVATE KEY-----|hvs\.[A-Za-z0-9]{20,}|https://[^ ]*[A-Za-z0-9]+:[^ /@ ]+@|"token": *"[A-Za-z0-9]"' "${f}"; then
        fail "secret-shape literal leaked into $(basename "${f}")"
        LEAK=1
    fi
done
if [ "${LEAK}" = "0" ]; then
    ok "no secret-shape literals anywhere in the pack (CRI-250 patterns)"
fi

# ═════════════════════════════════════════════════════════════════════════════
# 11. Tree still validates (grammar + interface contract)
# ═════════════════════════════════════════════════════════════════════════════
if command -v node >/dev/null 2>&1; then
    set +e
    (cd "${TREE_DIR}" && node scripts/interface_validate.mjs) >/dev/null 2>&1
    rc_node=$?
    (cd "${TREE_DIR}" && "${CRITERIA}" validate devops_maint_v1) >"${WORK_ROOT}/validate.out" 2>&1
    rc_crit=$?
    set -e
    if [ "${rc_node}" = "0" ]; then
        ok "interface contract validates (all trees)"
    else
        fail "interface contract validation failed (rc=${rc_node})"
    fi
    if [ "${rc_crit}" = "0" ]; then
        ok "criteria accepts the devops_maint_v1 tree"
    else
        fail "criteria validate devops_maint_v1 failed (rc=${rc_crit})"
    fi
    if grep -q '^devops_maint_v1: ok' "${WORK_ROOT}/validate.out" 2>/dev/null; then
        ok "criteria reports devops_maint_v1 ok"
    else
        fail "criteria output missing the ok line"
    fi
else
    skip_loud "tree-validate leg (node unavailable here; the CI image covers it)"
fi

if [ "${FAILED}" -ne 0 ]; then
    echo "FAILED: ${FAILED} assertion(s)" >&2
    exit 1
fi
echo "all assertions passed" >&2
