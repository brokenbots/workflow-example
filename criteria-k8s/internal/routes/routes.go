// Package routes reads and resolves the Criteria routes ConfigMap
// (CRI-216/217), the operator configuration of record assigning Linear
// tickets to workflow-library objects. The payload of record is
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

// DefaultState is the Linear workflow state a route triggers on when it
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
	APIVersion      string              `json:"apiVersion"`
	Kind            string              `json:"kind"`
	WorkflowLibrary map[string]Workflow `json:"workflowLibrary"`
	Routes          []Route             `json:"routes"`
}

// Workflow is a workflow-library object: how a run obtains its workflow and
// where/how it executes.
type Workflow struct {
	Type      string `json:"type"`
	Namespace string `json:"namespace"`
	// Class is the admission queue class (CRI-242): "dev" serializes per
	// repoURL, "triage" (read-only against the repo) admits concurrently.
	// Empty defaults to ClassDefault at stamping; Validate rejects any
	// other value.
	Class   string            `json:"class,omitempty"`
	Image   string            `json:"image,omitempty"`
	URL     string            `json:"url,omitempty"`
	Ref     string            `json:"ref,omitempty"`
	Volumes []Volume          `json:"volumes,omitempty"`
	Secrets []Secret          `json:"secrets,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	// AdapterImages is the per-adapter-kind image override (CRI-214 M14):
	// adapter kind ("shell", "copilot", ...) to a full image reference the
	// operator stamps on this workflow's adapter pods, ahead of the event's
	// image_reference and the configured registry/tag defaults.
	AdapterImages map[string]string `json:"adapterImages,omitempty"`
}

// Volume is a storage volume of kind pvc, nfs, or tmp.
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

// Route assigns Linear tickets to a workflow-library object by project, tag
// subset, and Linear workflow states.
type Route struct {
	Name     string   `json:"name"`
	Workflow string   `json:"workflow"`
	Project  string   `json:"project"`
	Tags     []string `json:"tags,omitempty"`
	TagMatch string   `json:"tagMatch,omitempty"`
	// States holds the Linear workflow state names this route triggers on.
	// Omitted in the payload defaults to [DefaultState] (CRI-218); an
	// explicit empty or null list is rejected by Validate, mirroring the
	// schema of record (minItems: 1).
	States []string `json:"states"`
}

var (
	labelRe      = mustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	envNameRe    = mustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	secretKeyRe  = mustCompile(`^[A-Za-z0-9._-]+$`)
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
		if r.Project == "" {
			return fmt.Errorf("routes[%d] (%s): project is required", i, r.Name)
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
	case TypeURL:
		if wf.URL == "" {
			return fmt.Errorf("type=url requires a url")
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
	if !labelRe.MatchString(wf.Namespace) {
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
// closed: no run, watcher log, Linear comment.
var ErrUnknownWorkflow = errors.New("workflow is not present in the routes workflowLibrary")

// ErrAmbiguousWorkflow reports several workflows-group labels on one ticket.
// Fail closed on the ambiguity: no run, watcher log, Linear comment.
var ErrAmbiguousWorkflow = errors.New("multiple workflow labels in the workflows label group")

// TicketStates returns the deduplicated, sorted union of all routes'
// declared states: the Linear workflow state names any route can match
// (CRI-218). The watcher queries Linear for tickets in these states; the
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
// declared states for ONE project. The dual-source watchers share one
// routes payload: the linear watcher must query Linear only for the states
// its own project's routes declare (the kanboard columns Backlog/Ready are
// not Linear state names), and likewise the kanboard watcher for its
// columns. TicketStates remains the whole-payload union for callers that
// genuinely want it.
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
	// Project is the ticket's Linear project name.
	Project string
	// State is the ticket's Linear workflow state name.
	State string
	// Labels holds the ticket's Linear label names.
	Labels []string
	// LabelGroups maps a label name to its label group's name (Linear
	// models label groups as parent labels; absent for ungrouped labels).
	LabelGroups map[string]string
	// WorkflowsLabelGroup is the label group whose labels name workflow
	// overrides ("workflows" per CRI-217).
	WorkflowsLabelGroup string
}

// Selection is the resolved route plus the workflow it assigns.
type Selection struct {
	// Route is the matched route entry.
	Route Route
	// Name is the resolved workflow-library name (override or default).
	Name string
	// Workflow is the resolved workflow-library object.
	Workflow Workflow
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
	return &Selection{Route: *route, Name: name, Workflow: wf}, nil
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
