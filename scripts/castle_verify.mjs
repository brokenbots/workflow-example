#!/usr/bin/env node
// castle_verify.mjs — KB-342 castle verification harness (KB-337/338 live proof).
//
// Runs the four-leg verification card's checklist against a live castle and
// renders the evidence document the card's close-out comment names:
//
//   leg 0 castle health          — grpc.health.v1.Health/Check; SERVING is
//          the prologue, unreachable is an observation (legs still run).
//   leg A  assignment disposition  — GetAssignmentDisposition(run_id) →
//          state (LEASED/…), leased_criteria_id (f18ceef3… for KB-342),
//          updated_at. Owner-scoped: requires the credential that owns the
//          assignment (the dispatching watcher trigger's lane).
//   leg B  run row                 — GetRun(run_id); the runs.project_id
//          column exists castle-side (pg/sqlite migration 0019) but the
//          public criteria.v1 Run proto does not expose it, so a present
//          field is reported and its absence is an honest observation with
//          the console/SQL alternative, not a failure.
//   leg C  podstate events         — pages ListRunEvents and extracts every
//          adapter.podstate.observed adapter-event (criteria-k8s
//          internal/events PodStateObservedKind) with its castle-stamped seq.
//   leg D  isolation observations  — the anonymous + unprivileged passes over
//          the read surfaces, classifying each refusal. KB-326 S3 scoping is
//          a verification TARGET: uniform not_found (scoped runs are never
//          anonymously readable) and permission_denied class gates are
//          recorded as PASS observations, not errors.
//   leg E  operator CR view      — GET /operator-view?job=<cr-name> on the
//          criteria-k8s operator (KB-234 boot-gate operator view): the
//          card's "CR stamped by the operator" chain evidence. Optional;
//          enabled by --operator or CRITERIA_K8S_OPERATOR_* env.
//
// READ-ONLY: the harness never calls SubmitWorkflowAssignment or any other
// mutating procedure — verification must not mint runs. Watcher parity
// counters are log-only (criteria-k8s internal/controller recordParity), so
// leg E renders the exact operator command instead of a value.
//
// Usage:
//   node scripts/castle_verify.mjs \
//     --castle http://castle.criteria-jobs.svc.cluster.local:8080 \
//     --run-id 7d8cd36e-… --cr-name kb-342-1793045450 \
//     --expected-criteria f18ceef3 --token-file /path/to/lane-token \
//     --out evidence.json [--pretty]
//
// Exit codes: 0 checklist completed (verdicts may still be pending-eyes), 2
// usage error, 3 unexpected harness failure (e.g. an unreadable token file) —
// transport and refusal answers are observations, never crashes.

import { writeFileSync } from 'node:fs';

// --- constants ------------------------------------------------------------

export const PodStateObservedKind = 'adapter.podstate.observed';
const PodStatePhases = new Set(['Pending', 'Running', 'Succeeded', 'Failed', 'Unknown']);

const ServerProcedures = {
  health: 'grpc.health.v1.Health/Check',
  getRun: 'criteria.v1.ServerService/GetRun',
  listRunEvents: 'criteria.v1.ServerService/ListRunEvents',
  disposition: 'criteria.v1.ServerService/GetAssignmentDisposition',
};
const OperatorViewPath = '/operator-view';

// castleBaseFromEnv / operatorBaseFromEnv render the in-cluster service env
// defaults a seat carries (CASTLE_SERVICE_HOST / CRITERIA_K8S_OPERATOR_*),
// so the harness runs flag-free from a pod; empty when the env is absent.
export function castleBaseFromEnv(env = process.env) {
  if (env.CASTLE_SERVICE_HOST && env.CASTLE_SERVICE_PORT_H2C) {
    return `http://${env.CASTLE_SERVICE_HOST}:${env.CASTLE_SERVICE_PORT_H2C}`;
  }
  return '';
}
export function operatorBaseFromEnv(env = process.env) {
  if (env.CRITERIA_K8S_OPERATOR_SERVICE_HOST && env.CRITERIA_K8S_OPERATOR_PORT_8082_TCP_PORT) {
    return `http://${env.CRITERIA_K8S_OPERATOR_SERVICE_HOST}:${env.CRITERIA_K8S_OPERATOR_PORT_8082_TCP_PORT}`;
  }
  return '';
}

// --- network (thin, injectable for tests) ---------------------------------

