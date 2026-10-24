package control

import (
	"context"
	"sort"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Divergence reports the outcome of comparing one assignment's derived
// decision against the watcher's CriteriaRun(s) for the same ticket.
type Divergence struct {
	// Clean is true when the two deciders agree (same ticket, repo, class
	// and workflow) and no field check was inconclusive.
	Clean bool
	// CRName names the watcher's CriteriaRun the comparison ran against
	// (empty when no run exists for the ticket).
	CRName string
	// Reasons lists every observed divergence; empty when Clean.
	Reasons []string
}

// Watchdog compares assignment-derived decisions against the watcher's
// stamped CriteriaRuns. It never writes: the shadow window's consumer is
// observational by design (watcher keeps deciding; castle dispatch also
// decides; the divergence log reconciles the two).
type Watchdog struct {
	Client    client.Client
	Namespace string
	// CRLabelTicket is the watcher's ticket label key on a CriteriaRun
	// ("ticket" — the buildCriteriaRun stamp).
	CRLabelTicket string
	// Builder (optional) is the assignment lane's spec-level mirror: when
	// set, CheckDecision additionally derives the would-be assignment-driven
	// CriteriaRun in memory and compares it field-for-field against the
	// watcher's CR (RunBuilder.CompareSpec — no API writes on either lane).
	// nil keeps the decision-granularity comparison only.
	Builder *RunBuilder
}

// CheckDecision compares one assignment-derived decision against the live
// CriteriaRun(s) carrying the same ticket label. Multiple runs for one
// ticket (refires) compare against the newest run (name's unix suffix is
// the tiebreaker — deterministic); one mismatching run is a divergence even
// when a sibling matches.
func (w *Watchdog) CheckDecision(ctx context.Context, d Decision) Divergence {
	if w == nil || w.Client == nil || d.Ticket == "" {
		div := Divergence{Reasons: []string{
			"watchdog disabled or ticket unattributable: missing ticket label — cannot locate the watcher's run"}}
		for _, miss := range d.Missing {
			div.Reasons = append(div.Reasons, "assignment-labels missing: "+miss)
		}
		return div
	}
	var runs criteriav1.CriteriaRunList
	if err := w.Client.List(ctx, &runs, client.InNamespace(w.Namespace),
		client.MatchingLabels{w.TicketLabel(): d.Ticket}); err != nil {
		return Divergence{Reasons: []string{"listing watcher runs failed: " + err.Error()}}
	}
	if len(runs.Items) == 0 {
		return Divergence{Reasons: []string{
			"no watcher CriteriaRun found for ticket " + d.Ticket,
			"the watcher did not decide on this assignment"}}
	}
	sort.Slice(runs.Items, func(i, j int) bool {
		return runs.Items[i].Name > runs.Items[j].Name
	})
	run := &runs.Items[0]

	div := Divergence{CRName: run.Name}
	for _, f := range compare(d, run) {
		if f.want != f.got {
			div.Reasons = append(div.Reasons,
				f.field+": assignment="+f.want+" watcher="+f.got)
		}
	}
	// Spec-level comparison (KB-319 rem1): the decision comparison above
	// only sees the label-derived identity fields; with a builder wired,
	// the would-be assignment-driven CR is stamped through the shared
	// runspec path and compared field-for-field against the watcher's CR.
	if w.Builder != nil {
		if wouldBe, buildReasons := w.Builder.BuildRun(ctx, d); wouldBe == nil {
			div.Reasons = append(div.Reasons, buildReasons...)
		} else {
			div.Reasons = append(div.Reasons, CompareSpec(wouldBe, run)...)
		}
	}
	div.Clean = len(div.Reasons) == 0
	return div
}

// TicketLabel resolves the CR-side ticket label key.
func (w *Watchdog) TicketLabel() string {
	if w.CRLabelTicket == "" {
		return DefaultLabelTicket
	}
	return w.CRLabelTicket
}

// compareField is one like-for-like decision field: the assignment's value
// (want) versus the watcher CR's (got).
type compareField struct {
	field string
	want  string
	got   string
}

// compare runs the field-by-field comparison. Field semantics mirror the
// operator's queue admission derivation (queueKeyOf): repoURL +
// class. The class falls back to dev when neither lane stamps one, so an
// unclassifiable run and an unlabeled assignment still agree.
func compare(d Decision, run *criteriav1.CriteriaRun) []compareField {
	fields := []compareField{
		// The repo URL is the serialization identity (the interface
		// lockKey's only value admits the repoURL derivation, KB-319).
		{field: "repoURL", want: d.RepoURL, got: run.Spec.RepoURL},
		{field: "class", want: normalizeClass(d.Class), got: normalizeClass(crClass(run))},
	}
	// The workflow name agrees when both lanes reference the same tree;
	// an assignment with workflow_name and a watcher run for an image
	// workflow never agree.
	if d.WorkflowName != "" {
		got := crWorkflowName(run)
		if got == "" {
			fields = append(fields, compareField{field: "workflow", want: d.WorkflowName, got: "none (no stamped workflow object)"})
		} else {
			fields = append(fields, compareField{field: "workflow", want: d.WorkflowName, got: got})
		}
	}
	for _, miss := range d.Missing {
		fields = append(fields, compareField{field: "assignment-labels", want: "present", got: "missing: " + miss})
	}
	return fields
}

// crWorkflowName returns the stamped workflow object's name, "" when the
// run carries no workflow (legacy image runs).
func crWorkflowName(run *criteriav1.CriteriaRun) string {
	if run.Spec.Workflow == nil || run.Spec.Workflow.Name == "" {
		return ""
	}
	return run.Spec.Workflow.Name
}

// crClass mirrors controller.queue.admissionClass: triage only on an
// explicit stamped declaration (the copies must stay in lockstep; note in
// both).
func crClass(run *criteriav1.CriteriaRun) string {
	if run.Spec.Workflow != nil && run.Spec.Workflow.Class == criteriav1.RunClassTriage {
		return criteriav1.RunClassTriage
	}
	return criteriav1.RunClassDev
}

// normalizeClass applies the same fallback both deciders' raw values need:
// an empty class is dev.
func normalizeClass(class string) string {
	if class == "" {
		return criteriav1.RunClassDev
	}
	return class
}
