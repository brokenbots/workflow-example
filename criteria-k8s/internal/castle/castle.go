// Package castle observes run lifecycle from the castle control plane
// (CRI-133 ServerService Connect API). It replaces the operator's PVC
// event-file reads: per-scope reconcile and terminal phase stamping consume
// the Observation returned by Client.Observe.
//
// The operator is read-only towards castle. Castle is populated by the runs
// themselves (CRI-134 server-mode dual-write); the operator-side publish
// loop from CRI-131 is retired with this package's introduction.
package castle

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	connect "connectrpc.com/connect"

	v1 "github.com/brokenbots/workflow-example/criteria-k8s/internal/criteria/pb/criteria/v1"
	v1connect "github.com/brokenbots/workflow-example/criteria-k8s/internal/criteria/pb/criteria/v1/criteriav1connect"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/events"
	"time"
)

// Page budgets for castle reads. Package-level vars (not consts) so tests
// can lower the budgets.
var (
	// defaultPageSize bounds ListRunEvents/ListRuns pages.
	defaultPageSize = 200
	// maxEventPages bounds a single Observe drain so a pathological backlog
	// cannot pin a reconcile pass.
	maxEventPages = 50
	// maxDiscoveryPages bounds agent and run discovery paging.
	maxDiscoveryPages = 10
)

const (
	// runStatusSucceeded is castle's terminal success status.
	runStatusSucceeded = "succeeded"
)

// ErrRunNotFound reports that castle conclusively has no record of the run:
// discovery paged the agent (and run) table to completion and found neither
// the runner's agent nor a run for the criteria. For a runner Job that is
// already terminal this is final — the agent registers from the runner pod,
// which no longer exists, so no later pass can find it (pre-castle-native
// runs whose runner never registered). Errors failing errors.Is(ErrRunNotFound)
// are transient (transport failures, inconclusive paging, ingest lag) and
// remain retryable.
var ErrRunNotFound = errors.New("castle has no record of the run")

// Config configures castle observation.
type Config struct {
	// Addr is the castle Connect endpoint (e.g.
	// http://castle.criteria-jobs.svc.cluster.local:8080). Empty disables
	// observation.
	Addr string
	// Token is the pre-shared criteria bearer token used for castle reads.
	Token string
}

// Terminal is the terminal run completion observed in castle.
type Terminal struct {
	// Success mirrors RunCompleted.success (false for RunFailed).
	Success bool
	// FinalState is the engine's workflow terminal state name carried by the
	// RunCompleted envelope (e.g. "handler_complete"). Castle run records
	// carry no final_state column (mapRun never sets it), so on the record
	// path this stays empty.
	FinalState string
	// PRNumber is the pull request number parsed from the castle Run's
	// pr_url. No castle build populates pr_url today (nothing publishes
	// run.metadata), so this stays empty in practice; it is informational
	// enrichment and is never part of the controller's terminal-completion
	// gate.
	PRNumber string
	// Reason is the RunFailed reason, when the run failed.
	Reason string
}

// Observation is what one Observe pass consumed for a CriteriaRun.
type Observation struct {
	// RunID is the castle run id backing the CriteriaRun, empty when the run
	// has not been registered in castle (yet).
	RunID string
	// Lifecycle is the full lifecycle event history known to this client
	// (provision-wanted and release events, in stream order). It is the
	// castle-sourced equivalent of the events.ndjson lifecycle stream and
	// feeds the same desired-state derivation as before.
	Lifecycle []events.LifecycleEvent
	// Terminal is the terminal run event, when one has been observed.
	Terminal *Terminal
	// LastProgress is the newest step-progress event timestamp the
	// observation has consumed (lifecycle envelopes, plus the copilot
	// adapter's agent-activity AdapterEvents — see progressFromEnvelope;
	// heartbeats, terminal envelopes and non-activity adapter chatter never
	// count; KB-24). Zero when no step-progress event has been observed
	// (yet): the controller then stamps its own baseline so the stall
	// watchdog's clock starts with the first authoritative observation.
	LastProgress time.Time
}

