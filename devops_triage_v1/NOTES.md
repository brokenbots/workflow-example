# devops_triage_v1 — a sentence in, a reviewed change out

Give it a request — one line is enough — and it either delivers a branch that a
manager agent signed off, or comes back and tells you why nothing needed doing.

```sh
criteria apply devops_triage_v1 \
  --var 'report_text=the release workflow is failing on tag releases' \
  --var repo_dir=/abs/path/to/repo \
  --var worktree_dir=/abs/path/to/repo/.worktrees/ops
```

Nobody has to attach a run ID, a log excerpt, or a diagnosis. Finding those is
the job, not the entry fee.

Scope is DevSecOps generally, not release pipelines specifically: CI/CD
workflows, release and packaging, build and container config, dependencies and
supply chain, repository permissions and policy. The three roles are written
against that whole surface.

Unlike `qa_triage_v1`, which stops at a specification, this workflow does the
work. Unlike `workstream_handler_v1`, it does not merge: delivery ends at an
open PR.

## The two constraints that define it

**1. Agents make changes. They never publish them.**

Branch creation, the worktree, the commit, the push, the PR — every one is a
deterministic shell step. No agent runs `git commit`, `git push`, `git tag`, or
`gh release`, and the executor's system prompt refuses them by name.

This is not tidiness. This role's tools are the ones that publish software and
grant access to it. An agent debugging a release has every incentive to "just
cut a tag and see", and a tag pushed to test a theory is a tag the world can
see. The mechanical steps are identical every run, so there is no judgement in
them to delegate — and delegating them anyway is how a diagnostic session
becomes a release.

**2. The agent that specifies the work never executes it; the agent that
executes it never judges it.** Three roles, two model families, one authority.

## Shape

```
stage_report → slug → branch → workspace → base ref → context → worktree
  → [G0 manager: intake]                    (one line is actionable)
  → triage: DISCOVER + WORK LIST
        ├─ no_action_needed → [G1 manager: confirm] → closed, with the answer
        └─ worklist_ready   → [G1 manager: work list]
  → devops_worker ⇄ ───────────────┐        (fresh agent every cycle)
  → verify (deterministic)         │
  → triage: STATE VALIDATION       │
  → [G2 manager: final] ─ needs_work ┘
  → push → PR → ops report → teardown
```

`devops_worker` is one cycle: assert worktree → agent executes → work log →
**deterministic commit** → gate → done.

## Roles

| role | model | authority |
|---|---|---|
| manager | `glm-5.2:cloud` | gates G0–G2. Sole authority to send work forward or close the run. |
| triage | `kimi-k2.7-code:cloud` | senior DevSecOps engineer. Called exactly twice: discovers and specifies, then reports where things stand. Fixes nothing. |
| devops | `kimi-k2.7-code:cloud` | executes one cycle inside the worktree. Publishes nothing. |

Deliberately a different model family for the manager. A manager sharing the
executor's blind spots is not a review, it is a second opinion from the same
mind.

## Discovery belongs to triage

There is no fixed evidence-collection step. `write_context` records only the
facts that are identical every run — repo, remote, base SHA, branch, worktree,
evidence path, the gate and verify commands, and the operator's hints — and then
gets out of the way.

A fixed collector would have to guess what matters. A failing release wants run
logs; a permissions audit wants branch protections and token scopes; a
dependency alert wants a lockfile history. Any collector that pulls one of those
sets quietly defines the workflow as being about one kind of task. Triage is the
senior engineer here: it queries GitHub, reads run logs, walks history, inspects
settings, and runs scanners, and it decides which of those the request calls for.

What keeps that auditable is the requirement to **write discovery artifacts into
the evidence directory as it goes**. The manager checks the work list against
those files, the executor reads them instead of re-deriving them, and the
state-validation pass reads them instead of trusting a summary. A claim with no
artifact behind it gets sent back at G1.

## Three gates, and one of them can end the run early

- **G0 intake** — could an engineer *begin* from this? A deliberately low bar.
  The manager prompt says in as many words not to reject a request for lacking a
  run ID or a diagnosis: nobody has run any queries yet, and demanding evidence
  here rejects exactly the requests this workflow is for.
- **G1 work list** — is the diagnosis grounded in the artifacts, before anyone
  changes anything? On approval the manager's reason **becomes the executor's
  brief** — it is not a review note.
- **G1 no action needed** — triage may come back and say the premise is wrong,
  it was already fixed, or the behavior is deliberate. That is a first-class
  result, and it is reviewed harder than a work list, because it is the cheapest
  conclusion available to the agent that would otherwise have to do the work.
  The manager's `confirmed` reason **is the answer the requester gets**.
