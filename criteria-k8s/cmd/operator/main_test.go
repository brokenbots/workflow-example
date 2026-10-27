// CRI-264: the wiring regression the controller suite cannot see. The
// shipped operator's scheme is built in package main, and a typed read of
// an unregistered kind fails before any API call ("no kind is registered
// for the type ..."). The missing apps/v1 registration made the
// DeploymentEnvProbe's Deployment read — and with it the base-image
// admission-race enforcement — dead in the shipped binary while every
// controller test (stub probe, test-local scheme with appsv1) stayed
// green. These tests exercise the production scheme itself: removing the
// appsv1 registration in operatorScheme() fails both of them.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	castlev1 "github.com/brokenbots/castle/castle/gen/castle/v1"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/castle"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/control"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/controller"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/jobbuilder"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/routes"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/runspec"
)

// The production scheme must resolve every kind the operator's clients Get
// by value: DeploymentEnvProbe reads a typed *appsv1.Deployment, which
// requires the apps/v1 Deployment GVK in the scheme.
func TestOperatorSchemeResolvesDeploymentKind(t *testing.T) {
	scheme, err := operatorScheme()
	require.NoError(t, err)

	kinds, unversioned, err := scheme.ObjectKinds(&appsv1.Deployment{})
	require.NoError(t, err, "the production scheme must register the apps/v1 Deployment kind")
	require.False(t, unversioned, "Deployment is a versioned apps/v1 type, not an unversioned one")
	assert.Contains(t, kinds, appsv1.SchemeGroupVersion.WithKind("Deployment"))
}

// The probe the binary deploys must read the live operator Deployment
// through the production scheme: the base-image resolution, the
// BaseImageMismatch fail-fast, and the status.baseImage stamp all depend on
// this read succeeding (the controller suite's own probe test builds a
// scheme that always contains apps/v1, so it cannot catch a missing
// registration here).
func TestDeploymentEnvProbeOverProductionScheme(t *testing.T) {
	scheme, err := operatorScheme()
	require.NoError(t, err)

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "criteria-k8s-operator", Namespace: "criteria-jobs"},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "operator",
							Env: []corev1.EnvVar{
								{Name: jobbuilder.EnvCriteriaBaseImage, Value: "localhost:5000/criteria-base:v27"},
								{Name: "OTHER", Value: "x"},
							},
						},
					},
				},
			},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy).Build()
	probe := &controller.DeploymentEnvProbe{Client: cl, Namespace: "criteria-jobs", Deployment: "criteria-k8s-operator"}

	env, err := probe.OperatorEnv(context.Background(), jobbuilder.EnvCriteriaBaseImage, jobbuilder.EnvDefaultImage)
	require.NoError(t, err, "the production scheme must serve the operator-env Deployment read")
	assert.Equal(t, map[string]string{jobbuilder.EnvCriteriaBaseImage: "localhost:5000/criteria-base:v27"}, env)
}

// backfillMainArchiver is the minimal castle.RunArchiver the CLI tests use.
type backfillMainArchiver struct {
	records  map[string]*castlev1.ArchiveRecord
	disabled bool
}

func (f *backfillMainArchiver) ArchiveRun(_ context.Context, req *connect.Request[castlev1.ArchiveRunRequest]) (*connect.Response[castlev1.ArchiveRunResponse], error) {
	if f.records == nil {
		f.records = map[string]*castlev1.ArchiveRecord{}
	}
	rec := req.Msg.GetRecord()
	key := rec.GetArchiveKey()
	if key == "" {
		key = rec.GetRunId()
	}
	if rec.GetRunId() == "" && key != "" && !strings.HasPrefix(key, castle.CRUIDArchiveKeyPrefix) {
		return nil, connect.NewError(connect.CodeInvalidArgument, nil)
	}
	f.records[key] = rec
	return connect.NewResponse(&castlev1.ArchiveRunResponse{ArchiveId: castle.RunArchiveID(key)}), nil
}

func (f *backfillMainArchiver) ListRunArchives(_ context.Context, _ *connect.Request[castlev1.ListRunArchivesRequest]) (*connect.Response[castlev1.ListRunArchivesResponse], error) {
	return connect.NewResponse(&castlev1.ListRunArchivesResponse{}), nil
}

func (f *backfillMainArchiver) Disabled() bool { return f.disabled }

