package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// CriteriaRunPhase describes the lifecycle phase of a run.
type CriteriaRunPhase string

const (
	PhasePending    CriteriaRunPhase = "Pending"
	PhaseRunning    CriteriaRunPhase = "Running"
	PhaseSucceeded  CriteriaRunPhase = "Succeeded"
	PhaseFailed     CriteriaRunPhase = "Failed"
	PhaseUnknown    CriteriaRunPhase = "Unknown"
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
}

// CriteriaRunStatus defines the observed state of a CriteriaRun.
type CriteriaRunStatus struct {
	// Phase mirrors the lifecycle of the child Job.
	Phase CriteriaRunPhase `json:"phase,omitempty"`

	// JobName is the name of the reconciled child Job.
	JobName string `json:"jobName,omitempty"`

	// PRNumber records the pull request number produced by the run, when known.
	PRNumber string `json:"prNumber,omitempty"`

	// TicketState records the final Linear ticket state read from the run events.
	TicketState string `json:"ticketState,omitempty"`

	// EventsPath is the absolute path to the run's events file on the shared /data volume.
	EventsPath string `json:"eventsPath,omitempty"`

	// ObservedGeneration tracks the last reconciled generation of the resource.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

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
	if in.Conditions != nil {
		in, out := &in.Conditions, &out.Conditions
		*out = make([]metav1.Condition, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
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
