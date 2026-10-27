package controller

// KB-344 castle run id carry: with the run-event proxy in place the runner
// never dials castle, so a proxy-minted run has no engine agent row for
// castle discovery to find. The observation must key off the annotation
// carry — the assignment lane's dispatch stamp and the proxy's watcher
// stamp — instead of the stamped status alone, and the observed id
// converges into status. (Internal test package: observeCastle and
// castleRunIDFromRun are unexported controller internals.)

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/castle"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/control"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/jobbuilder"
)

// carryStub records the identity observeCastle resolved the carry chain to.
type carryStub struct {
	observation *castle.Observation
	lastKnownID string
}

func (s *carryStub) Observe(ctx context.Context, runnerJob, knownRunID string) (*castle.Observation, error) {
	s.lastKnownID = knownRunID
	if s.observation == nil {
		return &castle.Observation{}, nil
	}
	return s.observation, nil
}

func (s *carryStub) Disabled() bool { return false }

func TestObserveCastleCarriesRunIDFromAnnotations(t *testing.T) {
	newRun := func(annotations map[string]string) *criteriav1.CriteriaRun {
		return &criteriav1.CriteriaRun{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "cri-42",
				Namespace:   "default",
				UID:         "uid-cri-42",
				Annotations: annotations,
			},
			Spec: criteriav1.CriteriaRunSpec{TicketID: "CRI-42"},
		}
	}

	// Watcher lane: the proxy stamped its minted castle run id.
	run := newRun(map[string]string{jobbuilder.AnnotationProxyCastleRunID: "proxy-run-1"})
	update := run.DeepCopy()
	stub := &carryStub{observation: &castle.Observation{RunID: "proxy-run-1"}}
	r := &CriteriaRunReconciler{Castle: stub}
	_, err := r.observeCastle(context.Background(), run, update, logr.Discard())
	require.NoError(t, err)
	assert.Equal(t, "proxy-run-1", stub.lastKnownID, "the proxy annotation is the known run id")
	assert.Equal(t, "proxy-run-1", update.Status.CastleRunID, "the observed id converges into status")

	// Dispatch lane: the agent stamped the assignment's castle run id.
	run = newRun(map[string]string{control.AnnAssignmentRunID: "asg-run-1"})
	update = run.DeepCopy()
	stub = &carryStub{observation: &castle.Observation{RunID: "asg-run-1"}}
	r = &CriteriaRunReconciler{Castle: stub}
	_, err = r.observeCastle(context.Background(), run, update, logr.Discard())
	require.NoError(t, err)
	assert.Equal(t, "asg-run-1", stub.lastKnownID)
	assert.Equal(t, "asg-run-1", update.Status.CastleRunID)

	// Precedence: a stamped status wins over a stale annotation.
	run = newRun(map[string]string{
		control.AnnAssignmentRunID:            "asg-run-1",
		jobbuilder.AnnotationProxyCastleRunID: "proxy-run-1",
	})
	run.Status.CastleRunID = "status-run-1"
	update = run.DeepCopy()
	stub = &carryStub{observation: &castle.Observation{RunID: "status-run-1"}}
	r = &CriteriaRunReconciler{Castle: stub}
	_, err = r.observeCastle(context.Background(), run, update, logr.Discard())
	require.NoError(t, err)
	assert.Equal(t, "status-run-1", stub.lastKnownID)
}
