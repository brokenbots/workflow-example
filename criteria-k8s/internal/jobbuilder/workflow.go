// Workflow-object rendering for the jobbuilder (CRI-222). The workflow
// object stamped on the run (CRI-217) is the pod-construction source for
// image-mode runs: it declares the target namespace, the volumes each
// environment mounts, and the OpenBao/CSI secrets the adapter environments
// consume. Only the stamped object is consumed. Source-mode runs (CRI-231)
// render the same plan for their declared volumes/secrets/env; only the
// runner image and the runner entrypoint differ. Every builder consumes the
// plan; runs without a stamped workflow keep the built-in defaults
// verbatim.
package jobbuilder

import (
	"path"
	"sort"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/routes"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// dataMountPath is the shared run-state mount: the runner, the
	// repo-clone init container, and every adapter environment mount the
	// run's data volume there (CRITERIA_HOME, per-scope discovery files,
	// the per-ticket repo clone).
	dataMountPath = "/data"
	// dataVolumeName is the pod volume name carrying the run state.
	dataVolumeName = "data"
	// scriptsVolumeName is the pod volume name carrying the pod-adapter
	// scripts ConfigMap.
	scriptsVolumeName = "scripts"
	// scriptsMountPath is the scripts mount point in every container.
	scriptsMountPath = "/opt/criteria-pod-adapter"
	// secretVolumePrefix namespaces CSI secret volumes away from declared
	// data volumes in the pod volume list.
	secretVolumePrefix = "secret-"
	// workflowVolumePrefix keeps a workflow volume whose declared name
	// collides with a built-in pod volume from shadowing operator plumbing.
	workflowVolumePrefix = "wf-"
)

// workflowPlan renders the workflow object stamped on the run into pod
// construction inputs. A nil plan (no workflow stamped) resolves every
// method to the built-in defaults, so builders stay branch-free.
type workflowPlan struct {
	volumes []criteriav1.RunWorkflowVolume
	secrets []criteriav1.RunWorkflowSecret
	env     map[string]string
	// runName scopes the same-host affinity labels to this run (CRI-235).
	runName string
}

// newWorkflowPlan renders the stamped workflow object.
func newWorkflowPlan(run *criteriav1.CriteriaRun) *workflowPlan {
	wf := run.Spec.Workflow
	if wf == nil {
		return nil
	}
	return &workflowPlan{volumes: wf.Volumes, secrets: wf.Secrets, env: wf.Env, runName: run.Name}
}

// targetNamespace resolves the namespace the run's children are created in:
// the workflow object's declaration when stamped, else the run's own
// namespace. The live watcher stamps both from the same criteria-routes
// ConfigMap, so they agree in practice; the fallback only covers a
// namespace-less declaration.
func targetNamespace(run *criteriav1.CriteriaRun) string {
	if wf := run.Spec.Workflow; wf != nil && wf.Namespace != "" {
		return wf.Namespace
	}
	return run.Namespace
}

// workflowPodVolumeName maps a declared volume to its pod volume name. A
// volume targeting /data re-sources the operator's default data volume: the
// shared run-state contract lives on that mount, the kubelet rejects two
// volumes on one mount path, and every existing mount keeps working under
// the stable "data" name. Any other declaration keeps its name unless it
// would shadow a built-in pod volume.
func workflowPodVolumeName(vol criteriav1.RunWorkflowVolume) string {
	if vol.MountPath == dataMountPath {
		return dataVolumeName
	}
	switch vol.Name {
	case dataVolumeName, scriptsVolumeName:
		return workflowVolumePrefix + vol.Name
	}
	return vol.Name
}

// Same-host affinity (CRI-235): every pod built from a workflow plan mounts
// all of the plan's declared volumes, so a host-affinity declaration must
// co-locate those pods onto one host. Each pod stamps one label per
// host-affinity volume — key namespacing the volume, value scoping the run
// — and carries one required pod-affinity term per volume selecting that
// label on the node hostname. The first pod to schedule anchors the host;
// required self-affinity is safe for it, because the scheduler passes a pod
// whose terms match its own labels when no other pod in the namespace
// matches yet.
const (
	// LabelHostAffinityPrefix namespaces the per-volume same-host affinity
	// keys on the pod labels the affinity terms select.
	hostAffinityLabelPrefix = "affinity-"
)

