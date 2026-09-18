package routes

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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
	if len(p.WorkflowLibrary) != 2 {
		t.Errorf("workflowLibrary size = %d, want 2", len(p.WorkflowLibrary))
	}
	if len(p.Routes) != 1 || p.Routes[0].Project != "Criteria K8s Workflow Runner" {
		t.Errorf("routes = %+v, want one route for project %q", p.Routes, "Criteria K8s Workflow Runner")
	}
	wf, ok := p.WorkflowLibrary["linear-intake-v1"]
	if !ok {
		t.Fatalf("workflowLibrary missing linear-intake-v1")
	}
	if wf.Type != TypeImage || wf.Image == "" || len(wf.Volumes) != 2 || len(wf.Secrets) != 2 {
		t.Errorf("linear-intake-v1 = %+v, want image workflow with 2 volumes and 2 secrets", wf)
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
			func(p *Payload) { p.WorkflowLibrary["wf-default"] = Workflow{Type: TypeImage, Image: "i", Namespace: "n", Ref: "abc"} },
			"url or ref",
		},
		{
			"url workflow without url",
			func(p *Payload) { p.WorkflowLibrary["wf-default"] = Workflow{Type: TypeURL, Namespace: "n"} },
			"requires a url",
		},
		{
			"bad workflow type",
			func(p *Payload) { p.WorkflowLibrary["wf-default"] = Workflow{Type: "tarball", Image: "i", Namespace: "n"} },
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