// Package routes reads and resolves the Criteria routes ConfigMap
// (CRI-216/217), the operator configuration of record assigning tickets
// to workflow-library objects. The payload of record is
// k8s/routes.schema.json; k8s/examples/routes-configmap.yaml is the shipped
// example. Lookup rules (CRI-217, per ADR-0005): the k8s-run gate stays
// global (watcher-side, not here); the ticket's project must exist in the
// routes map; the ticket's workflow state must be in the matched route's
// states list (CRI-218: omitted states default to [Triage]); the matching
// route's workflow is the project default; a ticket label in the "workflows"
// label group names a workflow that overrides the default; a missing
// workflow fails closed.
package routes

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"

	v1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
)

const (
	// ConfigMapName is the ConfigMap carrying the routes payload.
	ConfigMapName = "criteria-routes"
	// DataKey is the single ConfigMap data key holding the JSON payload.
	DataKey = "routes.json"
	// DefaultFile is the in-pod path the watcher reads the payload from:
	// the ConfigMap is mounted at /etc/criteria/routes and re-read every
	// poll, so ConfigMap changes never depend on a watcher restart.
	DefaultFile = "/etc/criteria/routes/routes.json"
	// APIVersion and Kind constants of the routes payload.
	APIVersion = "criteria.brokenbots.dev/v1"
	Kind       = "Routes"
)

// Workflow source types (ADR-0005 D1/D2). The mode is declared, never
// inferred: type=image is image-only, type=url is url-only or url+image.
const (
	TypeImage = "image"
	TypeURL   = "url"
)

// Volume backing kinds.
const (
	VolumePVC = "pvc"
	VolumeNFS = "nfs"
	VolumeTmp = "tmp"
	// VolumeK8sSecret mounts a plain Kubernetes Secret (same namespace as
	// the run) as files at mountPath (KB-7): token-file surfaces the runner
	// scripts read (e.g. /home/criteria/secrets/workflow_github_token) can
	// be served by a namespace Secret instead of a pre-populated PVC.
	// Distinct from the Secret entries: those render through the Secrets
	// Store CSI driver + OpenBao, while k8s-secret mounts a native Secret.
	VolumeK8sSecret = "k8s-secret"
)

// TagMatch semantics for a route's tag subset.
const (
	TagMatchAll = "all"
	TagMatchAny = "any"
)

// CutoverDone is the project-registry cutover marker (KB-311, ADR 307):
// the entry's namespace serves its own criteria-binding ConfigMap. The
// vocabulary of one value; anything else fails closed (an absent marker
// stays legal — pre-cutover projects keep the global workflow-library
// behavior until their binding cut-over: binding-first, deletion second).
const CutoverDone = "done"

// DefaultState is the workflow state a route triggers on when it
// omits its states list (CRI-218, ADR-0005 §3.4).
const DefaultState = "Triage"

// Admission queue classes (CRI-242). The class partitions the operator's
// admission queue: dev-class runs serialize per repoURL (concurrency 1),
// while triage-class runs are read-only against the repo and may run
// concurrently with any run on the same repoURL (per-ticket PVC clones
// isolate state). ClassDefault is what an omitted class resolves to, so
// routes without an explicit class keep the per-repo serialization they
// had before classes existed.
const (
	ClassDefault = "dev"
	ClassDev     = "dev"
	ClassTriage  = "triage"
)

// Payload is the routes ConfigMap payload (the 'routes.json' data key).
type Payload struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	// WorkflowLibrary is the workflow set routes select from.
	WorkflowLibrary map[string]Workflow `json:"workflowLibrary"`
	// ConfigLibrary (KB-103) is the per-repo configuration overlay routes
	// select alongside the workflow: entry name -> command overrides. The
	// name doubles as the per-repo ConfigMap the operator pins and renders
	// for the run. Optional: payloads predating KB-103 validate unchanged.
	ConfigLibrary map[string]ConfigEntry `json:"configLibrary,omitempty"`
	// ProjectRegistry (KB-311, ADR 307) maps a Kanboard project name to
	// the registry entry owning it: the namespace executing its runs and
	// serving its criteria-binding ConfigMap, advisory owner/name repo
	// members, and the cutover marker. The ONLY ticket->project routing
	// table (routes still select the workflow within the project by
	// project/tags/states). Optional: payloads predating KB-311 validate
	// unchanged, and a project without the cutover marker keeps the
	// global workflow-library binding data until its namespace serves its
	// own binding (binding-first, deletion second).
	ProjectRegistry map[string]ProjectEntry `json:"projectRegistry,omitempty"`
	Routes          []Route                 `json:"routes"`
}

