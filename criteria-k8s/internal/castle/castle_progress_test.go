// KB-24: the stall watchdog's step-progress tracking in the castle
// Observation. A run wedged in a step whose adapter session died emits only
// CriteriaHeartbeats (and the adapter's internal error loop surfaces as
// AdapterEvents): the newest step-progress timestamp the drain folds out of
// the stream is the watchdog's baseline, so heartbeats and adapter chatter
// must never advance it, and terminal envelopes must not either (a terminal
// run needs no watchdog).
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
// heartbeat and adapter chatter are not, so LastProgress stays at the last
// step event instead of advancing to the newest stream timestamp.
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