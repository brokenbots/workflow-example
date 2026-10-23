package routes

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// marshalPayload renders a payload the way the operator would author it.
func marshalPayload(t *testing.T, p *Payload) []byte {
	t.Helper()
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshaling payload: %v", err)
	}
	return data
}

func validPayload() *Payload {
	return &Payload{
		APIVersion: APIVersion,
		Kind:       Kind,
		WorkflowLibrary: map[string]Workflow{
			"wf-default": {
				Type:      TypeImage,
				Image:     "localhost:5000/wf:dev",
				Namespace: "criteria-jobs",
			},
			"wf-override": {
				Type:      TypeURL,
				URL:       "git::https://github.com/example/repo.git//wf",
				Ref:       "0123456789abcdef0123456789abcdef01234567",
				Namespace: "criteria-jobs",
				Interface: &WorkflowInterface{
					APIVersion: InterfaceAPIVersion,
					Kind:       InterfaceKind,
					Workflow:   "wf",
				},
			},
		},
		Routes: []Route{
			{
				Name:     "intake",
				Workflow: "wf-default",
				Project:  "Runner",
				States:   []string{"Triage"},
			},
			{
				Name:     "intake-tagged",
				Workflow: "wf-override",
				Project:  "Runner",
				Tags:     []string{"fast"},
				TagMatch: TagMatchAny,
				States:   []string{"Triage"},
			},
			{
				Name:     "other",
				Workflow: "wf-default",
				Project:  "Other",
				States:   []string{"In Progress"},
			},
		},
	}
}

func selectorFor(project, state string, labels []string, groups map[string]string) Selector {
	return Selector{
		Project:             project,
		State:               state,
		Labels:              labels,
		LabelGroups:         groups,
		WorkflowsLabelGroup: "workflows",
	}
}

// configsSelector is selectorFor with the ticket's single config-flavored
// label placed in the configs label group (KB-103/KB-154).
func configsSelector(tag, project, state string) Selector {
	sel := selectorFor(project, state, []string{tag}, map[string]string{tag: "configs"})
	sel.ConfigsLabelGroup = "configs"
	return sel
}

func TestParseAndValidate(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "k8s", "examples", "routes-configmap.yaml"))
	if err != nil {
		t.Fatalf("reading shipped example: %v", err)
	}
	block := extractRoutesJSON(t, string(data))
	p, err := Parse([]byte(block))
	if err != nil {
		t.Fatalf("Parse(shipped example) failed: %v", err)
	}
	if len(p.WorkflowLibrary) != 9 {
		t.Errorf("workflowLibrary size = %d, want 9 (KB-304: the linear-intake-v1 image object is removed)", len(p.WorkflowLibrary))
	}
	if len(p.Routes) != 9 {
		t.Fatalf("routes size = %d, want 9 (KB-281 retired the criteria triage/develop split routes; KB-304 removed the criteria-cleanup route)", len(p.Routes))
	}
	// KB-281/KB-304: the criteria project's cleanup route is gone. Surviving
	// route order: the two kanboard routes, the KB-311 per-project develop
	// pair, then the tag-gated harness legs on the criteria project.
	if n := p.Routes[0].Name; n != "kanboard-triage" {
		t.Errorf("routes[0] = %q, want %q", n, "kanboard-triage")
	}
	if p.Routes[0].Project != "Kanboard Tickets" || p.Routes[1].Project != "Kanboard Tickets" {
		t.Errorf("routes[0:2] projects = %q/%q, want the kanboard pair on %q", p.Routes[0].Project, p.Routes[1].Project, "Kanboard Tickets")
	}
	// KB-311: the acceptance pair — two distinct projects running THE SAME
	// workflow object (identical tree digest) by binding.
	if p.Routes[2].Project != "Criteria Go Dev" || p.Routes[3].Project != "TypeScript SDK" {
		t.Errorf("routes[2:4] projects = %q/%q, want the KB-311 acceptance pair", p.Routes[2].Project, p.Routes[3].Project)
	}
	if p.Routes[2].Workflow != "kanboard-develop-url" || p.Routes[3].Workflow != "kanboard-develop-url" {
		t.Errorf("routes[2:4] workflows = %q/%q, want BOTH on kanboard-develop-url (same tree, different bindings)", p.Routes[2].Workflow, p.Routes[3].Workflow)
	}
	for _, r := range p.Routes[2:4] {
		if !slices.Equal(r.States, []string{"Ready"}) {
			t.Errorf("route %s states = %v, want [Ready]", r.Name, r.States)
		}
	}
	// KB-277..KB-280 + KB-276: the criteria project's dave-go tag-gated harness
	// series legs (each firing its own *-url object), routes[4]..routes[8]
	// after the KB-304 cleanup-route retirement shifted the indices.
	if n := p.Routes[4].Name; n != "harness-contracts" {
		t.Errorf("routes[4] = %q, want %q (KB-277 harness contracts route)", n, "harness-contracts")
	}
	if p.Routes[4].Project != "Criteria K8s Workflow Runner" {
		t.Errorf("harness-contracts route project = %q, want %q", p.Routes[4].Project, "Criteria K8s Workflow Runner")
	}
	if p.Routes[4].Workflow != "harness-contracts-url" {
		t.Errorf("harness-contracts route workflow = %q, want %q", p.Routes[4].Workflow, "harness-contracts-url")
	}
	hc, ok := p.WorkflowLibrary["harness-contracts-url"]
	if !ok {
		t.Fatalf("workflowLibrary missing harness-contracts-url (KB-277)")
	}
	if hc.Type != TypeURL || hc.Class != ClassTriage || hc.Namespace != "criteria-jobs" ||
		hc.Ref != "" || len(hc.Volumes) != 3 || len(hc.Secrets) != 1 {
		t.Errorf("harness-contracts-url = %+v, want triage-class criteria-jobs url workflow with 3 volumes, 1 secret (github-tokens), no pin (KB-277)", hc)
	}
	// KB-278: the harness-composition leg rides routes[5].
	if n := p.Routes[5].Name; n != "harness-composition" {
		t.Errorf("routes[5] = %q, want %q (KB-278 harness composition route)", n, "harness-composition")
	}
	if p.Routes[5].Workflow != "harness-composition-url" {
		t.Errorf("harness-composition route workflow = %q, want %q", p.Routes[5].Workflow, "harness-composition-url")
	}
	// The object must stay url-ONLY — no commit ref may bind the
	// pinless-at-close harness series (CRI-260 three-way check).
	comp, ok := p.WorkflowLibrary["harness-composition-url"]
	if !ok {
		t.Fatalf("workflowLibrary missing harness-composition-url (KB-278)")
	}
	if comp.Type != TypeURL || comp.Class != ClassTriage || comp.Namespace != "criteria-jobs" ||
		comp.Ref != "" || len(comp.Volumes) != 3 || len(comp.Secrets) != 0 {
		t.Errorf("harness-composition-url = %+v, want triage-class criteria-jobs url workflow with 3 volumes, no secrets, no pin (KB-278)", comp)
	}
	// KB-279: the harness-failures leg rides routes[6] — the failures leg
	// fires harness_failures_v1 (engineered failure-path fixtures, matrix
	// E24-E32 + G12) with the same pinless-at-close convention.
	if n := p.Routes[6].Name; n != "harness-failures" {
		t.Errorf("routes[6] = %q, want %q (KB-279 harness failures route)", n, "harness-failures")
	}
	if p.Routes[6].Project != "Criteria K8s Workflow Runner" {
		t.Errorf("harness-failures route project = %q, want %q", p.Routes[6].Project, "Criteria K8s Workflow Runner")
	}
	if p.Routes[6].Workflow != "harness-failures-url" {
		t.Errorf("harness-failures route workflow = %q, want %q", p.Routes[6].Workflow, "harness-failures-url")
	}
	fail, ok := p.WorkflowLibrary["harness-failures-url"]
	if !ok {
		t.Fatalf("workflowLibrary missing harness-failures-url (KB-279)")
	}
	if fail.Type != TypeURL || fail.Class != ClassTriage || fail.Namespace != "criteria-jobs" ||
		fail.Ref != "" || len(fail.Volumes) != 3 || len(fail.Secrets) != 0 {
		t.Errorf("harness-failures-url = %+v, want triage-class criteria-jobs url workflow with 3 volumes, no secrets, no pin (KB-279)", fail)
	}
	if len(p.Routes[6].Tags) == 0 || p.Routes[6].Tags[0] != "harness-validation" {
		t.Errorf("harness-failures route tags = %v, want the tag-gated harness pair (KB-279)", p.Routes[6].Tags)
	}
	// KB-280: the harness-fleet-smoke leg rides routes[7] — the series'
	// fast-feedback canary (harness_fleet_smoke_v1, ~90s deterministic
	// no-agent pre-leg gate) with the same pinless-at-close convention.
	if n := p.Routes[7].Name; n != "harness-fleet-smoke" {
		t.Errorf("routes[7] = %q, want %q (KB-280 harness fleet-smoke route)", n, "harness-fleet-smoke")
	}
	if p.Routes[7].Workflow != "harness-fleet-smoke-url" {
		t.Errorf("harness-fleet-smoke route workflow = %q, want %q", p.Routes[7].Workflow, "harness-fleet-smoke-url")
	}
	fsmoke, ok := p.WorkflowLibrary["harness-fleet-smoke-url"]
	if !ok {
		t.Fatalf("workflowLibrary missing harness-fleet-smoke-url (KB-280)")
	}
	if fsmoke.Type != TypeURL || fsmoke.Class != ClassTriage || fsmoke.Namespace != "criteria-jobs" ||
		fsmoke.Ref != "" || len(fsmoke.Volumes) != 3 || len(fsmoke.Secrets) != 0 {
		t.Errorf("harness-fleet-smoke-url = %+v, want triage-class criteria-jobs url workflow with 3 volumes, no secrets, no pin (KB-280)", fsmoke)
	}
	// KB-280 is the no-agent slice: the object pins the Shell adapter ONLY
	// (no copilot identity to image — the canary declares no copilot member).
	if fsmoke.AdapterImages == nil {
		t.Errorf("harness-fleet-smoke-url adapterImages missing — want the shell-only surface (KB-280)")
	} else if _, hasCopilot := fsmoke.AdapterImages["copilot"]; hasCopilot {
		t.Errorf("harness-fleet-smoke-url adapterImages pins copilot — the canary is the no-agent slice (KB-280)")
	} else if fsmoke.AdapterImages["shell"] != "localhost:5000/criteria-adapter-shell-peer:k8s-0.5.56-1" {
		t.Errorf("harness-fleet-smoke-url shell pin = %q, want the recorded fleet pin set (KB-280)", fsmoke.AdapterImages["shell"])
	}
	// KB-276: the wire/transport leg closes the routes at [8]; its url object
	// ships in the same payload.
	if n := p.Routes[8].Name; n != "harness-wire" {
		t.Errorf("routes[8] = %q, want %q (KB-276 harness wire/transport route)", n, "harness-wire")
	}
	if p.Routes[8].Project != "Criteria K8s Workflow Runner" {
		t.Errorf("harness-wire route project = %q, want %q", p.Routes[8].Project, "Criteria K8s Workflow Runner")
	}
	if p.Routes[8].Workflow != "harness-wire-url" {
		t.Errorf("harness-wire route workflow = %q, want %q", p.Routes[8].Workflow, "harness-wire-url")
	}
	wurl, ok := p.WorkflowLibrary["harness-wire-url"]
	if !ok {
		t.Fatalf("workflowLibrary missing the harness-wire-url object (KB-276)")
	}
	if wurl.Class != ClassTriage {
		t.Errorf("harness-wire-url class = %q, want triage (tag-gated dave-go gate (KB-276))", wurl.Class)
	}
	// Absence regressions (KB-281/KB-304): the retired linear objects must
	// not return in the shipped example.
	for _, gone := range []string{"linear-intake-url", "linear-triage-url", "linear-develop-url", "linear-intake-v1"} {
		if _, still := p.WorkflowLibrary[gone]; still {
			t.Errorf("workflowLibrary still has the retired object %q (KB-281/KB-304)", gone)
		}
	}
	for _, r := range p.Routes {
		if r.Workflow == "linear-intake-url" || r.Workflow == "linear-triage-url" || r.Workflow == "linear-develop-url" {
			t.Errorf("route %q still fires the retired linear workflow %q (KB-281)", r.Name, r.Workflow)
		}
	}
	// The live kanboard objects survive KB-304 untouched.
	kb, ok := p.WorkflowLibrary["kanboard-triage-url"]
	if !ok {
		t.Fatalf("workflowLibrary missing kanboard-triage-url")
	}
	if kb.Type != TypeURL || kb.Class != ClassTriage || len(kb.Volumes) != 3 || len(kb.Secrets) != 2 {
		t.Errorf("kanboard-triage-url = %+v, want triage-class url workflow with 3 volumes and 2 secrets", kb)
	}
	// decision-demo-url (KB-205) is route prep only: a url workflow with no
	// route/ref/volumes/secrets yet — KB-207 wires those in one coherent
	// change when the validation run starts gating the demo.
	dec, ok := p.WorkflowLibrary["decision-demo-url"]
	if !ok {
		t.Fatalf("workflowLibrary missing decision-demo-url")
	}
	if dec.Type != TypeURL || dec.URL == "" || dec.Namespace != "criteria-jobs" ||
		dec.Class != "" || dec.Ref != "" || len(dec.Volumes) != 0 || len(dec.Secrets) != 0 {
		t.Errorf("decision-demo-url = %+v, want plain criteria-jobs url workflow with route-facing fields left for KB-207", dec)
	}
	kbRoutes := 0
	for _, r := range p.Routes {
		if r.Project == "Kanboard Tickets" {
			kbRoutes++
		}
	}
	if kbRoutes != 2 {
		t.Errorf("kanboard routes = %d, want 2 (triage + develop on the Kanboard Tickets project)", kbRoutes)
	}
}

