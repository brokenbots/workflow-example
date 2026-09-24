package jobbuilder_test

import (
	"strings"
	"testing"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/events"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/jobbuilder"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPerScopeAdapterPodNameIsDNSSafeAndShort(t *testing.T) {
	run := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{Name: "cri-116"},
		Spec:       criteriav1.CriteriaRunSpec{TicketID: "CRI-116"},
	}
	name := jobbuilder.PerScopeAdapterPodName(run, "copilot", "subworkflow/name:uuid-1234")
	assert.LessOrEqual(t, len(name), 63)
	assert.Regexp(t, `^[a-z0-9-]+$`, name)
	assert.Contains(t, name, "copilot")
}

func TestBuildPerScopeAdapterPod(t *testing.T) {
	run := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{Name: "cri-116", Namespace: "criteria-jobs", UID: "run-uid"},
		Spec:       criteriav1.CriteriaRunSpec{TicketID: "CRI-116"},
	}
	scope := events.LifecycleEvent{
		Event:       events.EventProvisionWanted,
		RunID:       "CRI-116",
		ScopeID:     "root",
		ScopeTag:    "root-scope",
		AdapterName: "shell",
		Digest:      "deadbeef",
		ShimAddress: "10.0.0.5:7778",
		TokenFile:   "/data/intake/CRI-116/tokens/root-shell",
	}

	pod := jobbuilder.BuildPerScopeAdapterPod(run, jobbuilder.Defaults{DataPVC: "criteria-data"}, scope, "10.0.0.10")
	require.NotNil(t, pod)

	assert.Equal(t, run.Namespace, pod.Namespace)
	require.Len(t, pod.OwnerReferences, 1)
	assert.Equal(t, "CriteriaRun", pod.OwnerReferences[0].Kind)

	assert.Equal(t, "adapter", pod.Labels["criteria.brokenbots.dev/role"])
	assert.Equal(t, "shell", pod.Labels["criteria.brokenbots.dev/adapter-kind"])

	assert.Empty(t, pod.Spec.ServiceAccountName)
	require.NotNil(t, pod.Spec.AutomountServiceAccountToken)
	assert.False(t, *pod.Spec.AutomountServiceAccountToken)

	// No CSI volumes, no credential env vars.
	for _, v := range pod.Spec.Volumes {
		assert.Nil(t, v.CSI, "per-scope adapter volume %q must not be CSI", v.Name)
	}
	container := pod.Spec.Containers[0]
	envNames := make(map[string]string)
	for _, e := range container.Env {
		envNames[e.Name] = e.Value
	}
	assert.Equal(t, "sha256:deadbeef", envNames["CRITERIA_REMOTE_DIGEST"])
	assert.Equal(t, "shell", envNames["CRITERIA_ADAPTER_NAME"])
	assert.Equal(t, "root", envNames["CRITERIA_SCOPE_ID"])
	assert.Equal(t, "root-scope", envNames["CRITERIA_SCOPE_TAG"])
	assert.Equal(t, "/data/intake/CRI-116/tokens/root-shell", envNames["CRITERIA_REMOTE_TOKEN_FILE"])

	// CRITERIA_REMOTE_HOST (and the wire token) must be absent: this event
	// carries no accept_token, so it keeps the pre-eae0181 legacy shape even
	// though a runner IP is resolvable. The event's shim listen address is
	// the engine's own-loopback bind address, unreachable from a separate
	// pod; adapter.sh discovers the routable address (POD_IP:7778) from the
	// shared discovery file and the token from the rotated token file when
	// these envs are unset.
	_, hasHost := envNames["CRITERIA_REMOTE_HOST"]
	assert.False(t, hasHost, "CRITERIA_REMOTE_HOST must not be set from the event's loopback shim address")

	assert.ElementsMatch(t, []string{"data", "scripts"}, volumeMountNames(container.VolumeMounts))

	assert.Equal(t, "amd64", pod.Spec.NodeSelector["kubernetes.io/arch"])
	require.Len(t, pod.Spec.Tolerations, 1)
	assert.Equal(t, "catch", pod.Spec.Tolerations[0].Key)
}

