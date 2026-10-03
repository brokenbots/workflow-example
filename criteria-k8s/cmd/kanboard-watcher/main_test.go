package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brokenbots/workflow-example/criteria-k8s/internal/kanboard"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
)

// logRecorder collects log entries so tests can assert watcher behavior.
type logEntry struct {
	level int // 0 = V(0) Info, 1 = V(1); -1 for Error
	msg   string
}

type logRecorder struct {
	mu      sync.Mutex
	entries []logEntry
}

func (r *logRecorder) Init(logr.RuntimeInfo) {}
func (r *logRecorder) Enabled(int) bool      { return true }
func (r *logRecorder) Info(level int, msg string, kv ...interface{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, logEntry{level: level, msg: msg})
}
func (r *logRecorder) Error(err error, msg string, kv ...interface{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, logEntry{level: -1, msg: msg + " (" + err.Error() + ")"})
}
func (r *logRecorder) WithName(string) logr.LogSink           { return r }
func (r *logRecorder) WithValues(...interface{}) logr.LogSink { return r }

func (r *logRecorder) contains(substr string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.entries {
		if strings.Contains(e.msg, substr) {
			return true
		}
	}
	return false
}

// kbServer fakes the Kanboard JSON-RPC surface the watcher uses.
type kbServer struct {
	mu      sync.Mutex
	tasks   map[int]map[string]interface{}
	tags    map[int][]string
	columns map[int][]map[string]interface{}
	project map[string]interface{}
	created []string // CriteriaRun ticket ids (asserted via k8s client instead)
	// block, when non-nil, hangs the getAllTasks response until it is
	// closed — the KB-22 reproduction hook for a poll-blocking upstream.
	block chan struct{}
	ts    *httptest.Server
}

func newKbServer(t *testing.T) *kbServer {
	t.Helper()
	s := &kbServer{
		tasks:   map[int]map[string]interface{}{},
		tags:    map[int][]string{},
		columns: map[int][]map[string]interface{}{},
	}
	s.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			JSONRPC string          `json:"jsonrpc"`
			Method  string          `json:"method"`
			ID      int             `json:"id"`
			Params  json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decoding request: %v", err)
		}
		user, pass, ok := r.BasicAuth()
		if !ok || user != "jsonrpc" || pass == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var params map[string]interface{}
		if len(req.Params) > 0 {
			_ = json.Unmarshal(req.Params, &params)
			for k, v := range params {
				if f, ok := v.(float64); ok {
					params[k] = int(f)
				}
			}
		}
		intParam := func(name string) int { v, _ := params[name].(int); return v }
		resp := map[string]interface{}{}
		switch req.Method {
		case "getVersion":
			resp["result"] = "1.2.54"
		case "getAllProjects":
			if s.block != nil {
				<-s.block
			}
			resp["result"] = []interface{}{s.project}
		case "getProjectById":
			resp["result"] = s.project
		case "getColumns":
			resp["result"] = s.columns[intParam("project_id")]
		case "getAllTasks":
			if s.block != nil {
				<-s.block
			}
			pid := intParam("project_id")
			tasks := []interface{}{}
			for _, task := range s.tasks {
				if task["project_id"].(int) == pid {
					tasks = append(tasks, task)
				}
			}
			resp["result"] = tasks
		case "getTask":
			resp["result"] = s.tasks[intParam("task_id")]
		case "getTaskTags":
			// Real Kanboard shape: task_id-only params, map result.
			tid := intParam("task_id")
			tagMap := map[string]interface{}{}
			for i, name := range s.tags[tid] {
				tagMap[fmt.Sprintf("%d", 1000+tid*10+i)] = name
			}
			resp["result"] = tagMap
		case "moveTaskPosition":
			tid := intParam("task_id")
			if task, ok := s.tasks[tid]; ok {
				task["column_id"] = intParam("column_id")
				task["position"] = intParam("position")
			}
			resp["result"] = true
		case "createComment":
			resp["result"] = true
		default:
			t.Errorf("unexpected method: %s", req.Method)
			resp["error"] = map[string]interface{}{"code": -32601, "message": "method not found"}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(s.ts.Close)
	return s
}

// addTask stages a task with the given column and tags.
func (s *kbServer) addTask(id int, columnID int, tags ...string) {
	s.tasks[id] = map[string]interface{}{
		"id": id, "title": "Test task", "description": "Repo: https://github.com/brokenbots/workflow-example",
		"project_id": 1, "column_id": columnID,
	}
	s.tags[id] = tags
}

// setTaskColumn changes a task's column (simulating watcher/workflow moves).
func (s *kbServer) setTaskColumn(id, columnID int) { s.tasks[id]["column_id"] = columnID }

// taskColumn returns the task's current column id.
func (s *kbServer) taskColumn(id int) int { return s.tasks[id]["column_id"].(int) }

const testRoutesJSON = `{
  "apiVersion": "criteria.brokenbots.dev/v1",
  "kind": "Routes",
  "workflowLibrary": {
    "kanboard-intake": {
      "type": "image",
      "image": "localhost:5000/linear-intake-remote:dev",
      "namespace": "criteria-jobs"
    }
  },
  "routes": [
    {"name": "kb-triage", "workflow": "kanboard-intake", "project": "Kanboard Tickets", "states": ["Backlog"]},
    {"name": "kb-develop", "workflow": "kanboard-intake", "project": "Kanboard Tickets", "states": ["Review"]}
  ]
}`

// chainRoutesJSON mirrors the live criteria-routes split for Kanboard
// (k8s/examples/routes-configmap.yaml): the triage route fires a
// triage-class workflow from Backlog and the develop route (default dev
// class) fires from Ready — the triage -> develop chain whose handoff the
// KB-9 reconciliation bug swallowed.
const chainRoutesJSON = `{
  "apiVersion": "criteria.brokenbots.dev/v1",
  "kind": "Routes",
  "workflowLibrary": {
    "kanboard-triage-wf": {
      "type": "image",
      "image": "localhost:5000/kanboard-triage:dev",
      "namespace": "criteria-jobs",
      "class": "triage"
    },
    "kanboard-develop-wf": {
      "type": "image",
      "image": "localhost:5000/kanboard-develop:dev",
      "namespace": "criteria-jobs"
    }
  },
  "routes": [
    {"name": "kanboard-triage", "workflow": "kanboard-triage-wf", "project": "Kanboard Tickets", "states": ["Backlog"]},
    {"name": "kanboard-develop", "workflow": "kanboard-develop-wf", "project": "Kanboard Tickets", "states": ["Ready"]}
  ]
}`