// connectUnary performs one connect-protocol unary JSON call. Returns the
// parsed body for 2xx, else {code, message} error shape. Never throws on
// transport errors — verification records refusals, it does not die on them.
export async function connectUnary(base, procedure, body, token, fetchImpl = fetch) {
  const headers = { 'Content-Type': 'application/json', 'Connect-Protocol-Version': '1' };
  if (token) headers.Authorization = `Bearer ${token}`;
  let res;
  try {
    res = await fetchImpl(base.replace(/\/$/, '') + '/' + procedure, {
      method: 'POST', headers, body: JSON.stringify(body ?? {}),
    });
  } catch (err) {
    return { code: 'unavailable', message: `transport: ${err.message}` };
  }
  let json = null;
  try { json = await res.json(); } catch { /* non-JSON answer below */ }
  if (res.ok) return json ?? {};
  return {
    code: json?.code ?? `http_${res.status}`,
    message: json?.message ?? `HTTP ${res.status}`,
  };
}

// --- pure checks (exported for the regression suite) ----------------------

// normalizeSeq coerces protojson's uint64-as-string (and the stored line's
// number) into a comparable integer.
export function normalizeSeq(v) {
  if (typeof v === 'number') return v;
  if (typeof v === 'string' && /^\d+$/.test(v)) return Number(v);
  return 0;
}

// parsePodstateEvent scans one Envelope (connect camelCase shape or the
// stored ndjson snake_case line) for an adapter.podstate.observed
// adapter-event and renders the summary the evidence comment quotes.
export function parsePodstateEvent(env) {
  if (!env || typeof env !== 'object') return null;
  // The AdapterEvent oneof field: protojson camelCase ("adapterEvent") from
  // connect, or the stored line's payload_type/payload nesting.
  const adapterEvent = env.adapterEvent ?? env.adapter_event
    ?? (env.payloadType === 'AdapterEvent' || env.payload_type === 'AdapterEvent'
      ? env.payload : null);
  if (!adapterEvent || adapterEvent.kind !== PodStateObservedKind) return null;
  const data = adapterEvent.data ?? {};
  const flat = (k) => {
    // google.protobuf.Struct renders as {fields:{k:{stringValue:…}}} over
    // connect; the stored line carries flat scalars.
    const f = data.fields?.[k];
    if (f && typeof f === 'object') return f.stringValue ?? f.numberValue ?? f.boolValue ?? f.nullValue ?? '';
    return data[k] ?? '';
  };
  const out = {
    seq: normalizeSeq(env.seq ?? env.Seq),
    observedAt: String(flat('observed_at') || (typeof env.ts === 'string' ? env.ts : '') || ''),
    adapter: String(flat('adapter') ?? ''),
    adapterType: String(flat('adapter_type') ?? ''),
    scopeName: String(flat('scope_name') ?? ''),
    scopeInstanceId: String(flat('scope_instance_id') ?? ''),
    pod: String(flat('pod') ?? ''),
    phase: String(flat('phase') ?? ''),
    reason: String(flat('reason') ?? ''),
    message: String(flat('message') ?? ''),
  };
  if (!out.observedAt) return null;
  if (!PodStatePhases.has(out.phase)) out.note = `unrecognized phase ${out.phase}`;
  return out;
}

// podstateSeqs reduces a ListRunEventsResponse (or raw Envelope array) to the
// podstate summaries sorted by castle-stamped seq, de-duplicated on seq.
export function podstateSeqs(resp) {
  const events = Array.isArray(resp) ? resp : (resp?.events ?? []);
  if (!Array.isArray(events)) return [];
  const seen = new Map();
  for (const env of events) {
    const parsed = parsePodstateEvent(env);
    if (parsed) seen.set(parsed.seq, parsed);
  }
  return [...seen.values()].sort((a, b) => a.seq - b.seq);
}

