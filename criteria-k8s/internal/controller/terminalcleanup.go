// Package controller: terminal-cleanup restart guard (KB-302).
//
// The KB-275 terminal reaper deletes a terminal run's child Jobs, but the
// runner pod's containers can still be mid-flight when that delete lands: the
// runner restarts on failure (jobbuilder sets RestartPolicy=OnFailure), and
// the kubelet starts a fresh agent container right through the background
// garbage collection of the deleted Job. That orphan agent registers with
// castle, emits one provision_wanted, and dies "failed to provision" — the
// corpse class the field kept seeing. So terminal cleanup delays the Job
// delete until the runner pod is settled — no container is running, none is
// between restart attempts, and none already exited nonzero under a restart
// policy that will start another attempt — reaping after a bounded force
// window so a pod that never settles cannot wedge the cleanup forever.
package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	logr "github.com/go-logr/logr"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/jobbuilder"
)

// ConditionTerminalCleanup is the CriteriaRun condition type stamped while
// terminal child-job deletion is deferred behind a pending runner restart
// (KB-302). The condition carries the deferral reason in its message and —
// since its LastTransitionTime is preserved while it stays True at the same
// reason — doubles as the deferral start stamp the force window reads.
const ConditionTerminalCleanup = "TerminalCleanup"

// ReasonTerminalRestartPending is the metav1.Condition reason for the
// TerminalCleanup condition: the reap is deferred because the runner pod can
// still start another container attempt.
const ReasonTerminalRestartPending = "runnerRestartPending"

// terminalReapRequeueInterval is the requeue used while a terminal run's
// child-job deletion is deferred behind a runner restart. It is shorter than
// perScopeRequeueInterval so the follow-up reap lands promptly after the
// restart resolves.
const terminalReapRequeueInterval = 5 * time.Second

// DefaultTerminalReapForceAfter is the default bounded window after which a
// deferred terminal reap is forced past a still-unsettled runner pod. The
// kubelet's shortest crash-loop backoff is a few seconds and its cap is
// minutes, so this window is long enough to let live-but-failing agents
// finish and short enough that a never-settling pod cannot wedge a queue
// slot; it is tunable with the TERMINAL_REAP_FORCE_AFTER env.
const DefaultTerminalReapForceAfter = 90 * time.Second

// runnerJobNameForRun resolves the runner Job of run from its stamped
// status; the Job name is derivable from the run identity, so the status
// stamp and the derived name agree in practice — prefer the stamp, fall back
// to the derivation.
func runnerJobNameForRun(run *criteriav1.CriteriaRun) string {
	if run.Status.JobName != "" {
		return run.Status.JobName
	}
	return jobbuilder.RunnerJobName(run)
}

// runnerRestartPending reports whether the run's runner pod can still start
// another container attempt (KB-302). A pod that can is a deletion hazard:
// deleting the terminal run's runner Job in background propagation does not
// stop the kubelet from starting an already-scheduled restart attempt, and
// the fresh agent container registers with castle mid-teardown — the corpse
// generator. Settled states reap immediately:
//
//   - no runner pod at all (the common fixture path: Job deleted or never
//     created, nothing to restart);
//   - the pod is already terminating (DeletionTimestamp set — the kubelet
//     owns its teardown, no further start attempt happens);
//   - the pod reached a terminal phase (Succeeded/Failed);
//   - every container is terminated, with exit code 0 or under a
//     restart policy that starts no further attempt (Never).
//
// Everything else defers the reap: a running container may exit nonzero and
// take a restart attempt with it, a Waiting container either has not started
// yet (a start attempt can still land) or sits between attempts after a
// prior crash, and a nonzero-terminated container under OnFailure has a
// restart attempt incoming.
func (r *CriteriaRunReconciler) runnerRestartPending(ctx context.Context, run *criteriav1.CriteriaRun, logger logr.Logger) (bool, string) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(run.Namespace),
		client.MatchingLabels(map[string]string{
			jobbuilder.LabelRun:  run.Name,
			jobbuilder.LabelRole: jobbuilder.RoleRunner,
		}),
	); err != nil {
		// A listing failure is not cleanup-worthy evidence either way: fail
		// the guard closed (defer), so an API blip cannot delete Jobs under
		// an unseen restart. The caller requeues and the next pass retries.
		logger.Error(err, "listing runner pods for the terminal-restart guard", "CriteriaRun", run.Name)
		return true, fmt.Sprintf("runner pod list failed: %v", err)
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if waitReason := podRestartPending(pod); waitReason != "" {
			return true, waitReason
		}
	}
	return false, ""
}

