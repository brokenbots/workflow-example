package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
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
	pollInterval        = flag.Duration("poll-interval", parseDuration(getenv("POLL_INTERVAL", "60s")), "How often to poll Linear")
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
	log    logr.Logger
}

func (w *watcher) run(ctx context.Context) error {
	w.log.Info("resolving Linear project", "project", w.projectName)
	projectID, err := w.linear.FindProjectID(ctx, w.projectName)
	if err != nil {
		return fmt.Errorf("resolve project: %w", err)
	}
	// CRI-219: the automation labels are team-scoped, so the watcher needs
	// the project's team before it can create or resolve them.
	teamID, err := w.linear.FindProjectTeamID(ctx, projectID)
	if err != nil {
		return fmt.Errorf("resolve project team: %w", err)
	}
	w.teamID = teamID
	w.log.Info("watching Linear project", "project", w.projectName, "projectID", projectID, "teamID", teamID, "interval", w.pollInterval)

	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()
	for {
		if err := w.poll(ctx, projectID); err != nil {
			w.log.Error(err, "poll failed")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
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
	w.log.V(1).Info("polling Linear for ticket states declared by routes", "states", states)
	issues, err := w.linear.IssuesInProjectStates(ctx, projectID, states)
	if err != nil {
		return err
	}
	// CRI-219: the automation label lifecycle is driven from the CR phases
	// observed each poll plus the issue's current label state, so it
	// converges after a watcher restart without in-memory run state.
	automationLabelID, dirtyLabelID := w.ensureAutomationLabels(ctx)
	for _, issue := range issues {
		ph, err := w.runPhases(ctx, issue.Identifier)
		if err != nil {
			w.log.Error(err, "checking CriteriaRun phases", "ticket", issue.Identifier)
			continue
		}
		// CRI-219 label reconciliation runs before the run-firing gates: it
		// only maintains labels for tickets with CriteriaRuns and must keep
		// cleaning them even when the ticket no longer passes a gate.
		w.reconcileAutomationLabels(ctx, issue, ph, automationLabelID, dirtyLabelID)
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
			w.postRoutingComment(ctx, issue, err)
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
		run := w.buildCriteriaRun(issue, repoURL, sel)
		if err := w.client.Create(ctx, run); err != nil {
			w.log.Error(err, "creating CriteriaRun", "ticket", issue.Identifier)
			continue
		}
		w.log.Info("created CriteriaRun", "ticket", issue.Identifier, "name", run.Name, "repoUrl", repoURL,
			"workflow", sel.Name, "route", sel.Route.Name)
		// CRI-219: mark the ticket as having an inflight CriteriaRun. The
		// add merges with the issue's existing labels (the client does a
		// read-modify-write, never a REPLACE), and reconciliation on later
		// polls converges the label anyway, so a failed add here is not
		// fatal.
		w.addLinearLabel(ctx, issue, automationLabelName, automationLabelID)
	}
	return nil
}

// runPhases captures what the watcher needs to know about a ticket's
// CriteriaRuns from a single list: whether any run is still in flight
// (hasActiveRun semantics, extended to count the empty phase a freshly
// created run carries until the controller sets status as in flight),
// whether any observed run failed, and whether any run exists at all. The
// zero value means no run was observed.
type runPhases struct {
	active    bool
	anyFailed bool
	anyRun    bool
}

func (w *watcher) runPhases(ctx context.Context, ticketID string) (runPhases, error) {
	list := &criteriav1.CriteriaRunList{}
	req := client.ListOptions{
		Namespace:     w.namespace,
		LabelSelector: mustSelector(map[string]string{"ticket": strings.ToLower(ticketID)}),
	}
	if err := w.client.List(ctx, list, &req); err != nil {
		return runPhases{}, err
	}
	var ph runPhases
	ph.anyRun = len(list.Items) > 0
	for _, run := range list.Items {
		switch run.Status.Phase {
		case criteriav1.PhaseFailed:
			ph.anyFailed = true
		case criteriav1.PhaseSucceeded:
			// Settled clean: nothing in flight from this run.
		default:
			// Pending, Running, Unknown — and the empty phase a freshly
			// created run carries until the controller sets status — are
			// all in flight, so the watcher neither re-fires nor strips
			// the inflight marker in that gap (CRI-219).
			ph.active = true
		}
	}
	return ph, nil
}

// ensureAutomationLabels resolves (or creates) the team-scoped automation
// labels and returns their Linear IDs. Empty IDs mean ensure failed for that
// label; label writes are then skipped (with a watcher log) until the next
// poll re-ensures, so a transient Linear hiccup never blocks run creation.
// Ensuring every poll self-heals a label deleted out-of-band and is
// idempotent: issueLabelCreate only fires when the name is missing from the
// team.
func (w *watcher) ensureAutomationLabels(ctx context.Context) (automationLabelID, dirtyLabelID string) {
	automationLabelID, err := w.linear.EnsureTeamLabel(ctx, w.teamID, automationLabelName, automationLabelColor)
	if err != nil {
		w.log.Error(err, "ensuring automation label; label lifecycle deferred to next poll", "label", automationLabelName, "teamID", w.teamID)
		automationLabelID = ""
	}
	dirtyLabelID, err = w.linear.EnsureTeamLabel(ctx, w.teamID, dirtyLabelName, dirtyLabelColor)
	if err != nil {
		w.log.Error(err, "ensuring dirty label; label lifecycle deferred to next poll", "label", dirtyLabelName, "teamID", w.teamID)
		dirtyLabelID = ""
	}
	return automationLabelID, dirtyLabelID
}

// reconcileAutomationLabels converges an issue's automation-label state with
// the observed CriteriaRun phases:
//   - runs in flight   → "criteria-automation" present,
//   - all runs settled → "criteria-automation" absent,
//   - any run failed   → "criteria-dirty" added.
//
// "criteria-dirty" is sticky: the watcher never removes it, because a failed
// run needs human attention; clearing it is an operator decision on the
// ticket. The issue's current labels are the source of truth, so this is
// idempotent and restart-safe. Tickets with no observed CriteriaRuns are left
// untouched (nothing to converge — e.g. runs garbage-collected).
func (w *watcher) reconcileAutomationLabels(ctx context.Context, issue linear.Issue, ph runPhases, automationLabelID, dirtyLabelID string) {
	if !ph.anyRun {
		return
	}
	hasAutomation := slices.Contains(issue.Labels, automationLabelName)
	if ph.active && !hasAutomation {
		w.addLinearLabel(ctx, issue, automationLabelName, automationLabelID)
	} else if !ph.active && hasAutomation {
		w.removeLinearLabel(ctx, issue, automationLabelName, automationLabelID)
	}
	if ph.anyFailed && !slices.Contains(issue.Labels, dirtyLabelName) {
		w.addLinearLabel(ctx, issue, dirtyLabelName, dirtyLabelID)
	}
}

func (w *watcher) addLinearLabel(ctx context.Context, issue linear.Issue, name, labelID string) {
	if labelID == "" {
		w.log.V(1).Info("skipping label add: label id unresolved (ensure failed this poll)", "ticket", issue.Identifier, "label", name)
		return
	}
	if err := w.linear.AddIssueLabel(ctx, issue.ID, labelID); err != nil {
		w.log.Error(err, "adding Linear label; retrying next poll", "ticket", issue.Identifier, "label", name)
		return
	}
	w.log.Info("added Linear label", "ticket", issue.Identifier, "label", name)
}

func (w *watcher) removeLinearLabel(ctx context.Context, issue linear.Issue, name, labelID string) {
	if labelID == "" {
		w.log.V(1).Info("skipping label remove: label id unresolved (ensure failed this poll)", "ticket", issue.Identifier, "label", name)
		return
	}
	if err := w.linear.RemoveIssueLabel(ctx, issue.ID, labelID); err != nil {
		w.log.Error(err, "removing Linear label; retrying next poll", "ticket", issue.Identifier, "label", name)
		return
	}
	w.log.Info("removed Linear label", "ticket", issue.Identifier, "label", name)
}

func (w *watcher) buildCriteriaRun(issue linear.Issue, repoURL string, sel *routes.Selection) *criteriav1.CriteriaRun {
	ticket := issue.Identifier
	name := fmt.Sprintf("%s-%d", strings.ToLower(ticket), time.Now().Unix())
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
		Spec: criteriav1.CriteriaRunSpec{
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
			// Image is intentionally unset: the operator's --default-image
			// (CRITERIA_IMAGE on the operator deployment) is the single
			// source of truth for the workflow image, so bumping the image
			// only requires one deployment update. A stamped workflow's
			// image/url/ref is consumed by the jobbuilder (CRI-222; CRI-231
			// adds workflowSource for url modes).
			BuildCmd:        w.buildCmd,
			TestCmd:         w.testCmd,
			CIGateCmd:       w.ciGateCmd,
			MaxAgentVisits:  w.maxAgentVisits,
			ProviderBaseURL: w.providerBaseURL,
		},
	}
}

// convertWorkflow deep-copies a resolved routes workflow object into the
// stamped CriteriaRun spec type, carrying the resolved workflow-library name.
func convertWorkflow(name string, wf routes.Workflow) *criteriav1.RunWorkflow {
	out := &criteriav1.RunWorkflow{
		Name:      name,
		Type:      wf.Type,
		Namespace: wf.Namespace,
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

	// CRI-219 automation label lifecycle. Both labels are team-scoped and
	// ensured every poll; the names and colors are fixed by the ticket so
	// every deployment observes the same marker vocabulary.
	automationLabelName  = "criteria-automation"
	dirtyLabelName       = "criteria-dirty"
	automationLabelColor = "#0ea5e9" // inflight marker: blue
	dirtyLabelColor      = "#f43f5e" // failed-run marker: red
)

// postRoutingComment posts the routing failure on the ticket, deduping by
// exact body so a stuck failure does not spam Linear across polls. When
// reading existing comments fails, posting is skipped this poll and retried
// next poll rather than risking duplicates.
func (w *watcher) postRoutingComment(ctx context.Context, issue linear.Issue, lookupErr error) {
	body := fmt.Sprintf("%s: %v", routingCommentPrefix, lookupErr)
	comments, err := w.linear.IssueComments(ctx, issue.ID, commentDedupLimit)
	if err != nil {
		w.log.Error(err, "reading Linear comments for routing-failure dedup; retrying next poll",
			"ticket", issue.Identifier)
		return
	}
	if slices.Contains(comments, body) {
		w.log.V(1).Info("routing failure already reported on ticket", "ticket", issue.Identifier)
		return
	}
	if err := w.linear.PostComment(ctx, issue.ID, body); err != nil {
		w.log.Error(err, "posting routing failure comment on ticket", "ticket", issue.Identifier)
		return
	}
	w.log.Info("posted routing failure comment on ticket", "ticket", issue.Identifier)
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
		return 60 * time.Second
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
