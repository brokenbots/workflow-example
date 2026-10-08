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

// PerScopePeerContainerName is the single container hosted by every (scope,
// environment) peer pod. Co-location is resolved by the engine's multi-adapter
// manifest (KB-213), not by container membership, so the container name is
// constant: there is exactly one per pod, and it must never change for
// membership drift (pod container sets are immutable).
const PerScopePeerContainerName = "criteria-peer"

// PerScopePeerPodName is the deterministic pod name for the ONE pod
// co-locating the adapters of one (scope, environment) pair (KB-214): the
// pod runs a SINGLE container whose ENTRYPOINT is the criteria peer
// (Dockerfile.peer images), hosting every adapter kind of the pair behind
// the engine's multi-adapter manifest (KB-213). The name pins the owning
// job, the scope id, and the environment identity, so a re-provision of the
// same pair reconciles the same object. The -peer- middle segment keeps
// the name in its own family, away from both the retired CRI-234 group-pod
// names and the per-adapter fallback names, so a rollout evicts the old
// shapes by name without any spec comparison.
func PerScopePeerPodName(run *criteriav1.CriteriaRun, scopeID, environment string) string {
	name := fmt.Sprintf("%s-peer-%s-%s",
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

// shortHash renders a 12-hex-character sha256 prefix of s, the same suffix
// scheme PerScopeAdapterPodName uses for scope ids.
func shortHash(s string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(s)))[:perScopePodHashLength]
}

// BuildPerScopePeerPod constructs the ONE pod hosting every adapter of one
// (scope, environment) pair as a SINGLE container running the criteria peer
// (KB-214). The container's image is a Dockerfile.peer image whose
// ENTRYPOINT is `criteria peer` (ADR-0008): the pod spec sets no command
// override and never passes through adapter.sh — adapter supervision and
// cycling happen inside the peer container, so membership changes within a
// live scope's lifetime never touch the pod.
//
// The hosted member set rides the engine's multi-adapter manifest
// (KB-213): CRITERIA_REMOTE_ADAPTERS carries the deduplicated, sorted
// adapter kinds for the pair (the env-first resolution manifest), and
// CRITERIA_ADAPTER_<KIND>_DIGEST pins each member's lockfile digest, the
// same per-member digest env semantics the group-pod builder carried.
// Engine-note deviation: the manifest variable is CRITERIA_REMOTE_ADAPTERS,
// not the workstream card's literal CRITERIA_ADAPTERS — the engine card
// renamed it because CRITERIA_ADAPTERS is the adapter binary install
// directory in-tree, and the pinned manifest contract
// (internal/peer/adapterset.go) owns the name.
//
// The pod dials the runner with the sorted-first member's identity
// (deterministic; the manifest's first child is the conn's dial child):
// per-scope token wiring semantics (CRI-236/237) are unchanged — a fully
// wire-shaped member set delivers the token on the wire
// (CRITERIA_REMOTE_TOKEN + CRITERIA_REMOTE_HOST), anything else keeps the
// legacy token-file surface. A mixed member set (wire plus legacy members)
// degrades to the legacy shape so no member loses its delivery channel.
//
// The image resolves from the dial member's kind (CRI-214 M14): the
// workflow object's adapterImages override, else the event's
// digest-verified image_reference, else the operator's registry/tag
// defaults. A multi-kind pair hosts every kind's child behind this one
// image's criteria binary; the fleet image set must supply binaries for
// every manifest kind, which the engine enforces at boot (missing adapter
// binaries fail closed — the pod never half-boots).
//
// members are the provision events sharing the pair. Their order is
// normalized (adapter type, then adapter node name, then scope key) so
// repeated reconciles produce byte-identical pod specs. Returns nil for an
// empty member set (defensive; the reconcile never calls with one).
func BuildPerScopePeerPod(run *criteriav1.CriteriaRun, defaults Defaults, scopeID, environment string, members []events.LifecycleEvent, runnerIP string) *corev1.Pod {
	if len(members) == 0 {
		return nil
	}

	members = append([]events.LifecycleEvent(nil), members...)
	sort.Slice(members, func(i, j int) bool {
		a, b := members[i], members[j]
		if a.ScopeKey() != b.ScopeKey() {
			return a.ScopeKey() < b.ScopeKey()
		}
		if adapterKind(a) != adapterKind(b) {
			return adapterKind(a) < adapterKind(b)
		}
		return a.AdapterName < b.AdapterName
	})
	dial := members[0]

	dataPVC := firstNonEmpty(defaults.DataPVC, "criteria-data")
	plan := newWorkflowPlan(run)
	wireDelivery := peerWireDelivery(members, runnerIP)
	includeData := !wireDelivery || plan.declaresDataMount()

	labels := baseLabels(run)
	labels[LabelRole] = RoleAdapter
	labels[LabelScopeID] = safeLabelValue(scopeID)
	labels[LabelEnvironment] = safeLabelValue(environment)
	// The comma-joined kind set is an ANNOTATION value, not a label value:
	// a comma is illegal in a Kubernetes label value (CRI-234 R1).
	annotations := map[string]string{
		AnnotationAdapterKinds: adapterKindsLabel(members),
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            PerScopePeerPodName(run, scopeID, environment),
			Namespace:       TargetNamespace(run),
			Labels:          labels,
			Annotations:     annotations,
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
			// OnFailure restarts the container in place after a child
			// crash inside the peer; the pod itself is never recreated
			// for membership drift (the manifest hosts the children).
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
				perScopePeerContainer(run, defaults, plan, dial, members, runnerIP, includeData),
			},
			Volumes: plan.podVolumes(dataPVC, includeData),
		},
	}
	if plan.hasSecrets() {
		// The OpenBao CSI provider authenticates the pod through its
		// service account token; pods carrying declared secrets therefore
		// run under the criteria-runner service account.
		pod.Spec.ServiceAccountName = "criteria-runner"
	}
	plan.applyHostAffinity(pod.Labels, &pod.Spec)
	return pod
}

