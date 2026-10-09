package controller

import (
	"context"
	"fmt"
	"sort"

	"time"

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
// Grouping (CRI-234 M7.2, KB-214): provisions carrying an environment
// identity are co-located — one pod per (scope, environment) running a
// SINGLE criteria-peer container that hosts every adapter kind of the pair
// (the engine's multi-adapter manifest). Provisions WITHOUT environment
// identity (engines older than CRI-233's fc95449) fall back to one pod per
// adapter, matching the pre-CRI-234 shape. Membership drift within a live
// scope's lifetime stays inside the peer container — the container set is
// constant, so the pod is never recreated for a released or newly
// provisioned member.
//
// Dial delivery (CRI-236/237/KB-214): wire-shaped provisions carry an
// accept_token (runner eae0181) delivered over the shim channel, and the
// legacy-shaped peer pods bake the dial host from the same address — both
// need the runner pod's routable IP. When any active provision needs it,
// the runner pod is resolved first; until it resolves (no running pod, no
// pod IP yet) no adapter mutation happens at all — existing pods stay
// untouched and the reconcile requeues on the next poll, so a peer pod is
// never built with a dangling dial address. Only the env-less fallback
// pods run adapter.sh's own discovery polling and can build without a
// runner IP.
func (r *CriteriaRunReconciler) reconcilePerScopeAdapters(ctx context.Context, run *criteriav1.CriteriaRun, castleRunID string, lifecycleEvents []events.LifecycleEvent, logger logr.Logger) (int, error) {
	if !run.Spec.PerScopeSessions {
		return 0, nil
	}

	active := events.ActiveProvisions(lifecycleEvents)

	// scopeOfPod maps each desired pod to the lifecycle member it hosts and
	// memberPod maps every active member to its pod, so the observed pod
	// states are reported per scope (KB-225 pod-state feed).
	memberPod := make(map[string]*corev1.Pod, len(active))
	memberScope := make(map[string]events.LifecycleEvent, len(active))

	runnerIP := ""
	needsRunnerIP := false
	for _, scope := range active {
		// Every peer pod needs the runner pod's routable IP: wire-shaped
		// delivery dials it to deliver the token, and the legacy peer
		// shape bakes the dial host from it (the direct `criteria peer`
		// ENTRYPOINT has no adapter.sh wrapper to poll the per-run
		// discovery files). Only the env-less fallback pods — they do run
		// adapter.sh's own discovery polling — can build without one.
		if scope.AcceptToken != "" || scope.Environment != "" {
			needsRunnerIP = true
			break
		}
	}
	if needsRunnerIP {
		var err error
		runnerIP, err = r.resolveRunnerIP(ctx, run)
		if err != nil {
			return 0, fmt.Errorf("resolving runner pod for peer/adapter token delivery: %w", err)
		}
		if runnerIP == "" {
			logger.Info("peer pod provisioning deferred: no runner pod with a routable IP yet; leaving adapter pods untouched",
				"run", run.Name)
			return len(active), nil
		}
	}

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
			pod := jobbuilder.BuildPerScopeAdapterPod(run, r.Defaults, scope, runnerIP)
			desired[pod.Name] = pod
			memberPod[pod.Name] = pod
			memberScope[pod.Name] = scope
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
		pod := jobbuilder.BuildPerScopePeerPod(run, r.Defaults, group.scopeID, group.environment, group.members, runnerIP)
		if pod == nil {
			continue
		}
		desired[pod.Name] = pod
		for _, member := range group.members {
			memberPod[pod.Name] = pod
			memberScope[pod.Name] = member
		}
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

	// Report the observed pod state per active scope to castle (KB-225):
	// best-effort adapter-event emission, deduped on transitions inside the
	// feed, so the engine's session-wait expiry can name the real pod phase
	// (KB-70 PodStateProbe). A pod that never leaves Pending is surfaced
	// with its scheduling/wait reason instead of burning the full 15m wait.
	if !r.PodState.Disabled() && castleRunID != "" {
		r.PodState.Emit(ctx, castleRunID, podStateReports(&existing, memberScope, time.Now(), logger), logger)
	}

	// Delete pods that are no longer desired first. This ensures a release for
	// one scope is processed before a provision-wanted for the next scope when
	// both events are pending. Desired names are stable functions of the
	// (scope, environment) pair: a released member changes the peer
	// container's manifest, never the pod — membership drift within a live
	// scope's lifetime stays inside the peer container (KB-214), so no
	// drift-recreate path remains. Pods still being deleted here are only
	// the retired name shapes (CRI-234 group pods, per-adapter fallback
	// pods whose provisions went away) and fully released scopes.
	deletedNames := make(map[string]struct{})
	for i := range existing.Items {
		pod := &existing.Items[i]
		if _, ok := desired[pod.Name]; ok {
			continue
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
// comma-joined kind set for (scope, environment) peer pods. Peer pods carry
// the kind set on the AnnotationAdapterKinds annotation — the comma
// separator is illegal in a label value (CRI-234 R1).
func adapterPodLogLabel(pod *corev1.Pod) string {
	if kind := pod.Labels[jobbuilder.LabelAdapterKind]; kind != "" {
		return kind
	}
	return pod.Annotations[jobbuilder.AnnotationAdapterKinds]
}

// resolveRunnerIP resolves the run's runner pod IP for wire token delivery
// (CRI-237): the runner pod is the one child carrying the runner role label,
// and its status.podIP is the routable address the shim's listen port dials
// to. Preference is deterministic — Running pods with an IP first, then any
// pod with an IP, each group ordered by name — so repeated reconciles pick
// the same pod while the runner restarts. Empty when no pod carries an IP
// yet (the caller defers adapter mutations and requeues).
func (r *CriteriaRunReconciler) resolveRunnerIP(ctx context.Context, run *criteriav1.CriteriaRun) (string, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(run.Namespace),
		client.MatchingLabels(map[string]string{
			jobbuilder.LabelRun:  run.Name,
			jobbuilder.LabelRole: jobbuilder.RoleRunner,
		}),
	); err != nil {
		return "", fmt.Errorf("listing runner pods: %w", err)
	}
	running := make([]corev1.Pod, 0, len(pods.Items))
	others := make([]corev1.Pod, 0, len(pods.Items))
	for _, pod := range pods.Items {
		if pod.Status.PodIP == "" {
			continue
		}
		if pod.Status.Phase == corev1.PodRunning {
			running = append(running, pod)
			continue
		}
		others = append(others, pod)
	}
	sortPodsByName(running)
	sortPodsByName(others)
	for _, pod := range append(running, others...) {
		if ip := pod.Status.PodIP; ip != "" {
			return ip, nil
		}
	}
	return "", nil
}

// sortPodsByName orders pods by name in place so the runner-IP preference is
// deterministic across reconciles.
func sortPodsByName(pods []corev1.Pod) {
	sort.Slice(pods, func(i, j int) bool { return pods[i].Name < pods[j].Name })
}
