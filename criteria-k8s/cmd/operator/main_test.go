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
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
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

	sdkv1 "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	criteriav1connect "github.com/brokenbots/criteria/sdk/pb/criteria/v1/criteriav1connect"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/castle"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/control"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/controller"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/events"
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

// fallbackCaptureSink records the error logs resolveAgentCastleToken emits,
// so the consumer-naming contract of the shared credential resolution's
// degrade path can be pinned without a logging dependency.
type fallbackCaptureSink struct {
	msgs []string
	errs []string
}

func (s *fallbackCaptureSink) Enabled(int) bool               { return true }
func (s *fallbackCaptureSink) Init(logr.RuntimeInfo)          {}
func (s *fallbackCaptureSink) Info(int, string, ...any)       {}
func (s *fallbackCaptureSink) WithValues(...any) logr.LogSink { return s }
func (s *fallbackCaptureSink) WithName(string) logr.LogSink   { return s }
func (s *fallbackCaptureSink) Error(err error, msg string, _ ...any) {
	s.msgs = append(s.msgs, msg)
	s.errs = append(s.errs, err.Error())
}

// TestResolveAgentCastleTokenFallbackLogsItsConsumer pins the KB-345
// review's log-naming rule: the shared agent-credential resolution serves
// several lanes (run-event proxy, pod-state feed), so an unreadable
// credential file logs WHICH lane degraded, and every lane keeps the
// diagnosable path+cause error — never secret material — while degrading to
// the shared token exactly as the resolution itself prescribes.
func TestResolveAgentCastleTokenFallbackLogsItsConsumer(t *testing.T) {
	t.Setenv("CRITERIA_AGENT_CASTLE_TOKEN", "")
	missing := filepath.Join(t.TempDir(), "missing")
	t.Setenv("CRITERIA_AGENT_CASTLE_TOKEN_FILE", missing)
	t.Setenv("CASTLE_TOKEN", "shared-orchestrator-token")
	sink := &fallbackCaptureSink{}

	token := resolveAgentCastleToken(logr.New(sink), "pod-state feed")

	require.Len(t, sink.msgs, 1, "the unreadable credential file must be logged loudly once")
	assert.Equal(t, "pod-state feed: agent credential file unreadable; falling back to the shared CASTLE_TOKEN", sink.msgs[0],
		"the fallback log must name the consumer so an operator sees which lane degraded")
	require.Len(t, sink.errs, 1)
	assert.Contains(t, sink.errs[0], "missing", "the logged error keeps the diagnosable path, never secret material")
	assert.Equal(t, "shared-orchestrator-token", token, "the unreadable file still resolves the lane-off fallback token")
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

func TestResolveRunEventsLane(t *testing.T) {
	// Default operator posture: the boot gate listener's port back-ends
	// the derived svc endpoint (KB-344), and the derivation reports no
	// disabling reason.
	endpoint, reason := resolveRunEventsLane(":8082", "", "criteria-k8s-operator", "criteria-system", "http://castle:8081", "tok")
	assert.Equal(t, "http://criteria-k8s-operator.criteria-system.svc:8082", endpoint)
	assert.Equal(t, "", reason)

	// An explicit --run-events-addr wins over the derivation.
	endpoint, reason = resolveRunEventsLane(":8082", "http://operator.ingress.svc:9090", "criteria-k8s-operator", "criteria-system", "http://castle:8081", "tok")
	assert.Equal(t, "http://operator.ingress.svc:9090", endpoint)
	assert.Equal(t, "", reason)

	// A host:port listener still yields the port-only svc endpoint.
	endpoint, reason = resolveRunEventsLane("0.0.0.0:9099", "", "criteria-k8s-operator", "criteria-system", "http://castle:8081", "tok")
	assert.Equal(t, "http://criteria-k8s-operator.criteria-system.svc:9099", endpoint)
	assert.Equal(t, "", reason)

	// KB-344 review remediation: the three off-cases each keep the
	// endpoint empty and name their reason, so a runner Job never dials a
	// proxy whose forward path is dead. Empty boot gate:
	endpoint, reason = resolveRunEventsLane("", "", "criteria-k8s-operator", "criteria-system", "http://castle:8081", "tok")
	assert.Equal(t, "", endpoint)
	assert.Contains(t, reason, "boot gate listener")
	// Empty castle endpoint:
	endpoint, reason = resolveRunEventsLane(":8082", "", "criteria-k8s-operator", "criteria-system", "", "tok")
	assert.Equal(t, "", endpoint)
	assert.Contains(t, reason, "castle endpoint")
	// Empty castle credential (no agent credential, no shared token):
	endpoint, reason = resolveRunEventsLane(":8082", "", "criteria-k8s-operator", "criteria-system", "http://castle:8081", "")
	assert.Equal(t, "", endpoint)
	assert.Contains(t, reason, "castle credential")

	// A partial configuration stays off even when an explicit endpoint is
	// configured: the gate runs before the override.
	endpoint, reason = resolveRunEventsLane("", "http://operator.ingress.svc:9090", "criteria-k8s-operator", "criteria-system", "http://castle:8081", "tok")
	assert.Equal(t, "", endpoint)
	assert.NotEqual(t, "", reason)

	// Derivation postures that cannot produce an endpoint stay off too.
	endpoint, reason = resolveRunEventsLane(":8082-noport", "", "criteria-k8s-operator", "criteria-system", "http://castle:8081", "tok")
	assert.Equal(t, "", endpoint)
	assert.NotEqual(t, "", reason)
}

// TestBootgateViewAddrDefault pins the KB-344 lane-off contract at the env
// → flag-default layer: BOOTGATE_VIEW_ADDR set to the empty string resolves
// to an empty --bootgate-view-addr (the chart renders that for
// runEventsPort 0: no listener, no derived proxy endpoint), while an unset
// variable keeps the legacy :8082 default. A plain getenv fallback cannot
// express the set-but-empty case — that is the defect this pins.
func TestBootgateViewAddrDefault(t *testing.T) {
	const envKey = "BOOTGATE_VIEW_ADDR"
	oldValue, hadValue := os.LookupEnv(envKey)
	t.Cleanup(func() {
		if hadValue {
			os.Setenv(envKey, oldValue)
		} else {
			os.Unsetenv(envKey)
		}
	})

	// Unset: the legacy default keeps the view (and the derived run-event
	// endpoint) up.
	os.Unsetenv(envKey)
	assert.Equal(t, ":8082", bootgateViewAddrDefault())

	// Explicitly empty: the operator's caller opted the lane out — an
	// empty listen address disables it.
	os.Setenv(envKey, "")
	assert.Equal(t, "", bootgateViewAddrDefault())

	// Explicitly set: the operator's value wins.
	os.Setenv(envKey, "0.0.0.0:9099")
	assert.Equal(t, "0.0.0.0:9099", bootgateViewAddrDefault())
}

// TestRunEventsLaneEnvShapes pins the deployment postures' env shapes to
// the lane state they must produce, mirroring main()'s composition (env →
// flag default → agent credential → lane derivation): the per-project
// render's explicitly empty BOOTGATE_VIEW_ADDR keeps the run-event proxy
// lane off (runner Jobs then render without CASTLE_ADDR and keep their
// local events file), while the unset-variable env keeps the :8082 lane
// derivable. CASTLE_ADDR rides the flag layer (bound at package init in
// production), so the test sets the flag global to the env value main()
// would have resolved.
func TestRunEventsLaneEnvShapes(t *testing.T) {
	envOverride := func(key, value string) func() {
		old, had := os.LookupEnv(key)
		return func() {
			if had {
				os.Setenv(key, old)
			} else {
				os.Unsetenv(key)
			}
		}
	}
	// The per-project env shape (k8s/project-lifecycle.sh): a cross-ns
	// castle endpoint + shared token file, no agent credential.
	for _, key := range []string{"CASTLE_ADDR", "CRITERIA_AGENT_CASTLE_TOKEN", "CRITERIA_AGENT_CASTLE_TOKEN_FILE"} {
		t.Cleanup(envOverride(key, os.Getenv(key)))
	}
	os.Unsetenv("CRITERIA_AGENT_CASTLE_TOKEN")
	os.Unsetenv("CRITERIA_AGENT_CASTLE_TOKEN_FILE")
	os.Setenv("CASTLE_ADDR", "http://castle.juice-platform.svc.cluster.local:8080")
	oldFlagCastleAddr := *castleAddr
	*castleAddr = os.Getenv("CASTLE_ADDR")
	t.Cleanup(func() { *castleAddr = oldFlagCastleAddr })

	// CASTLE_ADDR alone does not forward a run's own identity — the agent
	// token comes from the operator's own castle-token file only.
	tokenFile := filepath.Join(t.TempDir(), "castle_token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("project-castle-tok\n"), 0o600))
	t.Cleanup(envOverride("CASTLE_TOKEN_FILE", os.Getenv("CASTLE_TOKEN_FILE")))
	os.Setenv("CASTLE_TOKEN_FILE", tokenFile)
	proxyToken, err := agentCastleTokenFromEnv()
	require.NoError(t, err)
	require.Equal(t, "project-castle-tok", proxyToken)

	t.Run("explicitly empty BOOTGATE_VIEW_ADDR keeps the proxy lane off", func(t *testing.T) {
		t.Cleanup(envOverride("BOOTGATE_VIEW_ADDR", os.Getenv("BOOTGATE_VIEW_ADDR")))
		os.Setenv("BOOTGATE_VIEW_ADDR", "")
		addr := bootgateViewAddrDefault()
		assert.Equal(t, "", addr, "the lane-off render must resolve an empty --bootgate-view-addr")

		endpoint, reason := resolveRunEventsLane(addr, "", "criteria-k8s-operator", "criteria-go-dev", *castleAddr, proxyToken)
		assert.Equal(t, "", endpoint, "no endpoint may derive from an opted-out listener")
		assert.Contains(t, reason, "boot gate listener")
	})

	t.Run("unset BOOTGATE_VIEW_ADDR keeps the :8082 lane", func(t *testing.T) {
		t.Cleanup(envOverride("BOOTGATE_VIEW_ADDR", os.Getenv("BOOTGATE_VIEW_ADDR")))
		os.Unsetenv("BOOTGATE_VIEW_ADDR")
		addr := bootgateViewAddrDefault()
		assert.Equal(t, ":8082", addr, "an unset variable keeps the legacy default, unlike the explicit empty render")

		endpoint, reason := resolveRunEventsLane(addr, "", "criteria-k8s-operator", "criteria-go-dev", *castleAddr, proxyToken)
		assert.Equal(t, "http://criteria-k8s-operator.criteria-go-dev.svc:8082", endpoint)
		assert.Equal(t, "", reason)
	})
}

// podStateSubmitStub implements the slice of the SDK's CriteriaServiceHandler
// the pod-state feed consumes, recording the credential riding each submit.
// Other methods embed the nil handler and panic if called. Operator-level
// mirror of internal/castle's podStateCriteriaStub (CRI-264: composition in
// package main is invisible to the package suites, so the wiring is pinned
// here against a live criteria stub).
type podStateSubmitStub struct {
	criteriav1connect.CriteriaServiceHandler

	mu     sync.Mutex
	tokens []string
}

func (s *podStateSubmitStub) SubmitEvents(ctx context.Context, stream *connect.BidiStream[sdkv1.Envelope, sdkv1.Ack]) error {
	s.mu.Lock()
	s.tokens = append(s.tokens, stream.RequestHeader().Get("X-Criteria-Token"))
	s.mu.Unlock()
	for {
		if _, err := stream.Receive(); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return err
		}
	}
	return stream.Send(&sdkv1.Ack{RunId: "run-ack"})
}

func (s *podStateSubmitStub) tokenAt(i int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i >= len(s.tokens) {
		return ""
	}
	return s.tokens[i]
}

func (s *podStateSubmitStub) tokenCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.tokens)
}

