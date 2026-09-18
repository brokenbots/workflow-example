package linear_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/brokenbots/workflow-example/criteria-k8s/internal/linear"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newLinearTestServer returns a server answering the project lookup and the
// issues query, filtering issues by the queried `states` variable exactly
// like Linear's `state: {name: {in: $states}}` filter would.
func newLinearTestServer(t *testing.T, issues []map[string]interface{}, queries *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string `json:"query"`
			Variables struct {
				States []string `json:"states"`
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