// terminalMainRun builds a terminal CriteriaRun fixture for the backfill
// mode: CR-uid keyed (no castle run id — terminal runs' castle ids are
// typically unenrichable), minimal identity from the ticket.
func terminalMainRun(name, uid string) *criteriav1.CriteriaRun {
	return &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  "criteria-jobs",
			UID:        types.UID(uid),
			Finalizers: []string{"criteriarun.criteria.brokenbots.dev/finalizer"},
		},
		Spec:   criteriav1.CriteriaRunSpec{TicketID: "CRI-" + name},
		Status: criteriav1.CriteriaRunStatus{Phase: criteriav1.PhaseFailed},
	}
}

// The backfill mode's exits are its contract: a half-configured archive
// lane fails closed refusing to claim coverage; a run left unarchived
// fails the pass so a rerun resumes; full coverage passes cleanly. The
// kube client is injected so these run against a fake without a cluster.
func TestRunArchiveBackfillExitSemantics(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, criteriav1.AddToScheme(scheme))
	cfg := &rest.Config{} // never dialed: the client constructor is injected

	t.Run("disabled archiver fails closed without building a client", func(t *testing.T) {
		restore := newBackfillKubeClient
		defer func() { newBackfillKubeClient = restore }()
		newBackfillKubeClient = func(*rest.Config, *runtime.Scheme) (client.Client, error) {
			return nil, errors.New("the disabled archiver must refuse before any kube activity")
		}
		err := runArchiveBackfill(context.Background(), cfg, scheme, nil, "", "tok", "criteria-jobs", "test", logr.Discard())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--castle-addr", "the gap is named: half the lane is missing")
		assert.Contains(t, err.Error(), "CASTLE_TOKEN")

		err = runArchiveBackfill(context.Background(), cfg, scheme, &backfillMainArchiver{disabled: true}, "http://castle:8080", "", "criteria-jobs", "test", logr.Discard())
		require.Error(t, err, "an addr-set tokenless archiver is Disabled and must also fail closed")
	})

	t.Run("unarchived terminal run fails the pass non-zero", func(t *testing.T) {
		unarchived := terminalMainRun("cri-77", "cr-uid-77")
		cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(unarchived).WithObjects(unarchived).Build()
		restore := newBackfillKubeClient
		defer func() { newBackfillKubeClient = restore }()
		newBackfillKubeClient = func(*rest.Config, *runtime.Scheme) (client.Client, error) { return cl, nil }
		// The walk fails the run (unavailable write leg), so the mode must
		// not pass cleanly while coverage is short.
		failing := connect.NewError(connect.CodeUnavailable, nil)
		archiver := failingBackfillArchiver{base: &backfillMainArchiver{}, err: failing}

		err := runArchiveBackfill(context.Background(), cfg, scheme, &archiver, "", "tok", "criteria-jobs", "test", logr.Discard())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "remain without a castle archive record", "the failure names the coverage gap and the resume contract")

		// The leg recovers: this pass archives the run and the mode passes.
		err = runArchiveBackfill(context.Background(), cfg, scheme, &backfillMainArchiver{}, "", "tok", "criteria-jobs", "test", logr.Discard())
		require.NoError(t, err, "the resumed pass lands the record and clears the run")
	})

	t.Run("full coverage passes cleanly", func(t *testing.T) {
		already := terminalMainRun("cri-88", "cr-uid-88")
		already.Status.CastleRunID = "castle-run-88"
		already.Status.ArchiveID = castle.RunArchiveID("castle-run-88")
		cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(already).WithObjects(already).Build()
		restore := newBackfillKubeClient
		defer func() { newBackfillKubeClient = restore }()
		newBackfillKubeClient = func(*rest.Config, *runtime.Scheme) (client.Client, error) { return cl, nil }

		err := runArchiveBackfill(context.Background(), cfg, scheme, &backfillMainArchiver{}, "", "tok", "criteria-jobs", "test", logr.Discard())
		require.NoError(t, err)
	})
}

type failingBackfillArchiver struct {
	base *backfillMainArchiver
	err  error
}

func (f *failingBackfillArchiver) ArchiveRun(ctx context.Context, req *connect.Request[castlev1.ArchiveRunRequest]) (*connect.Response[castlev1.ArchiveRunResponse], error) {
	return nil, f.err
}

func (f *failingBackfillArchiver) ListRunArchives(ctx context.Context, req *connect.Request[castlev1.ListRunArchivesRequest]) (*connect.Response[castlev1.ListRunArchivesResponse], error) {
	return f.base.ListRunArchives(ctx, req)
}

