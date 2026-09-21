// Package jobbuilder constructs the batch/v1 Jobs that reconcile a CriteriaRun.
package jobbuilder

import (
	"fmt"
	"regexp"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
)

// Labels shared by every child object the operator reconciles. The
// per-scope reconcile, the finalize path, and the adapter sweeper key on
// the run/role pair, so the values must stay in one place.
const (
	// LabelRun carries the name of the owning CriteriaRun.
	LabelRun = "criteria.brokenbots.dev/run"
	// LabelRole distinguishes the runner from the adapter children.
	LabelRole = "criteria.brokenbots.dev/role"
	// LabelAdapterKind is the adapter implementation kind of a per-adapter
	// (environment-less) fallback pod. Group pods can host several adapters,
	// so their kind set lives on the AnnotationAdapterKinds annotation
	// instead: a comma-joined kind set is illegal as a label value
	// (CRI-234 R1).
	LabelAdapterKind = "criteria.brokenbots.dev/adapter-kind"
	// AnnotationAdapterKinds is the deduplicated, sorted set of adapter
	// kinds hosted by a (scope, environment) group pod, comma-joined. It is
	// an annotation, not a label, because the comma separator is illegal in
	// a Kubernetes label value (CRI-234 R1).
	AnnotationAdapterKinds = "criteria.brokenbots.dev/adapter-kinds"
	// LabelScopeID is the scope instance id a per-scope adapter pod serves.
	// Group pods serve exactly one scope, so the label stays single-valued
	// under the (scope, environment) grouping.
	LabelScopeID = "criteria.brokenbots.dev/scope-id"
	// LabelEnvironment is the environment identity a group pod was built
	// from (CRI-234). Absent on the per-adapter fallback pods.
	LabelEnvironment = "criteria.brokenbots.dev/environment"
	// LabelHostAffinityPrefix namespaces the per-volume same-host affinity
	// keys on the pod labels the affinity terms select (CRI-235): one
	// label per host-affinity volume declaration, valued by the run name.
	LabelHostAffinityPrefix = "criteria.brokenbots.dev/affinity-"
	// RoleRunner / RoleAdapter are the LabelRole values in use.
	RoleRunner  = "runner"
	RoleAdapter = "adapter"
)

// Runner container and image resolution names shared by the job builders
// and the reconciler (CRI-264): the reconciler reads the pinned image back
// off the runner container by name and compares it against the resolution
// the operator would perform today, so the two sides must agree on the
// container name and the fallback chain.
const (
	// RunnerContainerName is the workflow-runner container's name in the
	// runner Job pod template.
	RunnerContainerName = "workflow-runner"
	// DefaultWorkflowImage is the image-mode fallback when neither the run
	// spec nor the operator declares an image: the baked-tree workflow
	// image.
	DefaultWorkflowImage = "localhost:5000/linear-intake-remote:dev"
	// EnvCriteriaBaseImage is the operator env var the source-mode base
	// image is resolved from (CRI-230). cmd/operator mirrors it as the
	// --criteria-base-image flag default.
	EnvCriteriaBaseImage = "CRITERIA_BASE_IMAGE"
	// EnvDefaultImage is the operator env var the image-mode default image
	// is resolved from. cmd/operator mirrors it as the --default-image
	// flag default.
	EnvDefaultImage = "DEFAULT_CRITERIA_IMAGE"
)

var nonDNS = regexp.MustCompile(`[^a-z0-9-]+`)
var nonLabel = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

// safeObjectName returns a DNS-1123 subdomain-safe name derived from s.
func safeObjectName(s string) string {
	s = strings.ToLower(s)
	s = nonDNS.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 63 {
		s = s[:63]
	}
	s = strings.Trim(s, "-")
	if s == "" {
		s = "unknown"
	}
	return s
}

