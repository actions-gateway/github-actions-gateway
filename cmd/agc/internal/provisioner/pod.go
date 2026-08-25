package provisioner

import (
	"strings"

	"github.com/actions-gateway/github-actions-gateway/agc/names"
	"github.com/actions-gateway/github-actions-gateway/api/apilabels"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// setScaleSetWorkerMode sets WORKER_MODE=scaleset on the runner container so the
// injected wrapper runs the full runner (run.sh --jitconfig) rather than the classic
// Runner.Worker pipes handoff (§2.4). A no-op if no runner container is present.
func setScaleSetWorkerMode(pod *corev1.Pod) {
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == runnerContainer {
			pod.Spec.Containers[i].Env = mergeEnvOverride(pod.Spec.Containers[i].Env,
				[]corev1.EnvVar{{Name: workerModeEnvVar, Value: workerModeScaleSetValue}})
			return
		}
	}
}

// resolveWorkerImage returns the worker image used for a provision: the
// per-RunnerGroup override, else the WORKER_IMAGE environment variable, else the
// digest-pinned built-in default. Shared by buildPod and the version-label
// computation so both agree on which image (hence which runner version) a pod runs.
func (p *Provisioner) resolveWorkerImage(spec *ResolvedSpec) string {
	return p.EffectiveWorkerImage(spec.WorkerImage)
}

// EffectiveWorkerImage resolves the same three-rung chain as resolveWorkerImage from
// a bare per-owner override, for callers that hold a RunnerGroup/RunnerSet spec but
// no ResolvedSpec — the reconcilers judging the image's runner version (Q715).
func (p *Provisioner) EffectiveWorkerImage(workerImage string) string {
	if workerImage != "" {
		return workerImage
	}
	if p.DefaultWorkerImage != "" {
		return p.DefaultWorkerImage
	}
	return DefaultWorkerImage
}

// imageVersion extracts the tag from a container image reference for the
// app.kubernetes.io/version label. A digest-only or untagged reference has no
// version to report, so it falls back to names.RunnerVersion (the pinned default
// the project ships). Examples:
//
//	ghcr.io/actions/actions-runner:2.335.1@sha256:…  -> 2.335.1
//	ghcr.io/actions/actions-runner:2.335.1           -> 2.335.1
//	ghcr.io/actions/actions-runner@sha256:…          -> names.RunnerVersion
func imageVersion(image string) string {
	// Strip any digest first so an '@sha256:' colon is never mistaken for a tag.
	if at := strings.IndexByte(image, '@'); at >= 0 {
		image = image[:at]
	}
	// A tag follows the last ':' that comes after the last '/' — a registry port
	// (host:5000/repo) has its colon before the final path separator.
	if colon := strings.LastIndexByte(image, ':'); colon > strings.LastIndexByte(image, '/') {
		if tag := image[colon+1:]; tag != "" {
			return tag
		}
	}
	return names.RunnerVersion
}

func (p *Provisioner) buildSecret(target Target, name, planID, version string, payload []byte, jitConfig string) *corev1.Secret {
	data := map[string][]byte{
		planIDKey: []byte(planID),
	}
	// The classic path always carries the acquired AcquireJob payload; the scale-set
	// path (ProvisionScaleSetWorker) carries none — the runner pulls its own job — so
	// the key is omitted rather than stored empty.
	if len(payload) > 0 {
		data[payloadKey] = payload
	}
	if jitConfig != "" {
		data[jitConfigKey] = []byte(jitConfig)
	}
	// Recommended app.kubernetes.io/* metadata so the job Secret groups with the
	// worker pod it backs; the owner-identity label(s) the controller filters on
	// layer on top. managed-by is the AGC (managerName) — it, not the GMC, creates
	// these objects.
	labels := apilabels.Recommended(workerAppName, target.Key().Name, workerComponent, version, managerName)
	for k, v := range target.PodOwnerLabels() {
		labels[k] = v
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       target.Key().Namespace,
			Labels:          labels,
			OwnerReferences: []metav1.OwnerReference{target.OwnerRef()},
		},
		Data: data,
	}
}