// RunSource is the controller-facing observation surface. *Client implements
// it; tests provide fakes.
type RunSource interface {
	// Observe drains castle for the run backing the given runner job.
	// knownRunID short-circuits run discovery once the controller has
	// persisted the castle run id.
	Observe(ctx context.Context, runnerJob, knownRunID string) (*Observation, error)

	// Disabled reports whether castle observation is configured off.
	Disabled() bool
}

// Client observes run lifecycle from castle via the ServerService Connect
// API. It is read-only and safe for concurrent use.
type Client struct {
	addr string
	runs v1connect.ServerServiceClient

	mu        sync.Mutex
	cursors   map[string]uint64            // runID -> last seq consumed (exclusive)
	scopes    map[string]map[string]string // runID -> scopeInstanceID -> provisioned adapter
	lifecycle map[string][]events.LifecycleEvent
	terminals map[string]*Terminal
	// progress tracks the newest step-progress event timestamp per run
	// (KB-24): the stall watchdog's baseline. Retained like cursors so a
	// controller restart's full drain replay recomputes it from history.
	progress map[string]time.Time
}

// New builds a castle observation client. A nil httpClient uses
// http.DefaultClient.
func New(cfg Config, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if cfg.Token != "" {
		httpClient = &http.Client{Transport: &tokenTransport{base: httpClient.Transport, token: cfg.Token}}
	}
	return &Client{
		addr:      cfg.Addr,
		runs:      v1connect.NewServerServiceClient(httpClient, cfg.Addr),
		cursors:   map[string]uint64{},
		scopes:    map[string]map[string]string{},
		lifecycle: map[string][]events.LifecycleEvent{},
		terminals: map[string]*Terminal{},
		progress:  map[string]time.Time{},
	}
}

// Disabled reports whether castle observation is configured off (empty addr).
func (c *Client) Disabled() bool {
	return c == nil || c.addr == ""
}

// Observe drains castle events for the run backing the given runner job and
// returns the accumulated observation. It distinguishes "unavailable or not
// yet known" (error) from an authoritative observation: a castle outage, a
// runner whose agent/run is not registered yet, an empty runner job name, or
// discovery that cannot conclude within the page budget all return an error
// so the caller aborts the reconcile pass instead of converging desired
// state against an empty history (which would delete live adapter pods).
// The caller retries on the next reconcile interval.
func (c *Client) Observe(ctx context.Context, runnerJob, knownRunID string) (*Observation, error) {
	obs := &Observation{}
	if c.Disabled() {
		return obs, nil
	}
	if runnerJob == "" {
		return nil, fmt.Errorf("castle observation requires a runner job name: run discovery matches the agent the runner pod registers")
	}

	runID := knownRunID
	if runID == "" {
		discovered, err := c.findRunByRunner(ctx, runnerJob)
		if err != nil {
			return nil, fmt.Errorf("discovering castle run for runner job %s: %w", runnerJob, err)
		}
		runID = discovered.GetRunId()
	}
	obs.RunID = runID

	terminal, lifecycle, lastProgress, err := c.drain(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("draining castle events for run %s: %w", runID, err)
	}
	obs.Lifecycle = lifecycle
	obs.Terminal = terminal
	obs.LastProgress = lastProgress

	// The run record is authoritative for its terminal status and pr_url:
	// terminal envelopes carry neither, so enrich from GetRun (and fall back
	// to the record for runs that reached a terminal status without a
	// terminal envelope, e.g. cancellation). castle run records carry no
	// final_state column, so no ticket/workflow state comes from here; the
	// envelope path is the only FinalState source.
	resp, err := c.runs.GetRun(ctx, connect.NewRequest(&v1.GetRunRequest{RunId: runID}))
	if err != nil {
		if terminal == nil {
			return nil, fmt.Errorf("getting castle run %s: %w", runID, err)
		}
		// Keep the envelope terminal; the record enrichment is best-effort.
	} else if t := terminalFromRun(resp.Msg); terminal == nil && t != nil {
		obs.Terminal = t
	} else if terminal != nil && resp.Msg != nil {
		if obs.Terminal.PRNumber == "" {
			obs.Terminal.PRNumber = prNumberFromURL(resp.Msg.GetPrUrl())
		}
	}

	// Per-run state (cursor, scopes, accumulated lifecycle history) is
	// retained for the client's lifetime, including after a terminal
	// observation: the controller may re-observe a terminal run (e.g. a
	// failed status write), and evicting the cursor then would restart the
	// ListRunEvents drain at since_seq=0 — re-reading the run's whole event
	// stream. Retention is bounded by the set of runs the operator observes
	// within one process lifetime.
	return obs, nil
}

