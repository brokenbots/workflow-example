# workstream_handler_v1 — adapter/model pins

Every `adapter` block is pinned to an OCI `source` + `version` (the criteria
v0.5 "adapter-v2" model). All pins lock and compile end-to-end on this host
(darwin/arm64): `criteria adapter lock <dir>` resolves each adapter's digest
and `criteria compile <dir>` succeeds for the root workflow and all three
subworkflows.

## PR reviewer loop retry behavior (CRI-91)

`workflows/pr_reviewer_loop/main.chcl` bounds retries when the reviewer copilot
adapter returns a malformed tool-call or missing `submit_outcome`.

- A `failure` or `default` outcome from `step.pr_review` increments
  `data.internal._review_attempts` and routes through
  `switch.route_review_retry`.
- The switch re-enters `step.pr_review` only while
  `_review_attempts < max_review_visits`, so the existing visit budget is the
  hard ceiling and the loop cannot run forever.
- The repair prompt (`agents/review_pr.md.tftpl`) includes the previous error
  context on retry passes, telling the reviewer to emit a valid outcome.
- Once a valid `approve` or `changes_requested` outcome is returned, the repair
  context is cleared.

## Pins (as of 2026-07-28)

| adapter | source | version | notes |
|---|---|---|---|
| copilot      | `ghcr.io/brokenbots/criteria-adapter-copilot`      | `0.5.2` | multi-arch, signed. Used for every agent role: developer, coordinator, branch-repair, pair-loop reviewer, PR reviewer. |
| shell        | `ghcr.io/brokenbots/criteria-adapter-shell`        | `0.5.2` | multi-arch, keyless-signed. |

All agent roles now run on the copilot adapter; the `claude-agent` adapter is
no longer used anywhere in this workflow.

## Model pins (as of 2026-07-28)

| role | adapter | model |
|---|---|---|
| developer (pair loop)  | copilot | `kimi-k2.7-code:cloud` (Ollama cloud — `kimi-k3:cloud` is extra-usage-only on the current plan; `kimi-k2.7-code:cloud` is the latest included kimi and is coding-tuned) |
| coordinator (root)     | copilot | `minimax-m3:cloud` (Ollama cloud) |
| branch repair          | copilot | `minimax-m3:cloud` (Ollama cloud) |
| reviewer (pair loop)   | copilot | `glm-5.2:cloud` (Ollama cloud) |
| PR reviewer            | copilot | `glm-5.2:cloud` (Ollama cloud) |

Every copilot role resolves through the local Ollama endpoint
(`http://localhost:11434/v1`, `responses` wire API); pull the cloud models with
`ollama pull kimi-k2.7-code:cloud minimax-m3:cloud glm-5.2:cloud`. No `claude`
CLI is required — there are no claude-agent roles left.

## Git push credentials for agent shells (CRI-95)

Agent-driven shells in `pair_programming_loop` and the coordinator push step
need to push to GitHub, but the container-wide `git config` points to
`gh auth git-credential`, which does **not** resolve a token from the
environment.  Sandboxed adapter shells only have the injected
`GH_TOKEN` / `GITHUB_TOKEN` / `WORKFLOW_GITHUB_TOKEN`, so pushes must use a
credential helper that reads from those variables at runtime.

The shared helper lives in
`scripts/_github_token_git_credentials.sh.tftpl`.  It defines
`setup_gh_token_git_credentials()`, which installs a git credential helper
that:

- Responds only to `get` operations and ignores `store` / `erase`.
- Reads the token from `GH_TOKEN` → `GITHUB_TOKEN` → `WORKFLOW_GITHUB_TOKEN`.
- Returns `username=x-access-token` and `password=<token>` over the git
  credential protocol.
- Never writes the token to the worktree, global config, or logs — the helper
  string stored in git config only references the environment variable names.

Usage:

```sh
setup_gh_token_git_credentials
git push origin "$branch"
```

The helper snippet is injected by the parent workflow into the
`pair_programming_loop` subworkflow via `git_credential_helper`, so push steps
share one implementation instead of duplicating it.  The coordinator prompt
`agents/push_and_respond.md.tftpl` and the developer prompt
`workflows/pair_programming_loop/agents/developer.md` both instruct agents to
use the environment-token helper and forbid inline-token URLs.

## Refresh procedure

```sh
criteria adapter lock workstream_handler_v1
criteria compile workstream_handler_v1
```

`adapter lock` recurses into every subworkflow as of criteria #288, so the root
invocation locks the whole tree — it reports each workflow it visited and the
adapter count per workflow.
