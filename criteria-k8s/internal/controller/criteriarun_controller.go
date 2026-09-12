// Package controller reconciles CriteriaRun resources into batch/v1 Jobs.
package controller

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/structpb"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	logr "github.com/go-logr/logr"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/castle"
	v1 "github.com/brokenbots/workflow-example/criteria-k8s/internal/criteria/pb/criteria/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/events"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/jobbuilder"
)

const (
	criteriaRunFinalizer    = "criteriarun.criteria.brokenbots.dev/finalizer"
	perScopeRequeueInterval = 10 * time.Second
	queueRequeueInterval    = 5 * time.Second
	castleRequeueInterval   = 10 * time.Second
	criteriaFinalizedReason = "criteriarun finalized (deleted)"
)

// RunPublisher publishes CriteriaRun lifecycle state into the castle control
// plane. Implemented by the castle package's Publisher; publishing is skipped
// when Disabled returns true. All failures are best-effort: the reconciler
// logs them and retries on a later reconcile, never failing the run.
type RunPublisher interface {
	Disabled() bool
	EnsureRun(ctx context.Context, req castle.EnsureRunRequest) (string, error)
	PublishEvent(ctx context.Context, runID string, payload any) error
}

// EventsReader reads the events ndjson file produced by a completed run.
type EventsReader interface {
	// Read returns the events file content for the given CriteriaRun.
	Read(ctx context.Context, run *criteriav1.CriteriaRun) ([]byte, error)
}

// PodExecReader reads the events file by exec'ing into a pod owned by the run's Job.
type PodExecReader struct {
	Config *rest.Config
}

// Read executes `cat` on the run's events file inside the first available pod.
func (r *PodExecReader) Read(ctx context.Context, run *criteriav1.CriteriaRun) ([]byte, error) {
	if run.Status.JobName == "" {
		return nil, fmt.Errorf("no job associated with run")
	}
	cl, err := kubernetes.NewForConfig(r.Config)
	if err != nil {
		return nil, fmt.Errorf("building kubernetes client: %w", err)
	}
	podList, err := cl.CoreV1().Pods(run.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("job-name=%s", run.Status.JobName),
	})
	if err != nil {
		return nil, fmt.Errorf("listing pods: %w", err)
	}
	if len(podList.Items) == 0 {
		return nil, fmt.Errorf("no pods found for job %s", run.Status.JobName)
	}

	// Prefer a pod that has reached a terminal phase.
	var target *corev1.Pod
	for i := range podList.Items {
		p := &podList.Items[i]
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			target = p
			break
		}
	}
	if target == nil {
		target = &podList.Items[0]
	}

	eventsPath := eventsPath(run)
	req := cl.CoreV1().RESTClient().
		Post().
		Resource("pods").
		Name(target.Name).
		Namespace(target.Namespace).
		SubResource("exec").
		Param("container", "workflow-runner").
		Param("command", "sh").
		Param("command", "-c").
		Param("command", fmt.Sprintf("cat %s 2>/dev/null || true", shellQuote(eventsPath))).
		Param("stdout", "true").
		Param("stderr", "false").
		Param("tty", "false")

	exec, err := remotecommand.NewSPDYExecutor(r.Config, "POST", req.URL())
	if err != nil {
		return nil, fmt.Errorf("creating executor: %w", err)
	}
	var stdout, stderr bytes.Buffer
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil {
		return nil, fmt.Errorf("exec stream: %w (stderr: %s)", err, stderr.String())
	}
	return stdout.Bytes(), nil
}

// CriteriaRunReconciler reconciles a CriteriaRun object into a batch/v1 Job.
type CriteriaRunReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Config   *rest.Config
	Reader   EventsReader
	Defaults jobbuilder.Defaults
	Queue    *RunQueue
	Castle   RunPublisher
}

// +kubebuilder:rbac:groups=criteria.brokenbots.dev,resources=criteriaruns,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=criteria.brokenbots.dev,resources=criteriaruns/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=criteria.brokenbots.dev,resources=criteriaruns/finalizers,verbs=update
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/exec,verbs=create

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

	// Publish the run into castle (idempotent per ticket); a castle outage
	// only logs and requeues, it never blocks job creation.
	castleRunID, castlePending := r.ensureCastleRun(ctx, &run, logger)

	update := run.DeepCopy()
	update.Status.ObservedGeneration = run.Generation
	update.Status.EventsPath = eventsPath(&run)
	update.Status.Queue = qstatus
	if castleRunID != "" {
		// Persisted together with the phase below so the castle run id and
		// the k8s status share one write and never fight over the
		// resourceVersion.
		update.Status.CastleRunID = castleRunID
	}

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

	// Sync status from the runner Job.
	requeue, err := r.applyJobStatus(ctx, &run, runnerJob, update, castleRunID, logger)
	if err != nil {
		return ctrl.Result{}, err
	}
	castlePending = castlePending || requeue

	phase := derivePhase(runnerJob)

	// Reconcile per-scope adapter pods from the run event stream.
	activeAdapters, err := r.reconcilePerScopeAdapters(ctx, &run, logger)
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

	// Keep polling the event stream while the run is using per-scope adapters.
	if run.Spec.PerScopeSessions && (activeAdapters > 0 || !isTerminalPhase(phase)) {
		return ctrl.Result{RequeueAfter: perScopeRequeueInterval}, nil
	}

	if castlePending {
		// Castle publishing did not complete; retry soon without failing the
		// reconcile.
		return ctrl.Result{RequeueAfter: castleRequeueInterval}, nil
	}

	return ctrl.Result{}, nil
}

