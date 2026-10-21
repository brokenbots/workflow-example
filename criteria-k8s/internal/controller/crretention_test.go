package controller

// CR retention tests (KB-309): the sweep deletes archived terminal runs
// older than the retention window and reaps whatever their deletion left
// ownerless. Everything else is protected: parked runs (need_assistance /
// awaiting_human), runs the archive gate has not landed, runs it cannot
// date, recent siblings of their ticket family, and any namespace outside
// the deletion allowlist.
//
// Single-run families sit trivially inside any keep-last window (the newest
// run IS among the last K runs), so the baseline rule tests sweep with the
// window disabled (K=0); the window itself has its dedicated tests.

import (
	"context"
	"fmt"
	"testing"
	"time"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/castle"
	logr "github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	client "sigs.k8s.io/controller-runtime/pkg/client"
)

const crRetentionWindow = 7 * 24 * time.Hour

func retentionSweeper(cl client.Client) *CRRetentionSweeper {
	return &CRRetentionSweeper{
		Client:    cl,
		Namespace: "default",
		Retention: crRetentionWindow,
		KeepLast:  DefaultCRRetentionKeepLast,
		Allowlist: map[string]struct{}{"default": {}},
		Interval:  time.Hour,
	}
}

// retentionRun is a terminal run one retention window old, fully archived:
// the deletion-candidate baseline. It carries no finalizer, so a fake-client
// delete removes it outright — the orphan verify then sees its children as
// dead-owner orphans, exactly as after a real finalize completes.
func retentionRun(name, uid, ticket string) *criteriav1.CriteriaRun {
	return &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         "default",
			UID:               types.UID(uid),
			CreationTimestamp: metav1.NewTime(time.Now().Add(-31 * 24 * time.Hour)),
		},
		Spec: criteriav1.CriteriaRunSpec{TicketID: ticket},
		Status: criteriav1.CriteriaRunStatus{
			Phase:      criteriav1.PhaseSucceeded,
			FinalState: "handler_complete",
			TerminalAt: &metav1.Time{Time: time.Now().Add(-31 * 24 * time.Hour)},
			ArchiveID:  castle.RunArchiveID(name),
		},
	}
}

func parkRun(run *criteriav1.CriteriaRun, finalState string) *criteriav1.CriteriaRun {
	run.Status.FinalState = finalState
	return run
}

func sweepOnce(t *testing.T, cl client.Client) *CRRetentionSweeper {
	t.Helper()
	s := retentionSweeper(cl)
	s.sweep(context.Background(), logr.Discard())
	return s
}

// sweepBaseline is the keep-last-disabled sweep the single-run rule tests use.
func sweepBaseline(t *testing.T, cl client.Client) {
	t.Helper()
	s := retentionSweeper(cl)
	s.KeepLast = 0
	s.sweep(context.Background(), logr.Discard())
}

func listRun(t *testing.T, cl client.Client, name string) (*criteriav1.CriteriaRun, error) {
	t.Helper()
	var run criteriav1.CriteriaRun
	err := cl.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: name}, &run)
	if err != nil {
		return nil, err
	}
	return &run, nil
}

func assertGone(t *testing.T, cl client.Client, name string) {
	t.Helper()
	_, err := listRun(t, cl, name)
	require.Error(t, err, "expected CR %s to be deleted", name)
}

func assertRetained(t *testing.T, cl client.Client, name, msg string) {
	t.Helper()
	_, err := listRun(t, cl, name)
	require.NoError(t, err, "%s", msg)
}

func TestCRRetentionDeletesAgedArchivedTerminalRun(t *testing.T) {
	run := retentionRun("cri-900", "uid-cri-900", "CRI-900")
	cl := newSweepClient(t, run)

	sweepBaseline(t, cl)

	assertGone(t, cl, "cri-900")
}

func TestCRRetentionKeepsRunInsideRetentionWindow(t *testing.T) {
	run := retentionRun("cri-900", "uid-cri-900", "CRI-900")
	stamped := metav1.NewTime(time.Now().Add(-2 * 24 * time.Hour))
	run.Status.TerminalAt = &stamped
	cl := newSweepClient(t, run)

	sweepBaseline(t, cl)

	fetched, err := listRun(t, cl, "cri-900")
	require.NoError(t, err)
	assert.Equal(t, "handler_complete", fetched.Status.FinalState)
	assert.Equal(t, stamped.Time.Unix(), fetched.Status.TerminalAt.Time.Unix(), "an in-window run's terminal clock survives the sweep untouched")
}

