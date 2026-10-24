package control

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	connect "connectrpc.com/connect"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"

	v1 "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	criteriav1connect "github.com/brokenbots/criteria/sdk/pb/criteria/v1/criteriav1connect"
)

func testRun(name, ticket, repoURL, class, workflow string) *criteriav1.CriteriaRun {
	run := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "criteria-jobs",
			Labels:    map[string]string{DefaultLabelTicket: ticket},
		},
		Spec: criteriav1.CriteriaRunSpec{RepoURL: repoURL},
	}
	if workflow != "" {
		run.Spec.Workflow = &criteriav1.RunWorkflow{Name: workflow, Class: class}
	}
	return run
}

func TestTranslateMapsAssignmentLabels(t *testing.T) {
	cfg := Config{CriteriaID: "criteria-k8s-operator"}
	d := Translate(cfg, Assignment{
		RunID:        "run-11947c6b",
		WorkflowName: "linear-intake",
		Labels: map[string]string{
			"ticket":                            "KB-319",
			"criteria.brokenbots.dev/repo-url":  "https://github.com/org/repo",
			"criteria.brokenbots.dev/class":     "dev",
			"criteria.brokenbots.dev/project":   "castor",
			"criteria.brokenbots.dev/unrelated": "ignored",
		},
	})
	assert.Equal(t, "KB-319", d.Ticket)
	assert.Equal(t, "castor", d.Project)
	assert.Equal(t, "https://github.com/org/repo", d.RepoURL)
	assert.Equal(t, "dev", d.Class)
	assert.Equal(t, "linear-intake", d.WorkflowName)
	assert.Empty(t, d.Missing)
}

func TestTranslateReportsMissingAttribution(t *testing.T) {
	d := Translate(Config{}, Assignment{WorkflowName: ""})
	assert.Empty(t, d.Ticket)
	assert.Empty(t, d.RepoURL)
	assert.ElementsMatch(t, []string{"ticket", "repo-url", "class", "workflow_name"}, d.Missing)
}

func TestTranslateHonorsCustomLabelKeys(t *testing.T) {
	cfg := Config{
		LabelTicket:  "kanboard-id",
		LabelRepoURL: "repo",
		LabelClass:   "queue-class",
		LabelProject: "team",
	}
	d := Translate(cfg, Assignment{
		WorkflowName: "wf",
		Labels: map[string]string{
			"kanboard-id": "KB-1", "repo": "https://x", "queue-class": "triage", "team": "ops",
		},
	})
	assert.Equal(t, "KB-1", d.Ticket)
	assert.Equal(t, "https://x", d.RepoURL)
	assert.Equal(t, "triage", d.Class)
	assert.Equal(t, "ops", d.Project)
	assert.Empty(t, d.Missing)
}

