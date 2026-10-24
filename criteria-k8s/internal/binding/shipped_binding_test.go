package binding

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// kb312RepoPath resolves a repo-root-relative path from the package source
// location, so the test works from the module root or under -race wrappers.
func kb312RepoPath(t *testing.T, rel string) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed: cannot locate the package source tree")
	}
	// Walk up from criteria-k8s/internal/binding to the repo root (the dir
	// that carries the k8s/ tree) — the module is a subdirectory of it.
	dir := filepath.Dir(thisFile)
	for dir != filepath.Dir(dir) {
		if info, err := os.Stat(filepath.Join(dir, "k8s", "examples")); err == nil && info.IsDir() {
			return filepath.Join(dir, rel)
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("cannot locate the repo root (no k8s/ examples dir above the package)")
	return ""
}

// TestShippedGoBindingChecksumMatchesRenderedCM (KB-312 acceptance leg): the
// go domain's binding payload — the file k8s/project-lifecycle.sh renders the
// project binding CM from — must parse/validate through the operator's
// fail-closed gate, and its canonical JSON checksum (the operator's own
// algorithm, stamped into status.launchManifest.bindingChecksum at
// admission) must match what the RENDERED project binding ConfigMap in
// k8s/projects/criteria-go-dev.yaml carries. A drift between the example the
// template splices and the committed render would mean a run could be
// admitted against payload the render does not serve.
func TestShippedGoBindingChecksumMatchesRenderedCM(t *testing.T) {
	examplePath := kb312RepoPath(t, filepath.Join("k8s", "examples", "binding-criteria-go-dev.yaml"))
	renderPath := kb312RepoPath(t, filepath.Join("k8s", "projects", "criteria-go-dev.yaml"))

	payload := shippedBindingPayload(t, examplePath, "k8s/examples/binding-criteria-go-dev.yaml")
	parsed, err := Parse([]byte(payload))
	if err != nil {
		t.Fatalf("shipped go binding payload fails the operator's gate: %v", err)
	}
	assertKB312PerNSShape(t, parsed)

	exampleChecksum := parsed.Checksum()

	renderPayload := shippedBindingPayload(t, renderPath, "k8s/projects/criteria-go-dev.yaml")

	rendered, err := Parse([]byte(renderPayload))
	if err != nil {
		t.Fatalf("rendered binding CM payload fails the operator's gate: %v", err)
	}
	renderChecksum := rendered.Checksum()
	if exampleChecksum != renderChecksum {
		t.Fatalf("binding checksum drift: example hashes %s, rendered project CM hashes %s", exampleChecksum, renderChecksum)
	}

	// The render header stamps the SAME checksum it serves (the drift guard
	// the shell test double-checks with the jq mirror).
	stamp := shippedRenderChecksumStamp(t, renderPath)
	if stamp != renderChecksum {
		t.Fatalf("rendered target state stamps checksum %s but its binding payload hashes %s", stamp, renderChecksum)
	}

	// The checksum form must be plain sha256 hex (json.Unmarshal-safe).
	if len(renderChecksum) != 64 {
		t.Fatalf("binding checksum %q is not 64-char sha256 hex", renderChecksum)
	}
}

// assertKB312PerNSShape pins the card's per-namespace PVC shape: the go
// binding resolves the project namespace's criteria-data/criteria-repo
// claims (NOT project-suffixed claim names).
func assertKB312PerNSShape(t *testing.T, p Payload) {
	t.Helper()
	claims := map[string]bool{}
	for _, v := range p.Volumes {
		if v.Kind == "pvc" {
			claims[v.Claim] = true
		}
	}
	if !claims["criteria-data"] {
		t.Fatalf("go binding's data volume does not resolve the per-ns criteria-data claim; got %v", claims)
	}
	if !claims["criteria-repo"] {
		t.Fatalf("go binding's repo volume does not resolve the per-ns criteria-repo claim; got %v", claims)
	}
	for _, v := range p.Volumes {
		if strings.HasPrefix(v.Claim, "criteria-go-dev-") {
			t.Fatalf("go binding carries project-suffixed claim %q; cutover unifies on per-ns names", v.Claim)
		}
	}
}

// shippedBindingPayload extracts the binding.json data key from a shipped
// manifest (ConfigMap whose data key holds the JSON payload).
func shippedBindingPayload(t *testing.T, path, label string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", label, err)
	}
	// Project target-state files are multi-doc; the binding CM is the doc
	// carrying the binding.json data key. Payload lines are JSON and never a
	// bare '---', so a textual split is safe for the manifests we ship.
	for _, doc := range strings.Split(string(raw), "\n---\n") {
		if doc == "" || strings.HasPrefix(strings.TrimSpace(doc), "#") && !strings.Contains(doc, "apiVersion:") {
			continue
		}
		var cm struct {
			Data map[string]string `yaml:"data"`
		}
		if err := yaml.Unmarshal([]byte(doc), &cm); err != nil {
			continue
		}
		if payload, ok := cm.Data[BindingKey]; ok {
			return payload
		}
	}
	t.Fatalf("%s carries no %q data key", label, BindingKey)
	return ""
}

func shippedRenderChecksumStamp(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading k8s/projects/criteria-go-dev.yaml: %v", err)
	}
	// The header comment carries "Binding checksum (canonical JSON, operator
	// algorithm): <hex>"; a missing stamp means the render drifted from the
	// template (the shell test's rendered-vs-committed leg catches that too).
	const marker = "Binding checksum"
	for _, line := range strings.Split(string(raw), "\n") {
		if idx := strings.Index(line, marker); idx >= 0 {
			fields := strings.Fields(line[idx+len(marker):])
			if len(fields) > 0 {
				return fields[len(fields)-1]
			}
		}
	}
	t.Fatal("k8s/projects/criteria-go-dev.yaml carries no checksum stamp; the render template changed")
	return ""
}
