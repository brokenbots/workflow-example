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
//   leg C  podstate events         — pages ListRunEvents until the
//          continuation cursor stops advancing (bounded page budget); when
//          the budget is exhausted the evidence says so explicitly instead
//          of presenting a partial seq list as complete. Extracts every
//          adapter.podstate.observed adapter-event (criteria-k8s
//          internal/events PodStateObservedKind) with its castle-stamped seq.
//   leg D  isolation observations  — the anonymous pass over the read
//          surfaces (GetRun, ListRunEvents, ListRuns, ListAgents; GetAgent
//          needs a full criteria id the seat does not carry), classifying
//          each answer: a refusal is an observation, and a 200 answer by
//          an unauthenticated caller is recorded as an explicit KB-326 S3
//          isolation finding rather than being skipped.
//   leg E  operator CR view      — GET /operator-view?job=<cr-name> on the
//          criteria-k8s operator (KB-234 boot-gate operator view): the
//          card's "CR stamped by the operator" chain evidence. Optional;
//          enabled by --operator or CRITERIA_K8S_OPERATOR_* env. A failed
//          or undecodable probe renders "NOT PROBED" — only a genuine
//          {"phase":"NotFound"} answer (a 200 body; bootgate_view.go)
//          may claim the KB-234 orphan shape.
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

import { readFileSync, writeFileSync } from 'node:fs';

// --- constants ------------------------------------------------------------

export const PodStateObservedKind = 'adapter.podstate.observed';
const PodStatePhases = new Set(['Pending', 'Running', 'Succeeded', 'Failed', 'Unknown']);

const ServerProcedures = {
  health: 'grpc.health.v1.Health/Check',
  getRun: 'criteria.v1.ServerService/GetRun',
  listRunEvents: 'criteria.v1.ServerService/ListRunEvents',
  disposition: 'criteria.v1.ServerService/GetAssignmentDisposition',
  listRuns: 'criteria.v1.ServerService/ListRuns',
  listAgents: 'criteria.v1.ServerService/ListAgents',
};
const OperatorViewPath = '/operator-view';

