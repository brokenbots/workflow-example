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

	"github.com/brokenbots/workflow-example/criteria-k8s/internal/binding"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/kanboard"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/routes"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/runstamp"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
)

// Board column names the watcher moves tasks between as runs progress.
// They are the automation columns plus the states the workflows set:
// a run in flight leaves the card's column untouched (KB-302: park-only
// mechanics — the watcher fires runs from the route trigger columns and
// parks failed runs in reviewColumnName for a human; it owns no other
// mutation), a succeeded dev-class run moves it to doneColumnName. A
// succeeded triage-class run is the middle of the chain,
// not its end: the triage workflow's re-arm path moves the ticket into the
// develop route's trigger column and the watcher leaves that move in place
// (KB-9). A dev-class success is judged by the workflow verdict stamped on
// the run (KB-23): a failed/need_assistance verdict repairs a Done stamp
// into the review column and otherwise leaves the ticket where the
// workflow's own bookkeeping put it. Even a success verdict does not stamp
// Done on its own (KB-50): Done is the confirmation of delivered work, and
// the watcher requires a PR recorded on the run before it confirms one.
const (
	defaultPollInterval = 60 * time.Second
	defaultPollTimeout  = 5 * time.Minute
	defaultHealthAddr   = ":8081"
	reviewColumnName    = "Review"
	doneColumnName      = "Done"

	// Workflow terminal state names (RunCompleted.final_state) the watcher
	// judges a dev-class success by (KB-23). handler_complete delivered
	// the work; need_assistance handed the ticket to a human with the
	// bookkeeping intact; failed is the workflow's failure verdict. The
	// triage workflow ends in need_assistance/ready_for_development but is
	// excluded from the verdict branch by its class.
	// KB-50: a judgeable verdict is necessary but no longer sufficient for
	// the Done move — even handler_complete needs the run to carry a PR,
	// because a Completed pod is not a succeeded workflow.
	// KB-269: the trees' human-handoff terminal is need_assistance now.
	// The legacy awaiting_human spelling stays admitted during the
	// transition window (old pinned trees still declare it): both spellings
	// are accepted by the reconcile membership check (isHumanHandoffFinalState);
	// no normalization is performed.
	finalStateHandlerComplete = "handler_complete"
	finalStateFailed          = "failed"
	finalStateNeedAssistance  = "need_assistance"
	finalStateAwaitingHuman   = "awaiting_human"
)

// isHumanHandoffFinalState reports whether a workflow verdict parked the
// ticket with a human (the work is not complete): the need_assistance
// terminal or its legacy awaiting_human spelling (old pinned trees still
// declare awaiting_human until their pins are re-armed; KB-269).
func isHumanHandoffFinalState(finalState string) bool {
	return finalState == finalStateNeedAssistance || finalState == finalStateAwaitingHuman
}

// nowUnix is the run-name timestamp seam; tests override it so two runs
// fired within the same second can carry distinct names.
var nowUnix = func() int64 { return time.Now().Unix() }

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
	baseBranch        = flag.String("base-branch", getenv("BASE_BRANCH", ""), "Default branch runs fork from, validate against, and open PRs against (main when empty); overridden by the selected config entry")
	defaultRepoURL    = flag.String("default-repo-url", getenv("DEFAULT_REPO_URL", ""), "Default repo URL when the Kanboard task does not reference one")
	routesFile        = flag.String("routes-file", getenv("CRITERIA_ROUTES_FILE", routes.DefaultFile), "Routes payload file (mounted from the criteria-routes ConfigMap); re-read every poll")
	workflowsTagGroup = flag.String("kanboard-workflows-tag-group", getenv("KANBOARD_WORKFLOWS_TAG_GROUP", "workflows"), "Tag group whose tags name a workflow overriding the route's project default (empty disables overrides)")
	configsTagGroup   = flag.String("kanboard-configs-tag-group", getenv("KANBOARD_CONFIGS_TAG_GROUP", "configs"), "Tag group whose tags name a per-repo config (KB-103 configLibrary entry) for tasks whose route carries no configRef (empty disables the overrides)")
	routingTags       = flag.String("kanboard-routing-tags", getenv("KANBOARD_ROUTING_TAGS", ""), "Comma-separated task tags that are routing signals (e.g. component tags), never workflow-override candidates; they join the built-in exemption filter (empty disables the extra exemptions)")
)

