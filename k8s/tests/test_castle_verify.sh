#!/usr/bin/env bash
set -euo pipefail

# Regression test for the KB-342 castle verification harness
# (scripts/castle_verify.mjs). Exercises the pure checklist logic against
# pinned wire fixtures — no network, no cluster:
#
#   - podstate event parsing on BOTH wire shapes (connect protojson camelCase
#     Envelope with google.protobuf.Struct data fields, and the stored
#     snake_case ndjson line) — seq extraction and dedupe/sort;
#   - non-podstate event kinds and malformed payloads are ignored;
#   - disposition verdict mapping (state, leased_criteria_id — the f18ceef3…
#     agent lane —, expected-prefix match, rejection reason);
#   - run-row verdict (workflow/status/criteria; absent project field is a
#     documented observation, not a failure);
#   - refusal classification (KB-326 S3 isolation: anonymous pass refused,
#     class gates refused, uniform not_found);
#   - the evidence bundle renders the card's close-out fields: assignment
#     disposition, CR name, podstate seqs — and the log-only parity leg.

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
HARNESS="$REPO_ROOT/scripts/castle_verify.mjs"

fail() { echo "FAIL: $1" >&2; exit 1; }
pass() { echo "PASS: $1"; }

command -v node >/dev/null 2>&1 || fail "node is required"

# The test driver requires the harness to be importable (exported pure
# functions) — import it once up front so a syntax error fails loudly here.
node --input-type=module -e "import('$HARNESS').then(() => process.exit(0), (e) => { console.error(e.message); process.exit(1); })" \
  || fail "castle_verify.mjs does not import cleanly"

RESULTS="$(node --input-type=module - "$HARNESS" <<'EOF'
import { createRequire } from 'node:module';
const require_ = createRequire(process.cwd() + '/package.json');
const harnessUrl = process.argv[2];
const { parsePodstateEvent, podstateSeqs, dispositionVerdict, runRowVerdict, refusalObservation, assembleEvidence } = await import(harnessUrl);

const fails = [];
const eq = (label, got, want) => {
  const g = JSON.stringify(got), w = JSON.stringify(want);
  if (g !== w) fails.push(`${label}: got ${g}, want ${w}`);
};

// --- podstate: connect protojson shape (camelCase, Struct fields) ---
const connectShape = {
  seq: '12',
  ts: '2026-10-26T20:14:10Z',
  adapterEvent: {
    step: 'develop',
    adapter: 'intake',
    kind: 'adapter.podstate.observed',
    data: { fields: {
      adapter: { stringValue: 'intake' },
      adapter_type: { stringValue: 'copilot' },
      scope_name: { stringValue: 'workspace' },
      scope_instance_id: { stringValue: 'a14c03cda2b2' },
      pod: { stringValue: 'kb-342-peer-xyz' },
      phase: { stringValue: 'Running' },
      reason: { stringValue: '' },
      message: { stringValue: '' },
      observed_at: { stringValue: '2026-10-26T20:14:10Z' },
    } },
  },
};
const p1 = parsePodstateEvent(connectShape);
if (!p1) fails.push('connect shape: not recognized as podstate');
eq('connect seq', p1?.seq, 12);
eq('connect phase', p1?.phase, 'Running');
eq('connect adapterType', p1?.adapterType, 'copilot');
eq('connect scope', p1?.scopeInstanceId, 'a14c03cda2b2');

// --- podstate: stored snake_case ndjson line shape ---
const stored = {
  schema_version: 1,
  seq: 17,
  run_id: '7d8cd36e',
  payload_type: 'AdapterEvent',
  payload: {
    adapter: 'intake',
    kind: 'adapter.podstate.observed',
    data: {
      adapter: 'intake', adapter_type: 'copilot', scope_instance_id: 'a14c03cda2b2',
      scope_name: 'workspace', pod: 'kb-342-peer-abc', phase: 'Succeeded',
      reason: '', message: '', observed_at: '2026-10-26T20:20:00Z',
    },
  },
};
const p2 = parsePodstateEvent(stored);
if (!p2) fails.push('stored shape: not recognized as podstate');
eq('stored seq', p2?.seq, 17);
eq('stored phase', p2?.phase, 'Succeeded');

// --- non-podstate and malformed envelopes are ignored ---
eq('non-podstate ignored', podstateSeqs([{ seq: '5', adapterEvent: { kind: 'adapter.lifecycle.provision_wanted', data: {} } }]), []);
eq('malformed ignored', podstateSeqs([null, {}, { seq: 'x', adapterEvent: { kind: 'adapter.podstate.observed' } }]), []);

// --- dedupe, uint64-string compare, sort by castle-stamped seq ---
{
  const events = [
    { seq: '3', adapterEvent: { kind: 'adapter.podstate.observed', data: { fields: { phase: { stringValue: 'Pending' }, scope_instance_id: { stringValue: 's' }, observed_at: { stringValue: 't3' } } } } },
    { seq: '100', adapterEvent: { kind: 'adapter.podstate.observed', data: { fields: { phase: { stringValue: 'Running' }, scope_instance_id: { stringValue: 's' }, observed_at: { stringValue: 't100' } } } } },
    { seq: '100', adapterEvent: { kind: 'adapter.podstate.observed', data: { fields: { phase: { stringValue: 'Running' }, scope_instance_id: { stringValue: 's' }, observed_at: { stringValue: 't100' } } } } },
    { seq: '2', adapterEvent: { kind: 'adapter.podstate.observed', data: { fields: { phase: { stringValue: 'Pending' }, scope_instance_id: { stringValue: 's' }, observed_at: { stringValue: 't2' } } } } },
  ];
  const seqs = podstateSeqs({ events });
  eq('dedupe+sort', seqs.map((e) => e.seq), [2, 3, 100]);
}

