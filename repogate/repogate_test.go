package repogate

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMakefileDefinesGateContractTargets(t *testing.T) {
	// A test here runs with the package directory as cwd; the Makefile it
	// pins lives at the repo root, one level up.
	text, err := os.ReadFile(filepath.Join("..", "Makefile"))
	if err != nil {
		t.Fatalf("read ../Makefile: %v", err)
	}
	if missing := MissingGateTargets(string(text)); len(missing) > 0 {
		t.Fatalf("Makefile no longer defines gate-contract targets %v; the develop-loop ci_gate line (build && lint-go && validate && go test ./...) and the develop-run gate (ci && vuln-scan) invoke these by name — a missing target fails the fire like KB-250's `make: No rule to make target 'lint-go'`", missing)
	}
}