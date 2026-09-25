package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/brokenbots/workflow-example/criteria-k8s/internal/kanboard"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/linear"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/routes"
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
// (KB-9).
const (
	defaultPollInterval = 60 * time.Second
	workColumnName      = "Work in progress"
	reviewColumnName    = "Review"
	doneColumnName      = "Done"
)

var (
	namespace         = flag.String("namespace", getenv("NAMESPACE", "criteria-jobs"), "Namespace to watch and create CriteriaRuns in")
	kanboardURL       = flag.String("kanboard-url", getenv("KANBOARD_URL", ""), "Kanboard JSON-RPC endpoint (jsonrpc.php, subpath included)")
	kanboardToken     = flag.String("kanboard-token", getenv("KANBOARD_APP_TOKEN", ""), "Kanboard app API token (also read from /secrets/kanboard_app_token)")
	projectName       = flag.String("kanboard-project-name", getenv("KANBOARD_PROJECT_NAME", "Kanboard Tickets"), "Kanboard project to watch")
	triggerTag        = flag.String("kanboard-trigger-tag", getenv("KANBOARD_TRIGGER_TAG", "k8s-run"), "Require this tag on the task before triggering a run; empty means any task matching a route triggers")
	pollInterval      = flag.Duration("poll-interval", parseDuration(getenv("POLL_INTERVAL", "60s")), "How often to poll Kanboard")
	image             = flag.String("image", getenv("CRITERIA_IMAGE", "localhost:5000/linear-intake-remote:dev"), "Default Criteria workflow image")
	providerBaseURL   = flag.String("provider-base-url", getenv("PROVIDER_BASE_URL", "http://192.168.17.116:11434/v1"), "Default provider base URL")
	maxAgentVisits    = flag.Int("max-agent-visits", parseInt(getenv("MAX_AGENT_VISITS", "2"), 2), "Default max agent visits")
	buildCmd          = flag.String("build-cmd", getenv("BUILD_CMD", ""), "Default build command")
	testCmd           = flag.String("test-cmd", getenv("TEST_CMD", ""), "Default test command")
	ciGateCmd         = flag.String("ci-gate-cmd", getenv("CI_GATE_CMD", ""), "Default CI gate command")
	defaultRepoURL    = flag.String("default-repo-url", getenv("DEFAULT_REPO_URL", ""), "Default repo URL when the Kanboard task does not reference one")
	routesFile        = flag.String("routes-file", getenv("CRITERIA_ROUTES_FILE", routes.DefaultFile), "Routes payload file (mounted from the criteria-routes ConfigMap); re-read every poll")
	workflowsTagGroup = flag.String("kanboard-workflows-tag-group", getenv("KANBOARD_WORKFLOWS_TAG_GROUP", "workflows"), "Tag group whose tags name a workflow overriding the route's project default (empty disables overrides)")
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
		image:             *image,
		providerBaseURL:   *providerBaseURL,
		maxAgentVisits:    *maxAgentVisits,
		buildCmd:          *buildCmd,
		testCmd:           *testCmd,
		ciGateCmd:         *ciGateCmd,
		defaultRepoURL:    *defaultRepoURL,
		routesFile:        *routesFile,
		workflowsTagGroup: *workflowsTagGroup,
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

func parseDuration(s string) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return defaultPollInterval
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
	client            client.Client
	kb                *kanboard.Client
	namespace         string
	projectName       string
	triggerTag        string
	pollInterval      time.Duration
	image             string
	providerBaseURL   string
	maxAgentVisits    int
	buildCmd          string
	testCmd           string
	ciGateCmd         string
	defaultRepoURL    string
	routesFile        string
	workflowsTagGroup string
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
	projectID, err := w.kb.FindProjectID(ctx, w.projectName)
	if err != nil {
		return fmt.Errorf("resolve kanboard project: %w", err)
	}
	w.projectID = projectID
	w.log.Info("watching kanboard project", "project", w.projectName, "projectID", projectID, "interval", w.pollInterval)

	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()
	if err := w.poll(ctx); err != nil {
		w.log.Error(err, "poll failed")
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := w.poll(ctx); err != nil {
				w.log.Error(err, "poll failed")
			}
		}
	}
}

// poll mirrors the linear watcher's structure: routes fail closed, one
// listing read per poll, the runs index gates firing, in-flight duplicates
// suppressed. Kanboard tags are the analogue of Linear labels; the trigger
// tag gates cluster compute exactly like LINEAR_TRIGGER_LABEL.
func (w *watcher) poll(ctx context.Context) error {
	routesPayload, err := routes.LoadFile(w.routesFile)
	if err != nil {
		w.log.Error(err, "loading routes payload; failing closed and skipping poll", "routesFile", w.routesFile)
		return nil
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
		tagGroups := map[string]string{}
		for _, tag := range task.Tags {
			if w.isRoutingSignal(tag) {
				continue
			}
			tagGroups[tag] = w.workflowsTagGroup
		}
		sel, err := routesPayload.Resolve(routes.Selector{
			Project:             task.ProjectName,
			State:               task.ColumnName,
			Labels:              task.Tags,
			LabelGroups:         tagGroups,
			WorkflowsLabelGroup: w.workflowsTagGroup,
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
		run := w.buildCriteriaRun(task, repoURL, sel)
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
			active:      g.active,
			latestPhase: g.latest.Status.Phase,
			latestRun:   g.latest.Name,
			anyRun:      true,
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
	latestRun   string
	latestClass string
	anyRun      bool
}

// reconcileTaskColumn moves the Kanboard task between board columns as its
// latest run progresses — the Kanboard analogue of the Linear watcher's
// automation labels:
//   - run in flight        -> task to the work column
//   - dev run Succeeded    -> task to the done column
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
// it. Only a dev-class success stamps Done — or a legacy run predating
// class stamping (empty class), which ran the old terminal flow.
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
		target = doneColumnName
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
// selection exactly as the linear watcher stamps them (CRI-222 contract).
func (w *watcher) buildCriteriaRun(task kanboard.Task, repoURL string, sel *routes.Selection) *criteriav1.CriteriaRun {
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
	stampWorkflowSource(&spec, sel.Workflow)
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

// stampWorkflowSource resolves the source-mode spec fields from a url-type
// routes workflow object, identical to the linear watcher's stamping (CRI-231):
// spec.workflowSource carries the URL the runner fetches and applies at run
// time (plus the fail-closed ref pin), and url+image additionally stamps
// spec.image. Image-type routes leave both unset so the operator's default
// image remains the source of truth for baked workflows.
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