// KB-7: the workflow declares a k8s-secret volume (a plain namespace Secret
// mounted as files) instead of a pre-populated PVC for token files.
const kbK8sSecretRoutesJSON = `{
  "apiVersion": "criteria.brokenbots.dev/v1",
  "kind": "Routes",
  "workflowLibrary": {
    "kanboard-intake": {
      "type": "image",
      "image": "localhost:5000/linear-intake-remote:dev",
      "namespace": "criteria-jobs",
      "env": {"WORKFLOW_GITHUB_TOKEN": "/home/criteria/secrets/workflow_github_token"},
      "volumes": [
        {"name": "tokens", "kind": "k8s-secret", "secretName": "github-tokens", "mountPath": "/home/criteria/secrets", "readOnly": true}
      ]
    }
  },
  "routes": [
    {"name": "kb-triage", "workflow": "kanboard-intake", "project": "Kanboard Tickets", "states": ["Backlog"]}
  ]
}`

type testWatcher struct {
	w      *watcher
	kbS    *kbServer
	client client.Client
	logs   *logRecorder
}

func newTestWatcher(t *testing.T, routesJSON string) *testWatcher {
	t.Helper()
	routesPath := filepath.Join(t.TempDir(), "routes.json")
	if err := os.WriteFile(routesPath, []byte(routesJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	if err := criteriav1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&criteriav1.CriteriaRun{}).Build()

	s := newKbServer(t)
	s.project = map[string]interface{}{"id": 1, "name": "Kanboard Tickets"}
	s.columns[1] = []map[string]interface{}{
		{"id": 5, "title": "Backlog"},
		{"id": 6, "title": "Review"},
		{"id": 7, "title": "Work in progress"},
		{"id": 8, "title": "Done"},
		{"id": 9, "title": "Ready"},
	}
	recorder := &logRecorder{}
	w := &watcher{
		client:            k8sClient,
		kb:                kanboard.New(s.ts.URL, "test-token"),
		namespace:         "criteria-jobs",
		projectName:       "Kanboard Tickets",
		triggerTag:        "k8s-run",
		pollInterval:      time.Minute,
		image:             "localhost:5000/criteria-k8s:dev",
		providerBaseURL:   "http://provider/v1",
		maxAgentVisits:    2,
		routesFile:        routesPath,
		workflowsTagGroup: "workflows",
		repoValidator:     func(string) bool { return true },
		projectID:         1,
		log:               logr.New(recorder),
	}
	return &testWatcher{w: w, kbS: s, client: k8sClient, logs: recorder}
}

func (tw *testWatcher) pollOnce(t *testing.T) {
	t.Helper()
	if err := tw.w.poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
}

func (tw *testWatcher) runs(t *testing.T) []criteriav1.CriteriaRun {
	t.Helper()
	list := &criteriav1.CriteriaRunList{}
	if err := tw.client.List(context.Background(), list, client.InNamespace("criteria-jobs")); err != nil {
		t.Fatal(err)
	}
	return list.Items
}

func (tw *testWatcher) setRunPhase(t *testing.T, ticket string, phase criteriav1.CriteriaRunPhase) {
	t.Helper()
	list := &criteriav1.CriteriaRunList{}
	if err := tw.client.List(context.Background(), list, client.InNamespace("criteria-jobs"),
		client.MatchingLabels{"ticket": strings.ToLower(ticket)}); err != nil {
		t.Fatal(err)
	}
	require.Len(t, list.Items, 1)
	run := list.Items[0]
	run.Status.Phase = phase
	if err := tw.client.Status().Update(context.Background(), &run); err != nil {
		t.Fatal(err)
	}
}

// addRun pre-creates a settled CriteriaRun for a ticket, shaped the way an
// earlier poll's run looks to a later poll: kanboard source label, ticket
// label, and the admission class stamped on its spec workflow. The name
// must be timestamp-shaped ("kb-<id>-<unix>") because runs created later
// by the watcher win the newest-run tiebreak by name when creation
// timestamps are equal (the fake client leaves them zero).
func (tw *testWatcher) addRun(t *testing.T, name, ticket, class string, phase criteriav1.CriteriaRunPhase) {
	t.Helper()
	run := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "criteria-jobs",
			Labels: map[string]string{
				"ticket":                         strings.ToLower(ticket),
				"app.kubernetes.io/managed-by":   "criteria-kanboard-watcher",
				"criteria.brokenbots.dev/source": "kanboard",
			},
		},
		Spec: criteriav1.CriteriaRunSpec{
			TicketID: ticket,
			RepoURL:  "brokenbots/workflow-example",
		},
	}
	if class != "" {
		run.Spec.Workflow = &criteriav1.RunWorkflow{Name: "kanboard-triage", Class: class}
	}
	require.NoError(t, tw.client.Create(context.Background(), run))
	run.Status.Phase = phase
	require.NoError(t, tw.client.Status().Update(context.Background(), run))
}

// runByName fetches one run by name (a ticket can carry several runs —
// the triage run handing off to the dev run — so setRunPhase's
// single-run-per-ticket assumption does not hold on the chain).
func (tw *testWatcher) runByName(t *testing.T, name string) *criteriav1.CriteriaRun {
	t.Helper()
	list := &criteriav1.CriteriaRunList{}
	if err := tw.client.List(context.Background(), list, client.InNamespace("criteria-jobs")); err != nil {
		t.Fatal(err)
	}
	for i := range list.Items {
		if list.Items[i].Name == name {
			return &list.Items[i]
		}
	}
	t.Fatalf("run %q not found", name)
	return nil
}

func (tw *testWatcher) setRunPhaseByName(t *testing.T, name string, phase criteriav1.CriteriaRunPhase) {
	t.Helper()
	run := tw.runByName(t, name)
	run.Status.Phase = phase
	require.NoError(t, tw.client.Status().Update(context.Background(), run))
}

