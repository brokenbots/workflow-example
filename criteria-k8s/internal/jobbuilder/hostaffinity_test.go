package jobbuilder_test

// CRI-235: pods sharing a host-affinity PV must land on the same host. The
// jobbuilder stamps required pod affinity — keyed per declared host-affinity
// volume (kind=pvc) and scoped to the run — on every pod built from the
// workflow plan: runner jobs (image and source modes), adapter jobs, and
// per-scope adapter pods (fallback and group). NFS and tmp declarations are
// exempt, undeclared volumes carry no affinity, and the affinity labels are
// coherent across every pod of the run so the terms resolve to the same
// anchor host.

import (
	"reflect"
	"regexp"
	"strings"
	"testing"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/events"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/jobbuilder"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// affinityLabelsOf returns the same-host affinity labels stamped on a pod.
func affinityLabelsOf(labels map[string]string) map[string]string {
	out := map[string]string{}
	for key, value := range labels {
		if strings.HasPrefix(key, jobbuilder.LabelHostAffinityPrefix) {
			out[key] = value
		}
	}
	return out
}

// affinityGroupPod builds the (scope, environment) group pod for the run.
func affinityGroupPod(t *testing.T, run *criteriav1.CriteriaRun) *corev1.Pod {
	t.Helper()
	members := []events.LifecycleEvent{
		{
			Event: events.EventProvisionWanted, RunID: run.Name,
			ScopeID: "scope-a", ScopeTag: "root-scope",
			AdapterName: "intake", AdapterType: "shell",
			Digest: "sha256:deadbeef", TokenFile: "/tmp/tokens/intake",
			Environment: "ci",
		},
		{
			Event: events.EventProvisionWanted, RunID: run.Name,
			ScopeID: "scope-b", ScopeTag: "root-scope",
			AdapterName: "review", AdapterType: "copilot",
			Digest: "sha256:cafe", TokenFile: "/tmp/tokens/review",
			Environment: "ci",
		},
	}
	pod := jobbuilder.BuildPerScopeAdapterPodGroup(run, jobbuilder.Defaults{DataPVC: "criteria-data"}, "scope-a", "ci", members)
	require.NotNil(t, pod)
	return pod
}

// exampleWorkflow declares two host-affinity volumes: the /data re-sourcing
// declaration (pod volume "data") and "repo", alongside the exempt cache
// (nfs) and scratch (tmp) declarations.
func TestHostAffinityCoLocatesPodsSharingDeclaredPVC(t *testing.T) {
	run := workflowRun("cri-235-affinity", exampleWorkflow())
	defaults := jobbuilder.Defaults{DataPVC: "criteria-data"}

	value := "cri-235-affinity"
	dataKey := jobbuilder.LabelHostAffinityPrefix + "affinity-data"
	repoKey := jobbuilder.LabelHostAffinityPrefix + "affinity-repo"
	want := &corev1.Affinity{
		PodAffinity: &corev1.PodAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
				{
					LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{dataKey: value}},
					TopologyKey:   corev1.LabelHostname,
				},
				{
					LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{repoKey: value}},
					TopologyKey:   corev1.LabelHostname,
				},
			},
		},
	}

	// Every pod built from the plan carries the affinity and the labels:
	// both job modes, adapter jobs, and both per-scope pod shapes.
	runnerJob := jobbuilder.BuildRunnerJob(run, defaults)
	adapterJob := jobbuilder.BuildAdapterJob(run, defaults, "shell")
	fallbackPod := perScopePod(t, run)
	groupPod := affinityGroupPod(t, run)
	pods := map[string]struct {
		labels map[string]string
		spec   *corev1.PodSpec
	}{
		"runner job":             {runnerJob.Spec.Template.Labels, &runnerJob.Spec.Template.Spec},
		"adapter job":            {adapterJob.Spec.Template.Labels, &adapterJob.Spec.Template.Spec},
		"per-scope fallback pod": {fallbackPod.Labels, &fallbackPod.Spec},
		"per-scope group pod":    {groupPod.Labels, &groupPod.Spec},
	}
	for name, pod := range pods {
		require.NotNil(t, pod.spec.Affinity, "%s must carry same-host affinity", name)
		assert.True(t, reflect.DeepEqual(want, pod.spec.Affinity),
			"%s affinity must carry one term per declared host-affinity volume", name)
		assert.Equal(t, map[string]string{dataKey: value, repoKey: value}, affinityLabelsOf(pod.labels),
			"%s must stamp one run-scoped affinity label per declared host-affinity volume", name)
		for _, term := range pod.spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution {
			assert.Nil(t, term.Namespaces, "%s terms select within the pod's own namespace", name)
		}
	}

	// BuildAll emits the runner plus one job per adapter kind: all of them
	// carry the affinity.
	for _, job := range jobbuilder.BuildAll(run, defaults) {
		require.NotNil(t, job.Spec.Template.Spec.Affinity, "job %s must carry same-host affinity", job.Name)
		assert.True(t, reflect.DeepEqual(want, job.Spec.Template.Spec.Affinity), "job %s affinity mismatch", job.Name)
		assert.Equal(t, map[string]string{dataKey: value, repoKey: value}, affinityLabelsOf(job.Spec.Template.Labels),
			"job %s template labels must carry the affinity labels", job.Name)
		// The job object shares the template's label map by convention: the
		// affinity labels must surface on the job too.
		assert.Equal(t, map[string]string{dataKey: value, repoKey: value}, affinityLabelsOf(job.Labels),
			"job %s object labels must carry the affinity labels", job.Name)
	}
}

