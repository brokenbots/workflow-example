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
	"slices"
	"strings"
	"time"
)

// Client calls the Linear GraphQL API.
type Client struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
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
	// RepoLabel is an optional repository reference (owner/repo) configured on the issue.
	RepoLabel string `json:"repoLabel,omitempty"`
	// Labels holds the issue's Linear label names.
	Labels []string `json:"labels,omitempty"`
	// LabelGroups maps a label name to the name of its Linear label
	// group (Linear models label groups as parent labels marked
	// isGroup). Labels without a group parent are absent from the map.
	LabelGroups map[string]string `json:"labelGroups,omitempty"`
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
	Data   json.RawMessage `json:"data"`
	Errors []graphqlError  `json:"errors"`
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
		Query:     `query($name: String!) { projects(filter: {name: {eq: $name}}) { nodes { id name } } }`,
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

// IssuesInProjectStates returns issues in the project whose workflow state
// name is any of states (CRI-218: the watcher polls the union of the
// routes' declared states). With no states it queries nothing and returns
// no issues: fail closed.
func (c *Client) IssuesInProjectStates(ctx context.Context, projectID string, states []string) ([]Issue, error) {
	if len(states) == 0 {
		return nil, nil
	}
	req := graphqlRequest{
		Query: `query($project: ID!, $states: [String!]!) {
            issues(filter: {project: {id: {eq: $project}}, state: {name: {in: $states}}}) {
                nodes { id identifier title description state { name } project { id name }
                        labels { nodes { name parent { name isGroup } } } }
            }
        }`,
		Variables: map[string]interface{}{
			"project": projectID,
			"states":  states,
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
				Labels struct {
					Nodes []struct {
						Name   string `json:"name"`
						Parent struct {
							Name    string `json:"name"`
							IsGroup bool   `json:"isGroup"`
						} `json:"parent"`
					} `json:"nodes"`
				} `json:"labels"`
			} `json:"nodes"`
		} `json:"issues"`
	}
	if err := c.do(ctx, req, &result); err != nil {
		return nil, err
	}
	out := make([]Issue, 0, len(result.Issues.Nodes))
	for _, n := range result.Issues.Nodes {
		issue := Issue{
			ID:          n.ID,
			Identifier:  n.Identifier,
			Title:       n.Title,
			Description: n.Description,
			ProjectID:   n.Project.ID,
			ProjectName: n.Project.Name,
			StateName:   n.State.Name,
		}
		for _, l := range n.Labels.Nodes {
			issue.Labels = append(issue.Labels, l.Name)
			if l.Parent.IsGroup && l.Parent.Name != "" {
				if issue.LabelGroups == nil {
					issue.LabelGroups = make(map[string]string, len(n.Labels.Nodes))
				}
				issue.LabelGroups[l.Name] = l.Parent.Name
			}
		}
		out = append(out, issue)
	}
	return out, nil
}

// FindTicketsInStates returns issues in the named project that are in any
// of the given workflow states (CRI-218).
func (c *Client) FindTicketsInStates(ctx context.Context, projectName string, states []string) ([]Issue, error) {
	projectID, err := c.FindProjectID(ctx, projectName)
	if err != nil {
		return nil, err
	}
	return c.IssuesInProjectStates(ctx, projectID, states)
}

// PostComment creates a comment on the given issue (CRI-217 routing
// failure notifications).
func (c *Client) PostComment(ctx context.Context, issueID, body string) error {
	req := graphqlRequest{
		Query: `mutation($input: CommentCreateInput!) { commentCreate(input: $input) { success } }`,
		Variables: map[string]interface{}{
			"input": map[string]interface{}{"issueId": issueID, "body": body},
		},
	}
	var result struct {
		CommentCreate struct {
			Success bool `json:"success"`
		} `json:"commentCreate"`
	}
	if err := c.do(ctx, req, &result); err != nil {
		return err
	}
	if !result.CommentCreate.Success {
		return fmt.Errorf("linear commentCreate did not succeed for issue %s", issueID)
	}
	return nil
}