// dispositionVerdict maps GetAssignmentDisposition's answer into the
// checklist verdict: state, the leasing agent's criteria id and whether it
// carries the expected prefix.
function flatTimestamp(v) {
  // protojson Timestamp arrives as an RFC3339 string (connect) or a
  // {seconds, nanos} JSON (legacy wire); callers want one comparable string.
  if (v && typeof v === 'object') {
    const s = Number(v.seconds ?? 0);
    return s > 0 ? `${new Date(s * 1000).toISOString().replace(/\.\d+Z$/, 'Z')} (unix ${s})` : '';
  }
  return String(v ?? '');
}
export function dispositionVerdict(resp, expectedPrefix = '') {
  const state = String(resp?.state ?? '');
  const leasedCriteriaId = String(resp?.leasedCriteriaId ?? resp?.leased_criteria_id ?? '');
  const runId = String(resp?.runId ?? resp?.run_id ?? '');
  const verdict = {
    runId,
    state: state || 'unknown',
    leasedCriteriaId,
    rejectionReason: String(resp?.rejectionReason ?? resp?.rejection_reason ?? ''),
    updatedAt: flatTimestamp(resp?.updatedAt ?? resp?.updated_at),
    leased: state === 'LEASED' || state === 'WORKFLOW_ASSIGNMENT_STATE_LEASED',
  };
  if (expectedPrefix) {
    verdict.expectedCriteriaMatch = leasedCriteriaId.startsWith(expectedPrefix);
  }
  return verdict;
}

// runRowVerdict reads GetRun's answer. The public criteria.v1 Run proto has
// no project field — the runs.project_id column (migration 0019) is visible
// on the console/SQL side — so a present project field is reported and its
// absence is a documented observation rather than a checklist failure.
export function runRowVerdict(run) {
  const projectId = String(run?.projectId ?? run?.project_id ?? '');
  return {
    runId: String(run?.runId ?? run?.run_id ?? ''),
    criteriaId: String(run?.criteriaId ?? run?.criteria_id ?? ''),
    workflowName: String(run?.workflowName ?? run?.workflow_name ?? ''),
    status: String(run?.status ?? ''),
    projectFieldExposed: projectId !== '',
    projectId,
  };
}

// operatorViewVerdict reads the KB-234 boot-gate operator-view document for
// the run's CR: {"phase": "...", "terminal": bool}, job-first identity. A
// "NotFound" phase (terminal) is the orphan class — the CR was deleted while
// its runner survived. This leg is the card's "CR stamped by the operator"
// chain evidence: a present, non-terminal phase proves the operator hosted
// the run's CR (watcher castle-mode never stamps CRs itself).
export function operatorViewVerdict(json, crName) {
  const phase = String(json?.phase ?? '');
  const terminal = json?.terminal === true;
  return {
    job: crName,
    phase: phase || 'unknown',
    terminal,
    crPresent: phase !== '' && phase !== 'NotFound',
    running: phase === 'Running' && !terminal,
  };
}

// healthObservation classifies the grpc health answer. An unreachable castle
// is an observation; the legs still run and land in their own refusals.
export function healthObservation(resp) {
  if (resp?.code === 'unavailable') {
    return { probe: 'health', observed: 'unreachable', detail: resp.message };
  }
  const status = String(resp?.status ?? '');
  if (status) {
    return { probe: 'health', observed: status, detail: '' };
  }
  return { probe: 'health', observed: `unexpected-${resp?.code ?? 'no-code'}`, detail: resp?.message ?? '' };
}

// refusalObservation classifies one denied call. KB-326 S3 scoping is the
// verification target: the anonymous pass MUST be refused for scoped runs
// (uniform not_found — existence is not leaked), unprivileged classes MUST be
// refused (permission_denied). Each shape is an observation, not a crash.
export function refusalObservation(procedure, resp, { tokenless = false } = {}) {
  const cls = tokenless ? 'anonymous' : 'presented-token';
  if (resp?.code === 'unavailable') {
    return { procedure, credential: cls, observed: 'unreachable', detail: resp.message };
  }
  if (resp?.code === 'unauthenticated' || (resp?.code === 'http_401')) {
    return { procedure, credential: cls, observed: 'refused-401', detail: resp.message ?? '' };
  }
  if (resp?.code === 'permission_denied' || resp?.code === 'http_403') {
    return { procedure, credential: cls, observed: 'refused-class-gate', detail: resp.message ?? '' };
  }
  if (resp?.code === 'not_found' || resp?.code === 'http_404') {
    // Uniform refusal: missing AND scoped runs are indistinguishable by
    // design — existence is not leaked (KB-326 S3).
    return { procedure, credential: cls, observed: 'refused-uniform-not-found', detail: resp.message ?? '' };
  }
  return { procedure, credential: cls, observed: `unexpected-${resp?.code ?? 'no-code'}`, detail: resp?.message ?? '' };
}

