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
		t.Errorf("workflowLibrary size = %d, want 9", len(p.WorkflowLibrary))
	}
	if len(p.Routes) != 7 {
		t.Errorf("routes size = %d, want 7", len(p.Routes))
	}
	// KB-281: the retired criteria-triage / criteria-develop routes are gone.
	// Surviving route order: the criteria project's cleanup-only route, then
	// the two kanboard routes.
	if n := p.Routes[0].Name; n != "criteria-cleanup" {
		t.Errorf("routes[0] = %q, want %q (criteria project: cleanup only)", n, "criteria-cleanup")
	}
	if p.Routes[0].Project != "Criteria K8s Workflow Runner" {
		t.Errorf("cleanup route project = %q, want %q", p.Routes[0].Project, "Criteria K8s Workflow Runner")
	}
	if p.Routes[1].Project != "Kanboard Tickets" || p.Routes[2].Project != "Kanboard Tickets" {
		t.Errorf("routes[1:3] projects = %q/%q, want the kanboard pair on %q", p.Routes[1].Project, p.Routes[2].Project, "Kanboard Tickets")
	}
	// KB-277: harness-contracts, then the KB-278 composition leg and the
	// KB-279 failures leg — the criteria project's dave-go tag-gated harness
	// series legs (each firing its own *-url object).
	if n := p.Routes[3].Name; n != "harness-contracts" {
		t.Errorf("routes[3] = %q, want %q (KB-277 harness contracts route)", n, "harness-contracts")
	}
	if p.Routes[3].Project != "Criteria K8s Workflow Runner" {
		t.Errorf("harness-contracts route project = %q, want %q", p.Routes[3].Project, "Criteria K8s Workflow Runner")
	}
	if p.Routes[3].Workflow != "harness-contracts-url" {
		t.Errorf("harness-contracts route workflow = %q, want %q", p.Routes[3].Workflow, "harness-contracts-url")
	}
	if _, ok := p.WorkflowLibrary["harness-contracts-url"]; !ok {
		t.Errorf("workflowLibrary missing the harness-contracts-url object (KB-277)")
	}
	// KB-278: the harness-composition leg rides routes[4].
	if n := p.Routes[4].Name; n != "harness-composition" {
		t.Errorf("routes[4] = %q, want %q (KB-278 harness composition route)", n, "harness-composition")
	}
	if p.Routes[4].Workflow != "harness-composition-url" {
		t.Errorf("harness-composition route workflow = %q, want %q", p.Routes[4].Workflow, "harness-composition-url")
	}
	// KB-278: the object must stay url-ONLY — no commit ref/refKey may bind
	// the pinless-at-close harness series (CRI-260 three-way check).
	comp, ok := p.WorkflowLibrary["harness-composition-url"]
	if !ok {
		t.Fatalf("workflowLibrary missing harness-composition-url (KB-278)")
	}
	if comp.Type != TypeURL || comp.Class != ClassTriage || comp.Namespace != "criteria-jobs" ||
		comp.Ref != "" || len(comp.Volumes) != 3 || len(comp.Secrets) != 1 {
		t.Errorf("harness-composition-url = %+v, want triage-class criteria-jobs url workflow with 3 volumes, 1 secret, no pin (KB-278)", comp)
	}
	// KB-279: the harness-failures leg rides routes[5] — the failures leg
	// fires harness_failures_v1 (engineered failure-path fixtures, matrix
	// E24-E32 + G12) with the same pinless-at-close convention.
	if n := p.Routes[5].Name; n != "harness-failures" {
		t.Errorf("routes[5] = %q, want %q (KB-279 harness failures route)", n, "harness-failures")
	}
	if p.Routes[5].Project != "Criteria K8s Workflow Runner" {
		t.Errorf("harness-failures route project = %q, want %q", p.Routes[5].Project, "Criteria K8s Workflow Runner")
	}
	if p.Routes[5].Workflow != "harness-failures-url" {
		t.Errorf("harness-failures route workflow = %q, want %q", p.Routes[5].Workflow, "harness-failures-url")
	}
	fail, ok := p.WorkflowLibrary["harness-failures-url"]
	if !ok {
		t.Fatalf("workflowLibrary missing harness-failures-url (KB-279)")
	}
	if fail.Type != TypeURL || fail.Class != ClassTriage || fail.Namespace != "criteria-jobs" ||
		fail.Ref != "" || len(fail.Volumes) != 3 || len(fail.Secrets) != 1 {
		t.Errorf("harness-failures-url = %+v, want triage-class criteria-jobs url workflow with 3 volumes, 1 secret, no pin (KB-279)", fail)
	}
	if len(p.Routes[5].Tags) == 0 || p.Routes[5].Tags[0] != "harness-validation" {
		t.Errorf("harness-failures route tags = %v, want the tag-gated harness pair (KB-279)", p.Routes[5].Tags)
	}
	// KB-280: the harness-fleet-smoke leg rides routes[6] — the series'
	// fast-feedback canary (harness_fleet_smoke_v1, ~90s deterministic
	// no-agent pre-leg gate) with the same pinless-at-close convention.
	if n := p.Routes[6].Name; n != "harness-fleet-smoke" {
		t.Errorf("routes[6] = %q, want %q (KB-280 harness fleet-smoke route)", n, "harness-fleet-smoke")
	}
	if p.Routes[6].Project != "Criteria K8s Workflow Runner" {
		t.Errorf("harness-fleet-smoke route project = %q, want %q", p.Routes[6].Project, "Criteria K8s Workflow Runner")
	}
	if p.Routes[6].Workflow != "harness-fleet-smoke-url" {
		t.Errorf("harness-fleet-smoke route workflow = %q, want %q", p.Routes[6].Workflow, "harness-fleet-smoke-url")
	}
	fsmoke, ok := p.WorkflowLibrary["harness-fleet-smoke-url"]
	if !ok {
		t.Fatalf("workflowLibrary missing harness-fleet-smoke-url (KB-280)")
	}
	if fsmoke.Type != TypeURL || fsmoke.Class != ClassTriage || fsmoke.Namespace != "criteria-jobs" ||
		fsmoke.Ref != "" || len(fsmoke.Volumes) != 3 || len(fsmoke.Secrets) != 1 {
		t.Errorf("harness-fleet-smoke-url = %+v, want triage-class criteria-jobs url workflow with 3 volumes, 1 secret, no pin (KB-280)", fsmoke)
	}
	// KB-280 is the no-agent slice: the object pins the Shell adapter ONLY
	// (no copilot identity to image — the canary declares no copilot member).
	if fsmoke.AdapterImages == nil {
		t.Errorf("harness-fleet-smoke-url adapterImages missing — want the shell-only surface (KB-280)")
	} else if _, hasCopilot := fsmoke.AdapterImages["copilot"]; hasCopilot {
		t.Errorf("harness-fleet-smoke-url adapterImages pins copilot — the canary is the no-agent slice (KB-280)")
	} else if fsmoke.AdapterImages["shell"] != "localhost:5000/criteria-adapter-shell-peer:k8s-0.5.51-1" {
		t.Errorf("harness-fleet-smoke-url shell pin = %q, want the recorded fleet pin set (KB-280)", fsmoke.AdapterImages["shell"])
	}
	if len(p.Routes[6].Tags) == 0 || p.Routes[6].Tags[0] != "harness-validation" {
		t.Errorf("harness-fleet-smoke route tags = %v, want the tag-gated harness pair (KB-280)", p.Routes[6].Tags)
	}
	// Absence regressions (KB-281): the retired linear url objects must not
	// return in the shipped example.
	for _, gone := range []string{"linear-intake-url", "linear-triage-url", "linear-develop-url"} {
		if _, still := p.WorkflowLibrary[gone]; still {
			t.Errorf("workflowLibrary still has the retired object %q (KB-281)", gone)
		}
	}
	for _, r := range p.Routes {
		if r.Workflow == "linear-intake-url" || r.Workflow == "linear-triage-url" || r.Workflow == "linear-develop-url" {
			t.Errorf("route %q still fires the retired linear workflow %q (KB-281)", r.Name, r.Workflow)
		}
	}
	// The retained linear-intake-v1 image object coexists with the live
	// kanboard objects: no route assigns it by default; the override
	// resolution is exercised in TestShippedExampleResolvesKanboardRoutes.
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
	wf, ok := p.WorkflowLibrary["linear-intake-v1"]
	if !ok {
		t.Fatalf("workflowLibrary missing linear-intake-v1")
	}
	if wf.Type != TypeImage || wf.Image == "" || len(wf.Volumes) != 2 || len(wf.Secrets) != 2 {
		t.Errorf("linear-intake-v1 = %+v, want image workflow with 2 volumes and 2 secrets", wf)
	}
}

