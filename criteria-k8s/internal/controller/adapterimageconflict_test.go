// Reconcile-level coverage of the KB-319 adapter-image conflict enforcement
// (internal/controller/adapterimageconflict.go, consulted by
// criteriarun_controller.go's default castle-observation branch). The
// enforcement branch mutates run state (Phase=Failed, AdapterImageConflict
// condition), releases the repo queue slot, tears down child Jobs and
// per-scope adapter pods, and archives the terminal record. These tests
// pin all of that end-to-end through a real Reconcile against a fake
// client — the pure resolver matrix lives in jobbuilder.
package controller_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/castle"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/controller"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/events"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/jobbuilder"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/routes"
)

const (
	// Two PINNED metadata/event references for the same kind — the
	// disagreement is what turns the lane into a conflict.
	conflictMetaRef  = "localhost:5000/criteria-adapter-shell:fixed-v7"
	conflictEventRef = "localhost:5000/criteria-adapter-shell:live-v9"
)

// conflictRun is a per-scope dev run whose workflow pins the shell adapter's
// image (the metadata lane) with the KB-319 conflict policy stamped.
func conflictRun(name, policy string) *criteriav1.CriteriaRun {
	return &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			UID:       types.UID(name + "-uid"),
		},
		Spec: criteriav1.CriteriaRunSpec{
			TicketID:         "KB-319",
			RepoURL:          "https://github.com/brokenbots/workflow-example.git",
			PerScopeSessions: true,
			MaxAgentVisits:   2,
			WorkflowSource: &criteriav1.RunWorkflowSource{
				Type: "url",
				URL:  "git::https://github.com/brokenbots/workflow-example.git//linear_intake",
			},
			Workflow: &criteriav1.RunWorkflow{
				// Type "image": no KB-310 interface gate — the conflict
				// enforcement is what's under test, not binding admission.
				// "image" is the only workflow type whose runs carry NO
				// interface and are not refused for it.
				Name:                "linear-intake",
				Type:                routes.TypeImage,
				Class:               "dev",
				ImageConflictPolicy: policy, // the stamp under test
				AdapterImages:       map[string]string{"shell": conflictMetaRef},
			},
		},
	}
}

// conflictScope is a digest-verified provision_wanted with a PINNED event
// image reference — the event half of every conflict below.
func conflictScope() events.LifecycleEvent {
	return events.LifecycleEvent{
		Event:          events.EventProvisionWanted,
		RunID:          "castle-1",
		ScopeID:        "root",
		AdapterName:    "shell",
		AdapterType:    "shell",
		ShimAddress:    "10.0.0.1:7778",
		TokenFile:      "/data/intake/kb-319/tokens/root-shell",
		Digest:         "sha256:abcdef1234567890",
		ImageReference: conflictEventRef,
	}
}

// staleAdapterPod is a pre-existing per-scope adapter pod from an earlier
// scope of the same run: the enforcement branch must not leave adapters
// dialing a runner whose child Jobs are destroyed under them.
func staleAdapterPod(t *testing.T, run *criteriav1.CriteriaRun) *corev1.Pod {
	t.Helper()
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobbuilder.PerScopeAdapterPodName(run, "shell", "root"),
			Namespace: run.Namespace,
			Labels: map[string]string{
				jobbuilder.LabelRun:  run.Name,
				jobbuilder.LabelRole: jobbuilder.RoleAdapter,
			},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "adapter", Image: conflictEventRef}}},
	}
}

func conflictReconciler(t *testing.T, scheme *runtime.Scheme, run *criteriav1.CriteriaRun, obs castle.Observation, archiver *fakeArchiver) (*controller.CriteriaRunReconciler, client.Client) {
	t.Helper()
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(run).
		WithObjects(run).
		Build()
	r := &controller.CriteriaRunReconciler{
		Client:      cl,
		Scheme:      scheme,
		Castle:      &fakeCastle{observation: &obs},
		EnvProbe:    &stubProbe{env: map[string]string{jobbuilder.EnvCriteriaBaseImage: baseImageOldTag}},
		Defaults:    jobbuilder.Defaults{DataPVC: "criteria-data"},
		Queue:       controller.NewRunQueue(),
		RunArchiver: archiver,
	}
	return r, cl
}