// findRunByRunner resolves the castle run backing the given runner job.
// The engine inside the runner pod registers a castle agent whose name is
// the pod's hostname (pod name = "<job-name>-<suffix>") and whose criteria
// id pins the workflow it executes; the run created for that criteria id is
// the operator's run. Discovery keys only off fields the engine actually
// writes (Agent.Name, Agent.CriteriaId, Run.CriteriaId): the Run.ticket
// column is documented for k8s-native publishers but no merged engine or
// castle build populates it (CRI-131's publisher was never merged).
func (c *Client) findRunByRunner(ctx context.Context, runnerJob string) (*v1.Run, error) {
	criteriaID, err := c.findCriteriaID(ctx, runnerJob)
	if err != nil {
		return nil, err
	}
	run, err := c.findRunForCriteria(ctx, criteriaID)
	if err != nil {
		return nil, fmt.Errorf("criteria %s (agent for runner job %s): %w", criteriaID, runnerJob, err)
	}
	return run, nil
}

// findCriteriaID resolves the criteria id of the agent the runner job's pod
// registered. ListAgentsRequest carries no filter field, so discovery pages
// the whole agent table. Agent names are pod hostnames, matched on the
// runner job name with or without the pod-name suffix; more than one
// distinct criteria id among the matches is ambiguous and surfaces as an
// error rather than a guess. Non-conclusion within the page budget is an
// error too.
func (c *Client) findCriteriaID(ctx context.Context, runnerJob string) (string, error) {
	prefix := runnerJob + "-"
	ids := map[string]struct{}{}
	online := map[string]struct{}{}
	lastSeen := map[string]string{}
	pageToken := ""
	for page := 0; page < maxDiscoveryPages; page++ {
		resp, err := c.runs.ListAgents(ctx, connect.NewRequest(&v1.ListAgentsRequest{
			Limit:     int32(defaultPageSize),
			PageToken: pageToken,
		}))
		if err != nil {
			return "", err
		}
		for _, agent := range resp.Msg.GetAgents() {
			name := agent.GetName()
			if name != runnerJob && !strings.HasPrefix(name, prefix) {
				continue
			}
			if id := agent.GetCriteriaId(); id != "" {
				ids[id] = struct{}{}
				// CRI-137 follow-up: a runner restart registers a second
				// agent for the same job while the dead one lingers
				// offline. Prefer online agents; among equals take the
				// most recently seen. An offline agent's registrations are
				// dead state, not the run's identity.
				if agent.GetStatus() == "online" {
					online[id] = struct{}{}
				}
				if lastSeen[id] == "" || agent.GetLastSeenAt().AsTime().Format(time.RFC3339) > lastSeen[id] {
					lastSeen[id] = agent.GetLastSeenAt().AsTime().Format(time.RFC3339)
				}
			}
		}
		pageToken = resp.Msg.GetNextPageToken()
		if pageToken == "" {
			// Prefer online agents when the job's identity is ambiguous.
			search := ids
			if len(online) == 1 {
				search = online
			} else if len(online) > 1 {
				// Multiple online agents for one job: pick the most
				// recently seen rather than failing discovery.
				best, bestSeen := "", ""
				for id := range online {
					if lastSeen[id] > bestSeen {
						best, bestSeen = id, lastSeen[id]
					}
				}
				if best != "" {
					return best, nil
				}
			}
			switch len(search) {
			case 1:
				for id := range search {
					return id, nil
				}
			case 0:
				// The engine registers the agent when the runner starts, so a
				// missing agent means it has not started (or castle lost it).
				// Either way the operator must not treat this as an
				// authoritative empty history. ErrRunNotFound lets the
				// controller tell this conclusive negative apart from a
				// transient failure: for a runner Job that is already
				// terminal the agent can never appear, so polling stops.
				return "", fmt.Errorf("%w: no castle agent registered for runner job %s yet", ErrRunNotFound, runnerJob)
			default:
				return "", fmt.Errorf("ambiguous castle agents for runner job %s: %d distinct criteria ids", runnerJob, len(ids))
			}
		}
	}
	return "", fmt.Errorf("agent discovery for runner job %s did not conclude within %d pages (more pages remain)", runnerJob, maxDiscoveryPages)
}

