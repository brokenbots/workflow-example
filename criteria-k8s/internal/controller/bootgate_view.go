package controller

import (
	"fmt"
	"net/http"
	"strings"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// BootgateView serves the KB-234 boot gate's operator-view document: the
// tiny {"phase": "...", "terminal": bool} the runner's boot gate consults
// before registering a fresh (or adopting) run with castle. Ticket and job
// identify the CriteriaRun: ticket → the CR named kb-<id> (the watcher's
// naming); job → the CR resolved by its job label, covering flows without a
// ticket identity. A CR that is absent (deleted while its runner pod
// survived) is reported phase "NotFound", terminal true — the fresh-start
// hole the gate closes (KB-234's boot-1 orphan class). The read is over a
// direct (uncached) client: the boot decision must never act on a stale
// cached view (the CRI-264 posture).
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
	switch {
	case ticket != "":
		err := v.Client.Get(r.Context(), client.ObjectKey{Namespace: v.namespaceOr(r.URL.Query().Get("ns")), Name: "kb-" + strings.ToLower(strings.TrimPrefix(ticket, "KB-"))}, &run)
		switch {
		case err == nil:
			found = true
		case client.IgnoreNotFound(err) == nil:
			found = false
		default:
			http.Error(w, fmt.Sprintf("bootgate view: CR read: %v", err), http.StatusInternalServerError)
			return
		}
	default:
		var list criteriav1.CriteriaRunList
		if job == "" {
			http.Error(w, "bootgate view: ticket or job required", http.StatusBadRequest)
			return
		}
		if err := v.Client.List(r.Context(), &list, client.MatchingLabels{"criteria.brokenbots.dev/job": job}); err != nil {
			http.Error(w, fmt.Sprintf("bootgate view: CR list: %v", err), http.StatusInternalServerError)
			return
		}
		if len(list.Items) == 1 {
			run = list.Items[0]
			found = true
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
