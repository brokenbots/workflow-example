package jobbuilder_test

import (
	"testing"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/events"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/jobbuilder"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apivalidation "k8s.io/apimachinery/pkg/api/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metav1validation "k8s.io/apimachinery/pkg/apis/meta/v1/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// KB-214: adapters sharing one (scope, environment) pair are co-located in
// ONE pod with ONE container running the criteria peer (the Dockerfile.peer
// ENTRYPOINT shape); the hosted member set rides the engine's multi-adapter
// manifest; the runner is never a co-tenant; the pod never passes through
// adapter.sh; env-less events keep the per-adapter fallback shape.

func groupTestRun() *criteriav1.CriteriaRun {
	return &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{Name: "cri-234", Namespace: "criteria-jobs", UID: "run-uid"},
		Spec:       criteriav1.CriteriaRunSpec{TicketID: "CRI-234"},
	}
}

func groupMember(adapter, kind, scopeID, environment string) events.LifecycleEvent {
	return events.LifecycleEvent{
		Event:       events.EventProvisionWanted,
		RunID:       "CRI-234",
		ScopeID:     scopeID,
		ScopeTag:    "root-scope",
		AdapterName: adapter,
		AdapterType: kind,
		Digest:      "sha256:deadbeef",
		TokenFile:   "/data/intake/CRI-234/tokens/" + adapter,
		Environment: environment,
	}
}

func containerEnvMap(c corev1.Container) map[string]string {
	env := make(map[string]string, len(c.Env))
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	return env
}

func TestBuildPerScopePeerPodSameEnvironmentOnePod(t *testing.T) {
	run := groupTestRun()
	members := []events.LifecycleEvent{
		groupMember("intake", "shell", "scope-a", "ci"),
		groupMember("review", "copilot", "scope-a", "ci"),
	}

	pod := jobbuilder.BuildPerScopePeerPod(run, jobbuilder.Defaults{DataPVC: "criteria-data"}, "scope-a", "ci", members, "10.0.0.10")
	require.NotNil(t, pod)

	// Exactly one pod per (scope, environment): a single container running
	// the criteria peer hosts every kind behind the multi-adapter manifest.
	require.Len(t, pod.Spec.Containers, 1)
	assert.Equal(t, jobbuilder.PerScopePeerContainerName, pod.Spec.Containers[0].Name)
	assert.Nil(t, pod.Spec.Containers[0].Command,
		"the peer container runs the image's ENTRYPOINT (`criteria peer`) with no command override")

	assert.Equal(t, "adapter", pod.Labels["criteria.brokenbots.dev/role"])
	assert.Equal(t, "scope-a", pod.Labels["criteria.brokenbots.dev/scope-id"])
	assert.Equal(t, "ci", pod.Labels["criteria.brokenbots.dev/environment"])
	assert.Equal(t, "copilot,shell", pod.Annotations[jobbuilder.AnnotationAdapterKinds],
		"the kinds annotation is the deduplicated, sorted set of hosted adapters")
	assert.Empty(t, pod.Labels["criteria.brokenbots.dev/adapter-kind"],
		"peer pods carry the kinds-set annotation, not the single-kind label")

	env := containerEnvMap(pod.Spec.Containers[0])
	assert.Equal(t, "copilot,shell", env["CRITERIA_REMOTE_ADAPTERS"],
		"the manifest env hosts every adapter kind sharing the environment")
	assert.Equal(t, "scope-a", env["CRITERIA_SCOPE_ID"])
	assert.Equal(t, "root-scope/scope-a", env["CRITERIA_REMOTE_SCOPE"])
	assert.Equal(t, "sha256:deadbeef", env["CRITERIA_ADAPTER_SHELL_DIGEST"],
		"per-kind digest pins keep the per-member digest env semantics")
	assert.Equal(t, "sha256:deadbeef", env["CRITERIA_ADAPTER_COPILOT_DIGEST"])
	assert.NotContains(t, env, "ADAPTER_KIND",
		"the peer's hosted identity comes from the manifest, not a single-kind env")
}

