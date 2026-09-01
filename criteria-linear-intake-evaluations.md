# Criteria Linear Intake Evaluations

Date: 2026-08-30

Target repository: `https://github.com/brokenbots/criteria`

Method: Six deliberately varied Linear work items are submitted to `linear_intake_v1` and run one at a time through Docker Compose. Findings are intentionally scoped for investigation rather than prescribing exact fixes. Each run records routing, artifacts, Linear/GitHub outcome, intervention, and a post-run quality review.

## Candidate Work Items

### 1. CRI-47: Redact overlapping secrets deterministically

- Type: bug / security
- Expected complexity: small
- Observation: `internal/adapter/secrets/redaction.go` builds `strings.Replacer` arguments by iterating a map. For overlapping values such as `secret` and `secret123`, randomized ordering can replace the shorter value first and leave the suffix visible; the existing overlap test only asserts that the original full strings disappear.
- Requested investigation: reproduce the ordering-dependent output, define the desired redaction behavior, and add focused regression coverage.
- Expected intake route: bug -> QA triage.

### 2. CRI-48: Enforce environment filesystem and network policy in permission decisions

- Type: security / incomplete implementation
- Expected complexity: medium
- Observation: `internal/adapterhost/policy.go` checks `allow_tools`, but the environment filesystem read-only and network egress branches are placeholders and still return allow. A configured environment policy can therefore appear enforced at permission-decision time while having no effect there.
- Requested investigation: establish which layer owns enforcement, verify whether permission requests carry enough detail, and close or clearly remove the ineffective policy path.
- Expected intake route: bug -> QA triage.

### 3. CRI-49: Confine file secrets using path boundaries rather than string prefixes

- Type: security bug
- Expected complexity: small
- Observation: `internal/adapter/secrets/provider_file.go` uses `strings.HasPrefix(requested, root)` after symlink evaluation. A sibling such as `/root-secrets/value` shares the `/root` prefix and may pass confinement despite being outside the configured root.
- Requested investigation: add a minimal sibling-prefix reproduction and enforce path-component-aware confinement.
- Expected intake route: bug -> QA triage.

### 4. CRI-50: Synchronize graph adapter cache access

- Type: concurrency bug / investigation
- Expected complexity: medium
- Observation: `cacheGraphAdapterRef` writes `graphAdapters` and `adapterDirs` without taking `SessionManager.mu`, while lookup helpers read the same maps without synchronization. Determine whether graph verification and adapter resolution can overlap; if so, concurrent map access can race or panic.
- Requested investigation: verify reachable concurrency with the race detector and make cache ownership/synchronization explicit.
- Expected intake route: bug -> QA triage.

### 5. CRI-51: Report OCI signature-referrer copy failures

- Type: reliability / observability bug
- Expected complexity: small
- Observation: `internal/adapter/oci/pull.go` discards every error from `copyReferrers`. Strict verification still fails closed, but a transient discovery or copy failure is converted into a later generic missing-signature failure and the pull may leave an incomplete local cache without the causal error.
- Requested investigation: preserve best-effort behavior where intended while making referrer failures diagnosable and testing the strict-verification path.
- Expected intake route: bug -> QA triage.

### 6. CRI-52: Consolidate semver comparison into shared functions

- Type: refactor / redundant logic
- Expected complexity: medium
- Observation: semver validation, normalization, constraint matching, and comparison behavior is spread across OCI version selection and manifest validation. This duplication has been observed and should be consolidated behind a shared set of functions so callers use one semantic contract.
- Requested investigation: inventory the existing call sites, define the shared API, migrate callers, and preserve behavior with tests. Do not expand scope beyond semver handling.
- Expected intake route: feature -> implementation handler.

## Runs

### Setup observation

- Attempting to `source linear_intake_v1/.env` in Zsh failed on unquoted values containing spaces (for example, state and build-command values). Docker Compose's env-file parser accepts these values, so workflow execution is unaffected. Subsequent API setup extracted only `LINEAR_API_KEY` with `sed` rather than evaluating the env file as shell code.
- Intervention: changed the local metadata/ticket-creation command only; no workflow or Criteria source was changed.

Run records are appended below before and after each sequential Compose invocation.

### CRI-47 run

- Result: completed in 23m43s; Linear moved through Triage, In Progress, In Review, and Done.
- Workflow behavior: intake classified the ticket as a bug; QA reproduced it and generated a workstream; the handler implemented the change, ran its local CI gate, opened PR #336, waited through six one-minute CI backoffs, obtained an independent approval, merged, removed the worktree/branch, commented on Linear, and closed the ticket.
- Solution review: PR #336 sorts registered values longest-first before constructing the replacer and strengthens overlap/prefix-chain coverage for string and byte redaction. Focused tests, a 10,000-iteration regression run, race tests, the full local CI gate, and all GitHub checks passed. The solution directly addresses the partial-secret exposure without unrelated scope.
- Interventions: none during the workflow. Operator work was limited to post-run inspection of Linear, GitHub, and captured logs.
- Final artifacts: https://github.com/brokenbots/criteria/pull/336 (merged as `519e2ecf17ccb0cae8947accabaea554f6d0bd84`).

### CRI-48 run

- Result: completed in 30m53s; Linear ended in Done.
- Workflow behavior: QA reproduced that the permission policy returned allow after the `allow_tools` check despite configured read-only/no-egress policy. The handler implemented enforcement and documentation, opened PR #337, waited for CI, obtained approval, merged, cleaned up, and closed the ticket.
- Solution review: the change added a second policy layer with distinguishable filesystem and network denial reasons plus focused regression tests and documentation. GitHub CI passed all required checks. During review, tests initially failed because the shell environment supplied `CRITERIA_HOME`; the reviewer identified this as pre-existing environment contamination, reran with the variable unset, and confirmed build/full tests passed.
- Interventions: none by the operator. The reviewer autonomously diagnosed and worked around its test-process environment to evaluate the PR; no source or workflow workaround was introduced.
- Final artifacts: https://github.com/brokenbots/criteria/pull/337 (merged as `54e2d73596bb94ee0fbcfa21074d7be87471d23b`).

### CRI-49 run

- Result: completed in 20m9s; Linear ended in Done.
- Workflow behavior: QA reproduced the sibling-prefix escape, generated a narrowly scoped workstream, and handed it to implementation. The handler opened PR #338, passed local and remote validation, obtained approval, merged, cleaned up, and closed the Linear issue.
- Solution review: the fix replaced raw `strings.HasPrefix` confinement with a path-component-aware helper after cleaning, joining, and symlink evaluation. Regression coverage includes absolute sibling paths, relative traversal, symlink-mediated escapes, trailing-separator roots, and valid in-root reads. All GitHub checks passed. Reviewers correctly recorded the pre-existing TOCTOU window between `EvalSymlinks` and `ReadFile` as residual risk outside this ticket.
- Interventions: none by the operator. The same `CRITERIA_HOME` test-environment artifact appeared during review and was handled autonomously without changing source or workflow semantics.
- Final artifacts: https://github.com/brokenbots/criteria/pull/338 (merged as `3dbf662a0c595a051dfd7baf087a03c15423cafb`).

### CRI-50 run

- Result: workflow completed in 50.0s and moved Linear to In Review; no implementation PR was created.
- Workflow behavior: intake classified the report as a bug, but QA returned `insufficient_report` because the ticket supplied only a static concurrency hypothesis with no observed symptom, reachable concurrent call sequence, version, race-detector output, or reproduction workflow. Both requested refs were skipped. The independent intake reviewer agreed with `invalid_close`, posted detailed resubmission requirements, and routed to human review.
- Solution review: no code solution exists to evaluate. The workflow behaved appropriately by refusing to convert unknown reachability into a workstream. A stronger ticket would need a concrete overlapping operation path and focused `go test -race` evidence.
- Interventions: none. The operator intentionally did not invent evidence or modify the workflow to force progress. CRI-50 remains an open human-review item and is the only run so far that did not reach Done.

### CRI-51 run

- Result: workflow completed in 51.0s and moved Linear to In Review; no implementation PR was created.
- Workflow behavior: QA returned `insufficient_report`. Although the ticket named the discarded `copyReferrers` error and a plausible downstream effect, it lacked a version/commit, a registry or failure-injection scenario, an observed downstream error, and expected output. Both refs were skipped, the reviewer agreed with `invalid_close`, and Linear received concrete resubmission requirements.
- Solution review: no code solution exists to evaluate. The gate correctly distinguished an observable defect from a source-level observability concern. A future run needs a controlled referrer discovery/copy failure and the resulting strict-verification diagnostics.
- Interventions: none. No workflow workaround was made and no Criteria engine ticket was necessary because the run completed normally. CRI-51 remains in human review.

### CRI-52 run