func isTerminalPhase(phase criteriav1.CriteriaRunPhase) bool {
	return phase == criteriav1.PhaseSucceeded || phase == criteriav1.PhaseFailed
}

func (r *CriteriaRunReconciler) applyJobStatus(ctx context.Context, run *criteriav1.CriteriaRun, job *batchv1.Job, update *criteriav1.CriteriaRun, castleRunID string, logger logr.Logger) (bool, error) {
	phase := derivePhase(job)

	update.Status.Phase = phase
	update.Status.JobName = job.Name
	update.Status.ObservedGeneration = run.Generation

	var outcome *events.Outcome
	if phase == criteriav1.PhaseSucceeded || phase == criteriav1.PhaseFailed {
		out, err := r.readOutcome(ctx, run)
		if err != nil {
			logger.Error(err, "reading run outcome from events file")
			out = &events.Outcome{}
		}
		outcome = out
		update.Status.PRNumber = outcome.PRNumber
		update.Status.TicketState = outcome.TicketState
	}

	if !statusEqual(&run.Status, &update.Status) {
		logger.Info("updating CriteriaRun status", "phase", update.Status.Phase, "jobName", update.Status.JobName)
		if err := r.Status().Update(ctx, update); err != nil {
			return false, fmt.Errorf("updating status: %w", err)
		}
	}

	// Publish the phase transition into castle after the k8s status has been
	// persisted; failures are logged and retried on the next reconcile.
	return r.publishPhaseChange(ctx, run, castleRunID, phase, outcome, logger), nil
}

// ensureCastleRun makes sure the CriteriaRun is registered in castle,
// returning the castle run identifier. It is idempotent: once the run has a
// CastleRunID the castle side owns the ticket-backed dedupe. The identifier
// is only recorded in memory here; the caller persists it with the next
// status update. A castle outage is never an error — it logs and reports
// requeue so the next reconcile retries.
func (r *CriteriaRunReconciler) ensureCastleRun(ctx context.Context, run *criteriav1.CriteriaRun, logger logr.Logger) (string, bool) {
	if r.Castle == nil || r.Castle.Disabled() {
		return "", false
	}
	if run.Status.CastleRunID != "" {
		return run.Status.CastleRunID, false
	}
	runID, err := r.Castle.EnsureRun(ctx, castle.EnsureRunRequest{
		Ticket:       run.Spec.TicketID,
		RepoURL:      run.Spec.RepoURL,
		WorkflowName: "criteriarun/" + run.Name,
	})
	if err != nil {
		// Log and retry with backoff on a later reconcile; the run keeps
		// processing while castle is unreachable.
		logger.Info("publishing CriteriaRun to castle deferred", "error", err, "ticket", run.Spec.TicketID)
		return "", true
	}
	run.Status.CastleRunID = runID
	logger.Info("published CriteriaRun to castle", "castleRunId", runID, "ticket", run.Spec.TicketID)
	return runID, false
}

