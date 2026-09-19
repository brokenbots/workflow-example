package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/brokenbots/workflow-example/criteria-k8s/internal/linear"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/routes"
	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
)

var (
	namespace           = flag.String("namespace", getenv("NAMESPACE", "criteria-jobs"), "Namespace to watch and create CriteriaRuns in")
	linearAPIKey        = flag.String("linear-api-key", getenv("LINEAR_API_KEY", ""), "Linear API key (also read from /secrets/linear_api_key)")
	projectName         = flag.String("linear-project-name", getenv("LINEAR_PROJECT_NAME", "Criteria K8s Workflow Runner"), "Linear project to watch")
	triggerLabel        = flag.String("linear-trigger-label", getenv("LINEAR_TRIGGER_LABEL", ""), "Require this label on the issue before triggering a run; empty means any issue matching a route triggers")
	pollInterval        = flag.Duration("poll-interval", parseDuration(getenv("POLL_INTERVAL", "5m")), "How often to poll Linear")
	image               = flag.String("image", getenv("CRITERIA_IMAGE", "localhost:5000/linear-intake-remote:dev"), "Default Criteria workflow image")
	providerBaseURL     = flag.String("provider-base-url", getenv("PROVIDER_BASE_URL", "http://192.168.17.116:11434/v1"), "Default provider base URL")
	maxAgentVisits      = flag.Int("max-agent-visits", parseInt(getenv("MAX_AGENT_VISITS", "2"), 2), "Default max agent visits")
	buildCmd            = flag.String("build-cmd", getenv("BUILD_CMD", ""), "Default build command")
	testCmd             = flag.String("test-cmd", getenv("TEST_CMD", ""), "Default test command")
	ciGateCmd           = flag.String("ci-gate-cmd", getenv("CI_GATE_CMD", ""), "Default CI gate command")
	defaultRepoURL      = flag.String("default-repo-url", getenv("DEFAULT_REPO_URL", ""), "Default repo URL when Linear issue does not contain one")
	routesFile          = flag.String("routes-file", getenv("CRITERIA_ROUTES_FILE", routes.DefaultFile), "Routes payload file (mounted from the criteria-routes ConfigMap); re-read every poll")
	workflowsLabelGroup = flag.String("linear-workflows-label-group", getenv("LINEAR_WORKFLOWS_LABEL_GROUP", "workflows"), "Linear label group whose labels name a workflow overriding the route's project default")
)

func main() {
	flag.Parse()
	logger := zap.New()
	log.SetLogger(logger)

	apiKey := *linearAPIKey
	if apiKey == "" {
		b, err := os.ReadFile("/secrets/linear_api_key")
		if err == nil {
			apiKey = strings.TrimSpace(string(b))
		}
	}
	if apiKey == "" {
		logger.Error(fmt.Errorf("missing Linear API key"), "linear-api-key or /secrets/linear_api_key is required")
		os.Exit(1)
	}

	cfg, err := config.GetConfig()
	if err != nil {
		logger.Error(err, "loading kubeconfig")
		os.Exit(1)
	}

	scheme := runtime.NewScheme()
	if err := criteriav1.AddToScheme(scheme); err != nil {
		logger.Error(err, "registering CriteriaRun scheme")
		os.Exit(1)
	}

	k8s, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		logger.Error(err, "creating Kubernetes client")
		os.Exit(1)
	}

	linearClient := linear.NewClient(apiKey)
	githubToken := getenv("WORKFLOW_GITHUB_TOKEN", "")
	if githubToken == "" {
		if b, err := os.ReadFile("/secrets/workflow_github_token"); err == nil {
			githubToken = strings.TrimSpace(string(b))
		}
	}
	w := watcher{
		client:              k8s,
		linear:              linearClient,
		namespace:           *namespace,
		projectName:         *projectName,
		triggerLabel:        *triggerLabel,
		pollInterval:        *pollInterval,
		image:               *image,
		providerBaseURL:     *providerBaseURL,
		maxAgentVisits:      *maxAgentVisits,
		buildCmd:            *buildCmd,
		testCmd:             *testCmd,
		ciGateCmd:           *ciGateCmd,
		defaultRepoURL:      *defaultRepoURL,
		routesFile:          *routesFile,
		workflowsLabelGroup: *workflowsLabelGroup,
		repoValidator:       linear.DefaultRepoValidator(nil, githubToken, ""),
		log:                 logger,
	}

	ctx := ctrl.SetupSignalHandler()
	if err := w.run(ctx); err != nil {
		logger.Error(err, "watcher exited")
		os.Exit(1)
	}
}

type watcher struct {
	client              client.Client
	linear              *linear.Client
	namespace           string
	projectName         string
	triggerLabel        string
	pollInterval        time.Duration
	image               string
	providerBaseURL     string
	maxAgentVisits      int
	buildCmd            string
	testCmd             string
	ciGateCmd           string
	defaultRepoURL      string
	routesFile          string
	workflowsLabelGroup string
	repoValidator       linear.RepoValidator
	// teamID is the Linear team the watched project belongs to; the
	// automation labels are created in this team (CRI-219). Resolved once
	// in run before the poll loop starts.
	teamID string
	// CRI-252: the automation label IDs are resolved once at startup and
	// cached; ensureAutomationLabels re-resolves them only on a cache miss
	// (an empty cached ID, left behind when an earlier ensure failed).
	automationLabelID string
	dirtyLabelID      string
	// lastPhases records, per ticket, the latest CriteriaRun phase seen at
	// the previous poll, so a ticket reconciles only when that phase
	// changed (CRI-252). k8s-side only: never written to Linear.
	lastPhases map[string]criteriav1.CriteriaRunPhase
	// backoffAttempt counts consecutive rate-limited polls (CRI-252).
	backoffAttempt int
	log            logr.Logger
}

