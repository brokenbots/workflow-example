package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// CriteriaRunPhase describes the lifecycle phase of a run.
type CriteriaRunPhase string

const (
	PhasePending   CriteriaRunPhase = "Pending"
	PhaseRunning   CriteriaRunPhase = "Running"
	PhaseSucceeded CriteriaRunPhase = "Succeeded"
	PhaseFailed    CriteriaRunPhase = "Failed"
	PhaseUnknown   CriteriaRunPhase = "Unknown"
)

// CriteriaRunSpec defines the desired state of a CriteriaRun.
type CriteriaRunSpec struct {
	// TicketID is the Linear ticket identifier (e.g. CRI-104).
	TicketID string `json:"ticketId"`

	// RepoURL is the GitHub repository to clone and work on (e.g. brokenbots/workflow-example).
	RepoURL string `json:"repoUrl"`

	// Image is the Criteria workflow image to run. Defaults to the operator-wide default.
	Image string `json:"image,omitempty"`

	// BuildCmd is an optional command used to build the repository under test.
	BuildCmd string `json:"buildCmd,omitempty"`

	// TestCmd is an optional command used to run the repository test suite.
	TestCmd string `json:"testCmd,omitempty"`

	// CIGateCmd is the command used as the implementation CI gate.
	CIGateCmd string `json:"ciGateCmd,omitempty"`

	// MaxAgentVisits limits how many times each agent gate may be visited.
	MaxAgentVisits int `json:"maxAgentVisits,omitempty"`

	// ProviderBaseURL is the Ollama-compatible provider endpoint for adapters.
	ProviderBaseURL string `json:"providerBaseUrl,omitempty"`

	// PerScopeSessions opts into per-subworkflow adapter pods instead of
	// run-duration adapter pods. When true, the operator creates adapter pods
	// at subworkflow scope entry and deletes them at scope exit based on the
	// engine's provision-wanted / release lifecycle events.
	PerScopeSessions bool `json:"perScopeSessions,omitempty"`

	// Workflow is the resolved workflow-library object stamped onto the run
	// by the linear-watcher (CRI-217) from the criteria-routes ConfigMap.
	// Nil means no workflow was assigned and the operator defaults apply.
	// Consumed by the jobbuilder (CRI-222); workflowSource for url modes
	// arrives with CRI-231.
	Workflow *RunWorkflow `json:"workflow,omitempty"`
}

// RunWorkflow is a workflow-library object resolved from the routes
// ConfigMap (k8s/routes.schema.json) and stamped onto a CriteriaRun spec.
type RunWorkflow struct {
	// Name is the workflow-library name routes resolved for this run.
	Name string `json:"name"`

	// Type is the workflow source type (ADR-0005 D1): "image" (workflow
	// baked into the image) or "url" (fetched at run admission).
	Type string `json:"type"`

	// Namespace is the namespace the workflow's runs are admitted into.
	Namespace string `json:"namespace"`

	// Image is the container image for type=image, or the process image for
	// url+image. Empty for url-only runs (minimal criteria/runtime base).
	Image string `json:"image,omitempty"`

	// URL is the workflow source URL fetched at admission (type=url).
	URL string `json:"url,omitempty"`

	// Ref is the operator-declared expected pin for fetched content,
	// enforced fail-closed at admission (ADR-0005 D7, criteria-side CRI-223).
	Ref string `json:"ref,omitempty"`

	// Volumes are storage volumes the workflow's runs mount.
	Volumes []RunWorkflowVolume `json:"volumes,omitempty"`

	// Secrets are secrets the workflow's runs consume, referenced by
	// SecretProviderClass name (OpenBao via the Secrets Store CSI driver).
	Secrets []RunWorkflowSecret `json:"secrets,omitempty"`

	// Env is the workflow's static environment mapping.
	Env map[string]string `json:"env,omitempty"`
}