// safeLabelValue returns a Kubernetes label value derived from s.
func safeLabelValue(s string) string {
	s = nonLabel.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-_.")
	if len(s) > 63 {
		s = s[:63]
	}
	s = strings.Trim(s, "-_.")
	if s == "" {
		s = "unknown"
	}
	return s
}

// Defaults carries operator-wide defaults used when the CriteriaRun spec omits a value.
type Defaults struct {
	Image           string
	DataPVC         string
	ProviderBaseURL string
	// CastleAddr is the castle orchestrator address handed to runner Jobs.
	// When set, runners execute criteria in server mode: lifecycle is
	// published to castle (CRI-133/134), which is the operator's only run
	// observation surface. Empty keeps the runner in local file mode.
	CastleAddr string
	// DebugEventsFile is the debug-only events.ndjson path handed to runner
	// Jobs via EVENTS_FILE (CRI-136). The events.ndjson dual-write is
	// retired: only an explicitly configured debug path makes criteria
	// additionally mirror lifecycle events to a file. Empty (default) runs
	// castle-only with no events.ndjson write anywhere in the run.
	DebugEventsFile string
	// CriteriaBaseImage is the source-mode base image (CRI-230): the
	// minimal image carrying the criteria binary, from which a
	// spec.workflowSource run fetches and applies the workflow source. It
	// is the image-mode Defaults.Image's counterpart for source mode and is
	// never used by the baked-tree path. Empty falls back to the built-in
	// criteria-base default.
	CriteriaBaseImage string
}

// adapterKinds lists the adapter types that get a dedicated Job per CriteriaRun.
var adapterKinds = []string{"shell", "copilot"}

// adapterImage returns the remote adapter image for a given adapter kind.
func adapterImage(kind string) string {
	switch kind {
	case "shell":
		return "localhost:5000/criteria-adapter-shell:k8s-0.5.4-2"
	case "copilot":
		return "localhost:5000/criteria-adapter-copilot:k8s-0.5.8"
	default:
		return fmt.Sprintf("localhost:5000/criteria-adapter-%s:k8s-3", kind)
	}
}

// Build returns the runner Job for a CriteriaRun. It is retained for callers
// that only need the runner; new reconciler code should prefer BuildAll.
func Build(run *criteriav1.CriteriaRun, defaults Defaults) *batchv1.Job {
	return BuildRunnerJob(run, defaults)
}

// BuildAll returns the runner Job plus one adapter Job per adapter kind. All
// Jobs are owner-referenced to the CriteriaRun for orphan cleanup. When the
// run opts into per-scope sessions, only the runner Job is returned; adapter
// pods are reconciled independently from the engine's lifecycle events.
func BuildAll(run *criteriav1.CriteriaRun, defaults Defaults) []*batchv1.Job {
	jobs := []*batchv1.Job{BuildRunnerJob(run, defaults)}
	if run.Spec.PerScopeSessions {
		return jobs
	}
	for _, kind := range adapterKinds {
		jobs = append(jobs, BuildAdapterJob(run, defaults, kind))
	}
	return jobs
}

// JobName derives the child runner Job name from the CriteriaRun.
func JobName(run *criteriav1.CriteriaRun) string {
	if run.Labels != nil {
		if name := run.Labels["criteria.brokenbots.dev/job-name"]; name != "" {
			return name
		}
	}
	if run.Name != "" {
		return run.Name
	}
	return fmt.Sprintf("criteria-run-%s", safeObjectName(run.Spec.TicketID))
}

// RunnerJobName returns the runner Job name for a CriteriaRun.
func RunnerJobName(run *criteriav1.CriteriaRun) string {
	return JobName(run)
}

// AdapterJobName returns the dedicated adapter Job name for a CriteriaRun.
func AdapterJobName(run *criteriav1.CriteriaRun, kind string) string {
	return fmt.Sprintf("%s-adapter-%s", JobName(run), kind)
}

