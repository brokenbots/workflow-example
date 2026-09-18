package linear_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/brokenbots/workflow-example/criteria-k8s/internal/linear"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newLinearTestServer returns a server answering the project lookup, the
// issues query (filtered by the queried `states` variable exactly like
// Linear's `state: {name: {in: $states}}` filter) and the CRI-220
// label-candidates query (filtered by the issue's current label names).
func newLinearTestServer(t *testing.T, issues []map[string]interface{}, queries *[]string) *httptest.Server {
	t.Helper()
	issueLabelNames := func(issue map[string]interface{}) []string {
		names := []string{}
		if labelsObj, ok := issue["labels"].(map[string]interface{}); ok {
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
		return names
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string `json:"query"`
			Variables struct {
				States []string `json:"states"`
				Label  string   `json:"label"`
			} `json:"variables"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		if queries != nil {
			*queries = append(*queries, req.Query)
		}

		resp := map[string]interface{}{}
		if strings.Contains(req.Query, "projects") {
			resp["data"] = map[string]interface{}{
				"projects": map[string]interface{}{
					"nodes": []interface{}{
						map[string]interface{}{"id": "proj-1", "name": "Criteria K8s Workflow Runner"},
					},
				},
			}
		} else if strings.Contains(req.Query, "labels: {some:") {
			matched := []interface{}{}
			for _, issue := range issues {
				if slices.Contains(issueLabelNames(issue), req.Variables.Label) {
					matched = append(matched, issue)
				}
			}
			resp["data"] = map[string]interface{}{"issues": map[string]interface{}{"nodes": matched}}
		} else if strings.Contains(req.Query, "issues") {
			matched := []interface{}{}
			for _, issue := range issues {
				stateObj, _ := issue["state"].(map[string]interface{})
				state, _ := stateObj["name"].(string)
				if slices.Contains(req.Variables.States, state) {
					matched = append(matched, issue)
				}
			}
			resp["data"] = map[string]interface{}{"issues": map[string]interface{}{"nodes": matched}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

func TestFindTicketsInStates(t *testing.T) {
	issues := []map[string]interface{}{
		{
			"id":          "issue-1",
			"identifier":  "CRI-99",
			"title":       "Test ticket",
			"description": "Repo: https://github.com/brokenbots/workflow-example/issues/1",
			"state":       map[string]interface{}{"name": "Triage"},
			"project":     map[string]interface{}{"id": "proj-1", "name": "Criteria K8s Workflow Runner"},
		},
		{
			"id":          "issue-2",
			"identifier":  "CRI-100",
			"title":       "In progress ticket",
			"description": "Just text",
			"state":       map[string]interface{}{"name": "In Progress"},
			"project":     map[string]interface{}{"id": "proj-1", "name": "Criteria K8s Workflow Runner"},
		},
		// A ticket whose state Linear cannot resolve: the states filter can
		// never match it (CRI-218 fail-closed).
		{
			"id":         "issue-3",
			"identifier": "CRI-101",
			"title":      "Stateless ticket",
			"state":      nil,
			"project":    map[string]interface{}{"id": "proj-1", "name": "Criteria K8s Workflow Runner"},
		},
	}
	ts := newLinearTestServer(t, issues, nil)
	defer ts.Close()

	client := linear.NewClientWithBaseURL(ts.URL, "test-token")
	tickets, err := client.FindTicketsInStates(context.Background(), "Criteria K8s Workflow Runner", []string{"Triage", "In Progress"})
	require.NoError(t, err)
	require.Len(t, tickets, 2)
	assert.Equal(t, "CRI-99", tickets[0].Identifier)
	assert.Equal(t, "CRI-100", tickets[1].Identifier)

	// A narrower list must only surface the states it names.
	tickets, err = client.FindTicketsInStates(context.Background(), "Criteria K8s Workflow Runner", []string{"In Progress"})
	require.NoError(t, err)
	require.Len(t, tickets, 1)
	assert.Equal(t, "CRI-100", tickets[0].Identifier)
}

// The issues query must filter by the states list with the `in` comparator:
// a regression to `eq` would make the watcher poll a single state and miss
// every other declared state (CRI-218).
func TestIssuesQueryUsesInComparator(t *testing.T) {
	var queries []string
	ts := newLinearTestServer(t, nil, &queries)
	defer ts.Close()

	client := linear.NewClientWithBaseURL(ts.URL, "test-token")
	_, err := client.FindTicketsInStates(context.Background(), "Criteria K8s Workflow Runner", []string{"Triage", "In Progress"})
	require.NoError(t, err)
	require.NotEmpty(t, queries)

	issuesQuery := ""
	for _, q := range queries {
		if strings.Contains(q, "issues(") {
			issuesQuery = q
			break
		}
	}
	require.NotEmpty(t, issuesQuery, "an issues query was issued")

	compact := strings.Join(strings.Fields(issuesQuery), "")
	assert.Contains(t, compact, "state:{name:{in:$states}}",
		"issues query must filter the state name with the in comparator, got: %s", issuesQuery)
	assert.NotContains(t, compact, "state:{name:{eq:",
		"issues query must not filter a single state name, got: %s", issuesQuery)
}

// Fail closed: with no states to query, the client must not send an issues
// query at all and must report no tickets.
func TestFindTicketsInStatesWithoutStatesQueriesNothing(t *testing.T) {
	var queries []string
	ts := newLinearTestServer(t, nil, &queries)
	defer ts.Close()

	client := linear.NewClientWithBaseURL(ts.URL, "test-token")
	tickets, err := client.IssuesInProjectStates(context.Background(), "proj-1", nil)
	require.NoError(t, err)
	require.Empty(t, tickets)

	tickets, err = client.FindTicketsInStates(context.Background(), "Criteria K8s Workflow Runner", []string{})
	require.NoError(t, err)
	require.Empty(t, tickets)

	for _, q := range queries {
		assert.NotContains(t, q, "issues", "issues query must not be sent without states")
	}
}

// TestIssuesWithLabel covers the CRI-220 orphan-sweep candidates query: it
// must return every project issue carrying the named label regardless of
// workflow state (orphans usually sit on tickets the route-declared states
// do not cover), filter with `labels: {some: {name: {eq: $label}}}` — not
// client-side — and query nothing for an empty label name.
func TestIssuesWithLabel(t *testing.T) {
	issues := []map[string]interface{}{
		{
			"id":         "issue-1",
			"identifier": "CRI-1",
			"state":      map[string]interface{}{"name": "In Progress"},
			"project":    map[string]interface{}{"id": "proj-1", "name": "Criteria K8s Workflow Runner"},
			"labels":     map[string]interface{}{"nodes": []interface{}{map[string]interface{}{"name": "criteria-automation"}}},
		},
		{
			"id":         "issue-2",
			"identifier": "CRI-2",
			"state":      map[string]interface{}{"name": "Triage"},
			"project":    map[string]interface{}{"id": "proj-1", "name": "Criteria K8s Workflow Runner"},
			"labels":     map[string]interface{}{"nodes": []interface{}{map[string]interface{}{"name": "k8s-run"}}},
		},
		// A stateless ticket must still match: the label filter ignores
		// workflow state entirely.
		{
			"id":         "issue-3",
			"identifier": "CRI-3",
			"state":      nil,
			"project":    map[string]interface{}{"id": "proj-1", "name": "Criteria K8s Workflow Runner"},
			"labels":     map[string]interface{}{"nodes": []interface{}{map[string]interface{}{"name": "criteria-automation"}}},
		},
	}
	var queries []string
	ts := newLinearTestServer(t, issues, &queries)
	defer ts.Close()

	client := linear.NewClientWithBaseURL(ts.URL, "test-token")
	got, err := client.IssuesWithLabel(context.Background(), "proj-1", "criteria-automation")
	require.NoError(t, err)
	require.Len(t, got, 2, "both labeled tickets match, regardless of state")
	assert.Equal(t, "CRI-1", got[0].Identifier)
	assert.Equal(t, []string{"criteria-automation"}, got[0].Labels)
	assert.Equal(t, "CRI-3", got[1].Identifier)
	assert.Equal(t, []string{"criteria-automation"}, got[1].Labels)

	var candidatesQuery string
	for _, q := range queries {
		if strings.Contains(q, "labels: {some:") {
			candidatesQuery = q
		}
	}
	require.NotEmpty(t, candidatesQuery, "a label-candidates query was issued")

	compact := strings.Join(strings.Fields(candidatesQuery), "")
	assert.Contains(t, compact, "project:{id:{eq:$project}}",
		"candidates query must scope to the project, got: %s", candidatesQuery)
	assert.Contains(t, compact, "labels:{some:{name:{eq:$label}}}",
		"candidates query must filter the label server-side, got: %s", candidatesQuery)

	// Fail closed: an empty label name queries nothing and reports no
	// issues.
	before := len(queries)
	got, err = client.IssuesWithLabel(context.Background(), "proj-1", "")
	require.NoError(t, err)
	assert.Empty(t, got)
	assert.Len(t, queries, before, "no query is sent for an empty label name")
}

func TestExtractRepoURL(t *testing.T) {
	issue := linear.Issue{
		Title:       "Fix bug in brokenbots/workflow-example",
		Description: "Details at https://github.com/brokenbots/workflow-example/pull/12",
	}
	assert.Equal(t, "brokenbots/workflow-example", linear.ExtractRepoURL(issue, "", nil))
}

func TestDefaultRepoValidator(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/repos/brokenbots/workflow-example", r.URL.Path)
		assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	client := ts.Client()
	validator := linear.DefaultRepoValidator(client, "test-token", ts.URL)
	assert.True(t, validator("brokenbots/workflow-example"))

	notFoundTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer notFoundTS.Close()

	missing := linear.DefaultRepoValidator(notFoundTS.Client(), "test-token", notFoundTS.URL)
	assert.False(t, missing("owner/does-not-exist"))
}

func TestExtractRepoURLRepro(t *testing.T) {
	// mockValidator confirms only brokenbots/workflow-example is a real repo.
	mockValidator := func(repo string) bool {
		return repo == "brokenbots/workflow-example"
	}
	defaultRepoURL := "brokenbots/default-fallback"

	cases := []struct {
		name        string
		issue       linear.Issue
		wantRepoURL string
	}{
		{
			name:        "config path false positive",
			issue:       linear.Issue{Title: "Update k8s/service.yaml"},
			wantRepoURL: defaultRepoURL,
		},
		{
			name:        "file path false positive",
			issue:       linear.Issue{Title: "Refactor cmd/main.go"},
			wantRepoURL: defaultRepoURL,
		},
		{
			name:        "nested path false positive",
			issue:       linear.Issue{Title: "Issue in foo/bar/baz"},
			wantRepoURL: defaultRepoURL,
		},
		{
			name:        "unvalidated short-form false positive",
			issue:       linear.Issue{Title: "Fix fake/not-a-repo"},
			wantRepoURL: defaultRepoURL,
		},
		{
			name:        "explicit GitHub URL wins",
			issue:       linear.Issue{Title: "Update k8s/service.yaml", Description: "See https://github.com/brokenbots/workflow-example"},
			wantRepoURL: "brokenbots/workflow-example",
		},
		{
			name:        "validated short-form wins",
			issue:       linear.Issue{Title: "Fix brokenbots/workflow-example"},
			wantRepoURL: "brokenbots/workflow-example",
		},
		{
			name:        "plain text falls back to defaultRepoURL",
			issue:       linear.Issue{Title: "Just a regular issue"},
			wantRepoURL: defaultRepoURL,
		},
		{
			name:        "repo label falls back before defaultRepoURL",
			issue:       linear.Issue{Title: "Update k8s/service.yaml", RepoLabel: "brokenbots/labelled-repo"},
			wantRepoURL: "brokenbots/labelled-repo",
		},
		{
			name:        "validated short-form wins over repo label",
			issue:       linear.Issue{Title: "Fix brokenbots/workflow-example", RepoLabel: "brokenbots/labelled-repo"},
			wantRepoURL: "brokenbots/workflow-example",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.wantRepoURL, linear.ExtractRepoURL(tc.issue, defaultRepoURL, mockValidator))
		})
	}

	// Nil validator should never return an unvalidated short-form candidate; it must fall back.
	t.Run("nil validator short-form falls back before defaultRepoURL", func(t *testing.T) {
		issue := linear.Issue{Title: "Fix brokenbots/workflow-example"}
		assert.Equal(t, defaultRepoURL, linear.ExtractRepoURL(issue, defaultRepoURL, nil))
	})

	t.Run("nil validator short-form falls back before repo label", func(t *testing.T) {
		issue := linear.Issue{Title: "Fix brokenbots/workflow-example", RepoLabel: "brokenbots/labelled-repo"}
		assert.Equal(t, "brokenbots/labelled-repo", linear.ExtractRepoURL(issue, defaultRepoURL, nil))
	})
}

func TestIssuesInProjectStatesParsesLabelGroups(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query string `json:"query"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))

		resp := map[string]interface{}{}
		if strings.Contains(req.Query, "projects") {
			resp["data"] = map[string]interface{}{
				"projects": map[string]interface{}{
					"nodes": []interface{}{
						map[string]interface{}{"id": "proj-1", "name": "Runner"},
					},
				},
			}
		} else {
			resp["data"] = map[string]interface{}{
				"issues": map[string]interface{}{
					"nodes": []interface{}{
						map[string]interface{}{
							"id":         "issue-1",
							"identifier": "CRI-1",
							"state":      map[string]interface{}{"name": "Triage"},
							"project":    map[string]interface{}{"id": "proj-1", "name": "Runner"},
							"labels": map[string]interface{}{
								"nodes": []interface{}{
									map[string]interface{}{"name": "k8s-run"},
									map[string]interface{}{
										"name":   "linear-intake-v1",
										"parent": map[string]interface{}{"name": "workflows", "isGroup": true},
									},
									map[string]interface{}{
										"name":   "fastlane",
										"parent": map[string]interface{}{"name": "speed", "isGroup": true},
									},
								},
							},
						},
					},
				},
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	client := linear.NewClientWithBaseURL(ts.URL, "test-token")
	tickets, err := client.FindTicketsInStates(context.Background(), "Runner", []string{"Triage"})
	require.NoError(t, err)
	require.Len(t, tickets, 1)
	// Grouped labels map to their group's name; ungrouped labels stay absent.
	assert.Equal(t, map[string]string{
		"linear-intake-v1": "workflows",
		"fastlane":         "speed",
	}, tickets[0].LabelGroups)
	assert.Equal(t, []string{"k8s-run", "linear-intake-v1", "fastlane"}, tickets[0].Labels)
}

func TestPostCommentAndIssueComments(t *testing.T) {
	var createdComment string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string `json:"query"`
			Variables struct {
				Input struct {
					IssueID string `json:"issueId"`
					Body    string `json:"body"`
				} `json:"input"`
				ID    string `json:"id"`
				First int    `json:"first"`
			} `json:"variables"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))

		resp := map[string]interface{}{}
		switch {
		case strings.Contains(req.Query, "commentCreate"):
			assert.Equal(t, "issue-1", req.Variables.Input.IssueID)
			createdComment = req.Variables.Input.Body
			resp["data"] = map[string]interface{}{
				"commentCreate": map[string]interface{}{"success": true},
			}
		case strings.Contains(req.Query, "comments"):
			resp["data"] = map[string]interface{}{
				"issue": map[string]interface{}{
					"comments": map[string]interface{}{
						"nodes": []interface{}{
							map[string]interface{}{"body": "earlier comment"},
							map[string]interface{}{"body": createdComment},
						},
					},
				},
			}
		default:
			t.Fatalf("unexpected query: %s", req.Query)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	ctx := context.Background()
	client := linear.NewClientWithBaseURL(ts.URL, "test-token")

	// Fresh ticket: no comments yet.
	comments, err := client.IssueComments(ctx, "issue-1", 50)
	require.NoError(t, err)
	assert.Equal(t, []string{"earlier comment", ""}, comments)

	require.NoError(t, client.PostComment(ctx, "issue-1", "routing failure body"))
	assert.Equal(t, "routing failure body", createdComment)

	// The posted body is now visible to the dedup read.
	comments, err = client.IssueComments(ctx, "issue-1", 50)
	require.NoError(t, err)
	assert.Contains(t, comments, "routing failure body")
}

// CRI-219 label lifecycle primitives.

// fakeLinearLabel is a team-scoped Linear issue label in labelFakeServer.
type fakeLinearLabel struct {
	ID     string
	Name   string
	Color  string
	TeamID string
}

// labelFakeServer fakes the Linear GraphQL surface used by the CRI-219
// label primitives: team resolution, team-scoped label ensure, and
// read-modify-write label mutation on an issue. Label state is tracked by
// name per issue; IDs are synthesized deterministically as "lbl-"+name.
type labelFakeServer struct {
	ts           *httptest.Server
	mu           sync.Mutex
	projectTeams []string
	labels       []fakeLinearLabel
	// issueLabels maps issue id to the label names it currently carries.
	issueLabels     map[string][]string
	updates         int      // issueUpdate mutation count (no-ops excluded)
	lastIDs         []string // labelIds payload of the most recent issueUpdate
	failEnsure      bool     // serve a GraphQL error on the label queries
	failIssue       bool     // serve a GraphQL error on the issue(id:) query
	issueIdentifier string
	issueTitle      string
	// identifierToID maps human identifiers to canonical node ids so the
	// issue(id:) branch can resolve queries by identifier like Linear does.
	identifierToID map[string]string
}

func newLabelFakeServer(t *testing.T) *labelFakeServer {
	t.Helper()
	s := &labelFakeServer{
		projectTeams:   []string{"team-1"},
		issueLabels:    map[string][]string{},
		identifierToID: map[string]string{},
	}
	s.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string          `json:"query"`
			Variables json.RawMessage `json:"variables"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))

		resp := map[string]interface{}{}
		switch {
		case strings.Contains(req.Query, "teams"):
			s.mu.Lock()
			nodes := make([]interface{}, 0, len(s.projectTeams))
			for _, id := range s.projectTeams {
				nodes = append(nodes, map[string]interface{}{"id": id})
			}
			s.mu.Unlock()
			resp["data"] = map[string]interface{}{
				"project": map[string]interface{}{"teams": map[string]interface{}{"nodes": nodes}},
			}
		case strings.Contains(req.Query, "issueLabelCreate"):
			s.mu.Lock()
			fail := s.failEnsure
			s.mu.Unlock()
			if fail {
				resp = graphqlErrorResponse("label create down")
				break
			}
			var full struct {
				Input struct {
					Name   string `json:"name"`
					Color  string `json:"color"`
					TeamID string `json:"teamId"`
				} `json:"input"`
			}
			require.NoError(t, json.Unmarshal(req.Variables, &full))
			s.mu.Lock()
			id := "lbl-" + full.Input.Name
			for _, l := range s.labels {
				if l.Name == full.Input.Name && l.TeamID == full.Input.TeamID {
					id = l.ID
					break
				}
			}
			if id == "lbl-"+full.Input.Name {
				s.labels = append(s.labels, fakeLinearLabel{
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
		case strings.Contains(req.Query, "issueLabels"):
			s.mu.Lock()
			fail := s.failEnsure
			s.mu.Unlock()
			if fail {
				resp = graphqlErrorResponse("label query down")
				break
			}
			var variables struct {
				Name string `json:"name"`
			}
			require.NoError(t, json.Unmarshal(req.Variables, &variables))
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
		case strings.Contains(req.Query, "issueUpdate"):
			var full struct {
				ID    string `json:"id"`
				Input struct {
					LabelIDs []string `json:"labelIds"`
				} `json:"input"`
			}
			require.NoError(t, json.Unmarshal(req.Variables, &full))
			s.mu.Lock()
			names := make([]string, 0, len(full.Input.LabelIDs))
			for _, id := range full.Input.LabelIDs {
				names = append(names, s.labelNameLocked(id))
			}
			s.issueLabels[full.ID] = names
			s.updates++
			s.lastIDs = append([]string(nil), full.Input.LabelIDs...)
			s.mu.Unlock()
			resp["data"] = map[string]interface{}{
				"issueUpdate": map[string]interface{}{"success": true},
			}
		case strings.Contains(req.Query, "issue(id:"):
			var variables struct {
				ID string `json:"id"`
			}
			require.NoError(t, json.Unmarshal(req.Variables, &variables))
			s.mu.Lock()
			if s.failIssue {
				s.mu.Unlock()
				resp = graphqlErrorResponse("issue not found")
				break
			}
			// Linear resolves the query id (node id or human identifier) to
			// the issue's canonical node id.
			canonical := variables.ID
			if mapped, ok := s.identifierToID[variables.ID]; ok {
				canonical = mapped
			}
			names := append([]string(nil), s.issueLabels[canonical]...)
			nodes := make([]interface{}, 0, len(names))
			for _, name := range names {
				nodes = append(nodes, map[string]interface{}{"id": s.labelIDLocked(name), "name": name})
			}
			identifier, title := s.issueIdentifier, s.issueTitle
			s.mu.Unlock()
			resp["data"] = map[string]interface{}{
				"issue": map[string]interface{}{
					"id":         canonical,
					"identifier": identifier,
					"title":      title,
					"labels":     map[string]interface{}{"nodes": nodes},
				},
			}
		default:
			t.Fatalf("unexpected query: %s", req.Query)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(s.ts.Close)
	return s
}

// graphqlErrorResponse builds a Linear-style GraphQL error response.
func graphqlErrorResponse(message string) map[string]interface{} {
	return map[string]interface{}{
		"errors": []interface{}{map[string]interface{}{"message": message}},
	}
}

func (s *labelFakeServer) setProjectTeams(teams []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.projectTeams = teams
}

// seedLabel installs a pre-existing label in the fake's registry.
func (s *labelFakeServer) seedLabel(l fakeLinearLabel) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.labels = append(s.labels, l)
}

func (s *labelFakeServer) setFailEnsure(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failEnsure = v
}

// seedIssueLabels installs the label names an issue currently carries.
func (s *labelFakeServer) seedIssueLabels(issueID string, names ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.issueLabels[issueID] = append([]string(nil), names...)
}

// labelNameLocked resolves a label ID to its name. Unknown IDs follow the
// fake's deterministic "lbl-"+name synthesis.
func (s *labelFakeServer) labelNameLocked(id string) string {
	for _, l := range s.labels {
		if l.ID == id {
			return l.Name
		}
	}
	return strings.TrimPrefix(id, "lbl-")
}

// labelIDLocked resolves a label name to its ID.
func (s *labelFakeServer) labelIDLocked(name string) string {
	for _, l := range s.labels {
		if l.Name == name {
			return l.ID
		}
	}
	return "lbl-" + name
}

// labelsSnapshot returns a copy of the fake's label registry.
func (s *labelFakeServer) labelsSnapshot() []fakeLinearLabel {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]fakeLinearLabel(nil), s.labels...)
}

// issueLabelNames returns a copy of the label names the issue carries.
func (s *labelFakeServer) issueLabelNames(issueID string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.issueLabels[issueID]...)
}

