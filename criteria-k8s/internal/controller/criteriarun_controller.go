// Package controller reconciles CriteriaRun resources into batch/v1 Jobs.
package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
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
	Castle   castle.RunSource
	Defaults jobbuilder.Defaults
	Queue    *RunQueue
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
		// Stale per-scope adapters of a terminal run: the engine releases
		// every scope when the run ends, so any surviving adapter pod dials a
		// deregistered shim forever ("scope ... is not registered" every 2s)
		// — exactly the loop observed across CRI-130..142 for runs whose
		// runner job crash-looped to deletion. Delete them on every terminal
		// pass; the castle re-derivation below is independent of this
		// cleanup.
		if run.Spec.PerScopeSessions {
			if err := r.deleteRunAdapterPods(ctx, &run, logger); err != nil {
				logger.Error(err, "deleting stale per-scope adapter pods of terminal CriteriaRun")
				return ctrl.Result{RequeueAfter: perScopeRequeueInterval}, nil
			}
		}
		if r.Castle == nil || r.Castle.Disabled() {
			return ctrl.Result{}, nil
		}
		// Re-derive the terminal outcome from castle on every terminal pass,
		// even when the marker is already recorded: a stamp from an earlier
		// operator pod (or from an earlier terminal envelope) can disagree
		// with the run's final castle outcome, so the recorded phase is
		// reconciled against the castle run record and run events instead of
		// being trusted blindly.
		update := run.DeepCopy()
		if _, err := r.observeCastle(ctx, &run, update, logger); err != nil {
			if errors.Is(err, castle.ErrRunNotFound) {
				// Conclusive: the runner Job is terminal, so the agent that
				// registers from the runner pod can never appear and castle
				// will never record a terminal for this run (e.g. a run that
				// predates castle's dual-write). Stop polling instead of
				// looping on discovery errors every interval.
				logger.Info("stopping castle observation for Job-terminal CriteriaRun: the runner agent can no longer register with castle", "error", err)
				return ctrl.Result{}, nil
			}
			logger.Error(err, "observing castle terminal for Job-terminal CriteriaRun")
			return ctrl.Result{RequeueAfter: perScopeRequeueInterval}, nil
		}
		if !statusEqual(&run.Status, &update.Status) {
			if err := r.Status().Update(ctx, update); err != nil {
				return ctrl.Result{}, fmt.Errorf("updating terminal status: %w", err)
			}
		}
		if castleTerminalObserved(&update.Status) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{RequeueAfter: perScopeRequeueInterval}, nil
	}

	// Enqueue the run for repo-keyed admission control. Only admitted runs
	// are allowed to create child Jobs.
	admitted, qstatus, prevRunning := r.Queue.Enqueue(&run)

	update := run.DeepCopy()
	update.Status.ObservedGeneration = run.Generation
	// EventsPath records the debug-only events.ndjson mirror (CRI-136); it is
	// only meaningful when the operator was configured with a debug path, and
	// stays empty for the castle-only default.
	update.Status.EventsPath = r.Defaults.DebugEventsFile
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
		if desired.Labels[jobbuilder.LabelRole] == jobbuilder.RoleRunner {
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
	obs, obsErr := r.observeCastle(ctx, &run, update, logger)
	if obsErr != nil {
		// An unavailable or inconclusive castle source must not converge
		// desired state: no castle-derived status was stamped and the
		// per-scope reconcile below is skipped, so no adapter pod is touched
		// off a history we could not see. The Job-derived phase is still
		// persisted and the queue still releases so the run cannot stall its
		// repo queue; the observation is retried via the requeue below.
		logger.Error(obsErr, "observing run lifecycle from castle; continuing with Job-derived status only", "criteriarun", run.Name)
	}

	if !statusEqual(&run.Status, &update.Status) {
		logger.Info("updating CriteriaRun status", "phase", update.Status.Phase, "jobName", update.Status.JobName)
		if err := r.Status().Update(ctx, update); err != nil {
			return ctrl.Result{}, fmt.Errorf("updating status: %w", err)
		}
	}

	phase := update.Status.Phase

	// Reconcile per-scope adapter pods from the castle event stream. Only an
	// authoritative observation (a known castle run, no observation error)
	// may drive desired state; an unregistered run, a failed observation, or
	// a disabled source is skipped so live pods are never deleted off an
	// empty history.
	activeAdapters := 0
	if obsErr == nil && obs.RunID != "" {
		var err error
		activeAdapters, err = r.reconcilePerScopeAdapters(ctx, &run, obs.Lifecycle, logger)
		if err != nil {
			return ctrl.Result{}, err
		}
	} else if obsErr == nil && run.Spec.PerScopeSessions {
		if obs.RunID == "" && (r.Castle == nil || r.Castle.Disabled()) {
			logger.Info("castle observation disabled; per-scope adapter reconcile requires --castle-addr (castle is the only lifecycle source since CRI-135)")
		} else {
			logger.Info("castle run not yet known; skipping per-scope reconcile until observation succeeds")
		}
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

	// Requeue policy:
	//   - an observation error retries on the poll interval (the Job-derived
	//   status above is already persisted, so the error must not abort the
	//   pass);
	//   - per-scope runs keep polling until every scope is released;
	//   - a terminal run keeps polling until castle has recorded the
	//   terminal outcome, unless castle is disabled.
	if obsErr != nil {
		return ctrl.Result{RequeueAfter: perScopeRequeueInterval}, nil
	}
	if run.Spec.PerScopeSessions && (activeAdapters > 0 || !isTerminalPhase(phase)) {
		return ctrl.Result{RequeueAfter: perScopeRequeueInterval}, nil
	}
	if isTerminalPhase(phase) && !castleTerminalObserved(&update.Status) && !(r.Castle == nil || r.Castle.Disabled()) {
		return ctrl.Result{RequeueAfter: perScopeRequeueInterval}, nil
	}

	return ctrl.Result{}, nil
}

func isTerminalPhase(phase criteriav1.CriteriaRunPhase) bool {
	return phase == criteriav1.PhaseSucceeded || phase == criteriav1.PhaseFailed
}

// deleteRunAdapterPods deletes every per-scope adapter pod labeled for the
// run. Called from the finalize path (CR deletion) and on terminal passes
// (stale pods of a finished run): the per-scope reconcile only converges
// desired state while the run lives, so its pods would otherwise outlive
// the runner's shim and dial it forever. Only ever called for per-scope
// runs — legacy-path adapter pods are owned by their adapter Job and must
// not be deleted directly.
func (r *CriteriaRunReconciler) deleteRunAdapterPods(ctx context.Context, run *criteriav1.CriteriaRun, logger logr.Logger) error {
	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(run.Namespace),
		client.MatchingLabels(map[string]string{
			jobbuilder.LabelRun:  run.Name,
			jobbuilder.LabelRole: jobbuilder.RoleAdapter,
		}),
	); err != nil {
		return fmt.Errorf("listing adapter pods: %w", err)
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		logger.Info("deleting stale per-scope adapter pod", "pod", pod.Name)
		if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting adapter pod %s: %w", pod.Name, err)
		}
	}
	return nil
}

