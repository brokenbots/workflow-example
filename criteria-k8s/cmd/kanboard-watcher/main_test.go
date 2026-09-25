package main

import (
	"context"
	"encoding/json"
	"fmt"
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
	ts      *httptest.Server
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
			resp["result"] = []interface{}{s.project}
		case "getProjectById":
			resp["result"] = s.project
		case "getColumns":
			resp["result"] = s.columns[intParam("project_id")]
		case "getAllTasks":
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

	// Running: task moves to Work in progress.
	tw.setRunPhase(t, "KB-13", criteriav1.PhaseRunning)
	tw.pollOnce(t)
	assert.Equal(t, 7, tw.kbS.taskColumn(13), "running run moves task to work column")

	// Succeeded: task moves to Done.
	tw.setRunPhase(t, "KB-13", criteriav1.PhaseSucceeded)
	tw.pollOnce(t)
	assert.Equal(t, 8, tw.kbS.taskColumn(13), "succeeded run moves task to Done")
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
// the ticket in the work column -> only the develop run's own success
// stamps Done. The second Succeeded (develop) must reconcile even though
// the triage settle already recorded Succeeded — the reconcile gate tracks
// run identity, not just the phase value.
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

	// The develop run settles: only now does the ticket stamp Done.
	tw.setRunPhaseByName(t, devName, criteriav1.PhaseSucceeded)
	tw.pollOnce(t)

	assert.Equal(t, 8, tw.kbS.taskColumn(21), "only a dev-class success stamps Done")
	assert.Len(t, tw.runs(t), 2, "a settled ticket in Done fires nothing (no route on Done)")
}

// A legacy run predating class stamping (empty spec workflow class) keeps
// the old terminal flow: success stamps Done.
func TestLegacyRunWithoutClassStampsDone(t *testing.T) {
	tw := newTestWatcher(t, chainRoutesJSON)
	tw.kbS.addTask(22, 9, "k8s-run", "internal-reproduced")
	tw.addRun(t, "kb-22-0000000001", "KB-22", "", criteriav1.PhaseSucceeded)

	tw.pollOnce(t)

	assert.Equal(t, 8, tw.kbS.taskColumn(22), "pre-CRI-242 runs (empty class) keep the Done stamp")
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

	tw.pollOnce(t)

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
