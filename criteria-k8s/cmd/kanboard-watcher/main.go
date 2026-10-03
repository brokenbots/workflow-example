package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/brokenbots/workflow-example/criteria-k8s/internal/kanboard"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/linear"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/routes"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/runstamp"
	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
)

// Board column names the watcher moves tasks between as runs progress.
// They are the Kanboard analogue of the Linear watcher's automation labels
// (criteria-automation / criteria-dirty) plus the states the workflows set:
// a live run parks the task in workColumnName, a failed run parks it in
// reviewColumnName for a human, a succeeded dev-class run moves it to
// doneColumnName. A succeeded triage-class run is the middle of the chain,
// not its end: the triage workflow's re-arm path moves the ticket into the
// develop route's trigger column and the watcher leaves that move in place
// (KB-9). A dev-class success is judged by the workflow verdict stamped on
// the run (KB-23): a failed/awaiting_human verdict repairs a Done stamp
// into the review column and otherwise leaves the ticket where the
// workflow's own bookkeeping put it. Even a success verdict does not stamp
// Done on its own (KB-50): Done is the confirmation of delivered work, and
// the watcher requires a PR recorded on the run before it confirms one.
const (
	defaultPollInterval = 60 * time.Second
	defaultPollTimeout  = 5 * time.Minute
	defaultHealthAddr   = ":8081"
	workColumnName      = "Work in progress"
	reviewColumnName    = "Review"
	doneColumnName      = "Done"

	// Workflow terminal state names (RunCompleted.final_state) the watcher
	// judges a dev-class success by (KB-23). handler_complete delivered
	// the work; awaiting_human handed the ticket to a human with the
	// bookkeeping intact; failed is the workflow's failure verdict. The
	// triage workflow ends in awaiting_human/ready_for_development but is
	// excluded from the verdict branch by its class.
	// KB-50: a judgeable verdict is necessary but no longer sufficient for
	// the Done move — even handler_complete needs the run to carry a PR,
	// because a Completed pod is not a succeeded workflow.
	finalStateHandlerComplete = "handler_complete"
	finalStateFailed          = "failed"
	finalStateAwaitingHuman   = "awaiting_human"
)

var (
	namespace         = flag.String("namespace", getenv("NAMESPACE", "criteria-jobs"), "Namespace to watch and create CriteriaRuns in")
	kanboardURL       = flag.String("kanboard-url", getenv("KANBOARD_URL", ""), "Kanboard JSON-RPC endpoint (jsonrpc.php, subpath included)")
	kanboardToken     = flag.String("kanboard-token", getenv("KANBOARD_APP_TOKEN", ""), "Kanboard app API token (also read from /secrets/kanboard_app_token)")
	projectName       = flag.String("kanboard-project-name", getenv("KANBOARD_PROJECT_NAME", "Kanboard Tickets"), "Kanboard project to watch")
	triggerTag        = flag.String("kanboard-trigger-tag", getenv("KANBOARD_TRIGGER_TAG", "k8s-run"), "Require this tag on the task before triggering a run; empty means any task matching a route triggers")
	pollInterval      = flag.Duration("poll-interval", parseDuration(getenv("POLL_INTERVAL", "60s"), defaultPollInterval), "How often to poll Kanboard")
	pollTimeout       = flag.Duration("poll-timeout", parseDuration(getenv("POLL_TIMEOUT", "5m"), defaultPollTimeout), "Deadline for one poll cycle; a hung poll is aborted and logged, and the health endpoints report the stall (KB-22)")
	healthAddr        = flag.String("health-addr", getenv("HEALTH_ADDR", defaultHealthAddr), "Health endpoint listen address serving /readyz and /livez (empty disables the server)")
	image             = flag.String("image", getenv("CRITERIA_IMAGE", "localhost:5000/linear-intake-remote:dev"), "Default Criteria workflow image")
	providerBaseURL   = flag.String("provider-base-url", getenv("PROVIDER_BASE_URL", "http://192.168.17.116:11434/v1"), "Default provider base URL")
	maxAgentVisits    = flag.Int("max-agent-visits", parseInt(getenv("MAX_AGENT_VISITS", "2"), 2), "Default max agent visits")
	buildCmd          = flag.String("build-cmd", getenv("BUILD_CMD", ""), "Default build command")
	testCmd           = flag.String("test-cmd", getenv("TEST_CMD", ""), "Default test command")
	ciGateCmd         = flag.String("ci-gate-cmd", getenv("CI_GATE_CMD", ""), "Default CI gate command")
	defaultRepoURL    = flag.String("default-repo-url", getenv("DEFAULT_REPO_URL", ""), "Default repo URL when the Kanboard task does not reference one")
	routesFile        = flag.String("routes-file", getenv("CRITERIA_ROUTES_FILE", routes.DefaultFile), "Routes payload file (mounted from the criteria-routes ConfigMap); re-read every poll")
	workflowsTagGroup = flag.String("kanboard-workflows-tag-group", getenv("KANBOARD_WORKFLOWS_TAG_GROUP", "workflows"), "Tag group whose tags name a workflow overriding the route's project default (empty disables overrides)")
	configsTagGroup   = flag.String("kanboard-configs-tag-group", getenv("KANBOARD_CONFIGS_TAG_GROUP", "configs"), "Tag group whose tags name a per-repo config (KB-103 configLibrary entry) for tasks whose route carries no configRef (empty disables the overrides)")
	routingTags       = flag.String("kanboard-routing-tags", getenv("KANBOARD_ROUTING_TAGS", ""), "Comma-separated task tags that are routing signals (e.g. component tags), never workflow-override candidates; they join the built-in exemption filter (empty disables the extra exemptions)")
)

