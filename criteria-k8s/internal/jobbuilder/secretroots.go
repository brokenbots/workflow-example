package jobbuilder

import (
	"sort"
	"strings"
)

// trustedSecretFileRoots derives the runner env's CRITERIA_SECRET_FILE_ROOTS
// value from the workflow object's declared secrets — the SAME source of
// truth secretVarBindings (CRITERIA_SECRET_VARS) reads. Each declared secret
// entry's mountPath is a deployment-owned secret surface (CSI/SPC mount the
// operator already created and mounted); declaring it here lets the engine's
// file provider confine reads to mount+default without inventing a second
// trust decision anywhere else. Deduplicated, sorted, newline-separated.
// Empty when the workflow declares no secrets: the engine keeps the single
// default secrets-dir root (0.5.57 containment posture unchanged).
func trustedSecretFileRoots(plan *workflowPlan) string {
	if plan == nil || len(plan.secrets) == 0 {
		return ""
	}
	seen := map[string]bool{}
	roots := make([]string, 0, 4)
	for _, sec := range plan.secrets {
		if sec.MountPath == "" || seen[sec.MountPath] {
			continue
		}
		seen[sec.MountPath] = true
		roots = append(roots, sec.MountPath)
	}
	sort.Strings(roots)
	return strings.Join(roots, "\n")
}
