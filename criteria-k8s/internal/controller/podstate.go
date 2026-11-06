package controller

import (
	"sort"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"

	"github.com/brokenbots/workflow-example/criteria-k8s/internal/events"
)

// podStateReports derives pod-state reports for active scopes from the
// adapter pods observed in one reconcile pass. Every active member maps to
// the pod hosting it (peer pods host every member of one (scope,
// environment) group), and EACH member gets its own report — a shared peer
// pod reporting once per pod would leave the other scopes' session waits
// blind. A member without a live pod reports nothing — there is nothing
// observed to name. The returned reports are ordered by scope key so
// repeated passes behave deterministically regardless of provisioning map
// iteration order.
//
// KB-361: members whose scope identity resolves empty are skipped — every
// other shape would emit a report the consumer cannot join. Two shapes
// arrive here scope-less: the KB-253 synthetic graph members (definition
// identities the peer manifest boot-completes with; "manifest shape, never
// a session" — no scope instance was ever provisioned for them) and
// degenerate engine events that omit scope_instance_id. Emitting either
// wedged the pod-state lane: validate rejected the empty scope_instance_id
// on every reconcile and the reject/re-emit loop flooded the operator log
// for the run's whole lifetime (kb-159/kb-146/kb-171).
func podStateReports(existing *corev1.PodList, memberPods map[string][]events.LifecycleEvent, now time.Time, logger logr.Logger) []events.PodStateReport {
	reports := make([]events.PodStateReport, 0, len(memberPods))
	for i := range existing.Items {
		pod := &existing.Items[i]
		for _, scope := range memberPods[pod.Name] {
			if scope.AdapterType == "" {
				// Engines without adapter_type (pre-v0.5.22) cannot be joined
				// by the consumer's probe key; skip rather than emit an
				// ambiguous event.
				logger.V(1).Info("skipping pod-state report: provision carries no adapter_type",
					"pod", pod.Name, "adapter", scope.AdapterName, "scope_id", scope.ScopeID)
				continue
			}
			if scope.ScopeID == "" {
				// No scope instance, nothing to join: definition-only
				// manifest members and scope-less engine events report
				// nothing (see the KB-361 note above the function).
				logger.V(1).Info("skipping pod-state report: member resolves no scope instance (definition-only manifest member or scope-less engine event)",
					"pod", pod.Name, "adapter", scope.AdapterName, "adapter_type", scope.AdapterType)
				continue
			}
			phase := pod.Status.Phase
			if phase == "" {
				continue
			}
			reason, message := podStateWaitReason(pod)
			reports = append(reports, events.PodStateReport{
				AdapterName: scope.AdapterName,
				AdapterType: scope.AdapterType,
				ScopeName:   scope.ScopeTag,
				ScopeID:     scope.ScopeID,
				Pod:         pod.Name,
				Phase:       string(phase),
				Reason:      reason,
				Message:     message,
				ObservedAt:  now,
			})
		}
	}
	sort.Slice(reports, func(i, j int) bool {
		return reports[i].ScopeKey() < reports[j].ScopeKey()
	})
	return reports
}

// podStateMaxMessage bounds free-form cluster-provided strings (scheduler
// condition messages, container wait/termination messages, pod status
// message) that ride the pod-state event, so one pathological node message
// cannot flood the run's shared event stream. 2048 keeps every real
// scheduler/containment message intact.
const podStateMaxMessage = 2048

// clampMessage truncates s rune-safely to podStateMaxMessage.
func clampMessage(s string) string {
	if len(s) <= podStateMaxMessage {
		return s
	}
	runes := []rune(s)
	if len(runes) > podStateMaxMessage {
		runes = runes[:podStateMaxMessage]
	}
	return string(runes)
}

// podStateWaitReason extracts the pod's most informative wait reason and
// message: a false PodScheduled condition (scheduling problems like
// Unschedulable/SchedulingGated) first, then container wait/termination
// reasons (ImagePullBackOff, CrashLoopBackOff, Error, ...), then the pod
// status message. Running pods without waits return empty values.
func podStateWaitReason(pod *corev1.Pod) (reason, message string) {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionFalse && cond.Reason != "" {
			return cond.Reason, clampMessage(cond.Message)
		}
	}
	for i := range pod.Status.ContainerStatuses {
		state := pod.Status.ContainerStatuses[i].State
		if w := state.Waiting; w != nil && w.Reason != "" {
			return w.Reason, clampMessage(w.Message)
		}
		if t := state.Terminated; t != nil && t.Reason != "" {
			return t.Reason, clampMessage(t.Message)
		}
	}
	if pod.Status.Message != "" {
		return "", clampMessage(pod.Status.Message)
	}
	return "", ""
}
