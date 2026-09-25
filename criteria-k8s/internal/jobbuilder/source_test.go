package jobbuilder_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/events"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/jobbuilder"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// urlRun is a source-mode run base: spec.workflowSource selects source mode.
func urlRun(name string, workflowSource *criteriav1.RunWorkflowSource, image string) *criteriav1.CriteriaRun {
	return &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "criteria-jobs"},
		Spec: criteriav1.CriteriaRunSpec{
			TicketID:       "CRI-231",
			RepoURL:        "https://github.com/brokenbots/workflow-example.git",
			Image:          image,
			WorkflowSource: workflowSource,
		},
	}
}

// The url-only source-mode run executes on the CRI-230 base image: the
// operator's CriteriaBaseImage default (never the baked-workflow image),
// with no repo-clone init container and the inline fetch/apply runner.
func TestSourceModeURLOnlyRunsOnBaseImage(t *testing.T) {
	run := urlRun("cri-231-url", &criteriav1.RunWorkflowSource{
		Type: "url",
		URL:  "git::https://github.com/brokenbots/workflow-example.git//linear_intake_v1",
	}, "")

	job := jobbuilder.BuildRunnerJob(run, jobbuilder.Defaults{
		Image:             "localhost:5000/linear-intake-remote:dev",
		CriteriaBaseImage: "localhost:5000/criteria-base:abc123",
		DataPVC:           "criteria-data",
	})

	runner := job.Spec.Template.Spec.Containers[0]
	assert.Equal(t, "workflow-runner", runner.Name)
	assert.Equal(t, "localhost:5000/criteria-base:abc123", runner.Image,
		"url-only source mode must run on the operator's criteria base image, not the baked workflow image")
	init := job.Spec.Template.Spec.InitContainers
	require.Len(t, init, 1, "source mode carries the per-ticket repo-clone init container (the repo_dir contract is mode-independent)")
	assert.Equal(t, "repo-clone", init[0].Name)
	assert.Equal(t, "localhost:5000/criteria-base:abc123", init[0].Image,
		"the clone runs on the same process image as the runner (base image: git, no gh)")

	// The run context and source env reach the runner; the runner script
	// bridges repo_dir into the workflow vars itself.
	assert.Equal(t, "git::https://github.com/brokenbots/workflow-example.git//linear_intake_v1", envValue(runner.Env, "WORKFLOW_URL"))
	assert.Equal(t, "", envValue(runner.Env, "WORKFLOW_REF"), "no ref declared: WORKFLOW_REF must be absent")
	assert.Equal(t, "", envValue(runner.Env, "REPO_DIR"))
	assert.Equal(t, "CRI-231", envValue(runner.Env, "TICKET_ID"))
	assert.Equal(t, "https://github.com/brokenbots/workflow-example.git", envValue(runner.Env, "REPO_URL"))
	assert.Equal(t, job.Name, envValue(runner.Env, "JOB_NAME"))
	assert.Equal(t, "/data/criteria-home/cri-231-url", envValue(runner.Env, "CRITERIA_HOME"),
		"source-mode engine run state must live on the shared data PVC, pod-scoped by job name, so CRI-125 step checkpoints survive a runner-container restart (CRI-303); adapter pods still take the accept token on the wire (CRI-237)")
	for _, e := range runner.Env {
		if e.Name == "POD_IP" {
			require.NotNil(t, e.ValueFrom)
			require.NotNil(t, e.ValueFrom.FieldRef)
			assert.Equal(t, "status.podIP", e.ValueFrom.FieldRef.FieldPath)
		}
	}

	// The runner is an inline script, not the baked runner.sh.
	require.Len(t, runner.Command, 3)
	assert.Equal(t, "/bin/sh", runner.Command[0])
	script := runner.Command[2]
	assert.NotContains(t, script, "/opt/criteria-pod-adapter/runner.sh",
		"source mode must not execute the image-mode runner entrypoint")
	assert.Contains(t, script, "apply \"$workflow_url\"",
		"the fetched workflow source is applied at run time")

	// Scheduling and ownership match the image-mode runner job.
	assert.Equal(t, "criteria-runner", job.Spec.Template.Spec.ServiceAccountName)
	assert.Equal(t, "runner", job.Spec.Template.Labels["criteria.brokenbots.dev/role"])
	assert.Equal(t, run.Namespace, job.Namespace)
	volNames := make(map[string]bool)
	for _, v := range job.Spec.Template.Spec.Volumes {
		volNames[v.Name] = true
	}
	assert.True(t, volNames["data"], "engine run state lives on the shared data PVC (CRITERIA_HOME)")
}

// KB-7 regression: a source-mode run with no repo-under-test (empty
// spec.repoUrl) previously could not start at all — the unconditional
// repo-clone init container fails closed without a workflow_github_token
// secret file AND REPO_URL. A repo-less run (self-contained scan, fan-out
// without a repo-under-test) must start with no repo-clone init container
// and reach its first step: the runner container carries the inline
// fetch/apply script and the data volume, with REPO_URL unset.
func TestSourceModeWithoutRepoUrlSkipsRepoClone(t *testing.T) {
	run := urlRun("kb-7-repo-less", &criteriav1.RunWorkflowSource{
		Type: "url",
		URL:  "git::https://github.com/brokenbots/workflow-example.git//self_scan_v1",
	}, "")
	run.Spec.RepoURL = "" // the run declares no repo-under-test

	job := jobbuilder.BuildRunnerJob(run, jobbuilder.Defaults{
		Image:             "localhost:5000/linear-intake-remote:dev",
		CriteriaBaseImage: "localhost:5000/criteria-base:abc123",
		DataPVC:           "criteria-data",
	})

	podSpec := job.Spec.Template.Spec
	assert.Empty(t, podSpec.InitContainers,
		"a source-mode run without a repo-under-test must start with no repo-clone init container (KB-7)")

	runner := podSpec.Containers[0]
	assert.Equal(t, "workflow-runner", runner.Name, "the run still reaches its first step: the runner is built")
	assert.Equal(t, "", envValue(runner.Env, "REPO_URL"), "REPO_URL must be absent for a repo-less run")
	assert.Equal(t, "git::https://github.com/brokenbots/workflow-example.git//self_scan_v1", envValue(runner.Env, "WORKFLOW_URL"))
	assert.Contains(t, runner.Command[2], "apply \"$workflow_url\"",
		"the inline fetch/apply runner script is intact")
	volNames := make(map[string]bool)
	for _, v := range podSpec.Volumes {
		volNames[v.Name] = true
	}
	assert.True(t, volNames["data"], "engine run state still lives on the shared data PVC")
}