// assembleEvidence renders the card's close-out fields (assignment
// disposition, CR name, podstate seqs) into the comment body. Every leg is
// optional; missing legs render as explicit "observed from seat" gaps rather
// than being omitted.
export function assembleEvidence({ runId, crName, health, operatorView, disposition, runRow, podstate, isolation = [], fetchedAt }) {
  const lines = [];
  const stamp = fetchedAt ?? new Date().toISOString().replace(/\.\d+Z$/, 'Z');
  lines.push(`**castle verify (KB-342)** — evidence bundle, rendered ${stamp}`);
  lines.push('');
  lines.push(`- run: \`${runId ?? 'unknown'}\``);
  lines.push(`- CR name: \`${crName ?? 'unknown'}\``);
  if (health) {
    lines.push(`- castle health: ${health.observed}${health.observed === 'unreachable' ? ' — legs below may be transport refusals' : ''}`);
  }
  if (operatorView) {
    lines.push(operatorView.crPresent
      ? `- CR (operator view, job-first): phase=${operatorView.phase} terminal=${operatorView.terminal}`
        + (operatorView.running ? ' — operator-stamped CriteriaRun executing' : '')
      : `- CR (operator view, job-first): ABSENT (phase=${operatorView.phase}, terminal=${operatorView.terminal}) — KB-234 orphan shape`);
  } else if (crName) {
    lines.push('- CR (operator view): NOT PROBED — pass --operator (or run from a seat carrying CRITERIA_K8S_OPERATOR_* env) to check the operator-stamped CR');
  }
  if (disposition) {
    lines.push(`- assignment disposition: state=\`${disposition.state}\``
      + (disposition.leasedCriteriaId ? `, leased_criteria_id=\`${disposition.leasedCriteriaId}\`` : '')
      + (disposition.updatedAt ? `, updated_at=${disposition.updatedAt}` : '')
      + (disposition.expectedCriteriaMatch === false ? ' — WARNING: criteria id does not carry the expected prefix' : '')
      + (disposition.expectedCriteriaMatch === true ? ' (matches expected criteria lane)' : ''));
    if (disposition.rejectionReason) lines.push(`  - rejection_reason: ${disposition.rejectionReason}`);
  } else {
    lines.push('- assignment disposition: NOT VERIFIABLE FROM THIS CREDENTIAL SEAT (owner-scoped RPC; console or the dispatching watcher lane reads it)');
  }
  if (runRow) {
    lines.push(`- run row: workflow=\`${runRow.workflowName}\` status=\`${runRow.status}\` criteria=\`${runRow.criteriaId}\``);
    lines.push(`  - runs.project_id: ${runRow.projectFieldExposed ? `\`${runRow.projectId}\`` : 'not exposed by the public criteria.v1 Run proto — confirm `runs.project_id` via console/SQL (castle migration 0019 stamps it)'}`);
  }
  if (podstate) {
    if (podstate.length > 0) {
      const seqs = podstate.map((p) => p.seq).join(', ');
      lines.push(`- podstate events: ${podstate.length} × \`adapter.podstate.observed\`, seqs: ${seqs}`);
      const last = podstate[podstate.length - 1];
      lines.push(`  - latest: seq=${last.seq} phase=${last.phase || '?'} adapter=${last.adapterType}/${last.adapter} scope=${last.scopeName || 'root'}/${last.scopeInstanceId} observed_at=${last.observedAt}`);
    } else {
      lines.push('- podstate events: none observed on the run stream at fetch time');
    }
  } else {
    lines.push('- podstate events: NOT VERIFIABLE FROM THIS CREDENTIAL SEAT (run-scoped event reads require a permitted class)');
  }
  if (isolation.length > 0) {
    lines.push('- isolation observations (KB-326 S3):');
    for (const o of isolation) {
      lines.push(`  - ${o.procedure} [${o.credential}]: ${o.observed}${o.detail ? ` — ${o.detail}` : ''}`);
    }
  }
  lines.push('- watcher parity counters: log-only (recordParity) — recorded in watcher logs, not castle rows:');
  lines.push('  `kubectl -n criteria-jobs logs deploy/kanboard-watcher | grep -Ei "parity" | tail`');
  return lines.join('\n');
}

// --- CLI -------------------------------------------------------------------

function usage() {
  process.stderr.write(`\
usage: node scripts/castle_verify.mjs --castle URL --run-id ID [--cr-name NAME]
  [--expected-criteria PREFIX] [--token-file PATH] [--operator OPERATOR_URL]
  [--out FILE] [--pretty]
  (--castle and --operator default from CASTLE_* / CRITERIA_K8S_OPERATOR_* env)\n`);
}

