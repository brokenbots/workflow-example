package v1

import (
	"testing"
)

// KB-310 reviewer finding: RunWorkflow.DeepCopyInto must deep-copy the
// interface — a shallow struct copy aliases the adapters/mounts/slots
// slices, so mutating a copy would corrupt the stamped launch contract.
func TestRunWorkflowDeepCopyDoesNotAliasInterface(t *testing.T) {
	src := &RunWorkflow{
		Interface: &RunWorkflowInterface{
			Adapters: []string{"shell", "copilot"},
			Mounts: []RunWorkflowInterfaceMount{
				{Name: "workspace", MountPath: "/home/criteria/workspace", Kind: "pvc"},
				{Name: "scratch", MountPath: "/tmp/scratch", Kind: "tmp"},
			},
			SecretEnv: []string{"WORKFLOW_GITHUB_TOKEN"},
			Env:       []string{"CRITERIA_RUN_MODE"},
			ConfigSlots: []RunWorkflowInterfaceConfigSlot{
				{Slot: "ciGateCmd", Variable: "ci_gate_cmd", Target: "qa_gate"},
			},
		},
		AdapterImages: map[string]string{"shell": "localhost:5000/shell:dev"},
	}

	out := src.DeepCopy()
	if out == src || out.Interface == src.Interface {
		t.Fatal("DeepCopy must allocate a new interface object")
	}

	// Mutate every aliased slice/map on the copy; the source must not move.
	out.Interface.Adapters[0] = "mutated"
	out.Interface.Mounts[0] = RunWorkflowInterfaceMount{Name: "mutated"}
	out.Interface.ConfigSlots[0].Slot = "mutated"
	out.Interface.SecretEnv[0] = "MUTATED"
	out.Interface.Env[0] = "MUTATED"
	out.AdapterImages["shell"] = "mutated"

	if src.Interface.Adapters[0] != "shell" {
		t.Errorf("source adapters aliased: %q", src.Interface.Adapters[0])
	}
	if (src.Interface.Mounts[0] != RunWorkflowInterfaceMount{Name: "workspace", MountPath: "/home/criteria/workspace", Kind: "pvc"}) {
		t.Errorf("source mounts aliased: %+v", src.Interface.Mounts[0])
	}
	if src.Interface.ConfigSlots[0].Slot != "ciGateCmd" {
		t.Errorf("source config slots aliased: %+v", src.Interface.ConfigSlots[0])
	}
	if src.Interface.SecretEnv[0] != "WORKFLOW_GITHUB_TOKEN" || src.Interface.Env[0] != "CRITERIA_RUN_MODE" {
		t.Errorf("source env lanes aliased: %q / %q", src.Interface.SecretEnv[0], src.Interface.Env[0])
	}
	if src.AdapterImages["shell"] != "localhost:5000/shell:dev" {
		t.Errorf("source adapterImages aliased: %q", src.AdapterImages["shell"])
	}
}

// A nil interface stays nil through the copy.
func TestRunWorkflowDeepCopyNilInterface(t *testing.T) {
	src := &RunWorkflow{}
	out := src.DeepCopy()
	if out.Interface != nil {
		t.Fatalf("nil interface copied as %+v, want nil", out.Interface)
	}
}