// The runner (image mode), the source-mode runner, the adapter job, and both
// per-scope pod shapes must carry byte-identical affinity terms and labels:
// every term must resolve to the same anchor host, whichever pod schedules
// first.
func TestHostAffinityCoherentAcrossAllRunPods(t *testing.T) {
	run := workflowRun("cri-235-coherence", exampleWorkflow())
	defaults := jobbuilder.Defaults{DataPVC: "criteria-data"}

	sourceRun := *run
	sourceRun.Spec.WorkflowSource = &criteriav1.RunWorkflowSource{
		Type: "url",
		URL:  "git::https://github.com/brokenbots/workflow-example.git//linear_intake_v1",
	}

	runner := jobbuilder.BuildRunnerJob(run, defaults)
	reference := runner.Spec.Template.Spec.Affinity
	referenceLabels := affinityLabelsOf(runner.Spec.Template.Labels)
	require.NotNil(t, reference)
	require.NotEmpty(t, referenceLabels)

	pods := map[string]struct {
		labels map[string]string
		spec   *corev1.PodSpec
	}{
		"adapter job":            {jobbuilder.BuildAdapterJob(run, defaults, "shell").Spec.Template.Labels, &jobbuilder.BuildAdapterJob(run, defaults, "shell").Spec.Template.Spec},
		"source-mode runner":     {jobbuilder.BuildRunnerJob(&sourceRun, defaults).Spec.Template.Labels, &jobbuilder.BuildRunnerJob(&sourceRun, defaults).Spec.Template.Spec},
		"per-scope fallback pod": {perScopePod(t, run).Labels, &perScopePod(t, run).Spec},
		"per-scope group pod":    {affinityGroupPod(t, run).Labels, &affinityGroupPod(t, run).Spec},
	}
	for name, pod := range pods {
		require.NotNil(t, pod.spec.Affinity, "%s must carry same-host affinity", name)
		assert.True(t, reflect.DeepEqual(reference, pod.spec.Affinity),
			"%s affinity must be byte-identical to the runner's", name)
		assert.Equal(t, referenceLabels, affinityLabelsOf(pod.labels),
			"%s affinity labels must match the runner's", name)
	}
}

