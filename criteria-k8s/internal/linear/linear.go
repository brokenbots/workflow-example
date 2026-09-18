// Package linear provides a minimal GraphQL client for polling Linear issues.
package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
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
	// LabelIDs holds the issue's Linear label IDs, parallel to Labels.
	// It is populated by the queries that select label ids (CRI-252), so
	// label writes can reuse the same poll's read instead of re-reading
	// the issue's label set; absent for queries that do not select ids.
	LabelIDs []string `json:"labelIds,omitempty"`
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
	// Extensions is Linear's optional error extension payload (raw JSON:
	// absent or non-object extensions must be tolerated).
	Extensions json.RawMessage `json:"extensions"`
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
	if resp.StatusCode == http.StatusTooManyRequests {
		return &RateLimitError{
			Status:   resp.StatusCode,
			Body:     string(respBody),
			Duration: retryAfterDuration(resp.Header.Get("Retry-After")),
		}
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("linear returned HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var gr graphqlResponse
	if err := json.Unmarshal(respBody, &gr); err != nil {
		return fmt.Errorf("parsing linear response: %w", err)
	}
	if len(gr.Errors) > 0 {
		if rl := rateLimitFromGraphQL(gr.Errors); rl != nil {
			return rl
		}
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

// RateLimitError signals that Linear rejected a request because the shared
// rate budget was exhausted (HTTP 429, or a GraphQL RATELIMITED error).
// The watcher turns it into a poll skip plus back-off (CRI-252) instead of
// retrying at full cost.
type RateLimitError struct {
	// Status is the HTTP status Linear answered with (429, or 400 for the
	// RATELIMITED GraphQL error shape).
	Status int
	// Body is the raw response body or the GraphQL error message.
	Body string
	// Duration is how long to wait before the next request: Linear's
	// rateLimitResult.duration or the Retry-After header when provided.
	Duration time.Duration
}

func (e *RateLimitError) Error() string {
	if e.Duration > 0 {
		return fmt.Sprintf("linear: rate limited (status %d, retry after %s)", e.Status, e.Duration)
	}
	return fmt.Sprintf("linear: rate limited (status %d)", e.Status)
}

// IsRateLimit reports whether err is (or wraps) a Linear rate limit error.
func IsRateLimit(err error) bool {
	var rl *RateLimitError
	return errors.As(err, &rl)
}

// rateLimitFromGraphQL reports whether a Linear error response represents a
// rate limit rejection. Linear answers plain over-limit requests with
// HTTP 429, but some rejections surface as GraphQL errors carrying a
// RATELIMITED extension code plus a rateLimitResult duration in
// milliseconds (CRI-252).
func rateLimitFromGraphQL(errs []graphqlError) *RateLimitError {
	for _, e := range errs {
		var ext struct {
			Code string          `json:"code"`
			Meta json.RawMessage `json:"meta"`
		}
		if len(e.Extensions) > 0 && json.Unmarshal(e.Extensions, &ext) == nil {
			if strings.EqualFold(strings.TrimSpace(ext.Code), "ratelimited") {
				return &RateLimitError{
					Status:   http.StatusBadRequest,
					Body:     e.Message,
					Duration: durationFromRateLimitMeta(ext.Meta),
				}
			}
		}
		if strings.Contains(strings.ToUpper(e.Message), "RATELIMITED") {
			return &RateLimitError{Status: http.StatusBadRequest, Body: e.Message}
		}
	}
	return nil
}

// durationFromRateLimitMeta extracts rateLimitResult.duration (in
// milliseconds) from Linear's error extension meta, when present.
func durationFromRateLimitMeta(meta json.RawMessage) time.Duration {
	if len(meta) == 0 {
		return 0
	}
	var parsed struct {
		RateLimitResult struct {
			Duration int64 `json:"duration"`
		} `json:"rateLimitResult"`
	}
	if err := json.Unmarshal(meta, &parsed); err != nil {
		return 0
	}
	if parsed.RateLimitResult.Duration <= 0 {
		return 0
	}
	return time.Duration(parsed.RateLimitResult.Duration) * time.Millisecond
}

// retryAfterDuration parses a Retry-After header value (seconds) into a
// duration, ignoring unparseable or non-positive values.
func retryAfterDuration(header string) time.Duration {
	value := strings.TrimSpace(header)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if seconds, err := strconv.ParseFloat(value, 64); err == nil && seconds > 0 {
		return time.Duration(seconds * float64(time.Second))
	}
	return 0
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

// linearIssuesBatch is the page size for batched identifier lookups
// (CRI-252): one query per 100 identifiers keeps each response well inside
// Linear's complexity limits.
const linearIssuesBatch = 100

// issueNode is the GraphQL node shape shared by the issue listing queries
// (IssuesInProjectStates, IssuesByIdentifiers): all of them select the same
// issue fields, so every caller sees identical label data — names, group
// membership, and label IDs (CRI-252).
type issueNode struct {
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
		Nodes []labelNode `json:"nodes"`
	} `json:"labels"`
}

type labelNode struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Parent struct {
		Name    string `json:"name"`
		IsGroup bool   `json:"isGroup"`
	} `json:"parent"`
}

func (n issueNode) toIssue() Issue {
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
		if l.ID != "" {
			issue.LabelIDs = append(issue.LabelIDs, l.ID)
		}
		if l.Parent.IsGroup && l.Parent.Name != "" {
			if issue.LabelGroups == nil {
				issue.LabelGroups = make(map[string]string, len(n.Labels.Nodes))
			}
			issue.LabelGroups[l.Name] = l.Parent.Name
		}
	}
	return issue
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
                        labels { nodes { id name parent { name isGroup } } } }
            }
        }`,
		Variables: map[string]interface{}{
			"project": projectID,
			"states":  states,
		},
	}
	var result struct {
		Issues struct {
			Nodes []issueNode `json:"nodes"`
		} `json:"issues"`
	}
	if err := c.do(ctx, req, &result); err != nil {
		return nil, err
	}
	out := make([]Issue, 0, len(result.Issues.Nodes))
	for _, n := range result.Issues.Nodes {
		out = append(out, n.toIssue())
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

// IssuesWithLabel returns the issues in the given project carrying the
// named label, regardless of workflow state. It backs the orphaned
// automation-label sweep (CRI-220), which must reach tickets the
// route-declared state list does not cover — the intake workflow moves
// tickets out of the watched states while their runs execute, so an
// orphaned marker usually sits on a ticket in another state. An empty
// label name queries nothing and reports no issues: fail closed.
func (c *Client) IssuesWithLabel(ctx context.Context, projectID, labelName string) ([]Issue, error) {
	if labelName == "" {
		return nil, nil
	}
	req := graphqlRequest{
		Query: `query($project: ID!, $label: String!) {
            issues(filter: {project: {id: {eq: $project}}, labels: {some: {name: {eq: $label}}}}) {
                nodes { id identifier labels { nodes { id name } } }
            }
        }`,
		Variables: map[string]interface{}{
			"project": projectID,
			"label":   labelName,
		},
	}
	var result struct {
		Issues struct {
			Nodes []struct {
				ID         string `json:"id"`
				Identifier string `json:"identifier"`
				Labels     struct {
					Nodes []labelNode `json:"nodes"`
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
			ID:         n.ID,
			Identifier: n.Identifier,
		}
		for _, l := range n.Labels.Nodes {
			issue.Labels = append(issue.Labels, l.Name)
			if l.ID != "" {
				issue.LabelIDs = append(issue.LabelIDs, l.ID)
			}
		}
		out = append(out, issue)
	}
	return out, nil
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

// IssuesByIdentifiers resolves the given human-readable issue identifiers
// (e.g. "CRI-1") with one batched query (CRI-252): Linear's
// issues(id: {in: $ids}) filter accepts the same identifier values as the
// per-ticket issue(id:) lookup, so the watcher resolves every runs-index
// ticket in a single request per poll instead of one request per ticket.
// Unknown identifiers are simply absent from the result. Identifiers are
// queried in batches of linearIssuesBatch. The returned issues carry the
// same label data — names, group membership, and label IDs — as the
// state listing query. An empty input queries nothing.
func (c *Client) IssuesByIdentifiers(ctx context.Context, identifiers []string) ([]Issue, error) {
	if len(identifiers) == 0 {
		return nil, nil
	}
	out := make([]Issue, 0, len(identifiers))
	for start := 0; start < len(identifiers); start += linearIssuesBatch {
		end := min(start+linearIssuesBatch, len(identifiers))
		batch := identifiers[start:end]
		req := graphqlRequest{
			Query: `query($ids: [ID!]!, $first: Int!) {
            issues(filter: {id: {in: $ids}}, first: $first) {
                nodes { id identifier title description state { name } project { id name }
                        labels { nodes { id name parent { name isGroup } } } }
            }
        }`,
			Variables: map[string]interface{}{
				"ids":   batch,
				"first": len(batch),
			},
		}
		var result struct {
			Issues struct {
				Nodes []issueNode `json:"nodes"`
			} `json:"issues"`
		}
		if err := c.do(ctx, req, &result); err != nil {
			return nil, err
		}
		for _, n := range result.Issues.Nodes {
			out = append(out, n.toIssue())
		}
	}
	return out, nil
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

// SetIssueLabelIDs replaces the issue's label set. Linear labelIds have
// REPLACE semantics: callers must pass the full desired set, never just
// the delta (CRI-219). The watcher computes the desired set from the label
// data already read in the same poll (CRI-252), so a write needs no
// preceding per-issue label read; the write is skipped entirely when the
// issue's label set already matches the desired one.
func (c *Client) SetIssueLabelIDs(ctx context.Context, issueID string, labelIDs []string) error {
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