func main() {
	flag.Parse()
	logger := zap.New()
	log.SetLogger(logger)

	endpoint := *kanboardURL
	if endpoint == "" {
		logger.Error(fmt.Errorf("missing Kanboard endpoint"), "kanboard-url or KANBOARD_URL is required")
		os.Exit(1)
	}
	appToken := *kanboardToken
	if appToken == "" {
		if b, err := os.ReadFile("/secrets/kanboard_app_token"); err == nil {
			appToken = strings.TrimSpace(string(b))
		}
	}
	if appToken == "" {
		logger.Error(fmt.Errorf("missing Kanboard app token"), "kanboard-token or /secrets/kanboard_app_token is required")
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

	kb := kanboard.New(endpoint, appToken)
	githubToken := getenv("WORKFLOW_GITHUB_TOKEN", "")
	if githubToken == "" {
		if b, err := os.ReadFile("/secrets/workflow_github_token"); err == nil {
			githubToken = strings.TrimSpace(string(b))
		}
	}
	w := watcher{
		client:            k8s,
		kb:                kb,
		namespace:         *namespace,
		projectName:       *projectName,
		triggerTag:        *triggerTag,
		pollInterval:      *pollInterval,
		pollTimeout:       *pollTimeout,
		healthAddr:        *healthAddr,
		image:             *image,
		providerBaseURL:   *providerBaseURL,
		maxAgentVisits:    *maxAgentVisits,
		buildCmd:          *buildCmd,
		testCmd:           *testCmd,
		ciGateCmd:         *ciGateCmd,
		defaultRepoURL:    *defaultRepoURL,
		routesFile:        *routesFile,
		workflowsTagGroup: *workflowsTagGroup,
		configsTagGroup:   *configsTagGroup,
		routingTags:       parseTagList(*routingTags),
		repoValidator:     linear.DefaultRepoValidator(nil, githubToken, ""),
		log:               logger,
	}

	ctx := ctrl.SetupSignalHandler()
	if err := w.run(ctx); err != nil {
		logger.Error(err, "watcher exited")
		os.Exit(1)
	}
}

func getenv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func parseDuration(s string, fallback time.Duration) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}

func parseInt(s string, fallback int) int {
	var v int
	if _, err := fmt.Sscanf(s, "%d", &v); err != nil || v <= 0 {
		return fallback
	}
	return v
}

// parseTagList splits a comma-separated tag list into its entries,
// trimming whitespace and dropping empty entries ("criteria, operator,"
// -> [criteria operator]).
func parseTagList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if tag := strings.TrimSpace(part); tag != "" {
			out = append(out, tag)
		}
	}
	return out
}

type watcher struct {
	client       client.Client
	kb           *kanboard.Client
	namespace    string
	projectName  string
	triggerTag   string
	pollInterval time.Duration
	// pollTimeout bounds one poll cycle (KB-22): every blocking call inside
	// poll runs under this deadline, so a hung Kanboard RPC or k8s call
	// surfaces as an error instead of wedging the loop silently (the
	// 2026-09-26 incident saw the loop stall ~8.5h with no error and no log
	// line).
	pollTimeout time.Duration
	// healthAddr is the /readyz + /livez listener address for the pod's
	// readiness/liveness probes (KB-22); empty disables the server.
	healthAddr string
	// watchdog records poll outcomes; the health endpoints translate its
	// state into probe results.
	watchdog          pollWatchdog
	image             string
	providerBaseURL   string
	maxAgentVisits    int
	buildCmd          string
	testCmd           string
	ciGateCmd         string
	defaultRepoURL    string
	routesFile        string
	workflowsTagGroup string
	configsTagGroup   string
	// routingTags holds the KANBOARD_ROUTING_TAGS entries (comma-separated
	// env): tags that are board routing metadata (e.g. component tags like
	// criteria, operator) and must never become workflow-override
	// candidates. Two such tags on an armed ticket used to fail route
	// resolution closed with ErrAmbiguousWorkflow, re-failing the task on
	// every poll (KB-8).
	routingTags   []string
	repoValidator func(string) bool
	projectID     int
	// lastRuns records, per ticket, the runs index entry seen at the
	// previous poll, so reconciliation only acts on changes (the CRI-252
	// change-driven discipline). The run identity is part of the record:
	// two settled runs of different classes can both read Succeeded — the
	// triage run handing off to the dev run (KB-9) — and the second settle
	// must still reconcile. In-memory only.
	lastRuns map[string]runPhases
	log      logr.Logger
}

