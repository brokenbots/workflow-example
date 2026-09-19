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

// reconcilePerScopeAdapters consumes the castle-derived lifecycle event
// history and reconciles adapter Pods so that exactly the active
// provision-wanted events have a matching pod. Release events remove their
// scope's pod. Deletions are applied before creations so sequential scopes
// never share a pod name. It returns the number of still-active provisions
// so the caller can decide whether to keep polling castle.
//
// Grouping (CRI-234 M7.2): provisions carrying an environment identity are
// co-located — one pod per (scope, environment) with each adapter as a
// separate container. Provisions WITHOUT environment identity (engines older
// than CRI-233's fc95449) fall back to one pod per adapter, matching the
// pre-CRI-234 shape. A group pod is recreated whenever its member set drifts
// (a member was released or a new one provisioned): pod container sets are
// immutable in Kubernetes, so a stale container would keep dialing a
// deregistered shim otherwise.
func (r *CriteriaRunReconciler) reconcilePerScopeAdapters(ctx context.Context, run *criteriav1.CriteriaRun, lifecycleEvents []events.LifecycleEvent, logger logr.Logger) (int, error) {
	if !run.Spec.PerScopeSessions {
		return 0, nil
	}

	active := events.ActiveProvisions(lifecycleEvents)

	type envGroup struct {
		scopeID     string
		environment string
		members     []events.LifecycleEvent
	}
	type envGroupKey struct {
		scopeID     string
		environment string
	}
	groups := make(map[envGroupKey]*envGroup)

	desired := make(map[string]*corev1.Pod, len(active))
	for _, scope := range active {
		if scope.AdapterType == "" {
			// CRI-140 semantics, deliberately kept: engines older than the
			// pinned v0.5.22 do not publish adapter_type, so the image kind
			// falls back to the adapter node name — an image reference that
			// does not exist in the registry and wedges the pod in
			// ImagePull. The fallback is retained for such engines, and
			// this error-level log line (always emitted, V(0)) is the
			// operator's alert hook: alert on reason="adapter_type_missing".
			// Every silent ImagePull wedge of this shape surfaces here.
			logger.Error(nil, "per-scope provision_wanted carries no adapter_type; resolving image kind from the adapter node name, which likely does not exist in the registry (requires criteria >= v0.5.22)",
				"reason", "adapter_type_missing", "run", run.Name, "adapter", scope.AdapterName, "scope", scope.ScopeID)
		}
		if scope.Environment == "" {
			// No environment identity (pre-fc95449 engine): keep the
			// per-adapter fallback pod, name and shape unchanged.
			pod := jobbuilder.BuildPerScopeAdapterPod(run, r.Defaults, scope)
			desired[pod.Name] = pod
			continue
		}
		key := envGroupKey{scopeID: scope.ScopeID, environment: scope.Environment}
		group, ok := groups[key]
		if !ok {
			group = &envGroup{scopeID: scope.ScopeID, environment: scope.Environment}
			groups[key] = group
		}
		group.members = append(group.members, scope)
	}
	for _, group := range groups {
		pod := jobbuilder.BuildPerScopeAdapterPodGroup(run, r.Defaults, group.scopeID, group.environment, group.members)
		if pod == nil {
			continue
		}
		desired[pod.Name] = pod
	}

	var existing corev1.PodList
	if err := r.List(ctx, &existing,
		client.InNamespace(run.Namespace),
		client.MatchingLabels(map[string]string{
			jobbuilder.LabelRun:  run.Name,
			jobbuilder.LabelRole: jobbuilder.RoleAdapter,
		}),
	); err != nil {
		return 0, fmt.Errorf("listing adapter pods: %w", err)
	}

	// Delete pods that are no longer desired first. This ensures a release for
	// one scope is processed before a provision-wanted for the next scope when
	// both events are pending. Desired names are stable functions of the
	// (scope, environment) pair, so this covers both full releases and group
	// membership drift: a released member changes the group's container set,
	// and because pod container sets are immutable the stale group pod must
	// be deleted and recreated with the remaining members — otherwise a
	// released adapter's container would keep dialing a deregistered shim.
	// Per-adapter fallback pods cannot drift (their name pins the kind and
	// scope), so only group pods are drift-checked.
	deletedNames := make(map[string]struct{})
	for i := range existing.Items {
		pod := &existing.Items[i]
		if desiredPod, ok := desired[pod.Name]; ok {
			if !isAdapterGroupPod(pod) || adapterContainerNamesEqual(pod, desiredPod) {
				continue
			}
			logger.Info("recreating per-scope adapter group pod after membership change", "pod", pod.Name)
		}
		logger.Info("deleting per-scope adapter pod", "pod", pod.Name)
		if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
			return 0, fmt.Errorf("deleting adapter pod %s: %w", pod.Name, err)
		}
		deletedNames[pod.Name] = struct{}{}
	}

	// Create pods for active provisions that do not already exist.
	existingNames := make(map[string]struct{}, len(existing.Items))
	for i := range existing.Items {
		existingNames[existing.Items[i].Name] = struct{}{}
	}
	for name := range deletedNames {
		delete(existingNames, name)
	}

	for name, pod := range desired {
		if _, ok := existingNames[name]; ok {
			continue
		}
		logger.Info("creating per-scope adapter pod", "pod", name,
			"adapter", adapterPodLogLabel(pod),
			"scope", pod.Labels[jobbuilder.LabelScopeID])
		if err := ctrl.SetControllerReference(run, pod, r.Scheme); err != nil {
			return 0, fmt.Errorf("setting controller reference on adapter pod %s: %w", name, err)
		}
		if err := r.Create(ctx, pod); err != nil {
			if apierrors.IsAlreadyExists(err) {
				// The delete above is async on a real cluster: the pod is
				// still terminating, so the create is retried on a later
				// pass. Log at V(1) so drift-recreation convergence is
				// traceable without cluttering the default log level.
				logger.V(1).Info("adapter pod already exists after delete (still terminating); converging on a later pass", "pod", name)
				continue
			}
			return 0, fmt.Errorf("creating adapter pod %s: %w", name, err)
		}
	}

	return len(active), nil
}

