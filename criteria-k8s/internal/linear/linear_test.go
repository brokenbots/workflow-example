package linear_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/brokenbots/workflow-example/criteria-k8s/internal/linear"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFindTriageTickets(t *testing.T) {
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
						map[string]interface{}{"id": "proj-1", "name": "Criteria K8s Workflow Runner"},
					},
				},
			}
		} else if strings.Contains(req.Query, "issues") {
			resp["data"] = map[string]interface{}{
				"issues": map[string]interface{}{
					"nodes": []interface{}{
						map[string]interface{}{
							"id":          "issue-1",
							"identifier":  "CRI-99",
							"title":       "Test ticket",
							"description": "Repo: https://github.com/brokenbots/workflow-example/issues/1",
							"state":       map[string]interface{}{"name": "Triage"},
							"project":     map[string]interface{}{"id": "proj-1", "name": "Criteria K8s Workflow Runner"},
						},
						map[string]interface{}{
							"id":          "issue-2",
							"identifier":  "CRI-100",
							"title":       "No repo ticket",
							"description": "Just text",
							"state":       map[string]interface{}{"name": "Triage"},
							"project":     map[string]interface{}{"id": "proj-1", "name": "Criteria K8s Workflow Runner"},
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
	tickets, err := client.FindTriageTickets(context.Background(), "Criteria K8s Workflow Runner", "Triage")
	require.NoError(t, err)
	require.Len(t, tickets, 2)
	assert.Equal(t, "CRI-99", tickets[0].Identifier)
	assert.Equal(t, "CRI-100", tickets[1].Identifier)
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
		name         string
		issue        linear.Issue
		wantRepoURL  string
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
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.wantRepoURL, linear.ExtractRepoURL(tc.issue, defaultRepoURL, mockValidator))
		})
	}
}
