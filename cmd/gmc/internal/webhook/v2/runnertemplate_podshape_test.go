package v2

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	agcv2 "github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/yaml"
)

// podShapeCase edits a clean single-container template into the shape under test.
type podShapeCase struct {
	name    string
	mutate  func(*corev1.PodTemplateSpec)
	wantErr string // empty: admitted
}

func volume(name string, src corev1.VolumeSource) func(*corev1.PodTemplateSpec) {
	return func(t *corev1.PodTemplateSpec) {
		t.Spec.Volumes = append(t.Spec.Volumes, corev1.Volume{Name: name, VolumeSource: src})
	}
}

func runnerSecurityContext(sc *corev1.SecurityContext) func(*corev1.PodTemplateSpec) {
	return func(t *corev1.PodTemplateSpec) { t.Spec.Containers[0].SecurityContext = sc }
}

func saTokenVolume() corev1.VolumeSource {
	return corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
		Sources: []corev1.VolumeProjection{
			{ConfigMap: &corev1.ConfigMapProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "settings"}}},
			{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token"}},
		},
	}}
}

func podCertificateVolume(signer string) corev1.VolumeSource {
	return corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
		Sources: []corev1.VolumeProjection{{PodCertificate: &corev1.PodCertificateProjection{
			SignerName: signer, KeyType: "ED25519", CredentialBundlePath: "credentials.pem",
		}}},
	}}
}

const (
	errVolumeType = "restricted volume types"
	errBaseline   = "exceeds the Pod Security Standards baseline level"
	errSAToken    = "projected serviceAccountToken source is reserved"
	errPodCert    = "projected podCertificate source with signer"
	errNodeName   = "may not bind its worker to a node"
	errScheduler  = "a namespaced RunnerTemplate may use only default-scheduler"
)

// bindsBothKinds reports whether wantErr comes from the API-credential rule, the
// one pod-shape rule the cluster-scoped kind is held to as well.
func bindsBothKinds(wantErr string) bool {
	return wantErr == errSAToken || wantErr == errPodCert
}

// survivesOptOut reports whether wantErr comes from a rule the
// --allow-unchecked-runner-template-pod-shape opt-out leaves in force: the
// API-credential rule and the scheduler-bypass rule, neither of which Pod Security
// Admission checks.
func survivesOptOut(wantErr string) bool {
	return bindsBothKinds(wantErr) || wantErr == errNodeName || wantErr == errScheduler
}

