// Package controller: fail-closed admission validation of a run's project
// binding against the workflow tree's machine-readable interface (KB-310,
// ADR 307). A shared tree declares what a run needs — adapter kinds, mount
// paths, secret env lanes, env names — and is the only override surface:
// anything the binding supplies beyond the declaration would previously be
// silently ignored (the "bespoke entry hack" the declaration exists to
// kill), so admission rejects it instead. Validation runs before job
// creation, references both sides (tree + project binding), and is a pure
// function of the run spec — no cluster I/O, so the gate holds well inside
// the <5s admission budget. The launch manifest stamped onto the run status
// records what was bound (workflow ref+digest, interface/binding
// checksums, operator version, resolved secret lanes) — the
// reproducibility contract.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	logr "github.com/go-logr/logr"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/routes"
)

// routesWorkflowTreeRe extracts the tree segment a url-mode workflow url
// addresses (the same .git//<tree> segment the routes CM url carries, so a
// refusal names the tree even when the run is missing its interface — the
// declared workflow is nil there).
var routesWorkflowTreeRe = regexp.MustCompile(`\.git//([A-Za-z0-9_-]+)`)

// interfaceBindingApplies reports whether admission must validate the
// run's project binding against its stamped interface (KB-310): any
// workflow that carries an interface. The interface-less carve-out is
// narrowed to the pre-KB-310 image-mode path (interface-nil type=image
// runs: no tree contract exists to validate against, and KB-280 F36 keeps
// image mode declared-secret-free). A type=url workflow with a nil
// interface is NOT carved out — interfaceBindingGap refuses it closed
// first, so a binding authored directly against the API rather than
// through the routes watcher cannot skip validation and silently fall
// through to the baked-image path.
func interfaceBindingApplies(run *criteriav1.CriteriaRun) bool {
	wf := run.Spec.Workflow
	return wf != nil && wf.Interface != nil
}

// interfaceBindingGap reports whether a run must be refused because it
// declares a workflow type that requires a machine-readable interface yet
// carries none (KB-310, fail closed): there is no tree declaration for its
// project binding, and skipping validation would re-create the fail-open
// unknown-slot path — an unstated binding falls through to the baked-image
// path validated by nothing. Interface-less url workflows are rejected by
// the routes CM parse (routes.validateWorkflow), so a CR reaching
// admission with this gap bypassed the routes watcher; the refusal names
// the binding and the tree segment the workflow url addresses. Only
// type=image with a nil interface skips validation entirely.
func interfaceBindingGap(run *criteriav1.CriteriaRun) (string, bool) {
	wf := run.Spec.Workflow
	if wf == nil || wf.Interface != nil || wf.Type == routes.TypeImage {
		return "", false
	}
	var tree string
	if m := routesWorkflowTreeRe.FindStringSubmatch(wf.URL); m != nil {
		tree = m[1]
	}
	return fmt.Sprintf("workflow %q is type %q (url %s) but carries no stamped interface: admission cannot validate the binding against a tree declaration the run does not have, addressed tree %q, so the run is refused before job creation instead of falling through to the baked-image path with an unstated binding; stamp the tree's interface.json onto spec.workflow.interface and re-fire (KB-310)",
		wf.Name, wf.Type, wf.URL, tree), true
}