- Initial result: the direct feature route generated a workstream and PR #339, but GitHub's coverage ratchet failed. The reviewer child returned `checks_failed`; the parent repeatedly sent the coordinator to review-thread triage without supplying failed-check details. With no review comments, the coordinator returned `no_work`, pushed nothing, and repeated until `triage_pr_feedback` reached `max_visits = 10`. The handler failed after 19m24s and correctly moved Linear to In Review.
- Workflow diagnosis: workflow-owned information loss, not a Criteria engine defect. `pr_reviewer_loop` retained the detailed `status:checks_failed` output internally but exposed only `review_result`; the parent coordinator therefore could not distinguish red CI from an empty review queue. Criteria's visit guard fired as declared, so no engine ticket was created.
- Workflow intervention: added a declared `status_output` from the reviewer child, captured it on both parent outcomes, passed it into `triage_pr_feedback`, and instructed the coordinator that every listed `failing_check` requires a `has_work` developer brief even with no review comments. The workflow validated successfully; existing atomic-write warnings in the pair-programming child were unrelated.
- Second attempt: rebuilt the Compose image and reran CRI-52. The corrected coordinator path inspected the Coverage ratchet failure rather than returning `no_work`, confirming the behavioral fix. The run container then disappeared during a long coordinator investigation without a Criteria terminal event or Linear failure comment, leaving Linear In Progress and PR #339 unchanged. This was treated as an external/container interruption; no engine or workflow workaround was added.
- Final result: a third sequential run resumed from Linear/GitHub state, converted the failed check into a developer brief, added commit `31415a18afcf619a60cc8b4d71f2181121dcb276`, passed all GitHub checks, obtained approval, merged PR #339, cleaned up, and moved Linear to Done in 29m27s.
- Solution review: the implementation moved normalization, validation, comparison, and npm-style constraint parsing/matching into `internal/adapter/semver`, migrated OCI selection and manifest validation, removed roughly 150 lines of duplicated OCI helpers, and added focused tests for exact, caret, tilde, wildcard, partial, zero-major, and invalid constraints. The remediation adjusted the OCI coverage floor from 67.0 to 65.5 because statements moved to the new package; final OCI coverage was 65.9 and all remote checks passed. The final reviewer could not independently query GitHub from its read-only shell and relied on the supplied status output, but the isolated reviewer adapter posted the approval and external verification confirmed every check green.
- Interventions: one workflow code fix, one image rebuild, and two reruns after the initial failed run. The second rerun required another operator retry after unexplained container disappearance. No Criteria source workaround or engine ticket was created.
- Final artifacts: https://github.com/brokenbots/criteria/pull/339 (merged as `275ce434a04ad179b5e325e6ac38a7649416928c`).

## Overall Evaluation

- Six tickets created and executed sequentially; no workflows overlapped.
- Four tickets reached Done with merged PRs: CRI-47/#336, CRI-48/#337, CRI-49/#338, and CRI-52/#339.
- Two intentionally under-specified source findings, CRI-50 and CRI-51, were rejected as `insufficient_report` and left In Review with concrete evidence requests. This was appropriate behavior, not a crash.
- Three bug reports were reproduced, independently reviewed, implemented, fully validated, merged, and reflected back into Linear without operator intervention.
- The direct feature path exposed a real feedback-loop defect when CI failed without review comments. The workflow fix preserved bounded retries and supplied missing status context instead of increasing limits or bypassing Criteria behavior.
- Operator interventions: one setup-command correction for `.env` parsing; one workflow fix; one Compose image rebuild; two additional CRI-52 launches after the first failure; and one extra retry after an unexplained container disappearance. All other activity was observation and post-run evaluation.
- No observed failure was classified as a Criteria engine defect, so no engine ticket was created. The `max_visits` failure was correct enforcement of the workflow's bound.

## Experiment 2: State, Resume, and Remote Communication

Date: 2026-08-30

Method: Repeat the same sequential Linear intake evaluation with ten objects focused on state persistence, failure recovery, and remote communication. The requested category counts total nine, so the explicitly requested evidence-rich unreproducible bug is tracked as a tenth distinct object. Categories are 2 security, 4 ordinary bugs, 2 features, 1 detailed red-herring bug, and 1 intermittent/unreproducible bug. Exactly one Compose workflow runs at a time.

### CRI-53: Reject plaintext central-server connections outside loopback

- Type: security
- Expected complexity: small
- Observation: `internal/transport/server/client.go` accepts any `http://` server URL, selects `TLSDisable`, sends the server-issued bearer token in `Authorization`, and streams workflow events over cleartext HTTP/2. There is no loopback restriction or explicit insecure opt-in for remote hosts.
- Requested investigation: demonstrate the accepted non-loopback configuration and define a secure default that preserves intentional local development.

### CRI-54: Require authentication for non-loopback remote adapter listeners

- Type: security
- Expected complexity: medium
- Observation: a remote environment can bind `listen_address = "0.0.0.0:..."` without mTLS or `accept_token`. Lockfile digest verification checks a client-supplied digest against an expected public value, but does not prove client identity or possession of a secret.
- Requested investigation: verify whether an unauthenticated client that knows the pinned adapter name/digest can bind, and require an explicit secure configuration or insecure override for non-loopback TCP listeners.

### CRI-55: Write crash-recovery checkpoints atomically

- Type: bug
- Expected complexity: small
- Observation: `WriteStepCheckpoint` writes the sole per-run JSON checkpoint in place with `os.WriteFile`. A process or host crash in the truncate/write window can leave empty or partial JSON, after which startup cannot decode the only reattach record.
- Requested investigation: add a deterministic torn-write/failure-injection test and use a durable atomic replacement pattern while preserving owner-only permissions.

### CRI-56: Preserve invalid resume payloads until validation completes

- Type: bug
- Expected complexity: small
- Observation: approval and signal-wait nodes clear `ResumePayload` and `PendingSignal` before validating the supplied decision/outcome. An invalid approval decision therefore returns an error after destroying the payload needed for deterministic retry and audit; signal waits also silently fall back to the first outcome for unknown names.
- Requested investigation: reproduce invalid resume input across retry/reattach and define when payload state should be consumed.

### CRI-57: Avoid one global local-run state file for concurrent agents

- Type: bug
- Expected complexity: medium
- Observation: every process writes `<CRITERIA_HOME>/criteria-state.json`. Two concurrent local or server-backed agents sharing `CRITERIA_HOME` can overwrite each other's PID/run metadata, causing status or cleanup operations to target only the last writer even though per-run checkpoints are separate.
- Requested investigation: start two agents against one state directory and verify state discovery and cleanup behavior.

### CRI-58: Make remote handshake deadlines configurable and diagnosable

- Type: bug / availability
- Expected complexity: small
- Observation: the remote shim hardcodes 10 seconds for TLS and 5 seconds for the newline-delimited identity message, with no environment-level timeout fields. A valid adapter on a delayed or resource-constrained link can be rejected before identity verification, and operators cannot tune the deadline per deployment.
- Requested investigation: reproduce with a delayed handshake and add configuration plus clear timeout diagnostics without weakening authentication.

### CRI-59: Add a central run watch/attach command

- Type: feature
- Expected complexity: medium
- Observation: `criteria apply --server` already registers a container-started agent and streams events/control to a central server, but the CLI has no obvious read-only `watch` or `attach` command for an operator to follow a run by ID from another machine.
- Requested investigation: provide a server-backed observer command with historical replay followed by live events and terminal exit status.

### CRI-60: Add a long-lived centrally assigned agent mode

- Type: feature
- Expected complexity: large
- Observation: server mode is initiated by `criteria apply` for one locally selected workflow. A container cannot currently run as a durable agent that registers once, receives queued workflow assignments from the central server, reports progress, survives reconnects, and accepts later work without direct Docker output monitoring.
- Requested investigation: define the assignment/control protocol and a container-oriented daemon command, reusing existing registration, event streaming, cancellation, and reattach primitives.

### CRI-61: Red herring - Pause persists partial adapter snapshots

- Type: red-herring bug
- Expected complexity: small
- Claim: if one adapter fails during `SnapshotAll`, successful snapshots from other adapters are still written, so resume restores a mixed-generation set.
- Detailed evidence to inspect: `SessionManager.SnapshotAll` returns both successful entries and a joined error after a partial failure. Construct three sessions where the middle snapshot fails and check the snapshot directory for the other two.
- Disconfirming evidence: `Engine.Pause` checks the returned error and exits before its persistence loop. The expected result is no snapshots from that call; QA should verify this and reject the report rather than generate a fix.

### CRI-62: Intermittent control messages can be dropped under channel saturation

- Type: intentionally unreproducible/intermittent bug
- Expected complexity: medium
- Observation: `controlLoop` forwards `run.cancel` and `resume_run` into fixed 32-entry channels with non-blocking sends. When full, it logs a warning and permanently drops the command with no acknowledgement, retry, metric, or backpressure.
- Evidence-rich reproduction attempt: use a fake Control stream to send at least 33 distinct commands while deliberately not draining the consumer channel; verify the 33rd command is absent and the warning is emitted. Then exercise a real paused/high-event-rate run where consumer scheduling makes saturation timing-dependent. The deterministic unit harness should force QA to investigate even if the live race is not reproduced.

## Experiment 2 Runs

### CRI-53 run