func (w *watcher) run(ctx context.Context) error {
	if w.pollTimeout <= 0 {
		w.pollTimeout = defaultPollTimeout
	}
	// The startup project resolution runs under the same deadline as a poll
	// (KB-22): a wedged FindProjectID would otherwise hang before the loop
	// starts, with no deadline to surface it.
	startupCtx, cancel := context.WithTimeout(ctx, w.pollTimeout)
	projectID, err := w.kb.FindProjectID(startupCtx, w.projectName)
	cancel()
	if err != nil {
		return fmt.Errorf("resolve kanboard project: %w", err)
	}
	w.projectID = projectID
	w.log.Info("watching kanboard project", "project", w.projectName, "projectID", projectID, "interval", w.pollInterval, "pollTimeout", w.pollTimeout)

	if err := w.serveHealth(ctx); err != nil {
		return fmt.Errorf("start health server: %w", err)
	}

	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()
	if err := w.pollWithDeadline(ctx); err != nil {
		w.log.Error(err, "poll failed")
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := w.pollWithDeadline(ctx); err != nil {
				w.log.Error(err, "poll failed")
			}
		}
	}
}

// pollWatchdog records poll-loop outcomes for the health endpoints (KB-22).
// The zero value is ready for use. Readiness tracks the last *successful*
// poll: once polls stop succeeding — a persistent outage, or a poll killed
// by the per-poll deadline — the pod reports unready after stallThreshold,
// which turns a silent wedge into a visible, unready pod. Liveness tracks
// the last *completed* poll: polls that keep timing out keep the process
// live but unready.
type pollWatchdog struct {
	mu              sync.Mutex
	lastPollFinish  time.Time
	lastPollSuccess time.Time
}

func (pw *pollWatchdog) record(err error) {
	now := time.Now()
	pw.mu.Lock()
	defer pw.mu.Unlock()
	pw.lastPollFinish = now
	if err == nil {
		pw.lastPollSuccess = now
	}
}

func (pw *pollWatchdog) ready(now time.Time, threshold time.Duration) (bool, string) {
	pw.mu.Lock()
	defer pw.mu.Unlock()
	switch {
	case pw.lastPollSuccess.IsZero():
		return false, "no successful poll since startup"
	case now.Sub(pw.lastPollSuccess) > threshold:
		return false, fmt.Sprintf("last successful poll %s ago exceeds %s",
			now.Sub(pw.lastPollSuccess).Round(time.Second), threshold)
	}
	return true, ""
}

func (pw *pollWatchdog) live(now time.Time, threshold time.Duration) (bool, string) {
	pw.mu.Lock()
	defer pw.mu.Unlock()
	switch {
	case pw.lastPollFinish.IsZero():
		return false, "no poll has completed since startup"
	case now.Sub(pw.lastPollFinish) > threshold:
		return false, fmt.Sprintf("last completed poll %s ago exceeds %s",
			now.Sub(pw.lastPollFinish).Round(time.Second), threshold)
	}
	return true, ""
}

// stallThreshold is how long the health endpoints tolerate a stretch
// without a successful poll (readiness) or without any completed poll
// (liveness). It covers one full worst-case poll cycle — interval plus the
// whole poll deadline — and adds another deadline-length of slack, so a run
// of slow-but-completing polls never flaps the probes.
func (w *watcher) stallThreshold() time.Duration {
	return w.pollInterval + 2*w.pollTimeout
}

// pollWithDeadline runs one poll under the per-poll deadline (KB-22): the
// observed wedge was a poll() with no deadline blocking ~8.5h while the pod
// read Running 1/1. Every blocking call inside poll honors its context, so
// an expired deadline aborts the hung call and surfaces it as an error; the
// watchdog then reports the stall via /readyz.
func (w *watcher) pollWithDeadline(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, w.pollTimeout)
	defer cancel()
	err := w.poll(ctx)
	w.watchdog.record(err)
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("poll did not complete within %s: %w", w.pollTimeout, err)
	}
	return err
}

