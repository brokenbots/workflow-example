package control

import (
	"context"
	"strings"
	"testing"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/routes"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/runspec"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/runstamp"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// parityWorkflow is the routes workflow object both lanes resolve in the
// test fixtures (the assignment names it directly; the watcher routes to
// it).
func parityWorkflow() routes.Workflow {
	return routes.Workflow{
		Type:      routes.TypeURL,
		URL:       "https://github.com/brokenbots/workflow-example.git",
		Ref:       "main",
		Class:     routes.ClassDefault,
		Namespace: "criteria-jobs",
		Env:       map[string]string{"CRITERIA_RUN_MODE": "develop"},
		AdapterImages: map[string]string{
			"shell":   "localhost:5000/shell-adapter:dev",
			"copilot": "localhost:5000/copilot-adapter:dev",
		},
	}
}

// parityPayload serves the workflow to both lanes' fixtures.
func parityPayload() *routes.Payload {
	return &routes.Payload{
		WorkflowLibrary: map[string]routes.Workflow{"kanboard-develop-url": parityWorkflow()},
	}
}

// parityDefaults are the shared run-shape defaults both fixture lanes
// stamp.
func parityDefaults() runspec.Defaults {
	return runspec.Defaults{
		BuildCmd:        "make build",
		TestCmd:         "make test",
		CIGateCmd:       "make ci",
		BaseBranch:      "main",
		MaxAgentVisits:  3,
		ProviderBaseURL: "http://provider:11434/v1",
	}
}

// parityDecision is the assignment-lane decision for the fixture ticket.
func parityDecision() Decision {
	return Decision{
		Ticket:       "kb-319",
		Project:      "castor",
		RepoURL:      "https://github.com/brokenbots/workflow-example.git",
		Class:        routes.ClassDefault,
		WorkflowName: "kanboard-develop-url",
	}
}

// watcherStamp is the watcher-lane input for the same fixture ticket (a
// refire of runspec.StampRun with the watcher's lane wiring: the shared
// stamping path buildCriteriaRun delegates to since the refactor).
func watcherStamp(name string, sel *routes.Selection, defaults runspec.Defaults, ns string) *criteriav1.CriteriaRun {
	return runspec.StampRun(runspec.StampInput{
		Name:      name,
		TicketID:  "KB-319",
		RepoURL:   "https://github.com/brokenbots/workflow-example.git",
		Selection: sel,
		Defaults:  defaults,
		Namespace: ns,
	})
}

// BuildRunMatchesWatcherStamp is the KB-319 acceptance fixture: the
// assignment-driven would-be CR, stamped by the RunBuilder through the
// shared runspec path, must be IDENTICAL (spec, namespace, labels) to the
// watcher-driven CR for the same ticket. No API writes happen on either
// lane — both CRs are in-memory objects.
func TestBuildRunMatchesWatcherStamp(t *testing.T) {
	ctx := context.Background()
	watcherRun := watcherStamp("kb-319-1770000003", &routes.Selection{
		Name:           "kanboard-develop-url",
		Workflow:       parityWorkflow(),
		ConflictPolicy: criteriav1.ImageConflictPolicyFailClosed,
	}, parityDefaults(), "criteria-jobs")
	b := &RunBuilder{
		Defaults:  parityDefaults(),
		Namespace: "criteria-jobs",
		Routes:    parityPayload(),
		Log:       logr.Discard(),
	}
	wouldBe, reasons := b.BuildRun(ctx, parityDecision())
	require.Empty(t, reasons)
	require.NotNil(t, wouldBe)
	// TicketID differs by the lane's presentation seam (watcher stamps the
	// kanboard identifier; the assignment carries the queue's label);
	// normalize to lowercase, the same rule CompareSpec applies.
	watcherSpec, mirrorSpec := watcherRun.Spec, wouldBe.Spec
	watcherSpec.TicketID = strings.ToLower(watcherSpec.TicketID)
	mirrorSpec.TicketID = strings.ToLower(mirrorSpec.TicketID)
	require.Equal(t, watcherSpec, mirrorSpec,
		"the assignment-driven CR and the watcher-driven CR must be identical on the fixture ticket")
	require.Equal(t, watcherRun.Namespace, wouldBe.Namespace)
	require.Equal(t, watcherRun.Labels, wouldBe.Labels)
	assert.Empty(t, CompareSpec(wouldBe, watcherRun))
}

// BuildRunReportsConfigSelectorDivergence pins the documented divergence
// source: the assignment carries no config selector, so a watcher route
// that resolves per-repo config stamps a configRef the would-be CR lacks —
// and the compare must report it.
func TestBuildRunReportsConfigSelectorDivergence(t *testing.T) {
	ctx := context.Background()
	sel := &routes.Selection{
		Name:           "kanboard-develop-url",
		Workflow:       parityWorkflow(),
		ConfigName:     "repo-config",
		Config:         routes.ConfigEntry{BaseBranch: "main"},
		ConflictPolicy: criteriav1.ImageConflictPolicyFailClosed,
	}
	watcherRun := watcherStamp("kb-319-1770000004", sel, parityDefaults(), "criteria-jobs")
	// The watcher stamps the pinned configRef (name-only here; the pin
	// consult is the caller's to wire).
	watcherRun.Spec.ConfigRef = &criteriav1.CriteriaRunConfigRef{Name: "repo-config"}
	b := &RunBuilder{
		Defaults:  parityDefaults(),
		Namespace: "criteria-jobs",
		Routes:    parityPayload(),
		Log:       logr.Discard(),
	}
	wouldBe, reasons := b.BuildRun(ctx, parityDecision())
	require.Empty(t, reasons)
	reasons = CompareSpec(wouldBe, watcherRun)
	require.Len(t, reasons, 1)
	assert.Contains(t, reasons[0], "spec.configRef")
	assert.Contains(t, reasons[0], "assignment=unset")
	assert.Contains(t, reasons[0], "watcher=set")
}

// slotStampingPayload serves the workflow plus the per-repo config entry
// the dispatch slot names (KB-103 per-repo config, KB-338 envelope).
func slotStampingPayload() *routes.Payload {
	p := parityPayload()
	p.ConfigLibrary = map[string]routes.ConfigEntry{
		"repo-config": {
			BuildCmd:   "make build-all",
			TestCmd:    "make test-all",
			CIGateCmd:  "make gate-all",
			BaseBranch: "release-9.1",
		},
	}
	return p
}

// slotDecision is the assignment decision carrying the dispatch slot.
func slotDecision() Decision {
	d := parityDecision()
	d.Slot = "repo-config"
	return d
}

// pinResolverOver resolves the pin through the given client, the same
// closure shape the operator's configPinResolver wires.
func pinResolverOver(cl client.Client) runspec.CriteriaRunConfigRefProvider {
	return func(ns string, sel *routes.Selection) (*criteriav1.CriteriaRunConfigRef, error) {
		return runstamp.PinConfigRef(context.Background(), cl, ns, sel)
	}
}

// slotStampingScheme builds a scheme carrying both fixture types.
func slotStampingScheme(t *testing.T) *runtime.Scheme {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, criteriav1.AddToScheme(scheme))
	return scheme
}