// KB-281 + KB-304: the shipped example's live routing is the kanboard pair.
// A ticket in "Backlog" on the Kanboard Tickets project resolves to the
// kanboard-triage-url object (kanboard_triage_v1, url-only with the ADR-0005
// D7 commit pin, triage class), a ticket in "Ready" resolves to the
// kanboard-develop-url object (kanboard_develop_v1, pinned likewise), every
// retired criteria-project route fails closed, and the watcher-facing
// TicketStates union holds the kanboard states plus Ready for Development
// (carried solely by the criteria project's tag-gated harness legs).
func TestShippedExampleResolvesKanboardRoutes(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "k8s", "examples", "routes-configmap.yaml"))
	if err != nil {
		t.Fatalf("reading shipped example: %v", err)
	}
	p, err := Parse([]byte(extractRoutesJSON(t, string(data))))
	if err != nil {
		t.Fatalf("Parse(shipped example) failed: %v", err)
	}

	sel, err := p.Resolve(selectorFor("Kanboard Tickets", "Backlog", []string{"k8s-run"}, nil))
	if err != nil {
		t.Fatalf("Resolve(Kanboard Tickets, Backlog): %v", err)
	}
	if sel.Route.Name != "kanboard-triage" {
		t.Errorf("matched route = %q, want %q", sel.Route.Name, "kanboard-triage")
	}
	if sel.Name != "kanboard-triage-url" {
		t.Errorf("resolved workflow = %q, want %q", sel.Name, "kanboard-triage-url")
	}
	wf := sel.Workflow
	if wf.Type != TypeURL {
		t.Errorf("workflow type = %q, want %q (url-only, minimal criteria base runtime)", wf.Type, TypeURL)
	}
	if wf.URL != "git::https://github.com/brokenbots/workflow-example.git//kanboard_triage_v1?ref=63535719e37750301a8b7cc9be2c74b2d18c847e" {
		t.Errorf("workflow url = %q, want the kanboard_triage_v1 subtree pinned to the main-landed commit", wf.URL)
	}
	// Ref pins track main-landed squash commits (CRI-241/CRI-243 convention).
	if wf.Ref != "63535719e37750301a8b7cc9be2c74b2d18c847e" {
		t.Errorf("workflow ref = %q, want the main-landed kanboard_triage_v1 commit", wf.Ref)
	}
	if wf.Image != "" {
		t.Errorf("workflow image = %q, want empty (url-only must not declare a process image)", wf.Image)
	}
	if wf.Class != ClassTriage {
		t.Errorf("kanboard-triage-url class = %q, want %q (CRI-242 read-only admission)", wf.Class, ClassTriage)
	}
	if len(wf.Volumes) != 3 || len(wf.Secrets) != 2 {
		t.Errorf("kanboard-triage-url = %+v, want 3 volumes and 2 secrets", wf)
	}

	// The develop side of the split: Ready resolves to the
	// kanboard-develop-url object — no default class (dev admission), same
	// main-landed pin convention.
	develop, err := p.Resolve(selectorFor("Kanboard Tickets", "Ready", []string{"k8s-run"}, nil))
	if err != nil {
		t.Fatalf("Resolve(Kanboard Tickets, Ready): %v", err)
	}
	if develop.Route.Name != "kanboard-develop" || develop.Name != "kanboard-develop-url" {
		t.Errorf("Ready resolves to %q/%q, want kanboard-develop/kanboard-develop-url", develop.Route.Name, develop.Name)
	}
	developWf := develop.Workflow
	if developWf.Type != TypeURL || developWf.Image != "" {
		t.Errorf("kanboard-develop-url = %+v, want url-only (no process image)", developWf)
	}
	if developWf.URL != "git::https://github.com/brokenbots/workflow-example.git//kanboard_develop_v1?ref=63535719e37750301a8b7cc9be2c74b2d18c847e" {
		t.Errorf("develop workflow url = %q, want the kanboard_develop_v1 subtree pinned to the main-landed commit", developWf.URL)
	}
	if len(developWf.Volumes) != 3 || len(developWf.Secrets) != 2 {
		t.Errorf("kanboard-develop-url = %+v, want 3 volumes and 2 secrets", developWf)
	}

	// KB-281/KB-304 absence regressions: every retired criteria-project
	// route fails closed (the watcher's routes fail closed on unmapped
	// shapes).
	for _, tc := range []struct{ name, state string }{
		{"Triage state (criteria-triage retired, KB-281)", "Triage"},
		{"Backlog state (criteria routes never covered it)", "Backlog"},
		{"Ready without the dirty label (develop route retired KB-281; cleanup route retired KB-304)", "Ready"},
	} {
		if _, err := p.Resolve(selectorFor("Criteria K8s Workflow Runner", tc.state, []string{"k8s-run"}, nil)); !errors.Is(err, ErrNoRoute) {
			t.Errorf("Resolve(Criteria project, %s) err = %v, want ErrNoRoute (%s)", tc.state, err, tc.name)
		}
	}

	// KB-304: with the cleanup route retired nothing covers Ready for
	// Development without the harness-validation tag, so a RfD ticket
	// carrying the legacy linear override group fails closed — the
	// workflows-label override only remaps the object on a MATCHED route,
	// and the retired linear object it used to name is gone.
	if _, err := p.Resolve(selectorFor("Criteria K8s Workflow Runner", "Ready for Development", []string{"k8s-run", "criteria-dirty", "linear-intake-v1"},
		map[string]string{"linear-intake-v1": "workflows"})); !errors.Is(err, ErrNoRoute) {
		t.Errorf("Resolve(Ready for Development, linear override) err = %v, want ErrNoRoute (cleanup route retired, linear override unreachable)", err)
	}

	// The union of declared states is what the watcher queries for (the
	// criteria project's tag-gated harness legs carry Ready for Development;
	// each watcher filters per-project at Resolve time).
	want := []string{"Backlog", "Ready", "Ready for Development"}
	if got := p.TicketStates(); !slices.Equal(got, want) {
		t.Errorf("TicketStates() = %v, want %v", got, want)
	}
}