// ready is the /readyz probe: the watcher does its job only while polls
// keep succeeding (KB-22).
func (w *watcher) ready() (bool, string) {
	return w.watchdog.ready(time.Now(), w.stallThreshold())
}

// live is the /livez probe: the poll loop must keep completing polls. A
// loop wedged outside the per-poll deadline (or before the first poll)
// fails it and the kubelet restarts the pod — the same remedy the
// 2026-09-26 incident needed a manual rollout for.
func (w *watcher) live() (bool, string) {
	return w.watchdog.live(time.Now(), w.stallThreshold())
}

// healthMux builds the probe endpoints: 200 "ok" when healthy, 503 with
// the stall reason otherwise.
func (w *watcher) healthMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", healthProbe(w.ready))
	mux.HandleFunc("/livez", healthProbe(w.live))
	return mux
}

func healthProbe(fn func() (bool, string)) http.HandlerFunc {
	return func(rw http.ResponseWriter, _ *http.Request) {
		ok, reason := fn()
		if !ok {
			http.Error(rw, reason, http.StatusServiceUnavailable)
			return
		}
		rw.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(rw, "ok\n")
	}
}

// serveHealth runs the readiness/liveness endpoints for the pod's probes
// (KB-22); an empty healthAddr disables the server.
func (w *watcher) serveHealth(ctx context.Context) error {
	if w.healthAddr == "" {
		w.log.Info("health server disabled: no listen address configured")
		return nil
	}
	ln, err := net.Listen("tcp", w.healthAddr)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           w.healthMux(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	go func() {
		if serveErr := srv.Serve(ln); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			w.log.Error(serveErr, "health server failed", "addr", w.healthAddr)
		}
	}()
	w.log.Info("health server listening", "addr", w.healthAddr)
	return nil
}

// poll mirrors the linear watcher's structure: routes fail closed, one
// listing read per poll, the runs index gates firing, in-flight duplicates
// suppressed. Kanboard tags are the analogue of Linear labels; the trigger
// tag gates cluster compute exactly like LINEAR_TRIGGER_LABEL.
func (w *watcher) poll(ctx context.Context) error {
	routesPayload, err := routes.LoadFile(w.routesFile)
	if err != nil {
		w.log.Error(err, "loading routes payload; failing closed and skipping poll", "routesFile", w.routesFile)
		// Failing the poll (KB-22): a nil here would count as a successful
		// poll and keep the pod Ready through a persistent routes outage.
		return fmt.Errorf("load routes payload: %w", err)
	}
	// Dual-source scoping (mirrors the linear watcher): query only the
	// watched project's routes' columns, so linear state names never reach
	// the Kanboard query.
	states := routesPayload.ProjectStates(w.projectName)
	w.log.V(1).Info("polling kanboard for task columns declared by routes", "states", states)

	// Runs index: one CriteriaRun list per poll backs both the firing gate
	// and phase reconciliation. Only this watcher's runs (source=kanboard)
	// are indexed, so the Linear watcher's runs never collide.
	runs, err := w.indexRunPhases(ctx)
	if err != nil {
		return err
	}

	tasks, err := w.kb.GetAllTasks(ctx, w.projectID)
	if err != nil {
		return err
	}
	// Phase reconciliation: move task columns on run changes, then update
	// the in-memory task structs with the new column so the firing loop
	// below sees the post-reconciliation column — a task whose run just
	// settled must not fire again off its pre-settle column (observed live:
	// the smoke task re-fired after Succeeded because the listing still
	// said Backlog). The record carries the run identity, not just the
	// phase: a settled triage run followed by a settled dev run both read
	// Succeeded, and the dev settle must still reconcile (KB-9).
	for i := range tasks {
		ticketID := tasks[i].Identifier()
		ph, ok := runs[ticketID]
		if !ok || !ph.anyRun {
			continue
		}
		if prev, seen := w.lastRuns[ticketID]; seen && prev == ph {
			continue
		}
		if newCol, changed := w.reconcileTaskColumn(ctx, tasks[i], ph); changed {
			tasks[i].ColumnID = newCol
			tasks[i].ColumnName = w.columnNameFor(ctx, tasks[i].ProjectID, newCol)
		}
		if w.lastRuns == nil {
			w.lastRuns = make(map[string]runPhases)
		}
		w.lastRuns[ticketID] = ph
	}
	for ticketID := range w.lastRuns {
		if _, ok := runs[ticketID]; !ok {
			delete(w.lastRuns, ticketID)
		}
	}

	for _, task := range tasks {
		ticketID := task.Identifier()
		// Trigger tag gates cluster compute (analogue of the k8s-run label).
		if w.triggerTag != "" && !slices.Contains(task.Tags, w.triggerTag) {
			continue
		}
		// Route lookup: project name, column (state analogue), tags. Kanboard
		// has no Linear-style label groups, so a tag is treated as a workflow
		// override candidate only when it sits in the configured tag group —
		// enforced here by filtering the task's tags against the group before
		// Resolve sees them. WorkflowsLabelGroup is always passed as the
		// literal group name so routes.Resolve's override logic cannot treat
		// ungrouped tags as overrides.
		//
		// KB-103: the configs tag group works the same way; membership is
		// payload-driven — a tag maps to the configs group only when it
		// names a configLibrary entry in the live payload, so non-routing
		// tags keep mapping to the workflows group exactly as before.
		tagGroups := map[string]string{}
		for _, tag := range task.Tags {
			if w.isRoutingSignal(tag) {
				continue
			}
			if w.configsTagGroup != "" {
				if _, isConfig := routesPayload.ConfigLibrary[tag]; isConfig {
					tagGroups[tag] = w.configsTagGroup
					continue
				}
			}
			tagGroups[tag] = w.workflowsTagGroup
		}
		sel, err := routesPayload.Resolve(routes.Selector{
			Project:             task.ProjectName,
			State:               task.ColumnName,
			Labels:              task.Tags,
			LabelGroups:         tagGroups,
			WorkflowsLabelGroup: w.workflowsTagGroup,
			ConfigsLabelGroup:   w.configsTagGroup,
		})
		if err != nil {
			if errors.Is(err, routes.ErrNoRoute) {
				w.log.V(1).Info("skipping task: no route matches", "ticket", ticketID, "column", task.ColumnName)
				continue
			}
			w.log.Error(err, "route lookup failed closed; not creating CriteriaRun", "ticket", ticketID)
			continue
		}
		repoURL := extractRepoURL(task, w.defaultRepoURL, w.repoValidator)
		if repoURL == "" {
			w.log.Info("skipping Kanboard task without repo URL", "ticket", ticketID, "title", task.Title)
			continue
		}
		if ph, ok := runs[ticketID]; ok && ph.active {
			w.log.V(1).Info("run already active", "ticket", ticketID)
			continue
		}
		run := w.buildCriteriaRun(ctx, task, repoURL, sel)
		if err := w.client.Create(ctx, run); err != nil {
			w.log.Error(err, "creating CriteriaRun", "ticket", ticketID)
			continue
		}
		w.log.Info("created CriteriaRun", "ticket", ticketID, "name", run.Name, "repoUrl", repoURL, "workflow", sel.Name)
	}
	return nil
}

