package controller

import (
	"context"
	"fmt"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/events"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/jobbuilder"
	logr "github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// reconcilePerScopeAdapters tails the run event stream and reconciles adapter
// Pods so that exactly the active provision-wanted events have a matching pod.
// Release events remove their scope's pod. Deletions are applied before
// creations so sequential scopes never share a pod name. It returns the
// number of still-active provisions so the caller can decide whether to keep
// polling the event stream.
func (r *CriteriaRunReconciler) reconcilePerScopeAdapters(ctx context.Context, run *criteriav1.CriteriaRun, logger logr.Logger) (int, error) {
	if !run.Spec.PerScopeSessions {
		return 0, nil
	}

	data, err := r.Reader.Read(ctx, run)
	if err != nil {
		logger.Error(err, "reading run events for per-scope reconciliation")
		return 0, fmt.Errorf("reading run events: %w", err)
	}

	lifecycleEvents, err := events.ParseLifecycleEventsBytes(data)
	if err != nil {
		logger.Error(err, "parsing lifecycle events")
		return 0, fmt.Errorf("parsing lifecycle events: %w", err)
	}

	active := events.ActiveProvisions(lifecycleEvents)

	desired := make(map[string]*corev1.Pod, len(active))
	for _, scope := range active {
		pod := jobbuilder.BuildPerScopeAdapterPod(run, r.Defaults, scope)
		desired[pod.Name] = pod
	}

	var existing corev1.PodList
	if err := r.List(ctx, &existing,
		client.InNamespace(run.Namespace),
		client.MatchingLabels(map[string]string{
			"criteria.brokenbots.dev/run":  run.Name,
			"criteria.brokenbots.dev/role": "adapter",
		}),
	); err != nil {
		return 0, fmt.Errorf("listing adapter pods: %w", err)
	}

	// Delete pods that are no longer desired first. This ensures a release for
	// one scope is processed before a provision-wanted for the next scope when
	// both events are pending.
	for i := range existing.Items {
		pod := &existing.Items[i]
		if _, ok := desired[pod.Name]; ok {
			continue
		}
		logger.Info("deleting per-scope adapter pod", "pod", pod.Name)
		if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
			return 0, fmt.Errorf("deleting adapter pod %s: %w", pod.Name, err)
		}
	}

	// Create pods for active provisions that do not already exist.
	existingNames := make(map[string]struct{}, len(existing.Items))
	for i := range existing.Items {
		existingNames[existing.Items[i].Name] = struct{}{}
	}

	for name, pod := range desired {
		if _, ok := existingNames[name]; ok {
			continue
		}
		logger.Info("creating per-scope adapter pod", "pod", name, "adapter", pod.Labels["criteria.brokenbots.dev/adapter-kind"], "scope", pod.Labels["criteria.brokenbots.dev/scope-id"])
		if err := ctrl.SetControllerReference(run, pod, r.Scheme); err != nil {
			return 0, fmt.Errorf("setting controller reference on adapter pod %s: %w", name, err)
		}
		if err := r.Create(ctx, pod); err != nil {
			if apierrors.IsAlreadyExists(err) {
				continue
			}
			return 0, fmt.Errorf("creating adapter pod %s: %w", name, err)
		}
	}

	return len(active), nil
}