// Workflow is a workflow-library object: how a run obtains its workflow and
// where/how it executes.
type Workflow struct {
	Type string `json:"type"`
	// Namespace is the namespace the workflow's pre-cutover runs execute
	// in. Optional (KB-313, ADR 307): cutover-done projects execute through
	// their registry namespace and binding CM, and a namespace-less entry's
	// runs resolve the run's own namespace. Validate still rejects a
	// non-DNS-1123-label value.
	Namespace string `json:"namespace,omitempty"`
	// Class is the admission queue class (CRI-242): "dev" serializes per
	// repoURL, "triage" (read-only against the repo) admits concurrently.
	// Empty defaults to ClassDefault at stamping; Validate rejects any
	// other value.
	Class string `json:"class,omitempty"`
	// Image is the workflow's process image (url+image or image mode).
	Image string `json:"image,omitempty"`
	URL   string `json:"url,omitempty"`
	Ref   string `json:"ref,omitempty"`
	// Volumes, Secrets, Env, and AdapterImages are the KB-313 deprecated
	// global run-config surfaces (ADR 307): the global CM stops owning
	// project-shaped run config, so these fields are deprecated here and
	// must move into per-project binding CMs (binding-first, deletion
	// second — KB-311). While the migration window is open (any registry
	// project not yet cutover-done, or no registry at all) they still
	// validate and Payload.DeprecationWarnings lists them; once every
	// registry project is cutover-done, Validate rejects the payload
	// outright.
	Volumes []Volume          `json:"volumes,omitempty"`
	Secrets []Secret          `json:"secrets,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	// AdapterImages is the per-adapter-kind image override (CRI-214 M14):
	// adapter kind ("shell", "copilot", ...) to a full image reference the
	// operator stamps on this workflow's adapter pods, ahead of the event's
	// image_reference and the configured registry/tag defaults. Deprecated
	// post-cutover (KB-313): adapter images are a per-project binding
	// concern.
	AdapterImages map[string]string `json:"adapterImages,omitempty"`
	// Interface is the workflow tree's machine-readable interface
	// declaration (KB-310, ADR 307) embedded in the workflow object: the
	// schema of record is k8s/workflow-interface.schema.json, and the
	// routes schema mirror lives in k8s/routes.schema.json. Validate
	// requires it for type=url (the shared-tree contract) and rejects it
	// for type=image (the interface is the tree's, not the image's).
	Interface *WorkflowInterface `json:"interface,omitempty"`
}

// WorkflowInterface is the workflow tree's machine-readable interface
// declaration (KB-310, ADR 307): what a run needs from a project binding
// (required adapter kinds, mount paths, secret env lanes, env names) and
// the config slots the tree honors ("the declaration IS the supported
// override surface"). The operator validates bindings against it before
// job creation, fail closed.
type WorkflowInterface struct {
	// APIVersion is the declared apiVersion
	// (criteria.brokenbots.dev/v1).
	APIVersion string `json:"apiVersion"`
	// Kind is the declared kind: always WorkflowInterface.
	Kind string `json:"kind"`
	// Workflow is the workflow tree name the declaration belongs to.
	Workflow string `json:"workflow"`
	// Description is the tree's one-line description of the contract.
	Description string `json:"description,omitempty"`
	// Adapters are the required adapter kinds.
	Adapters []string `json:"adapters,omitempty"`
	// Mounts are the required mount paths.
	Mounts []WorkflowInterfaceMount `json:"mounts,omitempty"`
	// SecretEnv are the required secret env lanes (environment variable
	// names).
	SecretEnv []string `json:"secretEnv,omitempty"`
	// Env are the env names the tree reads on the workflow binding.
	Env []string `json:"env,omitempty"`
	// ConfigSlots are the config keys the tree honors.
	ConfigSlots []WorkflowInterfaceConfigSlot `json:"configSlots,omitempty"`
}

// WorkflowInterfaceMount is one required mount path of an interface
// declaration (KB-310).
type WorkflowInterfaceMount struct {
	// Name is the mount's logical name in the tree.
	Name string `json:"name"`
	// MountPath is the byte-exact container path the binding must satisfy.
	MountPath string `json:"mountPath"`
	// Kind is pvc (a bound claim) or tmp (an emptyDir scratch).
	Kind string `json:"kind"`
}

// WorkflowInterfaceConfigSlot is one config slot of an interface
// declaration (KB-310): a config key the tree honors, the tree variable it
// lands in, and the step/agent that consumes it. Declared slots are
// limited to the platform config key surface (buildCmd, testCmd, ciGateCmd,
// baseBranch) with their canonical delivery variables — those are the keys
// a project binding can carry through the KB-103 per-run config rendering.
type WorkflowInterfaceConfigSlot struct {
	// Slot is the config key name the binding may carry (camelCase,
	// lowercase first letter). Must be one of the platform config keys
	// (buildCmd, testCmd, ciGateCmd, baseBranch).
	Slot string `json:"slot"`
	// Variable is the tree variable the slot's value renders as.
	Variable string `json:"variable"`
	// Target is the step/agent within the tree that reads the variable.
	Target string `json:"target"`
	// Description describes the slot's purpose.
	Description string `json:"description,omitempty"`
}

// Volume is a storage volume of kind pvc, nfs, tmp, or k8s-secret.
type Volume struct {
	Name       string            `json:"name"`
	Kind       string            `json:"kind"`
	MountPath  string            `json:"mountPath"`
	SubPath    string            `json:"subPath,omitempty"`
	ReadOnly   bool              `json:"readOnly,omitempty"`
	Claim      string            `json:"claim,omitempty"`
	Server     string            `json:"server,omitempty"`
	Path       string            `json:"path,omitempty"`
	SizeLimit  string            `json:"sizeLimit,omitempty"`
	SecretName string            `json:"secretName,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
}

// Secret is a secret reference (SecretProviderClass name, OpenBao via the
// Secrets Store CSI driver). No credential material may appear in routes.
type Secret struct {
	Name                string            `json:"name"`
	SecretProviderClass string            `json:"secretProviderClass"`
	MountPath           string            `json:"mountPath"`
	Env                 map[string]string `json:"env,omitempty"`
}

// ConfigEntry is a per-repo configuration entry (KB-103): the value side
// of the routes payload's configLibrary. The entry's NAME identifies the
// per-repo ConfigMap the run pins (same name, in the execution namespace);
// the entry's command fields are what the watcher stamps into the run spec
// when a config is selected. The operator renders run-time commands from
// the pinned ConfigMap's identically named data keys
// (buildCmd/testCmd/ciGateCmd), falling back through these inline fields,
// the run spec fields, the watcher defaults, and empty: an empty gate
// self-skips exactly as before KB-103. Command fields carry shell command
// text, so whitespace is legal — only empty values fail validation.
//
// KB-154 tag-group config: an entry may declare the ticket tags it serves
// (Tags/TagMatch) — the declarative half of the tag-group config overlay
// on the KB-103 per-repo config. A ticket tag satisfying an entry's tag
// subset selects that entry's config for the run (tags: a `v0.6.0` group
// entry can set baseBranch to the release branch). The matched route's
// configRef stays the primary binding (KB-103 precedence); the tag-group
// overlay applies through the configs label group when the route carries
// no configRef, and the kanboard watcher uses entry tags to impute which
// flat tags belong to the configs group.
type ConfigEntry struct {
	// BuildCmd is the per-repo build command override.
	BuildCmd string `json:"buildCmd,omitempty"`
	// TestCmd is the per-repo test command override.
	TestCmd string `json:"testCmd,omitempty"`
	// CIGateCmd is the per-repo CI gate command override (the gate that
	// must pass before a PR is opened).
	CIGateCmd string `json:"ciGateCmd,omitempty"`
	// BaseBranch (KB-154) is the release-branch slice of the tag-group
	// config: the branch a run forks from, validates against, and opens
	// its PR against. Empty means the default branch ("main"), declared
	// by the routes schema of record. Whitespace rejects (git refs never
	// carry whitespace).
	BaseBranch string `json:"baseBranch,omitempty"`
	// Tags (KB-154) declares the ticket tags this entry serves: a ticket
	// carrying a tag in this subset selects the entry's config over the
	// plain repo config (the tag-group overlay). Omitted keeps the entry
	// tag-group-less: it is selected only by name (route configRef or a
	// configs-group label naming it), exactly as before KB-154.
	Tags []string `json:"tags,omitempty"`
	// TagMatch is the subset semantics for Tags: "all" (default) requires
	// the ticket to carry every declared tag, "any" at least one.
	TagMatch string `json:"tagMatch,omitempty"`
}