// KB-2: the per-scope adapter pod's node arch is operator config, not a
// hard-coded amd64 literal; an unconfigured operator keeps the amd64 default.
func TestBuildPerScopeAdapterPodArchFollowsOperatorConfig(t *testing.T) {
	run := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{Name: "cri-arch", Namespace: "criteria-jobs", UID: "run-uid"},
		Spec:       criteriav1.CriteriaRunSpec{TicketID: "CRI-ARCH"},
	}
	scope := events.LifecycleEvent{
		Event:       events.EventProvisionWanted,
		RunID:       "CRI-ARCH",
		ScopeID:     "root",
		ScopeTag:    "root-scope",
		AdapterName: "shell",
		AdapterType: "shell",
		Digest:      "sha256:deadbeef",
	}

	pod := jobbuilder.BuildPerScopeAdapterPod(run, jobbuilder.Defaults{JobArch: "arm64"}, scope, "10.0.0.10")
	require.NotNil(t, pod)
	assert.Equal(t, "arm64", pod.Spec.NodeSelector["kubernetes.io/arch"],
		"the per-scope adapter pod must stamp the operator-configured node arch")

	fallback := jobbuilder.BuildPerScopeAdapterPod(run, jobbuilder.Defaults{}, scope, "10.0.0.10")
	require.NotNil(t, fallback)
	assert.Equal(t, jobbuilder.JobArchDefault, fallback.Spec.NodeSelector["kubernetes.io/arch"],
		"an operator without a configured arch must keep the built-in default")
}

func TestBuildPerScopeAdapterPodResolvesKindFromAdapterType(t *testing.T) {
	// CRI-140: the provision-wanted event's adapter field is the workflow's
	// adapter node name (the instance, "intake"), not the implementation
	// kind. Building the image reference from it produced
	// criteria-adapter-intake, which does not exist in the registry and
	// wedged the per-scope pod in ImagePull. The operator must resolve the
	// kind from the event's adapter_type instead.
	run := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{Name: "cri-140", Namespace: "criteria-jobs", UID: "run-uid"},
		Spec:       criteriav1.CriteriaRunSpec{TicketID: "CRI-140"},
	}
	scope := events.LifecycleEvent{
		Event:       events.EventProvisionWanted,
		RunID:       "CRI-140",
		ScopeID:     "root",
		AdapterName: "intake",
		AdapterType: "shell",
	}

	pod := jobbuilder.BuildPerScopeAdapterPod(run, jobbuilder.Defaults{}, scope, "10.0.0.10")
	require.NotNil(t, pod)

	container := pod.Spec.Containers[0]
	assert.Equal(t, "adapter-shell", container.Name)
	// CRI-214 M14: the event carries neither a digest nor an
	// image_reference, so the image resolves from the operator's configured
	// registry/tag defaults.
	assert.Equal(t, "localhost:5000/criteria-adapter-shell:k8s-3", container.Image,
		"the per-scope pod image must resolve from the configured registry/tag defaults for the adapter KIND")
	assert.Equal(t, "shell", pod.Labels["criteria.brokenbots.dev/adapter-kind"])

	envNames := make(map[string]string)
	for _, e := range container.Env {
		envNames[e.Name] = e.Value
	}
	assert.Equal(t, "shell", envNames["ADAPTER_KIND"])
	assert.Equal(t, "shell", envNames["CRITERIA_ADAPTER_NAME"],
		"the handshake presents the adapter TYPE; the lockfile digest verifier keys by type and has no instance-level entries")
}