// IssueComments returns the bodies of the issue's most recent comments, up
// to limit. It backs the watcher's routing-failure comment dedup.
func (c *Client) IssueComments(ctx context.Context, issueID string, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 50
	}
	req := graphqlRequest{
		Query: `query($id: String!, $first: Int!) { issue(id: $id) { comments(first: $first) { nodes { body } } } }`,
		Variables: map[string]interface{}{
			"id":    issueID,
			"first": limit,
		},
	}
	var result struct {
		Issue struct {
			Comments struct {
				Nodes []struct {
					Body string `json:"body"`
				} `json:"nodes"`
			} `json:"comments"`
		} `json:"issue"`
	}
	if err := c.do(ctx, req, &result); err != nil {
		return nil, err
	}
	bodies := make([]string, 0, len(result.Issue.Comments.Nodes))
	for _, n := range result.Issue.Comments.Nodes {
		bodies = append(bodies, n.Body)
	}
	return bodies, nil
}

// IssueByIdentifier returns the Linear issue with the given human-readable
// identifier (e.g. "CRI-1"; Linear's issue(id:) resolves identifiers as
// well as UUIDs). It backs the watcher's CriteriaRun-driven label
// reconciliation, which must reach tickets in any workflow state
// (CRI-219). Unknown issues surface as GraphQL errors.
func (c *Client) IssueByIdentifier(ctx context.Context, identifier string) (Issue, error) {
	req := graphqlRequest{
		Query:     `query($id: String!) { issue(id: $id) { id identifier title labels { nodes { name } } } }`,
		Variables: map[string]interface{}{"id": identifier},
	}
	var result struct {
		Issue struct {
			ID         string `json:"id"`
			Identifier string `json:"identifier"`
			Title      string `json:"title"`
			Labels     struct {
				Nodes []struct {
					Name string `json:"name"`
				} `json:"nodes"`
			} `json:"labels"`
		} `json:"issue"`
	}
	if err := c.do(ctx, req, &result); err != nil {
		return Issue{}, err
	}
	if result.Issue.ID == "" {
		return Issue{}, fmt.Errorf("linear issue %s not found", identifier)
	}
	labels := make([]string, 0, len(result.Issue.Labels.Nodes))
	for _, n := range result.Issue.Labels.Nodes {
		labels = append(labels, n.Name)
	}
	return Issue{
		ID:         result.Issue.ID,
		Identifier: result.Issue.Identifier,
		Title:      result.Issue.Title,
		Labels:     labels,
	}, nil
}

// FindProjectTeamID returns the ID of the first team the given project
// belongs to. It backs the team-scoped automation label creation (CRI-219).
// Projects spanning multiple teams are outside the CRI-219 scope: the first
// team is assumed to own the project's issues (documented single-team
// assumption).
func (c *Client) FindProjectTeamID(ctx context.Context, projectID string) (string, error) {
	req := graphqlRequest{
		Query:     `query($id: String!) { project(id: $id) { teams { nodes { id } } } }`,
		Variables: map[string]interface{}{"id": projectID},
	}
	var result struct {
		Project struct {
			Teams struct {
				Nodes []struct {
					ID string `json:"id"`
				} `json:"nodes"`
			} `json:"teams"`
		} `json:"project"`
	}
	if err := c.do(ctx, req, &result); err != nil {
		return "", err
	}
	for _, t := range result.Project.Teams.Nodes {
		if t.ID != "" {
			return t.ID, nil
		}
	}
	return "", fmt.Errorf("linear project %s has no team", projectID)
}