// adapterPodLogLabel renders the pod's adapter identification for the
// reconcile create-log: the single kind for per-adapter fallback pods, the
// comma-joined kind set for (scope, environment) group pods. Group pods
// carry the kind set on the AnnotationAdapterKinds annotation — the comma
// separator is illegal in a label value (CRI-234 R1).
func adapterPodLogLabel(pod *corev1.Pod) string {
	if kind := pod.Labels[jobbuilder.LabelAdapterKind]; kind != "" {
		return kind
	}
	return pod.Annotations[jobbuilder.AnnotationAdapterKinds]
}

// isAdapterGroupPod reports whether pod is a (scope, environment) co-location
// pod rather than a per-adapter fallback pod. Group pods carry the
// environment label; the fallback builder never sets it.
func isAdapterGroupPod(pod *corev1.Pod) bool {
	return pod.Labels[jobbuilder.LabelEnvironment] != ""
}

// adapterContainerNamesEqual reports whether the existing pod's container
// name set matches the desired pod's. Only names are compared — the API
// server defaults mutable container fields, so a deep spec comparison would
// false-positive on cosmetic differences. Group container names are
// member-sensitive (they embed a hash of each member's handshake binding),
// so a name-set mismatch means the group's membership — or a member's
// binding — changed.
func adapterContainerNamesEqual(existing, desired *corev1.Pod) bool {
	if len(existing.Spec.Containers) != len(desired.Spec.Containers) {
		return false
	}
	desiredNames := make(map[string]struct{}, len(desired.Spec.Containers))
	for _, c := range desired.Spec.Containers {
		desiredNames[c.Name] = struct{}{}
	}
	for _, c := range existing.Spec.Containers {
		if _, ok := desiredNames[c.Name]; !ok {
			return false
		}
	}
	return true
}