// isRoutingSignal reports whether a task tag is routing metadata the
// watcher or the workflows themselves consume, and must therefore never
// become a workflow-override candidate:
//   - triggerTag: the cluster-compute gate tag (k8s-run);
//   - internal-reproduced: the triage bypass signal kanboard_triage_v1
//     reads (Kanboard analogue of the Linear internal-reproduced label);
//   - Bug / Feature: classification tags the workflows set via
//     set_classification_label — without the exemption the bypass path's
//     own tag write makes the next poll fail closed;
//   - repo:<owner>/<name>: the per-ticket repo binding;
//   - the KANBOARD_ROUTING_TAGS entries: board component tags (criteria,
//     operator) that carry no workflow meaning (KB-8).
//
// Anything else joins the workflows tag group, so a tag that genuinely
// names a workflow still overrides the route default — and two of those
// still fail closed on ambiguity (ErrAmbiguousWorkflow).
func (w *watcher) isRoutingSignal(tag string) bool {
	if tag == w.triggerTag || tag == "internal-reproduced" ||
		tag == "Bug" || tag == "Feature" ||
		strings.HasPrefix(tag, repoTagPrefix) {
		return true
	}
	return slices.Contains(w.routingTags, tag)
}

// indexRunPhases lists CriteriaRuns created by this watcher (source=kanboard)
// and groups them by ticket. The KB source label isolates them from the
// Linear watcher's runs so both watchers coexist in one namespace. The
// ticket's most recent run (creation time, name tiebreak — the linear
// watcher's runNewer) is the reconciliation authority, so its phase and
// admission class drive the column moves.
func (w *watcher) indexRunPhases(ctx context.Context) (map[string]runPhases, error) {
	list := &criteriav1.CriteriaRunList{}
	req := client.ListOptions{
		Namespace:     w.namespace,
		LabelSelector: labels.SelectorFromSet(map[string]string{"criteria.brokenbots.dev/source": "kanboard"}),
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
		t := run.Spec.TicketID
		if !strings.HasPrefix(t, "KB-") {
			continue
		}
		g := groups[t]
		if g == nil {
			g = &ticketRuns{}
			groups[t] = g
		}
		switch run.Status.Phase {
		case criteriav1.PhaseFailed, criteriav1.PhaseSucceeded:
			// settled: nothing in flight from this run
		default:
			// Pending, Running, Unknown — and the empty phase a freshly
			// created run carries until the controller sets status — are
			// all in flight (CRI-219).
			g.active = true
		}
		if g.latest.Name == "" || runNewer(run, &g.latest) {
			g.latest = *run
		}
	}
	out := make(map[string]runPhases, len(groups))
	for ticket, g := range groups {
		ph := runPhases{
			active:           g.active,
			latestPhase:      g.latest.Status.Phase,
			latestRun:        g.latest.Name,
			latestFinalState: g.latest.Status.FinalState,
			anyRun:           true,
		}
		if g.latest.Spec.Workflow != nil {
			ph.latestClass = g.latest.Spec.Workflow.Class
		}
		out[ticket] = ph
	}
	return out, nil
}