func TestCRRetentionNeverDeletesParkedRuns(t *testing.T) {
	for _, parked := range []string{"need_assistance", "awaiting_human"} {
		t.Run(parked, func(t *testing.T) {
			run := retentionRun("cri-900", "uid-cri-900", "CRI-900")
			parkRun(run, parked)
			cl := newSweepClient(t, run)

			// K=0 isolates the parked rule from the keep-last window.
			sweepBaseline(t, cl)

			fetched, err := listRun(t, cl, "cri-900")
			require.NoError(t, err, "a parked run is human attention pending: retention never deletes it, archived or not")
			assert.Equal(t, parked, fetched.Status.FinalState)
		})
	}
}

func TestCRRetentionStallsOnUnarchivedRun(t *testing.T) {
	t.Run("no-archive-id", func(t *testing.T) {
		run := retentionRun("cri-900", "uid-cri-900", "CRI-900")
		run.Status.ArchiveID = ""
		cl := newSweepClient(t, run)

		sweepBaseline(t, cl)

		fetched, err := listRun(t, cl, "cri-900")
		require.NoError(t, err, "deleting before the castle record lands would lose the only copy (fail-safe)")
		require.NotNil(t, fetched)
	})
	t.Run("archive-pending", func(t *testing.T) {
		run := retentionRun("cri-900", "uid-cri-900", "CRI-900")
		run.Status.ArchivePending = true
		cl := newSweepClient(t, run)

		sweepBaseline(t, cl)

		fetched, err := listRun(t, cl, "cri-900")
		require.NoError(t, err)
		assert.True(t, fetched.Status.ArchivePending)
	})
}

func TestCRRetentionStallsOnUnstampedTerminalRun(t *testing.T) {
	run := retentionRun("cri-900", "uid-cri-900", "CRI-900")
	run.Status.TerminalAt = nil
	cl := newSweepClient(t, run)

	sweepBaseline(t, cl)

	assertRetained(t, cl, "cri-900", "an unstamped terminal run has no known age: it is never a candidate")
}

func TestCRRetentionNeverDeletesNonTerminalRuns(t *testing.T) {
	for _, phase := range []criteriav1.CriteriaRunPhase{criteriav1.PhasePending, criteriav1.PhaseRunning} {
		t.Run(string(phase), func(t *testing.T) {
			run := retentionRun("cri-900", "uid-cri-900", "CRI-900")
			run.Status = criteriav1.CriteriaRunStatus{Phase: phase}
			cl := newSweepClient(t, run)

			sweepBaseline(t, cl)

			fetched, err := listRun(t, cl, "cri-900")
			require.NoError(t, err)
			assert.Equal(t, phase, fetched.Status.Phase)
		})
	}
}

func TestCRRetentionAllowlistGatesDeletion(t *testing.T) {
	run := retentionRun("cri-900", "uid-cri-900", "CRI-900")
	cl := newSweepClient(t, run)
	s := retentionSweeper(cl)
	s.KeepLast = 0

	// Default off: an empty allowlist opts no namespace in.
	s.Allowlist = map[string]struct{}{}
	s.sweep(context.Background(), logr.Discard())
	assertRetained(t, cl, "cri-900", "deletion must be opt-in per namespace; an empty allowlist deletes nothing")

	// A different namespace on the allowlist does not opt this one in.
	s.Allowlist = map[string]struct{}{"criteria-jobs": {}}
	s.sweep(context.Background(), logr.Discard())
	assertRetained(t, cl, "cri-900", "only the namespaces listed on the allowlist arm deletion")

	s.Allowlist = map[string]struct{}{"default": {}}
	s.sweep(context.Background(), logr.Discard())
	assertGone(t, cl, "cri-900")
}

