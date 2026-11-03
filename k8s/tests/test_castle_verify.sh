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
#   - collectPodstate paging: the since cursor advances on events' seqs and
#     next_since_seq, a short page drains, the page budget truncates with an
#     explicit marker, and a refusal is carried (never thrown);
#   - disposition verdict mapping (state, leased_criteria_id — the f18ceef3…
#     agent lane —, expected-prefix match, rejection reason);
#   - run-row verdict (workflow/status/criteria; --expected-workflow
#     prefix compare; absent project field is a documented observation, not
#     a failure);
#   - refusal/isolation classification (KB-326 S3): anonymous refusals
#     classify, AND an anonymous 200 is recorded as the explicit
#     ANONYMOUS-READ-ALLOWED violation finding (never skipped);
#   - leg 0 health prologue and leg E operator CR-view probe classes — a
#     probed 200 verdict (Running/present/orphan) versus probe failures
#     (http_500, transport, undecodable body) that render "NOT PROBED" and
#     NEVER the KB-234 orphan claim;
#   - the exported runVerify orchestration seam (fetch injection) — all legs
#     together over scripted castle answers;
#   - CLI-level behavior: exit 0 happy path over a local fixture server,
#     castle/operator base resolution from env, --expected-workflow
#     match/mismatch lines, exit 2 usage, exit 3 token-file failure;
#   - the evidence bundle renders the card's close-out fields: assignment
#     disposition, CR name, podstate seqs — and the log-only parity leg,
#     with truncation markers and gap rendering.

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
const harnessUrl = process.argv[2];
const { parsePodstateEvent, podstateSeqs, collectPodstate, dispositionVerdict, runRowVerdict, refusalObservation, healthObservation, operatorViewVerdict, castleBaseFromEnv, operatorBaseFromEnv, assembleEvidence, runVerify } = await import(harnessUrl);

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
{
  // seq '100' vs '99': string compare would order 100 < 99; numeric wins.
  const events = [
    { seq: '100', adapterEvent: { kind: 'adapter.podstate.observed', data: { fields: { phase: { stringValue: 'Running' }, observed_at: { stringValue: 't100' } } } } },
    { seq: '99', adapterEvent: { kind: 'adapter.podstate.observed', data: { fields: { phase: { stringValue: 'Running' }, observed_at: { stringValue: 't99' } } } } },
  ];
  eq('numeric seq order', podstateSeqs({ events }).map((e) => e.seq), [99, 100]);
}

