// Package controller: criteria-run stall watchdog (KB-24). A run whose
// castle event stream stops showing step progress while its phase is still
// Running is failed so CriteriaRun reflects Failed instead of hanging in
// Running forever — the CRI-271 wedge signature: a reviewer-loop step whose
// adapter session died emits only heartbeats, the runner Job stays Active,
// and the ticket is parked silently with no error and no timeout.
package controller

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	logr "github.com/go-logr/logr"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/castle"
)

// ConditionStallWatchdog is the CriteriaRun condition type and the
// metav1.Condition reason stamped when the stall watchdog fails a run for
// no step progress (KB-24).
const ConditionStallWatchdog = "StallWatchdog"

// applyStallWatchdog stamps the run's last step progress onto the pending
// status update and reports whether the run must be failed for stalling
// (KB-24).
//
// Progress is the newest step-progress event in the run's castle stream
// (lifecycle envelopes plus the copilot adapter's agent-activity
// AdapterEvents — see castle.progressFromEnvelope; heartbeats, terminal
// envelopes and non-activity adapter chatter never count). A run with no
// step-progress event yet gets a baseline stamp
// of "now" on its first authoritative observation, so its watchdog clock
// starts when the controller first sees the run. A stalled run (older than
// the stall window while still Running) gets Phase=Failed plus a
// StallWatchdog condition on the update; the caller then fails the run via
// failRunStalled.
//
// The watchdog runs only on an authoritative observation (a known castle
// run, no observation error) and only for a run whose phase is still
// Running; a zero stall window disables it. Castle disabled means no
// observation, hence an inert watchdog: without the event stream there is
// no step-progress signal. The reconciler additionally spares deliberately
// idle runs — the run record's paused/stopped operator signals (CRI-208,
// see castleRunStatusIdle) — where absent progress is the engine's choice,
// not a wedge.
func (r *CriteriaRunReconciler) applyStallWatchdog(run *criteriav1.CriteriaRun, update *criteriav1.CriteriaRun, obs *castle.Observation, logger logr.Logger) bool {
	if r.StallWindow <= 0 {
		return false
	}
	progress := obs.LastProgress
	if progress.IsZero() {
		// No step event consumed yet (run not started in the stream, or a
		// stream of heartbeats only — the CRI-271 wedge signature). Stamp
		// the baseline once so the clock starts at the first authoritative
		// observation instead of being reset every pass, and judge
		// staleness against the preserved baseline: a run wedged before its
		// first step event must still trip the watchdog once the baseline
		// ages past the window.
		if update.Status.LastStepProgress == nil {
			now := metav1.NewTime(time.Now().UTC())
			update.Status.LastStepProgress = &now
			return false
		}
		progress = update.Status.LastStepProgress.Time
	} else {
		stamp := metav1.NewTime(progress)
		update.Status.LastStepProgress = &stamp
	}
	if update.Status.Phase != criteriav1.PhaseRunning {
		return false
	}
	if time.Since(progress) <= r.StallWindow {
		return false
	}
	logger.Info("failing CriteriaRun on step-progress stall",
		"lastProgress", progress.Format(time.RFC3339), "window", r.StallWindow.String())
	update.Status.Phase = criteriav1.PhaseFailed
	markStallWatchdog(update, run.Generation, progress, r.StallWindow)
	return true
}

// failRunStalled fails a run the stall watchdog caught (KB-24), mirroring
// failBaseImageMismatch (CRI-264): the child Jobs are deleted so the wedged
// runner stops occupying its admission slot, per-scope adapter pods are
// deleted so they do not outlive the runner's shim, the run is marked
// Failed with the StallWatchdog condition, and the queue slot is released:
// the watcher refires the run. Castle records no terminal for such a run
// (the engine is gone), so the controller also stops polling castle for a
// terminal on it (stallWatchdogFired).
func (r *CriteriaRunReconciler) failRunStalled(ctx context.Context, run *criteriav1.CriteriaRun, update *criteriav1.CriteriaRun, logger logr.Logger) (ctrl.Result, error) {
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
	if err := r.Status().Update(ctx, update); err != nil {
		return ctrl.Result{}, fmt.Errorf("marking CriteriaRun failed on step-progress stall: %w", err)
	}
	// Release the queue slot so the next queued run is admitted (CRI-291:
	// it is picked up by its own requeue, not a nested reconcile).
	r.Queue.Release(run)
	return ctrl.Result{}, nil
}

// markStallWatchdog upserts the StallWatchdog condition on the pending
// status update, preserving the transition time when the condition is
// already stamped (a later operator pass must not churn the timestamp).
func markStallWatchdog(update *criteriav1.CriteriaRun, generation int64, lastProgress time.Time, window time.Duration) {
	upsertCondition(update, metav1.Condition{
		Type:   ConditionStallWatchdog,
		Status: metav1.ConditionTrue,
		Reason: ConditionStallWatchdog,
		Message: fmt.Sprintf("no step progress for %s (last step event %s); the run is failed so the watcher can refire it (KB-24)",
			window.String(), lastProgress.UTC().Format(time.RFC3339)),
		ObservedGeneration: generation,
	})
}

// stallWatchdogFired reports whether the run's status carries a True
// StallWatchdog condition. Such a run was failed by the watchdog: its
// engine is gone, so castle will never record a terminal outcome and the
// terminal-completion polling must stop instead of looping forever.
func stallWatchdogFired(status *criteriav1.CriteriaRunStatus) bool {
	if status == nil {
		return false
	}
	for i := range status.Conditions {
		if status.Conditions[i].Type == ConditionStallWatchdog && status.Conditions[i].Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}
