package runspec

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/routes"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/runstamp"
)

func newTestClient(t *testing.T, objs ...ctrlclient.Object) ctrlclient.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

// sharedTreeInterface is the fixture interface both lanes stamp; the
// expected copies below are hand-built from it (not parser-derived), so a
// conversion drop breaks the equality instead of echoing the parser.
func sharedTreeInterface() *routes.WorkflowInterface {
	return &routes.WorkflowInterface{
		APIVersion:  routes.InterfaceAPIVersion,
		Kind:        routes.InterfaceKind,
		Workflow:    "kanboard_develop_v1",
		Description: "shared tree contract",
		Adapters:    []string{"shell", "copilot"},
		Mounts: []routes.WorkflowInterfaceMount{
			{Name: "workspace", MountPath: "/home/criteria/workspace", Kind: "pvc"},
			{Name: "scratch", MountPath: "/tmp/scratch", Kind: "tmp"},
		},
		SecretEnv: []string{"WORKFLOW_GITHUB_TOKEN", "LINEAR_API_KEY"},
		Env:       []string{"CRITERIA_RUN_MODE"},
		ConfigSlots: []routes.WorkflowInterfaceConfigSlot{
			{Slot: "baseBranch", Variable: "base_branch", Target: "branch_manager/checkout + PR base",
				Description: "Branch the run forks from and opens its PR against."},
			{Slot: "ciGateCmd", Variable: "ci_gate_cmd", Target: "pair_programming_loop developer CI gate",
				Description: "Command the developer step uses as its CI gate before review."},
		},
		LockKey: "repoUrl",
	}
}

func expectedSharedTreeInterface() *criteriav1.RunWorkflowInterface {
	return &criteriav1.RunWorkflowInterface{
		APIVersion:  routes.InterfaceAPIVersion,
		Kind:        routes.InterfaceKind,
		Workflow:    "kanboard_develop_v1",
		Description: "shared tree contract",
		Adapters:    []string{"shell", "copilot"},
		Mounts: []criteriav1.RunWorkflowInterfaceMount{
			{Name: "workspace", MountPath: "/home/criteria/workspace", Kind: "pvc"},
			{Name: "scratch", MountPath: "/tmp/scratch", Kind: "tmp"},
		},
		SecretEnv: []string{"WORKFLOW_GITHUB_TOKEN", "LINEAR_API_KEY"},
		Env:       []string{"CRITERIA_RUN_MODE"},
		ConfigSlots: []criteriav1.RunWorkflowInterfaceConfigSlot{
			{Slot: "baseBranch", Variable: "base_branch", Target: "branch_manager/checkout + PR base",
				Description: "Branch the run forks from and opens its PR against."},
			{Slot: "ciGateCmd", Variable: "ci_gate_cmd", Target: "pair_programming_loop developer CI gate",
				Description: "Command the developer step uses as its CI gate before review."},
		},
		LockKey: "repoUrl",
	}
}

func testDefaults() Defaults {
	return Defaults{
		BuildCmd:        "make build",
		TestCmd:         "make test",
		CIGateCmd:       "make ci",
		BaseBranch:      "main",
		MaxAgentVisits:  3,
		ProviderBaseURL: "http://provider:11434/v1",
	}
}

func testSelection() *routes.Selection {
	return &routes.Selection{
		Name: "kanboard-develop-url",
		Workflow: routes.Workflow{
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
			Interface: sharedTreeInterface(),
		},
		ConflictPolicy: criteriav1.ImageConflictPolicyPreferEvent,
	}
}