func TestBuildPerScopeAdapterPodFallsBackToAdapterNameWithoutType(t *testing.T) {
	// Older engines do not emit adapter_type; the fallback keeps the
	// historical behavior of building the kind from the event's adapter
	// field.
	run := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{Name: "cri-140", Namespace: "criteria-jobs", UID: "run-uid"},
		Spec:       criteriav1.CriteriaRunSpec{TicketID: "CRI-140"},
	}
	scope := events.LifecycleEvent{
		Event:       events.EventProvisionWanted,
		RunID:       "CRI-140",
		ScopeID:     "root",
		AdapterName: "intake",
	}

	pod := jobbuilder.BuildPerScopeAdapterPod(run, jobbuilder.Defaults{}, scope, "10.0.0.10")
	require.NotNil(t, pod)
	assert.Equal(t, "intake", pod.Labels["criteria.brokenbots.dev/adapter-kind"])
	envNames := make(map[string]string)
	for _, e := range pod.Spec.Containers[0].Env {
		envNames[e.Name] = e.Value
	}
	assert.Equal(t, "intake", envNames["ADAPTER_KIND"])
}

func TestEngineProvisionWantedPayloadResolvesExistingImage(t *testing.T) {
	// CRI-140 contract check for the pinned engine: the workflow image builds
	// criteria >= v0.5.22 (linear_intake_v1/Dockerfile), which publishes the
	// provision-wanted envelope with adapter_type (the implementation kind)
	// in payload.data. The adapter node name ("intake") travels in "adapter".
	// Running the nested envelope through the parser and the pod builder must
	// yield an image that exists in the registry — never
	// criteria-adapter-<node-name>, which wedged per-scope pods in ImagePull.
	payload := `{"payload_type":"AdapterEvent","run_id":"CRI-140","payload":` +
		`{"kind":"adapter.lifecycle.provision_wanted","data":` +
		`{"adapter":"intake","adapter_type":"shell",` +
		`"digest":"sha256:d9f306c29f4145da8bcc44187c9e4ae0f69ed30db3b3edac6e9b6350469bc635",` +
		`"scope_instance_id":"root","shim_listen_address":"[::]:7778",` +
		`"token_ref":"/data/.criteria/runs/cri-140/token"}}}`

	evs, err := events.ParseLifecycleEventsBytes([]byte(payload))
	require.NoError(t, err)
	require.Len(t, evs, 1)
	require.Equal(t, "shell", evs[0].AdapterType,
		"the pinned engine's payload must carry adapter_type for the builder to resolve the kind")
	require.Equal(t, "intake", evs[0].AdapterName)

	run := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{Name: "cri-140", Namespace: "criteria-jobs", UID: "run-uid"},
		Spec:       criteriav1.CriteriaRunSpec{TicketID: "CRI-140"},
	}
	pod := jobbuilder.BuildPerScopeAdapterPod(run, jobbuilder.Defaults{}, evs[0], "10.0.0.10")
	require.NotNil(t, pod)

	container := pod.Spec.Containers[0]
	// CRI-214 M14: the pre-M14 emission carries a digest but no
	// image_reference, so the image resolves from the configured defaults.
	assert.Equal(t, "localhost:5000/criteria-adapter-shell:k8s-3", container.Image,
		"a shell/intake provision_wanted without image_reference must resolve to the configured shell adapter image, not criteria-adapter-intake")
	assert.NotContains(t, container.Image, "criteria-adapter-intake",
		"no code path may reference a criteria-adapter-intake image for this declaration")
	assert.Equal(t, "shell", pod.Labels["criteria.brokenbots.dev/adapter-kind"])

	envNames := make(map[string]string)
	for _, e := range container.Env {
		envNames[e.Name] = e.Value
	}
	assert.Equal(t, "shell", envNames["ADAPTER_KIND"])
	assert.Equal(t, "shell", envNames["CRITERIA_ADAPTER_NAME"],
		"the handshake presents the adapter TYPE for the type-keyed lockfile verifier")
}