func TestPollFiresOnTriggerTaggedBacklogTask(t *testing.T) {
	tw := newTestWatcher(t, testRoutesJSON)
	tw.kbS.addTask(10, 5, "k8s-run") // column 5 = Backlog

	tw.pollOnce(t)

	runs := tw.runs(t)
	require.Len(t, runs, 1)
	assert.Equal(t, "KB-10", runs[0].Spec.TicketID)
	assert.Equal(t, "brokenbots/workflow-example", runs[0].Spec.RepoURL)
	require.NotNil(t, runs[0].Spec.Workflow)
	assert.Equal(t, "kanboard-intake", runs[0].Spec.Workflow.Name)
	assert.Equal(t, "kanboard", runs[0].Labels["criteria.brokenbots.dev/source"])
}

func TestTriggerTagGatesFiring(t *testing.T) {
	tw := newTestWatcher(t, testRoutesJSON)
	tw.kbS.addTask(11, 5) // Backlog, no k8s-run tag

	tw.pollOnce(t)

	assert.Empty(t, tw.runs(t), "no run without the trigger tag")
}

func TestLiveRunBlocksDuplicateFiring(t *testing.T) {
	tw := newTestWatcher(t, testRoutesJSON)
	tw.kbS.addTask(12, 5, "k8s-run")

	tw.pollOnce(t)
	require.Len(t, tw.runs(t), 1)
	tw.pollOnce(t)

	assert.Len(t, tw.runs(t), 1, "a live run blocks a second run")
}

func TestColumnReconciliationOnPhaseChange(t *testing.T) {
	tw := newTestWatcher(t, testRoutesJSON)
	tw.kbS.addTask(13, 5, "k8s-run")
	tw.pollOnce(t)
	require.Len(t, tw.runs(t), 1)
	runName := tw.runs(t)[0].Name

	// Running: task moves to Work in progress.
	tw.setRunPhase(t, "KB-13", criteriav1.PhaseRunning)
	tw.pollOnce(t)
	assert.Equal(t, 7, tw.kbS.taskColumn(13), "running run moves task to work column")

	// Succeeded: the verdict is not observable yet, so the watcher abstains
	// (KB-50) — the Completed pod phase alone is not a succeeded workflow.
	tw.setRunPhase(t, "KB-13", criteriav1.PhaseSucceeded)
	tw.pollOnce(t)
	assert.Equal(t, 7, tw.kbS.taskColumn(13), "a Succeeded phase without a workflow verdict does not stamp Done")

	// handler_complete stamps Done: since the KB-49 route guard the
	// workflow itself refuses a handler success with an empty pr_url, so
	// the verdict IS the PR evidence (KB-101 removed Status.PRNumber —
	// permanently empty because no castle build ever published pr_url).
	tw.setRunFinalStateByName(t, runName, "handler_complete")
	tw.pollOnce(t)
	assert.Equal(t, 8, tw.kbS.taskColumn(13), "succeeded run with verdict moves task to Done")
}

func TestFailedRunMovesTaskToReview(t *testing.T) {
	tw := newTestWatcher(t, testRoutesJSON)
	tw.kbS.addTask(14, 5, "k8s-run")

	tw.pollOnce(t)
	require.Len(t, tw.runs(t), 1)
	tw.setRunPhase(t, "KB-14", criteriav1.PhaseFailed)
	tw.pollOnce(t)

	assert.Equal(t, 6, tw.kbS.taskColumn(14), "failed run moves task to Review")
}

// TestTriageRouteStampsTriageClass pins the fixture contract the KB-9
// regression relies on: the triage route's workflow class reaches the run
// spec, so the reconcile can tell a triage settle from a dev settle.
func TestTriageRouteStampsTriageClass(t *testing.T) {
	tw := newTestWatcher(t, chainRoutesJSON)
	tw.kbS.addTask(19, 5, "k8s-run") // Backlog

	tw.pollOnce(t)

	runs := tw.runs(t)
	require.Len(t, runs, 1)
	require.NotNil(t, runs[0].Spec.Workflow)
	assert.Equal(t, "kanboard-triage-wf", runs[0].Spec.Workflow.Name)
	assert.Equal(t, criteriav1.RunClassTriage, runs[0].Spec.Workflow.Class)
}

// KB-7: a spec-declared k8s-secret volume is stamped onto the run so the
// jobbuilder renders it as a native Secret volume (no pre-populated PVC).
func TestPollStampsK8sSecretVolume(t *testing.T) {
	tw := newTestWatcher(t, kbK8sSecretRoutesJSON)
	tw.kbS.addTask(21, 5, "k8s-run")

	tw.pollOnce(t)

	runs := tw.runs(t)
	require.Len(t, runs, 1)
	wf := runs[0].Spec.Workflow
	require.NotNil(t, wf)
	require.Len(t, wf.Volumes, 1)
	vol := wf.Volumes[0]
	assert.Equal(t, "k8s-secret", vol.Kind)
	assert.Equal(t, "github-tokens", vol.SecretName)
	assert.Equal(t, "/home/criteria/secrets", vol.MountPath)
	assert.True(t, vol.ReadOnly)
	assert.Empty(t, vol.Claim, "a k8s-secret volume never carries a claim")
}

// KB-9 regression: a triage run succeeded and the triage workflow's
// set_ready_state moved the ticket to the develop route's trigger column.
// Before the fix, the watcher's next poll reconciled Ready -> Done before
// the firing loop ran, and the develop route never fired (observed live on
// KB-2: triage kb-2-1790274734 succeeded, the watcher moved the ticket to
// Done at 18:33:13, and no develop run fired until an operator moved the
// ticket back). The next poll must fire the develop run and leave the
// ticket in Ready.
func TestTriageSucceededFiresDevelopFromReady(t *testing.T) {
	tw := newTestWatcher(t, chainRoutesJSON)
	tw.kbS.addTask(20, 9, "k8s-run", "internal-reproduced") // Ready, armed bypass ticket
	tw.addRun(t, "kb-20-0000000001", "KB-20", criteriav1.RunClassTriage, criteriav1.PhaseSucceeded)

	tw.pollOnce(t)

	assert.Equal(t, 9, tw.kbS.taskColumn(20),
		"triage-class success must not stamp Done over the develop trigger column")
	runs := tw.runs(t)
	require.Len(t, runs, 2, "develop run must fire within one poll of the ticket landing in Ready")
	var dev *criteriav1.CriteriaRun
	for i := range runs {
		if runs[i].Name != "kb-20-0000000001" {
			dev = &runs[i]
		}
	}
	require.NotNil(t, dev)
	require.NotNil(t, dev.Spec.Workflow)
	assert.Equal(t, "kanboard-develop-wf", dev.Spec.Workflow.Name, "the run fired from Ready is the develop route")
}