// podShapeCases is the shared table: wantErr is the namespaced kind's verdict.
func podShapeCases() []podShapeCase {
	unconfined := &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined}
	unmasked := corev1.UnmaskedProcMount
	root := int64(0)
	hostProcess := true
	return []podShapeCase{
		// Volume types (the restricted level's allowlist).
		{"nfs volume", volume("data", corev1.VolumeSource{NFS: &corev1.NFSVolumeSource{Server: "203.0.113.7", Path: "/export"}}), errVolumeType},
		{"iscsi volume", volume("data", corev1.VolumeSource{ISCSI: &corev1.ISCSIVolumeSource{TargetPortal: "203.0.113.7:3260", IQN: "iqn.2026-11.example:t", Lun: 0}}), errVolumeType},
		{"hostPath volume", volume("host", corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}}), errVolumeType},
		{"persistentVolumeClaim volume", volume("shared", corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "team-a-shared"}}), ""},
		{"emptyDir volume", volume("scratch", corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}), ""},
		{"generic ephemeral volume", volume("blk", corev1.VolumeSource{Ephemeral: &corev1.EphemeralVolumeSource{VolumeClaimTemplate: &corev1.PersistentVolumeClaimTemplate{}}}), ""},
		{"projected volume without a token", volume("cfg", corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
			Sources: []corev1.VolumeProjection{{ConfigMap: &corev1.ConfigMapProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "settings"}}}},
		}}), ""},

		// A projected Kubernetes API credential.
		{"projected serviceAccountToken", volume("creds", saTokenVolume()), errSAToken},
		{"projected podCertificate, the apiserver client signer", volume("creds", podCertificateVolume("kubernetes.io/kube-apiserver-client-pod")), errPodCert},
		{"projected podCertificate, another reserved signer", volume("creds", podCertificateVolume("kubernetes.io/not-yet-defined")), errPodCert},
		{"projected podCertificate, a workload signer", volume("mtls", podCertificateVolume("example.com/workload-mtls")), ""},

		// The baseline level.
		{"capability outside the baseline set", runnerSecurityContext(&corev1.SecurityContext{
			Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"SYS_ADMIN"}},
		}), errBaseline},
		{"capability inside the baseline set", runnerSecurityContext(&corev1.SecurityContext{
			Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"NET_BIND_SERVICE"}},
		}), ""},
		{"capability on an init container", func(t *corev1.PodTemplateSpec) {
			t.Spec.InitContainers = []corev1.Container{{Name: "dind", SecurityContext: &corev1.SecurityContext{
				Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"NET_ADMIN"}},
			}}}
		}, errBaseline},
		{"container seccomp Unconfined", runnerSecurityContext(&corev1.SecurityContext{SeccompProfile: unconfined}), errBaseline},
		{"pod seccomp Unconfined", func(t *corev1.PodTemplateSpec) {
			t.Spec.SecurityContext = &corev1.PodSecurityContext{SeccompProfile: unconfined}
		}, errBaseline},
		{"AppArmor Unconfined", runnerSecurityContext(&corev1.SecurityContext{
			AppArmorProfile: &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeUnconfined},
		}), errBaseline},
		{"procMount Unmasked", runnerSecurityContext(&corev1.SecurityContext{ProcMount: &unmasked}), errBaseline},
		{"hostPort", func(t *corev1.PodTemplateSpec) {
			t.Spec.Containers[0].Ports = []corev1.ContainerPort{{ContainerPort: 8080, HostPort: 8080}}
		}, errBaseline},
		{"unsafe sysctl", func(t *corev1.PodTemplateSpec) {
			t.Spec.SecurityContext = &corev1.PodSecurityContext{Sysctls: []corev1.Sysctl{{Name: "kernel.msgmax", Value: "65536"}}}
		}, errBaseline},
		{"AppArmor unconfined annotation", func(t *corev1.PodTemplateSpec) {
			t.Annotations = map[string]string{"container.apparmor.security.beta.kubernetes.io/runner": "unconfined"}
		}, errBaseline},
		{"SELinux type outside the baseline set", runnerSecurityContext(&corev1.SecurityContext{
			SELinuxOptions: &corev1.SELinuxOptions{Type: "spc_t"},
		}), errBaseline},
		{"Windows hostProcess", runnerSecurityContext(&corev1.SecurityContext{
			WindowsOptions: &corev1.WindowsSecurityContextOptions{HostProcess: &hostProcess},
		}), errBaseline},
		{"probe host", func(t *corev1.PodTemplateSpec) {
			t.Spec.Containers[0].LivenessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{Host: "169.254.169.254", Port: intstr.FromInt32(80)},
			}}
		}, errBaseline},
		{"lifecycle hook host", func(t *corev1.PodTemplateSpec) {
			t.Spec.Containers[0].Lifecycle = &corev1.Lifecycle{PostStart: &corev1.LifecycleHandler{
				HTTPGet: &corev1.HTTPGetAction{Host: "169.254.169.254", Port: intstr.FromInt32(80)},
			}}
		}, errBaseline},
		{"root user", runnerSecurityContext(&corev1.SecurityContext{RunAsUser: &root}), ""},

		// Placement past the default scheduler (Q1347).
		{"nodeName", func(t *corev1.PodTemplateSpec) { t.Spec.NodeName = "controller-node" }, errNodeName},
		{"another schedulerName", func(t *corev1.PodTemplateSpec) { t.Spec.SchedulerName = "other-scheduler" }, errScheduler},
		{"schedulerName default-scheduler", func(t *corev1.PodTemplateSpec) { t.Spec.SchedulerName = corev1.DefaultSchedulerName }, ""},
	}
}

func shapedSpec(mutate func(*corev1.PodTemplateSpec)) agcv2.RunnerTemplateSpec {
	spec := *specWith([]corev1.Container{{Name: "runner"}}, nil)
	mutate(&spec.PodTemplate)
	return spec
}

