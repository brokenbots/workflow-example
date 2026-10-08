// Adapter image resolution for the jobbuilder (CRI-214 M14). The workflow's
// lockfile pins the adapter images and publishes the resolved reference on
// provision_wanted events (engine commit 4079d836); the operator no longer
// invents per-kind images. Per-scope adapter pods resolve in order:
//
//  1. the workflow object's adapterImages override (the routes ConfigMap's
//     per-workflow setting, same surface as volumes/secrets/env),
//  2. the event's image_reference — only when the event carries the
//     lockfile digest, so only digest-verified references are consumed
//     (the trust posture is unchanged: the digest still travels to the
//     adapter and the engine-side verifier gates every session open),
//  3. the operator's configured registry/tag defaults.
//
// Peer-scope pods (KB-214) resolve through the same order but land on a
// peer-shaped default: <registry>/criteria-adapter-<kind>-peer:<tag>. The
// -peer images' ENTRYPOINT is `criteria peer` (Dockerfile.peer), so the
// legacy-named default must never backstop a peer pod — the peer container
// would boot with no peer entrypoint. resolvePeerAdapterImage is that
// order; resolveAdapterImage stays for the legacy per-adapter fallback
// builder the pre-CRI-233 mixed fleet keeps running.
//
// Legacy run-duration adapter Jobs (pre-per-scope shape) have no lifecycle
// event and resolve from the override and the defaults.
package jobbuilder

import (
	"fmt"

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

// resolveAdapterImage resolves the adapter image for one adapter kind, in
// the precedence order of the module comment. scope may be the zero event
// (legacy adapter Jobs and pre-image_reference engines), which falls
// through to the configured defaults.
func resolveAdapterImage(plan *workflowPlan, scope events.LifecycleEvent, kind string, defaults Defaults) string {
	if ref := plan.adapterImageOverride(kind); ref != "" {
		return ref
	}
	// The event's image_reference is consumed only alongside its digest:
	// a digest-verified pin is the trust boundary the engine-side verifier
	// enforces when the session opens. A reference without a digest is
	// unverified and must not displace the configured defaults.
	if scope.Digest != "" && scope.ImageReference != "" {
		return scope.ImageReference
	}
	return defaultAdapterImage(kind, defaults)
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
// adapter kind, in the same precedence order as resolveAdapterImage but
// with a peer-shaped default: the -peer repo's ENTRYPOINT is `criteria
// peer`, so the legacy-named default must not backstop a peer pod.
func resolvePeerAdapterImage(plan *workflowPlan, scope events.LifecycleEvent, kind string, defaults Defaults) string {
	if ref := plan.adapterImageOverride(kind); ref != "" {
		return ref
	}
	if scope.Digest != "" && scope.ImageReference != "" {
		return scope.ImageReference
	}
	return defaultPeerAdapterImage(kind, defaults)
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