// A fail-closed (default) conflict parks the run failed with the
// AdapterImageConflict condition naming both pins, tears down the child
// Jobs and the per-scope adapter pods, releases the repo queue slot, and
// archives the terminal record.
func TestReconcileFailsRunClosedOnAdapterImageConflict(t *testing.T) {
	scheme := newScheme(t)

	for name, policy := range map[string]string{
		"default (unset)":      "",
		"explicit fail-closed": criteriav1.ImageConflictPolicyFailClosed,
		"unknown policy value": "prefer-nothing",
	} {
		policy := policy
		t.Run(name, func(t *testing.T) {
			run := conflictRun("kb319-conflict", policy)
			r, cl := conflictReconciler(t, scheme, run, castle.Observation{
				RunID:     "castle-1",
				Lifecycle: []events.LifecycleEvent{conflictScope()},
			}, newFakeArchiver())
			pod := staleAdapterPod(t, run)
			require.NoError(t, cl.Create(context.Background(), pod))

			reconcileOnce(t, r, run)

			var updated criteriav1.CriteriaRun
			require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(run), &updated))
			assert.Equal(t, criteriav1.PhaseFailed, updated.Status.Phase)
			cond, ok := conditionFor(updated.Status, controller.ConditionAdapterImageConflict)
			require.True(t, ok, "the fail-closed conflict must stamp the AdapterImageConflict condition")
			assert.Equal(t, metav1.ConditionTrue, cond.Status)
			assert.Equal(t, controller.ConditionAdapterImageConflict, cond.Reason)
			assert.Equal(t, run.Generation, cond.ObservedGeneration)
			assert.Contains(t, cond.Message, conflictMetaRef, "the condition must name the metadata pin")
			assert.Contains(t, cond.Message, conflictEventRef, "the condition must name the event pin")
			assert.Contains(t, cond.Message, `scope "root"`)

			// Teardown: runner Job + pre-existing per-scope adapter pod gone.
			_, found := runnerJobImageFor(t, cl, run)
			assert.False(t, found, "child Jobs must be torn down")
			var pods corev1.PodList
			require.NoError(t, cl.List(context.Background(), &pods,
				client.InNamespace(run.Namespace),
				client.MatchingLabels(map[string]string{jobbuilder.LabelRun: run.Name, jobbuilder.LabelRole: jobbuilder.RoleAdapter})))
			assert.Empty(t, pods.Items, "per-scope adapter pods must be torn down")

			// Queue slot released: the next queued run for the same repo is
			// admitted immediately.
			next := conflictRun(nextRunName(name), "")
			admitted, _, _ := r.Queue.Enqueue(next)
			assert.True(t, admitted, "the parked failed run must release its queue slot")

			// Terminal record archived before teardown (KB-314 ordering).
			archiver := r.RunArchiver.(*fakeArchiver)
			assert.Equal(t, 1, archiver.keyCount(), "the terminal archive must be attempted")
			assert.NotEmpty(t, updated.Status.ArchiveID, "the archived record must stamp its id")
		})
	}
}

// Arbitrated policies admit the reconcile and resolve the expected pin on
// the provisioned adapter pod — the conflict NEVER silently wins.
func TestReconcileArbitratesAdapterImageConflict(t *testing.T) {
	scheme := newScheme(t)

	cases := []struct {
		policy    string
		expectRef string
	}{
		{policy: criteriav1.ImageConflictPolicyPreferMetadata, expectRef: conflictMetaRef},
		{policy: criteriav1.ImageConflictPolicyPreferEvent, expectRef: conflictEventRef},
	}
	for _, c := range cases {
		c := c
		t.Run(c.policy, func(t *testing.T) {
			run := conflictRun("kb319-arbitrate", c.policy)
			r, cl := conflictReconciler(t, scheme, run, castle.Observation{
				RunID:     "castle-1",
				Lifecycle: []events.LifecycleEvent{conflictScope()},
			}, newFakeArchiver())

			reconcileOnce(t, r, run)

			var updated criteriav1.CriteriaRun
			require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(run), &updated))
			assert.NotEqual(t, criteriav1.PhaseFailed, updated.Status.Phase)
			_, ok := conditionFor(updated.Status, controller.ConditionAdapterImageConflict)
			assert.False(t, ok, "an arbitrated conflict must not fail the run")

			// The provisioned adapter pod resolves to the policy's pin.
			var pods corev1.PodList
			require.NoError(t, cl.List(context.Background(), &pods,
				client.InNamespace(run.Namespace),
				client.MatchingLabels(map[string]string{jobbuilder.LabelRun: run.Name, jobbuilder.LabelRole: jobbuilder.RoleAdapter})))
			require.Len(t, pods.Items, 1)
			pod := pods.Items[0]
			assert.Equal(t, "shell", pod.Labels[jobbuilder.LabelAdapterKind])
			require.NotEmpty(t, pod.Spec.Containers)
			assert.Equal(t, c.expectRef, pod.Spec.Containers[0].Image,
				"the policy must decide the adapter pod's image")

			// The runner stays provisioned: the reconcile was admitted, not
			// parked.
			_, found := runnerJobImageFor(t, cl, run)
			assert.True(t, found)
			assert.Empty(t, updated.Status.ArchiveID, "admitting must not archive")
		})
	}
}

