// Package controller: CRITERIA_BASE_IMAGE / DEFAULT_CRITERIA_IMAGE
// admission-race handling (CRI-264).
package controller

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	logr "github.com/go-logr/logr"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/jobbuilder"
)

// ConditionBaseImageMismatch is the CriteriaRun condition type and the
// metav1.Condition reason stamped when a run is failed on a runner-image
// mismatch (CRI-264). The ticket names the reason explicitly, so the
// condition type mirrors it.
const ConditionBaseImageMismatch = "BaseImageMismatch"

// OperatorEnvProbe resolves operator env values as the cluster currently
// declares them. The reconciling pod's own process env goes stale while the
// operator Deployment rolls out — leader election is disabled, so old and
// new operator pods both reconcile during a rollout — so the runner image
// is resolved from the live Deployment env instead of the process env,
// which is what makes the fix work from either pod (CRI-264).
type OperatorEnvProbe interface {
	// OperatorEnv returns the requested env vars' current values as the
	// cluster declares them. Keys the declaration does not set are absent
	// from the result. An error means the probe could not read the live
	// declaration (the Deployment is absent, RBAC is missing, the API is
	// unreachable) and callers must degrade.
	OperatorEnv(ctx context.Context, keys ...string) (map[string]string, error)
}

// DeploymentEnvProbe reads env values off an appsv1 Deployment's container
// specs. It implements OperatorEnvProbe.
type DeploymentEnvProbe struct {
	Client     client.Client
	Namespace  string
	Deployment string
}

// OperatorEnv returns the requested literal env values declared by every
// container of the Deployment. valueFrom env entries are skipped (the
// operator's image envs are literal values); the first container setting a
// key wins, which is the single container the operator image envs live on
// today.
func (p *DeploymentEnvProbe) OperatorEnv(ctx context.Context, keys ...string) (map[string]string, error) {
	var deploy appsv1.Deployment
	if err := p.Client.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: p.Deployment}, &deploy); err != nil {
		return nil, fmt.Errorf("reading operator deployment %s/%s: %w", p.Namespace, p.Deployment, err)
	}
	wanted := make(map[string]bool, len(keys))
	for _, k := range keys {
		wanted[k] = true
	}
	env := make(map[string]string, len(keys))
	for i := range deploy.Spec.Template.Spec.Containers {
		for _, e := range deploy.Spec.Template.Spec.Containers[i].Env {
			if e.ValueFrom != nil || !wanted[e.Name] {
				continue
			}
			if _, set := env[e.Name]; !set && e.Value != "" {
				env[e.Name] = e.Value
			}
		}
	}
	return env, nil
}

// resolveRunnerDefaults resolves the defaults the reconciler builds child
// Jobs from for this pass, probing the live operator env first (CRI-264).
// The returned bool reports whether the resolution was verified against the
// cluster's declared env; base-image mismatch enforcement runs only on a
// verified resolution:
//   - no probe configured: the process env is the only signal — the
//     resolution degrades to the process-env defaults and mismatch checks
//     still run against them (documented degraded mode);
//   - probe read error: the live declaration is unverifiable — the
//     resolution degrades to the process-env defaults and enforcement is
//     deferred to a later pass rather than failing runs off unverified data.
func (r *CriteriaRunReconciler) resolveRunnerDefaults(ctx context.Context, run *criteriav1.CriteriaRun, logger logr.Logger) (jobbuilder.Defaults, bool) {
	defaults := r.Defaults
	if r.EnvProbe == nil {
		return defaults, true
	}
	env, err := r.EnvProbe.OperatorEnv(ctx, jobbuilder.EnvCriteriaBaseImage, jobbuilder.EnvDefaultImage)
	if err != nil {
		logger.Error(err, "probing the operator deployment env failed; using process-env defaults and deferring base-image mismatch checks")
		return defaults, false
	}
	if v := env[jobbuilder.EnvCriteriaBaseImage]; v != "" {
		defaults.CriteriaBaseImage = v
	}
	if v := env[jobbuilder.EnvDefaultImage]; v != "" {
		defaults.Image = v
	}
	return defaults, true
}