// CRI-234 R1 regression, re-hosted on the peer pod: the adapter-kinds set
// used to ride on a LABEL, whose comma-joined value ("copilot,shell") the
// API server rejects — a failure mode the fake.NewClientBuilder()
// controller tests cannot see, because they never apply API-server metadata
// validation. Validate the built pod's labels and annotations with the same
// helpers the API server uses, so this class of failure can no longer hide
// behind the fake client.
func TestBuildPerScopePeerPodMetadataIsValidForAPIServer(t *testing.T) {
	run := groupTestRun()
	pod := jobbuilder.BuildPerScopePeerPod(run, jobbuilder.Defaults{}, "scope-a", "ci",
		[]events.LifecycleEvent{
			groupMember("intake", "shell", "scope-a", "ci"),
			groupMember("review", "copilot", "scope-a", "ci"),
		}, "10.0.0.10")
	require.NotNil(t, pod)

	assert.Empty(t,
		metav1validation.ValidateLabels(pod.Labels, field.NewPath("metadata").Child("labels")),
		"peer pod labels must pass API-server label validation")
	assert.Empty(t,
		apivalidation.ValidateAnnotations(pod.Annotations, field.NewPath("metadata").Child("annotations")),
		"peer pod annotations must pass API-server annotation validation")

	// The comma-joined kind set is an annotation value now: absent from
	// labels (where a comma is illegal), present deduplicated and sorted on
	// the annotation.
	assert.NotContains(t, pod.Labels, "criteria.brokenbots.dev/adapter-kinds")
	assert.Equal(t, "copilot,shell", pod.Annotations[jobbuilder.AnnotationAdapterKinds])
}

func TestBuildPerScopePeerPodSeparateEnvironmentsSeparatePods(t *testing.T) {
	run := groupTestRun()

	ciPod := jobbuilder.BuildPerScopePeerPod(run, jobbuilder.Defaults{}, "scope-a", "ci",
		[]events.LifecycleEvent{groupMember("intake", "shell", "scope-a", "ci")}, "10.0.0.10")
	prodPod := jobbuilder.BuildPerScopePeerPod(run, jobbuilder.Defaults{}, "scope-a", "prod",
		[]events.LifecycleEvent{groupMember("review", "copilot", "scope-a", "prod")}, "10.0.0.10")

	require.NotNil(t, ciPod)
	require.NotNil(t, prodPod)
	assert.NotEqual(t, ciPod.Name, prodPod.Name,
		"adapters NOT sharing an environment must land in separate pods")
	assert.Len(t, ciPod.Spec.Containers, 1)
	assert.Len(t, prodPod.Spec.Containers, 1)
	// No pod ever mixes adapters from different environments: each builder
	// call hosts exactly the members of one (scope, environment) pair.
	assert.Equal(t, "ci", ciPod.Labels["criteria.brokenbots.dev/environment"])
	assert.Equal(t, "prod", prodPod.Labels["criteria.brokenbots.dev/environment"])
	assert.Equal(t, "shell", containerEnvMap(ciPod.Spec.Containers[0])["CRITERIA_REMOTE_ADAPTERS"])
	assert.Equal(t, "copilot", containerEnvMap(prodPod.Spec.Containers[0])["CRITERIA_REMOTE_ADAPTERS"])
}

func TestBuildPerScopePeerPodDeterministic(t *testing.T) {
	run := groupTestRun()
	forward := []events.LifecycleEvent{
		groupMember("intake", "shell", "scope-a", "ci"),
		groupMember("review", "copilot", "scope-a", "ci"),
		groupMember("audit", "copilot", "scope-a", "ci"),
	}
	reversed := []events.LifecycleEvent{forward[2], forward[1], forward[0]}

	first := jobbuilder.BuildPerScopePeerPod(run, jobbuilder.Defaults{}, "scope-a", "ci", forward, "10.0.0.10")
	second := jobbuilder.BuildPerScopePeerPod(run, jobbuilder.Defaults{}, "scope-a", "ci", reversed, "10.0.0.10")

	assert.Equal(t, first.Name, second.Name)
	require.Len(t, first.Spec.Containers, 1)
	assert.Equal(t, first.Spec.Containers[0].Env, second.Spec.Containers[0].Env,
		"member order must be normalized so repeated reconciles produce identical pod specs")
	assert.Equal(t, first.Spec.Containers[0].VolumeMounts, second.Spec.Containers[0].VolumeMounts)
}