// No disagreement, no conflict under ANY policy: agreeing pins, a missing
// digest, an unpinned event reference, and a missing metadata pin all
// admit — the policy arbitrates only a dispute, never displaces a lane the
// other half is missing.
func TestReconcileAdmitsWhenLanesAgreeOrAreMissing(t *testing.T) {
	scheme := newScheme(t)

	agreeing := conflictScope()
	agreeing.ImageReference = conflictMetaRef // the event's pin matches

	noDigest := conflictScope()
	noDigest.Digest = "" // not digest-verified: the event lane drops out

	unpinnedEvent := conflictScope()
	unpinnedEvent.ImageReference = "localhost:5000/criteria-adapter-shell" // bare repo: no pin after the last "/"

	policies := []string{"", criteriav1.ImageConflictPolicyFailClosed, criteriav1.ImageConflictPolicyPreferEvent, criteriav1.ImageConflictPolicyPreferMetadata}

	// noMeta drops the metadata pin (the event lane has nothing to disagree with).
	for _, policy := range policies {
		policy := policy
		t.Run("agreeing pins admitted under "+policyName(policy), func(t *testing.T) {
			run := conflictRun("kb319-agree", policy)
			r, cl := conflictReconciler(t, scheme, run, castle.Observation{RunID: "castle-1", Lifecycle: []events.LifecycleEvent{agreeing}}, newFakeArchiver())
			reconcileOnce(t, r, run)
			assertAdmitted(t, cl, run)
		})
		t.Run("missing digest admitted under "+policyName(policy), func(t *testing.T) {
			run := conflictRun("kb319-nodigest", policy)
			r, cl := conflictReconciler(t, scheme, run, castle.Observation{RunID: "castle-1", Lifecycle: []events.LifecycleEvent{noDigest}}, newFakeArchiver())
			reconcileOnce(t, r, run)
			assertAdmitted(t, cl, run)
		})
		t.Run("unpinned event reference admitted under "+policyName(policy), func(t *testing.T) {
			run := conflictRun("kb319-unpinned", policy)
			r, cl := conflictReconciler(t, scheme, run, castle.Observation{RunID: "castle-1", Lifecycle: []events.LifecycleEvent{unpinnedEvent}}, newFakeArchiver())
			reconcileOnce(t, r, run)
			assertAdmitted(t, cl, run)
		})
		t.Run("missing metadata pin admitted under "+policyName(policy), func(t *testing.T) {
			run := conflictRun("kb319-nometa", policy)
			run.Spec.Workflow.AdapterImages = nil // no metadata pin at all
			r, cl := conflictReconciler(t, scheme, run, castle.Observation{RunID: "castle-1", Lifecycle: []events.LifecycleEvent{conflictScope()}}, newFakeArchiver())
			reconcileOnce(t, r, run)
			assertAdmitted(t, cl, run)
		})
	}
}

// The consult only fires when an authoritative castle observation carries
// a run id: with the run unknown to castle (no RunID), the same disagreeing
// event set simply doesn't exist yet and the pass must NOT fail the run on
// an empty lifecycle.
func TestReconcileSkipsConflictConsultWithoutCastleRun(t *testing.T) {
	scheme := newScheme(t)
	run := conflictRun("kb319-nocastle", "")
	r, cl := conflictReconciler(t, scheme, run, castle.Observation{RunID: "", Lifecycle: []events.LifecycleEvent{conflictScope()}}, newFakeArchiver())
	reconcileOnce(t, r, run)
	assertAdmitted(t, cl, run)
}

// assertAdmitted: the run was NOT parked failed by the conflict consult and
// carries no conflict condition.
func assertAdmitted(t *testing.T, cl client.Client, run *criteriav1.CriteriaRun) {
	t.Helper()
	var updated criteriav1.CriteriaRun
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(run), &updated))
	assert.NotEqual(t, criteriav1.PhaseFailed, updated.Status.Phase)
	_, ok := conditionFor(updated.Status, controller.ConditionAdapterImageConflict)
	assert.False(t, ok, "no conflict condition may be stamped when the lane admits")
}

func policyName(policy string) string {
	switch policy {
	case "":
		return "unset(empty default)"
	case criteriav1.ImageConflictPolicyFailClosed:
		return "fail-closed"
	case criteriav1.ImageConflictPolicyPreferEvent:
		return "prefer-event"
	case criteriav1.ImageConflictPolicyPreferMetadata:
		return "prefer-metadata"
	}
	return policy
}

func nextRunName(name string) string {
	return name + "-next"
}
