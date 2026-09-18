package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brokenbots/workflow-example/criteria-k8s/internal/linear"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
)

// logRecorder is a logr.LogSink collecting message strings, so tests can
// assert the watcher logged a skip or a failure.
type logRecorder struct {
	mu      sync.Mutex
	entries []string
}

func (r *logRecorder) Init(logr.RuntimeInfo) {}
func (r *logRecorder) Enabled(int) bool      { return true }
func (r *logRecorder) Info(level int, msg string, kv ...interface{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, msg)
}
func (r *logRecorder) Error(err error, msg string, kv ...interface{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, msg+" ("+err.Error()+")")
}
func (r *logRecorder) WithName(string) logr.LogSink           { return r }
func (r *logRecorder) WithValues(...interface{}) logr.LogSink { return r }

func (r *logRecorder) contains(substr string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.entries {
		if strings.Contains(e, substr) {
			return true
		}
	}
	return false
}

// linearServer fakes the Linear GraphQL surface the watcher uses. The
// issues query filters returned issues by the requested `states` variable,
// mirroring Linear's `state: {name: {in: $states}}` filter.
type linearServer struct {
	mu          sync.Mutex
	issues      []map[string]interface{}
	comments    []string
	lastStates  []string
	leakyFilter bool
	ts          *httptest.Server
}

