// Package runstamp holds the CriteriaRun spec-stamping helpers the linear
// and kanboard watchers share: the KB-103 per-repo config fields
// (entry-by-entry outranking of the watcher-level fallback flags and the
// spec.configRef pin) plus the CRI-231 source-mode stamp both watchers
// carried as duplicated main-package helpers before KB-103.
package runstamp

import (
	"context"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/routes"
)

// StampRepoConfig stamps the KB-103 per-repo config onto a run spec: the
// selected configLibrary entry's command values outrank the watcher-level
// fallback flags entry-by-entry (a config entry that omits a key keeps the
// flag value for that key), and a selected entry pins spec.configRef to
// the ref resolved by PinConfigRef. The operator renders the pinned CM's
// values at admission — the CM's data keys outrank the inline entry values
// the watcher stamps, the same precedence the routes schema declares.
func StampRepoConfig(spec *criteriav1.CriteriaRunSpec, sel *routes.Selection, cfg *criteriav1.CriteriaRunConfigRef) {
	if sel == nil {
		return
	}
	cfgEntry := sel.Config
	if cfgEntry.BuildCmd != "" {
		spec.BuildCmd = cfgEntry.BuildCmd
	}
	if cfgEntry.TestCmd != "" {
		spec.TestCmd = cfgEntry.TestCmd
	}
	if cfgEntry.CIGateCmd != "" {
		spec.CIGateCmd = cfgEntry.CIGateCmd
	}
	spec.ConfigRef = cfg
}

// PinConfigRef resolves a selection's config pin: the ref name (the
// configLibrary entry name, which doubles as the ConfigMap name in the
// execution namespace) with the CM's resourceVersion when the CM is
// readable at stamping time. A CM that does not exist yet yields a
// name-only pin; any other read error is returned so the caller can log it.
// The operator renders a name-only pin against the CM as of job-build time
// and stamps what it actually executed into status — provenance instead of
// a lie.
func PinConfigRef(ctx context.Context, c client.Client, ns string, sel *routes.Selection) (*criteriav1.CriteriaRunConfigRef, error) {
	if sel == nil || sel.ConfigName == "" {
		return nil, nil
	}
	ref := &criteriav1.CriteriaRunConfigRef{
		Name: sel.ConfigName,
	}
	var cm corev1.ConfigMap
	err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, &cm)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return ref, nil
		}
		return ref, err
	}
	ref.ResourceVersion = cm.ResourceVersion
	return ref, nil
}

// StampWorkflowSource resolves the source-mode spec fields from a url-type
// routes workflow object (CRI-231): spec.workflowSource carries the URL the
// runner fetches and applies at run time (plus the fail-closed ref pin),
// and url+image routes additionally stamp spec.image so the process
// executes in the route-provided image. Image-type routes (baked tree)
// leave both unset. Type/url validation is the routes package's job
// (k8s/routes.schema.json); the guard here only keeps an unvalidated
// declaration from stamping an unusable source.
func StampWorkflowSource(spec *criteriav1.CriteriaRunSpec, wf routes.Workflow) {
	if wf.Type != routes.TypeURL || strings.TrimSpace(wf.URL) == "" {
		return
	}
	spec.WorkflowSource = &criteriav1.RunWorkflowSource{
		Type: "url",
		URL:  wf.URL,
		Ref:  wf.Ref,
	}
	if strings.TrimSpace(wf.Image) != "" {
		spec.Image = wf.Image
	}
}