// RunWorkflowVolume is a storage volume stamped from the routes ConfigMap.
type RunWorkflowVolume struct {
	Name      string            `json:"name"`
	Kind      string            `json:"kind"`
	MountPath string            `json:"mountPath"`
	SubPath   string            `json:"subPath,omitempty"`
	ReadOnly  bool              `json:"readOnly,omitempty"`
	Claim     string            `json:"claim,omitempty"`
	Server    string            `json:"server,omitempty"`
	Path      string            `json:"path,omitempty"`
	SizeLimit string            `json:"sizeLimit,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
}

// RunWorkflowSecret is a secret reference stamped from the routes ConfigMap.
// Only names are carried: credential material lives in OpenBao behind the
// SecretProviderClass (plan CRI-214 section 3.8).
type RunWorkflowSecret struct {
	Name                string            `json:"name"`
	SecretProviderClass string            `json:"secretProviderClass"`
	MountPath           string            `json:"mountPath"`
	Env                 map[string]string `json:"env,omitempty"`
}

// CriteriaRunQueueStatus exposes the repo-keyed admission queue state for this run.
type CriteriaRunQueueStatus struct {
	// RepoURL is the repository this queue is keyed on.
	RepoURL string `json:"repoUrl,omitempty"`

	// Position is this run's position in the pending queue. Zero means admitted
	// and currently running for the repo.
	Position int `json:"position,omitempty"`

	// Length is the total number of runs currently queued for this repoURL.
	Length int `json:"length,omitempty"`

	// Running is the name of the CriteriaRun currently admitted for this repoURL.
	Running string `json:"running,omitempty"`

	// Pending lists the names of queued CriteriaRuns for this repoURL, in FIFO order.
	Pending []string `json:"pending,omitempty"`
}

// CriteriaRunStatus defines the observed state of a CriteriaRun.
type CriteriaRunStatus struct {
	// Phase mirrors the lifecycle of the child Job.
	Phase CriteriaRunPhase `json:"phase,omitempty"`

	// JobName is the name of the reconciled child Job.
	JobName string `json:"jobName,omitempty"`

	// PRNumber records the pull request number produced by the run, when
	// known. Informational only: castle supplies no pr_url producer today
	// (nothing publishes run.metadata), so the castle path leaves this empty
	// in practice, and the terminal-completion gate does not depend on it.
	PRNumber string `json:"prNumber,omitempty"`

	// TicketState records the final Linear ticket state (CRI-132 semantics).
	// The castle path leaves it unset: castle carries no Linear ticket-state
	// source (the engine's RunCompleted.final_state is the workflow terminal
	// state name, not the Linear state), so this field is reserved until a
	// ticket-state producer exists. Not part of the terminal-completion gate.
	TicketState string `json:"ticketState,omitempty"`

	// EventsPath is the debug-only events.ndjson mirror path (CRI-136) the
	// runner was handed via EVENTS_FILE. Recorded for observability only:
	// the operator reads run lifecycle from castle, and the file exists just
	// when the operator was explicitly configured with a debug events path.
	EventsPath string `json:"eventsPath,omitempty"`

	// CastleRunID is the run id in the castle control plane backing this
	// run, resolved by castle agent/run discovery keyed off the runner
	// pod's registered agent, and persisted to short-circuit subsequent
	// observations.
	CastleRunID string `json:"castleRunId,omitempty"`

	// CastleTerminalObserved records that a castle observation delivered the
	// run's terminal outcome (the run record's terminal status and/or
	// RunCompleted/RunFailed envelopes). It is the completion signal for the
	// terminal requeue: castle does not supply prNumber/ticketState today,
	// so completion is gated on this marker, not on those fields.
	CastleTerminalObserved bool `json:"castleTerminalObserved,omitempty"`

	// ObservedGeneration tracks the last reconciled generation of the resource.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Queue exposes this run's position and repo-keyed queue state.
	Queue *CriteriaRunQueueStatus `json:"queue,omitempty"`

	// Conditions are optional status conditions for the run.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=cri;crun,scope=Namespaced

// CriteriaRun is the Schema for the criteriaruns API.
type CriteriaRun struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CriteriaRunSpec   `json:"spec,omitempty"`
	Status CriteriaRunStatus `json:"status,omitempty"`
}

// DeepCopyInto creates a deep copy of the CriteriaRun.
func (in *CriteriaRun) DeepCopyInto(out *CriteriaRun) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

// DeepCopy creates a typed clone.
func (in *CriteriaRun) DeepCopy() *CriteriaRun {
	if in == nil {
		return nil
	}
	out := new(CriteriaRun)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject implements runtime.Object.
func (in *CriteriaRun) DeepCopyObject() runtime.Object {
	return in.DeepCopy()
}

// DeepCopyInto for CriteriaRunSpec.
func (in *CriteriaRunSpec) DeepCopyInto(out *CriteriaRunSpec) {
	*out = *in
	if in.Workflow != nil {
		in, out := &in.Workflow, &out.Workflow
		*out = new(RunWorkflow)
		(*in).DeepCopyInto(*out)
	}
}

// DeepCopyInto for RunWorkflow.
func (in *RunWorkflow) DeepCopyInto(out *RunWorkflow) {
	*out = *in
	if in.Volumes != nil {
		in, out := &in.Volumes, &out.Volumes
		*out = make([]RunWorkflowVolume, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
	if in.Secrets != nil {
		in, out := &in.Secrets, &out.Secrets
		*out = make([]RunWorkflowSecret, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
	if in.Env != nil {
		in, out := &in.Env, &out.Env
		*out = make(map[string]string, len(*in))
		for k, v := range *in {
			(*out)[k] = v
		}
	}
}

// DeepCopy creates a copy of RunWorkflow.
func (in *RunWorkflow) DeepCopy() *RunWorkflow {
	if in == nil {
		return nil
	}
	out := new(RunWorkflow)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto for RunWorkflowVolume.
func (in *RunWorkflowVolume) DeepCopyInto(out *RunWorkflowVolume) {
	*out = *in
	if in.Env != nil {
		in, out := &in.Env, &out.Env
		*out = make(map[string]string, len(*in))
		for k, v := range *in {
			(*out)[k] = v
		}
	}
}

// DeepCopy creates a copy of RunWorkflowVolume.
func (in *RunWorkflowVolume) DeepCopy() *RunWorkflowVolume {
	if in == nil {
		return nil
	}
	out := new(RunWorkflowVolume)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto for RunWorkflowSecret.
func (in *RunWorkflowSecret) DeepCopyInto(out *RunWorkflowSecret) {
	*out = *in
	if in.Env != nil {
		in, out := &in.Env, &out.Env
		*out = make(map[string]string, len(*in))
		for k, v := range *in {
			(*out)[k] = v
		}
	}
}

// DeepCopy creates a copy of RunWorkflowSecret.
func (in *RunWorkflowSecret) DeepCopy() *RunWorkflowSecret {
	if in == nil {
		return nil
	}
	out := new(RunWorkflowSecret)
	in.DeepCopyInto(out)
	return out
}

// DeepCopy creates a copy of CriteriaRunSpec.
func (in *CriteriaRunSpec) DeepCopy() *CriteriaRunSpec {
	if in == nil {
		return nil
	}
	out := new(CriteriaRunSpec)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto for CriteriaRunStatus.
func (in *CriteriaRunStatus) DeepCopyInto(out *CriteriaRunStatus) {
	*out = *in
	if in.Queue != nil {
		in, out := &in.Queue, &out.Queue
		*out = new(CriteriaRunQueueStatus)
		(*in).DeepCopyInto(*out)
	}
	if in.Conditions != nil {
		in, out := &in.Conditions, &out.Conditions
		*out = make([]metav1.Condition, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

// DeepCopyInto for CriteriaRunQueueStatus.
func (in *CriteriaRunQueueStatus) DeepCopyInto(out *CriteriaRunQueueStatus) {
	*out = *in
	if in.Pending != nil {
		in, out := &in.Pending, &out.Pending
		*out = make([]string, len(*in))
		copy(*out, *in)
	}
}

// DeepCopy creates a copy of CriteriaRunQueueStatus.
func (in *CriteriaRunQueueStatus) DeepCopy() *CriteriaRunQueueStatus {
	if in == nil {
		return nil
	}
	out := new(CriteriaRunQueueStatus)
	in.DeepCopyInto(out)
	return out
}

// DeepCopy creates a copy of CriteriaRunStatus.
func (in *CriteriaRunStatus) DeepCopy() *CriteriaRunStatus {
	if in == nil {
		return nil
	}
	out := new(CriteriaRunStatus)
	in.DeepCopyInto(out)
	return out
}

// CriteriaRunList is a list of CriteriaRun resources.
type CriteriaRunList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CriteriaRun `json:"items"`
}

// DeepCopyInto creates a deep copy of the list.
func (in *CriteriaRunList) DeepCopyInto(out *CriteriaRunList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		in, out := &in.Items, &out.Items
		*out = make([]CriteriaRun, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

// DeepCopy creates a runtime.Object clone.
func (in *CriteriaRunList) DeepCopy() runtime.Object {
	if in == nil {
		return nil
	}
	out := new(CriteriaRunList)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject implements runtime.Object.
func (in *CriteriaRunList) DeepCopyObject() runtime.Object {
	return in.DeepCopy()
}

var SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

// AddToScheme adds the types in this group-version to the given scheme.
func AddToScheme(s *runtime.Scheme) error {
	return SchemeBuilder.AddToScheme(s)
}

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(GroupVersion,
		&CriteriaRun{},
		&CriteriaRunList{},
	)
	metav1.AddToGroupVersion(scheme, GroupVersion)
	return nil
}