// TestPodStateFeedSubmitCredentialClass pins the KB-345 wiring rule by
// driving the shipped operator path end to end: env → newPodStateFeed (the
// production constructor, which resolves the assignment lane's AGENT-class
// credential through resolveAgentCastleToken) → Emit → the credential
// header castle receives. The feed must submit under the lane's
// AGENT-class credential (CRITERIA_AGENT_CASTLE_TOKEN / its mounted file),
// falling back to the shared orchestrator CASTLE_TOKEN only when the
// lane's credential is unwired — castle's SubmitEvents gate denies
// orchestrator-class identities from agent-owned procedures, so the
// pre-345 wiring (Token: castleToken) left every podstate emit rejecting
// ('pod-state emit failed') and no event persisting on a lane-credential
// deployment. The stub is served over h2c exactly as castle's cleartext
// SubmitEvents port does, so the production nil-client default stays
// exercised (KB-239).
func TestPodStateFeedSubmitCredentialClass(t *testing.T) {
	newStubServer := func(t *testing.T) (*podStateSubmitStub, string) {
		t.Helper()
		stub := &podStateSubmitStub{}
		path, handler := criteriav1connect.NewCriteriaServiceHandler(stub)
		mux := http.NewServeMux()
		mux.Handle(path, handler)
		srv := httptest.NewServer(h2c.NewHandler(mux, &http2.Server{}))
		t.Cleanup(srv.Close)
		return stub, srv.URL
	}
	emitOne := func(t *testing.T, stub *podStateSubmitStub, addr string) {
		t.Helper()
		feed := newPodStateFeed(addr, logr.Discard())
		feed.Emit(context.Background(), "run-345",
			[]events.PodStateReport{{
				AdapterName: "intake",
				AdapterType: "shell",
				ScopeName:   "develop",
				ScopeID:     "dev-001",
				Pod:         "shell-develop-dev-001-adapter",
				Phase:       events.PodPhasePending,
				Reason:      "Unschedulable",
				Message:     "0/4 nodes are available",
				ObservedAt:  time.Now().UTC(),
			}}, logr.Discard())
		require.Equal(t, 1, stub.tokenCount(), "the emit must reach the stub (the podstate submit path)")
	}

	t.Run("lane credential configured: the emit carries the lane token, never the shared one", func(t *testing.T) {
		t.Setenv("CRITERIA_AGENT_CASTLE_TOKEN", "agent-lane-token")
		t.Setenv("CRITERIA_AGENT_CASTLE_TOKEN_FILE", "")
		t.Setenv("CASTLE_TOKEN", "shared-orchestrator-token")
		stub, addr := newStubServer(t)
		emitOne(t, stub, addr)
		assert.Equal(t, "agent-lane-token", stub.tokenAt(0),
			"the pod-state submit must ride the assignment lane's AGENT-class credential (KB-345)")
	})

	t.Run("mount-file lane credential: trimmed before the submit", func(t *testing.T) {
		t.Setenv("CRITERIA_AGENT_CASTLE_TOKEN", "")
		tokenFile := filepath.Join(t.TempDir(), "castle_token")
		require.NoError(t, os.WriteFile(tokenFile, []byte("agent-file-token\n"), 0o600))
		t.Setenv("CRITERIA_AGENT_CASTLE_TOKEN_FILE", tokenFile)
		stub, addr := newStubServer(t)
		emitOne(t, stub, addr)
		assert.Equal(t, "agent-file-token", stub.tokenAt(0),
			"the mounted-file credential rides the submit exactly as the env credential does")
	})

	t.Run("lane off: the emit falls back to the shared orchestrator token", func(t *testing.T) {
		t.Setenv("CRITERIA_AGENT_CASTLE_TOKEN", "")
		t.Setenv("CRITERIA_AGENT_CASTLE_TOKEN_FILE", "")
		t.Setenv("CASTLE_TOKEN", "shared-orchestrator-token")
		stub, addr := newStubServer(t)
		emitOne(t, stub, addr)
		assert.Equal(t, "shared-orchestrator-token", stub.tokenAt(0),
			"with the lane's credential unwired the feed keeps its pre-lane shared identity")
	})
}

// TestNewPodStateFeedIsTheSoleBuildSite guards the KB-345 wiring: the
// operator must construct the pod-state feed exclusively through
// newPodStateFeed, the function that carries the lane-credential rule.
// An inline castle.NewPodStateFeed call site (e.g. a revert to
// Token: castleToken) would bypass the rule invisibly to the suite above,
// so main.go is pinned to exactly one build site — inside the constructor.
func TestNewPodStateFeedIsTheSoleBuildSite(t *testing.T) {
	src, err := os.ReadFile("main.go")
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(string(src), "castle.NewPodStateFeed("),
		"the pod-state feed must be built only inside newPodStateFeed; an inline build site bypasses the KB-345 credential rule")
}