// proxyCAVolumeSource selects where the worker reads the egress proxy's public
// certificate from: the proxy's own TLS Secret when the proxy is colocated, or the
// ConfigMap the GMC projects into this namespace when the proxy is shared from
// another one (§H.9). Nil when egress is direct and no proxy CA applies.
//
// Items pins the projection to the single certificate key in both cases, so a
// Secret's private key can never reach the worker.
func proxyCAVolumeSource(spec *ResolvedSpec) *corev1.VolumeSource {
	caMode := int32(0o444)
	switch {
	case spec.ProxyTLSSecretName != "":
		return &corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName:  spec.ProxyTLSSecretName,
				Items:       []corev1.KeyToPath{{Key: corev1.TLSCertKey, Path: proxyCAFileName}},
				DefaultMode: &caMode,
			},
		}
	case spec.ProxyCAConfigMapName != "":
		return &corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: spec.ProxyCAConfigMapName},
				Items:                []corev1.KeyToPath{{Key: proxyShareCACertKey, Path: proxyCAFileName}},
				DefaultMode:          &caMode,
			},
		}
	default:
		return nil
	}
}

func (p *Provisioner) buildPod(target Target, spec *ResolvedSpec, podName, secretName, priorityClass string, meta jobMeta) *corev1.Pod {
	// Start from the resolved PodTemplate.
	template := spec.PodTemplate.DeepCopy()

	workerImage := p.resolveWorkerImage(spec)

	// Stamp the worker lifetime cap as activeDeadlineSeconds (Q438). This is the
	// only deadline that survives the AGC being down: a Running worker whose job
	// ended while the AGC was unavailable is indistinguishable in cluster state
	// from one running a long job, so nothing observed later can reclaim it — but
	// the kubelet enforces activeDeadlineSeconds with no controller involved.
	//
	// An explicit value on the tenant's podTemplate wins and is never overwritten,
	// the same gap-fill-don't-override rule the worker image follows below.
	if spec.MaxWorkerLifetime > 0 && template.Spec.ActiveDeadlineSeconds == nil {
		deadline := int64(spec.MaxWorkerLifetime.Seconds())
		if deadline > 0 {
			template.Spec.ActiveDeadlineSeconds = &deadline
		}
	}

	// Ensure a container named "runner" exists.
	runnerIdx := -1
	for i, c := range template.Spec.Containers {
		if c.Name == runnerContainer {
			runnerIdx = i
			break
		}
	}
	if runnerIdx == -1 {
		template.Spec.Containers = append([]corev1.Container{{
			Name:  runnerContainer,
			Image: workerImage,
		}}, template.Spec.Containers...)
		runnerIdx = 0
	} else if template.Spec.Containers[runnerIdx].Image == "" {
		// Gap-fill: a tenant podTemplate may name the runner container but omit
		// its image, which the API server rejects (spec.containers[].image:
		// Required value). Fill the resolved worker image without overriding an
		// image the tenant set explicitly.
		template.Spec.Containers[runnerIdx].Image = workerImage
	}

	// Inject Secret volume.
	volumeName := "job-payload"
	template.Spec.Volumes = append(template.Spec.Volumes, corev1.Volume{
		Name: volumeName,
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: secretName},
		},
	})

	// Mount into runner container and set env var.
	c := &template.Spec.Containers[runnerIdx]
	c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{
		Name:      volumeName,
		MountPath: payloadMountPath,
		ReadOnly:  true,
	})
	c.Env = append(c.Env, corev1.EnvVar{
		Name:  "PAYLOAD_SECRET_PATH",
		Value: payloadMountPath,
	})

	// Tell the wrapper where to hand its report back, and pin the path rather than
	// relying on the kubelet default so the two sides cannot drift (Q792). A template
	// that already set terminationMessagePath keeps it: the tenant may be reading the
	// message for their own purposes, and the wrapper writes wherever this points.
	//
	// The policy stays File. FallbackToLogsOnError would substitute the container's
	// log tail when the file is empty AND the container failed, which would hand the
	// reader arbitrary log output in the place a structured report is expected.
	if c.TerminationMessagePath == "" {
		c.TerminationMessagePath = corev1.TerminationMessagePathDefault
	}
	if c.TerminationMessagePolicy == "" {
		c.TerminationMessagePolicy = corev1.TerminationMessageReadFile
	}
	c.Env = append(c.Env, corev1.EnvVar{
		Name:  "WORKER_TERMINATION_LOG",
		Value: c.TerminationMessagePath,
	})

	// Project the per-tenant egress-proxy CA cert into the runner container.
	// Cert only — Items restricts the projection to tls.crt so the private key
	// never reaches the worker pod. Mount mode 0o444 + the PodSpec FSGroup keep
	// the cert world-readable to the runner user (UID 1001 in the actions-runner
	// base image) without requiring write capability.
	//
	// A cross-namespace proxy (§H.9) supplies the same cert from the ConfigMap the
	// GMC projects into this namespace instead, since the provider's TLS Secret is
	// not readable from here. Same mount path and file name either way, so the
	// worker wrapper sees one contract.
	if src := proxyCAVolumeSource(spec); src != nil {
		template.Spec.Volumes = append(template.Spec.Volumes, corev1.Volume{
			Name:         proxyCAVolumeName,
			VolumeSource: *src,
		})
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{
			Name:      proxyCAVolumeName,
			MountPath: proxyCAMountPath,
			ReadOnly:  true,
		})
	}

	// Project the GHES appliance's CA bundle (Q536) the same way, from the ConfigMap
	// the gateway's githubCABundleRef names. A certificate is public material, so the
	// carrier is a ConfigMap and Items still pins the projection to the one key.
	if spec.GitHubCAConfigMapName != "" {
		caMode := int32(0o444)
		template.Spec.Volumes = append(template.Spec.Volumes, corev1.Volume{
			Name: githubCAVolumeName,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: spec.GitHubCAConfigMapName},
					Items: []corev1.KeyToPath{{
						Key:  githubCAFileName,
						Path: githubCAFileName,
					}},
					DefaultMode: &caMode,
				},
			},
		})
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{
			Name:      githubCAVolumeName,
			MountPath: githubCAMountPath,
			ReadOnly:  true,
		})
	}

	// Inject proxy env vars into the runner container (controller-enforced invariants).
	// PROXY_CA_CERT_PATH and GITHUB_CA_CERT_PATH tell the worker wrapper which CAs to
	// add to the runner's trust store; each is empty when its mount is absent, and
	// with both empty the wrapper skips the trust-store install and traffic falls back
	// to whatever the base image already trusts.
	proxyCACertPath := ""
	if proxyCAVolumeSource(spec) != nil {
		proxyCACertPath = proxyCAMountPath + "/" + proxyCAFileName
	}
	githubCACertPath := ""
	if spec.GitHubCAConfigMapName != "" {
		githubCACertPath = githubCAMountPath + "/" + githubCAFileName
	}
	proxyEnvs := []corev1.EnvVar{
		{Name: "HTTP_PROXY", Value: spec.HTTPProxy},
		{Name: "HTTPS_PROXY", Value: spec.HTTPSProxy},
		{Name: "NO_PROXY", Value: spec.NoProxy},
		{Name: "PROXY_CA_CERT_PATH", Value: proxyCACertPath},
		{Name: "GITHUB_CA_CERT_PATH", Value: githubCACertPath},
	}
	c.Env = mergeEnvOverride(c.Env, proxyEnvs)

	// Inject the GAG worker wrapper (Q235) so the runner image can be an
	// unmodified upstream actions-runner. No-op when WrapperImage is unset
	// (legacy: the worker image carries the wrapper as its own entrypoint). Must
	// run before applySecurityDefaults so the initContainer is hardened too.
	if p.WrapperImage != "" {
		p.injectWrapper(&template.Spec, runnerIdx)
	}

	// Overwrite reserved fields (controller-enforced invariants).
	sa := p.WorkerSA
	autoMount := false
	template.Spec.AutomountServiceAccountToken = &autoMount
	if sa != "" {
		template.Spec.ServiceAccountName = sa
	}
	template.Spec.HostPID = false
	template.Spec.HostNetwork = false
	template.Spec.HostIPC = false
	template.Spec.RestartPolicy = corev1.RestartPolicyNever

	// Secure-by-default pod hardening. Both helpers gap-fill: an explicit value
	// in the tenant PodTemplate always wins, so a tenant can still opt out of
	// any individual default (e.g. runAsNonRoot:false for a root-based image).
	applySecurityDefaults(&template.Spec, spec.SecurityProfile)
	p.applyResourceDefaults(&template.Spec)

	key := target.Key()
	// Recommended app.kubernetes.io/* metadata for tooling interop. managed-by is
	// the AGC (managerName); version is the resolved runner image's version.
	labels := apilabels.Recommended(workerAppName, key.Name, workerComponent, imageVersion(workerImage), managerName)
	// Functional labels (additive to, never overwritten by, the recommended set):
	//   actions-gateway/component: workload — matches the workload NetworkPolicy
	//     podSelector so worker egress is restricted to the per-tenant proxy only.
	//   actions-gateway/plan-id — stable per-pod fragment for owner-scoped lookups.
	labels["actions-gateway/component"] = "workload"
	labels[labelPlanID] = safeName(podName)
	// Owner-identity label(s): LabelRunnerGroup for v1, LabelRunnerSet for v2 —
	// the key the owning controller's Pod watch and reaper filter on.
	for k, v := range target.PodOwnerLabels() {
		labels[k] = v
	}

	// Stamp node-disruption-safety defaults (gap-filled; a tenant PodTemplate
	// annotation for any of these keys wins) so consolidation/scale-down/
	// descheduling don't evict the pod mid-job and strand the CI run.
	annotations := applyDisruptionSafetyDefaults(meta.podAnnotations(), template.ObjectMeta.Annotations)

	// Carry the sizing-profile marker the controller stamps on the resolved template
	// when a profile derived this pod's ask (Q489) — it is what lets the
	// SizingProfileOverridden check tell a profile-built pod from a template-built
	// one. Copied by key, deliberately: worker-pod annotations are a controlled set,
	// and passing tenant template annotations through wholesale would be a different
	// (and much larger) change.
	if v, ok := template.ObjectMeta.Annotations[AnnotationSizingProfile]; ok {
		annotations[AnnotationSizingProfile] = v
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            podName,
			Namespace:       key.Namespace,
			Labels:          labels,
			Annotations:     annotations,
			OwnerReferences: []metav1.OwnerReference{target.OwnerRef()},
		},
		Spec: template.Spec,
	}

	if priorityClass != "" {
		pod.Spec.PriorityClassName = priorityClass
	}

	return pod
}