func baseLabels(run *criteriav1.CriteriaRun) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "criteria-run",
		"app.kubernetes.io/managed-by": "criteria-k8s",
		LabelRun:                       run.Name,
		"ticket":                       safeLabelValue(run.Spec.TicketID),
	}
}

func ownerReference(run *criteriav1.CriteriaRun) metav1.OwnerReference {
	ref := metav1.OwnerReference{
		APIVersion:         run.APIVersion,
		Kind:               run.Kind,
		Name:               run.Name,
		UID:                run.UID,
		Controller:         boolPtr(true),
		BlockOwnerDeletion: boolPtr(true),
	}
	if ref.APIVersion == "" {
		ref.APIVersion = schema.GroupVersion{Group: "criteria.brokenbots.dev", Version: "v1"}.String()
	}
	if ref.Kind == "" {
		ref.Kind = "CriteriaRun"
	}
	return ref
}

func buildJobBase(run *criteriav1.CriteriaRun, namespace, name string, labels map[string]string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			Labels:          labels,
			OwnerReferences: []metav1.OwnerReference{ownerReference(run)},
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: intPtr(86400),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					NodeSelector: map[string]string{
						"kubernetes.io/arch": "amd64",
					},
					Tolerations: []corev1.Toleration{
						{
							Key:      "catch",
							Operator: corev1.TolerationOpExists,
							Effect:   corev1.TaintEffectNoSchedule,
						},
					},
					RestartPolicy: corev1.RestartPolicyOnFailure,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: boolPtr(true),
						RunAsUser:    int64Ptr(10001),
						RunAsGroup:   int64Ptr(10001),
						FSGroup:      int64Ptr(10001),
						SeccompProfile: &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						},
					},
				},
			},
		},
	}
}

// ResolveRunnerImage resolves the image the runner Job pins for this run
// under the given defaults (CRI-264): source mode resolves
// spec.image > defaults.CriteriaBaseImage > the built-in criteria-base
// default, image mode resolves spec.image > defaults.Image > the built-in
// workflow image. The reconciler compares this resolution against the image
// already pinned on the run's child Jobs and its status.baseImage stamp to
// detect a CRITERIA_BASE_IMAGE / DEFAULT_CRITERIA_IMAGE change while a run
// is in flight, so every image-producing path must resolve through this
// function.
func ResolveRunnerImage(run *criteriav1.CriteriaRun, defaults Defaults) string {
	if run.Spec.WorkflowSource != nil {
		return sourceModeImage(run, defaults)
	}
	return firstNonEmpty(run.Spec.Image, defaults.Image, DefaultWorkflowImage)
}

// RunnerJobImage returns the image pinned on a runner Job's workflow-runner
// container, or "" when the container is absent (the job builders always
// emit the container, so "" means "no pinned image to compare"). Exported
// for the reconciler's base-image mismatch check (CRI-264) — the reconciler
// must read the runner container back by the same name the builders pin.
func RunnerJobImage(runnerJob *batchv1.Job) string {
	for i := range runnerJob.Spec.Template.Spec.Containers {
		if runnerJob.Spec.Template.Spec.Containers[i].Name == RunnerContainerName {
			return runnerJob.Spec.Template.Spec.Containers[i].Image
		}
	}
	return ""
}