// findRunForCriteria resolves the run for a criteria id via paged ListRuns,
// filtered server-side by the request's criteria_id field (the vendored
// ListRunsRequest carries it; the agent table has no equivalent, so agent
// discovery above still pages the whole table). Runs are partitioned
// client-side by terminal status because the server-side status filter
// accepts a single value. More than one non-terminal run for the criteria id
// is ambiguous and surfaces as an error rather than a guess (matching the
// agent-discovery ambiguity rule): two concurrently running CriteriaRuns
// with the same criteria id must not bind each other's run. When every run
// for the criteria is terminal, the newest one is returned so the controller
// can still stamp completion. Discovery that exhausts the page budget with
// more pages remaining is inconclusive and surfaces as an error: silently
// reporting "no run" would make the controller converge against an empty
// history.
func (c *Client) findRunForCriteria(ctx context.Context, criteriaID string) (*v1.Run, error) {
	var newestActive, newestTerminal *v1.Run
	activeRuns := 0
	pageToken := ""
	for page := 0; page < maxDiscoveryPages; page++ {
		resp, err := c.runs.ListRuns(ctx, connect.NewRequest(&v1.ListRunsRequest{
			CriteriaId: criteriaID,
			Limit:      int32(defaultPageSize),
			PageToken:  pageToken,
		}))
		if err != nil {
			return nil, err
		}
		for _, run := range resp.Msg.GetRuns() {
			if isTerminalRunStatus(run.GetStatus()) {
				newestTerminal = newerRun(newestTerminal, run)
			} else {
				activeRuns++
				newestActive = newerRun(newestActive, run)
			}
		}
		pageToken = resp.Msg.GetNextPageToken()
		if pageToken == "" {
			if activeRuns > 1 {
				return nil, fmt.Errorf("ambiguous castle runs for criteria %s: %d active", criteriaID, activeRuns)
			}
			if newestActive != nil {
				return newestActive, nil
			}
			if newestTerminal != nil {
				return newestTerminal, nil
			}
			return nil, fmt.Errorf("%w: no castle run for criteria %s yet", ErrRunNotFound, criteriaID)
		}
	}
	return nil, fmt.Errorf("run discovery for criteria %s did not conclude within %d pages (more pages remain); refusing to report an empty observation", criteriaID, maxDiscoveryPages)
}

