# workstream_handler_v1 — adapter/model pins

Every `adapter` block is pinned to an OCI `source` + `version` (the criteria
v0.5 "adapter-v2" model). All pins lock and compile end-to-end on this host
(darwin/arm64): `criteria adapter lock <dir>` resolves each adapter's digest
and `criteria compile <dir>` succeeds for the root workflow and all three
subworkflows.

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

## Refresh procedure

```sh
criteria adapter lock workstream_handler_v1
criteria compile workstream_handler_v1
```

`adapter lock` recurses into every subworkflow as of criteria #288, so the root
invocation locks the whole tree — it reports each workflow it visited and the
adapter count per workflow.