func TestRunnerTemplate_PodShape_Create(t *testing.T) {
	v := &RunnerTemplateCustomValidator{}
	for _, tc := range podShapeCases() {
		t.Run(tc.name, func(t *testing.T) {
			rt := newRunnerTemplate("team-a", "rt", nil)
			rt.Spec = shapedSpec(tc.mutate)
			_, err := v.ValidateCreate(context.Background(), rt)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// TestClusterRunnerTemplate_PodShape_Create: the platform-authored kind keeps every
// shape the namespaced kind loses, nodeName and schedulerName included, except a
// projected API credential, which is a floor on both.
func TestClusterRunnerTemplate_PodShape_Create(t *testing.T) {
	v := &ClusterRunnerTemplateCustomValidator{}
	for _, tc := range podShapeCases() {
		t.Run(tc.name, func(t *testing.T) {
			crt := newClusterRunnerTemplate("golden", nil)
			crt.Spec = shapedSpec(tc.mutate)
			_, err := v.ValidateCreate(context.Background(), crt)
			if !bindsBothKinds(tc.wantErr) {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// TestRunnerTemplate_PodShape_Unchecked covers the platform opt-out: the baseline and
// volume-type rules are skipped, and the rules that are floors on every install
// (a projected API credential, scheduler bypass, a privileged container) are not.
func TestRunnerTemplate_PodShape_Unchecked(t *testing.T) {
	v := &RunnerTemplateCustomValidator{AllowUncheckedPodShape: true}
	for _, tc := range podShapeCases() {
		t.Run(tc.name, func(t *testing.T) {
			rt := newRunnerTemplate("team-a", "rt", nil)
			rt.Spec = shapedSpec(tc.mutate)
			_, err := v.ValidateCreate(context.Background(), rt)
			if !survivesOptOut(tc.wantErr) {
				require.NoError(t, err, "the opt-out admits what the baseline and volume-type rules refuse")
				return
			}
			require.Error(t, err, "the API-credential and scheduler-bypass rules are not part of the opt-out")
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}

	t.Run("a privileged container is still refused", func(t *testing.T) {
		privileged := true
		rt := newRunnerTemplate("team-a", "rt", nil)
		rt.Spec = shapedSpec(runnerSecurityContext(&corev1.SecurityContext{Privileged: &privileged}))
		_, err := v.ValidateCreate(context.Background(), rt)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "privileged containers are not permitted")
	})
}

// TestPodShapeVersion_IsPinned: the rules evaluate at a named Pod Security
// Standards version. At the library's latest, a dependency bump carrying a stricter
// upstream check would refuse a manifest the previous release admitted.
func TestPodShapeVersion_IsPinned(t *testing.T) {
	assert.False(t, podShapeVersion.Latest(), "evaluate at a pinned version, not the vendored library's latest")
	assert.Equal(t, "v1.37", podShapeVersion.String())
	assert.Equal(t, podShapeVersion, baselineLevel.Version)
	assert.Equal(t, podShapeVersion, restrictedLevel.Version)
}

// TestRunnerTemplate_PodShape_StoredValuePassesUnchanged covers the upgrade path:
// a template stored before the rules is admitted while podTemplate is unchanged,
// and re-checked the moment it changes.
func TestRunnerTemplate_PodShape_StoredValuePassesUnchanged(t *testing.T) {
	stored := map[string]podShapeCase{}
	for _, tc := range podShapeCases() {
		stored[tc.name] = tc
	}
	for _, name := range []string{
		"nfs volume", "projected serviceAccountToken", "projected podCertificate, the apiserver client signer",
		"capability outside the baseline set", "nodeName", "another schedulerName",
	} {
		tc := stored[name]
		require.NotEmpty(t, tc.wantErr, "the case must be one a create refuses")

		t.Run(name, func(t *testing.T) {
			v := &RunnerTemplateCustomValidator{}
			oldObj := newRunnerTemplate("team-a", "rt", nil)
			oldObj.Spec = shapedSpec(tc.mutate)

			t.Run("an edit outside podTemplate is admitted", func(t *testing.T) {
				newObj := oldObj.DeepCopy()
				newObj.Spec.WorkerImage = "ghcr.io/example/runner:2"
				newObj.Labels = map[string]string{"team": "a"}
				_, err := v.ValidateUpdate(context.Background(), oldObj, newObj)
				require.NoError(t, err)
			})

			t.Run("an edit inside podTemplate is re-checked", func(t *testing.T) {
				newObj := oldObj.DeepCopy()
				newObj.Spec.PodTemplate.Spec.Containers[0].Image = "ghcr.io/example/runner:2"
				_, err := v.ValidateUpdate(context.Background(), oldObj, newObj)
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
			})

			t.Run("an update cannot introduce it", func(t *testing.T) {
				clean := newRunnerTemplate("team-a", "rt", []corev1.Container{{Name: "runner"}})
				_, err := v.ValidateUpdate(context.Background(), clean, oldObj)
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
			})
		})
	}

	t.Run("the older rules still re-check a stored template", func(t *testing.T) {
		v := &RunnerTemplateCustomValidator{}
		oldObj := rtWithPriorityClass("removed-class")
		newObj := oldObj.DeepCopy()
		newObj.Labels = map[string]string{"team": "a"}
		_, err := v.ValidateUpdate(context.Background(), oldObj, newObj)
		require.Error(t, err, "the unchanged-podTemplate pass covers only the pod-shape rules")
	})
}

func TestClusterRunnerTemplate_PodShape_StoredValuePassesUnchanged(t *testing.T) {
	v := &ClusterRunnerTemplateCustomValidator{}
	oldObj := newClusterRunnerTemplate("golden", nil)
	oldObj.Spec = shapedSpec(volume("creds", saTokenVolume()))

	t.Run("an edit outside podTemplate is admitted", func(t *testing.T) {
		newObj := oldObj.DeepCopy()
		newObj.Spec.WorkerImage = "ghcr.io/example/runner:2"
		_, err := v.ValidateUpdate(context.Background(), oldObj, newObj)
		require.NoError(t, err)
	})

	t.Run("an edit inside podTemplate is re-checked", func(t *testing.T) {
		newObj := oldObj.DeepCopy()
		newObj.Spec.PodTemplate.Spec.Containers[0].Image = "ghcr.io/example/runner:2"
		_, err := v.ValidateUpdate(context.Background(), oldObj, newObj)
		require.Error(t, err)
		assert.Contains(t, err.Error(), errSAToken)
	})
}

// templateLibraryDir is deploy/templates, read through a committed testdata/ symlink
// so the read resolves inside this module root and go keeps it in the test cache key.
const templateLibraryDir = "testdata/templates"

// TestTemplateLibrary_PodShape reads the shipped deploy/templates entries: each is
// admitted as the ClusterRunnerTemplate it ships as, and the two DinD entries are
// what the namespaced kind now refuses, which is why they ship cluster-scoped.
func TestTemplateLibrary_PodShape(t *testing.T) {
	library := templateLibraryDir
	namespacedErr := map[string]string{
		"plain":           "",
		"kata-dind":       errBaseline,
		"privileged-dind": "privileged containers are not permitted",
	}
	entries, err := os.ReadDir(library)
	require.NoError(t, err)
	seen := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		seen++
		t.Run(e.Name(), func(t *testing.T) {
			wantErr, known := namespacedErr[e.Name()]
			require.True(t, known, "new library entry: say whether the namespaced kind admits its shape")

			raw, err := os.ReadFile(filepath.Join(library, e.Name(), "template.yaml"))
			require.NoError(t, err)
			crt := &agcv2.ClusterRunnerTemplate{}
			require.NoError(t, yaml.UnmarshalStrict(raw, crt))
			require.Equal(t, "ClusterRunnerTemplate", crt.Kind)

			_, err = (&ClusterRunnerTemplateCustomValidator{}).ValidateCreate(context.Background(), crt)
			require.NoError(t, err, "the entry must apply as shipped")

			rt := &agcv2.RunnerTemplate{Spec: crt.Spec}
			_, err = (&RunnerTemplateCustomValidator{}).ValidateCreate(context.Background(), rt)
			if wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), wantErr)
		})
	}
	assert.Equal(t, len(namespacedErr), seen, "every library entry is covered")
}
