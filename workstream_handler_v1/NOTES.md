# workstream_handler_v1 — adapter/model pins

Every `adapter` block is pinned to an OCI `source` + `version` (the criteria
v0.5 "adapter-v2" model). All pins lock and compile end-to-end on this host
(darwin/arm64): `criteria adapter lock <dir>` resolves each adapter's digest
and `criteria compile <dir>` succeeds for the root workflow and all three
subworkflows.

## Pins (as of 2026-07-27)

| adapter | source | version | notes |
|---|---|---|---|
| copilot      | `ghcr.io/brokenbots/criteria-adapter-copilot`      | `0.5.2` | multi-arch, signed. Used for dev work: developer, coordinator, branch-repair. |
| shell        | `ghcr.io/brokenbots/criteria-adapter-shell`        | `0.5.2` | multi-arch, keyless-signed. |
| noop         | `ghcr.io/brokenbots/criteria-adapter-noop`         | `0.5.1` | multi-arch, keyless-signed. |
| claude-agent | `ghcr.io/brokenbots/criteria-adapter-claude-agent` | `0.5.5` | Claude Code CLI adapter. Used for review roles (pair-loop reviewer, PR reviewer). Signature warning: keyless signature has no Rekor transparency-log proof — locks in warn mode; re-publish with Rekor enabled to verify strictly. |

## Model pins (as of 2026-07-27)

| role | adapter | model |
|---|---|---|
| developer (pair loop)  | copilot      | `kimi-k2.7-code:cloud` (Ollama cloud — `kimi-k3:cloud` is extra-usage-only on the current plan) |
| coordinator (root)     | copilot      | `minimax-m3:cloud` (Ollama cloud) |
| branch repair          | copilot      | `minimax-m3:cloud` (Ollama cloud) |
| reviewer (pair loop)   | claude-agent | `claude-opus-5` |
| PR reviewer            | claude-agent | `claude-opus-5` |

Copilot roles resolve through the local Ollama endpoint
(`http://localhost:11434/v1`, `responses` wire API); pull the cloud models with
`ollama pull kimi-k3:cloud minimax-m3:cloud`. The claude-agent roles need the
`claude` CLI installed and authenticated on `PATH` (or set `claude_executable`
in the adapter config) — no provider config.

## Refresh procedure

```sh
criteria adapter lock workstream_handler_v1
criteria adapter lock workstream_handler_v1/pair_programming_loop
criteria adapter lock workstream_handler_v1/pr_reviewer_loop
criteria adapter lock workstream_handler_v1/branch_manager
criteria compile workstream_handler_v1
```