// BuildRunnerJob constructs the runner Job: repo-clone init container + workflow-runner.
// The workflow object stamped on the run (CRI-222) supplies the target
// namespace, the declared volumes, and the CSI secret delivery; the image
// resolves from the run spec and the operator default.
//
// Two modes branch here (CRI-231):
//   - Image mode (spec.workflowSource nil): the baked-tree path — the
//     repo-clone init container plus the runner.sh workflow image, unchanged.
//   - Source mode (spec.workflowSource set): no repo-clone; the runner
//     executes on the base/provided image and fetches/applies the declared
//     workflow source at run time.
func BuildRunnerJob(run *criteriav1.CriteriaRun, defaults Defaults) *batchv1.Job {
	ticket := run.Spec.TicketID
	jobName := JobName(run)
	repoURL := run.Spec.RepoURL
	if run.Spec.WorkflowSource != nil {
		return buildSourceRunnerJob(run, defaults)
	}
	image := ResolveRunnerImage(run, defaults)
	dataPVC := firstNonEmpty(defaults.DataPVC, "criteria-data")
	providerBaseURL := firstNonEmpty(run.Spec.ProviderBaseURL, defaults.ProviderBaseURL, "http://192.168.17.116:11434/v1")
	maxVisits := run.Spec.MaxAgentVisits
	if maxVisits == 0 {
		maxVisits = 2
	}

	// Per-ticket repo clone on the data PVC to avoid concurrent runs
	// clobbering each other's git clone on the shared repo PVC.
	repoDir := fmt.Sprintf("/data/intake/%s/repo", ticket)
	intakeRoot := "/data/intake"
	triageRoot := "/data/triage"

	plan := newWorkflowPlan(run)

	labels := baseLabels(run)
	labels[LabelRole] = RoleRunner

	job := buildJobBase(run, TargetNamespace(run), jobName, labels)
	job.Spec.Template.Spec.ServiceAccountName = "criteria-runner"
	job.Spec.Template.Spec.InitContainers = []corev1.Container{
		repoCloneContainer(image, repoURL, repoDir, plan),
	}
	job.Spec.Template.Spec.Containers = []corev1.Container{
		workflowRunnerContainer(run, image, repoDir, intakeRoot, triageRoot, providerBaseURL, maxVisits, defaults, plan),
	}
	job.Spec.Template.Spec.Volumes = plan.runnerVolumes(dataPVC)
	plan.applyHostAffinity(job.Spec.Template.Labels, &job.Spec.Template.Spec)
	return job
}

// BuildAdapterJob constructs a dedicated adapter Job for the given kind.
func BuildAdapterJob(run *criteriav1.CriteriaRun, defaults Defaults, kind string) *batchv1.Job {
	jobName := AdapterJobName(run, kind)
	image := adapterImage(kind)
	dataPVC := firstNonEmpty(defaults.DataPVC, "criteria-data")

	labels := baseLabels(run)
	labels[LabelRole] = RoleAdapter
	labels["criteria.brokenbots.dev/adapter-kind"] = kind

	plan := newWorkflowPlan(run)

	hasSecrets := plan.hasSecrets()
	job := buildJobBase(run, TargetNamespace(run), jobName, labels)
	job.Spec.Template.Spec.AutomountServiceAccountToken = boolPtr(hasSecrets)
	if hasSecrets {
		// The OpenBao CSI provider authenticates the pod through its
		// service account token; adapter pods carrying declared secrets
		// therefore run under the criteria-runner service account.
		job.Spec.Template.Spec.ServiceAccountName = "criteria-runner"
	}
	job.Spec.Template.Spec.Containers = []corev1.Container{
		adapterContainer(kind, image, JobName(run), plan),
	}
	job.Spec.Template.Spec.Volumes = plan.adapterVolumes(dataPVC, true)
	plan.applyHostAffinity(job.Spec.Template.Labels, &job.Spec.Template.Spec)
	return job
}

func restrictedContainerSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: boolPtr(false),
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
		},
	}
}