- Result: completed in 32m16s; Linear ended in Done.
- Workflow behavior: intake classified the security report as a bug, QA reproduced the accepted non-loopback plaintext configuration, and the handler opened PR #340. The workflow waited for all GitHub checks, obtained an independent approval, merged, removed its worktree and branch, posted the completion comment, and closed the issue.
- Solution review: PR #340 rejects non-loopback `http://` server URLs by default in `NewClient`, preserves plaintext loopback development, permits deliberate remote plaintext only through an explicit insecure option, and retains HTTPS-to-TLS behavior. A table-driven transport test covers the security boundary. All required GitHub checks passed, and the reviewer identified only an unrelated test failure already present on the base branch during its additional local validation.
- Interventions: none. The Compose image was rebuilt before this first Experiment 2 run so it contained the already validated Experiment 1 failed-CI feedback fix.
- Final artifacts: https://github.com/brokenbots/criteria/pull/340 (merged as `b141f0d3af72368f6de8130ae62e9c30e10e9f17`).

### CRI-54 run

- Initial attempt: intake classified the requested secure-listener default as a feature, generated a workstream, moved Linear to In Progress, created the isolated `CRI-54` branch/worktree from the newly merged CRI-53 base, and began implementation. During the developer's focused test phase, Docker terminated the run with `context canceled`; no preceding Criteria or workflow error was emitted, the failure-comment step was also canceled, no PR existed, and Linear remained In Progress.
- Intervention: treat the abrupt container cancellation as an external interruption and rerun only CRI-54 so the workflow can reconcile its durable Linear/GitHub/worktree state. No Criteria or workflow source is changed before the retry.
- Second attempt: the rerun correctly rediscovered the In Progress Linear issue but recreated the branch/worktree from `origin/main`, so the first attempt's uncommitted implementation was not recovered. The developer independently reimplemented the listener checks and again reached focused tests before another abrupt Docker cancellation. There was still no Criteria terminal event, failure comment, or PR; Linear remained In Progress.
- State/resume observation: Linear and merged-PR state are durable across launches, but edits that have not reached a commit/remote branch are disposable with the `--rm` container/worktree lifecycle. The next retry is intentionally limited to one final attempt before treating the repeated external cancellation as a blocked run rather than looping indefinitely.
- Final result: the third launch regenerated the workstream and implementation, completed in 24m50s, merged PR #341, cleaned up, and moved Linear to Done. The repeated intake-start comments remain as an accurate durable trace of all three launches.
- Solution review: the implementation enforces authentication at the common remote-listener construction path, permits non-loopback TCP only with mTLS, `accept_token`, or explicit `insecure` opt-in, and preserves loopback and Unix-socket behavior. Focused tests cover the security gate and accepted configurations; all GitHub checks passed. Review noted non-blocking documentation, relative-socket, redundant-condition, and DNS-resolution edge cases but found no exit-criteria violation.
- Interventions: two same-ticket retries after external cancellation; no workflow or Criteria source change. The experiment exposed that launch-level recovery reconstructs durable ticket/PR state but cannot recover uncommitted work from a removed container.
- Final artifacts: https://github.com/brokenbots/criteria/pull/341 (merged as `383ddaca009f30031983fd7aa31996d72fbaeb27`).

### CRI-55 run

- Result: completed at the QA gate in about two minutes; Linear ended in In Review and no PR was created.
- Workflow behavior: QA resolved main at `383ddaca009f30031983fd7aa31996d72fbaeb27` and stable at v0.5.11, then returned `insufficient_report` without executing either ref. The independent intake reviewer agreed with `invalid_close` and requested an affected version plus a reliable torn-write reproduction or explicit crash-injection method.
- Solution review: no implementation exists to evaluate. The source-level risk is credible, but the ticket did not identify an observed corrupted checkpoint or prescribe a deterministic interruption seam, so the gate consistently refused to invent evidence. This candidate was under-specified for the workflow's bug path despite naming the vulnerable write operation and desired atomic-replacement behavior.
- Interventions: none. CRI-55 remains available for human review or resubmission with a failure-injection harness.

### CRI-56 run

- Result: completed in 52.8s; Linear ended in In Review and no PR was created.
- Workflow behavior: QA returned `insufficient_report` at Gate 0 and skipped both main and v0.5.11. The independent reviewer agreed with `invalid_close`, identifying the missing version, triggering workflow, exact invalid resume payload and command sequence, server/orchestrator environment, and verbatim observed output.
- Solution review: no implementation exists to evaluate. Although the ticket points to a concrete mutation-before-validation path, reproducing approval and signal resume behavior requires protocol and workflow details that were not supplied. The rejection is consistent with the workflow's policy against substituting source inference for a reported runtime symptom.
- Interventions: none. CRI-56 remains in human review pending a deterministic server-backed resume scenario.

### CRI-57 run

- Result: completed in about 21 minutes; Linear ended in Done.
- Workflow behavior: intake treated the state ownership change as a feature, generated a workstream, and the handler implemented, independently reviewed, opened, monitored, approved, merged, and cleaned up PR #342 without operator intervention.
- Solution review: the implementation replaces the single global run-state owner with per-run files, updates local and server apply paths plus status discovery, and handles the legacy file during migration. Tests exercise multiple run records and ownership behavior; build, race tests, lint, example validation, vulnerability scanning, and every required GitHub check passed. Review identified only non-blocking output-ordering, repeated legacy-file removal, and fixture-combination notes.
- Interventions: none.
- Final artifacts: https://github.com/brokenbots/criteria/pull/342 (merged as `228a35a42f3347fd6a8b5ddcba69d3754cbe6397`).

### CRI-58 run

- Result: completed in about 16 minutes; Linear ended in Done.
- Workflow behavior: intake treated configurability as a feature, and the implementation handler completed the full developer, local-review, GitHub CI, independent approval, merge, cleanup, and Linear closure path in one launch.
- Solution review: PR #343 adds separately configurable and bounded TLS and identity-message handshake deadlines, threads them through remote environment configuration, applies deadlines in the shim, and emits phase-specific timeout diagnostics. Delayed-handshake and configuration tests provide regression coverage. All local gates and required GitHub checks passed. The independent review's only material note was a non-blocking documentation gap in the remote deployment option table.
- Interventions: none.
- Final artifacts: https://github.com/brokenbots/criteria/pull/343 (merged as `fd4422b4a03976e6e8c0e2ec74697c8983abc235`).

### CRI-59 run

- Result: completed in about 28 minutes; Linear ended in Done.
- Workflow behavior: the direct feature route generated a workstream and PR #344. Local review initially required command-level tests around the transport and argument surface; the developer added them, after which local review, remote CI, independent approval, merge, cleanup, and closure all completed in the same launch.
- Solution review: the new `watch` command reuses the existing server HTTP/2 protocol, replays history, continues live from the last sequence, short-circuits when history is already terminal, emits final status once, and supports human and JSON output. Eighteen focused tests cover argument validation, positional run IDs, JSON, history/live continuity, terminal history, and stream termination through the real client transport; race tests and every GitHub check passed.
- Interventions: none.
- Final artifacts: https://github.com/brokenbots/criteria/pull/344 (merged as `9c166c7914ce79b5670a1879e1dfe3b09f5ad038`).

### CRI-60 run

- Result: completed in about 90 minutes; Linear ended in Done.
- Workflow behavior: the feature route generated the largest workstream in the experiment. The handler implemented the protocol and CLI, completed a deep local review, added coverage-focused validation, remediated the coverage ratchet autonomously, waited for all remote checks, obtained independent approval, merged PR #345, cleaned up, and closed Linear in one launch.
- Solution review: PR #345 adds a long-lived `agent` command, centrally delivered assignments, sequential one-at-a-time execution, authentication, reconnect behavior, and a per-run publisher with exactly-once pending-event replay. It extends the protocol rather than duplicating existing registration and server apply primitives. Tests cover registration invariance, assignment order, authentication rejection, reconnect/replay, and transport behavior; server transport coverage reached 96.3% without lowering its floor, race tests were stable, and all GitHub checks passed.
- Residual review notes: a narrow self-healing completion-signal race was not reproduced; compile failures occur before the publisher exists and are therefore not reported centrally; one auth test retains a timeout fallback. None violated the requested lifecycle contract.
- Interventions: none.
- Final artifacts: https://github.com/brokenbots/criteria/pull/345 (merged as `8ef3c05514ede491a48a1e7a9715acf29d11c43b`).

### CRI-61 run

- Result: completed in about 15 minutes; Linear ended in In Review and no PR was created.
- Workflow behavior: QA accepted the detailed report for investigation, built and ran a focused three-session partial-snapshot harness on both main `8ef3c05514ede491a48a1e7a9715acf29d11c43b` and v0.5.11, retained clean evidence, and returned `not_reproduced` on both refs. The intake reviewer agreed with `invalid_close` and moved the ticket to human review.
- Solution review: the investigation confirmed the premise that `SnapshotAll` can return successful entries plus a joined error, but falsified the claimed consequence: `Engine.Pause` immediately returns that error before entering the `WriteSnapshot` loop. Every session remained on generation 1, with no mixed-generation persistence. QA correctly traced the caller, tested version skew and controls, and declined to create a fix.
- Interventions: none. This red herring successfully distinguished evidence-driven QA from source-pattern matching.

