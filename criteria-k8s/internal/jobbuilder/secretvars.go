package jobbuilder

import (
	"sort"
	"strings"
)

// secretVarBindings derives the source-mode entrypoint's file: OriginRef
// --var pairs from the workflow object's declared secrets (D69). The binding
// contract (KB-385, pinned both here and in docs/jobs-setup.md) is: each
// env entry's KEY is the binding env lane name carried by the secret's
// SCREAMING env name (validateSecretEnv's name side, e.g.
// WORKFLOW_UPSTREAM_TOKEN) and the VALUE is the secret file key rendered
// inside the CSI mount (e.g. workflow_github_token for the upstream lane,
// which reuses the classic lane's file); the derived --var pair is
// lower(lane)=file:<mountPath>/<fileKey>. The --var name MUST be the
// lowercase lane: the engine's --var override lookup is exact-name against
// the tree's lowercase secret-typed variables (KB-304 name parity), so
// deriving the name from the mount file key instead silently leaves such
// lanes unbound and the run dies with "missing required input(s)".
// Deriving the set keeps non-intake workflows (kanboard_triage_v1's
// kanboard_url/kanboard_app_token) from being dropped by a hardcoded list.
// Only mount paths appear in argv; values reach adapters over OpenSession.
// Env lanes lowering to the same varname are deduped deterministically
// (lexicographically first lane wins); a second binding for one tree var
// is a route authoring defect, not a runtime choice.
func secretVarBindings(plan *workflowPlan) string {
	if plan == nil || len(plan.secrets) == 0 {
		return ""
	}
	pairs := make([]string, 0, 8)
	seen := map[string]bool{}
	for _, sec := range plan.secrets {
		lanes := make([]string, 0, len(sec.Env))
		for lane, key := range sec.Env {
			if lane == "" || key == "" {
				continue
			}
			lanes = append(lanes, lane)
		}
		sort.Strings(lanes)
		for _, lane := range lanes {
			varName := strings.ToLower(lane)
			if seen[varName] {
				continue
			}
			seen[varName] = true
			pairs = append(pairs, varName+"=file:"+joinSecretMount(sec.MountPath, sec.Env[lane]))
		}
	}
	sort.Strings(pairs)
	return strings.Join(pairs, "\n")
}