func (f *failingBackfillArchiver) Disabled() bool { return false }

func TestGetInt(t *testing.T) {
	t.Run("env wins over default", func(t *testing.T) {
		t.Setenv("CR_RETENTION_DAYS", "14")
		assert.Equal(t, 14, getInt("CR_RETENTION_DAYS", 7))
	})
	t.Run("unset falls back", func(t *testing.T) {
		os.Unsetenv("CR_RETENTION_KEEP_LAST")
		assert.Equal(t, 3, getInt("CR_RETENTION_KEEP_LAST", 3))
	})
	t.Run("non-numeric falls back", func(t *testing.T) {
		t.Setenv("CR_RETENTION_DAYS", "ten")
		assert.Equal(t, 7, getInt("CR_RETENTION_DAYS", 7))
	})
}

// The retention allowlist rides the --cr-retention-namespaces flag value
// (whose default resolves the CR_RETENTION_NAMESPACES env var at flag
// declaration): parseCsv's contract is what main feeds the sweeper
// constructor, so a trimmed/emptied allowlist comes out the same on either
// input path.
func TestParseCsv(t *testing.T) {
	t.Run("flag value splits on commas and trims", func(t *testing.T) {
		assert.Equal(t, []string{"criteria-jobs", "review-jobs"}, parseCsv("criteria-jobs, review-jobs"))
	})
	t.Run("blank and whitespace entries are dropped", func(t *testing.T) {
		assert.Equal(t, []string{"criteria-jobs"}, parseCsv(" criteria-jobs ,, "))
	})
	t.Run("empty value parses empty (sweep inert)", func(t *testing.T) {
		assert.Empty(t, parseCsv(""))
	})
}

func TestCastleTokenFromEnv(t *testing.T) {
	t.Run("CASTLE_TOKEN wins", func(t *testing.T) {
		t.Setenv("CASTLE_TOKEN", "env-token")
		t.Setenv("CASTLE_TOKEN_FILE", filepath.Join(t.TempDir(), "missing"))
		require.Equal(t, "env-token", castleTokenFromEnv())
	})
	t.Run("file token trimmed", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "token")
		require.NoError(t, os.WriteFile(path, []byte("file-token\n"), 0o400))
		os.Unsetenv("CASTLE_TOKEN")
		t.Setenv("CASTLE_TOKEN", "")
		t.Setenv("CASTLE_TOKEN_FILE", path)
		require.Equal(t, "file-token", castleTokenFromEnv())
	})
	t.Run("unreadable file leaves token empty", func(t *testing.T) {
		os.Unsetenv("CASTLE_TOKEN")
		t.Setenv("CASTLE_TOKEN", "")
		t.Setenv("CASTLE_TOKEN_FILE", filepath.Join(t.TempDir(), "missing"))
		require.Empty(t, castleTokenFromEnv())
	})
}

func TestShadowCastleTokenFromEnv(t *testing.T) {
	t.Run("SHADOW_CASTLE_TOKEN wins over file", func(t *testing.T) {
		t.Setenv("SHADOW_CASTLE_TOKEN", "shadow-token")
		path := filepath.Join(t.TempDir(), "missing")
		os.Unsetenv("SHADOW_CASTLE_TOKEN_FILE")
		t.Setenv("SHADOW_CASTLE_TOKEN_FILE", path)
		token, err := shadowCastleTokenFromEnv()
		require.NoError(t, err)
		require.Equal(t, "shadow-token", token)
	})
	t.Run("file token trimmed", func(t *testing.T) {
		os.Unsetenv("SHADOW_CASTLE_TOKEN")
		t.Setenv("SHADOW_CASTLE_TOKEN", "")
		path := filepath.Join(t.TempDir(), "token")
		require.NoError(t, os.WriteFile(path, []byte("shadow-file-token\n"), 0o400))
		t.Setenv("SHADOW_CASTLE_TOKEN_FILE", path)
		token, err := shadowCastleTokenFromEnv()
		require.NoError(t, err)
		require.Equal(t, "shadow-file-token", token)
	})
	t.Run("unreadable file errors and names the path", func(t *testing.T) {
		os.Unsetenv("SHADOW_CASTLE_TOKEN")
		t.Setenv("SHADOW_CASTLE_TOKEN", "")
		t.Setenv("SHADOW_CASTLE_TOKEN_FILE", filepath.Join(t.TempDir(), "missing"))
		token, err := shadowCastleTokenFromEnv()
		require.Error(t, err)
		require.Empty(t, token)
		require.Contains(t, err.Error(), "missing", "the read error names the path so the lane's disable is diagnosable")
	})
	t.Run("no shadow vars falls back to the shared token", func(t *testing.T) {
		t.Setenv("CASTLE_TOKEN", "shared-token")
		os.Unsetenv("SHADOW_CASTLE_TOKEN")
		os.Unsetenv("SHADOW_CASTLE_TOKEN_FILE")
		t.Setenv("SHADOW_CASTLE_TOKEN", "")
		t.Setenv("SHADOW_CASTLE_TOKEN_FILE", "")
		token, err := shadowCastleTokenFromEnv()
		require.NoError(t, err)
		require.Equal(t, "shared-token", token)
	})
}

