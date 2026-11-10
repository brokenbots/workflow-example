# Pinned doctrine excerpts (ratified 2026-10-18)
- Arming is a dave decision by standing contract; within a ratified arc the
  shepherd auto-advances per the manifest (auto-advance ruling, #175).
- Arm-time compile gate: before any arming, the route's pinned trees must
  compile under the engine stamp of the criteria-base image the run will
  fire on (last vX.Y.Z token in the tag). Gate:
  `tools/armgate/arm-gate.sh` in the criteria repo (brokenbots/criteria),
  one --tree per pinned tree, binary at/above that engine stamp; a
  non-zero gate refuses the arm and the card stays dormant. A tree is
  compiled only at fire time, so the base-roll gate (zero RUNNING runs)
  does not cover a roll landing under an armed card.
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