### CRI-62 run

- Initial attempt: intake correctly classified the intermittent report as a bug. QA marked it actionable despite the unreliable production symptom because the 33-message saturation mechanism is deterministic, produced and approved a two-ref plan with five control shapes and an explicit falsification path, and began main-ref baseline testing. Docker then canceled the run before reproduction completed; Linear remained in Triage with no verdict comment or PR.
- Intervention: rerun only CRI-62 after the external cancellation. No workflow or Criteria source is changed. Main now includes CRI-60's substantial control-stream redesign, while v0.5.11 retains the reported fixed-channel implementation, so version-skew evidence is expected to be especially discriminating.
- Final result: the retry completed in about two minutes, returned `insufficient_report`, moved Linear to In Review, and created no PR. Its regenerated classifier report stated `Expected behavior: Not stated in the ticket`; QA therefore refused to run the same deterministic harness that the first launch had accepted.
- Evaluation: the original ticket did force QA to investigate as requested, proving that an intermittent production symptom can pass the gate with a deterministic mechanism. Restart behavior was not stable, however: the first intake/QA pass treated non-loss of control commands as an implied expectation suitable for Gate 2, while the retry treated the missing explicit contract and reporter version as Gate 0 blockers. Because the interrupted plan lived only in the removed container, the retry repeated classification instead of resuming approved evidence work. This is a workflow-quality and state-resume finding, not a Criteria engine crash, and no implementation solution exists to review.
- Interventions: one same-ticket retry after external cancellation; no source changes and no further retries, preserving the pre-run report rather than editing it to force a preferred verdict.

## Experiment 2 Overall Evaluation

- Ten Linear objects were created as CRI-53 through CRI-62 and executed with no overlapping Compose workflows.
- Six reached Done with merged PRs: CRI-53/#340, CRI-54/#341, CRI-57/#342, CRI-58/#343, CRI-59/#344, and CRI-60/#345. All required GitHub checks passed.
- Two source-backed bug candidates, CRI-55 and CRI-56, were rejected as `insufficient_report` because they lacked reporter versions and executable reproduction details. Both remain In Review without PRs.
- The CRI-61 red herring received the strongest QA treatment: a two-ref harness confirmed the partial-map premise, disproved persistence at the caller, and correctly returned `not_reproduced` without generating code.
- CRI-62 met the experiment's special requirement on its first launch: QA accepted the intermittent report, approved the deterministic saturation plan, and began execution. External cancellation exposed weak restart durability; the second launch regenerated intake artifacts and produced an inconsistent `insufficient_report` decision instead of resuming the approved plan.
- Central operation goals were substantially delivered. CRI-59 added remote run observation with history/live continuity, and CRI-60 added centrally assigned long-lived container agents with authentication, sequential execution, reconnect, and exactly-once event replay.
- State and recovery outcomes were mixed. CRI-57 fixed shared-home run-state ownership, but two canceled CRI-54 attempts demonstrated that uncommitted worktree edits are lost with the removed container, and CRI-62 demonstrated that approved QA plans are not durable across launches.
- Operator interventions in Experiment 2 were one initial image rebuild and three same-ticket retries after unexplained Docker `context canceled` interruptions: two for CRI-54 and one for CRI-62. No workflow or Criteria source was changed during this experiment, and no engine workaround was introduced.

## Experiment 3: Criteria Server on Castle

Date planned: 2026-08-30

Status: running — Wave 1

The final experiment spans `brokenbots/criteria` and `brokenbots/castle`. It will add Criteria-compatible services, durable assignment and lease handling, operator submission/control, and a Docker Compose system test in which long-lived Criteria containers connect to Castle.

The dependency-ordered ticket plan, two-lane execution rules, Compose topology, and project exit criteria are maintained in [criteria-castle-server-project.md](criteria-castle-server-project.md).

Preflight update: Castle and Parapet were extracted with relevant history and published as `brokenbots/castle`. Baseline `284d04b2af379da6c195f472ab9a8e0ee563b13f` passes local Go race tests, all 35 Parapet tests, Buf lint/drift checks, and a container health smoke test. The workflow identity has write access and the reviewer identity has admin access, so Castle Linear jobs can run through `linear_intake_v1`.

The initial GitHub Actions run `33351574243` exposed a baseline Castle defect under `go test -race ./...`: `TestWatchRun_CursorUpdate_Coalesced` delivered 500 events but persisted cursor sequence 451 after the final sequence-500 write returned `SQLITE_BUSY`. No direct source fix is retained. The evidence is planned as CSO-00, the first Castle Linear intake job, so QA and the implementation workflow own diagnosis, repair, review, and merge.

Criteria `v0.5.12` was released from main commit `8ef3c05514ede491a48a1e7a9715acf29d11c43b` by successful gated release run `33351961059`. All expected release assets were published, and both Linux archives passed the published SHA256 manifest. The Linear intake image was updated from `v0.5.11` to `v0.5.12`, rebuilt as `sha256:d27d1c64643fc7a46e9402aee6e581306d017f2b4c79acf2353a6b16cac09577`, reported `v0.5.12`, and validated `linear_intake_v1` successfully.

Fourteen Linear objects were created: CRI-63 through CRI-67 target `brokenbots/criteria`, and CRI-68 through CRI-76 target `brokenbots/castle`. The planned-key mapping is recorded in [criteria-castle-server-project.md](criteria-castle-server-project.md). Wave 1 starts CRI-63 (CSC-01) and CRI-68 (CSO-00), with one active job in each repository.

### CRI-63 run

- Result: completed in 19m39s on its isolated launch; Linear ended in Done.
- Workflow behavior: intake classified CSC-01 as a feature, generated a workstream, and implemented additive bootstrap credentials in the registration response and server client. Local build, race tests, lint, protocol drift, vulnerability, validation, plugin, and example gates passed. The workflow opened PR #346, waited through remote CI, obtained independent approval from `brokenbot`, merged, removed its worktree and branch, posted the completion comment, and closed the Linear issue.
- Solution review: PR #346 adds `bootstrap_credentials` as additive field 3 on `RegisterResponse`, captures the map synchronously, returns defensive copies through the client accessor, and logs only credential key names. Existing no-credential registration remains supported. Eleven required GitHub checks passed; the release-artifact PR check was correctly skipped.
- Interruptions: two earlier launches were canceled during the developer adapter step when later shell commands entered the same attached terminal; both traces contained literal `^C` immediately before Criteria reported `context canceled`. The third launch ran in a dedicated terminal with no concurrent shell command, traversed the same development path, and completed normally. This falsified the intrinsic adapter-crash hypothesis for those incidents and confirmed terminal-delivered SIGINT as the cause.
- Final artifacts: https://github.com/brokenbots/criteria/pull/346 (merged as `a88b551c2af4e7063c5eb1e652a037cbde6bd3e0`).

### CRI-64 run

- Result: run `dca12ea9-c178-4a93-ab20-554dde1e8a68` completed in 19m31s; Linear ended in Done.
- Workflow behavior: intake classified CSC-02 as a feature and implemented the portable operator submission/disposition contract on Criteria main after CRI-63. The change added `SubmitWorkflowAssignment`, `GetAssignmentDisposition`, the queued/leased/terminal/rejected state model, idempotency and authorization documentation, regenerated Go/Connect bindings, and contract coverage without changing Control-stream delivery.
- Validation: local proto lint/drift, build, race tests, lint, conformance, coverage, vulnerability, and CI gates passed. PR #347 passed all eleven required GitHub checks plus the aggregate gate; the release-artifact check was correctly skipped. Independent review approved and found no blocking issue.
- Final artifacts: https://github.com/brokenbots/criteria/pull/347 (merged to Criteria main as `788c8805e9eb35d831ba38f0b4a76d867de339cc`). The workflow removed the worktree and branch, posted completion, and moved CRI-64 to Done.

### CRI-65 run

- Initial result: run `f1d7004d-2a09-464e-a71e-a86661d30e4b` implemented `criteria submit`, opened PR #348, and completed the local `make build test` race gate and all eleven required remote checks; the release-only check was skipped. The command submits through the real Connect handler, prints the run ID, passes labels and an idempotency key, shares TLS options with `criteria watch`, optionally watches through terminal status, and exposes stable invalid/unreachable/duplicate/authentication exit codes with handler-level tests.
- Review feedback: GitHub code quality opened two duplicate threads for a possible nil dereference after workflow parsing. The workflow added an explicit nil-spec guard in commit `8311c4cb60df5578db110bf604366b432fdec33a`, reran the full race gate, replied to and resolved both threads, and obtained independent approval of the repaired head.
- Workflow failure: the first approval applied to the superseded commit and was dismissed when the repair was pushed. A subsequent reviewer created a pending review on the repaired head, but `post_approval` received HTTP 422 instead of submitting it. The handler then repeated review until its bounded loop failed, moved Linear to In Review, and ended after 39m33s despite green CI and zero unresolved threads.
- Intervention: submitted the already-created `brokenbot` pending review as approval without changing product or workflow source, then restarted only CRI-65 so the existing branch and PR retained ownership of merge and cleanup.
- Restart result: run `9f4541ef-c23f-4917-bd26-0c80caa7cc8d` found PR #348 ready and already merged, removed its worktree and branch, posted completion, and moved Linear to Done in 40.3s. Criteria main advanced to `add3c0795fc85a504048b9d59c38f8ae1dc632a2`.
- Final artifacts: https://github.com/brokenbots/criteria/pull/348.

