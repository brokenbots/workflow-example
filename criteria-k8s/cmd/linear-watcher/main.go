package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/brokenbots/workflow-example/criteria-k8s/internal/linear"
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
	namespace       = flag.String("namespace", getenv("NAMESPACE", "criteria-jobs"), "Namespace to watch and create CriteriaRuns in")
	linearAPIKey    = flag.String("linear-api-key", getenv("LINEAR_API_KEY", ""), "Linear API key (also read from /secrets/linear_api_key)")
	projectName     = flag.String("linear-project-name", getenv("LINEAR_PROJECT_NAME", "Criteria K8s Workflow Runner"), "Linear project to watch")
	triageState     = flag.String("linear-triage-state", getenv("LINEAR_TRIAGE_STATE", "Triage"), "Workflow state that triggers a run")
	pollInterval    = flag.Duration("poll-interval", parseDuration(getenv("POLL_INTERVAL", "60s")), "How often to poll Linear")
	image           = flag.String("image", getenv("CRITERIA_IMAGE", "localhost:5000/linear-intake-remote:dev"), "Default Criteria workflow image")
	providerBaseURL = flag.String("provider-base-url", getenv("PROVIDER_BASE_URL", "http://192.168.17.116:11434/v1"), "Default provider base URL")
	maxAgentVisits  = flag.Int("max-agent-visits", parseInt(getenv("MAX_AGENT_VISITS", "2"), 2), "Default max agent visits")
	buildCmd        = flag.String("build-cmd", getenv("BUILD_CMD", ""), "Default build command")
	testCmd         = flag.String("test-cmd", getenv("TEST_CMD", ""), "Default test command")
	ciGateCmd       = flag.String("ci-gate-cmd", getenv("CI_GATE_CMD", ""), "Default CI gate command")
	defaultRepoURL  = flag.String("default-repo-url", getenv("DEFAULT_REPO_URL", ""), "Default repo URL when Linear issue does not contain one")
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
	w := watcher{
		client:         k8s,
		linear:         linearClient,
		namespace:      *namespace,
		projectName:    *projectName,
		triageState:    *triageState,
		pollInterval:   *pollInterval,
		image:          *image,
		providerBaseURL: *providerBaseURL,
		maxAgentVisits: *maxAgentVisits,
		buildCmd:       *buildCmd,
		testCmd:        *testCmd,
		ciGateCmd:      *ciGateCmd,
		defaultRepoURL: *defaultRepoURL,
		log:            logger,
	}

	ctx := ctrl.SetupSignalHandler()
	if err := w.run(ctx); err != nil {
		logger.Error(err, "watcher exited")
		os.Exit(1)
	}
}

type watcher struct {
	client          client.Client
	linear          *linear.Client
	namespace       string
	projectName     string
	triageState     string
	pollInterval    time.Duration
	image           string
	providerBaseURL string
	maxAgentVisits  int
	buildCmd        string
	testCmd         string
	ciGateCmd       string
	defaultRepoURL  string
	log             logr.Logger
}

func (w *watcher) run(ctx context.Context) error {
	w.log.Info("resolving Linear project", "project", w.projectName)
	projectID, err := w.linear.FindProjectID(ctx, w.projectName)
	if err != nil {
		return fmt.Errorf("resolve project: %w", err)
	}
	w.log.Info("watching Linear project", "project", w.projectName, "projectID", projectID, "state", w.triageState, "interval", w.pollInterval)

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
	issues, err := w.linear.IssuesInProjectState(ctx, projectID, w.triageState)
	if err != nil {
		return err
	}
	for _, issue := range issues {
		repoURL := linear.ExtractRepoURL(issue, w.defaultRepoURL)
		if repoURL == "" {
			w.log.Info("skipping Linear issue without repo URL", "ticket", issue.Identifier, "title", issue.Title)
			continue
		}
		active, err := w.hasActiveRun(ctx, issue.Identifier)
		if err != nil {
			w.log.Error(err, "checking active run", "ticket", issue.Identifier)
			continue
		}
		if active {
			w.log.V(1).Info("run already active", "ticket", issue.Identifier)
			continue
		}
		run := w.buildCriteriaRun(issue, repoURL)
		if err := w.client.Create(ctx, run); err != nil {
			w.log.Error(err, "creating CriteriaRun", "ticket", issue.Identifier)
			continue
		}
		w.log.Info("created CriteriaRun", "ticket", issue.Identifier, "name", run.Name, "repoUrl", repoURL)
	}
	return nil
}

func (w *watcher) hasActiveRun(ctx context.Context, ticketID string) (bool, error) {
	list := &criteriav1.CriteriaRunList{}
	req := client.ListOptions{
		Namespace: w.namespace,
		LabelSelector: mustSelector(map[string]string{"ticket": strings.ToLower(ticketID)}),
	}
	if err := w.client.List(ctx, list, &req); err != nil {
		return false, err
	}
	for _, run := range list.Items {
		switch run.Status.Phase {
		case criteriav1.PhasePending, criteriav1.PhaseRunning, criteriav1.PhaseUnknown:
			return true, nil
		}
	}
	return false, nil
}

func (w *watcher) buildCriteriaRun(issue linear.Issue, repoURL string) *criteriav1.CriteriaRun {
	ticket := issue.Identifier
	name := fmt.Sprintf("%s-%d", strings.ToLower(ticket), time.Now().Unix())
	return &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: w.namespace,
			Labels: map[string]string{
				"ticket":                       strings.ToLower(ticket),
				"app.kubernetes.io/managed-by":   "criteria-linear-watcher",
				"criteria.brokenbots.dev/source": "linear",
			},
		},
		Spec: criteriav1.CriteriaRunSpec{
			TicketID:        ticket,
			RepoURL:         repoURL,
			Image:           w.image,
			BuildCmd:        w.buildCmd,
			TestCmd:         w.testCmd,
			CIGateCmd:       w.ciGateCmd,
			MaxAgentVisits:  w.maxAgentVisits,
			ProviderBaseURL: w.providerBaseURL,
		},
	}
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
