package controller

// KB-358: the CR phase derivation must follow the success terminal
// regardless of which success vocabulary the workflow's verdict uses. The
// kanboard triage rearm terminal (success=true final_state=
// ready_for_development) derives Succeeded — an operator must not
// re-derive the run Failed after a clean triage terminal.

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/castle"
)

func TestObserveCastleRearmVerdictDerivesSucceeded(t *testing.T) {
	run := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cri-151",
			Namespace: "default",
			UID:       "uid-cri-151",
		},
		Spec: criteriav1.CriteriaRunSpec{TicketID: "KB-151"},
	}
	update := run.DeepCopy()
	stub := &carryStub{observation: &castle.Observation{
		RunID:    "asg-run-1",
		Terminal: &castle.Terminal{Success: true, FinalState: "ready_for_development"},
	}}
	r := &CriteriaRunReconciler{Castle: stub}

	obs, err := r.observeCastle(context.Background(), run, update, logr.Discard())
	require.NoError(t, err)
	require.NotNil(t, obs.Terminal, "the observation carries the castle terminal")
	assert.True(t, obs.Terminal.Success)
	assert.Equal(t, "ready_for_development", obs.Terminal.FinalState)

	assert.Equal(t, criteriav1.PhaseSucceeded, update.Status.Phase,
		"the phase derives from the success terminal regardless of the verdict's vocabulary")
	assert.Equal(t, "ready_for_development", update.Status.FinalState,
		"the verdict is stamped from the envelope's final_state")
	assert.True(t, update.Status.CastleTerminalObserved,
		"the terminal marker is the completion signal and is stamped with the terminal")
	assert.Equal(t, "asg-run-1", update.Status.CastleRunID, "the observed id converges into status")
}