func TestWatchdogCompareMatrix(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, criteriav1.AddToScheme(scheme))
	build := func(objs ...*criteriav1.CriteriaRun) *Watchdog {
		b := fake.NewClientBuilder().WithScheme(scheme)
		for _, o := range objs {
			b = b.WithObjects(o)
		}
		return &Watchdog{Client: b.Build(), Namespace: "criteria-jobs"}
	}
	ctx := context.Background()

	t.Run("clean match", func(t *testing.T) {
		wd := build(testRun("kb319-100", "KB-319", "https://github.com/org/repo", "dev", "linear-intake"))
		div := wd.CheckDecision(ctx, Decision{
			Ticket: "KB-319", RepoURL: "https://github.com/org/repo",
			Class: "dev", WorkflowName: "linear-intake",
		})
		assert.True(t, div.Clean)
		assert.Empty(t, div.Reasons)
		assert.Equal(t, "kb319-100", div.CRName)
	})

	t.Run("repoURL mismatch diverges", func(t *testing.T) {
		wd := build(testRun("kb319-100", "KB-319", "https://github.com/org/repo", "dev", "linear-intake"))
		div := wd.CheckDecision(ctx, Decision{Ticket: "KB-319", RepoURL: "https://github.com/org/other",
			Class: "dev", WorkflowName: "linear-intake"})
		assert.False(t, div.Clean)
		require.Len(t, div.Reasons, 1)
		assert.Contains(t, div.Reasons[0], "repoURL")
	})

	t.Run("class mismatch diverges", func(t *testing.T) {
		wd := build(testRun("kb319-100", "KB-319", "https://github.com/org/repo", "dev", "linear-intake"))
		div := wd.CheckDecision(ctx, Decision{Ticket: "KB-319", RepoURL: "https://github.com/org/repo",
			Class: "triage", WorkflowName: "linear-intake"})
		assert.False(t, div.Clean)
		assert.Contains(t, div.Reasons[0], "class")
	})

	t.Run("workflow mismatch diverges", func(t *testing.T) {
		wd := build(testRun("kb319-100", "KB-319", "https://github.com/org/repo", "dev", "linear-intake"))
		div := wd.CheckDecision(ctx, Decision{Ticket: "KB-319", RepoURL: "https://github.com/org/repo",
			Class: "dev", WorkflowName: "linear-triage"})
		assert.False(t, div.Clean)
		assert.Contains(t, div.Reasons[0], "workflow")
	})

	t.Run("watcher run with no workflow diverges on workflow", func(t *testing.T) {
		wd := build(testRun("kb319-100", "KB-319", "https://github.com/org/repo", "", ""))
		div := wd.CheckDecision(ctx, Decision{Ticket: "KB-319", RepoURL: "https://github.com/org/repo",
			Class: "dev", WorkflowName: "linear-intake"})
		assert.False(t, div.Clean)
		assert.Contains(t, div.Reasons[0], "workflow")
	})

	t.Run("no watcher run diverges", func(t *testing.T) {
		wd := &Watchdog{Client: fake.NewClientBuilder().WithScheme(scheme).Build(), Namespace: "criteria-jobs"}
		div := wd.CheckDecision(ctx, Decision{Ticket: "KB-999", RepoURL: "https://r", Class: "dev",
			WorkflowName: "wf"})
		assert.False(t, div.Clean)
		assert.Empty(t, div.CRName)
		assert.Contains(t, div.Reasons[0], "no watcher CriteriaRun")
	})

	t.Run("missing assignment labels diverge", func(t *testing.T) {
		wd := build(testRun("kb319-100", "KB-319", "https://github.com/org/repo", "dev", "linear-intake"))
		d := Translate(Config{}, Assignment{RunID: "r1"})
		div := wd.CheckDecision(ctx, d)
		assert.False(t, div.Clean)
		assert.Contains(t, div.Reasons[0], "missing")
	})

	t.Run("newest run wins on refire", func(t *testing.T) {
		wd := build(
			testRun("kb319-100", "KB-319", "https://github.com/org/repo", "dev", "linear-intake"),
			testRun("kb319-200", "KB-319", "https://github.com/org/other", "dev", "linear-intake"),
		)
		div := wd.CheckDecision(ctx, Decision{Ticket: "KB-319", RepoURL: "https://github.com/org/other",
			Class: "dev", WorkflowName: "linear-intake"})
		assert.Equal(t, "kb319-200", div.CRName)
		assert.True(t, div.Clean)
	})

	t.Run("ticket missing diverges with reason", func(t *testing.T) {
		wd := build(testRun("kb319-100", "KB-319", "https://github.com/org/repo", "dev", "linear-intake"))
		div := wd.CheckDecision(ctx, Decision{RepoURL: "https://github.com/org/repo"})
		assert.False(t, div.Clean)
		assert.NotEmpty(t, div.Reasons)
	})
}

func TestDisabled(t *testing.T) {
	assert.True(t, Disabled(Config{}))
	assert.False(t, Disabled(Config{Addr: "castle:8080"}))
}

func TestNextBackoffCaps(t *testing.T) {
	assert.Equal(t, 2*time.Second, nextBackoff(time.Second))
	assert.Equal(t, backoffMax, nextBackoff(backoffMax))
	assert.Equal(t, backoffMax, nextBackoff(backoffMax+time.Hour))
}

// captureSink records logr entries for assertions on the divergence log.
type captureSink struct {
	mu      sync.Mutex
	entries []captureEntry
	errs    []error
}

type captureEntry struct {
	level int
	msg   string
}

func (s *captureSink) Init(logr.RuntimeInfo) {}
func (s *captureSink) Enabled(int) bool      { return true }
func (s *captureSink) WithName(string) logr.LogSink {
	return s
}
func (s *captureSink) WithCallDepth(int) logr.LogSink {
	return s
}
func (s *captureSink) WithValues(...any) logr.LogSink {
	return s
}
func (s *captureSink) Info(level int, msg string, _ ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, captureEntry{level: level, msg: msg})
}
func (s *captureSink) Error(err error, msg string, _ ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errs = append(s.errs, err)
	s.entries = append(s.entries, captureEntry{level: -1, msg: msg})
}

func (s *captureSink) snapshot() (messages []string, errs []error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.entries {
		messages = append(messages, e.msg)
	}
	return messages, append([]error{}, s.errs...)
}

// controlCriteriaStub implements the slice of the SDK's CriteriaServiceHandler
// the shadow-run consumer consumes: Control streams one ControlReady followed
// by the configured assignments, then ends the stream. controlErr (when set)
// makes every Control call fail; dials counts handler invocations.
type controlCriteriaStub struct {
	criteriav1connect.CriteriaServiceHandler

	mu          sync.Mutex
	assignments []*v1.WorkflowAssignment
	controlErr  error
	dials       int
}

