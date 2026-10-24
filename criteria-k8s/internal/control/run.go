package control

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/binding"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/routes"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/runspec"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// RunBuilder is the assignment lane's spec-level mirror: it derives the
// would-be CriteriaRun an assignment represents, through the SAME shared
// stamping path the watcher stamps its live runs with
// (internal/runspec.StampRun — reviewer blocker 1's "existing translation
// path"), and compares it field-for-field against the watcher's CR.
//
// Observational principle preserved (CRI-60 / KB-319): BuildRun never
// creates, admits, or leases anything — the CR stays in memory and the
// builder performs no API writes. The only reads are the pin consults the
// watcher's shape also performs (the KB-311 binding pin; the KB-103 config
// consult is skipped because the assignment's label vocabulary carries no
// config selector — a documented divergence source compareSpec reports).
//
// A nil Routes keeps the builder in decision-only degrade: BuildRun fails
// with a reason instead of guessing a spec — which is why wiring callers
// gate on Configured and skip wiring the builder entirely on the
// no-routes deployment posture (a missing routes file is a deployment
// posture, not a lane disagreement; the degrade reason must never reach
// the divergence log).
type RunBuilder struct {
	// Defaults are the run-shape defaults both lanes stamp
	// (runspec.Defaults).
	Defaults runspec.Defaults
	// Namespace is the pre-cutover landing namespace (the operator's
	// CRITERIA_NAMESPACE — the watcher's watch namespace).
	Namespace string
	// Routes is the routes payload resolved from --shadow-routes-file.
	Routes *routes.Payload
	// Client reads the cutover-done project's binding ConfigMap for the
	// name-only-vs-pinned project binding decision. nil is tolerated
	// (name-only pin, mirroring the watcher's unread-binding degrade).
	Client client.Client
	// Log receives the pin-read failures (no secret material).
	Log logr.Logger
}

// Configured reports whether the builder can actually derive a spec: a
// routes payload is loaded. Wiring callers (the operator main, the
// watchdog) treat an unconfigured builder as "spec mirror off" — the
// decision-granularity comparison stays, and BuildRun's not-configured
// degrade reason never enters any divergence set.
func (b *RunBuilder) Configured() bool {
	return b != nil && b.Routes != nil
}

// BuildRun derives the assignment's would-be CriteriaRun in memory.
// Returns (run, nil) on success and (nil, reasons) when the assignment
// cannot build a spec: the reasons are divergence-log fodder the watchdog
// appends. The run's name uses the watcher's "<lower(ticket)>-<unix>"
// seam (mirrored), so the like-for-like compare in CompareSpec skips the
// name; namespace, labels, and the full spec are compared. The reasons
// come from the exported CompareSpec (kept separately so callers' parity
// tests can drive both halves).
func (b *RunBuilder) BuildRun(ctx context.Context, d Decision) (*criteriav1.CriteriaRun, []string) {
	if b == nil || b.Routes == nil {
		return nil, []string{"routes payload not configured: spec-level comparison unavailable"}
	}
	var reasons []string
	if d.Ticket == "" {
		reasons = append(reasons, "assignment-labels missing: ticket (cannot build a spec)")
	}
	if d.RepoURL == "" {
		reasons = append(reasons, "assignment-labels missing: "+DefaultLabelRepoURL)
	}
	sel, selReasons := b.selection(d)
	if len(selReasons) > 0 {
		return nil, append(reasons, selReasons...)
	}
	if len(reasons) > 0 {
		return nil, reasons
	}
	return b.stamp(ctx, d, sel), nil
}

// selection resolves the routes workflow the assignment names. The
// dispatch lane queued an already-resolved workflow-library name, so the
// lookup is a like-for-like library resolution, not a re-route; the
// conflict policy still resolves through routes
// (EffectiveImageConflictPolicy), the same resolver the watcher's selection
// stamps.
func (b *RunBuilder) selection(d Decision) (*routes.Selection, []string) {
	wf, ok := b.Routes.WorkflowLibrary[d.WorkflowName]
	if !ok {
		return nil, []string{fmt.Sprintf(
			"spec.workflow.name: assignment names unknown workflow %q (absent from the routes workflow library)", d.WorkflowName)}
	}
	return &routes.Selection{
		Name:     d.WorkflowName,
		Workflow: wf,
		// The assignment carries no config selector (no configRef/tag
		// vocabulary in the queue's required labels), so ConfigName and
		// Config stay empty: when the watcher's route resolves per-repo
		// config, compareSpec reports the spec.configRef divergence. This
		// is the known, deliberately-documented divergence source of the
		// shadow window.
		ConfigName:     "",
		Config:         routes.ConfigEntry{},
		ConflictPolicy: b.Routes.EffectiveImageConflictPolicy(wf, d.Project),
	}, nil
}