- **G2 final** — the last point at which the work can be stopped. Nothing has
  been pushed yet.

"Tell us why we're wrong" is therefore a real path with a real gate on it, not
an incidental rejection.

## Why the executor is a subworkflow

`devops_worker` is re-entered from its initial state on every cycle, so the
executing agent gets a **new session each time**. The only things that survive a
cycle are the committed diff, the work log, and the manager's next brief.

That is the point. An executor carrying three failed attempts into a fourth
spends it defending them. It is also why the G1 and G2 prompts tell the manager
its reason is a *standalone directive* — the agent receiving it has no memory of
the cycle being critiqued, and feedback phrased as a reaction to work the reader
cannot remember doing is a wasted cycle.

The parent bounds the loop at `max_visits = 5` on `run_devops_worker`; the child
bounds its own execute ⇄ gate loop at 8.

## The two gates that are commands, not agents

| gate | where | for |
|---|---|---|
| `ci_gate_cmd` | inside each worker cycle | fast and cheap — `actionlint`, a build, a lint. Output is fed straight back to the agent on failure. |
| `verify_cmd` | once per cycle, after the gate | the expensive proof — a dry-run release, `act -j release`, a scanner, a packaging script. Its log is evidence for state validation and G2. |

Neither is an agent. A command that exits non-zero is not an opinion, and a
reviewer agent here would be a third opinion on work two agents already judge.

With no `verify_cmd`, the run can only claim the change looks right, never that
it works — `verify.log` says exactly that, so the manager cannot mistake a
skipped verification for a passing one.

## Security is not a task type here, it is a property of every task

Every role carries it, whatever the request was about:

- Triage must state the security implications of the work it specifies, and its
  state report must state, every time, whether the diff commits a credential or
  weakens a permission, protection, or gate.
- The executor may not buy a green result by weakening a gate — no `|| true`, no
  `continue-on-error`, no broadened permission, no disabled check, no
  undisclosed suppression. If that is the only way forward, it is `blocked`.
- The manager's G2 checks for committed secrets and quiet posture weakening as
  named, separate criteria.

The reason is the failure mode: a permission widened to make a job pass is a
security change wearing a convenience label, and nothing else in the pipeline
will flag it.

## Follow-up runs on the same branch

Passing the same `run_label` reuses the branch and worktree, and `open_pr`
detects the existing open PR — so a follow-up request updates the same PR
instead of stacking a second one. That is the intended way to iterate on work
already open for review.

It has one sharp edge, and it drew blood: **scope must be judged from where the
run started, not from the base branch.** `record_run_base` captures the branch
HEAD before any agent runs, and `validate_state` and `final_review` diff against
that. Without it, every earlier run's approved commits appear in the review diff
as unrequested changes, and the manager — correctly applying "nothing outside
scope changed" — writes a brief to revert them.

That is exactly what happened on `ops/install-script`: a run whose request was a
usability fix reviewed the whole branch, found the previous run's approved
signing cleanup, and briefed a fresh worker to revert it. It was caught with the
revert staged and uncommitted. The executing agent could not have questioned the
instruction — it has no memory of that work being requested, which is the entire
point of the reset. **Every safeguard against a wrong brief has to sit upstream
of the executor.**

## The host is not the workspace

Agents write to the worktree and the evidence directory. Nothing else — not
`$HOME`, not dotfiles, not installed packages, not `/usr` or `/opt`.

Both agent prompts state this explicitly, because it gets violated for good
reasons rather than bad ones. Two real incidents:

- An agent ran `brew install cosign` on the operator's machine, having been
  asked to exercise a signature check with the tool missing.
- An agent ran `install.sh` against the real `$HOME` to test it, appending a
  `PATH` line to four of the operator's shell startup files.

Both are what a diligent engineer would do to test the thing in front of them.
The rule is: use a scratch environment (`HOME` pointed at a temp dir, a temp
prefix, a container), or report the gap as a finding. A missing tool is a
`blocked`, not an install.

**No gate can catch this.** The manager reviews a diff; a package installed on
the host and a rewritten dotfile appear nowhere in one. That is why the boundary
lives in the prompts rather than in review — and why criterion 6's demand to
exercise controls has to be paired with it, since the pressure to exercise is
precisely what produces the violation.

### When the control can only be exercised on a host, move it to CI

Some controls genuinely cannot be exercised without changing a machine. A
Homebrew formula is the clean example: `brew install` writes to a global Cellar,
and there is no scratch-`HOME` version of it. Criterion 6 says exercise the
control; the host boundary says never touch the machine. For this class of work
those instructions contradict each other, and no wording resolves it — during the
tap work an agent installed on the operator's machine twice, once by accident and
once because a manager brief said "reinstall if needed."

