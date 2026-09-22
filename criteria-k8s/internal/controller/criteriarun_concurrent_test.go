package controller_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/castle"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/controller"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/events"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/jobbuilder"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// CRI-291 regression fixtures: two non-terminal CriteriaRuns for different
// tickets sharing one repo queue, exercised the way the manager drives them
// — every top-level reconcile stamped with its own request identity on the
// context logger.

// lineCapture collects the raw JSON lines a funcr logger emits.
type lineCapture struct {
	mu    sync.Mutex
	lines []string
}

func (c *lineCapture) add(line string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, line)
}

func (c *lineCapture) decoded() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []map[string]any
	for _, line := range c.lines {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			continue
		}
		out = append(out, obj)
	}
	return out
}

// writeLines returns the captured "updating CriteriaRun status" lines.
func (c *lineCapture) writeLines() []map[string]any {
	var out []map[string]any
	for _, obj := range c.decoded() {
		if msg, _ := obj["msg"].(string); msg == "updating CriteriaRun status" {
			out = append(out, obj)
		}
	}
	return out
}

// stampedContext mirrors controller-runtime's default LogConstructor: the
// manager stamps the reconcile request onto the context logger as the kind
// name ("CriteriaRun") before Reconcile is invoked, and the reconciler adds
// its own "criteriarun" field from the same request. For top-level passes
// the two always agree; a nested synchronous reconcile inside Reconcile is
// the only way they can diverge — the CRI-291 cross-contamination.
func stampedContext(ctx context.Context, capture *lineCapture, req ctrl.Request) context.Context {
	base := funcr.NewJSON(capture.add, funcr.Options{})
	return logf.IntoContext(ctx, base.WithValues("CriteriaRun", klog.KRef(req.Namespace, req.Name)))
}

// logRefName extracts the run name from a log identity field. The JSON
// renderings (funcr here, zapr in production) serialize the request stamp as
// an object {"name":...,"namespace":...}; text logr formats render it as
// "namespace/name". Both are accepted so the assertion mirrors production.
func logRefName(v any, namespace string) (string, bool) {
	switch ref := v.(type) {
	case map[string]any:
		name, _ := ref["name"].(string)
		return name, name != ""
	case string:
		name := strings.TrimPrefix(ref, namespace+"/")
		return name, name != "" && ref != ""
	}
	return "", false
}

// assertWriteLinesOwnIdentity asserts the identity invariant on every
// captured status-write line: the manager-stamped "CriteriaRun" name, the
// reconciler's "criteriarun" request and the written jobName must all
// belong to the same run (queued runs legitimately carry no jobName yet).
func assertWriteLinesOwnIdentity(t *testing.T, lines []map[string]any, namespace string) {
	t.Helper()
	require.NotEmpty(t, lines, "expected captured status-write log lines")
	for i, line := range lines {
		stamp, ok := line["CriteriaRun"].(map[string]any)
		require.True(t, ok, "line %d carries no CriteriaRun identity: %v", i, line)
		stampName, _ := stamp["name"].(string)
		criteriarunName, ok := logRefName(line["criteriarun"], namespace)
		require.True(t, ok, "line %d carries no criteriarun request identity: %v", i, line)
		jobName, _ := line["jobName"].(string)

		assert.Equal(t, stampName, criteriarunName,
			"line %d: the manager-stamped CriteriaRun and the reconciler's criteriarun must name the same run (CRI-291): %v", i, line)
		assert.True(t, jobName == "" || jobName == stampName,
			"line %d: the written jobName must belong to the run being written (CRI-291): %v", i, line)
	}
}

// writeLinesFor filters the captured status-write lines to one run.
func writeLinesFor(lines []map[string]any, namespace, run string) []map[string]any {
	var out []map[string]any
	for _, line := range lines {
		if name, _ := logRefName(line["criteriarun"], namespace); name == run {
			out = append(out, line)
		}
	}
	return out
}