func TestBuildPerScopePeerPodCarriesEnvironmentDeclarations(t *testing.T) {
	// The environment carries its volume/secret declarations via the run's
	// stamped workflow plan: declared volumes mount pod-wide, declared
	// secrets add the CSI volume and run the pod under the criteria-runner
	// service account for the OpenBao provider. The pod-adapter scripts
	// ConfigMap is NOT mounted: the peer container never passes through
	// adapter.sh (KB-214).
	run := groupTestRun()
	run.Spec.Workflow = &criteriav1.RunWorkflow{
		Name: "wf",
		Type: "url",
		Volumes: []criteriav1.RunWorkflowVolume{
			{Name: "declared", Kind: "pvc", Claim: "shared-claim", MountPath: "/mnt/declared"},
		},
		Secrets: []criteriav1.RunWorkflowSecret{
			{Name: "api-key", SecretProviderClass: "api-spc", MountPath: "/secrets/api-key", Env: map[string]string{"API_KEY_PATH": "token"}},
		},
	}

	pod := jobbuilder.BuildPerScopePeerPod(run, jobbuilder.Defaults{}, "scope-a", "ci",
		[]events.LifecycleEvent{groupMember("intake", "shell", "scope-a", "ci")}, "10.0.0.10")
	require.NotNil(t, pod)

	volumeNames := make(map[string]struct{}, len(pod.Spec.Volumes))
	for _, v := range pod.Spec.Volumes {
		volumeNames[v.Name] = struct{}{}
	}
	for _, want := range []string{"data", "declared", "secret-api-key"} {
		assert.Contains(t, volumeNames, want, "the environment's declared volumes/secrets must be applied pod-wide")
	}
	assert.NotContains(t, volumeNames, "scripts",
		"peer pods never mount the pod-adapter scripts ConfigMap")

	// The peer container mounts the declared volumes (pod-wide application).
	c := pod.Spec.Containers[0]
	mountNames := make(map[string]struct{}, len(c.VolumeMounts))
	for _, m := range c.VolumeMounts {
		mountNames[m.Name] = struct{}{}
	}
	assert.Contains(t, mountNames, "declared")
	assert.Contains(t, mountNames, "secret-api-key")
	assert.Equal(t, "/secrets/api-key/token", containerEnvMap(c)["API_KEY_PATH"],
		"the declared secret's env mapping anchors the rendered key path (never secret material)")

	assert.True(t, *pod.Spec.AutomountServiceAccountToken)
	assert.Equal(t, "criteria-runner", pod.Spec.ServiceAccountName)
}

func TestBuildPerScopePeerPodPodShapeMatchesFallback(t *testing.T) {
	// Peer pods share the fallback pod's zero-privilege shape: the same
	// node selector, toleration, restart policy, security context, and the
	// runner-never-a-co-tenant guarantee (only the peer container is ever
	// hosted here).
	run := groupTestRun()
	pod := jobbuilder.BuildPerScopePeerPod(run, jobbuilder.Defaults{}, "scope-a", "ci",
		[]events.LifecycleEvent{groupMember("intake", "shell", "scope-a", "ci")}, "10.0.0.10")
	require.NotNil(t, pod)

	assert.Equal(t, "amd64", pod.Spec.NodeSelector["kubernetes.io/arch"])
	require.Len(t, pod.Spec.Tolerations, 1)
	assert.Equal(t, "catch", pod.Spec.Tolerations[0].Key)
	assert.Equal(t, corev1.RestartPolicyOnFailure, pod.Spec.RestartPolicy)
	require.NotNil(t, pod.Spec.SecurityContext)
	assert.True(t, *pod.Spec.SecurityContext.RunAsNonRoot)
	assert.Equal(t, int64(10001), *pod.Spec.SecurityContext.RunAsUser)

	require.Len(t, pod.OwnerReferences, 1)
	assert.Equal(t, "CriteriaRun", pod.OwnerReferences[0].Kind)

	// The runner is never a co-tenant: exactly one container, the peer,
	// running the image's ENTRYPOINT with no command override.
	require.Len(t, pod.Spec.Containers, 1)
	assert.Nil(t, pod.Spec.Containers[0].Command)
	assert.Equal(t, "adapter", pod.Labels["criteria.brokenbots.dev/role"])
}