// KB-281: the shipped example's live routing is the kanboard pair. A ticket
// in "Backlog" on the Kanboard Tickets project resolves to the
// kanboard-triage-url object (kanboard_triage_v1, url-only with the ADR-0005
// D7 commit pin, triage class), a ticket in "Ready" resolves to the
// kanboard-develop-url object (kanboard_develop_v1, pinned likewise), the
// retired criteria-project routes resolve errNoRoute, and the
// watcher-facing TicketStates union shrinks to kanboard + cleanup states.
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

	// KB-281 absence regressions: the retireed criteria-project routes must
	// fail closed (the watcher's routes fail closed on unmapped shapes).
	for _, tc := range []struct{ name, state string }{
		{"Triage state (criteria-triage retired)", "Triage"},
		{"Backlog state (criteria routes never covered it)", "Backlog"},
		{"Ready without the dirty label (develop route retired)", "Ready"},
	} {
		if _, err := p.Resolve(selectorFor("Criteria K8s Workflow Runner", tc.state, []string{"k8s-run"}, nil)); !errors.Is(err, ErrNoRoute) {
			t.Errorf("Resolve(Criteria project, %s) err = %v, want ErrNoRoute (%s)", tc.state, err, tc.name)
		}
	}

	// The retained linear-intake-v1 image object is override-reachable (via
	// the workflows label group) but no route assigns it by default: the same
	// Ready-for-Development resolution with the object override selects the
	// image workflow, the plain one above fails closed.
	overridden, err := p.Resolve(selectorFor("Criteria K8s Workflow Runner", "Ready for Development", []string{"k8s-run", "criteria-dirty", "linear-intake-v1"},
		map[string]string{"linear-intake-v1": "workflows"}))
	if err != nil {
		t.Fatalf("Resolve(Ready for Development, linear-intake-v1 override): %v", err)
	}
	if overridden.Name != "linear-intake-v1" || overridden.Workflow.Type != TypeImage {
		t.Errorf("override resolution = %q/%s, want the linear-intake-v1 image object", overridden.Name, overridden.Workflow.Type)
	}

	// The union of declared states is what the kanboard watcher queries for
	// (the criteria-cleanup route carries Ready for Development; the linear
	// watcher filters per-project at Resolve time).
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
	if castle.BuildCmd != "make build-castle" {
		t.Errorf("castle buildCmd = %q, want make build-castle (%q: the build-parapet npm leg stays GitHub-CI-only)", castle.BuildCmd, castle.BuildCmd)
	}
	if castle.TestCmd != "make test" {
		t.Errorf("castle testCmd = %q, want make test", castle.TestCmd)
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

// CRI-310 (plan CRI-214 M10.4, carrier for CRI-246, validation run C): the
// dirty label is a ROUTING SURFACE. A ticket carrying the watcher's sticky
// "criteria-dirty" label in "Ready for Development" resolves to the
// criteria-cleanup route and the ticket-cleanup-url object — (KB-281: the
// tag-less general develop route the cleanup route used to out-prioritize is
// retired, so without the dirty label nothing matches and the watcher fails
// closed; specific-over-general priority still governs multi-candidate
// shapes). The object ships url-only, on the triage (read-only) admission
// class, pinned (this repository squash merges — every shipped pin names a
// main-landed squash commit set in a follow-up change, CRI-241/CRI-243
// convention) and credential-free beyond the Linear API key (the cleanup
// pass does zero git operations).
func TestShippedExampleResolvesDirtyCleanupRoute(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "k8s", "examples", "routes-configmap.yaml"))
	if err != nil {
		t.Fatalf("reading shipped example: %v", err)
	}
	p, err := Parse([]byte(extractRoutesJSON(t, string(data))))
	if err != nil {
		t.Fatalf("Parse(shipped example) failed: %v", err)
	}

	// The dirty-labeled ticket routes to the cleanup workflow — this is the
	// validation run C path.
	sel, err := p.Resolve(selectorFor("Criteria K8s Workflow Runner", "Ready for Development", []string{"criteria-dirty"}, nil))
	if err != nil {
		t.Fatalf("Resolve(Ready for Development, criteria-dirty): %v", err)
	}
	if sel.Route.Name != "criteria-cleanup" {
		t.Errorf("dirty ticket matched route = %q, want %q (the tag-based specific route must beat the tag-less develop route)", sel.Route.Name, "criteria-cleanup")
	}
	if sel.Name != "ticket-cleanup-url" {
		t.Errorf("dirty ticket resolved workflow = %q, want %q", sel.Name, "ticket-cleanup-url")
	}
	wf := sel.Workflow
	if wf.Type != TypeURL {
		t.Errorf("cleanup workflow type = %q, want %q (url-only, minimal criteria base runtime)", wf.Type, TypeURL)
	}
	// Validation run C corrected two assumptions from the original landing:
	// the pinless fetch fails to resolve the subtree, so the object pins the
	// squash-merge commit a84ef55; and the base entrypoint's repo-clone init
	// unconditionally requires WORKFLOW_GITHUB_TOKEN, so the object binds the
	// github-tokens CSI surface (the cleanup pass itself performs zero git
	// operations — the mount exists for the entrypoint, not the workflow).
	// linear-api-key rides at /home/criteria/linear-secrets: the runner
	// script's hardcoded file: OriginRef path (lstat-checked before any step).
	if !strings.HasPrefix(wf.URL, "git::https://github.com/brokenbots/workflow-example.git//ticket_cleanup_v1") {
		t.Errorf("cleanup workflow url = %q, want the ticket_cleanup_v1 subtree", wf.URL)
	}
	if wf.Ref != "0b8136650d5622381bf18a597ed93b8c272ba19b" {
		t.Errorf("cleanup workflow ref = %q, want the ticket_cleanup_v1 squash-merge commit 0b813665 (pinless HEAD resolution fails on the subtree; CRI-241/CRI-243 follow-up re-pin convention)", wf.Ref)
	}
	if wf.Image != "" {
		t.Errorf("cleanup workflow image = %q, want empty (url-only must not declare a process image)", wf.Image)
	}
	if wf.Class != ClassTriage {
		t.Errorf("cleanup workflow class = %q, want %q (CRI-242 read-only admission)", wf.Class, ClassTriage)
	}
	if len(wf.Volumes) != 3 || len(wf.Secrets) != 2 {
		t.Errorf("ticket-cleanup-url = %+v, want 3 volumes and 2 secrets (linear-api-key + github-tokens: the entrypoint requires WORKFLOW_GITHUB_TOKEN)", wf)
	}
	for _, s := range wf.Secrets {
		if s.Name == "linear-api-key" && s.MountPath != "/home/criteria/linear-secrets" {
			t.Errorf("ticket-cleanup-url linear-api-key mountPath = %q, want /home/criteria/linear-secrets (the runner's file: OriginRef path)", s.MountPath)
		}
	}

	// KB-281: the tag-less general develop route is retired, so a ticket
	// WITHOUT the dirty label fails closed (nothing matches).
	if _, err := p.Resolve(selectorFor("Criteria K8s Workflow Runner", "Ready for Development", []string{"k8s-run"}, nil)); !errors.Is(err, ErrNoRoute) {
		t.Errorf("non-dirty ticket err = %v, want ErrNoRoute (the general develop route is retired)", err)
	}

	// The state gate still applies: a dirty ticket in "Backlog" (never a
	// cleanup-route state) fails closed.
	if _, err := p.Resolve(selectorFor("Criteria K8s Workflow Runner", "Backlog", []string{"criteria-dirty"}, nil)); !errors.Is(err, ErrNoRoute) {
		t.Errorf("dirty Backlog ticket err = %v, want ErrNoRoute (the cleanup route is declared for Ready for Development only)", err)
	}

	// KB-103's configRef lived on the retired criteria-develop route; no
	// surviving route binds a config entry by default.
	for _, r := range p.Routes {
		if r.Name == "criteria-develop" {
			t.Fatalf("the criteria-develop route returned after KB-281")
		}
	}

	// The cleanup route declares no new linear states, so CRI-310 held the
	// union at the linear estate's states; KB-281 retired those routes and
	// the union shrank to kanboard + cleanup states.
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