// drain incrementally fetches run events from the last consumed sequence and
// folds them into the accumulated per-run state. It returns the terminal
// event seen so far, the full accumulated lifecycle history, and the newest
// step-progress event timestamp consumed so far (KB-24).
func (c *Client) drain(ctx context.Context, runID string) (*Terminal, []events.LifecycleEvent, time.Time, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	since := c.cursors[runID]
	terminal := c.terminals[runID]
	lastProgress := c.progress[runID]

	for page := 0; page < maxEventPages; page++ {
		resp, err := c.runs.ListRunEvents(ctx, connect.NewRequest(&v1.ListRunEventsRequest{
			RunId:    runID,
			SinceSeq: since,
			Limit:    int32(defaultPageSize),
		}))
		if err != nil {
			return nil, nil, time.Time{}, err
		}
		for _, env := range resp.Msg.GetEvents() {
			if progressFromEnvelope(env) {
				if ts := env.GetTs(); ts != nil {
					if t := ts.AsTime(); t.After(lastProgress) {
						lastProgress = t
					}
				}
			}
			if t := terminalFromEnvelope(env); t != nil {
				terminal = t
				c.terminals[runID] = t
			}
			if ev, ok := lifecycleFromEnvelope(env); ok {
				ev = c.resolveReleaseAdapter(runID, ev)
				c.lifecycle[runID] = append(c.lifecycle[runID], ev)
				if ev.IsProvisionWanted() {
					c.recordScope(runID, ev)
				}
			}
			if env.GetSeq() > since {
				since = env.GetSeq()
			}
		}
		if next := resp.Msg.GetNextSinceSeq(); next > since {
			since = next
		}
		if len(resp.Msg.GetEvents()) < defaultPageSize {
			break
		}
	}
	c.cursors[runID] = since
	if lastProgress.After(c.progress[runID]) {
		c.progress[runID] = lastProgress
	}

	history := c.lifecycle[runID]
	out := make([]events.LifecycleEvent, len(history))
	copy(out, history)
	return terminal, out, lastProgress, nil
}

// progressFromEnvelope reports whether an envelope is a step-progress event
// for the stall watchdog (KB-24): the run made visible progress when any of
// these arrives. Heartbeats and terminal envelopes never count — a wedged
// step emits only heartbeats, which is exactly the signature the watchdog
// must detect (the CRI-271 incident's wedge chatter — "remote shim accept
// failed", "no persisted scope instance record" — is engine slog output that
// never enters the envelope stream). Terminal envelopes are likewise
// excluded: a terminal run needs no watchdog.
//
// AdapterEvents count only for a fixed activity allowlist
// (adapterEventActivityKind): the copilot adapter (pinned v0.5.8) drops log
// lines (WS15/WS03), so a healthy turn ships ALL of its agent activity as
// structured AdapterEvents — agent.message deltas, tool invocations/results,
// permission requests and the finalize receipt — and a step that emits none
// of those while its session is wedged produces nothing but heartbeats.
// Counting every AdapterEvent would let crash/respawn chatter and scope
// provisioning bookkeeping hold the watchdog open, so unknown or non-activity
// kinds default to excluded; extend the allowlist deliberately per adapter
// emission (see adapterEventActivityKind for the current evidence).
// Corroborating the window: on healthy copilot turns (CRI-277 notes) the
// worst observed gap between activity events is 217s and gate-held permission
// waits run ~10m — both far inside the 30m default stall window, so activity
// progress and step-timeout budgets (e.g. 60m copilot turns) cannot contradict.
func progressFromEnvelope(env *v1.Envelope) bool {
	switch env.GetPayload().(type) {
	case *v1.Envelope_RunStarted,
		*v1.Envelope_StepEntered,
		*v1.Envelope_StepOutcome,
		*v1.Envelope_StepTransition,
		*v1.Envelope_StepLog,
		*v1.Envelope_StepResumed,
		*v1.Envelope_StepOutputCaptured,
		*v1.Envelope_VariableSet,
		*v1.Envelope_WaitEntered,
		*v1.Envelope_WaitResumed,
		*v1.Envelope_ApprovalRequested,
		*v1.Envelope_ApprovalDecision,
		*v1.Envelope_BranchEvaluated,
		*v1.Envelope_ForEachEntered,
		*v1.Envelope_StepIterationStarted,
		*v1.Envelope_StepIterationItem,
		*v1.Envelope_StepIterationCompleted,
		*v1.Envelope_ScopeIterCursorSet,
		*v1.Envelope_WatchReady:
		return true
	case *v1.Envelope_AdapterEvent:
		return adapterEventActivityKind(env.GetAdapterEvent().GetKind())
	default:
		return false
	}
}