// KB-7: a source-mode run WITH a repo-under-test keeps the per-ticket
// repo-clone init container — the linear_develop_v1 git steps consume the
// /data/intake/<TICKET>/repo the clone populates (the runner script bridges
// it into --var repo_dir). The clone script's fail-closed error signatures
// from the original reproduction are preserved verbatim.
func TestSourceModeWithRepoUrlKeepsRepoClone(t *testing.T) {
	run := urlRun("kb-7-repo-bearing", &criteriav1.RunWorkflowSource{
		Type: "url",
		URL:  "git::https://github.com/brokenbots/workflow-example.git//linear_develop_v1",
	}, "")

	job := jobbuilder.BuildRunnerJob(run, jobbuilder.Defaults{DataPVC: "criteria-data"})
	init := job.Spec.Template.Spec.InitContainers
	require.Len(t, init, 1, "a repo-bearing source-mode run keeps the per-ticket clone")
	assert.Equal(t, "repo-clone", init[0].Name)
	script := init[0].Command[2]
	assert.Contains(t, script, "WORKFLOW_GITHUB_TOKEN is required via /home/criteria/secrets/workflow_github_token",
		"the documented fail-closed token error signature is preserved")
	assert.Contains(t, script, "REPO_URL is required",
		"the documented fail-closed REPO_URL error signature is preserved")
	assert.Equal(t, "https://github.com/brokenbots/workflow-example.git", envValue(init[0].Env, "REPO_URL"))
}

// CRI-303: the source-mode engine's run state (CRI-125 step checkpoints, run
// metadata, the workflow cache) must live on the shared data PVC so a
// runner-container restart (OnFailure restart policy) resumes the in-flight
// run through checkpoint reattach instead of replaying fresh. The home is
// pod-scoped by job name so concurrent runs on the shared PVC never collide,
// mirroring the WORKFLOW_ORIGIN_METADATA runs/<JOB_NAME> layout.
func TestSourceModeCriteriaHomeOnDataPVC(t *testing.T) {
	run := urlRun("cri-303-home", &criteriav1.RunWorkflowSource{
		Type: "url",
		URL:  "git::https://example.com/wf.git",
	}, "")
	job := jobbuilder.BuildRunnerJob(run, jobbuilder.Defaults{DataPVC: "criteria-data"})
	runner := job.Spec.Template.Spec.Containers[0]

	home := envValue(runner.Env, "CRITERIA_HOME")
	assert.Equal(t, "/data/criteria-home/cri-303-home", home,
		"CRITERIA_HOME must be the per-run data-PVC directory keyed by the runner job name (CRI-303)")
	assert.Contains(t, home, "/data/criteria-home/"+envValue(runner.Env, "JOB_NAME"),
		"the home must be scoped by the same JOB_NAME the runner publishes its state under")

	// The home resolves through the shared data mount: the runner carries
	// the run's data PVC at /data, so the pod-scoped home is on the PVC and
	// outlives the container writable layer.
	mounts := make(map[string]corev1.VolumeMount)
	for _, m := range runner.VolumeMounts {
		mounts[m.MountPath] = m
	}
	dataMount, ok := mounts["/data"]
	require.True(t, ok, "the runner must mount the shared data PVC at /data: %v", runner.VolumeMounts)
	assert.Equal(t, "data", dataMount.Name)

	volumes := make(map[string]corev1.Volume)
	for _, v := range job.Spec.Template.Spec.Volumes {
		volumes[v.Name] = v
	}
	dataVol, ok := volumes[dataMount.Name]
	require.True(t, ok, "the mounted data volume must exist in the pod volume list")
	require.NotNil(t, dataVol.PersistentVolumeClaim,
		"the engine run state must ride a PVC, not a container-local emptyDir")
	assert.Equal(t, "criteria-data", dataVol.PersistentVolumeClaim.ClaimName,
		"the checkpoint-bearing home must land on the run's data PVC")

	// A second run on the same PVC gets its own pod-scoped directory.
	other := urlRun("cri-303-other", &criteriav1.RunWorkflowSource{Type: "url", URL: "git::https://example.com/wf.git"}, "")
	otherJob := jobbuilder.BuildRunnerJob(other, jobbuilder.Defaults{DataPVC: "criteria-data"})
	assert.Equal(t, "/data/criteria-home/cri-303-other",
		envValue(otherJob.Spec.Template.Spec.Containers[0].Env, "CRITERIA_HOME"),
		"concurrent runs on the shared PVC must not share a CRITERIA_HOME")
}

