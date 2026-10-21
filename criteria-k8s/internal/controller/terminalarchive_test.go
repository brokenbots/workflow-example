package controller_test

// Terminal archive gate tests (KB-314): the gate sits in every cleanup path
// (persisted terminal branch, watchdog teardown, finalize) and must land the
// archive record BEFORE child jobs are reaped or the finalizer is removed —
// a failed archive defers the cleanup with the TerminalArchivePending
// condition instead of deleting the run's evidence. The backfill tests cover
// the pre-gate backlog walk (batched, resumable, idempotent).

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	castlev1 "github.com/brokenbots/castle/castle/gen/castle/v1"
	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/castle"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/controller"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

// fakeArchiver collects the ArchiveRun calls the gate makes. runKeyedErrors
// can reproduce castle's ownership gate for records whose castle run id the
// archive lane's token does not own — the condition forcing the operator's
// cr-uid fallback (runner-pod agents register with castle anonymously; the
// archive lane carries a token).
type fakeArchiver struct {
	calls struct {
		sync.Mutex
		records       map[string]*castlev1.ArchiveRecord
		jobsAtArchive []int
		archivedKeys  []string
	}
	// err is the canned failure; errOnlyRunKeyed scopes it to run-keyed
	// records (castle's per-record ownership gate), so the cr-uid fallback
	// write can still succeed underneath it.
	err             error
	errOnlyRunKeyed bool
	jobProbe        func() int // child jobs visible at call time
	disabled        bool
}

func newFakeArchiver() *fakeArchiver {
	return &fakeArchiver{calls: struct {
		sync.Mutex
		records       map[string]*castlev1.ArchiveRecord
		jobsAtArchive []int
		archivedKeys  []string
	}{records: map[string]*castlev1.ArchiveRecord{}}}
}

func (f *fakeArchiver) ArchiveRun(ctx context.Context, req *connect.Request[castlev1.ArchiveRunRequest]) (*connect.Response[castlev1.ArchiveRunResponse], error) {
	f.calls.Lock()
	defer f.calls.Unlock()
	if f.jobProbe != nil {
		f.calls.jobsAtArchive = append(f.calls.jobsAtArchive, f.jobProbe())
	}
	rec := req.Msg.GetRecord()
	if f.err != nil && (!f.errOnlyRunKeyed || rec.GetRunId() != "") {
		return nil, f.err
	}
	if rec.GetRunId() != "" && rec.GetArchiveKey() != "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, nil)
	}
	key := rec.GetArchiveKey()
	if key == "" {
		key = rec.GetRunId()
	}
	if rec.GetRunId() == "" && key != "" && !strings.HasPrefix(key, castle.CRUIDArchiveKeyPrefix) {
		return nil, connect.NewError(connect.CodeInvalidArgument, nil)
	}
	f.calls.records[key] = rec
	f.calls.archivedKeys = append(f.calls.archivedKeys, key)
	return connect.NewResponse(&castlev1.ArchiveRunResponse{ArchiveId: castle.RunArchiveID(key), Created: true}), nil
}

func (f *fakeArchiver) ListRunArchives(ctx context.Context, req *connect.Request[castlev1.ListRunArchivesRequest]) (*connect.Response[castlev1.ListRunArchivesResponse], error) {
	return connect.NewResponse(&castlev1.ListRunArchivesResponse{}), nil
}

func (f *fakeArchiver) Disabled() bool { return f != nil && f.disabled }

func (f *fakeArchiver) record(key string) *castlev1.ArchiveRecord {
	f.calls.Lock()
	defer f.calls.Unlock()
	return f.calls.records[key]
}

func (f *fakeArchiver) keyCount() int {
	f.calls.Lock()
	defer f.calls.Unlock()
	return len(f.calls.records)
}

// terminalRun builds a persisted-terminal run with its finalizer and its
// (completed) runner job still alive — exactly the state the archive gate
// must preserve until the record lands.
func terminalArchivableRun(name, uid, phase, castleRunID string) *criteriav1.CriteriaRun {
	return &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  "default",
			UID:        types.UID(uid),
			Finalizers: []string{"criteriarun.criteria.brokenbots.dev/finalizer"},
		},
		Spec: criteriav1.CriteriaRunSpec{
			TicketID: "CRI-42",
			RepoURL:  "https://github.com/brokenbots/workflow-example.git",
		},
		Status: criteriav1.CriteriaRunStatus{
			Phase:       criteriav1.CriteriaRunPhase(phase),
			JobName:     name,
			CastleRunID: castleRunID,
		},
	}
}

func terminalRunnerJob(name string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
			},
		},
	}
}

type gateHarness struct {
	scheme   *runtime.Scheme
	cl       client.Client
	castle   *fakeCastle
	archiver *fakeArchiver
}