### CRI-66 run

- Result: run `8928d706-6b9c-4627-9ce6-401d79aa6f26` completed in 36m02s; Linear ended in Done.
- Workflow behavior: intake classified CSC-04 as a feature and implemented central terminal reporting for assigned-run compile and initialization failures. The agent now creates the run publisher before those failure points, persists `RunFailed` with the assigned run ID, remains available for subsequent assignments, and relies on the existing `(run_id, correlation_id)` server deduplication during reconnect and replay. A supporting `RunPublisher.Drain` counter fix prevents in-flight events from being dropped by the new publish-drain-close path.
- Validation: focused compile-failure, initialization-failure, subsequent-assignment, and reconnect-no-duplicate tests passed, including repeated and race-enabled runs. The full `make build test` gate passed across the root, SDK, tools, and workflow modules; lint baseline, import lint, coverage, vulnerability scan, protocol checks, and all eleven required GitHub checks passed.
- Repair cycle: PR #349's first cloud CI run found one new `gocritic` `hugeParam` diagnostic because `startTestAgentOpts` passed the 216-byte `agentOptions` value by copy. The workflow changed the helper and its two call sites to use `*agentOptions`, committed the localized repair as `3443a98`, pushed it to the existing branch, and reran the full race and lint gates successfully. Independent implementation and PR reviews approved the repaired head with no blocking findings or open threads.
- Final artifacts: https://github.com/brokenbots/criteria/pull/349 merged to Criteria main as `b996eed9977967d8439e355086f69b4bbead06ba`. The workflow removed the worktree and branch, posted completion, and moved CRI-66 to Done.

### CRI-67 run

- Initial result: run `c5b140c6-0b63-4f37-901e-f9b22b86a098` implemented persisted per-run recovery, canonical run reattachment, duplicate-delivery handling, and correlation-ID event replay. The first implementation review found that recovery incorrectly used the freshly registered agent identity; the fake server's constant identity masked the ownership failure. The workflow repaired this in commit `df210d9` by authenticating recovered runs with persisted owner credentials, pairing assignments with their owning clients, issuing unique fake-server identities, enforcing ownership, and strengthening the cross-incarnation tests.
- Initial workflow failure: the repaired branch passed the full race, build, lint, coverage, and vulnerability gates and opened PR #350 with all eleven required checks green. The PR reviewer adapter then failed after emitting malformed tool-call text while reading the large diff, before posting a review. The run ended after 51m55s, left the remote branch and PR intact, and moved Linear to In Review.
- Restart and review feedback: run `a5eb43d0-9f39-488f-900c-f9510c4cb845` resumed the existing branch and PR. Independent review found two remaining terminal-idempotency defects: cancellation cleanup keyed off the run context rather than the agent context, preserving stale state after a terminal user cancellation; and a server-terminal resume path removed local state but then continued into fresh execution. The workflow fixed both paths, added a regression assertion, committed `4d5b9aa`, and pushed it to the same PR.
- Validation and final result: focused queued/running/terminal duplicate, crash-recovery, cancellation, reattach, and exactly-once replay tests passed under the race detector. The complete `make ci`, lint, import lint, coverage, build, vulnerability, and remote check suites passed. Independent implementation and PR reviews approved the repaired head with no blocking findings. PR #350 merged to Criteria main as `bd440f99dd921c0330d384a3943119bd388e9087`; the workflow removed the worktree and branch, posted completion, moved CRI-67 to Done, and completed the restart in 35m14s.
- Final artifacts: https://github.com/brokenbots/criteria/pull/350.

### CRI-68 run

- Initial result: completed normally in 21.9s and moved Linear to In Review with `needs_human`. The classifier correctly reported that the ticket contained only its planned key and repository, with no observed behavior, expected behavior, reproduction, version, or environment.
- Setup diagnosis: the operator's ticket-generation loop compared the planned-key marker against the entire Markdown heading, which also contained the title. The extraction therefore appended no canonical section body to any of CRI-63 through CRI-76; all descriptions were only 80–82 bytes. CRI-63 happened to remain actionable as a feature from its title, but CRI-68 appropriately rejected the evidence-free bug report.
- Intervention: repaired all fourteen Linear descriptions from their canonical plan sections using a prefix match, verified every description is at least 600 bytes, spot-checked CRI-68's GitHub Actions run, assertion, `SQLITE_BUSY` warning, and acceptance criteria, and verified CRI-69 retained its cross-repository dependencies. CRI-68 alone was moved from In Review back to New for a same-ticket retry. No Criteria, Castle, or workflow source was changed.
- Second result: the repaired ticket classified correctly as a bug and preserved the supplied CI evidence, but `qa_triage_v1` failed during `resolve_stable_ref` before agent investigation. Castle has no semver tag, so the empty `STABLE_REF` fallback pipeline returned nonzero even with `TEST_REFS=main`.
- Second intervention: retry with explicit `STABLE_REF=284d04b2af379da6c195f472ab9a8e0ee563b13f`, the ticket's known Castle baseline. This only satisfies unconditional ref preflight; triage remains scoped to `main`. CRI-68 alone was returned to New. No product or workflow source was changed.
- Third result: run `206be69a-aba4-4ea9-9f85-6c24144eabd4` reached QA triage, approved a sound deterministic reproduction plan, then failed its mandatory pre-reproduction `make build` because the fresh clone had no Parapet dependencies (`tsc: command not found`). The supervisor correctly returned `insufficient_evidence`; two plan refinements could not alter the subworkflow's external build gate, and `plan_repro` exhausted its three visits. The run completed in 4m21s, posted an infrastructure-failure comment, and returned CRI-68 to In Review. No reproduction or product change occurred.
- Third intervention: use `BUILD_CMD='make bootstrap && make build'`. Castle's root Makefile documents `bootstrap` as the fresh-clone dependency installation (`go work sync` and `cd parapet && npm ci`), after which the existing full-repository `make build`, `make test`, and `make ci` gates remain authoritative. Keep the explicit Castle baseline and main-only matrix, and return only CRI-68 to New.
- Fourth result: run `fd824f3d-f480-4643-ac6f-d34404585d20` showed that repro-runner expands `BUILD_CMD` as command words rather than evaluating shell syntax. All three reproduction attempts therefore invoked Make with `&&` as a literal target and failed with `No rule to make target '&&'`; plan refinement again could not alter the external build gate. The run completed in 4m53s, posted an infrastructure-failure comment, and returned CRI-68 to In Review. No reproduction or product change occurred.
- Fourth intervention: replace the unsupported shell compound with `BUILD_CMD='make bootstrap build'`. A Make dry-run verified this single invocation executes `go work sync`, `npm ci`, Castle build, and Parapet build in order. Retain the native `make test` and `make ci` gates, explicit baseline, and main-only matrix, and return only CRI-68 to New.
- Fifth result: run `94f2a34c-2296-498f-adf7-1a7e87f65032` reproduced the defect, published an independently approved workstream, implemented the fix on branch `CRI-68`, and opened https://github.com/brokenbots/castle/pull/1. The handler's bounded review verified the required 20/20 race and non-race coalescing runs, both deterministic regressions, the full Castle race suite, and `make ci`; it approved the patch with a clean worktree. Cloud CI later reported the `go` check failed, after which the PR feedback agent escalated into 200-, 500-, and 1,000-run race stress tests and additional rounds without converging on a repair. The operator terminated the run during that excessive investigation to stop wasting compute. PR #1 and its implementation commit remain intact; Linear remains in review rather than being marked Done.
- Restart result: run `55621d1a-26fe-4725-83bf-08c93cb68ca5` resumed the confirmed bug and existing PR rather than repeating QA triage. It identified the cloud failure as the coalescing test's race-sensitive upper bound (`11` writes against a ceiling of `10`), widened the asserted ceiling to `15` with justification while preserving the coalescing check, and made local Castle race gates explicitly use `CGO_ENABLED=1`. The focused workstream checks, deterministic regressions, full race suite, and `make ci` passed; both duplicate GitHub CI suites passed all six jobs. Independent review approved with no open threads. PR #1 merged to Castle main as `9914be43a69837f73cbca16941dea289a9ececa2`, the remote branch/worktree were cleaned up, Linear moved to Done, and the run completed in 22m06s.

### CRI-69 run