func TestCRRetentionKeepsNewestKeepLastPerFamily(t *testing.T) {
	newest := retentionRun("cri-900-newest", "uid-900-n", "CRI-900")
	newest.Status.TerminalAt = &metav1.Time{Time: time.Now().Add(-2 * 24 * time.Hour)}
	mid := retentionRun("cri-900-mid", "uid-900-m", "CRI-900")
	mid.Status.TerminalAt = &metav1.Time{Time: time.Now().Add(-10 * 24 * time.Hour)}
	aged := retentionRun("cri-900-aged", "uid-900-a", "CRI-900")
	aged.Status.TerminalAt = &metav1.Time{Time: time.Now().Add(-20 * 24 * time.Hour)}
	oldest := retentionRun("cri-900-oldest", "uid-900-o", "CRI-900")
	other := retentionRun("cri-901", "uid-901", "CRI-901")
	cl := newSweepClient(t, newest, mid, aged, oldest, other)

	// Default keep-last is 3: the family's three newest terminal runs
	// survive regardless of age; the aged oldest falls out.
	sweepOnce(t, cl)

	assertGone(t, cl, "cri-900-oldest")
	assertRetained(t, cl, "cri-900-newest", "keep-last protects recent siblings for failure forensics")
	assertRetained(t, cl, "cri-900-mid", "keep-last protects recent siblings for failure forensics")
	assertRetained(t, cl, "cri-900-aged", "keep-last disregards age: an aged run inside the family window survives")
	assertRetained(t, cl, "cri-901", "each family keeps its own window")
}

func TestCRRetentionParkedRunHoldsFamilyWindow(t *testing.T) {
	parked := retentionRun("cri-900-parked", "uid-900-p", "CRI-900")
	parked.Status.TerminalAt = &metav1.Time{Time: time.Now().Add(-2 * 24 * time.Hour)}
	parkRun(parked, "need_assistance")
	midArchived := retentionRun("cri-900-mid", "uid-900-m", "CRI-900")
	midArchived.Status.TerminalAt = &metav1.Time{Time: time.Now().Add(-10 * 24 * time.Hour)}
	oldest := retentionRun("cri-900-aged", "uid-900-a", "CRI-900")
	oldest.Status.TerminalAt = &metav1.Time{Time: time.Now().Add(-20 * 24 * time.Hour)}
	cl := newSweepClient(t, parked, midArchived, oldest)
	s := retentionSweeper(cl)
	s.KeepLast = 2

	s.sweep(context.Background(), logr.Discard())

	assertRetained(t, cl, "cri-900-parked", "the parked run is never deleted and still counts in the family window")
	assertRetained(t, cl, "cri-900-mid", "the archived run inside the keep-last window survives")
	assertGone(t, cl, "cri-900-aged")
}

func TestCRRetentionZeroKeepLastDisablesFamilyProtection(t *testing.T) {
	cl := newSweepClient(t,
		retentionRun("cri-900-a", "uid-900-a", "CRI-900"),
		retentionRun("cri-900-b", "uid-900-b", "CRI-900"),
	)
	s := retentionSweeper(cl)
	s.KeepLast = 0

	s.sweep(context.Background(), logr.Discard())

	assertGone(t, cl, "cri-900-a")
	assertGone(t, cl, "cri-900-b")
}

// A family at its keep-last limit keeps every member: with K=3 no member
// ages out, though all are past the retention window.
func TestCRRetentionKeepLastDisregardsAgeInsideWindow(t *testing.T) {
	cl := newSweepClient(t,
		retentionRun("cri-902-a", "uid-902-a", "CRI-902"),
		retentionRun("cri-902-b", "uid-902-b", "CRI-902"),
		retentionRun("cri-902-c", "uid-902-c", "CRI-902"),
	)

	sweepOnce(t, cl)

	for _, name := range []string{"cri-902-a", "cri-902-b", "cri-902-c"} {
		assertRetained(t, cl, name, "a family at its keep-last limit keeps every member regardless of age")
	}
}

// The family ordering ranks unstamped terminal runs oldest, so they can
// never push a stamped (ageable) run out of the protected window.
func TestCRRetentionKeepLastPrefersStampedRuns(t *testing.T) {
	unstamped := retentionRun("cri-900-unstamped", "uid-900-u", "CRI-900")
	unstamped.Status.TerminalAt = nil
	unstamped.CreationTimestamp = metav1.NewTime(time.Now()) // created most recently
	mid := retentionRun("cri-900-mid", "uid-900-m", "CRI-900")
	mid.Status.TerminalAt = &metav1.Time{Time: time.Now().Add(-30 * 24 * time.Hour)}
	oldest := retentionRun("cri-900-oldest", "uid-900-o", "CRI-900")
	oldest.Status.TerminalAt = &metav1.Time{Time: time.Now().Add(-31 * 24 * time.Hour)}
	cl := newSweepClient(t, unstamped, mid, oldest)
	s := retentionSweeper(cl)
	s.KeepLast = 2

	s.sweep(context.Background(), logr.Discard())

	assertRetained(t, cl, "cri-900-mid", "the newest stamped run holds the top window slot")
	assertRetained(t, cl, "cri-900-oldest", "the unstamped run ranked oldest, so the second stamped run holds the second slot")
	assertRetained(t, cl, "cri-900-unstamped", "an unstamped run is never a deletion candidate")
}