// KB-2: the peer pod's node arch is operator config, not a hard-coded
// amd64 literal; an unconfigured operator keeps the amd64 default.
func TestBuildPerScopePeerPodArchFollowsOperatorConfig(t *testing.T) {
	run := groupTestRun()

	pod := jobbuilder.BuildPerScopePeerPod(run, jobbuilder.Defaults{JobArch: "arm64"}, "scope-a", "ci",
		[]events.LifecycleEvent{groupMember("intake", "shell", "scope-a", "ci")}, "10.0.0.10")
	require.NotNil(t, pod)
	assert.Equal(t, "arm64", pod.Spec.NodeSelector["kubernetes.io/arch"],
		"the per-scope peer pod must stamp the operator-configured node arch")

	fallback := jobbuilder.BuildPerScopePeerPod(run, jobbuilder.Defaults{}, "scope-a", "ci",
		[]events.LifecycleEvent{groupMember("intake", "shell", "scope-a", "ci")}, "10.0.0.10")
	require.NotNil(t, fallback)
	assert.Equal(t, jobbuilder.JobArchDefault, fallback.Spec.NodeSelector["kubernetes.io/arch"],
		"an operator without a configured arch must keep the built-in default")
}

func TestPerScopePeerPodName(t *testing.T) {
	run := groupTestRun()

	name := jobbuilder.PerScopePeerPodName(run, "scope-a", "ci")
	assert.LessOrEqual(t, len(name), 63)
	assert.Regexp(t, `^[a-z0-9-]+$`, name)
	assert.Contains(t, name, "-peer-", "the peer name pins the job, scope hash, and environment hash")
	assert.NotContains(t, name, "-adp-",
		"the peer name family must stay outside the retired CRI-234 group-pod family so name-based eviction never touches it")

	// Deterministic and unique per (scope, environment).
	assert.Equal(t, name, jobbuilder.PerScopePeerPodName(run, "scope-a", "ci"))
	assert.NotEqual(t, name, jobbuilder.PerScopePeerPodName(run, "scope-a", "prod"))
	assert.NotEqual(t, name, jobbuilder.PerScopePeerPodName(run, "scope-b", "ci"))
}

func TestPerScopePeerPodNameLongInputsStayDNSSafe(t *testing.T) {
	run := groupTestRun()
	name := jobbuilder.PerScopePeerPodName(run,
		"very-long-scope-instance-id-with-uuid-9f1d3c2b-6a4e-4f8a-9c1d-3e7b5a2f0d46",
		"production-environment-with-a-rather-long-descriptive-identity")
	assert.LessOrEqual(t, len(name), 63)
	assert.Regexp(t, `^[a-z0-9-]+$`, name)
}

func TestBuildPerScopePeerPodEmptyMembersNil(t *testing.T) {
	run := groupTestRun()
	assert.Nil(t, jobbuilder.BuildPerScopePeerPod(run, jobbuilder.Defaults{}, "scope-a", "ci", nil, "10.0.0.10"))
}

// Dupes from engine retries collapse before the builder runs, but the
// manifest still dedups kinds defensively: two same-kind members yield the
// single kind once, and the digest pin keeps the first-seen member's value.
func TestBuildPerScopePeerPodSameKindMembersShareManifestKind(t *testing.T) {
	run := groupTestRun()
	first := groupMember("intake", "shell", "scope-a", "ci")
	second := groupMember("audit", "shell", "scope-a", "ci")
	second.Digest = "sha256:bbbbbbbb"

	pod := jobbuilder.BuildPerScopePeerPod(run, jobbuilder.Defaults{}, "scope-a", "ci",
		[]events.LifecycleEvent{first, second}, "10.0.0.10")
	require.NotNil(t, pod)
	assert.Equal(t, "shell", pod.Annotations[jobbuilder.AnnotationAdapterKinds])
	assert.Equal(t, "shell", containerEnvMap(pod.Spec.Containers[0])["CRITERIA_REMOTE_ADAPTERS"])
	assert.Equal(t, "sha256:bbbbbbbb", containerEnvMap(pod.Spec.Containers[0])["CRITERIA_ADAPTER_SHELL_DIGEST"],
		"the sorted-first member's digest wins the per-kind pin (same normalization as the dial member)")
}

