// Package castle publishes CriteriaRun lifecycle state from the criteria-k8s
// operator into the castle control plane (CRI-131) over the criteria Connect
// API: CriteriaService.CreateRun registers the run and EventService.SubmitEvents
// streams phase transitions.
//
// Publishing is strictly best-effort from the operator's perspective: every
// method returns an error for the caller to log and retry on a later
// reconcile, so a castle outage must never block or fail a run. Publishing is
// disabled entirely when Config.Addr is empty (unit tests, local runs).
package castle

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"

	criteria "github.com/brokenbots/workflow-example/criteria-k8s/internal/criteria"
	v1 "github.com/brokenbots/workflow-example/criteria-k8s/internal/criteria/pb/criteria/v1"
	v1connect "github.com/brokenbots/workflow-example/criteria-k8s/internal/criteria/pb/criteria/v1/criteriav1connect"
)

// Publishing defaults. The retries stay small so a reconcile never spends
// more than a few seconds inside the publisher.
const (
	defaultAttemptTimeout = 5 * time.Second
	defaultMaxAttempts    = 3
	defaultBackoffBase    = 250 * time.Millisecond
	maxBackoff            = 2 * time.Second
)

// Config configures castle publishing.
type Config struct {
	// Addr is the castle Connect endpoint, e.g.
	// http://castle.criteria-jobs.svc.cluster.local:8080. Empty disables
	// publishing.
	Addr string

	// Token is a pre-shared criteria agent token (CASTLE_TOKEN). Preferred
	// over BootstrapToken because the agent identity stays stable across
	// operator restarts.
	Token string

	// BootstrapToken is the castle server bootstrap token
	// (CASTLE_BOOTSTRAP_TOKEN). Used only when Token is empty: the publisher
	// exchanges it for an agent token via Register, once per process.
	BootstrapToken string

	// AgentName is the agent name reported to castle during registration.
	AgentName string
}

// Disabled reports whether publishing is turned off (empty Addr).
func (c Config) Disabled() bool { return c.Addr == "" }

// Publisher streams CriteriaRun lifecycle updates into castle.
type Publisher struct {
	addr           string
	token          string
	bootstrapToken string
	agentName      string
	client         v1connect.CriteriaServiceClient

	// Retry policy. New sets the defaults; tests may shrink them.
	attemptTimeout time.Duration
	maxAttempts    int
	backoffBase    time.Duration

	// mu guards lazy registration state. Castle may reset its database (or the
	// bootstrap token may rotate), so a registered agent token can go stale;
	// the publisher re-registers when a call is rejected as unauthenticated.
	mu              sync.Mutex
	registered      bool
	registeredToken string
	criteriaID      string
}

// New builds a Publisher. A nil httpClient selects http.DefaultClient.
func New(cfg Config, httpClient *http.Client) *Publisher {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Publisher{
		addr:           cfg.Addr,
		token:          cfg.Token,
		bootstrapToken: cfg.BootstrapToken,
		agentName:      cfg.AgentName,
		client: v1connect.NewCriteriaServiceClient(
			httpClient,
			strings.TrimSuffix(cfg.Addr, "/"),
		),
		attemptTimeout: defaultAttemptTimeout,
		maxAttempts:    defaultMaxAttempts,
		backoffBase:    defaultBackoffBase,
	}
}

// Disabled reports whether publishing is disabled (nil publisher or empty
// Addr). Safe on a nil *Publisher so the reconciler needs no nil checks.
func (p *Publisher) Disabled() bool { return p == nil || p.addr == "" }

// EnsureRunRequest describes the run to publish.
type EnsureRunRequest struct {
	// Ticket is the Linear ticket identifier (CriteriaRun spec.ticketId).
	Ticket string

	// RepoURL is the GitHub repository under test (spec.repoUrl).
	RepoURL string

	// WorkflowName identifies the run in castle (required by the server).
	WorkflowName string
}