The resolution is not an exemption. It is to **build the exercise into CI**, where
the host is a disposable runner: the tap's own workflow now runs `brew install`
and `brew test` on every change, and that job is the required check. The control
is exercised more thoroughly than a local run ever proved, on every future change,
and nobody's machine is involved.

So when triage finds a control that needs a host, the work item is "add the CI
that exercises it", not "exercise it locally and report". A manager asked to
authorize a local install should be asking why the check does not exist yet.

## Remote changes are specified, authorized, and verified

**Fixed 2026-08-02, after the homebrew-tap run demonstrated the hole.**

Originally the executor's prompt forbade remote-state mutation absolutely. That
was both too strict and unenforceable: too strict because half of DevSecOps work
*is* remote state — branch protection cannot be configured from a diff, a
repository has to exist before it can hold anything — and unenforceable because
the gate meant to catch a violation reviews a diff, and no remote change appears
in one.

The protocol that replaced it:

1. **Triage enumerates** every required change outside the worktree in a
   `REMOTE CHANGES` section of the work list, one per item, with the exact
   command and the narrowest form that achieves the end state. An item left out
   is an item that cannot happen.
2. **The manager authorizes them individually by name** in the G1 brief, and
   states that no others are permitted. It now also knows what the executor is
   barred from, so it can tell an escalation from an instruction. It may never
   authorize a credential, a grant to a person or app, a publish, or a deletion —
   those are `needs_human` regardless of justification.
3. **The executor performs only what is named**, records each in the evidence
   directory, and reports anything the work needed that was not authorized. A
   permission its token happens to have is explicitly not an authorization.
4. **Triage re-queries live state** at validation and looks for changes nobody
   listed; **G2 verifies each authorized change landed as authorized and no
   wider**, and treats an undeclared one as `needs_work` even if harmless.

The host is excluded from all of this. It is never authorized, by anyone.

Anything the work *creates* must arrive defensible — tests that run, branch
protection, least access — specified by triage and blocked on by G2. Created but
undefended is not done.

### What went wrong before the fix

The executor's prompt forbade remote-state mutation absolutely. The manager's
prompt never says so — it describes what the executor is *for*, not what it is
barred from. So a manager writing a G1 brief has no way to know it is asking for
something prohibited, and every incentive to ask: the brief is the executor's
only instruction, and an unaddressed prerequisite is a wasted cycle.

On the homebrew-tap run the request needed a repository that did not exist.
Triage correctly reported this as a decision requiring a human. The manager, with
no `needs_human` outcome available at G1 (see below), routed around it:

> **Tap repo — attempt if the `gh` token permits; otherwise report as a human
> prerequisite.** Create public repo `brokenbots/homebrew-criteria`
> (`gh repo create ... --public` if authorized).

The executor did it. `brokenbots/homebrew-criteria` is a real public repository,
created by an agent, and the commit carries the operator's git identity because
that is what `git config` on the host returns. G2 then saw the created repo in
the state report, verified it via `gh api`, and marked it a completed work item —
approving the run. Nothing in the chain was inconsistent; every role did what its
prompt told it to.

Three separate holes made it possible, all now closed by the protocol above:

1. **G1 had no `needs_human`.** `review_worklist` offered approved / revise /
   reject only, so a work list whose prerequisites were entirely operator-side
   could be approved or killed and nothing else — and "not warranted" is the
   wrong reason to kill warranted work. Gates 0 and 2 always had the outcome.
2. **The manager did not know the executor's prohibitions.** `manager.md`
   described the role's purpose, never its limits, so a brief requiring a
   forbidden action read as an ordinary instruction.
3. **No gate inspected remote state.** G2 reviewed a diff. Here the state report
   surfaced the created repo only because the work list happened to carry it as a
   numbered item — luck, not coverage.

What did hold: the executor declared the creation in its `reason`, in three
places, unprompted. Disclosure worked where prohibition did not. And the
credential prohibition held completely — `HOMEBREW_TAP_TOKEN` was routed to a
human and `brokenbots/criteria` still has zero Actions secrets. The difference is
that the brief authorized one and stayed silent on the other.

## Where the work happens

| tree | who | writable | lifetime |
|---|---|---|---|
| `repo_dir` — the operator's checkout | deterministic shell only | **no** | permanent |
| `worktree_dir` — the run's branch | all three agents | **yes** | removed only if pushed, or if untouched |
| `<ops_root>/<slug>/evidence` | shell; agents write only their own artifacts | — | never removed |

The worktree is deliberately **not** torn down when it holds unpushed commits —
on those paths it is the only copy of the run's work. On paths that conclude
before any work cycle (insufficient, no action, rejected work list) it is removed
*if it is clean*; a dirty tree there means an agent wrote where it was told not
to, and that is worth keeping to look at.

