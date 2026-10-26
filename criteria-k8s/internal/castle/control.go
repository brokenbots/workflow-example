package castle

import (
	"net/http"
	"strings"

	v1connect "github.com/brokenbots/criteria/sdk/pb/criteria/v1/criteriav1connect"
)

// ControlClient is a typed alias so callers of the control stream (KB-319
// shadow-run) need not know the full criteria service interface — the control
// plane exposes the whole CriteriaService surface, but the operator's
// subscription lane uses only Control.
type ControlClient = v1connect.CriteriaServiceClient

// AgentClient builds a connect client for castle's CriteriaService on the
// assignment-agent lane (KB-338): the operator's REAL agent identity — a
// criteria identity whose overseer record turns "online" through the lane's
// Heartbeat loop, so castle's server-side dispatch (leaseAndDispatchNext)
// pushes queued WorkflowAssignments to it on the Control stream, and its
// event submissions on leased runs pass the ownership gate. Same transport
// requirements as NewControlClient (cleartext HTTP/2; nil httpClient
// defaults to the h2c prior-knowledge client; KB-239).
//
// Deliberately NOT the lease-safety posture: this lane heartbeats and
// consumes assignments to execute them (1 CR in flight per replica, the
// dave serial rule, enforced by the agent's serial processor). The token
// must be a dedicated pre-provisioned agent criteria token — the
// orchestrator-only token is rejected by castle's control lane the same way
// it is on the shadow stream.
func NewAgentClient(cfg Config, httpClient *http.Client) ControlClient {
	if httpClient == nil {
		httpClient = newClearTextH2CClient()
	}
	if cfg.Token != "" {
		httpClient = &http.Client{Transport: &tokenTransport{base: httpClient.Transport, token: cfg.Token}}
	}
	return v1connect.NewCriteriaServiceClient(httpClient, strings.TrimSuffix(cfg.Addr, "/"))
}

// NewControlClient builds a connect client for castle's CriteriaService —
// the service whose Control server-stream pushes queued WorkflowAssignments
// to a connected identity (CRI-60; KB-319 shadow-run consumes it).
//
// A nil httpClient defaults to the same cleartext HTTP/2 prior-knowledge
// client the PodStateFeed uses (KB-239): castle serves the Control stream
// from a cleartext port that requires HTTP/2 — connect answers server-stream
// requests over HTTP/1.1 with 505. Callers passing a custom client keep
// working unchanged; it is then their responsibility to speak HTTP/2.
//
// Lease-safety posture (KB-319 shadow-run): the stream is the ASSIGNMENT
// DISPATCH lane — an identity whose overseer record reads status "online"
// is eligible to be leased queued work, and a lease held by the operator
// would strand that work until the lease expires. This client therefore
// never calls Register or Heartbeat: nothing announces the consumer online,
// castle's lease path (LeaseWorkflowAssignment) sees status != "online" and
// skips the consumer, and the stream stays observational. Callers must not
// reuse this client for a Heartbeat lane; a dedicated criteria token for
// the shadow identity is the intended wiring.
func NewControlClient(cfg Config, httpClient *http.Client) ControlClient {
	if httpClient == nil {
		httpClient = newClearTextH2CClient()
	}
	if cfg.Token != "" {
		httpClient = &http.Client{Transport: &tokenTransport{base: httpClient.Transport, token: cfg.Token}}
	}
	return v1connect.NewCriteriaServiceClient(httpClient, strings.TrimSuffix(cfg.Addr, "/"))
}
