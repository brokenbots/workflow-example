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

// CRI-234 M7.2: adapters sharing one (scope, environment) pair are
// co-located as separate containers in ONE pod; the environment carries its
// volume/secret declarations; the runner is never a co-tenant; env-less
// events keep the per-adapter fallback shape.

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

func TestBuildPerScopeAdapterPodGroupSameEnvironmentOnePod(t *testing.T) {
	run := groupTestRun()
	members := []events.LifecycleEvent{
		groupMember("intake", "shell", "scope-a", "ci"),
		groupMember("review", "copilot", "scope-a", "ci"),
	}

	pod := jobbuilder.BuildPerScopeAdapterPodGroup(run, jobbuilder.Defaults{DataPVC: "criteria-data"}, "scope-a", "ci", members)
	require.NotNil(t, pod)

	// Exactly one pod per (scope, environment): every member is a separate
	// container of the single pod, with a member-sensitive name embedding
	// the member's own handshake binding.
	require.Len(t, pod.Spec.Containers, 2)
	for _, c := range pod.Spec.Containers {
		assert.Regexp(t, `^adapter-(shell|copilot)-[0-9a-f]{12}$`, c.Name,
			"container names are member-sensitive: kind + hash of the member binding")
	}

	assert.Equal(t, "adapter", pod.Labels["criteria.brokenbots.dev/role"])
	assert.Equal(t, "scope-a", pod.Labels["criteria.brokenbots.dev/scope-id"])
	assert.Equal(t, "ci", pod.Labels["criteria.brokenbots.dev/environment"])
	assert.Equal(t, "copilot,shell", pod.Annotations[jobbuilder.AnnotationAdapterKinds],
		"the kinds annotation is the deduplicated, sorted set of hosted adapters")
	assert.Empty(t, pod.Labels["criteria.brokenbots.dev/adapter-kind"],
		"group pods carry the kinds-set annotation, not the single-kind label")

	// Per-member handshake env is built from each member's own event.
	byKind := map[string]corev1.Container{}
	for _, c := range pod.Spec.Containers {
		byKind[containerEnvMap(c)["ADAPTER_KIND"]] = c
	}
	shellEnv := containerEnvMap(byKind["shell"])
	assert.Equal(t, "shell", shellEnv["ADAPTER_KIND"])
	assert.Equal(t, "shell", shellEnv["CRITERIA_ADAPTER_NAME"])
	assert.Equal(t, "scope-a", shellEnv["CRITERIA_SCOPE_ID"])
	assert.Equal(t, "/data/intake/CRI-234/tokens/intake", shellEnv["CRITERIA_REMOTE_TOKEN_FILE"])
	copilotEnv := containerEnvMap(byKind["copilot"])
	assert.Equal(t, "copilot", copilotEnv["ADAPTER_KIND"])
	assert.Equal(t, "/data/intake/CRI-234/tokens/review", copilotEnv["CRITERIA_REMOTE_TOKEN_FILE"])
}

// CRI-234 R1 regression: the group pod's adapter-kinds set used to ride on
// a LABEL, whose comma-joined value ("copilot,shell") the API server
// rejects — a failure mode the fake.NewClientBuilder() controller tests
// cannot see, because they never apply API-server metadata validation.
// Validate the built pod's labels and annotations with the same helpers the
// API server uses, so this class of failure can no longer hide behind the
// fake client.
func TestBuildPerScopeAdapterPodGroupMetadataIsValidForAPIServer(t *testing.T) {
	run := groupTestRun()
	pod := jobbuilder.BuildPerScopeAdapterPodGroup(run, jobbuilder.Defaults{}, "scope-a", "ci",
		[]events.LifecycleEvent{
			groupMember("intake", "shell", "scope-a", "ci"),
			groupMember("review", "copilot", "scope-a", "ci"),
		})
	require.NotNil(t, pod)

	assert.Empty(t,
		metav1validation.ValidateLabels(pod.Labels, field.NewPath("metadata").Child("labels")),
		"group pod labels must pass API-server label validation")
	assert.Empty(t,
		apivalidation.ValidateAnnotations(pod.Annotations, field.NewPath("metadata").Child("annotations")),
		"group pod annotations must pass API-server annotation validation")

	// The comma-joined kind set is an annotation value now: absent from
	// labels (where a comma is illegal), present deduplicated and sorted on
	// the annotation.
	assert.NotContains(t, pod.Labels, "criteria.brokenbots.dev/adapter-kinds")
	assert.Equal(t, "copilot,shell", pod.Annotations[jobbuilder.AnnotationAdapterKinds])
}