// CRI-237 preserved on the relocated path (CRI-303): a source-mode run's
// adapter pods keep taking the accept token on the wire and read nothing
// under the moved CRITERIA_HOME — the token files the engine keeps there
// are engine-internal legacy delivery for image-mode engines only.
func TestSourceModeAdapterPodsNeverReadMovedCriteriaHome(t *testing.T) {
	run := urlRun("cri-303-wire", &criteriav1.RunWorkflowSource{
		Type: "url",
		URL:  "git::https://example.com/wf.git",
	}, "")
	scope := events.LifecycleEvent{
		Event:       events.EventProvisionWanted,
		RunID:       "CRI-303",
		ScopeID:     "root",
		AdapterName: "shell",
		Digest:      "deadbeef",
		ShimAddress: "10.0.0.5:7778",
		AcceptToken: "accept-rotate-1",
	}

	pod := jobbuilder.BuildPerScopeAdapterPod(run, jobbuilder.Defaults{DataPVC: "criteria-data"}, scope, "10.0.0.10")
	require.NotNil(t, pod)
	container := pod.Spec.Containers[0]

	envNames := make(map[string]bool)
	for _, e := range container.Env {
		envNames[e.Name] = true
		assert.NotContains(t, e.Value, "/data/criteria-home",
			"adapter env %s must not reference the relocated CRITERIA_HOME: adapter tokens ride the wire (CRI-237)", e.Name)
	}
	assert.True(t, envNames["CRITERIA_REMOTE_TOKEN"], "the accept token must be delivered on the wire")
	assert.False(t, envNames["CRITERIA_REMOTE_TOKEN_FILE"], "no token-file surface on a wire-delivered adapter pod")
}

// CRI-231 requirement 3: in url+image source mode the URL is injected
// exactly as in url-only mode and the process runs in the provided image.
func TestSourceModeURLImageRunsProcessInProvidedImage(t *testing.T) {
	wfURL := "git::https://github.com/brokenbots/workflow-example.git//linear_intake_v1"

	urlOnly := urlRun("cri-231-url-only", &criteriav1.RunWorkflowSource{Type: "url", URL: wfURL}, "")
	urlImage := urlRun("cri-231-url-image", &criteriav1.RunWorkflowSource{Type: "url", URL: wfURL}, "localhost:5000/linear-intake-remote:dev")

	onlyJob := jobbuilder.BuildRunnerJob(urlOnly, jobbuilder.Defaults{
		CriteriaBaseImage: "localhost:5000/criteria-base:abc123",
	})
	imageJob := jobbuilder.BuildRunnerJob(urlImage, jobbuilder.Defaults{
		CriteriaBaseImage: "localhost:5000/criteria-base:abc123",
	})

	assert.Equal(t, "localhost:5000/linear-intake-remote:dev", imageJob.Spec.Template.Spec.Containers[0].Image,
		"spec.image is the url+image process image; the base image is not consulted")
	assert.Equal(t,
		envValue(onlyJob.Spec.Template.Spec.Containers[0].Env, "WORKFLOW_URL"),
		envValue(imageJob.Spec.Template.Spec.Containers[0].Env, "WORKFLOW_URL"),
		"the URL must be injected identically in url-only and url+image mode")
	assert.Equal(t,
		onlyJob.Spec.Template.Spec.Containers[0].Command[2],
		imageJob.Spec.Template.Spec.Containers[0].Command[2],
		"the runner contract is identical; only the image differs")
}

// Without spec.image or the operator base image, source mode falls back to
// the built-in criteria-base default (CRI-230).
func TestSourceModeImageFallsBackToBaseDefault(t *testing.T) {
	run := urlRun("cri-231-fallback", &criteriav1.RunWorkflowSource{Type: "url", URL: "git::https://example.com/wf.git"}, "")
	job := jobbuilder.BuildRunnerJob(run, jobbuilder.Defaults{})
	assert.Equal(t, "localhost:5000/criteria-base:dev", job.Spec.Template.Spec.Containers[0].Image)
}

// The operator's ref pin reaches the runner as WORKFLOW_REF, enforced
// fail-closed by the criteria binary (CRI-226).
func TestSourceModeRefPinStamped(t *testing.T) {
	run := urlRun("cri-231-ref", &criteriav1.RunWorkflowSource{
		Type: "url",
		URL:  "git::https://example.com/wf.git",
		Ref:  "28777aacc3cfbe85005ddb27f548116e692c0eb4",
	}, "")
	job := jobbuilder.BuildRunnerJob(run, jobbuilder.Defaults{})
	assert.Equal(t, "28777aacc3cfbe85005ddb27f548116e692c0eb4", envValue(job.Spec.Template.Spec.Containers[0].Env, "WORKFLOW_REF"))
}

// A nil spec.workflowSource keeps the image-mode baked-tree path byte for
// byte: repo-clone init container plus the runner.sh entrypoint, and
// spec.image/operator default image resolution untouched.
func TestImageModeUnchangedBySourceMode(t *testing.T) {
	run := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{Name: "cri-231-image"},
		Spec: criteriav1.CriteriaRunSpec{
			TicketID: "CRI-231",
			RepoURL:  "https://github.com/brokenbots/workflow-example.git",
		},
	}

	job := jobbuilder.BuildRunnerJob(run, jobbuilder.Defaults{
		Image:             "localhost:5000/linear-intake-remote:dev",
		CriteriaBaseImage: "localhost:5000/criteria-base:abc123",
	})
	runner := job.Spec.Template.Spec.Containers[0]
	require.Len(t, job.Spec.Template.Spec.InitContainers, 1)
	assert.Equal(t, "repo-clone", job.Spec.Template.Spec.InitContainers[0].Name)
	assert.Equal(t, "localhost:5000/linear-intake-remote:dev", runner.Image,
		"image mode must not pick up the criteria base image")
	assert.Equal(t, []string{"/opt/criteria-pod-adapter/runner.sh"}, runner.Command,
		"image mode keeps the baked runner entrypoint")
	assert.Equal(t, "/data/intake/CRI-231/repo", envValue(runner.Env, "REPO_DIR"))
	assert.Equal(t, "", envValue(runner.Env, "WORKFLOW_URL"))
}