// validateInterfaceBinding compares a run's project binding (spec.workflow:
// adapterImages, volumes, secrets, env — the binding is authored in the
// project's CM and stamped from the routes workflow object) against the
// tree's interface declaration, fail closed:
//
//   - every interface-required adapter kind must be present in the
//     binding's adapterImages (extra kinds are tolerated — not every tree
//     needs every possible adapter);
//   - every binding volume must appear in the interface's mounts at the
//     byte-exact mountPath with the declared kind (pvc|tmp), and every
//     interface mount must be satisfied by a binding volume;
//   - every interface-required secret env lane must resolve from the
//     binding's secrets env maps, and every env key the binding's secrets
//     carry must be declared by the interface (volume-level env maps on
//     volumes are operator plumbing and exempt; a secret with an empty
//     env map contributes no keys — the CSI file-mount lane);
//   - every binding env key must be declared by the interface. A
//     secret-lane name delivered as a plain env key is an unknown slot:
//     credentials ride secret lanes, never workflow env.
//
// Errors name both sides — the tree (interface workflow and name) and the
// project binding (workflow name and the offending binding entry) — so a
// rejected run is troubleshootable from the message alone.
func validateInterfaceBinding(run *criteriav1.CriteriaRun) error {
	wf := run.Spec.Workflow
	iface := wf.Interface

	// Adapter kinds: required ⊆ provided; extras tolerated.
	for _, kind := range iface.Adapters {
		if _, ok := wf.AdapterImages[kind]; !ok {
			provided := sortedKeys(wf.AdapterImages)
			return fmt.Errorf("interface %q requires adapter kind %q; binding %q adapterImages provide %s (declare the missing kind's image for this tree, KB-310)",
				iface.Workflow, kind, wf.Name, listOrNone(provided))
		}
	}

	// Mounts: exact (mountPath, kind) match both ways.
	ifacePaths := make(map[string]criteriav1.RunWorkflowInterfaceMount, len(iface.Mounts))
	for _, m := range iface.Mounts {
		ifacePaths[m.MountPath] = m
	}
	bindingPaths := make(map[string]criteriav1.RunWorkflowVolume, len(wf.Volumes))
	volumeList := make([]string, 0, len(wf.Volumes))
	for _, v := range wf.Volumes {
		bindingPaths[v.MountPath] = v
		volumeList = append(volumeList, v.Name+" ("+v.Kind+" at "+v.MountPath+")")
		if declared, ok := ifacePaths[v.MountPath]; !ok {
			return fmt.Errorf("binding %q supplies volume %q (%s at %s) which interface %q does not require; interface mounts: %s — unknown-slot, fail closed (the declaration is the supported surface, KB-310)",
				wf.Name, v.Name, v.Kind, v.MountPath, iface.Workflow, listOrNone(declaredMounts(iface)))
		} else if v.Kind != declared.Kind {
			return fmt.Errorf("binding %q volume %q wants kind %q at %s, interface %q declares %q (binding volumes and interface mounts must agree on kind, KB-310)",
				wf.Name, v.Name, v.Kind, v.MountPath, iface.Workflow, declared.Kind)
		}
	}
	for _, declared := range iface.Mounts {
		v, ok := bindingPaths[declared.MountPath]
		if !ok {
			return fmt.Errorf("interface %q requires mount %q (%s) at %q; binding %q volumes: %s (mount paths must match byte-exact, KB-310)",
				iface.Workflow, declared.Name, declared.Kind, declared.MountPath, wf.Name, listOrNone(volumeList))
		}
		if v.Kind != declared.Kind {
			return fmt.Errorf("interface %q requires mount %q as %q at %q; binding %q volume at that path is %q (KB-310)",
				iface.Workflow, declared.Name, declared.Kind, declared.MountPath, wf.Name, v.Kind)
		}
	}

	// Secret env lanes: required lanes resolve from binding secrets env
	// maps; binding secret env keys are declared by the interface.
	resolved := map[string]string{}
	secretList := make([]string, 0, len(wf.Secrets))
	for _, s := range wf.Secrets {
		secretList = append(secretList, s.Name)
		for name := range s.Env {
			if !slices.Contains(iface.SecretEnv, name) && !slices.Contains(iface.Env, name) {
				return fmt.Errorf("binding %q secret %q carries env %q which interface %q does not require; interface secretEnv: %s, env: %s (unknown-slot, fail closed, KB-310)",
					wf.Name, s.Name, name, iface.Workflow, listOrNone(iface.SecretEnv), listOrNone(iface.Env))
			}
			resolved[name] = s.Name
		}
	}
	for _, lane := range iface.SecretEnv {
		if _, ok := resolved[lane]; ok {
			continue
		}
		return fmt.Errorf("interface %q requires secret env %q; binding %q secrets (%s) carry no such env key (wire the lane in the binding secret's env map, KB-310)",
			iface.Workflow, lane, wf.Name, listOrNone(secretList))
	}

	// Env: binding env keys must be declared as interface env.
	for name := range wf.Env {
		if !slices.Contains(iface.Env, name) {
			return fmt.Errorf("binding %q env key %q is not declared by interface %q (interface env: %s, secretEnv: %s); unknown-slot, fail closed (KB-310)",
				wf.Name, name, iface.Workflow, listOrNone(iface.Env), listOrNone(iface.SecretEnv))
		}
	}
	return nil
}

// manifestSecretLanes resolves the binding's secret lanes: one
// "<ENV>@<secret-entry>" pair per interface-required secret env, sorted
// for a stable manifest. A required lane the binding does not resolve is
// omitted — admission validates before the manifest is stamped, so the
// omission is unreachable in the normal flow and keeps the manifest
// honest when stamped for a run that was refused on another gate.
func manifestSecretLanes(iface *criteriav1.RunWorkflowInterface, wf *criteriav1.RunWorkflow) []string {
	resolved := map[string]string{}
	for _, s := range wf.Secrets {
		for name := range s.Env {
			if slices.Contains(iface.SecretEnv, name) {
				resolved[name] = s.Name
			}
		}
	}
	lanes := make([]string, 0, len(resolved))
	for _, lane := range iface.SecretEnv {
		if entry, ok := resolved[lane]; ok {
			lanes = append(lanes, lane+"@"+entry)
		}
	}
	slices.Sort(lanes)
	return lanes
}