func newLinearServer(t *testing.T) *linearServer {
	t.Helper()
	s := &linearServer{}
	s.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var raw struct {
			Query     string          `json:"query"`
			Variables json.RawMessage `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Errorf("decoding request: %v", err)
		}
		resp := map[string]interface{}{}
		switch {
		case strings.Contains(raw.Query, "projects"):
			resp["data"] = map[string]interface{}{
				"projects": map[string]interface{}{
					"nodes": []interface{}{
						map[string]interface{}{"id": "proj-1", "name": "Criteria K8s Workflow Runner"},
					},
				},
			}
		case strings.Contains(raw.Query, "issues"):
			var variables struct {
				States []string `json:"states"`
			}
			if err := json.Unmarshal(raw.Variables, &variables); err != nil {
				t.Errorf("decoding issues variables: %v", err)
				return
			}
			s.mu.Lock()
			s.lastStates = append([]string(nil), variables.States...)
			nodes := make([]interface{}, 0, len(s.issues))
			for _, n := range s.issues {
				if s.leakyFilter {
					nodes = append(nodes, n)
					continue
				}
				stateObj, _ := n["state"].(map[string]interface{})
				state, _ := stateObj["name"].(string)
				// An issue with an unresolvable state (null or absent) can
				// never match a non-empty states filter.
				if state != "" && slices.Contains(variables.States, state) {
					nodes = append(nodes, n)
				}
			}
			s.mu.Unlock()
			resp["data"] = map[string]interface{}{
				"issues": map[string]interface{}{"nodes": nodes},
			}
		case strings.Contains(raw.Query, "commentCreate"):
			var full struct {
				Input struct {
					IssueID string `json:"issueId"`
					Body    string `json:"body"`
				} `json:"input"`
			}
			if err := json.Unmarshal(raw.Variables, &full); err != nil {
				t.Errorf("decoding commentCreate variables: %v", err)
				return
			}
			s.mu.Lock()
			s.comments = append(s.comments, full.Input.Body)
			s.mu.Unlock()
			resp["data"] = map[string]interface{}{
				"commentCreate": map[string]interface{}{"success": true},
			}
		case strings.Contains(raw.Query, "comments"):
			s.mu.Lock()
			nodes := make([]interface{}, len(s.comments))
			for i, body := range s.comments {
				nodes[i] = map[string]interface{}{"body": body}
			}
			s.mu.Unlock()
			resp["data"] = map[string]interface{}{
				"issue": map[string]interface{}{
					"comments": map[string]interface{}{"nodes": nodes},
				},
			}
		default:
			t.Errorf("unexpected query: %s", raw.Query)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(s.ts.Close)
	return s
}

// setIssues installs the issues returned by the next issues query.
func (s *linearServer) setIssues(issues ...map[string]interface{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.issues = issues
}

// setLeakyFilter makes the server ignore the states filter on the next
// queries, returning every staged issue (used to prove the watcher's own
// fail-closed handling of unresolvable state information).
func (s *linearServer) setLeakyFilter() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.leakyFilter = true
}

// queriedStates returns the states variable of the most recent issues query.
func (s *linearServer) queriedStates() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.lastStates...)
}

func (s *linearServer) postedComments() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.comments...)
}

// issue builds a Linear issue node with the given labels. Grouped labels map
// to their parent label group.
func issue(id, identifier string, labels ...map[string]interface{}) map[string]interface{} {
	labelNodes := make([]interface{}, 0, len(labels))
	for _, l := range labels {
		labelNodes = append(labelNodes, l)
	}
	return map[string]interface{}{
		"id":          id,
		"identifier":  identifier,
		"title":       "Test ticket " + identifier,
		"description": "Repo: https://github.com/brokenbots/workflow-example",
		"state":       map[string]interface{}{"name": "Triage"},
		"project":     map[string]interface{}{"id": "proj-1", "name": "Criteria K8s Workflow Runner"},
		"labels":      map[string]interface{}{"nodes": labelNodes},
	}
}

func gateLabel() map[string]interface{} {
	return map[string]interface{}{"name": "k8s-run"}
}

// withState returns the issue node with its Linear workflow state name set.
func withState(m map[string]interface{}, state string) map[string]interface{} {
	m["state"] = map[string]interface{}{"name": state}
	return m
}

// withoutState returns the issue node with an unresolvable (null) state.
func withoutState(m map[string]interface{}) map[string]interface{} {
	m["state"] = nil
	return m
}

func groupLabel(name, group string) map[string]interface{} {
	return map[string]interface{}{
		"name":   name,
		"parent": map[string]interface{}{"name": group, "isGroup": true},
	}
}

const routesJSON = `{
  "apiVersion": "criteria.brokenbots.dev/v1",
  "kind": "Routes",
  "workflowLibrary": {
    "linear-intake-v1": {
      "type": "image",
      "image": "localhost:5000/linear-intake-remote:dev",
      "namespace": "criteria-jobs",
      "env": {"CRITERIA_RUN_DIR_ROOT": "/data/.criteria/runs"},
      "volumes": [
        {"name": "data", "kind": "pvc", "claim": "criteria-data", "mountPath": "/data"}
      ],
      "secrets": [
        {"name": "linear-api-key", "secretProviderClass": "linear-spc", "mountPath": "/secrets"}
      ]
    },
    "linear-intake-url": {
      "type": "url",
      "url": "git::https://github.com/brokenbots/workflow-example.git//linear_intake_v1",
      "namespace": "criteria-jobs"
    }
  },
  "routes": [
    {"name": "criteria-intake", "workflow": "linear-intake-v1", "project": "Criteria K8s Workflow Runner", "states": ["Triage"]},
    {"name": "intake-fast", "workflow": "linear-intake-url", "project": "Criteria K8s Workflow Runner", "tags": ["fast"], "tagMatch": "any", "states": ["Triage"]}
  ]
}`

// routesJSONMultiState declares a route matching more than one state
// (CRI-218) and a tag route with the same list.
const routesJSONMultiState = `{
  "apiVersion": "criteria.brokenbots.dev/v1",
  "kind": "Routes",
  "workflowLibrary": {
    "linear-intake-v1": {
      "type": "image",
      "image": "localhost:5000/linear-intake-remote:dev",
      "namespace": "criteria-jobs"
    }
  },
  "routes": [
    {"name": "wide-intake", "workflow": "linear-intake-v1", "project": "Criteria K8s Workflow Runner", "states": ["Triage", "In Progress"]}
  ]
}`

// routesJSONOmittedStates declares a route with no states key: it must
// behave exactly like states=[Triage] (CRI-218 default).
const routesJSONOmittedStates = `{
  "apiVersion": "criteria.brokenbots.dev/v1",
  "kind": "Routes",
  "workflowLibrary": {
    "linear-intake-v1": {
      "type": "image",
      "image": "localhost:5000/linear-intake-remote:dev",
      "namespace": "criteria-jobs"
    }
  },
  "routes": [
    {"name": "default-states-intake", "workflow": "linear-intake-v1", "project": "Criteria K8s Workflow Runner"}
  ]
}`

// testWatcher wires a watcher against a fake k8s client, fake Linear API and
// a temp routes file, mirroring the deployed wiring.
type testWatcher struct {
	w       *watcher
	linearS *linearServer
	client  client.Client
	logs    *logRecorder
}

func newTestWatcher(t *testing.T, routesJSON string) *testWatcher {
	t.Helper()

	routesPath := filepath.Join(t.TempDir(), "routes.json")
	if routesJSON != "" {
		if err := os.WriteFile(routesPath, []byte(routesJSON), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	scheme := runtime.NewScheme()
	if err := criteriav1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	lin := newLinearServer(t)
	recorder := &logRecorder{}

	tw := &testWatcher{
		linearS: lin,
		client:  k8sClient,
		logs:    recorder,
		w: &watcher{
			client:              k8sClient,
			linear:              linear.NewClientWithBaseURL(lin.ts.URL, "test-key"),
			namespace:           "criteria-jobs",
			projectName:         "Criteria K8s Workflow Runner",
			triggerLabel:        "k8s-run",
			pollInterval:        time.Minute,
			image:               "localhost:5000/criteria-k8s:dev",
			providerBaseURL:     "http://provider/v1",
			maxAgentVisits:      2,
			routesFile:          routesPath,
			workflowsLabelGroup: "workflows",
			repoValidator:       func(string) bool { return true },
			log:                 logr.New(recorder),
		},
	}
	return tw
}

func (tw *testWatcher) pollOnce(t *testing.T) {
	t.Helper()
	if err := tw.w.poll(context.Background(), "proj-1"); err != nil {
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

func TestPollRoutesBehavior(t *testing.T) {
	t.Run("unlabeled ticket without k8s-run gate produces no run", func(t *testing.T) {
		tw := newTestWatcher(t, routesJSON)
		tw.linearS.setIssues(issue("i-1", "CRI-1"))
		tw.pollOnce(t)
		assert.Empty(t, tw.runs(t), "gate unchanged: no run without the k8s-run trigger label")
	})

	t.Run("gated ticket in unmapped project produces no run and logs the skip", func(t *testing.T) {
		tw := newTestWatcher(t, routesJSON)
		// An issue in a project absent from the routes map.
		unmapped := issue("i-2", "CRI-2", gateLabel())
		unmapped["project"] = map[string]interface{}{"id": "proj-9", "name": "Unmapped Project"}
		tw.linearS.setIssues(unmapped)
		tw.pollOnce(t)
		assert.Empty(t, tw.runs(t))
		assert.True(t, tw.logs.contains("no route in the routes map"),
			"watcher should log the skip")
	})

	t.Run("gated ticket in mapped project uses the project default workflow", func(t *testing.T) {
		tw := newTestWatcher(t, routesJSON)
		tw.linearS.setIssues(issue("i-3", "CRI-3", gateLabel()))
		tw.pollOnce(t)
		runs := tw.runs(t)
		require.Len(t, runs, 1)
		wf := runs[0].Spec.Workflow
		require.NotNil(t, wf, "spec.workflow must be stamped for the jobbuilder (CRI-222)")
		assert.Equal(t, "linear-intake-v1", wf.Name)
		assert.Equal(t, "image", wf.Type)
		assert.Equal(t, "criteria-jobs", wf.Namespace)
		assert.Equal(t, "localhost:5000/linear-intake-remote:dev", wf.Image)
		assert.Equal(t, map[string]string{"CRITERIA_RUN_DIR_ROOT": "/data/.criteria/runs"}, wf.Env)
		require.Len(t, wf.Volumes, 1)
		assert.Equal(t, "pvc", wf.Volumes[0].Kind)
		assert.Equal(t, "criteria-data", wf.Volumes[0].Claim)
		require.Len(t, wf.Secrets, 1)
		assert.Equal(t, "linear-spc", wf.Secrets[0].SecretProviderClass)
		assert.Empty(t, tw.linearS.postedComments(), "default routing posts no comment")
	})

	t.Run("workflows-group label overrides the project default", func(t *testing.T) {
		tw := newTestWatcher(t, routesJSON)
		tw.linearS.setIssues(issue("i-4", "CRI-4", gateLabel(), groupLabel("linear-intake-url", "workflows")))
		tw.pollOnce(t)
		runs := tw.runs(t)
		require.Len(t, runs, 1)
		wf := runs[0].Spec.Workflow
		require.NotNil(t, wf)
		assert.Equal(t, "linear-intake-url", wf.Name)
		assert.Equal(t, "url", wf.Type)
		assert.Equal(t, "git::https://github.com/brokenbots/workflow-example.git//linear_intake_v1", wf.URL)
		assert.Equal(t, "criteria-jobs", wf.Namespace)
	})

	t.Run("tag-specific route wins when its tags are satisfied", func(t *testing.T) {
		tw := newTestWatcher(t, routesJSON)
		tw.linearS.setIssues(issue("i-5", "CRI-5", gateLabel(), groupLabel("fast", "speed")))
		tw.pollOnce(t)
		runs := tw.runs(t)
		require.Len(t, runs, 1)
		assert.Equal(t, "linear-intake-url", runs[0].Spec.Workflow.Name,
			"the intake-fast route matches the 'fast' label and uses linear-intake-url")
	})

	t.Run("workflows label naming a missing workflow fails closed with a Linear comment", func(t *testing.T) {
		tw := newTestWatcher(t, routesJSON)
		tw.linearS.setIssues(issue("i-6", "CRI-6", gateLabel(), groupLabel("no-such-workflow", "workflows")))
		tw.pollOnce(t)
		assert.Empty(t, tw.runs(t), "fail closed: no run for a missing workflow")
		assert.True(t, tw.logs.contains("failed closed"),
			"watcher should log the failure")
		comments := tw.linearS.postedComments()
		require.Len(t, comments, 1, "a Linear comment is posted on the ticket")
		assert.Contains(t, comments[0], routingCommentPrefix)
		assert.Contains(t, comments[0], "no-such-workflow")

		// A second poll for the same failure must not spam Linear.
		tw.pollOnce(t)
		assert.Len(t, tw.linearS.postedComments(), 1, "comment dedup: the exact body is only posted once")
	})

	t.Run("ambiguous workflows labels fail closed with a Linear comment", func(t *testing.T) {
		tw := newTestWatcher(t, routesJSON)
		tw.linearS.setIssues(issue("i-7", "CRI-7", gateLabel(),
			groupLabel("linear-intake-v1", "workflows"),
			groupLabel("linear-intake-url", "workflows")))
		tw.pollOnce(t)
		assert.Empty(t, tw.runs(t), "fail closed on ambiguous workflow labels")
		comments := tw.linearS.postedComments()
		require.Len(t, comments, 1)
		assert.Contains(t, comments[0], "multiple workflow labels")
	})

	t.Run("routes file change is honored by the next poll", func(t *testing.T) {
		tw := newTestWatcher(t, routesJSON)
		tw.linearS.setIssues(issue("i-8", "CRI-8", gateLabel()))
		tw.pollOnce(t)
		require.Len(t, tw.runs(t), 1)
		assert.Equal(t, "linear-intake-v1", tw.runs(t)[0].Spec.Workflow.Name)

		// Point the route's project default at the URL workflow: a poll
		// without a watcher restart must use the new payload. A fresh
		// ticket avoids the already-active-run short-circuit.
		changed := strings.Replace(routesJSON, `"workflow": "linear-intake-v1"`, `"workflow": "linear-intake-url"`, 1)
		if changed == routesJSON {
			t.Fatal("routes change did not apply")
		}
		if err := os.WriteFile(tw.w.routesFile, []byte(changed), 0o600); err != nil {
			t.Fatal(err)
		}
		tw.linearS.setIssues(issue("i-9", "CRI-9", gateLabel()))
		tw.pollOnce(t)
		runs := tw.runs(t)
		require.Len(t, runs, 2)
		assert.Equal(t, "linear-intake-url", runs[1].Spec.Workflow.Name,
			"the next poll must honor the edited routes payload")
	})

	t.Run("broken routes file fails closed and skips the whole poll", func(t *testing.T) {
		tw := newTestWatcher(t, "{not-json")
		tw.linearS.setIssues(issue("i-10", "CRI-10", gateLabel()))
		tw.pollOnce(t)
		assert.Empty(t, tw.runs(t), "no runs when the routes payload cannot be loaded")
	})

	t.Run("absent routes file fails closed and skips the whole poll", func(t *testing.T) {
		tw := newTestWatcher(t, "")
		tw.linearS.setIssues(issue("i-11", "CRI-11", gateLabel()))
		tw.pollOnce(t)
		assert.Empty(t, tw.runs(t), "no runs when the routes ConfigMap is not mounted yet")
	})

	t.Run("created run carries jobbuilder-consumable spec", func(t *testing.T) {
		tw := newTestWatcher(t, routesJSON)
		tw.linearS.setIssues(issue("i-12", "CRI-12", gateLabel()))
		tw.pollOnce(t)
		runs := tw.runs(t)
		require.Len(t, runs, 1)
		spec := runs[0].Spec
		assert.Equal(t, "CRI-12", spec.TicketID)
		assert.Equal(t, "brokenbots/workflow-example", spec.RepoURL)
		assert.NotEmpty(t, spec.ProviderBaseURL)
		assert.EqualValues(t, 2, spec.MaxAgentVisits)
		// spec.image stays unset: the operator's default image remains the
		// source of truth for pre-workflow behavior.
		assert.Empty(t, spec.Image)
	})
}

// CRI-218: per-route trigger states with a [Triage] default.
func TestPollPerRouteStates(t *testing.T) {
	t.Run("watcher queries the union of the routes' declared states", func(t *testing.T) {
		tw := newTestWatcher(t, routesJSONMultiState)
		tw.linearS.setIssues(issue("i-20", "CRI-20", gateLabel()))
		tw.pollOnce(t)
		states := tw.linearS.queriedStates()
		if !slices.Equal(states, []string{"In Progress", "Triage"}) {
			t.Errorf("watcher queried states %v; want sorted dedup union [In Progress Triage]", states)
		}
	})

	t.Run("route with states=[Triage] fires only for Triage tickets", func(t *testing.T) {
		tw := newTestWatcher(t, routesJSON)
		// Leaky server: both tickets reach the watcher even though only
		// Triage was queried, so this exercises the per-route state gate
		// itself, not just the Linear states filter.
		tw.linearS.setLeakyFilter()
		tw.linearS.setIssues(
			issue("i-21", "CRI-21", gateLabel()),
			withState(issue("i-22", "CRI-22", gateLabel()), "In Progress"),
		)
		tw.pollOnce(t)
		runs := tw.runs(t)
		require.Len(t, runs, 1, "only the Triage ticket fires")
		assert.Equal(t, "CRI-21", runs[0].Spec.TicketID)
		assert.True(t, tw.logs.contains("no route in the routes map"),
			"the In Progress ticket is skipped by the per-route state gate")
	})

	t.Run("route with a custom states list fires for any listed state", func(t *testing.T) {
		tw := newTestWatcher(t, routesJSONMultiState)
		tw.linearS.setIssues(
			issue("i-23", "CRI-23", gateLabel()),
			withState(issue("i-24", "CRI-24", gateLabel()), "In Progress"),
		)
		tw.pollOnce(t)
		runs := tw.runs(t)
		require.Len(t, runs, 2, "both a Triage and an In Progress ticket fire")
		assert.Equal(t, "CRI-23", runs[0].Spec.TicketID)
		assert.Equal(t, "CRI-24", runs[1].Spec.TicketID)
	})

	t.Run("ticket in a state outside the route's list never fires", func(t *testing.T) {
		tw := newTestWatcher(t, routesJSONMultiState)
		// The states filter already hides unlisted-state tickets from the
		// watcher; with a leaky server (e.g. the routes payload gained a
		// state after the query was issued) the per-route check itself must
		// still reject the ticket. k8s-run gate, matching project and tag:
		// everything matches except the state.
		tw.linearS.setLeakyFilter()
		tw.linearS.setIssues(withState(issue("i-25", "CRI-25", gateLabel(), groupLabel("fast", "speed")), "Done"))
		tw.pollOnce(t)
		assert.Empty(t, tw.runs(t), "no run for a state outside the route's states list")
		assert.True(t, tw.logs.contains("no route in the routes map"))
	})

	t.Run("route omitting states behaves identically to states=[Triage]", func(t *testing.T) {
		tw := newTestWatcher(t, routesJSONOmittedStates)
		tw.linearS.setIssues(
			issue("i-26", "CRI-26", gateLabel()),
			withState(issue("i-27", "CRI-27", gateLabel()), "In Progress"),
		)
		tw.pollOnce(t)
		runs := tw.runs(t)
		require.Len(t, runs, 1, "the omitted-states route defaults to [Triage]")
		assert.Equal(t, "CRI-26", runs[0].Spec.TicketID)
		assert.Equal(t, "linear-intake-v1", runs[0].Spec.Workflow.Name)

		// The default must reach the Linear query: only [Triage] is polled.
		assert.True(t, tw.logs.contains("polling Linear for ticket states declared by routes"))
		states := tw.linearS.queriedStates()
		if !slices.Equal(states, []string{"Triage"}) {
			t.Errorf("watcher queried states %v; want [Triage]", states)
		}
	})

	t.Run("empty routes payload fails closed and never fires", func(t *testing.T) {
		// Validate rejects a routes payload with no routes; the watcher
		// logs the failure and skips the whole poll instead of firing.
		tw := newTestWatcher(t, `{"apiVersion":"criteria.brokenbots.dev/v1","kind":"Routes","workflowLibrary":{"linear-intake-v1":{"type":"image","image":"i","namespace":"criteria-jobs"}},"routes":[]}`)
		tw.linearS.setIssues(issue("i-28", "CRI-28", gateLabel()))
		tw.pollOnce(t)
		assert.Empty(t, tw.runs(t), "no routes: the whole poll fails closed")
		assert.True(t, tw.logs.contains("failing closed"))
	})

	t.Run("unresolvable ticket state does not trigger (fail closed)", func(t *testing.T) {
		tw := newTestWatcher(t, routesJSON)
		tw.linearS.setLeakyFilter()
		tw.linearS.setIssues(withoutState(issue("i-29", "CRI-29", gateLabel())))
		tw.pollOnce(t)
		assert.Empty(t, tw.runs(t), "a ticket whose state Linear cannot resolve never fires")
		assert.True(t, tw.logs.contains("no route in the routes map"),
			"watcher logs the fail-closed skip for the empty state")
	})
}