// assertAllLinesCarryOwnIdentity asserts the strongest CRI-291 invariant over
// every captured line: wherever a line carries both the manager-stamped
// "CriteriaRun" identity and the reconciler's "criteriarun" request, the two
// must name the same run — no reconcile pass may log under another run's
// stamped identity.
func assertAllLinesCarryOwnIdentity(t *testing.T, lines []map[string]any, namespace string) {
	t.Helper()
	for i, line := range lines {
		stamp, ok := line["CriteriaRun"].(map[string]any)
		if !ok {
			continue
		}
		stampName, _ := stamp["name"].(string)
		if criteriarunName, ok := logRefName(line["criteriarun"], namespace); ok {
			assert.Equal(t, stampName, criteriarunName,
				"line %d (msg %q) was logged under another run's stamped identity (CRI-291): %v", i, line["msg"], line)
		}
	}
}

// statusWrite records one CriteriaRun status write: the object written and
// the identity fields it carried.
type statusWrite struct {
	run     string
	phase   string
	jobName string
}

// statusWriteRecorder wraps a client and records every CriteriaRun status
// write with the identity it carried.
type statusWriteRecorder struct {
	client.Client
	mu     sync.Mutex
	writes []statusWrite
}

func (c *statusWriteRecorder) Status() client.SubResourceWriter {
	return &recordingStatusWriter{SubResourceWriter: c.Client.Status(), rec: c}
}

func (c *statusWriteRecorder) record(s statusWrite) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writes = append(c.writes, s)
}

func (c *statusWriteRecorder) writesFor(run string) []statusWrite {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []statusWrite
	for _, w := range c.writes {
		if w.run == run {
			out = append(out, w)
		}
	}
	return out
}

type recordingStatusWriter struct {
	client.SubResourceWriter
	rec *statusWriteRecorder
}

