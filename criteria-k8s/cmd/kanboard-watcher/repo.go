package main

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

// RepoValidator checks whether a short-form owner/repo reference names an existing
// repository. The watcher consults it before falling back to the default repo
// URL (the repo-label fallback the ticket-source integration carried).
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