// updateCount returns the number of issueUpdate mutations served.
func (s *labelFakeServer) updateCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.updates
}

// lastUpdateIDs returns the labelIds payload of the most recent issueUpdate.
func (s *labelFakeServer) lastUpdateIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.lastIDs...)
}

func TestFindProjectTeamID(t *testing.T) {
	ctx := context.Background()

	t.Run("returns the project's team", func(t *testing.T) {
		s := newLabelFakeServer(t)
		c := linear.NewClientWithBaseURL(s.ts.URL, "test-token")
		teamID, err := c.FindProjectTeamID(ctx, "proj-1")
		require.NoError(t, err)
		assert.Equal(t, "team-1", teamID)
	})

	t.Run("fails when the project has no team", func(t *testing.T) {
		s := newLabelFakeServer(t)
		s.setProjectTeams(nil)
		c := linear.NewClientWithBaseURL(s.ts.URL, "test-token")
		_, err := c.FindProjectTeamID(ctx, "proj-1")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "has no team")
	})
}

// EnsureTeamLabel must create the team-scoped label exactly once when it is
// missing, adopt it when it exists, and never fail when it already exists.
func TestEnsureTeamLabel(t *testing.T) {
	ctx := context.Background()

	t.Run("creates the label when missing and is idempotent", func(t *testing.T) {
		s := newLabelFakeServer(t)
		c := linear.NewClientWithBaseURL(s.ts.URL, "test-token")

		id, err := c.EnsureTeamLabel(ctx, "team-1", "criteria-automation", "#0ea5e9")
		require.NoError(t, err)
		assert.Equal(t, "lbl-criteria-automation", id)

		created := s.labelsSnapshot()
		require.Len(t, created, 1)
		assert.Equal(t, "criteria-automation", created[0].Name)
		assert.Equal(t, "#0ea5e9", created[0].Color)
		assert.Equal(t, "team-1", created[0].TeamID, "the label must be team-scoped")

		// Ensuring again must not create a duplicate.
		idAgain, err := c.EnsureTeamLabel(ctx, "team-1", "criteria-automation", "#0ea5e9")
		require.NoError(t, err)
		assert.Equal(t, id, idAgain)
		assert.Len(t, s.labelsSnapshot(), 1, "no duplicate label")
	})

	t.Run("ignores a same-named label scoped to another team", func(t *testing.T) {
		s := newLabelFakeServer(t)
		c := linear.NewClientWithBaseURL(s.ts.URL, "test-token")
		s.seedLabel(fakeLinearLabel{ID: "lbl-other", Name: "criteria-automation", Color: "#000000", TeamID: "team-2"})

		id, err := c.EnsureTeamLabel(ctx, "team-1", "criteria-automation", "#0ea5e9")
		require.NoError(t, err)
		assert.Equal(t, "lbl-criteria-automation", id, "team-1 gets its own label")
		created := s.labelsSnapshot()
		require.Len(t, created, 2)
		assert.Equal(t, "team-1", created[1].TeamID)
	})

	t.Run("surfaces ensure failures", func(t *testing.T) {
		s := newLabelFakeServer(t)
		s.setFailEnsure(true)
		c := linear.NewClientWithBaseURL(s.ts.URL, "test-token")

		_, err := c.EnsureTeamLabel(ctx, "team-1", "criteria-automation", "#0ea5e9")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "label query down")
		assert.Empty(t, s.labelsSnapshot(), "no label is created when the query fails")
	})
}