// --- collectPodstate paging (castle.go:468-516 semantics) ---
{
  const pod = (seq, phase) => ({ seq: String(seq), adapterEvent: { kind: 'adapter.podstate.observed', data: { fields: { phase: { stringValue: phase }, scope_instance_id: { stringValue: 's' }, observed_at: { stringValue: `t${seq}` } } } } });
  const other = { seq: '9', adapterEvent: { kind: 'adapter.lifecycle.provision_wanted', data: {} } };

  // Two pages: page 1 is full for its small limit and carries
  // next_since_seq; the cursor advances past the events' own max seq when
  // the continuation is greater, and a short second page drains the stream.
  const calls = [];
  const fetchImpl = (url, opts) => {
    const sinceSeq = JSON.parse(opts.body).sinceSeq;
    calls.push(sinceSeq);
    if (sinceSeq === 0) {
      return Promise.resolve({ ok: true, status: 200, json: async () => ({ events: [pod(3, 'Pending'), other, pod(9, 'Running')], nextSinceSeq: '15' }) });
    }
    return Promise.resolve({ ok: true, status: 200, json: async () => ({ events: [pod(20, 'Succeeded')] }) });
  };
  const r = await collectPodstate('http://castle.test', 'r1', 'tok', fetchImpl, 20, 3);
  eq('paging calls', calls, [0, 15]);
  eq('paging seqs', r.podstate.map((e) => e.seq), [3, 9, 20]);
  eq('paging not truncated', r.truncated, false);
  eq('paging cursor', r.nextSinceSeq, 20);
  eq('paging pages', r.pages, 2);

  // Truncation: every page is full and always advances — the page budget
  // bounds the loop and the result carries truncation + next unread seq.
  const cycFetch = (url, opts) => {
    const sinceSeq = JSON.parse(opts.body).sinceSeq;
    return Promise.resolve({ ok: true, status: 200, json: async () => ({ events: [pod(sinceSeq + 1), pod(sinceSeq + 2)], nextSinceSeq: String(sinceSeq + 3) }) });
  };
  const trunc = await collectPodstate('http://castle.test', 'r2', 'tok', cycFetch, 3, 2);
  eq('truncation detected', trunc.truncated, true);
  eq('truncation pages', trunc.pages, 3);
  eq('truncation cursor', trunc.nextSinceSeq, 9);
  const marker = assembleEvidence({ runId: 'r2', podstate: trunc.podstate, podstateTruncated: trunc.truncated, podstateNextSinceSeq: trunc.nextSinceSeq, fetchedAt: '2026-10-26T20:30:00Z' });
  if (!marker.includes('TRUNCATED at the 20-page budget')) fails.push('truncation marker missing');
  if (!marker.includes('next_unread_seq=9')) fails.push('truncation next_unread_seq missing');
  if (!marker.includes('quote this seq list as incomplete')) fails.push('truncation incompleteness note missing');

  // Refusal mid-paging: carried as an observation, never thrown.
  const refusing = () => Promise.resolve({ ok: false, status: 401, json: async () => ({ code: 'unauthenticated', message: 'no auth' }) });
  const ref = await collectPodstate('http://castle.test', 'r3', 'tok', refusing, 20, 2);
  eq('paging refusal carried', ref.refusal, { code: 'unauthenticated', message: 'no auth' });
  eq('paging refusal no events', ref.podstate, []);
  eq('paging refusal not truncated', ref.truncated, false);
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
  eq('disposition ts flattened', dispositionVerdict({ state: 'LEASED', updatedAt: { seconds: '1793045493' } }).updatedAt, '2026-10-26T20:11:33Z (unix 1793045493)');
}

// --- run row: project field absence is an observation, not a failure ---
{
  const r = runRowVerdict({ runId: 'r1', workflowName: 'kanboard_develop_v1', status: 'running', criteriaId: 'peer' });
  eq('run row workflow', r.workflowName, 'kanboard_develop_v1');
  eq('run row project exposed', r.projectFieldExposed, false);
  const withProject = runRowVerdict({ run_id: 'r2', projectId: 'kanboard-tickets' });
  eq('run row project value', withProject.projectId, 'kanboard-tickets');
  // --expected-workflow (card item 2: the run's workflow must be this repo's
  // kanboard-develop tree): prefix compare, mismatch carried explicitly.
  eq('workflow match', runRowVerdict({ workflowName: 'kanboard_develop_v1' }, 'kanboard_develop').workflowMatch, true);
  eq('workflow mismatch flagged', runRowVerdict({ workflowName: 'other_tree' }, 'kanboard_develop').workflowMatch, false);
  eq('workflow mismatch detail', runRowVerdict({ workflowName: 'other_tree' }, 'kanboard_develop').expectedWorkflow, 'kanboard_develop');
}

// --- refusal classification (KB-326 S3 isolation observations) ---
eq('anon 401', refusalObservation('P', { code: 'unauthenticated' }, { tokenless: true }).observed, 'refused-401');
eq('class gate', refusalObservation('P', { code: 'permission_denied', message: 'trigger identities cannot invoke this procedure' }).observed, 'refused-class-gate');
eq('uniform not_found', refusalObservation('P', { code: 'not_found', message: 'run not found' }, { tokenless: true }).observed, 'refused-uniform-not-found');
eq('anon transport', refusalObservation('P', { code: 'unavailable', message: 'transport: ECONNREFUSED' }, { tokenless: true }).observed, 'unreachable');
// An anonymous 200 is a recorded finding, never skipped: the KB-326 S3 hold
// says scoped runs are never anonymously readable — data means violation.
{
  const allowed = refusalObservation('GetRun', { runId: 'r1', status: 'RUNNING' }, { tokenless: true });
  eq('anon success recorded', allowed.observed, 'ANONYMOUS-READ-ALLOWED');
  if (!allowed.detail.includes('KB-326 S3')) fails.push('anon success must name the KB-326 S3 violation');
  const served = refusalObservation('GetRun', { runId: 'r1', status: 'RUNNING' }, {});
  eq('tokened success recorded', served.observed, 'served');
  eq('tokened success credential', served.credential, 'presented-token');
}

