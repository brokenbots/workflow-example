package controller

import (
	"context"
	"os"
	"path/filepath"
	"time"

	logr "github.com/go-logr/logr"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// RetentionSweeper periodically removes per-ticket intake and triage
// directories older than the retention window so the data PVC stays bounded.
//
// Sweep policy:
//   - /data/intake/<TICKET> is removed after Retention (default 7d) from the
//     newest mtime among the directory itself and its well-known artifact
//     files (approved-plan.json, review-notes.md). events.ndjson is ignored:
//     the engine owns that file (CRI-134 dual-write keeps it on the PVC for
//     debugging), so it no longer reflects operator-visible activity.
//   - /data/triage/<TICKET> follows the same rule and still considers
//     events.ndjson.
//   - Castle's own database files live at /data root and are never touched.
type RetentionSweeper struct {
	// DataRoot is the path backing the criteria-data PVC (default /data).
	DataRoot string
	// Retention keeps run artifacts reviewable for this long after the last
	// write to the ticket directory.
	Retention time.Duration
	// Interval between sweeps (default 1h).
	Interval time.Duration
}

func NewRetentionSweeper(dataRoot string, retention, interval time.Duration) *RetentionSweeper {
	if retention <= 0 {
		retention = 7 * 24 * time.Hour
	}
	if interval <= 0 {
		interval = time.Hour
	}
	if dataRoot == "" {
		dataRoot = "/data"
	}
	return &RetentionSweeper{DataRoot: dataRoot, Retention: retention, Interval: interval}
}

func (s *RetentionSweeper) Run(ctx context.Context) {
	logger := logf.FromContext(ctx).WithValues("component", "retention-sweep")
	ticker := time.NewTicker(s.Interval)
	defer ticker.Stop()

	// Sweep once at startup so a long backlog clears promptly.
	s.sweep(logger)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sweep(logger)
		}
	}
}

func (s *RetentionSweeper) sweep(logger logr.Logger) {
	cutoff := time.Now().Add(-s.Retention)
	removed, kept := 0, 0

	for _, target := range []struct {
		root   string
		intake bool
	}{
		{filepath.Join(s.DataRoot, "intake"), true},
		{filepath.Join(s.DataRoot, "triage"), false},
	} {
		entries, err := os.ReadDir(target.root)
		if err != nil {
			if !os.IsNotExist(err) {
				logger.Error(err, "reading retention root", "root", target.root)
			}
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			dir := filepath.Join(target.root, entry.Name())
			info, err := os.Stat(dir)
			if err != nil {
				continue
			}
			// The directory mtime only updates on direct-child churn; a run
			// writes deep into its tree, so also consider the newest mtime of
			// the well-known artifact files. events.ndjson is excluded for
			// intake: the engine owns it during the dual-write transition, so
			// engine-side writes alone must not keep an operator-expired
			// directory alive.
			candidates := artifactFiles(target.intake)
			if maxMod(dir, info.ModTime(), candidates).After(cutoff) {
				kept++
				continue
			}
			if err := os.RemoveAll(dir); err != nil {
				logger.Error(err, "removing expired run directory", "dir", dir)
				continue
			}
			removed++
			logger.Info("swept expired run artifacts", "dir", dir, "age", time.Since(info.ModTime()).Round(time.Hour))
		}
	}
	logger.Info("retention sweep complete", "removed", removed, "kept", kept, "retention", s.Retention.String())
}

// artifactFiles returns the well-known artifact file names whose mtimes keep
// a ticket directory alive. events.ndjson only counts for triage; for intake
// the engine owns it (CRI-134 dual-write) and the operator must not extend
// retention based on engine-side writes.
func artifactFiles(intake bool) []string {
	if intake {
		return []string{"approved-plan.json", "review-notes.md"}
	}
	return []string{"events.ndjson", "approved-plan.json", "review-notes.md"}
}

// maxMod returns the newest mtime among dir itself and a bounded set of
// well-known artifact files inside it. It avoids a full recursive walk while
// still respecting active runs that append to files created early in the run.
func maxMod(dir string, base time.Time, candidates []string) time.Time {
	recent := base
	for _, name := range candidates {
		c := filepath.Join(dir, name)
		if info, err := os.Stat(c); err == nil && info.ModTime().After(recent) {
			recent = info.ModTime()
		}
	}
	return recent
}