// AddIssueLabel must merge: Linear labelIds have REPLACE semantics, so the
// write must carry the full desired set. A regression writing only the new
// label would silently drop every label the issue already had.
func TestAddIssueLabelMergesExistingLabels(t *testing.T) {
	ctx := context.Background()
	s := newLabelFakeServer(t)
	c := linear.NewClientWithBaseURL(s.ts.URL, "test-token")
	s.seedIssueLabels("issue-1", "k8s-run", "fast")

	require.NoError(t, c.AddIssueLabel(ctx, "issue-1", "lbl-criteria-automation"))

	assert.Equal(t, []string{"k8s-run", "fast", "criteria-automation"},
		s.issueLabelNames("issue-1"), "existing labels are preserved and the new label is added")
	// The REPLACE regression: the single write must carry the full merged set.
	assert.Equal(t, []string{"lbl-k8s-run", "lbl-fast", "lbl-criteria-automation"}, s.lastUpdateIDs())
	assert.Equal(t, 1, s.updateCount(), "exactly one issueUpdate")
}

func TestAddIssueLabelNoOpWhenPresent(t *testing.T) {
	ctx := context.Background()
	s := newLabelFakeServer(t)
	c := linear.NewClientWithBaseURL(s.ts.URL, "test-token")
	s.seedIssueLabels("issue-1", "criteria-automation", "fast")

	require.NoError(t, c.AddIssueLabel(ctx, "issue-1", "lbl-criteria-automation"))

	assert.Equal(t, []string{"criteria-automation", "fast"}, s.issueLabelNames("issue-1"))
	assert.Equal(t, 0, s.updateCount(), "no issueUpdate when the label is already present")
}

