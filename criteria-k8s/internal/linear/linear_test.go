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
	assert.Equal(t, "brokenbots/workflow-example", linear.ExtractRepoURL(issue, ""))
}