func TestBuildPerScopeAdapterPodWireTokenDelivery(t *testing.T) {
	// CRI-237: an event carrying accept_token (runner commit eae0181,
	// CRI-236) gets its token on the wire. The pod sets CRITERIA_REMOTE_TOKEN
	// and a routable CRITERIA_REMOTE_HOST built from the resolved runner pod
	// IP plus the event's shim listen port, and mounts NO shared data
	// volume: no rotated token file is read anywhere.
	run := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{Name: "cri-237", Namespace: "criteria-jobs", UID: "run-uid"},
		Spec:       criteriav1.CriteriaRunSpec{TicketID: "CRI-237"},
	}
	scope := events.LifecycleEvent{
		Event:       events.EventProvisionWanted,
		RunID:       "CRI-237",
		ScopeID:     "root",
		ScopeTag:    "root-scope",
		AdapterName: "shell",
		Digest:      "deadbeef",
		ShimAddress: "[::]:7778",
		TokenFile:   "/data/intake/CRI-237/tokens/root-shell",
		AcceptToken: "accept-rotate-1",
	}

	pod := jobbuilder.BuildPerScopeAdapterPod(run, jobbuilder.Defaults{DataPVC: "criteria-data"}, scope, "10.0.0.10")
	require.NotNil(t, pod)

	container := pod.Spec.Containers[0]
	envNames := make(map[string]string)
	for _, e := range container.Env {
		envNames[e.Name] = e.Value
	}
	assert.Equal(t, "accept-rotate-1", envNames["CRITERIA_REMOTE_TOKEN"])
	assert.Equal(t, "10.0.0.10:7778", envNames["CRITERIA_REMOTE_HOST"],
		"the dial address is the runner pod IP plus the shim listen port; the bind host never routes")
	_, hasTokenFile := envNames["CRITERIA_REMOTE_TOKEN_FILE"]
	assert.False(t, hasTokenFile, "wire delivery must not carry the token-file path")

	assert.ElementsMatch(t, []string{"scripts"}, volumeMountNames(container.VolumeMounts))
	assert.False(t, hasVolume(pod.Spec.Volumes, "data"),
		"the adapter pod must not mount the shared /data PVC for wire delivery")
}

func TestBuildPerScopeAdapterPodWireDegradesToLegacyWithoutRunnerIP(t *testing.T) {
	// Defensive: an accept_token event with an unresolved runner IP must not
	// produce a half-wired pod that dials nothing and mounts nothing. The
	// builder falls back to the legacy token-file shape (the reconcile never
	// builds in this state — it requeues instead).
	run := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{Name: "cri-237", Namespace: "criteria-jobs", UID: "run-uid"},
		Spec:       criteriav1.CriteriaRunSpec{TicketID: "CRI-237"},
	}
	scope := events.LifecycleEvent{
		Event:       events.EventProvisionWanted,
		RunID:       "CRI-237",
		ScopeID:     "root",
		AdapterName: "shell",
		Digest:      "deadbeef",
		ShimAddress: "[::]:7778",
		TokenFile:   "/data/intake/CRI-237/tokens/root-shell",
		AcceptToken: "accept-rotate-1",
	}

	pod := jobbuilder.BuildPerScopeAdapterPod(run, jobbuilder.Defaults{DataPVC: "criteria-data"}, scope, "")
	require.NotNil(t, pod)

	envNames := make(map[string]string)
	for _, e := range pod.Spec.Containers[0].Env {
		envNames[e.Name] = e.Value
	}
	assert.Equal(t, "/data/intake/CRI-237/tokens/root-shell", envNames["CRITERIA_REMOTE_TOKEN_FILE"])
	_, hasHost := envNames["CRITERIA_REMOTE_HOST"]
	assert.False(t, hasHost)
	_, hasToken := envNames["CRITERIA_REMOTE_TOKEN"]
	assert.False(t, hasToken)
	assert.ElementsMatch(t, []string{"data", "scripts"}, volumeMountNames(pod.Spec.Containers[0].VolumeMounts))
}