// hostAffinityVolumeKeys returns the pod volume names of the plan's
// host-affinity declarations, deduplicated in declaration order. Only PVC
// declarations are host-affinity: a claim is the volume kind whose
// underlying PV can be node-local, so its pods must share the PV's host.
// NFS is shared across hosts and tmp is pod-local — both are exempt
// (CRI-235). Undeclared volumes — including the built-in default data
// volume of a workflow that does not re-source /data — carry no affinity:
// only a declaration opts a volume in.
func (p *workflowPlan) hostAffinityVolumeKeys() []string {
	if p == nil {
		return nil
	}
	seen := make(map[string]bool, len(p.volumes))
	keys := make([]string, 0, len(p.volumes))
	for _, vol := range p.volumes {
		if vol.Kind != routes.VolumePVC {
			continue
		}
		key := workflowPodVolumeName(vol)
		if seen[key] {
			continue
		}
		seen[key] = true
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		return nil
	}
	return keys
}

// hostAffinityLabelKey returns the pod label key carrying the same-host
// affinity group of one host-affinity volume. The name part is capped at
// the 63-character label-key limit; any prefix of a pod volume name is
// still a valid label-key name segment.
func hostAffinityLabelKey(volumeKey string) string {
	name := hostAffinityLabelPrefix + volumeKey
	if len(name) > maxLabelValueLength {
		name = trimHyphens(name[:maxLabelValueLength])
	}
	return LabelHostAffinityPrefix + name
}

// hostAffinityLabels returns one label per host-affinity volume key, valued
// by the run name so distinct runs never group together. Nil when the plan
// declares no host-affinity volumes.
func (p *workflowPlan) hostAffinityLabels() map[string]string {
	keys := p.hostAffinityVolumeKeys()
	if keys == nil {
		return nil
	}
	value := safeLabelValue(p.runName)
	labels := make(map[string]string, len(keys))
	for _, key := range keys {
		labels[hostAffinityLabelKey(key)] = value
	}
	return labels
}

// podAffinity renders the plan's same-host affinity: one required term per
// host-affinity volume, selecting that volume's affinity label on the node
// hostname. All pods built from the plan carry all of the labels, so every
// term resolves to the same anchor pod. Terms select within the pod's own
// namespace (nil Namespaces): all children of a run are created in
// targetNamespace. Nil when the plan declares no host-affinity volumes.
func (p *workflowPlan) podAffinity() *corev1.Affinity {
	labels := p.hostAffinityLabels()
	if labels == nil {
		return nil
	}
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	terms := make([]corev1.PodAffinityTerm, 0, len(labels))
	for _, key := range keys {
		terms = append(terms, corev1.PodAffinityTerm{
			LabelSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{key: labels[key]},
			},
			TopologyKey: corev1.LabelHostname,
		})
	}
	return &corev1.Affinity{
		PodAffinity: &corev1.PodAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: terms,
		},
	}
}

// applyHostAffinity stamps the plan's same-host affinity onto a pod built
// from it: the affinity labels merge into the pod labels (which the job
// builders share with the job object) and the affinity onto the pod spec.
// Explicit host constraints win: a pod already carrying an affinity or a
// nodeName keeps its scheduling intent untouched — co-location applies only
// where the scheduler is free to place the pod (CRI-235).
func (p *workflowPlan) applyHostAffinity(labels map[string]string, spec *corev1.PodSpec) {
	if spec == nil || spec.NodeName != "" || spec.Affinity != nil {
		return
	}
	affinityLabels := p.hostAffinityLabels()
	if affinityLabels == nil {
		return
	}
	for key, value := range affinityLabels {
		labels[key] = value
	}
	spec.Affinity = p.podAffinity()
}