func (s *controlCriteriaStub) Control(ctx context.Context, _ *connect.Request[v1.ControlSubscribeRequest], stream *connect.ServerStream[v1.ControlMessage]) error {
	s.mu.Lock()
	s.dials++
	asgs := append([]*v1.WorkflowAssignment{}, s.assignments...)
	err := s.controlErr
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if err := stream.Send(&v1.ControlMessage{Command: &v1.ControlMessage_ControlReady{ControlReady: &v1.ControlReady{}}}); err != nil {
		return err
	}
	for _, a := range asgs {
		if err := stream.Send(&v1.ControlMessage{Command: &v1.ControlMessage_WorkflowAssignment{WorkflowAssignment: a}}); err != nil {
			return err
		}
	}
	return nil
}

func (s *controlCriteriaStub) dialCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dials
}

// captureComparator records decisions and verdicts, answering clean.
type captureComparator struct {
	mu        sync.Mutex
	decisions []Decision
	diverged  []Divergence
}

func (c *captureComparator) CheckDecision(_ context.Context, d Decision) Divergence {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.decisions = append(c.decisions, d)
	return Divergence{Clean: true, CRName: "kb-1-100"}
}

func TestStreamDeliversAssignmentsEndToEnd(t *testing.T) {
	stub := &controlCriteriaStub{assignments: []*v1.WorkflowAssignment{
		{
			RunId:        "run-1",
			WorkflowName: "linear-intake",
			Labels: map[string]string{
				"ticket":                           "KB-319",
				"criteria.brokenbots.dev/repo-url": "https://github.com/org/repo",
				"criteria.brokenbots.dev/class":    "dev",
			},
		},
		{
			RunId:        "run-2",
			WorkflowName: "linear-triage",
			Labels: map[string]string{
				"ticket":                           "KB-320",
				"criteria.brokenbots.dev/repo-url": "https://github.com/org/other",
				"criteria.brokenbots.dev/class":    "triage",
			},
		},
	}}
	path, handler := criteriav1connect.NewCriteriaServiceHandler(stub)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewServer(h2c.NewHandler(mux, &http2.Server{}))
	t.Cleanup(srv.Close)

	sink := &captureSink{}
	compare := &captureComparator{}
	cfg := Config{Addr: srv.URL, CriteriaID: "criteria-k8s-test"}
	stream := StartStream(cfg, compare, logr.New(sink))
	stream.dial = defaultDialer(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- stream.Run(ctx) }()

	// Both assignments flow through translate + compare; then the ctx kill
	// switch stops the loop cleanly instead of the stream running forever.
	if !assert.Eventually(t, func() bool {
		compare.mu.Lock()
		defer compare.mu.Unlock()
		return len(compare.decisions) == 2
	}, 5*time.Second, 20*time.Millisecond, "assignments must be delivered to the comparator") {
		cancel()
		msgs, errs := sink.snapshot()
		t.Logf("log messages: %v", msgs)
		t.Logf("log errors: %v", errs)
		t.Logf("dial count: %d", stub.dialCount())
		compare.mu.Lock()
		t.Logf("decisions: %#v", compare.decisions)
		compare.mu.Unlock()
		return
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("consumer did not stop after context cancel")
	}

	compare.mu.Lock()
	first, second := compare.decisions[0], compare.decisions[1]
	compare.mu.Unlock()
	assert.Equal(t, "KB-319", first.Ticket)
	assert.Equal(t, "linear-intake", first.WorkflowName)
	assert.Equal(t, "https://github.com/org/repo", first.RepoURL)
	assert.Equal(t, "dev", first.Class)
	assert.Empty(t, first.Missing)
	assert.Equal(t, "KB-320", second.Ticket)
	assert.Equal(t, "triage", second.Class)

	messages, errs := sink.snapshot()
	require.NotEmpty(t, messages)
	assert.Contains(t, messages[0], "assignment divergence: clean")
	assert.Empty(t, errs, "a clean match must not be an error-level signal")
}

func TestStreamNonAssignmentCommandsIgnored(t *testing.T) {
	// Only ControlReady: no decision must reach the comparator.
	stub := &controlCriteriaStub{}
	path, handler := criteriav1connect.NewCriteriaServiceHandler(stub)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewServer(h2c.NewHandler(mux, &http2.Server{}))
	t.Cleanup(srv.Close)

	compare := &captureComparator{}
	cfg := Config{Addr: srv.URL, CriteriaID: "criteria-k8s-test"}
	stream := StartStream(cfg, compare, logr.Discard())
	stream.dial = defaultDialer(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- stream.Run(ctx) }()

	require.Eventually(t, func() bool { return stub.dialCount() >= 2 }, 5*time.Second, 10*time.Millisecond,
		"the stream must reconnect after cleanly ending")
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("consumer did not stop after context cancel")
	}
	compare.mu.Lock()
	count := len(compare.decisions)
	compare.mu.Unlock()
	assert.Equal(t, 0, count, "ControlReady must not be treated as an assignment")
}