// TestBuildRunSlotsStampConfigEntry proves the envelope's slot lands on
// the stamped CR (KB-338 reviewer acceptance b): the configurationLibrary
// ENTRY's spec overrides stamp whenever the slot resolves — with or
// without a pinner — and with a pinner wired the CR also carries the
// ConfigMap pin (resourceVersion when the CM is readable, name-only when
// it is not rendered yet); an unknown slot fails closed.
func TestBuildRunSlotsStampConfigEntry(t *testing.T) {
	ctx := context.Background()
	scheme := slotStampingScheme(t)
	cm := &corev1.ConfigMap{}
	cm.Name = "repo-config"
	cm.Namespace = "criteria-jobs"
	cm.ResourceVersion = "777"

	t.Run("pinner wired: entry overrides and configRef stamp", func(t *testing.T) {
		b := &RunBuilder{
			Defaults:     parityDefaults(),
			Namespace:    "criteria-jobs",
			Routes:       slotStampingPayload(),
			Client:       fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build(),
			Log:          logr.Discard(),
			ConfigPinner: pinResolverOver(fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()),
		}
		run, reasons := b.BuildRun(ctx, slotDecision())
		require.Empty(t, reasons)
		require.NotNil(t, run)
		require.Equal(t, "make build-all", run.Spec.BuildCmd)
		require.Equal(t, "make test-all", run.Spec.TestCmd)
		require.Equal(t, "make gate-all", run.Spec.CIGateCmd)
		require.Equal(t, "release-9.1", run.Spec.BaseBranch)
		require.NotNil(t, run.Spec.ConfigRef)
		assert.Equal(t, "repo-config", run.Spec.ConfigRef.Name)
		assert.Equal(t, "777", run.Spec.ConfigRef.ResourceVersion)
	})

	t.Run("unreadable ConfigMap degrades to a name-only pin", func(t *testing.T) {
		absent := fake.NewClientBuilder().WithScheme(scheme).Build()
		b := &RunBuilder{
			Defaults:     parityDefaults(),
			Namespace:    "criteria-jobs",
			Routes:       slotStampingPayload(),
			Client:       absent,
			Log:          logr.Discard(),
			ConfigPinner: pinResolverOver(absent),
		}
		run, reasons := b.BuildRun(ctx, slotDecision())
		require.Empty(t, reasons)
		require.NotNil(t, run.Spec.ConfigRef, "a read-deferred ConfigMap stamps a name-only pin, not nothing")
		assert.Equal(t, "repo-config", run.Spec.ConfigRef.Name)
		assert.Empty(t, run.Spec.ConfigRef.ResourceVersion, "no resourceVersion may be fabricated")
	})

	t.Run("nil pinner still stamps entry overrides (pin-less posture)", func(t *testing.T) {
		b := &RunBuilder{
			Defaults:  parityDefaults(),
			Namespace: "criteria-jobs",
			Routes:    slotStampingPayload(),
			Log:       logr.Discard(),
		}
		run, reasons := b.BuildRun(ctx, slotDecision())
		require.Empty(t, reasons)
		require.Equal(t, "make build-all", run.Spec.BuildCmd, "the slot entry's overrides land even pin-less (stamping is decoupled from the consult)")
		assert.Nil(t, run.Spec.ConfigRef)
	})

	t.Run("unknown slot fails closed", func(t *testing.T) {
		b := &RunBuilder{
			Defaults:     parityDefaults(),
			Namespace:    "criteria-jobs",
			Routes:       slotStampingPayload(),
			Log:          logr.Discard(),
			ConfigPinner: pinResolverOver(fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()),
		}
		d := slotDecision()
		d.Slot = "ghost-slot"
		run, reasons := b.BuildRun(ctx, d)
		require.Nil(t, run)
		require.Len(t, reasons, 1)
		assert.Contains(t, reasons[0], "dispatch slot")
	})
}

