package jobbuilder

// KB-5: the legacy (run-duration) adapter Job fan-out derives its kinds
// from the run's workflow record — the stamped workflow object's
// adapterImages keys — and keeps the built-in {shell, copilot} default when
// nothing is recorded. Pinned directly on the derivation so the fallback
// and the sort stay covered independently of the builders.

import (
	"testing"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestLegacyAdapterKindsFallsBackWithoutRecordedKinds(t *testing.T) {
	run := &criteriav1.CriteriaRun{Spec: criteriav1.CriteriaRunSpec{TicketID: "KB-5"}}
	assert.Equal(t, []string{"shell", "copilot"}, legacyAdapterKinds(run),
		"a run without a stamped workflow keeps the built-in default kinds")

	run = &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{Name: "kb5-no-images"},
		Spec: criteriav1.CriteriaRunSpec{
			TicketID: "KB-5",
			Workflow: &criteriav1.RunWorkflow{Name: "linear-intake-v1", Type: "image"},
		},
	}
	assert.Equal(t, []string{"shell", "copilot"}, legacyAdapterKinds(run),
		"a workflow that records no adapterImages keeps the built-in default kinds")
}

func TestLegacyAdapterKindsDerivesRecordedKindsSorted(t *testing.T) {
	// Map iteration order is nondeterministic, so the derivation must sort:
	// the fan-out order may not drift run to run.
	run := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{Name: "kb5-kinds"},
		Spec: criteriav1.CriteriaRunSpec{
			TicketID: "KB-5",
			Workflow: &criteriav1.RunWorkflow{
				Name: "linear-intake-v1",
				Type: "image",
				AdapterImages: map[string]string{
					"shell":   "localhost:5000/criteria-adapter-shell:k8s-3",
					"copilot": "localhost:5000/criteria-adapter-copilot:k8s-3",
					"noop":    "localhost:5000/criteria-adapter-noop:k8s-3",
				},
			},
		},
	}
	assert.Equal(t, []string{"copilot", "noop", "shell"}, legacyAdapterKinds(run),
		"the recorded kinds drive the legacy fan-out, sorted for deterministic ordering")
}