// --- leg 0: health prologue classification ---
eq('health serving', healthObservation({ status: 'SERVING' }).observed, 'SERVING');
eq('health enum shape', healthObservation({ status: 'SERVING_STATUS_SERVING' }).observed, 'SERVING_STATUS_SERVING');
eq('health unreachable', healthObservation({ code: 'unavailable', message: 'transport: connect ECONNREFUSED' }).observed, 'unreachable');
eq('health refusal shape', healthObservation({ code: 'http_404', message: 'HTTP 404' }).observed, 'unexpected-http_404');

// --- leg E: operator CR view probe classes (KB-234) ---
// A probed 200 body {"phase": "..."} may claim Running/present/orphan.
{
  const running = operatorViewVerdict({ status: 200, body: { phase: 'Running', terminal: false } }, 'kb-342-1793068430');
  eq('operator probe class', running.probe, 'probed');
  eq('operator running', running.phase, 'Running');
  eq('operator executing', running.running, true);
  eq('operator present', running.crPresent, true);
  eq('operator job echoed', running.job, 'kb-342-1793068430');
  const succeeded = operatorViewVerdict({ status: 200, body: { phase: 'Succeeded', terminal: true } }, 'kb-342-1793045450');
  eq('succeeded not running', succeeded.running, false);
  eq('succeeded present', succeeded.crPresent, true);
  const orphan = operatorViewVerdict({ status: 200, body: { phase: 'NotFound', terminal: true } }, 'kb-342-1793045450');
  eq('orphan probe class', orphan.probe, 'probed');
  eq('orphan absent', orphan.crPresent, false);
  // A failed or undecodable probe means NOTHING was observed: it must NOT
  // carry the orphan (absent) claim.
  const http500 = operatorViewVerdict({ status: 500, body: null }, 'kb-342-1793068430');
  eq('http500 probe class', http500.probe, 'probe-failed');
  eq('http500 detail', http500.detail, 'http_500');
  eq('http500 not probed', http500.crPresent, false);
  eq('http500 no phase claim', http500.phase, '');
  const http400 = operatorViewVerdict({ status: 400, body: null }, 'kb-342-1793068430');
  eq('http400 probe class', http400.probe, 'probe-failed');
  const transport = operatorViewVerdict({ transportError: 'ECONNREFUSED' }, 'kb-342-1793068430');
  eq('transport probe class', transport.probe, 'probe-failed');
  eq('transport detail', transport.detail, 'transport: ECONNREFUSED');
  const undecodable = operatorViewVerdict({ status: 200, body: 'bootgate view: ticket or job required' }, 'j');
  eq('undecodable probe class', undecodable.probe, 'probe-unparseable');
  const noPhaseBody = operatorViewVerdict({ status: 200, body: {} }, 'j');
  eq('phase-less 200 unparseable', noPhaseBody.probe, 'probe-unparseable');
  eq('null probe failed', operatorViewVerdict(null, 'j').probe, 'probe-failed');
}

// --- in-cluster env defaults for the castle/operator seats ---
eq('castle env default', castleBaseFromEnv({ CASTLE_SERVICE_HOST: 'castle.criteria-jobs.svc.cluster.local', CASTLE_SERVICE_PORT_H2C: '8080' }), 'http://castle.criteria-jobs.svc.cluster.local:8080');
eq('castle env empty', castleBaseFromEnv({}), '');
eq('operator env default', operatorBaseFromEnv({ CRITERIA_K8S_OPERATOR_SERVICE_HOST: '10.43.156.32', CRITERIA_K8S_OPERATOR_PORT_8082_TCP_PORT: '8082' }), 'http://10.43.156.32:8082');
eq('operator env empty', operatorBaseFromEnv({}), '');