// TestStampRunMatchesWatcherStamp is the KB-319 acceptance fixture: the
// shared stamp's pre-cutover output must equal the CR the watcher's
// buildCriteriaRun stamped for the same resolved decision — hand-built
// field-for-field, not derived from the stamp under test.
func TestStampRunMatchesWatcherStamp(t *testing.T) {
	name := "kb-319-1770000000"
	got := StampRun(StampInput{
		Name:      name,
		TicketID:  "KB-319",
		RepoURL:   "https://github.com/brokenbots/workflow-example.git",
		Selection: testSelection(),
		Defaults:  testDefaults(),
		Namespace: "criteria-jobs",
	})
	// No pin resolver: the KB-103 consult belongs to the caller (the
	// watcher's own read + card comment).
	require.Equal(t, criteriav1.CriteriaRunSpec{
		TicketID:         "KB-319",
		RepoURL:          "https://github.com/brokenbots/workflow-example.git",
		PerScopeSessions: true,
		BuildCmd:         "make build",
		TestCmd:          "make test",
		CIGateCmd:        "make ci",
		BaseBranch:       "main",
		MaxAgentVisits:   3,
		ProviderBaseURL:  "http://provider:11434/v1",
		WorkflowSource: &criteriav1.RunWorkflowSource{
			Type: "url",
			URL:  "https://github.com/brokenbots/workflow-example.git",
			Ref:  "main",
		},
		Workflow: &criteriav1.RunWorkflow{
			Name:      "kanboard-develop-url",
			Type:      routes.TypeURL,
			Namespace: "criteria-jobs",
			Class:     routes.ClassDefault,
			URL:       "https://github.com/brokenbots/workflow-example.git",
			Ref:       "main",
			Interface: expectedSharedTreeInterface(),
			Env:       map[string]string{"CRITERIA_RUN_MODE": "develop"},
			AdapterImages: map[string]string{
				"shell":   "localhost:5000/shell-adapter:dev",
				"copilot": "localhost:5000/copilot-adapter:dev",
			},
			ImageConflictPolicy: criteriav1.ImageConflictPolicyPreferEvent,
		},
	}, got.Spec, "the shared stamp must reproduce the watcher's spec field-for-field")
	require.Equal(t, metav1.ObjectMeta{
		Name:      name,
		Namespace: "criteria-jobs",
		Labels: map[string]string{
			LabelTicket:    "kb-319",
			LabelManagedBy: ManagedBy,
			LabelSource:    Source,
		},
	}, got.ObjectMeta, "the shared stamp must reproduce the watcher's metadata and labels")
}

// TestStampRunInterfaceCopy: the stamped interface equals the hand-built
// declared contract and carries the declared lock key (KB-319
// dev-class serialization).
func TestStampRunInterfaceCopy(t *testing.T) {
	got := StampRun(StampInput{
		Name:      "kb-319-1770000007",
		TicketID:  "KB-319",
		RepoURL:   "repo",
		Selection: testSelection(),
		Defaults:  testDefaults(),
		Namespace: "criteria-jobs",
	})
	require.Equal(t, expectedSharedTreeInterface(), got.Spec.Workflow.Interface)
}

// TestStampRunBindingFirstShape pins the cutover-done shape (KB-311): tree
// references only, no KB-103 spec fields, binding pin, landing namespace.
func TestStampRunBindingFirstShape(t *testing.T) {
	sel := testSelection()
	sel.Workflow.Namespace = "" // cutover-done: the global exec ns is not stamped
	got := StampRun(StampInput{
		Name:                   "kb-311-1770000001",
		TicketID:               "KB-311",
		RepoURL:                "https://github.com/brokenbots/workflow-example.git",
		Selection:              sel,
		Defaults:               testDefaults(),
		CutoverNamespace:       "project-a",
		BindingRefName:         "criteria-binding",
		BindingResourceVersion: "12345",
	})
	require.Equal(t, "project-a", got.Namespace)
	require.NotNil(t, got.Spec.Workflow)
	require.Equal(t, "kanboard-develop-url", got.Spec.Workflow.Name)
	require.Equal(t, routes.TypeURL, got.Spec.Workflow.Type)
	require.Equal(t, routes.ClassDefault, got.Spec.Workflow.Class)
	require.Equal(t, &criteriav1.CriteriaRunProjectBindingRef{
		Name:            "criteria-binding",
		ResourceVersion: "12345",
	}, got.Spec.ProjectBindingRef)
	require.Empty(t, got.Spec.BuildCmd, "binding-first: no KB-103 spec-field stamp (would collide with binding-owned config at admission)")
	require.Empty(t, got.Spec.TestCmd)
	require.Empty(t, got.Spec.CIGateCmd)
	require.Empty(t, got.Spec.BaseBranch)
	require.Nil(t, got.Spec.ConfigRef, "no config pin on the binding-first shape")
	require.NotNil(t, got.Spec.Workflow.Interface, "binding-first keeps the interface declaration (the secrets lane)")
	require.Empty(t, got.Spec.Workflow.Env, "binding-first: the ADR hands env to the binding")
	require.Empty(t, got.Spec.Workflow.AdapterImages, "binding-first: the ADR hands adapter images to the binding")
	// Per-scope + contract flags stay stamped on both shapes.
	require.True(t, got.Spec.PerScopeSessions)
	require.Equal(t, 3, got.Spec.MaxAgentVisits)
	require.Equal(t, "http://provider:11434/v1", got.Spec.ProviderBaseURL)
}