// injectWrapper delivers the GAG worker wrapper into the pod and points the
// runner container's command at it, so the runner image can be the unmodified
// upstream actions-runner (or any actions/runner-derived image) instead of a
// baked-in wrapper image (Q235). The binary is exposed at wrapperMountDir/wrapper
// either as a read-only OCI image volume (UseImageVolume; K8s ≥ 1.33, no init
// container) or copied into an emptyDir by an initContainer that self-installs
// from the wrapper image. Called from buildPod before applySecurityDefaults so
// the initContainer inherits the secure-by-default SecurityContext.
func (p *Provisioner) injectWrapper(spec *corev1.PodSpec, runnerIdx int) {
	if p.UseImageVolume {
		spec.Volumes = append(spec.Volumes, corev1.Volume{
			Name: wrapperVolumeName,
			VolumeSource: corev1.VolumeSource{
				Image: &corev1.ImageVolumeSource{
					Reference:  p.WrapperImage,
					PullPolicy: corev1.PullIfNotPresent,
				},
			},
		})
	} else {
		spec.Volumes = append(spec.Volumes, corev1.Volume{
			Name:         wrapperVolumeName,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		})
		// The scratch wrapper image has no shell/cp, so the binary copies itself
		// into the shared emptyDir via its `install` subcommand.
		spec.InitContainers = append(spec.InitContainers, corev1.Container{
			Name:    wrapperInitName,
			Image:   p.WrapperImage,
			Command: []string{"/" + wrapperBinName, "install", wrapperMountDir},
			VolumeMounts: []corev1.VolumeMount{{
				Name:      wrapperVolumeName,
				MountPath: wrapperMountDir,
			}},
		})
	}

	c := &spec.Containers[runnerIdx]
	c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{
		Name:      wrapperVolumeName,
		MountPath: wrapperMountDir,
		ReadOnly:  true,
	})
	// Override the image entrypoint with the injected wrapper. Clear Args too: a
	// command override drops the image's default CMD, and any tenant-set Args were
	// meant for the original entrypoint, not the wrapper.
	c.Command = []string{wrapperMountDir + "/" + wrapperBinName}
	c.Args = nil
}

// mergeEnvOverride appends or replaces env vars in base with those in overrides.
// Entries in overrides take precedence; base entries with the same Name are dropped.
func mergeEnvOverride(base, overrides []corev1.EnvVar) []corev1.EnvVar {
	names := make(map[string]struct{}, len(overrides))
	for _, e := range overrides {
		names[e.Name] = struct{}{}
	}
	result := make([]corev1.EnvVar, 0, len(base)+len(overrides))
	for _, e := range base {
		if _, drop := names[e.Name]; !drop {
			result = append(result, e)
		}
	}
	return append(result, overrides...)
}