// runNewer reports whether run sorts after cur (creation time, with the
// name as a deterministic tiebreak), used to pick a ticket's most recent
// CriteriaRun. Identical to the linear watcher's runNewer.
func runNewer(run, cur *criteriav1.CriteriaRun) bool {
	if !run.CreationTimestamp.Equal(&cur.CreationTimestamp) {
		return cur.CreationTimestamp.Before(&run.CreationTimestamp)
	}
	return run.Name > cur.Name
}

type runPhases struct {
	active      bool
	latestPhase criteriav1.CriteriaRunPhase
	// latestRun is the name of the run holding latestPhase and latestClass
	// the admission class stamped on its spec workflow (triage vs dev).
	// The class decides whether a Succeeded phase stamps Done (KB-9); the
	// identity lets the reconcile gate tell two settled runs apart when
	// both read Succeeded.
	latestRun string
	// latestFinalState is the latest run's stamped workflow verdict (the
	// workflow's terminal state name, KB-23). The engine exits 0 even when
	// the workflow ended in its failure terminal, so a Succeeded phase can
	// mask a failed verdict; the verdict decides where a dev-class success
	// lands. Empty for record-derived terminals and castle-less runs.
	latestFinalState string
	latestClass      string
	anyRun           bool
}

// reconcileTaskColumn moves the Kanboard task between board columns as its
// latest run progresses — the Kanboard analogue of the Linear watcher's
// automation labels:
//   - run in flight        -> task to the work column
//   - dev run Succeeded    -> task per the workflow verdict; the Done move
//     requires PR evidence too (KB-23, KB-50)
//   - triage run Succeeded -> column untouched (KB-9)
//   - run Failed           -> task to the review column (human)
//
// A triage-class success is the middle of the triage -> develop chain, not
// its end: the triage workflow's re-arm path (rearm_k8s_run +
// set_ready_state) moves the ticket into the develop route's trigger
// column, and stamping Done here would reconcile the ticket out from under
// that handoff before the develop route can fire (observed live on KB-2:
// triage succeeded, the watcher moved the ticket Ready -> Done, and the
// develop run never fired). The triage workflow owns the column choice on
// its success path, so the watcher leaves the task where the workflow put
// it. A dev-class success stamps Done only once the run's verdict and PR
// evidence are verifiable (KB-50).
//
// A dev-class success is judged by the workflow's own verdict, not the
// phase: the engine exits 0 even when the workflow ends in its failure
// terminal, so the Succeeded phase alone masks a failure verdict (observed
// live on KB-17, run kb-17-1790397477: the workflow's verdict was failure —
// comment_handler_failed logged outcome=failure — but the run read
// Succeeded and the watcher moved the ticket to Done). A "failed" or
// "awaiting_human" verdict means the work is not complete — the develop
// workflow's failure path parks the ticket in review (set_review_state ran
// before the terminal) — so a Done stamp that predates the verdict is
// repaired into the review column, and any other column is left untouched:
// Review is the workflow's own parking spot, and Ready is the route's
// re-fire trigger after a fetch failure.
//
// KB-50 + KB-101: even a "handler_complete" verdict stamps Done only with
// the run's PR evidence — which the workflow delivers itself: since the
// KB-49 route guard a handler success with an empty pr_url never reaches
// the done-path bookkeeping (flag_missing_pr_url fails the run loudly), so
// a handler_complete verdict implies a merged PR. Status.PRNumber is REMOVED
// (KB-101): it summarized the castle run record's pr_url, no castle build
// ever published run.metadata, and it was therefore permanently empty —
// a gate key that can never be set gates nothing. A Completed pod is still
// not a succeeded workflow: the runner job exits 0 when the workflow
// reaches ANY terminal. Observed live on KB-40 and KB-39 (2026-09-28): the
// watcher moved Review -> Done off the Completed pod phase while
// run_handler had logged outcome=failure — no PR, no evidence comment, main
// untouched. An empty verdict (the run's terminal not observable yet:
// castle observation lag, a record-derived terminal, or castle observation
// disabled) is equally unverified and abstains; the verdict lands on a
// later poll and re-fires this reconcile, and a Done stamp predating a
// failure verdict is still repaired by the branch above. Any other terminal
// value is left untouched until the watcher learns it.
//
// reconcileTaskColumn returns the task's new Kanboard column id and whether
// the column changed. The caller updates its in-memory task copy so the
// firing loop sees the post-reconciliation column.
func (w *watcher) reconcileTaskColumn(ctx context.Context, task kanboard.Task, ph runPhases) (int, bool) {
	var target string
	switch ph.latestPhase {
	case criteriav1.PhaseSucceeded:
		if ph.latestClass == criteriav1.RunClassTriage {
			return task.ColumnID, false
		}
		// KB-23: judge a dev-class success by the workflow's verdict, not
		// the phase — the runner job exits 0 even when the workflow ended
		// in its failure terminal.
		switch ph.latestFinalState {
		case finalStateFailed, finalStateAwaitingHuman:
			// The work is not complete. A Done stamp that predates the
			// verdict — applied by a poll that saw the Job-derived
			// Succeeded phase while the castle observation had not yet
			// landed the terminal state — must be repaired into the
			// review column; every other column reflects the workflow's
			// own bookkeeping or the route's re-fire trigger and is left
			// untouched.
			if task.ColumnName != doneColumnName {
				return task.ColumnID, false
			}
			target = reviewColumnName
		case finalStateHandlerComplete:
			// handler_complete delivered the work — and since the KB-49
			// route guard the workflow itself refuses a handler success with
			// an empty pr_url (flag_missing_pr_url fails the run loudly), a
			// handler_complete verdict implies a merged PR. KB-101 removed
			// Status.PRNumber (the speculative castle pr_url consumption —
			// no castle build ever published run.metadata, so the field was
			// permanently empty and could never gate anything): the verdict
			// plus the workflow's own KB-49 guard are the PR evidence now.
			target = doneColumnName
		case "":
			// KB-50: an empty verdict is an unverified run — the workflow's
			// terminal has not been observed yet (castle observation lag), or
			// will not be (record-derived terminal, castle observation
			// disabled). The Job-derived Succeeded phase is exactly the
			// signal that masks a failure (the engine exits 0 for any
			// terminal), so the watcher must not stamp Done off the pod
			// phase alone; the verdict lands on a later poll and re-fires
			// this reconcile, and a Done stamp predating a failure verdict
			// is still repaired by the branch above.
			w.log.Info("workflow verdict not observable on the run; not marking Done",
				"ticket", task.Identifier(), "run", ph.latestRun)
			return task.ColumnID, false
		default:
			// An unrecognized terminal value (a future terminal this
			// watcher predates): abstain rather than silently stamp Done
			// over a verdict the watcher does not understand.
			return task.ColumnID, false
		}
	case criteriav1.PhaseFailed:
		target = reviewColumnName
	default:
		target = workColumnName
	}
	if target == "" || target == task.ColumnName {
		return task.ColumnID, false
	}
	newCol, err := w.kb.MoveTaskToColumn(ctx, task.ID, target)
	if err != nil {
		w.log.Error(err, "moving task column", "ticket", task.Identifier(), "target", target)
		return task.ColumnID, false
	}
	w.log.Info("moved task column", "ticket", task.Identifier(), "from", task.ColumnName, "to", target)
	return newCol, true
}