// Full KB-9 chain: triage succeeded and the ticket is armed in Ready ->
// the develop route fires within one poll -> the live develop run parks
// the ticket in the work column -> only the develop run's own success,
// with the verdict and PR evidence recorded, stamps Done. The second
// Succeeded (develop) must reconcile even though the triage settle already
// recorded Succeeded — the reconcile gate tracks run identity, not just
// the phase value.
func TestTriageSucceededThenDevelopSucceedsStampsDone(t *testing.T) {
	tw := newTestWatcher(t, chainRoutesJSON)
	tw.kbS.addTask(21, 9, "k8s-run", "internal-reproduced") // Ready, armed bypass ticket
	tw.addRun(t, "kb-21-0000000001", "KB-21", criteriav1.RunClassTriage, criteriav1.PhaseSucceeded)

	tw.pollOnce(t)
	require.Len(t, tw.runs(t), 2, "develop fires within one poll of Ready")
	assert.Equal(t, 9, tw.kbS.taskColumn(21), "develop fire is not pre-empted by a Done reconcile")
	var devName string
	for _, r := range tw.runs(t) {
		if r.Name != "kb-21-0000000001" {
			devName = r.Name
		}
	}
	require.NotEmpty(t, devName, "the watcher-created run is the develop run")

	// Next poll: the develop run is in flight; the ticket parks in the work column.
	tw.pollOnce(t)
	assert.Equal(t, 7, tw.kbS.taskColumn(21), "live develop run parks the ticket in the work column")
	assert.Len(t, tw.runs(t), 2, "no duplicate run while the develop run is live")

	// The develop run settles: without a verdict the watcher abstains
	// (KB-50); handler_complete stamps Done — since the KB-49 route guard
	// the workflow refuses a handler success with an empty pr_url, so the
	// verdict IS the PR evidence (KB-101 removed Status.PRNumber).
	tw.setRunPhaseByName(t, devName, criteriav1.PhaseSucceeded)
	tw.pollOnce(t)
	assert.Equal(t, 7, tw.kbS.taskColumn(21), "a Completed pod alone does not stamp Done (KB-50)")

	tw.setRunFinalStateByName(t, devName, "handler_complete")
	tw.pollOnce(t)
	assert.Equal(t, 8, tw.kbS.taskColumn(21), "a dev-class success with the verdict stamps Done")
	assert.Len(t, tw.runs(t), 2, "a settled ticket in Done fires nothing (no route on Done)")
}

// KB-50: a legacy run predating class stamping (empty spec workflow class)
// carries no verdict either — the empty-verdict abstain applies, and the
// watcher must not stamp Done off the Succeeded pod phase alone. The
// workflow's own bookkeeping owns the Done move.
func TestLegacyRunWithoutClassDoesNotStampDone(t *testing.T) {
	tw := newTestWatcher(t, chainRoutesJSON)
	tw.kbS.addTask(22, 9, "k8s-run", "internal-reproduced")
	tw.addRun(t, "kb-22-0000000001", "KB-22", "", criteriav1.PhaseSucceeded)

	tw.pollOnce(t)

	assert.Equal(t, 9, tw.kbS.taskColumn(22),
		"pre-CRI-242 runs (empty class) with no verdict recorded do not stamp Done (KB-50)")
}

// The KB-50 gate is evidence-based, not class-gated: a legacy run whose
// record carries the verified handler_complete verdict stamps Done (the
// verdict is the PR evidence since the workflow's KB-49 guard; KB-101
// removed Status.PRNumber).
func TestLegacyRunWithVerdictStampsDone(t *testing.T) {
	tw := newTestWatcher(t, chainRoutesJSON)
	tw.kbS.addTask(31, 9, "k8s-run", "internal-reproduced")
	tw.addRun(t, "kb-31-0000000001", "KB-31", "", criteriav1.PhaseSucceeded)
	tw.setRunFinalStateByName(t, "kb-31-0000000001", "handler_complete")

	tw.pollOnce(t)

	assert.Equal(t, 8, tw.kbS.taskColumn(31),
		"a legacy run with a verified verdict stamps Done")
}

// KB-50 acceptance regression: pod Completed + workflow failure + no PR.
// The develop run's runner Job reached Succeeded — pods complete when the
// workflow reaches ANY terminal, and the workflow's failure terminal exits
// 0 — the workflow had parked the ticket in Review, no PR exists, and the
// failed verdict had not been stamped when the watcher polled. The watcher
// must not reconcile the ticket to Done (observed live on KB-40, run
// kb-40-1790625661: Review -> Done at 21:03:01Z off the Completed pod
// while run_handler had logged outcome=failure, main untouched), neither
// off the empty verdict nor after the failure verdict lands — Review is
// the workflow's own parking spot.
func TestPodCompletedWorkflowFailureNoPRDoesNotReconcileDone(t *testing.T) {
	tw := newTestWatcher(t, chainRoutesJSON)
	tw.kbS.addTask(32, 6, "k8s-run", "internal-reproduced") // Review: parked by the workflow's failure bookkeeping
	tw.addRun(t, "kb-32-0000000001", "KB-32", criteriav1.RunClassDev, criteriav1.PhaseSucceeded)

	// Poll in the KB-40 window: pod Completed, verdict not observable.
	tw.pollOnce(t)
	assert.Equal(t, 6, tw.kbS.taskColumn(32),
		"the Completed pod phase alone must not reconcile Review to Done")

	// Even after the failure verdict lands, Review stays: the repair only
	// applies to a Done stamp.
	tw.setRunFinalStateByName(t, "kb-32-0000000001", "failed")
	tw.pollOnce(t)
	assert.Equal(t, 6, tw.kbS.taskColumn(32),
		"the failed verdict leaves the workflow-parked Review ticket untouched")
}

