package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestDefaultRepoValidator exercises the GitHub repo-existence validator the
// watcher consults before the default-repo fallback: an existing repo is
// validated against the live endpoint (with the bearer token attached), a
// missing repo fails closed.
func TestDefaultRepoValidator(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/repos/brokenbots/workflow-example", r.URL.Path)
		// Compare the Authorization shape rather than echoing the test token
		// verbatim: the header must be the validator's "Bearer <token>" form.
		assert.True(t, strings.HasPrefix(r.Header.Get("Authorization"), "Bearer "))
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	client := ts.Client()
	validator := DefaultRepoValidator(client, "test-token", ts.URL)
	assert.True(t, validator("brokenbots/workflow-example"))

	notFoundTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer notFoundTS.Close()

	missing := DefaultRepoValidator(notFoundTS.Client(), "test-token", notFoundTS.URL)
	assert.False(t, missing("owner/does-not-exist"))
}
