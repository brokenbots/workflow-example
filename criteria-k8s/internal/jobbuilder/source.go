// Source-mode job construction (CRI-231, ADR-0005 D1/D2). When the run's
// spec carries a workflowSource, the runner does not execute a baked
// /workflows tree: it executes on a base/provided image, fetches the
// declared workflow source at run time, and applies it — mirroring the
// criteria-base entrypoint contract (CRI-230). The URL is content; the
// image is the process. Run provenance is recorded at admission on both
// sides of the path (CRI-232): the criteria binary's run-metadata
// publisher (CRI-225) records the resolver's origin under
// CRITERIA_HOME/runs/<run-id>/ keyed by the run id it mints inside the
// apply, and the runner records the k8s-side origin — the declared source
// (redacted), the ref pin, and the url+image process image — under
// CRITERIA_HOME/runs/<job-name>/, so provenance survives runs that fail
// before the binary admits anything and carries what only the k8s path
// knows.

package jobbuilder

import (
	"encoding/json"
	"fmt"
	"strings"

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
//   - record the workflow origin into run metadata at admission (CRI-232),
//     degrading to a warning on failure: provenance recording must not
//     fail the run it describes;
//   - apply WORKFLOW_URL, pinning WORKFLOW_REF when declared (CRI-226).
//
// Discovery publishing is host-only: per-scope adapter pods learn the
// runner's dial address from the shared host file (CRITERIA_REMOTE_HOST is
// never set by the operator for legacy engines), while the digest and the
// accept token arrive through the operator's env (event digest; and the
// accept token itself once the runner emits it, CRI-237 — engine-rotated
// token files under CRITERIA_HOME remain the delivery channel only for
// pre-eae0181 engines). Source mode never substitutes image-mode token
// placeholders into a fetched tree.
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

criteria_home="${CRITERIA_HOME:-${HOME:-/tmp}/.local/criteria}"
if ! mkdir -p "$criteria_home" 2>/dev/null || [ ! -w "$criteria_home" ]; then
    echo "CRITERIA_HOME $criteria_home is not a directory writable by the runner uid" >&2
    exit 70
fi
# The engine keeps run state (rotated accept-token files, run metadata, the
# workflow cache) under CRITERIA_HOME. Source-mode runs keep it
# container-local (CRI-237): the operator delivers adapter tokens on the
# wire, so no state has to be shared through the data volume. Export the
# verified path so criteria uses the same home even when the container env
# did not declare one.
export CRITERIA_HOME

# CRI-232: record the run's workflow origin into run metadata at admission,
# keyed by job name under the criteria state's runs/ layout. The criteria
# binary's CRI-225 publisher keys its own origin record (the resolver's
# source and ref) by the run id it mints inside the apply; this job-keyed
# record adds what only the k8s path knows — the declared origin (source
# pre-redacted by the operator, the ref pin) and the url+image process
# image — and survives runs that fail before the binary admits anything.
# WORKFLOW_ORIGIN_METADATA is operator-built JSON with the source already
# redacted; the raw URL is never echoed here.
if [ -n "${JOB_NAME:-}" ] && [ -n "${WORKFLOW_ORIGIN_METADATA:-}" ]; then
    origin_dir="$criteria_home/runs/${JOB_NAME}"
    if ! (umask 077 && mkdir -p "$origin_dir" && printf '%s' "$WORKFLOW_ORIGIN_METADATA" > "$origin_dir/run-metadata.json"); then
        echo "warning: could not record workflow origin metadata under $criteria_home/runs/$JOB_NAME" >&2
    fi
fi

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
	plan.applyHostAffinity(job.Spec.Template.Labels, &job.Spec.Template.Spec)
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

// workflowOriginMetadata is the k8s-path run metadata record the source-mode
// runner writes at admission (CRI-232): runs/<job>/run-metadata.json under
// CRITERIA_HOME, alongside the criteria binary's own CRI-225 record under
// runs/<run-id>/. Every recorded field is credential-free: the source is
// stored through RedactWorkflowSource (the criteria binary's
// redactSourceForLog rules, pinned by tests).
type workflowOriginMetadata struct {
	// Job is the k8s runner job name keying the record.
	Job string `json:"job"`
	// Source is the declared workflow source with userinfo credentials
	// redacted.
	Source string `json:"source"`
	// ResolvedRef is the declared workflow ref pin (CRI-226): the resolver
	// must match it exactly or refuse to run, so for pinned runs it is the
	// resolved ref. Unpinned runs omit it; the criteria binary's
	// runs/<run-id>/ record carries the resolver's ref there.
	ResolvedRef string `json:"resolved_ref,omitempty"`
	// Image is the url+image process image reference (ADR-0005 D6): what
	// only the k8s path knows. Empty in url-only mode.
	Image string `json:"image,omitempty"`
}

// RedactWorkflowSource mirrors the criteria binary's redactSourceForLog
// (workflow.RedactSource at the pinned criteria commit, CRI-225): it masks
// URL userinfo credentials in the authority component so a
// credential-bearing workflow source never reaches the recorded origin or
// the logs. Sources without a "://" separator (local paths, scp-style git
// forms) are returned unchanged; an "@" later in the path or query is not
// mistaken for a userinfo delimiter.
func RedactWorkflowSource(source string) string {
	idx := strings.Index(source, "://")
	if idx == -1 {
		return source
	}
	rest := source[idx+3:]
	// The userinfo delimiter can only appear in the authority component,
	// which ends at the first "/", "?" or "#"; an "@" later in the path
	// must not be mistaken for one. Within the authority, the first "@"
	// is the delimiter (userinfo cannot contain a literal "@").
	authority := rest
	if end := strings.IndexAny(rest, "/?#"); end != -1 {
		authority = rest[:end]
	}
	at := strings.Index(authority, "@")
	if at == -1 {
		return source
	}
	return source[:idx+3] + "redacted@" + rest[at+1:]
}

// workflowOriginRecord builds the JSON origin record the source-mode runner
// records into run metadata at admission (CRI-232). The source is recorded
// redacted; the image reference is recorded only in url+image mode, where
// spec.image is the process image. The record is passed to the runner
// verbatim (WORKFLOW_ORIGIN_METADATA) so the shell never touches the raw
// source or builds JSON itself.
func workflowOriginRecord(run *criteriav1.CriteriaRun, jobName string, source *criteriav1.RunWorkflowSource) string {
	rec := workflowOriginMetadata{
		Job:         jobName,
		Source:      RedactWorkflowSource(source.URL),
		ResolvedRef: source.Ref,
		Image:       run.Spec.Image,
	}
	b, _ := json.Marshal(rec)
	return string(b)
}

func sourceRunnerContainer(run *criteriav1.CriteriaRun, image, providerBaseURL string, maxVisits int, defaults Defaults, plan *workflowPlan) corev1.Container {
	source := run.Spec.WorkflowSource
	env := []corev1.EnvVar{
		{Name: "TICKET_ID", Value: run.Spec.TicketID},
		{Name: "REPO_URL", Value: run.Spec.RepoURL},
		{Name: "JOB_NAME", Value: JobName(run)},
		{Name: "PROVIDER_BASE_URL", Value: providerBaseURL},
		{Name: "MAX_AGENT_VISITS", Value: fmt.Sprintf("%d", maxVisits)},
		// CRITERIA_HOME container-local (CRI-237): source-mode runners build
		// the eae0181 engine, which carries the accept token on provision
		// events, so the operator delivers it to adapter pods on the wire
		// and no token file has to be readable from the shared volume. The
		// engine's run state (rotated token files, run metadata, the
		// workflow cache) lives inside the runner container and is cleaned
		// up with it. The path is fixed so the runner script's writability
		// check and the engine agree regardless of the image's own HOME.
		{Name: "CRITERIA_HOME", Value: "/tmp/criteria-home"},
		{Name: "WORKFLOW_URL", Value: source.URL},
	}
	if source.Ref != "" {
		env = append(env, corev1.EnvVar{Name: "WORKFLOW_REF", Value: source.Ref})
	}
	// CRI-232: the runner records the workflow origin into run metadata at
	// admission; the operator supplies the record (source already redacted,
	// url+image process image included) so the shell only writes it.
	env = append(env, corev1.EnvVar{
		Name:  "WORKFLOW_ORIGIN_METADATA",
		Value: workflowOriginRecord(run, JobName(run), source),
	})
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
