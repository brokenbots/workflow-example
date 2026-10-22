package binding

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/brokenbots/workflow-example/criteria-k8s/internal/routes"
)

// examplePayload mirrors the shipped k8s/examples/binding-criteria-go-dev.yaml
// binding.json shape: the acceptance pair uses THIS project's binding while
// typescript-sdk's binding differs only in images/env (same tree).
func examplePayload() string {
	return `{
		"apiVersion": "criteria.brokenbots.dev/v1",
		"kind": "CriteriaBinding",
		"images": {
			"peer-go": "us-docker.pkg.dev/brokenbots/workflow-example/peer-go:0.5.56"
		},
		"env": {
			"GOFLAGS": "-mod=mod",
			"GOPROXY": "https://proxy.golang.org"
		},
		"volumes": [
			{
				"name": "gopkg",
				"kind": "pvc",
				"mountPath": "/go/pkg",
				"claim": "criteria-go-dev-gopkg"
			},
			{
				"name": "scratch",
				"kind": "tmp",
				"mountPath": "/tmp/scratch",
				"sizeLimit": "8Gi"
			}
		],
		"secrets": [
			{
				"name": "kanboard",
				"secretProviderClass": "kanboard-spc",
				"mountPath": "/mnt/secrets/kanboard",
				"env": {
					"KANBOARD_URL": "kanboard_url",
					"KANBOARD_TOKEN": "kanboard_token"
				}
			}
		],
		"config": {
			"baseBranch": "main",
			"ciGateCmd": "go vet ./... && go test ./..."
		}
	}`
}

func mustParse(t *testing.T, data string) Payload {
	t.Helper()
	p, err := Parse([]byte(data))
	if err != nil {
		t.Fatalf("Parse(example payload) failed: %v", err)
	}
	return p
}

// TestParseExamplePayload covers the valid example: every binding surface
// survives parse+validate with the project-owned values intact.
func TestParseExamplePayload(t *testing.T) {
	p := mustParse(t, examplePayload())

	if p.APIVersion != APIVersion || p.Kind != Kind {
		t.Errorf("unmarshal mismatch: %s/%s", p.APIVersion, p.Kind)
	}
	if p.Images["peer-go"] == "" {
		t.Errorf("images missing the peer-go adapter override")
	}
	if p.Env["GOFLAGS"] == "" || p.Env["GOPROXY"] == "" {
		t.Errorf("env missing the project env vars: %v", p.Env)
	}
	if len(p.Volumes) != 2 || p.Volumes[0].Kind != "pvc" || p.Volumes[0].Claim != "criteria-go-dev-gopkg" {
		t.Errorf("volumes = %+v, want gopkg(pvc) + scratch(tmp)", p.Volumes)
	}
	if len(p.Secrets) != 1 || p.Secrets[0].SecretProviderClass != "kanboard-spc" {
		t.Errorf("secrets = %+v, want the kanboard SPC lane", p.Secrets)
	}
	if p.Secrets[0].Env["KANBOARD_TOKEN"] != "kanboard_token" {
		t.Errorf("secret env must map env name -> SPC key, got %v", p.Secrets[0].Env)
	}
	if p.Config["baseBranch"] != "main" {
		t.Errorf("config.baseBranch = %q, want main", p.Config["baseBranch"])
	}
}

func TestParseUnknownKeyFailsClosed(t *testing.T) {
	top := []string{
		"registry", "workflow", "workflows", "configLibrary", "workflowLibrary",
		"configs", "namespace", "project", "bindingRef", "image", "secretsOut",
	}
	for _, key := range top {
		var raw map[string]json.RawMessage
		if err := json.Unmarshal([]byte(examplePayload()), &raw); err != nil {
			t.Fatalf("example payload must stay valid JSON: %v", err)
		}
		raw[key] = json.RawMessage(`"injected"`)
		data, _ := json.Marshal(raw)
		if _, err := Parse(data); err == nil {
			t.Errorf("Parse accepted unknown top-level key %q — the binding carries NOTHING else", key)
		}
	}

	// Nested unknown fields reject too: volume payloads decode through the
	// routes Volume struct, secrets through routes.Secret.
	vol := `{"apiVersion":"criteria.brokenbots.dev/v1","kind":"CriteriaBinding","images":{"shell":"reg/img:1"},"env":{},"volumes":[{"name":"x","kind":"tmp","mountPath":"/x","nfs-server":"evil"}],"secrets":[],"config":{}}`
	if _, err := Parse([]byte(vol)); err == nil || !strings.Contains(err.Error(), "nfs-server") {
		t.Errorf("Parse accepted unknown volume field nfs-server: %v", err)
	}
	sec := `{"apiVersion":"criteria.brokenbots.dev/v1","kind":"CriteriaBinding","images":{"shell":"reg/img:1"},"env":{},"volumes":[],"secrets":[{"name":"s","secretProviderClass":"spc","mountPath":"/s","data":"{\"k\":\"v\"}"}],"config":{}}`
	if _, err := Parse([]byte(sec)); err == nil || !strings.Contains(err.Error(), "data") {
		t.Errorf("Parse accepted unknown secret field data: %v", err)
	}
}

