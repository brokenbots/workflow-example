package jobbuilder

import (
	"crypto/sha256"
	"fmt"
	"net"

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
// Token delivery is version-keyed (CRI-237): events carrying accept_token
// (runner commit eae0181, CRI-236) get the token on the wire through the
// pod spec's CRITERIA_REMOTE_TOKEN, dialing the resolved runner pod IP, and
// mount no shared data volume; events without it keep the legacy shape —
// CRITERIA_REMOTE_TOKEN_FILE pointing at the engine-rotated token file on
// the shared volume. The legacy branch is byte-identical to the pre-CRI-237
// builder so image-mode runs (frozen pre-eae0181 engine) keep working
// unchanged.
//
// The pod has zero Kubernetes privileges and, absent a stamped workflow
// object, mounts only the shared data volume (legacy delivery only) plus
// the pod-adapter scripts ConfigMap; the workflow object's declared volumes
// and CSI secrets are mounted as well, and declared secrets additionally run
// the pod under the criteria-runner service account for the OpenBao
// provider. The repo clone lives on the data PVC at
// /data/intake/<ticket>/repo. runnerIP is the resolved runner pod IP; wire
// delivery requires it, and an empty IP with an accept_token event degrades
// to the legacy shape (defensive — the reconcile never builds in that
// state).
func BuildPerScopeAdapterPod(run *criteriav1.CriteriaRun, defaults Defaults, scope events.LifecycleEvent, runnerIP string) *corev1.Pod {
	// Prefer the adapter implementation kind (shell/copilot/...) from the
	// event's adapter_type; older engines only carry the workflow's adapter
	// node name (AdapterName), which is not an image kind.
	kind := adapterKind(scope)
	name := PerScopeAdapterPodName(run, kind, scope.ScopeID)
	dataPVC := firstNonEmpty(defaults.DataPVC, "criteria-data")
	plan := newWorkflowPlan(run)
	wireDelivery := wireTokenDelivery(scope, runnerIP)
	includeData := !wireDelivery || plan.declaresDataMount()

	labels := baseLabels(run)
	labels[LabelRole] = RoleAdapter
	labels[LabelAdapterKind] = kind
	labels[LabelScopeID] = safeLabelValue(scope.ScopeID)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       TargetNamespace(run),
			Labels:          labels,
			OwnerReferences: []metav1.OwnerReference{ownerReference(run)},
		},
		Spec: corev1.PodSpec{
			NodeSelector: map[string]string{
				"kubernetes.io/arch": jobNodeArch(defaults),
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
				perScopeAdapterContainer(run, defaults, scope, plan, fmt.Sprintf("adapter-%s", kind), runnerIP),
			},
			Volumes: plan.adapterVolumes(dataPVC, includeData),
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
// contract exactly. Token delivery is version-keyed (CRI-237): accept_token
// events (runner eae0181) carry the token on the wire — CRITERIA_REMOTE_TOKEN
// plus a routable CRITERIA_REMOTE_HOST dial address built from the resolved
// runner pod IP and the event's shim listen port — while pre-eae0181 events
// keep the CRITERIA_REMOTE_TOKEN_FILE file surface and the discovery-file
// host polling. The container's image resolves per member (CRI-214 M14):
// the workflow object's adapterImages override, else the member event's
// digest-verified image_reference, else the operator's registry/tag
// defaults.
func perScopeAdapterContainer(run *criteriav1.CriteriaRun, defaults Defaults, scope events.LifecycleEvent, plan *workflowPlan, name, runnerIP string) corev1.Container {
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
	if wireTokenDelivery(scope, runnerIP) {
		// Wire token delivery (CRI-237): the token reaches the adapter pod
		// on the wire through its existing authenticated shim channel, so
		// the pod mounts no shared data volume and reads no rotated token
		// file. CRITERIA_REMOTE_HOST is the runner pod's routable dial
		// address (the event's shim_listen_address is the engine's bind
		// address, e.g. "[::]:7778" — only its port is usable; the host
		// would not route from a separate pod).
		env = append(env,
			corev1.EnvVar{Name: "CRITERIA_REMOTE_HOST", Value: runnerDialAddr(runnerIP, scope.ShimAddress)},
			corev1.EnvVar{Name: "CRITERIA_REMOTE_TOKEN", Value: scope.AcceptToken},
		)
	} else {
		// Legacy delivery (pre-eae0181 engines): the adapter resolves the
		// host from the runner's discovery file and the token from the
		// engine-rotated token file on the shared data volume.
		if scope.TokenFile != "" {
			env = append(env, corev1.EnvVar{Name: "CRITERIA_REMOTE_TOKEN_FILE", Value: scope.TokenFile})
		}
	}

	return corev1.Container{
		Name:            name,
		Image:           resolveAdapterImage(plan, scope, kind, defaults),
		ImagePullPolicy: corev1.PullIfNotPresent,
		SecurityContext: restrictedContainerSecurityContext(),
		Command:         []string{"/opt/criteria-pod-adapter/adapter.sh"},
		Env:             appendEnvDistinct(env, plan.adapterEnvs()),
		VolumeMounts:    plan.adapterMounts(!wireTokenDelivery(scope, runnerIP) || plan.declaresDataMount()),
		Resources:       adapterResources(kind),
	}
}

// wireTokenDelivery reports whether the member's token reaches the adapter
// pod on the wire (CRI-237): the event carries the engine-minted accept
// token (runner commit eae0181, CRI-236) and the reconcile resolved a
// runner pod IP to dial. Without either input the legacy token-file
// delivery applies.
func wireTokenDelivery(scope events.LifecycleEvent, runnerIP string) bool {
	return scope.AcceptToken != "" && runnerIP != ""
}

// runnerDialAddr renders the runner pod's routable shim dial address:
// runnerIP plus the port of the engine's shim listen address (e.g.
// "[::]:7778" -> ":7778"). The listen address is the engine's bind address,
// whose host component never routes from a separate pod, so only the port
// is consumed; an unparsable or empty address falls back to the shim's
// conventional k8s port.
func runnerDialAddr(runnerIP, shimListenAddress string) string {
	port := shimPort(shimListenAddress)
	return runnerIP + ":" + port
}

// shimPort extracts the TCP port from the engine's shim listen address.
func shimPort(shimListenAddress string) string {
	if _, port, err := net.SplitHostPort(shimListenAddress); err == nil && port != "" {
		return port
	}
	return "7778"
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
				corev1.ResourceMemory: resourceQuantity("2Gi"),
				corev1.ResourceCPU:    resourceQuantity("500m"),
			},
			Limits: corev1.ResourceList{
				// CRI-272: the 4Gi cap OOM-killed the adapter mid-tool on
				// memory-heavy workloads (npm ci + full test baseline + Go
				// builds in one sandbox), which surfaced as the copilot
				// session-death crash family. Raise to 8Gi.
				corev1.ResourceMemory: resourceQuantity("8Gi"),
				corev1.ResourceCPU:    resourceQuantity("2000m"),
			},
		}
	}
	return resources
}
