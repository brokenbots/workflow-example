// Package controller: per-repo config ConfigMap render (KB-103). A run
// pinned via spec.configRef executes on the config ConfigMap's values
// rendered ONCE at admission and recorded into status.configRender; later
// reconcile passes only re-verify the pin. A ConfigMap replaced under an
// admitted run fails it fast with a ConfigRenderDrift condition (the
// workflowSource ref-pin fail-closed symmetry, CRI-226) — patches never
// silently switch an in-flight or parked run's config; the watcher refires
// the run on the current pin instead.
package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	logr "github.com/go-logr/logr"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/jobbuilder"
)

// ConditionConfigRenderDrift is the CriteriaRun condition type and the
// metav1.Condition reason stamped when a run is failed on a config
// ConfigMap pin mismatch (KB-103).
const ConditionConfigRenderDrift = "ConfigRenderDrift"

// renderRunConfig renders the run's per-repo config the way KB-103
// prescribes and hands the rendered values to the child-Job builders:
//
//   - verify-only pass (status.configRender already recorded): re-GET the
//     recorded ConfigMap only; its current resourceVersion must equal the
//     recorded one or the run drifts (fail-fast). A rendered ConfigMap name
//     of "" is a spec-fields-only render (no pin or a CM that did not exist
//     at admission) — nothing to verify, never re-derived.
//   - fresh derive (no recorded render yet): no pin renders the spec fields
//     alone (recorded only when any command or pin exists — nil keeps the
//     pre-KB-103 shape); a pin is read from jobbuilder.TargetNamespace, a
//     declared resourceVersion that no longer matches fails closed, a
//     missing ConfigMap with no declared resourceVersion renders the spec
//     fields alone, and a found ConfigMap's non-empty data keys outrank the
//     spec fields per key.
//
// The rendered values are applied onto the passed run's spec copy (the
// spec is never persisted — only Status().Update writes below) so
// jobbuilder.BuildAll threads them into the runner env. The drift return,
// when non-empty, is the reason string for the fail-fast; call
// failConfigRenderDrift with it. A returned error is an unexpected CM read
// failure (not drift): the caller propagates it so the controller-runtime
// backoff retries the pass instead of failing runs off infrastructure
// problems.
func (r *CriteriaRunReconciler) renderRunConfig(ctx context.Context, run *criteriav1.CriteriaRun, update *criteriav1.CriteriaRun, logger logr.Logger) (driftMsg string, err error) {
	ref := run.Spec.ConfigRef
	if ref == nil {
		// No pin: the render is the spec fields alone.
		if run.Status.ConfigRender == nil && hasSpecRender(run.Spec) {
			update.Status.ConfigRender = specFieldsRender(run.Spec)
			logger.Info("recording CriteriaRun config render (spec fields only, no config pin)", "criteriarun", run.Name)
		}
		return "", nil
	}

	ns := jobbuilder.TargetNamespace(run)
	if render := run.Status.ConfigRender; render != nil {
		// Verify-only: the recorded render is the ground truth and is
		// never re-derived (patched ConfigMaps must not touch in-flight
		// or parked runs — the recorded values are what the run executes
		// on).
		if render.ConfigMap == "" {
			return "", nil
		}
		var cm corev1.ConfigMap
		err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: render.ConfigMap}, &cm)
		if err == nil && cm.ResourceVersion == render.ResourceVersion {
			// The pin still holds: the recorded render is what the run keeps
			// executing on, so the child Jobs built this pass (e.g. one was
			// deleted and needs re-creation) must be built from it, not from
			// the watcher's stamp.
			applyRenderToSpec(&run.Spec, *render)
			return "", nil
		}
		if err != nil && !apierrors.IsNotFound(err) {
			return "", fmt.Errorf("verifying criteriaRun config render against ConfigMap %s/%s: %w", ns, render.ConfigMap, err)
		}
		if err != nil {
			return fmt.Sprintf("the rendered config ConfigMap %q (resourceVersion %q) no longer exists", render.ConfigMap, render.ResourceVersion), nil
		}
		return driftMessage(render.ConfigMap, render.ResourceVersion, cm.ResourceVersion), nil
	}

	var cm corev1.ConfigMap
	err = r.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, &cm)
	switch {
	case apierrors.IsNotFound(err):
		// Name-only pin (the CM did not exist when the watcher stamped)
		// with the ConfigMap STILL absent: render the spec fields alone
		// instead of failing the run — a run may be admitted before its
		// repo's config ConfigMap is first created. A declared
		// resourceVersion that cannot be honored is different: the pinned
		// ConfigMap was deleted after stamping, so the pin cannot be
		// rendered — drift.
		if ref.ResourceVersion != "" {
			return fmt.Sprintf("the pinned config ConfigMap %q (resourceVersion %q) no longer exists", ref.Name, ref.ResourceVersion), nil
		}
		update.Status.ConfigRender = specFieldsRender(run.Spec)
		logger.Info("recording CriteriaRun config render (spec fields only, pinned ConfigMap absent)", "criteriarun", run.Name, "configMap", ref.Name)
		return "", nil
	case err != nil:
		return "", fmt.Errorf("reading pinned config ConfigMap %s/%s: %w", ns, ref.Name, err)
	}
	if ref.ResourceVersion != "" && ref.ResourceVersion != cm.ResourceVersion {
		return driftMessage(ref.Name, ref.ResourceVersion, cm.ResourceVersion), nil
	}

	render := &criteriav1.RunConfigRender{
		ConfigMap:       ref.Name,
		ResourceVersion: cm.ResourceVersion,
		BuildCmd:        firstNonEmpty(cm.Data["buildCmd"], run.Spec.BuildCmd),
		TestCmd:         firstNonEmpty(cm.Data["testCmd"], run.Spec.TestCmd),
		CIGateCmd:       firstNonEmpty(cm.Data["ciGateCmd"], run.Spec.CIGateCmd),
	}
	update.Status.ConfigRender = render
	// The rendered values are what the run executes on: apply them onto
	// the run's spec copy so BuildAll's runner env threads them through
	// (BUILD_CMD/TEST_CMD/CI_GATE_CMD). Never written back to the spec —
	// the spec keeps the watcher's stamp, the status keeps the provenance.
	applyRenderToSpec(&run.Spec, *render)
	logger.Info("recording CriteriaRun config render", "criteriarun", run.Name,
		"configMap", render.ConfigMap, "resourceVersion", render.ResourceVersion)
	return "", nil
}

