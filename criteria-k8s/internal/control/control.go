// Package control implements the KB-319 shadow-run window: the operator
// consumes castle's Control-stream WorkflowAssignments (CRI-60) and, without
// creating anything, derives the run-decision the dispatch lane made and
// compares it against the CriteriaRun the watcher (the deciding lane that
// keeps deciding) stamped for the same ticket. The comparison is the
// divergence log: clean when the two deciders agree, a warning when they do
// not. While the window is open the assignment lane never writes — the
// kill-switch flag turns the consumer off entirely.
package control

// Assignment is the decoded ControlMessage_WorkflowAssignment payload: the
// identity of the queued workflow work the dispatch lane decided on, plus
// the labels the orchestrator queue passed through (the assignment's
// required-label vocabulary).
type Assignment struct {
	// RunID is the assignment's castle run id.
	RunID string
	// WorkflowName is the workflow the dispatch lane queued.
	WorkflowName string
	// WorkflowSource is the raw HCL/CHCL source (empty when the dispatch
	// lane references a tree by name).
	WorkflowSource string
	// LockfileSource is the tree's .criteria.lock.hcl, when carried.
	LockfileSource string
	// Labels is the orchestrator queue's label pass-through.
	Labels map[string]string
}

// Label keys the assignment lane's queue stamps (the kanboard provenance the
// dispatch decision carries). The defaults mirror the CriteriaRun's watcher
// stamps (buildCriteriaRun) so Translate is a like-for-like comparison.
const (
	// DefaultLabelTicket mirrors the CR's "ticket" label.
	DefaultLabelTicket = "ticket"
	// DefaultLabelRepoURL mirrors the CR's spec.repoURL.
	DefaultLabelRepoURL = "criteria.brokenbots.dev/repo-url"
	// DefaultLabelClass mirrors the stamped workflow object's class.
	DefaultLabelClass = "criteria.brokenbots.dev/class"
	// DefaultLabelProject mirrors the routes project that resolved the
	// workflow.
	DefaultLabelProject = "criteria.brokenbots.dev/project"
)

// Config configures the shadow-run lane.
type Config struct {
	// Addr is the castle Connect endpoint. Empty disables the lane.
	Addr string
	// Token is the criteria token the control stream authenticates with.
	// NOTE the lease-safety posture: the token must be one whose overseer
	// record never turns "online" (this lane never heartbeats), so castle
	// never leases queued work to the consumer.
	Token string
	// CriteriaID is the request-supplied criteria id. Castle uses the
	// authenticated caller's identity when the auth interceptor is wired;
	// this matters only on the direct-call/test path.
	CriteriaID string

	// Label keys the queue's assignment labels use. Empty selects the
	// defaults above.
	LabelTicket  string
	LabelRepoURL string
	LabelClass   string
	LabelProject string
}

// TicketLabel resolves the ticket-label key ( defaulted to DefaultLabelTicket).
func (c Config) TicketLabel() string { return labelKey(c.LabelTicket, DefaultLabelTicket) }

// RepoURLLabel resolves the repo-url-label key.
func (c Config) RepoURLLabel() string { return labelKey(c.LabelRepoURL, DefaultLabelRepoURL) }

// ClassLabel resolves the class-label key.
func (c Config) ClassLabel() string { return labelKey(c.LabelClass, DefaultLabelClass) }

// ProjectLabel resolves the project-label key.
func (c Config) ProjectLabel() string { return labelKey(c.LabelProject, DefaultLabelProject) }

func labelKey(key, def string) string {
	if key == "" {
		return def
	}
	return key
}

// Decision is the run decision derived from one assignment: the identity
// fields the watcher's CR also carries, so the two deciders can be compared
// like-for-like at decision granularity (the shadow window's whole point).
type Decision struct {
	Ticket       string
	Project      string
	RepoURL      string
	Class        string
	WorkflowName string
	// Missing names every required decision field the assignment did not
	// carry. A non-empty Missing is itself a divergence: the dispatch lane
	// queued work the operator cannot attribute.
	Missing []string
}

// Translate derives the would-be run decision from an assignment.
func Translate(cfg Config, a Assignment) Decision {
	d := Decision{
		WorkflowName: a.WorkflowName,
	}
	for k, v := range a.Labels {
		switch k {
		case cfg.TicketLabel():
			d.Ticket = v
		case cfg.ProjectLabel():
			d.Project = v
		case cfg.RepoURLLabel():
			d.RepoURL = v
		case cfg.ClassLabel():
			d.Class = v
		}
	}
	for _, miss := range []struct{ have, what string }{
		{d.Ticket, "ticket"},
		{d.RepoURL, "repo-url"},
		{d.Class, "class"},
	} {
		if miss.have == "" {
			d.Missing = append(d.Missing, miss.what)
		}
	}
	if d.WorkflowName == "" {
		d.Missing = append(d.Missing, "workflow_name")
	}
	return d
}