// buildCriteriaRun stamps a CriteriaRun for a Kanboard task. TicketID is
// KB-<id>; the workflow and its volumes/secrets are stamped from the routes
// selection exactly as the linear watcher stamps them (CRI-222 contract);
// the per-repo config pin consults the pinned CM at stamping time so
// spec.configRef carries its resourceVersion when it exists.
func (w *watcher) buildCriteriaRun(ctx context.Context, task kanboard.Task, repoURL string, sel *routes.Selection) *criteriav1.CriteriaRun {
	ticketID := task.Identifier()
	name := fmt.Sprintf("%s-%d", strings.ToLower(ticketID), time.Now().Unix())
	spec := criteriav1.CriteriaRunSpec{
		TicketID:         ticketID,
		RepoURL:          repoURL,
		PerScopeSessions: true,
		Workflow:         convertWorkflow(sel.Name, sel.Workflow),
		BuildCmd:         w.buildCmd,
		TestCmd:          w.testCmd,
		CIGateCmd:        w.ciGateCmd,
		MaxAgentVisits:   w.maxAgentVisits,
		ProviderBaseURL:  w.providerBaseURL,
	}
	// KB-103: per-repo config outranks the watcher-level fallback flags
	// entry-by-entry; spec.configRef pins the ConfigMap the operator
	// renders (fail-closed on drift).
	execNS := w.namespace
	if sel.Workflow.Namespace != "" {
		execNS = sel.Workflow.Namespace
	}
	cfgRef, pinErr := runstamp.PinConfigRef(ctx, w.client, execNS, sel)
	if pinErr != nil {
		w.log.Error(pinErr, "per-repo config ConfigMap read failed at stamping; stamping a name-only pin",
			"ticket", ticketID, "configMap", sel.ConfigName)
	}
	runstamp.StampRepoConfig(&spec, sel, cfgRef)
	runstamp.StampWorkflowSource(&spec, sel.Workflow)
	return &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: w.namespace,
			Labels: map[string]string{
				"ticket":                         strings.ToLower(ticketID),
				"app.kubernetes.io/managed-by":   "criteria-kanboard-watcher",
				"criteria.brokenbots.dev/source": "kanboard",
			},
		},
		Spec: spec,
	}
}