// podRestartPending returns the wait-reasons why pod can still start another
// container attempt, or "" when the pod is settled. See runnerRestartPending
// for the settled/deferral state table.
func podRestartPending(pod *corev1.Pod) string {
	if pod == nil {
		return ""
	}
	if pod.DeletionTimestamp != nil {
		// Already terminating: the kubelet owns its teardown and starts no
		// further attempt, no matter the restart policy.
		return ""
	}
	switch pod.Status.Phase {
	case corev1.PodSucceeded, corev1.PodFailed:
		return ""
	}
	noFurtherRestart := pod.Spec.RestartPolicy == corev1.RestartPolicyNever
	for _, statuses := range [][]corev1.ContainerStatus{
		pod.Status.InitContainerStatuses,
		pod.Status.ContainerStatuses,
	} {
		for i := range statuses {
			cs := statuses[i]
			name := pod.Name + "/" + cs.Name
			if term := cs.State.Terminated; term != nil {
				if term.ExitCode == 0 || noFurtherRestart {
					continue
				}
				return fmt.Sprintf("container %s exited with code %d; an %s restart will follow", name, term.ExitCode, pod.Spec.RestartPolicy)
			}
			if cs.State.Running != nil {
				return fmt.Sprintf("container %s is running", name)
			}
			if cs.State.Waiting != nil {
				if cs.LastTerminationState.Terminated != nil {
					return fmt.Sprintf("container %s is between restart attempts (%s, restarts=%d)", name, cs.State.Waiting.Reason, cs.RestartCount)
				}
				return fmt.Sprintf("container %s has not started yet (%s, restarts=%d); a start attempt can still land", name, cs.State.Waiting.Reason, cs.RestartCount)
			}
			// No state at all: a start attempt can still land.
			return fmt.Sprintf("container %s has no observed state yet; a start attempt can still land", name)
		}
	}
	return ""
}

// terminalCleanupCondition returns run's persisted TerminalCleanup condition
// when one is stamped at status True.
func terminalCleanupCondition(run *criteriav1.CriteriaRun) (metav1.Condition, bool) {
	for i := range run.Status.Conditions {
		if run.Status.Conditions[i].Type == ConditionTerminalCleanup && run.Status.Conditions[i].Status == metav1.ConditionTrue {
			return run.Status.Conditions[i], true
		}
	}
	return metav1.Condition{}, false
}

// markTerminalCleanupDeferred stamps the TerminalCleanup condition on run
// (after a fresh re-read, so the write carries the run's latest
// resourceVersion) and reports whether the deferral has outlived the
// reconciler's force window. The condition's LastTransitionTime is preserved
// while it stays True, so it doubles as the deferral-start stamp.
func (r *CriteriaRunReconciler) markTerminalCleanupDeferred(ctx context.Context, run *criteriav1.CriteriaRun, reason string) (metav1.Condition, bool, error) {
	key := types.NamespacedName{Name: run.Name, Namespace: run.Namespace}
	var fresh criteriav1.CriteriaRun
	if err := r.Get(ctx, key, &fresh); err != nil {
		return metav1.Condition{}, false, fmt.Errorf("re-reading CriteriaRun for the TerminalCleanup condition: %w", err)
	}
	cond, ok := terminalCleanupCondition(&fresh)
	forced := r.TerminalReapForceAfter > 0 && time.Since(cond.LastTransitionTime.Time) > r.TerminalReapForceAfter
	update := fresh.DeepCopy()
	upsertCondition(update, metav1.Condition{
		Type:               ConditionTerminalCleanup,
		Status:             metav1.ConditionTrue,
		Reason:             ReasonTerminalRestartPending,
		Message:            fmt.Sprintf("terminal child-job deletion deferred: %s (KB-302)", reason),
		ObservedGeneration: fresh.Generation,
	})
	if !statusEqual(&fresh.Status, &update.Status) {
		if err := r.Status().Update(ctx, update); err != nil {
			return metav1.Condition{}, forced, fmt.Errorf("stamping TerminalCleanup condition: %w", err)
		}
	}
	return cond, ok && forced, nil
}

// clearTerminalCleanup removes the TerminalCleanup condition once terminal
// child-job deletion actually completed, so the deferral stamp never
// outlives the deferral it describes. No-op when none is stamped.
func (r *CriteriaRunReconciler) clearTerminalCleanup(ctx context.Context, run *criteriav1.CriteriaRun) error {
	key := types.NamespacedName{Name: run.Name, Namespace: run.Namespace}
	var fresh criteriav1.CriteriaRun
	if err := r.Get(ctx, key, &fresh); err != nil {
		return fmt.Errorf("re-reading CriteriaRun to clear the TerminalCleanup condition: %w", err)
	}
	if _, ok := terminalCleanupCondition(&fresh); !ok {
		return nil
	}
	update := fresh.DeepCopy()
	for i := range update.Status.Conditions {
		if update.Status.Conditions[i].Type == ConditionTerminalCleanup {
			update.Status.Conditions = append(update.Status.Conditions[:i], update.Status.Conditions[i+1:]...)
			break
		}
	}
	if err := r.Status().Update(ctx, update); err != nil {
		return fmt.Errorf("clearing TerminalCleanup condition: %w", err)
	}
	return nil
}