// KB-273: the shipped 'castle' configLibrary entry mirrors the live
// criteria-jobs entry (created 2026-10-16 on KB-270) with the KB-273 gate
// split. The live pre-split gate ran the parapet npm suite on whatever
// node the session pod ships; castle's shell peer (Node 24) produced the
// cross-realm AbortSignal false-failure wall of KB-270 run f5c205c2, and
// the suite is only green under Node 20 — the version castle's own GitHub
// CI pins (README-documented trap: verify parapet the CI way). The
// run-side gate therefore keeps the Go/proto legs in-session and pins the
// parapet leg (npm ci + test:run) to the Node 20 runtime on the PVC
// toolchain at /data/tools/node20; the vite production build (make build's
// build-parapet leg) stays GitHub-CI-only, and proto-check-drift runs
// after the parapet npm ci because its generated-bindings plugins come
// from parapet/node_modules/.bin. If this test fails, the shipped example
// drifted back toward the ambient-node npm gate and live/tree diverged
// (ConfigRenderDrift): re-sync both, don't ship the drift.
// KB-310: the develop tree's interface declares base_branch and
// ci_gate_cmd as its config slots, so the entry carries no buildCmd/testCmd
// keys — unknown slots reject fail-closed.
func TestShippedExampleCastleGatePinsNode20ParapetSuite(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "k8s", "examples", "routes-configmap.yaml"))
	if err != nil {
		t.Fatalf("reading shipped example: %v", err)
	}
	p, err := Parse([]byte(extractRoutesJSON(t, string(data))))
	if err != nil {
		t.Fatalf("Parse(shipped example) failed: %v", err)
	}
	castle, ok := p.ConfigLibrary["castle"]
	if !ok {
		t.Fatalf("configLibrary missing the castle entry (mirrors the live criteria-jobs CM)")
	}
	if castle.BuildCmd != "" || castle.TestCmd != "" {
		// KB-310: kanboard_develop_v1 declares no build_cmd/test_cmd slot,
		// so config keys outside the declared slots reject fail-closed —
		// the build/test legs live inside ciGateCmd, never as pinned
		// buildCmd/testCmd keys.
		t.Errorf("castle buildCmd = %q, testCmd = %q, want empty (KB-310 declares no such slots; the legs ride ciGateCmd)", castle.BuildCmd, castle.TestCmd)
	}
	gate := castle.CIGateCmd
	// The in-session legs: Go/proto gates under the PVC toolchain path,
	// castle's CGO race suite, and the parapet suite under the pinned
	// Node 20 runtime — never the ambient session pod's node.
	for _, leg := range []string{
		"/data/tools/node20/bin",
		"export CGO_ENABLED=1",
		"make build-castle test-conformance proto-lint",
		"(cd castle && CGO_ENABLED=1 go test -race ./...)",
		"(cd parapet && npm ci && npm run test:run)",
		"make proto-check-drift",
	} {
		if !strings.Contains(gate, leg) {
			t.Errorf("castle ciGateCmd missing %q: %q", leg, gate)
		}
	}
	// The gate is exactly the split: no monolithic make test/build goals
	// (the KB-270-era chain carried them and ran npm on the ambient node)
	// and every npm invocation belongs to the pinned parapet leg.
	if strings.Contains(gate, "make build test") {
		t.Errorf("castle ciGateCmd reintroduced the monolithic KB-270-era goals: %q", gate)
	}
	if strings.Contains(gate, " make test;") || strings.Contains(gate, " make test &&") {
		t.Errorf("castle ciGateCmd runs make test directly: %q (parapet leg must stay npm-pinned)", gate)
	}
	if n := strings.Count(gate, "npm"); n != 2 {
		t.Errorf("castle ciGateCmd carries %d npm invocations, want exactly 2 (ci + test:run, only inside the pinned parapet leg): %q", n, gate)
	}
}

// KB-304: the criteria-cleanup route (CRI-310's dirty-label routing surface)
// is retired — the Linear-era cleanup pass is dead and the example carries
// only the tag-gated harness legs on the criteria project. Every dirty-label
// resolution on the criteria project fails closed, and the surviving
// ticket-cleanup-url object is checked directly in the library (unreferenced
// library objects are legal; the tree's ticket_cleanup_v1 interface keeps
// the linear-api-key binding, so the object shape is unchanged).
func TestShippedExampleRetiredCleanupRoute(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "k8s", "examples", "routes-configmap.yaml"))
	if err != nil {
		t.Fatalf("reading shipped example: %v", err)
	}
	p, err := Parse([]byte(extractRoutesJSON(t, string(data))))
	if err != nil {
		t.Fatalf("Parse(shipped example) failed: %v", err)
	}

	// KB-304: the dirty label no longer routes anywhere — the criteria-cleanup
	// route is retired, so the validation run C path fails closed.
	if _, err := p.Resolve(selectorFor("Criteria K8s Workflow Runner", "Ready for Development", []string{"criteria-dirty"}, nil)); !errors.Is(err, ErrNoRoute) {
		t.Errorf("dirty RfD ticket err = %v, want ErrNoRoute (the cleanup route is retired, KB-304)", err)
	}

	// The retired route's ticket-cleanup-url object survives in the
	// workflowLibrary and keeps its tree-contract shape: url-only, pinned to
	// the squash-merge commit, triage (read-only) admission, the github-tokens
	// CSI mount for the base entrypoint's repo-clone init, and linear-api-key
	// riding at /home/criteria/linear-secrets (the runner script's hardcoded
	// file: OriginRef path, lstat-checked before any step).
	cleanupWf, ok := p.WorkflowLibrary["ticket-cleanup-url"]
	if !ok {
		t.Fatalf("workflowLibrary missing ticket-cleanup-url")
	}
	if cleanupWf.Type != TypeURL {
		t.Errorf("cleanup workflow type = %q, want %q (url-only, minimal criteria base runtime)", cleanupWf.Type, TypeURL)
	}
	if !strings.HasPrefix(cleanupWf.URL, "git::https://github.com/brokenbots/workflow-example.git//ticket_cleanup_v1") {
		t.Errorf("cleanup workflow url = %q, want the ticket_cleanup_v1 subtree", cleanupWf.URL)
	}
	if cleanupWf.Ref != "0b8136650d5622381bf18a597ed93b8c272ba19b" {
		t.Errorf("cleanup workflow ref = %q, want the ticket_cleanup_v1 squash-merge commit 0b813665 (pinless HEAD resolution fails on the subtree; CRI-241/CRI-243 follow-up re-pin convention)", cleanupWf.Ref)
	}
	if cleanupWf.Image != "" {
		t.Errorf("cleanup workflow image = %q, want empty (url-only must not declare a process image)", cleanupWf.Image)
	}
	if cleanupWf.Class != ClassTriage {
		t.Errorf("cleanup workflow class = %q, want %q (CRI-242 read-only admission)", cleanupWf.Class, ClassTriage)
	}
	if len(cleanupWf.Volumes) != 2 || len(cleanupWf.Secrets) != 2 {
		t.Errorf("ticket-cleanup-url = %+v, want 2 volumes (data + repo — scratch is not declared by the tree interface, KB-310) and 2 secrets (linear-api-key + the github-tokens CSI mount: the entrypoint's repo-clone requires the token surface, the cleanup pass itself does not consume its env)", cleanupWf)
	}
	for _, s := range cleanupWf.Secrets {
		if s.Name == "linear-api-key" && s.MountPath != "/home/criteria/linear-secrets" {
			t.Errorf("ticket-cleanup-url linear-api-key mountPath = %q, want /home/criteria/linear-secrets (the runner's file: OriginRef path)", s.MountPath)
		}
	}

	// KB-281: the tag-less general develop route is retired, so a ticket
	// WITHOUT the dirty label still fails closed (nothing matches).
	if _, err := p.Resolve(selectorFor("Criteria K8s Workflow Runner", "Ready for Development", []string{"k8s-run"}, nil)); !errors.Is(err, ErrNoRoute) {
		t.Errorf("non-dirty ticket err = %v, want ErrNoRoute (the general develop route is retired)", err)
	}

	// KB-304: the dirty label no longer opens any state gate — a dirty ticket
	// in "Backlog" fails closed (no criteria route covers Backlog at all).
	if _, err := p.Resolve(selectorFor("Criteria K8s Workflow Runner", "Backlog", []string{"criteria-dirty"}, nil)); !errors.Is(err, ErrNoRoute) {
		t.Errorf("dirty Backlog ticket err = %v, want ErrNoRoute (no criteria route covers Backlog)", err)
	}

	// KB-103's configRef lived on the retired criteria-develop route; no
	// surviving route binds a config entry by default.
	for _, r := range p.Routes {
		if r.Name == "criteria-develop" {
			t.Fatalf("the criteria-develop route returned after KB-281")
		}
	}

	// KB-281/KB-304 retired every linear-era criteria route; the union now
	// holds only the kanboard states plus Ready for Development (carried by
	// the criteria project's tag-gated harness legs).
	want := []string{"Backlog", "Ready", "Ready for Development"}
	if got := p.TicketStates(); !slices.Equal(got, want) {
		t.Errorf("TicketStates() = %v, want %v (the union holds only the surviving routes' states)", got, want)
	}
}

// extractRoutesJSON pulls the routes.json scalar out of the shipped example
// ConfigMap, mirroring how the kubelet projects it into the mounted file.
func extractRoutesJSON(t *testing.T, cm string) string {
	t.Helper()
	marker := "routes.json: |"
	idx := strings.Index(cm, marker)
	if idx < 0 {
		t.Fatalf("example ConfigMap has no %q block", marker)
	}
	rest := cm[idx+len(marker):]
	// The scalar is every following line indented by four spaces.
	var lines []string
	for _, line := range strings.Split(rest, "\n")[1:] {
		if strings.TrimSpace(line) == "" {
			break
		}
		if !strings.HasPrefix(line, "    ") {
			break
		}
		lines = append(lines, strings.TrimPrefix(line, "    "))
	}
	return strings.Join(lines, "\n")
}

func TestResolveDefaultWorkflow(t *testing.T) {
	p := validPayload()
	sel, err := p.Resolve(selectorFor("Runner", "Triage", []string{"k8s-run"}, nil))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if sel.Name != "wf-default" {
		t.Errorf("resolved workflow = %q, want project default %q", sel.Name, "wf-default")
	}
	if sel.Route.Name != "intake" {
		t.Errorf("matched route = %q, want %q (tag-less route ignored the unrelated label)", sel.Route.Name, "intake")
	}
}

func TestResolveLabelGroupOverride(t *testing.T) {
	p := validPayload()
	groups := map[string]string{"wf-override": "workflows", "k8s-run": "gates"}
	sel, err := p.Resolve(selectorFor("Runner", "Triage", []string{"k8s-run", "wf-override"}, groups))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if sel.Name != "wf-override" {
		t.Errorf("resolved workflow = %q, want label override %q", sel.Name, "wf-override")
	}
}

func TestResolveUnknownWorkflowFailsClosed(t *testing.T) {
	p := validPayload()
	groups := map[string]string{"no-such-wf": "workflows"}
	_, err := p.Resolve(selectorFor("Runner", "Triage", []string{"no-such-wf"}, groups))
	if !errors.Is(err, ErrUnknownWorkflow) {
		t.Fatalf("err = %v, want ErrUnknownWorkflow", err)
	}
}

func TestResolveAmbiguousWorkflowsFailsClosed(t *testing.T) {
	p := validPayload()
	groups := map[string]string{"wf-a": "workflows", "wf-b": "workflows"}
	_, err := p.Resolve(selectorFor("Runner", "Triage", []string{"wf-a", "wf-b"}, groups))
	if !errors.Is(err, ErrAmbiguousWorkflow) {
		t.Fatalf("err = %v, want ErrAmbiguousWorkflow", err)
	}
}