// ProjectEntry (KB-311, ADR 307) is the project-registry entry a Kanboard
// project name resolves to: the namespace executing the project's runs
// (and serving the project's own criteria-binding ConfigMap), the advisory
// owner/name repo members, and the cutover marker.
type ProjectEntry struct {
	// Namespace is where the project's CriteriaRuns are admitted and where
	// its criteria-binding ConfigMap lives. Required.
	Namespace string `json:"namespace"`
	// Repos lists the owner/name repository references the project's
	// tickets touch. Advisory only — never routed by (routes still select
	// the workflow within the project; the registry route is the Kanboard
	// project name, nothing else).
	Repos []string `json:"repos,omitempty"`
	// Cutover is "done" when the project's namespace serves its own
	// binding CRITERIA-BINDING ConfigMap — the intake then stamps binding
	// references only into the project namespace. Absent keeps the
	// pre-cutover behavior (binding-first, deletion second, KB-311).
	Cutover string `json:"cutover,omitempty"`
}

// IsCutoverDone reports whether the project's namespace serves its own
// criteria-binding ConfigMap (binding-first execution, KB-311).
func (e ProjectEntry) IsCutoverDone() bool {
	return e.Cutover == CutoverDone
}

// Route assigns tickets to a workflow-library object by project, tag
// subset, and workflow states.
type Route struct {
	Name     string   `json:"name"`
	Workflow string   `json:"workflow"`
	Project  string   `json:"project"`
	Tags     []string `json:"tags,omitempty"`
	TagMatch string   `json:"tagMatch,omitempty"`
	// States holds the workflow state names this route triggers on.
	// Omitted in the payload defaults to [DefaultState] (CRI-218); an
	// explicit empty or null list is rejected by Validate, mirroring the
	// schema of record (minItems: 1).
	States []string `json:"states"`
	// ConfigRef (KB-103) names the configLibrary entry this route's runs
	// execute under: the entry name doubles as the per-repo ConfigMap the
	// operator renders at admission. The route match wins over the
	// configs-label-group override; a ref naming an absent entry fails
	// closed in Validate (payload errors surface when the ConfigMap is
	// applied, not per ticket).
	ConfigRef string `json:"configRef,omitempty"`
}

var (
	labelRe      = mustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	envNameRe    = mustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	secretKeyRe  = mustCompile(`^[A-Za-z0-9._-]+$`)
	repoRefRe    = mustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)
	whitespaceRe = mustCompile(`\s`)
)

func mustCompile(pattern string) *regexp.Regexp {
	re, err := regexp.Compile(pattern)
	if err != nil {
		panic("invalid pattern: " + err.Error())
	}
	return re
}

// Parse decodes and validates a routes payload.
func Parse(data []byte) (*Payload, error) {
	var p Payload
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", DataKey, err)
	}
	var presence routeStatesPresence
	if err := json.Unmarshal(data, &presence); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", DataKey, err)
	}
	p.applyDefaults(presence)
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

// routeStatesPresence shadows Route with a raw states field so JSON key
// presence is observable: an omitted states key decodes to a nil
// RawMessage, while an explicit "states": [] or "states": null decodes to
// a non-nil one.
type routeStatesPresence struct {
	Routes []struct {
		StatesRaw json.RawMessage `json:"states"`
	} `json:"routes"`
}

// applyDefaults applies the payload defaults of k8s/routes.schema.json
// (CRI-218): a route omitting the states key triggers on [DefaultState].
// A route that declares states explicitly — empty, null, or a list — keeps
// what it declared, so Validate rejects an empty or null list exactly as
// the schema of record does (minItems: 1): fail closed, no silent [Triage].
func (p *Payload) applyDefaults(presence routeStatesPresence) {
	for i := range p.Routes {
		if i < len(presence.Routes) && presence.Routes[i].StatesRaw != nil {
			continue
		}
		p.Routes[i].States = []string{DefaultState}
	}
}

// LoadFile reads and validates the routes payload from its in-pod file.
// Callers re-invoke it per poll so ConfigMap changes are honored without a
// watcher restart.
func LoadFile(path string) (*Payload, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading routes file %s: %w", path, err)
	}
	return Parse(data)
}

