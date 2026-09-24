package main

import (
	"context"
	"encoding/json"
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
			tid := intParam("task_id")
			tags := []interface{}{}
			for _, name := range s.tags[tid] {
				tags = append(tags, map[string]interface{}{"name": name})
			}
			resp["result"] = tags
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