function parseArgs(argv) {
  const args = { _: [] };
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (!a.startsWith('--')) { args._.push(a); continue; }
    const key = a.slice(2);
    if (key === 'pretty') { args.pretty = true; continue; }
    const val = argv[++i];
    if (val === undefined || val.startsWith('--')) { usage(); process.exit(2); }
    args[key] = val;
  }
  return args;
}

async function main() {
  const args = parseArgs(process.argv.slice(2));
  const castle = args.castle ?? castleBaseFromEnv();
  const operator = args.operator ?? operatorBaseFromEnv();
  if (!castle || !args['run-id']) { usage(); process.exit(2); }
  const base = castle;
  const runId = args['run-id'];
  const token = args['token-file'] ? (await import('node:fs')).readFileSync(args['token-file'], 'utf8').trim() : undefined;

  // Leg 0 — castle health prologue.
  const healthResp = await connectUnary(base, ServerProcedures.health, { service: '' }, undefined);
  const health = healthObservation(healthResp);

  // Leg D first — the isolation observations are evidence regardless of what
  // the privileged legs return.
  const isolation = [];
  const anonGets = [];
  for (const procedure of [ServerProcedures.getRun, ServerProcedures.listRunEvents]) {
    const body = procedures.body(procedure, runId);
    anonGets.push({ procedure, resp: await connectUnary(base, procedure, body, undefined) });
  }
  for (const { procedure, resp } of anonGets) {
    if (['unauthenticated', 'http_401', 'not_found', 'http_404'].includes(resp?.code)
      || resp?.code === 'permission_denied' || resp?.code === 'http_403'
      || resp?.code === 'unavailable') {
      isolation.push(refusalObservation(procedure, resp, { tokenless: true }));
    }
  }

  // Legs A/B/C — tokened pass (whatever class the lane carries; refusals
  // stay observations).
  const dispResp = await connectUnary(base, ServerProcedures.disposition, { runId }, token);
  const disposition = dispResp?.code
    ? (isolation.push(refusalObservation(ServerProcedures.disposition, dispResp)), null)
    : dispositionVerdict(dispResp, args['expected-criteria'] ?? '');

  const runsResp = await connectUnary(base, ServerProcedures.getRun, { runId }, token);
  const runRow = runsResp?.code
    ? (isolation.push(refusalObservation(ServerProcedures.getRun, runsResp)), null)
    : runRowVerdict(runsResp);

  let podstate = null;
  const eventsResp = await connectUnary(base, ServerProcedures.listRunEvents, { runId, sinceSeq: 0, limit: 500 }, token);
  if (eventsResp?.code) {
    isolation.push(refusalObservation(ServerProcedures.listRunEvents, eventsResp));
  } else {
    podstate = podstateSeqs(eventsResp);
  }

  // Leg E — operator CR view (operator-stamped CriteriaRun executing).
  let operatorView = null;
  if (operator && args['cr-name']) {
    try {
      const res = await fetch(`${operator.replace(/\/$/, '')}${OperatorViewPath}?job=${encodeURIComponent(args['cr-name'])}`);
      const body = res.ok ? await res.json() : null;
      operatorView = operatorViewVerdict(body, args['cr-name']);
      if (!res.ok) operatorView.probe = `http_${res.status}`;
    } catch (err) {
      operatorView = { job: args['cr-name'], phase: 'unavailable', terminal: false, crPresent: false, running: false, probe: `transport: ${err.message}` };
    }
  }

  const evidence = assembleEvidence({
    runId, crName: args['cr-name'], health, operatorView, disposition, runRow, podstate, isolation,
  });

  const doc = { runId, crName: args['cr-name'] ?? null, health, operatorView, disposition, runRow, podstate, isolation, evidence };
  const out = JSON.stringify(doc, null, args.pretty ? 2 : undefined);
  if (args.out) writeFileSync(args.out, `${out}\n`);
  process.stdout.write(`${args.out ? `wrote ${args.out}\n` : ''}${evidence}\n`);
  process.exit(0);
}

const procedures = {
  body(procedure, runId) {
    if (procedure === ServerProcedures.listRunEvents) return { runId, sinceSeq: 0, limit: 1 };
    return { runId };
  },
};

if (process.argv[1] && import.meta.url === `file://${process.argv[1]}`) {
  main().catch((err) => { console.error(`castle_verify: ${err.message}`); process.exit(3); });
}