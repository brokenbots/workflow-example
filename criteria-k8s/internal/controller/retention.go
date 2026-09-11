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
//     files (events.ndjson, approved-plan.json, review-notes.md).
//   - /data/triage/<TICKET> follows the same rule.
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

	for _, root := range []string{
		filepath.Join(s.DataRoot, "intake"),
		filepath.Join(s.DataRoot, "triage"),
	} {
		entries, err := os.ReadDir(root)
		if err != nil {
			if !os.IsNotExist(err) {
				logger.Error(err, "reading retention root", "root", root)
			}
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			dir := filepath.Join(root, entry.Name())
			info, err := os.Stat(dir)
			if err != nil {
				continue
			}
			// The directory mtime only updates on direct-child churn; a run
			// writes deep into its tree, so also consider the newest mtime of
			// the well-known artifact files.
			if maxMod(dir, info.ModTime()).After(cutoff) {
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

// maxMod returns the newest mtime among dir itself and a bounded set of
// well-known artifact files inside it. It avoids a full recursive walk while
// still respecting active runs that append to files created early in the run.
func maxMod(dir string, base time.Time) time.Time {
	candidates := []string{
		filepath.Join(dir, "events.ndjson"),
		filepath.Join(dir, "approved-plan.json"),
		filepath.Join(dir, "review-notes.md"),
	}
	recent := base
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && info.ModTime().After(recent) {
			recent = info.ModTime()
		}
	}
	return recent
}