// A failed triage-class run still parks the ticket in Review for a human —
// the triage guard only changes the success path.
func TestFailedTriageRunMovesTaskToReview(t *testing.T) {
	tw := newTestWatcher(t, chainRoutesJSON)
	tw.kbS.addTask(23, 7, "k8s-run", "internal-reproduced") // Work in progress
	tw.addRun(t, "kb-23-0000000001", "KB-23", criteriav1.RunClassTriage, criteriav1.PhaseFailed)

	tw.pollOnce(t)

	assert.Equal(t, 6, tw.kbS.taskColumn(23), "failed triage run moves task to Review")
}

// setRunFinalStateByName stamps the workflow verdict (Status.FinalState,
// KB-23) on a pre-created run — the way the controller's castle observation
// leaves the run after the workflow's terminal state reached the operator.
func (tw *testWatcher) setRunFinalStateByName(t *testing.T, name, finalState string) {
	t.Helper()
	run := tw.runByName(t, name)
	run.Status.FinalState = finalState
	require.NoError(t, tw.client.Status().Update(context.Background(), run))
}

// KB-23 regression: the runner job exits 0 even when the develop workflow
// ended in its failure terminal, so the run read Succeeded while the
// workflow's verdict was failure (observed live on KB-17, run
// kb-17-1790397477: comment_handler_failed logged outcome=failure, but the
// run CR read Succeeded). A Done stamp that predates the verdict — applied
// by a poll that saw the Job-derived Succeeded phase before the verdict was
// observable — must be repaired into the Review column, not left in Done.
func TestDevSucceededWithFailedVerdictMovesTaskToReview(t *testing.T) {
	tw := newTestWatcher(t, chainRoutesJSON)
	tw.kbS.addTask(24, 8, "k8s-run", "internal-reproduced") // Done: stamped before the verdict landed
	tw.addRun(t, "kb-24-0000000001", "KB-24", criteriav1.RunClassDev, criteriav1.PhaseSucceeded)
	tw.setRunFinalStateByName(t, "kb-24-0000000001", "failed")

	tw.pollOnce(t)

	assert.Equal(t, 6, tw.kbS.taskColumn(24),
		"a Succeeded run with a failed workflow verdict repairs a Done stamp into Review, not Done")
}

// KB-50 regression over the KB-23 ordering case (review R1): the phase
// settles before the verdict is observable. The controller persists the
// Job-derived Succeeded phase with no FinalState whenever the castle
// observation is inconclusive, and poll of that state must NOT stamp Done
// off the empty verdict — that stamping is exactly how failed develop runs
// ended up in Done (KB-39/KB-40). The late verdict still reconciles: a
// Done stamp predating the verdict — here left behind by a pre-KB-50
// watcher — is repaired into Review when the awaiting_human verdict lands
// (abstaining there would park the ticket in Done forever).
func TestDevSucceededLateVerdictRepairsDoneToReview(t *testing.T) {
	tw := newTestWatcher(t, chainRoutesJSON)
	tw.kbS.addTask(27, 8, "k8s-run", "internal-reproduced") // Done: a stale stamp a pre-KB-50 watcher left behind
	tw.addRun(t, "kb-27-0000000001", "KB-27", criteriav1.RunClassDev, criteriav1.PhaseSucceeded)

	// First poll: the verdict is not stamped yet, so the watcher abstains —
	// the Completed pod phase alone must not reconcile the ticket.
	tw.pollOnce(t)
	assert.Equal(t, 8, tw.kbS.taskColumn(27),
		"the pre-verdict poll abstains: no Done move, no other move (KB-50)")

	// Second poll: the castle observation landed the awaiting_human
	// verdict; the stale Done stamp must be repaired into Review.
	tw.setRunFinalStateByName(t, "kb-27-0000000001", "awaiting_human")
	tw.pollOnce(t)
	assert.Equal(t, 6, tw.kbS.taskColumn(27),
		"the late awaiting_human verdict reconciles the ticket out of Done into Review")
}

// KB-23 mirror (review R1): a non-Done column is left untouched by a
// non-handler_complete verdict — the ticket in the develop route's trigger
// column Ready must stay put so the firing loop can re-fire, and the run
// actually firing from Ready is what proves the loop survived the repair.
func TestDevSucceededAwaitingHumanKeepsReadyForRefire(t *testing.T) {
	tw := newTestWatcher(t, chainRoutesJSON)
	tw.kbS.addTask(28, 9, "k8s-run", "internal-reproduced") // Ready: the develop route's trigger column
	tw.addRun(t, "kb-28-0000000001", "KB-28", criteriav1.RunClassDev, criteriav1.PhaseSucceeded)
	tw.setRunFinalStateByName(t, "kb-28-0000000001", "awaiting_human")

	tw.pollOnce(t)

	assert.Equal(t, 9, tw.kbS.taskColumn(28),
		"an awaiting_human verdict leaves a non-Done column untouched (re-fire path)")
	require.Len(t, tw.runs(t), 2, "the develop route still fires from Ready after the verdict is observed")
}

// An unrecognized terminal value (a future terminal this watcher predates)
// must not silently stamp Done: the watcher abstains and leaves the column
// untouched.
func TestDevSucceededUnknownVerdictLeavesColumn(t *testing.T) {
	tw := newTestWatcher(t, chainRoutesJSON)
	tw.kbS.addTask(29, 7, "k8s-run", "internal-reproduced") // Work in progress
	tw.addRun(t, "kb-29-0000000001", "KB-29", criteriav1.RunClassDev, criteriav1.PhaseSucceeded)
	tw.setRunFinalStateByName(t, "kb-29-0000000001", "cancelled")

	tw.pollOnce(t)

	assert.Equal(t, 7, tw.kbS.taskColumn(29),
		"an unrecognized terminal state leaves the column untouched instead of stamping Done")
}

// KB-23 companion: an awaiting_human verdict handed the ticket to a human
// with the workflow's own bookkeeping (set_review_state) already applied —
// the ticket sits in Review and must not be reconciled out from under that
// handoff by a Done stamp.
func TestDevSucceededWithAwaitingHumanVerdictLeavesColumn(t *testing.T) {
	tw := newTestWatcher(t, chainRoutesJSON)
	tw.kbS.addTask(25, 6, "k8s-run", "internal-reproduced") // Review: parked by set_review_state
	tw.addRun(t, "kb-25-0000000001", "KB-25", criteriav1.RunClassDev, criteriav1.PhaseSucceeded)
	tw.setRunFinalStateByName(t, "kb-25-0000000001", "awaiting_human")

	tw.pollOnce(t)

	assert.Equal(t, 6, tw.kbS.taskColumn(25),
		"an awaiting_human verdict leaves the ticket where the workflow parked it")
}

