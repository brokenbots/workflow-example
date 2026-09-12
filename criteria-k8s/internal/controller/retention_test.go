package controller

// Retention regression tests for CRI-135: the intake sweep must ignore
// events.ndjson (the engine owns it during the CRI-134 dual-write
// transition), while triage sweeping still considers it.

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	logr "github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func touchFile(t *testing.T, path string, mod time.Time) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("{}"), 0o644))
	require.NoError(t, os.Chtimes(path, mod, mod))
}

func touchDir(t *testing.T, path string, mod time.Time) {
	t.Helper()
	require.NoError(t, os.Chtimes(path, mod, mod))
}

// A fresh engine-owned events.ndjson must not keep an otherwise expired
// intake directory alive.
func TestRetentionIntakeIgnoresEventsFile(t *testing.T) {
	root := t.TempDir()
	stale := time.Now().Add(-8 * 24 * time.Hour)
	ticket := filepath.Join(root, "intake", "CRI-135")
	touchFile(t, filepath.Join(ticket, "approved-plan.json"), stale)
	touchFile(t, filepath.Join(ticket, "review-notes.md"), stale)
	touchFile(t, filepath.Join(ticket, "events.ndjson"), time.Now())
	touchDir(t, ticket, stale)

	s := &RetentionSweeper{DataRoot: root, Retention: 7 * 24 * time.Hour}
	s.sweep(logr.Discard())

	_, err := os.Stat(ticket)
	assert.True(t, os.IsNotExist(err), "expired intake dir must be swept despite the fresh events.ndjson")
}

// Triage sweeping is unchanged: a fresh events.ndjson keeps the directory.
func TestRetentionTriageStillConsidersEventsFile(t *testing.T) {
	root := t.TempDir()
	stale := time.Now().Add(-8 * 24 * time.Hour)
	ticket := filepath.Join(root, "triage", "CRI-135")
	touchFile(t, filepath.Join(ticket, "approved-plan.json"), stale)
	touchFile(t, filepath.Join(ticket, "events.ndjson"), time.Now())

	s := &RetentionSweeper{DataRoot: root, Retention: 7 * 24 * time.Hour}
	s.sweep(logr.Discard())

	_, err := os.Stat(ticket)
	assert.NoError(t, err, "triage dir with a fresh events.ndjson must be kept")
}

// A fresh intake artifact (approved-plan.json) keeps the directory, and
// expired intake directories are removed.
func TestRetentionIntakeArtifactsAndExpiry(t *testing.T) {
	root := t.TempDir()
	stale := time.Now().Add(-8 * 24 * time.Hour)

	active := filepath.Join(root, "intake", "CRI-100")
	touchFile(t, filepath.Join(active, "approved-plan.json"), time.Now())
	touchFile(t, filepath.Join(active, "events.ndjson"), stale)
	touchDir(t, active, stale)

	expired := filepath.Join(root, "intake", "CRI-99")
	touchFile(t, filepath.Join(expired, "review-notes.md"), stale)
	touchDir(t, expired, stale)

	s := &RetentionSweeper{DataRoot: root, Retention: 7 * 24 * time.Hour}
	s.sweep(logr.Discard())

	_, err := os.Stat(active)
	assert.NoError(t, err, "fresh intake artifact keeps the directory")
	_, err = os.Stat(expired)
	assert.True(t, os.IsNotExist(err), "expired intake dir must be removed")

	// The engine-owned events file never rescues an intake dir, but it is
	// not deleted either: the whole directory (including events.ndjson) is
	// removed together.
}

// Missing data roots are tolerated.
func TestRetentionMissingRootsAreTolerated(t *testing.T) {
	s := &RetentionSweeper{DataRoot: filepath.Join(t.TempDir(), "does-not-exist"), Retention: 7 * 24 * time.Hour}
	s.sweep(logr.Discard()) // must not panic or log errors
}