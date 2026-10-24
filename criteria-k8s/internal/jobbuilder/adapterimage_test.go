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

// CRI-214 M14, KB-306 flip, KB-319 re-flip: the resolution order is the
// KB-319 chain — pinned metadata (spec.workflow.adapterImages) →
// digest-verified event image_reference → unpinned metadata → configured
// registry/tag defaults. These tests pin each branch and the interactions
// between them.
func TestResolveAdapterImageMetadataLaneLeads(t *testing.T) {
	// KB-319: the pinned metadata lane is the version authority — it
	// displaces a digest-verified event and the configured default.
	plan := &workflowPlan{adapterImages: map[string]string{"shell": "localhost:5000/criteria-adapter-shell:k8s-0.5.4-2"}}
	scope := events.LifecycleEvent{
		Event:          events.EventProvisionWanted,
		Digest:         "sha256:d9f306c29f4145da8bcc44187c9e4ae0f69ed30db3b3edac6e9b6350469bc635",
		ImageReference: "registry.example:5000/criteria-adapter-shell@sha256:abc",
	}

	img := resolveAdapterImage(plan, scope, "shell", adapterImageDefaults())
	assert.Equal(t, "localhost:5000/criteria-adapter-shell:k8s-0.5.4-2", img,
		"the pinned metadata lane outranks a digest-verified event")
}

func TestResolveAdapterImageUnpinnedMetadataDoesNotDisplaceVerifiedEvent(t *testing.T) {
	// KB-319: an UNPINNED metadata value is not a target declaration (e.g.
	// a KB-253 artifact source copied through) and must not displace a
	// digest-verified event reference.
	plan := &workflowPlan{adapterImages: map[string]string{"shell": "registry.example:5000/criteria-adapter-shell"}}
	scope := events.LifecycleEvent{
		Event:          events.EventProvisionWanted,
		Digest:         "sha256:d9f306c29f4145da8bcc44187c9e4ae0f69ed30db3b3edac6e9b6350469bc635",
		ImageReference: "registry.example:5000/criteria-adapter-shell@sha256:abc",
	}
	assert.Equal(t, "registry.example:5000/criteria-adapter-shell@sha256:abc",
		resolveAdapterImage(plan, scope, "shell", adapterImageDefaults()),
		"unpinned metadata fills in only where the event lane has nothing pinned")

	// Without the event the unpinned metadata is still the image for the
	// kind (it is the only declaration the run has).
	assert.Equal(t, "registry.example:5000/criteria-adapter-shell",
		resolveAdapterImage(plan, events.LifecycleEvent{}, "shell", adapterImageDefaults()))
}

