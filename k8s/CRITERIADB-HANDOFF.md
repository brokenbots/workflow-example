# CRITERIADB DESIGN STATE — HANDOFF (2026-09-17)

Pickup phrase: "pickup criteriadb" -> read this doc first, then file the tickets (section 12).

## 1. Origin

Locked in the Slack session "Research workflow effort in k8s" (2026-09-17,
@session:default/20260917_132441_feaf2ff9). User instruction "lock in the plan"
arrived at session end and was NEVER executed: no Linear project, milestones,
or tickets exist yet. This doc preserves every design decision so the ticket
filing needs no re-derivation.

## 2. Roles (the boundary — nothing crosses except keys, bytes, DSNs)

- criteria engine: checkpoint/run semantics. When to checkpoint (step/turn
  boundaries, pause-points=checkpoint-points), durability ordering (state
  durable BEFORE step-done event), restore protocol (prior_state in
  OpenSession/Spawn, digest verify, restore failure = LOUD terminal error),
  run lifecycle (RUNNING/STOPPED/paused, reaper exemption), pointer events
  into the stream (map in EVERY consumer: events parser AND castle wire.go),
  engine-driven retention. Adapters NEVER see a DSN or token — they reach
  state only through the engine's session contract (OpenSession secrets
  posture). AuthZ enforced at the criteria boundary by construction.
- criteriadb: semantic substrate ONLY. Graph/temporal/location semantics +
  generic mechanisms: keyed KV (get/put/delete/list by id), declared
  field-level indexes + ordered listing, retention PRIMITIVES (delete-by-
  prefix, TTL hooks — policy lives with criteria/castle), per-tenant authZ.
  No checkpoint/run semantics in this repo ever. A criteriadb PR that
  mentions "checkpoint" or "run state" semantics is a PR in the wrong repo.
- castle: tenant + control plane. Realtime/liveness stays the event stream
  (SubscribeRunEvents). Shared store = DEPTH (run history, checkpoint
  metadata, attempts) for Parapet panels.
- engines (bbolt/postgres/cockroach + disk below): durability, encryption,
  HA. "Engines lift, criteriadb specializes in graph/temporal/location" —
  user-stated philosophy; the repo is already shaped for it (thin Store
  interface, dumb backends, cleverness in pkg/memory).

## 3. Storage topology (settled)

- bbolt = local runs; postgres = k8s/remote runs; cockroach = free HA bump
  (store_postgres.go is vanilla pgx/v5 — conn-string swap). State no longer
  depends on PVs; durability story moves to the db.
- Ownership-scoped tables in ONE store (revises the old "castle read-only"
  rule): criteria owns run/checkpoint tables (RW its own), castle owns its
  runs/events/attempt tables (RW its own), each READ-ONLY on the other's.
  Two writers of the SAME tables is the thing that was banned; disjoint
  tenants in one store is fine. Migrations: criteriadb-team-owned, postgres
  level (user-set).
- KV face status (verified): storage layer ALREADY KV — bbolt = 2 buckets
  keyed by id (put/delete exist), postgres = INSERT..ON CONFLICT(id) upsert.
  Missing only the API face: Engine GetNode/DeleteNode + proto RPCs.
  Trivial pass-through.
- Field indexes: postgres already extracts label/type/project columns +
  btree indexes; ORDER BY falls out. bbolt = index buckets keyed
  <field>_<value>_<id>, prefix scan = ordered listing (~100-150 lines).
  Keep the declared-index API tiny. Postgres choice SURVIVES the KV+index
  requirements — ordered listing is the requirement that argues FOR SQL.
- Local optional encryption: disk layer (LUKS/fscrypt/encrypted CSI), NOT
  app-level. bbolt 0600 file perms are the local auth model. No v1 keyfile.

## 4. Transport-security doctrine (user-directed, ALL criteria projects)

mTLS everywhere is the GOAL. Ladder: mTLS -> accept_token + TLS (cert
validation preferably ON, with disable-validation escape hatch for
self-signed/local) -> bare accept_token (floor). Projects bootstrap low and
climb; support ALL paths so operators choose low-friction local,
cloud-hardened, or mesh offload.
Design rule that keeps it cheap: AuthN mode = deployment/transport config
(flags/env), NEVER proto fields. Interceptor resolves every mode to ONE
internal type: authenticated tenant identity. AuthZ is the invariant:
token->tenant today, cert-SAN->tenant at mTLS, same ownership model
underneath. Climbing hardens the handshake, not the authz model.

## 5. Auth status (verified 2026-09-17)