// TestCompareSpecSlotLanesAgree is the contract test the reviewer's
// blocking defect would have tripped: for the same ticket and slot, the
// watcher lane (StampRun + pinner) and the assignment lane (BuildRun +
// pinner) stamp IDENTICAL CRs — and a builder that silently drops its
// pinner diverges on exactly spec.configRef, which the shadow window
// reports instead of silently running lane defaults.
func TestCompareSpecSlotLanesAgree(t *testing.T) {
	ctx := context.Background()
	scheme := slotStampingScheme(t)
	cm := &corev1.ConfigMap{}
	cm.Name = "repo-config"
	cm.Namespace = "criteria-jobs"
	cm.ResourceVersion = "777"
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()
	resolver := pinResolverOver(cl)

	sel := &routes.Selection{
		Name:           "kanboard-develop-url",
		Workflow:       parityWorkflow(),
		ConfigName:     "repo-config",
		Config:         slotStampingPayload().ConfigLibrary["repo-config"],
		ConflictPolicy: criteriav1.ImageConflictPolicyFailClosed,
	}
	watcherRun := runspec.StampRun(runspec.StampInput{
		Name:             "kb-319-1770000010",
		TicketID:         "KB-319",
		RepoURL:          "https://github.com/brokenbots/workflow-example.git",
		Selection:        sel,
		Defaults:         parityDefaults(),
		Namespace:        "criteria-jobs",
		ResolveConfigPin: resolver,
	})

	b := &RunBuilder{
		Defaults:     parityDefaults(),
		Namespace:    "criteria-jobs",
		Routes:       slotStampingPayload(),
		Log:          logr.Discard(),
		ConfigPinner: resolver,
	}
	wouldBe, reasons := b.BuildRun(ctx, slotDecision())
	require.Empty(t, reasons)
	assert.Empty(t, CompareSpec(wouldBe, watcherRun),
		"both slot lanes wired to the same pinner must stamp identical CRs")

	// The defect arm: a builder without its pinner still matches the
	// entry overrides but loses the pin — exactly one divergence reason,
	// spec.configRef.
	pinless := &RunBuilder{
		Defaults:  parityDefaults(),
		Namespace: "criteria-jobs",
		Routes:    slotStampingPayload(),
		Log:       logr.Discard(),
	}
	pinlessRun, reasons := pinless.BuildRun(ctx, slotDecision())
	require.Empty(t, reasons)
	div := CompareSpec(pinlessRun, watcherRun)
	require.Len(t, div, 1)
	assert.Contains(t, div[0], "spec.configRef")
}