// The workflow object's declared volumes/secrets/env still render in source
// mode: url+image runs of a workflow-library object carry the same storage
// surface as their image-mode counterparts (CRI-222 plan machinery).
func TestSourceModeRendersWorkflowPlan(t *testing.T) {
	run := urlRun("cri-231-plan", &criteriav1.RunWorkflowSource{Type: "url", URL: "git::https://example.com/wf.git"}, "")
	run.Spec.Workflow = &criteriav1.RunWorkflow{
		Name:      "linear-intake-url",
		Type:      "url",
		Namespace: "wf-ns",
		Env:       map[string]string{"CRITERIA_RUN_DIR_ROOT": "/data/.criteria/runs"},
		Volumes: []criteriav1.RunWorkflowVolume{{
			Name: "shared", Kind: "pvc", Claim: "wf-data", MountPath: "/mnt/wf",
			Env: map[string]string{"WF_VOLUME_ENV": "1"},
		}},
	}

	job := jobbuilder.BuildRunnerJob(run, jobbuilder.Defaults{})
	assert.Equal(t, "wf-ns", job.Namespace, "children land in the workflow's declared namespace")
	runner := job.Spec.Template.Spec.Containers[0]
	assert.Equal(t, "1", envValue(runner.Env, "WF_VOLUME_ENV"))
	volNames := make(map[string]bool)
	for _, v := range job.Spec.Template.Spec.Volumes {
		volNames[v.Name] = true
	}
	assert.True(t, volNames["shared"], "declared volumes render in source mode")
}

// --- behavioral contract of the generated source-mode runner script --------

// execResult is the exit status and combined output of a helper run.
type execResult struct {
	code   int
	output string
}

// execCommand runs a command with the given environment and directory,
// returning its exit status and combined output.
func execCommand(t *testing.T, args, env []string, dir string) execResult {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = env
	cmd.Dir = dir
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("run %v: %v", args, err)
		}
		code = exitErr.ExitCode()
	}
	return execResult{code: code, output: buf.String()}
}

func readStubLog(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		// The stub never ran: the log is empty by definition.
		return ""
	}
	require.NoError(t, err)
	return string(b)
}

// writeCriteriaStub writes a stub criteria binary recording its argv, the
// effective CRITERIA_HOME, and the discovery host file, then exiting with
// STUB_EXIT (0 unless set).
func writeCriteriaStub(t *testing.T, dir string) string {
	t.Helper()
	stub := filepath.Join(dir, "stub-criteria")
	script := fmt.Sprintf(`#!/usr/bin/env bash
{
    printf 'argv:'
    for a in "$@"; do
        printf ' [%%s]' "$a"
    done
    printf '\n'
    printf 'criteria_home=%%s\n' "${CRITERIA_HOME-}"
    if [ -n "${JOB_NAME:-}" ]; then
        host_file="${CRITERIA_RUN_DIR_ROOT:-/data/.criteria/runs}/${JOB_NAME}/host"
        if [ -r "$host_file" ]; then
            printf 'host=%%s\n' "$(cat "$host_file")"
        fi
    fi
} >>"$STUB_LOG"
exit "${STUB_EXIT:-0}"
`)
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return stub
}

// runSourceRunnerScript runs a generated runner script with a stub criteria
// binary and the given extra env (CRITERIA_HOME overrides the default temp
// home), returning the exit status, the script's combined output, and the
// stub's log.
func runSourceRunnerScript(t *testing.T, script string, env map[string]string) (code int, out string, stubLog string) {
	t.Helper()
	dir := t.TempDir()
	stub := writeCriteriaStub(t, dir)
	logPath := filepath.Join(dir, "stub.log")
	cmdEnv := []string{
		"STUB_LOG=" + logPath,
		"CRITERIA_BIN=" + stub,
		"CRITERIA_HOME=" + filepath.Join(dir, "home"),
	}
	for k, v := range env {
		if k == "CRITERIA_HOME" {
			cmdEnv[2] = "CRITERIA_HOME=" + v
			continue
		}
		cmdEnv = append(cmdEnv, k+"="+v)
	}
	proc := execCommand(t, []string{"/bin/sh", "-c", script}, cmdEnv, dir)
	return proc.code, proc.output, readStubLog(t, logPath)
}