func (w *recordingStatusWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	if cr, ok := obj.(*criteriav1.CriteriaRun); ok {
		w.rec.record(statusWrite{run: cr.Name, phase: string(cr.Status.Phase), jobName: cr.Status.JobName})
	}
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

// castleCall records one Observe call at the castle seam.
type castleCall struct {
	runnerJob string
	knownID   string
}

// identityCastle answers Observe with a per-runner-job scripted sequence of
// observations and records every call, pinning the CRI-291 fix direction:
// a run may only ever be observed under its own runner job name and its own
// castle run id.
type identityCastle struct {
	script map[string][]*castle.Observation
	calls  []castleCall
}

func (c *identityCastle) Observe(ctx context.Context, runnerJob, knownRunID string) (*castle.Observation, error) {
	c.calls = append(c.calls, castleCall{runnerJob: runnerJob, knownID: knownRunID})
	script := c.script[runnerJob]
	if len(script) == 0 {
		return &castle.Observation{}, nil
	}
	obs := script[0]
	c.script[runnerJob] = script[1:]
	if obs == nil {
		return &castle.Observation{}, nil
	}
	return obs, nil
}

func (c *identityCastle) Disabled() bool { return false }

func (c *identityCastle) callsForRunnerJob(runnerJob string) []castleCall {
	var out []castleCall
	for _, call := range c.calls {
		if call.runnerJob == runnerJob {
			out = append(out, call)
		}
	}
	return out
}

// concurrentFixture wires two per-scope CriteriaRuns for different tickets
// on the same repo: run A admits first, run B queues behind it. The castle
// stub scripts per-run observations and records every Observe call; the
// client records every status write; the logger captures every line the
// reconciler emits under the manager-style request stamp.
type concurrentFixture struct {
	r       *controller.CriteriaRunReconciler
	cl      *statusWriteRecorder
	castle  *identityCastle
	capture *lineCapture
	runA    *criteriav1.CriteriaRun
	runB    *criteriav1.CriteriaRun
}

func concurrentRunsFixture(t *testing.T) *concurrentFixture {
	t.Helper()
	scheme := newScheme(t)
	repo := "https://github.com/brokenbots/workflow-example.git"

	runA := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{Name: "cri-291-a", Namespace: "default", UID: types.UID("uid-a")},
		Spec: criteriav1.CriteriaRunSpec{
			TicketID:         "CRI-291-A",
			RepoURL:          repo,
			Image:            "localhost:5000/linear-intake-remote:dev",
			MaxAgentVisits:   2,
			ProviderBaseURL:  "http://provider/v1",
			PerScopeSessions: true,
		},
	}
	runB := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{Name: "cri-291-b", Namespace: "default", UID: types.UID("uid-b")},
		Spec: criteriav1.CriteriaRunSpec{
			TicketID:         "CRI-291-B",
			RepoURL:          repo,
			Image:            "localhost:5000/linear-intake-remote:dev",
			MaxAgentVisits:   2,
			ProviderBaseURL:  "http://provider/v1",
			PerScopeSessions: true,
		},
	}

	provisionA := events.LifecycleEvent{
		Event: events.EventProvisionWanted, RunID: "castle-run-a", ScopeID: "root",
		AdapterName: "shell", AdapterType: "shell", ShimAddress: "10.0.0.1:7778",
		TokenFile: "/data/cri-291-a/tokens/root-shell", Digest: "sha256:aaa",
	}
	provisionB := events.LifecycleEvent{
		Event: events.EventProvisionWanted, RunID: "castle-run-b", ScopeID: "root",
		AdapterName: "shell", AdapterType: "shell", ShimAddress: "10.0.0.2:7778",
		TokenFile: "/data/cri-291-b/tokens/root-shell", Digest: "sha256:bbb",
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(runA, runB).
		WithObjects(runA, runB).
		Build()
	cl := &statusWriteRecorder{Client: fakeClient}

	castleStub := &identityCastle{script: map[string][]*castle.Observation{
		"cri-291-a": {
			{RunID: "castle-run-a", Lifecycle: []events.LifecycleEvent{provisionA}},
			{RunID: "castle-run-a", Lifecycle: []events.LifecycleEvent{provisionA}},
			{RunID: "castle-run-a", Terminal: &castle.Terminal{Success: true}},
		},
		"cri-291-b": {
			{RunID: "castle-run-b", Lifecycle: []events.LifecycleEvent{provisionB}},
			{RunID: "castle-run-b", Terminal: &castle.Terminal{Success: true}},
		},
	}}

	capture := &lineCapture{}
	r := &controller.CriteriaRunReconciler{
		Client:   cl,
		Scheme:   scheme,
		Castle:   castleStub,
		Defaults: jobbuilder.Defaults{DataPVC: "criteria-data"},
		Queue:    controller.NewRunQueue(),
	}

	return &concurrentFixture{r: r, cl: cl, castle: castleStub, capture: capture, runA: runA, runB: runB}
}

// reconcile drives one top-level reconcile pass with the manager-style
// request stamp on the context, exactly as controller-runtime does.
func (f *concurrentFixture) reconcile(t *testing.T, run *criteriav1.CriteriaRun) ctrl.Result {
	t.Helper()
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)}
	res, err := f.r.Reconcile(stampedContext(context.Background(), f.capture, req), req)
	require.NoError(t, err)
	return res
}

// assertCastleCallsOwnIdentity pins the castle seam: every Observe call must
// key on the reconciled run's own runner job name, and the persisted castle
// run id it carries may only be that run's own (empty before the first
// stamp).
func (f *concurrentFixture) assertCastleCallsOwnIdentity(t *testing.T) {
	t.Helper()
	for _, tc := range []struct {
		run   *criteriav1.CriteriaRun
		runID string
	}{
		{f.runA, "castle-run-a"},
		{f.runB, "castle-run-b"},
	} {
		runnerJob := jobbuilder.JobName(tc.run)
		calls := f.castle.callsForRunnerJob(runnerJob)
		require.NotEmpty(t, calls, "%s must have been observed under its own runner job", runnerJob)
		for i, call := range calls {
			assert.Equal(t, runnerJob, call.runnerJob,
				"castle observation %d must key on the run's own runner job name", i)
			assert.True(t, call.knownID == "" || call.knownID == tc.runID,
				"castle observation %d of %s carried a foreign castle run id %q", i, runnerJob, call.knownID)
		}
	}
	assert.NotEmpty(t, f.castle.calls)
	for _, call := range f.castle.calls {
		assert.Contains(t, []string{jobbuilder.JobName(f.runA), jobbuilder.JobName(f.runB)},
			call.runnerJob, "no run may be observed under a foreign runner job name")
	}
}

