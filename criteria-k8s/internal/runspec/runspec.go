// Package runspec is the shared stamping path that turns one resolved run
// decision (ticket, repo, routes selection, cutover state) into a
// CriteriaRun. Both decision lanes — the kanboard watcher stamping live
// runs, and the operator's shadow-run mirror building an assignment-driven
// would-be run in memory — stamp through THIS code, so an assignment-driven
// CR and a watcher-driven CR for the same ticket are identical by
// construction (KB-319 deliverable: "assignment-driven CR identical to
// watcher-driven CR"), and any residual divergence comes from the two
// lanes' INPUTS (defaults/env/routes), not the stamping.
//
// The package is pure: it never talks to an API server and never writes
// anything. The only side effects live behind the caller-supplied pin
// resolver (the KB-103 ConfigMap consult), which the watcher wires to its
// own reads/log/card-comments and the operator wires to its own read/log.
package runspec

import (
	"strings"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/routes"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/runstamp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CR metadata labels both lanes stamp. The operator's would-be run mirrors
// the watcher's provenance values on purpose: the shadow CR is never
// written, so its labels exist only to be compared like-for-like against
// the watcher's (an actual creator stamp would have diverged by design).
const (
	// LabelTicket is the CR's ticket label ("ticket" — buildCriteriaRun's stamp).
	LabelTicket = "ticket"
	// LabelManagedBy is the CR's managed-by label key.
	LabelManagedBy = "app.kubernetes.io/managed-by"
	// LabelSource is the CR's source label key.
	LabelSource = "criteria.brokenbots.dev/source"
	// ManagedBy stamps which lane produced the run (both decision lanes
	// stamp the watcher's value; see the comment on StampRun's labels).
	ManagedBy = "criteria-kanboard-watcher"
	// Source stamps the decision provenance ("kanboard").
	Source = "kanboard"
)

// Defaults is the shared run-shape default set both lanes stamp (KB-319):
// the operator's shadow-run mirror reads the same env keys the watcher's
// flags resolve (BUILD_CMD/TEST_CMD/CI_GATE_CMD/BASE_BRANCH/
// MAX_AGENT_VISITS/PROVIDER_BASE_URL), so real deployment drift between
// the lanes surfaces as decision divergence, not silent agreement.
type Defaults struct {
	BuildCmd        string
	TestCmd         string
	CIGateCmd       string
	BaseBranch      string
	MaxAgentVisits  int
	ProviderBaseURL string
}

// CriteriaRunConfigRefProvider resolves the KB-103 ConfigMap pin for a
// selection in a namespace. It returns the ref (name-only when the
// ConfigMap does not exist yet) and an error for any other read failure —
// the caller has already handled its side effects for the error case
// (logging, card comments): a pin failure keeps the name-only pin (KB-250
// no-silent-degrade) and never gates the fire.
type CriteriaRunConfigRefProvider func(ns string, sel *routes.Selection) (*criteriav1.CriteriaRunConfigRef, error)

// StampInput is one resolved run decision: everything a stamping lane knows
// before touching a ConfigMap. Selection carries the workflow tree, its
// resolved library name, and the KB-319 conflict policy the resolver
// selected; the cutover fields (KB-311, ADR 307) switch the stamping shape.
type StampInput struct {
	// Name is the CR's full name (including the lane's run-name timestamp:
	// "<lower(ticket)>-<unix>"). Each lane stamps its own timestamp seam.
	Name string
	// TicketID is the intake ticket identifier (KB-<id>).
	TicketID string
	// RepoURL is the repository the run clones and works on.
	RepoURL string
	// Selection is the resolved route/workflow/config/policy.
	Selection *routes.Selection
	// Defaults is the lane's shared run-shape default set.
	Defaults Defaults
	// Namespace is the pre-cutover (workflow-first) CR's landing namespace —
	// the watcher's watch namespace.
	Namespace string
	// CutoverNamespace switches to the binding-first shape when non-empty:
	// the CR lands in the project's registry namespace and carries
	// binding references only (KB-311). Empty keeps the pre-cutover shape.
	CutoverNamespace string
	// BindingRefName is the cutover-done project's binding ConfigMap pin
	// (internal/binding.CMName); binding-first shape only.
	BindingRefName string
	// BindingResourceVersion pins the binding ConfigMap read at stamp
	// time; empty is a name-only pin (read failure or not-read-yet).
	BindingResourceVersion string
	// ResolveConfigPin consults the KB-103 pinned per-repo ConfigMap.
	// Consulted ONLY on the pre-cutover shape and only when the selection
	// carries a config slot: a nil provider degrades to the pin-less
	// stamp (the slot's ENTRY overrides still land — the stamp is
	// decoupled from the consult — but spec.configRef stays unset).
	ResolveConfigPin CriteriaRunConfigRefProvider
}

// StampRun stamps a complete (in-memory, never created) CriteriaRun from
// the input decision. Field order and stamping order mirror the watcher's
// original buildCriteriaRun/buildBindingFirstRun exactly — this body IS
// that logic, moved here so both decision lanes share it (KB-319 rem1).
func StampRun(in StampInput) *criteriav1.CriteriaRun {
	binding := in.CutoverNamespace != ""
	spec := criteriav1.CriteriaRunSpec{
		TicketID:         in.TicketID,
		RepoURL:          in.RepoURL,
		PerScopeSessions: true,
		// MaxAgentVisits and ProviderBaseURL stamp on BOTH shapes: they are
		// lane-level contract flags, not KB-103 per-repo config fields, so
		// the binding-first lane keeps stamping them.
		MaxAgentVisits:  in.Defaults.MaxAgentVisits,
		ProviderBaseURL: in.Defaults.ProviderBaseURL,
	}
	if binding {
		// KB-311, ADR 307: cutover-done projects execute through their
		// binding ConfigMap, so the KB-103 spec-field stamp is skipped (it
		// would collide with binding-owned config at admission) and the CR
		// carries binding references only.
		spec.Workflow = StampBindingRefWorkflowSpec(in.Selection.Name, in.Selection.Workflow, in.Selection.ConflictPolicy)
		if in.BindingRefName != "" {
			spec.ProjectBindingRef = &criteriav1.CriteriaRunProjectBindingRef{
				Name:            in.BindingRefName,
				ResourceVersion: in.BindingResourceVersion,
			}
		}
	} else {
		spec.Workflow = StampWorkflowSpec(in.Selection.Name, in.Selection.Workflow, in.Selection.ConflictPolicy)
		spec.BuildCmd = in.Defaults.BuildCmd
		spec.TestCmd = in.Defaults.TestCmd
		spec.CIGateCmd = in.Defaults.CIGateCmd
		spec.BaseBranch = in.Defaults.BaseBranch
		// KB-103: per-repo config outranks the lane-level fallback flags
		// entry-by-entry, and the stamp is DECOUPLED from the pin consult:
		// a selection naming a config slot stamps the ENTRY's values
		// whenever it resolves (KB-338 — a missing pinner must never
		// silently reduce the lane to its fallback flags), while
		// spec.configRef stamps only when a provider resolved the
		// ConfigMap pin.
		if in.Selection.ConfigName != "" {
			var cfgRef *criteriav1.CriteriaRunConfigRef
			if in.ResolveConfigPin != nil {
				cfgRef, _ = in.ResolveConfigPin(ExecNamespace(in.Namespace, in.Selection), in.Selection)
			}
			runstamp.StampRepoConfig(&spec, in.Selection, cfgRef)
		}
	}
	runstamp.StampWorkflowSource(&spec, in.Selection.Workflow)
	return &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      in.Name,
			Namespace: landingNS(in.CutoverNamespace, in.Namespace),
			Labels: map[string]string{
				LabelTicket:    strings.ToLower(in.TicketID),
				LabelManagedBy: ManagedBy,
				LabelSource:    Source,
			},
		},
		Spec: spec,
	}
}