func TestSourceRunnerScriptBehavior(t *testing.T) {
	job := jobbuilder.BuildRunnerJob(urlRun("cri-231-script", &criteriav1.RunWorkflowSource{
		Type: "url",
		URL:  "git::https://example.com/org/workflows.git?ref=main",
	}, ""), jobbuilder.Defaults{DataPVC: "criteria-data"})
	runner := job.Spec.Template.Spec.Containers[0]
	require.Len(t, runner.Command, 3)
	script := runner.Command[2]

	t.Run("applies the workflow source with pin, server, events, and host file", func(t *testing.T) {
		dir := t.TempDir()
		stub := writeCriteriaStub(t, dir)
		runDir := filepath.Join(dir, "runs", "cri-231")
		proc := execCommand(t, []string{"/bin/sh", "-c", script}, []string{
			"STUB_LOG=" + filepath.Join(dir, "stub.log"),
			"CRITERIA_BIN=" + stub,
			"CRITERIA_HOME=" + filepath.Join(dir, "home"),
			"WORKFLOW_URL=git::https://example.com/org/workflows.git?ref=main",
			"WORKFLOW_REF=28777aacc3cfbe85005ddb27f548116e692c0eb4",
			"CASTLE_ADDR=http://castle:9443",
			"EVENTS_FILE=/tmp/events.ndjson",
			"JOB_NAME=cri-231",
			"POD_IP=10.42.0.5",
			"CRITERIA_RUN_DIR_ROOT=" + filepath.Join(dir, "runs"),
			// The linear routes' declared secrets render this pair set via
			// secretVarBindings; the script loops it into --var args.
			"CRITERIA_SECRET_VARS=linear_api_key=file:/home/criteria/linear-secrets/linear_api_key\n" +
				"reviewer_github_token=file:/home/criteria/secrets/reviewer_github_token\n" +
				"workflow_github_token=file:/home/criteria/secrets/workflow_github_token",
		}, dir)
		require.Equal(t, 0, proc.code, "script must succeed: %s", proc.output)
		log := readStubLog(t, filepath.Join(dir, "stub.log"))
		assert.Contains(t, log, "argv: [apply] [git::https://example.com/org/workflows.git?ref=main] "+
			"[--var] [ticket_id=] [--var] [repo_dir=/data/intake//repo] "+
			"[--var] [intake_root=/data/intake] [--var] [triage_root=/data/triage] "+
			"[--var] [linear_review_state=In Review] "+
			"[--var] [linear_work_state=In Progress] [--var] [linear_done_state=Done] "+
			"[--var] [base_branch=main] [--var] [ci_gate_cmd=] [--var] [provider_base_url=] "+
			"[--workflow-ref] [28777aacc3cfbe85005ddb27f548116e692c0eb4] "+
			"[--server] [http://castle:9443] [--events-file] [/tmp/events.ndjson]",
			"apply must receive the URL, the bridged runtime vars, the pin, and the forwarded flags in order: %s", log)
		assert.Contains(t, log, "[--var] [linear_api_key=file:/home/criteria/linear-secrets/linear_api_key]",
			"secret variables must ride as file: OriginRefs (D69), never as raw values: %s", log)
		assert.Contains(t, log, "host=10.42.0.5:7778",
			"the runner must publish its routable dial address for per-scope pods")
		// The discovery dir is removed on exit so a later run cannot reuse it.
		_, err := os.Stat(runDir)
		assert.True(t, os.IsNotExist(err), "discovery dir must be cleaned up on exit")
	})

	t.Run("no castle or events config keeps the run local", func(t *testing.T) {
		code, out, _ := runSourceRunnerScript(t, script, map[string]string{
			"WORKFLOW_URL": "git::https://example.com/wf.git",
		})
		require.Equal(t, 0, code, out)
	})

	t.Run("undeclared workflow source fails closed without invoking criteria", func(t *testing.T) {
		for _, bad := range []string{"", "   ", "\t"} {
			code, out, _ := runSourceRunnerScript(t, script, map[string]string{"WORKFLOW_URL": bad})
			assert.Equal(t, 64, code, "empty WORKFLOW_URL must fail closed with exit 64: %q", out)
			assert.Contains(t, out, "WORKFLOW_URL is not set")
			assert.NotContains(t, out, "argv:", "criteria must not be invoked")
		}
	})

	t.Run("missing criteria binary fails fast with an explicit error", func(t *testing.T) {
		dir := t.TempDir()
		args := []string{"/bin/sh", "-c", script}
		cmdEnv := []string{
			"CRITERIA_BIN=" + filepath.Join(dir, "absent-criteria"),
			"CRITERIA_HOME=" + filepath.Join(dir, "home"),
			"WORKFLOW_URL=git::https://example.com/wf.git",
		}
		proc := execCommand(t, args, cmdEnv, dir)
		assert.Equal(t, 69, proc.code, "a missing criteria binary must fail fast")
		assert.Contains(t, proc.output, "not found or not executable",
			"the failure must name the missing binary explicitly")
		assert.NotContains(t, proc.output, "argv:", "criteria must not be invoked")
	})

	t.Run("unusable CRITERIA_HOME fails closed before any fetch", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "not-a-dir"), []byte("x"), 0o644))
		code, out, _ := runSourceRunnerScript(t, script, map[string]string{
			"WORKFLOW_URL":  "git::https://example.com/wf.git",
			"CRITERIA_HOME": filepath.Join(dir, "not-a-dir/sub"),
		})
		assert.Equal(t, 70, code, "an unusable CRITERIA_HOME must fail closed: %s", out)
		assert.Contains(t, out, "not a directory writable")
		assert.NotContains(t, out, "argv:", "criteria must not be invoked")
	})

	t.Run("creatable CRITERIA_HOME is created and used", func(t *testing.T) {
		dir := t.TempDir()
		home := filepath.Join(dir, "deep", "home")
		code, out, stubLog := runSourceRunnerScript(t, script, map[string]string{
			"WORKFLOW_URL":  "git::https://example.com/wf.git",
			"CRITERIA_HOME": home,
		})
		require.Equal(t, 0, code, out)
		assert.Contains(t, stubLog, "criteria_home="+home,
			"the verified home must be exported to the criteria process")
	})

	t.Run("criteria exit status propagates", func(t *testing.T) {
		dir := t.TempDir()
		stub := writeCriteriaStub(t, dir)
		proc := execCommand(t, []string{"/bin/sh", "-c", script}, []string{
			"STUB_LOG=" + filepath.Join(dir, "stub.log"),
			"CRITERIA_BIN=" + stub,
			"CRITERIA_HOME=" + filepath.Join(dir, "home"),
			"WORKFLOW_URL=git::https://example.com/wf.git",
			"STUB_EXIT=42",
		}, dir)
		assert.Equal(t, 42, proc.code, "the criteria exit status must propagate")
	})

	t.Run("whitespace-only ref is treated as absent", func(t *testing.T) {
		dir := t.TempDir()
		stub := writeCriteriaStub(t, dir)
		proc := execCommand(t, []string{"/bin/sh", "-c", script}, []string{
			"STUB_LOG=" + filepath.Join(dir, "stub.log"),
			"CRITERIA_BIN=" + stub,
			"CRITERIA_HOME=" + filepath.Join(dir, "home"),
			"WORKFLOW_URL=git::https://example.com/wf.git",
			"WORKFLOW_REF=   ",
		}, dir)
		require.Equal(t, 0, proc.code, proc.output)
		log := readStubLog(t, filepath.Join(dir, "stub.log"))
		assert.NotContains(t, log, "--workflow-ref", "a whitespace ref must not pin the run")
	})

	t.Run("no JOB_NAME or POD_IP skips the host publish", func(t *testing.T) {
		dir := t.TempDir()
		stub := writeCriteriaStub(t, dir)
		proc := execCommand(t, []string{"/bin/sh", "-c", script}, []string{
			"STUB_LOG=" + filepath.Join(dir, "stub.log"),
			"CRITERIA_BIN=" + stub,
			"CRITERIA_HOME=" + filepath.Join(dir, "home"),
			"WORKFLOW_URL=git::https://example.com/wf.git",
			"CRITERIA_RUN_DIR_ROOT=" + filepath.Join(dir, "runs"),
		}, dir)
		require.Equal(t, 0, proc.code, proc.output)
		_, err := os.Stat(filepath.Join(dir, "runs"))
		assert.True(t, os.IsNotExist(err), "no discovery dir without JOB_NAME/POD_IP")
	})
}