func newGateHarness(t *testing.T, objs ...client.Object) *gateHarness {
	t.Helper()
	s := newScheme(t)
	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(objs...).
		WithObjects(objs...).
		Build()
	return &gateHarness{scheme: s, cl: cl, castle: &fakeCastle{}, archiver: newFakeArchiver()}
}

// newSchemeAndClient builds a bare client (no harness) for the backfill
// walks, which construct their own gate.
func newSchemeAndClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	s := newScheme(t)
	return fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(objs...).
		WithObjects(objs...).
		Build()
}

func (h *gateHarness) reconciler() *controller.CriteriaRunReconciler {
	return &controller.CriteriaRunReconciler{
		Client:          h.cl,
		Scheme:          h.scheme,
		Castle:          h.castle,
		RunArchiver:     h.archiver,
		OperatorVersion: "test-1.2.3",
		Queue:           controller.NewRunQueue(),
	}
}

func (h *gateHarness) reconcile(t *testing.T, run client.Object) (ctrl.Result, error) {
	t.Helper()
	return h.reconciler().Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
}

func pendingConditionTrue(run *criteriav1.CriteriaRun) bool {
	for _, cond := range run.Status.Conditions {
		if cond.Type == criteriav1.ConditionTerminalArchivePending {
			return cond.Status == metav1.ConditionTrue
		}
	}
	return false
}

func childJobCount(t *testing.T, cl client.Client, namespace string) int {
	t.Helper()
	var jobs batchv1.JobList
	require.NoError(t, cl.List(context.Background(), &jobs, client.InNamespace(namespace)))
	return len(jobs.Items)
}

// The core guarantee: an archive record exists before the runner job is
// reaped, the archive id lands in status, and the terminal pass stops
// polling after landing it.
func TestTerminalGateArchivesBeforeChildJobReap(t *testing.T) {
	run := terminalArchivableRun("cri-42", "cr-uid-1", "Succeeded", "castle-run-1")
	job := terminalRunnerJob("cri-42")
	harness := newGateHarness(t, run, job)
	harness.castle.observation = &castle.Observation{RunID: "castle-run-1", Terminal: castleTerminal(true)}
	cl, archiver := harness.cl, harness.archiver
	archiver.jobProbe = func() int { return childJobCount(t, cl, "default") }

	res, err := harness.reconcile(t, run)
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, res, "a landed archive with an observed terminal stops the poll")

	archiver.calls.Lock()
	probe := append([]int(nil), archiver.calls.jobsAtArchive...)
	keys := append([]string(nil), archiver.calls.archivedKeys...)
	archiver.calls.Unlock()
	require.Len(t, probe, 1)
	assert.Positive(t, probe[0], "the archive call ran while the runner job was still alive")
	assert.Equal(t, []string{"castle-run-1"}, keys, "run-keyed records key off the persisted castle run id")

	var updated criteriav1.CriteriaRun
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(run), &updated))
	assert.Equal(t, castle.RunArchiveID("castle-run-1"), updated.Status.ArchiveID)
	assert.False(t, updated.Status.ArchivePending)
	assert.False(t, pendingConditionTrue(&updated))
	assert.Equal(t, 0, childJobCount(t, cl, "default"), "the runner job is reaped only after the archive landed")

	stored := archiver.record("castle-run-1")
	assert.Equal(t, "castle-run-1", stored.GetRunId())
	assert.Equal(t, "CRI-42", stored.GetTicket())
	assert.Equal(t, "Succeeded", stored.GetFinalPhase())
	assert.Equal(t, "test-1.2.3", stored.GetOperatorVersion())
}

// When the archive write leg fails, the pass requeues (returned error) with
// TerminalArchivePending stamped, and NOTHING is reaped: the job — and the
// evidence — survives until the archive lands. A later pass with a healthy
// leg archives, stamps the id, flips the condition, and completes the reap.
func TestTerminalGateFailureDefersReapUntilArchived(t *testing.T) {
	run := terminalArchivableRun("cri-42", "cr-uid-2", "Succeeded", "castle-run-1")
	job := terminalRunnerJob("cri-42")
	harness := newGateHarness(t, run, job)
	harness.castle.observation = &castle.Observation{RunID: "castle-run-1", Terminal: castleTerminal(true)}
	cl, archiver := harness.cl, harness.archiver
	archiver.err = connect.NewError(connect.CodeUnavailable, nil)

	_, err := harness.reconcile(t, run)
	require.Error(t, err, "the failed gate requeues with backoff (returned error)")

	var updated criteriav1.CriteriaRun
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(run), &updated))
	assert.Empty(t, updated.Status.ArchiveID)
	assert.True(t, pendingConditionTrue(&updated))
	assert.Equal(t, 1, childJobCount(t, cl, "default"), "the runner job survives an unarchived terminal pass")

	// The write leg recovers: the next pass lands the record, flips the
	// condition, and completes the reap — exactly one landed key (the
	// failure wrote none).
	archiver.err = nil
	_, err = harness.reconcile(t, run)
	require.NoError(t, err)

	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(run), &updated))
	assert.Equal(t, castle.RunArchiveID("castle-run-1"), updated.Status.ArchiveID)
	assert.False(t, pendingConditionTrue(&updated))
	assert.Equal(t, 0, childJobCount(t, cl, "default"))
	assert.Equal(t, 1, archiver.keyCount())
}