// EnsureRun creates (or idempotently re-reads) the castle run for a
// CriteriaRun and returns the castle run identifier. Castle deduplicates
// ticket-backed creates while an open run for the same ticket exists, so
// repeated reconciles are safe.
func (p *Publisher) EnsureRun(ctx context.Context, req EnsureRunRequest) (string, error) {
	if p.Disabled() {
		return "", nil
	}
	var runID string
	err := p.retry(ctx, "create_run", func(ctx context.Context) error {
		if err := p.ensureIdentity(ctx); err != nil {
			return err
		}
		creq := connect.NewRequest(&v1.CreateRunRequest{
			CriteriaId:   p.callerID(),
			WorkflowName: req.WorkflowName,
			Ticket:       req.Ticket,
			RepoUrl:      req.RepoURL,
		})
		creq.Header().Set("X-Criteria-Token", p.agentToken())
		res, err := p.client.CreateRun(ctx, creq)
		if err != nil {
			return err
		}
		runID = res.Msg.RunId
		return nil
	})
	return runID, err
}

// PublishEvent submits a single lifecycle envelope for runID on the
// SubmitEvents stream.
func (p *Publisher) PublishEvent(ctx context.Context, runID string, payload any) error {
	if p.Disabled() {
		return nil
	}
	env := criteria.NewEnvelope(runID, payload)
	return p.retry(ctx, "submit_events", func(ctx context.Context) error {
		if err := p.ensureIdentity(ctx); err != nil {
			return err
		}
		stream := p.client.SubmitEvents(ctx)
		stream.RequestHeader().Set("X-Criteria-Token", p.agentToken())
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

// retry runs attempt up to defaultMaxAttempts times with exponential backoff
// and a per-attempt timeout, so a hung castle cannot stall a reconcile.
// Unauthenticated failures invalidate a lazily registered agent token so the
// next attempt re-registers.
func (p *Publisher) retry(ctx context.Context, op string, attempt func(ctx context.Context) error) error {
	var lastErr error
	for i := 0; i < p.maxAttempts; i++ {
		if i > 0 {
			delay := p.backoffBase << (i - 1)
			if delay > maxBackoff {
				delay = maxBackoff
			}
			select {
			case <-ctx.Done():
				return fmt.Errorf("castle %s: %w", op, ctx.Err())
			case <-time.After(delay):
			}
		}
		attemptCtx, cancel := context.WithTimeout(ctx, p.attemptTimeout)
		err := attempt(attemptCtx)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
		if errors.Is(err, context.Canceled) {
			return fmt.Errorf("castle %s: %w", op, err)
		}
		if connect.CodeOf(err) == connect.CodeUnauthenticated && p.usesBootstrap() {
			p.invalidate()
		}
	}
	return fmt.Errorf("castle %s: %w", op, lastErr)
}

// ensureIdentity resolves the agent token: a pre-shared token needs nothing,
// a bootstrap token is exchanged for an agent token via Register exactly once
// per process (and again after invalidation).
func (p *Publisher) ensureIdentity(ctx context.Context) error {
	if p.token != "" || !p.usesBootstrap() {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.registered {
		return nil
	}
	req := connect.NewRequest(&v1.RegisterRequest{Name: p.agentName})
	req.Header().Set("X-Server-Bootstrap", p.bootstrapToken)
	res, err := p.client.Register(ctx, req)
	if err != nil {
		return err
	}
	p.registered = true
	p.criteriaID = res.Msg.CriteriaId
	p.registeredToken = res.Msg.Token
	return nil
}

func (p *Publisher) usesBootstrap() bool { return p.token == "" && p.bootstrapToken != "" }

// callerID returns the registered criteria agent ID (empty for pre-shared
// tokens; the server derives identity from the token itself).
func (p *Publisher) callerID() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.criteriaID
}

func (p *Publisher) agentToken() string {
	if p.token != "" {
		return p.token
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.registeredToken
}

// invalidate drops the lazily registered agent identity.
func (p *Publisher) invalidate() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.registered = false
	p.criteriaID = ""
	p.registeredToken = ""
}