// stamp mirrors the watcher's buildCriteriaRun lane wiring onto the
// builder's own inputs (defaults namespace, binding pin read) and stamps
// through the shared path.
func (b *RunBuilder) stamp(ctx context.Context, d Decision, sel *routes.Selection) *criteriav1.CriteriaRun {
	in := runspec.StampInput{
		Name:      fmt.Sprintf("%s-%d", strings.ToLower(d.Ticket), nowUnix()),
		TicketID:  d.Ticket,
		RepoURL:   d.RepoURL,
		Selection: sel,
		Defaults:  b.Defaults,
		Namespace: b.Namespace,
	}
	if entry, cutover := b.Routes.Project(d.Project); cutover && entry.IsCutoverDone() {
		in.CutoverNamespace = entry.Namespace
		in.BindingRefName = binding.CMName
		in.BindingResourceVersion = b.bindingPin(ctx, entry.Namespace)
		return runspec.StampRun(in)
	}
	// The KB-103 config consult is skipped: the selection carries no
	// config entry (see selection). ResolveConfigPin stays nil — the
	// documented no-pin-degrade path.
	return runspec.StampRun(in)
}

// bindingPin reads the cutover-done project's binding ConfigMap so the
// would-be run's project-binding pin mirrors the watcher's (KB-250): a
// missing ConfigMap or no client degrades to a name-only pin, any other
// read failure logs and degrades the same way — a divergence, not a crash.
func (b *RunBuilder) bindingPin(ctx context.Context, ns string) string {
	if b.Client == nil {
		return ""
	}
	var cm corev1.ConfigMap
	if err := b.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: binding.CMName}, &cm); err != nil {
		b.Log.Error(err, "binding ConfigMap read failed for the assignment mirror; name-only project binding pin",
			"namespace", ns, "configMap", binding.CMName)
		return ""
	}
	return cm.ResourceVersion
}

// nowUnix mirrors the watcher's run-name timestamp seam (the watcher
// defines its own nowUnix; here it is re-declared so the mirrored name
// format stays identical without importing the watcher binary).
var nowUnix = func() int64 { return time.Now().Unix() }

// CompareSpec field-for-field compares the would-be assignment-driven
// CriteriaRun against the watcher's stamped CR for the same ticket (the
// acceptance criterion: "assignment-driven CR identical to watcher-driven
// CR"). Every divergence reason is prefixed "spec." (or "labels." /
// "meta." for the out-of-spec strips the queue admission does not read) so
// the divergence log stays scrapeable. The CR name is NOT compared: it is
// each lane's timestamp seam. A final full-spec DeepEqual catch-all
// reports differences the curated list does not curate yet.
func CompareSpec(want, got *criteriav1.CriteriaRun) []string {
	var reasons []string
	add := func(format string, args ...any) {
		reasons = append(reasons, fmt.Sprintf(format, args...))
	}
	if want.Namespace != got.Namespace {
		add("meta.namespace: assignment=%s watcher=%s", want.Namespace, got.Namespace)
	}
	addLabelDiffs(&reasons, want.Labels, got.Labels)
	s1, s2 := want.Spec, got.Spec

	if s1.TicketID != s2.TicketID && !strings.EqualFold(s1.TicketID, s2.TicketID) {
		// Ticket casing is each lane's presentation seam (the watcher
		// stamps the kanboard identifier, the assignment carries the
		// queue's label); a case-insensitive ticket identity is a match.
		add("spec.ticketID: assignment=%q watcher=%q", s1.TicketID, s2.TicketID)
	}
	if s1.RepoURL != s2.RepoURL {
		add("spec.repoURL: assignment=%q watcher=%q", s1.RepoURL, s2.RepoURL)
	}
	if s1.PerScopeSessions != s2.PerScopeSessions {
		add("spec.perScopeSessions: assignment=%t watcher=%t", s1.PerScopeSessions, s2.PerScopeSessions)
	}
	if s1.MaxAgentVisits != s2.MaxAgentVisits {
		add("spec.maxAgentVisits: assignment=%d watcher=%d", s1.MaxAgentVisits, s2.MaxAgentVisits)
	}
	if s1.ProviderBaseURL != s2.ProviderBaseURL {
		add("spec.providerBaseURL: assignment=%q watcher=%q", s1.ProviderBaseURL, s2.ProviderBaseURL)
	}
	if s1.BuildCmd != s2.BuildCmd {
		add("spec.buildCmd: assignment=%q watcher=%q", s1.BuildCmd, s2.BuildCmd)
	}
	if s1.TestCmd != s2.TestCmd {
		add("spec.testCmd: assignment=%q watcher=%q", s1.TestCmd, s2.TestCmd)
	}
	if s1.CIGateCmd != s2.CIGateCmd {
		add("spec.ciGateCmd: assignment=%q watcher=%q", s1.CIGateCmd, s2.CIGateCmd)
	}
	if s1.BaseBranch != s2.BaseBranch {
		add("spec.baseBranch: assignment=%q watcher=%q", s1.BaseBranch, s2.BaseBranch)
	}
	reasons = append(reasons, compareConfigRef(s1.ConfigRef, s2.ConfigRef)...)
	reasons = append(reasons, compareBindingRef(s1.ProjectBindingRef, s2.ProjectBindingRef)...)
	reasons = append(reasons, compareWorkflow(s1.Workflow, s2.Workflow)...)
	if (s1.WorkflowSource == nil) != (s2.WorkflowSource == nil) {
		add("spec.workflowSource: assignment=%s watcher=%s", srcShape(s1.WorkflowSource), srcShape(s2.WorkflowSource))
	} else if s1.WorkflowSource != nil && *s1.WorkflowSource != *s2.WorkflowSource {
		add("spec.workflowSource: assignment=%+v watcher=%+v", *s1.WorkflowSource, *s2.WorkflowSource)
	}
	// Catch-all: surface anything the curated list above does not curate
	// yet. DeepEqual on the full spec turns on whenever a future spec
	// field starts diverging without a curated name; the ticket-id field
	// is normalized first because case-only differences are already
	// accepted above (each lane's presentation seam).
	c1, c2 := s1, s2
	if strings.EqualFold(c1.TicketID, c2.TicketID) && c1.TicketID != c2.TicketID {
		c1.TicketID = strings.ToLower(c1.TicketID)
		c2.TicketID = strings.ToLower(c2.TicketID)
	}
	if len(reasons) == 0 && !reflect.DeepEqual(c1, c2) {
		add("spec: assignment and watcher CR specs differ beyond the curated comparison")
	}
	sort.Strings(reasons)
	return reasons
}

