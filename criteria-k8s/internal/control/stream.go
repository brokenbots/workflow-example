package control

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	connect "connectrpc.com/connect"
	"github.com/go-logr/logr"

	v1 "github.com/brokenbots/criteria/sdk/pb/criteria/v1"

	"github.com/brokenbots/workflow-example/criteria-k8s/internal/castle"
)

// Comparator is the divergence checker between one assignment's derived
// decision and the watcher's stamped CriteriaRuns.
type Comparator interface {
	CheckDecision(Decision) Divergence
}

// Reconnect cadence for the observation stream. Backoff is exponential from
// backoffBase up to backoffMax.
var (
	backoffBase = time.Second
	backoffMax  = 30 * time.Second
	dialTimeout = 10 * time.Second
)

// errDivergence marks a divergent decision in the log stream (a signal, not
// an exception — the window exists to surface it; watchers scrape it).
var errDivergence = errors.New("assignment/watcher decision divergence")

// Stream consumes castle's Control stream for WorkflowAssignments and logs
// the decision-divergence against the watcher. Observational only: nothing
// is created and no work state is acknowledged — the stream is a passive
// subscriber, and castle's lease path skips a never-online identity (see
// NewControlClient's lease-safety posture), so consuming assignments here
// can never be charged work.
type Stream struct {
	cfg     Config
	dial    func(ctx context.Context) (assignSink, error)
	compare Comparator
	log     logr.Logger
}

// assignSink is one live Control stream in pull form: next() blocks until
// the next message, returning io.EOF when castle closes the stream.
type assignSink interface {
	next() (*v1.ControlMessage, error)
	close()
}

// connectSink adapts connect-go's pull-style server stream (Receive() bool +
// Msg()/Err()) to assignSink.
type connectSink struct {
	stream *connect.ServerStreamForClient[v1.ControlMessage]
}

func (s connectSink) next() (*v1.ControlMessage, error) {
	if !s.stream.Receive() {
		if err := s.stream.Err(); err != nil {
			return nil, err
		}
		return nil, io.EOF
	}
	return s.stream.Msg(), nil
}

func (s connectSink) close() { _ = s.stream.Close() }

// defaultDialer opens one Control subscribe stream per call, bounded by
// dialTimeout so a hung dial cannot pin the watch loop.
func defaultDialer(cfg Config) func(ctx context.Context) (assignSink, error) {
	return func(ctx context.Context) (assignSink, error) {
		client := castle.NewControlClient(castle.Config{Addr: cfg.Addr, Token: cfg.Token}, nil)
		dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
		defer cancel()
		stream, err := client.Control(dialCtx, connect.NewRequest(
			&v1.ControlSubscribeRequest{CriteriaId: cfg.CriteriaID}))
		if err != nil {
			return nil, err
		}
		return connectSink{stream: stream}, nil
	}
}

// StartStream builds the consumer on the default castle dialer.
func StartStream(cfg Config, cmp Comparator, logger logr.Logger) *Stream {
	return &Stream{cfg: cfg, compare: cmp, log: logger, dial: defaultDialer(cfg)}
}

// Run consumes until ctx is done: dial, subscribe, decode every assignment,
// log the divergence verdict, reconnect with bounded backoff on transport
// failures. Castle rejecting the identity outright on the Control stream (an
// orchestrator token on an agent-owned lane) kills the whole loop after one
// loud log — retrying a rejected identity is log spam, not resilience.
func (s *Stream) Run(ctx context.Context) error {
	defer s.log.Info("shadow-run watchdog stopped")
	backoff := backoffBase
	for {
		sink, err := s.dial(ctx)
		if err != nil {
			if !isLaneRejected(err) {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return ctxErr
				}
				s.log.Error(err, "shadow-run watchdog dial to castle failed; retrying", "addr", s.cfg.Addr)
				if !sleepCtx(ctx, backoff) {
					return ctx.Err()
				}
				backoff = nextBackoff(backoff)
				continue
			}
			s.log.Error(err, "shadow-run watchdog disabled: castle rejected this identity on the Control stream (agent-owned lane); an orchestrator token cannot subscribe — wire a criteria token for the shadow identity, or flip the kill-switch off")
			return err
		}
		backoff = backoffBase
		if err := s.consume(ctx, sink); err != nil {
			sink.close()
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			s.log.Error(err, "shadow-run watchdog stream dropped; reconnecting", "addr", s.cfg.Addr)
			if !sleepCtx(ctx, backoff) {
				return ctx.Err()
			}
			backoff = nextBackoff(backoff)
		}
	}
}

// consume decodes one live stream until it ends. ControlReady and any other
// non-assignment command carry no decision — ignore them (the CRI-60
// contract: clients ignore everything but the assigned work).
func (s *Stream) consume(ctx context.Context, sink assignSink) error {
	for {
		msg, err := sink.next()
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		asg := msg.GetWorkflowAssignment()
		if asg == nil {
			continue
		}
		s.logAssignment(Translate(s.cfg, Assignment{
			RunID:          asg.GetRunId(),
			WorkflowName:   asg.GetWorkflowName(),
			WorkflowSource: asg.GetWorkflowSource(),
			LockfileSource: asg.GetLockfileSource(),
			Labels:         asg.GetLabels(),
		}))
	}
}

// logAssignment emits the divergence verdict: an Info when the dispatch
// lane's decision matches the watcher's run, an Error carrying the reasons
// (scrapeable via errDivergence) when it does not.
func (s *Stream) logAssignment(d Decision) {
	div := s.compare.CheckDecision(d)
	kv := []any{
		"ticket", d.Ticket, "project", d.Project, "workflow", d.WorkflowName,
		"class", d.Class, "watcherRun", div.CRName, "missing", d.Missing,
	}
	if div.Clean {
		s.log.Info("assignment divergence: clean — assignment-driven decision matches the watcher's run", kv...)
		return
	}
	s.log.Error(fmt.Errorf("%w: %v", errDivergence, div.Reasons), "assignment divergence detected", kv...)
}

func nextBackoff(b time.Duration) time.Duration {
	if b *= 2; b > backoffMax {
		return backoffMax
	}
	return b
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// isLaneRejected reports whether an error is castle rejecting the identity
// outright (PERMISSION_DENIED / UNAUTHENTICATED / UNIMPLEMENTED) — states a
// retry cannot repair.
func isLaneRejected(err error) bool {
	code := connect.CodeOf(err)
	return code == connect.CodePermissionDenied ||
		code == connect.CodeUnauthenticated ||
		code == connect.CodeUnimplemented
}