criteriadb service layer has ZERO auth today: grpc.NewServer() no
interceptors, http.ListenAndServe no middleware, no tls/token flags.
Postgres DSN is the entire remote credential story (pgx sslmode). Local =
file perms. :8080 gRPC and :8081 viz dashboard wide open.
v1 target: token over TLS-capable listener + disable-validate flag; mTLS as
the same interceptor's second mode, designed-in, enabled later. Three
tenants: criteria runner (run-scoped RW), castle (RW its tables, RO on
criteria's), adapters (NO token ever).

## 6. Visualizer (user-directed 2026-09-17)

Viz becomes its own tool in the future; criteria calls must support that
(keep graph/list APIs in the PUBLIC proto — viz is just another client).
Near-term: bind viz to loopback when enabled (security fix). Interim: castle
may proxy it. Target state: castle exposes the memory-graph API and PARAPET
renders it (same pattern as criteria.v1 proxy today) — one UI, console-auth
inherited, no second dashboard to harden.

## 7. Repo facts (verified)

github.com/brokenbots/criteriadb — "Portable, Pure-Go Agent Memory Graph
Database for AI Workflows". Created 2026-09-12, ~2.5k LOC Go, zero CGO.
Cloned at ~/Projects/criteriadb. pkg/memory (engine: temporal, digital/
physical location, 768D vector, lexical, graph, consolidation; Store
interface: store_bbolt.go + store_postgres.go), proto/criteriadb/v1
(MemoryService = Remember + Recall ONLY), cmd/criteriadb (serve/query/list/
stats/consolidate/export/import/viz), adapter v2 (remember, remember_relation,
recall, fact_lookup), pkg/criteria ND-JSON event ingest. Tags v0.1.0-v0.2.0
exist, ZERO published releases. Dev paths in PR body trace /Users/dave/.../
momentodb (former name). No references in criteria/castle/workflow-example.
CI on main: 4 checks green.

## 8. Open PR #9 (merge first)

fix/audit-hardening-and-test-coverage, 1 ahead of main, CI green (lint/
tests/security scan), mergeable clean, no reviews. Contents: import drops-
edges fix, dangling-edge pruning, multi-hop BFS fact search, XSS/SSRF/NaN
hardening, graceful shutdown, edge map dedup, proto.Clone isolation.
Merge it first; it is the hardening baseline.

## 9. Known correctness/hardening debt (ticket list for the milestone)

- Server-side authz (section 5) — FIRST ticket.
- Single global RWMutex; LoadAll-entire-DB at startup (fine for memory
  graph, wrong for checkpoint scale). Postgres read path needs run-keyed
  scoped loads.
- Keyed upserts don't check ownership (silent overwrite) — needs tenant
  check once authz lands.
- No migration tooling (bare CREATE TABLE IF NOT EXISTS) — versioned
  migrations required BEFORE criteria/castle integrate.
- Publish real releases (tags exist, no GH releases; GHCR pin story).

## 10. Checkpoint contract (criteria-side, for the criteria project)

Declaration in InfoResponse (mode none|blob|ref, schema, max_bytes ~400K,
granularity per-step|per-turn). Step done only once state durable (no
step-done without checkpoint — ordering makes crash-between impossible).
Restore: prior_state{schema, blob|ref, digest} in OpenSession/Spawn; restore
failure loud. Large/file-shaped state = environment, out of scope (idempotent
setup re-runs). Stage 1 = contract + pause (pods stay up, pause verb, adapter
idle-not-die). Stage 2 = stop/resume spikes (checkpoint->terminate->teardown,
re-provision on resume, STOPPED = reaper-exempt resumable state; do NOT
build on the crash-recovery reattach path). Per-turn granularity ships v1.

## 11. Castle integration

Implements its internal/store.Store interface on criteriadb (the interface
comment already invites this). HA ladder: castle+bbolt single machine ->
castle+postgres HA storage (NOT leader election — bbolt/sqlite are single-
writer; multi-replica castle still needs write coordination; the castle
reaper SQLITE_INTERRUPT wedge is the cautionary tale). Castle janitors its
own tables; criteria janitors its own; nobody cross-deletes. Retention must
exempt STOPPED runs when stop/resume exists. Bootstrap: static token in k8s
secret (criteria-secrets pattern).

## 12. TICKETS — FILED (by the parallel session, 2026-09-17 20:42)

The "lock in the plan" step was executed by another session while design
discussions continued here. VERIFIED in Linear:
- Project "Research Workflows on k8s - Stage 1: Checkpoint + Pause"
  (cb4cd1a5-3e0b-42c6-b4e1-063695ea9bfc), milestones M1-M4, detailed
  tickets CRI-199..CRI-208 (criteriadb state tables, client integration,
  InfoResponse declaration, engine save/restore, pointer events, session
  protocol pause verb, shell+copilot adapters, castle STOPPED, operator).
- Project "Research Workflows on k8s - Stage 2: Stop/Resume"
  (bbd957c7-33b3-4a21-a62d-f902199aaffa), spikes CRI-209/210.
NOTE: ticket bodies predate the design refinements in THIS doc (sections
4-6: transport ladder, castle-as-tenant RW revision, auth reality, viz
target, PR #9 merge-first). CRI-199/200 implementers must read this doc;
its sections carry the constraints the ticket text doesn't have.
Schema-migrations ownership: criteriadb team, postgres level. User said
criteria/castle/adapters just implement the criteriadb client.