// addLabelDiffs compares the CR label maps key-by-key over the union of
// keys (both lanes stamp the same provenance values by design — see the
// runspec label constants).
func addLabelDiffs(reasons *[]string, want, got map[string]string) {
	keys := make([]string, 0, len(want)+len(got))
	for k := range want {
		keys = append(keys, k)
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		if want[k] != got[k] {
			*reasons = append(*reasons, fmt.Sprintf("labels.%s: assignment=%q watcher=%q", k, want[k], got[k]))
		}
	}
}

func compareConfigRef(want, got *criteriav1.CriteriaRunConfigRef) []string {
	if (want == nil) != (got == nil) {
		return []string{fmt.Sprintf("spec.configRef: assignment=%s watcher=%s",
			refShape(want != nil), refShape(got != nil))}
	}
	if want == nil {
		return nil
	}
	var reasons []string
	if want.Name != got.Name {
		reasons = append(reasons, fmt.Sprintf("spec.configRef.name: assignment=%q watcher=%q", want.Name, got.Name))
	}
	if want.ResourceVersion != got.ResourceVersion {
		reasons = append(reasons, fmt.Sprintf("spec.configRef.resourceVersion: assignment=%q watcher=%q", want.ResourceVersion, got.ResourceVersion))
	}
	return reasons
}

func compareBindingRef(want, got *criteriav1.CriteriaRunProjectBindingRef) []string {
	if (want == nil) != (got == nil) {
		return []string{fmt.Sprintf("spec.projectBindingRef: assignment=%s watcher=%s",
			refShape(want != nil), refShape(got != nil))}
	}
	if want == nil {
		return nil
	}
	var reasons []string
	if want.Name != got.Name {
		reasons = append(reasons, fmt.Sprintf("spec.projectBindingRef.name: assignment=%q watcher=%q", want.Name, got.Name))
	}
	if want.ResourceVersion != got.ResourceVersion {
		reasons = append(reasons, fmt.Sprintf("spec.projectBindingRef.resourceVersion: assignment=%q watcher=%q", want.ResourceVersion, got.ResourceVersion))
	}
	return reasons
}

