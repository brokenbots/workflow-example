// KB-24: the stall watchdog's step-progress tracking in the castle
// Observation. A run wedged in a step whose adapter session died emits only
// CriteriaHeartbeats: the newest step-progress timestamp the drain folds out
// of the stream is the watchdog's baseline, so heartbeats, terminal
// envelopes and non-activity adapter chatter must never advance it, while
// the copilot adapter's agent-activity AdapterEvents (agent.message,
// tool.invocation, tool.result, permission.request, limit.reached,
// outcome.finalized — the adapter ships ALL agent activity as structured
// events, its v0.5.8 Log path drops log lines) must count, or the watchdog
// would measure step duration and fail healthy long turns.
package castle

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/brokenbots/workflow-example/criteria-k8s/internal/criteria/pb/criteria/v1"
)

// withTs stamps an envelope with a timestamp (castle stores agent-supplied
// event timestamps when present).
func withTs(env *v1.Envelope, ts time.Time) *v1.Envelope {
	env.Ts = timestamppb.New(ts)
	return env
}

// A mixed stream: RunStarted and StepEntered are progress, the interleaved
// heartbeat and adapter bookkeeping (scope provisioning) are not, so
// LastProgress stays at the last step event instead of advancing to the
// newest stream timestamp.
func TestObserveTracksStepProgress(t *testing.T) {
	now := time.Now().UTC()
	startedAt := now.Add(-20 * time.Minute)
	enteredAt := now.Add(-10 * time.Minute)

	server := &stubServer{
		agents: []*v1.Agent{{CriteriaId: "crit-24", Name: "cri-24-abc12", Status: "online"}},
		runs:   []*v1.Run{{RunId: "run-24", CriteriaId: "crit-24", WorkflowName: "kanboard_develop_v1", Status: "running"}},
		events: map[string][]*v1.Envelope{
			"run-24": {
				withTs(&v1.Envelope{RunId: "run-24", Seq: 1, Payload: &v1.Envelope_RunStarted{RunStarted: &v1.RunStarted{WorkflowName: "kanboard_develop_v1"}}}, startedAt),
				withTs(&v1.Envelope{RunId: "run-24", Seq: 2, Payload: &v1.Envelope_StepEntered{StepEntered: &v1.StepEntered{Step: "run_handler", Adapter: "copilot"}}}, enteredAt),
				withTs(&v1.Envelope{RunId: "run-24", Seq: 3, Payload: &v1.Envelope_CriteriaHeartbeat{CriteriaHeartbeat: &v1.CriteriaHeartbeat{CriteriaId: "crit-24"}}}, now),
				withTs(&v1.Envelope{RunId: "run-24", Seq: 4, Payload: &v1.Envelope_AdapterEvent{AdapterEvent: &v1.AdapterEvent{Adapter: "copilot", Kind: "adapter.lifecycle.provision_wanted"}}}, now),
			},
		},
	}

	c := New(Config{Addr: newTestServer(t, server).URL}, nil)
	obs, err := c.Observe(context.Background(), "cri-24", "")
	require.NoError(t, err)
	assert.Equal(t, "run-24", obs.RunID)
	assert.True(t, obs.LastProgress.Equal(enteredAt),
		"LastProgress must be the newest step-progress event, not the newest heartbeat/adapter event: got %v", obs.LastProgress)
}