// Without an archiver the gate is a pass-through: the pre-archive-leg
// behavior (terminal reap proceeds) is preserved and no archive fields are
// stamped.
func TestTerminalGateSkipsWithoutArchiver(t *testing.T) {
	run := terminalArchivableRun("cri-42", "cr-uid-3", "Succeeded", "castle-run-1")
	job := terminalRunnerJob("cri-42")
	harness := newGateHarness(t, run, job)
	harness.castle.observation = &castle.Observation{RunID: "castle-run-1", Terminal: castleTerminal(true)}
	cl := harness.cl

	r := harness.reconciler()
	r.RunArchiver = nil
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	require.NoError(t, err)

	var updated criteriav1.CriteriaRun
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(run), &updated))
	assert.Empty(t, updated.Status.ArchiveID)
	assert.Equal(t, 0, childJobCount(t, cl, "default"), "reaping is untouched with the archive leg off")
}

// Records whose castle run id resolves to a run the archive lane's token
// does not own (runner-pod agents register anonymously; the archive lane
// carries a token) fall back to the CR-uid derived key: run_id is cleared,
// the record lands under cr-uid/<uid>, and the status stamp carries
// castle's run-archive/<cr-uid/...> id. The reap completes in the same pass.
func TestTerminalGateCRUIDFallbackOnPermissionDenied(t *testing.T) {
	run := terminalArchivableRun("cri-42", "cr-uid-4", "Succeeded", "castle-run-1")
	job := terminalRunnerJob("cri-42")
	harness := newGateHarness(t, run, job)
	harness.castle.observation = &castle.Observation{RunID: "castle-run-1", Terminal: castleTerminal(true)}
	cl, archiver := harness.cl, harness.archiver
	archiver.err = connect.NewError(connect.CodePermissionDenied, nil)
	archiver.errOnlyRunKeyed = true

	res, err := harness.reconcile(t, run)
	require.NoError(t, err, "the fallback keys the record onto the CR uid and lands it in the same pass")
	assert.Equal(t, ctrl.Result{}, res)

	uidKey := castle.CRUIDArchiveKey("cr-uid-4")
	archiver.calls.Lock()
	keys := append([]string(nil), archiver.calls.archivedKeys...)
	archiver.calls.Unlock()
	assert.Equal(t, []string{uidKey}, keys)
	stored := archiver.record(uidKey)
	require.NotNil(t, stored)
	assert.Empty(t, stored.GetRunId(), "castle refuses both idents; the fallback clears run_id")
	assert.Equal(t, "CRI-42", stored.GetTicket())

	var updated criteriav1.CriteriaRun
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(run), &updated))
	assert.Equal(t, castle.RunArchiveID(uidKey), updated.Status.ArchiveID)
	assert.Equal(t, 0, childJobCount(t, cl, "default"), "the fallback landing completes the reap in the same pass")
}

// A delete of a terminal CR cannot finalize while its archive write leg is
// down: the finalizer stays (the run and its evidence survive), and the next
// pass with a healthy leg archives before the finalizer comes off.
func TestFinalizeGateBlocksFinalizerRemoval(t *testing.T) {
	deleteReq := terminalArchivableRun("cri-42", "cr-uid-5", "Succeeded", "castle-run-1")
	deleter := deleteReq.DeepCopy()
	deleter.DeletionTimestamp = &mowTime
	job := terminalRunnerJob("cri-42")
	harness := newGateHarness(t, deleter, job)
	harness.castle.observation = &castle.Observation{RunID: "castle-run-1", Terminal: castleTerminal(true)}
	cl, archiver := harness.cl, harness.archiver

	archiver.err = connect.NewError(connect.CodeUnavailable, nil)
	_, err := harness.reconcile(t, deleter)
	require.Error(t, err, "the finalize pass must fail while the archive leg is down")

	var held criteriav1.CriteriaRun
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(deleteReq), &held))
	assert.NotEmpty(t, held.Finalizers, "the finalizer holds the run until the record lands")
	assert.Equal(t, 1, childJobCount(t, cl, "default"), "the runner job survives too")

	archiver.err = nil
	_, err = harness.reconcile(t, deleter)
	require.NoError(t, err)

	var gone criteriav1.CriteriaRun
	errGet := cl.Get(context.Background(), client.ObjectKeyFromObject(deleteReq), &gone)
	require.Error(t, errGet, "the CR is deleted once the finalizer came off")
	assert.Equal(t, 1, archiver.keyCount(), "the healthy pass archived the run before finalizing")

	archiver.calls.Lock()
	keys := append([]string(nil), archiver.calls.archivedKeys...)
	archiver.calls.Unlock()
	assert.Equal(t, []string{"castle-run-1"}, keys)
}