// --- CRI-232: workflow origin provenance on the k8s runner path -------------

// TestRedactWorkflowSourceMatchesCriteriaPublisher pins the k8s-side mirror
// of the criteria binary's redactSourceForLog (workflow.RedactSource at the
// pinned criteria commit, CRI-225): the recorded origin stores URL userinfo
// credentials redacted, never raw. The cases are ported from the criteria
// repo's redact_test.go so both sides redact identically.
func TestRedactWorkflowSourceMatchesCriteriaPublisher(t *testing.T) {
	tests := []struct {
		name   string
		source string
		want   string
	}{
		{name: "archive userinfo", source: "https://user:pass@host/x.tar.gz", want: "https://redacted@host/x.tar.gz"},
		{name: "user only", source: "https://user@host/x.tar.gz", want: "https://redacted@host/x.tar.gz"},
		{name: "no userinfo", source: "https://host/x.tar.gz", want: "https://host/x.tar.gz"},
		{name: "git scheme", source: "https://user:token@github.com/org/repo.git?ref=v1", want: "https://redacted@github.com/org/repo.git?ref=v1"},
		{name: "git force prefix keeps scheme redaction", source: "git::https://user:token@github.com/org/repo.git", want: "git::https://redacted@github.com/org/repo.git"},
		{name: "at in path is not userinfo", source: "https://host/a@b/c", want: "https://host/a@b/c"},
		{name: "userinfo kept out of query", source: "https://host/a?x=u@ser", want: "https://host/a?x=u@ser"},
		{name: "userinfo kept out of fragment", source: "https://host/a#x=u@ser", want: "https://host/a#x=u@ser"},
		{name: "local path unchanged", source: "./local/workflow", want: "./local/workflow"},
		{name: "scp-style git form unchanged", source: "git@github.com:org/repo.git", want: "git@github.com:org/repo.git"},
		{name: "malformed scheme only", source: "://", want: "://"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, jobbuilder.RedactWorkflowSource(tt.source))
		})
	}
}

// TestSourceModePublishesOriginMetadataEnv pins the origin record the
// operator stamps onto the runner env (CRI-232): the declared source
// (redacted when it carries credentials), the ref pin, and the url+image
// process image. The record is the exact JSON the runner writes into
// run-metadata.json at admission.
func TestSourceModePublishesOriginMetadataEnv(t *testing.T) {
	const wfURL = "git::https://github.com/brokenbots/workflow-example.git//linear_intake_v1"

	t.Run("url-only records redacted source and job, no image", func(t *testing.T) {
		run := urlRun("cri-232-url-only", &criteriav1.RunWorkflowSource{Type: "url", URL: wfURL}, "")
		job := jobbuilder.BuildRunnerJob(run, jobbuilder.Defaults{CriteriaBaseImage: "localhost:5000/criteria-base:abc123"})
		env := job.Spec.Template.Spec.Containers[0].Env
		assert.Equal(t, `{"job":"cri-232-url-only","source":"`+wfURL+`"}`,
			envValue(env, "WORKFLOW_ORIGIN_METADATA"),
			"url-only mode records the declared origin without an image reference")
	})

	t.Run("url+image additionally records the process image", func(t *testing.T) {
		run := urlRun("cri-232-url-image", &criteriav1.RunWorkflowSource{
			Type: "url",
			URL:  wfURL,
			Ref:  "28777aacc3cfbe85005ddb27f548116e692c0eb4",
		}, "registry.example.com/team/process:1.2.3")
		job := jobbuilder.BuildRunnerJob(run, jobbuilder.Defaults{})
		env := job.Spec.Template.Spec.Containers[0].Env
		assert.Equal(t,
			`{"job":"cri-232-url-image","source":"`+wfURL+`","resolved_ref":"28777aacc3cfbe85005ddb27f548116e692c0eb4","image":"registry.example.com/team/process:1.2.3"}`,
			envValue(env, "WORKFLOW_ORIGIN_METADATA"),
			"url+image mode records the ref pin and the process image alongside the source")
	})

	t.Run("credential-bearing source is recorded redacted", func(t *testing.T) {
		run := urlRun("cri-232-redacted", &criteriav1.RunWorkflowSource{
			Type: "url",
			URL:  "https://ci-bot:s3cret@example.com/team/workflows.tar.gz",
		}, "")
		job := jobbuilder.BuildRunnerJob(run, jobbuilder.Defaults{})
		env := job.Spec.Template.Spec.Containers[0].Env
		record := envValue(env, "WORKFLOW_ORIGIN_METADATA")
		assert.Contains(t, record, "https://redacted@example.com/team/workflows.tar.gz",
			"the recorded origin must be redacted per the criteria publisher's rules")
		assert.NotContains(t, record, "ci-bot", "no userinfo may reach the recorded origin")
		assert.NotContains(t, record, "s3cret", "no credential material may reach the recorded origin")
	})
}

