// Adapter image resolution for the jobbuilder (CRI-214 M14, precedence
// flipped by KB-306, re-flipped by KB-319). The workflow's lockfile pins
// the adapter images and publishes the resolved reference on
// provision_wanted events (engine commit 4079d836). Per-scope adapter pods
// resolve through the KB-319 chain of record:
//
//		target_metadata > digest-verified event > binding > operator defaults
//
//	  - target_metadata — the pinned adapter-image reference the run's
//	    spec.workflow.adapterImages carries for the kind. That slot is the
//	    single writer-lane for the run's declared intent: pre-cutover it
//	    holds the routes workflow entry's per-kind pin; post-cutover the
//	    project binding's materialization (KB-311) REPLACES the slot with
//	    the binding's per-kind pins, so the binding is never a third
//	    contender beside the metadata — it is the metadata lane for
//	    cutover-done projects (the chain's documented "binding" element
//	    degenerates into the same lane the materialization is the only
//	    writer of). An UNPINNED metadata value (a bare repository name,
//	    e.g. a KB-253 artifact source copied through) is not a target
//	    declaration and must not displace a digest-verified event; it only
//	    fills in where the event lane has nothing pinned.
//	  - digest-verified event — the provision event's image_reference, only
//	    when the event carries the lockfile digest and the reference itself
//	    is pin-qualified (a tag or a digest suffix) — the CRI-214/KB-306
//	    trust posture, unchanged: the digest still travels to the adapter
//	    and the engine-side verifier gates every session open.
//	  - operator defaults — the configured registry/tag fallback
//	    (<registry>/criteria-adapter-<kind>[:peer]:<tag>).
//
// CONFLICT (KB-319): both lanes pinned and disagreeing for the same kind
// is a metadata-vs-digest conflict, arbitrated by the run's stamped
// ImageConflictPolicy — configurable per project, DEFAULT FAIL-CLOSED:
// ImageConflictPolicyFailClosed refuses the run before any builder runs
// (the controller consults DetectAdapterImageConflict ahead of the
// per-scope reconcile and parks the run failed with
// ConditionAdapterImageConflict); prefer-metadata/prefer-event pick a lane.
// The policy never displaces a lane the other one is missing.
//
// The legacy run-duration adapter Jobs (pre-per-scope shape,
// jobbuilder.go's BuildAdapterJob) pass a zero event: with no event lane
// they resolve metadata-or-defaults exactly as they always have.
//
// Peer-scope pods (KB-214) resolve through the same chain but land on a
// peer-shaped default: <registry>/criteria-adapter-<kind>-peer:<tag>. The
// -peer images' ENTRYPOINT is `criteria peer` (Dockerfile.peer), so the
// legacy-named default must never backstop a peer pod — the peer container
// would boot with no peer entrypoint. resolvePeerAdapterImage is that
// order; resolveAdapterImage stays for the legacy per-adapter fallback
// builder the pre-CRI-233 mixed fleet keeps running.
package jobbuilder

import (
	"fmt"
	"strings"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/events"
)

const (
	// EnvAdapterRegistry is the operator env var the adapter registry host
	// resolves from. cmd/operator mirrors it as the --adapter-registry
	// flag default.
	EnvAdapterRegistry = "ADAPTER_REGISTRY"
	// EnvAdapterTag is the operator env var the default adapter image tag
	// resolves from. cmd/operator mirrors it as the --adapter-tag flag
	// default.
	EnvAdapterTag = "ADAPTER_TAG"
	// DefaultAdapterRegistry is the adapter registry host when neither the
	// operator config nor the environment declares one: the in-cluster
	// registry the shipped images publish to.
	DefaultAdapterRegistry = "localhost:5000"
	// DefaultAdapterTag is the default adapter image tag for references the
	// operator composes itself.
	DefaultAdapterTag = "k8s-3"
)

// ImageConflict is a KB-319 adapter-image conflict: the metadata lane's
// pinned reference and the digest-verified event lane's pinned reference
// disagree for one adapter kind. The controller fails a run that carries
// this conflict under the fail-closed default policy, naming the run and
// both references.
type ImageConflict struct {
	// Kind is the adapter kind the two lanes disagree on.
	Kind string
	// MetadataRef is the spec.workflow.adapterImages pin (route-declared
	// pre-cutover, binding-materialized post-cutover).
	MetadataRef string
	// EventRef is the digest-verified provision event's pin.
	EventRef string
}

// DetectAdapterImageConflict reports the KB-319 metadata-vs-digest
// conflict for resolving kind's image for scope, or nil when the
// resolution would not cross one. Exported so the controller can consult
// the fail-closed default BEFORE any per-scope builder runs.
func DetectAdapterImageConflict(wf *criteriav1.RunWorkflow, scope events.LifecycleEvent, kind string) *ImageConflict {
	if wf == nil || kind == "" {
		return nil
	}
	meta := wf.AdapterImages[kind]
	if !pinnedImageReference(meta) || !digestVerifiedPinned(scope) {
		return nil
	}
	if meta == scope.ImageReference {
		return nil
	}
	return &ImageConflict{Kind: kind, MetadataRef: meta, EventRef: scope.ImageReference}
}