// --- runVerify orchestration seam (fetch injection) ---
{
  const podEnvelope = (seq, phase) => ({ seq: String(seq), adapterEvent: { kind: 'adapter.podstate.observed', data: { fields: { phase: { stringValue: phase }, scope_instance_id: { stringValue: 's' }, observed_at: { stringValue: `t${seq}` } } } } });
  const fixtures = (code) => ({
    'grpc.health.v1.Health/Check': { status: 'SERVING' },
    'GetRun': code ? { code, message: 'denied' } : { runId: 'r1', workflowName: 'kanboard_develop_v1', status: 'RUNNING' },
    'ListRunEvents': { events: [podEnvelope(5, 'Running')], nextSinceSeq: '5' },
    'GetAssignmentDisposition': { runId: 'r1', state: 'LEASED', leasedCriteriaId: 'f18ceef3-9a' },
    'ListRuns': {},
    'ListAgents': { agents: [] },
  });
  const fakeFetch = (code, operatorAnswer) => (url, opts) => {
    if (url.includes('/operator-view')) return operatorAnswer();
    const body = (() => { for (const key of Object.keys(fixtures(code))) { if (url.includes(`/${key}`)) return fixtures(code)[key]; } return {}; })();
    return Promise.resolve({ ok: !code, status: code ? 401 : 200, json: async () => body });
  };

  // Happy path: every leg served. The anonymous pass therefore records the
  // KB-326 S3 violation (data without a credential) — never skipped.
  const legs = await runVerify({
    castle: 'http://castle.test', runId: 'r1', crName: 'cr1',
    expectedCriteria: 'f18ceef3', expectedWorkflow: 'kanboard_develop',
    operator: 'http://operator.test', token: 'tok',
    fetchImpl: fakeFetch(null, () => Promise.resolve({ ok: true, status: 200, json: async () => ({ phase: 'Running', terminal: false }) })),
  });
  eq('orchestration health', legs.health.observed, 'SERVING');
  eq('orchestration disposition', legs.disposition.state, 'LEASED');
  eq('orchestration criteria match', legs.disposition.expectedCriteriaMatch, true);
  eq('orchestration run row', legs.runRow.workflowName, 'kanboard_develop_v1');
  eq('orchestration workflow match', legs.runRow.workflowMatch, true);
  eq('orchestration podstate', legs.podstate.map((e) => e.seq), [5]);
  eq('orchestration operator probed', legs.operatorView.probe, 'probed');
  eq('orchestration operator running', legs.operatorView.running, true);
  eq('orchestration anon all recorded', legs.isolation.filter((o) => o.observed === 'ANONYMOUS-READ-ALLOWED').length, 4);
  eq('orchestration anon surfaces probed', legs.isolation.length, 4);
  eq('orchestration no truncation', legs.podstateTruncated, false);
  const happy = assembleEvidence({ ...legs, fetchedAt: '2026-10-26T20:30:00Z' });
  if (!happy.includes('ANONYMOUS-READ-ALLOWED')) fails.push('orchestration evidence misses anon-success record');
  if (!happy.includes('- isolation observations (KB-326 S3): VIOLATION')) fails.push('orchestration evidence misses violation headline');
  if (!happy.includes('- assignment keyed by run_id `r1`')) fails.push('orchestration evidence misses run-keyed assignment note');
  if (happy.includes('TRUNCATED')) fails.push('orchestration evidence must not claim truncation');
  if (!happy.includes('workflow vs --expected-workflow: matches')) fails.push('orchestration evidence misses workflow match line');

  // Failure posture: castle refuses (401) and the operator probe fails —
  // the evidence must render NOT PROBED, never the orphan (absent) claim.
  const refused = await runVerify({
    castle: 'http://castle.test', runId: 'r1', crName: 'cr1',
    operator: 'http://operator.test', token: 'tok',
    fetchImpl: fakeFetch('unauthenticated', () => Promise.resolve({ ok: false, status: 500, json: async () => null })),
  });
  eq('refused legs null', refused.runRow, null);
  eq('refused disposition null', refused.disposition, null);
  eq('refused podstate null', refused.podstate, null);
  eq('refused operator probe class', refused.operatorView.probe, 'probe-failed');
  const bad = assembleEvidence({ ...refused, fetchedAt: '2026-10-26T20:30:00Z' });
  if (!bad.includes('NOT PROBED — probe=http_500')) fails.push('failed operator probe must render NOT PROBED with probe status');
  if (bad.includes('KB-234 orphan shape')) fails.push('failed probe must NOT carry the orphan (absent) claim');
  if (bad.includes('ABSENT')) fails.push('failed probe must not render any ABSENT claim');
  if (!bad.includes('refused-401')) fails.push('refusal posture must classify refused-401');
  if (!bad.includes('NOT VERIFIABLE FROM THIS CREDENTIAL SEAT')) fails.push('refused legs render as seat gaps');

  // Transport failure on the operator probe (everything unreachable).
  const dead = await runVerify({
    castle: 'http://castle.test', runId: 'r1', crName: 'cr1',
    operator: 'http://operator.test',
    fetchImpl: () => Promise.reject(Object.assign(new Error('boom'), { cause: { code: 'ECONNREFUSED' } })),
  });
  eq('dead health unreachable', dead.health.observed, 'unreachable');
  eq('dead operator transport detail', dead.operatorView.detail, 'transport: ECONNREFUSED');
  const deadEvidence = assembleEvidence({ ...dead, fetchedAt: '2026-10-26T20:30:00Z' });
  if (!deadEvidence.includes('NOT PROBED — probe=transport: ECONNREFUSED')) fails.push('transport probe failure must render its transport detail');
}