func landingNS(bindingNS, ns string) string {
	if bindingNS != "" {
		return bindingNS
	}
	return ns
}

// ExecNamespace resolves the namespace a selection's per-repo ConfigMap
// lives in: the workflow's declared execution namespace when set, else the
// lane's own namespace.
func ExecNamespace(defaultNS string, sel *routes.Selection) string {
	if sel != nil && sel.Workflow.Namespace != "" {
		return sel.Workflow.Namespace
	}
	return defaultNS
}

// StampWorkflowSpec deep-copies a resolved routes workflow object into the
// CriteriaRun spec (CRI-222). conflictPolicy is the KB-319 adapter-image
// conflict policy the resolver selected for the ticket's project
// (Selection.ConflictPolicy).
func StampWorkflowSpec(name string, wf routes.Workflow, conflictPolicy string) *criteriav1.RunWorkflow {
	out := &criteriav1.RunWorkflow{
		Name:      name,
		Type:      wf.Type,
		Namespace: wf.Namespace,
		Class:     StampClass(wf),
		Image:     wf.Image,
		URL:       wf.URL,
		Ref:       wf.Ref,
		Interface: StampWorkflowInterface(wf.Interface),
		// The conflict policy is STAMPED AS-SELECTED, unconditionally: the
		// conflict vocabulary is non-empty by construction (the resolver
		// always fills it, fail-closed included), so an empty value would
		// only be a silent skip of the KB-319 consult.
		ImageConflictPolicy: conflictPolicy,
	}
	if wf.Env != nil {
		out.Env = make(map[string]string, len(wf.Env))
		for k, v := range wf.Env {
			out.Env[k] = v
		}
	}
	if wf.AdapterImages != nil {
		out.AdapterImages = make(map[string]string, len(wf.AdapterImages))
		for k, v := range wf.AdapterImages {
			out.AdapterImages[k] = v
		}
	}
	if len(wf.Volumes) > 0 {
		out.Volumes = make([]criteriav1.RunWorkflowVolume, 0, len(wf.Volumes))
		for _, v := range wf.Volumes {
			out.Volumes = append(out.Volumes, criteriav1.RunWorkflowVolume{
				Name:       v.Name,
				Kind:       v.Kind,
				MountPath:  v.MountPath,
				SubPath:    v.SubPath,
				ReadOnly:   v.ReadOnly,
				Claim:      v.Claim,
				Server:     v.Server,
				Path:       v.Path,
				SizeLimit:  v.SizeLimit,
				SecretName: v.SecretName,
				Env:        copyStringMap(v.Env),
			})
		}
	}
	if len(wf.Secrets) > 0 {
		out.Secrets = make([]criteriav1.RunWorkflowSecret, 0, len(wf.Secrets))
		for _, s := range wf.Secrets {
			out.Secrets = append(out.Secrets, criteriav1.RunWorkflowSecret{
				Name:                s.Name,
				SecretProviderClass: s.SecretProviderClass,
				MountPath:           s.MountPath,
				Env:                 copyStringMap(s.Env),
			})
		}
	}
	return out
}

