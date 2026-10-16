// The repo root is a Go module (github.com/brokenbots/workflow-example) so
// root-level go tool invocations resolve to a module instead of failing with
// "directory prefix . does not contain main module" — the develop-loop
// ci_gate line runs `go test ./...` here, and that leg needs at least one
// matching package (go exits 1 on "no packages to test"). The repogate/
// package fulfills that: it pins the gate-contract make targets.
//
// All application Go code lives in the nested criteria-k8s module (go.mod per
// directory), which `./...` deliberately excludes per Go's nested-module
// semantics. The canonical Go build/vet/test legs remain `make ci` and
// .github/workflows/ci.yml, both running inside criteria-k8s.
module github.com/brokenbots/workflow-example

go 1.26.9