// A heartbeat-only stream (the wedged signature) folds out a zero
// LastProgress: the controller stamps its own baseline instead.
func TestObserveProgressZeroWhenHeartbeatsOnly(t *testing.T) {
	now := time.Now().UTC()
	server := &stubServer{
		agents: []*v1.Agent{{CriteriaId: "crit-24", Name: "cri-24-abc12", Status: "online"}},
		runs:   []*v1.Run{&v1.Run{RunId: "run-24", CriteriaId: "crit-24", WorkflowName: "kanboard_develop_v1", Status: "running"}},
		events: map[string][]*v1.Envelope{
			"run-24": {
				withTs(&v1.Envelope{RunId: "run-24", Seq: 1, Payload: &v1.Envelope_CriteriaHeartbeat{CriteriaHeartbeat: &v1.CriteriaHeartbeat{CriteriaId: "crit-24"}}}, now),
				withTs(&v1.Envelope{RunId: "run-24", Seq: 2, Payload: &v1.Envelope_AdapterEvent{AdapterEvent: &v1.AdapterEvent{Adapter: "copilot", Kind: "adapter.lifecycle.provision_wanted"}}}, now),
			},
		},
	}

	c := New(Config{Addr: newTestServer(t, server).URL}, nil)
	obs, err := c.Observe(context.Background(), "cri-24", "")
	require.NoError(t, err)
	assert.True(t, obs.LastProgress.IsZero(),
		"heartbeat and adapter events must not count as step progress")
}

// The copilot adapter ships all agent activity as structured AdapterEvents
// (its Log path drops log lines), so every genuine activity kind must
// advance the progress baseline or the watchdog would measure step
// duration and fail healthy long turns.
func TestObserveCopilotActivityKindsCountAsProgress(t *testing.T) {
	activityKinds := []string{
		"agent.message",
		"tool.invocation",
		"tool.result",
		"permission.request",
		"limit.reached",
		"outcome.finalized",
	}
	for _, kind := range activityKinds {
		t.Run(kind, func(t *testing.T) {
			now := time.Now().UTC().Truncate(time.Second)
			enteredAt := now.Add(-40 * time.Minute)
			server := &stubServer{
				agents: []*v1.Agent{{CriteriaId: "crit-24", Name: "cri-24-abc12", Status: "online"}},
				runs:   []*v1.Run{&v1.Run{RunId: "run-24", CriteriaId: "crit-24", WorkflowName: "linear_develop_v1", Status: "running"}},
				events: map[string][]*v1.Envelope{
					"run-24": {
						withTs(&v1.Envelope{RunId: "run-24", Seq: 1, Payload: &v1.Envelope_StepEntered{StepEntered: &v1.StepEntered{Step: "develop", Adapter: "copilot"}}}, enteredAt),
						withTs(&v1.Envelope{RunId: "run-24", Seq: 2, Payload: &v1.Envelope_AdapterEvent{AdapterEvent: &v1.AdapterEvent{Adapter: "copilot", Kind: kind}}}, now),
					},
				},
			}

			c := New(Config{Addr: newTestServer(t, server).URL}, nil)
			obs, err := c.Observe(context.Background(), "cri-24", "run-24")
			require.NoError(t, err)
			assert.True(t, obs.LastProgress.Equal(now),
				"copilot activity kind %q must count as step progress: got %v", kind, obs.LastProgress)
		})
	}
}