// KB-214 accepted trade: the pod name and the manifest are functions of the
// (scope, environment) pair alone — a same-kind member swap or a digest
// change within the live scope's lifetime can never mutate the pod, and
// must not churn its name either. The reconcile holds the same object; the
// member re-provision rides the manifest kind it was provisioned with.
func TestBuildPerScopePeerPodNameUnchangedByMembershipDrift(t *testing.T) {
	run := groupTestRun()

	before := jobbuilder.BuildPerScopePeerPod(run, jobbuilder.Defaults{}, "scope-a", "ci",
		[]events.LifecycleEvent{groupMember("first", "shell", "scope-a", "ci")}, "10.0.0.10")
	after := jobbuilder.BuildPerScopePeerPod(run, jobbuilder.Defaults{}, "scope-a", "ci",
		[]events.LifecycleEvent{groupMember("second", "shell", "scope-a", "ci")}, "10.0.0.10")
	assert.Equal(t, before.Name, after.Name,
		"a same-kind member swap must NOT change the pod name: child cycling happens inside the container")

	// A digest change on re-provision is invisible to the pod name too: the
	// pod spec is pinned at creation and the reconcile never mutates it.
	reprovisioned := groupMember("first", "shell", "scope-a", "ci")
	reprovisioned.Digest = "sha256:bbbbbbbb"
	newer := jobbuilder.BuildPerScopePeerPod(run, jobbuilder.Defaults{}, "scope-a", "ci",
		[]events.LifecycleEvent{reprovisioned}, "10.0.0.10")
	assert.Equal(t, before.Name, newer.Name,
		"a digest change on re-provision must NOT churn the pod name")
}

// CRI-237: wire token delivery for peer pods. When every member carries an
// accept_token (runner commit eae0181, CRI-236) and the runner IP resolved,
// the peer pod is wire-shaped: the dial member's CRITERIA_REMOTE_TOKEN env,
// a routable CRITERIA_REMOTE_HOST, no CRITERIA_REMOTE_TOKEN_FILE anywhere,
// and no shared data volume.
func wireGroupMember(adapter, kind, scopeID, environment string) events.LifecycleEvent {
	member := groupMember(adapter, kind, scopeID, environment)
	member.ShimAddress = "[::]:7778"
	member.AcceptToken = "accept-" + kind
	return member
}

func TestBuildPerScopePeerPodWireDelivery(t *testing.T) {
	run := groupTestRun()
	members := []events.LifecycleEvent{
		wireGroupMember("intake", "shell", "scope-a", "ci"),
		wireGroupMember("review", "copilot", "scope-a", "ci"),
	}

	pod := jobbuilder.BuildPerScopePeerPod(run, jobbuilder.Defaults{DataPVC: "criteria-data"}, "scope-a", "ci", members, "10.0.0.10")
	require.NotNil(t, pod)

	require.Len(t, pod.Spec.Containers, 1)
	env := containerEnvMap(pod.Spec.Containers[0])
	// The dial member is the sorted-first member (adapter node name
	// ordering: "intake" < "review", so the shell member dials); the single
	// conn presents its accept token and the runner's routable port.
	assert.Equal(t, "accept-shell", env["CRITERIA_REMOTE_TOKEN"])
	assert.Equal(t, "10.0.0.10:7778", env["CRITERIA_REMOTE_HOST"])
	assert.NotContains(t, env, "CRITERIA_REMOTE_TOKEN_FILE")
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		assert.NotEqual(t, "data", m.Name)
	}
	assert.False(t, hasVolume(pod.Spec.Volumes, "data"),
		"the peer pod must not mount the shared /data PVC for wire delivery")
}