func TestResolveAdapterImageOverrideWinsWithoutEventPin(t *testing.T) {
	// The override is per-kind and applies whenever the event lane cannot
	// verify its reference (no digest, or a bare/absent reference) — CRI-214
	// M14 behavior preserved where the chain has no earlier lane.
	plan := &workflowPlan{adapterImages: map[string]string{"shell": "localhost:5000/criteria-adapter-shell:k8s-0.5.4-2"}}

	// A zero event (no digest, no reference) falls through to the override,
	// then to the configured defaults.
	assert.Equal(t, "localhost:5000/criteria-adapter-shell:k8s-0.5.4-2", resolveAdapterImage(plan, events.LifecycleEvent{}, "shell", adapterImageDefaults()))

	// copilot has no override entry, so the zero event falls all the way
	// through to the configured defaults.
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
	// Engines before the image_reference emission publish no digest; a
	// reference without a digest is unverified and must not displace the
	// configured defaults — whatever its pin shape. Combined with the
	// KB-306 flip this is also the guard against a bare repository name
	// (a plugin-ADAPTER artifact source) masquerading as a pod image.
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

func TestResolveAdapterImageBareArtifactSourceIsNotAPodImage(t *testing.T) {
	// KB-253 graph members carry the plugin-ADAPTER artifact source as
	// ImageReference (a bare repo name) alongside their digest. Repo@digest
	// of a plugin artifact cannot run as a container image (docker pull
	// cannot fetch a plugin artifact), so the digest-verified lane must
	// reject the bare shape and fall through — never resolve an :latest
	// pull (or a repo@digest pull) of an artifact repo as a pod image.
	plan := &workflowPlan{adapterImages: map[string]string{"shell": "localhost:5000/criteria-adapter-shell:k8s-0.5.4-2"}}
	scope := events.LifecycleEvent{
		Event:          events.EventProvisionWanted,
		Digest:         "sha256:d9f306c29f4145da8bcc44187c9e4ae0f69ed30db3b3edac6e9b6350469bc635",
		ImageReference: "registry.example:5000/criteria-adapter-shell",
	}
	assert.Equal(t, "localhost:5000/criteria-adapter-shell:k8s-0.5.4-2", resolveAdapterImage(plan, scope, "shell", adapterImageDefaults()),
		"a bare artifact source is not pin-qualified; the override is the pod image")

	assert.Equal(t, "registry.example:5000/criteria-adapter-shell:k8s-9", resolveAdapterImage(nil, scope, "shell", adapterImageDefaults()),
		"without an override a bare artifact source falls through to the defaults")

	assert.False(t, pinnedImageReference("registry.example:5000/criteria-adapter-shell"))
	assert.False(t, pinnedImageReference("localhost:5000/repo"), "a registry port is not a tag")
	assert.True(t, pinnedImageReference("registry.example:5000/criteria-adapter-shell:k8s-3"))
	assert.True(t, pinnedImageReference("registry.example:5000/criteria-adapter-shell@sha256:abc"))
	assert.True(t, pinnedImageReference("localhost:5000/repo@sha256:abc"))
	assert.False(t, pinnedImageReference(""))
}

// KB-319: the adapter-image conflict policy matrix — a PINNED metadata
// pin and a digest-verified pinned event pin that DISAGREE on one kind.
// The policy arbitrates only that disagreement; agreement (or one lane
// missing) is resolved by the chain regardless of policy.
func TestResolveAdapterImageConflictPolicyMatrix(t *testing.T) {
	digest := "sha256:d9f306c29f4145da8bcc44187c9e4ae0f69ed30db3b3edac6e9b6350469bc635"
	scope := events.LifecycleEvent{
		Event:          events.EventProvisionWanted,
		Digest:         digest,
		ImageReference: "registry.example:5000/criteria-adapter-shell@sha256:eventpin",
	}
	metaPinned := func(policy string) *workflowPlan {
		return &workflowPlan{
			adapterImages:       map[string]string{"shell": "registry.example:5000/criteria-adapter-shell@sha256:metapin"},
			imageConflictPolicy: policy,
		}
	}

	t.Run("no policy fails closed at the controller", func(t *testing.T) {
		// The resolver falls back to the metadata pin (never an empty pod
		// image); enforcing the refusal is the controller's consult.
		conflict := DetectAdapterImageConflict(&criteriav1.RunWorkflow{
			AdapterImages: map[string]string{"shell": "registry.example:5000/criteria-adapter-shell@sha256:metapin"},
		}, scope, "shell")
		require.NotNil(t, conflict)
		assert.Equal(t, "shell", conflict.Kind)
		assert.Equal(t, "registry.example:5000/criteria-adapter-shell@sha256:metapin", conflict.MetadataRef)
		assert.Equal(t, "registry.example:5000/criteria-adapter-shell@sha256:eventpin", conflict.EventRef)
		assert.Equal(t, "registry.example:5000/criteria-adapter-shell@sha256:metapin",
			resolveAdapterImage(metaPinned(""), scope, "shell", adapterImageDefaults()))
		assert.Equal(t, criteriav1.ImageConflictPolicyFailClosed, (&workflowPlan{}).conflictPolicy())
	})

	t.Run("prefer-event gives the digest-verified pin the precedence", func(t *testing.T) {
		assert.Equal(t, "registry.example:5000/criteria-adapter-shell@sha256:eventpin",
			resolveAdapterImage(metaPinned(criteriav1.ImageConflictPolicyPreferEvent), scope, "shell", adapterImageDefaults()))
	})

	t.Run("prefer-metadata keeps the target declaration", func(t *testing.T) {
		assert.Equal(t, "registry.example:5000/criteria-adapter-shell@sha256:metapin",
			resolveAdapterImage(metaPinned(criteriav1.ImageConflictPolicyPreferMetadata), scope, "shell", adapterImageDefaults()))
	})

	t.Run("agreeing pins resolve by the chain, policy blind", func(t *testing.T) {
		agree := events.LifecycleEvent{
			Event:          events.EventProvisionWanted,
			Digest:         digest,
			ImageReference: "registry.example:5000/criteria-adapter-shell:metapin",
		}
		pin := &workflowPlan{adapterImages: map[string]string{"shell": "registry.example:5000/criteria-adapter-shell:metapin"}}
		assert.Equal(t, "registry.example:5000/criteria-adapter-shell:metapin",
			resolveAdapterImage(pin, agree, "shell", adapterImageDefaults()),
			"an agreeing digest-verified event never conflicts")
		assert.Nil(t, DetectAdapterImageConflict(&criteriav1.RunWorkflow{
			AdapterImages: map[string]string{"shell": "registry.example:5000/criteria-adapter-shell:metapin"},
		}, agree, "shell"))
	})

	t.Run("a missing lane never conflicts, policy blind", func(t *testing.T) {
		noMeta := &workflowPlan{}
		assert.Equal(t, "registry.example:5000/criteria-adapter-shell@sha256:eventpin",
			resolveAdapterImage(noMeta, scope, "shell", adapterImageDefaults()))
		assert.Nil(t, DetectAdapterImageConflict(noEventWorkflow(), events.LifecycleEvent{}, "shell"))
	})
}

// noEventWorkflow is a run workflow with a pinned metadata value whose
// conflict detection is checked against an eventless resolution.
func noEventWorkflow() *criteriav1.RunWorkflow {
	return &criteriav1.RunWorkflow{AdapterImages: map[string]string{"shell": "registry.example:5000/criteria-adapter-shell@sha256:metapin"}}
}

// KB-319: the peer container resolves the same conflict policy — the
// matrix spot-checks the peer path so the peer-shaped default can never
// shadow a policy outcome.
func TestResolvePeerAdapterImageConflictPolicy(t *testing.T) {
	digest := "sha256:d9f306c29f4145da8bcc44187c9e4ae0f69ed30db3b3edac6e9b6350469bc635"
	scope := events.LifecycleEvent{
		Event:          events.EventProvisionWanted,
		Digest:         digest,
		ImageReference: "registry.example:5000/criteria-adapter-shell-peer@sha256:eventpin",
	}
	plan := func(policy string) *workflowPlan {
		return &workflowPlan{
			adapterImages:       map[string]string{"shell": "registry.example:5000/criteria-adapter-shell-peer@sha256:metapin"},
			imageConflictPolicy: policy,
		}
	}

	// KB-319 chain: the peer-shaped pinned metadata lane leads.
	assert.Equal(t, "registry.example:5000/criteria-adapter-shell-peer@sha256:metapin",
		resolvePeerAdapterImage(plan(""), scope, "shell", adapterImageDefaults()))
	// prefer-event hands the digest-verified pin the precedence.
	assert.Equal(t, "registry.example:5000/criteria-adapter-shell-peer@sha256:eventpin",
		resolvePeerAdapterImage(plan(criteriav1.ImageConflictPolicyPreferEvent), scope, "shell", adapterImageDefaults()))
	// An unpinned metadata value must not displace the verified event,
	// still resolving on the -peer default shape only when nothing else
	// declares the image.
	assert.Equal(t, "registry.example:5000/criteria-adapter-shell-peer@sha256:eventpin",
		resolvePeerAdapterImage(&workflowPlan{adapterImages: map[string]string{"shell": "registry.example:5000/criteria-adapter-shell-peer"}}, scope, "shell", adapterImageDefaults()))
	assert.Equal(t, "registry.example:5000/criteria-adapter-shell-peer:k8s-9",
		resolvePeerAdapterImage(nil, events.LifecycleEvent{}, "shell", adapterImageDefaults()),
		"the peer-shaped default is preserved")
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

// CRI-214 M14: BuildPerScopeAdapterPod consumes the whole resolution chain,
// not just the defaults branch — the KB-319 chain decides and an
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

	t.Run("workflow override wins when the event lane cannot verify", func(t *testing.T) {
		run := perScopeAdapterTestRun()
		run.Spec.Workflow = &criteriav1.RunWorkflow{AdapterImages: map[string]string{"shell": "localhost:5000/criteria-adapter-shell:k8s-0.5.4-2"}}
		// The event carries no digest, so the reference is unverified and
		// the override applies (a digest-verified reference would have
		// outranked it — tested above and in the peer builder's pin).
		scope := events.LifecycleEvent{
			Event:          events.EventProvisionWanted,
			RunID:          "CRI-42",
			ScopeID:        "root",
			AdapterName:    "intake",
			AdapterType:    "shell",
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

// KB-214: peer pods resolve through the same precedence order as the
// legacy fallback but land on a peer-shaped default:
// <registry>/criteria-adapter-<kind>-peer:<tag>. The -peer repo's
// ENTRYPOINT is `criteria peer`, so the legacy-named default must never
// backstop a peer pod. These tests mirror the resolveAdapterImage branch
// pins on the peer path and pin the builder-level resolution.
func TestResolvePeerAdapterImageDefaultsToPeerRepo(t *testing.T) {
	assert.Equal(t, "registry.example:5000/criteria-adapter-shell-peer:k8s-9", resolvePeerAdapterImage(nil, events.LifecycleEvent{}, "shell", adapterImageDefaults()))

	// Zero Defaults fall back to the built-in registry and tag on the
	// -peer repo.
	assert.Equal(t, "localhost:5000/criteria-adapter-copilot-peer:k8s-3", resolvePeerAdapterImage(nil, events.LifecycleEvent{}, "copilot", Defaults{}))

	// Components override independently, same as the legacy default.
	registryOnly := resolvePeerAdapterImage(nil, events.LifecycleEvent{}, "shell", Defaults{AdapterRegistry: "reg2.example:443"})
	assert.Equal(t, "reg2.example:443/criteria-adapter-shell-peer:k8s-3", registryOnly)
	tagOnly := resolvePeerAdapterImage(nil, events.LifecycleEvent{}, "shell", Defaults{AdapterTag: "k8s-9"})
	assert.Equal(t, "localhost:5000/criteria-adapter-shell-peer:k8s-9", tagOnly)
}

func TestResolvePeerAdapterImageDigestVerifiedReferenceWins(t *testing.T) {
	// KB-319: with the digest-verified event as the ONLY pinned lane — no
	// pinned metadata — the event reference resolves the peer image. (The
	// pinned-metadata-differs case is the conflict policy's, tested above.)
	scope := events.LifecycleEvent{
		Event:          events.EventProvisionWanted,
		Digest:         "sha256:d9f306c29f4145da8bcc44187c9e4ae0f69ed30db3b3edac6e9b6350469bc635",
		ImageReference: "registry.example:5000/criteria-adapter-shell@sha256:abc",
	}

	img := resolvePeerAdapterImage(nil, scope, "shell", adapterImageDefaults())
	assert.Equal(t, "registry.example:5000/criteria-adapter-shell@sha256:abc", img,
		"a digest-verified image_reference with no pinned metadata resolves the peer image")
}

func TestResolvePeerAdapterImageOverrideWinsWithoutEventPin(t *testing.T) {
	// The override applies whenever the event lane cannot verify its
	// reference (no digest, bare/absent reference): it is the optional
	// escape hatch beneath the lockfile pin (KB-306).
	plan := &workflowPlan{adapterImages: map[string]string{"shell": "localhost:5000/criteria-adapter-shell-peer:k8s-0.5.4"}}

	assert.Equal(t, "localhost:5000/criteria-adapter-shell-peer:k8s-0.5.4",
		resolvePeerAdapterImage(plan, events.LifecycleEvent{}, "shell", adapterImageDefaults()),
		"the workflow object's adapterImages override wins when the event carries no digest")
}

func TestResolvePeerAdapterImageUsesDigestVerifiedReference(t *testing.T) {
	scope := events.LifecycleEvent{
		Event:          events.EventProvisionWanted,
		Digest:         "sha256:d9f306c29f4145da8bcc44187c9e4ae0f69ed30db3b3edac6e9b6350469bc635",
		ImageReference: "registry.example:5000/criteria-adapter-shell-peer@sha256:abc",
	}

	assert.Equal(t, "registry.example:5000/criteria-adapter-shell-peer@sha256:abc",
		resolvePeerAdapterImage(nil, scope, "shell", adapterImageDefaults()),
		"a digest-verified image_reference displaces the peer default")
}

func TestResolvePeerAdapterImageWithoutDigestIgnoresReference(t *testing.T) {
	scope := events.LifecycleEvent{
		Event:          events.EventProvisionWanted,
		ImageReference: "evil-registry.example/criteria-adapter-shell-peer:latest",
	}

	assert.Equal(t, "registry.example:5000/criteria-adapter-shell-peer:k8s-9",
		resolvePeerAdapterImage(nil, scope, "shell", adapterImageDefaults()),
		"an image_reference without a digest is unverified and falls through to the peer default")
}

// BuildPerScopePeerPod consumes the whole peer resolution order, not just
// the defaults branch: an unverified reference must not leak into the pod,
// and the no-pin/no-ref default must be the -peer shape even when the
// operator's legacy tag is configured.
func peerImageTestRun(adapterImages map[string]string) *criteriav1.CriteriaRun {
	run := perScopeAdapterTestRun()
	if adapterImages != nil {
		run.Spec.Workflow = &criteriav1.RunWorkflow{AdapterImages: adapterImages}
	}
	return run
}

func TestBuildPerScopePeerPodResolvesPeerImage(t *testing.T) {
	digest := "sha256:d9f306c29f4145da8bcc44187c9e4ae0f69ed30db3b3edac6e9b6350469bc635"
	member := func(digest, ref string) events.LifecycleEvent {
		m := events.LifecycleEvent{
			Event:       events.EventProvisionWanted,
			RunID:       "CRI-42",
			ScopeID:     "root",
			ScopeTag:    "root-scope",
			AdapterName: "intake",
			AdapterType: "shell",
			TokenFile:   "/data/intake/CRI-42/tokens/intake",
			Environment: "ci",
		}
		if digest != "" {
			m.Digest = digest
		}
		if ref != "" {
			m.ImageReference = ref
		}
		return m
	}
	verified := member(digest, "registry.example:5000/criteria-adapter-shell-peer@sha256:abc")
	unverified := member("", "evil-registry.example/criteria-adapter-shell-peer:latest")
	mkPod := func(run *criteriav1.CriteriaRun, members []events.LifecycleEvent) corev1.Container {
		t.Helper()
		pod := BuildPerScopePeerPod(run, Defaults{AdapterTag: "k8s-5"}, "root", "ci", members, "")
		require.NotNil(t, pod)
		return pod.Spec.Containers[0]
	}

	// A digest-verified event reference wins over the configured default.
	got := mkPod(peerImageTestRun(nil), []events.LifecycleEvent{verified})
	assert.Equal(t, "registry.example:5000/criteria-adapter-shell-peer@sha256:abc", got.Image)

	// An unverified reference must not displace the peer default — and the
	// default is the -peer shape, not the legacy criteria-adapter-<kind>
	// image whose ENTRYPOINT cannot host the manifest.
	got = mkPod(peerImageTestRun(nil), []events.LifecycleEvent{unverified})
	assert.Equal(t, "localhost:5000/criteria-adapter-shell-peer:k8s-5", got.Image)

	// The pinned metadata lane outranks the digest-verified reference
	// (KB-319 chain; the digest-verified pin survives as the resolution
	// only under the prefer-event conflict policy).
	got = mkPod(peerImageTestRun(map[string]string{"shell": "localhost:5000/criteria-adapter-shell-peer:k8s-0.5.4"}),
		[]events.LifecycleEvent{verified})
	assert.Equal(t, "localhost:5000/criteria-adapter-shell-peer:k8s-0.5.4", got.Image)

	run := peerImageTestRun(map[string]string{"shell": "localhost:5000/criteria-adapter-shell-peer:k8s-0.5.4"})
	run.Spec.Workflow.ImageConflictPolicy = criteriav1.ImageConflictPolicyPreferEvent
	got = mkPod(run, []events.LifecycleEvent{verified})
	assert.Equal(t, "registry.example:5000/criteria-adapter-shell-peer@sha256:abc", got.Image)

	// An override still resolves the pod when the event lane cannot verify
	// (no digest on the event → the reference is unverified).
	got = mkPod(peerImageTestRun(map[string]string{"shell": "localhost:5000/criteria-adapter-shell-peer:k8s-0.5.4"}),
		[]events.LifecycleEvent{unverified})
	assert.Equal(t, "localhost:5000/criteria-adapter-shell-peer:k8s-0.5.4", got.Image)
}
