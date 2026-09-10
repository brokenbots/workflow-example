// Package jobbuilder constructs the batch/v1 Job that reconciles a CriteriaRun.
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
	Namespace       string
	ProviderBaseURL string
}

// Build returns the desired Job for a CriteriaRun.
func Build(run *criteriav1.CriteriaRun, defaults Defaults) *batchv1.Job {
	ticket := run.Spec.TicketID
	jobName := JobName(run)
	repoURL := run.Spec.RepoURL
	image := firstNonEmpty(run.Spec.Image, defaults.Image, "localhost:5000/linear-intake-remote:dev")
	dataPVC := firstNonEmpty(defaults.DataPVC, "criteria-data")
	providerBaseURL := firstNonEmpty(run.Spec.ProviderBaseURL, defaults.ProviderBaseURL, "http://192.168.17.116:11434/v1")
	maxVisits := run.Spec.MaxAgentVisits
	if maxVisits == 0 {
		maxVisits = 2
	}

	repoDir := "/repo"
	intakeRoot := "/data/intake"
	triageRoot := "/data/triage"
	eventsFile := fmt.Sprintf("%s/%s/events.ndjson", intakeRoot, ticket)

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: run.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "criteria-run",
				"app.kubernetes.io/managed-by":   "criteria-k8s",
				"criteria.brokenbots.dev/run":  run.Name,
				"ticket":                       safeLabelValue(ticket),
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         run.APIVersion,
					Kind:               run.Kind,
					Name:               run.Name,
					UID:                run.UID,
					Controller:         boolPtr(true),
					BlockOwnerDeletion: boolPtr(true),
				},
			},
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: intPtr(86400),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app.kubernetes.io/name":      "criteria-run",
						"app.kubernetes.io/managed-by": "criteria-k8s",
						"criteria.brokenbots.dev/run": run.Name,
						"ticket":                      safeLabelValue(ticket),
					},
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: "criteria-runner",
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
					InitContainers: []corev1.Container{
						repoCloneContainer(image, repoURL, repoDir),
					},
					Containers: []corev1.Container{
						workflowRunnerContainer(run, image, repoDir, intakeRoot, triageRoot, eventsFile, providerBaseURL, maxVisits),
						adapterContainer("adapter-copilot", "copilot", image),
						adapterContainer("adapter-shell", "shell", image),
					},
					Volumes: []corev1.Volume{
						dataVolume(dataPVC),
						repoVolume(),
						csiVolume("linear-secrets", "linear-spc"),
						csiVolume("copilot-secrets", "copilot-spc"),
						csiVolume("shell-secrets", "shell-spc"),
						scriptsVolume(),
					},
				},
			},
		},
	}

	// Set GVK for the owner reference when it is missing (common in tests).
	if job.OwnerReferences[0].APIVersion == "" {
		job.OwnerReferences[0].APIVersion = schema.GroupVersion{Group: "criteria.brokenbots.dev", Version: "v1"}.String()
	}
	if job.OwnerReferences[0].Kind == "" {
		job.OwnerReferences[0].Kind = "CriteriaRun"
	}
	return job
}

// JobName derives the child Job name from the CriteriaRun.
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

func restrictedContainerSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: boolPtr(false),
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
		},
	}
}

func repoCloneContainer(image, repoURL, repoDir string) corev1.Container {
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
if [ -r /secrets/workflow_github_token ]; then
    WORKFLOW_GITHUB_TOKEN=$(cat /secrets/workflow_github_token)
fi
if [ -z "$WORKFLOW_GITHUB_TOKEN" ]; then
    echo "WORKFLOW_GITHUB_TOKEN is required via /secrets/workflow_github_token" >&2
    exit 1
fi
if [ -z "$REPO_URL" ]; then
    echo "REPO_URL is required" >&2
    exit 1
fi
find /repo -mindepth 1 -delete 2>/dev/null || true
git config --global credential.https://github.com.helper '!gh auth git-credential'
GH_TOKEN="$WORKFLOW_GITHUB_TOKEN" gh repo clone "$REPO_URL" /repo`,
		},
		Env: []corev1.EnvVar{
			{Name: "REPO_URL", Value: repoURL},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "repo", MountPath: repoDir},
			{Name: "shell-secrets", MountPath: "/secrets"},
		},
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

func workflowRunnerContainer(run *criteriav1.CriteriaRun, image, repoDir, intakeRoot, triageRoot, eventsFile, providerBaseURL string, maxVisits int) corev1.Container {
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
		{Name: "EVENTS_FILE", Value: eventsFile},
	}

	return corev1.Container{
		Name:            "workflow-runner",
		Image:           image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		SecurityContext: restrictedContainerSecurityContext(),
		Command:         []string{"/opt/criteria-pod-adapter/runner.sh"},
		Env:             env,
		VolumeMounts: []corev1.VolumeMount{
			{Name: "data", MountPath: "/data"},
			{Name: "repo", MountPath: repoDir},
			{Name: "linear-secrets", MountPath: "/secrets"},
			{Name: "copilot-secrets", MountPath: "/home/criteria/secrets"},
			{Name: "scripts", MountPath: "/opt/criteria-pod-adapter"},
		},
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

func adapterContainer(name, kind, image string) corev1.Container {
	return corev1.Container{
		Name:            name,
		Image:           image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		SecurityContext: restrictedContainerSecurityContext(),
		Command:         []string{"/opt/criteria-pod-adapter/sidecar.sh"},
		Env:             []corev1.EnvVar{{Name: "ADAPTER_KIND", Value: kind}},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "data", MountPath: "/data"},
			{Name: "repo", MountPath: "/repo"},
			{Name: fmt.Sprintf("%s-secrets", kind), MountPath: "/secrets"},
			{Name: "scripts", MountPath: "/opt/criteria-pod-adapter"},
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceMemory: resourceQuantity("1Gi"),
				corev1.ResourceCPU:    resourceQuantity("500m"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceMemory: resourceQuantity("4Gi"),
				corev1.ResourceCPU:    resourceQuantity("2000m"),
			},
		},
	}
}

func dataVolume(pvc string) corev1.Volume {
	return corev1.Volume{
		Name: "data",
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvc},
		},
	}
}

func repoVolume() corev1.Volume {
	return corev1.Volume{
		Name: "repo",
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		},
	}
}

func csiVolume(name, spc string) corev1.Volume {
	return corev1.Volume{
		Name: name,
		VolumeSource: corev1.VolumeSource{
			CSI: &corev1.CSIVolumeSource{
				Driver:    "secrets-store.csi.k8s.io",
				ReadOnly:  boolPtr(true),
				VolumeAttributes: map[string]string{
					"secretProviderClass": spc,
				},
			},
		},
	}
}

func scriptsVolume() corev1.Volume {
	return corev1.Volume{
		Name: "scripts",
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: "pod-adapter-scripts"},
				DefaultMode:            int32Ptr(0755),
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

func boolPtr(b bool) *bool          { return &b }
func intPtr(i int32) *int32         { return &i }
func int32Ptr(i int32) *int32       { return &i }
func int64Ptr(i int64) *int64       { return &i }
func resourceQuantity(q string) resource.Quantity {
	return resource.MustParse(q)
}