func TestCRRetentionReapsOrphanedOwnerlessChildren(t *testing.T) {
	// Children of a CR that no longer exists (skipped finalize / force
	// delete, dead owner UID), children of a live run, and a label-only Job
	// with no ownerReference at all.
	orphan := adapterJob("adp-cri-144-1", "default", "cri-144")
	withCriteriaRunOwnerRef(orphan, "cri-144", "uid-gone-144", true)
	live := adapterJob("adp-cri-145-1", "default", "cri-145")
	withCriteriaRunOwnerRef(live, "cri-145", "uid-cri-145", true)
	labelOnly := adapterJob("adp-cri-manual", "default", "cri-manual")
	orphanPod := adapterPod("adp-cri-144-0", "default", "cri-144")
	withCriteriaRunOwnerRef(orphanPod, "cri-144", "uid-gone-144", true)
	liveRun := retentionRun("cri-145", "uid-cri-145", "CRI-145")
	liveRun.Status = criteriav1.CriteriaRunStatus{Phase: criteriav1.PhaseRunning}
	cl := newSweepClient(t, orphan, live, labelOnly, orphanPod, liveRun)

	sweepBaseline(t, cl)

	if _, err := getSweptJob(t, cl, "default", "adp-cri-144-1"); err == nil {
		t.Fatal("orphaned child Job of a deleted CR must be removed by the orphan verify")
	}
	if _, err := getSweptPod(t, cl, "default", "adp-cri-144-0"); err == nil {
		t.Fatal("orphaned child Pod of a deleted CR must be removed by the orphan verify")
	}
	if _, err := getSweptJob(t, cl, "default", "adp-cri-145-1"); err != nil {
		t.Fatal("a live run's adapter Job must never be swept")
	}
	if _, err := getSweptJob(t, cl, "default", "adp-cri-manual"); err != nil {
		t.Fatal("a label-only Job (no ownerReference) must never be swept")
	}
	liveRunAfter, err := listRun(t, cl, "cri-145")
	require.NoError(t, err)
	assert.Equal(t, criteriav1.PhaseRunning, liveRunAfter.Status.Phase)
}

// getSweptJob mirrors getSweptPod for child Jobs.
func getSweptJob(t *testing.T, cl client.Client, namespace, name string) (*batchv1.Job, error) {
	t.Helper()
	var job batchv1.Job
	err := cl.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: name}, &job)
	if err != nil {
		return nil, err
	}
	return &job, nil
}

// The acceptance contract across passes: a deletion that cascades nothing
// (no finalizer — the CR vanishes outright) leaves its children ownerless;
// the pass list is taken before the deletes, so the children get their
// cascade grace in the deletion pass and the NEXT pass reaps whatever
// actually leaked — zero ownerless Jobs/Pods, after the dust settles.
func TestCRRetentionOrphanVerifyReapsChildrenOfVanishedRun(t *testing.T) {
	run := retentionRun("cri-900", "uid-cri-900", "CRI-900")
	job := adapterJob("adp-cri-900-1", "default", "cri-900")
	withCriteriaRunOwnerRef(job, "cri-900", "uid-cri-900", true)
	pod := adapterPod("adp-cri-900-0", "default", "cri-900")
	withCriteriaRunOwnerRef(pod, "cri-900", "uid-cri-900", true)
	cl := newSweepClient(t, run, job, pod)
	s := retentionSweeper(cl)
	s.KeepLast = 0

	s.sweep(context.Background(), logr.Discard())

	assertGone(t, cl, "cri-900")
	if _, err := getSweptJob(t, cl, "default", "adp-cri-900-1"); err != nil {
		t.Fatal("children of a CR deleted this pass get their cascade grace: not reaped in the deletion pass itself")
	}

	// Pass 2: the CR is gone from the namespace; whatever the cascade left
	// is a dead-owner orphan now.
	s.sweep(context.Background(), logr.Discard())

	if _, err := getSweptJob(t, cl, "default", "adp-cri-900-1"); err == nil {
		t.Fatal("the orphan verify must reap the child Job of a vanished CR")
	}
	if _, err := getSweptPod(t, cl, "default", "adp-cri-900-0"); err == nil {
		t.Fatal("the orphan verify must reap the child Pod of a vanished CR")
	}
}