// failConfigRenderDrift fails a run whose config ConfigMap pin drifted out
// from under it (KB-103): child Jobs (and per-scope adapter pods) are torn
// down so nothing keeps executing against a config the cluster no longer
// declares at the pinned version, the run is marked Failed with the
// ConfigRenderDrift condition, and the queue slot is released so the
// watcher refires on the current pin. Mirrors failBaseImageMismatch.
func (r *CriteriaRunReconciler) failConfigRenderDrift(ctx context.Context, run *criteriav1.CriteriaRun, update *criteriav1.CriteriaRun, detail string, logger logr.Logger) (ctrl.Result, error) {
	logger.Info("failing CriteriaRun on config render drift", "criteriarun", run.Name, "detail", detail)
	if err := r.deleteChildJobs(ctx, run); err != nil {
		return ctrl.Result{}, err
	}
	// Per-scope adapter pods dial the runner's shim; with the runner Job
	// deleted the shim is going away, so the adapters must not outlive it.
	if run.Spec.PerScopeSessions {
		if err := r.deleteRunAdapterPods(ctx, run, logger); err != nil {
			return ctrl.Result{}, err
		}
	}
	upsertCondition(update, metav1.Condition{
		Type:               ConditionConfigRenderDrift,
		Status:             metav1.ConditionTrue,
		Reason:             ConditionConfigRenderDrift,
		Message:            fmt.Sprintf("%s; the run is failed so the watcher can refire on the current ConfigMap (KB-103)", detail),
		ObservedGeneration: run.Generation,
	})
	update.Status.Phase = criteriav1.PhaseFailed
	if err := r.Status().Update(ctx, update); err != nil {
		return ctrl.Result{}, fmt.Errorf("marking CriteriaRun failed on config render drift: %w", err)
	}
	// Release the queue slot so the next queued run is admitted.
	r.Queue.Release(run)
	return ctrl.Result{}, nil
}

// driftMessage is the drift detail recorded on the fail-fast condition:
// what the run executes on versus what the ConfigMap now is.
func driftMessage(name, recorded, current string) string {
	return fmt.Sprintf("the config ConfigMap %q was pinned at resourceVersion %q but now reads %q", name, recorded, current)
}

// hasSpecRender reports whether a spec-fields-only render is worth
// recording: nothing is configured on a pre-KB-103 run (no pin, no
// commands), so its status stays exactly as before KB-103.
func hasSpecRender(spec criteriav1.CriteriaRunSpec) bool {
	return spec.BuildCmd != "" || spec.TestCmd != "" || spec.CIGateCmd != ""
}

// applyRenderToSpec forces the recorded render's command values onto the
// run's spec copy (never persisted) so every pass builds child Jobs on the
// config the run was admitted under.
func applyRenderToSpec(spec *criteriav1.CriteriaRunSpec, render criteriav1.RunConfigRender) {
	spec.BuildCmd = render.BuildCmd
	spec.TestCmd = render.TestCmd
	spec.CIGateCmd = render.CIGateCmd
}

// specFieldsRender renders the spec fields alone (no ConfigMap behind
// it): the provenance record still shows which commands the run executes
// on.
func specFieldsRender(spec criteriav1.CriteriaRunSpec) *criteriav1.RunConfigRender {
	return &criteriav1.RunConfigRender{
		BuildCmd:  spec.BuildCmd,
		TestCmd:   spec.TestCmd,
		CIGateCmd: spec.CIGateCmd,
	}
}

// firstNonEmpty returns the first non-empty string.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}