// maxLabelValueLength is the Kubernetes label value limit.
const maxLabelValueLength = 63

// perScopePeerContainer builds the peer pod's single container: no command
// override (the Dockerfile.peer image's ENTRYPOINT runs `criteria peer`),
// the multi-adapter manifest env, per-kind digest pins, the dial member's
// scope/token handshake env, and the aggregate of the hosted kinds'
// resources. A multi-kind pair shares one container, so its resources take
// the per-resource maximum across the hosted members.
func perScopePeerContainer(run *criteriav1.CriteriaRun, defaults Defaults, plan *workflowPlan, dial events.LifecycleEvent, members []events.LifecycleEvent, runnerIP string, includeData bool) corev1.Container {
	dialKind := adapterKind(dial)
	wireDelivery := peerWireDelivery(members, runnerIP)

	env := []corev1.EnvVar{
		{Name: "CRITERIA_RUN_JOB_NAME", Value: JobName(run)},
		// CRITERIA_REMOTE_ADAPTERS is the engine's env-first multi-adapter
		// manifest (KB-213): the deduplicated, sorted kind set hosted by
		// this peer container. Per-kind CRITERIA_ADAPTER_<KIND>_DIGEST pins
		// each member's lockfile digest, mirroring the per-member digest
		// env semantics the group-pod builder carried.
		{Name: "CRITERIA_REMOTE_ADAPTERS", Value: adapterKindsLabel(members)},
		// The remote-runner binary presents CRITERIA_REMOTE_SCOPE in the
		// identity handshake; the shim registers scope tokens under
		// <scopeName>/<scopeInstanceID> and rejects an empty scope in
		// per-scope mode. ScopeTag is the human-readable tag (empty for the
		// root scope) - the registration key is the name/scopeID pair.
		{Name: "CRITERIA_REMOTE_SCOPE", Value: dial.ScopeTag + "/" + dial.ScopeID},
		{Name: "CRITERIA_SCOPE_ID", Value: dial.ScopeID},
	}
	if dial.ScopeTag != "" {
		env = append(env, corev1.EnvVar{Name: "CRITERIA_SCOPE_TAG", Value: dial.ScopeTag})
	}
	env = append(env, peerAdapterManifestEnv(members)...)
	if wireDelivery {
		// Wire token delivery (CRI-237): the token reaches the peer pod on
		// the wire through its authenticated shim channel, so the pod
		// mounts no shared data volume and reads no rotated token file.
		// CRITERIA_REMOTE_HOST is the runner pod's routable dial address
		// (the event's shim_listen_address is the engine's bind address,
		// e.g. "[::]:7778" — only its port is usable; the host would not
		// route from a separate pod).
		env = append(env,
			corev1.EnvVar{Name: "CRITERIA_REMOTE_HOST", Value: runnerDialAddr(runnerIP, dial.ShimAddress)},
			corev1.EnvVar{Name: "CRITERIA_REMOTE_TOKEN", Value: dial.AcceptToken},
		)
	} else {
		// Legacy delivery (pre-eae0181 engines): the peer resolves the
		// host from the runner's discovery file and the token from the
		// engine-rotated token file on the shared data volume.
		if dial.TokenFile != "" {
			env = append(env, corev1.EnvVar{Name: "CRITERIA_REMOTE_TOKEN_FILE", Value: dial.TokenFile})
		}
	}

	return corev1.Container{
		Name:            PerScopePeerContainerName,
		Image:           resolveAdapterImage(plan, dial, dialKind, defaults),
		ImagePullPolicy: corev1.PullIfNotPresent,
		// No Command: the criteria-adapter-<kind>-peer image's ENTRYPOINT
		// runs `criteria peer`, hosting the manifest's adapters directly
		// (ADR-0008). adapter.sh is never passed through in peer mode.
		SecurityContext: restrictedContainerSecurityContext(),
		Env:             appendEnvDistinct(env, plan.adapterEnvs()),
		VolumeMounts:    plan.peerAdapterMounts(includeData),
		Resources:       peerContainerResources(members),
	}
}