// A run mid-delete (finalizer still on) owns its children legitimately: the
// orphan verify gives the finalize/GC cascade that pass before reaping
// anything belonging to it.
func TestCRRetentionOrphanVerifyGraceForTerminatingRuns(t *testing.T) {
	run := retentionRun("cri-900", "uid-cri-900", "CRI-900")
	run.Finalizers = []string{"criteriarun.criteria.brokenbots.dev/finalizer"}
	job := adapterJob("adp-cri-900-1", "default", "cri-900")
	withCriteriaRunOwnerRef(job, "cri-900", "uid-cri-900", true)
	cl := newSweepClient(t, run, job)

	sweepBaseline(t, cl)

	terminating, err := listRun(t, cl, "cri-900")
	require.NoError(t, err, "the finalizer holds the CR as terminating")
	require.NotNil(t, terminating.DeletionTimestamp)
	if _, err := getSweptJob(t, cl, "default", "adp-cri-900-1"); err != nil {
		t.Fatal("the children of a still-terminating CR are its own: the orphan verify must not reap them")
	}
}

func TestStampTerminalAtPrefersCastleTerminal(t *testing.T) {
	terminalAt := time.Now().Add(-3 * time.Hour).Round(0)
	update := &criteriav1.CriteriaRun{Status: criteriav1.CriteriaRunStatus{Phase: criteriav1.PhaseSucceeded}}
	obs := &castle.Observation{Terminal: &castle.Terminal{At: terminalAt}}

	stampTerminalAt(update, obs)
	require.NotNil(t, update.Status.TerminalAt)
	assert.Equal(t, terminalAt.UTC(), update.Status.TerminalAt.Time.UTC(), "the envelope's own timestamp is the truthful terminal age")

	first := update.Status.TerminalAt
	stampTerminalAt(update, nil)
	assert.Same(t, first, update.Status.TerminalAt, "the stamp is written once; re-reconciles never rewrite it")
}

func TestStampTerminalAtFallsBackToRunRecordAndClock(t *testing.T) {
	ended := time.Now().Add(-2 * time.Hour).Round(0)
	update := &criteriav1.CriteriaRun{Status: criteriav1.CriteriaRunStatus{Phase: criteriav1.PhaseFailed}}
	stampTerminalAt(update, &castle.Observation{RunRecord: &castle.RunRecord{EndedAt: ended}})
	require.NotNil(t, update.Status.TerminalAt)
	assert.Equal(t, ended.UTC(), update.Status.TerminalAt.Time.UTC())

	// No observation at all (observation error path): the reconcile clock.
	clockStarted := time.Now()
	update2 := &criteriav1.CriteriaRun{Status: criteriav1.CriteriaRunStatus{Phase: criteriav1.PhaseSucceeded}}
	stampTerminalAt(update2, nil)
	require.NotNil(t, update2.Status.TerminalAt)
	assert.WithinDuration(t, clockStarted, update2.Status.TerminalAt.Time, 5*time.Second)
}

func TestStampTerminalAtIgnoresNonTerminalAndZeroEnvelope(t *testing.T) {
	update := &criteriav1.CriteriaRun{Status: criteriav1.CriteriaRunStatus{Phase: criteriav1.PhaseRunning}}
	stampTerminalAt(update, &castle.Observation{Terminal: &castle.Terminal{At: time.Now()}})
	assert.Nil(t, update.Status.TerminalAt, "a running run has no terminal clock yet")

	update = &criteriav1.CriteriaRun{Status: criteriav1.CriteriaRunStatus{Phase: criteriav1.PhaseSucceeded}}
	stampTerminalAt(update, &castle.Observation{Terminal: &castle.Terminal{At: time.Time{}}})
	require.NotNil(t, update.Status.TerminalAt, "a zero envelope timestamp falls back, never stamps zero")
	assert.False(t, update.Status.TerminalAt.IsZero())
}

