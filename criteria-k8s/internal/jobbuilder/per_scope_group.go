package jobbuilder

import (
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/events"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PerScopeAdapterGroupName is the deterministic pod name for the single pod
// co-locating the adapters of one (scope, environment) pair (CRI-234): the
// pod's name pins the owning job, the scope id, and the environment identity
// so a re-provision of the same pair reconciles the same object. Hashing
// mirrors PerScopeAdapterPodName so both name shapes stay within the
// 63-character limit.
func PerScopeAdapterGroupName(run *criteriav1.CriteriaRun, scopeID, environment string) string {
	if environment == "" {
		return PerScopeAdapterPodName(run, "", scopeID)
	}
	name := fmt.Sprintf("%s-adp-%s-%s",
		JobName(run),
		shortHash(scopeID),
		shortHash(environment),
	)
	base := safeObjectName(name)
	if len(base) > 63 {
		base = base[:63]
		base = trimHyphens(base)
	}
	return base
}

// shortHash renders a 12-hex-character sha256 prefix of s, the same
// suffix scheme PerScopeAdapterPodName uses for scope ids.
func shortHash(s string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(s)))[:perScopePodHashLength]
}

// BuildPerScopeAdapterPodGroup constructs the ONE pod hosting every adapter
// of one (scope, environment) pair, each member as a separate container
// (CRI-234 M7.2). Co-location is config-declared trust: adapters NOT sharing
// an environment are grouped into different pods by the reconcile, because
// they never share this builder's call. The environment carries its
// volume/secret declarations via the run's stamped workflow plan, which is
// applied pod-wide — every container mounts the same declared volumes and
// secrets. The runner is never a co-tenant: only provision events reach this
// builder, and the runner pod is built separately.
//
// members are the provision events sharing the pair; their per-container
// handshake env is built from each member's own event, exactly as the
// per-adapter fallback builder does. The member order is normalized
// (sorted by scope key) so repeated reconciles produce byte-identical pod
// specs. Returns nil for an empty member set (defensive; the reconcile
// never calls with one).
func BuildPerScopeAdapterPodGroup(run *criteriav1.CriteriaRun, defaults Defaults, scopeID, environment string, members []events.LifecycleEvent) *corev1.Pod {
	if len(members) == 0 {
		return nil
	}

	members = append([]events.LifecycleEvent(nil), members...)
	sort.Slice(members, func(i, j int) bool {
		return members[i].ScopeKey() < members[j].ScopeKey()
	})

	dataPVC := firstNonEmpty(defaults.DataPVC, "criteria-data")
	plan := newWorkflowPlan(run)

	labels := baseLabels(run)
	labels[LabelRole] = RoleAdapter
	labels[LabelScopeID] = safeLabelValue(scopeID)
	labels[LabelAdapterKinds] = adapterKindsLabel(members)
	labels[LabelEnvironment] = safeLabelValue(environment)

	containers := make([]corev1.Container, 0, len(members))
	usedNames := make(map[string]int, len(members))
	for _, member := range members {
		name := uniqueAdapterContainerName(member, usedNames)
		usedNames[name]++
		containers = append(containers, perScopeAdapterContainer(run, member, plan, name))
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            PerScopeAdapterGroupName(run, scopeID, environment),
			Namespace:       targetNamespace(run),
			Labels:          labels,
			OwnerReferences: []metav1.OwnerReference{ownerReference(run)},
		},
		Spec: corev1.PodSpec{
			NodeSelector: map[string]string{
				"kubernetes.io/arch": "amd64",
			},
			Tolerations: []corev1.Toleration{
				{
					Key:      "catch",
					Operator: corev1.TolerationOpExists,
					Effect:   corev1.TaintEffectNoSchedule,
				},
			},
			RestartPolicy:                corev1.RestartPolicyOnFailure,
			AutomountServiceAccountToken: boolPtr(plan.hasSecrets()),
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: boolPtr(true),
				RunAsUser:    int64Ptr(10001),
				RunAsGroup:   int64Ptr(10001),
				FSGroup:      int64Ptr(10001),
				SeccompProfile: &corev1.SeccompProfile{
					Type: corev1.SeccompProfileTypeRuntimeDefault,
				},
			},
			Containers: containers,
			Volumes:    plan.adapterVolumes(dataPVC),
		},
	}
	if plan.hasSecrets() {
		pod.Spec.ServiceAccountName = "criteria-runner"
	}
	return pod
}

// adapterKindsLabel renders the deduplicated, sorted set of adapter kinds
// hosted by a group pod, comma-joined, for `kubectl` triage and the
// reconcile create-log. Each kind is sanitized individually so the joined
// value stays a valid Kubernetes label value.
func adapterKindsLabel(members []events.LifecycleEvent) string {
	seen := make(map[string]struct{}, len(members))
	kinds := make([]string, 0, len(members))
	for _, member := range members {
		kind := safeLabelValue(adapterKind(member))
		if _, ok := seen[kind]; ok {
			continue
		}
		seen[kind] = struct{}{}
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	return strings.Join(kinds, ",")
}

// uniqueAdapterContainerName derives a deterministic per-member container
// name: adapter-<sanitized member name>, with a -2/-3/... suffix on
// collisions. Two members with the same name cannot occur in a healthy
// history (ActiveProvisions keys by adapter/scope), but the engine resends
// provision_wanted on retries, so the suffix keeps the pod spec valid and
// byte-stable regardless.
func uniqueAdapterContainerName(member events.LifecycleEvent, used map[string]int) string {
	base := safeObjectName("adapter-" + adapterKind(member))
	if _, taken := used[base]; !taken {
		return base
	}
	for n := 2; ; n++ {
		candidate := fmt.Sprintf("%s-%d", base, n)
		if _, taken := used[candidate]; !taken {
			return candidate
		}
	}
}