// adapterEventActivityKind reports whether an AdapterEvent kind is genuine
// agent activity that must refresh the stall watchdog's progress baseline
// (KB-24). The allowlist is the copilot adapter's emission surface (v0.5.8
// sources: copilot_turn.go, copilot_outcome.go, copilot_permission.go) plus
// the engine host's shared tool-call loader, and the kinds a wedge or a
// crash loop can produce are deliberately absent:
//
//   - agent.message / tool.invocation / tool.result / limit.reached:
//     the turn state machine's per-event traffic — continuous on healthy
//     turns (worst CRI-277 gap 217s), absent once the agent session is dead.
//   - permission.request: the model asking for permission; gate-held waits
//     run ~10m on healthy turns (CRI-277).
//   - outcome.finalized: the model's successful submit_outcome call.
//
// Excluded on purpose: adapter lifecycle kinds (scope provisioning
// bookkeeping, not step work), outcome.failure (the finalize-failed verdict,
// immediately followed by the step's own StepOutcome), session.crash and
// session.respawned (host crash handling — a respawn loop must not read as
// progress), permission.denied and step.outcome.unknown (malformed-payload
// and unknown-outcome bookkeeping), and every unknown kind (future chatter
// kinds must not hold the watchdog open by default).
func adapterEventActivityKind(kind string) bool {
	switch kind {
	case "agent.message",
		"tool.invocation",
		"tool.result",
		"permission.request",
		"limit.reached",
		"outcome.finalized":
		return true
	default:
		return false
	}
}

// resolveReleaseAdapter normalizes the engine's release-event adapter
// asymmetry: released AdapterEvents report the internal shim registration
// name (e.g. "noop.default") instead of the provisioned adapter kind
// ("default") for the same scope instance. Releases are matched by scope
// instance, so the release is keyed to the adapter actually provisioned for
// that scope. CRI-132 deliberately deferred this resolution; over the castle
// wire there is no flat-release fallback, so the boundary resolves it.
func (c *Client) resolveReleaseAdapter(runID string, ev events.LifecycleEvent) events.LifecycleEvent {
	if !ev.IsRelease() {
		return ev
	}
	if prov := c.scopes[runID][ev.ScopeID]; prov != "" && prov != ev.AdapterName {
		ev.AdapterName = prov
	}
	return ev
}

func (c *Client) recordScope(runID string, ev events.LifecycleEvent) {
	if c.scopes[runID] == nil {
		c.scopes[runID] = map[string]string{}
	}
	c.scopes[runID][ev.ScopeID] = ev.AdapterName
}

// newerRun returns the run with the newer CreatedAt.
func newerRun(a, b *v1.Run) *v1.Run {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	if b.GetCreatedAt().AsTime().After(a.GetCreatedAt().AsTime()) {
		return b
	}
	return a
}

func isTerminalRunStatus(status string) bool {
	switch status {
	case runStatusSucceeded, "failed", "cancelled":
		return true
	}
	return false
}

// terminalFromRun derives a Terminal from a castle Run record, or nil when
// the run is not terminal. The record contributes only the success/failure
// verdict (and pr_url, when a producer exists): castle run records carry no
// final_state column, so FinalState stays empty here and terminal envelopes
// are the only source for it.
func terminalFromRun(run *v1.Run) *Terminal {
	if run == nil || !isTerminalRunStatus(run.GetStatus()) {
		return nil
	}
	return &Terminal{
		Success:  run.GetStatus() == runStatusSucceeded,
		PRNumber: prNumberFromURL(run.GetPrUrl()),
		Reason:   run.GetFailureReason(),
	}
}

// prNumberFromURL extracts the trailing pull request number from a pr_url
// (e.g. https://github.com/o/r/pull/42 -> "42").
func prNumberFromURL(prURL string) string {
	if prURL == "" {
		return ""
	}
	idx := strings.LastIndexByte(prURL, '/')
	if idx < 0 || idx == len(prURL)-1 {
		return ""
	}
	num := prURL[idx+1:]
	for _, r := range num {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return num
}

// tokenTransport attaches the criteria bearer token to castle requests.
type tokenTransport struct {
	base  http.RoundTripper
	token string
}

func (t *tokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header.Set("X-Criteria-Token", t.token)
	clone.Header.Set("Authorization", "Bearer "+t.token)
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(clone)
}