// columnNameFor resolves a column id to its title via the cached project map.
func (w *watcher) columnNameFor(ctx context.Context, projectID, columnID int) string {
	cols, err := w.kb.GetColumns(ctx, projectID)
	if err != nil {
		return ""
	}
	for title, id := range cols {
		if id == columnID {
			return title
		}
	}
	return ""
}

// convertWorkflow deep-copies a resolved routes workflow object into the
// CriteriaRun spec, identical to the linear watcher's stamping (CRI-222).
func convertWorkflow(name string, wf routes.Workflow) *criteriav1.RunWorkflow {
	out := &criteriav1.RunWorkflow{
		Name:      name,
		Type:      wf.Type,
		Namespace: wf.Namespace,
		Class:     workflowClass(wf),
		Image:     wf.Image,
		URL:       wf.URL,
		Ref:       wf.Ref,
	}
	if wf.Env != nil {
		out.Env = make(map[string]string, len(wf.Env))
		for k, v := range wf.Env {
			out.Env[k] = v
		}
	}
	if wf.AdapterImages != nil {
		out.AdapterImages = make(map[string]string, len(wf.AdapterImages))
		for k, v := range wf.AdapterImages {
			out.AdapterImages[k] = v
		}
	}
	if len(wf.Volumes) > 0 {
		out.Volumes = make([]criteriav1.RunWorkflowVolume, 0, len(wf.Volumes))
		for _, v := range wf.Volumes {
			out.Volumes = append(out.Volumes, criteriav1.RunWorkflowVolume{
				Name:       v.Name,
				Kind:       v.Kind,
				MountPath:  v.MountPath,
				SubPath:    v.SubPath,
				ReadOnly:   v.ReadOnly,
				Claim:      v.Claim,
				Server:     v.Server,
				Path:       v.Path,
				SizeLimit:  v.SizeLimit,
				SecretName: v.SecretName,
				Env:        copyStringMap(v.Env),
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

// workflowClass resolves the admission queue class (CRI-242): an omitted
// class defaults to dev.
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

// repoTagPrefix marks the tag that binds a task to a repository: a
// "repo:<owner>/<name>" tag is the per-ticket repo binding (the Kanboard
// analogue of the Linear repo-label fallback) and wins over any repo
// reference found in the title/description and over the configured
// default. Without it a ticket's description text decides the repo, which
// mis-scoped KB-1 to the engine repo when its work belonged to
// workflow-example.
const repoTagPrefix = "repo:"

// extractRepoURL binds the task to a repository: an explicit
// "repo:<owner>/<name>" tag wins; otherwise the GitHub repo reference in
// the task title/description; then defaultRepoURL.
func extractRepoURL(task kanboard.Task, defaultRepoURL string, validate func(string) bool) string {
	for _, tag := range task.Tags {
		if rest, ok := strings.CutPrefix(tag, repoTagPrefix); ok {
			m := repoPattern.FindStringSubmatch(rest)
			if m != nil {
				return m[1]
			}
			if validate == nil || validate(strings.TrimPrefix(tag, repoTagPrefix)) {
				return strings.TrimPrefix(tag, repoTagPrefix)
			}
		}
	}
	candidate := task.Title + "\n" + task.Description
	if m := repoPattern.FindStringSubmatch(candidate); m != nil {
		return m[1]
	}
	if defaultRepoURL != "" && (validate == nil || validate(defaultRepoURL)) {
		return defaultRepoURL
	}
	return ""
}

var repoPattern = regexp.MustCompile(`(?:https?://)?github\.com/([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)`)
