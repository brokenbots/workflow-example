# linear_intake_v1 — Linear ticket intake to release

Takes a Linear ticket identifier, pulls the ticket from the Linear GraphQL API,
classifies it as a bug or feature request, and then either:

- labels bugs and runs `qa_triage_v1` before implementation,
- labels concrete feature requests and sends their generated workstream
  directly to `workstream_handler_v1`, or
- moves ambiguous or failed intake to `linear_review_state` with context.

Each invocation starts without workflow resume state. The Linear issue is the
durable process record: intake reads its current state, labels, description,
and complete chronological comment history. Human replies after an automated
question are treated as new input on the next run. Local report and workstream
files are overwritten from that issue history rather than trusted as state.

An explicit earlier QA confirmation in the issue history allows a bug to skip
duplicate reproduction: intake writes a fresh confirmed-bug workstream and
sends it to the handler. State or labels alone are not confirmation. Completed
or canceled Linear state types stop without changing the issue.

## Run

```sh
export LINEAR_API_KEY=lin_api_...
export INTAKE_GITHUB_TOKEN=...           # copilot adapter auth for this workflow's agents
export TRIAGE_GITHUB_TOKEN=...           # qa_triage_v1's agents (investigator, supervisor)
# workstream_handler_v1 (only reached on valid findings):
export COORDINATOR_GITHUB_TOKEN=...      # handler root, branch_manager, pair_programming_loop
export REVIEWER_GITHUB_TOKEN=...         # pr_reviewer_loop

criteria adapter lock linear_intake_v1   # recursive: locks this dir, both
                                         # subworkflows, and their subworkflows
criteria apply linear_intake_v1 \
  --var ticket_id=CRI-32 \
  --var-file varfiles/linear_intake_v1.chcl
```

A missing handler token fails the run at `run_handler` with
`adapter "coordinator" secret "GITHUB_TOKEN": ... "COORDINATOR_GITHUB_TOKEN" is
not set` and routes the ticket to the review state with a failure comment —
annoying but safe. The GitHub identities behind those tokens must match
`coordinator_gh_user` / `reviewer_gh_user` (the handler verifies the active
`gh auth` account against them).

Host requirements: `curl`, `jq`, and the subworkflows' own requirements
(git, gh, build/test tooling for the repo under test).

## Layout

Per-ticket artifacts under `<intake_root>/<ticket_id>/`:

| path | what |
|---|---|
| `ticket.json` | raw Linear issue payload |
| `<ticket_id>.md` | classified bug report — qa_triage_v1's input |
| `workstreams/<ticket_id>.md` | feature workstream or QA-confirmed bug workstream |
| `worktree/` | workstream_handler_v1's isolated worktree |
| `review-notes.md` | triage reviewer's reasoning, posted to Linear |
| `intake-notes.md` | actionable intake questions posted when human input is required |

Triage evidence lives separately at `<triage_root>/<ticket_id>/evidence/`
(qa_triage_v1 derives its run slug from the bug report's basename).

## Linear integration

All Linear I/O is the GraphQL API (`https://api.linear.app/graphql`) via curl
on the shell adapter. The in-tree criteria `mcp` adapter
(`cmd/criteria-adapter-mcp` in brokenbots/criteria) was considered and
rejected: it is stdio-only, has no output schema (tool results never reach the
workflow as data — only outcomes do), and gates every tool call on a host
permission decision.

- `fetch_ticket` queries `issue(id:)`; Linear accepts the human identifier
  ("CRI-32") in place of the UUID. The classifier then writes either a bug
  report or a feature workstream.
- `set_bug_label` and `set_feature_label` preserve unrelated labels, replace
  the opposing classification label, and apply the configured team label
  before the downstream workflow starts. If the configured label does not
  exist, the opposing label is still removed and routing continues; the
  workflow does not create team-wide labels implicitly.
- Comments use `commentCreate`; state changes use `issueUpdate` with the state
  id resolved by name via `workflowStates` filtered to the ticket's team
  (the `team` filter takes an `ID!` variable — `String!` fails GraphQL
  validation with a bare HTTP 400). The default `linear_review_state` is
  Linear's built-in "In Review"; set it to a custom state (e.g. "Human
  Review") only if that state exists on the team. An unknown name fails the
  step loudly.
- `LINEAR_API_KEY` must be exported before `criteria apply`. The adapter block
  also declares it under `secrets`, but whether the shell adapter surfaces
  declared secrets as command env vars is unverified — export is the reliable
  path. The key is never interpolated into step input.

## Couplings to be aware of

- **qa_triage_v1 outputs.** The parent captures `subworkflow.verdict` and
  `subworkflow.workstream_file` on both child outcomes. Criteria v0.5.9
  re-seeds switches from the current DataStore before evaluating conditions,
  so the parent route sees those writes without a persisted-file handoff.
- **One value per step for data writes.** The engine's `applyDataWrites`
  skips (does not fail) a write whose value resolves to nil, so splitting one
  step's stdout into several writes with `regexall(...)[0][0]`-style
  expressions fails invisibly. Every workflow in this suite captures exactly
  one value per step via `write { value = output.stdout }` — keep it that way.
- **Basename-derived slug.** qa_triage_v1 derives its run slug from the bug
  report's basename; the report is therefore written as `<ticket_id>.md`, which
  pins the evidence path this workflow's reviewer reads.
- **worktree_dir.** workstream_handler_v1's branch_manager creates the
  worktree; this workflow only chooses the path
  (`<intake_root>/<ticket_id>/worktree`).
- **Artifact authoring.** The intake classifier writes bug reports and feature
  workstreams with its file tools, never through shell interpolation. Linear
  comments use jq arguments or `--rawfile` for the same reason.