// launchManifest builds the run's launch manifest (KB-310): the
// reproducibility contract recording what the run was bound to — workflow
// ref and digest pin, the interface and binding checksums (canonical JSON
// sha256s; encoding/json sorts map keys, so both are deterministic), the
// operator version, and the resolved secret lanes.
func launchManifest(run *criteriav1.CriteriaRun, operatorVersion string) (*criteriav1.RunLaunchManifest, error) {
	wf := run.Spec.Workflow
	iface := wf.Interface
	ifaceChecksum, err := interfaceChecksum(iface)
	if err != nil {
		return nil, fmt.Errorf("checksumming interface %q: %w", iface.Workflow, err)
	}
	bindingBytes, err := json.Marshal(wf)
	if err != nil {
		return nil, fmt.Errorf("checksumming binding %q: %w", wf.Name, err)
	}
	return &criteriav1.RunLaunchManifest{
		WorkflowRef:       wf.URL,
		WorkflowDigest:    wf.Ref,
		InterfaceChecksum: ifaceChecksum,
		BindingChecksum:   fmt.Sprintf("sha256:%x", sha256.Sum256(bindingBytes)),
		OperatorVersion:   operatorVersion,
		Lanes:             manifestSecretLanes(iface, wf),
	}, nil
}

// interfaceChecksum sha256s the interface declaration's canonical JSON
// encoding. Go structs marshal with fields in declaration order and
// encoding/json sorts map keys, so the same declaration always yields the
// same checksum.
func interfaceChecksum(iface *criteriav1.RunWorkflowInterface) (string, error) {
	bytes, err := json.Marshal(iface)
	if err != nil {
		return "", err
	}
	return "sha256:" + fmt.Sprintf("%x", sha256.Sum256(bytes)), nil
}

func declaredMounts(iface *criteriav1.RunWorkflowInterface) []string {
	names := make([]string, 0, len(iface.Mounts))
	for _, m := range iface.Mounts {
		names = append(names, m.Name+" ("+m.Kind+" at "+m.MountPath+")")
	}
	return names
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// listOrNone renders a name list into a message-friendly bracketed list,
// empty lists staying explicit so a missing declaration is never
// indistinguishable from an empty one.
func listOrNone(names []string) string {
	if len(names) == 0 {
		return "[] (none)"
	}
	return "[" + strings.Join(names, " ") + "]"
}

// refuseInterfaceBindingMismatch fails a run closed at admission (KB-310),
// the interface analogue of refuseMissingWorkflowSource (KB-6): any child
// Jobs a pre-gate operator provisioned are deleted, the run is marked
// Failed with an InterfaceBindingMismatch condition carrying the
// both-sides mismatch message, and a warning event records the refusal.
// The queue slot is released so the refused run never starves its repo
// queue; the project CM or the tree must change before a re-fire admits.
func (r *CriteriaRunReconciler) refuseInterfaceBindingMismatch(ctx context.Context, run *criteriav1.CriteriaRun, message string, logger logr.Logger) (ctrl.Result, error) {
	wf := run.Spec.Workflow
	// The tree exists on mismatched-interface refusals; the interface-less
	// gap refusal (interfaceBindingGap) has nil, so it omits the field
	// rather than nil-deref — the gap message carries the addressed tree.
	attrs := []any{"workflow", wf.Name, "ticket", run.Spec.TicketID, "mismatch", message}
	if wf.Interface != nil {
		attrs = append(attrs, "tree", wf.Interface.Workflow)
	}
	logger.Info("refusing to admit CriteriaRun: binding does not satisfy the workflow interface (KB-310)", attrs...)
	update := run.DeepCopy()
	update.Status.Phase = criteriav1.PhaseFailed
	upsertCondition(update, metav1.Condition{
		Type:               criteriav1.ConditionInterfaceBindingMismatch,
		Status:             metav1.ConditionTrue,
		Reason:             criteriav1.ConditionInterfaceBindingMismatch,
		Message:            message,
		ObservedGeneration: run.Generation,
	})
	if err := r.Status().Update(ctx, update); err != nil {
		return ctrl.Result{}, fmt.Errorf("marking CriteriaRun failed on interface binding mismatch: %w", err)
	}
	r.Queue.Release(run)
	if err := r.ensureTerminalArchived(ctx, update, nil, logger); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.deleteChildJobs(ctx, run); err != nil {
		return ctrl.Result{}, err
	}
	if run.Spec.PerScopeSessions {
		if err := r.deleteRunAdapterPods(ctx, run, logger); err != nil {
			return ctrl.Result{}, err
		}
	}
	if r.Recorder != nil {
		r.Recorder.Event(run, corev1.EventTypeWarning, eventReasonInterfaceBindingMismatch, message)
	}
	return ctrl.Result{}, nil
}

// stampLaunchManifest records the run's launch manifest on the pending
// status update (KB-310): stamped once, from the admitted binding — a
// later pass whose env or operator moved on must not silently re-stamp
// what the run was actually bound to, so the manifest pins the first
// admission. Runs without a stamped interface (the legacy carve-out)
// carry no manifest.
func (r *CriteriaRunReconciler) stampLaunchManifest(update *criteriav1.CriteriaRun, logger logr.Logger) {
	wf := update.Spec.Workflow
	if wf == nil || wf.Interface == nil || update.Status.LaunchManifest != nil {
		return
	}
	manifest, err := launchManifest(update, r.operatorVersion())
	if err != nil {
		// Marshaling a struct-carried interface cannot fail in practice;
		// a checksum failure here must not wedge the run mid-admission.
		logger.Info("building launch manifest failed; run stays unmanifested",
			"criteriarun", update.Name, "error", err.Error())
		return
	}
	update.Status.LaunchManifest = manifest
}
