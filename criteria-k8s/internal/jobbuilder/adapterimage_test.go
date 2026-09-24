package jobbuilder

import (
	"testing"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// adapterImageDefaults returns a Defaults carrying non-builtin adapter
// registry/tag so tests can tell configured values from the built-in
// fallbacks.
func adapterImageDefaults() Defaults {
	return Defaults{
		AdapterRegistry: "registry.example:5000",
		AdapterTag:      "k8s-9",
	}
}

// CRI-214 M14: the resolution order is workflow-object override → event
// image_reference (digest-verified) → configured registry/tag defaults.
// These tests pin each branch and the interactions between them.
func TestResolveAdapterImageOverrideWins(t *testing.T) {
	plan := &workflowPlan{adapterImages: map[string]string{"shell": "localhost:5000/criteria-adapter-shell:k8s-0.5.4-2"}}
	scope := events.LifecycleEvent{
		Event:          events.EventProvisionWanted,
		Digest:         "sha256:d9f306c29f4145da8bcc44187c9e4ae0f69ed30db3b3edac6e9b6350469bc635",
		ImageReference: "localhost:5000/criteria-adapter-shell:k8s-5",
	}

	img := resolveAdapterImage(plan, scope, "shell", adapterImageDefaults())
	assert.Equal(t, "localhost:5000/criteria-adapter-shell:k8s-0.5.4-2", img,
		"the workflow object's adapterImages override wins over both the event's image_reference and the defaults")

	// The override is per-kind: copilot has no override entry, so a zero
	// event (no digest, no reference) falls through to the configured
	// defaults.
	assert.Equal(t, "registry.example:5000/criteria-adapter-copilot:k8s-9", resolveAdapterImage(plan, events.LifecycleEvent{}, "copilot", adapterImageDefaults()))
}

func TestResolveAdapterImageUsesDigestVerifiedImageReference(t *testing.T) {
	// M14 engine shape: the lockfile-pinned reference travels on the wire
	// with the digest the engine-side verifier enforces.
	scope := events.LifecycleEvent{
		Event:          events.EventProvisionWanted,
		Digest:         "sha256:d9f306c29f4145da8bcc44187c9e4ae0f69ed30db3b3edac6e9b6350469bc635",
		ImageReference: "registry.example:5000/criteria-adapter-shell@sha256:abc",
	}

	img := resolveAdapterImage(nil, scope, "shell", adapterImageDefaults())
	assert.Equal(t, "registry.example:5000/criteria-adapter-shell@sha256:abc", img,
		"a digest-verified image_reference displaces the configured defaults")
}

func TestResolveAdapterImageWithoutDigestIgnoresImageReference(t *testing.T) {
	// Pre-M14 engines emit no image_reference; a reference without a
	// digest is unverified and must not displace the configured defaults.
	scope := events.LifecycleEvent{
		Event:          events.EventProvisionWanted,
		ImageReference: "evil-registry.example/criteria-adapter-shell:latest",
	}

	img := resolveAdapterImage(nil, scope, "shell", adapterImageDefaults())
	assert.Equal(t, "registry.example:5000/criteria-adapter-shell:k8s-9", img,
		"an image_reference without a digest is unverified and falls through to the defaults")

	// The zero event (no digest, no reference) resolves from the defaults.
	assert.Equal(t, "registry.example:5000/criteria-adapter-shell:k8s-9", resolveAdapterImage(nil, events.LifecycleEvent{}, "shell", adapterImageDefaults()))
}

func TestResolveAdapterImageDefaultsFromConfiguredRegistryAndTag(t *testing.T) {
	// Legacy adapter Jobs have no lifecycle event at all.
	img := resolveAdapterImage(nil, events.LifecycleEvent{}, "shell", adapterImageDefaults())
	assert.Equal(t, "registry.example:5000/criteria-adapter-shell:k8s-9", img)

	// Zero Defaults fall back to the built-in registry and tag.
	assert.Equal(t, "localhost:5000/criteria-adapter-copilot:k8s-3", resolveAdapterImage(nil, events.LifecycleEvent{}, "copilot", Defaults{}))

	// Each configured component can be overridden independently.
	registryOnly := resolveAdapterImage(nil, events.LifecycleEvent{}, "shell", Defaults{AdapterRegistry: "reg2.example:443"})
	assert.Equal(t, "reg2.example:443/criteria-adapter-shell:k8s-3", registryOnly)
	tagOnly := resolveAdapterImage(nil, events.LifecycleEvent{}, "shell", Defaults{AdapterTag: "k8s-9"})
	assert.Equal(t, "localhost:5000/criteria-adapter-shell:k8s-9", tagOnly)
}

// CRI-214 M14: BuildPerScopeAdapterPod consumes the whole resolution order,
// not just the defaults branch — the workflow object's override wins and an
// unverified reference must not leak into the pod.
func TestBuildPerScopeAdapterPodResolvesImageReference(t *testing.T) {
	digest := "sha256:d9f306c29f4145da8bcc44187c9e4ae0f69ed30db3b3edac6e9b6350469bc635"

	t.Run("digest-verified reference wins over defaults", func(t *testing.T) {
		run := perScopeAdapterTestRun()
		scope := events.LifecycleEvent{
			Event:          events.EventProvisionWanted,
			RunID:          "CRI-42",
			ScopeID:        "root",
			AdapterName:    "intake",
			AdapterType:    "shell",
			Digest:         digest,
			ImageReference: "registry.example:5000/criteria-adapter-shell:k8s-5",
		}

		pod := jobbuilderBuildPerScopeAdapterPod(t, run, Defaults{}, scope)
		assert.Equal(t, "registry.example:5000/criteria-adapter-shell:k8s-5", pod.Spec.Containers[0].Image)
	})

	t.Run("workflow override wins over verified reference", func(t *testing.T) {
		run := perScopeAdapterTestRun()
		run.Spec.Workflow = &criteriav1.RunWorkflow{AdapterImages: map[string]string{"shell": "localhost:5000/criteria-adapter-shell:k8s-0.5.4-2"}}
		scope := events.LifecycleEvent{
			Event:          events.EventProvisionWanted,
			RunID:          "CRI-42",
			ScopeID:        "root",
			AdapterName:    "intake",
			AdapterType:    "shell",
			Digest:         digest,
			ImageReference: "registry.example:5000/criteria-adapter-shell:k8s-5",
		}

		pod := jobbuilderBuildPerScopeAdapterPod(t, run, Defaults{}, scope)
		assert.Equal(t, "localhost:5000/criteria-adapter-shell:k8s-0.5.4-2", pod.Spec.Containers[0].Image)
	})
}

func perScopeAdapterTestRun() *criteriav1.CriteriaRun {
	return &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{Name: "cri-42", Namespace: "criteria-jobs", UID: "run-uid"},
		Spec:       criteriav1.CriteriaRunSpec{TicketID: "CRI-42"},
	}
}

func jobbuilderBuildPerScopeAdapterPod(t *testing.T, run *criteriav1.CriteriaRun, defaults Defaults, scope events.LifecycleEvent) *corev1.Pod {
	t.Helper()
	pod := BuildPerScopeAdapterPod(run, defaults, scope, "10.0.0.10")
	require.NotNil(t, pod)
	return pod
}
