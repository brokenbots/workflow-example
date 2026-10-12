package controller

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/brokenbots/workflow-example/criteria-k8s/internal/events"
)

// graphAdapterDef is one compiled adapter binding consumed for pod
// composition (KB-253): the definition identity plus the environment it is
// bound to and its compile-time pin.
type graphAdapterDef struct {
	Type         string `json:"type"`
	Name         string `json:"name"`
	Environment  string `json:"environment"`
	Source       string `json:"source"`
	PinnedDigest string `json:"pinned_digest"`
}

// envGraphDefs parses the run's compiled-workflow-graphs payload (the
// castle-stream WorkflowGraphs event: the "root" layer plus every compiled
// subworkflow layer) and returns the merged, deduplicated definition set per
// environment identity ("<type>.<name>"). This is the pod-composition
// source of record: the compiled graph is the only interface — a
// definition bound to an environment in ANY layer (root included) belongs
// to that environment's peer manifest, so the peer boots hosting every
// definition its environment will ever dial, at pod creation.
//
// Bodies without bindings (older engines) contribute nothing; the caller
// then keeps the provision-derived behavior, which is the pre-KB-253 shape.
func envGraphDefs(graphsRaw []byte) (map[string][]graphAdapterDef, error) {
	if len(graphsRaw) == 0 {
		return nil, nil
	}
	var payload struct {
		Subworkflows []struct {
			Name string `json:"name"`
			Body string `json:"body"`
		} `json:"subworkflows"`
	}
	if err := json.Unmarshal(graphsRaw, &payload); err != nil {
		return nil, fmt.Errorf("parse workflowGraphs payload: %w", err)
	}
	out := map[string][]graphAdapterDef{}
	seen := map[string]struct{}{}
	for _, layer := range payload.Subworkflows {
		var body struct {
			Adapters []graphAdapterDef `json:"adapters"`
		}
		if err := json.Unmarshal([]byte(layer.Body), &body); err != nil {
			return nil, fmt.Errorf("parse layer %q body: %w", layer.Name, err)
		}
		for _, ad := range body.Adapters {
			if ad.Type == "" || ad.Name == "" || ad.Environment == "" {
				continue
			}
			// The graph publishes bindings as "<env_type>.<env_name>"; the
			// reconcile groups by the CRI-234 identity "<type>/<name>".
			envType, envName, _ := strings.Cut(ad.Environment, ".")
			envKey := envType + "/" + envName
			key := ad.Type + "." + ad.Name + "@" + envKey
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			out[envKey] = append(out[envKey], graphAdapterDef{
				Type:         ad.Type,
				Name:         ad.Name,
				Environment:  ad.Environment,
				Source:       ad.Source,
				PinnedDigest: ad.PinnedDigest,
			})
		}
	}
	for env := range out {
		sort.Slice(out[env], func(i, j int) bool {
			a, b := out[env][i], out[env][j]
			if a.Environment != b.Environment {
				return a.Environment < b.Environment
			}
			if a.Type != b.Type {
				return a.Type < b.Type
			}
			return a.Name < b.Name
		})
	}
	return out, nil
}

// graphMember returns the builder-facing member for one graph definition:
// it fills the definition fields the pod builder reads (kind, definition
// name, digest) and zeroes every dial-identity field (no token, no scope,
// no shim address) — it contributes manifest shape, never a session.
func syntheticManifestMember(def graphAdapterDef) events.LifecycleEvent {
	ev := events.LifecycleEvent{
		Event:           "provision_wanted",
		AdapterType:     def.Type,
		AdapterName:     def.Name,
		Digest:          def.PinnedDigest,
		ImageReference:  def.Source,
		EnvironmentType: def.Type,
		EnvironmentName: strings.TrimPrefix(def.Environment, def.Type+"."),
	}
	ev.Environment = events.EnvironmentIdentity(ev.EnvironmentType, ev.EnvironmentName)
	return ev
}

// adapterKind returns the image/kind identity of a lifecycle member (the
// jobbuilder's rule, mirrored locally: the typed kind when stamped, the
// definition name for engines that predate adapter_type).
func localAdapterKind(m events.LifecycleEvent) string {
	if m.AdapterType != "" {
		return m.AdapterType
	}
	return m.AdapterName
}