func compareWorkflow(want, got *criteriav1.RunWorkflow) []string {
	if (want == nil) != (got == nil) {
		return []string{fmt.Sprintf("spec.workflow: assignment=%s watcher=%s",
			wfShape(want), wfShape(got))}
	}
	if want == nil {
		return nil
	}
	var reasons []string
	add := func(format string, args ...any) {
		reasons = append(reasons, fmt.Sprintf(format, args...))
	}
	if want.Name != got.Name {
		add("spec.workflow.name: assignment=%q watcher=%q", want.Name, got.Name)
	}
	if want.Type != got.Type {
		add("spec.workflow.type: assignment=%q watcher=%q", want.Type, got.Type)
	}
	if want.Namespace != got.Namespace {
		add("spec.workflow.namespace: assignment=%q watcher=%q", want.Namespace, got.Namespace)
	}
	if want.Class != got.Class {
		add("spec.workflow.class: assignment=%q watcher=%q", want.Class, got.Class)
	}
	if want.Image != got.Image {
		add("spec.workflow.image: assignment=%q watcher=%q", want.Image, got.Image)
	}
	if want.URL != got.URL {
		add("spec.workflow.url: assignment=%q watcher=%q", want.URL, got.URL)
	}
	if want.Ref != got.Ref {
		add("spec.workflow.ref: assignment=%q watcher=%q", want.Ref, got.Ref)
	}
	if want.ImageConflictPolicy != got.ImageConflictPolicy {
		add("spec.workflow.imageConflictPolicy: assignment=%q watcher=%q", want.ImageConflictPolicy, got.ImageConflictPolicy)
	}
	addMapDiffs(&reasons, "spec.workflow.adapterImages", want.AdapterImages, got.AdapterImages)
	addMapDiffs(&reasons, "spec.workflow.env", want.Env, got.Env)
	if len(want.Volumes) != len(got.Volumes) {
		add("spec.workflow.volumes: count assignment=%d watcher=%d", len(want.Volumes), len(got.Volumes))
	} else {
		for i := range want.Volumes {
			wv, gv := want.Volumes[i], got.Volumes[i]
			wj, _ := json.Marshal(wv)
			gj, _ := json.Marshal(gv)
			if string(wj) != string(gj) {
				add("spec.workflow.volumes[%d]: assignment=%s watcher=%s", i, wj, gj)
			}
		}
	}
	if len(want.Secrets) != len(got.Secrets) {
		add("spec.workflow.secrets: count assignment=%d watcher=%d", len(want.Secrets), len(got.Secrets))
	} else if !reflect.DeepEqual(want.Secrets, got.Secrets) {
		wj, _ := json.Marshal(want.Secrets)
		gj, _ := json.Marshal(got.Secrets)
		add("spec.workflow.secrets: assignment=%s watcher=%s", wj, gj)
	}
	reasons = append(reasons, compareInterface(want.Interface, got.Interface)...)
	return reasons
}

func compareInterface(want, got *criteriav1.RunWorkflowInterface) []string {
	if (want == nil) != (got == nil) {
		return []string{fmt.Sprintf("spec.workflow.interface: assignment=%s watcher=%s",
			ifaceShape(want), ifaceShape(got))}
	}
	if want == nil {
		return nil
	}
	if !reflect.DeepEqual(want, got) {
		wj, _ := json.Marshal(want)
		gj, _ := json.Marshal(got)
		return []string{fmt.Sprintf("spec.workflow.interface: assignment=%s watcher=%s", wj, gj)}
	}
	return nil
}

func refShape(present bool) string {
	if !present {
		return "unset"
	}
	return "set"
}

func wfShape(wf *criteriav1.RunWorkflow) string {
	if wf == nil {
		return "unset"
	}
	return fmt.Sprintf("set(%s %s)", wf.Type, wf.Name)
}

func srcShape(src *criteriav1.RunWorkflowSource) string {
	if src == nil {
		return "unset"
	}
	return fmt.Sprintf("set(%s)", src.Type)
}

func ifaceShape(wi *criteriav1.RunWorkflowInterface) string {
	if wi == nil {
		return "unset"
	}
	return "set"
}

// addMapDiffs compares string maps key-by-key over the union of keys.
func addMapDiffs(reasons *[]string, field string, want, got map[string]string) {
	for k, v := range want {
		if g, ok := got[k]; !ok {
			*reasons = append(*reasons, fmt.Sprintf("%s.%s: assignment=%q watcher=absent", field, k, v))
		} else if g != v {
			*reasons = append(*reasons, fmt.Sprintf("%s.%s: assignment=%q watcher=%q", field, k, v, g))
		}
	}
	for k, v := range got {
		if _, ok := want[k]; !ok {
			*reasons = append(*reasons, fmt.Sprintf("%s.%s: assignment=absent watcher=%q", field, k, v))
		}
	}
}
