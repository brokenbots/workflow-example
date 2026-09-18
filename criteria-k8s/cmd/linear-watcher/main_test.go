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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
)

// logRecorder is a logr.LogSink collecting log entries with their levels,
// so tests can assert the watcher logged a skip or a failure and that
// verbosity-gated messages (V(1)) appear exactly as often as intended.
type logEntry struct {
	level int // Info verbosity (0, 1, ...); -1 for Error
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

// infoCount counts always-emitted (V(0)) Info entries whose message
// contains substr.
func (r *logRecorder) infoCount(substr string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	count := 0
	for _, e := range r.entries {
		if e.level == 0 && strings.Contains(e.msg, substr) {
			count++
		}
	}
	return count
}

// v1InfoCount counts verbosity-gated (V(1)) Info entries whose message
// contains substr.
func (r *logRecorder) v1InfoCount(substr string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	count := 0
	for _, e := range r.entries {
		if e.level == 1 && strings.Contains(e.msg, substr) {
			count++
		}
	}
	return count
}

// fakeLabel is a team-scoped Linear issue label in the fake server.
type fakeLabel struct {
	ID     string
	Name   string
	Color  string
	TeamID string
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
	// CRI-219: label lifecycle state. issueLabelNames is the source of
	// truth for an issue's labels; ids are synthesized as "lbl-"+name.
	teamID          string
	labels          []fakeLabel
	issueLabelNames map[string][]string
	labelUpdates    int      // issueUpdate mutation count (no-ops excluded)
	lastLabelIDs    []string // labelIds payload of the most recent issueUpdate
	failLabelEnsure bool
	// CRI-220: when set, the orphan-sweep candidates query fails with a
	// GraphQL error.
	failCandidates bool
	// CRI-252: request accounting and rate-limit injection. While
	// rateLimited is set, every query is answered with a rate limit
	// rejection instead of its normal response: HTTP 429 (with an optional
	// Retry-After header) or a 200 GraphQL RATELIMITED error carrying a
	// rateLimitResult duration in milliseconds.
	requests            int
	queriesServed       []string
	// timestamps and rejected record, per request, when it was served and
	// whether it got the rate-limit rejection — the back-off cadence test
	// (CRI-252) asserts when the watcher actually re-issues Linear
	// requests relative to the advertised duration.
	timestamps          []time.Time
	rejected            []time.Time
	rateLimited         bool
	rateLimitHTTP429    bool
	retryAfterHeader    string
	rateLimitDurationMS int
	ts                  *httptest.Server
}

func newLinearServer(t *testing.T) *linearServer {
	t.Helper()
	s := &linearServer{
		teamID:          "team-1",
		issueLabelNames: map[string][]string{},
	}
	s.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var raw struct {
			Query     string          `json:"query"`
			Variables json.RawMessage `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Errorf("decoding request: %v", err)
		}
		// CRI-252: count every request and, while the rate-limit injection
		// is set, reject every query with a rate limit response before any
		// normal handling.
		s.mu.Lock()
		s.requests++
		s.queriesServed = append(s.queriesServed, raw.Query)
		now := time.Now()
		s.timestamps = append(s.timestamps, now)
		limited, four29, retryAfter, durationMS := s.rateLimited, s.rateLimitHTTP429, s.retryAfterHeader, s.rateLimitDurationMS
		if limited {
			s.rejected = append(s.rejected, now)
		}
		s.mu.Unlock()
		if limited {
			w.Header().Set("Content-Type", "application/json")
			if four29 {
				if retryAfter != "" {
					w.Header().Set("Retry-After", retryAfter)
				}
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte("rate limited"))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"errors": []interface{}{map[string]interface{}{
					"message": "Too many requests",
					"extensions": map[string]interface{}{
						"code": "RATELIMITED",
						"meta": map[string]interface{}{
							"rateLimitResult": map[string]interface{}{"duration": durationMS},
						},
					},
				}},
			})
			return
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
		case strings.Contains(raw.Query, "teams"):
			s.mu.Lock()
			resp["data"] = map[string]interface{}{
				"project": map[string]interface{}{
					"teams": map[string]interface{}{
						"nodes": []interface{}{map[string]interface{}{"id": s.teamID}},
					},
				},
			}
			s.mu.Unlock()
		case strings.Contains(raw.Query, "labels: {some:"):
			// CRI-220: the orphan sweep's candidates query. Filter the
			// staged issues by their current label state (issueLabelNames
			// is the runtime truth), regardless of workflow state —
			// mirroring Linear's `labels: {some: {name: {eq: $label}}}`
			// filter.
			var variables struct {
				Project string `json:"project"`
				Label   string `json:"label"`
			}
			if err := json.Unmarshal(raw.Variables, &variables); err != nil {
				t.Errorf("decoding candidates variables: %v", err)
				return
			}
			s.mu.Lock()
			fail := s.failCandidates
			if fail {
				s.mu.Unlock()
				resp["errors"] = []interface{}{map[string]interface{}{"message": "candidates query down"}}
				break
			}
			nodes := []interface{}{}
			for _, n := range s.issues {
				if slices.Contains(s.issueLabelNames[n["id"].(string)], variables.Label) {
					nodes = append(nodes, s.servedIssueLocked(n))
				}
			}
			s.mu.Unlock()
			resp["data"] = map[string]interface{}{
				"issues": map[string]interface{}{"nodes": nodes},
			}
		case strings.Contains(raw.Query, "id: {in:"):
			// CRI-252: the batched identifier lookup for change-driven
			// reconciliation. Unknown identifiers are simply absent from
			// the response.
			var variables struct {
				IDs []string `json:"ids"`
			}
			if err := json.Unmarshal(raw.Variables, &variables); err != nil {
				t.Errorf("decoding batched ids variables: %v", err)
				return
			}
			s.mu.Lock()
			nodes := []interface{}{}
			for _, n := range s.issues {
				ident, _ := n["identifier"].(string)
				if slices.Contains(variables.IDs, ident) {
					nodes = append(nodes, s.servedIssueLocked(n))
				}
			}
			s.mu.Unlock()
			resp["data"] = map[string]interface{}{
				"issues": map[string]interface{}{"nodes": nodes},
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
					nodes = append(nodes, s.servedIssueLocked(n))
					continue
				}
				stateObj, _ := n["state"].(map[string]interface{})
				state, _ := stateObj["name"].(string)
				// An issue with an unresolvable state (null or absent) can
				// never match a non-empty states filter.
				if state != "" && slices.Contains(variables.States, state) {
					nodes = append(nodes, s.servedIssueLocked(n))
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
		case strings.Contains(raw.Query, "issueUpdate"):
			var full struct {
				ID    string `json:"id"`
				Input struct {
					LabelIDs []string `json:"labelIds"`
				} `json:"input"`
			}
			if err := json.Unmarshal(raw.Variables, &full); err != nil {
				t.Errorf("decoding issueUpdate variables: %v", err)
				return
			}
			s.mu.Lock()
			names := make([]string, 0, len(full.Input.LabelIDs))
			for _, id := range full.Input.LabelIDs {
				names = append(names, s.labelNameLocked(id))
			}
			s.issueLabelNames[full.ID] = names
			s.labelUpdates++
			s.lastLabelIDs = append([]string(nil), full.Input.LabelIDs...)
			s.mu.Unlock()
			resp["data"] = map[string]interface{}{
				"issueUpdate": map[string]interface{}{"success": true},
			}
		case strings.Contains(raw.Query, "issueLabelCreate"):
			s.mu.Lock()
			fail := s.failLabelEnsure
			s.mu.Unlock()
			if fail {
				resp = map[string]interface{}{
					"errors": []interface{}{map[string]interface{}{"message": "label create down"}},
				}
				break
			}
			var full struct {
				Input struct {
					Name   string `json:"name"`
					Color  string `json:"color"`
					TeamID string `json:"teamId"`
				} `json:"input"`
			}
			if err := json.Unmarshal(raw.Variables, &full); err != nil {
				t.Errorf("decoding issueLabelCreate variables: %v", err)
				return
			}
			s.mu.Lock()
			id := "lbl-" + full.Input.Name
			exists := false
			for _, l := range s.labels {
				if l.Name == full.Input.Name && l.TeamID == full.Input.TeamID {
					id = l.ID
					exists = true
					break
				}
			}
			if !exists {
				s.labels = append(s.labels, fakeLabel{
					ID: id, Name: full.Input.Name, Color: full.Input.Color, TeamID: full.Input.TeamID,
				})
			}
			s.mu.Unlock()
			resp["data"] = map[string]interface{}{
				"issueLabelCreate": map[string]interface{}{
					"success":    true,
					"issueLabel": map[string]interface{}{"id": id},
				},
			}
		case strings.Contains(raw.Query, "issueLabels"):
			s.mu.Lock()
			fail := s.failLabelEnsure
			s.mu.Unlock()
			if fail {
				resp = map[string]interface{}{
					"errors": []interface{}{map[string]interface{}{"message": "label query down"}},
				}
				break
			}
			var variables struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(raw.Variables, &variables); err != nil {
				t.Errorf("decoding issueLabels variables: %v", err)
				return
			}
			s.mu.Lock()
			nodes := []interface{}{}
			for _, l := range s.labels {
				if l.Name == variables.Name {
					nodes = append(nodes, map[string]interface{}{
						"id":   l.ID,
						"name": l.Name,
						"team": map[string]interface{}{"id": l.TeamID},
					})
				}
			}
			s.mu.Unlock()
			resp["data"] = map[string]interface{}{
				"issueLabels": map[string]interface{}{"nodes": nodes},
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

// setIssues installs the issues returned by the next issues query. The
// labels of each issue seed the fake's per-issue label state, which is then
// the source of truth the issues and `issue(id:)` queries serve.
func (s *linearServer) setIssues(issues ...map[string]interface{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.issues = issues
	for _, n := range issues {
		id, _ := n["id"].(string)
		if id == "" {
			continue
		}
		if _, ok := s.issueLabelNames[id]; ok {
			continue
		}
		var names []string
		if labelsObj, ok := n["labels"].(map[string]interface{}); ok {
			if labelNodes, ok := labelsObj["nodes"].([]interface{}); ok {
				for _, ln := range labelNodes {
					lm, _ := ln.(map[string]interface{})
					name, _ := lm["name"].(string)
					if name != "" {
						names = append(names, name)
					}
				}
			}
		}
		s.issueLabelNames[id] = names
	}
}

// servedIssueLocked returns the issue node with its labels derived from the
// fake's per-issue label state (the lifecycle mutates labels at runtime, so
// the static fixture must not shadow it). Fixture label nodes are kept —
// they may carry group info the route resolver needs — and runtime-added
// labels are synthesized as name-only nodes. Every served label node gets
// the fake's deterministic `lbl-`+name id (CRI-252: the batched query must
// return the same label ids the per-ticket resolution did). Callers must
// hold s.mu.
func (s *linearServer) servedIssueLocked(n map[string]interface{}) map[string]interface{} {
	id, _ := n["id"].(string)
	names, ok := s.issueLabelNames[id]
	if !ok {
		return n
	}
	fixtureNodes := map[string]interface{}{}
	if labelsObj, ok := n["labels"].(map[string]interface{}); ok {
		if labelNodes, ok := labelsObj["nodes"].([]interface{}); ok {
			for _, ln := range labelNodes {
				lm, _ := ln.(map[string]interface{})
				name, _ := lm["name"].(string)
				fixtureNodes[name] = ln
			}
		}
	}
	labelNodes := make([]interface{}, 0, len(names))
	for _, name := range names {
		node := map[string]interface{}{"name": name, "id": s.labelIDLocked(name)}
		if fixture, ok := fixtureNodes[name]; ok {
			if fm, ok := fixture.(map[string]interface{}); ok {
				for k, v := range fm {
					if k != "id" {
						node[k] = v
					}
				}
			}
		}
		labelNodes = append(labelNodes, node)
	}
	clone := make(map[string]interface{}, len(n))
	for k, v := range n {
		clone[k] = v
	}
	clone["labels"] = map[string]interface{}{"nodes": labelNodes}
	return clone
}

// labelNameLocked resolves a label ID to its name; unknown IDs follow the
// fake's deterministic "lbl-"+name synthesis. Callers must hold s.mu.
func (s *linearServer) labelNameLocked(id string) string {
	for _, l := range s.labels {
		if l.ID == id {
			return l.Name
		}
	}
	return strings.TrimPrefix(id, "lbl-")
}

// labelIDLocked resolves a label name to its ID. Callers must hold s.mu.
func (s *linearServer) labelIDLocked(name string) string {
	for _, l := range s.labels {
		if l.Name == name {
			return l.ID
		}
	}
	return "lbl-" + name
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

// issueLabelsOf returns a copy of the label names the issue currently
// carries in the fake.
func (s *linearServer) issueLabelsOf(issueID string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.issueLabelNames[issueID]...)
}

// setIssueLabels replaces the issue's label state directly, simulating
// out-of-band label changes (e.g. a human removing the trigger label).
func (s *linearServer) setIssueLabels(issueID string, names ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.issueLabelNames[issueID] = append([]string(nil), names...)
}

// setFailLabelServe makes the label ensure queries fail with a GraphQL
// error until re-enabled.
func (s *linearServer) setFailLabelServe(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failLabelEnsure = v
}

// setFailCandidates makes the CRI-220 orphan-sweep candidates query fail
// with a GraphQL error until re-enabled.
func (s *linearServer) setFailCandidates(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failCandidates = v
}

// requestCount returns the number of Linear queries the server served.
func (s *linearServer) requestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests
}

// requestTimes returns the wall-clock time at which every request was
// served, so the back-off cadence test can assert when the watcher
// re-issues Linear requests relative to a rate-limit rejection (CRI-252).
func (s *linearServer) requestTimes() []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Time(nil), s.timestamps...)
}

// rejectedTimes returns the serve times of the requests that got the
// rate-limit rejection.
func (s *linearServer) rejectedTimes() []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Time(nil), s.rejected...)
}

// resetRequests zeroes the request counter, so a subsequent poll can assert
// its own request budget (CRI-252: a no-change poll must issue exactly two
// listing queries — the states query and the orphan-sweep candidates query).
func (s *linearServer) resetRequests() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = 0
	s.queriesServed = nil
}

// queriesServed returns the query texts of every request served since the
// last resetRequests, for debugging request-budget assertions.
func (s *linearServer) queriesServedCopy() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.queriesServed...)
}

// queryKinds classifies every request served since the last resetRequests
// into a stable vocabulary, so tests can pin exact Linear request budgets
// (CRI-252) without matching raw GraphQL text.
func (s *linearServer) queryKinds() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	kinds := make([]string, 0, len(s.queriesServed))
	for _, q := range s.queriesServed {
		switch {
		case strings.Contains(q, "id: {in:"):
			kinds = append(kinds, "ticketBatch")
		case strings.Contains(q, "labels: {some:"):
			kinds = append(kinds, "sweepCandidates")
		case strings.Contains(q, "issueUpdate"):
			kinds = append(kinds, "labelWrite")
		case strings.Contains(q, "commentCreate"):
			kinds = append(kinds, "commentWrite")
		case strings.Contains(q, "comments"):
			kinds = append(kinds, "commentRead")
		case strings.Contains(q, "issueLabelCreate"):
			kinds = append(kinds, "labelCreate")
		case strings.Contains(q, "issueLabels"):
			kinds = append(kinds, "labelRead")
		case strings.Contains(q, "projects"):
			kinds = append(kinds, "projectLookup")
		case strings.Contains(q, "teams"):
			kinds = append(kinds, "teamLookup")
		case strings.Contains(q, "issues"):
			kinds = append(kinds, "stateListing")
		default:
			kinds = append(kinds, "unknown")
		}
	}
	return kinds
}

// setRateLimited turns on rate-limit rejection. http429 selects the
// transport-level rejection (with an optional Retry-After header);
// otherwise the server answers 200 with a GraphQL RATELIMITED error
// carrying durationMS — Linear reports the rateLimitResult duration in
// milliseconds.
func (s *linearServer) setRateLimited(http429 bool, retryAfter string, durationMS int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rateLimited = true
	s.rateLimitHTTP429 = http429
	s.retryAfterHeader = retryAfter
	s.rateLimitDurationMS = durationMS
}

// clearRateLimited heals the server: queries are answered normally again.
func (s *linearServer) clearRateLimited() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rateLimited = false
	s.rateLimitHTTP429 = false
	s.retryAfterHeader = ""
	s.rateLimitDurationMS = 0
}

// createdLabels returns a copy of the fake's label registry.
func (s *linearServer) createdLabels() []fakeLabel {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]fakeLabel(nil), s.labels...)
}

// labelUpdateCount returns the number of issueUpdate mutations served.
func (s *linearServer) labelUpdateCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.labelUpdates
}

// lastLabelUpdateIDs returns the labelIds payload of the most recent
// issueUpdate mutation.
func (s *linearServer) lastLabelUpdateIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.lastLabelIDs...)
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
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&criteriav1.CriteriaRun{}).Build()

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
			// The deployed watcher resolves the team in run(); tests set it
			// directly (CRI-219 team-scoped labels).
			teamID: lin.teamID,
			log:    logr.New(recorder),
		},
	}
	return tw
}

// restarted simulates killing and restarting the watcher: a fresh watcher
// over the same fake Linear server and the same k8s client (both persist
// across a restart), with a fresh log recorder and empty in-memory state.
func (tw *testWatcher) restarted() *testWatcher {
	recorder := &logRecorder{}
	fresh := *tw.w
	// CRI-252: a restart drops the watcher's in-memory state — the cached
	// automation label ids and the recorded per-ticket phases — while the
	// fake Linear server and the k8s objects persist across the restart.
	fresh.automationLabelID = ""
	fresh.dirtyLabelID = ""
	fresh.lastPhases = nil
	fresh.backoffAttempt = 0
	fresh.log = logr.New(recorder)
	return &testWatcher{w: &fresh, linearS: tw.linearS, client: tw.client, logs: recorder}
}

// setRunPhase moves the ticket's single CriteriaRun to the given phase in
// the fake k8s client. It covers live phases (Pending, Running, the empty
// pre-status phase) as well as terminal ones.
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

// CRI-219: automation label lifecycle.

// TestPollEnsuresAutomationLabels proves the watcher creates both
// team-scoped labels when missing (distinct colors), does not duplicate
// them on later polls, and writes no label state onto issues without
// CriteriaRuns.
func TestPollEnsuresAutomationLabels(t *testing.T) {
	tw := newTestWatcher(t, routesJSON)
	tw.linearS.setIssues(issue("i-1", "CRI-1"))

	tw.pollOnce(t)

	created := tw.linearS.createdLabels()
	require.Len(t, created, 2, "both automation labels are created when missing")
	assert.Equal(t, "criteria-automation", created[0].Name)
	assert.Equal(t, automationLabelColor, created[0].Color)
	assert.Equal(t, "criteria-dirty", created[1].Name)
	assert.Equal(t, dirtyLabelColor, created[1].Color)
	assert.NotEqual(t, created[0].Color, created[1].Color, "the two labels must be visually distinct")
	for _, l := range created {
		assert.Equal(t, "team-1", l.TeamID, "labels are team-scoped")
	}
	assert.Equal(t, 0, tw.linearS.labelUpdateCount(),
		"no label writes on an issue without CriteriaRuns")

	// A second poll must not create duplicates.
	tw.pollOnce(t)
	assert.Len(t, tw.linearS.createdLabels(), 2, "no duplicate label on re-ensure")
}

// TestPollCreateRunAddsAutomationLabel covers the add on CriteriaRun
// creation: the issue carries the automation label alongside every label it
// already had. The write payload must be the full merged set — a regression
// to a REPLACE-only delta would drop the issue's existing labels.
func TestPollCreateRunAddsAutomationLabel(t *testing.T) {
	tw := newTestWatcher(t, routesJSON)
	tw.linearS.setIssues(issue("i-1", "CRI-1", gateLabel(), groupLabel("fast", "speed")))

	tw.pollOnce(t)

	require.Len(t, tw.runs(t), 1)
	assert.Equal(t, []string{"k8s-run", "fast", "criteria-automation"},
		tw.linearS.issueLabelsOf("i-1"),
		"the automation label is added without dropping existing labels")
	assert.Equal(t, 1, tw.linearS.labelUpdateCount())
	assert.Equal(t, []string{"lbl-k8s-run", "lbl-fast", "lbl-criteria-automation"},
		tw.linearS.lastLabelUpdateIDs(),
		"the write must carry the full merged label set (REPLACE semantics)")
	assert.True(t, tw.logs.contains("wrote Linear labels"))
}

// TestPollSucceededRemovesAutomationNoDirty covers the clean terminal state:
// when the CriteriaRun succeeds, the automation label is removed and the
// dirty label is not added.
func TestPollSucceededRemovesAutomationNoDirty(t *testing.T) {
	tw := newTestWatcher(t, routesJSON)
	tw.linearS.setIssues(issue("i-1", "CRI-1", gateLabel(), groupLabel("fast", "speed")))
	tw.pollOnce(t)
	require.Len(t, tw.runs(t), 1)
	assert.Contains(t, tw.linearS.issueLabelsOf("i-1"), automationLabelName)

	// The run settles as Succeeded and the trigger label is cleared with
	// it (as operator automation does on completion). The inflight marker
	// is still on the issue: removing it is the watcher's job.
	tw.setRunPhase(t, "CRI-1", criteriav1.PhaseSucceeded)
	tw.linearS.setIssueLabels("i-1", "fast", automationLabelName)

	writes := tw.linearS.labelUpdateCount()
	tw.pollOnce(t)

	assert.Equal(t, []string{"fast"}, tw.linearS.issueLabelsOf("i-1"),
		"the automation label is removed and no dirty label is added")
	assert.Equal(t, 1, tw.linearS.labelUpdateCount()-writes, "exactly one removal write")
	assert.Equal(t, []string{"lbl-fast"}, tw.linearS.lastLabelUpdateIDs(),
		"the removal write carries only the remaining labels")
	assert.True(t, tw.logs.contains("wrote Linear labels"))
}

// TestPollFailedRemovesAutomationAddsDirty covers the dirty terminal state:
// when the CriteriaRun fails, the automation label is removed and the dirty
// label is added.
func TestPollFailedRemovesAutomationAddsDirty(t *testing.T) {
	tw := newTestWatcher(t, routesJSON)
	tw.linearS.setIssues(issue("i-1", "CRI-1", gateLabel(), groupLabel("fast", "speed")))
	tw.pollOnce(t)
	require.Len(t, tw.runs(t), 1)
	assert.Contains(t, tw.linearS.issueLabelsOf("i-1"), automationLabelName)

	// The run fails and the trigger label is cleared with it. The inflight
	// marker is still on the issue: the watcher must swap it for the dirty
	// marker.
	tw.setRunPhase(t, "CRI-1", criteriav1.PhaseFailed)
	tw.linearS.setIssueLabels("i-1", "fast", automationLabelName)

	writes := tw.linearS.labelUpdateCount()
	tw.pollOnce(t)

	assert.Equal(t, []string{"fast", "criteria-dirty"},
		tw.linearS.issueLabelsOf("i-1"),
		"automation label removed, dirty label present, other labels intact")
	assert.Equal(t, 1, tw.linearS.labelUpdateCount()-writes,
		"a single REPLACE write performs the removal and the dirty add")
	assert.True(t, tw.logs.contains("wrote Linear labels"), "the label write is logged")
}

// TestRestartConvergesLabelState proves restart-safety: a fresh watcher with
// no in-memory run state reconciles label state from the observed
// CriteriaRun phase and the issue's current labels, converging after a kill
// between phase changes.
func TestRestartConvergesLabelState(t *testing.T) {
	t.Run("succeeded run: fresh watcher removes the inflight marker", func(t *testing.T) {
		tw := newTestWatcher(t, routesJSON)
		tw.linearS.setIssues(issue("i-1", "CRI-1", gateLabel()))
		tw.pollOnce(t)
		require.Len(t, tw.runs(t), 1)
		assert.Contains(t, tw.linearS.issueLabelsOf("i-1"), automationLabelName)

		// The watcher dies; the run then succeeds and the trigger label is
		// cleared before the replacement watcher starts.
		tw.setRunPhase(t, "CRI-1", criteriav1.PhaseSucceeded)
		tw.linearS.setIssueLabels("i-1", "fast", automationLabelName)

		tw2 := tw.restarted()
		writes := tw2.linearS.labelUpdateCount()
		tw2.pollOnce(t)

		assert.Equal(t, []string{"fast"}, tw2.linearS.issueLabelsOf("i-1"),
			"no lingering criteria-automation after Succeeded, no dirty label")
		assert.Equal(t, 1, tw2.linearS.labelUpdateCount()-writes,
			"the fresh watcher performs the removal itself")

		// Converged state stays converged: the next poll is a no-op.
		tw2.pollOnce(t)
		assert.Equal(t, 1, tw2.linearS.labelUpdateCount()-writes,
			"the next poll performs no further label writes")
	})

	t.Run("failed run: fresh watcher removes the marker and marks dirty", func(t *testing.T) {
		tw := newTestWatcher(t, routesJSON)
		tw.linearS.setIssues(issue("i-1", "CRI-1", gateLabel()))
		tw.pollOnce(t)
		require.Len(t, tw.runs(t), 1)

		tw.setRunPhase(t, "CRI-1", criteriav1.PhaseFailed)
		tw.linearS.setIssueLabels("i-1", "fast", automationLabelName)

		tw2 := tw.restarted()
		writes := tw2.linearS.labelUpdateCount()
		tw2.pollOnce(t)

		assert.Equal(t, []string{"fast", "criteria-dirty"}, tw2.linearS.issueLabelsOf("i-1"),
			"no lingering criteria-automation after Failed and criteria-dirty present")
		assert.Equal(t, 1, tw2.linearS.labelUpdateCount()-writes,
			"a single REPLACE write performs the removal and the dirty add")

		// Converged state stays converged: the next poll is a no-op.
		tw2.pollOnce(t)
		assert.Equal(t, 1, tw2.linearS.labelUpdateCount()-writes,
			"the next poll performs no further label writes")
	})
}

// TestReconcileTicketOutsideRouteStates covers the production path the run
// firing cannot see: the intake workflow moves the ticket out of the
// route-declared states (to "Done" on success, "In Review" on handler
// failure) before its CriteriaRun settles. Reconciliation is driven by the
// CriteriaRun list, so the watcher still converges the ticket's labels; run
// firing stays scoped to the route-declared states, so no successor run
// fires for a settled ticket regardless of its trigger label.
func TestReconcileTicketOutsideRouteStates(t *testing.T) {
	t.Run("succeeded run while the ticket is in Done", func(t *testing.T) {
		tw := newTestWatcher(t, routesJSON)
		tw.linearS.setIssues(issue("i-9", "CRI-9", gateLabel(), groupLabel("fast", "speed")))
		tw.pollOnce(t)
		require.Len(t, tw.runs(t), 1)
		assert.Contains(t, tw.linearS.issueLabelsOf("i-9"), automationLabelName)

		// The run succeeds; the workflow moves the ticket to "Done" (a
		// state no route declares) and the trigger label stays armed.
		tw.setRunPhase(t, "CRI-9", criteriav1.PhaseSucceeded)
		tw.linearS.setIssues(withState(
			issue("i-9", "CRI-9", gateLabel(), groupLabel("fast", "speed")), "Done"))
		tw.linearS.setIssueLabels("i-9", "k8s-run", "fast", automationLabelName)

		tw2 := tw.restarted()
		writes := tw2.linearS.labelUpdateCount()
		tw2.pollOnce(t)

		assert.Equal(t, []string{"k8s-run", "fast"}, tw2.linearS.issueLabelsOf("i-9"),
			"criteria-automation removed outside the route-declared states, other labels intact")
		assert.Equal(t, 1, tw2.linearS.labelUpdateCount()-writes,
			"exactly one removal write")
		assert.Len(t, tw2.runs(t), 1,
			"run firing stays scoped to route-declared states; no successor run fires")
	})

	t.Run("failed run while the ticket is in In Review", func(t *testing.T) {
		tw := newTestWatcher(t, routesJSON)
		tw.linearS.setIssues(issue("i-9", "CRI-9", gateLabel(), groupLabel("fast", "speed")))
		tw.pollOnce(t)
		require.Len(t, tw.runs(t), 1)

		// The run fails; the workflow moves the ticket to "In Review" for
		// triage (also outside the route-declared states).
		tw.setRunPhase(t, "CRI-9", criteriav1.PhaseFailed)
		tw.linearS.setIssues(withState(
			issue("i-9", "CRI-9", gateLabel(), groupLabel("fast", "speed")), "In Review"))
		tw.linearS.setIssueLabels("i-9", "k8s-run", "fast", automationLabelName)

		tw2 := tw.restarted()
		writes := tw2.linearS.labelUpdateCount()
		tw2.pollOnce(t)

		assert.Equal(t, []string{"k8s-run", "fast", "criteria-dirty"},
			tw2.linearS.issueLabelsOf("i-9"),
			"criteria-automation removed and criteria-dirty added outside the route-declared states")
		assert.Equal(t, 1, tw2.linearS.labelUpdateCount()-writes,
			"a single REPLACE write performs the removal and the dirty add")
		assert.Len(t, tw2.runs(t), 1,
			"run firing stays scoped to route-declared states; no successor run fires")
	})
}

// TestPollEnsureFailureDoesNotBlockRunCreation covers the degraded Linear
// path: when the label ensure fails, run creation still proceeds and label
// writes are deferred; the next healthy poll converges the label.
func TestPollEnsureFailureDoesNotBlockRunCreation(t *testing.T) {
	tw := newTestWatcher(t, routesJSON)
	tw.linearS.setIssues(issue("i-1", "CRI-1", gateLabel()))
	tw.linearS.setFailLabelServe(true)

	tw.pollOnce(t)

	require.Len(t, tw.runs(t), 1, "run creation is not blocked by the label ensure failure")
	assert.NotContains(t, tw.linearS.issueLabelsOf("i-1"), automationLabelName,
		"no label write with unresolved label ids")
	assert.Equal(t, 0, tw.linearS.labelUpdateCount())
	assert.True(t, tw.logs.contains("ensuring automation label"))
	assert.True(t, tw.logs.contains("ensuring dirty label"))

	// The ensure heals on the next poll and reconciliation converges the
	// label for the still-active run.
	tw.linearS.setFailLabelServe(false)
	tw.pollOnce(t)
	assert.Equal(t, []string{"k8s-run", "criteria-automation"},
		tw.linearS.issueLabelsOf("i-1"), "the next poll converges the inflight marker")
}

// TestPollDeferredLabelTransitionRetriedNextPoll pins the CRI-252 deferral
// contract for change-driven reconciliation: when a needed automation-label
// ID is unresolved (the ensure failed this poll), the Failed transition is
// deferred without a write and the ticket's phase stays unrecorded, so the
// next poll — with the phase unchanged — retries it. Recording a deferred
// transition as reconciled would drop it forever. The ticket sits outside
// the route-declared states ("In Review") so only the reconciliation path
// is exercised.
func TestPollDeferredLabelTransitionRetriedNextPoll(t *testing.T) {
	tw := newTestWatcher(t, routesJSON)
	tw.linearS.setIssues(withState(issue("i-1", "CRI-1", gateLabel()), "In Review"))
	liveRun(t, tw, "CRI-1", criteriav1.PhaseFailed)
	tw.linearS.setFailLabelServe(true)

	tw.pollOnce(t)

	assert.Equal(t, 0, tw.linearS.labelUpdateCount(), "the deferred transition is not written")
	assert.NotContains(t, tw.linearS.queryKinds(), "labelWrite")
	_, recorded := tw.w.lastPhases["CRI-1"]
	assert.False(t, recorded, "the deferred transition is not recorded as reconciled")
	assert.True(t, tw.logs.contains("label transition deferred: criteria-dirty id unresolved"))

	// The ensure heals; the next poll retries the unchanged phase's
	// transition and records it.
	tw.linearS.setFailLabelServe(false)
	tw.linearS.resetRequests()
	tw.pollOnce(t)

	assert.Contains(t, tw.linearS.queryKinds(), "ticketBatch",
		"the unchanged phase is re-resolved for the retry")
	assert.Equal(t, 1, tw.linearS.labelUpdateCount())
	assert.Equal(t, []string{"lbl-k8s-run", "lbl-criteria-dirty"}, tw.linearS.lastLabelUpdateIDs())
	assert.Equal(t, []string{"k8s-run", "criteria-dirty"}, tw.linearS.issueLabelsOf("i-1"))
	assert.Equal(t, criteriav1.PhaseFailed, tw.w.lastPhases["CRI-1"])

	// Converged: the following poll neither retries nor re-writes.
	tw.linearS.resetRequests()
	tw.pollOnce(t)
	assert.Equal(t, 1, tw.linearS.labelUpdateCount(), "the retried transition is not re-written")
	assert.Equal(t, []string{"stateListing", "sweepCandidates"}, tw.linearS.queryKinds())
}

// TestPollFiringReplaceKeepsSamePollLabelWrites pins the CRI-252
// REPLACE-safety rule for the firing path: Linear labelIds have REPLACE
// semantics, so the firing write must build on the ticket's current label
// state — reconciliation wrote criteria-dirty earlier in this same poll —
// not on the poll's stale listing read, which would silently drop the dirty
// marker (and with change-driven reconciliation no later poll converges
// it: the phase is already recorded).
func TestPollFiringReplaceKeepsSamePollLabelWrites(t *testing.T) {
	tw := newTestWatcher(t, routesJSON)
	tw.prewarmLabelIDs()
	tw.linearS.setIssues(issue("i-1", "CRI-1", gateLabel()))
	liveRun(t, tw, "CRI-1", criteriav1.PhaseFailed)

	tw.pollOnce(t)

	// Reconciliation performs the Failed transition (dirty raised), and
	// the firing path re-fires a successor run in the same poll; its
	// REPLACE write must keep the dirty marker.
	assert.Equal(t, []string{"k8s-run", "criteria-dirty", "criteria-automation"},
		tw.linearS.issueLabelsOf("i-1"),
		"the firing REPLACE write keeps the dirty marker written earlier in this poll")
	assert.Equal(t, 2, tw.linearS.labelUpdateCount(),
		"one write from reconciliation and one from the firing path")
	assert.Equal(t, []string{"lbl-k8s-run", "lbl-criteria-dirty", "lbl-criteria-automation"},
		tw.linearS.lastLabelUpdateIDs(),
		"the firing write builds on the reconciled label set, not the listing read")
}

// CRI-220: orphaned automation-label sweep.

// TestPollOrphanSweepDirtiesLabelWithoutRuns covers the orphan rule: a
// ticket bearing criteria-automation with no live CriteriaRun in any phase
// and no recorded terminal event gets the marker swapped for criteria-dirty
// on the poll. The ticket sits in a state no route declares ("In
// Progress", where the intake workflow leaves it) — the sweep's dedicated
// candidates query is what reaches it. The sweep is idempotent: the next
// poll writes nothing.
func TestPollOrphanSweepDirtiesLabelWithoutRuns(t *testing.T) {
	t.Run("orphaned marker outside the route-declared states is dirtied", func(t *testing.T) {
		tw := newTestWatcher(t, routesJSON)
		tw.linearS.setIssues(withState(issue("i-1", "CRI-1", gateLabel()), "In Progress"))
		tw.linearS.setIssueLabels("i-1", "k8s-run", automationLabelName)

		tw.pollOnce(t)

		assert.Equal(t, []string{"k8s-run", "criteria-dirty"}, tw.linearS.issueLabelsOf("i-1"),
			"automation marker removed, dirty marker added, unrelated labels intact")
		assert.Equal(t, 1, tw.linearS.labelUpdateCount(),
			"a single REPLACE write performs the removal and the dirty add")
		assert.True(t, tw.logs.contains("sweeping orphaned automation label"))
		assert.True(t, tw.logs.contains("orphan sweep complete"))
		assert.Empty(t, tw.runs(t), "no run exists for the orphaned ticket")

		// Idempotent: with the marker gone, the next poll writes nothing.
		writes := tw.linearS.labelUpdateCount()
		tw.pollOnce(t)
		assert.Equal(t, writes, tw.linearS.labelUpdateCount(),
			"the swept ticket is no longer a candidate on the next poll")
		assert.Equal(t, []string{"k8s-run", "criteria-dirty"}, tw.linearS.issueLabelsOf("i-1"))
	})

	t.Run("orphaned tickets are swept in deterministic identifier order", func(t *testing.T) {
		tw := newTestWatcher(t, routesJSON)
		// Staged in reverse identifier order: the sweep must still act on
		// CRI-1 first, mirroring the operator's stale-adapter sweep.
		tw.linearS.setIssues(
			withState(issue("i-2", "CRI-2", map[string]interface{}{"name": "fast"}), "In Review"),
			withState(issue("i-1", "CRI-1", gateLabel()), "In Progress"))
		tw.linearS.setIssueLabels("i-2", "fast", automationLabelName)
		tw.linearS.setIssueLabels("i-1", "k8s-run", automationLabelName)

		tw.pollOnce(t)

		assert.Equal(t, []string{"k8s-run", "criteria-dirty"}, tw.linearS.issueLabelsOf("i-1"))
		assert.Equal(t, []string{"fast", "criteria-dirty"}, tw.linearS.issueLabelsOf("i-2"))
		assert.Equal(t, 2, tw.linearS.labelUpdateCount(), "one write per orphan")
		assert.Equal(t, []string{"lbl-fast", "lbl-criteria-dirty"}, tw.linearS.lastLabelUpdateIDs(),
			"the last write is CRI-2's removal: CRI-1 was processed first")
	})
}

// TestPollOrphanSweepSparesLiveRuns covers the no-false-positive rule: a
// ticket with criteria-automation and a live CriteriaRun in any phase —
// including Pending and the empty phase a freshly created run carries until
// the controller sets status — is left untouched by the sweep.
func TestPollOrphanSweepSparesLiveRuns(t *testing.T) {
	phases := []criteriav1.CriteriaRunPhase{"", criteriav1.PhasePending, criteriav1.PhaseRunning}
	for _, phase := range phases {
		t.Run("live run in phase "+string(phase), func(t *testing.T) {
			tw := newTestWatcher(t, routesJSON)
			tw.linearS.setIssues(issue("i-1", "CRI-1", gateLabel()))
			tw.pollOnce(t)
			require.Len(t, tw.runs(t), 1)
			assert.Contains(t, tw.linearS.issueLabelsOf("i-1"), automationLabelName)

			tw.setRunPhase(t, "CRI-1", phase)

			writes := tw.linearS.labelUpdateCount()
			tw.pollOnce(t)

			assert.Equal(t, writes, tw.linearS.labelUpdateCount(),
				"the sweep must not touch a ticket with a live run in phase "+string(phase))
			assert.Contains(t, tw.linearS.issueLabelsOf("i-1"), automationLabelName,
				"the automation marker stays on a live ticket")
			assert.NotContains(t, tw.linearS.issueLabelsOf("i-1"), dirtyLabelName,
				"no dirty marker for a live run")
			assert.False(t, tw.logs.contains("sweeping orphaned automation label"))
		})
	}
}

// TestPollOrphanSweepGuardRespectsTerminalEvents covers the terminal-event
// guard: a ticket with criteria-automation, no live CriteriaRun, but a
// recorded terminal event (its CriteriaRun settled Failed or Succeeded) is
// not swept as orphaned — reconciliation owns that transition, so a
// settled ticket converges without gaining a spurious dirty marker on
// success.
func TestPollOrphanSweepGuardRespectsTerminalEvents(t *testing.T) {
	t.Run("succeeded run: reconciled clean, never swept", func(t *testing.T) {
		tw := newTestWatcher(t, routesJSON)
		tw.linearS.setIssues(issue("i-1", "CRI-1", gateLabel()))
		tw.pollOnce(t)
		require.Len(t, tw.runs(t), 1)
		assert.Contains(t, tw.linearS.issueLabelsOf("i-1"), automationLabelName)

		// The intake workflow moves the ticket out of the route-declared
		// states, then the run settles.
		tw.linearS.setIssues(withState(issue("i-1", "CRI-1", gateLabel()), "In Progress"))
		tw.setRunPhase(t, "CRI-1", criteriav1.PhaseSucceeded)

		writes := tw.linearS.labelUpdateCount()
		tw.pollOnce(t)

		assert.Equal(t, []string{"k8s-run"}, tw.linearS.issueLabelsOf("i-1"),
			"the automation marker is removed and no dirty marker is added for a succeeded run")
		assert.Equal(t, 1, tw.linearS.labelUpdateCount()-writes, "exactly one removal write")
		assert.False(t, tw.logs.contains("sweeping orphaned automation label"),
			"a ticket with a recorded terminal event is not swept as orphaned")
	})

	t.Run("failed run: reconciled dirty by reconciliation, not swept", func(t *testing.T) {
		tw := newTestWatcher(t, routesJSON)
		tw.linearS.setIssues(issue("i-1", "CRI-1", gateLabel()))
		tw.pollOnce(t)
		require.Len(t, tw.runs(t), 1)
		assert.Contains(t, tw.linearS.issueLabelsOf("i-1"), automationLabelName)

		tw.linearS.setIssues(withState(issue("i-1", "CRI-1", gateLabel()), "In Review"))
		tw.setRunPhase(t, "CRI-1", criteriav1.PhaseFailed)

		writes := tw.linearS.labelUpdateCount()
		tw.pollOnce(t)

		assert.Equal(t, []string{"k8s-run", "criteria-dirty"}, tw.linearS.issueLabelsOf("i-1"))
		assert.Equal(t, 1, tw.linearS.labelUpdateCount()-writes,
			"a single REPLACE write performs the removal and the dirty add")
		assert.False(t, tw.logs.contains("sweeping orphaned automation label"),
			"a ticket with a recorded terminal event is not swept as orphaned")
	})
}

// TestPollDeletedWhileRunningRunDirtiesTicket covers edge case (a): a
// CriteriaRun deleted manually while its ticket is running leaves the
// automation marker orphaned with no live run and no terminal event; the
// next poll detects the orphan and applies the dirty treatment, and no
// successor run fires for the ticket outside the route-declared states.
func TestPollDeletedWhileRunningRunDirtiesTicket(t *testing.T) {
	tw := newTestWatcher(t, routesJSON)
	tw.linearS.setIssues(issue("i-1", "CRI-1", gateLabel()))
	tw.pollOnce(t)
	require.Len(t, tw.runs(t), 1)
	assert.Contains(t, tw.linearS.issueLabelsOf("i-1"), automationLabelName)

	// The CriteriaRun is deleted manually while running (the CRI-223-style
	// case) and the intake workflow has moved the ticket out of the
	// route-declared states.
	runs := tw.runs(t)
	require.NoError(t, tw.client.Delete(context.Background(), &runs[0]))
	tw.linearS.setIssues(withState(issue("i-1", "CRI-1", gateLabel()), "In Progress"))

	writes := tw.linearS.labelUpdateCount()
	tw.pollOnce(t)

	assert.Empty(t, tw.runs(t), "no successor run fires for the deleted run's ticket")
	assert.Equal(t, []string{"k8s-run", "criteria-dirty"}, tw.linearS.issueLabelsOf("i-1"),
		"the deleted run's ticket receives the dirty treatment on the next poll")
	assert.Equal(t, 1, tw.linearS.labelUpdateCount()-writes)
	assert.True(t, tw.logs.contains("sweeping orphaned automation label"))
}

// TestPollOrphanSweepAfterRestart covers edge case (b): an automation
// marker persisting across a watcher restart with no live CriteriaRun and
// no terminal event is still treated as orphaned by the fresh watcher —
// the sweep decides from observed state only, never remembered history.
func TestPollOrphanSweepAfterRestart(t *testing.T) {
	tw := newTestWatcher(t, routesJSON)
	tw.linearS.setIssues(issue("i-1", "CRI-1", gateLabel()))
	tw.pollOnce(t)
	require.Len(t, tw.runs(t), 1)

	// The CriteriaRun is deleted and the ticket moved out of the
	// route-declared states while the watcher is down; the automation
	// marker persists on the ticket.
	runs := tw.runs(t)
	require.NoError(t, tw.client.Delete(context.Background(), &runs[0]))
	tw.linearS.setIssues(withState(issue("i-1", "CRI-1", gateLabel()), "In Progress"))

	tw2 := tw.restarted()
	writes := tw2.linearS.labelUpdateCount()
	tw2.pollOnce(t)

	assert.Equal(t, []string{"k8s-run", "criteria-dirty"}, tw2.linearS.issueLabelsOf("i-1"),
		"the fresh watcher sweeps the persisted orphan")
	assert.Equal(t, 1, tw2.linearS.labelUpdateCount()-writes)
	assert.True(t, tw2.logs.contains("sweeping orphaned automation label"))
}

// TestReArmAfterDirtyKeepsDirtyLabel covers edge case (c): criteria-dirty
// is sticky. A ticket re-armed after the dirty marker was applied fires a
// fresh run and regains the automation marker, but the dirty marker
// survives the sweep, the run, and reconciliation — only a cleanup
// workflow or a human removes it.
func TestReArmAfterDirtyKeepsDirtyLabel(t *testing.T) {
	t.Run("dirty marker survives a re-arm and its run", func(t *testing.T) {
		tw := newTestWatcher(t, routesJSON)
		// An orphaned automation marker in a watched state (no trigger
		// label): the first poll dirties it.
		tw.linearS.setIssues(issue("i-1", "CRI-1"))
		tw.linearS.setIssueLabels("i-1", automationLabelName)
		tw.pollOnce(t)
		assert.Equal(t, []string{"criteria-dirty"}, tw.linearS.issueLabelsOf("i-1"))

		// Re-armed AFTER the dirty marker: a human (or workflow) adds the
		// trigger label back.
		tw.linearS.setIssueLabels("i-1", "k8s-run", "criteria-dirty")
		tw.pollOnce(t)

		assert.Equal(t, []string{"k8s-run", "criteria-dirty", automationLabelName},
			tw.linearS.issueLabelsOf("i-1"),
			"the re-armed ticket fires a run and regains the automation marker, dirty intact")
		require.Len(t, tw.runs(t), 1)

		// The run settles; reconciliation clears the automation marker but
		// the dirty marker persists.
		tw.setRunPhase(t, "CRI-1", criteriav1.PhaseSucceeded)
		tw.linearS.setIssues(withState(issue("i-1", "CRI-1", gateLabel()), "Done"))
		tw.pollOnce(t)

		assert.Equal(t, []string{"k8s-run", "criteria-dirty"}, tw.linearS.issueLabelsOf("i-1"),
			"criteria-dirty survives the sweep after the re-arm; only a cleanup workflow or human removes it")
	})

	t.Run("dirty-only ticket is never a sweep candidate", func(t *testing.T) {
		tw := newTestWatcher(t, routesJSON)
		tw.linearS.setIssues(withState(issue("i-1", "CRI-1"), "In Progress"))
		tw.linearS.setIssueLabels("i-1", dirtyLabelName)

		writes := tw.linearS.labelUpdateCount()
		tw.pollOnce(t)

		assert.Equal(t, writes, tw.linearS.labelUpdateCount(),
			"a ticket without the automation marker is never a sweep candidate")
		assert.Equal(t, []string{"criteria-dirty"}, tw.linearS.issueLabelsOf("i-1"),
			"the dirty marker is never removed")
	})
}

// TestPollOrphanSweepDeferredWhenLabelEnsureFails covers the degraded
// Linear path: the sweep is an atomic label-state transition and candidates
// are only visible through the automation marker, so with unresolved label
// ids it defers entirely — no partial removal that would lose the orphan
// evidence. The next healthy poll converges the ticket.
func TestPollOrphanSweepDeferredWhenLabelEnsureFails(t *testing.T) {
	tw := newTestWatcher(t, routesJSON)
	tw.linearS.setIssues(withState(issue("i-1", "CRI-1"), "In Progress"))
	tw.linearS.setIssueLabels("i-1", automationLabelName)
	tw.linearS.setFailLabelServe(true)

	writes := tw.linearS.labelUpdateCount()
	tw.pollOnce(t)

	assert.Equal(t, writes, tw.linearS.labelUpdateCount(),
		"no label write with unresolved label ids")
	assert.Contains(t, tw.linearS.issueLabelsOf("i-1"), automationLabelName,
		"the orphan evidence is preserved for the next poll")
	assert.True(t, tw.logs.contains("skipping orphan sweep"))

	// The ensure heals on the next poll and the sweep converges the ticket.
	tw.linearS.setFailLabelServe(false)
	tw.pollOnce(t)
	assert.Equal(t, []string{"criteria-dirty"}, tw.linearS.issueLabelsOf("i-1"))
}

// TestPollOrphanSweepDeferredWhenCandidatesQueryFails covers the sweep's
// own failure path: when the candidates query fails, the poll does not fail
// and no label is written — the sweep defers to the next poll, preserving
// the orphan evidence.
func TestPollOrphanSweepDeferredWhenCandidatesQueryFails(t *testing.T) {
	tw := newTestWatcher(t, routesJSON)
	tw.linearS.setIssues(withState(issue("i-1", "CRI-1"), "In Progress"))
	tw.linearS.setIssueLabels("i-1", automationLabelName)
	tw.linearS.setFailCandidates(true)

	writes := tw.linearS.labelUpdateCount()
	tw.pollOnce(t)

	assert.Equal(t, writes, tw.linearS.labelUpdateCount(),
		"no label write when the candidates query fails")
	assert.Contains(t, tw.linearS.issueLabelsOf("i-1"), automationLabelName,
		"the orphan evidence is preserved for the next poll")
	assert.True(t, tw.logs.contains("orphan sweep failed; deferred"),
		"the deferral is logged and the poll continues")

	// The query heals on the next poll and the sweep converges the ticket.
	tw.linearS.setFailCandidates(false)
	tw.pollOnce(t)
	assert.Equal(t, []string{"criteria-dirty"}, tw.linearS.issueLabelsOf("i-1"))
}

// TestPollSkipsWhenLiveRunMatchesSelector pins the watcher's existing
// pre-create gate (CRI-221 exit criterion): a live CriteriaRun matching the
// watcher's source selector blocks a second run for the ticket.
func TestPollSkipsWhenLiveRunMatchesSelector(t *testing.T) {
	tw := newTestWatcher(t, routesJSON)
	tw.linearS.setIssues(issue("i-1", "CRI-1", gateLabel()))

	tw.pollOnce(t)
	require.Len(t, tw.runs(t), 1)

	tw.pollOnce(t)

	assert.Len(t, tw.runs(t), 1, "a live run blocks a second run for the ticket")
	assert.True(t, tw.logs.contains("run already active"))
}

// TestPollSkipsWhenAutomationLabelPresentWithoutMatchingRun covers the
// CRI-221 skip side: the automation label gates firing even when no
// CriteriaRun matches the watcher's source selector. A live run created
// outside the selector convention (a legacy run carrying a different label
// key) is invisible to the runs index, and the label is the only other
// witness the invariant has; the operator admission assert (CRI-221) is the
// enforcement backstop for what this gate cannot see.
func TestPollSkipsWhenAutomationLabelPresentWithoutMatchingRun(t *testing.T) {
	tw := newTestWatcher(t, routesJSON)
	tw.linearS.setIssues(issue("i-1", "CRI-1", gateLabel(),
		map[string]interface{}{"name": automationLabelName}))
	// A legacy live run for the same ticket, created outside the selector
	// convention: no criteria source label, so the runs index cannot see it.
	legacy := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cri-1-legacy",
			Namespace: "criteria-jobs",
			Labels:    map[string]string{"ticket": "cri-1"},
		},
		Spec:   criteriav1.CriteriaRunSpec{TicketID: "CRI-1", RepoURL: "https://github.com/brokenbots/workflow-example"},
		Status: criteriav1.CriteriaRunStatus{Phase: criteriav1.PhaseRunning},
	}
	require.NoError(t, tw.client.Create(context.Background(), legacy))

	tw.pollOnce(t)

	runs := tw.runs(t)
	require.Len(t, runs, 1, "the automation label gates firing although no run matches the source selector")
	assert.Equal(t, "cri-1-legacy", runs[0].Name, "only the legacy run exists; no watcher run was created")
	assert.True(t, tw.logs.contains("single-active invariant"),
		"the skip is logged with the invariant reason")
}

// CRI-252: Linear API consumer discipline.

// prewarmLabelIDs seeds the watcher's cached automation label ids, so a
// poll issues no ensure queries at all (the steady-state request budget is
// exactly the two listing queries).
func (tw *testWatcher) prewarmLabelIDs() {
	tw.w.automationLabelID = "lbl-" + automationLabelName
	tw.w.dirtyLabelID = "lbl-" + dirtyLabelName
}

// liveRun builds a CriteriaRun in the given phase that matches the
// watcher's source selector, without the watcher creating it.
func liveRun(t *testing.T, tw *testWatcher, ticket string, phase criteriav1.CriteriaRunPhase) {
	t.Helper()
	run := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      strings.ToLower(ticket) + "-live",
			Namespace: tw.w.namespace,
			Labels: map[string]string{
				"ticket":                         strings.ToLower(ticket),
				"app.kubernetes.io/managed-by":   "criteria-linear-watcher",
				"criteria.brokenbots.dev/source": "linear",
			},
		},
		Spec:   criteriav1.CriteriaRunSpec{TicketID: ticket, RepoURL: "https://github.com/brokenbots/workflow-example"},
		Status: criteriav1.CriteriaRunStatus{Phase: phase},
	}
	require.NoError(t, tw.client.Create(context.Background(), run))
}

// TestPollSteadyStateZeroRequests pins the CRI-252 exit criterion: a poll
// in which no k8s CR phase changed issues exactly two Linear requests (the
// states listing and the orphan-sweep candidates query), ZERO label reads,
// ZERO label writes and ZERO constant lookups. A phase change triggers one
// extra batched read and the required write — with no pre-write label read,
// since the batched read already carries the label ids — and the next
// unchanged poll is frugal again.
func TestPollSteadyStateZeroRequests(t *testing.T) {
	tw := newTestWatcher(t, routesJSON)
	tw.prewarmLabelIDs()
	// A running run for CRI-1 whose labels are already converged.
	tw.linearS.setIssues(issue("i-1", "CRI-1", gateLabel(),
		map[string]interface{}{"name": automationLabelName}))
	liveRun(t, tw, "CRI-1", criteriav1.PhaseRunning)

	// First sight: the fresh watcher reconciles from the batched read
	// (labels already correct, so no write) and records the phase.
	tw.pollOnce(t)
	assert.Equal(t, []string{"stateListing", "ticketBatch", "sweepCandidates"},
		tw.linearS.queryKinds(), "first poll: the two listing queries plus one batched ticket read")
	assert.Equal(t, 0, tw.linearS.labelUpdateCount())
	assert.Len(t, tw.runs(t), 1)

	// Steady state: nothing changed — exactly the two listing queries.
	tw.linearS.resetRequests()
	tw.pollOnce(t)
	assert.Equal(t, []string{"stateListing", "sweepCandidates"},
		tw.linearS.queryKinds(),
		"no-change poll: only the states and candidates queries, no ticket reads, no label reads")
	assert.Equal(t, 0, tw.linearS.labelUpdateCount(), "no-change poll: zero label writes")

	// The run succeeds: exactly one phase-change poll performs the
	// reconciliation write. The intake workflow clears the trigger label as
	// the run settles (as in TestPollSucceededRemovesAutomationNoDirty), so
	// the ticket does not re-fire.
	tw.setRunPhase(t, "CRI-1", criteriav1.PhaseSucceeded)
	tw.linearS.setIssueLabels("i-1", automationLabelName)
	tw.linearS.resetRequests()
	tw.pollOnce(t)
	assert.Equal(t, []string{"stateListing", "ticketBatch", "labelWrite", "sweepCandidates"},
		tw.linearS.queryKinds(),
		"change poll: one batched read and one write with NO label read in between (write dedupe)")
	assert.Equal(t, 1, tw.linearS.labelUpdateCount(), "change poll: exactly one label write")
	assert.Empty(t, tw.linearS.lastLabelUpdateIDs(),
		"the write payload comes from the batched read: no pre-write label read")

	// Converged again: back to the two-query budget with zero writes.
	tw.linearS.resetRequests()
	writes := tw.linearS.labelUpdateCount()
	tw.pollOnce(t)
	assert.Equal(t, []string{"stateListing", "sweepCandidates"},
		tw.linearS.queryKinds(), "post-change steady state: back to the two-query budget")
	assert.Equal(t, writes, tw.linearS.labelUpdateCount())
}

// TestPollRateLimitedSkipsAndBacksOff pins the CRI-252 rate-limit handling
// at the poll boundary: a rate-limited query (HTTP 429 or a GraphQL
// RATELIMITED error) aborts the poll with an IsRateLimit error before any
// run is created, and Linear's rateLimitResult.duration is surfaced on the
// error for the back-off loop to honor.
func TestPollRateLimitedSkipsAndBacksOff(t *testing.T) {
	t.Run("HTTP 429 skips the poll and creates no run", func(t *testing.T) {
		tw := newTestWatcher(t, routesJSON)
		tw.prewarmLabelIDs()
		tw.linearS.setIssues(issue("i-1", "CRI-1", gateLabel()))
		tw.linearS.setRateLimited(true, "7", 0)

		err := tw.w.poll(context.Background(), "proj-1")

		require.Error(t, err, "the rate-limited poll must surface the failure")
		assert.True(t, linear.IsRateLimit(err), "the poll error is a rate-limit error")
		assert.Empty(t, tw.runs(t), "a rate-limited poll creates no run")
		assert.Equal(t, 1, tw.linearS.requestCount(),
			"the poll stops at the first rate-limited query instead of looping")
	})

	t.Run("GraphQL RATELIMITED error carries Linear's duration", func(t *testing.T) {
		tw := newTestWatcher(t, routesJSON)
		tw.prewarmLabelIDs()
		tw.linearS.setIssues(issue("i-1", "CRI-1", gateLabel()))
		tw.linearS.setRateLimited(false, "", 40000)

		err := tw.w.poll(context.Background(), "proj-1")

		require.Error(t, err)
		var rl *linear.RateLimitError
		require.ErrorAs(t, err, &rl)
		assert.Equal(t, 40*time.Second, rl.Duration,
			"Linear's rateLimitResult.duration (milliseconds) is surfaced as a duration")
		assert.Empty(t, tw.runs(t))
		assert.Equal(t, 1, tw.linearS.requestCount())
	})

	t.Run("after the rate limit clears the poll fires normally", func(t *testing.T) {
		tw := newTestWatcher(t, routesJSON)
		tw.prewarmLabelIDs()
		tw.linearS.setIssues(issue("i-1", "CRI-1", gateLabel()))
		tw.linearS.setRateLimited(true, "", 0)
		require.Error(t, tw.w.poll(context.Background(), "proj-1"))

		tw.linearS.clearRateLimited()
		tw.pollOnce(t)

		require.Len(t, tw.runs(t), 1, "the healed poll proceeds normally")
		assert.Contains(t, tw.linearS.issueLabelsOf("i-1"), automationLabelName)
	})
}

// TestRunRateLimitBackoffLoop pins the CRI-252 back-off loop: while Linear
// rate-limits, the watcher skips polls and backs the interval off
// exponentially (logging a single V(0) rate-limit event per back-off
// window, with V(1) continuations, and never logging "poll failed"); when
// the rate limit clears, the next poll succeeds, the interval returns to
// normal, and the watcher resumes.
func TestRunRateLimitBackoffLoop(t *testing.T) {
	tw := newTestWatcher(t, routesJSON)
	tw.linearS.setIssues(issue("i-1", "CRI-1", gateLabel()))
	tw.w.pollInterval = 20 * time.Millisecond
	tw.w.teamID = "team-1"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- tw.w.run(ctx) }()

	// Let the healthy startup and first polls pass, then rate-limit.
	time.Sleep(60 * time.Millisecond)
	tw.linearS.setRateLimited(true, "", 0)
	// Back off for a while (several polls at a doubling interval).
	time.Sleep(100 * time.Millisecond)
	tw.linearS.clearRateLimited()
	// Let the healed poll run and the interval be restored.
	time.Sleep(250 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not exit after context cancel")
	}

	assert.Equal(t, 1, tw.logs.infoCount("linear rate limited"),
		"a single rate-limit event is logged per back-off window")
	assert.True(t, tw.logs.v1InfoCount("linear rate limited") >= 1,
		"back-off continuations are logged at V(1), not as new rate-limit events")
	assert.False(t, tw.logs.contains("poll failed"),
		"rate-limited polls are skipped, not reported as poll failures")
	require.Len(t, tw.runs(t), 1, "the watcher resumed and fired the staged ticket")
	assert.True(t, tw.logs.contains("poll interval restored"),
		"a successful poll after back-off returns the interval to normal")
}

// TestRunBackoffCadenceHonorsAdvertisedDuration pins the loop cadence, not
// just the logs (CRI-252): the wait that follows a rate-limited poll must
// honor Linear's advertised rateLimitResult.duration — the next request
// comes after the advertised window, not one tick of the pre-back-off
// interval later — and the cadence after a successful poll must be back at
// the normal interval rather than the backed-off one.
func TestRunBackoffCadenceHonorsAdvertisedDuration(t *testing.T) {
	tw := newTestWatcher(t, routesJSON)
	tw.linearS.setIssues(issue("i-1", "CRI-1", gateLabel()))
	tw.w.pollInterval = 20 * time.Millisecond
	tw.w.teamID = "team-1"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- tw.w.run(ctx) }()

	// Establish the healthy 20ms cadence, then advertise a 150ms rate-limit
	// window (>= 3x the base interval) via the GraphQL RATELIMITED shape —
	// the response shape that carries rateLimitResult.duration. With the
	// interval applied before the wait, the first post-429 request lands
	// ~150ms later; applying it after the wait (the defect this pins) would
	// put it ~20ms later.
	time.Sleep(80 * time.Millisecond)
	tw.linearS.setRateLimited(false, "", 150)
	time.Sleep(300 * time.Millisecond)
	tw.linearS.clearRateLimited()
	tClear := time.Now()
	time.Sleep(320 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not exit after context cancel")
	}

	times := tw.linearS.requestTimes()
	rejected := tw.linearS.rejectedTimes()
	require.NotEmpty(t, rejected, "the injected rate limit was served at least once")

	// The wait immediately after the first rate-limited poll: the next
	// Linear request must come no earlier than the advertised 150ms window
	// (with scheduling slack). Backed-off attempt 2 waits even longer, so a
	// later first rejection still satisfies this.
	first := rejected[0]
	var next time.Time
	for _, ts := range times {
		if ts.After(first) {
			next = ts
			break
		}
	}
	require.False(t, next.IsZero(), "a request follows the rate-limited poll before cancel")
	assert.GreaterOrEqual(t, next.Sub(first), 120*time.Millisecond,
		"the next Linear request waits out the advertised rateLimitResult.duration, not the pre-back-off interval")

	// After the limit clears, the watcher must be back on the ~20ms
	// cadence: several healthy requests are served post-clear, and the
	// smallest trailing inter-request gap is the normal interval, not the
	// backed-off 300ms. (The minimum is robust to a single scheduler stall.)
	rejectedSet := make(map[time.Time]bool, len(rejected))
	for _, ts := range rejected {
		rejectedSet[ts] = true
	}
	var postClear []time.Time
	for _, ts := range times {
		if ts.After(tClear) && !rejectedSet[ts] {
			postClear = append(postClear, ts)
		}
	}
	require.GreaterOrEqual(t, len(postClear), 3,
		"the restored cadence serves several healthy polls after the limit clears")
	minGap := time.Duration(1 << 62)
	for i := 1; i < len(postClear); i++ {
		if g := postClear[i].Sub(postClear[i-1]); g < minGap {
			minGap = g
		}
	}
	assert.Less(t, minGap, 5*tw.w.pollInterval,
		"the post-recovery cadence is the normal poll interval, not the backed-off interval")
}

// TestNextBackoffInterval pins the back-off math: the interval doubles per
// skipped poll up to the cap, Linear's advertised duration wins when
// provided (even beyond the cap), and the rate-limit error alone does not
// mutate the watcher state.
func TestNextBackoffInterval(t *testing.T) {
	t.Run("doubles the current interval", func(t *testing.T) {
		w := &watcher{log: logr.New(&logRecorder{})}
		assert.Equal(t, 2*time.Minute, w.nextBackoffInterval(time.Minute, &linear.RateLimitError{}))
		assert.Equal(t, 1, w.backoffAttempt)
	})
	t.Run("compounds over repeated skips", func(t *testing.T) {
		w := &watcher{log: logr.New(&logRecorder{})}
		next := w.nextBackoffInterval(time.Minute, &linear.RateLimitError{})
		assert.Equal(t, 2*time.Minute, next)
		next = w.nextBackoffInterval(next, &linear.RateLimitError{})
		assert.Equal(t, 4*time.Minute, next)
		next = w.nextBackoffInterval(next, &linear.RateLimitError{})
		assert.Equal(t, 8*time.Minute, next,
			"the caller feeds the returned interval back in")
	})
	t.Run("caps at the configured cap", func(t *testing.T) {
		w := &watcher{log: logr.New(&logRecorder{})}
		assert.Equal(t, backoffCap, w.nextBackoffInterval(20*time.Minute, &linear.RateLimitError{}))
		assert.Equal(t, backoffCap, w.nextBackoffInterval(backoffCap, &linear.RateLimitError{}))
		assert.Equal(t, backoffCap, w.nextBackoffInterval(45*time.Minute, &linear.RateLimitError{}))
	})
	t.Run("Linear's duration wins over the computed next", func(t *testing.T) {
		w := &watcher{log: logr.New(&logRecorder{})}
		rl := &linear.RateLimitError{Duration: 17 * time.Minute}
		assert.Equal(t, 17*time.Minute, w.nextBackoffInterval(8*time.Minute, rl),
			"never back off less than Linear's advertised duration")
	})
	t.Run("Linear's duration wins even beyond the cap", func(t *testing.T) {
		w := &watcher{log: logr.New(&logRecorder{})}
		rl := &linear.RateLimitError{Duration: 45 * time.Minute}
		assert.Equal(t, 45*time.Minute, w.nextBackoffInterval(backoffCap, rl))
	})
}

// TestDefaultPollIntervalIsFiveMinutes pins the CRI-252 default poll
// interval of 5m and its env override: the flag default parses
// POLL_INTERVAL when set (falling back to 5m when absent or unparseable).
func TestDefaultPollIntervalIsFiveMinutes(t *testing.T) {
	assert.Equal(t, 5*time.Minute, defaultPollInterval)
	assert.Equal(t, 5*time.Minute, *pollInterval,
		"the built-in flag default is 5m (tests run without POLL_INTERVAL set)")
	assert.Equal(t, 5*time.Minute, parseDuration("bogus"),
		"an unparseable POLL_INTERVAL falls back to the 5m default")
	assert.Equal(t, 90*time.Second, parseDuration("90s"))

	t.Setenv("POLL_INTERVAL", "90s")
	assert.Equal(t, 90*time.Second, parseDuration(getenv("POLL_INTERVAL", "5m")),
		"POLL_INTERVAL overrides the default")
}