- Initial result: run `d7281111-ece9-45c4-8f9a-04ba1e687f50` implemented the broad Criteria protocol and Parapet migration and passed the repository's full `make ci` gate: Castle race tests, Parapet's 35 tests, SDK tests, build, Buf lint, and generated-code drift. It also exercised live health/service endpoints and added storage/protobuf boundary and event round-trip coverage.
- Workflow failure: the independent review agent failed after 49.9s while emitting malformed tool-call text, before it could return an approval or rejection. No PR had been created and the branch had not yet been pushed. Because Compose uses an ephemeral `run --rm` container with no `/data` volume, the committed worktree was removed with the container. The run ended after 23m41s and moved Linear to In Review.
- Intervention: verified that neither `refs/heads/CRI-69` nor a new PR ref existed remotely. Resolved the immutable Criteria SDK dependency as `github.com/brokenbots/criteria/sdk v0.0.0-20260831144929-add3c0795fc8`, backed by Criteria commit `add3c0795fc85a504048b9d59c38f8ae1dc632a2`, and appended that exact coordinate to CRI-69 so the retry consumes the module rather than copying an SDK tree. No Criteria, Castle, or workflow source was changed directly.
- Retry result: run `d7c210e5-3db3-47a1-812e-e668ceb5557d` rebuilt the migration from Castle main, consumed the pinned SDK module, removed the legacy `overlord.v1`, shared SDK, and generated client trees, registered Criteria and Server services with health/reflection, isolated persistence behind protobuf-neutral events, and migrated Parapet to the generated Criteria API. The implementation and all repair cycles passed `make ci`, including Castle's full race suite, Parapet's 35 tests, Buf lint, and generated-code drift.
- Review feedback: implementation review first caught an incomplete `overlord-codec` to `criteria-codec` meta-tag rename. PR review then caught a dangerous `PauseRun` implementation that returned success after enqueueing `RunCancel`; the workflow changed it to `CodeUnimplemented`. A subsequent review required a focused regression proving `PauseRun` neither succeeds nor enqueues cancellation, and the test was verified to fail against the old behavior. All findings were repaired on the same branch and revalidated.
- Final result: Castle PR #2 passed all six checks and received approval after the repair commits were pushed. It merged to main as `d3a316ee48c3feafc80523981233d8dd84fb2214`; the remote branch and workflow worktree were removed, Linear moved to Done, and the retry completed in 74m27s.
- Final artifacts: https://github.com/brokenbots/castle/pull/2.

### CRI-70 run

- Result: run `a93c5c3c-f700-4df0-ad0a-ce5386e4e3b8` aligned Castle registration with the Criteria SDK's `X-Server-Bootstrap` contract, updated the conformance subject to `RegisterAgent`, and added regressions proving issued bearer tokens are stored only as hashes and bootstrap/bearer secrets do not appear in logs. Existing auth-context identity and cross-agent ownership enforcement passed the full Criteria conformance suite.
- Validation: `make ci` passed locally, including Castle build and full race tests, Parapet build and all 35 tests, Buf lint, and generated-code drift. The six remote checks also passed. Independent implementation and PR reviews approved the change with only non-blocking stale-name and log-attribute coverage notes.
- Final result: Castle PR #3 merged to main as `5e644696c8bea41c9dac35c32dbdaa9f1637dd2c`; the remote branch and workflow worktree were removed, Linear moved to Done, and the run completed in 12m19s.
- Final artifacts: https://github.com/brokenbots/castle/pull/3.

### CRI-71 run

- Initial result: run `34d60e85-1426-4d01-88d0-2cb55673a9cb` implemented the complete Criteria run/event persistence lifecycle. Castle now rejects unknown or empty Envelope payload arms before acknowledgement, flushes pending scope before reattach, and has descriptor-driven event-arm coverage plus restart deduplication, replay-to-live-watch continuity, and reattach scope/signal regressions.
- Validation and repair: Castle race tests, Criteria conformance, Parapet's 35 tests, Buf lint, and generated-code drift passed. The Compose worker initially lacked `buf`; the workflow installed the pinned tool into its ephemeral environment and reran the native `make ci` gate successfully without changing repository setup. Implementation review found that the reattach regression did not assert the required `LastSeq` field. The workflow appended a non-zero event, added a regressive `LastSeq` assertion, committed the complete branch as `1a69882`, and received implementation approval.
- Initial workflow failure: PR #4 opened and all six Castle checks passed, but the PR reviewer adapter failed after emitting malformed tool-call text while inspecting test helpers. The run ended after 17m12s, left the remote branch and PR intact, and moved Linear to In Review.
- Restart result: run `2b70a656-35c6-42bb-a00e-57b317f258f2` resumed the existing branch and green PR. Independent review verified all five exit criteria and approved without further changes. PR #4 merged to Castle main as `2da7d786a20efeec02b538f87a6019ef2abbc01d`; the workflow removed the worktree and branch, posted completion, moved CRI-71 to Done, and completed in 4m25s.
- Final artifacts: https://github.com/brokenbots/castle/pull/4.

### CRI-72 run

- Result: run `f87c50c2-a28f-4cbe-aec9-93d257121862` completed in 38m40s; Linear ended in Done.
- Workflow behavior: intake classified CSO-04 as a feature and added Castle's durable SQLite assignment queue, transactional queue-and-run creation, restart-safe submission idempotency, eligibility and online-agent filtering, leases and attempts, assignment ownership, expiry handling, and guarded terminal dispositions. Migration, store, RPC, restart, and concurrent-claim regressions exercise the new lifecycle under the race detector.
- Review repair: implementation review found that submit-time dispatch could atomically lease the oldest matching queued assignment, compare it to the newly submitted run, and continue without sending either assignment, stranding the leased backlog item until expiry. The workflow changed dispatch to send whichever assignment wins the lease and added `TestSubmitWorkflowAssignment_DispatchesOldestQueuedAssignmentOnNewSubmit`, verified regressively. The repaired branch head was `20a4257`.
- Validation: Castle's full race suite, `go vet`, formatting, Parapet's 35 tests, Buf lint, generated-code drift, and the complete `make ci` gate passed. One later review invocation saw only a transient BSR `resource_exhausted` response while fetching remote generation plugins; no proto or generated files changed, the drift gate passed before and after the repair, and all six GitHub checks passed.
- Final result: independent implementation and PR reviews approved all queue, transaction, idempotency, eligibility, expiry, terminal, migration, and concurrent-claim criteria. Castle PR #5 merged to main as `f26c2cd859a27de125ae97784baea971a9bb2142`; the workflow removed its worktree and branch, posted completion, and moved CRI-72 to Done.
- Final artifacts: https://github.com/brokenbots/castle/pull/5.

### CRI-74 run

- Result: run `41d93302-fe60-413a-b569-6856b25b63ec` completed in 36m42s; Linear ended in Done.
- Workflow behavior: intake classified CSO-06 as a feature and implemented `PauseRun`, `ResumeRun`, `InspectRun`, and `SendPrompt`, with stop/pause/resume/prompt commands routed through the authenticated owner's Criteria Control stream. Offline and terminal states return deterministic failed-precondition or unavailable errors, inspection is read-only, and ownership negatives cover every new RPC.
- Protocol and tooling: the published Criteria SDK pin did not contain the required `PauseRun` service contract, so the Castle-owned change added a local `criteria-sdk` module, extended the additive Criteria proto surface, regenerated Go and TypeScript bindings, and changed generation to use pinned local protoc plugins. This preserved Castle/Parapet's direct Criteria SDK contract without an Overseer compatibility layer. PR #6 consequently touched 53 protocol, SDK, generated, handler, test, module, and CI files.
- Review repair: implementation review found `InspectRun` scanning only the earliest 1,000 events, which could report a stale adapter and activity timestamp for long runs. The workflow added latest-event and latest-step store queries using descending sequence order, plus a regression with more than 1,200 events and multiple adapters. It also disclosed all 11 inherited `criteria-sdk` `//nolint` entries with file, rule, and justification and refreshed a stale `SendPrompt` proto comment. The complete repaired branch head was `60fa4b5`.
- Validation and final result: Castle's full race suite, local Criteria SDK tests, Parapet's 35 tests, build, Buf lint, generated-code drift, and `make ci` all passed. Independent implementation and PR reviews approved the repaired behavior, backward-compatible protocol changes, authorization, and security posture. Castle PR #6 merged to main as `bbc238646f4d497710dc8139588cad797d615e22`; the workflow removed its worktree and branch, posted completion, and moved CRI-74 to Done.
- Final artifacts: https://github.com/brokenbots/castle/pull/6.

### CRI-73 run