// BuildRunReportsDefaultDivergence: operator env drift (a different
// buildCmd than the watcher's flag) must surface as spec-level divergence,
// not silent agreement.
func TestBuildRunReportsDefaultDivergence(t *testing.T) {
	ctx := context.Background()
	watcherRun := watcherStamp("kb-319-1770000005", &routes.Selection{
		Name:           "kanboard-develop-url",
		Workflow:       parityWorkflow(),
		ConflictPolicy: criteriav1.ImageConflictPolicyFailClosed,
	}, parityDefaults(), "criteria-jobs")
	drifted := parityDefaults()
	drifted.BuildCmd = "make build-all"
	b := &RunBuilder{Defaults: drifted, Namespace: "criteria-jobs", Routes: parityPayload(), Log: logr.Discard()}
	wouldBe, reasons := b.BuildRun(ctx, parityDecision())
	require.Empty(t, reasons)
	div := CompareSpec(wouldBe, watcherRun)
	require.Len(t, div, 1)
	assert.Contains(t, div[0], "spec.buildCmd")
	assert.Contains(t, div[0], `assignment="make build-all"`)
	assert.Contains(t, div[0], `watcher="make build"`)
}

// BuildRunUnknownWorkflow fails closed: an assignment naming a workflow
// absent from the routes library builds no spec and reports the reason.
func TestBuildRunUnknownWorkflow(t *testing.T) {
	ctx := context.Background()
	b := &RunBuilder{Defaults: parityDefaults(), Namespace: "criteria-jobs", Routes: parityPayload(), Log: logr.Discard()}
	d := parityDecision()
	d.WorkflowName = "ghost-workflow"
	run, reasons := b.BuildRun(ctx, d)
	require.Nil(t, run)
	require.Len(t, reasons, 1)
	assert.Contains(t, reasons[0], "spec.workflow.name")
	assert.Contains(t, reasons[0], "ghost-workflow")
}

// BuildRunDegradesWithoutRoutes keeps the decision-only posture: a nil
// routes payload reports exactly one reason and builds nothing — the
// operator must not guess a spec.
func TestBuildRunDegradesWithoutRoutes(t *testing.T) {
	ctx := context.Background()
	b := &RunBuilder{Defaults: parityDefaults(), Namespace: "criteria-jobs", Log: logr.Discard()}
	run, reasons := b.BuildRun(ctx, parityDecision())
	require.Nil(t, run)
	require.Len(t, reasons, 1)
	assert.Contains(t, reasons[0], "routes payload not configured")
}

// BuildRunCutoverShapeMirrorsWatcher stamps the binding-first shape: the
// CR lands in the cutover-done project's namespace with a binding pin read
// from the project ConfigMap, and stays identical to the watcher's CR.
func TestBuildRunCutoverShapeMirrorsWatcher(t *testing.T) {
	ctx := context.Background()
	sel := &routes.Selection{
		Name:           "kanboard-develop-url",
		Workflow:       parityWorkflow(),
		ConflictPolicy: criteriav1.ImageConflictPolicyFailClosed,
	}
	payload := &routes.Payload{
		WorkflowLibrary: map[string]routes.Workflow{"kanboard-develop-url": parityWorkflow()},
		ProjectRegistry: map[string]routes.ProjectEntry{
			"castor": {Namespace: "project-castor", Cutover: routes.CutoverDone},
		},
	}
	bindingCM := &corev1.ConfigMap{}
	bindingCM.Name = "criteria-binding"
	bindingCM.Namespace = "project-castor"
	bindingCM.ResourceVersion = "42"
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, criteriav1.AddToScheme(scheme))
	b := &RunBuilder{
		Defaults:  parityDefaults(),
		Namespace: "criteria-jobs",
		Routes:    payload,
		Client:    fake.NewClientBuilder().WithScheme(scheme).WithObjects(bindingCM).Build(),
		Log:       logr.Discard(),
	}
	watcherRun := runspec.StampRun(runspec.StampInput{
		Name:                   "kb-319-1770000006",
		TicketID:               "KB-319",
		RepoURL:                "https://github.com/brokenbots/workflow-example.git",
		Selection:              sel,
		Defaults:               parityDefaults(),
		Namespace:              "criteria-jobs",
		CutoverNamespace:       "project-castor",
		BindingRefName:         "criteria-binding",
		BindingResourceVersion: "42",
	})
	wouldBe, reasons := b.BuildRun(ctx, parityDecision())
	require.Empty(t, reasons)
	require.Equal(t, "project-castor", wouldBe.Namespace)
	// TicketID differs by the lane's presentation seam (watcher stamps the
	// kanboard identifier; the assignment carries the queue's label), so
	// compare spec with that field normalized — the same rule
	// CompareSpec applies.
	watcherSpec, mirrorSpec := watcherRun.Spec, wouldBe.Spec
	watcherSpec.TicketID, mirrorSpec.TicketID = "KB-319", "KB-319"
	require.Equal(t, watcherSpec, mirrorSpec,
		"the cutover-done assignment mirror must be identical to the watcher-driven binding-first CR")
	assert.Empty(t, CompareSpec(wouldBe, watcherRun))
}