// peerAdapterManifestEnv renders the manifest and the per-kind digest pins
// for one peer container: the kind list plus one
// CRITERIA_ADAPTER_<KIND>_DIGEST entry per hosted member kind (the kind is
// uppercased and non-alphanumerics become underscores, the engine's env
// name convention). Duplicate kinds — same kind hosted twice in one pair —
// collide on one env entry, so the first member's digest wins; digests of
// same-kind members come from the same lockfile entry in practice.
func peerAdapterManifestEnv(members []events.LifecycleEvent) []corev1.EnvVar {
	envs := make([]corev1.EnvVar, 0, len(members))
	seen := make(map[string]struct{}, len(members))
	for _, member := range members {
		kind := adapterKind(member)
		if _, ok := seen[kind]; ok {
			continue
		}
		seen[kind] = struct{}{}
		if member.Digest == "" {
			continue
		}
		digest := member.Digest
		if !hasDigestPrefix(digest) {
			digest = "sha256:" + digest
		}
		envs = append(envs, corev1.EnvVar{
			Name:  peerAdapterEnvName(kind) + "_DIGEST",
			Value: digest,
		})
	}
	return envs
}

// peerAdapterManifestEnv returns the env names the engine's multi-adapter
// resolution contract keys per adapter kind: the kind uppercased with
// non-alphanumerics replaced by underscores (shell -> SHELL, triage-reviewer
// -> TRIAGE_REVIEWER).
func peerAdapterEnvName(kind string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(kind) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			continue
		}
		b.WriteByte('_')
	}
	return "CRITERIA_ADAPTER_" + b.String()
}

// peerContainerResources takes the per-resource maximum across the hosted
// members: a multi-kind pair hosts every kind's children in one container,
// so the container must fit the hungriest member's declaration.
func peerContainerResources(members []events.LifecycleEvent) corev1.ResourceRequirements {
	res := adapterResources(adapterKind(members[0]))
	for _, member := range members[1:] {
		other := adapterResources(adapterKind(member))
		for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
			resMax(res.Requests, other.Requests, name)
			resMax(res.Limits, other.Limits, name)
		}
	}
	return res
}

// peerWireDelivery reports whether every member of the pair delivers its
// token on the wire (CRI-237). A mixed member set — impossible in practice,
// since one engine emits one event shape — degrades to the legacy
// token-file shape so no member loses its delivery channel.
func peerWireDelivery(members []events.LifecycleEvent, runnerIP string) bool {
	if runnerIP == "" {
		return false
	}
	for _, member := range members {
		if member.AcceptToken == "" {
			return false
		}
	}
	return len(members) > 0
}

// resMax keeps the larger of the two per-resource quantities in `into`.
func resMax(into, other corev1.ResourceList, name corev1.ResourceName) {
	otherQ, ok := other[name]
	if !ok || otherQ.IsZero() {
		return
	}
	if intoQ, exists := into[name]; !exists || otherQ.Cmp(intoQ) > 0 {
		into[name] = otherQ
	}
}

// adapterKindsLabel renders the deduplicated, sorted set of adapter kinds
// hosted by a peer pod, comma-joined, for `kubectl` triage, the manifest
// env value, and the reconcile create-log. The value is an ANNOTATION value
// (AnnotationAdapterKinds), not a label value: the comma separator is
// illegal in a Kubernetes label value (CRI-234 R1), while annotations
// accept any string. Each kind is sanitized individually so the dedup
// stays stable across engine kind spellings.
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