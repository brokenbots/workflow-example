// Source-mode job construction (CRI-231, ADR-0005 D1/D2). When the run's
// spec carries a workflowSource, the runner does not execute a baked
// /workflows tree: it executes on a base/provided image, fetches the
// declared workflow source at run time, and applies it — mirroring the
// criteria-base entrypoint contract (CRI-230). The URL is content; the
// image is the process. Run provenance (resolved source, ref, cache path)
// is recorded by the criteria binary's run-metadata publisher (CRI-225) at
// run admission; nothing needs to be recorded on the Kubernetes side.

package jobbuilder

import (
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
)

// CriteriaBaseImageDefault is the source-mode fallback when neither the run
// spec nor the operator declares an image: the CRI-230 minimal criteria
// base image. The Makefile's CRITERIA_BASE_IMAGE variable mirrors the name.
const CriteriaBaseImageDefault = "localhost:5000/criteria-base:dev"

// sourceRunnerScript is the inline runner contract for source-mode runs. It
// implements the criteria-base entrypoint semantics (CRI-230) so url-only
// and url+image runs behave identically — the image only changes which
// binary serves the apply:
//
//   - fail fast when the image lacks the criteria binary (a provided
//     process image without it cannot run the fetched workflow);
//   - fail closed on an undeclared workflow source (D2: no baked tree to
//     fall back to);
//   - verify CRITERIA_HOME is a writable directory before any fetch;
//   - apply WORKFLOW_URL, pinning WORKFLOW_REF when declared (CRI-226).
//
// Discovery publishing is host-only: per-scope adapter pods learn the
// runner's dial address from the shared host file (CRITERIA_REMOTE_HOST is
// deliberately never set by the operator), while digest and accept-token
// material arrives through the operator's env (event digest) and the
// engine-rotated token files under CRITERIA_HOME. Source mode never
// substitutes image-mode token placeholders into a fetched tree.
const sourceRunnerScript = `set -eu

criteria_bin="${CRITERIA_BIN:-/usr/local/bin/criteria}"
if ! [ -x "$criteria_bin" ]; then
    echo "criteria binary $criteria_bin not found or not executable: source-mode runs require an image carrying the criteria binary" >&2
    exit 69
fi

workflow_url="${WORKFLOW_URL-}"
if [ "$(printf '%s' "$workflow_url" | tr -d '[:space:]')" = "" ]; then
    echo "WORKFLOW_URL is not set: source-mode runs require a declared workflow source (there is no baked /workflows tree to fall back to)" >&2
    exit 64
fi

criteria_home="${CRITERIA_HOME:-/data/criteria}"
if ! mkdir -p "$criteria_home" 2>/dev/null || [ ! -w "$criteria_home" ]; then
    echo "CRITERIA_HOME $criteria_home is not a directory writable by the runner uid" >&2
    exit 70
fi
# The engine keeps run state (rotated accept-token files) under
# CRITERIA_HOME: export the verified path so criteria uses the same home
# even when the container env did not declare one.
export CRITERIA_HOME

# Publish the runner's routable dial address for per-scope adapter pods
# polling the shared discovery directory. Only the host is published: the
# digest is carried per pod by the operator's env, and accept-tokens are
# rotated by the engine into CRITERIA_HOME files. 7778 is the shim's
# conventional port for k8s-runs workflows (adapters.chcl listen default).
run_dir=""
run_dir_root="${CRITERIA_RUN_DIR_ROOT:-/data/.criteria/runs}"
if [ -n "${JOB_NAME:-}" ] && [ -n "${POD_IP:-}" ]; then
    run_dir="$run_dir_root/$JOB_NAME"
    mkdir -p "$run_dir"
    printf '%s' "${POD_IP}:7778" > "$run_dir/host"
    trap 'rm -rf "$run_dir"' EXIT
fi

set -- apply "$workflow_url"
workflow_ref="${WORKFLOW_REF-}"
if [ "$(printf '%s' "$workflow_ref" | tr -d '[:space:]')" != "" ]; then
    set -- "$@" --workflow-ref "$workflow_ref"
fi

# Server mode (CRI-133/134): publish run lifecycle to castle when
# configured. Castle dev mode serves plaintext; the engine refuses
# plaintext connections to non-loopback hosts unless TLS is explicitly
# disabled (CRI-138).
: "${CASTLE_ADDR:=}"
if [ -n "$CASTLE_ADDR" ]; then
    export CRITERIA_SERVER_TLS=disable
    set -- "$@" --server "$CASTLE_ADDR"
fi

# Debug-only events mirror (CRI-136): unset keeps the run castle-only.
: "${EVENTS_FILE:=}"
if [ -n "$EVENTS_FILE" ]; then
    set -- "$@" --events-file "$EVENTS_FILE"
fi

# Foreground (not exec) so the EXIT trap still cleans the discovery dir;
# set -e propagates the criteria exit status as the container exit code.
"$criteria_bin" "$@" --output concise
`

