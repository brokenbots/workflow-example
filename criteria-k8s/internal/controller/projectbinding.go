// Package controller: project binding admission (KB-311, ADR 307). A run
// stamped with spec.projectBindingRef admits through the project's
// criteria-binding ConfigMap in the run's namespace — the project's
// COMPLETE execution config, owned by the project (no platform write path
// except the routes registry entry). Admission reads the binding, verifies
// the pin, parses it fail closed, materializes its values onto the
// in-memory run the validation and the job builders act on, and keeps the
// rendered config pins (KB-103 shape) pointed at the binding CM so a
// binding replaced under an admitted run drifts the run fast.
package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	logr "github.com/go-logr/logr"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/binding"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/jobbuilder"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/routes"
)

// readProjectBinding reads the run's project binding ConfigMap (KB-311),
// verifies the stamped resourceVersion pin, and parses + validates the
// payload, fail closed. Returns (payload, checksum, "", nil) on success;
// (zero, "", refusal, nil) when the run must be refused (the refusal names
// the binding and the pin it broke); (zero, "", "", err) on an
// infrastructure failure the caller propagates so the controller-runtime
// backoff retries the pass instead of failing runs off transient reads.
func (r *CriteriaRunReconciler) readProjectBinding(ctx context.Context, run *criteriav1.CriteriaRun) (binding.Payload, string, string, error) {
	ref := run.Spec.ProjectBindingRef

	var cm corev1.ConfigMap
	err := r.Get(ctx, client.ObjectKey{Namespace: run.Namespace, Name: ref.Name}, &cm)
	switch {
	case apierrors.IsNotFound(err):
		return binding.Payload{}, "", fmt.Sprintf("the project binding ConfigMap %q (resourceVersion %q) no longer exists", ref.Name, ref.ResourceVersion), nil
	case err != nil:
		return binding.Payload{}, "", "", fmt.Errorf("reading project binding ConfigMap %s/%s: %w", run.Namespace, ref.Name, err)
	}
	if ref.ResourceVersion != "" && ref.ResourceVersion != cm.ResourceVersion {
		return binding.Payload{}, "", fmt.Sprintf("the project binding ConfigMap %q was pinned at resourceVersion %q but now reads %q", ref.Name, ref.ResourceVersion, cm.ResourceVersion), nil
	}
	payload, err := binding.Parse([]byte(cm.Data[binding.BindingKey]))
	if err != nil {
		return binding.Payload{}, "", err.Error(), nil
	}
	return payload, fmt.Sprintf("sha256:%s", payload.Checksum()), "", nil
}

// materializeProjectBinding overwrites the run's IN-MEMORY binding-shaped
// workflow fields with the project binding's values (KB-311): the spec is
// never persisted (only Status().Update writes below), so this copy is
// exactly what the interface validation and the child-Job builders act on.
// Adapter images, env, volumes, and secrets come wholly from the binding;
// the workflow entry's tree references (name, type, url, ref, interface,
// class, url+image process image) are preserved. A nil workflow is
// pre-KB-311 shape and carries no binding — untouched.
func materializeProjectBinding(run *criteriav1.CriteriaRun, payload binding.Payload) {
	wf := run.Spec.Workflow
	if wf == nil {
		return
	}
	wf.Volumes = nil
	wf.Secrets = nil
	wf.Env = nil
	wf.AdapterImages = nil

	if len(payload.Images) > 0 {
		wf.AdapterImages = make(map[string]string, len(payload.Images))
		for kind, ref := range payload.Images {
			wf.AdapterImages[kind] = ref
		}
	}
	if len(payload.Env) > 0 {
		wf.Env = make(map[string]string, len(payload.Env))
		for name, value := range payload.Env {
			wf.Env[name] = value
		}
	}
	wf.Volumes = runWorkflowVolumes(payload.Volumes)
	wf.Secrets = runWorkflowSecrets(payload.Secrets)
}

// runWorkflowVolumes converts binding volumes into the CriteriaRun
// workflow shape: identical fields, read-only copies (the binding payload
// is re-parsed per pass, so no aliasing surprises).
func runWorkflowVolumes(vs []routes.Volume) []criteriav1.RunWorkflowVolume {
	if len(vs) == 0 {
		return nil
	}
	out := make([]criteriav1.RunWorkflowVolume, len(vs))
	for i, v := range vs {
		env := make(map[string]string, len(v.Env))
		for name, value := range v.Env {
			env[name] = value
		}
		out[i] = criteriav1.RunWorkflowVolume{
			Name:       v.Name,
			Kind:       v.Kind,
			MountPath:  v.MountPath,
			SubPath:    v.SubPath,
			ReadOnly:   v.ReadOnly,
			Claim:      v.Claim,
			Server:     v.Server,
			Path:       v.Path,
			SizeLimit:  v.SizeLimit,
			SecretName: v.SecretName,
			Env:        env,
		}
	}
	return out
}

