package kanboard

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// fakeServer implements the Kanboard JSON-RPC surface the client uses.
type fakeServer struct {
	mu        sync.Mutex
	tasks     map[int]map[string]interface{} // task_id -> task row
	tags      map[int][]string               // task_id -> tag names
	comments  []map[string]interface{}       // appended by createComment
	projects  []map[string]interface{}
	columns   map[int][]map[string]interface{} // project_id -> columns
	moves     int
	lastMove  map[string]interface{}
	ts        *httptest.Server
	failCalls map[string]bool // method -> force error
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	s := &fakeServer{
		tasks:     map[int]map[string]interface{}{},
		tags:      map[int][]string{},
		columns:   map[int][]map[string]interface{}{},
		failCalls: map[string]bool{},
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
		// App auth: jsonrpc:<token>. The fake accepts any non-empty token.
		user, pass, ok := r.BasicAuth()
		if !ok || user != "jsonrpc" || pass != s.appToken() {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if s.failCalls[req.Method] {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"jsonrpc": "2.0", "id": req.ID,
				"error": map[string]interface{}{"code": 1, "message": "injected failure"},
			})
			return
		}
		var params map[string]interface{}
		if len(req.Params) > 0 {
			_ = json.Unmarshal(req.Params, &params)
		}
		// Numeric params arrive as float64 (JSON decode); test fixtures
		// store them as int. Normalize before any int(v.(float64)) cast.
		for k, v := range params {
			if f, ok := v.(float64); ok {
				params[k] = int(f)
			}
		}
		resp := map[string]interface{}{}
		switch req.Method {
		case "getVersion":
			resp["result"] = "1.2.54"
		case "getAllProjects":
			projs := []interface{}{}
			for _, p := range s.projects {
				projs = append(projs, p)
			}
			resp["result"] = projs
		case "getProjectById":
			pid := params["project_id"].(int)
			for _, p := range s.projects {
				if p["id"].(int) == pid {
					resp["result"] = p
				}
			}
		case "getColumns":
			pid := params["project_id"].(int)
			resp["result"] = s.columns[pid]
		case "getAllTasks":
			pid := params["project_id"].(int)
			tasks := []interface{}{}
			for id, task := range s.tasks {
				if task["project_id"].(int) == pid {
					tasks = append(tasks, task)
				}
				_ = id
			}
			resp["result"] = tasks
		case "getTask":
			tid := params["task_id"].(int)
			task, ok := s.tasks[tid]
			if !ok {
				resp["error"] = map[string]interface{}{"code": 1, "message": "task not found"}
			} else {
				resp["result"] = task
			}
		case "getTaskTags":
			// Real Kanboard shape: task_id-only params, result is a map of
			// tag-link-id -> tag name.
			tid := params["task_id"].(int)
			tagMap := map[string]interface{}{}
			for i, name := range s.tags[tid] {
				tagMap[fmt.Sprintf("%d", 1000+tid*10+i)] = name
			}
			resp["result"] = tagMap
		case "moveTaskPosition":
			tid := params["task_id"].(int)
			if task, ok := s.tasks[tid]; ok {
				task["column_id"] = params["column_id"]
			}
			resp["result"] = true
		case "createComment":
			tid := params["task_id"].(int)
			if _, ok := s.tasks[tid]; ok {
				s.comments = append(s.comments, map[string]interface{}{"task_id": tid, "content": params["content"]})
				resp["result"] = true
			} else {
				resp["error"] = map[string]interface{}{"code": 5, "message": "task not found"}
			}
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

func (s *fakeServer) appToken() string { return "test-app-token" }

func TestPingAndVersion(t *testing.T) {
	s := newFakeServer(t)
	c := New(s.ts.URL, s.appToken())
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}
}

func TestPingRejectsBadAuth(t *testing.T) {
	s := newFakeServer(t)
	c := New(s.ts.URL, "")
	if err := c.Ping(context.Background()); err == nil {
		t.Fatal("ping with empty token must fail")
	}
}

func TestFindProjectIDCaches(t *testing.T) {
	s := newFakeServer(t)
	s.projects = []map[string]interface{}{
		{"id": 1, "name": "Tickets"},
		{"id": 2, "name": "Other"},
	}
	c := New(s.ts.URL, s.appToken())
	ctx := context.Background()
	id, err := c.FindProjectID(ctx, "Tickets")
	if err != nil || id != 1 {
		t.Fatalf("FindProjectID = %d, %v; want 1, nil", id, err)
	}
	// Cached second call must not hit the server again.
	s.projects = nil // would fail a second live lookup
	id, err = c.FindProjectID(ctx, "Tickets")
	if err != nil || id != 1 {
		t.Fatalf("cached FindProjectID = %d, %v; want 1, nil", id, err)
	}
}

func TestGetAllTasksHydratesNamesAndTags(t *testing.T) {
	s := newFakeServer(t)
	s.projects = []map[string]interface{}{{"id": 1, "name": "Tickets"}}
	s.columns[1] = []map[string]interface{}{
		{"id": 5, "title": "Backlog"},
		{"id": 6, "title": "Ready"},
	}
	s.tasks[10] = map[string]interface{}{"id": 10, "title": "T", "description": "D", "project_id": 1, "column_id": 5}
	s.tasks[11] = map[string]interface{}{"id": 11, "title": "T2", "description": "", "project_id": 1, "column_id": 6}
	s.tags[10] = []string{"k8s-run", "bug"}

	c := New(s.ts.URL, s.appToken())
	tasks, err := c.GetAllTasks(context.Background(), 1)
	if err != nil {
		t.Fatalf("GetAllTasks: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("got %d tasks, want 2", len(tasks))
	}
	byID := map[int]Task{}
	for _, task := range tasks {
		byID[task.ID] = task
	}
	// fetchTagNames sorts tag names (deterministic map iteration order).
	if got := byID[10]; got.ProjectName != "Tickets" || got.ColumnName != "Backlog" ||
		len(got.Tags) != 2 || got.Tags[0] != "bug" || got.Tags[1] != "k8s-run" {
		t.Errorf("task 10 hydration wrong: %+v", got)
	}
	if got := byID[11]; got.ColumnName != "Ready" || len(got.Tags) != 0 {
		t.Errorf("task 11 hydration wrong: %+v", got)
	}
	if byID[10].Identifier() != "KB-10" {
		t.Errorf("identifier = %q; want KB-10", byID[10].Identifier())
	}
}

func TestMoveTaskToColumnIdempotent(t *testing.T) {
	s := newFakeServer(t)
	s.columns[1] = []map[string]interface{}{{"id": 5, "title": "Backlog"}, {"id": 6, "title": "Ready"}}
	s.tasks[10] = map[string]interface{}{"id": 10, "title": "T", "project_id": 1, "column_id": 5}
	c := New(s.ts.URL, s.appToken())

	col, err := c.MoveTaskToColumn(context.Background(), 10, "Ready")
	if err != nil {
		t.Fatalf("move: %v", err)
	}
	if col != 6 {
		t.Fatalf("move returned col %d, want 6", col)
	}
	if s.tasks[10]["column_id"] != 6 {
		t.Fatalf("task not moved: %+v", s.tasks[10])
	}
	// Moving to the current column is a no-op.
	if _, err := c.MoveTaskToColumn(context.Background(), 10, "Ready"); err != nil {
		t.Fatalf("idempotent move: %v", err)
	}
	// Unknown column fails loudly.
	if _, err := c.MoveTaskToColumn(context.Background(), 10, "Nowhere"); err == nil {
		t.Fatal("move to unknown column must fail")
	}
}

func TestAddComment(t *testing.T) {
	s := newFakeServer(t)
	s.tasks[10] = map[string]interface{}{"id": 10, "title": "T", "project_id": 1, "column_id": 5}
	c := New(s.ts.URL, s.appToken())
	if err := c.AddComment(context.Background(), 10, "hello"); err != nil {
		t.Fatalf("add comment: %v", err)
	}
	if len(s.comments) != 1 || s.comments[0]["content"] != "hello" {
		t.Errorf("comment not recorded: %v", s.comments)
	}
}

func TestInjectedFailureSurfaces(t *testing.T) {
	s := newFakeServer(t)
	s.failCalls["getAllProjects"] = true
	c := New(s.ts.URL, s.appToken())
	if _, err := c.FindProjectID(context.Background(), "Tickets"); err == nil {
		t.Fatal("expected error from injected failure")
	}
}
