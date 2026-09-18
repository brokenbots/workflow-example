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
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/jobbuilder"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	assert.Empty(t, job.Spec.Template.Spec.InitContainers,
		"source mode has no repo-clone: the base image ships no gh and the workflow source is content")

	// The run context and source env reach the runner; REPO_DIR (the
	// repo-clone contract) deliberately does not.
	assert.Equal(t, "git::https://github.com/brokenbots/workflow-example.git//linear_intake_v1", envValue(runner.Env, "WORKFLOW_URL"))
	assert.Equal(t, "", envValue(runner.Env, "WORKFLOW_REF"), "no ref declared: WORKFLOW_REF must be absent")
	assert.Equal(t, "", envValue(runner.Env, "REPO_DIR"))
	assert.Equal(t, "CRI-231", envValue(runner.Env, "TICKET_ID"))
	assert.Equal(t, "https://github.com/brokenbots/workflow-example.git", envValue(runner.Env, "REPO_URL"))
	assert.Equal(t, job.Name, envValue(runner.Env, "JOB_NAME"))
	assert.Equal(t, "/data/criteria", envValue(runner.Env, "CRITERIA_HOME"))
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
		}, dir)
		require.Equal(t, 0, proc.code, "script must succeed: %s", proc.output)
		log := readStubLog(t, filepath.Join(dir, "stub.log"))
		assert.Contains(t, log, "argv: [apply] [git::https://example.com/org/workflows.git?ref=main] "+
			"[--workflow-ref] [28777aacc3cfbe85005ddb27f548116e692c0eb4] "+
			"[--server] [http://castle:9443] [--events-file] [/tmp/events.ndjson]",
			"apply must receive the URL, the pin, and the forwarded flags in order: %s", log)
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

// originRecordPath returns the record path the runner writes for a job.
func originRecordPath(home, jobName string) string {
	return filepath.Join(home, "runs", jobName, "run-metadata.json")
}