// digestVerifiedPinned reports whether the scope's event lane may produce
// a pod image at all: the lockfile digest AND a pin-qualified reference.
func digestVerifiedPinned(scope events.LifecycleEvent) bool {
	return scope.Digest != "" && pinnedImageReference(scope.ImageReference)
}

// resolveAdapterImage resolves the adapter image for one adapter kind, in
// the precedence order of the module comment. scope may be the zero event
// (legacy adapter Jobs and pre-image_reference engines), which falls
// through to the configured defaults. Under the fail-closed policy the
// controller refuses the run BEFORE builders run, so a builder meeting a
// fail-closed conflict resolves to the metadata pin (the controller-level
// refusal is the enforcement; this is a belt for any future call path).
func resolveAdapterImage(plan *workflowPlan, scope events.LifecycleEvent, kind string, defaults Defaults) string {
	return resolveImage(plan, scope, kind, defaults, defaultAdapterImage(kind, defaults))
}

// defaultAdapterImage renders the operator-configured fallback:
// <registry>/criteria-adapter-<kind>:<tag>, each component falling back to
// its built-in default when unset.
func defaultAdapterImage(kind string, defaults Defaults) string {
	registry := firstNonEmpty(defaults.AdapterRegistry, DefaultAdapterRegistry)
	tag := firstNonEmpty(defaults.AdapterTag, DefaultAdapterTag)
	return fmt.Sprintf("%s/criteria-adapter-%s:%s", registry, kind, tag)
}

// resolvePeerAdapterImage resolves the peer container's image for one
// adapter kind, in the same precedence as resolveAdapterImage but with a
// peer-shaped default: the -peer repo's ENTRYPOINT is `criteria peer`, so
// the legacy-named default must not backstop a peer pod.
func resolvePeerAdapterImage(plan *workflowPlan, scope events.LifecycleEvent, kind string, defaults Defaults) string {
	return resolveImage(plan, scope, kind, defaults, defaultPeerAdapterImage(kind, defaults))
}

// resolveImage walks the KB-319 chain for one (plan, scope, kind): the
// pinned metadata lane leads, a digest-verified pinned event is the next
// lane (or wins the conflict under prefer-event; the conflict under any
// policy resolves to the metadata pin here — fail-closed enforcement is
// the controller's refusal before building), then UNPINNED metadata, then
// the fallback default the caller names (the plain or peer-shape repo).
func resolveImage(plan *workflowPlan, scope events.LifecycleEvent, kind string, defaults Defaults, fallback string) string {
	meta := plan.adapterImageOverride(kind)
	metaPinned := pinnedImageReference(meta)
	if metaPinned && digestVerifiedPinned(scope) && meta != scope.ImageReference {
		// KB-319 conflict: both lanes pin a different reference.
		if plan.conflictPolicy() == criteriav1.ImageConflictPolicyPreferEvent {
			return scope.ImageReference
		}
		return meta
	}
	if metaPinned {
		return meta
	}
	if digestVerifiedPinned(scope) {
		return scope.ImageReference
	}
	if meta != "" {
		// Unpinned metadata fills in only where the event lane has nothing
		// pinned: a bare repository name (e.g. a KB-253 artifact source
		// carried in adapterImages) is not a target declaration.
		return meta
	}
	return fallback
}

// conflictPolicy resolves the plan's stamped policy, defaulting to the
// fail-closed policy when unstamped.
func (p *workflowPlan) conflictPolicy() string {
	if p == nil || p.imageConflictPolicy == "" {
		return criteriav1.ImageConflictPolicyFailClosed
	}
	return p.imageConflictPolicy
}

// defaultPeerAdapterImage renders the peer-shape fallback:
// <registry>/criteria-adapter-<kind>-peer:<tag>. The registry and tag
// resolve from the same operator knobs as the legacy default — only the
// repo name carries the peer shape (KB-214).
func defaultPeerAdapterImage(kind string, defaults Defaults) string {
	registry := firstNonEmpty(defaults.AdapterRegistry, DefaultAdapterRegistry)
	tag := firstNonEmpty(defaults.AdapterTag, DefaultAdapterTag)
	return fmt.Sprintf("%s/criteria-adapter-%s-peer:%s", registry, kind, tag)
}

// pinnedImageReference reports whether ref is a container-image reference
// the digest-verified event lane may consume as a POD image: the reference
// must already carry its own pin — a tag or a digest suffix. A bare
// repository name is a plugin-adapter ARTIFACT source (the compiled
// workflow graph publishes `source` exactly so — KB-253), not a container
// image: docker pull cannot fetch a plugin artifact, so consuming a bare
// source as a pod image would resolve an :latest pull of a repo that
// carries no tags and wedge the pod in ImagePull. The graph's source
// belongs to the artifact-staging lane instead (BuildPerScopePeerPod).
func pinnedImageReference(ref string) bool {
	if ref == "" {
		return false
	}
	if at := strings.LastIndex(ref, "@"); at > strings.LastIndex(ref, "/") {
		return true // digest suffix
	}
	// Tag suffix: a colon after the last slash. A registry port's colon
	// sits before the last slash ("localhost:5000/repo"), so a bare
	// repo-on-port stays unpinned.
	return strings.Contains(ref[strings.LastIndex(ref, "/")+1:], ":")
}