// Validate enforces the fail-closed constraints of k8s/routes.schema.json:
// the payload shape, the ADR-0005 D1/D2 source modes (declared, never
// inferred), the pvc/nfs/tmp volume shapes, and route/library integrity.
// KB-313 (ADR 307) adds the post-cutover gate: once every projectRegistry
// entry is cutover-done, the workflowLibrary may no longer carry the global
// run-config surfaces (volumes, secrets, env, adapterImages) the global CM
// stopped owning — the payload hard-rejects so stale project-shaped data
// cannot ship. While any registry project is un-cut (and on payloads
// predating the registry) the same surfaces are allow-with-warning, surfaced
// through DeprecationWarnings.
func (p *Payload) Validate() error {
	if p.APIVersion != APIVersion {
		return fmt.Errorf("apiVersion must be %q, got %q", APIVersion, p.APIVersion)
	}
	if p.Kind != Kind {
		return fmt.Errorf("kind must be %q, got %q", Kind, p.Kind)
	}
	if len(p.WorkflowLibrary) == 0 {
		return errors.New("workflowLibrary must not be empty")
	}
	for name, wf := range p.WorkflowLibrary {
		if err := validateWorkflow(name, wf); err != nil {
			return fmt.Errorf("workflowLibrary[%q]: %w", name, err)
		}
	}
	for name, entry := range p.ConfigLibrary {
		if err := validateConfigEntry(name, entry); err != nil {
			return fmt.Errorf("configLibrary[%q]: %w", name, err)
		}
	}
	// KB-311 (ADR 307): the project registry — the ONLY ticket->project
	// routing table — is fail-closed at every field: blank project names,
	// non-label namespaces, malformed repo refs, and cutover markers
	// outside the closed vocabulary reject.
	for name, entry := range p.ProjectRegistry {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("projectRegistry has a blank project name")
		}
		if !labelRe.MatchString(entry.Namespace) {
			return fmt.Errorf("projectRegistry[%q]: namespace %q is not a DNS-1123 label", name, entry.Namespace)
		}
		for _, repo := range entry.Repos {
			if !repoRefRe.MatchString(repo) {
				return fmt.Errorf("projectRegistry[%q]: repo %q is not an \"owner/name\" reference", name, repo)
			}
		}
		switch entry.Cutover {
		case "", CutoverDone:
		default:
			return fmt.Errorf("projectRegistry[%q]: cutover must be %q or absent, got %q (binding-first, deletion second)", name, CutoverDone, entry.Cutover)
		}
	}
	// KB-313 (ADR 307): the post-cutover gate — see DeprecationWarnings.
	// Fail closed here so the shipped CM cannot keep project-shaped run
	// config after its owning projects have all cut over; the intake logs
	// the same surfaces as warnings while the migration window is open.
	if stale := p.staleBindingShapes(); len(stale) > 0 && p.PostCutover() {
		return fmt.Errorf("post-cutover payload still carries global run-config surfaces the CM no longer owns (KB-313, ADR 307): %s", strings.Join(stale, "; "))
	}
	if len(p.Routes) == 0 {
		return errors.New("routes must not be empty")
	}
	seenRoutes := make(map[string]bool, len(p.Routes))
	for i, r := range p.Routes {
		if !labelRe.MatchString(r.Name) {
			return fmt.Errorf("routes[%d].name %q is not a DNS-1123 label", i, r.Name)
		}
		if seenRoutes[r.Name] {
			return fmt.Errorf("routes[%d].name %q is not unique", i, r.Name)
		}
		seenRoutes[r.Name] = true
		if _, ok := p.WorkflowLibrary[r.Workflow]; !ok {
			return fmt.Errorf("routes[%d] (%s): workflow %q is not present in workflowLibrary", i, r.Name, r.Workflow)
		}
		// KB-103: a route's configRef must resolve against the payload's
		// configLibrary — the same integrity rule the workflow ref has —
		// so payload errors surface when the ConfigMap is applied.
		if r.ConfigRef != "" {
			if _, ok := p.ConfigLibrary[r.ConfigRef]; !ok {
				return fmt.Errorf("routes[%d] (%s): configRef %q is not present in configLibrary", i, r.Name, r.ConfigRef)
			}
		}
		if r.Project == "" {
			return fmt.Errorf("routes[%d] (%s): project is required", i, r.Name)
		}
		// KB-311: when the payload carries a project registry, a route's
		// project must resolve through it — the registry is the only
		// ticket->project routing table, and a route selecting an
		// unregistered project would route nowhere.
		if len(p.ProjectRegistry) > 0 {
			if _, ok := p.ProjectRegistry[r.Project]; !ok {
				return fmt.Errorf("routes[%d] (%s): project %q is not present in projectRegistry", i, r.Name, r.Project)
			}
		}
		if len(r.States) == 0 {
			return fmt.Errorf("routes[%d] (%s): states must not be empty (omit the states key to default to [%s])", i, r.Name, DefaultState)
		}
		for _, s := range r.States {
			if s == "" {
				return fmt.Errorf("routes[%d] (%s): states must not contain empty names", i, r.Name)
			}
		}
		for _, tag := range r.Tags {
			if tag == "" {
				return fmt.Errorf("routes[%d] (%s): tags must not contain empty names", i, r.Name)
			}
		}
		switch r.TagMatch {
		case "", TagMatchAll, TagMatchAny:
		default:
			return fmt.Errorf("routes[%d] (%s): tagMatch must be %q or %q, got %q", i, r.Name, TagMatchAll, TagMatchAny, r.TagMatch)
		}
	}
	return nil
}

// Project resolves a Kanboard project name to its registry entry (KB-311,
// ADR 307): the registry is the ONLY ticket->project routing table, so the
// intake routes every ticket through this lookup (namespace + cutover
// state) before anything else decides. Missing registry entries return
// false and callers fail closed.
func (p *Payload) Project(name string) (ProjectEntry, bool) {
	entry, ok := p.ProjectRegistry[name]
	return entry, ok
}

// PostCutover reports whether the payload's migration window (KB-311,
// KB-313 — the #311 cutover flags) is closed: a non-empty projectRegistry
// in which every project is cutover-done. A payload without a registry
// predates the registry and stays in the warning window.
func (p *Payload) PostCutover() bool {
	if len(p.ProjectRegistry) == 0 {
		return false
	}
	for _, entry := range p.ProjectRegistry {
		if !entry.IsCutoverDone() {
			return false
		}
	}
	return true
}

// staleBindingShapes returns one "entry: keys" string per workflowLibrary
// entry still carrying a deprecated global run-config surface (KB-313,
// ADR 307), sorted by entry name for deterministic output.
func (p *Payload) staleBindingShapes() []string {
	var stale []string
	names := make([]string, 0, len(p.WorkflowLibrary))
	for name := range p.WorkflowLibrary {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		wf := p.WorkflowLibrary[name]
		var keys []string
		if len(wf.Volumes) > 0 {
			keys = append(keys, "volumes")
		}
		if len(wf.Secrets) > 0 {
			keys = append(keys, "secrets")
		}
		if len(wf.Env) > 0 {
			keys = append(keys, "env")
		}
		if len(wf.AdapterImages) > 0 {
			keys = append(keys, "adapterImages")
		}
		if len(keys) > 0 {
			stale = append(stale, fmt.Sprintf("workflowLibrary[%q]: %s", name, strings.Join(keys, ", ")))
		}
	}
	return stale
}

// DeprecationWarnings lists the deprecated global run-config surfaces the
// payload's workflowLibrary entries still carry (KB-313, ADR 307): the
// migration window is allow-with-warning, so the intake logs these instead
// of passing over them silently. Empty on a post-cutover payload — Validate
// hard-rejects the stale surfaces there, so a valid post-cutover payload
// never carries them.
func (p *Payload) DeprecationWarnings() []string {
	stale := p.staleBindingShapes()
	if len(stale) == 0 || p.PostCutover() {
		return nil
	}
	warnings := make([]string, 0, len(stale))
	for _, entry := range stale {
		warnings = append(warnings, entry+": deprecated global run-config surfaces (KB-313, ADR 307) — per-project bindings own run config; these reject once every projectRegistry entry is cutover-done")
	}
	return warnings
}