func TestResolveUnmappedProjectOrState(t *testing.T) {
	p := validPayload()
	cases := []struct {
		name    string
		project string
		state   string
	}{
		{"unmapped project", "Unknown Project", "Triage"},
		{"unmapped state", "Runner", "Done"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := p.Resolve(selectorFor(tc.project, tc.state, nil, nil))
			if !errors.Is(err, ErrNoRoute) {
				t.Fatalf("err = %v, want ErrNoRoute", err)
			}
		})
	}
}

func TestMatchRouteTagPreference(t *testing.T) {
	p := validPayload()

	// The tag-specific route wins when its tags are satisfied...
	sel, err := p.Resolve(selectorFor("Runner", "Triage", []string{"fast"}, nil))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if sel.Route.Name != "intake-tagged" {
		t.Errorf("matched route = %q, want tag-specific %q", sel.Route.Name, "intake-tagged")
	}

	// ...but a project without the tag still reaches the tag-less route.
	if _, err := p.Resolve(selectorFor("Runner", "Triage", nil, nil)); err != nil {
		t.Fatalf("Resolve without tags: %v", err)
	}

	// A route whose tags are not satisfied must not match.
	restricted := validPayload()
	restricted.Routes = []Route{{
		Name:     "strict",
		Workflow: "wf-default",
		Project:  "Runner",
		Tags:     []string{"must-have"},
		TagMatch: TagMatchAll,
		States:   []string{"Triage"},
	}}
	if _, err := restricted.Resolve(selectorFor("Runner", "Triage", []string{"other-tag"}, nil)); !errors.Is(err, ErrNoRoute) {
		t.Fatalf("err = %v, want ErrNoRoute for unsatisfied tag subset", err)
	}
}

// CRI-218: a route omitting states defaults to [Triage] at parse time.
func TestParseDefaultsOmittedStatesToTriage(t *testing.T) {
	// Raw JSON without a states key — the schema accepts the omission and
	// documents the [Triage] default.
	data := []byte(`{"apiVersion":"criteria.brokenbots.dev/v1","kind":"Routes","workflowLibrary":{"wf-default":{"type":"image","image":"i","namespace":"criteria-jobs"}},"routes":[{"name":"intake","workflow":"wf-default","project":"Runner"}]}`)
	got, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse(payload without states): %v", err)
	}
	if len(got.Routes) != 1 || !slices.Equal(got.Routes[0].States, []string{DefaultState}) {
		t.Fatalf("parsed states = %v, want [%s]", got.Routes[0].States, DefaultState)
	}
}

// CRI-218: an explicit empty (or null) states list is rejected at parse
// time — the schema of record declares minItems: 1, so the Go JSON path
// must stay fail-closed instead of silently defaulting to [Triage]. This
// pins the JSON decode path (not Validate in isolation) against drift.
func TestParseRejectsExplicitEmptyOrNullStates(t *testing.T) {
	const head = `{"apiVersion":"criteria.brokenbots.dev/v1","kind":"Routes","workflowLibrary":{"wf-default":{"type":"image","image":"i","namespace":"criteria-jobs"}},"routes":[{"name":"intake","workflow":"wf-default","project":"Runner"`
	for _, statesJSON := range []string{`"states":[]`, `"states":null`} {
		t.Run(statesJSON, func(t *testing.T) {
			data := []byte(head + "," + statesJSON + `}]}`)
			_, err := Parse(data)
			if err == nil {
				t.Fatalf("Parse(%s) succeeded; want an explicit states list of no entries to be rejected", statesJSON)
			}
			if !strings.Contains(err.Error(), "states") {
				t.Fatalf("Parse(%s) err = %v; want a states validation error", statesJSON, err)
			}
		})
	}
}

func TestResolveOmittedStatesBehavesAsTriage(t *testing.T) {
	// One route omits states; the other declares [Triage] explicitly. Both
	// must behave identically.
	raw := []byte(`{"apiVersion":"criteria.brokenbots.dev/v1","kind":"Routes","workflowLibrary":{"wf-default":{"type":"image","image":"i","namespace":"criteria-jobs"}},"routes":[
		{"name":"implicit","workflow":"wf-default","project":"Runner"},
		{"name":"explicit","workflow":"wf-default","project":"Other","states":["Triage"]}
	]}`)
	p, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	sel, err := p.Resolve(selectorFor("Runner", "Triage", nil, nil))
	if err != nil {
		t.Fatalf("Resolve(implicit route, Triage): %v", err)
	}
	if sel.Route.Name != "implicit" {
		t.Errorf("matched route = %q, want implicit", sel.Route.Name)
	}
	if sel, err := p.Resolve(selectorFor("Other", "Triage", nil, nil)); err != nil || sel.Route.Name != "explicit" {
		t.Errorf("Resolve(explicit route, Triage) = %v, %v; want explicit match", sel, err)
	}
	// A state outside the effective [Triage] list never matches.
	if _, err := p.Resolve(selectorFor("Runner", "In Progress", nil, nil)); !errors.Is(err, ErrNoRoute) {
		t.Fatalf("err = %v, want ErrNoRoute for unlisted state", err)
	}
}

func TestResolveCustomStatesList(t *testing.T) {
	p := validPayload()
	p.Routes = []Route{{
		Name:     "wide",
		Workflow: "wf-default",
		Project:  "Runner",
		States:   []string{"Triage", "In Progress"},
	}}
	for _, state := range []string{"Triage", "In Progress"} {
		sel, err := p.Resolve(selectorFor("Runner", state, nil, nil))
		if err != nil {
			t.Fatalf("Resolve(%q): %v", state, err)
		}
		if sel.Route.Name != "wide" {
			t.Errorf("Resolve(%q) matched route %q; want wide", state, sel.Route.Name)
		}
	}
	if _, err := p.Resolve(selectorFor("Runner", "Done", nil, nil)); !errors.Is(err, ErrNoRoute) {
		t.Fatalf("err = %v, want ErrNoRoute for state outside the route's list", err)
	}
}

// Fail closed: Linear tickets whose state is unresolvable (empty selector
// state) never match any route.
func TestResolveUnresolvableStateFailsClosed(t *testing.T) {
	p := validPayload()
	for _, state := range []string{"", "  "} {
		if _, err := p.Resolve(selectorFor("Runner", state, nil, nil)); !errors.Is(err, ErrNoRoute) {
			t.Fatalf("Resolve(state %q) err = %v, want ErrNoRoute", state, err)
		}
	}
}

func TestTicketStates(t *testing.T) {
	var p Payload
	p.Routes = []Route{
		{States: []string{"In Progress", "Triage"}},
		{States: []string{"Triage", "Done"}},
		{States: nil},
	}
	got := p.TicketStates()
	want := []string{"Done", "In Progress", "Triage"}
	if !slices.Equal(got, want) {
		t.Errorf("TicketStates() = %v, want sorted dedup %v", got, want)
	}
	if s := (&Payload{}).TicketStates(); len(s) != 0 {
		t.Errorf("TicketStates() with no routes = %v, want empty", s)
	}
}

func TestLoadFileAndPerPollReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), DataKey)
	original := marshalPayload(t, validPayload())
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}

	first, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if first.Routes[0].Workflow != "wf-default" {
		t.Fatalf("first load route workflow = %q, want %q", first.Routes[0].Workflow, "wf-default")
	}

	// A subsequent poll must observe the edited payload without any restart
	// hook: LoadFile is stateless, the watcher simply calls it again.
	changed := strings.Replace(string(original), `"workflow":"wf-default"`, `"workflow":"wf-override"`, 1)
	if err := os.WriteFile(path, []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile after change: %v", err)
	}
	if second.Routes[0].Workflow != "wf-override" {
		t.Errorf("second load route workflow = %q, want changed %q", second.Routes[0].Workflow, "wf-override")
	}

	// A missing file (ConfigMap not applied yet) fails closed.
	if _, err := LoadFile(filepath.Join(t.TempDir(), "absent", DataKey)); err == nil {
		t.Error("LoadFile on missing file should fail")
	}
}

func TestValidateNegative(t *testing.T) {
	cases := []struct {
		name    string
		mutator func(*Payload)
		wantSub string
	}{
		{"bad apiVersion", func(p *Payload) { p.APIVersion = "v2" }, "apiVersion"},
		{"bad kind", func(p *Payload) { p.Kind = "Other" }, "kind"},
		{"empty library", func(p *Payload) { p.WorkflowLibrary = nil }, "workflowLibrary"},
		{"empty routes", func(p *Payload) { p.Routes = nil }, "routes"},
		{
			"image workflow with ref (D2 fail-closed)",
			func(p *Payload) {
				p.WorkflowLibrary["wf-default"] = Workflow{Type: TypeImage, Image: "i", Namespace: "n", Ref: "abc"}
			},
			"url or ref",
		},
		{
			"url workflow without url",
			func(p *Payload) { p.WorkflowLibrary["wf-default"] = Workflow{Type: TypeURL, Namespace: "n"} },
			"requires a url",
		},
		{
			"bad workflow type",
			func(p *Payload) {
				p.WorkflowLibrary["wf-default"] = Workflow{Type: "tarball", Image: "i", Namespace: "n"}
			},
			"type must be",
		},
		{
			"pvc volume without claim",
			func(p *Payload) {
				p.WorkflowLibrary["wf-default"] = Workflow{
					Type: TypeImage, Image: "i", Namespace: "n",
					Volumes: []Volume{{Name: "v", Kind: VolumePVC, MountPath: "/v"}},
				}
			},
			"requires a claim",
		},
		{
			"nfs volume without server",
			func(p *Payload) {
				p.WorkflowLibrary["wf-default"] = Workflow{
					Type: TypeImage, Image: "i", Namespace: "n",
					Volumes: []Volume{{Name: "v", Kind: VolumeNFS, MountPath: "/v", Path: "/p"}},
				}
			},
			"requires server and path",
		},
		{
			"tmp volume with claim",
			func(p *Payload) {
				p.WorkflowLibrary["wf-default"] = Workflow{
					Type: TypeImage, Image: "i", Namespace: "n",
					Volumes: []Volume{{Name: "v", Kind: VolumeTmp, MountPath: "/v", Claim: "c"}},
				}
			},
			"must not declare claim",
		},
		{
			"k8s-secret volume without secretName",
			func(p *Payload) {
				p.WorkflowLibrary["wf-default"] = Workflow{
					Type: TypeImage, Image: "i", Namespace: "n",
					Volumes: []Volume{{Name: "v", Kind: VolumeK8sSecret, MountPath: "/v"}},
				}
			},
			"requires a secretName",
		},
		{
			"k8s-secret volume with claim",
			func(p *Payload) {
				p.WorkflowLibrary["wf-default"] = Workflow{
					Type: TypeImage, Image: "i", Namespace: "n",
					Volumes: []Volume{{Name: "v", Kind: VolumeK8sSecret, MountPath: "/v", SecretName: "s", Claim: "c"}},
				}
			},
			"must not declare claim",
		},
		{
			"secret without provider class",
			func(p *Payload) {
				p.WorkflowLibrary["wf-default"] = Workflow{
					Type: TypeImage, Image: "i", Namespace: "n",
					Secrets: []Secret{{Name: "s", MountPath: "/secrets"}},
				}
			},
			"secretProviderClass",
		},
		{
			"route referencing unknown workflow",
			func(p *Payload) { p.Routes[0].Workflow = "missing" },
			"not present in workflowLibrary",
		},
		{
			"route without states",
			func(p *Payload) { p.Routes[0].States = nil },
			"states",
		},
		{
			"bad tagMatch",
			func(p *Payload) { p.Routes[0].TagMatch = "some" },
			"tagMatch",
		},
		{
			"duplicate route name",
			func(p *Payload) { p.Routes[1].Name = p.Routes[0].Name },
			"not unique",
		},
		{
			"uppercase workflow name",
			func(p *Payload) { p.WorkflowLibrary["Bad_Name"] = p.WorkflowLibrary["wf-default"] },
			"DNS-1123",
		},
		{
			"relative volume mount path",
			func(p *Payload) {
				p.WorkflowLibrary["wf-default"] = Workflow{
					Type: TypeImage, Image: "i", Namespace: "n",
					Volumes: []Volume{{Name: "v", Kind: VolumeTmp, MountPath: "relative"}},
				}
			},
			"absolute",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := validPayload()
			tc.mutator(p)
			err := p.Validate()
			if err == nil {
				t.Fatalf("Validate succeeded, want error containing %q", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("err = %v, want substring %q", err, tc.wantSub)
			}
		})
	}
}