// TestParseUnknownConfigSlot rejects any config key outside the KB-311
// slot surface: VALUES for the tree's declared config slots only.
func TestParseUnknownConfigSlot(t *testing.T) {
	for _, slot := range []string{"buildcmd", "BuildCmd", "gateCmd", "unknownSlot"} {
		data := strings.Replace(examplePayload(),
			`"baseBranch": "main"`, fmt.Sprintf(`"baseBranch": "main", %q: "main-2"`, slot), 1)
		if _, err := Parse([]byte(data)); err == nil {
			t.Errorf("config slot %q accepted; only buildCmd/testCmd/ciGateCmd/baseBranch ship", slot)
		}
	}
}

func TestParseValidationFailures(t *testing.T) {
	cases := map[string]string{
		"wrong apiVersion": strings.Replace(examplePayload(), "criteria.brokenbots.dev/v1", "criteria.brokenbots.dev/v2", 1),
		"wrong kind":       strings.Replace(examplePayload(), `"CriteriaBinding"`, `"Binding"`, 1),
		"empty value env":  strings.Replace(examplePayload(), `"GOFLAGS": "-mod=mod"`, `"GOFLAGS": ""`, 1),
		"bad env name":     strings.Replace(examplePayload(), `"GOFLAGS"`, `"GO FLAGS"`, 1),
		"blank image":      strings.Replace(examplePayload(), "peer-go:0.5.56", "peer-go:0.5.56 ", 1),
		"bad volume kind":  strings.Replace(examplePayload(), `"kind": "pvc"`, `"kind": "nfs"`, 1),
		"dup volume name": strings.Replace(examplePayload(),
			`"name": "scratch",`, `"name": "gopkg",`, 1),
		"secret without SPC": strings.Replace(examplePayload(),
			`"secretProviderClass": "kanboard-spc",`, "", 1),
		"config empty value": strings.Replace(examplePayload(),
			`"baseBranch": "main"`, `"baseBranch": ""`, 1),
	}
	for name, data := range cases {
		if _, err := Parse([]byte(data)); err == nil {
			t.Errorf("%s: Parse accepted an invalid binding", name)
		}
	}
}

// TestChecksumStableAndSensitive: the checksum is the launch manifest's
// binding digest — deterministic across parses, changed by any binding
// content edit, and stable regardless of JSON key order (canonical
// re-marshal).
func TestChecksumStableAndSensitive(t *testing.T) {
	p := mustParse(t, examplePayload())

	again := mustParse(t, examplePayload())
	if p.Checksum() != again.Checksum() {
		t.Errorf("checksum differs between parses of identical bytes")
	}

	reordered := `{
		"config": {"ciGateCmd": "go vet ./... && go test ./...", "baseBranch": "main"},
		"secrets": [{"env": {"KANBOARD_TOKEN": "kanboard_token", "KANBOARD_URL": "kanboard_url"},
			"mountPath": "/mnt/secrets/kanboard", "secretProviderClass": "kanboard-spc", "name": "kanboard"}],
		"volumes": [{"claim": "criteria-go-dev-gopkg", "mountPath": "/go/pkg", "kind": "pvc", "name": "gopkg"},
			{"sizeLimit": "8Gi", "mountPath": "/tmp/scratch", "kind": "tmp", "name": "scratch"}],
		"env": {"GOPROXY": "https://proxy.golang.org", "GOFLAGS": "-mod=mod"},
		"images": {"peer-go": "us-docker.pkg.dev/brokenbots/workflow-example/peer-go:0.5.56"},
		"kind": "CriteriaBinding",
		"apiVersion": "criteria.brokenbots.dev/v1"
	}`
	same, err := Parse([]byte(reordered))
	if err != nil {
		t.Fatalf("reordered payload must parse: %v", err)
	}
	if p.Checksum() != same.Checksum() {
		t.Errorf("checksum must be order-insensitive for maps (canonical re-marshal)")
	}

	nudged := strings.Replace(examplePayload(), "-mod=mod", "-mod=readonly", 1)
	other := mustParse(t, nudged)
	if p.Checksum() == other.Checksum() {
		t.Errorf("checksum must change when the binding content changes")
	}
}