// validateWorkflow enforces the per-workflow constraints, including the
// ADR-0005 D1/D2 mode mapping: type=image means image-only (no url/ref,
// D2 fail-closed), type=url means the url is required (with or without an
// image).
func validateWorkflow(name string, wf Workflow) error {
	if !labelRe.MatchString(name) {
		return fmt.Errorf("name is not a DNS-1123 label")
	}
	switch wf.Type {
	case TypeImage:
		if wf.Image == "" {
			return fmt.Errorf("type=image requires an image")
		}
		if wf.URL != "" || wf.Ref != "" {
			// D2 fail-closed: an image-only run must not carry fetched
			// content or a content pin.
			return fmt.Errorf("type=image must not declare url or ref")
		}
		if wf.Interface != nil {
			// The interface is the tree's, not the image's: image mode is
			// the pre-KB-310 baked-tree path with no declared contract.
			return fmt.Errorf("type=image must not declare interface")
		}
	case TypeURL:
		if wf.URL == "" {
			return fmt.Errorf("type=url requires a url")
		}
		// KB-310: every shared-tree url workflow carries the
		// machine-readable interface the operator validates bindings
		// against. Fail closed on its absence rather than admitting an
		// undeclared contract.
		if wf.Interface == nil {
			return fmt.Errorf("type=url requires interface (KB-310)")
		}
		if err := validateInterface(wf.Interface); err != nil {
			return fmt.Errorf("interface: %w", err)
		}
	default:
		return fmt.Errorf("type must be %q or %q, got %q", TypeImage, TypeURL, wf.Type)
	}
	// Admission queue class (CRI-242): empty defaults to dev, so routes
	// without an explicit class keep the per-repo serialization they had
	// before classes existed. Anything else fails closed like tagMatch.
	switch wf.Class {
	case "", ClassDev, ClassTriage:
	default:
		return fmt.Errorf("class must be %q or %q, got %q", ClassDev, ClassTriage, wf.Class)
	}
	// Namespace is optional since KB-313 (ADR 307): cutover-done entries
	// route into their project's registry namespace and a namespace-less
	// entry's runs resolve the run's own namespace. A present value must
	// still be a DNS-1123 label.
	if wf.Namespace != "" && !labelRe.MatchString(wf.Namespace) {
		return fmt.Errorf("namespace %q is not a DNS-1123 label", wf.Namespace)
	}
	if err := validateEnvMap(wf.Env, "env"); err != nil {
		return err
	}
	if err := validateAdapterImages(wf.AdapterImages); err != nil {
		return err
	}
	seenVolumes := make(map[string]bool, len(wf.Volumes))
	for i, v := range wf.Volumes {
		if err := validateVolume(v); err != nil {
			return fmt.Errorf("volumes[%d] (%s): %w", i, v.Name, err)
		}
		if seenVolumes[v.Name] {
			return fmt.Errorf("volumes[%d] (%s): name is not unique within the workflow", i, v.Name)
		}
		seenVolumes[v.Name] = true
	}
	seenSecrets := make(map[string]bool, len(wf.Secrets))
	for i, s := range wf.Secrets {
		if err := validateSecret(s); err != nil {
			return fmt.Errorf("secrets[%d] (%s): %w", i, s.Name, err)
		}
		if seenSecrets[s.Name] {
			return fmt.Errorf("secrets[%d] (%s): name is not unique within the workflow", i, s.Name)
		}
		seenSecrets[s.Name] = true
	}
	return nil
}

// Interface declaration constants (KB-310): the declared apiVersion/kind
// of a tree interface, mirroring k8s/workflow-interface.schema.json.
const (
	// InterfaceAPIVersion is the interface object's declared apiVersion.
	InterfaceAPIVersion = "criteria.brokenbots.dev/v1"
	// InterfaceKind is the interface object's declared kind.
	InterfaceKind = "WorkflowInterface"
)

var (
	interfaceNameRe = mustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)
	// Trailing slash is rejected to match scripts/interface_validate.mjs
	// (byte-exact mount paths, no trailing-slash normalization).
	interfacePathRe = mustCompile(`^/.+[^/]$`)
	interfaceEnvRe  = mustCompile(`^[A-Z][A-Z0-9_]*$`)
	interfaceVarRe  = mustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	interfaceSlotRe = mustCompile(`^[a-z][a-zA-Z0-9_]*$`)
)

// validateInterface enforces the shape constraints of
// k8s/workflow-interface.schema.json on a tree's declared interface
// (KB-310): the declared apiVersion/kind, the tree name, mount shapes
// (pvc|tmp at an absolute, non-recursive path), env-name lanes, and
// config-slot names. Uniqueness per name set holds like volumes/secrets.
func validateInterface(iface *WorkflowInterface) error {
	if iface.APIVersion != InterfaceAPIVersion {
		return fmt.Errorf("apiVersion must be %q, got %q", InterfaceAPIVersion, iface.APIVersion)
	}
	if iface.Kind != InterfaceKind {
		return fmt.Errorf("kind must be %q, got %q", InterfaceKind, iface.Kind)
	}
	if iface.Workflow == "" || !interfaceNameRe.MatchString(iface.Workflow) {
		return fmt.Errorf("workflow (%q) is not a tree name", iface.Workflow)
	}
	for _, kind := range iface.Adapters {
		if !interfaceNameRe.MatchString(kind) {
			return fmt.Errorf("adapter kind %q is not a valid kind name", kind)
		}
	}
	if hasDuplicate(iface.Adapters) {
		return fmt.Errorf("adapter kinds are not unique")
	}
	seenMounts := make(map[string]bool, len(iface.Mounts))
	for i, m := range iface.Mounts {
		if !interfaceNameRe.MatchString(m.Name) {
			return fmt.Errorf("mounts[%d]: name %q is not a mount name", i, m.Name)
		}
		if seenMounts[m.Name] {
			return fmt.Errorf("mounts[%d] (%s): name is not unique", i, m.Name)
		}
		seenMounts[m.Name] = true
		if !interfacePathRe.MatchString(m.MountPath) {
			return fmt.Errorf("mounts[%d] (%s): mountPath %q must be an absolute path without trailing slash", i, m.Name, m.MountPath)
		}
		if m.Kind != "pvc" && m.Kind != "tmp" {
			return fmt.Errorf("mounts[%d] (%s): kind %q must be pvc or tmp", i, m.Name, m.Kind)
		}
	}
	if hasDuplicate(iface.SecretEnv) {
		return fmt.Errorf("secretEnv lanes are not unique")
	}
	for _, name := range iface.SecretEnv {
		if !interfaceEnvRe.MatchString(name) {
			return fmt.Errorf("secretEnv %q is not an env name (UPPER_SNAKE)", name)
		}
	}
	if hasDuplicate(iface.Env) {
		return fmt.Errorf("env names are not unique")
	}
	for _, name := range iface.Env {
		if !interfaceEnvRe.MatchString(name) {
			return fmt.Errorf("env %q is not an env name (UPPER_SNAKE)", name)
		}
	}
	seenSlots := make(map[string]bool, len(iface.ConfigSlots))
	for i, s := range iface.ConfigSlots {
		if !interfaceSlotRe.MatchString(s.Slot) {
			return fmt.Errorf("configSlots[%d]: slot %q is not a slot name (lowercase-first)", i, s.Slot)
		}
		if seenSlots[s.Slot] {
			return fmt.Errorf("configSlots[%d] (%s): slot is not unique", i, s.Slot)
		}
		seenSlots[s.Slot] = true
		wantVar, ok := v1.KB310ConfigSlotVariables[s.Slot]
		if !ok {
			return fmt.Errorf("configSlots[%d] (%s): slot is not a platform config key (declare only buildCmd, testCmd, ciGateCmd, or baseBranch — the keys a project binding can carry, KB-310)", i, s.Slot)
		}
		if s.Variable != wantVar {
			return fmt.Errorf("configSlots[%d] (%s): variable %q is not the slot's delivery variable %q (the KB-103 render delivers the value as the tree variable, KB-310)", i, s.Slot, s.Variable, wantVar)
		}
		if !interfaceVarRe.MatchString(s.Variable) {
			return fmt.Errorf("configSlots[%d] (%s): variable %q is not a tree variable", i, s.Slot, s.Variable)
		}
		if s.Target == "" {
			return fmt.Errorf("configSlots[%d] (%s): target is required", i, s.Slot)
		}
	}
	return nil
}

