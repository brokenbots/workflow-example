// Package controller reconciles CriteriaRun resources into batch/v1 Jobs.
package controller

import (
	"context"
	"fmt"
	"reflect"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	logr "github.com/go-logr/logr"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/castle"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/jobbuilder"
)

const (
	criteriaRunFinalizer    = "criteriarun.criteria.brokenbots.dev/finalizer"
	perScopeRequeueInterval = 10 * time.Second
	queueRequeueInterval    = 5 * time.Second
)

// CriteriaRunReconciler reconciles a CriteriaRun object into a batch/v1 Job.
type CriteriaRunReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Castle observes run lifecycle from the castle control plane (CRI-133
	// API). Nil or disabled means observation is off; the reconciler then
	// relies purely on Job conditions for phase stamping.
	Castle    castle.RunSource
	Defaults  jobbuilder.Defaults
	Queue     *RunQueue
}

// +kubebuilder:rbac:groups=criteria.brokenbots.dev,resources=criteriaruns,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=criteria.brokenbots.dev,resources=criteriaruns/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=criteria.brokenbots.dev,resources=criteriaruns/finalizers,verbs=update
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;delete

func (r *CriteriaRunReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := logf.FromContext(ctx).WithValues("criteriarun", req.NamespacedName)

	var run criteriav1.CriteriaRun
	if err := r.Get(ctx, req.NamespacedName, &run); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Finalizer-driven cleanup: delete child Jobs when the CriteriaRun is deleted.
	if !run.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &run, logger)
	}

	if !controllerutil.ContainsFinalizer(&run, criteriaRunFinalizer) {
		controllerutil.AddFinalizer(&run, criteriaRunFinalizer)
		if err := r.Update(ctx, &run); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
		// Re-fetch so subsequent updates work against the latest resource.
		if err := r.Get(ctx, req.NamespacedName, &run); err != nil {
			return ctrl.Result{}, err
		}
	}

	// If the run is already terminal, do not re-enter the queue on a resync.
	// Release any stale admission slot and prompt the next queued run.
	if isTerminalPhase(run.Status.Phase) {
		if next := r.Queue.Release(req.NamespacedName, run.Spec.RepoURL); next != nil {
			if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: *next}); err != nil {
				logger.Error(err, "reconciling next queued CriteriaRun after terminal resync", "next", *next)
			}
		}
		return ctrl.Result{}, nil
	}

	// Enqueue the run for repo-keyed admission control. Only admitted runs
	// are allowed to create child Jobs.
	admitted, qstatus, prevRunning := r.Queue.Enqueue(&run)

	update := run.DeepCopy()
	update.Status.ObservedGeneration = run.Generation
	update.Status.EventsPath = eventsPath(&run)
	update.Status.Queue = qstatus

	if !admitted {
		update.Status.Phase = criteriav1.PhasePending
		update.Status.JobName = ""
		if !statusEqual(&run.Status, &update.Status) {
			logger.Info("queuing CriteriaRun", "repo", run.Spec.RepoURL, "position", qstatus.Position)
			if err := r.Status().Update(ctx, update); err != nil {
				return ctrl.Result{}, fmt.Errorf("updating queue status: %w", err)
			}
		}
		// Refresh the currently running run's queue status so it shows the
		// newly queued run in its pending list.
		if prevRunning != nil {
			if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: *prevRunning}); err != nil {
				logger.Error(err, "reconciling running CriteriaRun after enqueue", "running", *prevRunning)
			}
		}
		return ctrl.Result{RequeueAfter: queueRequeueInterval}, nil
	}

	desiredJobs := jobbuilder.BuildAll(&run, r.Defaults)

	var runnerJob *batchv1.Job
	for _, desired := range desiredJobs {
		var found batchv1.Job
		err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, &found)
		if err != nil && apierrors.IsNotFound(err) {
			logger.Info("creating child job", "job", desired.Name)
			if err := ctrl.SetControllerReference(&run, desired, r.Scheme); err != nil {
				return ctrl.Result{}, fmt.Errorf("setting controller reference: %w", err)
			}
			if err := r.Create(ctx, desired); err != nil {
				return ctrl.Result{}, fmt.Errorf("creating job %s: %w", desired.Name, err)
			}
			found = *desired
		} else if err != nil {
			return ctrl.Result{}, fmt.Errorf("getting job %s: %w", desired.Name, err)
		}
		if desired.Labels["criteria.brokenbots.dev/role"] == "runner" {
			runnerJob = &found
		}
	}

	if runnerJob == nil {
		return ctrl.Result{}, fmt.Errorf("no runner job found in desired set")
	}

	// Base status mirrors the runner Job, then the castle observation layers
	// on top: the castle run id, terminal completion (RunCompleted/RunFailed
	// from castle, not the events file), and the per-scope lifecycle events
	// consumed below. One status write.
	update.Status.Phase = derivePhase(runnerJob)
	update.Status.JobName = runnerJob.Name
	obs := r.observeCastle(ctx, &run, update, logger)

	if !statusEqual(&run.Status, &update.Status) {
		logger.Info("updating CriteriaRun status", "phase", update.Status.Phase, "jobName", update.Status.JobName)
		if err := r.Status().Update(ctx, update); err != nil {
			return ctrl.Result{}, fmt.Errorf("updating status: %w", err)
		}
	}

	phase := update.Status.Phase

	// Reconcile per-scope adapter pods from the castle event stream.
	activeAdapters, err := r.reconcilePerScopeAdapters(ctx, &run, obs.Lifecycle, logger)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Release the queue slot when the run has finished. If another run is
	// queued for the same repo, reconcile it so it can start promptly.
	if isTerminalPhase(phase) {
		if next := r.Queue.Release(client.ObjectKeyFromObject(&run), run.Spec.RepoURL); next != nil {
			if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: *next}); err != nil {
				logger.Error(err, "reconciling next queued CriteriaRun", "next", *next)
			}
		}
	}

	// Keep polling castle while the run is using per-scope adapters and has
	// not reached a terminal phase.
	if run.Spec.PerScopeSessions && (activeAdapters > 0 || !isTerminalPhase(phase)) {
		return ctrl.Result{RequeueAfter: perScopeRequeueInterval}, nil
	}

	return ctrl.Result{}, nil
}

