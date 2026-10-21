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
	"errors"
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
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/controller"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/jobbuilder"
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