func hasDuplicate(names []string) bool {
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if seen[name] {
			return true
		}
		seen[name] = true
	}
	return false
}

// validateConfigEntry enforces the per-config constraints (KB-103): the
// name is a DNS-1123 label (it doubles as the pinned ConfigMap's name) and
// every declared command value is non-empty. Shell text legitimately
// contains whitespace, so values are only length-checked. KB-154: a
// declared baseBranch must be a git-ref-shaped name without whitespace,
// tags must not contain empty names, and tagMatch obeys the route's
// tagMatch vocabulary.
func validateConfigEntry(name string, entry ConfigEntry) error {
	if !labelRe.MatchString(name) {
		return fmt.Errorf("name is not a DNS-1123 label")
	}
	for field, value := range map[string]string{
		"buildCmd":   entry.BuildCmd,
		"testCmd":    entry.TestCmd,
		"ciGateCmd":  entry.CIGateCmd,
		"baseBranch": entry.BaseBranch,
	} {
		if value == "" {
			continue
		}
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s must not be blank when declared", field)
		}
	}
	if whitespaceRe.MatchString(entry.BaseBranch) {
		return fmt.Errorf("baseBranch %q must be a git ref name without whitespace", entry.BaseBranch)
	}
	for _, tag := range entry.Tags {
		if tag == "" {
			return fmt.Errorf("tags must not contain empty names")
		}
	}
	switch entry.TagMatch {
	case "", TagMatchAll, TagMatchAny:
	default:
		return fmt.Errorf("tagMatch must be %q or %q, got %q", TagMatchAll, TagMatchAny, entry.TagMatch)
	}
	return nil
}

// validateVolume enforces the pvc/nfs/tmp/k8s-secret shapes: a pvc carries
// a claim, an nfs carries server+path, tmp carries only a sizeLimit, and a
// k8s-secret carries the namespace Secret name; sibling-kind fields must
// stay unset.
func validateVolume(v Volume) error {
	if !labelRe.MatchString(v.Name) {
		return fmt.Errorf("name %q is not a DNS-1123 label", v.Name)
	}
	if !strings.HasPrefix(v.MountPath, "/") {
		return fmt.Errorf("mountPath %q must be absolute", v.MountPath)
	}
	if err := validateEnvMap(v.Env, "env"); err != nil {
		return err
	}
	switch v.Kind {
	case VolumePVC:
		if v.Claim == "" {
			return fmt.Errorf("kind=pvc requires a claim")
		}
		if v.Server != "" || v.Path != "" || v.SizeLimit != "" || v.SecretName != "" {
			return fmt.Errorf("kind=pvc must not declare server, path, sizeLimit, or secretName")
		}
	case VolumeNFS:
		if v.Server == "" || v.Path == "" {
			return fmt.Errorf("kind=nfs requires server and path")
		}
		if v.Claim != "" || v.SizeLimit != "" || v.SecretName != "" {
			return fmt.Errorf("kind=nfs must not declare claim, sizeLimit, or secretName")
		}
	case VolumeTmp:
		if v.Claim != "" || v.Server != "" || v.Path != "" || v.SecretName != "" {
			return fmt.Errorf("kind=tmp must not declare claim, server, path, or secretName")
		}
	case VolumeK8sSecret:
		if v.SecretName == "" {
			return fmt.Errorf("kind=k8s-secret requires a secretName")
		}
		if v.Claim != "" || v.Server != "" || v.Path != "" || v.SizeLimit != "" {
			return fmt.Errorf("kind=k8s-secret must not declare claim, server, path, or sizeLimit")
		}
	default:
		return fmt.Errorf("kind must be %q, %q, %q, or %q, got %q", VolumePVC, VolumeNFS, VolumeTmp, VolumeK8sSecret, v.Kind)
	}
	return nil
}

// validateSecret enforces name-only secret references: no credential
// material may appear in routes (plan CRI-214 section 3.8).
func validateSecret(s Secret) error {
	if !labelRe.MatchString(s.Name) {
		return fmt.Errorf("name %q is not a DNS-1123 label", s.Name)
	}
	if s.SecretProviderClass == "" {
		return fmt.Errorf("secretProviderClass is required")
	}
	if !strings.HasPrefix(s.MountPath, "/") {
		return fmt.Errorf("mountPath %q must be absolute", s.MountPath)
	}
	return validateSecretEnv(s.Env)
}

// validateEnvMap checks environment variable names and non-empty string
// values.
func validateEnvMap(env map[string]string, where string) error {
	for name, value := range env {
		if !envNameRe.MatchString(name) {
			return fmt.Errorf("%s has invalid env var name %q", where, name)
		}
		if value == "" {
			return fmt.Errorf("%s.%s must be a string value", where, name)
		}
	}
	return nil
}

