package controller

// KB-314 leg-4 backfill: every terminal CriteriaRun that predates the
// archive gate gets its castle archive record BEFORE #309's retention can
// delete the castle history it documents. The run is the same mechanism the
// reconciler uses (ensureTerminalArchived), so backfilled records are
// byte-identical to gate-produced ones and re-running is a no-op on two
// levels: a run already carrying Status.ArchiveID is skipped, and castle
// re-archives the same key idempotently (latest wins). Batched through
// k8s List paging and status-stamped per run, an interrupted backfill
// resumes where it left off.

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/castle"
)

// BackfillResult summarizes one backfill pass.
type BackfillResult struct {
	// TerminalRuns is the count of terminal CriteriaRuns seen across every
	// list page.
	TerminalRuns int
	// AlreadyArchived is the count skipped because the gate had already
	// stamped an archive id (crash-and-resume safety).
	AlreadyArchived int
	// Archived is the count of archive records this pass landed (including
	// no-op re-archives landing on an existing key).
	Archived int
	// Failed is the count whose archive landing failed after the gate's
	// fallback path — each carries the TerminalArchivePending condition for
	// the on-reconcile gate to retry.
	Failed int
}

// BackfillRunArchives walks every CriteriaRun in the namespace and archives
// each terminal one that is not already stamped. Enrichment uses the
// castle client when the run's castle id (or runner job) is still
// discoverable; unenrichable runs archive on CR-derived fields with the
// cr-uid key. Per-run failures are logged and counted, never aborting the
// walk — a backfill that stops on the first outage leaves the rest of the
// fleet unarchived — and the caller decides whether partial coverage is
// acceptable (the result carries the split).
//
// The archiver may be disabled (no castle configured): the pass then only
// counts, archiving nothing — running backfill without an archive leg is a
// coverage report, not a silent no-op.
func BackfillRunArchives(ctx context.Context, c client.Client, archiver castle.RunArchiver, enricher castle.RunSource, namespace, operatorVersion string, logger logr.Logger) (BackfillResult, error) {
	var res BackfillResult
	// The gate logic (record build, ceilings, cr-uid fallback, pending
	// stamping) is the reconciler's — a shallow reconciler bound to this
	// client reuses it exactly.
	reconciler := &CriteriaRunReconciler{
		Client:          c,
		RunArchiver:     archiver,
		OperatorVersion: operatorVersion,
	}
	list := &criteriav1.CriteriaRunList{}
	for {
		if err := c.List(ctx, list, client.InNamespace(namespace), client.Continue(list.ListMeta.Continue)); err != nil {
			return res, fmt.Errorf("listing CriteriaRuns for run-archive backfill: %w", err)
		}
		for i := range list.Items {
			run := &list.Items[i]
			if !isTerminalPhase(run.Status.Phase) {
				continue
			}
			res.TerminalRuns++
			if run.Status.ArchiveID != "" {
				res.AlreadyArchived++
				continue
			}
			if archiver == nil || archiver.Disabled() {
				// No archive leg: leave the count as the coverage report.
				continue
			}
			obs := enrichBackfillRun(ctx, enricher, run, logger)
			if err := reconciler.ensureTerminalArchived(ctx, run, obs, logger); err != nil {
				res.Failed++
				continue
			}
			res.Archived++
			logger.Info("backfill archived terminal CriteriaRun",
				"criteriarun", run.Name, "phase", run.Status.Phase,
				"castleRunID", run.Status.CastleRunID)
		}
		if list.ListMeta.Continue == "" {
			break
		}
	}
	return res, nil
}

// enrichBackfillRun best-effort enriches a backfill record from the run's
// castle observation (workflow name/ref/digest, failure reason, timestamps,
// step summary). A terminal run's runner job is usually already deleted and
// its castle run may be beyond discovery; any such gap degrades to CR-only
// fields, never blocks the archive.
func enrichBackfillRun(ctx context.Context, enricher castle.RunSource, run *criteriav1.CriteriaRun, logger logr.Logger) *castle.Observation {
	obs := &castle.Observation{}
	if enricher == nil || enricher.Disabled() {
		return obs
	}
	knownRunID := run.Status.CastleRunID
	runnerJob := run.Status.JobName
	if knownRunID == "" && runnerJob == "" {
		return obs
	}
	enriched, err := enricher.Observe(ctx, runnerJob, knownRunID)
	if err != nil {
		logger.Info("backfill observation unavailable; archiving CR-derived fields only",
			"criteriarun", run.Name, "castleRunID", knownRunID, "error", err)
		return obs
	}
	return enriched
}

// CountMissingRunArchives reports how many terminal CriteriaRuns still lack
// an archive id — the backfill's coverage gate (all terminal CRs covered
// means zero) and the operator's re-run diagnostic.
func CountMissingRunArchives(ctx context.Context, c client.Client, namespace string) (int, error) {
	list := &criteriav1.CriteriaRunList{}
	if err := c.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return 0, fmt.Errorf("listing CriteriaRuns for archive coverage: %w", err)
	}
	missing := 0
	for i := range list.Items {
		if isTerminalPhase(list.Items[i].Status.Phase) && list.Items[i].Status.ArchiveID == "" {
			missing++
		}
	}
	return missing, nil
}