// publishPhaseChange emits the overlord lifecycle event for a CriteriaRun
// phase transition. It returns true when publishing is still pending (castle
// unreachable) so the caller can requeue. Events are published at most once
// per phase, tracked by Status.CastlePhase.
func (r *CriteriaRunReconciler) publishPhaseChange(ctx context.Context, run *criteriav1.CriteriaRun, castleRunID string, phase criteriav1.CriteriaRunPhase, outcome *events.Outcome, logger logr.Logger) bool {
	if r.Castle == nil || r.Castle.Disabled() || castleRunID == "" || run.Status.CastlePhase == string(phase) {
		return false
	}

	var payloads []any
	switch phase {
	case criteriav1.PhaseRunning:
		payloads = append(payloads, &v1.RunStarted{})
	case criteriav1.PhaseSucceeded:
		if outcome != nil && outcome.PRNumber != "" {
			if url := prURL(run.Spec.RepoURL, outcome.PRNumber); url != "" {
				data, derr := structpb.NewStruct(map[string]any{"url": url})
				if derr != nil {
					logger.Info("building pr_link payload deferred", "error", derr)
				} else {
					payloads = append(payloads, &v1.AdapterEvent{Kind: "pr_link", Data: data})
				}
			}
		}
		payloads = append(payloads, &v1.RunCompleted{FinalState: "succeeded", Success: true})
	case criteriav1.PhaseFailed:
		payloads = append(payloads, &v1.RunFailed{Reason: failureReason(outcome)})
	default:
		// Pending has no overlord event: the run record is created in the
		// pending state by EnsureRun.
		return false
	}

	for _, payload := range payloads {
		if err := r.Castle.PublishEvent(ctx, castleRunID, payload); err != nil {
			logger.Info("publishing CriteriaRun phase to castle deferred", "phase", phase, "error", err)
			return true
		}
	}

	base := run.DeepCopy()
	base.Status.CastlePhase = string(phase)
	if err := r.Status().Patch(ctx, base, client.MergeFrom(run)); err != nil {
		// The patch failing means the phase may be re-published on the next
		// reconcile; castle tolerates duplicate lifecycle events.
		logger.Info("recording published castle phase deferred", "error", err, "phase", phase)
		return true
	}
	run.Status.CastlePhase = string(phase)
	logger.Info("published CriteriaRun phase to castle", "phase", phase, "castleRunId", castleRunID)
	return false
}

// prURL builds a GitHub pull request URL from the repo and PR number.
// RepoURL may be an https URL or an "owner/name" shorthand.
func prURL(repoURL, prNumber string) string {
	if repoURL == "" || prNumber == "" {
		return ""
	}
	if strings.HasPrefix(repoURL, "https://github.com/") || strings.HasPrefix(repoURL, "http://github.com/") {
		repo := strings.TrimPrefix(strings.TrimPrefix(repoURL, "https://github.com/"), "http://github.com/")
		repo = strings.TrimSuffix(repo, ".git")
		return fmt.Sprintf("https://github.com/%s/pull/%s", repo, prNumber)
	}
	if !strings.Contains(repoURL, "://") {
		return fmt.Sprintf("https://github.com/%s/pull/%s", strings.TrimSuffix(repoURL, ".git"), prNumber)
	}
	return ""
}

// failureReason maps a failed run's parsed outcome into the overlord
// RunFailed.reason vocabulary.
func failureReason(outcome *events.Outcome) string {
	if outcome != nil && outcome.Terminal != "" {
		return "criteriarun job failed: " + outcome.Terminal
	}
	return "criteriarun job failed"
}

// publishFinalize emits the terminal lifecycle event for a deleted CriteriaRun
// so castle marks the run complete instead of leaving it dangling open. If the
// run reached a terminal phase that was never published, the matching terminal
// event is used so a succeeded run is not flipped to failed.
func (r *CriteriaRunReconciler) publishFinalize(ctx context.Context, run *criteriav1.CriteriaRun, logger logr.Logger) {
	if r.Castle == nil || r.Castle.Disabled() || run.Status.CastleRunID == "" {
		return
	}
	if run.Status.CastlePhase == string(criteriav1.PhaseSucceeded) ||
		run.Status.CastlePhase == string(criteriav1.PhaseFailed) {
		// Castle already recorded a terminal state for this run.
		return
	}
	var payload any
	if run.Status.Phase == criteriav1.PhaseSucceeded {
		payload = &v1.RunCompleted{FinalState: "succeeded", Success: true}
	} else {
		payload = &v1.RunFailed{Reason: criteriaFinalizedReason}
	}
	if err := r.Castle.PublishEvent(ctx, run.Status.CastleRunID, payload); err != nil {
		logger.Info("publishing finalize event to castle deferred", "error", err, "castleRunId", run.Status.CastleRunID)
		return
	}
	logger.Info("published CriteriaRun finalize to castle", "castleRunId", run.Status.CastleRunID)
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

func eventsPath(run *criteriav1.CriteriaRun) string {
	return fmt.Sprintf("/data/intake/%s/events.ndjson", run.Spec.TicketID)
}

// shellQuote returns a single-quoted shell literal for s.
// It assumes the remote shell is POSIX /bin/sh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func (r *CriteriaRunReconciler) readOutcome(ctx context.Context, run *criteriav1.CriteriaRun) (*events.Outcome, error) {
	data, err := r.Reader.Read(ctx, run)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return &events.Outcome{}, nil
	}
	return events.ParseBytes(data)
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

	// Emit the terminal event into castle so a deleted run is not left
	// dangling open there. Best-effort: a castle outage must never block
	// deletion, so the finalizer is removed regardless of the outcome.
	r.publishFinalize(ctx, run, logger)

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
