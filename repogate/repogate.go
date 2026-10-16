// Package repogate pins the repo structural invariants that the external
// gates depend on, so a gate-contract drift fails at test time (and in the
// develop-loop ci_gate's root-level `go test ./...` leg) instead of as an
// opaque `make: No rule to make target` at fire time.
//
// This is the root-module package of github.com/brokenbots/workflow-example:
// the repo root carries a go.mod so root-level go tool invocations resolve to
// a module at all. All application Go code lives in the nested criteria-k8s
// module, which `./...` deliberately excludes per Go's nested-module
// semantics; canonical build/vet/test legs stay `make ci` and
// .github/workflows/ci.yml.
package repogate

import (
	"regexp"
	"strings"
)

// gateContractTargets lists the make targets the develop-loop ci_gate line
// and the Makefile's own documented gate compositions invoke by name:
//
//	build lint-go validate   (ci_gate line, pair_programming_loop/main.chcl)
//	lint                     (ci_gate build fallback: `make validate lint`)
//	ci vuln-scan             (develop-run gate: `make ci && make vuln-scan`)
var gateContractTargets = []string{
	"build", "lint-go", "validate", "lint", "ci", "vuln-scan",
}

var targetLineRe = regexp.MustCompile(`^([^\t#][A-Za-z0-9_./-]*)\s*:`)

// DefinedMakeTargets returns the make targets defined in a Makefile's text.
// Recipe lines (tab-indented) are not target definitions; dotted special
// targets like .PHONY and variable assignments (name := value) are excluded.
func DefinedMakeTargets(makefileText string) []string {
	var targets []string
	for _, line := range strings.Split(makefileText, "\n") {
		m := targetLineRe.FindStringSubmatch(line)
		if m == nil || strings.HasPrefix(m[1], ".") {
			continue
		}
		for _, t := range strings.Fields(m[1]) {
			targets = append(targets, t)
		}
	}
	return targets
}

// MissingGateTargets returns the gate-contract targets that the Makefile
// text does not define.
func MissingGateTargets(makefileText string) []string {
	defined := make(map[string]bool)
	for _, t := range DefinedMakeTargets(makefileText) {
		defined[t] = true
	}
	var missing []string
	for _, t := range gateContractTargets {
		if !defined[t] {
			missing = append(missing, t)
		}
	}
	return missing
}