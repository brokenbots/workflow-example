package castle

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	connect "connectrpc.com/connect"
	sdk "github.com/brokenbots/criteria/sdk"
	v1 "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	v1connect "github.com/brokenbots/criteria/sdk/pb/criteria/v1/criteriav1connect"
)

// AssignmentEventFeed emits the assignment bridge's lifecycle events onto an
// assignment run's castle event stream (KB-338): RunStarted when the leased
// run's CriteriaRun enters Running, and one terminal envelope
// (RunCompleted/RunFailed) when the run finishes. It submits through
// CriteriaService.SubmitEvents with the LEASED agent's identity — castle's
// per-envelope ownership gate rejects every other caller, so the feed is
// usable only with the agent token the assignment was leased to
// (runs.overseer_id).
//
// Emission is bounded best-effort, exactly like the PodStateFeed: a small
// retry budget per envelope, one bidi stream per envelope, and a non-nil
// error only when the budget exhausts. Correlation ids carry the bridge's
// idempotency: castle dedupes (run_id, correlation_id) re-submits, so a
// bridge restart re-emitting the same lifecycle event is an ack, not a
// duplicate row.
type AssignmentEventFeed struct {
	addr   string
	client v1connect.CriteriaServiceClient

	// Retry policy. New sets the defaults; tests may shrink them.
	attemptTimeout time.Duration
	maxAttempts    int
	backoffBase    time.Duration
	backoffMax     time.Duration
}

// Assignment bridge correlation-id prefixes. The bridge's lifecycle events
// carry these so a re-delivery (an envelope submitted twice by a restart)
// dedupes server-side while distinct lifecycle events never collide.
const (
	// CorrelationRunStarted prefixes the RunStarted bridge envelope's
	// correlation id.
	CorrelationRunStarted = "agent-run-started"
	// CorrelationRunTerminal prefixes the terminal bridge envelope's
	// correlation id.
	CorrelationRunTerminal = "agent-run-terminal"
)

// NewAssignmentEventFeed builds the feed. A nil httpClient defaults to the
// cleartext HTTP/2 prior-knowledge client the PodStateFeed and
// NewControlClient use (KB-239): castle serves the SubmitEvents bidi stream
// from a cleartext port that requires HTTP/2. The token must be the leased
// agent's criteria token — castle's ownership gate rejects event submits
// from any other identity.
func NewAssignmentEventFeed(cfg Config, httpClient *http.Client) *AssignmentEventFeed {
	if httpClient == nil {
		httpClient = newClearTextH2CClient()
	}
	if cfg.Token != "" {
		httpClient = &http.Client{Transport: &tokenTransport{base: httpClient.Transport, token: cfg.Token}}
	}
	return &AssignmentEventFeed{
		addr:           cfg.Addr,
		client:         v1connect.NewCriteriaServiceClient(httpClient, strings.TrimSuffix(cfg.Addr, "/")),
		attemptTimeout: 5 * time.Second,
		maxAttempts:    3,
		backoffBase:    250 * time.Millisecond,
		backoffMax:     2 * time.Second,
	}
}

// Disabled reports whether the feed is disabled (nil feed or empty Addr).
// Safe on a nil *AssignmentEventFeed so callers need no nil checks.
func (f *AssignmentEventFeed) Disabled() bool {
	return f == nil || f.addr == ""
}

// Submit submits one payload envelope onto the run's event stream. payload
// must be a sdk payload pointer (e.g. *v1.RunStarted) — the same contract
// criteria.NewEnvelope enforces (unsupported types panic there).
func (f *AssignmentEventFeed) Submit(ctx context.Context, runID, correlationID string, payload any) error {
	if f.Disabled() || f.client == nil {
		return errors.New("assignment event feed: no client")
	}
	if runID == "" {
		return errors.New("assignment event feed: empty run_id")
	}
	env := sdk.NewEnvelope(runID, payload)
	if correlationID != "" {
		env.CorrelationId = correlationID
	}
	return f.submitEnvelope(ctx, env)
}

// submitEnvelope mirrors PodStateFeed.emit: one bidi stream per envelope,
// bounded retries.
func (f *AssignmentEventFeed) submitEnvelope(ctx context.Context, env *v1.Envelope) error {
	return f.retry(ctx, "assignment event", func(ctx context.Context) error {
		stream := f.client.SubmitEvents(ctx)
		if err := stream.Send(env); err != nil {
			return err
		}
		if err := stream.CloseRequest(); err != nil {
			return err
		}
		if _, err := stream.Receive(); err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		return stream.CloseResponse()
	})
}