func TestCRFinalStateParked(t *testing.T) {
	for _, parked := range []string{"need_assistance", "awaiting_human", " need_assistance "} {
		assert.True(t, crFinalStateParked(parked), parked)
	}
	for _, verdict := range []string{"handler_complete", "failed", "", "Need_assistance"} {
		assert.False(t, crFinalStateParked(verdict), verdict)
	}
}

func TestNewCRRetentionSweeperTrimsAllowlist(t *testing.T) {
	s := NewCRRetentionSweeper(nil, "default", crRetentionWindow, -2, []string{" criteria-jobs ", "", "dev"}, time.Hour)
	assert.Equal(t, 0, s.KeepLast, "a negative keep-last clamps to zero (window disabled), not the default")
	_, ok := s.Allowlist["criteria-jobs"]
	assert.True(t, ok)
	_, ok = s.Allowlist["dev"]
	assert.True(t, ok)
	assert.Len(t, s.Allowlist, 2, "blank entries are dropped")
}

// A non-positive interval must never reach time.NewTicker (it panics): the
// flag/environment accepts --retention-interval=0, a disabled-sweep idiom
// the sibling sweeps treat as benign, so the constructor clamps it.
func TestNewCRRetentionSweeperClampsNonPositiveInterval(t *testing.T) {
	for _, interval := range []time.Duration{0, -1 * time.Second} {
		s := NewCRRetentionSweeper(nil, "default", crRetentionWindow, 3, []string{"default"}, interval)
		assert.Equal(t, time.Hour, s.Interval, "interval %s must clamp to the 1h default", interval)
	}
	s := NewCRRetentionSweeper(nil, "default", crRetentionWindow, 3, []string{"default"}, 15*time.Minute)
	assert.Equal(t, 15*time.Minute, s.Interval, "a positive interval is kept")
}

func TestCRRetentionNeverTouchesOtherNamespaces(t *testing.T) {
	// Children owned by a dead run but living in another namespace are a
	// different sweep's jurisdiction: the retention sweep is strictly
	// namespace-scoped.
	deadOwner := func(obj metav1.Object) {
		obj.SetOwnerReferences([]metav1.OwnerReference{{
			APIVersion: "criteria.brokenbots.dev/v1",
			Kind:       "CriteriaRun",
			Name:       "cri-dead",
			UID:        types.UID("uid-dead"),
		}})
	}
	otherPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "adp-other-ns",
			Namespace: "elsewhere",
		},
	}
	deadOwner(otherPod)
	otherJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "adp-other-ns-job",
			Namespace: "elsewhere",
		},
	}
	deadOwner(otherJob)
	cl := newSweepClient(t, otherPod, otherJob)

	sweepBaseline(t, cl)

	if _, err := getSweptPod(t, cl, "elsewhere", "adp-other-ns"); err != nil {
		t.Fatal("objects outside the managed namespace are never touched")
	}
	if _, err := getSweptJob(t, cl, "elsewhere", "adp-other-ns-job"); err != nil {
		t.Fatal("Jobs outside the managed namespace are never touched")
	}
}