// TestImageModeUnchangedByOriginMetadata: the image-mode runner keeps the
// baked-tree contract with no origin-metadata recording — origin provenance
// is source-mode only (CRI-232).
func TestImageModeUnchangedByOriginMetadata(t *testing.T) {
	run := &criteriav1.CriteriaRun{
		ObjectMeta: metav1.ObjectMeta{Name: "cri-232-image"},
		Spec: criteriav1.CriteriaRunSpec{
			TicketID: "CRI-232",
			RepoURL:  "https://github.com/brokenbots/workflow-example.git",
		},
	}
	job := jobbuilder.BuildRunnerJob(run, jobbuilder.Defaults{
		Image: "localhost:5000/linear-intake-remote:dev",
	})
	runner := job.Spec.Template.Spec.Containers[0]
	assert.Equal(t, "", envValue(runner.Env, "WORKFLOW_ORIGIN_METADATA"),
		"image mode must not carry the origin record: its workflow is baked, not fetched")
	assert.Equal(t, []string{"/opt/criteria-pod-adapter/runner.sh"}, runner.Command,
		"image mode keeps the baked runner entrypoint")
}

// TestSourceRunnerOriginMetadataBehavior pins the runner's origin-recording
// contract (CRI-232): the operator-supplied record lands at
// CRITERIA_HOME/runs/<job>/run-metadata.json at admission — before the
// criteria invocation, surviving a failing run — with restrictive
// permissions, and no credential-bearing source reaches the script's output
// (the pod log) or the recorded file.
func TestSourceRunnerOriginMetadataBehavior(t *testing.T) {
	job := jobbuilder.BuildRunnerJob(urlRun("cri-232-script", &criteriav1.RunWorkflowSource{
		Type: "url",
		URL:  "git::https://ci-bot:s3cret@example.com/org/workflows.git//linear_intake_v1",
		Ref:  "28777aacc3cfbe85005ddb27f548116e692c0eb4",
	}, "registry.example.com/team/process:1.2.3"), jobbuilder.Defaults{DataPVC: "criteria-data"})
	runner := job.Spec.Template.Spec.Containers[0]
	script := runner.Command[2]
	record := envValue(runner.Env, "WORKFLOW_ORIGIN_METADATA")
	require.NotEmpty(t, record)
	require.NotContains(t, record, "s3cret", "the operator-supplied record must be pre-redacted")

	baseEnv := func() map[string]string {
		return map[string]string{
			"WORKFLOW_URL":             "git::https://ci-bot:s3cret@example.com/org/workflows.git//linear_intake_v1",
			"WORKFLOW_REF":             "28777aacc3cfbe85005ddb27f548116e692c0eb4",
			"WORKFLOW_ORIGIN_METADATA": record,
			"JOB_NAME":                 "cri-232-script",
			"POD_IP":                   "10.42.0.9",
			"CRITERIA_RUN_DIR_ROOT":    "/tmp/discovery-irrelevant",
		}
	}

	t.Run("records the origin into run metadata at admission", func(t *testing.T) {
		env := baseEnv()
		dir := t.TempDir()
		home := filepath.Join(dir, "home")
		env["CRITERIA_HOME"] = home
		code, out, stubLog := runSourceRunnerScript(t, script, env)
		require.Equal(t, 0, code, out)
		assert.Contains(t, stubLog, "argv:", "the criteria invocation must still happen")

		b, err := os.ReadFile(originRecordPath(home, "cri-232-script"))
		require.NoError(t, err, "the origin record must be written at admission")
		assert.Equal(t, record, string(b), "the record lands verbatim, operator-built JSON")

		// Restrictive state-file permissions, mirroring the criteria
		// binary's CRI-225 record (0600 file, 0700 dir).
		info, err := os.Stat(originRecordPath(home, "cri-232-script"))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "the origin record is a state file")
		dirInfo, err := os.Stat(filepath.Dir(originRecordPath(home, "cri-232-script")))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o700), dirInfo.Mode().Perm(), "the record directory is state-dir private")
	})

	t.Run("record content is the redacted origin with the image reference", func(t *testing.T) {
		var parsed struct {
			Job         string `json:"job"`
			Source      string `json:"source"`
			ResolvedRef string `json:"resolved_ref"`
			Image       string `json:"image"`
		}
		require.NoError(t, json.Unmarshal([]byte(record), &parsed))
		assert.Equal(t, "cri-232-script", parsed.Job)
		assert.Equal(t, "git::https://redacted@example.com/org/workflows.git//linear_intake_v1", parsed.Source)
		assert.Equal(t, "28777aacc3cfbe85005ddb27f548116e692c0eb4", parsed.ResolvedRef)
		assert.Equal(t, "registry.example.com/team/process:1.2.3", parsed.Image)
	})

	t.Run("no unredacted source reaches the pod log", func(t *testing.T) {
		code, out, _ := runSourceRunnerScript(t, script, baseEnv())
		require.Equal(t, 0, code, out)
		assert.NotContains(t, out, "s3cret", "the credential must never appear in the script's output")
		assert.NotContains(t, out, "ci-bot", "the userinfo must never appear in the script's output")
	})

	t.Run("origin record survives a failing run", func(t *testing.T) {
		env := baseEnv()
		dir := t.TempDir()
		home := filepath.Join(dir, "home")
		env["CRITERIA_HOME"] = home
		env["STUB_EXIT"] = "42"
		code, out, _ := runSourceRunnerScript(t, script, env)
		assert.Equal(t, 42, code, "the criteria exit status still propagates: %s", out)
		b, err := os.ReadFile(originRecordPath(home, "cri-232-script"))
		require.NoError(t, err, "provenance must survive a run that fails at admission or after")
		assert.Equal(t, record, string(b))
	})

	t.Run("recording failure degrades to a warning and does not fail the run", func(t *testing.T) {
		env := baseEnv()
		dir := t.TempDir()
		home := filepath.Join(dir, "home")
		env["CRITERIA_HOME"] = home
		// A pre-existing file where the record directory must be created
		// makes the record write fail without touching anything else.
		require.NoError(t, os.MkdirAll(filepath.Join(home, "runs"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(home, "runs", "cri-232-script"), []byte("x"), 0o644))
		code, out, stubLog := runSourceRunnerScript(t, script, env)
		assert.Equal(t, 0, code, "a recording failure must not fail the run: %s", out)
		assert.Contains(t, out, "warning: could not record workflow origin metadata")
		assert.Contains(t, stubLog, "argv:", "the criteria invocation must still happen")
	})

	t.Run("no JOB_NAME or no record skips the origin record", func(t *testing.T) {
		for name, mutate := range map[string]func(map[string]string){
			"no JOB_NAME": func(env map[string]string) { delete(env, "JOB_NAME") },
			"no record":   func(env map[string]string) { delete(env, "WORKFLOW_ORIGIN_METADATA") },
		} {
			t.Run(name, func(t *testing.T) {
				env := baseEnv()
				mutate(env)
				dir := t.TempDir()
				home := filepath.Join(dir, "home")
				env["CRITERIA_HOME"] = home
				code, out, _ := runSourceRunnerScript(t, script, env)
				require.Equal(t, 0, code, out)
				_, err := os.Stat(filepath.Join(home, "runs"))
				assert.True(t, os.IsNotExist(err), "no run metadata without the record inputs")
			})
		}
	})
}