// Per-call transport timeout and the leg C page budget. The harness records
// an unreachable observation instead of hanging on an unresponsive castle.
const FetchTimeoutMs = 10_000;
const MaxEventPages = 20;
const EventPageLimit = 500;

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
// Each call is bounded by timeoutMs (default 10s) so an unresponsive castle
// surfaces as an unavailable observation rather than a hang.
export async function connectUnary(base, procedure, body, token, fetchImpl = fetch, timeoutMs = FetchTimeoutMs) {
  const headers = { 'Content-Type': 'application/json', 'Connect-Protocol-Version': '1' };
  if (token) headers.Authorization = `Bearer ${token}`;
  let res;
  try {
    const ctl = new AbortController();
    const timer = setTimeout(() => ctl.abort(), timeoutMs);
    try {
      res = await fetchImpl(base.replace(/\/$/, '') + '/' + procedure, {
        method: 'POST', headers, body: JSON.stringify(body ?? {}), signal: ctl.signal,
      });
    } finally {
      clearTimeout(timer);
    }
  } catch (err) {
    const cause = err?.cause?.code ?? err?.code;
    return { code: 'unavailable', message: `transport: ${cause ?? err.message}` };
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

// collectPodstate pages ListRunEvents until the run stream is drained,
// mirroring the in-repo castle event consumer (criteria-k8s
// internal/castle/castle.go:468-516): each page advances the `since` cursor
// to the max of the events' seqs and the response's next_since_seq when
// greater; a short page (fewer events than the page limit) ends the loop.
// maxPages bounds the loop so a pathological producer cannot spin the
// harness; exhausting the budget sets `truncated` and the evidence bundle
// renders an explicit marker instead of presenting a partial seq list as
// complete. `limit` is injectable for the tests' small-page fixtures.
export async function collectPodstate(base, runId, token, fetchImpl = fetch, maxPages = MaxEventPages, limit = EventPageLimit) {
  const events = [];
  let since = 0;
  let nextSinceSeq = 0;
  let truncated = false;
  let refusal = null;
  let pages = 0;
  let drained = false;
  for (let page = 0; page < maxPages; page++) {
    const resp = await connectUnary(base, ServerProcedures.listRunEvents, { runId, sinceSeq: since, limit }, token, fetchImpl);
    if (resp?.code) { refusal = resp; break; }
    pages = page + 1;
    const pageEvents = Array.isArray(resp.events) ? resp.events : [];
    events.push(...pageEvents);
    let cursor = since;
    for (const env of pageEvents) {
      const seq = normalizeSeq(env.seq ?? env.Seq);
      if (seq > cursor) cursor = seq;
    }
    const next = normalizeSeq(resp.nextSinceSeq ?? resp.next_since_seq);
    if (next > cursor) cursor = next;
    if (cursor > since) { since = cursor; nextSinceSeq = cursor; }
    if (pageEvents.length < limit) { drained = true; break; } // short page: run stream drained
  }
  truncated = !drained && !refusal && pages === maxPages;
  return { podstate: podstateSeqs(events), refusal, truncated, nextSinceSeq, pages };
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
// absence is a documented observation rather than a checklist failure. When
// expectedWorkflow is set (card item 2: the run's workflow must be this
// repo's kanboard-develop tree), the workflow name is prefix-compared and a
// mismatch is carried as an explicit checklist failure.
export function runRowVerdict(run, expectedWorkflow = '') {
  const projectId = String(run?.projectId ?? run?.project_id ?? '');
  const workflowName = String(run?.workflowName ?? run?.workflow_name ?? '');
  const verdict = {
    runId: String(run?.runId ?? run?.run_id ?? ''),
    criteriaId: String(run?.criteriaId ?? run?.criteria_id ?? ''),
    workflowName,
    status: String(run?.status ?? ''),
    projectFieldExposed: projectId !== '',
    projectId,
  };
  if (expectedWorkflow) {
    verdict.expectedWorkflow = expectedWorkflow;
    verdict.workflowMatch = verdict.workflowName.startsWith(expectedWorkflow);
  }
  return verdict;
}

// operatorViewVerdict reads the KB-234 boot-gate operator-view document for
// the run's CR: {"phase": "...", "terminal": bool}, job-first identity. The
// operator returns "NotFound" as a 200 body when no CR is known for the job
// — that honest answer is the KB-234 orphan class. But a transport failure,
// a non-2xx status (400 missing param, 401, 500) or an undecodable body
// means NOTHING was observed: the verdict must carry a failed/unparseable
// probe rather than asserting absence. Evidence that claims "CR ABSENT"
// from a probe that never succeeded would be the false-orphan failure mode.
export function operatorViewVerdict(resp, crName) {
  const job = crName;
  const ok = resp && resp.transportError === undefined && resp.status >= 200 && resp.status < 300;
  const body = ok ? resp.body : null;
  if (!ok) {
    const detail = resp?.transportError ? `transport: ${resp.transportError}` : `http_${resp?.status ?? 'unknown'}`;
    return { job, probe: 'probe-failed', detail, phase: '', terminal: false, crPresent: false, running: false };
  }
  if (body == null || typeof body !== 'object' || !('phase' in body)) {
    // A 200 that is not JSON or carries no phase field: the probe answered
    // but the verdict is unknowable — never claim present or absent from it.
    return { job, probe: 'probe-unparseable', detail: `undecodable body (http_200)`, phase: '', terminal: false, crPresent: false, running: false };
  }
  const phase = String(body.phase ?? '');
  const terminal = body.terminal === true;
  return {
    job,
    probe: 'probed',
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
  if (!resp?.code) {
    // A 2xx answer with a decodable body: an unauthenticated caller reading
    // run data violates the KB-326 S3 isolation the card verifies — record
    // it explicitly; a tokened 2xx is the ordinary served answer.
    return tokenless
      ? { procedure, credential: cls, observed: 'ANONYMOUS-READ-ALLOWED', detail: 'read returned data without a credential — KB-326 S3 isolation violation' }
      : { procedure, credential: cls, observed: 'served', detail: '' };
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
// than being omitted. A failed operator probe renders "NOT PROBED" — only a
// genuine probed NotFound answer renders the orphan claim.
export function assembleEvidence({
  runId, crName, health, operatorView, disposition, runRow,
  podstate, podstateTruncated = false, podstateNextSinceSeq = 0, isolation = [],
  fetchedAt,
}) {
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
    if (operatorView.probe === 'probed') {
      lines.push(operatorView.crPresent
        ? `- CR (operator view, job-first): phase=${operatorView.phase} terminal=${operatorView.terminal}`
          + (operatorView.running ? ' — operator-stamped CriteriaRun executing' : '')
        : `- CR (operator view, job-first): ABSENT (phase=${operatorView.phase}, terminal=${operatorView.terminal}) — KB-234 orphan shape`);
    } else {
      lines.push(`- CR (operator view, job-first): NOT PROBED — probe=${operatorView.detail || operatorView.probe}`);
    }
  } else if (crName) {
    lines.push('- CR (operator view): NOT PROBED — pass --operator (or run from a seat carrying CRITERIA_K8S_OPERATOR_* env) to check the operator-stamped CR');
  }
  if (disposition) {
    lines.push(`- assignment keyed by run_id \`${disposition.runId || (runId ?? 'unknown')}\` (GetAssignmentDisposition is run-keyed; the RPC carries no separate assignment id)`);
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
    if (runRow.expectedWorkflow) {
      lines.push(`  - workflow vs --expected-workflow: ${runRow.workflowMatch
        ? 'matches'
        : `MISMATCH — observed \`${runRow.workflowName}\` does not carry the expected \`${runRow.expectedWorkflow}\` prefix`}`);
    }
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
    if (podstateTruncated) {
      lines.push(`  - TRUNCATED at the ${MaxEventPages}-page budget: next_unread_seq=${podstateNextSinceSeq} — quote this seq list as incomplete`);
    }
  } else {
    lines.push('- podstate events: NOT VERIFIABLE FROM THIS CREDENTIAL SEAT (run-scoped event reads require a permitted class)');
  }
  if (isolation.length > 0) {
    const violation = isolation.some((o) => o.observed === 'ANONYMOUS-READ-ALLOWED');
    lines.push(violation
      ? '- isolation observations (KB-326 S3): VIOLATION — an unauthenticated read returned data:'
      : '- isolation observations (KB-326 S3):');
    for (const o of isolation) {
      lines.push(`  - ${o.procedure} [${o.credential}]: ${o.observed}${o.detail ? ` — ${o.detail}` : ''}`);
    }
  } else {
    lines.push('- isolation observations: none recorded');
  }
  lines.push('- watcher parity counters: log-only (recordParity) — recorded in watcher logs, not castle rows:');
  lines.push('  `kubectl -n criteria-jobs logs deploy/kanboard-watcher | grep -Ei "parity" | tail`');
  return lines.join('\n');
}

// runVerify drives every leg against `opts.fetchImpl` (injectable for the
// regression suite) and returns the raw leg results. It performs no I/O
// beyond the injected fetch and never throws on a refusal — evidence
// integrity lives here, so the suite exercises this seam directly.
export async function runVerify(opts) {
  const {
    castle, runId, crName = '', expectedCriteria = '', expectedWorkflow = '',
    operator = '', token, fetchImpl = fetch,
  } = opts;
  const isolation = [];

  // Leg 0 — castle health prologue.
  const health = healthObservation(await connectUnary(castle, ServerProcedures.health, { service: '' }, undefined, fetchImpl));

  // Leg D first — the isolation observations are evidence regardless of what
  // the privileged legs return. EVERY anonymous answer is recorded: a
  // refusal is classified (uniform not_found / class gates), and a 200
  // answer from an unauthenticated caller is the KB-326 S3 violation the
  // card verifies against — never silently skipped.
  for (const [procedure, body] of [
    [ServerProcedures.getRun, { runId }],
    [ServerProcedures.listRunEvents, { runId, sinceSeq: 0, limit: 1 }],
    [ServerProcedures.listRuns, {}],
    [ServerProcedures.listAgents, { limit: 10 }],
  ]) {
    isolation.push(refusalObservation(procedure, await connectUnary(castle, procedure, body, undefined, fetchImpl), { tokenless: true }));
  }

  // Legs A/B/C — tokened pass (whatever class the lane carries; refusals
  // stay observations).
  const dispResp = await connectUnary(castle, ServerProcedures.disposition, { runId }, token, fetchImpl);
  const disposal = dispResp?.code
    ? (isolation.push(refusalObservation(ServerProcedures.disposition, dispResp)), null)
    : dispositionVerdict(dispResp, expectedCriteria);

  const runsResp = await connectUnary(castle, ServerProcedures.getRun, { runId }, token, fetchImpl);
  const runRow = runsResp?.code
    ? (isolation.push(refusalObservation(ServerProcedures.getRun, runsResp)), null)
    : runRowVerdict(runsResp, expectedWorkflow);

  const podstateRun = await collectPodstate(castle, runId, token, fetchImpl);
  if (podstateRun.refusal) {
    isolation.push(refusalObservation(ServerProcedures.listRunEvents, podstateRun.refusal));
  }

  // Leg E — operator CR view (operator-stamped CriteriaRun executing). The
  // probe result carries its own status so operatorViewVerdict can tell a
  // genuine NotFound (KB-234 orphan class) apart from a failed probe.
  let operatorView = null;
  if (operator && crName) {
    const probe = await operatorViewFetch(operator, crName, fetchImpl);
    operatorView = operatorViewVerdict(probe, crName);
  }

  return {
    runId, crName, health, operatorView, disposition: disposal, runRow,
    podstate: podstateRun.refusal ? null : podstateRun.podstate,
    podstateTruncated: podstateRun.truncated,
    podstateNextSinceSeq: podstateRun.nextSinceSeq,
    isolation,
  };
}

// operatorViewFetch GETs the operator view with a bounded timeout and
// returns a {status, body} / {transportError} probe answer for the verdict.
async function operatorViewFetch(operator, job, fetchImpl = fetch, timeoutMs = FetchTimeoutMs) {
  const ctl = new AbortController();
  const timer = setTimeout(() => ctl.abort(), timeoutMs);
  try {
    const res = await fetchImpl(`${operator.replace(/\/$/, '')}${OperatorViewPath}?job=${encodeURIComponent(job)}`, { signal: ctl.signal });
    let body = null;
    try { body = await res.json(); } catch { /* undecodable body — the verdict carries the failed probe */ }
    return { status: res.status, body };
  } catch (err) {
    return { transportError: err?.cause?.code ?? err.message };
  } finally {
    clearTimeout(timer);
  }
}

// --- CLI -------------------------------------------------------------------

// --- CLI -------------------------------------------------------------------

function usage() {
  process.stderr.write(`\
usage: node scripts/castle_verify.mjs --castle URL --run-id ID [--cr-name NAME]
  [--expected-criteria PREFIX] [--expected-workflow PREFIX] [--token-file PATH]
  [--operator OPERATOR_URL] [--out FILE] [--pretty]
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
  const runId = args['run-id'];
  let token;
  if (args['token-file']) {
    try { token = readFileSync(args['token-file'], 'utf8').trim(); }
    catch (err) { console.error(`castle_verify: token file unreadable: ${err.message}`); process.exit(3); }
  }

  const legs = await runVerify({
    castle, runId, crName: args['cr-name'] ?? '',
    expectedCriteria: args['expected-criteria'] ?? '',
    expectedWorkflow: args['expected-workflow'] ?? '',
    operator, token,
  });

  const evidence = assembleEvidence(legs);

  const doc = { ...legs, evidence };
  const out = JSON.stringify(doc, null, args.pretty ? 2 : undefined);
  if (args.out) writeFileSync(args.out, `${out}\n`);
  process.stdout.write(`${args.out ? `wrote ${args.out}\n` : ''}${evidence}\n`);
  process.exit(0);
}

if (process.argv[1] && import.meta.url === `file://${process.argv[1]}`) {
  main().catch((err) => { console.error(`castle_verify: ${err.message}`); process.exit(3); });
}