// Package binding parses, validates, and materializes per-project
// CriteriaBinding ConfigMap payloads (KB-311, ADR 307): the project's
// COMPLETE execution config, owned in the project namespace. The global
// routes CM stops carrying project-shaped data for projects marked
// "cutover: done" in the routes projectRegistry; the workflow-catalog
// entry keeps only the tree references (workflow URL/ref, interface) and
// the run stamps a projectBindingRef instead (binding REFERENCES only).
//
// The payload shape is schema-checked by k8s/criteria-binding.schema.json
// and the examples validate the same interface contract via
// scripts/interface_validate.mjs; this package is the Go gate the
// operator reads bindings through. Parse fails on unknown fields at every
// level (the binding carries NOTHING else — fail closed), Validate holds
// the semantic rules, and Checksum produces the launch-manifest digest
// stamped at admission.
package binding

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/brokenbots/workflow-example/criteria-k8s/internal/routes"
)

const (
	// APIVersion is the binding payload's apiVersion.
	APIVersion = "criteria.brokenbots.dev/v1"
	// Kind is the binding payload's kind.
	Kind = "CriteriaBinding"

	// CMName is the binding ConfigMap's name inside the project namespace.
	CMName = "criteria-binding"
	// BindingKey is the data key carrying the binding payload.
	BindingKey = "binding.json"
)

// configSlots (KB-103) are the only config keys a binding's config map
// may carry: the platform config keys a workflow tree's interface may
// declare as slots — each config VALUE renders into the tree's declared
// slot variable through the per-run config rendering.
var configSlots = map[string]bool{
	"buildCmd":   true,
	"testCmd":    true,
	"ciGateCmd":  true,
	"baseBranch": true,
}

// Payload is one project's binding (the criteria-binding ConfigMap's
// binding.json value). Volumes and secrets reuse the routes payload
// shapes: a project's OWN PVCs and SecretProviderClasses, named
// namespace-locally — cross-namespace references are impossible because
// every volume/secret name resolves inside the run's namespace.
type Payload struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Images     map[string]string `json:"images"`
	Env        map[string]string `json:"env"`
	Volumes    []routes.Volume   `json:"volumes"`
	Secrets    []routes.Secret   `json:"secrets"`
	Config     map[string]string `json:"config"`
}

// Parse decodes a binding payload, failing closed on unknown fields at
// every level: the binding carries images, env, volumes, secrets, config
// and NOTHING else (unknown keys reject, per ADR 307). Nested volume and
// secret objects decode through the routes payloads, so their unknown
// fields reject here too.
func Parse(data []byte) (Payload, error) {
	var p Payload
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return Payload{}, fmt.Errorf("binding payload: %w", err)
	}
	if err := p.Validate(); err != nil {
		return Payload{}, err
	}
	return p, nil
}

// Validate enforces the binding's semantic rules: the fixed
// apiVersion/kind, adapter image refs, env names and values, per-kind
// volume/secret shapes (through the routes validators), the KB-311 config
// slot surface, and unique volume/secret names.
func (p Payload) Validate() error {
	if p.APIVersion != APIVersion {
		return fmt.Errorf("binding apiVersion must be %q, got %q", APIVersion, p.APIVersion)
	}
	if p.Kind != Kind {
		return fmt.Errorf("binding kind must be %q, got %q", Kind, p.Kind)
	}
	if err := routes.ValidateAdapterImages(p.Images); err != nil {
		return fmt.Errorf("binding images: %w", err)
	}
	if err := routes.ValidateEnvMap(p.Env, "env"); err != nil {
		return fmt.Errorf("binding %w", err)
	}
	seen := map[string]bool{}
	for i, v := range p.Volumes {
		if seen[v.Name] {
			return fmt.Errorf("binding volumes[%d] duplicates name %q", i, v.Name)
		}
		seen[v.Name] = true
		if err := routes.ValidateVolume(v); err != nil {
			return fmt.Errorf("binding volume %q: %w", v.Name, err)
		}
	}
	clear(seen)
	for i, s := range p.Secrets {
		if seen[s.Name] {
			return fmt.Errorf("binding secrets[%d] duplicates name %q", i, s.Name)
		}
		seen[s.Name] = true
		if err := routes.ValidateSecret(s); err != nil {
			return fmt.Errorf("binding secret %q: %w", s.Name, err)
		}
	}
	for slot := range p.Config {
		if !configSlots[slot] {
			return fmt.Errorf("binding config slot %q is not one of the platform config keys (buildCmd, testCmd, ciGateCmd, baseBranch)", slot)
		}
		if p.Config[slot] == "" {
			return fmt.Errorf("binding config.%s must be a string value", slot)
		}
	}
	return nil
}

// Checksum hashes the binding's canonical JSON form (parsed, re-marshaled):
// the digest stamped into the launch manifest at admission, so the
// operator can report which exact binding bytes a run executed with.
func (p Payload) Checksum() string {
	canonical, err := json.Marshal(p)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

// Apply materializes the binding onto a resolved workflow definition (the
// in-memory routes.Workflow value the operator assembles its launch
// from): binding-shaped fields are REPLACED — adapter images, env,
// volumes, secrets — because the project now owns the execution config
// (ADR 307). Tree-shaped fields the workflow entry still provides (Type,
// Image — the url+image process image —, URL, Ref, Interface, Class) are
// preserved.
func (p Payload) Apply(wf routes.Workflow) routes.Workflow {
	wf.Volumes = nil
	wf.Secrets = nil
	wf.Env = nil
	wf.AdapterImages = nil

	if len(p.Images) > 0 {
		wf.AdapterImages = make(map[string]string, len(p.Images))
		for kind, ref := range p.Images {
			wf.AdapterImages[kind] = ref
		}
	}
	if len(p.Env) > 0 {
		wf.Env = make(map[string]string, len(p.Env))
		for name, value := range p.Env {
			wf.Env[name] = value
		}
	}
	if len(p.Volumes) > 0 {
		wf.Volumes = make([]routes.Volume, len(p.Volumes))
		copy(wf.Volumes, p.Volumes)
	}
	if len(p.Secrets) > 0 {
		wf.Secrets = make([]routes.Secret, len(p.Secrets))
		copy(wf.Secrets, p.Secrets)
	}
	return wf
}
