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

const (
	errVolumeType = "restricted volume types"
	errBaseline   = "exceeds the Pod Security Standards baseline level"
	errSAToken    = "projected serviceAccountToken source is reserved"
)

// podShapeCases is the shared table: wantErr is the namespaced kind's verdict.
func podShapeCases() []podShapeCase {
	unconfined := &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined}
	unmasked := corev1.UnmaskedProcMount
	root := int64(0)
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

		// The projected ServiceAccount token.
		{"projected serviceAccountToken", volume("creds", saTokenVolume()), errSAToken},

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
		{"root user", runnerSecurityContext(&corev1.SecurityContext{RunAsUser: &root}), ""},
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
// shape the namespaced kind loses, except the ServiceAccount token, which is a
// floor on both.
func TestClusterRunnerTemplate_PodShape_Create(t *testing.T) {
	v := &ClusterRunnerTemplateCustomValidator{}
	for _, tc := range podShapeCases() {
		t.Run(tc.name, func(t *testing.T) {
			crt := newClusterRunnerTemplate("golden", nil)
			crt.Spec = shapedSpec(tc.mutate)
			_, err := v.ValidateCreate(context.Background(), crt)
			if tc.wantErr != errSAToken {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), errSAToken)
		})
	}
}

// TestRunnerTemplate_PodShape_StoredValuePassesUnchanged covers the upgrade path:
// a template stored before the rules is admitted while podTemplate is unchanged,
// and re-checked the moment it changes.
func TestRunnerTemplate_PodShape_StoredValuePassesUnchanged(t *testing.T) {
	stored := map[string]podShapeCase{}
	for _, tc := range podShapeCases() {
		stored[tc.name] = tc
	}
	for _, name := range []string{"nfs volume", "projected serviceAccountToken", "capability outside the baseline set"} {
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