func TestRemoveIssueLabel(t *testing.T) {
	ctx := context.Background()

	t.Run("removes only the target label", func(t *testing.T) {
		s := newLabelFakeServer(t)
		c := linear.NewClientWithBaseURL(s.ts.URL, "test-token")
		s.seedIssueLabels("issue-1", "criteria-automation", "fast")

		require.NoError(t, c.RemoveIssueLabel(ctx, "issue-1", "lbl-criteria-automation"))

		assert.Equal(t, []string{"fast"}, s.issueLabelNames("issue-1"), "other labels untouched")
		assert.Equal(t, []string{"lbl-fast"}, s.lastUpdateIDs(),
			"the write carries the remaining set, not a delta")
		assert.Equal(t, 1, s.updateCount())
	})

	t.Run("no write when the label is absent", func(t *testing.T) {
		s := newLabelFakeServer(t)
		c := linear.NewClientWithBaseURL(s.ts.URL, "test-token")
		s.seedIssueLabels("issue-1", "fast")

		require.NoError(t, c.RemoveIssueLabel(ctx, "issue-1", "lbl-criteria-automation"))

		assert.Equal(t, []string{"fast"}, s.issueLabelNames("issue-1"))
		assert.Equal(t, 0, s.updateCount(), "no issueUpdate for an absent label")
	})
}

