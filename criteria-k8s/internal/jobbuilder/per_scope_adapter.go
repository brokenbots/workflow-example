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
// event. The pod has zero Kubernetes privileges, zero CSI mounts, and only
// the shared data volume plus the pod-adapter scripts ConfigMap. The repo
// clone lives on the data PVC at /data/intake/<ticket>/repo.
func BuildPerScopeAdapterPod(run *criteriav1.CriteriaRun, defaults Defaults, scope events.LifecycleEvent) *corev1.Pod {
	kind := scope.AdapterName
	name := PerScopeAdapterPodName(run, kind, scope.ScopeID)
	dataPVC := firstNonEmpty(defaults.DataPVC, "criteria-data")

	labels := baseLabels(run)
	labels["criteria.brokenbots.dev/role"] = "adapter"
	labels["criteria.brokenbots.dev/adapter-kind"] = kind
	labels["criteria.brokenbots.dev/scope-id"] = safeLabelValue(scope.ScopeID)

	digest := scope.Digest
	if digest != "" && !hasDigestPrefix(digest) {
		digest = "sha256:" + digest
	}

	env := []corev1.EnvVar{
		{Name: "ADAPTER_KIND", Value: kind},
		{Name: "CRITERIA_RUN_JOB_NAME", Value: JobName(run)},
		// CRITERIA_REMOTE_HOST is deliberately NOT set here. The event's
		// ShimListenAddress is the engine's bind address on its own loopback
		// (e.g. "[::]:7778") - unreachable from a separate adapter pod. The
		// runner publishes its routable address (POD_IP:7778) to the shared
		// discovery file; adapter.sh falls back to polling it when this env
		// is unset. An engine-side fix to emit a routable address in the
		// event can reintroduce the env later.
		{Name: "CRITERIA_REMOTE_DIGEST", Value: digest},
		{Name: "CRITERIA_SCOPE_ID", Value: scope.ScopeID},
	}
	if scope.ScopeTag != "" {
		env = append(env, corev1.EnvVar{Name: "CRITERIA_SCOPE_TAG", Value: scope.ScopeTag})
	}
	if scope.TokenFile != "" {
		env = append(env, corev1.EnvVar{Name: "CRITERIA_REMOTE_TOKEN_FILE", Value: scope.TokenFile})
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       run.Namespace,
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
			AutomountServiceAccountToken: boolPtr(false),
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
				{
					Name:            fmt.Sprintf("adapter-%s", kind),
					Image:           adapterImage(kind),
					ImagePullPolicy: corev1.PullIfNotPresent,
					SecurityContext: restrictedContainerSecurityContext(),
					Command:         []string{"/opt/criteria-pod-adapter/adapter.sh"},
					Env:             env,
					VolumeMounts: []corev1.VolumeMount{
						{Name: "data", MountPath: "/data"},
						{Name: "scripts", MountPath: "/opt/criteria-pod-adapter"},
					},
					Resources: adapterResources(kind),
				},
			},
			Volumes: []corev1.Volume{
				dataVolume(dataPVC),
				scriptsVolume(),
			},
		},
	}
	return pod
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
