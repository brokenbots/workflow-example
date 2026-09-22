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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
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
	// eventReasonSingleActive is the Kubernetes event reason emitted when
	// admission refuses a second live CriteriaRun for a ticket (CRI-221).
	eventReasonSingleActive = "SecondActiveRunForTicket"
)

// CriteriaRunReconciler reconciles a CriteriaRun object into a batch/v1 Job.
type CriteriaRunReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Recorder emits Kubernetes events for admission decisions (CRI-221
	// single-active invariant). Nil disables event emission.
	Recorder record.EventRecorder
	// Castle observes run lifecycle from the castle control plane (CRI-133
	// API). Nil or disabled means observation is off; the reconciler then
	// relies purely on Job conditions for phase stamping.
	Castle castle.RunSource
	// EnvProbe resolves operator env values as the cluster currently
	// declares them (CRI-264): the runner image is resolved from the live
	// operator Deployment env rather than the reconciling pod's process
	// env, which goes stale while the operator Deployment rolls out.
	// Nil keeps the process-env resolution and mismatch checks run against
	// it; a probe read error defers mismatch checks to a later pass.
	EnvProbe OperatorEnvProbe
	Defaults jobbuilder.Defaults
	Queue    *RunQueue
}

// +kubebuilder:rbac:groups=criteria.brokenbots.dev,resources=criteriaruns,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=criteria.brokenbots.dev,resources=criteriaruns/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=criteria.brokenbots.dev,resources=criteriaruns/finalizers,verbs=update
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

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
	// Release any stale admission slot so the next queued run is admitted.
	if isTerminalPhase(run.Status.Phase) {
		r.Queue.Release(&run)
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

	// Enqueue the run for (repoURL, class)-keyed admission control
	// (CRI-242). Only admitted runs are allowed to create child Jobs.
	admitted, qstatus, _ := r.Queue.Enqueue(&run)

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
		// CRI-291: the currently running run's queue status is refreshed on
		// its own next pass (poll cadence or watch event), never by a nested
		// synchronous reconcile here. A nested call re-enters Reconcile with
		// this run's request still stamped on the context, so every status
		// write inside it logs and keys off the WRONG run identity — the
		// cross-contamination signature of CRI-291 — and its requeue
		// decisions are silently dropped. The queued run's own
		// queueRequeueInterval requeue keeps admission progressing.
		return ctrl.Result{RequeueAfter: queueRequeueInterval}, nil
	}

	// CRI-221 single-active invariant (enforcement side): refuse to admit a
	// second non-terminal CriteriaRun for the same ticket. The watcher's
	// pre-create gate can be raced or bypassed (runs created outside its
	// selector convention), so admission re-asserts the invariant: at most
	// one live run per ticket. Runs the controller already actuated skip
	// the assert — their Jobs exist and refusing their reconcile would
	// strand them; between two un-actuated runs the older one wins the
	// admission race deterministically.
	if run.Status.JobName == "" {
		blocker, err := r.singleActiveTicketRun(ctx, &run)
		if err != nil {
			return ctrl.Result{}, err
		}
		if blocker != "" {
			msg := fmt.Sprintf("ticket %s already has a live CriteriaRun %s; at most one active workflow is allowed per ticket", run.Spec.TicketID, blocker)
			if r.Recorder != nil {
				r.Recorder.Event(&run, corev1.EventTypeWarning, eventReasonSingleActive, msg)
			}
			logger.Info("refusing to admit CriteriaRun: a live CriteriaRun already exists for the ticket",
				"ticket", run.Spec.TicketID, "liveRun", blocker)
			// Free the admission slot so other runs for the repo are
			// not starved behind the refused run; the run's own retry comes
			// from this reconcile error's backoff.
			r.Queue.Release(&run)
			return ctrl.Result{}, fmt.Errorf("refusing to admit CriteriaRun %s: %s", run.Name, msg)
		}
	}

	// CRI-264: resolve the runner image against the operator env the
	// cluster currently declares before touching child Jobs. Peek the
	// runner Job first: its existence decides between the reconcile pass
	// (mismatch check against the pinned image) and the admission pass
	// (build and create with the live-resolved defaults).
	runnerKey := client.ObjectKey{
		Name:      jobbuilder.RunnerJobName(&run),
		Namespace: jobbuilder.TargetNamespace(&run),
	}
	var existingRunner batchv1.Job
	runnerFound := true
	if err := r.Get(ctx, runnerKey, &existingRunner); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("peeking runner job %s: %w", runnerKey, err)
		}
		runnerFound = false
	}
	resolvedDefaults, resolvedLive := r.resolveRunnerDefaults(ctx, &run, logger)
	currentImage := jobbuilder.ResolveRunnerImage(&run, resolvedDefaults)

	if runnerFound {
		// Enforce only on non-terminal Jobs: a Job that already completed
		// keeps its recorded outcome even if the env moved on afterwards.
		if resolvedLive && !isTerminalPhase(derivePhase(&existingRunner)) {
			if pinned, mismatch := runnerImageMismatch(&run.Status, &existingRunner, currentImage); mismatch {
				return r.failBaseImageMismatch(ctx, &run, pinned, currentImage, logger)
			}
		}
	} else if resolvedLive && run.Status.BaseImage != "" && run.Status.BaseImage != currentImage {
		// Admission pass on a run whose status already carries a stamp
		// from a prior operator (its Jobs are gone) with the env moved on
		// since: fail fast instead of re-admitting on the old stamp.
		return r.failBaseImageMismatch(ctx, &run, run.Status.BaseImage, currentImage, logger)
	}

	desiredJobs := jobbuilder.BuildAll(&run, resolvedDefaults)

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

	// Stamp the runner image the child Jobs are pinned with (CRI-264). The
	// image actually on the runner container is the ground truth: with a
	// live resolution it agrees with the resolution (a disagreeing Job was
	// failed fast above), and with a degraded resolution it records the
	// process-env pin the Job was actually built with.
	if update.Status.BaseImage == "" {
		if pinned := jobbuilder.RunnerJobImage(runnerJob); pinned != "" {
			update.Status.BaseImage = pinned
		} else {
			update.Status.BaseImage = currentImage
		}
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

	// Release the queue slot when the run has finished so the next queued
	// run for the same repo is admitted. It is picked up by its own requeue
	// or watch event — never a nested reconcile (CRI-291).
	if isTerminalPhase(phase) {
		r.Queue.Release(&run)
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

// singleActiveTicketRun returns the name of the live CriteriaRun blocking
// run's admission under the single-active invariant (CRI-221: at most one
// active workflow per ticket), or "" when the run may be admitted. A blocker
// is another CriteriaRun in the same namespace with the same ticket id that
// is neither being deleted nor terminal, and that either the controller
// already actuated (its runner Job exists, so it holds the ticket regardless
// of age) or that is older than run (between two un-actuated runs the older
// one wins the admission race deterministically).
func (r *CriteriaRunReconciler) singleActiveTicketRun(ctx context.Context, run *criteriav1.CriteriaRun) (string, error) {
	var runs criteriav1.CriteriaRunList
	if err := r.List(ctx, &runs, client.InNamespace(run.Namespace)); err != nil {
		return "", fmt.Errorf("listing CriteriaRuns for the single-active invariant: %w", err)
	}
	for i := range runs.Items {
		other := &runs.Items[i]
		if other.Name == run.Name || other.Spec.TicketID != run.Spec.TicketID {
			continue
		}
		if !other.DeletionTimestamp.IsZero() || isTerminalPhase(other.Status.Phase) {
			continue
		}
		if other.Status.JobName != "" || runOlder(other, run) {
			return other.Name, nil
		}
	}
	return "", nil
}

// runOlder reports whether a was created before b, using the name as a
// deterministic tiebreak for equal creation timestamps.
func runOlder(a, b *criteriav1.CriteriaRun) bool {
	if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.CreationTimestamp.Before(&b.CreationTimestamp)
	}
	return a.Name < b.Name
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
	// can be admitted; it is picked up by its own requeue, not a nested
	// reconcile (CRI-291).
	r.Queue.Release(run)

	if err := r.deleteChildJobs(ctx, run); err != nil {
		return ctrl.Result{}, err
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