// EnsureTeamLabel returns the ID of the team-scoped issue label with the
// given name, creating it with the given color when missing (CRI-219).
// Labels matching the name but scoped to another team are ignored, so the
// call never duplicates an existing label and never fails when the label
// already exists.
func (c *Client) EnsureTeamLabel(ctx context.Context, teamID, name, color string) (string, error) {
	req := graphqlRequest{
		Query: `query($name: String!, $first: Int!) {
            issueLabels(filter: {name: {eq: $name}}, first: $first) {
                nodes { id name team { id } }
            }
        }`,
		Variables: map[string]interface{}{"name": name, "first": 250},
	}
	var result struct {
		IssueLabels struct {
			Nodes []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
				Team struct {
					ID string `json:"id"`
				} `json:"team"`
			} `json:"nodes"`
		} `json:"issueLabels"`
	}
	if err := c.do(ctx, req, &result); err != nil {
		return "", err
	}
	for _, n := range result.IssueLabels.Nodes {
		if n.Name == name && n.Team.ID == teamID {
			return n.ID, nil
		}
	}
	return c.createTeamLabel(ctx, teamID, name, color)
}

// createTeamLabel creates a team-scoped issue label and returns its ID.
func (c *Client) createTeamLabel(ctx context.Context, teamID, name, color string) (string, error) {
	req := graphqlRequest{
		Query: `mutation($input: IssueLabelCreateInput!) { issueLabelCreate(input: $input) { success issueLabel { id } } }`,
		Variables: map[string]interface{}{
			"input": map[string]interface{}{"name": name, "color": color, "teamId": teamID},
		},
	}
	var result struct {
		IssueLabelCreate struct {
			Success    bool `json:"success"`
			IssueLabel struct {
				ID string `json:"id"`
			} `json:"issueLabel"`
		} `json:"issueLabelCreate"`
	}
	if err := c.do(ctx, req, &result); err != nil {
		return "", err
	}
	if !result.IssueLabelCreate.Success || result.IssueLabelCreate.IssueLabel.ID == "" {
		return "", fmt.Errorf("linear issueLabelCreate did not succeed for label %q", name)
	}
	return result.IssueLabelCreate.IssueLabel.ID, nil
}

// issueLabelIDs returns the IDs of the issue's current labels.
func (c *Client) issueLabelIDs(ctx context.Context, issueID string) ([]string, error) {
	req := graphqlRequest{
		Query:     `query($id: String!) { issue(id: $id) { labels { nodes { id } } } }`,
		Variables: map[string]interface{}{"id": issueID},
	}
	var result struct {
		Issue struct {
			Labels struct {
				Nodes []struct {
					ID string `json:"id"`
				} `json:"nodes"`
			} `json:"labels"`
		} `json:"issue"`
	}
	if err := c.do(ctx, req, &result); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(result.Issue.Labels.Nodes))
	for _, n := range result.Issue.Labels.Nodes {
		ids = append(ids, n.ID)
	}
	return ids, nil
}

// setIssueLabelIDs replaces the issue's label set. Linear labelIds have
// REPLACE semantics: callers must pass the full desired set, never just the
// delta (CRI-219).
func (c *Client) setIssueLabelIDs(ctx context.Context, issueID string, labelIDs []string) error {
	req := graphqlRequest{
		Query: `mutation($id: String!, $input: IssueUpdateInput!) { issueUpdate(id: $id, input: $input) { success } }`,
		Variables: map[string]interface{}{
			"id":    issueID,
			"input": map[string]interface{}{"labelIds": labelIDs},
		},
	}
	var result struct {
		IssueUpdate struct {
			Success bool `json:"success"`
		} `json:"issueUpdate"`
	}
	if err := c.do(ctx, req, &result); err != nil {
		return err
	}
	if !result.IssueUpdate.Success {
		return fmt.Errorf("linear issueUpdate did not succeed for issue %s", issueID)
	}
	return nil
}