// runnerVolumes renders the runner job's pod volume list: the run-state
// data volume, the declared volumes and CSI secret volumes, and the
// pod-adapter scripts ConfigMap. Runs without a stamped workflow object
// keep the built-in secret volumes — a stamped workflow declares its own
// secrets and replaces them.
func (p *workflowPlan) runnerVolumes(dataPVC string) []corev1.Volume {
	volumes := p.podVolumes(dataPVC, true)
	if p == nil {
		volumes = append(volumes,
			csiVolume("linear-secrets", "linear-spc"),
			csiVolume("copilot-secrets", "copilot-spc"),
		)
	}
	return append(volumes, scriptsVolume())
}

// adapterVolumes renders the adapter pods' volume list: the declared volumes
// and CSI secret volumes, the scripts ConfigMap, and — when includeData is
// set — the run-state data volume (CRI-237). Wire-shaped adapter pods (the
// accept token delivered over the shim channel) mount no shared data
// volume: their token-file and discovery-file surfaces are gone. A declared
// /data re-source keeps its volume regardless, because the workflow itself
// asked for it; pass declaresDataMount through the includeData decision.
// Adapter pods carry no built-in secret volumes; secrets arrive only
// through the workflow object's declarations.
func (p *workflowPlan) adapterVolumes(dataPVC string, includeData bool) []corev1.Volume {
	volumes := p.podVolumes(dataPVC, includeData)
	return append(volumes, scriptsVolume())
}

// declaresDataMount reports whether a declared volume re-sources the
// built-in data volume at /data. Wire-shaped adapter pods keep the data
// volume then: the declaration, not the token-file delivery, is what mounts
// it (CRI-237).
func (p *workflowPlan) declaresDataMount() bool {
	if p == nil {
		return false
	}
	for _, vol := range p.volumes {
		if vol.MountPath == dataMountPath {
			return true
		}
	}
	return false
}

// podVolumes renders the shared pod volume prefix: the declared volumes, a
// CSI volume per declared secret, and — when includeData is set — the
// default data volume (re-sourced by a /data declaration), whose run-state
// contract the legacy token-file delivery lives on.
func (p *workflowPlan) podVolumes(dataPVC string, includeData bool) []corev1.Volume {
	volumes := make([]corev1.Volume, 0, 8)
	if includeData {
		volumes = append(volumes, dataVolume(dataPVC))
	}
	if p == nil {
		return volumes
	}
	for _, vol := range p.volumes {
		if workflowPodVolumeName(vol) == dataVolumeName {
			// A /data declaration re-sources the built-in data volume:
			// it replaces the default volume when that is present, and
			// appends its own volume under the same stable "data" name
			// when the built-in volume is withheld (defensive — callers
			// pass includeData whenever a /data declaration exists).
			if includeData {
				volumes[0] = workflowVolume(vol)
			} else {
				volumes = append(volumes, workflowVolume(vol))
			}
			continue
		}
		volumes = append(volumes, workflowVolume(vol))
	}
	for _, sec := range p.secrets {
		volumes = append(volumes, csiVolume(secretVolumePrefix+sec.Name, sec.SecretProviderClass))
	}
	return volumes
}

// workflowVolume renders one declared volume by kind: pvc, nfs, or tmp.
func workflowVolume(vol criteriav1.RunWorkflowVolume) corev1.Volume {
	src := corev1.VolumeSource{}
	switch vol.Kind {
	case routes.VolumePVC:
		src.PersistentVolumeClaim = &corev1.PersistentVolumeClaimVolumeSource{
			ClaimName: vol.Claim,
			ReadOnly:  vol.ReadOnly,
		}
	case routes.VolumeNFS:
		src.NFS = &corev1.NFSVolumeSource{Server: vol.Server, Path: vol.Path, ReadOnly: vol.ReadOnly}
	case routes.VolumeTmp:
		src.EmptyDir = &corev1.EmptyDirVolumeSource{}
		if vol.SizeLimit != "" {
			// A sizeLimit is a free-form string at every boundary: the
			// routes schema only requires a non-empty string and the
			// CRD carries no pattern, so an unparsable declaration can
			// reach the builder. Omit the limit — the tmp volume stays
			// an unbounded emptyDir — and surface the misconfiguration,
			// because a panic here would crash-loop the operator and
			// stall every run (CRI-222 review B1).
			q, err := resource.ParseQuantity(vol.SizeLimit)
			if err != nil {
				ctrllog.Log.Error(err, "workflow volume has an invalid sizeLimit; building the tmp volume without a size limit",
					"volume", vol.Name, "sizeLimit", vol.SizeLimit)
			} else {
				src.EmptyDir.SizeLimit = &q
			}
		}
	}
	return corev1.Volume{Name: workflowPodVolumeName(vol), VolumeSource: src}
}

