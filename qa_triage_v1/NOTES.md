# qa_triage_v1 — bug report in, validated workstream file out

Takes a raw bug report and either produces a workstream file backed by an
executed reproduction, or closes the report with a triage record explaining why
no workstream was warranted. It never writes code, never commits, and never
opens a PR. The workstream file is written to disk and the run stops — a human
queues it into `workstream_handler_v1`.

## The constraint that defines it

A workstream file cannot be emitted unless a reproduction was **executed** and a
**second model** independently confirmed the evidence matches the *reported*
symptom.

This exists because the opposite is the default failure mode: an investigator
reproduces *some* behavior, recognizes it as similar to the report, and declares
the bug confirmed — producing a specification for a fix to a bug that was never
observed, and sometimes for behavior that was intended design all along.

## Shape

```
read_report → prepare_workspace → resolve refs → [G0 intake]
  → plan_repro ⇄ [G1 plan]
  → repro_runner(main) → repro_runner(stable)
  → aggregate → [G2 verdict]
  → draft_workstream ⇄ [G3 final] → publish → triage report → teardown
```

`repro_runner` runs once per ref: worktree → build → baseline tests →
provided reproduction → investigator execution → result.

## Roles

| role | model | authority |
|---|---|---|
| investigator | `kimi-k2.7-code:cloud` | plans, builds, executes, drafts. Never decides a bug is real. |
| supervisor | `glm-5.2:cloud` | gates G0–G3. Sole authority to emit a workstream file. |

Deliberately different model families. A supervisor sharing the investigator's
blind spots provides no independent check, which is the whole point of the role.

Four gates rather than one end-check, because a bad plan should die before it
costs a build and a reproduction cycle:

- **G0 intake** — is the report actionable at all? Rejects vague reports for the
  price of one supervisor turn.
- **G1 plan** — is the test plan adequate, *before* execution?
- **G2 verdict** — does the evidence support the claim?
- **G3 final** — approve or reject the drafted workstream.

## The ref matrix

Tests `main` and the latest stable tag by default. The delta is the finding:

| main | stable | verdict |
|---|---|---|
| repro | repro | `reproduced` → workstream |
| repro | clean | `regression` → workstream, flagged |
| clean | repro | `already_fixed` → triage report, no fix workstream |
| clean | clean | `not_reproduced` → triage report |

This is what turns "maybe they were on an old build" from a hypothesis into a
measured result. `inconclusive` on either arm is never rounded to `no`.

## Where the triage team works

Three distinct trees, and the distinction is the whole point:

| tree | who uses it | writable | lifetime |
|---|---|---|---|
| `repo_dir` — the operator's checkout | deterministic shell steps (git, ref resolution) | **no** | permanent |
| `<triage_root>/scratch` — detached at mainline | investigator + supervisor, all root phases | **yes, freely** | discarded at teardown |
| `<run_dir>/wt/<ref>` — detached at each ref | investigator inside `repro_runner` | **yes, freely** | discarded per arm |

Agents get real checkouts they own outright. They may write reproduction
harnesses, add tests, instrument, and patch code to test a hypothesis — a
hypothesis you cannot test is one you cannot rule out.

What they may never touch is `repo_dir`. The `assert_repo_clean` step checks it
before the verdict gate and aborts with `aborted_repo_modified` if it is dirty,
because evidence gathered while the operator's tree was being edited cannot be
attributed to the refs under test. It does **not** revert — discarding changes
in someone's working tree unattended is not this workflow's call.

Measurements taken from a patched tree are legitimate **when labelled**. Both
investigator prompts require recording what was changed alongside the number.

`triage_root` defaults to `/tmp/criteria-triage` and is rejected at startup if
it points inside `repo_dir`.

## Context discipline

Every artifact goes to `<triage_root>/<slug>/evidence/`. Prompts pass **paths,
not contents**. Each phase is a fresh adapter session, and the supervisor reads
evidence files — never the investigator's transcript — so a poisoned
investigator context cannot propagate into the gate that authorizes a workstream.

Retry budgets are bounded at every gate (`max_visits`).

## Running it

```sh
export WORKFLOW_GITHUB_TOKEN="$(gh auth token)"

criteria apply qa_triage_v1 \
  --var bug_report_file=/abs/path/to/report.md \
  --var repo_dir=/abs/path/to/repo \
  --var workstream_out_dir=/abs/path/to/workstreams \
  --var 'build_cmd=make build' \
  --var 'test_cmd=make test' \
  --var test_refs=both
```

Optional: `--var repro_workflow_dir=...` when the report ships a reproduction.
It is then run **first and unmodified** on every ref, and its result is the
primary evidence — an investigator-authored substitute tests whatever the
investigator understood the report to mean.

Set `build_cmd` for any compiled project. A reproduction against a stale tree
proves nothing.

Terminal states: `workstream_published` (success), `closed_no_workstream`
(success — a report that does not reproduce is a legitimate result),
`needs_human`, `failed`.

## Refresh procedure

Locking is not recursive (see
`workstreams/recursive-lock-and-workflow-refs.md`), so lock both directories:

```sh
criteria adapter lock qa_triage_v1
criteria adapter lock qa_triage_v1/workflows/repro_runner
criteria compile qa_triage_v1
```

## Gotchas hit while building this

- **Every variable referenced from adapter config needs a default.** A required
  variable leaves the config unknown at compile time, so `repo_dir` and
  `worktree_dir` default to `""` and are always passed at apply time.
- **Shell parameter expansion collides with HCL interpolation.** `${slug%.*}`
  inside a heredoc is parsed by HCL. Escape it as `$${slug%.*}`.
- `.triage/` under the repo holds throwaway worktrees and evidence — add it to
  the target repo's `.gitignore`.