// runWorkflowSecrets converts binding secrets into the CriteriaRun
// workflow shape (names and env key mappings only — credential material
// lives behind the SecretProviderClass, never in the CR).
func runWorkflowSecrets(ss []routes.Secret) []criteriav1.RunWorkflowSecret {
	if len(ss) == 0 {
		return nil
	}
	out := make([]criteriav1.RunWorkflowSecret, len(ss))
	for i, s := range ss {
		env := make(map[string]string, len(s.Env))
		for name, key := range s.Env {
			env[name] = key
		}
		out[i] = criteriav1.RunWorkflowSecret{
			Name:                s.Name,
			SecretProviderClass: s.SecretProviderClass,
			MountPath:           s.MountPath,
			Env:                 env,
		}
	}
	return out
}

// renderProjectBindingConfig renders a project-bound run's config from the
// binding CM's config slots (KB-311), the KB-103 delivery shape: the
// binding's config VALUES (buildCmd/testCmd/ciGateCmd/baseBranch) render
// once at admission into status.configRender — provenance pinned to the
// binding ConfigMap's resourceVersion — and every later pass re-verifies
// the pin, drift-failing fast when the binding CM changed under the run.
// The recorded render's provenance carries the binding CM's RV even when
// the payload config map is empty: the pin re-verification watches the
// WHOLE binding, not just the config surface.
func (r *CriteriaRunReconciler) renderProjectBindingConfig(ctx context.Context, run *criteriav1.CriteriaRun, update *criteriav1.CriteriaRun, logger logr.Logger) (driftMsg string, err error) {
	ref := run.Spec.ProjectBindingRef
	ns := jobbuilder.TargetNamespace(run)

	if render := run.Status.ConfigRender; render != nil {
		// Verify-only pass: the recorded render is ground truth.
		if render.ConfigMap == "" {
			return "", nil
		}
		var cm corev1.ConfigMap
		if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: render.ConfigMap}, &cm); err != nil {
			if !apierrors.IsNotFound(err) {
				return "", fmt.Errorf("verifying criteriaRun project binding render against ConfigMap %s/%s: %w", ns, render.ConfigMap, err)
			}
			return fmt.Sprintf("the rendered project binding ConfigMap %q (resourceVersion %q) no longer exists", render.ConfigMap, render.ResourceVersion), nil
		}
		if cm.ResourceVersion != render.ResourceVersion {
			return driftMessage(render.ConfigMap, render.ResourceVersion, cm.ResourceVersion), nil
		}
		applyRenderToSpec(&run.Spec, *render)
		return "", nil
	}

	var cm corev1.ConfigMap
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Sprintf("the project binding ConfigMap %q (resourceVersion %q) no longer exists", ref.Name, ref.ResourceVersion), nil
		}
		return "", fmt.Errorf("reading project binding ConfigMap %s/%s: %w", ns, ref.Name, err)
	}
	if ref.ResourceVersion != "" && ref.ResourceVersion != cm.ResourceVersion {
		return driftMessage(ref.Name, ref.ResourceVersion, cm.ResourceVersion), nil
	}
	payload, err := binding.Parse([]byte(cm.Data[binding.BindingKey]))
	if err != nil {
		return "", fmt.Errorf("parsing project binding ConfigMap %s/%s: %w", ns, ref.Name, err)
	}
	render := &criteriav1.RunConfigRender{
		ConfigMap:       ref.Name,
		ResourceVersion: cm.ResourceVersion,
		BuildCmd:        payload.Config["buildCmd"],
		TestCmd:         payload.Config["testCmd"],
		CIGateCmd:       payload.Config["ciGateCmd"],
		BaseBranch:      payload.Config["baseBranch"],
	}
	// Config slots stay tree-declared (KB-310 unknown-slot rule holds for
	// the VALUES dimension too): the payload parse rejects unknown slot
	// NAMES, and kb310ConfigSlotDetail rejects values for slots the tree
	// does not declare. cmData is nil — the payload is structured, there
	// is no raw CM data key surface beyond binding.json.
	if detail := kb310ConfigSlotDetail(run, render, nil); detail != "" {
		return detail, nil
	}
	update.Status.ConfigRender = render
	applyRenderToSpec(&run.Spec, *render)
	logger.Info("recording CriteriaRun config render (project binding)", "criteriarun", run.Name,
		"configMap", render.ConfigMap, "resourceVersion", render.ResourceVersion)
	return "", nil
}