// runnerMounts renders the workflow-runner container's mounts. The runner
// always carries the run-state data mount: legacy (pre-eae0181) engines
// still deliver adapter tokens through files on the shared volume, and the
// runner's discovery publishing needs the workflow's declared /data (CRI-237).
func (p *workflowPlan) runnerMounts() []corev1.VolumeMount {
	if p == nil {
		return []corev1.VolumeMount{
			{Name: dataVolumeName, MountPath: dataMountPath},
			{Name: "linear-secrets", MountPath: "/secrets/linear_api_key", SubPath: "linear_api_key"},
			{Name: "copilot-secrets", MountPath: "/home/criteria/secrets"},
			{Name: scriptsVolumeName, MountPath: scriptsMountPath},
		}
	}
	return p.envMounts(dataMountPath, true)
}

// cloneMounts renders the repo-clone init container's mounts.
func (p *workflowPlan) cloneMounts() []corev1.VolumeMount {
	if p == nil {
		return []corev1.VolumeMount{
			{Name: dataVolumeName, MountPath: dataMountPath},
			{Name: "copilot-secrets", MountPath: "/home/criteria/secrets"},
		}
	}
	return p.envMounts(dataMountPath, true)
}

// adapterMounts renders an adapter container's mounts. When includeData is
// set the run-state data mount leads, as before (CRI-237); wire-shaped
// adapter pods mount no data volume, so the mount is omitted and only the
// declared volumes, secrets, and scripts ConfigMap remain. A declared /data
// re-source is still mounted then: its mount IS the built-in data mount.
func (p *workflowPlan) adapterMounts(includeData bool) []corev1.VolumeMount {
	if p == nil {
		if includeData {
			return []corev1.VolumeMount{
				{Name: dataVolumeName, MountPath: dataMountPath},
				{Name: scriptsVolumeName, MountPath: scriptsMountPath},
			}
		}
		return []corev1.VolumeMount{
			{Name: scriptsVolumeName, MountPath: scriptsMountPath},
		}
	}
	return p.envMounts(dataMountPath, includeData)
}

// envMounts renders the declared volumes and secrets into container mounts
// around the run-state data mount. Every container built from the workflow
// object mounts every declaration: the environments that declare them are
// exactly the pods built from this workflow object. The /data declaration
// is not re-mounted — it re-sources the default data volume, whose mount
// every data-carrying container already leads with. includeData=false
// (wire-shaped adapter pods, CRI-237) omits that lead mount; a /data
// declaration cannot co-occur with it, because callers force includeData
// whenever the workflow declares /data.
func (p *workflowPlan) envMounts(dataMount string, includeData bool) []corev1.VolumeMount {
	mounts := make([]corev1.VolumeMount, 0, 8)
	if includeData {
		mounts = append(mounts, corev1.VolumeMount{Name: dataVolumeName, MountPath: dataMount})
	}
	for _, vol := range p.volumes {
		if name := workflowPodVolumeName(vol); name == dataVolumeName {
			continue
		}
		mounts = append(mounts, corev1.VolumeMount{
			Name:      workflowPodVolumeName(vol),
			MountPath: vol.MountPath,
			SubPath:   vol.SubPath,
			ReadOnly:  vol.ReadOnly,
		})
	}
	for _, sec := range p.secrets {
		mounts = append(mounts, corev1.VolumeMount{
			Name:      secretVolumePrefix + sec.Name,
			MountPath: sec.MountPath,
			ReadOnly:  true,
		})
	}
	return append(mounts, corev1.VolumeMount{Name: scriptsVolumeName, MountPath: scriptsMountPath})
}

