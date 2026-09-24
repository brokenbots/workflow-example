// Package controller: fail-closed admission for url-type workflow runs
// without a workflow source (KB-6). A CriteriaRun whose stamped workflow
// declares type "url" but carries no spec.workflowSource has no workflow
// source to fetch — the url contract says the URL is the source — and the
// image-mode fallback would silently run it on the operator's baked
// workflow image (observed: runner and repo-clone both pulled the baked
// Linear image and ImagePullBackOff'd on a cluster without it). Admission
// refuses such runs with an explicit reason instead of letting them fall
// through to the baked image. Runs without a stamped workflow keep the
// genuinely legacy (non-url) image-mode path, including its KB-3 legacy
// secret-volume gating.
package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	logr "github.com/go-logr/logr"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/routes"
)

// ConditionWorkflowSourceMissing is the CriteriaRun condition type and the
// metav1.Condition reason stamped when admission refuses a url-type run
// whose spec carries no workflowSource (KB-6). The refusal reason is named
// explicitly, so the condition type mirrors it.
const ConditionWorkflowSourceMissing = "WorkflowSourceMissing"

// workflowSourceMissing reports whether the run's stamped workflow declares
// the url source type while the run carries no spec.workflowSource (KB-6).
// Only a stamped url-type workflow triggers the gate: a run without one is
// the genuinely legacy (non-url) path, whose image-mode fallback stays
// intact, and a stamped workflowSource is the source-mode contract (CRI-231)
// working as designed.
func workflowSourceMissing(run *criteriav1.CriteriaRun) bool {
	wf := run.Spec.Workflow
	return wf != nil && wf.Type == routes.TypeURL && run.Spec.WorkflowSource == nil
}

// upsertCondition upserts a condition on the pending status update,
// preserving the transition time when the condition is already stamped at
// the same status (a later operator pass must not churn the timestamp).
func upsertCondition(update *criteriav1.CriteriaRun, cond metav1.Condition) {
	for i := range update.Status.Conditions {
		if update.Status.Conditions[i].Type != cond.Type {
			continue
		}
		if update.Status.Conditions[i].Status != cond.Status {
			cond.LastTransitionTime = metav1.Now()
		} else {
			cond.LastTransitionTime = update.Status.Conditions[i].LastTransitionTime
		}
		update.Status.Conditions[i] = cond
		return
	}
	cond.LastTransitionTime = metav1.Now()
	update.Status.Conditions = append(update.Status.Conditions, cond)
}

// refuseMissingWorkflowSource fails a run closed at admission (KB-6): any
// child Jobs pinning the silently-resolved baked image are deleted (they can
// only exist when an operator predating the gate admitted the run), the run
// is marked Failed with a WorkflowSourceMissing condition, and a warning
// event records the explicit refusal reason. The queue slot is released so
// the refused run never starves its repo queue; the watcher can re-fire the
// ticket once the routes declaration or the run spec declares the source.
func (r *CriteriaRunReconciler) refuseMissingWorkflowSource(ctx context.Context, run *criteriav1.CriteriaRun, logger logr.Logger) (ctrl.Result, error) {
	wf := run.Spec.Workflow
	message := fmt.Sprintf("workflow %q is type %q but spec.workflowSource is absent: the url contract says the URL is the workflow source, so there is no source to fetch and the run is refused instead of falling back to the baked workflow image; declare spec.workflowSource (or a type=%q workflow) and re-fire (KB-6)", wf.Name, wf.Type, routes.TypeImage)
	logger.Info("refusing to admit CriteriaRun: url-type workflow without spec.workflowSource",
		"workflow", wf.Name, "ticket", run.Spec.TicketID)
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
	update := run.DeepCopy()
	update.Status.Phase = criteriav1.PhaseFailed
	upsertCondition(update, metav1.Condition{
		Type:               ConditionWorkflowSourceMissing,
		Status:             metav1.ConditionTrue,
		Reason:             ConditionWorkflowSourceMissing,
		Message:            message,
		ObservedGeneration: run.Generation,
	})
	if r.Recorder != nil {
		r.Recorder.Event(run, corev1.EventTypeWarning, eventReasonWorkflowSourceMissing, message)
	}
	if err := r.Status().Update(ctx, update); err != nil {
		return ctrl.Result{}, fmt.Errorf("marking CriteriaRun failed on missing workflowSource: %w", err)
	}
	// Release any admission slot the run holds (it never enqueues on the
	// gate's own pass, but a pre-gate operator may have admitted it) so the
	// next queued run is admitted; it is picked up by its own requeue, not a
	// nested reconcile (CRI-291).
	r.Queue.Release(run)
	return ctrl.Result{}, nil
}