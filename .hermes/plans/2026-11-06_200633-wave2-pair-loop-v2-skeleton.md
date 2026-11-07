# Wave 2 — Upstream-Sync Stage 2: bind WORKFLOW_UPSTREAM_TOKEN (card 385 tracking) + the tree-skeleton PR body

## Status
- Card KB-375 (Wave 1 live validation): closed. handler_complete @ Succeeded; PR #252 merged; product PR #256 merged same-day.
- Card KB-385 (`/tmp/plan_wave2.md` body, written 2026-11-06): **ARMED**, mapped to the ENGINE/OPERATOR lane (interface-schema change in `workstation` handler) — running now via its CriteriaRun.

## The bridge (the one wiring rule to carry into every future credential)
`workstation_handler_v1` variables are typed, BUT the credential VALUE never rides var plumbing (secret taint). Instead:
1. The binding env map names `WORKFLOW_UPSTREAM_TOKEN` → maps to the CSI-projected file.
2. The adapter containers receive `WORKFLOW_UPSTREAM_TOKEN=<CSI file path>` via `adapterEnvs()` (volumeEnvs + secretEnvs) — verified stamped in the launch manifest `lanes`, and read directly by the pushed file's script (`sed -e s/^[[:space:]]*$//` reads it).
3. The step's declared `env` input bridges it INTO the shell command when the step needs it inline: `[scripts] env = '${var.workflow_upstream_token}'` → the binding value names the file; `git`'s run-time inline credential helper reads the file at credential time.

That's the FULL wiring: **binding env value → file path → adapter container env → read by the script under `templatefile`**. Zero token material in argv, logs, or git config. Rebinding gitlab later changes ONLY the value-side mapping in the routes CM github-tokens lane: zero tree change.

## Next phase task breakdown (Wave 2 execution)
Per the body in `/tmp/plan_wave2.md`:
- The CriteriaRun for KB-385 is in the dev lane's serial queue (length 1 behind kb-375's lane-drain at this hour).
- On completion: verify the binding contract test holds and the CI contracts suite gains the leg (see Acceptance).
- Post-completion close: the workstream republishes the binding verbatim (KB-310 fail-closed).

## The two standing gates to respect at this hour
1. No refire caps — fix root causes and shepherd the run indefinitely.
2. Merge waits on the squash commit's OWN CI to complete green BEFORE reconciling the ticket to Done (KB-311/313 discipline).


## 2026-11-07 UPDATE — body pass landed + card 385 closed
- Body pass complete (deleg_4d1f601b): TODO-2..-6,-8..-10 closed, TODO-1 settled-not-needed, TODO-7 untouched.
- My independent audit: 0 string-contains, 0 steps.outcome reads, 0 secrets, 4 deep adapters + 4 lockfile entries, `brokenbons` typo 0; all 9 remaining TODO texts are COMMENT-only (no runtime values).
- One defect found in MY audit, fixed by me: pair_part_verify.sh.tftpl crash under `set -u` when CI_GATE_CMD unset (interface leaves ci_gate empty) → `${CI_GATE_CMD:-}`; re-verified green/foreign/dirty/gate-green/gate-red arms in a synthetic repo; container validate re-run → 0 Errors.
- Card 385: CR Succeeded handler_complete @03:34:31Z; run succeeded (set_done_state, 'task moved to Done'); pr_url empty by design (probe card, no code change); evidence comment posted (KB-51); disarmed + closed.