func repoCloneContainer(image, repoURL, repoDir string, plan *workflowPlan) corev1.Container {
	return corev1.Container{
		Name:            "repo-clone",
		Image:           image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		SecurityContext: restrictedContainerSecurityContext(),
		Command: []string{
			"/bin/sh",
			"-c",
			`set -eu
WORKFLOW_GITHUB_TOKEN=""
if [ -r /home/criteria/secrets/workflow_github_token ]; then
    WORKFLOW_GITHUB_TOKEN=$(cat /home/criteria/secrets/workflow_github_token)
fi
if [ -z "$WORKFLOW_GITHUB_TOKEN" ]; then
    echo "WORKFLOW_GITHUB_TOKEN is required via /home/criteria/secrets/workflow_github_token" >&2
    exit 1
fi
if [ -z "$REPO_URL" ]; then
    echo "REPO_URL is required" >&2
    exit 1
fi
mkdir -p "$(dirname "$REPO_DIR")"
rm -rf "$REPO_DIR"
git config --global credential.https://github.helper '!gh auth git-credential'
GH_TOKEN="$WORKFLOW_GITHUB_TOKEN" gh repo clone "$REPO_URL" "$REPO_DIR"`,
		},
		Env: appendEnvDistinct([]corev1.EnvVar{
			{Name: "REPO_URL", Value: repoURL},
			{Name: "REPO_DIR", Value: repoDir},
		}, plan.volumeEnvs()),
		VolumeMounts: plan.cloneMounts(),
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceMemory: resourceQuantity("512Mi"),
				corev1.ResourceCPU:    resourceQuantity("250m"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceMemory: resourceQuantity("2Gi"),
				corev1.ResourceCPU:    resourceQuantity("1000m"),
			},
		},
	}
}

func workflowRunnerContainer(run *criteriav1.CriteriaRun, image, repoDir, intakeRoot, triageRoot, providerBaseURL string, maxVisits int, defaults Defaults, plan *workflowPlan) corev1.Container {
	env := []corev1.EnvVar{
		{Name: "TICKET_ID", Value: run.Spec.TicketID},
		{Name: "REPO_URL", Value: run.Spec.RepoURL},
		{Name: "REPO_DIR", Value: repoDir},
		{Name: "INTAKE_ROOT", Value: intakeRoot},
		{Name: "TRIAGE_ROOT", Value: triageRoot},
		{Name: "LINEAR_REVIEW_STATE", Value: "In Review"},
		{Name: "LINEAR_TRIAGE_STATE", Value: "Triage"},
		{Name: "LINEAR_WORK_STATE", Value: "In Progress"},
		{Name: "LINEAR_DONE_STATE", Value: "Done"},
		{Name: "BASE_BRANCH", Value: "main"},
		{Name: "BUILD_CMD", Value: run.Spec.BuildCmd},
		{Name: "TEST_CMD", Value: run.Spec.TestCmd},
		{Name: "CI_GATE_CMD", Value: run.Spec.CIGateCmd},
		{Name: "TEST_REFS", Value: "both"},
		{Name: "MAIN_REF", Value: "origin/main"},
		{Name: "STABLE_REF", Value: ""},
		{Name: "DESIGN_INTENT_FILE", Value: ""},
		{Name: "REPRO_WORKFLOW_DIR", Value: ""},
		{Name: "ALLOW_DIRTY", Value: "false"},
		{Name: "MAX_AGENT_VISITS", Value: fmt.Sprintf("%d", maxVisits)},
		{Name: "PROVIDER_BASE_URL", Value: providerBaseURL},
		// CRITERIA_HOME on the shared data PVC: the engine's run state
		// (including rotated per-scope accept-token files) must be readable
		// by the per-scope adapter pods, which mount /data but have no other
		// view into the runner's container filesystem. The default
		// (~/.local/criteria) is container-local and the token_ref paths in
		// the lifecycle events would dangle for every adapter pod.
		//
		// Image-mode runners stay on the PVC (CRI-237): their engine is the
		// frozen pre-eae0181 workflow image, which emits token_ref without
		// accept_token, so the legacy file delivery — and a PVC-visible
		// CRITERIA_HOME — must survive until image mode is re-audited and
		// rolled. Source-mode runners (criteria-base >= eae0181) deliver
		// tokens on the wire and keep their engine state container-local.
		{Name: "CRITERIA_HOME", Value: "/data/criteria"},
		{Name: "JOB_NAME", Value: JobName(run)},
		{
			Name: "POD_IP",
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{
					FieldPath: "status.podIP",
				},
			},
		},
	}
	// Debug-only events mirror (CRI-136): the env var is only injected when
	// the operator is explicitly configured with a debug events path, so the
	// default runner writes no events.ndjson anywhere.
	if defaults.DebugEventsFile != "" {
		env = append(env, corev1.EnvVar{Name: "EVENTS_FILE", Value: defaults.DebugEventsFile})
	}
	if defaults.CastleAddr != "" {
		env = append(env, corev1.EnvVar{Name: "CASTLE_ADDR", Value: defaults.CastleAddr})
	}
	env = appendEnvDistinct(env, plan.runnerEnvs())

	return corev1.Container{
		Name:            RunnerContainerName,
		Image:           image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		SecurityContext: restrictedContainerSecurityContext(),
		Command:         []string{"/opt/criteria-pod-adapter/runner.sh"},
		Env:             env,
		VolumeMounts:    plan.runnerMounts(),
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceMemory: resourceQuantity("2Gi"),
				corev1.ResourceCPU:    resourceQuantity("1000m"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceMemory: resourceQuantity("8Gi"),
				corev1.ResourceCPU:    resourceQuantity("4000m"),
			},
		},
	}
}