func TestBuildPerScopeAdapterPodGroupSeparateEnvironmentsSeparatePods(t *testing.T) {
	run := groupTestRun()

	ciPod := jobbuilder.BuildPerScopeAdapterPodGroup(run, jobbuilder.Defaults{}, "scope-a", "ci",
		[]events.LifecycleEvent{groupMember("intake", "shell", "scope-a", "ci")})
	prodPod := jobbuilder.BuildPerScopeAdapterPodGroup(run, jobbuilder.Defaults{}, "scope-a", "prod",
		[]events.LifecycleEvent{groupMember("review", "copilot", "scope-a", "prod")})

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
}

func TestBuildPerScopeAdapterPodGroupDeterministic(t *testing.T) {
	run := groupTestRun()
	forward := []events.LifecycleEvent{
		groupMember("intake", "shell", "scope-a", "ci"),
		groupMember("review", "copilot", "scope-a", "ci"),
		groupMember("audit", "copilot", "scope-a", "ci"),
	}
	reversed := []events.LifecycleEvent{forward[2], forward[1], forward[0]}

	first := jobbuilder.BuildPerScopeAdapterPodGroup(run, jobbuilder.Defaults{}, "scope-a", "ci", forward)
	second := jobbuilder.BuildPerScopeAdapterPodGroup(run, jobbuilder.Defaults{}, "scope-a", "ci", reversed)

	assert.Equal(t, first.Name, second.Name)
	require.Len(t, first.Spec.Containers, len(second.Spec.Containers))
	for i := range first.Spec.Containers {
		assert.Equal(t, first.Spec.Containers[i].Name, second.Spec.Containers[i].Name,
			"member order must be normalized so repeated reconciles produce identical pod specs")
		assert.Equal(t, first.Spec.Containers[i].Env, second.Spec.Containers[i].Env)
	}
}

func TestBuildPerScopeAdapterPodGroupCarriesEnvironmentDeclarations(t *testing.T) {
	// The environment carries its volume/secret declarations via the run's
	// stamped workflow plan: declared volumes mount pod-wide, declared
	// secrets add the CSI volume and run the pod under the criteria-runner
	// service account for the OpenBao provider.
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

	pod := jobbuilder.BuildPerScopeAdapterPodGroup(run, jobbuilder.Defaults{}, "scope-a", "ci",
		[]events.LifecycleEvent{groupMember("intake", "shell", "scope-a", "ci")})
	require.NotNil(t, pod)

	volumeNames := make(map[string]struct{}, len(pod.Spec.Volumes))
	for _, v := range pod.Spec.Volumes {
		volumeNames[v.Name] = struct{}{}
	}
	for _, want := range []string{"data", "scripts", "declared", "secret-api-key"} {
		assert.Contains(t, volumeNames, want, "the environment's declared volumes/secrets must be applied pod-wide")
	}

	// Every container mounts the declared volumes (pod-wide application).
	for _, c := range pod.Spec.Containers {
		mountNames := make(map[string]struct{}, len(c.VolumeMounts))
		for _, m := range c.VolumeMounts {
			mountNames[m.Name] = struct{}{}
		}
		assert.Contains(t, mountNames, "declared")
		assert.Contains(t, mountNames, "secret-api-key")
		envMap := containerEnvMap(c)
		assert.Equal(t, "/secrets/api-key/token", envMap["API_KEY_PATH"],
			"the declared secret's env mapping anchors the rendered key path (never secret material)")
	}

	assert.True(t, *pod.Spec.AutomountServiceAccountToken)
	assert.Equal(t, "criteria-runner", pod.Spec.ServiceAccountName)
}

func TestBuildPerScopeAdapterPodGroupContainerNameCollision(t *testing.T) {
	// Engine retries can deliver duplicate provision_wanted events;
	// ActiveProvisions collapses them so distinct members always hash apart,
	// but if two members still resolve to one base name (hash-prefix
	// collision defense) the -N suffix keeps the pod spec valid.
	run := groupTestRun()
	member := groupMember("intake", "shell", "scope-a", "ci")

	pod := jobbuilder.BuildPerScopeAdapterPodGroup(run, jobbuilder.Defaults{}, "scope-a", "ci",
		[]events.LifecycleEvent{member, member})
	require.NotNil(t, pod)
	require.Len(t, pod.Spec.Containers, 2)
	assert.NotEqual(t, pod.Spec.Containers[0].Name, pod.Spec.Containers[1].Name,
		"container names must stay unique even for identical-binding members")
	assert.Regexp(t, `^adapter-shell-[0-9a-f]{12}(-2)?$`, pod.Spec.Containers[0].Name)
	assert.Regexp(t, `^adapter-shell-[0-9a-f]{12}(-2)?$`, pod.Spec.Containers[1].Name)
}