// TestIssueByIdentifier covers the identifier-based issue lookup the
// CriteriaRun-driven reconciliation uses: Linear's issue(id:) query accepts
// the human identifier as well as the node id and returns the issue with
// its current label names.
func TestIssueByIdentifier(t *testing.T) {
	t.Run("found by identifier", func(t *testing.T) {
		s := newLabelFakeServer(t)
		s.issueIdentifier = "CRI-9"
		s.issueTitle = "Fix the flaky test"
		s.identifierToID["CRI-9"] = "issue-9"
		s.seedIssueLabels("issue-9", "fast", "criteria-automation")
		c := linear.NewClientWithBaseURL(s.ts.URL, "test-token")

		issue, err := c.IssueByIdentifier(context.Background(), "CRI-9")

		require.NoError(t, err)
		assert.Equal(t, "issue-9", issue.ID)
		assert.Equal(t, "CRI-9", issue.Identifier)
		assert.Equal(t, "Fix the flaky test", issue.Title)
		assert.True(t, slices.Equal(issue.Labels, []string{"fast", "criteria-automation"}),
			"labels carry names, not ids")
	})

	t.Run("missing issue surfaces the GraphQL error", func(t *testing.T) {
		s := newLabelFakeServer(t)
		s.failIssue = true
		c := linear.NewClientWithBaseURL(s.ts.URL, "test-token")

		_, err := c.IssueByIdentifier(context.Background(), "CRI-404")

		require.Error(t, err)
		assert.Contains(t, err.Error(), "issue not found")
	})
}