// TestAgentCastleTokenFromEnv pins the assignment agent lane's token
// resolution (KB-338): agent env first, agent token file second (trimmed,
// path named on read errors — never the secret material), shared
// CASTLE_TOKEN last.
func TestAgentCastleTokenFromEnv(t *testing.T) {
	t.Run("CRITERIA_AGENT_CASTLE_TOKEN wins over file", func(t *testing.T) {
		t.Setenv("CRITERIA_AGENT_CASTLE_TOKEN", "agent-token")
		path := filepath.Join(t.TempDir(), "missing")
		t.Setenv("CRITERIA_AGENT_CASTLE_TOKEN_FILE", path)
		token, err := agentCastleTokenFromEnv()
		require.NoError(t, err)
		require.Equal(t, "agent-token", token)
	})
	t.Run("file token trimmed", func(t *testing.T) {
		t.Setenv("CRITERIA_AGENT_CASTLE_TOKEN", "")
		path := filepath.Join(t.TempDir(), "token")
		require.NoError(t, os.WriteFile(path, []byte("agent-file-token\n"), 0o400))
		t.Setenv("CRITERIA_AGENT_CASTLE_TOKEN_FILE", path)
		token, err := agentCastleTokenFromEnv()
		require.NoError(t, err)
		require.Equal(t, "agent-file-token", token)
	})
	t.Run("unreadable file errors and names the path", func(t *testing.T) {
		t.Setenv("CRITERIA_AGENT_CASTLE_TOKEN", "")
		t.Setenv("CRITERIA_AGENT_CASTLE_TOKEN_FILE", filepath.Join(t.TempDir(), "missing"))
		token, err := agentCastleTokenFromEnv()
		require.Error(t, err)
		require.Empty(t, token)
		require.Contains(t, err.Error(), "missing", "the read error names the path so the lane's disable is diagnosable")
	})
	t.Run("no agent vars falls back to the shared token", func(t *testing.T) {
		t.Setenv("CASTLE_TOKEN", "shared-token")
		t.Setenv("CRITERIA_AGENT_CASTLE_TOKEN", "")
		t.Setenv("CRITERIA_AGENT_CASTLE_TOKEN_FILE", "")
		token, err := agentCastleTokenFromEnv()
		require.NoError(t, err)
		require.Equal(t, "shared-token", token)
	})
}

// TestNewShadowRunBuilderWiring pins the operator's spec-mirror wiring
// seam: a deployment without a routes payload wires NO builder
// (decision-only posture — the "routes payload not configured" degrade
// must never reach the divergence log), and a configured payload wires the
// builder the watchdog's Configured() gate runs.
func TestNewShadowRunBuilderWiring(t *testing.T) {
	assert.False(t, (&control.RunBuilder{}).Configured(), "a nil-Routes builder is unconfigured")
	var absentBuilder *control.RunBuilder
	assert.False(t, absentBuilder.Configured(), "nil builder must not panic the gate")

	assert.Nil(t, newShadowRunBuilder(nil, runspec.Defaults{}, "criteria-jobs", nil, logr.Discard()),
		"no routes payload => no spec mirror; the shadow lane stays decision-only")

	payload := &routes.Payload{
		WorkflowLibrary: map[string]routes.Workflow{"kanboard-develop-url": {}},
	}
	b := newShadowRunBuilder(payload, runspec.Defaults{TestCmd: "make test"}, "criteria-jobs", nil, logr.Discard())
	require.NotNil(t, b, "a routes payload wires the spec mirror")
	assert.True(t, b.Configured())
	assert.Same(t, payload, b.Routes)
	assert.Equal(t, "criteria-jobs", b.Namespace)
	assert.Equal(t, "make test", b.Defaults.TestCmd)
	assert.NotNil(t, b.ConfigPinner,
		"the KB-103 config pinner is wired whenever the payload is (KB-338): a dispatch slot must stamp through to the CR, never silently resolve to lane defaults")
}

