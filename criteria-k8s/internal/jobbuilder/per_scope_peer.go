package jobbuilder

import (
	"crypto/sha256"
	"fmt"
	"path/filepath"
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
//
// Engine contract source (verified 2026-10-08): repo brokenbots/criteria PR
// #517 (KB-213), merge commit
// 3a4bac57db1f7c3bd18c04afd7722389fd638836. The manifest variable is
// CRITERIA_REMOTE_ADAPTERS, NOT the workstream card's literal
// CRITERIA_ADAPTERS: internal/peer/config.go:54 pins
// EnvAdapters = "CRITERIA_REMOTE_ADAPTERS", and its const-block comment
// explains the rename — the pre-existing CRITERIA_ADAPTERS name is the
// adapter install DIRECTORY consumed by adapterhost discovery, so the
// multi-adapter list moved under the CRITERIA_REMOTE_* family (also
// scrubbed from child environments). The rest of the grammar lives in
// internal/peer/adapterset.go (ParseAdaptersConfig, applyAdapterSpecOverrides,
// adapterEnvName, resolveSpecDigest) and is asserted against a pinned
// engine-contract fixture in per_scope_engine_contract_test.go.
//
// The pod dials the runner with the sorted-first member's identity
// (deterministic; the manifest's first child is the conn's dial child):
// per-scope token wiring semantics (CRI-236/237) are unchanged — a fully
// wire-shaped member set delivers the token on the wire
// (CRITERIA_REMOTE_TOKEN + CRITERIA_REMOTE_HOST), anything else keeps the
// legacy discovery-dir shape: the peer reads rotating per-(scope, adapter)
// token files from the runner's remote-tokens root on the shared data
// volume (CRITERIA_REMOTE_SCOPES_DIR) and dials the runner itself from the
// operator-baked CRITERIA_REMOTE_HOST.
// A mixed member set (wire plus legacy members) degrades to the legacy
// shape so no member loses its delivery channel.
//
// The image resolves from the dial member's kind (CRI-214 M14): the
// workflow object's adapterImages override, else the event's
// digest-verified image_reference, else the operator's registry/tag
// defaults.
//
// A multi-kind pair cannot boot from that one dial-kind image alone: a
// Dockerfile.peer engine image installs exactly ONE adapter binary
// (images/Dockerfile.peer puts the per-kind binary at
// /usr/local/bin/criteria-adapter-<ADAPTER_NAME>), and the engine's
// multi-adapter boot validates EVERY hosted child's binary and fails
// closed (internal/peer/adapterset.go, criteria PR #517, so a multi-kind
// manifest under a single-binary image is a pod that never boots).
// Single-kind pods stay image-supplied and image-complete. Multi-kind
// pods therefore stage every hosted kind's binary out of that kind's
// ALREADY-PINNED per-kind engine image — resolvePeerAdapterImage again,
// so the fleet's per-kind images and pins need no repin churn: one init
// container per kind (copy-<kind>) copies
// /usr/local/bin/criteria-adapter-<kind> from its kind's image into a
// shared emptyDir (PerScopePeerBinVolumeName mounted at
// PerScopePeerBinVolumePath), and the peer container mounts that dir
// read-only with a CRITERIA_ADAPTER_<KIND>_BINARY override for every
// hosted kind — the engine's per-adapter binary override grammar
// (applyAdapterSpecOverrides), the same keys the digest pins ride. The
// staged name keeps the conventional criteria-adapter-<kind> spelling,
// so the staged dir also stays scannable by the engine's directory
// fallback. A kind whose image lacks its binary fails closed at the init
// container, exactly like the engine's own boot validation.
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
			// The sequential per-kind init containers are the multi-kind
			// image-coherence mechanism staged below (empty member set is
			// defended above; single-kind pods stay image-supplied).
			InitContainers: peerBinCopyContainers(plan, defaults, members),
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
			Volumes: peerPodVolumes(plan, dataPVC, includeData, members),
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
// the per-resource maximum across the hosted members. A multi-kind pair
// additionally mounts the staged-binaries dir read-only and sets a
// CRITERIA_ADAPTER_<KIND>_BINARY override for every hosted kind (the
// peerBinCopyContainers staging contract); a single-kind pair stays
// image-supplied with no overrides.
func perScopePeerContainer(run *criteriav1.CriteriaRun, defaults Defaults, plan *workflowPlan, dial events.LifecycleEvent, members []events.LifecycleEvent, runnerIP string, includeData bool) corev1.Container {
	dialKind := adapterKind(dial)
	wireDelivery := peerWireDelivery(members, runnerIP)

	env := []corev1.EnvVar{
		{Name: "CRITERIA_RUN_JOB_NAME", Value: JobName(run)},
		// CRITERIA_REMOTE_ADAPTERS is the engine's env-first multi-adapter
		// manifest (KB-213; source-of-record citation in
		// BuildPerScopePeerPod's doc): the deduplicated, sorted kind set
		// hosted by this peer container. Per-kind
		// CRITERIA_ADAPTER_<KIND>_DIGEST pins each member's lockfile
		// digest, mirroring the per-member digest env semantics the
		// group-pod builder carried.
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
		// Legacy delivery (pre-eae0181 engines): no accept_token rode the
		// event, so the token cannot ride the wire either — and the peer
		// is a direct `criteria peer` ENTRYPOINT with no adapter.sh
		// wrapper to poll the per-run discovery files. The operator
		// therefore resolves the dial address itself and points the peer
		// at the runner's rotating-token root:
		//   - CRITERIA_REMOTE_HOST is baked from the runner pod's
		//     routable IP (the event's shim_listen_address supplies only
		//     the port); the peer's own config Resolve() requires it at
		//     boot, so the reconcile must defer pod creation while no
		//     runner pod carries an IP instead of baking a half-shaped
		//     env.
		//   - CRITERIA_REMOTE_SCOPES_DIR (internal/peer ScanRemoteScopes)
		//     makes the peer scan (scope, adapter) token files under the
		//     run discovery dir's remote-tokens root on the shared data
		//     volume and dial ONE conn per file, reading the rotated
		//     token material itself — the token never crosses the
		//     operator or the pod spec.
		// An unusable CRITERIA_REMOTE_SCOPES_DIR is not fatal at boot:
		// an empty scan leaves the peer waiting (a re-scan recovers on
		// the next rotation), while a missing host fails the boot loudly.
		if runnerIP != "" {
			env = append(env, corev1.EnvVar{
				Name:  "CRITERIA_REMOTE_HOST",
				Value: runnerDialAddr(runnerIP, dial.ShimAddress),
			})
		}
		if root := peerDiscoveryRoot(dial.TokenFile); root != "" {
			env = append(env, corev1.EnvVar{
				Name:  "CRITERIA_REMOTE_SCOPES_DIR",
				Value: root,
			})
		}
	}

	// A multi-kind pair stages every hosted kind's binary out of its own
	// per-kind engine image (peerBinCopyContainers) and points the engine's
	// per-adapter binary-override grammar at the staged copies: one
	// CRITERIA_ADAPTER_<KIND>_BINARY per kind, mounted read-only. Kinds are
	// the deduplicated sorted set, so the overrides and the mount land in a
	// deterministic order. A single-kind pair emits none — its image
	// supplies the (sole) binary conventionally, no emptyDir, no staging.
	env = append(env, peerAdapterBinariesEnv(members)...)

	return corev1.Container{
		Name:            PerScopePeerContainerName,
		Image:           resolvePeerAdapterImage(plan, dial, dialKind, defaults),
		ImagePullPolicy: corev1.PullIfNotPresent,
		// No Command: the criteria-adapter-<kind>-peer image's ENTRYPOINT
		// runs `criteria peer`, hosting the manifest's adapters directly
		// (ADR-0008). adapter.sh is never passed through in peer mode.
		SecurityContext: restrictedContainerSecurityContext(),
		Env:             appendEnvDistinct(env, plan.adapterEnvs()),
		VolumeMounts:    peerContainerMounts(plan, includeData, members),
		Resources:       peerContainerResources(members),
	}
}

// peerAdapterManifestEnv renders the peer container's multi-adapter
// manifest (KB-213): CRITERIA_REMOTE_ADAPTERS already declares the kind set
// as an env-first manifest, and this function returns the per-kind digest
// pins the engine keys each hosted member by — CRITERIA_ADAPTER_<KIND>_
// DIGEST, rendered with the kind uppercased and non-alphanumerics replaced
// by underscores (shell -> CRITERIA_ADAPTER_SHELL_DIGEST, triage-reviewer
// -> CRITERIA_ADAPTER_TRIAGE_REVIEWER_DIGEST). Identical kinds collapse;
// the sorted-first member's digest wins (the same member that dials, so
// the pin always matches the conn's handshake identity; same-kind members
// come from the same lockfile entry in practice).
//
// A kind with no digest gets NO digest env var at all, never an empty one:
// the engine's resolveSpecDigest (internal/peer/adapterset.go, criteria PR
// #517 commit 3a4bac5) short-circuits on an empty spec.Digest, and
// applyAdapterSpecOverrides trim-normalizes override values — so an absent
// variable and an empty-valued one are equally "no pinning". Emitting no
// variable for an unpinned member keeps the engine-facing manifest clean of
// pin placeholders that never pin anything.
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

// peerAdapterEnvName renders the env name prefix the engine's multi-adapter
// resolution contract keys per adapter kind.
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

// PerScopePeerBinVolumeName is the shared emptyDir a multi-kind peer pod
// stages the hosted adapter binaries into (the KB-214 review-remediation
// mechanism: a Dockerfile.peer engine image installs exactly ONE adapter
// binary, so a multi-kind manifest cannot boot from the dial kind's image
// alone — every kind's binary is staged out of that kind's own pinned
// image instead). Single-kind pods carry no such volume: their image is
// image-complete for the one manifest kind.
const PerScopePeerBinVolumeName = "peer-adapter-binaries"

// PerScopePeerBinVolumePath is the mount path of PerScopePeerBinVolumeName
// on both the staging init containers and the (read-only) peer container;
// each CRITERIA_ADAPTER_<KIND>_BINARY override points at a binary under
// this path.
const PerScopePeerBinVolumePath = "/usr/local/share/criteria-peer-adapters"

// engineImageAdapterBinaryPath prefixes the install path of an adapter
// binary inside the per-kind Dockerfile.peer engine images
// (images/Dockerfile.peer: /usr/local/bin/criteria-adapter-<ADAPTER_NAME>).
const engineImageAdapterBinaryPath = "/usr/local/bin/criteria-adapter-"

// peerBinBinaryPath renders the staged path of one hosted kind's adapter
// binary. The staged name keeps the engine's conventional install spelling,
// so the staged dir stays scannable by the engine's directory fallback too.
func peerBinBinaryPath(kind string) string {
	return filepath.Join(PerScopePeerBinVolumePath, "criteria-adapter-"+kind)
}

// peerAdapterKinds returns the deduplicated, sorted, raw adapter kinds
// hosted by a peer pod's member set — the set behind the manifest
// env value (adapterKindsLabel), the staged-binary init containers, and the
// per-kind binary overrides. Uses the raw kind, the same value the digest
// pins key off, so every per-kind family keys identically.
func peerAdapterKinds(members []events.LifecycleEvent) []string {
	seen := make(map[string]struct{}, len(members))
	kinds := make([]string, 0, len(members))
	for _, member := range members {
		kind := adapterKind(member)
		if _, ok := seen[kind]; ok {
			continue
		}
		seen[kind] = struct{}{}
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	return kinds
}

// peerBinCopyContainers builds the multi-kind staging init containers (one
// per hosted kind, sorted): each copies its kind's adapter binary out of
// that kind's ALREADY-PINNED engine image (resolvePeerAdapterImage — the
// same resolution the digest-verified event lane and the workflow override
// lane ride, so no new image shapes and no pin churn) into the shared
// staged-binaries emptyDir. Init containers run as the pod's criteria user
// (10001) before any container starts, mount the stage read-write, and get
// the same restricted container security context. Sequential per
// Kubernetes semantics; a missing binary fails the init container and the
// pod, mirroring the engine's fail-closed boot validation.
func peerBinCopyContainers(plan *workflowPlan, defaults Defaults, members []events.LifecycleEvent) []corev1.Container {
	kinds := peerAdapterKinds(members)
	if len(kinds) <= 1 {
		return nil
	}
	// Represent each kind's image resolution with the kind's first hosted
	// member (members arrive sorted): the same event resolvePeerAdapterImage
	// is evaluated against for the digest-verified image_reference lane.
	byKind := make(map[string]events.LifecycleEvent, len(kinds))
	for _, member := range members {
		kind := adapterKind(member)
		if _, ok := byKind[kind]; !ok {
			byKind[kind] = member
		}
	}
	containers := make([]corev1.Container, 0, len(kinds))
	for _, kind := range kinds {
		containers = append(containers, corev1.Container{
			Name:            peerBinCopyContainerName(kind),
			Image:           resolvePeerAdapterImage(plan, byKind[kind], kind, defaults),
			ImagePullPolicy: corev1.PullIfNotPresent,
			Command: []string{
				"/bin/cp",
				engineImageAdapterBinaryPath + kind,
				peerBinBinaryPath(kind),
			},
			VolumeMounts: []corev1.VolumeMount{
				{Name: PerScopePeerBinVolumeName, MountPath: PerScopePeerBinVolumePath},
			},
			SecurityContext: restrictedContainerSecurityContext(),
		})
	}
	return containers
}

// peerBinCopyContainerName renders an RFC1123-safe init container name for
// one hosted kind; the copy- prefix keeps the name in its own family and
// guarantees a valid leading character.
func peerBinCopyContainerName(kind string) string {
	var b strings.Builder
	b.WriteString("copy-")
	for _, r := range strings.ToLower(kind) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return trimHyphens(b.String())
}

// peerAdapterBinariesEnv renders the CRITERIA_ADAPTER_<KIND>_BINARY
// overrides a multi-kind peer container needs (the engine's per-adapter
// binary-override grammar, the same keys the digest pins ride), pointing
// every hosted kind at its staged copy under PerScopePeerBinVolumePath. A
// single-kind member set emits nothing: its image supplies the sole binary
// conventionally, and an override would pin it to a directory the pod does
// not even mount.
func peerAdapterBinariesEnv(members []events.LifecycleEvent) []corev1.EnvVar {
	kinds := peerAdapterKinds(members)
	if len(kinds) <= 1 {
		return nil
	}
	envs := make([]corev1.EnvVar, 0, len(kinds))
	for _, kind := range kinds {
		envs = append(envs, corev1.EnvVar{
			Name:  peerAdapterEnvName(kind) + "_BINARY",
			Value: peerBinBinaryPath(kind),
		})
	}
	return envs
}

// peerPodVolumes is the peer pod's volume set: the declared volumes (pod
// re-sourcing rules kept) plus the staged-binaries emptyDir whenever the
// pair actually hosts more than one kind.
func peerPodVolumes(plan *workflowPlan, dataPVC string, includeData bool, members []events.LifecycleEvent) []corev1.Volume {
	volumes := plan.podVolumes(dataPVC, includeData)
	if len(peerAdapterKinds(members)) <= 1 {
		return volumes
	}
	return append(volumes, corev1.Volume{
		Name:         PerScopePeerBinVolumeName,
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	})
}

// peerContainerMounts is the peer container's volume-mount set: the
// adapter mounts plus a READ-ONLY staged-binaries mount whenever the pair
// hosts more than one kind (the init containers stage binaries; the peer
// container only reads them).
func peerContainerMounts(plan *workflowPlan, includeData bool, members []events.LifecycleEvent) []corev1.VolumeMount {
	mounts := plan.peerAdapterMounts(includeData)
	if len(peerAdapterKinds(members)) <= 1 {
		return mounts
	}
	return append(mounts, corev1.VolumeMount{
		Name:      PerScopePeerBinVolumeName,
		MountPath: PerScopePeerBinVolumePath,
		ReadOnly:  true,
	})
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

// peerDiscoveryRoot derives the run discovery dir's rotating-token root a
// peer scan consumes (internal/peer ScanRemoteScopes) from a provision
// event's token_ref. Two live shapes exist and both derive the SAME run
// root:
//   - the rotating layout itself: a token_ref inside the run's
//     "<...>/remote-tokens" tree derives its nearest "remote-tokens"
//     ancestor;
//   - the fc95449/CRI-234 shape: token_ref = "<run>/tokens/<name>.token",
//     the static per-adapter sibling directory "<run>/tokens" — its
//     rotation root is the sibling "<run>/remote-tokens".
//
// Empty when neither shape matches: the peer then boots with no scopes
// dir and its host handshake fails loudly on the missing token instead of
// silently scanning an unrelated directory.
func peerDiscoveryRoot(tokenRef string) string {
	if tokenRef == "" {
		return ""
	}
	clean := filepath.Clean(tokenRef)
	if filepath.Base(filepath.Dir(clean)) == "tokens" {
		runDir := filepath.Dir(filepath.Dir(clean))
		return filepath.Join(runDir, "remote-tokens")
	}
	dir := filepath.Dir(clean)
	for hops := 0; hops < 4; hops++ {
		if filepath.Base(dir) == "remote-tokens" {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
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
// accept any string. Each kind is sanitized individually (peerAdapterKinds
// supplies the raw set) so the dedup stays stable across engine kind
// spellings.
func adapterKindsLabel(members []events.LifecycleEvent) string {
	kinds := peerAdapterKinds(members)
	for i, kind := range kinds {
		kinds[i] = safeLabelValue(kind)
	}
	return strings.Join(kinds, ",")
}