// runnerEnvs injects the workflow object's own env map plus each declared
// volume's env map into the runner container. Secret env entries are
// deliberately absent: the runner reads its credentials from the CSI-synced
// files, and pre-setting a secret-named env var would shadow the engine's
// own secret-channel resolution with a path string.
func (p *workflowPlan) runnerEnvs() []corev1.EnvVar {
	return append(p.workflowEnvs(), p.volumeEnvs()...)
}

// adapterEnvs injects each declared volume's env map plus the declared
// secrets' env mapping into adapter containers. Secret entries are
// name-only references: each value is the rendered file path inside the
// secret's CSI mount, never secret material. Credentials reach the adapters
// over the OpenSession SDK contract; the mapping only anchors where the
// rendered keys live.
func (p *workflowPlan) adapterEnvs() []corev1.EnvVar {
	return append(p.volumeEnvs(), p.secretEnvs()...)
}

// volumeEnvs injects each declared volume's env map.
func (p *workflowPlan) volumeEnvs() []corev1.EnvVar {
	if p == nil {
		return nil
	}
	var envs []corev1.EnvVar
	for _, vol := range p.volumes {
		for name, value := range vol.Env {
			envs = append(envs, corev1.EnvVar{Name: name, Value: value})
		}
	}
	return sortEnvs(envs)
}

// secretEnvs renders the declared secrets' env mapping.
func (p *workflowPlan) secretEnvs() []corev1.EnvVar {
	if p == nil {
		return nil
	}
	var envs []corev1.EnvVar
	for _, sec := range p.secrets {
		for name, key := range sec.Env {
			envs = append(envs, corev1.EnvVar{Name: name, Value: joinSecretMount(sec.MountPath, key)})
		}
	}
	return sortEnvs(envs)
}

// workflowEnvs renders the workflow object's own env map into the runner
// container. Adapters deliberately do not receive it: their env is the
// handshake contract, not workflow configuration.
func (p *workflowPlan) workflowEnvs() []corev1.EnvVar {
	if p == nil {
		return nil
	}
	envs := make([]corev1.EnvVar, 0, len(p.env))
	for name, value := range p.env {
		envs = append(envs, corev1.EnvVar{Name: name, Value: value})
	}
	return sortEnvs(envs)
}

// hasSecrets reports whether the plan declares CSI secrets; pods carrying
// them need the criteria-runner service account so the OpenBao CSI provider
// can authenticate the pod against its own service account token.
func (p *workflowPlan) hasSecrets() bool {
	return p != nil && len(p.secrets) > 0
}

// appendEnvDistinct appends extra env vars to base, dropping entries whose
// name is already present: first wins, so the builder's own env contract
// (handshake, run state) is never displaced by a declaration, and no
// duplicate name reaches the kubelet.
func appendEnvDistinct(base, extra []corev1.EnvVar) []corev1.EnvVar {
	seen := make(map[string]bool, len(base))
	for _, e := range base {
		seen[e.Name] = true
	}
	for _, e := range extra {
		if !seen[e.Name] {
			seen[e.Name] = true
			base = append(base, e)
		}
	}
	return base
}

func sortEnvs(envs []corev1.EnvVar) []corev1.EnvVar {
	sort.Slice(envs, func(i, j int) bool { return envs[i].Name < envs[j].Name })
	return envs
}

// joinSecretMount renders a declared key inside a secret's CSI mount as the
// file path the sync dir presents. path.Join (not filepath) keeps the
// rendering platform-independent: the pod runs Linux regardless of where
// the operator builds.
func joinSecretMount(mountPath, key string) string {
	return path.Join(mountPath, key)
}
