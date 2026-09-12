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
	"fmt"
	"net/http"
	"strings"
	"sync"

	connect "connectrpc.com/connect"

	v1 "github.com/brokenbots/workflow-example/criteria-k8s/internal/criteria/pb/criteria/v1"
	v1connect "github.com/brokenbots/workflow-example/criteria-k8s/internal/criteria/pb/criteria/v1/criteriav1connect"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/events"
)

const (
	// defaultPageSize bounds ListRunEvents/ListRuns pages.
	defaultPageSize = 200
	// maxEventPages bounds a single Observe drain so a pathological backlog
	// cannot pin a reconcile pass.
	maxEventPages = 50
	// maxDiscoveryPages bounds ticket-based run discovery.
	maxDiscoveryPages = 10

	// runStatusSucceeded is castle's terminal success status.
	runStatusSucceeded = "succeeded"
)

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
	// FinalState is the engine's final state for the run.
	FinalState string
	// PRNumber is the pull request number produced by the run, parsed from
	// the castle Run's pr_url.
	PRNumber string
	// Reason is the RunFailed reason, when the run failed.
	Reason string
	// TicketState is the engine-reported final ticket state.
	TicketState string
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
}

// RunSource is the controller-facing observation surface. *Client implements
// it; tests provide fakes.
type RunSource interface {
	// Observe drains castle for the run backing ticket. knownRunID
	// short-circuits run discovery once the controller has persisted the
	// castle run id.
	Observe(ctx context.Context, ticket, knownRunID string) (*Observation, error)

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
	}
}

// Disabled reports whether castle observation is configured off (empty addr).
func (c *Client) Disabled() bool {
	return c == nil || c.addr == ""
}

// Observe drains castle events for the run backing the given ticket and
// returns the accumulated observation. It distinguishes "unavailable or not
// yet known" (error) from an authoritative observation: a castle outage, a
// ticket with no registered run yet, an empty ticket, or discovery that
// cannot conclude within the page budget all return an error so the caller
// aborts the reconcile pass instead of converging desired state against an
// empty history (which would delete live adapter pods). The caller retries
// on the next reconcile interval.
func (c *Client) Observe(ctx context.Context, ticket, knownRunID string) (*Observation, error) {
	obs := &Observation{}
	if c.Disabled() {
		return obs, nil
	}
	if ticket == "" {
		return nil, fmt.Errorf("castle observation requires a ticket: run discovery is ticket-keyed")
	}

	runID := knownRunID
	if runID == "" {
		discovered, err := c.findRunByTicket(ctx, ticket)
		if err != nil {
			return nil, fmt.Errorf("discovering castle run for ticket %s: %w", ticket, err)
		}
		if discovered == nil {
			// The engine registers runs at CreateRun, so a missing run means
			// it has not started (or castle lost it). Either way the operator
			// must not treat this as an authoritative empty history.
			return nil, fmt.Errorf("no castle run registered for ticket %s yet", ticket)
		}
		runID = discovered.GetRunId()
	}
	obs.RunID = runID

	terminal, lifecycle, err := c.drain(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("draining castle events for run %s: %w", runID, err)
	}
	obs.Lifecycle = lifecycle
	obs.Terminal = terminal

	// The run record is authoritative for pr_url/final_state: terminal
	// envelopes carry neither, so enrich from GetRun (and fall back to the
	// record for runs that reached a terminal status without a terminal
	// envelope, e.g. cancellation).
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
		if obs.Terminal.FinalState == "" {
			obs.Terminal.FinalState = resp.Msg.GetFinalState()
		}
		if obs.Terminal.TicketState == "" {
			obs.Terminal.TicketState = resp.Msg.GetFinalState()
		}
	}

	// The controller stamps the CR terminal from this observation and stops
	// reconciling, so the per-run caches are no longer needed. Evict them so
	// a long-lived operator does not accumulate state for every finished
	// run. A later observation for the same run re-drains from seq 0 and
	// reconstructs terminal state from the stream or the run record.
	if obs.Terminal != nil {
		c.evictRun(runID)
	}
	return obs, nil
}

// evictRun drops all per-run client state. The caller must have already
// built the observation for the terminal run.
func (c *Client) evictRun(runID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.cursors, runID)
	delete(c.scopes, runID)
	delete(c.lifecycle, runID)
	delete(c.terminals, runID)
}

// findRunByTicket resolves the castle run backing a ticket via paged
// ListRuns (which has no ticket filter). Non-terminal runs are preferred;
// when every run for the ticket is terminal, the newest one is returned so
// the controller can still stamp completion. Discovery that exhausts the
// page budget with more pages remaining is inconclusive and surfaces as an
// error: silently reporting "no run" would make the controller converge
// against an empty history.
func (c *Client) findRunByTicket(ctx context.Context, ticket string) (*v1.Run, error) {
	if ticket == "" {
		return nil, nil
	}
	var newestActive, newestTerminal *v1.Run
	pageToken := ""
	for page := 0; page < maxDiscoveryPages; page++ {
		resp, err := c.runs.ListRuns(ctx, connect.NewRequest(&v1.ListRunsRequest{
			Limit:     defaultPageSize,
			PageToken: pageToken,
		}))
		if err != nil {
			return nil, err
		}
		for _, run := range resp.Msg.GetRuns() {
			if run.GetTicket() != ticket {
				continue
			}
			if isTerminalRunStatus(run.GetStatus()) {
				newestTerminal = newerRun(newestTerminal, run)
			} else {
				newestActive = newerRun(newestActive, run)
			}
		}
		pageToken = resp.Msg.GetNextPageToken()
		if pageToken == "" {
			if newestActive != nil {
				return newestActive, nil
			}
			return newestTerminal, nil
		}
	}
	return nil, fmt.Errorf("run discovery for ticket %s did not conclude within %d pages (more pages remain); refusing to report an empty observation", ticket, maxDiscoveryPages)
}

// drain incrementally fetches run events from the last consumed sequence and
// folds them into the accumulated per-run state. It returns the terminal
// event seen so far and the full accumulated lifecycle history.
func (c *Client) drain(ctx context.Context, runID string) (*Terminal, []events.LifecycleEvent, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	since := c.cursors[runID]
	terminal := c.terminals[runID]

	for page := 0; page < maxEventPages; page++ {
		resp, err := c.runs.ListRunEvents(ctx, connect.NewRequest(&v1.ListRunEventsRequest{
			RunId:    runID,
			SinceSeq: since,
			Limit:    defaultPageSize,
		}))
		if err != nil {
			return nil, nil, err
		}
		for _, env := range resp.Msg.GetEvents() {
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

	history := c.lifecycle[runID]
	out := make([]events.LifecycleEvent, len(history))
	copy(out, history)
	return terminal, out, nil
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
// the run is not terminal.
func terminalFromRun(run *v1.Run) *Terminal {
	if run == nil || !isTerminalRunStatus(run.GetStatus()) {
		return nil
	}
	return &Terminal{
		Success:     run.GetStatus() == runStatusSucceeded,
		FinalState:  run.GetFinalState(),
		PRNumber:    prNumberFromURL(run.GetPrUrl()),
		Reason:      run.GetFailureReason(),
		TicketState: run.GetFinalState(),
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