// StampWorkflowInterface deep-copies a routes interface declaration
// (KB-310): the operator validates the project binding against it before
// job creation and records its checksum in the launch manifest.
func StampWorkflowInterface(in *routes.WorkflowInterface) *criteriav1.RunWorkflowInterface {
	if in == nil {
		return nil
	}
	return &criteriav1.RunWorkflowInterface{
		APIVersion:  in.APIVersion,
		Kind:        in.Kind,
		Workflow:    in.Workflow,
		Description: in.Description,
		Adapters:    copyStringSlice(in.Adapters),
		Mounts:      copyInterfaceMounts(in.Mounts),
		SecretEnv:   copyStringSlice(in.SecretEnv),
		Env:         copyStringSlice(in.Env),
		ConfigSlots: copyInterfaceConfigSlots(in.ConfigSlots),
		// KB-319: the declared admission-lock key rides the stamped
		// interface so the operator's derivation reads the tree's
		// declaration, not a default.
		LockKey: in.LockKey,
	}
}

// StampBindingRefWorkflowSpec stamps the workflow TREE references onto a
// cutover-done project's CriteriaRun (KB-311, ADR 307): name, type, url+ref,
// class, and the interface declaration (the secrets lane lives there).
//
// Everything the ADR hands to the project binding stays OFF the CR — env,
// adapter images, volumes, secrets — so the CR carries binding REFERENCES
// only and the project's criteria-binding CM owns the execution config.
// The workflow declaration's namespace is deliberately not stamped: the
// run executes in the project namespace, so jobbuilder.TargetNamespace
// must resolve through the run's own namespace, not the global
// workflow-library one. conflictPolicy is the KB-319 adapter-image
// conflict policy (Selection.ConflictPolicy); the binding materialization
// replaces the adapter-image lane post-cutover, but the policy governs
// the pinned-vs-verified disagreement the same way on both lanes.
func StampBindingRefWorkflowSpec(name string, wf routes.Workflow, conflictPolicy string) *criteriav1.RunWorkflow {
	return &criteriav1.RunWorkflow{
		Name:                name,
		Type:                wf.Type,
		Class:               StampClass(wf),
		Image:               wf.Image,
		URL:                 wf.URL,
		Ref:                 wf.Ref,
		Interface:           StampWorkflowInterface(wf.Interface),
		ImageConflictPolicy: conflictPolicy,
	}
}

// StampClass resolves the admission queue class (CRI-242): an omitted
// class defaults to dev.
func StampClass(wf routes.Workflow) string {
	if wf.Class == routes.ClassTriage {
		return routes.ClassTriage
	}
	return routes.ClassDefault
}

func copyStringMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func copyStringSlice(names []string) []string {
	if names == nil {
		return nil
	}
	out := make([]string, len(names))
	copy(out, names)
	return out
}

func copyInterfaceMounts(mounts []routes.WorkflowInterfaceMount) []criteriav1.RunWorkflowInterfaceMount {
	if mounts == nil {
		return nil
	}
	out := make([]criteriav1.RunWorkflowInterfaceMount, len(mounts))
	for i, m := range mounts {
		out[i] = criteriav1.RunWorkflowInterfaceMount{
			Name:      m.Name,
			MountPath: m.MountPath,
			Kind:      m.Kind,
		}
	}
	return out
}

func copyInterfaceConfigSlots(slots []routes.WorkflowInterfaceConfigSlot) []criteriav1.RunWorkflowInterfaceConfigSlot {
	if slots == nil {
		return nil
	}
	out := make([]criteriav1.RunWorkflowInterfaceConfigSlot, len(slots))
	for i, s := range slots {
		out[i] = criteriav1.RunWorkflowInterfaceConfigSlot{
			Slot:        s.Slot,
			Variable:    s.Variable,
			Target:      s.Target,
			Description: s.Description,
		}
	}
	return out
}