// TestStampRunConfigPinConsult consults the resolver only with a config
// entry; a resolver error keeps the name-only pin (KB-250: the caller
// logs; nothing is lost beyond provenance).
func TestStampRunConfigPinConsult(t *testing.T) {
	sel := testSelection()
	sel.ConfigName = "workflow-example"
	sel.Config = routes.ConfigEntry{BuildCmd: "make build && make vet"}

	var consultedNS string
	pinOK := func(ns string, s *routes.Selection) (*criteriav1.CriteriaRunConfigRef, error) {
		consultedNS = ns
		return &criteriav1.CriteriaRunConfigRef{
			Name:            s.ConfigName,
			ResourceVersion: "42",
		}, nil
	}
	got := StampRun(StampInput{
		Name:             "kb-103-1770000002",
		TicketID:         "KB-103",
		RepoURL:          "repo",
		Selection:        sel,
		Defaults:         testDefaults(),
		Namespace:        "criteria-jobs",
		ResolveConfigPin: pinOK,
	})
	require.Equal(t, "criteria-jobs", consultedNS, "the consult runs in ExecNamespace(sel)")
	require.Equal(t, &criteriav1.CriteriaRunConfigRef{Name: "workflow-example", ResourceVersion: "42"}, got.Spec.ConfigRef)
	require.Equal(t, "make build && make vet", got.Spec.BuildCmd,
		"config entry outranks the fallback flag entry-by-entry")

	// The entry's empty testCmd keeps the fallback flag: KB-103 stamps
	// entry-by-entry, not wholesale.
	sel.Config = routes.ConfigEntry{BuildCmd: "make build && make vet", CIGateCmd: "make ci"}
	got = StampRun(StampInput{
		Name:             "kb-103-1770000003",
		TicketID:         "KB-103",
		RepoURL:          "repo",
		Selection:        sel,
		Defaults:         testDefaults(),
		Namespace:        "criteria-jobs",
		ResolveConfigPin: pinOK,
	})
	require.Equal(t, "make test", got.Spec.TestCmd)
	require.Equal(t, "make ci", got.Spec.CIGateCmd)

	// Resolver error → name-only pin, spec fields still override where set.
	got = StampRun(StampInput{
		Name:      "kb-103-1770000004",
		TicketID:  "KB-103",
		RepoURL:   "repo",
		Selection: sel,
		Defaults:  testDefaults(),
		Namespace: "criteria-jobs",
		ResolveConfigPin: func(ns string, s *routes.Selection) (*criteriav1.CriteriaRunConfigRef, error) {
			return &criteriav1.CriteriaRunConfigRef{Name: s.ConfigName}, errPinFailed
		},
	})
	require.Equal(t, &criteriav1.CriteriaRunConfigRef{Name: "workflow-example"}, got.Spec.ConfigRef,
		"a failed pin read keeps the name-only pin (KB-250 no-silent-degrade)")

	// Real read path: the pinned ConfigMap's resourceVersion lands on the
	// ref (the watcher's actual PinConfigRef closure wired through the
	// shared stamp).
	c := newTestClient(t, pinnedCM("criteria-jobs", "workflow-example", "777"))
	sel.Config = routes.ConfigEntry{}
	got = StampRun(StampInput{
		Name:      "kb-103-1770000005",
		TicketID:  "KB-103",
		RepoURL:   "repo",
		Selection: sel,
		Defaults:  testDefaults(),
		Namespace: "criteria-jobs",
		ResolveConfigPin: func(ns string, s *routes.Selection) (*criteriav1.CriteriaRunConfigRef, error) {
			return runstamp.PinConfigRef(context.Background(), c, ns, s)
		},
	})
	require.Equal(t, &criteriav1.CriteriaRunConfigRef{Name: "workflow-example", ResourceVersion: "777"}, got.Spec.ConfigRef)
}

// TestStampRunSkipsConsultWithoutConfigEntry: no config entry → no consult,
// no ConfigRef, and the fallback flags ride untouched.
func TestStampRunSkipsConsultWithoutConfigEntry(t *testing.T) {
	var consulted bool
	got := StampRun(StampInput{
		Name:      "kb-103-1770000006",
		TicketID:  "KB-103",
		RepoURL:   "repo",
		Selection: testSelection(),
		Defaults:  testDefaults(),
		Namespace: "criteria-jobs",
		ResolveConfigPin: func(ns string, s *routes.Selection) (*criteriav1.CriteriaRunConfigRef, error) {
			consulted = true
			return nil, nil
		},
	})
	require.False(t, consulted)
	require.Nil(t, got.Spec.ConfigRef)
	require.Equal(t, "make build", got.Spec.BuildCmd)
}