// KB-7: a k8s-secret volume mounts a plain namespace Secret as files, e.g.
// to serve /home/criteria/secrets/workflow_github_token without a
// pre-populated PVC. secretName is required and the sibling-kind fields are
// rejected.
func TestValidateK8sSecretVolume(t *testing.T) {
	p := validPayload()
	p.WorkflowLibrary["wf-k8s-secret"] = Workflow{
		Type: TypeImage, Image: "i", Namespace: "n",
		Volumes: []Volume{{
			Name: "tokens", Kind: VolumeK8sSecret, MountPath: "/home/criteria/secrets",
			SecretName: "github-tokens",
			Env:        map[string]string{"WORKFLOW_GITHUB_TOKEN": "workflow_github_token"},
		}},
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("Validate(k8s-secret volume) failed: %v", err)
	}
}

// KB-310: every type=url workflow carries its tree's machine-readable
// interface (the admission contract) and type=image carries none. The
// interface's own shape validates against k8s/workflow-interface.schema.json.
func TestValidateWorkflowInterfaceRequirement(t *testing.T) {
	p := validPayload()
	wf := p.WorkflowLibrary["wf-override"]
	wf.Interface = nil
	p.WorkflowLibrary["wf-override"] = wf
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "type=url requires interface") {
		t.Errorf("Validate(url without interface) = %v, want fail-closed gap", err)
	}

	// Legacy carve-out: the baked-tree image path has no declared contract.
	img := validPayload()
	if err := img.Validate(); err != nil {
		t.Fatalf("Validate(image-only) failed: %v", err)
	}
	iwf := img.WorkflowLibrary["wf-default"]
	iwf.Interface = &WorkflowInterface{APIVersion: InterfaceAPIVersion, Kind: InterfaceKind, Workflow: "wf"}
	img.WorkflowLibrary["wf-default"] = iwf
	if err := img.Validate(); err == nil || !strings.Contains(err.Error(), "type=image must not declare interface") {
		t.Errorf("Validate(image with interface) = %v, want rejection", err)
	}
}

func TestValidateWorkflowInterfaceShape(t *testing.T) {
	valid := func() *WorkflowInterface {
		return &WorkflowInterface{
			APIVersion: InterfaceAPIVersion,
			Kind:       InterfaceKind,
			Workflow:   "kanboard_develop_v1",
			Adapters:   []string{"shell", "copilot"},
			Mounts:     []WorkflowInterfaceMount{{Name: "workspace", MountPath: "/home/criteria/workspace", Kind: "pvc"}},
			SecretEnv:  []string{"WORKFLOW_GITHUB_TOKEN"},
			Env:        []string{"CRITERIA_RUN_MODE"},
			ConfigSlots: []WorkflowInterfaceConfigSlot{{
				Slot: "ciGateCmd", Variable: "ci_gate_cmd", Target: "qa_gate",
			}},
		}
	}
	for label, mutate := range map[string]func(*WorkflowInterface){
		"apiVersion": func(i *WorkflowInterface) { i.APIVersion = "v9" },
		"kind":       func(i *WorkflowInterface) { i.Kind = "Workflow" },
		"empty tree": func(i *WorkflowInterface) { i.Workflow = "" },
		"bad tree":   func(i *WorkflowInterface) { i.Workflow = "1bad name" },
		// KB-310 finding 5: the name grammar is one definition across the
		// go/js/shell/schema copies — the previously-divergent inputs
		// (leading digit, dot) must reject here too.
		"leading digit tree": func(i *WorkflowInterface) { i.Workflow = "0valid_name" },
		"dotted tree":        func(i *WorkflowInterface) { i.Workflow = "valid.dotted" },
		"bad adapter":        func(i *WorkflowInterface) { i.Adapters = []string{"sh ll"} },
		"dup adapter":        func(i *WorkflowInterface) { i.Adapters = []string{"shell", "shell"} },
		"relative path":      func(i *WorkflowInterface) { i.Mounts[0].MountPath = "workspace" },
		// KB-310: byte-exact mount paths — interface_validate.mjs rejects
		// a trailing slash and the routes validator must agree with it.
		"trailing slash": func(i *WorkflowInterface) { i.Mounts[0].MountPath = "/home/criteria/workspace/" },
		// KB-310 finding 5: single-segment "/a" has no parent container
		// path — `^/.+[^/]$` demands two segments, so both must reject.
		"single-segment path": func(i *WorkflowInterface) { i.Mounts[0].MountPath = "/a" },
		"bad kind":            func(i *WorkflowInterface) { i.Mounts[0].Kind = "hostPath" },
		"dup mount":           func(i *WorkflowInterface) { i.Mounts = append(i.Mounts, i.Mounts[0]) },
		"lower env":           func(i *WorkflowInterface) { i.SecretEnv = []string{"workflow_token"} },
		"dup env":             func(i *WorkflowInterface) { i.Env = []string{"A", "A"} },
		"dup secret":          func(i *WorkflowInterface) { i.SecretEnv = []string{"A", "A"} },
		"bad slot":            func(i *WorkflowInterface) { i.ConfigSlots[0].Slot = "CiGateCmd" },
		"dup slot": func(i *WorkflowInterface) {
			i.ConfigSlots = append(i.ConfigSlots, i.ConfigSlots[0])
		},
		"bad slot var":   func(i *WorkflowInterface) { i.ConfigSlots[0].Variable = "1x" },
		"missing target": func(i *WorkflowInterface) { i.ConfigSlots[0].Target = "" },
		// KB-310: the platform slot surface is closed — only the config
		// keys a project binding can carry through the KB-103 render, each
		// with its canonical delivery variable.
		"unknown slot key": func(i *WorkflowInterface) {
			i.ConfigSlots[0] = WorkflowInterfaceConfigSlot{Slot: "deployCmd", Variable: "deploy_cmd", Target: "deploy"}
		},
		"non-canonical slot var": func(i *WorkflowInterface) {
			i.ConfigSlots[0].Variable = "gate_cmd"
		},
	} {
		iface := valid()
		mutate(iface)
		if err := validateInterface(iface); err == nil {
			t.Errorf("validateInterface(%s) succeeded, want rejection", label)
		}
	}
	if err := validateInterface(valid()); err != nil {
		t.Errorf("validateInterface(valid) failed: %v", err)
	}
}

// CRI-242: the workflow-library object carries the admission queue class.
// Empty defaults (dev at stamping) and both declared values validate; any
// other value fails closed like tagMatch.
func TestValidateWorkflowClass(t *testing.T) {
	for _, class := range []string{"", ClassDev, ClassTriage} {
		p := validPayload()
		wf := p.WorkflowLibrary["wf-default"]
		wf.Class = class
		p.WorkflowLibrary["wf-default"] = wf
		if err := p.Validate(); err != nil {
			t.Errorf("Validate(class=%q) failed: %v", class, err)
		}
	}

	p := validPayload()
	wf := p.WorkflowLibrary["wf-default"]
	wf.Class = "concurrent"
	p.WorkflowLibrary["wf-default"] = wf
	err := p.Validate()
	if err == nil {
		t.Fatal("Validate succeeded for an unknown class, want error")
	}
	if !strings.Contains(err.Error(), "class must be") {
		t.Fatalf("err = %v, want substring %q", err, "class must be")
	}
}

// CRI-242: Parse preserves the class declared on a workflow-library object
// and Resolve carries it through, so the watcher stamps what the routes
// payload declared (the empty class stays empty here; the dev default is
// applied at stamping, not at parse).
func TestParseAndResolveCarryWorkflowClass(t *testing.T) {
	raw := []byte(`{"apiVersion":"criteria.brokenbots.dev/v1","kind":"Routes","workflowLibrary":{"wf-triage":{"type":"image","image":"i","namespace":"criteria-jobs","class":"triage"}},"routes":[{"name":"intake","workflow":"wf-triage","project":"Runner"}]}`)
	p, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if got := p.WorkflowLibrary["wf-triage"].Class; got != ClassTriage {
		t.Fatalf("parsed class = %q, want %q", got, ClassTriage)
	}
	sel, err := p.Resolve(selectorFor("Runner", "Triage", nil, nil))
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if sel.Workflow.Class != ClassTriage {
		t.Fatalf("resolved class = %q, want %q", sel.Workflow.Class, ClassTriage)
	}
}

