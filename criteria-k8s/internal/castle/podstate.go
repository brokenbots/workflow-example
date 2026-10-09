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

	"github.com/go-logr/logr"
	"google.golang.org/protobuf/types/known/structpb"

	v1 "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	v1connect "github.com/brokenbots/criteria/sdk/pb/criteria/v1/criteriav1connect"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/events"
)

// Pod-state feed limits. Retries stay small so a reconcile pass never spends
// more than a few seconds inside a failed submit; the reconcile requeues on
// its regular interval anyway and the pod state is re-emitted until it is
// accepted.
const (
	podStateAttemptTimeout = 5 * time.Second
	podStateMaxAttempts    = 3
	podStateBackoffBase    = 250 * time.Millisecond
	podStateBackoffMax     = 2 * time.Second
)

// PodStateFeedConfig configures the pod-state feed.
type PodStateFeedConfig struct {
	// Addr is the castle Connect endpoint, e.g.
	// http://castle.criteria-jobs.svc.cluster.local:8080. Empty disables the
	// feed.
	Addr string
	// Token is the pre-shared criteria bearer token (CASTLE_TOKEN), the
	// operator's established castle identity. No bootstrap/register path:
	// the feed reuses the same stable identity castle observation reads
	// with.
	Token string
}

// PodStateFeed emits per-scope adapter pod-state events (KB-225) onto the
// run's castle event stream through CriteriaService.SubmitEvents — the same
// adapter-event channel the engine uses for adapter.lifecycle.* events (the
// intake design ruling: reuse the adapter-event channel, not a side-channel).
//
// Emission is strictly best-effort: submits are retried with a small
// backoff, failures are logged through the caller's logger and never fail
// the reconciliation. Events are emitted on observed-state transitions only
// (deduped per scope on phase + reason), so a healthy pod emits exactly one
// event once it starts dialing, and a stuck pod emits one event per
// distinct observed wait state.
type PodStateFeed struct {
	addr   string
	token  string
	client v1connect.CriteriaServiceClient

	// Retry policy. New sets the defaults; tests may shrink them.
	attemptTimeout time.Duration
	maxAttempts    int
	backoffBase    time.Duration

	// mu guards last: scope key -> last accepted "phase|reason" signal.
	mu   sync.Mutex
	last map[string]string
}

// NewPodStateFeed builds a feed. A nil httpClient uses http.DefaultClient.
func NewPodStateFeed(cfg PodStateFeedConfig, httpClient *http.Client) *PodStateFeed {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if cfg.Token != "" {
		httpClient = &http.Client{Transport: &tokenTransport{base: httpClient.Transport, token: cfg.Token}}
	}
	return &PodStateFeed{
		addr:           cfg.Addr,
		token:          cfg.Token,
		client:         v1connect.NewCriteriaServiceClient(httpClient, strings.TrimSuffix(cfg.Addr, "/")),
		attemptTimeout: podStateAttemptTimeout,
		maxAttempts:    podStateMaxAttempts,
		backoffBase:    podStateBackoffBase,
		last:           map[string]string{},
	}
}

// Disabled reports whether the feed is disabled (nil feed or empty Addr).
// Safe on a nil *PodStateFeed so the reconciler needs no nil checks.
func (f *PodStateFeed) Disabled() bool {
	return f == nil || f.addr == ""
}

// PodStateEventEnvelope builds the typed submission envelope for one report:
// the nested AdapterEvent payload carrying the pinned flat data keys. The
// report is validated first (via events.MarshalPodStateEvent's contract) and
// the data keys are the exact pinned set from events.PodStateReport, so the
// typed proto and the pinned JSON line cannot grow apart in shape.
func PodStateEventEnvelope(runID string, report events.PodStateReport) (*v1.Envelope, error) {
	data, err := structpb.NewStruct(map[string]any{
		"adapter":           report.AdapterName,
		"adapter_type":      report.AdapterType,
		"scope_instance_id": report.ScopeID,
		"scope_name":        report.ScopeName,
		"pod":               report.Pod,
		"phase":             report.Phase,
		"reason":            report.Reason,
		"message":           report.Message,
		"observed_at":       report.ObservedAt.UTC().Format(time.RFC3339),
	})
	if err != nil {
		return nil, fmt.Errorf("building pod-state data: %w", err)
	}
	if err := report.Validate(); err != nil {
		return nil, err
	}
	if runID == "" {
		return nil, errors.New("pod-state event: empty run_id")
	}
	return &v1.Envelope{
		RunId: runID,
		Payload: &v1.Envelope_AdapterEvent{
			AdapterEvent: &v1.AdapterEvent{
				Adapter: report.AdapterName,
				Kind:    events.PodStateObservedKind,
				Data:    data,
			},
		},
	}, nil
}

// Emit reports the observed pod state of active scopes onto the run's event
// stream. It submits only reports whose (phase, reason) signal differs from
// the last accepted emit for the scope (transition-only emission: the
// healthy path emits once per state a pod moves through); failures are
// logged and retried on the next reconcile pass — Emit never returns an
// error and never blocks reconciliation for more than its bounded retry
// budget per changed scope.
func (f *PodStateFeed) Emit(ctx context.Context, runID string, reports []events.PodStateReport, logger logr.Logger) {
	if f.Disabled() || runID == "" {
		return
	}

	// Choose changed reports under one lock, then submit outside it.
	pending := make([]events.PodStateReport, 0, len(reports))
	f.mu.Lock()
	for _, report := range reports {
		key := report.ScopeKey()
		signal := report.PodStateSignal()
		if f.last[key] == signal {
			continue
		}
		pending = append(pending, report)
	}
	f.mu.Unlock()

	for _, report := range pending {
		if err := f.emit(ctx, runID, report); err != nil {
			// Accepted-state is not advanced on failure: the next
			// reconcile pass re-emits until castle takes it.
			logger.Error(err, "pod-state emit failed; re-emitting on the next reconcile",
				"run", runID, "pod", report.Pod,
				"adapter", report.AdapterName, "adapter_type", report.AdapterType,
				"scope_id", report.ScopeID, "phase", report.Phase, "reason", report.Reason)
			continue
		}
		f.mu.Lock()
		f.last[report.ScopeKey()] = report.PodStateSignal()
		f.mu.Unlock()
		logger.V(1).Info("emitted adapter pod-state event",
			"run", runID, "pod", report.Pod,
			"adapter", report.AdapterName, "adapter_type", report.AdapterType,
			"scope_id", report.ScopeID, "scope_name", report.ScopeName,
			"phase", report.Phase, "reason", report.Reason)
	}
}

// emit submits one report with bounded retries.
func (f *PodStateFeed) emit(ctx context.Context, runID string, report events.PodStateReport) error {
	return f.retry(ctx, "podstate", func(ctx context.Context) error {
		env, err := PodStateEventEnvelope(runID, report)
		if err != nil {
			return err
		}
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

// retry runs the attempt up to podStateMaxAttempts times with exponential
// backoff and a per-attempt timeout, so a hung castle cannot stall a
// reconcile.
func (f *PodStateFeed) retry(ctx context.Context, op string, attempt func(ctx context.Context) error) error {
	var lastErr error
	for i := 0; i < f.maxAttempts; i++ {
		if i > 0 {
			delay := f.backoffBase << (i - 1)
			if delay > podStateBackoffMax {
				delay = podStateBackoffMax
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
		if errors.Is(err, context.Canceled) {
			return fmt.Errorf("castle %s: %w", op, err)
		}
	}
	return fmt.Errorf("castle %s: %w", op, lastErr)
}
