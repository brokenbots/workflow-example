// Workflow-object rendering for the jobbuilder (CRI-222). The workflow
// object stamped on the run (CRI-217) is the pod-construction source for
// image-mode runs: it declares the target namespace, the volumes each
// environment mounts, and the OpenBao/CSI secrets the adapter environments
// consume. Only the stamped object is consumed — url/source modes (CRI-231)
// are out of scope, and the run's spec.image/operator default still resolves
// the image. Every builder consumes the plan; runs without a stamped
// workflow keep the built-in defaults verbatim.
package jobbuilder

import (
	"path"
	"sort"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/routes"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
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
}

// newWorkflowPlan renders the stamped workflow object.
func newWorkflowPlan(run *criteriav1.CriteriaRun) *workflowPlan {
	wf := run.Spec.Workflow
	if wf == nil {
		return nil
	}
	return &workflowPlan{volumes: wf.Volumes, secrets: wf.Secrets, env: wf.Env}
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

// runnerVolumes renders the runner job's pod volume list: the run-state
// data volume, the declared volumes and CSI secret volumes, and the
// pod-adapter scripts ConfigMap. Runs without a stamped workflow object
// keep the built-in secret volumes — a stamped workflow declares its own
// secrets and replaces them.
func (p *workflowPlan) runnerVolumes(dataPVC string) []corev1.Volume {
	volumes := p.podVolumes(dataPVC)
	if p == nil {
		volumes = append(volumes,
			csiVolume("linear-secrets", "linear-spc"),
			csiVolume("copilot-secrets", "copilot-spc"),
		)
	}
	return append(volumes, scriptsVolume())
}

// adapterVolumes renders the adapter pods' volume list: the run-state data
// volume, the declared volumes and CSI secret volumes, and the scripts
// ConfigMap. Adapter pods carry no built-in secret volumes; secrets arrive
// only through the workflow object's declarations.
func (p *workflowPlan) adapterVolumes(dataPVC string) []corev1.Volume {
	return append(p.podVolumes(dataPVC), scriptsVolume())
}

// podVolumes renders the shared pod volume prefix: the default data volume
// (re-sourced by a /data declaration), the declared volumes, and a CSI
// volume per declared secret. The default data PVC stays when no
// declaration targets /data, because the run-state contract lives there.
func (p *workflowPlan) podVolumes(dataPVC string) []corev1.Volume {
	volumes := []corev1.Volume{dataVolume(dataPVC)}
	if p == nil {
		return volumes
	}
	for _, vol := range p.volumes {
		if name := workflowPodVolumeName(vol); name == dataVolumeName {
			volumes[0] = workflowVolume(vol)
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
			q := resource.MustParse(vol.SizeLimit)
			src.EmptyDir.SizeLimit = &q
		}
	}
	return corev1.Volume{Name: workflowPodVolumeName(vol), VolumeSource: src}
}

// runnerMounts renders the workflow-runner container's mounts.
func (p *workflowPlan) runnerMounts() []corev1.VolumeMount {
	if p == nil {
		return []corev1.VolumeMount{
			{Name: dataVolumeName, MountPath: dataMountPath},
			{Name: "linear-secrets", MountPath: "/secrets/linear_api_key", SubPath: "linear_api_key"},
			{Name: "copilot-secrets", MountPath: "/home/criteria/secrets"},
			{Name: scriptsVolumeName, MountPath: scriptsMountPath},
		}
	}
	return p.envMounts(dataMountPath)
}

// cloneMounts renders the repo-clone init container's mounts.
func (p *workflowPlan) cloneMounts() []corev1.VolumeMount {
	if p == nil {
		return []corev1.VolumeMount{
			{Name: dataVolumeName, MountPath: dataMountPath},
			{Name: "copilot-secrets", MountPath: "/home/criteria/secrets"},
		}
	}
	return p.envMounts(dataMountPath)
}

// adapterMounts renders an adapter container's mounts.
func (p *workflowPlan) adapterMounts() []corev1.VolumeMount {
	if p == nil {
		return []corev1.VolumeMount{
			{Name: dataVolumeName, MountPath: dataMountPath},
			{Name: scriptsVolumeName, MountPath: scriptsMountPath},
		}
	}
	return p.envMounts(dataMountPath)
}

// envMounts renders the declared volumes and secrets into container mounts
// around the run-state data mount. Every container built from the workflow
// object mounts every declaration: the environments that declare them are
// exactly the pods built from this workflow object. The /data declaration
// is not re-mounted — it re-sources the default data volume, whose mount
// every container already carries.
func (p *workflowPlan) envMounts(dataMount string) []corev1.VolumeMount {
	mounts := []corev1.VolumeMount{{Name: dataVolumeName, MountPath: dataMount}}
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
