# devops_maint_v1 — Lane B CI template pack (KB-408, ADR-0015 wave 3)

This directory ships the per-language CI-compliance templates from the
ADR-0015 Lane B language pack (anchor: KB-405; KB-404 design; see
`docs/devops-maint-adr.md`):

- four GitHub Actions workflow templates (`github-ci-{go,ts,py,starter}.yml`),
  each carrying the docs-only PR skip for repo-state heavy jobs;
- three per-repo `dependabot.yml` policy variants
  (`dependabot-{go,ts,py}.yml`).

`devops_maint_v1/tests/test_ci_templates_standalone.sh` (wired into the
Makefile `ci` leg and the CI suite leg) is the assertion of record for this
directory.

## Template-to-repo mapping (application contract, KB-409)

| Repo language | CI template applied as `.github/workflows/ci.yml` | Dependabot variant applied as `.github/dependabot.yml` | Required-check contexts recorded by KB-409 |
| --- | --- | --- | --- |
| Go | `github-ci-go.yml` | `dependabot-go.yml` | `changes`, `lint`, `test`, `govulncheck`, `osv` |
| TypeScript | `github-ci-ts.yml` | `dependabot-ts.yml` | `changes`, `typecheck`, `test`, `osv` |
| Python | `github-ci-py.yml` | `dependabot-py.yml` | `changes`, `lint`, `test`, `osv` |
| Starter (go/ts/py starter deck) | `github-ci-starter.yml` | the starter's language variant | `changes`, `build-parity`, `smoke` |

Application rules:

- Files land **byte-exact**: these are plain `.yml` files, not `.sh.tftpl`
  templates. GitHub Actions `${{ }}` expressions are Go text/template syntax,
  which the devops_maint_v1 engine renders through templatefile(3) —
  rendering would mangle expressions. Application (KB-409) writes via
  `file()` and reads the file back to verify the exact bytes.
- Triggers are fixed by the templates: `push` to `main` (always a full run)
  and `pull_request` (full run except docs-only PRs). Templates never gate
  `push` on docs state: push-to-main always runs full.
- Required checks: the job `name:` values are the KB-409 protection contract
  and MUST stay in lockstep with this table. On docs-only PRs the heavy jobs
  record a `skipped` conclusion, which branch protection treats as satisfied,
  so listing heavy contexts stays safe.

## Docs-only PR skip (KB-404 design)

Every heavy job in every template carries:

- `needs: changes` — the classification gate job;
- `if: needs.changes.outputs.docs_only == 'false'`.

Gate semantics (fail-safe direction: the skip requires positive evidence):

- any non-`pull_request` event (i.e. push-to-main) → full run;
- mixed diff (docs + anything else) → full run;
- docs-only PR (every changed file matches the fail-safe docs set:
  `*.md`, `*.markdown`, `*.mdx`, `*.rst`, `docs/` image assets, `LICENSE`,
  `LICENSE.*`, `CODEOWNERS`) → heavy jobs skip;
- any non-documentation state file under `.github/` (workflows, dependabot
  policy) and any code file under any directory → full run — the docs-set
  patterns, not the directory, decide, so a stray `.md` under `.github/`
  (PR/issue templates) correctly matches the docs set and skips;
- `docs/` image assets match top-level `docs/*.ext` only; a nested asset
  (e.g. `docs/assets/x.png`) deliberately keeps the full run (conservative,
  fail-safe direction);
- detection tool failure or empty diff → full run, gate exits 0 (infra
  failures never red-block a PR from the gate job and never skip without
  evidence).

The `changes` gate block is byte-identical across all four templates
(drift-checked by the suite), so the classification contract cannot silently
diverge between languages.

## Dependabot policy (per-repo `.github/dependabot.yml`)

- `version: 2`, every in-scope ecosystem on a `weekly` schedule;
- a `minor-and-patch` group collecting
  `version-update:semver-minor` + `version-update:semver-patch` into ONE PR
  per ecosystem;
- **no semver-major entry, ever**: majors fall outside the group and arrive as
  individual PRs, which the devops_maint_v1 classify gate (KB-407, gate #3)
  routes to the `needs_judgment` lane instead of auto-action;
- every variant also tracks `github-actions` — integer action majors
  (`actions/checkout@v6 → v7`) are exactly the judgment-lane class.

## Pin discipline

- `govulncheck@v1.9.0` in `github-ci-go.yml` must match the root Makefile
  `vuln-scan` target pin — the template suite cross-checks the literals and
  reds on drift.
- `ghcr.io/google/osv-scanner:v1.8.5` is the CI scanner pin. The templates
  pass args straight through the image's own `ENTRYPOINT ["/osv-scanner"]`
  (verified against the published v1.8.5 image config for linux/amd64 and
  linux/arm64): no `--entrypoint` override, because the binary lives at
  `/osv-scanner`, off the container PATH, so a
  bare-name override would red every osv leg instantly. The tag is a mutable
  org-managed pin, not a digest; digest pinning is deliberately out of scope
  for these plain `.yml` templates. The collect sweep (KB-406) runs the
  operator-provided `osv-scanner` from PATH with the same v1 CLI shape
  (`--recursive`). The suite asserts the exact docker-run invocation so any
  drift back toward a broken override reds.
- `pytest==9.2.0` in `github-ci-py.yml` bootstraps the test leg: setup-python
  provisions a bare interpreter, so the pytest runner must be installed
  explicitly (latest stable at pin time, 2026-11-15; Python 3.14 explicitly
  supported per its PyPI classifiers). The pytest step keeps its test-file
  presence guard, so test-less repos degrade instead of red.
- `ruff==0.17.5` in `github-ci-py.yml` pins the lint leg: latest stable
  release, verified against the PyPI JSON API on 2026-11-15 (no `ruff` 1.x
  has ever been published). Every published artifact is a
  Python-version-agnostic `py3-none` wheel with explicit Python 3.14
  support, so `python -m ruff` runs on the template's pinned interpreter.
  The suite extracts the pin from the template itself and probes PyPI for
  that exact release (loud-skip on transport trouble), so a non-resolvable
  pin cannot be locked in again.
- Interpreter/container/tool pins (`go-version-file: go.mod` for Go; Node 24
  as setup-node `"24"` + `node:24-bookworm`, lockstep-asserted; Python
  `"3.14"` via the workflow-level `PYTHON_VERSION` env; `ruff==0.17.5`) are
  org-managed literals; the suite asserts every pin is present in its
  expected form.
- Actions are allowlisted to the org set; the suite reds on any `uses:` not
  in: `actions/checkout@v7`, `actions/setup-go@v7`, `actions/setup-node@v4`,
  `actions/setup-python@v5`, `actions/cache@v4`.

## Tests

Run via `make ci` (or directly):

```
bash devops_maint_v1/tests/test_ci_templates_standalone.sh
```

The suite covers the full template shape (structure, skips, pins, triggers,
required-check names, dependabot policy incl. the no-major negative) AND the
behavioral gate contract by extracting the KB-404 detection script and
running it against real fixture git repositories (docs-only, mixed, push,
empty diff, `.github/` state files, nested docs, detection-failure fallback).
The YAML-parse leg requires `python3` + PyYAML and skips loudly when the
local toolchain lacks them (CI provides both).