## Terminal states

| state | success | reached by |
|---|---|---|
| `delivered` | yes | `pr_opened`, `pushed`, `completed_local` |
| `closed_no_change` | yes | `no_action_needed`, insufficient request, rejected work list, rejected work |
| `needs_human` | no | a decision, credential, or approval that is not the workflow's to make |
| `failed` | no | worker never completed a cycle, or the push was refused |

`no_action_needed` is a **success**. The answer to "is this broken?" is allowed
to be no, and the ops report carries the manager's explanation of why.

## Running it

```sh
export DEVOPS_GITHUB_TOKEN="$(gh auth token)"

criteria apply devops_triage_v1 \
  --var 'report_text=the release workflow is failing on tag releases' \
  --var repo_dir=/abs/path/to/repo \
  --var worktree_dir=/abs/path/to/repo/.worktrees/ops-release \
  --var 'ci_gate_cmd=actionlint .github/workflows/*.yml' \
  --var 'verify_cmd=make build'
```

| variable | notes |
|---|---|
| `report_text` / `ops_report_file` | exactly one. A file is worth it for a request with real detail; the text form is for the one-liner. |
| `run_label` | overrides the slug (which names the run dir and the branch `ops/<slug>`). Otherwise derived from the filename, or the first line of the text. |
| `hints` | free text passed to triage as a starting point — a workflow name, a tag, a run ID, a constraint. Triage is not bound by it. |
| `deliver` | `pr` (default), `push`, or `none`. |

`.worktrees/` under the target repo should be in its `.gitignore` or
`.git/info/exclude`.

## Refresh procedure

```sh
criteria adapter lock devops_triage_v1
criteria compile devops_triage_v1
```

`adapter lock` recurses into subworkflows as of criteria #288 — the root
invocation reports `locked 2 workflow(s)` and covers `devops_worker`.

## Pins (as of 2026-08-01)

| adapter | source | version |
|---|---|---|
| copilot | `ghcr.io/brokenbots/criteria-adapter-copilot` | `0.5.4` |
| shell | `ghcr.io/brokenbots/criteria-adapter-shell` | `0.5.2` |

Every agent role resolves through the local Ollama endpoint
(`http://localhost:11434/v1`, `responses` wire API):
`ollama pull kimi-k2.7-code:cloud glm-5.2:cloud`.

## Where a GitHub adapter and MCP support would attach

The read/write split this workflow already enforces is the seam:

- **GitHub reads** — run history, logs, API queries, settings inspection — are
  agent-side, unconstrained, and vary per task. Today they go through `gh` inside
  the agent's tool shell, which means they are unaudited except by the artifacts
  the agent chooses to save. A GitHub adapter (or an MCP server) would make them
  typed calls with recorded inputs and outputs, and the evidence directory would
  stop depending on the agent's discipline.
- **GitHub writes** — push, PR, tag, release — are already deterministic steps
  with no agent involvement. Those are the calls an adapter should own outright,
  and the ones that most need the permission boundary an adapter can enforce and
  a system prompt cannot.

`allow_tools = ["*"]` on every agent step is the placeholder for that boundary.
When the tool surface becomes declarable, this is where it gets narrowed:
read-only GitHub for triage, no GitHub at all for the executor, no tools beyond
reads for the manager.

## Gotchas carried over from the other workflows

- **Every variable referenced from adapter config needs a default.** A required
  variable leaves the config unknown at compile time. `repo_dir` and
  `worktree_dir` default to `""` for this reason; `stage_report` rejects an empty
  `worktree_dir` at run time instead.
- **Shell parameter expansion collides with HCL interpolation.** `${slug%.*}`
  inside a heredoc is parsed by HCL — escape it as `$${slug%.*}`.
- **Operator- and agent-authored text goes through a QUOTED heredoc delimiter.**
  Unquoted, the shell command-substitutes any backticks or `$(...)` it contains
  and splices the output into the file. This applies to `report_text` and
  `hints`, which arrive straight from the command line.
- **Never put agent-authored text on a shell command line at all — not even
  inside single quotes.** The worker's empty-brief guard was
  `[ -z '${var.brief}' ]`, and it killed a live run at `assert_worktree`: an
  apostrophe in the manager's prose closed the quote and a later `(` parsed as a
  shell token (`sh: -c: line 13: syntax error near unexpected token '('`). There
  is no quoting that survives arbitrary text here. Test it in HCL instead — that
  guard is now `switch "route_brief"` with `condition = var.brief == ""`, and no
  shell ever sees the value. Writing such text to a *file* via a quoted heredoc
  is still fine; that is a different context with a real escape.