// KB-23 companion: a handler_complete verdict delivered the work and keeps
// the Done stamp — the verdict only reroutes the failure and human-handoff
// paths — and since KB-50 the Done move additionally requires the PR the
// run actually delivered to be recorded on the run.
func TestDevSucceededWithHandlerCompleteVerdictStampsDone(t *testing.T) {
	tw := newTestWatcher(t, chainRoutesJSON)
	tw.kbS.addTask(26, 7, "k8s-run", "internal-reproduced") // Work in progress
	tw.addRun(t, "kb-26-0000000001", "KB-26", criteriav1.RunClassDev, criteriav1.PhaseSucceeded)
	tw.setRunFinalStateByName(t, "kb-26-0000000001", "handler_complete")

	tw.pollOnce(t)

	assert.Equal(t, 8, tw.kbS.taskColumn(26),
		"a handler_complete verdict stamps Done (KB-49 guard makes it the PR evidence)")
}

// KB-101 companion regression: a Succeeded pod phase with NO verdict still
// abstains — the empty-verdict path (KB-50) is the remaining abstain, since
// handler_complete itself now stamps Done (the KB-49 guard makes the
// verdict the PR evidence) and Status.PRNumber is gone.
func TestDevSucceededWithoutVerdictDoesNotStampDone(t *testing.T) {
	tw := newTestWatcher(t, chainRoutesJSON)
	tw.kbS.addTask(30, 7, "k8s-run", "internal-reproduced") // Work in progress
	tw.addRun(t, "kb-30-0000000001", "KB-30", criteriav1.RunClassDev, criteriav1.PhaseSucceeded)

	tw.pollOnce(t)

	assert.Equal(t, 7, tw.kbS.taskColumn(30),
		"a Succeeded pod phase without a workflow verdict leaves the column untouched")
}

// runNewer picks a ticket's most recent run by creation time with the name
// as the deterministic tiebreak.
func TestRunNewer(t *testing.T) {
	early := metav1.NewTime(time.Now().Add(-time.Hour))
	late := metav1.NewTime(time.Now())
	older := &criteriav1.CriteriaRun{ObjectMeta: metav1.ObjectMeta{Name: "kb-1-100", CreationTimestamp: early}}
	newer := &criteriav1.CriteriaRun{ObjectMeta: metav1.ObjectMeta{Name: "kb-1-200", CreationTimestamp: late}}
	assert.True(t, runNewer(newer, older), "later creation timestamp sorts newer")
	assert.False(t, runNewer(older, newer), "earlier creation timestamp sorts older")

	sameTime := metav1.NewTime(time.Now())
	a := &criteriav1.CriteriaRun{ObjectMeta: metav1.ObjectMeta{Name: "kb-1-100", CreationTimestamp: sameTime}}
	b := &criteriav1.CriteriaRun{ObjectMeta: metav1.ObjectMeta{Name: "kb-1-200", CreationTimestamp: sameTime}}
	assert.True(t, runNewer(b, a), "equal timestamps tiebreak by name (b > a)")
	assert.False(t, runNewer(a, b), "equal timestamps tiebreak by name (a < b)")
}

func TestNoRouteFailsClosed(t *testing.T) {
	tw := newTestWatcher(t, testRoutesJSON)
	// Column 7 = Work in progress: no route declares it.
	tw.kbS.addTask(15, 7, "k8s-run")

	tw.pollOnce(t)

	assert.Empty(t, tw.runs(t), "no run for a task in a column no route declares")
}

func TestBrokenRoutesFailClosed(t *testing.T) {
	tw := newTestWatcher(t, "{not-json")
	tw.kbS.addTask(16, 5, "k8s-run")

	err := tw.w.poll(context.Background())

	require.Error(t, err, "a broken routes payload must fail the poll so the watchdog sees the outage")
	assert.Contains(t, err.Error(), "load routes payload")
	assert.Empty(t, tw.runs(t), "no runs when the routes payload cannot be loaded")
}

func TestNoRepoURLSkips(t *testing.T) {
	tw := newTestWatcher(t, testRoutesJSON)
	// Task with no github.com reference and no default.
	tw.w.defaultRepoURL = ""
	tw.kbS.tasks[17] = map[string]interface{}{
		"id": 17, "title": "No repo", "description": "nothing here", "project_id": 1, "column_id": 5,
	}
	tw.kbS.tags[17] = []string{"k8s-run"}

	tw.pollOnce(t)

	assert.Empty(t, tw.runs(t), "no run without a resolvable repo URL")
}

func TestDefaultRepoURLFallback(t *testing.T) {
	tw := newTestWatcher(t, testRoutesJSON)
	tw.w.defaultRepoURL = "brokenbots/workflow-example"
	tw.kbS.tasks[18] = map[string]interface{}{
		"id": 18, "title": "No explicit repo", "description": "x", "project_id": 1, "column_id": 5,
	}
	tw.kbS.tags[18] = []string{"k8s-run"}

	tw.pollOnce(t)

	runs := tw.runs(t)
	require.Len(t, tw.runs(t), 1)
	assert.Equal(t, "brokenbots/workflow-example", runs[0].Spec.RepoURL)
}

func TestExtractRepoURLPrefersRepoTag(t *testing.T) {
	validate := func(string) bool { return true }
	cases := []struct {
		name string
		task kanboard.Task
		def  string
		want string
	}{
		{
			name: "repo tag wins over description reference and default",
			task: kanboard.Task{
				Title:       "fix something in brokenbots/criteria",
				Description: "Repo: https://github.com/brokenbots/criteria",
				Tags:        []string{"k8s-run", "repo:brokenbots/workflow-example"},
			},
			def:  "brokenbots/default-repo",
			want: "brokenbots/workflow-example",
		},
		{
			name: "repo tag with short form",
			task: kanboard.Task{
				Title:       "no repo mentioned",
				Description: "nothing",
				Tags:        []string{"repo:brokenbots/workflow-example"},
			},
			want: "brokenbots/workflow-example",
		},
		{
			name: "no repo tag falls back to description",
			task: kanboard.Task{
				Title:       "t",
				Description: "Repo: https://github.com/brokenbots/criteria",
				Tags:        []string{"k8s-run"},
			},
			want: "brokenbots/criteria",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractRepoURL(tc.task, tc.def, validate)
			if got != tc.want {
				t.Fatalf("extractRepoURL = %q, want %q", got, tc.want)
			}
		})
	}
}