func TestBuildPerScopeAdapterPodWireKeepsDeclaredDataMount(t *testing.T) {
	// The wire path drops the built-in data volume, but a workflow that
	// explicitly re-sources /data keeps its declared mount (it is workload
	// state, not token delivery).
	run := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{Name: "cri-237", Namespace: "criteria-jobs", UID: "run-uid"},
		Spec: criteriav1.CriteriaRunSpec{
			TicketID: "CRI-237",
			Workflow: &criteriav1.RunWorkflow{
				Volumes: []criteriav1.RunWorkflowVolume{{
					Name:      "run-state",
					Kind:      "pvc",
					MountPath: "/data",
					Claim:     "criteria-data",
				}},
			},
		},
	}
	scope := events.LifecycleEvent{
		Event:       events.EventProvisionWanted,
		RunID:       "CRI-237",
		ScopeID:     "root",
		AdapterName: "shell",
		Digest:      "deadbeef",
		ShimAddress: "[::]:7778",
		AcceptToken: "accept-rotate-1",
	}

	pod := jobbuilder.BuildPerScopeAdapterPod(run, jobbuilder.Defaults{DataPVC: "criteria-data"}, scope, "10.0.0.10")
	require.NotNil(t, pod)
	container := pod.Spec.Containers[0]
	assert.True(t, hasVolume(pod.Spec.Volumes, "data"),
		"a declared /data volume is workload state and survives the wire path")
	assert.Contains(t, volumeMountNames(container.VolumeMounts), "data")
}

func TestEngineProvisionWantedPayloadCarriesWireToken(t *testing.T) {
	// End-to-end parser -> builder contract for the eae0181 engine (CRI-236):
	// the nested provision_wanted envelope carries accept_token in
	// payload.data; parsing yields the field and the builder renders the
	// wire shape.
	payload := `{"payload_type":"AdapterEvent","run_id":"CRI-237","payload":` +
		`{"kind":"adapter.lifecycle.provision_wanted","data":` +
		`{"adapter":"intake","adapter_type":"shell",` +
		`"digest":"sha256:d9f306c29f4145da8bcc44187c9e4ae0f69ed30db3b3edac6e9b6350469bc635",` +
		`"scope_instance_id":"root","shim_listen_address":"[::]:7778",` +
		`"token_ref":"/data/.criteria/runs/cri-237/token",` +
		`"accept_token":"accept-rotate-1"}}}`

	evs, err := events.ParseLifecycleEventsBytes([]byte(payload))
	require.NoError(t, err)
	require.Len(t, evs, 1)
	require.Equal(t, "accept-rotate-1", evs[0].AcceptToken)

	run := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{Name: "cri-237", Namespace: "criteria-jobs", UID: "run-uid"},
		Spec:       criteriav1.CriteriaRunSpec{TicketID: "CRI-237"},
	}
	pod := jobbuilder.BuildPerScopeAdapterPod(run, jobbuilder.Defaults{}, evs[0], "10.0.0.10")
	require.NotNil(t, pod)

	envNames := make(map[string]string)
	for _, e := range pod.Spec.Containers[0].Env {
		envNames[e.Name] = e.Value
	}
	assert.Equal(t, "accept-rotate-1", envNames["CRITERIA_REMOTE_TOKEN"])
	assert.Equal(t, "10.0.0.10:7778", envNames["CRITERIA_REMOTE_HOST"])
	_, hasTokenFile := envNames["CRITERIA_REMOTE_TOKEN_FILE"]
	assert.False(t, hasTokenFile, "token_ref is still populated by the engine but must not be consumed for wire delivery")
	assert.ElementsMatch(t, []string{"scripts"}, volumeMountNames(pod.Spec.Containers[0].VolumeMounts))
}

