// Package controller reconciles CriteriaRun resources into batch/v1 Jobs.
package controller

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

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
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/events"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/jobbuilder"
)

const (
	criteriaRunFinalizer      = "criteriarun.criteria.brokenbots.dev/finalizer"
	perScopeRequeueInterval   = 10 * time.Second
)

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
	if err := r.updateStatus(ctx, &run, runnerJob, logger); err != nil {
		return ctrl.Result{}, err
	}

	// Reconcile per-scope adapter pods from the run event stream.
	activeAdapters, err := r.reconcilePerScopeAdapters(ctx, &run, logger)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Keep polling the event stream while the run is using per-scope adapters.
	if run.Spec.PerScopeSessions && (activeAdapters > 0 || !isTerminalPhase(derivePhase(runnerJob))) {
		return ctrl.Result{RequeueAfter: perScopeRequeueInterval}, nil
	}

	return ctrl.Result{}, nil
}

func isTerminalPhase(phase criteriav1.CriteriaRunPhase) bool {
	return phase == criteriav1.PhaseSucceeded || phase == criteriav1.PhaseFailed
}

func (r *CriteriaRunReconciler) updateStatus(ctx context.Context, run *criteriav1.CriteriaRun, job *batchv1.Job, logger logr.Logger) error {
	phase := derivePhase(job)

	update := run.DeepCopy()
	update.Status.Phase = phase
	update.Status.JobName = job.Name
	update.Status.EventsPath = eventsPath(run)
	update.Status.ObservedGeneration = run.Generation

	if phase == criteriav1.PhaseSucceeded || phase == criteriav1.PhaseFailed {
		outcome, err := r.readOutcome(ctx, run)
		if err != nil {
			logger.Error(err, "reading run outcome from events file")
		} else {
			update.Status.PRNumber = outcome.PRNumber
			update.Status.TicketState = outcome.TicketState
		}
	}

	if !statusEqual(&run.Status, &update.Status) {
		logger.Info("updating CriteriaRun status", "phase", update.Status.Phase, "jobName", update.Status.JobName)
		if err := r.Status().Update(ctx, update); err != nil {
			return fmt.Errorf("updating status: %w", err)
		}
	}
	return nil
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
	return a.Phase == b.Phase &&
		a.JobName == b.JobName &&
		a.PRNumber == b.PRNumber &&
		a.TicketState == b.TicketState &&
		a.EventsPath == b.EventsPath &&
		a.ObservedGeneration == b.ObservedGeneration
}

