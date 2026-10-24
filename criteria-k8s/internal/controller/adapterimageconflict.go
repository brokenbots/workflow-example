// Package controller: adapter-image conflict enforcement (KB-319). The
// adapter-image resolution chain is pinned metadata (spec.workflow
// adapterImages) → digest-verified event image_reference → unpinned
// metadata → operator defaults. When the two pinned lanes disagree on one
// kind, the stamped ImageConflictPolicy arbitrates; the fail-closed
// policy (the default, and the posture for any unregistered value) parks
// the run failed at the consult instead of silently executing one lane's
// choice. A parked run is terminal — the shepherd owns any refire
// decision (KB-302 symmetry).
package controller

import (
	"context"
	"fmt"
	"sort"

	logr "github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/events"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/jobbuilder"
)

// adapterImageConflictPolicyArbitrated reports whether the run's stamped
// conflict policy hands the arbitration to the resolver (a non-fail-closed
// registered policy) instead of refusing up front. Any policy value that
// is not explicitly lenient resolves fail-closed — the posture for an
// unset stamp and for values this binary predates.
func adapterImageConflictPolicyArbitrated(run *criteriav1.CriteriaRun) bool {
	wf := run.Spec.Workflow
	if wf == nil {
		return false
	}
	switch wf.ImageConflictPolicy {
	case criteriav1.ImageConflictPolicyPreferEvent, criteriav1.ImageConflictPolicyPreferMetadata:
		return true
	default:
		return false
	}
}

// adapterImageConflictDetail is the fail-fast detail for the first
// fail-closed adapter-image conflict across the run's active provision
// scopes. Detection consumes the MATERIALIZED workflow spec — the same
// spec the builder's plan is built from — so the consult and the
// resolution decide off one spec state. "" admits the reconcile.
func adapterImageConflictDetail(run *criteriav1.CriteriaRun, lifecycle []events.LifecycleEvent) string {
	if adapterImageConflictPolicyArbitrated(run) {
		return ""
	}
	details := []string{}
	for _, scope := range events.ActiveProvisions(lifecycle) {
		conflict := jobbuilder.DetectAdapterImageConflict(run.Spec.Workflow, scope, localAdapterKind(scope))
		if conflict == nil {
			continue
		}
		details = append(details, fmt.Sprintf(
			"scope %q adapter %q (kind %q): the workflow pins %q but the castle event pins %q — ImageConflictPolicy is fail-closed",
			scope.ScopeID, scope.AdapterName, conflict.Kind, conflict.MetadataRef, conflict.EventRef))
	}
	switch len(details) {
	case 0:
		return ""
	case 1:
		return details[0]
	default:
		sort.Strings(details)
		return fmt.Sprintf("%d adapter-image conflicts (%s; %s)", len(details), details[0], details[1])
	}
}

// failAdapterImageConflict fails a run whose pinned metadata adapter image
// and digest-verified castle event image disagree on one kind under the
// fail-closed policy: child Jobs (and per-scope adapter pods) are torn
// down so nothing executes against an image the run's own declarations
// contradict, the run is marked Failed with the AdapterImageConflict
// condition, and the queue slot is released. Mirrors failConfigRenderDrift.
func (r *CriteriaRunReconciler) failAdapterImageConflict(ctx context.Context, run *criteriav1.CriteriaRun, update *criteriav1.CriteriaRun, detail string, logger logr.Logger) (ctrl.Result, error) {
	logger.Info("failing CriteriaRun on adapter image conflict", "criteriarun", run.Name, "detail", detail)
	// The failed phase and conflict condition persist before anything is
	// deleted: the run is terminal the moment this write lands, so its
	// castle archive record must exist BEFORE the runner evidence (the
	// child Jobs and their pods) is destroyed. A failed archive fails this
	// pass with the TerminalArchivePending condition and the persisted
	// failed status waits unarchived like any other terminal run.
	upsertCondition(update, metav1.Condition{
		Type:               ConditionAdapterImageConflict,
		Status:             metav1.ConditionTrue,
		Reason:             ConditionAdapterImageConflict,
		Message:            fmt.Sprintf("%s; the run is parked failed — refire decisions belong to the shepherd (KB-319, KB-302)", detail),
		ObservedGeneration: run.Generation,
	})
	update.Status.Phase = criteriav1.PhaseFailed
	if err := r.Status().Update(ctx, update); err != nil {
		return ctrl.Result{}, fmt.Errorf("marking CriteriaRun failed on adapter image conflict: %w", err)
	}
	// Release the queue slot so the next queued run is admitted (CRI-291:
	// it is picked up by its own requeue, not a nested reconcile).
	r.Queue.Release(run)
	if err := r.ensureTerminalArchived(ctx, update, nil, logger); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.deleteChildJobs(ctx, run); err != nil {
		return ctrl.Result{}, err
	}
	// Per-scope adapter pods dial the runner's shim; with the runner Job
	// deleted the shim is going away, so the adapters must not outlive it.
	if run.Spec.PerScopeSessions {
		if err := r.deleteRunAdapterPods(ctx, run, logger); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}
