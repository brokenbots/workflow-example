package jobbuilder

import (
	"sort"
	"strings"
)

// secretVarBindings derives the source-mode entrypoint's file: OriginRef
// --var pairs from the workflow object's declared secrets (D69). Each env
// entry maps an env var name to the secret key rendered inside the CSI
// mount; the workflow's secret VARIABLE name is that key (lowercase), so
// the binding is varname=file:<mountPath>/<key>. Deriving the set keeps
// non-linear workflows (kanboard_triage_v1's kanboard_url/kanboard_app_token)
// from being silently dropped by the previously hardcoded linear-only list.
// Only mount paths appear in argv; values reach adapters over OpenSession.
func secretVarBindings(plan *workflowPlan) string {
	if plan == nil || len(plan.secrets) == 0 {
		return ""
	}
	pairs := make([]string, 0, 8)
	seen := map[string]bool{}
	for _, sec := range plan.secrets {
		for _, key := range sec.Env {
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			pairs = append(pairs, key+"=file:"+joinSecretMount(sec.MountPath, key))
		}
	}
	sort.Strings(pairs)
	return strings.Join(pairs, "\n")
}
