# Phase 2 Handoff — criteria-k8s adapter isolation

State as of 2026-09-10. Written for session pickup: "start criteria k8s phase2".

## Where things stand

Phase 1 (CRI-114, merged PR #20 + fixes) is done and validated end-to-end:
one runner Job + one adapter Job per adapter type (shell, copilot), each
adapter pod with zero CSI volumes and zero credential env vars, secrets
flowing only via OpenSession, shim on the pod network with accept_token,
digest-pinned handshakes. A full CriteriaRun (CRI-112) completed through
this topology autonomously: classify -> develop -> PR #21 -> merge -> Done.

## Tickets to do next (in order)

1. CRI-115 — repo github.com/brokenbots/criteria (ENGINE code; must go
   through a ticket + the workflow, never edit engine Go directly):
   - provision-wanted / release events emitted into the run event stream at
     remote-adapter scope init (initScopeAdapters) and teardown
     (tearDownScopeAdapters). Payload: run_id, scope id (root vs named
     subworkflow + scope instance UUID), adapter name, lockfile digest,
     shim address, token as a FILE REFERENCE under the run's data dir
     (never the raw token - events are archived by castle).
   - per-scope accept-token rotation, gated behind a new remote env
     attribute per_scope_sessions = true (default false; default keeps
     phase-1 behavior). Rotated token makes a torn-down pod's redial fail
     until the operator deletes it.
   - scope-tagged handshakes: WaitForHandle keys by adapter NAME only
     today (internal/adapterhost/sessions.go:883-886), so two parallel
     subworkflow scopes using the same adapter type collide. Add optional
     scope field to the SDK remoteHandshake + shim verify + WaitForHandle
     keying. criteria-go-adapter-sdk bump, backward compatible; note
     version coordination for shell/copilot adapter releases.
   - Key source files: internal/engine/lifecycle.go (initScopeAdapters,
     tearDownScopeAdapters), internal/engine/node_workflow.go:81-102
     (subworkflow scopes), internal/engine/engine.go maybeStartRemoteShim,
     internal/adapter/environment/remote/{shim,handle}.go,
     criteria-go-adapter-sdk/adapterhost/serve_remote.go (handshake).

2. CRI-116 — repo github.com/brokenbots/workflow-example (operator code,
   criteria-k8s/): operator tails each active CriteriaRun's events file
   (shared /data PVC; path is in CriteriaRun.status.eventsPath), reconciles
   provision-wanted -> create per-scope adapter pod (token from referenced
   path, scope tag, runner dial address, pinned digest) and release ->
   delete pod. Rules: process releases before provision-wants for the same
   adapter type (pod-name collision avoidance); desired state fully
   re-derivable from the event stream (idempotent, survives operator
   restart, orphan-free incl. after operator kill mid-run). Runner and
   adapter pods keep zero k8s RBAC; adapters keep zero CSI.

3. CRI-117 — per-repo run serialization queue in the operator (low
   priority; dovetails with the future New-queue ordering agent).

## Design decisions already made (do not relitigate)

- Operator-mediated lifecycle (option B), NOT engine-side provisioning:
  all k8s privilege stays in the operator; the engine only emits events.
- accept_token auth for now (no mTLS yet) - user deferred mTLS to focus
  on remote environment semantics.
- Watcher triggers on Triage state ONLY. A future, separate agent will
  watch the New queue and decide ordering/promotion to Triage. Do not
  change the watcher trigger.
- Adapters never read their own process env for workflow secrets (SDK
  contract D69); secrets reach them via OpenSession. The shell adapter
  overlays session secrets into child env (shell.go:141); copilot
  forwards them to bash via SpawnEnv as of criteria-adapter-copilot
  v0.5.6 (CRI-118, merged PR #23).
- per-adapter pods mount /repo RW (steps edit code) and /data RW
  (artifacts); adapter pods get NO CSI mounts.

## Environment / gotchas

- KUBECONFIG=/home/dave/.kube/config. criteria-jobs namespace enforces
  restricted PodSecurity; only the amd64 catch node runs workloads
  (nodeSelector kubernetes.io/arch=amd64 + catch toleration).
- Local registry localhost:5000. USE UNIQUE TAGS: the kubelet never
  re-resolves a reused tag with IfNotPresent (this bit us 3 times).
  Current: operator localhost:5000/criteria-k8s:v4; workflow image
  linear-intake-remote:v0.5.18-4; fat adapter images
  criteria-adapter-{shell:k8s-0.5.3,copilot:k8s-0.5.6} (contain git/gh;
  copilot one also has node + @github/copilot CLI - the minimal ghcr
  adapter images have NONE of these and die on step execution).
- Deployments live: criteria-k8s-operator (v4), criteria-linear-watcher
  (CRITERIA_IMAGE=linear-intake-remote:v0.5.18-4, polls Linear Triage
  tickets in project "Criteria K8s Workflow Runner" every 60s),
  castle (restricted-compliant), secrets-store CSI driver + OpenBao
  CSI provider in ns csi. OpenBao root token: ~/k3s-dynamo/openbao.keys
  (KV criteria/linear holds linear_api_key + both GitHub tokens).
- To clean up a finished run: delete the CriteriaRun (cascades all its
  Jobs via owner refs). Deleting individual Jobs makes the Job
  controller recreate pods.
- Adapter Jobs linger after run completion (ServeRemote reconnect loop
  never exits) - that's the exact gap phase 2 closes; until CRI-116,
  delete the CriteriaRun to clean up.
- Engine releases: criteria v0.5.18 is current (contains CRI-111 lockfile
  digest-verification fix + CRI-109 env-scrub fix). Engine changes need
  a new tag -> release workflow -> bump linear_intake_v1/Dockerfile
  CRITERIA_VERSION -> rebuild + re-lock (criteria adapter lock in a
  container against a writable copy of the tree; host binary is too old
  at v0.5.7).
- Workflow runs re-verify adapter digests against the lockfile; when
  bumping adapter versions, bump adapters.chcl + criteria adapter lock
  ALL workflow trees, then the Dockerfile pre-pull, then rebuild.
- Repo URLs in ticket descriptions: lead with the full github.com URL -
  the ExtractRepoURL short-form heuristic (fixed in CRI-113) prefers
  explicit URLs; DEFAULT_REPO_URL is set on the watcher as fallback.

## Process conventions (user-mandated)

- Engine/adapter Go code changes go through Linear tickets + the intake
  workflow; workflow infra files (.chcl, Dockerfile, entrypoint, Makefile,
  k8s manifests/scripts) may be edited directly.
- PRs need an approval from the opposite GitHub identity: PRs authored by
  handcaught get approved by brokenbot (reviewer token), authored by
  brokenbot get approved by handcaught (workflow token). Both tokens in
  OpenBao criteria/linear. Repo rules require an approving review + green
  CI; merge with the non-author identity.
- Drive work autonomously; intervene immediately when a run stalls
  (approve/merge its PR) rather than waiting. Don't stop to ask when the
  next step is clear.

## Suggested kickoff sequence

1. Move CRI-115 to Triage (stateId 34864fc9-f5df-4d71-b0a6-6d3075fa93dd)
   in Linear - the watcher + pipeline take it from there. Monitor with:
   kubectl get criteriaruns -n criteria-jobs; the run's events file is
   at /data/intake/CRI-115/events.ndjson on the criteria-data PVC
   (read via: kubectl exec -n criteria-jobs deploy/castle -- tail ...)
2. When its PR is green, approve/merge per the identity rule above,
   tag criteria vX.Y.Z, rebuild the workflow image, then do CRI-116.
3. CRI-116 validation: kubectl get pods -w during a run must show
   adapter pods created at subworkflow scope entry and deleted at exit;
   kill the operator mid-run and confirm no orphans after restart.

Full session detail if needed:
session_search(session_id='20260907_145606_5a49fe', query='CRI-115 phase 2 adapter pods')