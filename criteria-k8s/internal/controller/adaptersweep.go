package controller

import (
	"context"
	"time"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/jobbuilder"
	logr "github.com/go-logr/logr"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// AdapterSweeper periodically reaps adapter Pods and legacy-path adapter
// Jobs whose owning CriteriaRun no longer exists (CRI-144).
//
// The per-scope reconcile derives its desired pod set from a live run's
// lifecycle events, so once the CriteriaRun is deleted nothing reconciles
// its adapter pods anymore: ownerReference-based GC reaps them only when the
// CR delete was deletionTimestamp-propagated, and force-deletes can orphan
// them. The stale pods keep dialing the old runner's shim forever ("scope
// ... is not registered" every 2s). The sweep is the safety net that closes
// both gaps, alongside the finalize and terminal-pass cleanups:
//   - every adapter pod or adapter Job (legacy path) carrying a CriteriaRun
//     ownerReference whose run is gone is deleted — namespace-scoped to the
//     managed namespace, so pods in other namespaces are never touched;
//   - a stale ownerReference UID (a deleted run whose name a new run
//     recycled) is treated as gone: the pod belonged to the deleted run;
//   - adapter objects of a live run that carry no ownerReference at all get
//     one set, so GC reaps them at CR deletion.
//
// Objects without a CriteriaRun ownerReference are never deleted, even when
// they carry a run label: the label is attribution, not ownership. Adapter
// Jobs and Pods of manually launched runs (k8s/launch-pod-adapter-job.sh)
// are labeled but have no CriteriaRun, so an empty lookup by run name proves
// nothing about the object being orphaned. They are left untouched and
// logged: the sweep only ever deletes what it can prove is orphaned via a
// CriteriaRun ownerReference.
type AdapterSweeper struct {
	client.Client
	Scheme    *runtime.Scheme
	Namespace string
	// Interval between sweeps. Sweeps also run once at startup so a backlog
	// of leaked pods clears promptly.
	Interval time.Duration
}

// NewAdapterSweeper builds the sweeper. A non-positive interval falls back
// to the default sweep cadence.
func NewAdapterSweeper(cl client.Client, scheme *runtime.Scheme, namespace string, interval time.Duration) *AdapterSweeper {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	return &AdapterSweeper{Client: cl, Scheme: scheme, Namespace: namespace, Interval: interval}
}

// Run sweeps immediately and then on every interval tick until ctx is done.
func (s *AdapterSweeper) Run(ctx context.Context) {
	logger := logf.FromContext(ctx).WithValues("component", "adapter-sweep", "namespace", s.Namespace)
	ticker := time.NewTicker(s.Interval)
	defer ticker.Stop()

	s.sweep(ctx, logger)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sweep(ctx, logger)
		}
	}
}

func (s *AdapterSweeper) sweep(ctx context.Context, logger logr.Logger) {
	podsDeleted, jobsDeleted, adopted := 0, 0, 0

	var pods corev1.PodList
	if err := s.Client.List(ctx, &pods,
		client.InNamespace(s.Namespace),
		client.MatchingLabels(map[string]string{jobbuilder.LabelRole: jobbuilder.RoleAdapter}),
	); err != nil {
		logger.Error(err, "listing adapter pods for sweep")
	} else {
		for i := range pods.Items {
			pod := &pods.Items[i]
			switch s.sweepObject(ctx, "Pod", pod, logger) {
			case sweepDeleted:
				podsDeleted++
			case sweepAdopted:
				adopted++
			}
		}
	}

	var jobs batchv1.JobList
	if err := s.Client.List(ctx, &jobs,
		client.InNamespace(s.Namespace),
		client.MatchingLabels(map[string]string{jobbuilder.LabelRole: jobbuilder.RoleAdapter}),
	); err != nil {
		logger.Error(err, "listing adapter jobs for sweep")
	} else {
		for i := range jobs.Items {
			job := &jobs.Items[i]
			switch s.sweepObject(ctx, "Job", job, logger) {
			case sweepDeleted:
				jobsDeleted++
			case sweepAdopted:
				adopted++
			}
		}
	}

	logger.Info("adapter sweep complete", "podsDeleted", podsDeleted, "jobsDeleted", jobsDeleted, "adopted", adopted)
}

type sweepOutcome int

const (
	sweepKept sweepOutcome = iota
	sweepDeleted
	sweepAdopted
)

// sweepObject reaps or repairs one adapter Pod or legacy adapter Job.
//
// Deletion is gated on CriteriaRun ownership evidence: the object must carry
// a CriteriaRun ownerReference whose run is gone, or whose UID no longer
// matches the live run of the same name (name recycling). A run label alone
// is attribution, not ownership — manually launched adapter Jobs and Pods
// carry the run label but have no CriteriaRun and no ownerReference — so
// labeled objects without an ownerReference are never deleted here.
func (s *AdapterSweeper) sweepObject(ctx context.Context, kind string, obj client.Object, logger logr.Logger) sweepOutcome {
	if ref := criteriaRunOwnerRefOf(obj); ref != nil {
		return s.sweepOwnedObject(ctx, kind, obj, ref, logger)
	}
	return s.adoptByRunLabel(ctx, kind, obj, logger)
}

