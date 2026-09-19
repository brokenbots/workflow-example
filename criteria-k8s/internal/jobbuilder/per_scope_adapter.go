package jobbuilder

import (
	"crypto/sha256"
	"fmt"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/events"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const perScopePodHashLength = 12

// PerScopeAdapterPodName returns a DNS-safe pod name for a per-scope adapter.
// The scope id is hashed so the name stays within the 63-character limit while
// remaining deterministic and unique enough to avoid collisions between
// sequential scopes.
func PerScopeAdapterPodName(run *criteriav1.CriteriaRun, kind, scopeID string) string {
	job := JobName(run)
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(scopeID)))[:perScopePodHashLength]
	base := fmt.Sprintf("%s-adp-%s-%s", job, kind, hash)
	base = safeObjectName(base)
	if len(base) > 63 {
		base = base[:63]
		base = trimHyphens(base)
	}
	return base
}

// BuildPerScopeAdapterPod constructs a Pod for one provision-wanted lifecycle
// event. It is the per-adapter fallback pod for events without environment
// identity (engines older than CRI-233's fc95449): its name and shape match
// the pre-CRI-234 builder verbatim, so an operator rollout against older
// events keeps reconciling the same pods. Adapters sharing an environment are
// co-located by BuildPerScopeAdapterPodGroup instead.
//
// The pod has zero Kubernetes privileges and, absent a stamped workflow
// object, mounts only the shared data volume plus the pod-adapter scripts
// ConfigMap; the workflow object's declared volumes and CSI secrets are
// mounted as well, and declared secrets additionally run the pod under the
// criteria-runner service account for the OpenBao provider. The repo clone
// lives on the data PVC at /data/intake/<ticket>/repo.
func BuildPerScopeAdapterPod(run *criteriav1.CriteriaRun, defaults Defaults, scope events.LifecycleEvent) *corev1.Pod {
	// Prefer the adapter implementation kind (shell/copilot/...) from the
	// event's adapter_type; older engines only carry the workflow's adapter
	// node name (AdapterName), which is not an image kind.
	kind := adapterKind(scope)
	name := PerScopeAdapterPodName(run, kind, scope.ScopeID)
	dataPVC := firstNonEmpty(defaults.DataPVC, "criteria-data")
	plan := newWorkflowPlan(run)

	labels := baseLabels(run)
	labels[LabelRole] = RoleAdapter
	labels[LabelAdapterKind] = kind
	labels[LabelScopeID] = safeLabelValue(scope.ScopeID)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
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
			Containers: []corev1.Container{
				perScopeAdapterContainer(run, scope, plan, fmt.Sprintf("adapter-%s", kind)),
			},
			Volumes: plan.adapterVolumes(dataPVC),
		},
	}
	if plan.hasSecrets() {
		// The OpenBao CSI provider authenticates the pod through its
		// service account token; adapter pods carrying declared secrets
		// therefore run under the criteria-runner service account.
		pod.Spec.ServiceAccountName = "criteria-runner"
	}
	plan.applyHostAffinity(pod.Labels, &pod.Spec)
	return pod
}

// perScopeAdapterContainer builds one adapter container for a per-scope
// adapter: the handshake env from the member's own provision event, the
// workflow plan's declared volumes/secrets/env, and the kind's resources.
// Both the per-adapter fallback pod and the (scope, environment) group pod
// build their containers here, so the two pod shapes share the handshake
// contract exactly.
func perScopeAdapterContainer(run *criteriav1.CriteriaRun, scope events.LifecycleEvent, plan *workflowPlan, name string) corev1.Container {
	kind := adapterKind(scope)

	digest := scope.Digest
	if digest != "" && !hasDigestPrefix(digest) {
		digest = "sha256:" + digest
	}

	env := []corev1.EnvVar{
		{Name: "ADAPTER_KIND", Value: kind},
		// CRITERIA_ADAPTER_NAME is the adapter TYPE presented in the
		// identity handshake; the engine's lockfile digest verifier keys by
		// type. The workflow's adapter node name (instance) is NOT usable
		// here: the lockfile has no instance-level entries, so verifying by
		// node name fails with "adapter not found in lockfile". The instance
		// identity is already bound by the scope handshake.
		{Name: "CRITERIA_ADAPTER_NAME", Value: kind},
		{Name: "CRITERIA_RUN_JOB_NAME", Value: JobName(run)},
		// CRITERIA_REMOTE_HOST is deliberately NOT set here. The event's
		// ShimListenAddress is the engine's bind address on its own loopback
		// (e.g. "[::]:7778") - unreachable from a separate adapter pod. The
		// runner publishes its routable address (POD_IP:7778) to the shared
		// discovery file; adapter.sh falls back to polling it when this env
		// is unset. An engine-side fix to emit a routable address in the
		// event can reintroduce the env later.
		{Name: "CRITERIA_REMOTE_DIGEST", Value: digest},
		// The remote-runner binary presents CRITERIA_REMOTE_SCOPE in the
		// identity handshake; the shim registers scope tokens under
		// <scopeName>/<scopeInstanceID> and rejects an empty scope in
		// per-scope mode. ScopeTag is the human-readable tag (empty for the
		// root scope) - the registration key is the name/scopeID pair.
		{Name: "CRITERIA_REMOTE_SCOPE", Value: scope.ScopeTag + "/" + scope.ScopeID},
		{Name: "CRITERIA_SCOPE_ID", Value: scope.ScopeID},
	}
	if scope.ScopeTag != "" {
		env = append(env, corev1.EnvVar{Name: "CRITERIA_SCOPE_TAG", Value: scope.ScopeTag})
	}
	if scope.TokenFile != "" {
		env = append(env, corev1.EnvVar{Name: "CRITERIA_REMOTE_TOKEN_FILE", Value: scope.TokenFile})
	}

	return corev1.Container{
		Name:            name,
		Image:           adapterImage(kind),
		ImagePullPolicy: corev1.PullIfNotPresent,
		SecurityContext: restrictedContainerSecurityContext(),
		Command:         []string{"/opt/criteria-pod-adapter/adapter.sh"},
		Env:             appendEnvDistinct(env, plan.adapterEnvs()),
		VolumeMounts:    plan.adapterMounts(),
		Resources:       adapterResources(kind),
	}
}

// adapterKind resolves the adapter implementation kind for an event,
// preferring adapter_type and falling back to the adapter node name for
// engines that predate its emission (CRI-140 semantics).
func adapterKind(scope events.LifecycleEvent) string {
	if scope.AdapterType != "" {
		return scope.AdapterType
	}
	return scope.AdapterName
}

func hasDigestPrefix(s string) bool {
	return len(s) > 7 && s[:7] == "sha256:"
}

func trimHyphens(s string) string {
	for len(s) > 0 && s[len(s)-1] == '-' {
		s = s[:len(s)-1]
	}
	for len(s) > 0 && s[0] == '-' {
		s = s[1:]
	}
	if s == "" {
		return "unknown"
	}
	return s
}

func adapterResources(kind string) corev1.ResourceRequirements {
	resources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceMemory: resourceQuantity("512Mi"),
			corev1.ResourceCPU:    resourceQuantity("250m"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceMemory: resourceQuantity("2Gi"),
			corev1.ResourceCPU:    resourceQuantity("1000m"),
		},
	}
	if kind == "copilot" {
		resources = corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceMemory: resourceQuantity("1Gi"),
				corev1.ResourceCPU:    resourceQuantity("500m"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceMemory: resourceQuantity("4Gi"),
				corev1.ResourceCPU:    resourceQuantity("2000m"),
			},
		}
	}
	return resources
}