// CRI-214 M14: the workflow-library object carries the per-adapter-kind
// image override. An omitted or populated map validates; bad adapter kinds,
// empty values, and whitespace in values fail closed.
func TestValidateAdapterImages(t *testing.T) {
	for _, images := range []map[string]string{
		nil,
		{},
		{"shell": "localhost:5000/criteria-adapter-shell:k8s-0.5.4-2"},
		{"shell": "localhost:5000/criteria-adapter-shell:k8s-0.5.4-2", "copilot": "localhost:5000/criteria-adapter-copilot:k8s-0.5.8"},
		{"intake": "ghcr.io/acme/adapter-intake@sha256:0000000000000000000000000000000000000000000000000000000000000000"},
	} {
		p := validPayload()
		wf := p.WorkflowLibrary["wf-default"]
		wf.AdapterImages = images
		p.WorkflowLibrary["wf-default"] = wf
		if err := p.Validate(); err != nil {
			t.Errorf("Validate(adapterImages=%v) failed: %v", images, err)
		}
	}

	cases := []struct {
		name    string
		images  map[string]string
		wantSub string
	}{
		{"uppercase kind", map[string]string{"Shell": "i"}, "invalid adapter kind"},
		{"kind with underscore", map[string]string{"bad_kind": "i"}, "invalid adapter kind"},
		{"empty value", map[string]string{"shell": ""}, "without whitespace"},
		{"whitespace in value", map[string]string{"shell": "reg.io/i:tag extra"}, "without whitespace"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := validPayload()
			wf := p.WorkflowLibrary["wf-default"]
			wf.AdapterImages = tc.images
			p.WorkflowLibrary["wf-default"] = wf
			err := p.Validate()
			if err == nil {
				t.Fatalf("Validate succeeded, want error containing %q", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("err = %v, want substring %q", err, tc.wantSub)
			}
		})
	}
}

// CRI-214 M14: Parse preserves the adapterImages block declared on a
// workflow-library object and Resolve carries it through, so the watcher
// stamps the per-kind overrides the routes payload declared.
func TestParseAndResolveCarryAdapterImages(t *testing.T) {
	raw := []byte(`{"apiVersion":"criteria.brokenbots.dev/v1","kind":"Routes","workflowLibrary":{"wf-default":{"type":"image","image":"i","namespace":"criteria-jobs","adapterImages":{"shell":"localhost:5000/criteria-adapter-shell:k8s-0.5.4-2"}}},"routes":[{"name":"intake","workflow":"wf-default","project":"Runner"}]}`)
	p, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	want := "localhost:5000/criteria-adapter-shell:k8s-0.5.4-2"
	if got := p.WorkflowLibrary["wf-default"].AdapterImages["shell"]; got != want {
		t.Fatalf("parsed adapterImages.shell = %q, want %q", got, want)
	}
	sel, err := p.Resolve(selectorFor("Runner", "Triage", nil, nil))
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if got := sel.Workflow.AdapterImages["shell"]; got != want {
		t.Fatalf("resolved adapterImages.shell = %q, want %q", got, want)
	}
}

// validConfigPayload is a config-bearing variant of the standard fixture:
// the intake route binds configRef "repo-a", and the configLibrary holds
// "repo-a" and "repo-b" (the latter omitting testCmd so per-key
// precedence is observable).
func validConfigPayload() *Payload {
	p := validPayload()
	p.ConfigLibrary = map[string]ConfigEntry{
		"repo-a": {BuildCmd: "make build-a", TestCmd: "make test-a", CIGateCmd: "make ci-a"},
		"repo-b": {BuildCmd: "make build-b"},
	}
	routes := p.Routes
	routes[0].ConfigRef = "repo-a"
	p.Routes = routes
	return p
}

func TestResolveRouteConfigRef(t *testing.T) {
	p := validConfigPayload()
	// Label also present and in the configs group: the route's configRef
	// deliberately outranks it.
	sel, err := p.Resolve(selectorFor("Runner", "Triage", []string{"repo-b"}, map[string]string{"repo-b": "configs"}))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if sel.ConfigName != "repo-a" {
		t.Errorf("ConfigName = %q, want route configRef %q", sel.ConfigName, "repo-a")
	}
	if sel.Config.BuildCmd != "make build-a" {
		t.Errorf("Config.BuildCmd = %q, want %q", sel.Config.BuildCmd, "make build-a")
	}
}

func TestResolveConfigLabelOverride(t *testing.T) {
	p := validConfigPayload()
	// Same route shape but without the configRef binding: the configs
	// group label picks the entry.
	p.Routes[0].ConfigRef = ""
	sel := selectorFor("Runner", "Triage", []string{"repo-b"}, map[string]string{"repo-b": "configs"})
	sel.ConfigsLabelGroup = "configs"
	got, err := p.Resolve(sel)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.ConfigName != "repo-b" {
		t.Errorf("ConfigName = %q, want label override %q", got.ConfigName, "repo-b")
	}
	if got.Config.BuildCmd != "make build-b" {
		t.Errorf("Config.BuildCmd = %q, want %q", got.Config.BuildCmd, "make build-b")
	}
}

func TestResolveNoConfigLeavesSelectionEmpty(t *testing.T) {
	// Plain fixture: no configLibrary at all.
	sel, err := validPayload().Resolve(selectorFor("Runner", "Triage", nil, nil))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// Empty-selection check: ConfigEntry carries a Tags slice (KB-154), so
	// the zero value is not comparable with ==.
	if sel.ConfigName != "" || !reflect.DeepEqual(sel.Config, ConfigEntry{}) {
		t.Errorf("no config selected: got ConfigName %q, Config %+v", sel.ConfigName, sel.Config)
	}
}

func TestResolveConfigsGroupsUnsetIgnoresConfigLabels(t *testing.T) {
	p := validConfigPayload()
	p.Routes[0].ConfigRef = ""
	// The label is in group "configs" but the selector's ConfigsLabelGroup
	// is unset: the selection carries no config.
	sel, err := p.Resolve(selectorFor("Runner", "Triage", []string{"repo-b"}, map[string]string{"repo-b": "configs"}))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if sel.ConfigName != "" {
		t.Errorf("ConfigName = %q, want empty (configs group unset)", sel.ConfigName)
	}
}

func TestResolveUnknownConfigFailsClosed(t *testing.T) {
	p := validConfigPayload()
	p.Routes[0].ConfigRef = ""
	sel := selectorFor("Runner", "Triage", []string{"missing-config"}, map[string]string{"missing-config": "configs"})
	sel.ConfigsLabelGroup = "configs"
	if _, err := p.Resolve(sel); !errors.Is(err, ErrUnknownConfig) {
		t.Fatalf("err = %v, want ErrUnknownConfig", err)
	}
}

func TestResolveAmbiguousConfigsFailsClosed(t *testing.T) {
	p := validConfigPayload()
	p.Routes[0].ConfigRef = ""
	sel := selectorFor("Runner", "Triage", []string{"repo-a", "repo-b"}, map[string]string{"repo-a": "configs", "repo-b": "configs"})
	sel.ConfigsLabelGroup = "configs"
	if _, err := p.Resolve(sel); !errors.Is(err, ErrAmbiguousConfig) {
		t.Fatalf("err = %v, want ErrAmbiguousConfig", err)
	}
}

func TestValidateRejectsDanglingRouteConfigRef(t *testing.T) {
	p := validPayload()
	routes := p.Routes
	routes[0].ConfigRef = "repo-a" // no such entry
	p.Routes = routes
	if _, err := Parse(marshalPayload(t, p)); err == nil {
		t.Fatal("Parse accepted a route configRef naming a missing configLibrary entry")
	}
}

func TestValidateRejectsBadConfigEntry(t *testing.T) {
	cases := []struct {
		name  string
		entry ConfigEntry
	}{
		{"blank buildCmd", ConfigEntry{BuildCmd: "   "}},
		{"blank ciGateCmd", ConfigEntry{CIGateCmd: "\t"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := validPayload()
			p.ConfigLibrary = map[string]ConfigEntry{"repo-a": tc.entry}
			if _, err := Parse(marshalPayload(t, p)); err == nil {
				t.Fatalf("Parse accepted config entry %+v", tc.entry)
			}
		})
	}
}

func TestValidatePositiveConfigEntry(t *testing.T) {
	p := validPayload()
	p.ConfigLibrary = map[string]ConfigEntry{
		"repo-a": {BuildCmd: "make build", CIGateCmd: "make ci"},
	}
	if _, err := Parse(marshalPayload(t, p)); err != nil {
		t.Fatalf("Parse rejected a valid config entry: %v", err)
	}
}

// taggedConfigPayload is a KB-154 tag-group config variant of the KB-103
// fixture: the intake route binds the plain repo config "repo-a";
// "repo-a-v060" carries the v0.6.0 tag group and the release branch, and
// "repo-b" is a second repo's config for cross-repo overlay tests. The
// tagged entry names its branch only — the release-branch slice the
// overlay exists to select. Overlay-dependent tests strip the route
// binding to exercise the fallback path.
func taggedConfigPayload() *Payload {
	p := validConfigPayload()
	p.ConfigLibrary["repo-a-v060"] = ConfigEntry{
		BuildCmd:   "make build-a-v060",
		TestCmd:    "make test-a-v060",
		CIGateCmd:  "make ci-a-v060",
		BaseBranch: "v0.6.0-release",
		Tags:       []string{"v0.6.0"},
	}
	return p
}

// unboundConfigPayload strips the intake route's configRef so the
// configs-group overlay is what selects the entry.
func unboundConfigPayload() *Payload {
	p := taggedConfigPayload()
	routes := p.Routes
	routes[0].ConfigRef = ""
	p.Routes = routes
	return p
}