// castleTerminalObserved reports whether a castle observation has already
// delivered this run's terminal outcome (CastleTerminalObserved is set by
// observeCastle whenever the observation carries a castle terminal — the run
// record's terminal status and/or RunCompleted/RunFailed envelopes). That is
// the only terminal signal castle actually provides: prNumber/ticketState
// have no castle producer today, so the completion gate must not depend on
// them. A recorded marker does not exempt the run from observation: a later
// pass re-derives the outcome from castle and corrects a recorded phase that
// disagrees with the run's terminal (a stamp can predate the run's final
// castle state, e.g. when a previous operator pod wrote it).
func castleTerminalObserved(status *criteriav1.CriteriaRunStatus) bool {
	return status != nil && status.CastleTerminalObserved
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
// recorded terminal marker. An unavailable or inconclusive source (castle
// outage, run not registered yet, discovery that cannot conclude) returns an
// error; the caller must then not reconcile per-scope desired state (desired
// state must never be converged from an empty history) and must treat any
// castle-derived stamping in the update as absent, while the Job-derived
// phase it already set still persists.
func (r *CriteriaRunReconciler) observeCastle(ctx context.Context, run *criteriav1.CriteriaRun, update *criteriav1.CriteriaRun, logger logr.Logger) (*castle.Observation, error) {
	if r.Castle == nil || r.Castle.Disabled() {
		return &castle.Observation{}, nil
	}

	// The runner job name is the agent-name key: the engine registers a
	// castle agent named after the runner pod's hostname, which carries the
	// job name as its prefix.
	obs, err := r.Castle.Observe(ctx, jobbuilder.JobName(run), run.Status.CastleRunID)
	if err != nil {
		logger.Error(err, "observing run lifecycle from castle")
		return nil, err
	}

	if obs.RunID != "" && obs.RunID != run.Status.CastleRunID {
		update.Status.CastleRunID = obs.RunID
	}
	if obs.Terminal != nil {
		// Terminal stamping comes from castle (the run record's terminal
		// status and/or RunCompleted/RunFailed envelopes), keeping the same
		// phase semantics the Job conditions use. Job conditions remain the
		// base phase so the queue can still release if castle goes silent.
		//
		// The marker is the completion signal: it is the only satisfiable
		// record that a castle observation delivered the terminal. The
		// prNumber/ticketState fields are informational only — castle
		// supplies no pr_url or ticket-state producer today — so they are
		// enriched when present but never gate completion.
		update.Status.CastleTerminalObserved = true
		if obs.Terminal.Success {
			update.Status.Phase = criteriav1.PhaseSucceeded
		} else {
			update.Status.Phase = criteriav1.PhaseFailed
		}
		update.Status.PRNumber = obs.Terminal.PRNumber
	}
	return obs, nil
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

	// Per-scope adapter pods are not among the desired Jobs (BuildAll omits
	// them for per-scope runs): delete them here so a deletionTimestamp-
	// propagated CR delete reaps its pods within one reconcile interval.
	// Force-deletes that orphan the pods are covered by the adapter sweep.
	if run.Spec.PerScopeSessions {
		if err := r.deleteRunAdapterPods(ctx, run, logger); err != nil {
			return ctrl.Result{}, err
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