// A healthy copilot turn running longer than the stall window: the step
// entered 40m ago (a legal 60m-budget turn) and emits its usual in-step
// stream — agent.message deltas, a tool round-trip, a ~10m gate-held
// permission wait and heartbeats — with NO StepLog and NO StepOutcome. The
// newest activity event is 5m old, so the observation stays inside a 30m
// window and the watchdog must not fire on step duration.
func TestObserveHealthyCopilotStreamLongerThanWindowKeepsFreshProgress(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	enteredAt := now.Add(-40 * time.Minute)
	ae := func(kind string) *v1.Envelope_AdapterEvent {
		return &v1.Envelope_AdapterEvent{AdapterEvent: &v1.AdapterEvent{Adapter: "copilot", Kind: kind}}
	}
	heartbeat := &v1.Envelope_CriteriaHeartbeat{CriteriaHeartbeat: &v1.CriteriaHeartbeat{CriteriaId: "crit-24"}}
	events := []*v1.Envelope{
		withTs(&v1.Envelope{RunId: "run-24", Seq: 1, Payload: &v1.Envelope_StepEntered{StepEntered: &v1.StepEntered{Step: "develop", Adapter: "copilot"}}}, enteredAt),
		withTs(&v1.Envelope{RunId: "run-24", Seq: 2, Payload: ae("agent.message")}, now.Add(-35*time.Minute)),
		withTs(&v1.Envelope{RunId: "run-24", Seq: 3, Payload: ae("tool.invocation")}, now.Add(-30*time.Minute)),
		withTs(&v1.Envelope{RunId: "run-24", Seq: 4, Payload: ae("tool.result")}, now.Add(-29*time.Minute)),
		withTs(&v1.Envelope{RunId: "run-24", Seq: 5, Payload: ae("agent.message")}, now.Add(-25*time.Minute)),
		withTs(&v1.Envelope{RunId: "run-24", Seq: 6, Payload: ae("permission.request")}, now.Add(-22*time.Minute)),
		withTs(&v1.Envelope{RunId: "run-24", Seq: 7, Payload: ae("tool.invocation")}, now.Add(-12*time.Minute)),
		withTs(&v1.Envelope{RunId: "run-24", Seq: 8, Payload: ae("tool.result")}, now.Add(-11*time.Minute)),
		withTs(&v1.Envelope{RunId: "run-24", Seq: 9, Payload: ae("agent.message")}, now.Add(-6*time.Minute)),
		withTs(&v1.Envelope{RunId: "run-24", Seq: 10, Payload: ae("agent.message")}, now.Add(-5*time.Minute)),
	}
	// Heartbeats every minute throughout the turn: they keep the stream
	// alive but must never advance the baseline.
	seq := uint64(11)
	for at := now.Add(-40 * time.Minute); at.Before(now); at = at.Add(time.Minute) {
		events = append(events, withTs(&v1.Envelope{RunId: "run-24", Seq: seq, Payload: heartbeat}, at))
		seq++
	}
	server := &stubServer{
		agents: []*v1.Agent{{CriteriaId: "crit-24", Name: "cri-24-abc12", Status: "online"}},
		runs:   []*v1.Run{&v1.Run{RunId: "run-24", CriteriaId: "crit-24", WorkflowName: "linear_develop_v1", Status: "running"}},
		events: map[string][]*v1.Envelope{"run-24": events},
	}

	c := New(Config{Addr: newTestServer(t, server).URL}, nil)
	obs, err := c.Observe(context.Background(), "cri-24", "run-24")
	require.NoError(t, err)
	assert.True(t, obs.LastProgress.Equal(now.Add(-5*time.Minute)),
		"the healthy stream's newest activity event must be the watchdog baseline: got %v", obs.LastProgress)
}

// The wedged stream with error chatter: the step entered 40m ago and since
// then only heartbeats and non-activity AdapterEvents arrive (host crash
// handling, malformed-payload and finalize-failure bookkeeping, an unknown
// future kind). None of that chatter may refresh the baseline, so
// LastProgress stays at the step entry — 40m old, outside a 30m window —
// and the watchdog still trips.
func TestObserveWedgeChatterKeepsProgressStale(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	enteredAt := now.Add(-40 * time.Minute)
	chatterKinds := []string{
		"session.crash",
		"session.respawned",
		"permission.denied",
		"outcome.failure",
		"step.outcome.unknown",
		"some.future.adapter.kind",
	}
	var events []*v1.Envelope
	events = append(events, withTs(&v1.Envelope{RunId: "run-24", Seq: 1, Payload: &v1.Envelope_StepEntered{StepEntered: &v1.StepEntered{Step: "pr_review", Adapter: "copilot"}}}, enteredAt))
	seq := uint64(2)
	for _, kind := range chatterKinds {
		ae := &v1.Envelope_AdapterEvent{AdapterEvent: &v1.AdapterEvent{Adapter: "copilot", Kind: kind}}
		events = append(events, withTs(&v1.Envelope{RunId: "run-24", Seq: seq, Payload: ae}, now))
		events = append(events, withTs(&v1.Envelope{RunId: "run-24", Seq: seq+1, Payload: &v1.Envelope_CriteriaHeartbeat{CriteriaHeartbeat: &v1.CriteriaHeartbeat{CriteriaId: "crit-24"}}}, now))
		seq += 2
	}
	server := &stubServer{
		agents: []*v1.Agent{{CriteriaId: "crit-24", Name: "cri-24-abc12", Status: "online"}},
		runs:   []*v1.Run{&v1.Run{RunId: "run-24", CriteriaId: "crit-24", WorkflowName: "linear_develop_v1", Status: "running"}},
		events: map[string][]*v1.Envelope{"run-24": events},
	}

	c := New(Config{Addr: newTestServer(t, server).URL}, nil)
	obs, err := c.Observe(context.Background(), "cri-24", "run-24")
	require.NoError(t, err)
	assert.True(t, obs.LastProgress.Equal(enteredAt),
		"non-activity adapter chatter must not refresh the watchdog baseline: got %v", obs.LastProgress)
}