// NFS and tmp declarations are exempt: no affinity, no affinity labels, on
// any pod built from the plan. The built-in default data volume — undeclared
// in this workflow — must not gain affinity either.
func TestHostAffinityExemptsNFSAndTmp(t *testing.T) {
	wf := &criteriav1.RunWorkflow{
		Name: "linear-intake-v1", Type: "image", Namespace: "wf-jobs",
		Volumes: []criteriav1.RunWorkflowVolume{
			{Name: "cache", Kind: "nfs", MountPath: "/mnt/cache", Server: "192.168.17.20", Path: "/exports/cache"},
			{Name: "scratch", Kind: "tmp", MountPath: "/tmp/scratch", SizeLimit: "8Gi"},
		},
	}
	run := workflowRun("cri-235-nfs", wf)
	defaults := jobbuilder.Defaults{DataPVC: "criteria-data"}

	for _, job := range jobbuilder.BuildAll(run, defaults) {
		assert.Nil(t, job.Spec.Template.Spec.Affinity, "job %s must not carry same-host affinity", job.Name)
		assert.Empty(t, affinityLabelsOf(job.Spec.Template.Labels), "job %s must not stamp affinity labels", job.Name)
	}
	fallback := perScopePod(t, run)
	assert.Nil(t, fallback.Spec.Affinity)
	assert.Empty(t, affinityLabelsOf(fallback.Labels))
	group := affinityGroupPod(t, run)
	assert.Nil(t, group.Spec.Affinity)
	assert.Empty(t, affinityLabelsOf(group.Labels))
}

// No stamped workflow — no declarations, so no affinity anywhere, including
// for the built-in default data PVC.
func TestNoAffinityWithoutStampedWorkflow(t *testing.T) {
	run := workflowRun("cri-235-plain", nil)
	defaults := jobbuilder.Defaults{DataPVC: "criteria-data"}

	for _, job := range jobbuilder.BuildAll(run, defaults) {
		assert.Nil(t, job.Spec.Template.Spec.Affinity, "job %s must not carry same-host affinity", job.Name)
		assert.Empty(t, affinityLabelsOf(job.Spec.Template.Labels))
	}
	fallback := perScopePod(t, run)
	assert.Nil(t, fallback.Spec.Affinity)
	assert.Empty(t, affinityLabelsOf(fallback.Labels))
}

// The affinity value is scoped to the run: two runs sharing a workflow must
// never group onto one host through each other's terms.
func TestHostAffinityValuesAreRunScoped(t *testing.T) {
	defaults := jobbuilder.Defaults{DataPVC: "criteria-data"}
	dataKey := jobbuilder.LabelHostAffinityPrefix + "affinity-data"
	repoKey := jobbuilder.LabelHostAffinityPrefix + "affinity-repo"
	runA := workflowRun("cri-235-run-a", exampleWorkflow())
	runB := workflowRun("cri-235-run-b", exampleWorkflow())

	// Same keys, run-scoped values: the label sets differ across runs, so a
	// run's terms never match another run's pods.
	labelsA := affinityLabelsOf(jobbuilder.BuildRunnerJob(runA, defaults).Spec.Template.Labels)
	labelsB := affinityLabelsOf(jobbuilder.BuildRunnerJob(runB, defaults).Spec.Template.Labels)
	require.Equal(t, map[string]string{dataKey: "cri-235-run-a", repoKey: "cri-235-run-a"}, labelsA)
	require.Equal(t, map[string]string{dataKey: "cri-235-run-b", repoKey: "cri-235-run-b"}, labelsB)
	assert.NotEqual(t, labelsA, labelsB)

	affinityA := jobbuilder.BuildRunnerJob(runA, defaults).Spec.Template.Spec.Affinity
	require.NotNil(t, affinityA)
	for _, term := range affinityA.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution {
		for _, value := range term.LabelSelector.MatchLabels {
			assert.Equal(t, "cri-235-run-a", value, "run A terms must select only run A pods")
		}
	}
	affinityB := jobbuilder.BuildRunnerJob(runB, defaults).Spec.Template.Spec.Affinity
	require.NotNil(t, affinityB)
	for _, term := range affinityB.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution {
		for _, value := range term.LabelSelector.MatchLabels {
			assert.Equal(t, "cri-235-run-b", value, "run B terms must select only run B pods")
		}
	}
}