- Result: run `59595109-63d1-4450-b69a-9d67d1f3a3c9` completed in 27m26s; Linear ended in Done.
- Workflow behavior: intake classified CSO-05 as a feature and connected Castle's durable assignment queue to the Criteria Control registry. Dispatch is label-aware, a global lease lock prevents one agent from receiving two unstarted assignments concurrently, and run lifecycle events trigger continued dispatch so one agent works sequentially while distinct eligible agents work concurrently.
- Lease and recovery behavior: only pending, unstarted leases expire; expiry clears the stale overseer owner and requeues the assignment for redispatch. A disconnect after `RunStarted` retains ownership for safe reattachment rather than concurrent duplicate execution. Castle restart recovery preserves queue and lease state and redelivers active pending leases when the assigned agent reconnects.
- Validation and review: dedicated regressions cover all five exit criteria, and Castle's full race suite, `go vet`, build, Parapet build and all 35 tests, Buf lint, generated-code drift, and the complete `make ci` gate passed. Implementation review approved with one non-blocking test-hardening suggestion to assert explicitly that no second message arrives before `RunStarted`; PR review independently approved the store guard and lease-lock invariants with all six GitHub checks green.
- Final result: the clean two-commit implementation ended at branch head `a0cb0a7`. Castle PR #7 merged to main as `5469701b6cda02db3139e95618ac1effab1ed8d4`; the workflow removed its worktree and branch, posted completion, and moved CRI-73 to Done.
- Final artifacts: https://github.com/brokenbots/castle/pull/7.

### CRI-75 run

- Initial attempt: run `4d1fbbbc-7513-4c5e-9ad1-9ce3544e2035` implemented an in-process Castle conformance subject using real Connect handlers and SQLite, added Castle-specific assignment, bootstrap, ownership, restart, and negative-authorization scenarios, and passed the standalone race target and full `make ci`. The implementation reviewer then emitted malformed tool-call text while checking RPC coverage. The run failed after 7m25s before pushing commit `2d0ce71` or opening a PR, moved Linear to In Review, and lost the ephemeral worktree as expected.
- Recovery result: run `5f486b0e-6956-4e82-a3bd-f9dfa3379a1d` restarted from unchanged Castle main, reproduced and broadened the conformance implementation, added `test-conformance` and `compose-conformance` Make targets, wired conformance into `make ci`, and added a dedicated `Dockerfile.conformance` plus Compose profile service so the suite can run independently from the system test.
- Review repair: implementation review found a substantive coverage gap behind the initial claim that every Criteria RPC had a success case. Ten RPCs had only authorization or error coverage: `Heartbeat`, `ListAgents`, `GetAgent`, `ListRuns`, `GetRun`, `WatchRun`, `PauseRun`, `ResumeRun`, `InspectRun`, and `SendPrompt`. The workflow added owner-authenticated success scenarios with substantive record, control-message, state-transition, and live-event assertions for all ten and removed the unnecessary dependency from the in-process conformance service to the Castle container. The repaired branch head was `fc4ba0c`.
- Validation: the dedicated conformance suite passed under `-race`, the full Castle race suite and `go vet` passed, Parapet's 35 tests passed, and build, Buf lint, generated-code drift, and `make ci` all passed. Reviews confirmed every Criteria RPC now has success plus relevant authorization/error coverage, no timing-only sleeps are used, restart/lease timing uses controlled state, and the standalone Compose invocation is valid. All six GitHub checks passed.
- Final result: Castle PR #8 merged to main as `c5ee769eef5a5d8a09cdcc230020a3a1e0fc830e`; the workflow removed its worktree and branch, posted completion, moved CRI-75 to Done, and completed the recovery run in 32m52s.
- Final artifacts: https://github.com/brokenbots/castle/pull/8.

### CRI-76 run

- Result: run `6f24692b-ba9e-410d-909c-a3dcbef12efe` completed in 51m30s; Linear ended in Done.
- Workflow behavior: intake classified CSO-08 as a feature and added a Castle-plus-Criteria-agent system topology with two long-lived, label-specific test agents, a separate submission/control harness, dedicated agent and harness images, named Castle SQLite and per-agent home volumes, health checks, deterministic timeouts, and failure log collection. The harness covers two routed successful workflows, durable queued work, invalid-workflow failure visibility, agent and Castle restart behavior, ordered replay, and stop/pause/resume operations.
- Review repair: initial implementation review found three blockers despite green repository CI. The separate control client registered a fresh identity and therefore could not control an agent-owned run; duplicate detection keyed on `(sequence, correlation ID)` and could never detect a repeated correlation ID; and the new reattach execution path lacked restart coverage. The workflow changed the separate control container to load the owning agent's persisted token from a read-only volume, keyed duplicate checks on correlation ID alone, and added regressions proving owner controls, duplicate detection, process-restart reattachment, and exactly-once replay. The repaired branch head was `8880bdc`.
- Validation: focused agent and harness integration tests, full Castle race tests, Criteria conformance, `go vet`, Castle and Parapet builds, all 35 Parapet tests, Buf lint, generated-code drift, and repeated `make ci` gates passed. Both reviews approved the remediation and all six GitHub checks passed. Non-blocking notes remain for unused client/config fields, a single-agent in-process integration test, and absent Compose restart policies.
- Validation limitation: the intake container had no Docker or Podman runtime, so it could not execute the live `docker compose up --build` smoke topology. Compose wiring, image paths, volumes, service names, and harness commands were statically reviewed, while equivalent in-process Castle/agent behavior ran under the race detector. This does not satisfy the project's required clean-volume and reused-volume Compose repetitions.
- Final result: Castle PR #9 merged to main as `da34b2a36cee982af9f8819fc9042558d7948d54`; the workflow removed its worktree and branch, posted completion, and moved CRI-76 to Done.
- Final artifacts: https://github.com/brokenbots/castle/pull/9.

## Experiment 3 project review

Observed final revisions: Criteria `bd440f99dd921c0330d384a3943119bd388e9087`; Castle `da34b2a36cee982af9f8819fc9042558d7948d54`.

1. **Met:** all fourteen implementation tickets, CRI-63 through CRI-76, have recorded terminal dispositions; each ended in Done after any same-ticket recovery.
2. **Met:** every merged Criteria and Castle PR passed its repository's required GitHub checks before merge.
3. **Met:** Castle and Parapet use `criteria.v1`; runtime `overlord.v1` services and the Overseer compatibility path were removed rather than retained alongside Criteria.
4. **Met:** bootstrap success, missing/incorrect bootstrap credentials, issued bearer-token use, ownership, and negative authorization are covered by Criteria and Castle conformance tests without credential logging.
5. **Met in executable integration coverage:** submission, label matching, sequential execution by one agent, and concurrent execution by distinct matching agents are covered under the race detector.
6. **Partially demonstrated:** agent restart, Castle persistence, canonical run identity, queue/lease recovery, monotonic sequence handling, and correlation-ID deduplication are covered by focused restart and in-process integration tests. The final live multi-container restart sequence was not executed because no container runtime was available inside the intake worker.
7. **Met:** assigned compile and initialization failures persist a centrally watchable terminal `RunFailed` event and do not prevent the agent from accepting later work.
8. **Met in executable integration coverage and static Compose review:** stop, pause/resume, inspect, and watch traverse Criteria services; the separate control container uses the executing owner's persisted token read-only. The live containerized path remains part of the unexecuted Compose smoke.
9. **Not met:** the full Compose smoke test has not passed twice from clean volumes and once with the Castle volume preserved and reused. No live Compose run was possible in the CRI-76 environment.
10. **Met:** this ledger records each ticket's workflow result, same-ticket retries, review repairs, PR, merge SHA, validation, infrastructure limitations, and residual risks.

Project disposition: all implementation tickets are complete and merged, but Experiment 3 is not yet fully complete because exit criterion 9 remains unverified. The required next action is to run the merged `compose.system.yml` smoke twice with clean named volumes and once reusing the Castle volume, retain the logs/results here, and repair any live-only defect through a new explicitly scoped ticket rather than rewriting CRI-76's completed disposition.

### Experiment 3 live Compose validation

- Clean-volume repetition 1 at Castle `da34b2a36cee982af9f8819fc9042558d7948d54` failed before the submission harness started. Both agents registered successfully, opened Control streams, and continued sending authenticated heartbeats, but Compose marked both containers unhealthy.
- Root cause evidence: the health command used `pgrep -x criteria-test-agent`; Alpine `pgrep` rejects exact process-name patterns longer than 15 characters, returned exit 1 on every check, and advised using `pgrep -f`. Castle remained healthy and both agent processes remained running, so this was a health-check false negative rather than registration or runtime failure.
- Operator action: collected timestamped Castle/agent logs and Docker health history, removed the failed containers and all three named volumes, and created CRI-77, `Fix Criteria agent Compose health check`, as a new scoped Castle defect. The required three smoke repetitions remain pending until CRI-77 is merged.

### CRI-77 run