// A terminal envelope and a step event in the same stream: the step event
// still counts as progress (the watchdog only ever consumes it for
// non-terminal phases).
func TestObserveStepProgressIgnoresTerminalEnvelope(t *testing.T) {
	now := time.Now().UTC()
	enteredAt := now.Add(-5 * time.Minute)
	server := &stubServer{
		agents: []*v1.Agent{{CriteriaId: "crit-24", Name: "cri-24-abc12", Status: "online"}},
		runs:   []*v1.Run{&v1.Run{RunId: "run-24", CriteriaId: "crit-24", WorkflowName: "kanboard_develop_v1", Status: "failed"}},
		events: map[string][]*v1.Envelope{
			"run-24": {
				withTs(&v1.Envelope{RunId: "run-24", Seq: 1, Payload: &v1.Envelope_StepEntered{StepEntered: &v1.StepEntered{Step: "run_handler"}}}, enteredAt),
				withTs(&v1.Envelope{RunId: "run-24", Seq: 2, Payload: &v1.Envelope_RunFailed{RunFailed: &v1.RunFailed{Reason: "boom"}}}, now),
			},
		},
	}

	c := New(Config{Addr: newTestServer(t, server).URL}, nil)
	obs, err := c.Observe(context.Background(), "cri-24", "")
	require.NoError(t, err)
	require.NotNil(t, obs.Terminal)
	assert.False(t, obs.Terminal.Success)
	assert.True(t, obs.LastProgress.Equal(enteredAt))
}

// The progress tracker is incremental: a second Observe advancing the
// stream moves LastProgress forward, and the drain replay after a client
// restart recomputes it from history (the cursor restarts at 0).
func TestObserveProgressAdvancesAcrossDrains(t *testing.T) {
	now := time.Now().UTC()
	server := &stubServer{
		agents: []*v1.Agent{{CriteriaId: "crit-24", Name: "cri-24-abc12", Status: "online"}},
		runs:   []*v1.Run{&v1.Run{RunId: "run-24", CriteriaId: "crit-24", WorkflowName: "kanboard_develop_v1", Status: "running"}},
		events: map[string][]*v1.Envelope{
			"run-24": {
				withTs(&v1.Envelope{RunId: "run-24", Seq: 1, Payload: &v1.Envelope_StepEntered{StepEntered: &v1.StepEntered{Step: "fetch_ticket"}}}, now.Add(-10*time.Minute)),
				withTs(&v1.Envelope{RunId: "run-24", Seq: 2, Payload: &v1.Envelope_StepEntered{StepEntered: &v1.StepEntered{Step: "run_handler"}}}, now),
			},
		},
	}

	c := New(Config{Addr: newTestServer(t, server).URL}, nil)
	obs, err := c.Observe(context.Background(), "cri-24", "run-24")
	require.NoError(t, err)
	assert.True(t, obs.LastProgress.Equal(now))

	// A restart is a fresh client: the replay from seq 0 re-derives the
	// same newest step timestamp.
	restarted := New(Config{Addr: newTestServer(t, server).URL}, nil)
	obs, err = restarted.Observe(context.Background(), "cri-24", "run-24")
	require.NoError(t, err)
	assert.True(t, obs.LastProgress.Equal(now))
}