func TestBuildPerScopePeerPodMixedMembersKeepDataVolume(t *testing.T) {
	// Defensive: a pair holding one pre-eae0181 member (no accept_token —
	// impossible in practice, since one engine emits one event shape)
	// degrades the whole peer container to the legacy shape — the single
	// container must keep the shared data volume for the token file, and
	// never sends a half-shaped wire handshake.
	run := groupTestRun()
	members := []events.LifecycleEvent{
		wireGroupMember("intake", "shell", "scope-a", "ci"),
		groupMember("review", "copilot", "scope-a", "ci"),
	}

	pod := jobbuilder.BuildPerScopePeerPod(run, jobbuilder.Defaults{DataPVC: "criteria-data"}, "scope-a", "ci", members, "10.0.0.10")
	require.NotNil(t, pod)
	env := containerEnvMap(pod.Spec.Containers[0])
	assert.True(t, hasVolume(pod.Spec.Volumes, "data"))
	// The dial member is the sorted-first member ("intake", the wire-shaped
	// shell member): the whole container degrades to the legacy branch, so
	// it pins that member's token file and sends no wire handshake.
	assert.Equal(t, "/data/intake/CRI-234/tokens/intake", env["CRITERIA_REMOTE_TOKEN_FILE"])
	assert.NotContains(t, env, "CRITERIA_REMOTE_TOKEN")
}

func TestBuildPerScopePeerPodWireWithoutRunnerIPLegacy(t *testing.T) {
	// Defensive: accept_token members with an unresolved runner IP keep the
	// legacy shape (the reconcile never builds this state — it requeues).
	run := groupTestRun()
	members := []events.LifecycleEvent{
		wireGroupMember("intake", "shell", "scope-a", "ci"),
	}

	pod := jobbuilder.BuildPerScopePeerPod(run, jobbuilder.Defaults{DataPVC: "criteria-data"}, "scope-a", "ci", members, "")
	require.NotNil(t, pod)
	assert.True(t, hasVolume(pod.Spec.Volumes, "data"))
	env := containerEnvMap(pod.Spec.Containers[0])
	assert.Contains(t, env, "CRITERIA_REMOTE_TOKEN_FILE")
	assert.NotContains(t, env, "CRITERIA_REMOTE_TOKEN")
}

func TestBuildPerScopePeerPodNameStableAcrossTokenRotation(t *testing.T) {
	// CRI-237: the accept token rotates per provision event; a wire member's
	// re-provision must NOT churn the pod name. The peer container env
	// carries the rotated token on the next reconcile's desired spec, while
	// the reconcile holds the same pod object for the running scope.
	run := groupTestRun()

	before := wireGroupMember("intake", "shell", "scope-a", "ci")
	rotated := wireGroupMember("intake", "shell", "scope-a", "ci")
	rotated.AcceptToken = "accept-rotated-2"
	// Token rotation comes with a fresh token_ref too; the PATH stays stable.
	rotated.TokenFile = before.TokenFile

	first := jobbuilder.BuildPerScopePeerPod(run, jobbuilder.Defaults{}, "scope-a", "ci",
		[]events.LifecycleEvent{before}, "10.0.0.10")
	second := jobbuilder.BuildPerScopePeerPod(run, jobbuilder.Defaults{}, "scope-a", "ci",
		[]events.LifecycleEvent{rotated}, "10.0.0.10")
	require.Len(t, first.Spec.Containers, 1)
	assert.Equal(t, first.Name, second.Name,
		"a wire token rotation must not churn the peer pod name")

	// The wire env still carries the rotated token so the handshake works
	// on the next reconcile's desired spec.
	assert.Equal(t, "accept-shell", containerEnvMap(first.Spec.Containers[0])["CRITERIA_REMOTE_TOKEN"])
	assert.Equal(t, "accept-rotated-2", containerEnvMap(second.Spec.Containers[0])["CRITERIA_REMOTE_TOKEN"])
}