func hasVolume(volumes []corev1.Volume, name string) bool {
	for _, v := range volumes {
		if v.Name == name {
			return true
		}
	}
	return false
}

func TestBuildAllPerScopeSessionsOmitsAdapterJobs(t *testing.T) {
	run := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{Name: "cri-116"},
		Spec: criteriav1.CriteriaRunSpec{
			TicketID:         "CRI-116",
			RepoURL:          "https://github.com/brokenbots/workflow-example.git",
			PerScopeSessions: true,
		},
	}

	jobs := jobbuilder.BuildAll(run, jobbuilder.Defaults{DataPVC: "criteria-data"})
	require.Len(t, jobs, 1)
	assert.Equal(t, "cri-116", jobs[0].Name)
	assert.Equal(t, "runner", jobs[0].Spec.Template.Labels["criteria.brokenbots.dev/role"])
}

func TestBuildAllWithoutPerScopeSessionsKeepsAdapterJobs(t *testing.T) {
	run := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{Name: "cri-116"},
		Spec: criteriav1.CriteriaRunSpec{
			TicketID: "CRI-116",
			RepoURL:  "https://github.com/brokenbots/workflow-example.git",
		},
	}

	jobs := jobbuilder.BuildAll(run, jobbuilder.Defaults{DataPVC: "criteria-data"})
	require.Len(t, jobs, 3)
}

func TestPerScopeDigestPrefixAdded(t *testing.T) {
	run := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{Name: "cri-116", Namespace: "default"},
		Spec:       criteriav1.CriteriaRunSpec{TicketID: "CRI-116"},
	}
	scope := events.LifecycleEvent{Event: events.EventProvisionWanted, AdapterName: "shell", ScopeID: "root", Digest: "deadbeef"}
	pod := jobbuilder.BuildPerScopeAdapterPod(run, jobbuilder.Defaults{}, scope, "10.0.0.10")
	env := findEnv(t, pod.Spec.Containers[0].Env, "CRITERIA_REMOTE_DIGEST")
	assert.Equal(t, "sha256:deadbeef", env.Value)
}

func TestPerScopeDigestPrefixPreserved(t *testing.T) {
	run := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{Name: "cri-116", Namespace: "default"},
		Spec:       criteriav1.CriteriaRunSpec{TicketID: "CRI-116"},
	}
	scope := events.LifecycleEvent{Event: events.EventProvisionWanted, AdapterName: "shell", ScopeID: "root", Digest: "sha256:deadbeef"}
	pod := jobbuilder.BuildPerScopeAdapterPod(run, jobbuilder.Defaults{}, scope, "10.0.0.10")
	env := findEnv(t, pod.Spec.Containers[0].Env, "CRITERIA_REMOTE_DIGEST")
	assert.Equal(t, "sha256:deadbeef", env.Value)
}

func volumeMountNames(mounts []corev1.VolumeMount) []string {
	out := make([]string, len(mounts))
	for i, m := range mounts {
		out[i] = m.Name
	}
	return out
}

func findEnv(t *testing.T, envs []corev1.EnvVar, name string) corev1.EnvVar {
	t.Helper()
	for _, e := range envs {
		if e.Name == name {
			return e
		}
	}
	t.Fatalf("env var %q not found", name)
	return corev1.EnvVar{}
}

func TestPerScopeAdapterPodNameAvoidsCollisions(t *testing.T) {
	run := &criteriav1.CriteriaRun{ObjectMeta: metav1.ObjectMeta{Name: "cri-116"}, Spec: criteriav1.CriteriaRunSpec{TicketID: "CRI-116"}}
	names := make(map[string]bool)
	for i := 0; i < 100; i++ {
		name := jobbuilder.PerScopeAdapterPodName(run, "shell", "scope-"+strings.Repeat("x", i))
		require.LessOrEqual(t, len(name), 63)
		assert.False(t, names[name], "duplicate pod name %q", name)
		names[name] = true
	}
}