func TestStreamPermanentDisableOnPermissionDenied(t *testing.T) {
	stub := &controlCriteriaStub{controlErr: connect.NewError(connect.CodePermissionDenied, errors.New("rejected"))}
	path, handler := criteriav1connect.NewCriteriaServiceHandler(stub)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewServer(h2c.NewHandler(mux, &http2.Server{}))
	t.Cleanup(srv.Close)

	sink := &captureSink{}
	cfg := Config{Addr: srv.URL, CriteriaID: "criteria-k8s-test"}
	stream := StartStream(cfg, &captureComparator{}, logr.New(sink))
	stream.dial = defaultDialer(cfg)

	done := make(chan error, 1)
	go func() { done <- stream.Run(context.Background()) }()
	select {
	case err := <-done:
		require.Error(t, err)
		assert.Equal(t, connect.CodePermissionDenied, connect.CodeOf(err))
	case <-time.After(5 * time.Second):
		t.Fatal("identity rejection must stop the run, not retry forever")
	}
	require.Equal(t, 1, stub.dialCount(), "one rejection is one dial: the lane permanently disables itself")
	messages, errs := sink.snapshot()
	require.NotEmpty(t, errs, "the disable must be a loud, scrapeable log")
	found := false
	for _, m := range messages {
		if strings.Contains(m, "shadow-run watchdog disabled") {
			found = true
		}
	}
	assert.True(t, found, "disable log must name the lane, got %v", messages)
}

// replaySink is an assignSink that yields queued messages then a clean
// EOF — the minimal live stream a scripted dialer hands back after a
// non-lethal failure, so the test can prove Run actually consumed the
// retry dial's sink (the delivered assignment reaches the comparator)
// instead of merely counting dial attempts.
type replaySink struct {
	msgs []*v1.ControlMessage
}

func (s *replaySink) next() (*v1.ControlMessage, error) {
	if len(s.msgs) == 0 {
		return nil, io.EOF
	}
	m := s.msgs[0]
	s.msgs = s.msgs[1:]
	return m, nil
}

func (s *replaySink) close() {}

func TestStreamRetriesNonLethalDialFailures(t *testing.T) {
	// Resilience contract: one non-lethal dial failure (Unavailable) must
	// NOT permanently disable the lane — Run dials again, and the retry's
	// sink is CONSUMED: an assignment delivered on the second dial reaches
	// the comparator. Both sides are asserted observable-side, so a Run
	// that gives up after the first failure or leaks a nil sink fails here.
	prevBase, prevMax := backoffBase, backoffMax
	backoffBase, backoffMax = 5*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { backoffBase, backoffMax = prevBase, prevMax })

	var mu sync.Mutex
	dials := 0
	compare := &captureComparator{}
	stream := &Stream{compare: compare, log: logr.Discard()}
	stream.dial = func(ctx context.Context) (assignSink, error) {
		mu.Lock()
		n := dials
		dials++
		mu.Unlock()
		switch n {
		case 0:
			return nil, connect.NewError(connect.CodeUnavailable, errors.New("castle down"))
		case 1:
			// The retry dial: one live assignment, then a clean EOF;
			// consumption is proven by the comparator receiving it.
			return &replaySink{msgs: []*v1.ControlMessage{{
				Command: &v1.ControlMessage_WorkflowAssignment{WorkflowAssignment: &v1.WorkflowAssignment{
					RunId:        "run-retry",
					WorkflowName: "triage_v1",
					Labels: map[string]string{
						"ticket":                           "kb-319-retry",
						"criteria.brokenbots.dev/repo-url": "https://github.com/org/repo",
						"criteria.brokenbots.dev/class":    "dev",
					},
				}},
			}}}, nil
		default:
			return &replaySink{}, nil
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- stream.Run(ctx) }()

	require.Eventually(t, func() bool {
		compare.mu.Lock()
		defer compare.mu.Unlock()
		return len(compare.decisions) == 1
	}, 5*time.Second, 10*time.Millisecond,
		"the assignment delivered on the retry dial must reach the comparator (the sink was consumed)")
	assert.Equal(t, Decision{Ticket: "kb-319-retry", RepoURL: "https://github.com/org/repo",
		Class: "dev", WorkflowName: "triage_v1"}, compare.decisions[0])

	mu.Lock()
	count := dials
	mu.Unlock()
	assert.GreaterOrEqual(t, count, 2, "the non-lethal failure must be retried, not permanently disable the lane")

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("consumer did not stop after context cancel")
	}
}