// --- evidence bundle carries the card's close-out fields ---
{
  const body = assembleEvidence({
    runId: '7d8cd36e-60d0-47aa-a0c7-57fed50b6dfd',
    crName: 'kb-342-1793045450',
    disposition: dispositionVerdict({ runId: '7d8cd36e', state: 'WORKFLOW_ASSIGNMENT_STATE_LEASED', leasedCriteriaId: 'f18ceef3-abcd' }, 'f18ceef3'),
    runRow: runRowVerdict({ run_id: '7d8cd36e', workflow_name: 'kanboard_develop_v1', status: 'running' }),
    podstate: podstateSeqs([connectShape, stored]),
    health: healthObservation({ status: 'SERVING' }),
    operatorView: operatorViewVerdict({ status: 200, body: { phase: 'Running', terminal: false } }, 'kb-342-1793068430'),
    isolation: [refusalObservation('GetRun', { code: 'not_found' }, { tokenless: true })],
    fetchedAt: '2026-10-26T20:30:00Z',
  });
  for (const wants of [
    '- run: `7d8cd36e-60d0-47aa-a0c7-57fed50b6dfd`',
    '- CR name: `kb-342-1793045450`',
    '- castle health: SERVING',
    'phase=Running terminal=false',
    'operator-stamped CriteriaRun executing',
    'state=`WORKFLOW_ASSIGNMENT_STATE_LEASED`',
    'leased_criteria_id=`f18ceef3-abcd`',
    '(matches expected criteria lane)',
    'assignment keyed by run_id `7d8cd36e`',
    'seqs: 12, 17',
    'refused-uniform-not-found',
    'kubectl -n criteria-jobs logs deploy/kanboard-watcher',
  ]) {
    if (!body.includes(wants)) fails.push(`evidence bundle missing: ${wants}`);
  }
  if (body.includes('TRUNCATED')) fails.push('truncation marker must not render without truncation');
  const orphanBundle = assembleEvidence({
    runId: 'r', crName: 'kb-342-1793045450',
    operatorView: operatorViewVerdict({ status: 200, body: { phase: 'NotFound', terminal: true } }, 'kb-342-1793045450'),
    fetchedAt: '2026-10-26T20:30:00Z',
  });
  if (!orphanBundle.includes('ABSENT (phase=NotFound')) fails.push('orphan CR rendering missing (genuine probed NotFound)');
  // A failed probe NEVER renders the orphan claim.
  const failedProbeBundle = assembleEvidence({
    runId: 'r', crName: 'kb-342-1793045450',
    operatorView: operatorViewVerdict({ status: 500, body: null }, 'kb-342-1793045450'),
    fetchedAt: '2026-10-26T20:30:00Z',
  });
  if (!failedProbeBundle.includes('NOT PROBED — probe=http_500')) fails.push('failed probe rendering missing');
  if (failedProbeBundle.includes('ABSENT') || failedProbeBundle.includes('KB-234 orphan shape')) fails.push('failed probe must not render the orphan claim');
  // Seat-gap rendering when a leg cannot be verified.
  const gaps = assembleEvidence({ crName: 'kb-342-1793045450', fetchedAt: '2026-10-26T20:30:00Z' });
  if (!gaps.includes('NOT VERIFIABLE FROM THIS CREDENTIAL SEAT')) fails.push('gap rendering missing for missing legs');
  if (!gaps.includes('CR (operator view): NOT PROBED')) fails.push('operator-view gap rendering missing');
  if (!gaps.includes('- isolation observations: none recorded')) fails.push('empty isolation section must render');
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

# --- CLI-level behavior: exit codes, env base resolution, orchestration ---
# A local fixture HTTP server stands in for castle + operator so the CLI
# child exercises the real fetch path end-to-end.
CLI_RESULTS="$(node --input-type=module - "$HARNESS" <<'EOF'
import { spawnSync, spawn } from 'node:child_process';
import { readFileSync, writeFileSync, mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const harness = process.argv[2];
const fails = [];

// Fixture run stream: two podstate events (seqs 1, 4) — a single short page
// (the default leg limit is 500) — plus a non-podstate event.
const events = [
  { seq: '1', adapterEvent: { kind: 'adapter.podstate.observed', data: { fields: { phase: { stringValue: 'Running' }, scope_instance_id: { stringValue: 's' }, observed_at: { stringValue: 't1' } } } } },
  { seq: '2', adapterEvent: { kind: 'adapter.lifecycle.provision_wanted', data: { fields: {} } } },
  { seq: '4', adapterEvent: { kind: 'adapter.podstate.observed', data: { fields: { phase: { stringValue: 'Succeeded' }, scope_instance_id: { stringValue: 's' }, observed_at: { stringValue: 't4' } } } } },
];
const tmp = mkdtempSync(join(tmpdir(), 'castle-verify-cli-'));
// The fixture server MUST live in a separate process: spawnSync blocks the
// parent's event loop, and a same-process listener would leave the child's
// fetches unanswered in the TCP backlog (every leg aborts at the timeout).
const serverScript = join(tmp, 'server.mjs');
writeFileSync(serverScript, `\
import http from 'node:http';
import { writeFileSync } from 'node:fs';
const events = ${JSON.stringify(events)};
const seen = [];
const server = http.createServer((req, res) => {
  seen.push(\`\${req.method} \${req.url}\`);
  writeFileSync(${JSON.stringify(join(tmp, 'seen.json'))}, JSON.stringify(seen));
  if (req.method === 'GET' && req.url.startsWith('/operator-view')) {
    res.writeHead(200, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify({ phase: 'Running', terminal: false }));
    return;
  }
  res.writeHead(200, { 'Content-Type': 'application/json' });
  const path = req.url.slice(1);
  const body = path.includes('Health/') ? { status: 'SERVING' }
    : path.includes('GetRun') ? { runId: 't-run', workflowName: 'kanboard_develop_v1', status: 'RUNNING', criteriaId: 'f18ceef3' }
    : path.includes('ListRunEvents') ? { events, nextSinceSeq: '4' }
    : path.includes('GetAssignmentDisposition') ? { runId: 't-run', state: 'LEASED', leasedCriteriaId: 'f18ceef3-9a' }
    : path.includes('ListRun') ? {}
    : path.includes('ListAgents') ? { agents: [] }
    : {};
  res.end(JSON.stringify(body));
});
process.on('SIGTERM', () => process.exit(0));
server.listen(0, '127.0.0.1', () => writeFileSync(${JSON.stringify(join(tmp, 'port'))}, String(server.address().port)));
`);
const serverProc = spawn(process.execPath, [serverScript], {
  detached: true, stdio: 'ignore', env: { PATH: process.env.PATH },
});
let port = '';
for (let i = 0; i < 50 && !port; i++) {
  await new Promise((ok) => setTimeout(ok, 100));
  try { port = readFileSync(join(tmp, 'port'), 'utf8').trim(); } catch { /* not up yet */ }
}
const castle = `http://127.0.0.1:${port}`;

try {
  const spawnArgs = (extraArgs, extraEnv = {}) => spawnSync(process.execPath, [harness, ...extraArgs], {
    encoding: 'utf8',
    env: { PATH: process.env.PATH, ...extraEnv },
  });

  // Happy path (flags): exit 0, evidence over the wire, out file rendered.
  const out1 = join(tmp, 'evidence.json');
  const happy = spawnArgs([
    '--castle', castle, '--run-id', 't-run', '--cr-name', 't-cr',
    '--operator', castle, '--expected-criteria', 'f18ceef3',
    '--expected-workflow', 'kanboard_develop', '--out', out1, '--pretty',
  ]);
  if (happy.status !== 0) fails.push(`CLI happy path exit ${happy.status}: ${happy.stderr}`);
  if (!happy.stdout.includes('seqs: 1, 4')) fails.push(`CLI happy path misses podstate seqs: ${happy.stdout}`);
  if (!happy.stdout.includes('operator-stamped CriteriaRun executing')) fails.push('CLI happy path misses operator leg');
  if (!happy.stdout.includes('matches expected criteria lane')) fails.push('CLI happy path misses criteria match');
  if (!happy.stdout.includes('workflow vs --expected-workflow: matches')) fails.push('CLI happy path misses workflow match');
  // The fixture server serves anonymously too: the run is unauthenticated
  // readable — the bundle must record the KB-326 S3 violation.
  if (!happy.stdout.includes('ANONYMOUS-READ-ALLOWED')) fails.push('CLI happy path misses anon-success record');
  if (!happy.stdout.includes('isolation observations (KB-326 S3): VIOLATION')) fails.push('CLI happy path misses violation headline');
  if (happy.stdout.includes('TRUNCATED')) fails.push('CLI happy path must not claim truncation');
  const doc = JSON.parse(readFileSync(out1, 'utf8'));
  if (doc.podstate.length !== 2) fails.push(`CLI out.json podstate count ${doc.podstate.length}`);
  if (doc.operatorView?.probe !== 'probed') fails.push('CLI out.json operator probe class');
  const seen = JSON.parse(readFileSync(join(tmp, 'seen.json'), 'utf8'));
  const anonProcs = new Set(doc.isolation.map((o) => o.procedure.split('/').pop()));
  for (const p of ['GetRun', 'ListRunEvents', 'ListRuns', 'ListAgents']) {
    if (!anonProcs.has(p)) fails.push(`CLI anon pass missing ${p}`);
  }
  for (const p of ['GetRun', 'ListRunEvents', 'ListRuns', 'ListAgents']) {
    if (!seen.some((req) => req.includes(p))) fails.push(`fixture server never saw ${p}`);
  }

  // Env fallback: no --castle/--operator flags — the bases resolve from the
  // service env (main()'s castleBaseFromEnv/operatorBaseFromEnv seam).
  const envHappy = spawnArgs(['--run-id', 't-run', '--cr-name', 't-cr', '--out', join(tmp, 'e2.json')], {
    CASTLE_SERVICE_HOST: '127.0.0.1',
    CASTLE_SERVICE_PORT_H2C: String(port),
    CRITERIA_K8S_OPERATOR_SERVICE_HOST: '127.0.0.1',
    CRITERIA_K8S_OPERATOR_PORT_8082_TCP_PORT: String(port),
  });
  if (envHappy.status !== 0) fails.push(`CLI env fallback exit ${envHappy.status}: ${envHappy.stderr}`);
  if (!envHappy.stdout.includes('operator-stamped CriteriaRun executing')) fails.push(`CLI env fallback misses operator leg: ${envHappy.stderr}`);
  if (!envHappy.stdout.includes('seqs: 1, 4')) fails.push('CLI env fallback misses podstate seqs');

  // --expected-workflow mismatch renders the explicit MISMATCH line.
  const mismatch = spawnArgs([
    '--castle', castle, '--run-id', 't-run', '--cr-name', 't-cr',
    '--expected-workflow', 'other_tree',
  ]);
  if (mismatch.status !== 0) fails.push(`CLI mismatch exit ${mismatch.status}: ${mismatch.stderr}`);
  if (!mismatch.stdout.includes('MISMATCH — observed `kanboard_develop_v1` does not carry the expected `other_tree` prefix')) {
    fails.push(`CLI mismatch line: ${mismatch.stdout}`);
  }

  // Exit 2: usage error (no castle resolution, no run id).
  const noArgs = spawnArgs([]);
  if (noArgs.status !== 2) fails.push(`CLI no-args exit ${noArgs.status}, want 2`);
  if (!noArgs.stderr.includes('usage:')) fails.push('CLI no-args misses usage');
  const noRun = spawnArgs(['--castle', castle]);
  if (noRun.status !== 2) fails.push(`CLI no-run-id exit ${noRun.status}, want 2`);

  // Exit 3: unreadable token file (no network touched).
  const badToken = spawnArgs(['--castle', 'http://127.0.0.1:1', '--run-id', 'x', '--token-file', join(tmp, 'nope/token')]);
  if (badToken.status !== 3) fails.push(`CLI bad token-file exit ${badToken.status}, want 3`);
  if (!badToken.stderr.includes('token file unreadable')) fails.push('CLI bad token-file misses stderr note');

  console.log('castle_verify CLI orchestration: ok');
} finally {
  rmSync(tmp, { recursive: true, force: true });
  serverProc?.kill('SIGTERM');
}

if (fails.length > 0) {
  console.error(fails.join('\n'));
  process.exit(1);
}
EOF
)"

[ "$(echo "$CLI_RESULTS" | grep -c 'castle_verify CLI orchestration: ok')" -eq 1 ] || fail "CLI orchestration: $CLI_RESULTS"
pass "castle_verify CLI orchestration (exit codes, env resolution, paged evidence)"

# The harness refuses to mutate castle: no mutating procedure may appear as a
# call site (SubmitWorkflowAssignment / SendPrompt / pause-stop misuse).
# Comments are stripped first so doc references do not mask the search; only
# real code lines remain, and none may name a mutating procedure.
MUTATING_CALLS="$(sed 's|//.*$||' "$HARNESS" | grep -nE 'SubmitWorkflowAssignment|SendPrompt|StopRun|PauseRun|ResumeRun' || true)"
if [ -n "$MUTATING_CALLS" ]; then
  fail "harness references a mutating procedure: $MUTATING_CALLS"
fi
pass "harness is read-only (no mutating call sites)"

# The harness carries no credential material (a Bearer literal would embed a
# credential) and no inline castle addresses beyond the documented example
# flags.
if grep -qE "Bearer [A-Za-z0-9_\-\.]{8,}" "$HARNESS"; then
  fail "harness appears to embed a credential literal"
fi
pass "harness secret-free"

echo "test_castle_verify: all legs green"