// The acceptance's bulk clause: deleting the 100 aged archived Succeeded
// runs of criteria-jobs leaves zero ownerless Jobs/Pods. In the fake the CR
// deletions land during the first pass (no finalizer) while their children
// are still live, so the orphan verify needs the follow-up pass — the same
// two-pass shape as production, where the finalizer's finalize deletes the
// child Jobs and the next sweep reaps whatever leaked.
func TestCRRetentionBulkDeletionLeavesZeroOwnerlessChildren(t *testing.T) {
	const count = 100
	objs := make([]client.Object, 0, count*3)
	for i := 0; i < count; i++ {
		run := retentionRun(fmt.Sprintf("cri-bulk-%03d", i), fmt.Sprintf("uid-bulk-%03d", i), "CRI-BULK")
		job := adapterJob(fmt.Sprintf("cri-bulk-%03d-runner", i), "default", run.Name)
		withCriteriaRunOwnerRef(job, run.Name, string(run.UID), true)
		pod := adapterPod(fmt.Sprintf("cri-bulk-%03d-adapter", i), "default", run.Name)
		withCriteriaRunOwnerRef(pod, run.Name, string(run.UID), true)
		objs = append(objs, run, job, pod)
	}
	cl := newSweepClient(t, objs...)

	s := retentionSweeper(cl)
	s.KeepLast = 0
	s.sweep(context.Background(), logr.Discard())
	// The deletions landed; the children are still present pending the orphan sweep.
	var jobs batchv1.JobList
	require.NoError(t, cl.List(context.Background(), &jobs, client.InNamespace("default")))
	require.Len(t, jobs.Items, count)
	s.sweep(context.Background(), logr.Discard())

	// Zero ownerless children after the follow-up pass, zero runs left.
	require.NoError(t, cl.List(context.Background(), &jobs, client.InNamespace("default")))
	assert.Empty(t, jobs.Items, "no orphaned Jobs may remain after the bulk deletion")
	var pods corev1.PodList
	require.NoError(t, cl.List(context.Background(), &pods, client.InNamespace("default")))
	assert.Empty(t, pods.Items, "no orphaned Pods may remain after the bulk deletion")
	for i := 0; i < count; i++ {
		assertGone(t, cl, fmt.Sprintf("cri-bulk-%03d", i))
	}
}

// The verify's delete decision re-reads the owning run from the API at
// delete time: a run admitted after the pass's snapshot (absent from the
// set handed to the verify) is proven live by its Get, so its children are
// never treated as orphaned while it is in flight. Only a genuinely
// unrecoverable owner — no same-name run and no snapshot trace — is reaped.
func TestOrphanVerifyKeepsOwnersTheSnapshotMisses(t *testing.T) {
	run := newSweepTestRun("cri-midpass", "uid-midpass")
	runJob := adapterJob("cri-midpass-runner", "default", run.Name)
	withCriteriaRunOwnerRef(runJob, "cri-midpass", string(run.UID), true)
	runPod := adapterPod("cri-midpass-adapter", "default", run.Name)
	withCriteriaRunOwnerRef(runPod, "cri-midpass", string(run.UID), true)
	orphan := adapterJob("cri-orphaned-runner", "default", "cri-orphan")
	withCriteriaRunOwnerRef(orphan, "cri-orphan", "uid-orphan", true)
	cl := newSweepClient(t, run, runJob, runPod, orphan)

	s := retentionSweeper(cl)
	stats := &crRetentionStats{}
	s.sweepOrphans(context.Background(), logr.Discard(), map[types.UID]struct{}{}, stats)

	if _, err := getSweptJob(t, cl, "default", "cri-midpass-runner"); err != nil {
		t.Fatal("a live run's child Job must survive the verify even when the owner is missing from the pass snapshot")
	}
	if _, err := getSweptPod(t, cl, "default", "cri-midpass-adapter"); err != nil {
		t.Fatal("a live run's child Pod must survive the verify even when the owner is missing from the pass snapshot")
	}
	if _, err := getSweptJob(t, cl, "default", "cri-orphaned-runner"); err == nil {
		t.Fatal("a child whose owning run does not exist at all must still be reaped")
	}
	assert.Equal(t, 1, stats.orphanJobs, "only the unrecoverable owner's Job is reaped")
	assert.Equal(t, 0, stats.orphanPods, "the live run's Pod is not reaped")
}

// A same-name run with a DIFFERENT uid is a recycled name, not the owner:
// its existence must not keep the dead owner's children alive.
func TestOrphanVerifyRejectsRecycledNameOwners(t *testing.T) {
	recycled := retentionRun("cri-144", "uid-new-144", "CRI-144")
	orphan := adapterJob("adp-cri-144-1", "default", "cri-144")
	withCriteriaRunOwnerRef(orphan, "cri-144", "uid-gone-144", true)
	cl := newSweepClient(t, recycled, orphan)

	s := retentionSweeper(cl)
	stats := &crRetentionStats{}
	s.sweepOrphans(context.Background(), logr.Discard(), map[types.UID]struct{}{}, stats)

	if _, err := getSweptJob(t, cl, "default", "adp-cri-144-1"); err == nil {
		t.Fatal("a recycled name under a dead UID must not adopt the dead owner's children")
	}
	assert.Equal(t, 1, stats.orphanJobs)
}