// watcherScheme builds the scheme main() serves the watcher's k8s client
// with. Every kind any read Gets by value must be registered here: a typed
// Get of an unregistered kind fails before any API call with "no kind is
// registered for the type ..." (CRI-264). This is CRI-264's wiring
// regression shape — the tests that matter build their own scheme with
// every kind registered, so only exercising the production scheme itself
// catches a missing registration. KB-250 adds the client scheme (corev1):
// PinConfigRef reads the per-repo config ConfigMap by value, and its
// failure degrades CI gate stamping to the bare routes value — the
// sh: make: not found class.
func watcherScheme() (*runtime.Scheme, error) {
	scheme := runtime.NewScheme()
	if err := criteriav1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("registering CriteriaRun scheme: %w", err)
	}
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("registering k8s client scheme (corev1 ConfigMap): %w", err)
	}
	return scheme, nil
}

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

	scheme, err := watcherScheme()
	if err != nil {
		logger.Error(err, "building watcher scheme")
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
		baseBranch:        *baseBranch,
		defaultRepoURL:    *defaultRepoURL,
		routesFile:        *routesFile,
		workflowsTagGroup: *workflowsTagGroup,
		configsTagGroup:   *configsTagGroup,
		routingTags:       parseTagList(*routingTags),
		repoValidator:     DefaultRepoValidator(nil, githubToken, ""),
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
	baseBranch        string
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
	// lastDeprecationMemo dedupes the KB-313 deprecation warnings across
	// polls: the payload re-reads every poll, so a migration-window CM
	// carrying stale global run-config surfaces would otherwise repeat the
	// same warning on every 60s cycle forever.
	lastDeprecationMemo string
	// lastRuns records, per ticket, the runs index entry seen at the
	// previous poll, so reconciliation only acts on changes (the CRI-252
	// change-driven discipline). The run identity is part of the record:
	// two settled runs of different classes can both read Succeeded — the
	// triage run handing off to the dev run (KB-9) — and the second settle
	// must still reconcile. In-memory only.
	lastRuns map[string]runPhases
	// lastCards records, per ticket, the Kanboard column id seen at the
	// previous poll, so the firing loop fires on ENTRY into a trigger
	// column, not on residency (KB-302). With park-only reconciliation a
	// settled run leaves the card parked on the trigger column it fired
	// from (no verdict → abstain); without this memo every later poll
	// would re-fire it — the exact refire churn KB-302 parks. The card
	// moving columns again — including the shepherd's re-arm move back to
	// a trigger column — is what earns a fire. In-memory only.
	lastCards map[string]int
	log       logr.Logger
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

// reportBindingDeprecations surfaces the KB-313 (ADR 307) deprecation
// warnings the routes payload carries: the migration window is
// allow-with-warning, and this intake is the operator's running view of the
// shipped CM, so its stale global run-config surfaces are logged (with the
// workflow entries still carrying them) whenever the warning set changes —
// once per change, not once per poll. The post-cutover case never reaches
// here as a warning: LoadFile hard-rejects that payload and poll fails
// closed above.
func (w *watcher) reportBindingDeprecations(p *routes.Payload) {
	warnings := p.DeprecationWarnings()
	if len(warnings) == 0 {
		w.lastDeprecationMemo = ""
		return
	}
	memo := strings.Join(warnings, "\n")
	if memo == w.lastDeprecationMemo {
		return
	}
	w.lastDeprecationMemo = memo
	w.log.Info("routes payload carries KB-313 deprecated global run-config surfaces; migrate the entries' projects onto per-project bindings (binding-first, KB-311) before the cut-overs make them a validation error",
		"surfaces", strings.Join(warnings, " | "))
}

// poll: routes fail closed, one
// listing read per poll, the runs index gates firing, in-flight duplicates
// suppressed. Kanboard tags drive the workflow override; the trigger
// tag gates cluster compute.
func (w *watcher) poll(ctx context.Context) error {
	routesPayload, err := routes.LoadFile(w.routesFile)
	if err != nil {
		w.log.Error(err, "loading routes payload; failing closed and skipping poll", "routesFile", w.routesFile)
		// Failing the poll (KB-22): a nil here would count as a successful
		// poll and keep the pod Ready through a persistent routes outage.
		return fmt.Errorf("load routes payload: %w", err)
	}
	w.reportBindingDeprecations(routesPayload)
	// Project scoping: query only the
	// watched project's routes' declared columns; other projects' column
	// names never reach the Kanboard query.
	states := routesPayload.ProjectStates(w.projectName)
	w.log.V(1).Info("polling kanboard for task columns declared by routes", "states", states)

	// KB-311 (ADR 307): resolve this watcher's project through the registry
	// — the only ticket->project routing table. A cutover-done entry routes
	// the project's runs into its own namespace, where admission reads the
	// project-owned criteria-binding CM instead of the global
	// workflow-library data.
	projectEntry, ok := routesPayload.Project(w.projectName)
	if !ok {
		// The registry is optional (pre-ADR-307 payloads); an absent entry
		// keeps the pre-cutover global-library behavior.
		projectEntry = routes.ProjectEntry{}
	}
	bindingNS := ""
	cutover := (*routes.ProjectEntry)(nil)
	if ok && projectEntry.IsCutoverDone() {
		bindingNS = projectEntry.Namespace
		cutover = &projectEntry
		w.log.V(1).Info("project registry: cutover done; runs serve from the project namespace through its binding CM",
			"project", w.projectName, "namespace", bindingNS)
	}

	// Runs index: one CriteriaRun list per poll backs both the firing gate
	// and phase reconciliation. Only this watcher's runs (source=kanboard)
	// are indexed. Runs live in the project namespace, so both listings
	// are indexed.
	runs, err := w.indexRunPhases(ctx, bindingNS)
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
	for ticketID := range w.lastCards {
		if _, ok := runs[ticketID]; !ok {
			delete(w.lastCards, ticketID)
		}
	}

	for _, task := range tasks {
		ticketID := task.Identifier()
		// KB-302 entry memo: record the card's column every poll, before
		// anything route-related, so the refire gate below compares against
		// the true previous column (a card parked in Review by a failure
		// and re-armed back to a trigger column counts as an entry). The
		// task copy is already post-reconciliation: reconcileTaskColumn's
		// moves are reflected in this id.
		prevCardCol, cardKnown := w.lastCards[ticketID]
		if w.lastCards == nil {
			w.lastCards = make(map[string]int)
		}
		w.lastCards[ticketID] = task.ColumnID
		// Trigger tag gates cluster compute (analogue of the k8s-run label).
		if w.triggerTag != "" && !slices.Contains(task.Tags, w.triggerTag) {
			continue
		}
		// Route lookup: project name, column (state analogue), tags. Kanboard
		// has no label groups, so a tag is treated as a workflow
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
		//
		// KB-154: tag-group config extends the same payload-driven rule to
		// tag-subset entries — a tag matching an entry's declared tag set
		// (e.g. "v0.6.0" on an entry tagged v0.6.0-release targeting) also
		// maps to the configs group. Selection still fails closed: a
		// configs-group tag that matches nothing is ErrUnknownConfig.
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
				if routesPayload.ConfigsTagGroupMember(tag) {
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
		// KB-302: fire on entry into the trigger column, not residency. With
		// park-only reconciliation a settled run with no observable verdict
		// leaves the card parked on the trigger column; firing on residency
		// would re-fire it every poll until a verdict lands. A refire
		// therefore needs the card to change columns again — the shepherd's
		// re-arm move is the refire decision (KB-302 ownership), and a
		// first-sighting of a card counts as entry.
		if cardKnown && prevCardCol == task.ColumnID {
			w.log.V(1).Info("skipping task: card has not entered this column since the last poll",
				"ticket", ticketID, "column", task.ColumnName)
			continue
		}
		run := w.buildCriteriaRun(ctx, task, repoURL, sel, cutover)
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
//     reads (the bypass exemption label);
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
// in the watcher's namespace plus — when a cutover-done project namespace
// is in play (KB-311) — that namespace too, groups them by ticket, and
// returns the phase summary poll() needs to reconcile Kanboard columns and
// gate firing: whether any run of the ticket is still in flight, and the
// most recent run's phase/final state. The KB source label isolates runs
// by ticket source so coexisting watchers never collide. The ticket's
// most recent run (creation time, name tiebreak) is the reconciliation
// authority, so its phase and admission class drive the column moves.
func (w *watcher) indexRunPhases(ctx context.Context, bindingNS string) (map[string]runPhases, error) {
	type ticketRuns struct {
		active bool
		latest criteriav1.CriteriaRun
	}
	groups := make(map[string]*ticketRuns)
	accumulate := func(ns string) error {
		list := &criteriav1.CriteriaRunList{}
		req := client.ListOptions{
			Namespace:     ns,
			LabelSelector: labels.SelectorFromSet(map[string]string{"criteria.brokenbots.dev/source": "kanboard"}),
		}
		if err := w.client.List(ctx, list, &req); err != nil {
			return err
		}
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
		return nil
	}
	if err := accumulate(w.namespace); err != nil {
		return nil, err
	}
	if bindingNS != "" && bindingNS != w.namespace {
		// KB-311: post-cutover runs live in the project namespace; a settled
		// pre-cutover run and an active post-cutover one must reconcile
		// against each other.
		if err := accumulate(bindingNS); err != nil {
			return nil, err
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
		if ph.latestPhase == criteriav1.PhaseFailed {
			ph.failureReason = failedRunReason(&g.latest)
		}
		out[ticket] = ph
	}
	return out, nil
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
	// failureReason is the latest run's failure explanation, filled only
	// when the run read Failed (KB-302): specific controller verdict
	// conditions first, then the workflow verdict, else "no reason
	// recorded". The Review-park comment is built from it exactly once.
	failureReason string
}

// reconcileTaskColumn moves the Kanboard task between board columns as its
// latest run progresses:
//   - run in flight        -> column untouched: wait (KB-302 park-only —
//     nothing mutates the card while the run runs)
//   - dev run Succeeded    -> task per the workflow verdict; the Done move
//     requires PR evidence too (KB-23, KB-50)
//   - triage run Succeeded -> column untouched (KB-9)
//   - run Failed           -> task to the review column (human), with one
//     accurate failure comment posted exactly once, gated on the move
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
// "need_assistance" verdict means the work is not complete — the develop
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
// KB-302 park-only mechanics: the watcher's column ownership reduces to the
// route trigger columns (fire), Review (failed park) and Done (verified
// success). Pending/Running/unknown-phase observations mutate nothing — the
// old default moved the task into the work column on every in-flight poll,
// but the work column matches no route trigger, so the move bought nothing
// and clobbered whatever bookkeeping the card carried mid-flight. A Failed
// run parks the card in Review exactly once — the "already there" gate
// makes the park idempotent — and posts a single failure comment at that
// move, built from the run's most specific failure explanation
// (failedRunReason). The comment fires only at the move, so one failure
// never leaves more than one watcher comment on the card.
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
		switch {
		case isHumanHandoffFinalState(ph.latestFinalState) || ph.latestFinalState == finalStateFailed:
			// KB-269: the need_assistance terminal and its legacy
			// awaiting_human spelling both mean a human holds the ticket.
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
		case ph.latestFinalState == finalStateHandlerComplete:
			// handler_complete delivered the work — and since the KB-49
			// route guard the workflow itself refuses a handler success with
			// an empty pr_url (flag_missing_pr_url fails the run loudly), a
			// handler_complete verdict implies a merged PR. KB-101 removed
			// Status.PRNumber (the speculative castle pr_url consumption —
			// no castle build ever published run.metadata, so the field was
			// permanently empty and could never gate anything): the verdict
			// plus the workflow's own KB-49 guard are the PR evidence now.
			target = doneColumnName
		case ph.latestFinalState == "":
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
		// KB-302: the Failed park is the one mutation a failure earns the
		// card — into Review, once, with one accurate comment. There is no
		// automated refire behind this move: Review matches no route trigger
		// column, and refire decisions belong to the shepherd.
		target = reviewColumnName
	default:
		// KB-302: park-only. A run in flight (Pending, Running, or the
		// phases this watcher predates) means wait — the watcher fires runs
		// from the route trigger columns and owns the failure park, not
		// in-flight card moves.
		return task.ColumnID, false
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
	if ph.latestPhase == criteriav1.PhaseFailed && target == reviewColumnName {
		w.postFailureParkComment(ctx, task, ph)
	}
	return newCol, true
}

// failedRunReason picks the failed run's most specific failure explanation
// for the Review-park comment (KB-302). Specific controller verdict
// conditions win, in fixed precedence so the choice stays deterministic
// (StallWatchdog > BaseImageMismatch > ConfigRenderDrift — see
// api/v1/criteriarun_types.go; their truthy Message is the explanation);
// otherwise the run's workflow verdict ("failed" / "need_assistance");
// otherwise "no reason recorded" — a comment is always posted, and a run
// that explains nothing says so instead of staying silent. Only the
// controller's own condition messages are consumed; card content from
// Kanboard comments is never echoed back (it could carry anything).
func failedRunReason(run *criteriav1.CriteriaRun) string {
	for _, condType := range []string{
		criteriav1.ConditionStallWatchdog,
		criteriav1.ConditionBaseImageMismatch,
		criteriav1.ConditionConfigRenderDrift,
	} {
		for i := range run.Status.Conditions {
			c := &run.Status.Conditions[i]
			if c.Type == condType && c.Status == metav1.ConditionTrue && strings.TrimSpace(c.Message) != "" {
				return c.Message
			}
		}
	}
	if run.Status.FinalState != "" {
		return fmt.Sprintf("workflow terminal %q", run.Status.FinalState)
	}
	return "no reason recorded"
}

// postFailureParkComment posts the single failure explanation on the ticket
// when the watcher parks it in Review over a Failed run (KB-302). It fires
// only at the Review move, so the card gets exactly one watcher comment per
// failure. Best-effort like the pin warning: a failed comment post is
// logged for the scrape, never propagated — the park already happened and
// must not be undone by a comment failure (the card may still carry the
// workflow's own failure bookkeeping comment).
func (w *watcher) postFailureParkComment(ctx context.Context, task kanboard.Task, ph runPhases) {
	body := fmt.Sprintf("criteria-kanboard-watcher: run %s failed: %s",
		ph.latestRun, ph.failureReason)
	if err := w.kb.AddComment(ctx, task.ID, body); err != nil {
		w.log.Error(err, "posting failure-park comment on ticket", "ticket", task.Identifier(), "run", ph.latestRun)
		return
	}
	w.log.Info("posted failure-park comment on ticket", "ticket", task.Identifier(), "run", ph.latestRun)
}

// buildCriteriaRun stamps a CriteriaRun for a Kanboard task. TicketID is
// KB-<id>; the workflow and its volumes/secrets are stamped from the routes
// selection (CRI-222 contract);
// the per-repo config pin consults the pinned CM at stamping time so
// spec.configRef carries its resourceVersion when it exists.
//
// A cutover-done project entry (KB-311, ADR 307) dispatches to the
// binding-first stamping in buildBindingFirstRun.
func (w *watcher) buildCriteriaRun(ctx context.Context, task kanboard.Task, repoURL string, sel *routes.Selection, cutover *routes.ProjectEntry) *criteriav1.CriteriaRun {
	if cutover != nil {
		return w.buildBindingFirstRun(ctx, task, repoURL, sel, cutover)
	}
	ticketID := task.Identifier()
	name := fmt.Sprintf("%s-%d", strings.ToLower(ticketID), nowUnix())
	spec := criteriav1.CriteriaRunSpec{
		TicketID:         ticketID,
		RepoURL:          repoURL,
		PerScopeSessions: true,
		Workflow:         convertWorkflow(sel.Name, sel.Workflow, sel.ConflictPolicy),
		BuildCmd:         w.buildCmd,
		TestCmd:          w.testCmd,
		CIGateCmd:        w.ciGateCmd,
		BaseBranch:       w.baseBranch,
		MaxAgentVisits:   w.maxAgentVisits,
		ProviderBaseURL:  w.providerBaseURL,
	}
	// KB-103: per-repo config outranks the watcher-level fallback flags
	// entry-by-entry; spec.configRef pins the ConfigMap the operator
	// renders (fail-closed on drift). KB-154: the same stamp carries the
	// entry's baseBranch onto the spec.
	//
	// KB-250 no-silent-degrade: a pin read failure (unreadable CM; NotFound
	// already yields a name-only pin without an error) keeps the run
	// flowing on the name-only pin — the operator renders it at job-build
	// time — but the degradation is never silent: the watcher logs it AND
	// surfaces a warning comment on the ticket (best-effort; a failed
	// comment post is logged, not propagated — a warning must not gate the
	// fire). It never stamps a run without the config entry's pin.
	execNS := w.namespace
	if sel.Workflow.Namespace != "" {
		execNS = sel.Workflow.Namespace
	}
	cfgRef, pinErr := runstamp.PinConfigRef(ctx, w.client, execNS, sel)
	if pinErr != nil {
		w.log.Error(pinErr, "per-repo config ConfigMap read failed at stamping; stamping a name-only pin",
			"ticket", ticketID, "configMap", sel.ConfigName)
		w.postPinWarning(ctx, task.ID, ticketID, sel.ConfigName, pinErr)
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

// postPinWarning surfaces the pin-read degradation on the ticket card so
// it is visible where the work is tracked ("per-repo config ConfigMap read
// failed at stamping" must not live only in the watcher log, KB-250
// no-silent-degrade). Best-effort: a failed comment post is logged for the
// log scrape instead of failing the fire — a warning must not gate the
// fire it warns about.
func (w *watcher) postPinWarning(ctx context.Context, taskID int, ticketID, cmName string, pinErr error) {
	w.postWarning(ctx, taskID, ticketID, fmt.Sprintf(
		"criteria-kanboard-watcher: the per-repo config ConfigMap %s could not be pinned at stamp time (%v); the run is stamped with a name-only pin and the operator renders the ConfigMap at job-build time",
		cmName, pinErr))
}

// postWarning posts a best-effort degraded-mode warning comment on the
// ticket card (KB-250 no-silent-degrade): the degradation is already
// carried by the stamped run, so a failed comment post is logged for the
// log scrape instead of failing the fire — a warning must not gate the
// fire it warns about.
func (w *watcher) postWarning(ctx context.Context, taskID int, ticketID, body string) {
	if err := w.kb.AddComment(ctx, taskID, body); err != nil {
		w.log.Error(err, "posting degraded-stamp warning comment on ticket", "ticket", ticketID)
		return
	}
	w.log.Info("posted degraded-stamp warning comment on ticket", "ticket", ticketID)
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
// CriteriaRun spec (CRI-222). conflictPolicy is the KB-319
// adapter-image conflict policy the resolver selected for the ticket's
// project (Selection.ConflictPolicy).
func convertWorkflow(name string, wf routes.Workflow, conflictPolicy string) *criteriav1.RunWorkflow {
	out := &criteriav1.RunWorkflow{
		Name:      name,
		Type:      wf.Type,
		Namespace: wf.Namespace,
		Class:     workflowClass(wf),
		Image:     wf.Image,
		URL:       wf.URL,
		Ref:       wf.Ref,
		Interface: convertWorkflowInterface(wf.Interface),
		// The conflict policy is STAMPED AS-SELECTED, unconditionally: the
		// conflict vocabulary is non-empty by construction (the resolver
		// always fills it, fail-closed included), so an empty value would
		// only be a silent skip of the KB-319 consult.
		ImageConflictPolicy: conflictPolicy,
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

// convertWorkflowInterface deep-copies a routes interface declaration
// (KB-310): the operator validates the project binding against it before
// job creation and records its checksum in the launch manifest.
func convertWorkflowInterface(in *routes.WorkflowInterface) *criteriav1.RunWorkflowInterface {
	if in == nil {
		return nil
	}
	return &criteriav1.RunWorkflowInterface{
		APIVersion:  in.APIVersion,
		Kind:        in.Kind,
		Workflow:    in.Workflow,
		Description: in.Description,
		Adapters:    copyStringSlice(in.Adapters),
		Mounts:      copyInterfaceMounts(in.Mounts),
		SecretEnv:   copyStringSlice(in.SecretEnv),
		Env:         copyStringSlice(in.Env),
		ConfigSlots: copyInterfaceConfigSlots(in.ConfigSlots),
		// KB-319: the declared admission-lock key rides the stamped
		// interface so the operator's derivation reads the tree's
		// declaration, not a default.
		LockKey: in.LockKey,
	}
}

// convertBindingRefWorkflow stamps the workflow TREE references onto a
// cutover-done project's CriteriaRun (KB-311, ADR 307): name, type, url+ref,
// class, and the interface declaration (the secrets lane lives there).
//
// Everything the ADR hands to the project binding stays OFF the CR — env,
// adapter images, volumes, secrets — so the CR carries binding REFERENCES
// only and the project's criteria-binding CM owns the execution config.
// The workflow declaration's namespace is deliberately not stamped: the
// run executes in the project namespace, so jobbuilder.TargetNamespace
// must resolve through the run's own namespace, not the global
// workflow-library one. conflictPolicy is the KB-319 adapter-image
// conflict policy (Selection.ConflictPolicy); the binding materialization
// replaces the adapter-image lane post-cutover, but the policy governs
// the pinned-vs-verified disagreement the same way on both lanes.
func convertBindingRefWorkflow(name string, wf routes.Workflow, conflictPolicy string) *criteriav1.RunWorkflow {
	return &criteriav1.RunWorkflow{
		Name:                name,
		Type:                wf.Type,
		Class:               workflowClass(wf),
		Image:               wf.Image,
		URL:                 wf.URL,
		Ref:                 wf.Ref,
		Interface:           convertWorkflowInterface(wf.Interface),
		ImageConflictPolicy: conflictPolicy,
	}
}

// buildBindingFirstRun stamps a cutover-done project's CriteriaRun
// (KB-311, ADR 307): the CR lands in the registry entry's namespace and
// carries ONLY references — the workflow tree (tree digest unchanged: same
// name/type/url/ref/class/interface), a pin on the project-owned
// criteria-binding ConfigMap, and the same watcher-contract fields the
// pre-cutover stamper sets.
//
// The KB-103 spec-field stamping (buildCmd/testCmd/ciGateCmd/baseBranch and
// the per-repo configRef pin) is skipped: the project binding's config
// slots serve those values at admission, and a KB-103 spec-field stamp
// would collide with binding-owned config at admission (undeclared-slot
// refusal). The pre-cutover paths keep KB-103; the binding-first path
// replaces it.
//
// KB-250 no-silent-degrade: the binding CM read at stamp time pins its
// resourceVersion so admission proves the binding did not change between
// stamp and admit. A read failure degrades to the name-only pin with a
// card-comment warning — the degradation is never silent — and admission
// still fails closed on the ref-less read, so nothing is lost beyond
// provenance.
func (w *watcher) buildBindingFirstRun(ctx context.Context, task kanboard.Task, repoURL string, sel *routes.Selection, entry *routes.ProjectEntry) *criteriav1.CriteriaRun {
	ticketID := task.Identifier()
	name := fmt.Sprintf("%s-%d", strings.ToLower(ticketID), nowUnix())
	spec := criteriav1.CriteriaRunSpec{
		TicketID:          ticketID,
		RepoURL:           repoURL,
		PerScopeSessions:  true,
		MaxAgentVisits:    w.maxAgentVisits,
		ProviderBaseURL:   w.providerBaseURL,
		Workflow:          convertBindingRefWorkflow(sel.Name, sel.Workflow, sel.ConflictPolicy),
		ProjectBindingRef: &criteriav1.CriteriaRunProjectBindingRef{Name: binding.CMName},
	}
	var cm corev1.ConfigMap
	if err := w.client.Get(ctx, client.ObjectKey{Namespace: entry.Namespace, Name: binding.CMName}, &cm); err != nil {
		w.log.Error(err, "binding ConfigMap read failed at stamping; stamping a name-only project binding pin",
			"ticket", ticketID, "namespace", entry.Namespace, "configMap", binding.CMName)
		w.postWarning(ctx, task.ID, ticketID, fmt.Sprintf(
			"criteria-kanboard-watcher: the project binding ConfigMap %s in namespace %s could not be pinned at stamp time (%v); the run is stamped with a name-only pin and admission re-reads and proves the binding before job build",
			binding.CMName, entry.Namespace, err))
	} else {
		spec.ProjectBindingRef.ResourceVersion = cm.ResourceVersion
	}
	runstamp.StampWorkflowSource(&spec, sel.Workflow)
	return &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: entry.Namespace,
			Labels: map[string]string{
				"ticket":                         strings.ToLower(ticketID),
				"app.kubernetes.io/managed-by":   "criteria-kanboard-watcher",
				"criteria.brokenbots.dev/source": "kanboard",
			},
		},
		Spec: spec,
	}
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

func copyStringSlice(names []string) []string {
	if names == nil {
		return nil
	}
	out := make([]string, len(names))
	copy(out, names)
	return out
}

func copyInterfaceMounts(mounts []routes.WorkflowInterfaceMount) []criteriav1.RunWorkflowInterfaceMount {
	if mounts == nil {
		return nil
	}
	out := make([]criteriav1.RunWorkflowInterfaceMount, len(mounts))
	for i, m := range mounts {
		out[i] = criteriav1.RunWorkflowInterfaceMount{
			Name:      m.Name,
			MountPath: m.MountPath,
			Kind:      m.Kind,
		}
	}
	return out
}

func copyInterfaceConfigSlots(slots []routes.WorkflowInterfaceConfigSlot) []criteriav1.RunWorkflowInterfaceConfigSlot {
	if slots == nil {
		return nil
	}
	out := make([]criteriav1.RunWorkflowInterfaceConfigSlot, len(slots))
	for i, s := range slots {
		out[i] = criteriav1.RunWorkflowInterfaceConfigSlot{
			Slot:        s.Slot,
			Variable:    s.Variable,
			Target:      s.Target,
			Description: s.Description,
		}
	}
	return out
}

// repoTagPrefix marks the tag that binds a task to a repository: a
// "repo:<owner>/<name>" tag is the per-ticket repo binding and wins over
// any repo reference found in the title/description and over the configured
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