// buildSourceRunnerJob constructs the runner Job for a spec.workflowSource
// run. There is no repo-clone init container: the base image ships no gh
// and the workflow source is content, so a fetched workflow that needs the
// ticket repository clones it itself. The workflow object's declared
// volumes, secrets, and env still render through the plan, so url+image
// runs of a workflow-library object carry the same storage surface as their
// image-mode counterparts.
func buildSourceRunnerJob(run *criteriav1.CriteriaRun, defaults Defaults) *batchv1.Job {
	jobName := JobName(run)
	dataPVC := firstNonEmpty(defaults.DataPVC, "criteria-data")
	providerBaseURL := firstNonEmpty(run.Spec.ProviderBaseURL, defaults.ProviderBaseURL, "http://192.168.17.116:11434/v1")
	maxVisits := run.Spec.MaxAgentVisits
	if maxVisits == 0 {
		maxVisits = 2
	}

	plan := newWorkflowPlan(run)

	labels := baseLabels(run)
	labels[LabelRole] = RoleRunner

	job := buildJobBase(run, targetNamespace(run), jobName, labels)
	job.Spec.Template.Spec.ServiceAccountName = "criteria-runner"
	job.Spec.Template.Spec.Containers = []corev1.Container{
		sourceRunnerContainer(run, sourceModeImage(run, defaults), providerBaseURL, maxVisits, defaults, plan),
	}
	job.Spec.Template.Spec.Volumes = plan.runnerVolumes(dataPVC)
	return job
}

// sourceModeImage resolves the source-mode process image: spec.image is the
// url+image process image, the operator's CRITERIA_BASE_IMAGE the url-only
// default. Image-mode defaults.Image (the baked workflow image) is
// deliberately not consulted — it carries the baked tree, not the criteria
// binary contract (CRI-230).
func sourceModeImage(run *criteriav1.CriteriaRun, defaults Defaults) string {
	return firstNonEmpty(run.Spec.Image, defaults.CriteriaBaseImage, CriteriaBaseImageDefault)
}

func sourceRunnerContainer(run *criteriav1.CriteriaRun, image, providerBaseURL string, maxVisits int, defaults Defaults, plan *workflowPlan) corev1.Container {
	source := run.Spec.WorkflowSource
	env := []corev1.EnvVar{
		{Name: "TICKET_ID", Value: run.Spec.TicketID},
		{Name: "REPO_URL", Value: run.Spec.RepoURL},
		{Name: "JOB_NAME", Value: JobName(run)},
		{Name: "PROVIDER_BASE_URL", Value: providerBaseURL},
		{Name: "MAX_AGENT_VISITS", Value: fmt.Sprintf("%d", maxVisits)},
		// CRITERIA_HOME on the shared data PVC: the engine's run state
		// (including rotated per-scope accept-token files) must be readable
		// by the per-scope adapter pods, which mount /data but have no other
		// view into the runner's container filesystem.
		{Name: "CRITERIA_HOME", Value: "/data/criteria"},
		{Name: "WORKFLOW_URL", Value: source.URL},
	}
	if source.Ref != "" {
		env = append(env, corev1.EnvVar{Name: "WORKFLOW_REF", Value: source.Ref})
	}
	env = append(env, corev1.EnvVar{
		Name: "POD_IP",
		ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{
				FieldPath: "status.podIP",
			},
		},
	})
	if defaults.DebugEventsFile != "" {
		env = append(env, corev1.EnvVar{Name: "EVENTS_FILE", Value: defaults.DebugEventsFile})
	}
	if defaults.CastleAddr != "" {
		env = append(env, corev1.EnvVar{Name: "CASTLE_ADDR", Value: defaults.CastleAddr})
	}
	env = appendEnvDistinct(env, plan.runnerEnvs())

	return corev1.Container{
		Name:            "workflow-runner",
		Image:           image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		SecurityContext: restrictedContainerSecurityContext(),
		Command:         []string{"/bin/sh", "-c", sourceRunnerScript},
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