- Initial triage: run `1d80b028-e2ef-4a4a-8764-0f63fba6bb5e` was mistakenly launched with `TEST_REFS=main`. QA reproduced the `pgrep -x criteria-test-agent` failure with the real agent binary and Alpine-compatible `procps`, while confirming registration, Control streaming, heartbeats, and Castle health remained operational. The verdict correctly remained `insufficient_evidence` because the configured matrix omitted the stable arm; repeated planning exhausted `max_visits`, and the run moved Linear to In Review after 12m29s without implementation.
- Corrected result: run `bb802ad6-c170-43cf-82e1-2eb91928037b` used `TEST_REFS=both`. Main `da34b2a36cee982af9f8819fc9042558d7948d54` reproduced the false-negative health check, while stable `284d04b2af379da6c195f472ab9a8e0ee563b13f` did not contain the system Compose topology, establishing a regression. QA corrected two unsupported draft details before publishing the implementation workstream.
- Repair: both agent health checks now use `pgrep -f criteria-test-agent`. A regression test runs the actual Compose health command against a live in-process agent and gRPC health endpoint; reviewers independently verified it fails with the former `pgrep -x` form and passes with `pgrep -f`. No production binary, entrypoint, Castle health check, or `compose.local.yml` behavior changed. The branch head was `f25b0c7`.
- Validation and final result: full Castle race tests, `go vet`, build, Criteria conformance, Parapet's 35 tests, Buf lint, generated-code drift, and `make ci` passed; all six GitHub checks passed. Castle PR #10 merged to main as `7be0a57c690dd04a75a8d67825b8d2e683a4e3a6`; the workflow removed its worktree and branch, posted completion, moved CRI-77 to Done, and completed the corrected run in 32m15s.
- Final artifacts: https://github.com/brokenbots/castle/pull/10.

### Experiment 3 live Compose validation after CRI-77

- Clean-volume repetition 1 at Castle `7be0a57c690dd04a75a8d67825b8d2e683a4e3a6` confirmed CRI-77: Castle became healthy, both agents registered, opened Control streams, heartbeated, and became Compose-healthy, allowing the submission harness to start.
- The smoke then exposed a second live-only defect. `valid-alpha` submitted, dispatched to `agent-a`, and completed as run `3e10b6fb-ca58-4e0f-a195-fd65120cc0a6`; the immediately following `valid-beta` `SubmitWorkflowAssignment` returned `internal: database is locked (5) (SQLITE_BUSY)` while alpha dispatch/event writes were active. The harness exited 1 and emitted the expected Castle, agent, and client log block.
- Operator action: removed the failed topology and all named volumes and created CRI-78, `Prevent SQLITE_BUSY during concurrent assignment submission`, scoped to deterministic contention coverage and retry or serialization that preserves transactional queue/run creation and idempotency. The smoke matrix restarts after CRI-78 merges.

### CRI-78 run

- First operator error: run `6a9c5f95-9396-4998-a40a-ba67d11155ff` inherited the Criteria repository target. QA correctly returned `insufficient_report` because Castle owns the server SQLite path and the Compose reproducer was absent from Criteria; review routed it to In Review without a workstream.
- Second operator error: run `492909c5-c361-4227-9cb2-040594a015b5` targeted Castle but omitted its explicit stable baseline. Castle has no semver release tag, so unconditional `resolve_stable_ref` preflight failed before investigation and the ticket remained In Review.
- Corrected result: run `17516f83-5cf1-465d-b1d6-42fcfdd5fec6` targeted `brokenbots/castle`, used `TEST_REFS=both`, pinned stable extraction baseline `284d04b2af379da6c195f472ab9a8e0ee563b13f`, and supplied Castle's `make bootstrap build`, `make test`, and `make ci` gates.
- Repair: Castle configures SQLite with `SetMaxOpenConns(1)`, serializing access through one pooled connection as explicitly allowed by the reviewed workstream. Store-level and RPC-level deterministic contention regressions cover concurrent writers and assignment submission without duplicate runs. Full Castle race tests, Parapet tests, and all six GitHub checks passed; the independent reviewer approved with one non-blocking documentation note.
- Final result: Castle PR #11 merged as `a249f06f9288ad8a78e74363bd993628955acf4c`; branch `CRI-78` at `8e6d8c4` and its worktree were removed, Linear moved CRI-78 to Done, and the corrected run completed in 28m53s.
- Final artifacts: https://github.com/brokenbots/castle/pull/11.

### Experiment 3 live Compose validation after CRI-78

- Clean-volume repetition 1 at Castle `a249f06f9288ad8a78e74363bd993628955acf4c` passed phase 1: `valid-alpha` and `valid-beta` both submitted, routed to their matching agents, and completed. It also passed phase 2: run `43300cc3-af1e-4eb2-a372-9ae8ec1f77b7` reattached after the agent-a container restart, completed, and retained exactly-once event history.
- Phase 3 exposed another live-only defect. Run `e0800513-d5ba-468e-9da8-f41f5ed0f01a` began on agent-b before Castle restarted. Agent-b received EOF and immediately attempted `ReattachRun` while Castle was still unavailable, receiving connection refused. Although Control reopened one second later, reattachment was not retried; the executor retained its stale SubmitEvents stream, failed to emit the step outcome 30 seconds later with `write envelope: EOF`, then failed to report run failure through the same stream. Castle retained the run as `running`, and the harness timed out after 4m29s.
- Operator action: removed the failed topology and all named volumes and created CRI-79, `Recover in-flight runs after Castle restart`, scoped to retrying reattachment after availability returns, replacing stale event streams, preserving exactly-once replay, and reaching the true terminal run state. The smoke matrix restarts after CRI-79 merges.

### CRI-79 run

- Result: run `5ae32513-b562-4817-ba76-f72e39f86c3a` targeted Castle main `a249f06f9288ad8a78e74363bd993628955acf4c` and stable extraction baseline `284d04b2af379da6c195f472ab9a8e0ee563b13f` with the full Castle gates. QA confirmed the one-shot reattach and stale SubmitEvents failure and published a scoped recovery workstream.
- Repair: after a Control reconnect, the test agent re-handshakes in-flight runs from persisted `last_seq` and step state. Restarting a run executor cancels the stale goroutine so the replacement opens a fresh SubmitEvents stream; Castle's `(run_id, correlation_id)` dedup preserves exactly-once replay. Three focused race tests cover transient unavailability, executor replacement, and replay behavior.
- Validation and final result: all six GitHub checks passed. The independent reviewer reran the focused race tests plus Castle build/vet and approved, recording one non-blocking latent edge for a send failure that occurs before reattach supersedes the executor. Castle PR #12 merged; branch `CRI-79` at `06004cc` and its worktree were removed, Linear moved CRI-79 to Done, and the run completed in 50m50s.
- Final artifacts: https://github.com/brokenbots/castle/pull/12.

### Experiment 3 live Compose validation after CRI-79

- Clean-volume repetition 1 passed phases 1 through 4: labeled alpha/beta routing, agent restart recovery with exactly-once history, Castle restart recovery, and centrally visible invalid-workflow failure. This conclusively validates the CRI-78 and CRI-79 repairs in the live topology.
- Phase 5 exposed a harness orchestration defect. The pause/resume run `b89d4c41-c9dd-41e0-bc70-468402d5dd73` reached `paused`, and the harness launched the separate authenticated control identity via `docker run --rm` on the Compose project network. Its successful exit was observed by the active `docker compose up --abort-on-container-exit submission` monitor, which aborted the submission service with code 2 before pause/resume and stop assertions completed.
- Operator action: removed the failed topology and all named volumes and created CRI-80, `Keep one-off control clients from aborting Compose smoke`, scoped to retaining a genuinely separate control container and read-only owner token while making the documented one-command smoke lifecycle reach and report phase 5. The successful repetition count remains zero.

### CRI-80 run

- Initial result: run `9bc40649-a063-451e-8a89-b76103153977` could not reproduce inside the intake environment because it lacks Docker tooling and a daemon. QA returned `plan_rejected`; the reviewer correctly escalated the still-actionable report to In Review rather than closing it.
- Human confirmation: a Docker-capable host reran the exact documented command on current main and confirmed phases 1 through 4, the separate read-only-token helper's successful `ResumeRun`, and the following `Aborting on container exit` termination. The confirmation explicitly authorized a workstream without attempting the impossible in-container reproduction.
- Corrected result: run `a53c2c77-8065-4688-9284-4986b0e8877c` used that confirmation. The repair isolated one-off control helpers onto a separate Docker network while retaining their separate identity and read-only owner-token volume; command-construction, network-sequencing, and race-enabled integration tests passed. All six GitHub checks passed, the independent reviewer approved, Castle PR #13 merged, branch `CRI-80` at `b101d91` and its worktree were removed, and Linear moved the ticket to Done in 28m32s.
- Final artifacts: https://github.com/brokenbots/castle/pull/13.

### Experiment 3 live Compose validation after CRI-80

- Clean-volume repetition 1 at the CRI-80 merge again passed phases 1 through 4. The harness logged creation of isolated network `castle-system-test_control`, then launched the separate helper for paused run `3cf05b35-5fba-4617-aa16-3a168f672342`; the helper exited 0, but Compose still printed `Aborting on container exit` and stopped submission with code 2.
- This falsified CRI-80's network-membership hypothesis: Compose still observed the helper lifecycle despite it being attached only to the isolated network. Operator action: removed the failed topology and all named volumes and created CRI-81, `Run control helper outside Compose abort event scope`, to preserve separate-client/token isolation while removing the helper from the active Compose abort event scope. The successful repetition count remains zero.
