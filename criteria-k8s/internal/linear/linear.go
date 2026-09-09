// Package linear provides a minimal GraphQL client for polling Linear issues.
package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Client calls the Linear GraphQL API.
type Client struct {
	BaseURL   string
	APIKey    string
	HTTP      *http.Client
}

// Issue represents a Linear issue relevant to the watcher.
type Issue struct {
	ID          string `json:"id"`
	Identifier  string `json:"identifier"`
	Title       string `json:"title"`
	Description string `json:"description"`
	ProjectID   string `json:"projectId"`
	ProjectName string `json:"projectName"`
	StateName   string `json:"stateName"`
}

// NewClient returns a Linear client using the provided API key.
func NewClient(apiKey string) *Client {
	return &Client{
		BaseURL: "https://api.linear.app/graphql",
		APIKey:  apiKey,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

// NewClientWithBaseURL returns a Linear client for tests or alternate endpoints.
func NewClientWithBaseURL(baseURL, apiKey string) *Client {
	c := NewClient(apiKey)
	c.BaseURL = baseURL
	return c
}

// graphqlRequest is the standard Linear request envelope.
type graphqlRequest struct {
	Query     string                 `json:"query"`
	Variables map[string]interface{} `json:"variables,omitempty"`
}

// graphqlResponse is a minimal response envelope.
type graphqlResponse struct {
	Data   json.RawMessage   `json:"data"`
	Errors []graphqlError    `json:"errors"`
}

type graphqlError struct {
	Message string `json:"message"`
}

func (c *Client) do(ctx context.Context, req graphqlRequest, target interface{}) error {
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	hReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	hReq.Header.Set("Authorization", c.APIKey)
	hReq.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(hReq)
	if err != nil {
		return fmt.Errorf("linear request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading linear response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("linear returned HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var gr graphqlResponse
	if err := json.Unmarshal(respBody, &gr); err != nil {
		return fmt.Errorf("parsing linear response: %w", err)
	}
	if len(gr.Errors) > 0 {
		msgs := make([]string, len(gr.Errors))
		for i, e := range gr.Errors {
			msgs[i] = e.Message
		}
		return fmt.Errorf("linear errors: %s", strings.Join(msgs, "; "))
	}
	if target != nil {
		if err := json.Unmarshal(gr.Data, target); err != nil {
			return fmt.Errorf("decoding linear data: %w", err)
		}
	}
	return nil
}

// Project represents a Linear project.
type project struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// FindProjectID returns the project ID for the given project name.
func (c *Client) FindProjectID(ctx context.Context, name string) (string, error) {
	req := graphqlRequest{
		Query: `query($name: String!) { projects(filter: {name: {eq: $name}}) { nodes { id name } } }`,
		Variables: map[string]interface{}{"name": name},
	}
	var result struct {
		Projects struct {
			Nodes []project `json:"nodes"`
		} `json:"projects"`
	}
	if err := c.do(ctx, req, &result); err != nil {
		return "", err
	}
	for _, p := range result.Projects.Nodes {
		if p.Name == name {
			return p.ID, nil
		}
	}
	return "", fmt.Errorf("linear project %q not found", name)
}

// IssuesInProjectState returns issues in the project that are in the named workflow state.
func (c *Client) IssuesInProjectState(ctx context.Context, projectID, stateName string) ([]Issue, error) {
	req := graphqlRequest{
		Query: `query($project: ID!, $state: String!) {
            issues(filter: {project: {id: {eq: $project}}, state: {name: {eq: $state}}}) {
                nodes { id identifier title description state { name } project { id name } }
            }
        }`,
		Variables: map[string]interface{}{
			"project": projectID,
			"state":   stateName,
		},
	}
	var result struct {
		Issues struct {
			Nodes []struct {
				ID          string `json:"id"`
				Identifier  string `json:"identifier"`
				Title       string `json:"title"`
				Description string `json:"description"`
				State       struct {
					Name string `json:"name"`
				} `json:"state"`
				Project struct {
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"project"`
			} `json:"nodes"`
		} `json:"issues"`
	}
	if err := c.do(ctx, req, &result); err != nil {
		return nil, err
	}
	out := make([]Issue, 0, len(result.Issues.Nodes))
	for _, n := range result.Issues.Nodes {
		out = append(out, Issue{
			ID:          n.ID,
			Identifier:  n.Identifier,
			Title:       n.Title,
			Description: n.Description,
			ProjectID:   n.Project.ID,
			ProjectName: n.Project.Name,
			StateName:   n.State.Name,
		})
	}
	return out, nil
}

// FindTriageTickets returns issues in the named project that are in the named state.
func (c *Client) FindTriageTickets(ctx context.Context, projectName, stateName string) ([]Issue, error) {
	projectID, err := c.FindProjectID(ctx, projectName)
	if err != nil {
		return nil, err
	}
	return c.IssuesInProjectState(ctx, projectID, stateName)
}

// ExtractRepoURL tries to find a GitHub repository reference in the issue.
// It looks for owner/repo or https://github.com/owner/repo in the title and
// description, then falls back to the provided defaultRepoURL.
func ExtractRepoURL(issue Issue, defaultRepoURL string) string {
	candidate := issue.Title + "\n" + issue.Description
	repoPattern := regexp.MustCompile(`(?:https?://)?github\.com/([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)`)
	if m := repoPattern.FindStringSubmatch(candidate); m != nil {
		return m[1]
	}
	shortPattern := regexp.MustCompile(`\b([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)\b`)
	for _, m := range shortPattern.FindAllStringSubmatch(candidate, -1) {
		parts := strings.Split(m[1], "/")
		if len(parts) == 2 && len(parts[0]) > 0 && len(parts[1]) > 0 {
			return m[1]
		}
	}
	return defaultRepoURL
}