// WatchdogCheckDecisionSpecParity exercises the watchdog seam end to end:
// a live (fake-client) watcher CR and a wired builder produce a Clean
// divergence for a fixture ticket whose assignment drives an identical CR.
func TestWatchdogCheckDecisionSpecParity(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, criteriav1.AddToScheme(scheme))
	watcherRun := watcherStamp("kb-319-1770000007", &routes.Selection{
		Name:           "kanboard-develop-url",
		Workflow:       parityWorkflow(),
		ConflictPolicy: criteriav1.ImageConflictPolicyFailClosed,
	}, parityDefaults(), "criteria-jobs")
	b := &RunBuilder{
		Defaults:  parityDefaults(),
		Namespace: "criteria-jobs",
		Routes:    parityPayload(),
		Log:       logr.Discard(),
	}
	wd := &Watchdog{
		Client:    fake.NewClientBuilder().WithScheme(scheme).WithObjects(watcherRun).Build(),
		Namespace: "criteria-jobs",
		Builder:   b,
	}
	div := wd.CheckDecision(ctx, parityDecision())
	assert.True(t, div.Clean)
	assert.Empty(t, div.Reasons)
	assert.Equal(t, "kb-319-1770000007", div.CRName)

	// The decision fields alone (no builder) compare clean too — so the
	// divergent field in the next test is genuinely only observable at
	// spec granularity.
	wdNoSpec := &Watchdog{Client: wd.Client, Namespace: "criteria-jobs"}
	assert.True(t, wdNoSpec.CheckDecision(ctx, parityDecision()).Clean)
}

// WatchdogCheckDecisionReportsSpecDivergence: with a builder wired, an
// operator/watcher defaults drift that decision-only comparison cannot see
// (repoURL, class, workflow all agree) still diverges — the point of the
// spec-level shadow window.
func TestWatchdogCheckDecisionReportsSpecDivergence(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, criteriav1.AddToScheme(scheme))
	watcherRun := watcherStamp("kb-319-1770000008", &routes.Selection{
		Name:           "kanboard-develop-url",
		Workflow:       parityWorkflow(),
		ConflictPolicy: criteriav1.ImageConflictPolicyFailClosed,
	}, parityDefaults(), "criteria-jobs")
	drifted := parityDefaults()
	drifted.TestCmd = "make test-all"
	b := &RunBuilder{Defaults: drifted, Namespace: "criteria-jobs", Routes: parityPayload(), Log: logr.Discard()}
	wd := &Watchdog{
		Client:    fake.NewClientBuilder().WithScheme(scheme).WithObjects(watcherRun).Build(),
		Namespace: "criteria-jobs",
		Builder:   b,
	}
	div := wd.CheckDecision(ctx, parityDecision())
	assert.False(t, div.Clean)
	require.NotEmpty(t, div.Reasons)
	var specReasons []string
	for _, r := range div.Reasons {
		if len(r) > 5 && r[:5] == "spec." {
			specReasons = append(specReasons, r)
		}
	}
	require.Len(t, specReasons, 1)
	assert.Contains(t, specReasons[0], "spec.testCmd")
	assert.Contains(t, specReasons[0], `assignment="make test-all"`)
}

// WatchdogCheckDecisionWithoutBuilder stays decision-only: nil builder
// must not change the divergences the pre-spec comparison reports.
func TestWatchdogCheckDecisionWithoutBuilder(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, criteriav1.AddToScheme(scheme))
	run := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{Name: "kb-319-1770000009", Namespace: "criteria-jobs",
			Labels: map[string]string{DefaultLabelTicket: "kb-319"}},
		Spec: criteriav1.CriteriaRunSpec{RepoURL: "https://github.com/org/repo"},
	}
	wd := &Watchdog{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(run).Build(), Namespace: "criteria-jobs"}
	div := wd.CheckDecision(ctx, Decision{Ticket: "kb-319", RepoURL: "https://github.com/org/other"})
	assert.False(t, div.Clean)
	require.Len(t, div.Reasons, 1)
	assert.Contains(t, div.Reasons[0], "repoURL")
}