// TestApplyMaterializesBinding: Apply replaces the workflow definition's
// binding-shaped fields with the project's own values and preserves the
// tree-shaped fields — the reuse-by-binding shape (same tree, different
// bindings).
func TestApplyMaterializesBinding(t *testing.T) {
	goBinding := mustParse(t, examplePayload())
	tsBinding := mustParse(t, strings.Replace(examplePayload(),
		`"peer-go": "us-docker.pkg.dev/brokenbots/workflow-example/peer-go:0.5.56"`,
		`"peer-node": "us-docker.pkg.dev/brokenbots/workflow-example/peer-node:0.5.56"`, 1))

	// Same tree on both projects: one workflowLibrary entry (KB-310 shape).
	tree := routes.Workflow{
		Type:    "url",
		URL:     "https://github.com/brokenbots/workflow-catalog",
		Ref:     "abc1234",
		Image:   "legacy-image:1",
		Volumes: []routes.Volume{{Name: "legacy", Kind: "tmp", MountPath: "/tmp"}},
		Env:     map[string]string{"STALE": "yes"},
	}
	goApplied := goBinding.Apply(tree)
	tsApplied := tsBinding.Apply(tree)

	if goApplied.URL != tree.URL || goApplied.Ref != "abc1234" || goApplied.Type != "url" {
		t.Errorf("Apply must preserve the tree references, got %+v", goApplied)
	}
	if goApplied.Image != "legacy-image:1" || goApplied.AdapterImages["peer-go"] == "" {
		t.Errorf("Apply must preserve the workflow entry's process image and replace adapter images with the binding's: %+v", goApplied)
	}
	if tsApplied.AdapterImages["peer-go"] != "" || tsApplied.AdapterImages["peer-node"] == "" {
		t.Errorf("typescript-sdk binding must not carry the go project's images: %+v", tsApplied.AdapterImages)
	}
	if goApplied.Env["GOPROXY"] == "" || tsApplied.Env["STALE"] != "" {
		t.Errorf("env must come wholly from the binding (no tree entry residue): %v / %v", goApplied.Env, tsApplied.Env)
	}
	if len(goApplied.Volumes) != 2 || len(tsApplied.Volumes) != 2 {
		t.Errorf("volumes must come wholly from the binding: %d/%d", len(goApplied.Volumes), len(tsApplied.Volumes))
	}
	if goApplied.Volumes[0].Claim != "criteria-go-dev-gopkg" {
		t.Errorf("claims are the project's OWN PVCs: %+v", goApplied.Volumes[0])
	}

	// Materialization must not alias the binding's maps/slices.
	goApplied.Env["GOFLAGS"] = "mutated"
	goBinding.Env["GOFLAGS"] = "original"
	fresh := goBinding.Apply(tree)
	if fresh.Env["GOFLAGS"] != "original" {
		t.Errorf("Apply must copy binding values, not alias them")
	}
}

// TestParseRoutesPayloadStillSeparate guards the split boundary: the
// binding package must not accidentally decode a ROUTES payload
// (workflowLibrary et al) as a binding — unknown key rejection makes that fail closed.
func TestParseRoutesPayloadStillSeparate(t *testing.T) {
	routesShaped := `{"apiVersion":"criteria.brokenbots.dev/v1","kind":"Payload","workflowLibrary":{},"configLibrary":{},"routes":{}}`
	if _, err := Parse([]byte(routesShaped)); err == nil {
		t.Errorf("a routes CM payload must not parse as a binding")
	}
}