// TestExecNamespace: the workflow's declared execution namespace wins;
// absent, the lane's own namespace applies.
func TestExecNamespace(t *testing.T) {
	require.Equal(t, "wf-ns", ExecNamespace("criteria-jobs", &routes.Selection{
		Workflow: routes.Workflow{Namespace: "wf-ns"},
	}))
	require.Equal(t, "criteria-jobs", ExecNamespace("criteria-jobs", &routes.Selection{}))
	require.Equal(t, "criteria-jobs", ExecNamespace("criteria-jobs", nil))
}

// TestStampWorkflowSpecDeepCopy: mutating the stamped tree must not leak
// into the routes entry (the routes payload is reused across polls).
func TestStampWorkflowSpecDeepCopy(t *testing.T) {
	src := &routes.WorkflowInterface{
		Adapters: []string{"shell", "copilot"},
		Mounts:   []routes.WorkflowInterfaceMount{{Name: "workspace", MountPath: "/w", Kind: "pvc"}},
	}
	wf := routes.Workflow{
		Type:          routes.TypeImage,
		Image:         "localhost:5000/linear-intake-remote:dev",
		Interface:     src,
		Env:           map[string]string{"MODE": "develop"},
		AdapterImages: map[string]string{"shell": "img"},
	}
	out := StampWorkflowSpec("kanboard-intake", wf, criteriav1.ImageConflictPolicyFailClosed)
	require.Equal(t, criteriav1.ImageConflictPolicyFailClosed, out.ImageConflictPolicy,
		"the KB-319 conflict policy is stamped on the run's workflow")
	require.Equal(t, routes.TypeImage, out.Type)
	require.Equal(t, "localhost:5000/linear-intake-remote:dev", out.Image)
	require.Equal(t, routes.ClassDefault, out.Class, "an omitted class defaults to dev")

	out.Env["MODE"] = "mutated"
	out.AdapterImages["shell"] = "mutated"
	out.Interface.Adapters[0] = "mutated"
	out.Interface.Mounts[0] = criteriav1.RunWorkflowInterfaceMount{Name: "mutated"}
	require.Equal(t, "develop", wf.Env["MODE"], "env copies are deep")
	require.Equal(t, "img", wf.AdapterImages["shell"], "adapter images copy deeply")
	require.Equal(t, "shell", src.Adapters[0], "interface copies are deep")
	require.Equal(t, routes.WorkflowInterfaceMount{Name: "workspace", MountPath: "/w", Kind: "pvc"}, src.Mounts[0])
}

// TestStampWorkflowSpecImageModeNilInterface: image mode carries no
// interface.
func TestStampWorkflowSpecImageModeNilInterface(t *testing.T) {
	out := StampWorkflowSpec("kanboard-intake", routes.Workflow{
		Type:  routes.TypeImage,
		Image: "localhost:5000/linear-intake-remote:dev",
	}, criteriav1.ImageConflictPolicyFailClosed)
	require.NotNil(t, out)
	require.Nil(t, out.Interface)
}

// TestStampClassQueueClass: triage only on an explicit triage declaration.
func TestStampClassQueueClass(t *testing.T) {
	require.Equal(t, routes.ClassTriage, StampClass(routes.Workflow{Class: routes.ClassTriage}))
	require.Equal(t, routes.ClassDefault, StampClass(routes.Workflow{}))
	require.Equal(t, routes.ClassDefault, StampClass(routes.Workflow{Class: "unclassified"}))
}

// TestStampWorkflowNilInterface: a nil routes interface stamps nil.
func TestStampWorkflowNilInterface(t *testing.T) {
	out := StampWorkflowSpec("wf-url", routes.Workflow{Type: routes.TypeURL, URL: "git::x?ref=abc"},
		criteriav1.ImageConflictPolicyFailClosed)
	require.NotNil(t, out)
	require.Nil(t, out.Interface)
}

func pinnedCM(ns, name, rv string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Namespace: ns, Name: name, ResourceVersion: rv,
	}}
}

var errPinFailed = errors.New("pin failed")