// runnerImageMismatch reports whether the runner image the run's children
// were pinned with disagrees with the operator's current resolution
// (CRI-264). The status.baseImage stamp records the resolution at
// admission; the pinned image on the runner Job's runner container is the
// ground truth that covers stamp-less Jobs admitted by pre-fix operators
// and status writes that never landed. The run spec's own image pin is
// part of the resolution chain on both sides, so a spec-pinned run never
// mismatches.
func runnerImageMismatch(status *criteriav1.CriteriaRunStatus, runnerJob *batchv1.Job, current string) (pinned string, mismatch bool) {
	if stamped := status.BaseImage; stamped != "" && stamped != current {
		return stamped, true
	}
	if job := jobbuilder.RunnerJobImage(runnerJob); job != "" && job != current {
		return job, true
	}
	return "", false
}

// failBaseImageMismatch fails a run whose child Jobs pin a runner image the
// operator no longer resolves (CRI-264): the runner Job's pod template is
// immutable, so a mid-flight image change cannot be applied in place —
// letting the Job continue would replay the stale image through k8s
// backoff until BackoffLimitExceeded (the 30-90m churn the ticket
// documents). The child Jobs are deleted so nothing keeps running on an
// image the cluster no longer declares, the run is marked Failed with a
// BaseImageMismatch condition, and the queue slot is released: the watcher
// refires the run on the current image once the rollout settles.
func (r *CriteriaRunReconciler) failBaseImageMismatch(ctx context.Context, run *criteriav1.CriteriaRun, pinned, current string, logger logr.Logger) (ctrl.Result, error) {
	logger.Info("failing CriteriaRun on runner-image mismatch", "pinned", pinned, "current", current)
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
	markBaseImageMismatch(update, run.Generation, pinned, current)
	if err := r.Status().Update(ctx, update); err != nil {
		return ctrl.Result{}, fmt.Errorf("marking CriteriaRun failed on base-image mismatch: %w", err)
	}
	if next := r.Queue.Release(run); next != nil {
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: *next}); err != nil {
			logger.Error(err, "reconciling next queued CriteriaRun", "next", *next)
		}
	}
	return ctrl.Result{}, nil
}

// markBaseImageMismatch upserts the BaseImageMismatch condition on the
// status update, preserving the transition time when the condition is
// already stamped (a later operator pass must not churn the timestamp).
func markBaseImageMismatch(update *criteriav1.CriteriaRun, generation int64, pinned, current string) {
	cond := metav1.Condition{
		Type:               ConditionBaseImageMismatch,
		Status:             metav1.ConditionTrue,
		Reason:             ConditionBaseImageMismatch,
		Message:            fmt.Sprintf("runner image pinned as %q at admission but the operator now resolves %q; the run is failed so the watcher can refire on the current image (CRI-264)", pinned, current),
		ObservedGeneration: generation,
	}
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

// deleteChildJobs deletes the child Jobs the job builders would reconcile
// for run, foreground-propagating so their pods die with them. Shared by
// the finalize path and the base-image mismatch fail-fast (CRI-264): the
// job names derive from the run identity only, so the desired set's
// namespaces and names are stable regardless of defaults.
func (r *CriteriaRunReconciler) deleteChildJobs(ctx context.Context, run *criteriav1.CriteriaRun) error {
	for _, desired := range jobbuilder.BuildAll(run, r.Defaults) {
		var job batchv1.Job
		err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, &job)
		if err == nil {
			if err := r.Delete(ctx, &job, client.PropagationPolicy(metav1.DeletePropagationForeground)); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("deleting child job %s: %w", desired.Name, err)
			}
		} else if !apierrors.IsNotFound(err) {
			return fmt.Errorf("getting child job %s for deletion: %w", desired.Name, err)
		}
	}
	return nil
}