// validateAdapterImages checks the workflow's per-kind adapter image
// override (CRI-214 M14): keys are adapter kinds (DNS-1123 labels, matching
// the kinds resolved from adapter_type), values are non-empty image
// references without whitespace.
func validateAdapterImages(images map[string]string) error {
	for kind, ref := range images {
		if !labelRe.MatchString(kind) {
			return fmt.Errorf("adapterImages has invalid adapter kind %q", kind)
		}
		if ref == "" || whitespaceRe.MatchString(ref) {
			return fmt.Errorf("adapterImages.%s must be a non-empty image reference without whitespace", kind)
		}
	}
	return nil
}

// The exported wrappers below reuse the routes payload rules for the
// per-project binding payload (KB-311, ADR 307): the binding's volumes,
// secrets, env, and adapter images validate against the same single
// source of truth.

// ValidateVolume exposes the routes volume rules to the binding package.
func ValidateVolume(v Volume) error { return validateVolume(v) }

// ValidateSecret exposes the routes secret rules to the binding package.
func ValidateSecret(s Secret) error { return validateSecret(s) }

// ValidateEnvMap exposes the env map rules to the binding package.
func ValidateEnvMap(env map[string]string, where string) error {
	return validateEnvMap(env, where)
}

// ValidateAdapterImages exposes the adapter image rules to the binding
// package.
func ValidateAdapterImages(images map[string]string) error {
	return validateAdapterImages(images)
}

// validateSecretEnv checks the secret's env mapping: env var names to key
// (rendered file) names within the SecretProviderClass mount. Values are
// names only, never secret values.
func validateSecretEnv(env map[string]string) error {
	for name, key := range env {
		if !envNameRe.MatchString(name) {
			return fmt.Errorf("env has invalid env var name %q", name)
		}
		if !secretKeyRe.MatchString(key) {
			return fmt.Errorf("env.%s value %q is not a valid key name", name, key)
		}
	}
	return nil
}

// ErrNoRoute reports that no route matches the ticket's project and state:
// the project is absent from the routes map, or no route covers the state.
// The watcher skips the ticket (no run, no comment).
var ErrNoRoute = errors.New("no route matches the ticket's project and state")

// ErrUnknownWorkflow reports that the resolved workflow name (project
// default or workflows-group label) is absent from workflowLibrary. Fail
// closed: no run, watcher log, ticket comment.
var ErrUnknownWorkflow = errors.New("workflow is not present in the routes workflowLibrary")

// ErrAmbiguousWorkflow reports several workflows-group labels on one ticket.
// Fail closed on the ambiguity: no run, watcher log, ticket comment.
var ErrAmbiguousWorkflow = errors.New("multiple workflow labels in the workflows label group")

// ErrUnknownConfig (KB-103) reports that a selected config name (a route's
// configRef or a configs-group label) is absent from the routes
// configLibrary. Fail closed: no run, watcher log, ticket comment.
var ErrUnknownConfig = errors.New("config is not present in the routes configLibrary")

// ErrAmbiguousConfig (KB-103) reports several configs-group labels on one
// ticket. Fail closed on the ambiguity: no run, watcher log, ticket
// comment.
var ErrAmbiguousConfig = errors.New("multiple config labels in the configs label group")

// TicketStates returns the deduplicated, sorted union of all routes'
// declared states: the workflow states any route can match (CRI-218).
// The watcher queries its ticket source for tickets in these states; the
// per-route selection itself happens in Resolve.
func (p *Payload) TicketStates() []string {
	seen := make(map[string]bool)
	states := make([]string, 0, len(p.Routes))
	for i := range p.Routes {
		for _, s := range p.Routes[i].States {
			if !seen[s] {
				seen[s] = true
				states = append(states, s)
			}
		}
	}
	slices.Sort(states)
	return states
}

// ProjectStates returns the deduplicated, sorted union of the routes'
// declared states for ONE project: the watcher queries its ticket source
// only for the columns its own project's routes declare, so other
// projects' column names never reach the query. TicketStates remains the
// whole-payload union for callers that genuinely want it.
func (p *Payload) ProjectStates(project string) []string {
	seen := make(map[string]bool)
	states := make([]string, 0)
	for i := range p.Routes {
		if p.Routes[i].Project != project {
			continue
		}
		for _, s := range p.Routes[i].States {
			if !seen[s] {
				seen[s] = true
				states = append(states, s)
			}
		}
	}
	slices.Sort(states)
	return states
}

// Selector carries the ticket-facing inputs to Resolve.
type Selector struct {
	// Project is the ticket's project name.
	Project string
	// State is the ticket's workflow state name.
	State string
	// Labels holds the ticket's label names.
	Labels []string
	// LabelGroups maps a label name to its label group's name (a grouped
	// label's parent; absent for ungrouped labels).
	LabelGroups map[string]string
	// WorkflowsLabelGroup is the label group whose labels name workflow
	// overrides ("workflows" per CRI-217).
	WorkflowsLabelGroup string
	// ConfigsLabelGroup (KB-103) is the label group whose labels name
	// config overrides (per-repo configLibrary entries). Only consulted
	// when the matched route carries no configRef: the explicit route
	// binding wins (same precedence the workflow override chain has
	// between tag-specific and tag-less routes).
	ConfigsLabelGroup string
}

// Selection is the resolved route plus the workflow it assigns.
type Selection struct {
	// Route is the matched route entry.
	Route Route
	// Name is the resolved workflow-library name (override or default).
	Name string
	// Workflow is the resolved workflow-library object.
	Workflow Workflow
	// ConfigName (KB-103) is the resolved configLibrary name: the matched
	// route's configRef when set, else the single configs-group label
	// name, else empty (no per-repo config — runs behave exactly as
	// before KB-103).
	ConfigName string
	// Config is the resolved configLibrary entry (zero value when no
	// config was selected).
	Config ConfigEntry
}

// Resolve applies the CRI-217 lookup rules: find the route for the ticket's
// project and state (tag-specific routes win over tag-less defaults), honor
// the project's default workflow, let a workflows-label-group label override
// it, and fail closed on missing or ambiguous workflows.
func (p *Payload) Resolve(sel Selector) (*Selection, error) {
	route, err := p.matchRoute(sel)
	if err != nil {
		return nil, err
	}

	name := route.Workflow
	override, err := workflowOverride(sel)
	if err != nil {
		return nil, err
	}
	if override != "" {
		name = override
	}

	wf, ok := p.WorkflowLibrary[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownWorkflow, name)
	}

	// KB-103 config selection: the matched route's configRef wins; absent
	// it, a single configs-group label names the config; more than one is
	// ambiguous and fails closed. The explicit route binding outranks the
	// label override (a route's configRef is deliberately placed config).
	// KB-154: a configs-group label resolves by entry name OR by the label
	// satisfying an entry's tag subset (the tag-group config overlay) —
	// the specificity ordering stays fail-closed on ambiguity.
	configName := route.ConfigRef
	if configName == "" {
		override, err := configOverride(p, sel)
		if err != nil {
			return nil, err
		}
		configName = override
	}
	var config ConfigEntry
	if configName != "" {
		var ok bool
		if config, ok = p.ConfigLibrary[configName]; !ok {
			return nil, fmt.Errorf("%w: %q", ErrUnknownConfig, configName)
		}
	}

	return &Selection{
		Route:      *route,
		Name:       name,
		Workflow:   wf,
		ConfigName: configName,
		Config:     config,
	}, nil
}