func (w *watcher) run(ctx context.Context) error {
	w.log.Info("resolving Linear project", "project", w.projectName)
	projectID, err := w.linear.FindProjectID(ctx, w.projectName)
	if err != nil {
		return fmt.Errorf("resolve project: %w", err)
	}
	// CRI-219: the automation labels are team-scoped, so the watcher needs
	// the project's team before it can create or resolve them.
	// CRI-252: resolved once here and cached; a later cache miss re-resolves
	// on the next poll instead of every poll.
	teamID, err := w.resolveTeamID(ctx, projectID)
	if err != nil {
		return fmt.Errorf("resolve project team: %w", err)
	}
	w.teamID = teamID
	// CRI-252: warm the label-id cache once at startup so the first poll is
	// already a good consumer. Linear being rate limited (or briefly down)
	// must not kill the watcher: the first poll re-ensures on the cache
	// miss.
	if err := w.ensureAutomationLabels(ctx); err != nil {
		w.log.Info("warming the Linear label cache failed; label lookups deferred to the first poll", "error", err)
	}
	w.log.Info("watching Linear project", "project", w.projectName, "projectID", projectID, "teamID", teamID, "interval", w.pollInterval)

	// CRI-252: rate limits back off the poll interval (up to backoffCap,
	// and at least Linear's rateLimitResult.duration) and a successful poll
	// restores it. A skipped poll costs no requests, so a rate-limited
	// watcher stops amplifying the exhaustion it is reacting to. The
	// recomputed period is applied to the ticker BEFORE the wait that
	// follows the poll, so the next Linear request honors the new interval
	// — waiting out Linear's advertised duration, not one tick of the old
	// cadence.
	interval := w.pollInterval
	tickerPeriod := interval
	ticker := time.NewTicker(interval)
	defer func() { ticker.Stop() }()
	for {
		if err := w.poll(ctx, projectID); err != nil {
			var rl *linear.RateLimitError
			if errors.As(err, &rl) {
				interval = w.nextBackoffInterval(interval, rl)
			} else {
				w.log.Error(err, "poll failed")
			}
		} else {
			if w.backoffAttempt > 0 {
				w.backoffAttempt = 0
				w.log.Info("linear rate limit cleared; poll interval restored", "interval", w.pollInterval)
			}
			interval = w.pollInterval
		}
		if interval != tickerPeriod {
			// A fresh ticker drains any stale tick buffered during the
			// poll, so the next wait is the full recomputed interval.
			ticker.Stop()
			ticker = time.NewTicker(interval)
			tickerPeriod = interval
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// resolveTeamID returns the cached project team id, re-resolving only on a
// cache miss (CRI-252).
func (w *watcher) resolveTeamID(ctx context.Context, projectID string) (string, error) {
	if w.teamID != "" {
		return w.teamID, nil
	}
	return w.linear.FindProjectTeamID(ctx, projectID)
}

// nextBackoffInterval doubles the current poll interval after a rate limit
// (CRI-252), capped at backoffCap. Linear's own rateLimitResult.duration
// (or Retry-After) wins over both, so the watcher waits exactly as long as
// Linear demands. Each consecutive rate-limited poll records one back-off
// attempt; the first attempt logs the single rate-limit event for the
// back-off window.
func (w *watcher) nextBackoffInterval(current time.Duration, rl *linear.RateLimitError) time.Duration {
	w.backoffAttempt++
	next := current * 2
	if next > backoffCap {
		next = backoffCap
	}
	if rl != nil && rl.Duration > next {
		next = rl.Duration
	}
	if w.backoffAttempt == 1 {
		w.log.Info("linear rate limited; skipping poll and backing off",
			"retryAfter", rl.Duration, "nextInterval", next)
	} else {
		w.log.V(1).Info("linear rate limited; poll skipped, back-off continues",
			"attempt", w.backoffAttempt, "retryAfter", rl.Duration, "nextInterval", next)
	}
	return next
}

func (w *watcher) poll(ctx context.Context, projectID string) error {
	// CRI-217: the routes payload is the configuration of record for which
	// tickets run. It is re-read every poll from the mounted ConfigMap, so
	// route changes never depend on a watcher restart. A broken or absent
	// payload fails closed: skip the whole poll and retry next cycle.
	routesPayload, err := routes.LoadFile(w.routesFile)
	if err != nil {
		w.log.Error(err, "loading routes payload; failing closed and skipping poll", "routesFile", w.routesFile)
		return nil
	}
	// CRI-218: the routes payload is also the configuration of record for
	// which workflow states trigger runs. The watcher queries the union of
	// all routes' declared states; routes omitting states default to
	// [Triage]. An empty union (no routes) queries nothing: fail closed.
	states := routesPayload.TicketStates()
	// CRI-252: resolve the automation label ids once (startup-cached); a
	// cache miss (an ID an earlier ensure failed to resolve) re-ensures
	// here, and a rate limit aborts the poll before any further Linear
	// traffic.
	if err := w.ensureAutomationLabels(ctx); err != nil {
		return err
	}
	automationLabelID, dirtyLabelID := w.automationLabelID, w.dirtyLabelID
	w.log.V(1).Info("polling Linear for ticket states declared by routes", "states", states)
	issues, err := w.linear.IssuesInProjectStates(ctx, projectID, states)
	if err != nil {
		return err
	}
	// CRI-219: one CriteriaRun list per poll backs both the run-firing gate
	// below and the label lifecycle (no per-issue run list).
	runs, err := w.indexRunPhases(ctx)
	if err != nil {
		return err
	}
	// CRI-219: label reconciliation is driven by the CriteriaRun list — not
	// by the route-declared state list — because the intake workflow moves
	// tickets out of the watched states (In Progress / Done / In Review)
	// while and after their runs execute, so a run often settles while its
	// ticket is outside the run-firing scope.
	//
	// CRI-252: reconciliation is change-driven and batched. The per-ticket
	// last phase is recorded in the watcher's poll state (k8s-side only, no
	// Linear-side state), so a ticket reconciles only when its latest CR
	// phase differs from the phase recorded at the previous poll — a
	// steady-state no-change poll resolves no tickets and writes no labels.
	// Map presence (not the zero value) is the seen-check: a freshly created
	// run's empty phase reconciles on first sight like any other phase
	// change. All changed tickets resolve with one batched query per poll
	// (N+1 → 1), and iteration is ordered for deterministic logs and write
	// order.
	// CRI-252: label writes earlier in this poll (change-driven
	// reconciliation, the orphan sweep) replace the poll's listing read for
	// the tickets they touch, and Linear labelIds have REPLACE semantics —
	// so a later REPLACE write in this poll must build on the freshest
	// per-ticket label set this poll produced, not on the stale listing
	// read.
	currentLabelIDs := make(map[string][]string)
	tickets := make([]string, 0, len(runs))
	for ticket, ph := range runs {
		if prev, seen := w.lastPhases[ticket]; seen && prev == ph.latestPhase {
			continue
		}
		tickets = append(tickets, ticket)
	}
	sort.Strings(tickets)
	if len(tickets) > 0 {
		resolved, err := w.linear.IssuesByIdentifiers(ctx, tickets)
		if err != nil {
			if linear.IsRateLimit(err) {
				return err
			}
			w.log.Error(err, "resolving tickets for label reconciliation; deferred to next poll")
			resolved = nil
		}
		byIdentifier := make(map[string]linear.Issue, len(resolved))
		for _, issue := range resolved {
			byIdentifier[issue.Identifier] = issue
		}
		for _, ticket := range tickets {
			issue, ok := byIdentifier[ticket]
			if !ok {
				w.log.V(1).Info("ticket missing from batched resolution; label reconciliation deferred to next poll", "ticket", ticket)
				continue
			}
			if err := w.reconcileAutomationLabels(ctx, issue, runs[ticket], automationLabelID, dirtyLabelID, currentLabelIDs); err != nil {
				if linear.IsRateLimit(err) {
					return err
				}
				if errors.Is(err, errLabelWriteDeferred) {
					// CRI-252: a needed automation-label id was
					// unresolved, so the transition was not written; the
					// phase stays unrecorded, and the next poll re-ensures
					// the id and retries the transition.
					w.log.V(1).Info(err.Error(), "ticket", ticket)
					continue
				}
				// Already logged; the phase stays unrecorded, so the next
				// poll retries the transition.
				continue
			}
			if w.lastPhases == nil {
				w.lastPhases = make(map[string]criteriav1.CriteriaRunPhase)
			}
			w.lastPhases[ticket] = runs[ticket].latestPhase
		}
	}
	// CRI-252: drop the recorded phase of tickets with no observed runs
	// anymore (the run was deleted), so a future run for the same ticket
	// reconciles from scratch.
	for ticket := range w.lastPhases {
		if _, ok := runs[ticket]; !ok {
			delete(w.lastPhases, ticket)
		}
	}
	// CRI-220: reconciliation above only reaches tickets with observed
	// CriteriaRuns. The orphan sweep closes the remaining gap: a ticket can
	// carry the inflight marker with no CriteriaRun behind it at all (the
	// run was deleted manually while running, or the label was orphaned by
	// a watcher restart), leaving it invisible to the runs-driven
	// reconciliation. Like the operator's stale-adapter sweep, it runs
	// deterministically on every poll.
	if err := w.sweepOrphanedAutomationLabels(ctx, projectID, runs, automationLabelID, dirtyLabelID, currentLabelIDs); err != nil {
		if linear.IsRateLimit(err) {
			return err
		}
		w.log.Error(err, "orphan sweep failed; deferred to next poll")
	}
	for _, issue := range issues {
		// CRI-147 gating: when a trigger label is configured, only issues
		// carrying it fire a k8s run (this gate doubles as the k8s-run
		// arming gate in deployments that configure it). Other tickets in a
		// watched state stay untouched so teams can exercise their
		// workflows locally without consuming cluster compute. This global
		// gate is unchanged by CRI-217 and CRI-218.
		if w.triggerLabel != "" && !slices.Contains(issue.Labels, w.triggerLabel) {
			w.log.V(1).Info("issue in a watched state without trigger label; not firing a k8s run",
				"ticket", issue.Identifier, "triggerLabel", w.triggerLabel)
			continue
		}
		// CRI-219: the ticket's observed runs gate firing (any run in flight
		// blocks a duplicate); tickets with no observed runs get the zero
		// value, exactly like the per-issue list the gate replaced.
		ph := runs[issue.Identifier]
		// CRI-217/218 route lookup: the ticket's project must exist in the
		// routes map, its state must be in the matched route's states list
		// (omitted states default to [Triage]), and the route's tag subset
		// must be satisfied; the matched route supplies the project
		// default workflow; a workflows-label-group label overrides it; a
		// missing workflow fails closed with a Linear comment.
		sel, err := routesPayload.Resolve(routes.Selector{
			Project:             issue.ProjectName,
			State:               issue.StateName,
			Labels:              issue.Labels,
			LabelGroups:         issue.LabelGroups,
			WorkflowsLabelGroup: w.workflowsLabelGroup,
		})
		if err != nil {
			if errors.Is(err, routes.ErrNoRoute) {
				w.log.Info("skipping ticket: no route in the routes map matches this project, state, and tags",
					"ticket", issue.Identifier, "project", issue.ProjectName, "state", issue.StateName)
				continue
			}
			// Unknown or ambiguous workflow (or any future lookup
			// failure): no run, watcher log, and a Linear comment on the
			// ticket (deduped).
			w.log.Error(err, "route lookup failed closed; not creating CriteriaRun",
				"ticket", issue.Identifier, "project", issue.ProjectName)
			if cErr := w.postRoutingComment(ctx, issue, err); cErr != nil && linear.IsRateLimit(cErr) {
				return cErr
			}
			continue
		}
		repoURL := linear.ExtractRepoURL(issue, w.defaultRepoURL, w.repoValidator)
		if repoURL == "" {
			w.log.Info("skipping Linear issue without repo URL", "ticket", issue.Identifier, "title", issue.Title)
			continue
		}
		if ph.active {
			w.log.V(1).Info("run already active", "ticket", issue.Identifier)
			continue
		}
		// CRI-221 single-active invariant (skip side): the automation label
		// gates firing even when no run matched the watcher's source
		// selector — runs created outside the selector convention (e.g.
		// legacy runs carrying a different label key) are invisible to the
		// runs index, and the label is the only other witness the invariant
		// has. A label orphaned by a deleted run was already converged to
		// the dirty label by the sweep above, so a label still present here
		// backs a live workflow.
		if slices.Contains(issue.Labels, automationLabelName) {
			w.log.Info("skipping CriteriaRun creation: ticket carries the automation label without a matching live run (single-active invariant)",
				"ticket", issue.Identifier)
			continue
		}
		run := w.buildCriteriaRun(issue, repoURL, sel)
		if err := w.client.Create(ctx, run); err != nil {
			w.log.Error(err, "creating CriteriaRun", "ticket", issue.Identifier)
			continue
		}
		w.log.Info("created CriteriaRun", "ticket", issue.Identifier, "name", run.Name, "repoUrl", repoURL,
			"workflow", sel.Name, "route", sel.Route.Name)
		// CRI-219: mark the ticket as having an inflight CriteriaRun.
		// CRI-252: the write still needs no pre-write label read, but its
		// REPLACE payload must build on the ticket's current label ids —
		// reconciliation or the orphan sweep may have written labels for
		// this ticket earlier in this same poll, and a set computed from
		// the poll's listing read would silently drop them.
		if w.writeReady(automationLabelID, automationLabelName, issue.Identifier, "add") {
			ids := issue.LabelIDs
			if fresh, ok := currentLabelIDs[issue.Identifier]; ok {
				ids = fresh
			}
			if err := w.setIssueLabels(ctx, issue, append(slices.Clone(ids), automationLabelID)); err != nil && linear.IsRateLimit(err) {
				return err
			}
		}
	}
	return nil
}

// runPhases captures what the watcher needs to know about a ticket's
// CriteriaRuns: whether any run is still in flight (the run-firing gate;
// extended to count the empty phase a freshly created run carries until the
// controller sets status as in flight) and the phase of the ticket's most
// recent run (the label-reconciliation authority). The zero value means no
// run was observed.
type runPhases struct {
	active      bool
	latestPhase criteriav1.CriteriaRunPhase
	anyRun      bool
}

// indexRunPhases lists every CriteriaRun this watcher created in the
// namespace once per poll and groups them by ticket. Reconciliation is
// driven by this list — not by the route-declared state list — because the
// intake workflow moves tickets out of the watched states while and after
// their runs execute, so a run often settles while its ticket is outside
// the run-firing scope (CRI-219). Runs created by other components (no
// linear source label) or without a ticket id are ignored.
func (w *watcher) indexRunPhases(ctx context.Context) (map[string]runPhases, error) {
	list := &criteriav1.CriteriaRunList{}
	req := client.ListOptions{
		Namespace:     w.namespace,
		LabelSelector: mustSelector(map[string]string{"criteria.brokenbots.dev/source": "linear"}),
	}
	if err := w.client.List(ctx, list, &req); err != nil {
		return nil, err
	}
	type ticketRuns struct {
		active bool
		latest criteriav1.CriteriaRun
	}
	groups := make(map[string]*ticketRuns)
	for i := range list.Items {
		run := &list.Items[i]
		ticket := run.Spec.TicketID
		if ticket == "" {
			w.log.V(1).Info("skipping CriteriaRun without a ticket id", "run", run.Name)
			continue
		}
		g := groups[ticket]
		if g == nil {
			g = &ticketRuns{}
			groups[ticket] = g
		}
		switch run.Status.Phase {
		case criteriav1.PhaseFailed, criteriav1.PhaseSucceeded:
			// Settled: nothing in flight from this run.
		default:
			// Pending, Running, Unknown — and the empty phase a freshly
			// created run carries until the controller sets status — are
			// all in flight, so the watcher neither re-fires nor strips
			// the inflight marker in that gap (CRI-219).
			g.active = true
		}
		if g.latest.Name == "" || runNewer(run, &g.latest) {
			g.latest = *run
		}
	}
	index := make(map[string]runPhases, len(groups))
	for ticket, g := range groups {
		index[ticket] = runPhases{active: g.active, latestPhase: g.latest.Status.Phase, anyRun: true}
	}
	return index, nil
}

// runNewer reports whether run sorts after cur (creation time, with the
// name as a deterministic tiebreak), used to pick a ticket's most recent
// CriteriaRun.
func runNewer(run, cur *criteriav1.CriteriaRun) bool {
	if !run.CreationTimestamp.Equal(&cur.CreationTimestamp) {
		return cur.CreationTimestamp.Before(&run.CreationTimestamp)
	}
	return run.Name > cur.Name
}

// ensureAutomationLabels resolves (or creates) the team-scoped automation
// labels and caches their Linear IDs in the watcher (CRI-252): re-ensuring
// on every poll re-issued two constant label lookups per poll, which the
// shared rate budget cannot afford. The ensure only re-runs on a cache
// miss, so the cache is the single source for the IDs; a label deleted
// out-of-band is not detected until restart (an operator action, out of the
// watcher's steady-state scope), and issueLabelCreate only fires when the
// name is missing from the team.
// Empty IDs mean ensure failed for that label; label writes needing it are
// then skipped (with a watcher log), and retry is per caller: change-driven
// reconciliation defers the whole transition (errLabelWriteDeferred) and
// leaves the ticket's recorded phase unchanged, so the next poll re-ensures
// and retries it, while the firing and orphan-sweep paths run on every poll
// and retry naturally. Either way a transient Linear hiccup never blocks
// run creation. A rate limit aborts the ensure and is returned so the poll
// skips instead of pressing on.
func (w *watcher) ensureAutomationLabels(ctx context.Context) error {
	if w.automationLabelID == "" {
		id, err := w.linear.EnsureTeamLabel(ctx, w.teamID, automationLabelName, automationLabelColor)
		if err != nil {
			if linear.IsRateLimit(err) {
				return err
			}
			w.log.Error(err, "ensuring automation label; label writes needing it are skipped until it resolves", "label", automationLabelName, "teamID", w.teamID)
		} else {
			w.automationLabelID = id
			w.log.V(1).Info("resolved automation label", "label", automationLabelName, "labelID", id)
		}
	}
	if w.dirtyLabelID == "" {
		id, err := w.linear.EnsureTeamLabel(ctx, w.teamID, dirtyLabelName, dirtyLabelColor)
		if err != nil {
			if linear.IsRateLimit(err) {
				return err
			}
			w.log.Error(err, "ensuring dirty label; label writes needing it are skipped until it resolves", "label", dirtyLabelName, "teamID", w.teamID)
		} else {
			w.dirtyLabelID = id
			w.log.V(1).Info("resolved dirty label", "label", dirtyLabelName, "labelID", id)
		}
	}
	return nil
}

// errLabelWriteDeferred reports that a label transition was deferred
// because a needed automation-label ID was unresolved (the ensure failed
// this poll); the wrapped message names the blocked label. The poll-loop
// caller must leave the ticket's recorded phase unchanged so the next poll
// re-ensures the ID and retries the transition — recording a deferred
// transition as reconciled would drop it forever under change-driven
// reconciliation (CRI-252).
var errLabelWriteDeferred = errors.New("label transition deferred")

// reconcileAutomationLabels converges an issue's automation-label state with
// the phase of the ticket's most recent CriteriaRun (the reconciliation
// authority — an in-flight successor of a failed run governs while it runs):
//   - latest run in flight → "criteria-automation" present,
//   - latest run Succeeded → "criteria-automation" absent, "criteria-dirty"
//     untouched,
//   - latest run Failed    → "criteria-automation" absent and
//     "criteria-dirty" present.
//
// "criteria-dirty" is sticky: the watcher never removes it, because a failed
// run needs human attention. Reconciliation runs only when the ticket's
// latest CR phase changed since the previous poll (CRI-252), so an operator
// clearing the dirty marker on an unchanged phase is no longer re-added
// every poll; the next phase change re-evaluates the label state from the
// issue's current labels.
//
// The issue's labels come from the poll's batched read (CRI-252), which
// supplies both label names and label IDs, so the desired set is computed
// without an extra read and applied with at most one REPLACE write; a
// converged issue is left untouched. When a needed label ID is unresolved
// (the ensure failed this poll), the whole transition is deferred without
// a write and reported as errLabelWriteDeferred — the caller must leave
// the ticket's recorded phase unchanged, so the next poll re-ensures the
// ID and retries it; a partially applied transition recorded as reconciled
// would never be retried under change-driven reconciliation. A successful
// transition (write performed or already converged) records the resulting
// label set in currentLabelIDs, so later REPLACE writes in the same poll
// (run firing) build on the ticket's current label state. Tickets with no
// observed CriteriaRuns are left untouched here — the orphan sweep owns
// that case (CRI-220): a ticket can carry the inflight marker with no run
// behind it at all, and the sweep converges it to the dirty marker.
func (w *watcher) reconcileAutomationLabels(ctx context.Context, issue linear.Issue, ph runPhases, automationLabelID, dirtyLabelID string, currentLabelIDs map[string][]string) error {
	if !ph.anyRun {
		return nil
	}
	hasAutomation := slices.Contains(issue.Labels, automationLabelName)
	hasDirty := slices.Contains(issue.Labels, dirtyLabelName)
	desired := slices.Clone(issue.LabelIDs)
	var unresolved []string
	switch ph.latestPhase {
	case criteriav1.PhaseFailed:
		// Latest run failed: no inflight marker, dirty raised.
		if hasAutomation {
			if w.writeReady(automationLabelID, automationLabelName, issue.Identifier, "remove") {
				desired = deleteLabelID(desired, automationLabelID)
			} else {
				unresolved = append(unresolved, automationLabelName)
			}
		}
		if !hasDirty {
			if w.writeReady(dirtyLabelID, dirtyLabelName, issue.Identifier, "add") {
				if !slices.Contains(desired, dirtyLabelID) {
					desired = append(desired, dirtyLabelID)
				}
			} else {
				unresolved = append(unresolved, dirtyLabelName)
			}
		}
	case criteriav1.PhaseSucceeded:
		// Latest run succeeded: the issue is clean; the dirty label is
		// sticky and only an operator (or run deletion) clears it.
		if hasAutomation {
			if w.writeReady(automationLabelID, automationLabelName, issue.Identifier, "remove") {
				desired = deleteLabelID(desired, automationLabelID)
			} else {
				unresolved = append(unresolved, automationLabelName)
			}
		}
	default:
		// Pending, Running, Unknown, or the empty phase a freshly created
		// run carries until the controller sets status: in flight.
		if !hasAutomation {
			if w.writeReady(automationLabelID, automationLabelName, issue.Identifier, "add") {
				if !slices.Contains(desired, automationLabelID) {
					desired = append(desired, automationLabelID)
				}
			} else {
				unresolved = append(unresolved, automationLabelName)
			}
		}
	}
	// No write on a deferral: the transition stays atomic and unrecorded,
	// so the next poll retries it as a unit.
	if len(unresolved) > 0 {
		return fmt.Errorf("%w: %s id unresolved", errLabelWriteDeferred, strings.Join(unresolved, ", "))
	}
	if err := w.setIssueLabels(ctx, issue, desired); err != nil {
		return err
	}
	// CRI-252: the ticket's label state is now `desired`; later REPLACE
	// writes in this poll (run firing) must build on it.
	currentLabelIDs[issue.Identifier] = desired
	return nil
}

// writeReady reports whether a label write can proceed: the label's ID must
// be resolved (CRI-252 resolves IDs once and caches them). A false result
// only skips the write for this poll; whether the skipped transition is
// retried is the caller's contract — change-driven reconciliation defers
// via errLabelWriteDeferred and leaves the recorded phase unchanged, so the
// next poll re-ensures the ID and retries, while the firing and orphan-sweep
// paths run on every poll and retry naturally.
func (w *watcher) writeReady(labelID, name, ticket, action string) bool {
	if labelID != "" {
		return true
	}
	w.log.V(1).Info("skipping label write: label id unresolved (ensure failed this poll)",
		"ticket", ticket, "label", name, "action", action)
	return false
}

// setIssueLabels applies the desired label set to the issue when it differs
// from the set read in the same poll (CRI-252): no read precedes the write,
// and a converged issue is left untouched. Linear labelIds have REPLACE
// semantics (CRI-219), so the caller passes the full desired set.
func (w *watcher) setIssueLabels(ctx context.Context, issue linear.Issue, desired []string) error {
	if slices.Equal(desired, issue.LabelIDs) {
		return nil
	}
	if err := w.linear.SetIssueLabelIDs(ctx, issue.ID, desired); err != nil {
		w.log.Error(err, "writing Linear labels; retrying next poll", "ticket", issue.Identifier, "labelIDs", desired)
		return err
	}
	w.log.Info("wrote Linear labels", "ticket", issue.Identifier, "labelIDs", desired)
	return nil
}

// deleteLabelID returns ids without the given label ID, leaving the input
// slice untouched.
func deleteLabelID(ids []string, id string) []string {
	idx := slices.Index(ids, id)
	if idx < 0 {
		return ids
	}
	return slices.Delete(slices.Clone(ids), idx, idx+1)
}

// sweepOrphanedAutomationLabels implements the CRI-220 orphan rule: a
// ticket bearing "criteria-automation" with no live CriteriaRun in any
// phase (including Pending) and no recorded terminal event carries a marker
// nothing lives behind — the run was deleted manually while running, or the
// label was orphaned by a watcher restart — so the marker is swapped for
// "criteria-dirty".
//
// The sweep mirrors the operator's stale-adapter sweep (CRI-144): it runs
// deterministically on every poll, decides purely from what exists now and
// never from remembered history, and acts once per orphan. The poll's
// existing CriteriaRun index supplies both guards, so nothing additional is
// listed on the k8s side:
//   - a live run in any phase (Pending, Running, Unknown, or the empty
//     phase a freshly created run carries until the controller sets status)
//     leaves the label lifecycle to reconciliation;
//   - a run that settled (Failed or Succeeded) recorded a terminal event:
//     reconciliation owns that transition, so a Succeeded run must not gain
//     the dirty marker here.
//
// Candidates are the project's tickets carrying "criteria-automation" in
// any workflow state, so the sweep reaches tickets outside the
// route-declared states. Iteration is ordered by identifier for
// deterministic logs and write order.
//
// The sweep requires both label ids: it is an atomic label-state
// transition, and candidates are only ever visible through the automation
// label — removing that marker before the dirty marker is durable would
// lose the orphan evidence with no retry. Writes are therefore ordered
// dirty-marker first, and a failed dirty write defers the removal with it.
// "criteria-dirty" itself is never removed here or anywhere in the watcher:
// it is sticky until a cleanup workflow or a human clears it (CRI-220).
func (w *watcher) sweepOrphanedAutomationLabels(ctx context.Context, projectID string, runs map[string]runPhases, automationLabelID, dirtyLabelID string, currentLabelIDs map[string][]string) error {
	if automationLabelID == "" || dirtyLabelID == "" {
		w.log.V(1).Info("skipping orphan sweep: automation label ids unresolved (ensure failed this poll); deferred to next poll")
		return nil
	}
	candidates, err := w.linear.IssuesWithLabel(ctx, projectID, automationLabelName)
	if err != nil {
		return err
	}
	slices.SortFunc(candidates, func(a, b linear.Issue) int {
		return strings.Compare(a.Identifier, b.Identifier)
	})
	swept := 0
	for _, issue := range candidates {
		if ph := runs[issue.Identifier]; ph.anyRun {
			// The ticket has observed CriteriaRuns: either one is still live
			// (any phase, including Pending) or its runs all settled,
			// recording a terminal event. Reconciliation owns the label
			// lifecycle in both cases.
			continue
		}
		w.log.Info("sweeping orphaned automation label: no live CriteriaRun and no recorded terminal event",
			"ticket", issue.Identifier)
		// CRI-252: the candidate read above supplies the issue's label IDs,
		// so the swap is computed from that read and applied with a single
		// REPLACE write — no per-orphan label read.
		desired := deleteLabelID(issue.LabelIDs, automationLabelID)
		if !slices.Contains(issue.Labels, dirtyLabelName) && w.writeReady(dirtyLabelID, dirtyLabelName, issue.Identifier, "add") {
			if !slices.Contains(desired, dirtyLabelID) {
				desired = append(desired, dirtyLabelID)
			}
		}
		if slices.Equal(desired, issue.LabelIDs) {
			continue
		}
		if err := w.linear.SetIssueLabelIDs(ctx, issue.ID, desired); err != nil {
			if linear.IsRateLimit(err) {
				return err
			}
			w.log.Error(err, "swapping orphaned automation label for dirty; deferring to next poll",
				"ticket", issue.Identifier, "labelIDs", desired)
			continue
		}
		w.log.Info("swapped orphaned automation label for dirty", "ticket", issue.Identifier, "labelIDs", desired)
		// CRI-252: the ticket's label state is now `desired`; later REPLACE
		// writes in this poll (run firing) must build on it.
		currentLabelIDs[issue.Identifier] = desired
		swept++
	}
	w.log.Info("orphan sweep complete", "candidates", len(candidates), "swept", swept)
	return nil
}

func (w *watcher) buildCriteriaRun(issue linear.Issue, repoURL string, sel *routes.Selection) *criteriav1.CriteriaRun {
	ticket := issue.Identifier
	name := fmt.Sprintf("%s-%d", strings.ToLower(ticket), time.Now().Unix())
	spec := criteriav1.CriteriaRunSpec{
		TicketID: ticket,
		RepoURL:  repoURL,
		// Per-scope sessions: the engine emits provision/release events,
		// castle fans them out to the operator (CRI-133/134/135), and the
		// operator reconciles per-scope adapter pods, tearing each scope's
		// pod down on release instead of leaving idle adapter jobs running
		// after the workflow finishes. The operator's lifecycle parser has
		// understood the engine's nested event envelope since CRI-132, so
		// this is safe to enable again (CRI-136). Workflows can opt out by
		// flipping this field on the created CriteriaRun.
		PerScopeSessions: true,
		// CRI-217: the workflow resolved from the routes ConfigMap
		// (project default or workflows-label-group override) is stamped
		// here for the jobbuilder to consume (CRI-222). Nil means the
		// operator's defaults apply.
		Workflow: convertWorkflow(sel.Name, sel.Workflow),
		// CRI-231: url-type routes run in source mode — spec.workflowSource
		// carries the fetched source (and the fail-closed ref pin), and
		// url+image additionally stamps spec.image as the process image
		// (URL is content, image is process). Url-only leaves spec.image
		// unset so the base image applies; type=image runs keep it unset
		// so the operator's --default-image remains the single source of
		// truth for the baked workflow image, so bumping that image only
		// requires one deployment update.
		BuildCmd:        w.buildCmd,
		TestCmd:         w.testCmd,
		CIGateCmd:       w.ciGateCmd,
		MaxAgentVisits:  w.maxAgentVisits,
		ProviderBaseURL: w.providerBaseURL,
	}
	stampWorkflowSource(&spec, sel.Workflow)
	return &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: w.namespace,
			Labels: map[string]string{
				"ticket":                         strings.ToLower(ticket),
				"app.kubernetes.io/managed-by":   "criteria-linear-watcher",
				"criteria.brokenbots.dev/source": "linear",
			},
		},
		Spec: spec,
	}
}

// stampWorkflowSource resolves the source-mode spec fields from a url-type
// routes workflow object (CRI-231): spec.workflowSource carries the URL the
// runner fetches and applies at run time (plus the operator's fail-closed
// ref pin), and url+image routes additionally stamp spec.image so the
// process executes in the route-provided image. Image-type routes (baked
// tree) leave both unset. Type/url validation is the routes package's job
// (k8s/routes.schema.json); the guards here only keep an unvalidated
// declaration from stamping an unusable source.
func stampWorkflowSource(spec *criteriav1.CriteriaRunSpec, wf routes.Workflow) {
	if wf.Type != routes.TypeURL || strings.TrimSpace(wf.URL) == "" {
		return
	}
	spec.WorkflowSource = &criteriav1.RunWorkflowSource{
		Type: "url",
		URL:  wf.URL,
		Ref:  wf.Ref,
	}
	if strings.TrimSpace(wf.Image) != "" {
		spec.Image = wf.Image
	}
}

// convertWorkflow deep-copies a resolved routes workflow object into the
// stamped CriteriaRun spec type, carrying the resolved workflow-library name.
func convertWorkflow(name string, wf routes.Workflow) *criteriav1.RunWorkflow {
	out := &criteriav1.RunWorkflow{
		Name:      name,
		Type:      wf.Type,
		Namespace: wf.Namespace,
		// CRI-242: the admission queue class comes off the routes
		// workflow-library object; an omitted class resolves to dev so
		// routes without an explicit class keep the per-repo
		// serialization they had before classes existed.
		Class: workflowClass(wf),
		Image: wf.Image,
		URL:   wf.URL,
		Ref:   wf.Ref,
	}
	if wf.Env != nil {
		out.Env = make(map[string]string, len(wf.Env))
		for k, v := range wf.Env {
			out.Env[k] = v
		}
	}
	if len(wf.Volumes) > 0 {
		out.Volumes = make([]criteriav1.RunWorkflowVolume, 0, len(wf.Volumes))
		for _, v := range wf.Volumes {
			out.Volumes = append(out.Volumes, criteriav1.RunWorkflowVolume{
				Name:      v.Name,
				Kind:      v.Kind,
				MountPath: v.MountPath,
				SubPath:   v.SubPath,
				ReadOnly:  v.ReadOnly,
				Claim:     v.Claim,
				Server:    v.Server,
				Path:      v.Path,
				SizeLimit: v.SizeLimit,
				Env:       copyStringMap(v.Env),
			})
		}
	}
	if len(wf.Secrets) > 0 {
		out.Secrets = make([]criteriav1.RunWorkflowSecret, 0, len(wf.Secrets))
		for _, s := range wf.Secrets {
			out.Secrets = append(out.Secrets, criteriav1.RunWorkflowSecret{
				Name:                s.Name,
				SecretProviderClass: s.SecretProviderClass,
				MountPath:           s.MountPath,
				Env:                 copyStringMap(s.Env),
			})
		}
	}
	return out
}

// workflowClass resolves the admission queue class stamped onto the run
// (CRI-242): the routes workflow's declared class, defaulted to dev so
// routes without an explicit class keep the per-repo serialization they
// had before classes existed. Routes validation rejects any other value;
// an unrecognized value on an unvalidated payload degrades to dev (fail
// safe: the run keeps serializing).
func workflowClass(wf routes.Workflow) string {
	if wf.Class == routes.ClassTriage {
		return routes.ClassTriage
	}
	return routes.ClassDefault
}

func copyStringMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

const (
	// routingCommentPrefix marks Linear comments the watcher posts for
	// CRI-217 routing failures; the exact body is the dedup key.
	routingCommentPrefix = "criteria-linear-watcher: route lookup failed"
	// commentDedupLimit bounds the comments fetched for dedup.
	commentDedupLimit = 50

	// CRI-252 poll discipline: the default interval is 5 minutes (a good
	// consumer of the shared 2500 req/hr budget; k8s-side reactions are
	// watch-driven, so firing latency is unaffected) and rate limits back
	// the interval off up to this cap.
	defaultPollInterval = 5 * time.Minute
	backoffCap          = 30 * time.Minute

	// CRI-219 automation label lifecycle. Both labels are team-scoped and
	// their IDs are resolved once and cached (re-ensured only on a cache
	// miss since CRI-252); the names and colors are fixed by the ticket so
	// every deployment observes the same marker vocabulary.
	automationLabelName  = "criteria-automation"
	dirtyLabelName       = "criteria-dirty"
	automationLabelColor = "#0ea5e9" // inflight marker: blue
	dirtyLabelColor      = "#f43f5e" // failed-run marker: red
)

// postRoutingComment posts the routing failure on the ticket, deduping by
// exact body so a stuck failure does not spam Linear across polls. When
// reading existing comments fails, posting is skipped this poll and retried
// next poll rather than risking duplicates. The comment read happens only
// on actual routing failures (CRI-252 keeps this read on the failure path).
func (w *watcher) postRoutingComment(ctx context.Context, issue linear.Issue, lookupErr error) error {
	body := fmt.Sprintf("%s: %v", routingCommentPrefix, lookupErr)
	comments, err := w.linear.IssueComments(ctx, issue.ID, commentDedupLimit)
	if err != nil {
		w.log.Error(err, "reading Linear comments for routing-failure dedup; retrying next poll",
			"ticket", issue.Identifier)
		return err
	}
	if slices.Contains(comments, body) {
		w.log.V(1).Info("routing failure already reported on ticket", "ticket", issue.Identifier)
		return nil
	}
	if err := w.linear.PostComment(ctx, issue.ID, body); err != nil {
		w.log.Error(err, "posting routing failure comment on ticket", "ticket", issue.Identifier)
		return err
	}
	w.log.Info("posted routing failure comment on ticket", "ticket", issue.Identifier)
	return nil
}

func mustSelector(m map[string]string) client.MatchingLabelsSelector {
	sel, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: m})
	if err != nil {
		panic("invalid selector: " + err.Error())
	}
	return client.MatchingLabelsSelector{Selector: sel}
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func parseDuration(s string) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		return defaultPollInterval
	}
	return d
}

func parseInt(s string, fallback int) int {
	var v int
	if _, err := fmt.Sscanf(s, "%d", &v); err != nil {
		return fallback
	}
	return v
}