// assertStatusWritesOwnIdentity pins the client seam: every status write for
// a run must carry only that run's identity (queued runs legitimately carry
// no jobName yet).
func (f *concurrentFixture) assertStatusWritesOwnIdentity(t *testing.T) {
	t.Helper()
	for _, write := range f.cl.writes {
		assert.True(t, write.jobName == "" || write.jobName == write.run,
			"status write for %s carried a foreign jobName %q (CRI-291)", write.run, write.jobName)
	}
}

// TestConcurrentRunsStatusWritesCarryOwnIdentityOnly is the CRI-291
// regression for the log-signature reproduction: with run A live and run B
// queued on the same repo, B's queued pass must not act on A's behalf —
// before the fix, the nested synchronous reconcile inside B's pass wrote A's
// status under B's manager-stamped identity, so the log line carried
// CriteriaRun=cri-291-b but phase/jobName of A (the ticket's byte-for-byte
// signature). Every status write for a run must carry only that run's
// identity, in the log line and in the object written.
func TestConcurrentRunsStatusWritesCarryOwnIdentityOnly(t *testing.T) {
	f := concurrentRunsFixture(t)

	// A is admitted and actuated: runner job created, castle observed under
	// A's own runner job, status written under A's own identity.
	f.reconcile(t, f.runA)
	var jobA batchv1.Job
	require.NoError(t, f.cl.Get(context.Background(), client.ObjectKey{Name: "cri-291-a", Namespace: "default"}, &jobA))
	jobA.Status.Active = 1
	require.NoError(t, f.cl.Status().Update(context.Background(), &jobA))

	res := f.reconcile(t, f.runA)
	assert.Equal(t, ctrl.Result{RequeueAfter: 10 * time.Second}, res)

	var running criteriav1.CriteriaRun
	require.NoError(t, f.cl.Get(context.Background(), client.ObjectKeyFromObject(f.runA), &running))
	assert.Equal(t, criteriav1.PhaseRunning, running.Status.Phase)
	assert.Equal(t, "cri-291-a", running.Status.JobName)
	assert.Equal(t, "castle-run-a", running.Status.CastleRunID)

	// B's queued pass: the contamination window. It may only write B's own
	// pending status; it must not touch A's status or identity.
	writesForABefore := len(f.cl.writesFor("cri-291-a"))
	res = f.reconcile(t, f.runB)
	assert.Equal(t, ctrl.Result{RequeueAfter: 5 * time.Second}, res,
		"the queued run requeues itself; it must not reconcile the running run")

	assert.Len(t, f.cl.writesFor("cri-291-a"), writesForABefore,
		"B's pass must not write A's status (CRI-291)")

	var queued criteriav1.CriteriaRun
	require.NoError(t, f.cl.Get(context.Background(), client.ObjectKeyFromObject(f.runB), &queued))
	assert.Equal(t, criteriav1.PhasePending, queued.Status.Phase)
	assert.Empty(t, queued.Status.JobName, "a queued run has no job identity of its own")

	// The queued run must not provision anything, and no job or pod may
	// carry B's identity yet.
	var jobs batchv1.JobList
	require.NoError(t, f.cl.List(context.Background(), &jobs, client.InNamespace("default")))
	for _, job := range jobs.Items {
		assert.NotEqual(t, "cri-291-b", job.Labels[jobbuilder.LabelRun],
			"the queued run must not create jobs")
	}
	var pods corev1.PodList
	require.NoError(t, f.cl.List(context.Background(), &pods, client.InNamespace("default")))
	for _, pod := range pods.Items {
		assert.NotEqual(t, "cri-291-b", pod.Labels[jobbuilder.LabelRun],
			"the queued run must not provision adapter pods")
	}

	// The regression invariant: every captured status-write line carries one
	// run's identity only, and no line is logged under another run's stamp.
	assertWriteLinesOwnIdentity(t, f.capture.writeLines(), "default")
	assertAllLinesCarryOwnIdentity(t, f.capture.decoded(), "default")

	linesA := writeLinesFor(f.capture.writeLines(), "default", "cri-291-a")
	assert.NotEmpty(t, linesA, "A's status writes are captured")

	// B's queued pass writes B's own pending status under B's identity (the
	// queued branch logs "queuing CriteriaRun", not the actuated write line).
	writesB := f.cl.writesFor("cri-291-b")
	require.Len(t, writesB, 1, "B's pass writes only B's own status")
	assert.Equal(t, string(criteriav1.PhasePending), writesB[0].phase)
	assert.Empty(t, writesB[0].jobName)
}