// configOverride returns the config-library entry the ticket's label in
// the configured configs label group selects, or "" when the ticket
// carries none. KB-154 tag-group config: the label resolves by entry name
// first (KB-103), else by satisfying an entry's tag subset — a ticket tag
// like `v0.6.0` selects the entry that declares it. An entry that carries
// no tags is name-selectable only, exactly as before KB-154.
//
// Every configs-group label is resolved independently and each must select
// exactly one entry: an ambiguous selection (two names, or one tag
// satisfied by several entries — map iteration order would otherwise pick
// one arbitrarily) and a selection naming no entry both fail closed. The
// single-overlay-selection invariant keeps the overlay from becoming a
// second routing surface: multiple ticket tags in the configs group are a
// wiring error, not a preference list.
func configOverride(p *Payload, sel Selector) (string, error) {
	if sel.ConfigsLabelGroup == "" {
		return "", nil
	}
	var overrides []string
	for _, label := range sel.Labels {
		if sel.LabelGroups[label] == sel.ConfigsLabelGroup {
			overrides = append(overrides, label)
		}
	}
	switch len(overrides) {
	case 0:
		return "", nil
	case 1:
		return p.configNameForLabel(overrides[0])
	default:
		return "", fmt.Errorf("%w: %s", ErrAmbiguousConfig, strings.Join(overrides, ", "))
	}
}

// configNameForLabel resolves one configs-group label to its configLibrary
// entry name: the label naming the entry (KB-103) wins, else the label
// satisfying an entry's tag subset (KB-154 tag-group overlay). An entry
// that carries no tags is name-selectable only. No entry matches is
// ErrUnknownConfig and several tag-subset matches is ErrAmbiguousConfig —
// both fail closed: map iteration order must never decide the selection.
func (p *Payload) configNameForLabel(label string) (string, error) {
	if _, ok := p.ConfigLibrary[label]; ok {
		return label, nil
	}
	matches := make([]string, 0, 2)
	for name, entry := range p.ConfigLibrary {
		if entryTagsSatisfied(&entry, label) {
			matches = append(matches, name)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("%w: %q", ErrUnknownConfig, label)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("%w: %s", ErrAmbiguousConfig, strings.Join(matches, ", "))
	}
}

// entryTagsSatisfied applies an entry's tagMatch semantics to a single
// ticket tag: "all" (default) requires the entry to declare exactly the
// given tag, "any" requires the ticket tag to be among the entry's
// declared tags.
func entryTagsSatisfied(entry *ConfigEntry, tag string) bool {
	if entry.TagMatch == TagMatchAny {
		return slices.Contains(entry.Tags, tag)
	}
	// "all" (the schema default): one label satisfies the subset only
	// when the subset is exactly that one tag.
	return len(entry.Tags) == 1 && entry.Tags[0] == tag
}

// ConfigsTagGroupMember reports whether a ticket tag belongs in the
// configs label group (KB-154): the tag names a configLibrary entry, or
// satisfies the tag subset of some entry. Kanboard has flat tags — the
// watcher consults this per tag to impute which tag group the tag is a
// member of, so a ticket tag like v0.6.0 selects the tag-group config
// instead of failing closed as a phantom workflow override. When the
// subset is satisfied by several entries the tag is still a member: the
// resolver's configs-group selection then fails closed as
// ErrAmbiguousConfig, and a matched route carrying a configRef (KB-103
// precedence) never consults the overlay at all.
func (p *Payload) ConfigsTagGroupMember(tag string) bool {
	if _, ok := p.ConfigLibrary[tag]; ok {
		return true
	}
	for _, entry := range p.ConfigLibrary {
		if entryTagsSatisfied(&entry, tag) {
			return true
		}
	}
	return false
}

// matchRoute returns the first route matching the ticket's project and
// state; among matches, routes whose tag subset is satisfied take priority
// over tag-less routes (most specific first), otherwise document order
// decides.
func (p *Payload) matchRoute(sel Selector) (*Route, error) {
	specific, general := (*Route)(nil), (*Route)(nil)
	for i := range p.Routes {
		r := &p.Routes[i]
		if r.Project != sel.Project || !slices.Contains(r.States, sel.State) {
			continue
		}
		if len(r.Tags) == 0 {
			if general == nil {
				general = r
			}
			continue
		}
		if tagsSatisfied(r, sel.Labels) && specific == nil {
			specific = r
		}
	}
	if specific != nil {
		return specific, nil
	}
	if general != nil {
		return general, nil
	}
	return nil, fmt.Errorf("%w (project %q, state %q)", ErrNoRoute, sel.Project, sel.State)
}

// workflowOverride returns the workflow name carried by the ticket's label
// in the configured workflows label group, or "" when the ticket carries
// none. More than one such label is ambiguous and fails closed.
func workflowOverride(sel Selector) (string, error) {
	var overrides []string
	for _, label := range sel.Labels {
		if sel.LabelGroups[label] == sel.WorkflowsLabelGroup {
			overrides = append(overrides, label)
		}
	}
	switch len(overrides) {
	case 0:
		return "", nil
	case 1:
		return overrides[0], nil
	default:
		return "", fmt.Errorf("%w: %s", ErrAmbiguousWorkflow, strings.Join(overrides, ", "))
	}
}

// tagsSatisfied applies the route's tagMatch semantics: "all" (default)
// requires every listed tag, "any" requires at least one.
func tagsSatisfied(r *Route, labels []string) bool {
	if r.TagMatch == TagMatchAny {
		return slices.ContainsFunc(r.Tags, func(tag string) bool {
			return slices.Contains(labels, tag)
		})
	}
	// "all" (the schema default).
	for _, tag := range r.Tags {
		if !slices.Contains(labels, tag) {
			return false
		}
	}
	return true
}
