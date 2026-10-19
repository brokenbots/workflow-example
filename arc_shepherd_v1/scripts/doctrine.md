# Pinned doctrine excerpts (ratified 2026-10-18)
- Arming is a dave decision by standing contract; within a ratified arc the
  shepherd auto-advances per the manifest (auto-advance ruling, #175).
- Shepherd actions NEVER include code edits: trees/engine/operator move by
  arm card -> develop job -> PR (engineering lane).
- The DISARMED validation card (#184) is never shepherd-armed.
- Merge gate: ALL checks completed AND green, >=1 non-author approval,
  squash-merge under brokenbot ONLY, PR author MUST be handcaught (pinned
  per-child in the manifest; mismatch = gate violation -> consult).
- Stall handling: 3 consecutive no-change rounds -> guidance consult (this
  pathway); budget end -> summarize + end cycle (no in-run extension).
- Cycles end, then the orchestrator lane (human + hermes) relaunches after
  evaluating shepherd+guidance quality.