// TestConcurrentRunsProvisionOwnJobsAndReachTerminalIndependently drives the
// full concurrent lifecycle the ticket documents as broken: A runs while B
// queues; A goes terminal and releases the repo slot; B is actuated by its
// own reconcile pass, provisions its own runner job and adapter pod (never
// A's identity), and reaches its terminal normally — the death-spiral the
// ticket observed for the second run must not happen.
func TestConcurrentRunsProvisionOwnJobsAndReachTerminalIndependently(t *testing.T) {
	f := concurrentRunsFixture(t)

	// A runs: admitted, actuated, one per-scope adapter pod provisioned from
	// its own castle lifecycle, all under A's identity.
	f.reconcile(t, f.runA)

	var jobA batchv1.Job
	require.NoError(t, f.cl.Get(context.Background(), client.ObjectKey{Name: "cri-291-a", Namespace: "default"}, &jobA))
	jobA.Status.Active = 1
	require.NoError(t, f.cl.Status().Update(context.Background(), &jobA))
	f.reconcile(t, f.runA)

	var runningA criteriav1.CriteriaRun
	require.NoError(t, f.cl.Get(context.Background(), client.ObjectKeyFromObject(f.runA), &runningA))
	assert.Equal(t, criteriav1.PhaseRunning, runningA.Status.Phase)
	assert.Equal(t, "cri-291-a", runningA.Status.JobName)
	assert.Equal(t, "castle-run-a", runningA.Status.CastleRunID)
	assert.NotEmpty(t, adapterPodsFor(t, f.cl, "cri-291-a"),
		"A's per-scope adapter pod is provisioned from A's own castle lifecycle")

	// B queues behind A: no jobs, no pods, own pending status only.
	f.reconcile(t, f.runB)
	assert.Empty(t, jobNamesForRun(t, f.cl, "cri-291-b"), "the queued run has no jobs while A runs")
	assert.Empty(t, adapterPodsFor(t, f.cl, "cri-291-b"))

	// A finishes: its terminal pass stamps the castle terminal and releases
	// the repo slot, admitting B in queue state.
	var completeA batchv1.Job
	require.NoError(t, f.cl.Get(context.Background(), client.ObjectKey{Name: "cri-291-a", Namespace: "default"}, &completeA))
	completeA.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	require.NoError(t, f.cl.Status().Update(context.Background(), &completeA))
	f.reconcile(t, f.runA)

	var settledA criteriav1.CriteriaRun
	require.NoError(t, f.cl.Get(context.Background(), client.ObjectKeyFromObject(f.runA), &settledA))
	assert.Equal(t, criteriav1.PhaseSucceeded, settledA.Status.Phase)
	assert.True(t, settledA.Status.CastleTerminalObserved)

	// A's terminal-resync pass must not act on B's behalf either: it only
	// reaps A's stale per-scope adapters and re-derives A's own outcome.
	f.reconcile(t, f.runA)
	assert.Empty(t, adapterPodsFor(t, f.cl, "cri-291-a"),
		"the terminal pass reaps A's stale per-scope adapters")
	assert.Empty(t, jobNamesForRun(t, f.cl, "cri-291-b"),
		"A's terminal pass admits B in queue state without actuating it")

	// B's own pass: admitted, actuated, provisions its own runner job and
	// its own per-scope adapter pod with its own castle identity.
	f.reconcile(t, f.runB)
	assert.Equal(t, []string{"cri-291-b"}, jobNamesForRun(t, f.cl, "cri-291-b"),
		"B provisions its own runner job")

	var actuatedB criteriav1.CriteriaRun
	require.NoError(t, f.cl.Get(context.Background(), client.ObjectKeyFromObject(f.runB), &actuatedB))
	assert.Equal(t, "cri-291-b", actuatedB.Status.JobName, "B's runner identity is its own")
	assert.Equal(t, "castle-run-b", actuatedB.Status.CastleRunID)
	assert.NotEmpty(t, adapterPodsFor(t, f.cl, "cri-291-b"),
		"B's per-scope adapter pod is provisioned from B's own castle lifecycle")

	// A's identity is untouched by B's pass.
	var untouchedA criteriav1.CriteriaRun
	require.NoError(t, f.cl.Get(context.Background(), client.ObjectKeyFromObject(f.runA), &untouchedA))
	assert.Equal(t, "cri-291-a", untouchedA.Status.JobName)
	assert.Equal(t, "castle-run-a", untouchedA.Status.CastleRunID)

	// B finishes normally.
	var completeB batchv1.Job
	require.NoError(t, f.cl.Get(context.Background(), client.ObjectKey{Name: "cri-291-b", Namespace: "default"}, &completeB))
	completeB.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	require.NoError(t, f.cl.Status().Update(context.Background(), &completeB))
	f.reconcile(t, f.runB)

	var settledB criteriav1.CriteriaRun
	require.NoError(t, f.cl.Get(context.Background(), client.ObjectKeyFromObject(f.runB), &settledB))
	assert.Equal(t, criteriav1.PhaseSucceeded, settledB.Status.Phase,
		"the second run reaches terminal normally (no death spiral)")
	assert.Equal(t, "cri-291-b", settledB.Status.JobName)
	assert.Equal(t, "castle-run-b", settledB.Status.CastleRunID)
	assert.True(t, settledB.Status.CastleTerminalObserved)
	assert.Empty(t, adapterPodsFor(t, f.cl, "cri-291-b"),
		"B's per-scope adapters are reaped at its terminal")

	// Nothing left non-terminal: a further pass of either run writes nothing
	// new (terminal resync is identity-stable).
	writesBefore := len(f.cl.writes)
	f.reconcile(t, f.runA)
	f.reconcile(t, f.runB)
	assert.Len(t, f.cl.writes, writesBefore,
		"terminal resync passes must not churn status")

	// The castle seam: every observation keyed on the observed run's own
	// identity, and every status write carrying only its own identity.
	f.assertCastleCallsOwnIdentity(t)
	f.assertStatusWritesOwnIdentity(t)
	assertWriteLinesOwnIdentity(t, f.capture.writeLines(), "default")
}

// adapterPodsFor lists adapter pods labeled for runName.
func adapterPodsFor(t *testing.T, cl client.Client, runName string) []string {
	t.Helper()
	var pods corev1.PodList
	require.NoError(t, cl.List(context.Background(), &pods, client.InNamespace("default"),
		client.MatchingLabels(map[string]string{jobbuilder.LabelRun: runName, jobbuilder.LabelRole: jobbuilder.RoleAdapter})))
	var names []string
	for _, p := range pods.Items {
		names = append(names, p.Name)
	}
	return names
}