// KB-8: component/routing tags (criteria, operator) are board routing
// metadata, never workflow-override candidates. An armed Backlog task
// carrying both tags must resolve the route's default workflow and fire
// exactly one CriteriaRun — the live failure was ErrAmbiguousWorkflow on
// every poll, never firing.
func TestRoutingTagsExemptComponentTagsFromOverride(t *testing.T) {
	tw := newTestWatcher(t, chainRoutesJSON)
	tw.w.routingTags = []string{"criteria", "operator"}
	tw.kbS.addTask(30, 5, "k8s-run", "internal-reproduced",
		"repo:brokenbots/workflow-example", "criteria", "operator")

	tw.pollOnce(t)

	runs := tw.runs(t)
	require.Len(t, runs, 1, "armed task with two routing tags fires exactly one CriteriaRun")
	assert.Equal(t, "KB-30", runs[0].Spec.TicketID)
	assert.Equal(t, "brokenbots/workflow-example", runs[0].Spec.RepoURL)
	require.NotNil(t, runs[0].Spec.Workflow)
	assert.Equal(t, "kanboard-triage-wf", runs[0].Spec.Workflow.Name,
		"route default workflow, never a component tag name")
	assert.Equal(t, criteriav1.RunClassTriage, runs[0].Spec.Workflow.Class)

	// A settled run's ticket must not re-fire: the live bug re-failed the
	// task on every poll, so exactly-one must hold across polls.
	tw.setRunPhase(t, "KB-30", criteriav1.PhaseRunning)
	tw.pollOnce(t)
	assert.Len(t, tw.runs(t), 1, "live run blocks a second run across polls")
}

// One component tag alone on an armed ticket used to become a bogus
// override name and fail closed with ErrUnknownWorkflow; with the routing
// exemption it resolves the route's default workflow too.
func TestSingleRoutingTagArmFires(t *testing.T) {
	tw := newTestWatcher(t, chainRoutesJSON)
	tw.w.routingTags = []string{"criteria", "operator"}
	tw.kbS.addTask(31, 5, "k8s-run", "internal-reproduced", "criteria")

	tw.pollOnce(t)

	runs := tw.runs(t)
	require.Len(t, runs, 1)
	require.NotNil(t, runs[0].Spec.Workflow)
	assert.Equal(t, "kanboard-triage-wf", runs[0].Spec.Workflow.Name)
}

// Fail-closed semantics survive the routing-tag exemption: two tags that
// genuinely name workflows in the configured tag group are still an
// ambiguous override pair and must refuse to fire (KB-8 keeps the
// negative path).
func TestGenuineOverrideAmbiguityStillFailsClosed(t *testing.T) {
	tw := newTestWatcher(t, chainRoutesJSON)
	tw.w.routingTags = []string{"criteria", "operator"}
	tw.kbS.addTask(32, 5, "k8s-run", "kanboard-triage-wf", "kanboard-develop-wf")

	tw.pollOnce(t)

	assert.Empty(t, tw.runs(t), "two genuine workflow-override tags refuse to fire")
	assert.True(t, tw.logs.contains("route lookup failed closed"),
		"ambiguity is logged as a fail-closed routing failure")
	assert.True(t, tw.logs.contains("multiple workflow labels in the workflows label group"),
		"the ambiguity error names the conflicting tags")
}

// Without KANBOARD_ROUTING_TAGS configured the component tags keep the
// pre-fix fail-closed posture: a lone component tag is a bogus override
// name (ErrUnknownWorkflow) and must not fire. This pins the exemption to
// the configuration, not to a hardcoded list in the watcher.
func TestUnconfiguredComponentTagStillFailsClosed(t *testing.T) {
	tw := newTestWatcher(t, chainRoutesJSON)
	tw.w.routingTags = nil
	tw.kbS.addTask(33, 5, "k8s-run", "criteria")

	tw.pollOnce(t)

	assert.Empty(t, tw.runs(t), "component tag stays an override candidate without the env")
	assert.True(t, tw.logs.contains("workflow is not present in the routes workflowLibrary"),
		"the bogus override name fails closed with ErrUnknownWorkflow")
}

func TestParseTagList(t *testing.T) {
	assert.Equal(t, []string{"criteria", "operator"}, parseTagList("criteria,operator"))
	assert.Equal(t, []string{"criteria", "operator"}, parseTagList(" criteria , operator ,"))
	assert.Equal(t, []string{"criteria"}, parseTagList("criteria"))
	assert.Empty(t, parseTagList(""), "empty env disables the extra exemptions")
	assert.Empty(t, parseTagList(" , "), "whitespace-only entries are dropped")
}

// KB-22: the observed wedge was a poll() with no per-poll deadline blocking
// ~8.5h while the pod read Running 1/1. With the fake Kanboard server
// holding the getAllTasks response, a poll without the deadline would hang
// this test until the go-test timeout; pollWithDeadline must abort it at
// the configured deadline and surface the documented error signature.
func TestPollDeadlineAbortsHungPoll(t *testing.T) {
	tw := newTestWatcher(t, testRoutesJSON)
	tw.w.pollTimeout = 50 * time.Millisecond
	tw.kbS.addTask(40, 5, "k8s-run")
	tw.kbS.block = make(chan struct{})
	// release unblocks the hung getAllTasks handler; idempotent because the
	// httptest server cleanup must never wait on a still-blocked handler.
	release := sync.OnceFunc(func() { close(tw.kbS.block) })
	defer release()

	err := tw.w.pollWithDeadline(context.Background())

	require.Error(t, err, "the hung poll must be aborted by the deadline")
	assert.True(t, errors.Is(err, context.DeadlineExceeded),
		"deadline error expected, got: %v", err)
	assert.Contains(t, err.Error(), "poll did not complete within 50ms",
		"the deadline error carries the per-poll deadline signature")

	// The aborted poll counts as finished-but-unsuccessful: the process is
	// still live, but readiness has nothing to show yet.
	var zero time.Time
	tw.w.watchdog.mu.Lock()
	lastSuccess, lastFinish := tw.w.watchdog.lastPollSuccess, tw.w.watchdog.lastPollFinish
	tw.w.watchdog.mu.Unlock()
	assert.True(t, lastSuccess.Equal(zero), "an aborted poll is not a successful poll")
	assert.False(t, lastFinish.Equal(zero), "an aborted poll still counts as completed")

	ok, reason := tw.w.ready()
	assert.False(t, ok)
	assert.Contains(t, reason, "no successful poll since startup")
	ok, _ = tw.w.live()
	assert.True(t, ok, "the poll loop completed the poll, so the process is live")

	// With the hang released, the next poll succeeds and readiness flips.
	release()
	require.NoError(t, tw.w.pollWithDeadline(context.Background()))
	ok, reason = tw.w.ready()
	assert.True(t, ok, "a successful poll makes the watcher ready")
	assert.Empty(t, reason)
	ok, _ = tw.w.live()
	assert.True(t, ok)
}