func adapterContainer(kind, image, runnerJobName string, plan *workflowPlan) corev1.Container {
	resources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceMemory: resourceQuantity("512Mi"),
			corev1.ResourceCPU:    resourceQuantity("250m"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceMemory: resourceQuantity("2Gi"),
			corev1.ResourceCPU:    resourceQuantity("1000m"),
		},
	}
	if kind == "copilot" {
		resources = corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceMemory: resourceQuantity("2Gi"),
				corev1.ResourceCPU:    resourceQuantity("500m"),
			},
			Limits: corev1.ResourceList{
				// CRI-272: the 4Gi cap OOM-killed the adapter mid-tool on
				// memory-heavy workloads (npm ci + full test baseline + Go
				// builds in one sandbox), which surfaced as the copilot
				// session-death crash family. Raise to 8Gi.
				corev1.ResourceMemory: resourceQuantity("8Gi"),
				corev1.ResourceCPU:    resourceQuantity("2000m"),
			},
		}
	}

	return corev1.Container{
		Name:            fmt.Sprintf("adapter-%s", kind),
		Image:           image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		SecurityContext: restrictedContainerSecurityContext(),
		Command:         []string{"/opt/criteria-pod-adapter/adapter.sh"},
		Env: appendEnvDistinct([]corev1.EnvVar{
			{Name: "ADAPTER_KIND", Value: kind},
			{Name: "CRITERIA_RUN_JOB_NAME", Value: runnerJobName},
		}, plan.adapterEnvs()),
		VolumeMounts: plan.adapterMounts(true),
		Resources:    resources,
	}
}

func dataVolume(pvc string) corev1.Volume {
	return corev1.Volume{
		Name: dataVolumeName,
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvc},
		},
	}
}

func csiVolume(name, spc string) corev1.Volume {
	return corev1.Volume{
		Name: name,
		VolumeSource: corev1.VolumeSource{
			CSI: &corev1.CSIVolumeSource{
				Driver:   "secrets-store.csi.k8s.io",
				ReadOnly: boolPtr(true),
				VolumeAttributes: map[string]string{
					"secretProviderClass": spc,
				},
			},
		},
	}
}

func scriptsVolume() corev1.Volume {
	return corev1.Volume{
		Name: scriptsVolumeName,
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: "pod-adapter-scripts"},
				DefaultMode:          int32Ptr(0755),
			},
		},
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func boolPtr(b bool) *bool    { return &b }
func intPtr(i int32) *int32   { return &i }
func int32Ptr(i int32) *int32 { return &i }
func int64Ptr(i int64) *int64 { return &i }
func resourceQuantity(q string) resource.Quantity {
	return resource.MustParse(q)
}