// The kanboard routes stamp a workflow object whose secrets declare
// KANBOARD_URL/kanboard_url and KANBOARD_APP_TOKEN/kanboard_app_token; the
// source runner must derive those file: OriginRef pairs (this is the KB-1
// deadlock fix — the hardcoded linear-only list silently dropped them, so
// every kanboard triage run died at fetch_ticket's env guard and refired in
// a loop). The linear triples must keep arriving too.
func TestSourceRunnerSecretVarsDerivedFromWorkflowSecrets(t *testing.T) {
	run := urlRun("kb-1-1790222552", &criteriav1.RunWorkflowSource{
		Type: "url",
		URL:  "git::https://github.com/brokenbots/workflow-example.git//kanboard_triage_v1?ref=e2dd01dcf44c8d2cf197d49461748dfcb0055f91",
	}, "")
	run.Spec.Workflow = &criteriav1.RunWorkflow{
		Name:      "kanboard-triage-url",
		Type:      "url",
		Namespace: "criteria-jobs",
		Secrets: []criteriav1.RunWorkflowSecret{
			{Name: "github-tokens", SecretProviderClass: "copilot-spc", MountPath: "/home/criteria/secrets",
				Env: map[string]string{"WORKFLOW_GITHUB_TOKEN": "workflow_github_token", "REVIEWER_GITHUB_TOKEN": "reviewer_github_token"}},
			{Name: "kanboard-secrets", SecretProviderClass: "kanboard-spc", MountPath: "/home/criteria/kanboard-secrets",
				Env: map[string]string{"KANBOARD_APP_TOKEN": "kanboard_app_token", "KANBOARD_URL": "kanboard_url"}},
		},
	}
	job := jobbuilder.BuildRunnerJob(run, jobbuilder.Defaults{DataPVC: "criteria-data"})
	runner := job.Spec.Template.Spec.Containers[0]
	var cvs string
	for _, e := range runner.Env {
		if e.Name == "CRITERIA_SECRET_VARS" {
			cvs = e.Value
		}
	}
	require.NotEmpty(t, cvs, "source runner must carry the derived secret-var pair set")
	for _, want := range []string{
		"kanboard_app_token=file:/home/criteria/kanboard-secrets/kanboard_app_token",
		"kanboard_url=file:/home/criteria/kanboard-secrets/kanboard_url",
		"workflow_github_token=file:/home/criteria/secrets/workflow_github_token",
		"reviewer_github_token=file:/home/criteria/secrets/reviewer_github_token",
	} {
		assert.Contains(t, cvs, want, "derived pair set must cover every declared secret key")
	}
	// And the script turns the pair set into --var args ahead of --output.
	dir := t.TempDir()
	code, out, log := runSourceRunnerScript(t, job.Spec.Template.Spec.Containers[0].Command[2], map[string]string{
		"WORKFLOW_URL":         "git::https://example.com/wf.git",
		"CRITERIA_SECRET_VARS": cvs,
		"CRITERIA_HOME":        filepath.Join(dir, "home"),
	})
	require.Equal(t, 0, code, out)
	assert.Contains(t, log, "[--var] [kanboard_app_token=file:/home/criteria/kanboard-secrets/kanboard_app_token]",
		"the derived kanboard pair must reach the criteria apply argv: %s", log)
}

// originRecordPath returns the record path the runner writes for a job.
func originRecordPath(home, jobName string) string {
	return filepath.Join(home, "runs", jobName, "run-metadata.json")
}