// --- disposition verdict: LEASED on the f18ceef3 agent lane ---
{
  const d = dispositionVerdict({
    runId: '7d8cd36e',
    state: 'WORKFLOW_ASSIGNMENT_STATE_LEASED',
    leasedCriteriaId: 'f18ceef3-abcd',
    updatedAt: { seconds: '1793045493' },
  }, 'f18ceef3');
  eq('disposition leased', d.leased, true);
  eq('disposition criteria', d.leasedCriteriaId, 'f18ceef3-abcd');
  eq('disposition prefix match', d.expectedCriteriaMatch, true);
  const snake = dispositionVerdict({ run_id: 'r1', state: 'LEASED', leased_criteria_id: 'f18ceef3', rejection_reason: '' }, 'nope');
  eq('snake prefix mismatch', snake.expectedCriteriaMatch, false);
  const rejected = dispositionVerdict({ state: 'REJECTED', rejectionReason: 'no eligible agent' });
  eq('rejected reason', rejected.rejectionReason, 'no eligible agent');
}

// --- run row: project field absence is an observation, not a failure ---
{
  const r = runRowVerdict({ runId: 'r1', workflowName: 'kanboard_develop_v1', status: 'running', criteriaId: 'peer' });
  eq('run row workflow', r.workflowName, 'kanboard_develop_v1');
  eq('run row project exposed', r.projectFieldExposed, false);
  const withProject = runRowVerdict({ run_id: 'r2', projectId: 'kanboard-tickets' });
  eq('run row project value', withProject.projectId, 'kanboard-tickets');
}

// --- refusal classification (KB-326 S3 isolation observations) ---
eq('anon 401', refusalObservation('P', { code: 'unauthenticated' }, { tokenless: true }).observed, 'refused-401');
eq('class gate', refusalObservation('P', { code: 'permission_denied', message: 'trigger identities cannot invoke this procedure' }).observed, 'refused-class-gate');
eq('uniform not_found', refusalObservation('P', { code: 'not_found', message: 'run not found' }, { tokenless: true }).observed, 'refused-uniform-not-found');

// --- evidence bundle carries the card's close-out fields ---
{
  const body = assembleEvidence({
    runId: '7d8cd36e-60d0-47aa-a0c7-57fed50b6dfd',
    crName: 'kb-342-1793045450',
    disposition: dispositionVerdict({ runId: '7d8cd36e', state: 'WORKFLOW_ASSIGNMENT_STATE_LEASED', leasedCriteriaId: 'f18ceef3-abcd' }, 'f18ceef3'),
    runRow: runRowVerdict({ run_id: '7d8cd36e', workflow_name: 'kanboard_develop_v1', status: 'running' }),
    podstate: podstateSeqs([connectShape, stored]),
    isolation: [refusalObservation('GetRun', { code: 'not_found' }, { tokenless: true })],
    fetchedAt: '2026-10-26T20:30:00Z',
  });
  for (const wants of [
    '- run: `7d8cd36e-60d0-47aa-a0c7-57fed50b6dfd`',
    '- CR name: `kb-342-1793045450`',
    'state=`WORKFLOW_ASSIGNMENT_STATE_LEASED`',
    'leased_criteria_id=`f18ceef3-abcd`',
    '(matches expected criteria lane)',
    'seqs: 12, 17',
    'refused-uniform-not-found',
    'kubectl -n criteria-jobs logs deploy/kanboard-watcher',
  ]) {
    if (!body.includes(wants)) fails.push(`evidence bundle missing: ${wants}`);
  }
  // Seat-gap rendering when a leg cannot be verified.
  const gaps = assembleEvidence({ crName: 'kb-342-1793045450', fetchedAt: '2026-10-26T20:30:00Z' });
  if (!gaps.includes('NOT VERIFIABLE FROM THIS CREDENTIAL SEAT')) fails.push('gap rendering missing for missing legs');
}

if (fails.length > 0) {
  console.error(fails.join('\n'));
  process.exit(1);
}
console.log('castle_verify regression fixtures: ok');
EOF
)"

[ "$(echo "$RESULTS" | grep -c 'castle_verify regression fixtures: ok')" -eq 1 ] || fail "harness logic: $RESULTS"
pass "castle_verify pure logic fixtures"

# The harness refuses to mutate castle: no mutating procedure may appear as a
# call site (SubmitWorkflowAssignment / SendPrompt submit-side misuse).
if grep -nE 'SubmitWorkflowAssignment|SendPrompt|StopRun|PauseRun|ResumeRun' "$HARNESS" | grep -vE '\*|//'; then
  fail "harness references a mutating procedure"
fi
pass "harness is read-only (no mutating call sites)"

# The harness carries no credential material and no inline castle addresses
# beyond the documented example flags.
if grep -qE "Bearer [A-Za-z0-9]{20,}|sk-[A-Za-z0-9]{20,}" "$HARNESS"; then
  fail "harness appears to embed a credential literal"
fi
pass "harness secret-free"

echo "test_castle_verify: all legs green"