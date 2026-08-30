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
export WORKFLOW_GITHUB_TOKEN=...          # all GitHub work except PR review
export REVIEWER_GITHUB_TOKEN=...          # dedicated PR reviewer identity

criteria adapter lock linear_intake_v1   # recursive: locks this dir, both
                                         # subworkflows, and their subworkflows
criteria apply linear_intake_v1 \
  --var ticket_id=CRI-32 \
  --var-file varfiles/linear_intake_v1.chcl
```

A missing token fails at the first adapter that needs it and routes the ticket
to the review state with a failure comment. Startup resolves both tokens through
`gh api user` and rejects them when they identify the same account.

Host requirements: `curl`, `jq`, and the subworkflows' own requirements
(git, gh, build/test tooling for the repo under test).

### Container

The container is the primary operator interface. Copy `.env.example` to a
private env file, set the three credentials, `TICKET_ID`, and `REPO_URL`,
then run the ticket workflow with:

```sh
docker compose --env-file linear_intake_v1/.env \
  -f linear_intake_v1/compose.yaml run --rm linear-intake
```

The default Compose run uses no mounts. The entrypoint verifies both identities
without storing either credential, clones `REPO_URL` to `/repo` with a
one-command `GH_TOKEN` assignment using the non-review token, writes only
non-secret workflow settings to a temporary JSON varfile, configures Git commit
identity from the non-review GitHub login, and runs
`linear_intake_v1`. A custom deployment
may instead mount an existing checkout at `REPO_DIR`; cloning is skipped when
that path is already a Git repository. Without a `/data` volume, run artifacts
are intentionally ephemeral. Extend the Dockerfile when the target repository
needs build tools beyond the included Go/Make toolchain.
`PROVIDER_BASE_URL` defaults to the host's Ollama-compatible endpoint through
`host.docker.internal` and is passed to every agent in the composed workflow.
The Dockerfile installs the published, architecture-specific Criteria v0.5.11
release after verifying the archive against that release's `SHA256SUMS` file.
Version updates therefore change only `CRITERIA_VERSION`; the matching archive
digest is selected from the release manifest at build time. It does not compile
or patch Criteria, adapters, or any other software. Both amd64 and arm64 are
supported. Criteria state and locked adapters use the current
`/home/criteria/.local/criteria` root rather than the legacy `~/.criteria` path.
The image also installs GitHub CLI, the pinned GitHub Copilot and Linear CLIs,
and common Go repository development tools. It uses a glibc-based image because
the Copilot CLI native addon does not load reliably on Alpine/musl. `/bin/sh`
resolves to Bash because the shell adapter invokes `sh -c` and the workflow
scripts require `pipefail`. Compose selects concise output explicitly so step
outcomes and terminal failures remain visible in a non-TTY stream.
The Docker build runs `criteria adapter lock` recursively. Adapter references
and versions come only from the workflow declarations; Criteria verifies their
signatures and lockfile digests and populates the image's OCI cache. No adapter
source build or unsigned adapter install is used. Compose sets only
`seccomp=unconfined` so Criteria can create its nested namespaces; each adapter
then runs under Criteria's own narrower seccomp policy.

All workflow execution environments use Criteria's sandbox runtime. It scrubs
token-like host variables before launching an adapter, then the secret channel
injects only the names declared on that adapter. Coordinator shell and Copilot
adapters receive `WORKFLOW_GITHUB_TOKEN` as `GH_TOKEN`/`GITHUB_TOKEN`; the PR
review child receives only `REVIEWER_GITHUB_TOKEN`; the Linear status adapter
receives only `LINEAR_API_KEY`. No `gh auth login` or identity switching is used.

The executable regression workflow at `tests/credential_isolation` uses
sentinel values to verify both adapter roles receive their own `GH_TOKEN` and
cannot see `WORKFLOW_GITHUB_TOKEN`, `REVIEWER_GITHUB_TOKEN`, or
`LINEAR_API_KEY` from the host environment:

```sh
docker run --rm --security-opt seccomp=unconfined \
  --entrypoint /usr/local/bin/criteria \
  -e LINEAR_API_KEY=linear-sentinel \
  -e WORKFLOW_GITHUB_TOKEN=workflow-sentinel \
  -e REVIEWER_GITHUB_TOKEN=reviewer-sentinel \
  linear_intake_v1-linear-intake:latest \
  apply /workflows/linear_intake_v1/tests/credential_isolation
```

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
- Classified bugs move to `linear_triage_state` before `qa_triage_v1` starts,
  so Linear reports `Triage` throughout verification.
- The parent moves accepted work to `linear_work_state` before invoking the
  handler and to `linear_done_state` after the handler reports a merged PR.
- Before every initial PR review or re-review, `workstream_handler_v1` resolves
  the same state against the ticket's team and moves the ticket there. This
  keeps Linear in review while GitHub is in review, including feedback cycles.
- `LINEAR_API_KEY` must be exported before `criteria apply`. The adapter resolves
  it from the host environment and injects it into the shell command through
  the secret channel after sandbox scrubbing. The key is never interpolated
  into step input.

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