// Review R1 (CRI-234): container names are member-sensitive, so a
// count-preserving, same-kind membership change in one (scope, environment)
// alters the desired container name set and the reconcile recreates the
// pod — the failure mode a kind-only name derivation cannot detect.
func TestBuildPerScopeAdapterPodGroupNameSetTracksMembership(t *testing.T) {
	run := groupTestRun()

	before := jobbuilder.BuildPerScopeAdapterPodGroup(run, jobbuilder.Defaults{}, "scope-a", "ci",
		[]events.LifecycleEvent{groupMember("first", "shell", "scope-a", "ci")})
	after := jobbuilder.BuildPerScopeAdapterPodGroup(run, jobbuilder.Defaults{}, "scope-a", "ci",
		[]events.LifecycleEvent{groupMember("second", "shell", "scope-a", "ci")})
	require.Len(t, after.Spec.Containers, 1)
	assert.NotEqual(t, before.Spec.Containers[0].Name, after.Spec.Containers[0].Name,
		"a same-kind member swap must be detectable by the container name set")

	// A re-provision of the same member with a new digest is a binding
	// change too: the name must follow so the stale-digest container cannot
	// linger.
	reprovisioned := groupMember("first", "shell", "scope-a", "ci")
	reprovisioned.Digest = "sha256:bbbbbbbb"
	newer := jobbuilder.BuildPerScopeAdapterPodGroup(run, jobbuilder.Defaults{}, "scope-a", "ci",
		[]events.LifecycleEvent{reprovisioned})
	require.Len(t, newer.Spec.Containers, 1)
	assert.NotEqual(t, before.Spec.Containers[0].Name, newer.Spec.Containers[0].Name,
		"a digest change on re-provision must be detectable by the container name set")
}

func TestBuildPerScopeAdapterPodGroupPodShapeMatchesFallback(t *testing.T) {
	// Group pods share the fallback pod's zero-privilege shape: the same
	// node selector, toleration, restart policy, security context, and the
	// runner-never-a-co-tenant guarantee (only adapter containers, built
	// from provision events, are ever hosted here).
	run := groupTestRun()
	pod := jobbuilder.BuildPerScopeAdapterPodGroup(run, jobbuilder.Defaults{}, "scope-a", "ci",
		[]events.LifecycleEvent{groupMember("intake", "shell", "scope-a", "ci")})
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

	// The runner is never a co-tenant: every container is an adapter
	// container running the pod-adapter entrypoint.
	for _, c := range pod.Spec.Containers {
		assert.Equal(t, []string{"/opt/criteria-pod-adapter/adapter.sh"}, c.Command)
	}
	assert.Equal(t, "adapter", pod.Labels["criteria.brokenbots.dev/role"])
}

func TestPerScopeAdapterGroupName(t *testing.T) {
	run := groupTestRun()

	name := jobbuilder.PerScopeAdapterGroupName(run, "scope-a", "ci")
	assert.LessOrEqual(t, len(name), 63)
	assert.Regexp(t, `^[a-z0-9-]+$`, name)
	assert.Contains(t, name, "-adp-", "the group name pins the job, scope hash, and environment hash")

	// Deterministic and unique per (scope, environment).
	assert.Equal(t, name, jobbuilder.PerScopeAdapterGroupName(run, "scope-a", "ci"))
	assert.NotEqual(t, name, jobbuilder.PerScopeAdapterGroupName(run, "scope-a", "prod"))
	assert.NotEqual(t, name, jobbuilder.PerScopeAdapterGroupName(run, "scope-b", "ci"))
}

func TestPerScopeAdapterGroupNameLongInputsStayDNSSafe(t *testing.T) {
	run := groupTestRun()
	name := jobbuilder.PerScopeAdapterGroupName(run,
		"very-long-scope-instance-id-with-uuid-9f1d3c2b-6a4e-4f8a-9c1d-3e7b5a2f0d46",
		"production-environment-with-a-rather-long-descriptive-identity")
	assert.LessOrEqual(t, len(name), 63)
	assert.Regexp(t, `^[a-z0-9-]+$`, name)
}

func TestBuildPerScopeAdapterPodGroupEmptyMembersNil(t *testing.T) {
	run := groupTestRun()
	assert.Nil(t, jobbuilder.BuildPerScopeAdapterPodGroup(run, jobbuilder.Defaults{}, "scope-a", "ci", nil))
}
