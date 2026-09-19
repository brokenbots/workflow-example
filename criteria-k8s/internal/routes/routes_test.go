package routes

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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
	if len(p.WorkflowLibrary) != 3 {
		t.Errorf("workflowLibrary size = %d, want 3", len(p.WorkflowLibrary))
	}
	if len(p.Routes) != 2 {
		t.Errorf("routes size = %d, want 2", len(p.Routes))
	}
	if p.Routes[0].Project != "Criteria K8s Workflow Runner" || p.Routes[1].Project != "Criteria K8s Workflow Runner" {
		t.Errorf("routes = %+v, want both routed for project %q", p.Routes, "Criteria K8s Workflow Runner")
	}
	wf, ok := p.WorkflowLibrary["linear-intake-v1"]
	if !ok {
		t.Fatalf("workflowLibrary missing linear-intake-v1")
	}
	if wf.Type != TypeImage || wf.Image == "" || len(wf.Volumes) != 2 || len(wf.Secrets) != 2 {
		t.Errorf("linear-intake-v1 = %+v, want image workflow with 2 volumes and 2 secrets", wf)
	}
}

// CRI-241: the shipped example carries the dev route — a ticket in the
// "Ready for Development" workflow state resolves to the linear-develop-url
// library object (linear_develop_v1, url-only with the ADR-0005 D7 commit
// pin), and the watcher-facing TicketStates union grows to include the new
// state alongside the intake route's [Triage].
func TestShippedExampleResolvesDevRoute(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "k8s", "examples", "routes-configmap.yaml"))
	if err != nil {
		t.Fatalf("reading shipped example: %v", err)
	}
	p, err := Parse([]byte(extractRoutesJSON(t, string(data))))
	if err != nil {
		t.Fatalf("Parse(shipped example) failed: %v", err)
	}

	sel, err := p.Resolve(selectorFor("Criteria K8s Workflow Runner", "Ready for Development", []string{"k8s-run"}, nil))
	if err != nil {
		t.Fatalf("Resolve(Ready for Development): %v", err)
	}
	if sel.Route.Name != "criteria-develop" {
		t.Errorf("matched route = %q, want %q", sel.Route.Name, "criteria-develop")
	}
	if sel.Name != "linear-develop-url" {
		t.Errorf("resolved workflow = %q, want %q", sel.Name, "linear-develop-url")
	}
	wf := sel.Workflow
	if wf.Type != TypeURL {
		t.Errorf("workflow type = %q, want %q (url-only, minimal criteria base runtime)", wf.Type, TypeURL)
	}
	if wf.URL != "git::https://github.com/brokenbots/workflow-example.git//linear_develop_v1" {
		t.Errorf("workflow url = %q, want the linear_develop_v1 subtree", wf.URL)
	}
	if wf.Ref != "7645feb42e6f2c473696bd63997fca111d41453d" {
		t.Errorf("workflow ref = %q, want the merged develop tree commit (ADR-0005 D7 pin)", wf.Ref)
	}
	if wf.Image != "" {
		t.Errorf("workflow image = %q, want empty (url-only must not declare a process image)", wf.Image)
	}
	if len(wf.Volumes) != 4 || len(wf.Secrets) != 2 {
		t.Errorf("linear-develop-url = %+v, want 4 volumes and 2 secrets like linear-intake-url", wf)
	}

	// The union of declared states is what the watcher queries Linear for;
	// the dev route must grow it beyond [Triage].
	want := []string{"Ready for Development", "Triage"}
	if got := p.TicketStates(); !slices.Equal(got, want) {
		t.Errorf("TicketStates() = %v, want %v", got, want)
	}

	// The intake behavior is unchanged: Triage still resolves to the baked
	// intake image workflow.
	intake, err := p.Resolve(selectorFor("Criteria K8s Workflow Runner", "Triage", nil, nil))
	if err != nil {
		t.Fatalf("Resolve(Triage): %v", err)
	}
	if intake.Route.Name != "criteria-intake" || intake.Name != "linear-intake-v1" {
		t.Errorf("Triage resolves to %q/%q, want criteria-intake/linear-intake-v1", intake.Route.Name, intake.Name)
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
