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
//   - every adapter pod or adapter Job (legacy path) labeled
//     criteria.brokenbots.dev/role=adapter whose owning CriteriaRun is gone
//     is deleted — namespace-scoped to the managed namespace, so pods in
//     other namespaces are never touched;
//   - a stale ownerReference UID (a deleted run whose name a new run
//     recycled) is treated as gone: the pod belonged to the deleted run;
//   - adapter objects whose run still exists but that carry no
//     ownerReference at all get one set, so GC reaps them at CR deletion.
//
// Objects that cannot be attributed to a CriteriaRun (no run label and no
// CriteriaRun ownerReference) are left untouched and logged: the sweep only
// ever deletes what it can prove is orphaned.
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
			if s.sweepObject(ctx, "Job", job, logger) == sweepDeleted {
				jobsDeleted++
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

// sweepObject reaps or repairs one adapter Pod or legacy adapter Job. The
// owning CriteriaRun is resolved from the run label first, then from a
// CriteriaRun ownerReference.
func (s *AdapterSweeper) sweepObject(ctx context.Context, kind string, obj client.Object, logger logr.Logger) sweepOutcome {
	runName := runNameOf(obj)
	if runName == "" {
		logger.Info("adapter object carries no CriteriaRun attribution; leaving untouched",
			"kind", kind, "namespace", obj.GetNamespace(), "name", obj.GetName())
		return sweepKept
	}

	var run criteriav1.CriteriaRun
	err := s.Client.Get(ctx, client.ObjectKey{Namespace: obj.GetNamespace(), Name: runName}, &run)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return s.deleteAdapterObject(ctx, kind, obj, runName, logger, "owning CriteriaRun not found")
		}
		logger.Error(err, "getting owning CriteriaRun for adapter object", "run", runName, "name", obj.GetName())
		return sweepKept
	}

	// A CriteriaRun ownerReference whose UID does not match the live run
	// belongs to a deleted run whose name the current one recycled: the
	// object is orphaned even though the name resolves.
	if ref := criteriaRunOwnerRefOf(obj); ref != nil && ref.UID != "" && ref.UID != run.UID {
		return s.deleteAdapterObject(ctx, kind, obj, runName, logger, "ownerReference belongs to a deleted CriteriaRun (name recycled)")
	}

	// Repair missing ownerReferences so GC reaps the object at CR deletion.
	// Objects already controlled by something else (e.g. the Job owning its
	// own pods) are left alone.
	if criteriaRunOwnerRefOf(obj) == nil && metav1.GetControllerOf(obj) == nil {
		if err := controllerutil.SetControllerReference(&run, obj, s.Scheme); err != nil {
			logger.Error(err, "setting CriteriaRun ownerReference on adapter object", "run", runName, "name", obj.GetName())
			return sweepKept
		}
		if err := s.Client.Update(ctx, obj); err != nil {
			logger.Error(err, "adopting adapter object for its CriteriaRun", "run", runName, "name", obj.GetName())
			return sweepKept
		}
		return sweepAdopted
	}

	return sweepKept
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

// runNameOf resolves the CriteriaRun name an adapter object belongs to: the
// run label first (what the reconciler keys on), then a CriteriaRun
// ownerReference.
func runNameOf(obj client.Object) string {
	if name := obj.GetLabels()[jobbuilder.LabelRun]; name != "" {
		return name
	}
	if ref := criteriaRunOwnerRefOf(obj); ref != nil {
		return ref.Name
	}
	return ""
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