func TestResolveConfigTagGroupOverlay(t *testing.T) {
	p := unboundConfigPayload()
	// The ticket's configs-group v0.6.0 label satisfies the tagged entry's
	// tag subset but names no entry: the overlay resolves it by tags.
	sel, err := p.Resolve(configsSelector("v0.6.0", "Runner", "Triage"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if sel.ConfigName != "repo-a-v060" {
		t.Fatalf("ConfigName = %q, want tag-group entry %q", sel.ConfigName, "repo-a-v060")
	}
	entry := p.ConfigLibrary["repo-a-v060"]
	if sel.Config.BaseBranch != "v0.6.0-release" || sel.Config.BuildCmd != entry.BuildCmd {
		t.Fatalf("Config = %+v, want the repo-a-v060 entry", sel.Config)
	}
}

func TestResolveTaggedConfigEntryIsOverlayOnly(t *testing.T) {
	// The route binding (repo-a) outranks the tag-group overlay: the
	// ticket's v0.6.0 label does not redirect a configRef-bound route.
	// The tag-group entry is intended to bind through a tagged route; a
	// route-bound ticket carrying the label keeps the route's config.
	p := taggedConfigPayload()
	sel, err := p.Resolve(configsSelector("v0.6.0", "Runner", "Triage"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if sel.Route.ConfigRef != "repo-a" || sel.ConfigName != "repo-a" {
		t.Fatalf("route binding outranked by overlay: ConfigName = %q, want %q", sel.ConfigName, "repo-a")
	}
}

func TestResolveUntaggedTicketKeepsRepoConfig(t *testing.T) {
	// Without the v0.6.0 tag the plain repo config applies (KB-103).
	p := taggedConfigPayload()
	sel, err := p.Resolve(selectorFor("Runner", "Triage", nil, nil))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if sel.ConfigName != "repo-a" {
		t.Fatalf("ConfigName = %q, want %q", sel.ConfigName, "repo-a")
	}
}

func TestResolveConfigTagGroupAmbiguousFailsClosed(t *testing.T) {
	// Two entries declare the same tag group: map iteration order must
	// never decide the selection, so it fails closed instead.
	p := unboundConfigPayload()
	p.ConfigLibrary["repo-b-v060"] = ConfigEntry{BuildCmd: "make build-b-v060", Tags: []string{"v0.6.0"}}
	sel := selectorFor("Runner", "Triage", []string{"v0.6.0"}, map[string]string{"v0.6.0": "configs"})
	sel.ConfigsLabelGroup = "configs"
	if _, err := p.Resolve(sel); !errors.Is(err, ErrAmbiguousConfig) {
		t.Fatalf("err = %v, want ErrAmbiguousConfig", err)
	}
}

func TestResolveConfigTagGroupUnknownFailsClosed(t *testing.T) {
	// A configs-group label matching no entry name and no tag subset
	// fails closed (the decided KB-154 failure mode): a config that does
	// not exist is a wiring error worth surfacing, not a silent
	// mis-branching.
	p := unboundConfigPayload()
	delete(p.ConfigLibrary, "repo-a-v060")
	sel := selectorFor("Runner", "Triage", []string{"v0.6.0"}, map[string]string{"v0.6.0": "configs"})
	sel.ConfigsLabelGroup = "configs"
	if _, err := p.Resolve(sel); !errors.Is(err, ErrUnknownConfig) {
		t.Fatalf("err = %v, want ErrUnknownConfig", err)
	}
}

func TestResolveConfigTagGroupNoEntryTagsStaysNameOnly(t *testing.T) {
	// A tag-less entry (plain KB-103 repo config) is name-selectable
	// only: the label "repo-a-v060" (an entry NAME in the configs group)
	// selects it, while "v0.6.0" against a tag-less payload fails closed.
	p := unboundConfigPayload()
	p.ConfigLibrary["repo-a-v060"] = ConfigEntry{BuildCmd: "make build-a-v060"} // tag stripped
	sel := selectorFor("Runner", "Triage", []string{"v0.6.0"}, map[string]string{"v0.6.0": "configs"})
	sel.ConfigsLabelGroup = "configs"
	if _, err := p.Resolve(sel); !errors.Is(err, ErrUnknownConfig) {
		t.Fatalf("err = %v, want ErrUnknownConfig for a tag with no tag-group entry", err)
	}
}

func TestResolveConfigTagGroupAnyMatch(t *testing.T) {
	// tagMatch=any: the tag is among the declared tags, and the other
	// tagged entry (v0.6.0 under all-semantics) is not satisfied by
	// "beta".
	p := unboundConfigPayload()
	p.ConfigLibrary["repo-a-beta"] = ConfigEntry{BuildCmd: "make build-beta", Tags: []string{"beta", "v0.6.0"}, TagMatch: TagMatchAny}
	sel, err := p.Resolve(configsSelector("beta", "Runner", "Triage"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if sel.ConfigName != "repo-a-beta" {
		t.Fatalf("ConfigName = %q, want %q", sel.ConfigName, "repo-a-beta")
	}
}

func TestResolveConfigTagGroupMultiTagAllNotSatisfiedBySingleLabel(t *testing.T) {
	// tagMatch=all with two declared tags: a single configs-group label
	// cannot satisfy the subset, so the label fails closed rather than
	// half-selecting the entry.
	p := unboundConfigPayload()
	entry := p.ConfigLibrary["repo-a-v060"]
	entry.TagMatch = TagMatchAll
	entry.Tags = []string{"v0.6.0", "urgent"}
	p.ConfigLibrary["repo-a-v060"] = entry
	sel := selectorFor("Runner", "Triage", []string{"v0.6.0"}, map[string]string{"v0.6.0": "configs"})
	sel.ConfigsLabelGroup = "configs"
	if _, err := p.Resolve(sel); !errors.Is(err, ErrUnknownConfig) {
		t.Fatalf("err = %v, want ErrUnknownConfig", err)
	}
}

func TestConfigsTagGroupMember(t *testing.T) {
	p := taggedConfigPayload()
	cases := []struct {
		tag  string
		want bool
	}{
		{"repo-a-v060", true}, // names an entry (KB-103)
		{"repo-a", true},      // names the untagged entry
		{"v0.6.0", true},      // single-tag all-subset match
		{"repo-b", true},      // names an entry
		{"beta", false},       // declared only under all-semantics with v0.6.0
		{"nonsense", false},   // neither name nor tag
		{"v0.7.0", false},     // no entry declares it
	}
	for _, tc := range cases {
		if got := p.ConfigsTagGroupMember(tc.tag); got != tc.want {
			t.Errorf("ConfigsTagGroupMember(%q) = %v, want %v", tc.tag, got, tc.want)
		}
	}
	// An ambiguous tag subset is a member: the resolver fails closed on
	// it when a route-without-configRef relies on the overlay.
	p.ConfigLibrary["repo-b-v060"] = ConfigEntry{BuildCmd: "make build-b-v060", Tags: []string{"v0.6.0"}}
	if !p.ConfigsTagGroupMember("v0.6.0") {
		t.Errorf("ConfigsTagGroupMember(v0.6.0) = false with an ambiguous subset, want true (ambiguous membership fails closed at selection)")
	}
}

func TestValidateRejectsBadTagGroupEntry(t *testing.T) {
	cases := []struct {
		name  string
		entry ConfigEntry
	}{
		{"whitespace baseBranch", ConfigEntry{BaseBranch: "v0.6.0 release"}},
		{"blank baseBranch", ConfigEntry{BaseBranch: "   "}},
		{"bad tagMatch", ConfigEntry{Tags: []string{"v0.6.0"}, TagMatch: "some"}},
		{"empty tag name", ConfigEntry{Tags: []string{""}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := validPayload()
			p.ConfigLibrary = map[string]ConfigEntry{"repo-a": tc.entry}
			if _, err := Parse(marshalPayload(t, p)); err == nil {
				t.Fatalf("Parse accepted tag-group config entry %+v", tc.entry)
			}
		})
	}
}

func TestValidatePositiveTagGroupEntry(t *testing.T) {
	p := validPayload()
	p.ConfigLibrary = map[string]ConfigEntry{
		"repo-a-v060": {
			BuildCmd:   "make build",
			BaseBranch: "v0.6.0-release",
			Tags:       []string{"v0.6.0", "urgent"},
			TagMatch:   TagMatchAny,
		},
	}
	if _, err := Parse(marshalPayload(t, p)); err != nil {
		t.Fatalf("Parse rejected a valid tag-group config entry: %v", err)
	}
}

// KB-311 (ADR 307): the project-registry entry shape — closed vocabulary,
// DNS-1123 namespace, advisory owner/name repo members — with the absent
// marker staying legal (pre-cutover projects keep the global
// workflow-library behavior: binding-first, deletion second).
func TestValidateProjectRegistry(t *testing.T) {
	entry := func() ProjectEntry {
		return ProjectEntry{Namespace: "criteria-go-dev", Repos: []string{"brokenbots/criteria-go-dev"}}
	}
	p := validPayload()
	p.ProjectRegistry = map[string]ProjectEntry{
		// The payload's own routes ("Runner", "Other") must stay resolvable
		// through the registry — the registry is the routing table.
		"Runner":          {Namespace: "criteria-jobs"},
		"Other":           {Namespace: "other-jobs"},
		"Criteria Go Dev": entry(),
		"TypeScript SDK": {
			Namespace: "typescript-sdk",
			Repos:     []string{"brokenbots/typescript-sdk"},
			Cutover:   CutoverDone,
		},
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("Validate(registry) failed: %v", err)
	}
	if !p.ProjectRegistry["TypeScript SDK"].IsCutoverDone() {
		t.Errorf("Project(entry with cutover=done).IsCutoverDone() = false, want true")
	}
	if p.ProjectRegistry["Criteria Go Dev"].IsCutoverDone() {
		t.Errorf("Project(entry without marker).IsCutoverDone() = true, want false")
	}
	if _, ok := p.Project("Unknown Project"); ok {
		t.Errorf("Project(unknown) = ok, want miss")
	}

	for label, mutate := range map[string]func(map[string]ProjectEntry){
		"blank project name": func(reg map[string]ProjectEntry) {
			delete(reg, "Criteria Go Dev")
			reg["   "] = entry()
		},
		"bad namespace": func(reg map[string]ProjectEntry) {
			reg["Bad Namespace"] = ProjectEntry{Namespace: "Bad_NS"}
		},
		"missing namespace": func(reg map[string]ProjectEntry) {
			reg["No Namespace"] = ProjectEntry{Repos: []string{"o/n"}}
		},
		"bad repo": func(reg map[string]ProjectEntry) {
			reg["Bad Repo"] = ProjectEntry{Namespace: "ns", Repos: []string{"missing-slash"}}
		},
		"unknown cutover": func(reg map[string]ProjectEntry) {
			reg["Half Done"] = ProjectEntry{Namespace: "ns", Cutover: "half done"}
		},
	} {
		bad := validPayload()
		mutated := map[string]ProjectEntry{"Demo Project": entry()}
		if bad.ProjectRegistry != nil {
			t.Fatalf("unexpected preset registry")
		}
		mutate(mutated)
		bad.ProjectRegistry = mutated
		err := bad.Validate()
		if err == nil {
			t.Errorf("Validate(%s) accepted the registry entry", label)
			continue
		}
		if !strings.Contains(err.Error(), "projectRegistry") {
			t.Errorf("Validate(%s) error %v does not name projectRegistry", label, err)
		}
	}
}

// KB-311: when the payload carries a registry, every route's project must
// resolve through it — the registry is the ONLY ticket->project routing
// table. A payload without a registry keeps the pre-KB-311 behavior
// (route projects unbound by the registry).
func TestValidateRouteResolvesThroughProjectRegistry(t *testing.T) {
	unregistered := validPayload()
	err := unregistered.Validate()
	if err != nil {
		t.Fatalf("Validate(pre-KB-311 payload without registry) failed: %v", err)
	}

	withRegistry := validPayload()
	withRegistry.ProjectRegistry = map[string]ProjectEntry{
		"Runner": {Namespace: "criteria-jobs"},
	}
	err = withRegistry.Validate()
	if err == nil || !strings.Contains(err.Error(), "projectRegistry") {
		t.Errorf("Validate(route project not in registry) = %v, want routing-table rejection", err)
	}

	covered := validPayload()
	covered.ProjectRegistry = map[string]ProjectEntry{
		"Runner": {Namespace: "criteria-jobs"},
		"Other":  {Namespace: "other-jobs"},
	}
	if err := covered.Validate(); err != nil {
		t.Fatalf("Validate(routes registered) failed: %v", err)
	}
	entry, ok := covered.Project("Runner")
	if !ok || entry.Namespace != "criteria-jobs" {
		t.Errorf("Project(Runner) = (%+v, %v), want the registry entries", entry, ok)
	}
}

// KB-311: the shipped routes example carries the registry both states
// demonstrate — pre-cutover projects (global workflow-library binding data)
// and cutover:done projects (namespace-bound criteria-binding execution);
// the acceptance projects resolve with distinct namespaces.
func TestShippedExampleProjectRegistry(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "k8s", "examples", "routes-configmap.yaml"))
	if err != nil {
		t.Fatalf("reading shipped example: %v", err)
	}
	p, err := Parse([]byte(extractRoutesJSON(t, string(data))))
	if err != nil {
		t.Fatalf("Parse(shipped example) failed: %v", err)
	}
	if len(p.ProjectRegistry) == 0 {
		t.Fatal("shipped example carries no projectRegistry (KB-311)")
	}
	for project, wantNS := range map[string]string{
		"Criteria Go Dev": "criteria-go-dev",
		"TypeScript SDK":  "typescript-sdk",
	} {
		entry, ok := p.Project(project)
		if !ok {
			t.Fatalf("registry missing the %q acceptance project", project)
		}
		if entry.Namespace != wantNS {
			t.Errorf("%s namespace = %q, want %q", project, entry.Namespace, wantNS)
		}
		if !entry.IsCutoverDone() {
			t.Errorf("%s is not cutover:done (the acceptance demonstrates binding-first execution)", project)
		}
	}
	// The intake's pre-cutover route: those projects resolve but keep the
	// global workflow-library behavior.
	for _, project := range []string{"Criteria K8s Workflow Runner", "Kanboard Tickets"} {
		entry, ok := p.Project(project)
		if !ok {
			t.Fatalf("registry missing %q", project)
		}
		if entry.IsCutoverDone() {
			t.Errorf("%s reports cutover done (pre-cutover while the global CM still carries binding data)", project)
		}
	}
}

// KB-313 (ADR 307): namespace is optional on a workflow-library entry —
// cutover-done entries route into their project's registry namespace and a
// namespace-less entry's runs resolve the run's own namespace. A present
// value must still be a DNS-1123 label.
func TestNamespaceOptionalWorkflowEntry(t *testing.T) {
	noNS := validPayload()
	wf := noNS.WorkflowLibrary["wf-default"]
	wf.Namespace = ""
	noNS.WorkflowLibrary["wf-default"] = wf
	if err := noNS.Validate(); err != nil {
		t.Fatalf("Validate(namespace-less entry) failed: %v", err)
	}
	var wfJSON struct {
		Namespace string `json:"namespace"`
	}
	raw, err := json.Marshal(noNS.WorkflowLibrary["wf-default"])
	if err != nil {
		t.Fatalf("marshaling namespace-less workflow: %v", err)
	}
	if err := json.Unmarshal(raw, &wfJSON); err != nil {
		t.Fatalf("unmarshaling namespace-less workflow: %v", err)
	}
	if wfJSON.Namespace != "" {
		t.Errorf("namespace-less entry marshals %q, want an omitted key", wfJSON.Namespace)
	}

	bad := validPayload()
	badWf := bad.WorkflowLibrary["wf-default"]
	badWf.Namespace = "Bad_NS"
	bad.WorkflowLibrary["wf-default"] = badWf
	err = bad.Validate()
	if err == nil || !strings.Contains(err.Error(), "DNS-1123") {
		t.Errorf("Validate(bad namespace label) = %v, want a DNS-1123 rejection", err)
	}
}

// KB-313 (ADR 307): the migration window is allow-with-warning. A payload
// with no registry (pre-KB-311) or with any non-cutover-done registry
// entry validates while its workflowLibrary carries the global run-config
// surfaces, and DeprecationWarnings lists exactly the entries still
// carrying them.
func TestDeprecatedGlobalSurfacesAllowWithWarning(t *testing.T) {
	warnList := func(p *Payload) []string {
		t.Helper()
		if err := p.Validate(); err != nil {
			t.Fatalf("Validate(migration window with stale surfaces) failed: %v", err)
		}
		return p.DeprecationWarnings()
	}
	assertWarnings := func(p *Payload, wantEntry string) {
		t.Helper()
		warnings := warnList(p)
		if len(warnings) != 1 {
			t.Fatalf("DeprecationWarnings() = %v, want one warning for %q", warnings, wantEntry)
		}
		if !strings.Contains(warnings[0], wantEntry) {
			t.Errorf("warning %q does not name %q", warnings[0], wantEntry)
		}
		if !strings.Contains(warnings[0], "volumes, secrets, env, adapterImages") {
			t.Errorf("warning %q does not list the stale surface keys", warnings[0])
		}
	}
	stalePayload := func() *Payload {
		p := validPayload()
		wf := p.WorkflowLibrary["wf-default"]
		wf.Volumes = []Volume{{Name: "scratch", Kind: "tmp", MountPath: "/scratch"}}
		wf.Secrets = []Secret{{Name: "gh", SecretProviderClass: "spc", MountPath: "/secrets"}}
		wf.Env = map[string]string{"CRITERIA_RUN_MODE": "k8s"}
		wf.AdapterImages = map[string]string{"shell": "reg.dev/shell:1"}
		p.WorkflowLibrary["wf-default"] = wf
		return p
	}

	// No registry: the pre-KB-311 payload shape stays in the warning window.
	assertWarnings(stalePayload(), "wf-default")

	// A registry with any non-cutover-done entry keeps the window open.
	mixed := stalePayload()
	mixed.ProjectRegistry = map[string]ProjectEntry{
		"Runner":          {Namespace: "criteria-jobs"},
		"Other":           {Namespace: "other-jobs"},
		"Criteria Go Dev": {Namespace: "criteria-go-dev", Cutover: CutoverDone},
		"TypeScript SDK":  {Namespace: "typescript-sdk", Cutover: CutoverDone},
	}
	assertWarnings(mixed, "wf-default")
}

// KB-313 (ADR 307): the post-cutover gate. Once EVERY projectRegistry
// entry is cutover-done, the payload hard-rejects any workflowLibrary
// entry still carrying volumes/secrets/env/adapterImages — the global CM
// owns zero project-shaped data in the end state — while a clean
// post-cutover payload validates and carries no warnings.
func TestPostCutoverRejectsGlobalRunConfigSurfaces(t *testing.T) {
	doneRegistry := func() map[string]ProjectEntry {
		return map[string]ProjectEntry{
			"Runner":          {Namespace: "criteria-jobs", Cutover: CutoverDone},
			"Other":           {Namespace: "other-jobs", Cutover: CutoverDone},
			"Criteria Go Dev": {Namespace: "criteria-go-dev", Cutover: CutoverDone},
		}
	}
	stale := validPayload()
	wf := stale.WorkflowLibrary["wf-default"]
	wf.Env = map[string]string{"CRITERIA_RUN_MODE": "k8s"}
	wf.AdapterImages = map[string]string{"shell": "reg.dev/shell:1"}
	stale.WorkflowLibrary["wf-default"] = wf
	stale.ProjectRegistry = doneRegistry()
	err := stale.Validate()
	if err == nil {
		t.Fatal("Validate(post-cutover payload with stale surfaces) accepted the payload")
	}
	for _, want := range []string{"post-cutover", "KB-313", "workflowLibrary", "env, adapterImages"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("rejection %v does not mention %q", err, want)
		}
	}
	// The warning lane is moot post-cutover: Validate rejects, so no valid
	// post-cutover payload ever carries a warning.
	if warnings := stale.DeprecationWarnings(); warnings != nil {
		t.Errorf("DeprecationWarnings(post-cutover) = %v, want nil", warnings)
	}

	clean := validPayload()
	clean.ProjectRegistry = doneRegistry()
	if err := clean.Validate(); err != nil {
		t.Fatalf("Validate(clean post-cutover payload) failed: %v", err)
	}
	if warnings := clean.DeprecationWarnings(); warnings != nil {
		t.Errorf("DeprecationWarnings(clean) = %v, want nil", warnings)
	}
}

// KB-313 acceptance: two cut-over projects and three un-cut projects
// coexist through the migration window — the un-cut side keeps the global
// library stamping (warned), the cut-over side serves binding references,
// and the payload validates as one unit while any un-cut entry exists.
func TestMigrationWindowCoexistence(t *testing.T) {
	p := validPayload()
	wf := p.WorkflowLibrary["wf-default"]
	wf.Env = map[string]string{"CRITERIA_RUN_MODE": "k8s"}
	wf.Volumes = []Volume{{Name: "scratch", Kind: "tmp", MountPath: "/scratch"}}
	p.WorkflowLibrary["wf-default"] = wf
	p.ProjectRegistry = map[string]ProjectEntry{
		// Two cut-over projects: binding-first execution.
		"Criteria Go Dev": {Namespace: "criteria-go-dev", Repos: []string{"brokenbots/criteria-go-dev"}, Cutover: CutoverDone},
		"TypeScript SDK":  {Namespace: "typescript-sdk", Repos: []string{"brokenbots/typescript-sdk"}, Cutover: CutoverDone},
		// Three un-cut projects: the global library keeps serving them,
		// warned, until each cuts over.
		"Runner":           {Namespace: "criteria-jobs"},
		"Other":            {Namespace: "other-jobs"},
		"Kanboard Tickets": {Namespace: "criteria-jobs", Repos: []string{"brokenbots/workflow-example"}},
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("Validate(two cut-over + three un-cut coexistence) failed: %v", err)
	}
	warnings := p.DeprecationWarnings()
	if len(warnings) != 1 || !strings.Contains(warnings[0], "wf-default") {
		t.Errorf("DeprecationWarnings() = %v, want the single stale entry named", warnings)
	}
	if p.PostCutover() {
		t.Error("PostCutover() = true during the migration window, want false")
	}
}