// The backfill walks terminal CRs, skips stamped ones, archives the rest on
// the CR-uid key (terminal runs' castle ids are typically unenrichable), and
// re-running converges: stamped runs are no-ops. Running CRs are untouched.
func TestBackfillRunArchivesWalksAndConverges(t *testing.T) {
	stamped := terminalArchivableRun("cri-20", "cr-uid-20", "Succeeded", "castle-run-20")
	stamped.Status.ArchiveID = castle.RunArchiveID("castle-run-20")
	failed := terminalArchivableRun("cri-21", "cr-uid-21", "Failed", "")
	ticketRuns := terminalArchivableRun("cri-23", "cr-uid-23", "Failed", "castle-run-23")
	ticketRuns.Spec.TicketID = "CRI-23"

	cl := newSchemeAndClient(t, stamped, failed, ticketRuns)
	archiver := newFakeArchiver()

	res, err := controller.BackfillRunArchives(context.Background(), cl, archiver, nil, "default", "test-1.2.3", logr.Discard())
	require.NoError(t, err)
	assert.Equal(t, 3, res.TerminalRuns)
	assert.Equal(t, 1, res.AlreadyArchived, "a stamped run is skipped without an archive call")
	assert.Equal(t, 2, res.Archived)
	assert.Equal(t, 0, res.Failed)

	// The unenrichable terminal run archives under its CR-uid key with
	// run_id cleared; the run-keyed one keeps its castle run id.
	uidStored := archiver.record(castle.CRUIDArchiveKey("cr-uid-21"))
	require.NotNil(t, uidStored)
	assert.Empty(t, uidStored.GetRunId())
	assert.Equal(t, "Failed", uidStored.GetFinalPhase())
	require.NotNil(t, archiver.record("castle-run-23"))

	var stamped21 criteriav1.CriteriaRun
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(failed), &stamped21))
	assert.Equal(t, castle.RunArchiveID(castle.CRUIDArchiveKey("cr-uid-21")), stamped21.Status.ArchiveID,
		"backfill stamps the archive id, making reconciles and re-runs no-ops")

	missing, err := controller.CountMissingRunArchives(context.Background(), cl, "default")
	require.NoError(t, err)
	assert.Equal(t, 0, missing, "the coverage gate closes")

	// Re-run: everything is stamped; nothing is archived again.
	res, err = controller.BackfillRunArchives(context.Background(), cl, archiver, nil, "default", "test-1.2.3", logr.Discard())
	require.NoError(t, err)
	assert.Equal(t, 0, res.Archived)
	assert.Equal(t, 3, res.AlreadyArchived)
	assert.Equal(t, 2, archiver.keyCount())
}

// The archive leg being down during backfill leaves those runs FAILED but
// does not panic or skip silently: the result carries the failure count and
// coverage stays short, so the caller (runArchiveBackfill) exits non-zero
// and a re-run resumes.
func TestBackfillRunArchivesCountsLegFailure(t *testing.T) {
	failed := terminalArchivableRun("cri-31", "cr-uid-31", "Failed", "")
	ok := terminalArchivableRun("cri-32", "cr-uid-32", "Succeeded", "castle-run-32")

	cl := newSchemeAndClient(t, failed, ok)
	archiver := newFakeArchiver()
	archiver.err = connect.NewError(connect.CodeUnavailable, nil)

	res, err := controller.BackfillRunArchives(context.Background(), cl, archiver, nil, "default", "test-1.2.3", logr.Discard())
	require.NoError(t, err)
	assert.Equal(t, 2, res.TerminalRuns)
	assert.Equal(t, 2, res.Failed, "both archive attempts failed; the walk continued")
	assert.Equal(t, 0, res.Archived)
	assert.Equal(t, 0, archiver.keyCount())

	missing, err := controller.CountMissingRunArchives(context.Background(), cl, "default")
	require.NoError(t, err)
	assert.Equal(t, 2, missing, "coverage stays short; the caller must exit non-zero")
}

var (
	mowTime = metav1.NewTime(time.Now())
)