// retry runs the attempt up to maxAttempts times with exponential backoff
// and a per-attempt timeout, so a hung castle cannot stall the bridge.
func (f *AssignmentEventFeed) retry(ctx context.Context, op string, attempt func(ctx context.Context) error) error {
	var lastErr error
	for i := 0; i < f.maxAttempts; i++ {
		if i > 0 {
			delay := f.backoffBase << (i - 1)
			if delay > f.backoffMax {
				delay = f.backoffMax
			}
			select {
			case <-ctx.Done():
				return fmt.Errorf("castle %s: %w", op, ctx.Err())
			case <-time.After(delay):
			}
		}
		attemptCtx, cancel := context.WithTimeout(ctx, f.attemptTimeout)
		err := attempt(attemptCtx)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
	}
	if lastErr == nil {
		return nil
	}
	return fmt.Errorf("castle %s: %w", op, lastErr)
}

// ServerClient is the assignment bridge's disposition reader surface
// (GetAssignmentDisposition, KB-338 deliverable 3): the desired-run
// record's state read that gates every lifecycle submit — a run whose
// assignment is no longer leased to this identity is never written.
type ServerClient struct {
	client v1connect.ServerServiceClient
	addr   string
}

// NewServerClient builds a connect client for castle's ServerService
// (assignment dispositions and queue-state reads). A nil httpClient
// defaults to the same cleartext HTTP/2 prior-knowledge client as the
// other castle clients (KB-239); Token carries the criteria agent token —
// the disposition read rides the same agent identity as the Control and
// event lanes it gates.
func NewServerClient(cfg Config, httpClient *http.Client) *ServerClient {
	if httpClient == nil {
		httpClient = newClearTextH2CClient()
	}
	if cfg.Token != "" {
		httpClient = &http.Client{Transport: &tokenTransport{base: httpClient.Transport, token: cfg.Token}}
	}
	return &ServerClient{client: v1connect.NewServerServiceClient(httpClient, strings.TrimSuffix(cfg.Addr, "/")), addr: cfg.Addr}
}

// Submit records one workflow assignment (KB-338 deliverable 4: the
// adapter's dispatch entry point). The caller MUST authenticate with an
// agent identity: castle's handler keys the assignment's owner on
// CallerCriteriaID, which an orchestrator identity never resolves, so
// submitting rides the adapter's agent token. Ownership is sticky even
// after a lease moves runs.overseer_id to the executing agent — the same
// criteria identity keeps reading the disposition for the run's whole life.
//
// Required by castle: workflow_name + workflow_source + idempotency_key.
func (s *ServerClient) Submit(ctx context.Context, req *v1.SubmitWorkflowAssignmentRequest) (*v1.SubmitWorkflowAssignmentResponse, error) {
	if s == nil || s.addr == "" {
		return nil, errors.New("castle server client: no address")
	}
	if req == nil {
		return nil, errors.New("castle submit: nil request")
	}
	resp, err := s.client.SubmitWorkflowAssignment(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

// EnvelopeStream is the read view over a run's WatchRun stream — the four
// operations *connect.ServerStreamForClient[v1.Envelope] performs, narrowed
// so consumers hold a seam instead of the concrete connect type.
type EnvelopeStream interface {
	Receive() bool
	Msg() *v1.Envelope
	Err() error
	Close() error
}

// WatchRun opens the assignment run's event stream for the adapter's mirror
// reads (decision 7): the envelopes the executing operator submits
// (RunStarted, RunCompleted, RunFailed) are the only run-state vocabulary
// the submitting side can observe, and the adapter maps them onto the board
// ladder. Read-only observation surface: any authenticated identity may
// subscribe; the adapter reads with the same submitter identity that owns
// the assignment.
func (s *ServerClient) WatchRun(ctx context.Context, runID string, sinceSeq uint64) (EnvelopeStream, error) {
	if s == nil || s.addr == "" {
		return nil, errors.New("castle server client: no address")
	}
	if runID == "" {
		return nil, errors.New("castle watch: run_id required")
	}
	return s.client.WatchRun(ctx, connect.NewRequest(&v1.WatchRunRequest{
		RunId:    runID,
		SinceSeq: sinceSeq,
	}))
}

// Disposition returns the assignment's current queue state (control
// package DispositionReader).
func (s *ServerClient) Disposition(ctx context.Context, runID string) (*v1.GetAssignmentDispositionResponse, error) {
	if s == nil || s.addr == "" {
		return nil, errors.New("castle server client: no address")
	}
	resp, err := s.client.GetAssignmentDisposition(ctx, connect.NewRequest(&v1.GetAssignmentDispositionRequest{RunId: runID}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}