// KB-22: the watchdog must render a persistent stall unready — the health
// endpoints are what the pod's readiness/liveness probes call, and a hung
// (now deadline-aborted) poll or a failing Kanboard flips them to 503.
func TestHealthEndpointsReportStall(t *testing.T) {
	tw := newTestWatcher(t, testRoutesJSON)
	srv := httptest.NewServer(tw.w.healthMux())
	t.Cleanup(srv.Close)

	get := func(path string) (int, string) {
		resp, err := srv.Client().Get(srv.URL + path)
		require.NoError(t, err)
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return resp.StatusCode, string(body)
	}

	// No poll yet: the watcher cannot claim to be watching anything.
	code, body := get("/readyz")
	assert.Equal(t, http.StatusServiceUnavailable, code)
	assert.Contains(t, body, "no successful poll since startup")
	code, _ = get("/livez")
	assert.Equal(t, http.StatusServiceUnavailable, code, "no poll has completed either")

	// A successful poll readies the watcher (pollWithDeadline is the path
	// run() feeds the watchdog through).
	tw.w.pollTimeout = 5 * time.Second
	require.NoError(t, tw.w.pollWithDeadline(context.Background()))
	code, body = get("/readyz")
	assert.Equal(t, http.StatusOK, code)
	assert.Equal(t, "ok\n", body)
	code, _ = get("/livez")
	assert.Equal(t, http.StatusOK, code)

	// A stalled poll loop (successes stopped) un-readies the watcher while
	// the loop keeps finishing polls, so the process stays live.
	threshold := tw.w.stallThreshold()
	tw.w.watchdog.mu.Lock()
	tw.w.watchdog.lastPollSuccess = time.Now().Add(-threshold - time.Minute)
	tw.w.watchdog.mu.Unlock()
	code, body = get("/readyz")
	assert.Equal(t, http.StatusServiceUnavailable, code)
	assert.Contains(t, body, "last successful poll")
	code, _ = get("/livez")
	assert.Equal(t, http.StatusOK, code)

	// A loop wedged outside the per-poll deadline stops finishing polls
	// entirely: liveness fails and the kubelet restarts the pod.
	tw.w.watchdog.mu.Lock()
	tw.w.watchdog.lastPollFinish = time.Now().Add(-threshold - time.Minute)
	tw.w.watchdog.mu.Unlock()
	code, body = get("/livez")
	assert.Equal(t, http.StatusServiceUnavailable, code)
	assert.Contains(t, body, "last completed poll")
}

// The stall threshold must exceed the worst-case gap between two poll
// completions (interval plus the whole in-flight poll), with slack, so
// slow-but-completing polls never flap the probes.
func TestStallThresholdCoversWorstCaseCycle(t *testing.T) {
	tw := newTestWatcher(t, testRoutesJSON)
	tw.w.pollInterval = time.Minute
	tw.w.pollTimeout = 5 * time.Minute
	assert.Equal(t, 11*time.Minute, tw.w.stallThreshold())
}

func TestParseDurationFallback(t *testing.T) {
	assert.Equal(t, time.Minute, parseDuration("60s", time.Minute))
	assert.Equal(t, 5*time.Minute, parseDuration("5m", time.Minute))
	assert.Equal(t, 5*time.Minute, parseDuration("garbage", 5*time.Minute), "unparseable input takes the fallback")
	assert.Equal(t, 5*time.Minute, parseDuration("0", 5*time.Minute), "non-positive input takes the fallback")
	assert.Equal(t, 5*time.Minute, parseDuration("", 5*time.Minute), "empty input takes the fallback")
}

// KB-22: run()'s startup project resolution is bounded by the same
// per-poll deadline as a poll. A hung getAllProjects (the fake server
// holds the response) must abort run() at the deadline instead of hanging
// before the loop ever starts.
func TestRunStartupDeadlineAbortsHungStartup(t *testing.T) {
	tw := newTestWatcher(t, testRoutesJSON)
	tw.w.pollTimeout = 50 * time.Millisecond
	tw.kbS.block = make(chan struct{})
	release := sync.OnceFunc(func() { close(tw.kbS.block) })
	defer release()

	err := tw.w.run(context.Background())

	require.Error(t, err, "the wedged startup resolution must be aborted")
	assert.True(t, errors.Is(err, context.DeadlineExceeded),
		"deadline error expected, got: %v", err)
	assert.Contains(t, err.Error(), "resolve kanboard project")
}

// A routes payload that fails to load must fail the poll (KB-22): a nil
// here would count as a successful poll and keep the pod Ready through a
// persistent routes outage while it does nothing.
func TestRoutesLoadFailureFailsThePoll(t *testing.T) {
	tw := newTestWatcher(t, testRoutesJSON)
	tw.w.routesFile = filepath.Join(t.TempDir(), "missing.json")
	tw.w.pollTimeout = 5 * time.Second

	err := tw.w.pollWithDeadline(context.Background())

	require.Error(t, err, "a routes outage must fail the poll")
	assert.Contains(t, err.Error(), "load routes payload")

	ok, reason := tw.w.ready()
	assert.False(t, ok, "a failed poll is not a successful poll")
	assert.Contains(t, reason, "no successful poll since startup")
	ok, _ = tw.w.live()
	assert.True(t, ok, "the poll loop completed the poll, so the process is live")
}