// AddIssueLabel adds labelID to the issue's existing label set. The set is
// read-modify-write: because Linear labelIds have REPLACE semantics, the
// write always carries the full merged set, so no label the issue already
// carries can be dropped. Adding a label the issue already carries is a
// no-op.
func (c *Client) AddIssueLabel(ctx context.Context, issueID, labelID string) error {
	ids, err := c.issueLabelIDs(ctx, issueID)
	if err != nil {
		return err
	}
	if slices.Contains(ids, labelID) {
		return nil
	}
	return c.setIssueLabelIDs(ctx, issueID, append(slices.Clone(ids), labelID))
}

// RemoveIssueLabel removes labelID from the issue's label set, leaving
// every other label untouched (read-modify-write; see AddIssueLabel).
// Removing a label the issue does not carry is a no-op.
func (c *Client) RemoveIssueLabel(ctx context.Context, issueID, labelID string) error {
	ids, err := c.issueLabelIDs(ctx, issueID)
	if err != nil {
		return err
	}
	idx := slices.Index(ids, labelID)
	if idx < 0 {
		return nil
	}
	ids = slices.Delete(slices.Clone(ids), idx, idx+1)
	return c.setIssueLabelIDs(ctx, issueID, ids)
}

// RepoValidator checks whether a short-form owner/repo reference names an existing
// GitHub repository. It is used by ExtractRepoURL to avoid returning file paths or
// arbitrary slash-separated tokens.
type RepoValidator func(repo string) bool

// DefaultRepoValidator returns a RepoValidator that confirms repository existence
// via the GitHub REST API. A nil httpClient uses a client with a 10-second timeout.
// An empty token performs unauthenticated requests; callers should supply a token
// when available to avoid rate limits. An empty apiURL defaults to
// https://api.github.com.
func DefaultRepoValidator(httpClient *http.Client, token, apiURL string) RepoValidator {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	if apiURL == "" {
		apiURL = "https://api.github.com"
	}
	return func(repo string) bool {
		req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/repos/%s", strings.TrimSuffix(apiURL, "/"), repo), nil)
		if err != nil {
			return false
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}
}

var (
	fileExtensionSuffixes = []string{".go", ".yaml", ".yml", ".json", ".md"}
	configStyleDirs       = []string{"k8s", "config", "manifests", "deploy"}
)

func hasFileExtension(s string) bool {
	lower := strings.ToLower(s)
	for _, ext := range fileExtensionSuffixes {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

func isConfigStyleDir(s string) bool {
	for _, dir := range configStyleDirs {
		if strings.EqualFold(s, dir) {
			return true
		}
	}
	return false
}

// ExtractRepoURL tries to find a GitHub repository reference in the issue.
// It looks for owner/repo or https://github.com/owner/repo in the title and
// description, validates short-form candidates via validate, then falls back to
// the issue's RepoLabel and finally the provided defaultRepoURL.
func ExtractRepoURL(issue Issue, defaultRepoURL string, validate RepoValidator) string {
	candidate := issue.Title + "\n" + issue.Description

	// Prefer an explicit github.com URL.
	repoPattern := regexp.MustCompile(`(?:https?://)?github\.com/([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)`)
	if m := repoPattern.FindStringSubmatch(candidate); m != nil {
		return m[1]
	}

	// Validate any remaining short-form candidates.
	shortPattern := regexp.MustCompile(`\b([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)\b`)
	for _, m := range shortPattern.FindAllStringSubmatch(candidate, -1) {
		parts := strings.Split(m[1], "/")
		if len(parts) != 2 || len(parts[0]) == 0 || len(parts[1]) == 0 {
			continue
		}
		if hasFileExtension(parts[1]) {
			continue
		}
		if isConfigStyleDir(parts[0]) {
			continue
		}
		if validate != nil && validate(m[1]) {
			return m[1]
		}
	}

	// Fall back to a repository label configured on the issue.
	if issue.RepoLabel != "" {
		return issue.RepoLabel
	}

	return defaultRepoURL
}