// sweepOwnedObject verifies a CriteriaRun ownerReference against the live
// run: a missing run or a stale UID means the object is an orphan of a
// deleted run and is reaped.
func (s *AdapterSweeper) sweepOwnedObject(ctx context.Context, kind string, obj client.Object, ref *metav1.OwnerReference, logger logr.Logger) sweepOutcome {
	var run criteriav1.CriteriaRun
	err := s.Client.Get(ctx, client.ObjectKey{Namespace: obj.GetNamespace(), Name: ref.Name}, &run)
	switch {
	case apierrors.IsNotFound(err):
		return s.deleteAdapterObject(ctx, kind, obj, ref.Name, logger, "owning CriteriaRun not found")
	case err != nil:
		logger.Error(err, "getting owning CriteriaRun for adapter object", "run", ref.Name, "name", obj.GetName())
		return sweepKept
	case ref.UID != "" && ref.UID != run.UID:
		// The ownerReference belongs to a deleted run whose name the
		// current one recycled: the object is orphaned even though the
		// name resolves.
		return s.deleteAdapterObject(ctx, kind, obj, ref.Name, logger, "ownerReference belongs to a deleted CriteriaRun (name recycled)")
	}
	// The ownerReference already points at the live run: nothing to repair.
	return sweepKept
}

// adoptByRunLabel handles objects without a CriteriaRun ownerReference. The
// run label never justifies deletion: when the labeled run does not exist —
// including manually launched runs that have no CriteriaRun at all — the
// object is left untouched and logged. When it does, the object is adopted
// so GC reaps it at CR deletion.
func (s *AdapterSweeper) adoptByRunLabel(ctx context.Context, kind string, obj client.Object, logger logr.Logger) sweepOutcome {
	runName := obj.GetLabels()[jobbuilder.LabelRun]
	if runName == "" {
		logger.Info("adapter object carries no CriteriaRun attribution; leaving untouched",
			"kind", kind, "namespace", obj.GetNamespace(), "name", obj.GetName())
		return sweepKept
	}

	var run criteriav1.CriteriaRun
	err := s.Client.Get(ctx, client.ObjectKey{Namespace: obj.GetNamespace(), Name: runName}, &run)
	if apierrors.IsNotFound(err) {
		logger.Info("adapter object labeled for a CriteriaRun that does not exist; leaving untouched (label alone is not ownership evidence)",
			"kind", kind, "namespace", obj.GetNamespace(), "name", obj.GetName(), "run", runName)
		return sweepKept
	}
	if err != nil {
		logger.Error(err, "getting labeled CriteriaRun for adapter object", "run", runName, "name", obj.GetName())
		return sweepKept
	}
	return s.adoptObject(ctx, kind, obj, &run, runName, logger)
}

// adoptObject repairs a missing CriteriaRun ownerReference so GC reaps the
// object at CR deletion. Objects already controlled by something else (e.g.
// the Job owning its own pods) are left alone.
func (s *AdapterSweeper) adoptObject(ctx context.Context, kind string, obj client.Object, run *criteriav1.CriteriaRun, runName string, logger logr.Logger) sweepOutcome {
	if metav1.GetControllerOf(obj) != nil {
		return sweepKept
	}
	if err := controllerutil.SetControllerReference(run, obj, s.Scheme); err != nil {
		logger.Error(err, "setting CriteriaRun ownerReference on adapter object", "run", runName, "name", obj.GetName())
		return sweepKept
	}
	if err := s.Client.Update(ctx, obj); err != nil {
		logger.Error(err, "adopting adapter object for its CriteriaRun", "run", runName, "name", obj.GetName())
		return sweepKept
	}
	return sweepAdopted
}

func (s *AdapterSweeper) deleteAdapterObject(ctx context.Context, kind string, obj client.Object, runName string, logger logr.Logger, reason string) sweepOutcome {
	logger.Info("sweeping adapter object of a deleted CriteriaRun",
		"kind", kind, "namespace", obj.GetNamespace(), "name", obj.GetName(), "run", runName, "reason", reason)
	opts := []client.DeleteOption{}
	if _, isJob := obj.(*batchv1.Job); isJob {
		// Cascade the job's pods instead of leaving them to be orphaned.
		opts = append(opts, client.PropagationPolicy(metav1.DeletePropagationForeground))
	}
	if err := s.Client.Delete(ctx, obj, opts...); err != nil && !apierrors.IsNotFound(err) {
		logger.Error(err, "deleting adapter object", "name", obj.GetName())
		return sweepKept
	}
	return sweepDeleted
}

// criteriaRunOwnerRefOf returns the object's ownerReference to a CriteriaRun,
// or nil when it has none.
func criteriaRunOwnerRefOf(obj client.Object) *metav1.OwnerReference {
	refs := obj.GetOwnerReferences()
	for i := range refs {
		if refs[i].Kind == "CriteriaRun" {
			return &refs[i]
		}
	}
	return nil
}