// A volume name pushing "affinity-<name>" past the 63-character label-key
// limit is truncated; the resulting key stays a valid label-key name
// segment and keeps its run-scoped value.
func TestHostAffinityLabelKeyCapsVolumeName(t *testing.T) {
	wf := &criteriav1.RunWorkflow{
		Name: "linear-intake-v1", Type: "image", Namespace: "wf-jobs",
		Volumes: []criteriav1.RunWorkflowVolume{
			{Name: strings.Repeat("a", 60), Kind: "pvc", MountPath: "/mnt/long", Claim: "some-claim"},
		},
	}
	run := workflowRun("cri-235-long", wf)
	job := jobbuilder.BuildRunnerJob(run, jobbuilder.Defaults{DataPVC: "criteria-data"})

	affinity := job.Spec.Template.Spec.Affinity
	require.NotNil(t, affinity)
	terms := affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	require.Len(t, terms, 1)

	key := jobbuilder.LabelHostAffinityPrefix + "affinity-" + strings.Repeat("a", 54)
	require.Contains(t, terms[0].LabelSelector.MatchLabels, key)
	assert.Equal(t, "cri-235-long", terms[0].LabelSelector.MatchLabels[key])
	assert.Equal(t, "cri-235-long", job.Spec.Template.Labels[key])

	namePart := strings.TrimPrefix(key, jobbuilder.LabelHostAffinityPrefix)
	assert.LessOrEqual(t, len(namePart), 63, "label-key name part must fit the Kubernetes limit")
	assert.Regexp(t, regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`), namePart)
}

// Two declarations re-sourcing /data collapse onto the one "data" pod
// volume, so the affinity carries exactly one term and one label.
func TestHostAffinityDeduplicatesReSourcedDataDeclarations(t *testing.T) {
	wf := &criteriav1.RunWorkflow{
		Name: "linear-intake-v1", Type: "image", Namespace: "wf-jobs",
		Volumes: []criteriav1.RunWorkflowVolume{
			{Name: "first", Kind: "pvc", MountPath: "/data", Claim: "claim-a"},
			{Name: "second", Kind: "pvc", MountPath: "/data", Claim: "claim-b"},
		},
	}
	run := workflowRun("cri-235-dedup", wf)
	job := jobbuilder.BuildRunnerJob(run, jobbuilder.Defaults{DataPVC: "criteria-data"})

	affinity := job.Spec.Template.Spec.Affinity
	require.NotNil(t, affinity)
	terms := affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	require.Len(t, terms, 1)
	dataKey := jobbuilder.LabelHostAffinityPrefix + "affinity-data"
	assert.Equal(t, map[string]string{dataKey: "cri-235-dedup"}, terms[0].LabelSelector.MatchLabels)
	assert.Equal(t, map[string]string{dataKey: "cri-235-dedup"}, affinityLabelsOf(job.Spec.Template.Labels))
}

// Repeated builds of the same run produce byte-identical affinity objects
// and labels: the reconcile must not churn child specs.
func TestHostAffinityDeterministic(t *testing.T) {
	run := workflowRun("cri-235-det", exampleWorkflow())
	defaults := jobbuilder.Defaults{DataPVC: "criteria-data"}

	j1 := jobbuilder.BuildRunnerJob(run, defaults)
	j2 := jobbuilder.BuildRunnerJob(run, defaults)
	assert.True(t, reflect.DeepEqual(j1.Spec.Template.Spec.Affinity, j2.Spec.Template.Spec.Affinity))
	assert.True(t, reflect.DeepEqual(affinityLabelsOf(j1.Spec.Template.Labels), affinityLabelsOf(j2.Spec.Template.Labels)))

	g1 := affinityGroupPod(t, run)
	g2 := affinityGroupPod(t, run)
	assert.True(t, reflect.DeepEqual(g1.Spec.Affinity, g2.Spec.Affinity))
	assert.True(t, reflect.DeepEqual(affinityLabelsOf(g1.Labels), affinityLabelsOf(g2.Labels)))
}