// WatchdogUnconfiguredBuilderStaysClean pins the no-routes deployment
// posture: a builder wired WITHOUT a routes payload never runs the spec
// mirror, so the "routes payload not configured" degrade never enters the
// divergence log — agreeing lanes log clean and a missing config file
// stays distinguishable from a real lane disagreement.
func TestWatchdogUnconfiguredBuilderStaysClean(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, criteriav1.AddToScheme(scheme))
	watcherRun := watcherStamp("kb-319-1770000011", &routes.Selection{
		Name:           "kanboard-develop-url",
		Workflow:       parityWorkflow(),
		ConflictPolicy: criteriav1.ImageConflictPolicyFailClosed,
	}, parityDefaults(), "criteria-jobs")
	unconfigured := &RunBuilder{Defaults: parityDefaults(), Namespace: "criteria-jobs", Log: logr.Discard()}
	assert.False(t, unconfigured.Configured(), "a builder without a routes payload must report unconfigured")
	wd := &Watchdog{
		Client:    fake.NewClientBuilder().WithScheme(scheme).WithObjects(watcherRun).Build(),
		Namespace: "criteria-jobs",
		Builder:   unconfigured,
	}
	div := wd.CheckDecision(ctx, parityDecision())
	assert.True(t, div.Clean)
	assert.Empty(t, div.Reasons)
}

// WatchdogUnconfiguredBuilderKeepsDecisionDivergence: the no-routes
// posture must not blind the decision comparison — a repoURL disagreement
// still reports through the decision-granularity fields.
func TestWatchdogUnconfiguredBuilderKeepsDecisionDivergence(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, criteriav1.AddToScheme(scheme))
	watcherRun := watcherStamp("kb-319-1770000012", &routes.Selection{
		Name:           "kanboard-develop-url",
		Workflow:       parityWorkflow(),
		ConflictPolicy: criteriav1.ImageConflictPolicyFailClosed,
	}, parityDefaults(), "criteria-jobs")
	unconfigured := &RunBuilder{Defaults: parityDefaults(), Namespace: "criteria-jobs", Log: logr.Discard()}
	wd := &Watchdog{
		Client:    fake.NewClientBuilder().WithScheme(scheme).WithObjects(watcherRun).Build(),
		Namespace: "criteria-jobs",
		Builder:   unconfigured,
	}
	// Same ticket, different repo: the decision-granularity identity
	// disagreement the no-routes posture must keep reporting.
	different := parityDecision()
	different.RepoURL = "https://github.com/brokenbots/other.git"
	div := wd.CheckDecision(ctx, different)
	assert.False(t, div.Clean)
	require.Len(t, div.Reasons, 1)
	assert.Contains(t, div.Reasons[0], "repoURL",
		"the decision comparison must stay live on the no-routes deployment posture")
}

// CompareSpecAdapterImagesDivergence: a per-kind adapter pin that exists on
// one lane only names the kind in the reason (the metadata lane divergence
// the conflict enforcement arbitrates).
func TestCompareSpecAdapterImagesDivergence(t *testing.T) {
	watcherRun := watcherStamp("kb-319-1770000010", &routes.Selection{
		Name:           "kanboard-develop-url",
		Workflow:       parityWorkflow(),
		ConflictPolicy: criteriav1.ImageConflictPolicyFailClosed,
	}, parityDefaults(), "criteria-jobs")
	b := &RunBuilder{Defaults: parityDefaults(), Namespace: "criteria-jobs", Routes: parityPayload(), Log: logr.Discard()}
	wouldBe, reasons := b.BuildRun(context.Background(), parityDecision())
	require.Empty(t, reasons)
	// The watcher's route carries an adapter pin the assignment mirror
	// resolved too — drop it on the mirror to manufacture the divergence.
	delete(wouldBe.Spec.Workflow.AdapterImages, "copilot")
	div := CompareSpec(wouldBe, watcherRun)
	require.Len(t, div, 1)
	assert.Equal(t, "spec.workflow.adapterImages.copilot: assignment=absent watcher=\"localhost:5000/copilot-adapter:dev\"", div[0])
}
