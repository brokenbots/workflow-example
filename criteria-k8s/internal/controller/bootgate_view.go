package controller

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// BootgateView serves the KB-234 boot gate's operator-view document: the
// tiny {"phase": "...", "terminal": bool} the runner's boot gate consults
// before registering a fresh (or adopting) run with castle. The CR is
// identified per run, never per ticket family (KB-301): when the request
// carries a job name (the runner always sends one — jobbuilder injects
// CRITERIA_RUN_JOB), that identity resolves the booting CR exactly — its job
// label (criteria.brokenbots.dev/job; the afff356 shape, now namespaced)
// or, for the watcher's label-less CRs, the CR name itself (the runner job
// name is JobName(run), which the CR carries for every watcher-shaped run).
// A ticket-only request resolves the ticket label family newest-wins — the
// legacy path for callers without a job identity. A CR that is absent
// (deleted while its runner pod survived) is reported phase "NotFound",
// terminal true — the fresh-start hole the gate closes (KB-234's boot-1
// orphan class); family resolution is never consulted on a job miss, so an
// orphaned CR stays NotFound instead of borrowing a sibling's open verdict.
// Every read is namespaced (the operator's Role grants no cluster scope)
// and over a direct (uncached) client: the boot decision must never act on
// a stale cached view (the CRI-264 posture).
type BootgateView struct {
	Client client.Client
	// Namespace is the operator's managed namespace; the runner's request
	// carries no namespace and cross-namespace Gets are invalid.
	Namespace string
}

// namespaceOr resolves the CR namespace: the request's ns when supplied,
// else the operator's own managed namespace.
func (v *BootgateView) namespaceOr(requested string) string {
	if requested != "" {
		return requested
	}
	if v.Namespace != "" {
		return v.Namespace
	}
	return "criteria-jobs"
}

// unixSuffix extracts the trailing numeric unix segment of a kb-<ticket>-<unix>
// CR name, 0 when absent.
func unixSuffix(name string) int64 {
	i := strings.LastIndexByte(name, '-')
	if i < 0 {
		return 0
	}
	n, err := strconv.ParseInt(name[i+1:], 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// runNewest picks the newest of a non-empty run set: real CRs carry
// monotonic creation timestamps; where they tie (fake-client granularity),
// the larger unix suffix breaks the tie — numeric, not lexicographic (100 >
// 99 though the names sort 'kb-42-100' < 'kb-42-99').
func runNewest(runs []criteriav1.CriteriaRun) criteriav1.CriteriaRun {
	best := runs[0]
	for i := 1; i < len(runs); i++ {
		c := &runs[i]
		if best.CreationTimestamp.Before(&c.CreationTimestamp) ||
			(best.CreationTimestamp.Equal(&c.CreationTimestamp) && unixSuffix(c.Name) > unixSuffix(best.Name)) {
			best = *c
		}
	}
	return best
}

var bootgateTerminalPhases = map[string]bool{
	"Succeeded": true,
	"Failed":    true,
	"Destroyed": true,
}

func (v *BootgateView) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if v.Client == nil {
		http.Error(w, "bootgate view: no client wired", http.StatusInternalServerError)
		return
	}
	q := r.URL.Query()
	ticket := strings.TrimSpace(q.Get("ticket"))
	job := strings.TrimSpace(q.Get("job"))
	if ticket == "" && job == "" {
		http.Error(w, "bootgate view: ticket or job required", http.StatusBadRequest)
		return
	}

	var run criteriav1.CriteriaRun
	var found bool
	// KB-301 job-first: the booting CR is resolved by the job identity the
	// request carries, never by the ticket family. Resolution by family on
	// a job miss ("borrow a sibling's phase") is deliberately NOT done: a
	// family row is never proven to be the booting CR, and consulting it
	// would both shadow a fresh boot behind a newer terminal row (the
	// KB-301 false-park shadow) and hand an orphaned CR a sibling's open
	// verdict (the KB-234 orphan hole). The family branch is the legacy
	// path, used only when the request carries no job identity.
	requested := r.URL.Query().Get("ns")
	ticketLabel := ""
	if ticket != "" {
		ticketLabel = "kb-" + strings.ToLower(strings.TrimPrefix(ticket, "KB-"))
	}
	if job != "" {
		// The afff356 per-job exact match (KB-301: now namespaced — the
		// cluster-scoped List is RBAC-forbidden for the operator's Role and
		// was unreachable behind the hasTicket-first switch since cac4def).
		// Unambiguous only when exactly one CR carries the job label.
		var labeled criteriav1.CriteriaRunList
		if err := v.Client.List(r.Context(), &labeled,
			client.InNamespace(v.namespaceOr(requested)),
			client.MatchingLabels{"criteria.brokenbots.dev/job": job}); err != nil {
			http.Error(w, fmt.Sprintf("bootgate view: CR list: %v", err), http.StatusInternalServerError)
			return
		}
		if len(labeled.Items) == 1 {
			run, found = labeled.Items[0], true
		}
		if !found {
			// The watcher's CRs carry no job label, and the runner job name
			// is JobName(run) — the CR name for every watcher-shaped run. A
			// namespaced Get on the job identity resolves those; names are
			// unique within the namespace, so the match is exact.
			var byName criteriav1.CriteriaRun
			err := v.Client.Get(r.Context(), client.ObjectKey{Namespace: v.namespaceOr(requested), Name: job}, &byName)
			switch {
			case err == nil:
				run, found = byName, true
			case client.IgnoreNotFound(err) == nil:
			default:
				http.Error(w, fmt.Sprintf("bootgate view: CR read: %v", err), http.StatusInternalServerError)
				return
			}
		}
	}
	if job == "" && ticket != "" {
		// The ticket's CRs are named kb-<ticket>-<unix> — the name is not
		// derivable from the ticket alone. The ticket label (the watcher
		// stamps it) selects the family; the NEWEST run is the live one.
		var list criteriav1.CriteriaRunList
		if err := v.Client.List(r.Context(), &list,
			client.InNamespace(v.namespaceOr(requested)),
			client.MatchingLabels{"ticket": ticketLabel}); err != nil {
			http.Error(w, fmt.Sprintf("bootgate view: CR list: %v", err), http.StatusInternalServerError)
			return
		}
		if len(list.Items) > 0 {
			run, found = runNewest(list.Items), true
		}
	}

	doc := struct {
		Phase    string `json:"phase"`
		Terminal bool   `json:"terminal"`
	}{Phase: "NotFound", Terminal: true}
	if found {
		if run.DeletionTimestamp != nil {
			doc.Phase, doc.Terminal = "Deleted", true
		} else {
			doc.Phase = string(run.Status.Phase)
			doc.Terminal = bootgateTerminalPhases[doc.Phase]
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(fmt.Sprintf(`{"phase":%q,"terminal":%t}`, doc.Phase, doc.Terminal)))
}