func isTerminalPhase(phase criteriav1.CriteriaRunPhase) bool {
	return phase == criteriav1.PhaseSucceeded || phase == criteriav1.PhaseFailed
}

func derivePhase(job *batchv1.Job) criteriav1.CriteriaRunPhase {
	for _, c := range job.Status.Conditions {
		if c.Status == "True" {
			switch c.Type {
			case batchv1.JobComplete:
				return criteriav1.PhaseSucceeded
			case batchv1.JobFailed:
				return criteriav1.PhaseFailed
			}
		}
	}
	if job.Status.Active > 0 {
		return criteriav1.PhaseRunning
	}
	if job.Status.Succeeded > 0 {
		return criteriav1.PhaseSucceeded
	}
	if job.Status.Failed > 0 {
		return criteriav1.PhaseFailed
	}
	return criteriav1.PhasePending
}

// observeCastle observes the run's lifecycle from castle and layers it onto
// the pending status update: the castle run id, terminal completion, and the
// run outcome (PR number, ticket state). Transient castle failures are
// logged and skipped — castle observation must never block k8s actuation,
// and the reconcile cadence retries on the next pass.
func (r *CriteriaRunReconciler) observeCastle(ctx context.Context, run *criteriav1.CriteriaRun, update *criteriav1.CriteriaRun, logger logr.Logger) *castle.Observation {
	empty := &castle.Observation{}
	if r.Castle == nil || r.Castle.Disabled() {
		return empty
	}

	obs, err := r.Castle.Observe(ctx, run.Spec.TicketID, run.Status.CastleRunID)
	if err != nil {
		logger.Error(err, "observing run lifecycle from castle")
		return empty
	}

	if obs.RunID != "" && obs.RunID != run.Status.CastleRunID {
		update.Status.CastleRunID = obs.RunID
	}
	if obs.Terminal != nil {
		// Terminal state stamping comes from castle (RunCompleted/RunFailed),
		// keeping the same phase semantics the Job conditions use. Job
		// conditions remain the base phase so the queue can still release if
		// castle goes silent.
		if obs.Terminal.Success {
			update.Status.Phase = criteriav1.PhaseSucceeded
		} else {
			update.Status.Phase = criteriav1.PhaseFailed
		}
		update.Status.PRNumber = obs.Terminal.PRNumber
		update.Status.TicketState = obs.Terminal.TicketState
	}
	return obs
}

func eventsPath(run *criteriav1.CriteriaRun) string {
	return fmt.Sprintf("/data/intake/%s/events.ndjson", run.Spec.TicketID)
}

func (r *CriteriaRunReconciler) finalize(ctx context.Context, run *criteriav1.CriteriaRun, logger logr.Logger) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(run, criteriaRunFinalizer) {
		return ctrl.Result{}, nil
	}
	logger.Info("finalizing CriteriaRun")

	// Release the queue slot before deleting the run so the next queued run
	// can be admitted.
	if next := r.Queue.Release(client.ObjectKeyFromObject(run), run.Spec.RepoURL); next != nil {
		defer func() {
			if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: *next}); err != nil {
				logger.Error(err, "reconciling next queued CriteriaRun", "next", *next)
			}
		}()
	}

	desiredJobs := jobbuilder.BuildAll(run, r.Defaults)
	for _, desired := range desiredJobs {
		var job batchv1.Job
		err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, &job)
		if err == nil {
			if err := r.Delete(ctx, &job, client.PropagationPolicy(metav1.DeletePropagationForeground)); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("deleting child job %s: %w", desired.Name, err)
			}
		} else if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("getting child job %s for deletion: %w", desired.Name, err)
		}
	}

	controllerutil.RemoveFinalizer(run, criteriaRunFinalizer)
	if err := r.Update(ctx, run); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

// SetupWithManager wires the reconciler and watches child Jobs.
func (r *CriteriaRunReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&criteriav1.CriteriaRun{}).
		Watches(
			&batchv1.Job{},
			handler.EnqueueRequestForOwner(
				mgr.GetScheme(), mgr.GetRESTMapper(),
				&criteriav1.CriteriaRun{}, handler.OnlyControllerOwner(),
			),
		).
		Complete(r)
}

func statusEqual(a, b *criteriav1.CriteriaRunStatus) bool {
	if a == nil || b == nil {
		return a == b
	}
	return reflect.DeepEqual(a, b)
}