// TestResolveAgentRoutesPath pins the assignment lane's routes-file
// fallback (PR #232 review): an explicit --assignment-routes-file always
// wins, an empty one rides --shadow-routes-file (the documented way to
// enable the lane on a deployment that already runs the KB-319 mirror),
// and neither leaves the lane off. The second half asserts the resolved
// path is what actually gets loaded.
func TestResolveAgentRoutesPath(t *testing.T) {
	for _, tc := range []struct {
		name     string
		explicit string
		shadow   string
		want     string
	}{
		{name: "explicit wins over empty shadow", explicit: "routes-a.json", shadow: "", want: "routes-a.json"},
		{name: "explicit wins over set shadow", explicit: "routes-a.json", shadow: "routes-b.json", want: "routes-a.json"},
		{name: "empty explicit rides shadow", explicit: "", shadow: "routes-b.json", want: "routes-b.json"},
		{name: "neither set keeps the lane off", explicit: "", shadow: "", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveAgentRoutesPath(tc.explicit, tc.shadow)
			assert.Equal(t, tc.want, got)
		})
	}

	// The resolved path is the file actually read: a payload written to a
	// temp file loads through the shadow fallback, while the neither-set
	// path loads nothing (the lane-off posture).
	payload := map[string]any{
		"apiVersion": routes.APIVersion,
		"kind":       routes.Kind,
		"workflowLibrary": map[string]any{
			"smoke-workflow": map[string]any{"type": "image", "image": "localhost:5000/smoke:dev"},
		},
		"routes": []any{
			map[string]any{"name": "smoke-route", "workflow": "smoke-workflow", "project": "smoke-project", "states": []any{"Ready"}},
		},
	}
	data, err := json.Marshal(payload)
	require.NoError(t, err)
	tmp := filepath.Join(t.TempDir(), "shadow-routes.json")
	require.NoError(t, os.WriteFile(tmp, data, 0o644))

	loaded, err := loadShadowRoutes(resolveAgentRoutesPath("", tmp))
	require.NoError(t, err)
	require.NotNil(t, loaded, "the shadow path must load the lane's payload")
	assert.Equal(t, "image", loaded.WorkflowLibrary["smoke-workflow"].Type)
	assert.Equal(t, "localhost:5000/smoke:dev", loaded.WorkflowLibrary["smoke-workflow"].Image)

	absentPath := resolveAgentRoutesPath("", "")
	absent, absentErr := loadShadowRoutes(absentPath)
	require.NoError(t, absentErr, "a neither-set posture is not an error, it is lane-off")
	assert.Nil(t, absent, "an empty resolved path loads no payload")
}

func TestDeriveRunEventsEndpoint(t *testing.T) {
	// Default operator posture: the boot gate listener's port back-ends
	// the derived svc endpoint (KB-344).
	assert.Equal(t,
		"http://criteria-k8s-operator.criteria-system.svc:8082",
		deriveRunEventsEndpoint(":8082", "", "criteria-k8s-operator", "criteria-system"))

	// An explicit --run-events-addr wins over the derivation.
	assert.Equal(t,
		"http://operator.ingress.svc:9090",
		deriveRunEventsEndpoint(":8082", "http://operator.ingress.svc:9090", "criteria-k8s-operator", "criteria-system"))

	// A host:port listener still yields the port-only svc endpoint.
	assert.Equal(t,
		"http://criteria-k8s-operator.criteria-system.svc:9099",
		deriveRunEventsEndpoint("0.0.0.0:9099", "", "criteria-k8s-operator", "criteria-system"))

	// Lane-off postures keep the endpoint empty: runner Jobs then render
	// without CASTLE_ADDR and stay in local file mode.
	assert.Equal(t, "", deriveRunEventsEndpoint("", "", "criteria-k8s-operator", "criteria-system"))
	assert.Equal(t, "", deriveRunEventsEndpoint(":8082-noport", "", "criteria-k8s-operator", "criteria-system"))
}
