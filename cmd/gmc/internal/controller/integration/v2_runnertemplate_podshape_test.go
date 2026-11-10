//go:build integration

package integration_test

import (
	"context"
	"strings"
	"testing"
	"time"

	agcv2 "github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The Q1322 pod-shape rules through the real apiserver: a namespaced RunnerTemplate
// must meet the Pod Security Standards baseline level and the restricted level's
// volume types, and neither template kind may project a ServiceAccount token. The
// two TestUpstream tests pin the apiserver behaviour the rules exist for.

const (
	podShapeVolumeTypeMsg = "restricted volume types"
	podShapeBaselineMsg   = "exceeds the Pod Security Standards baseline level"
	podShapeSATokenMsg    = "projected serviceAccountToken source is reserved"
)

func nfsVolume() corev1.Volume {
	return corev1.Volume{Name: "data", VolumeSource: corev1.VolumeSource{
		NFS: &corev1.NFSVolumeSource{Server: "203.0.113.7", Path: "/export"},
	}}
}

func iscsiVolume() corev1.Volume {
	return corev1.Volume{Name: "data", VolumeSource: corev1.VolumeSource{
		ISCSI: &corev1.ISCSIVolumeSource{TargetPortal: "203.0.113.7:3260", IQN: "iqn.2026-11.com.example:target", Lun: 0},
	}}
}

func saTokenVolume() corev1.Volume {
	return corev1.Volume{Name: "creds", VolumeSource: corev1.VolumeSource{
		Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{{
			ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token"},
		}}},
	}}
}

// podShapes are the three shapes the rules refuse, one per rule, keyed by the
// message the namespaced kind answers with.
func podShapes() map[string]func(*agcv2.RunnerTemplateSpec) {
	return map[string]func(*agcv2.RunnerTemplateSpec){
		podShapeVolumeTypeMsg: func(s *agcv2.RunnerTemplateSpec) {
			s.PodTemplate.Spec.Volumes = []corev1.Volume{nfsVolume()}
		},
		podShapeBaselineMsg: func(s *agcv2.RunnerTemplateSpec) {
			s.PodTemplate.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{
				Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"SYS_ADMIN"}},
			}
		},
		podShapeSATokenMsg: func(s *agcv2.RunnerTemplateSpec) {
			s.PodTemplate.Spec.Volumes = []corev1.Volume{saTokenVolume()}
		},
	}
}

func TestV2_RunnerTemplate_PodShape_RejectsNewValue(t *testing.T) {
	const ns = "v2-rt-podshape"
	createNamespace(t, ns)

	i := 0
	for msg, shape := range podShapes() {
		i++
		rt := runnerTemplateWithContainer(ns, "shape-"+string(rune('a'+i)), corev1.Container{Name: "runner"})
		shape(&rt.Spec)
		err := k8sClient.Create(ctx, rt)
		require.Error(t, err, "a namespaced RunnerTemplate must be refused: %s", msg)
		assert.Contains(t, err.Error(), msg)
	}

	pvc := runnerTemplateWithContainer(ns, "pvc", corev1.Container{Name: "runner"})
	pvc.Spec.PodTemplate.Spec.Volumes = []corev1.Volume{{Name: "shared", VolumeSource: corev1.VolumeSource{
		PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "team-a-shared"},
	}}}
	require.NoError(t, k8sClient.Create(ctx, pvc), "a PersistentVolumeClaim volume stays admitted")
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), pvc) })
}

// TestV2_ClusterRunnerTemplate_PodShape: the platform-authored kind keeps the
// shapes the namespaced kind loses and is refused only the ServiceAccount token.
func TestV2_ClusterRunnerTemplate_PodShape(t *testing.T) {
	for msg, shape := range podShapes() {
		name := "podshape-" + map[string]string{
			podShapeVolumeTypeMsg: "nfs", podShapeBaselineMsg: "caps", podShapeSATokenMsg: "token",
		}[msg]
		crt := clusterRunnerTemplateWithContainer(name, corev1.Container{Name: "runner"})
		shape(&crt.Spec)
		err := k8sClient.Create(ctx, crt)
		if msg != podShapeSATokenMsg {
			require.NoError(t, err, "a ClusterRunnerTemplate may carry this shape: %s", msg)
			t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), crt) })
			continue
		}
		require.Error(t, err, "the ServiceAccount token is refused on the cluster-scoped kind too")
		assert.Contains(t, err.Error(), podShapeSATokenMsg)
	}
}

// podShapeUncheckedLabel marks a template the two RunnerTemplate webhooks skip while
// bypassRunnerTemplateAdmission is in force.
const podShapeUncheckedLabel = "q1322.test/stored-before-the-rule"

// bypassRunnerTemplateAdmission scopes both RunnerTemplate webhooks away from
// objects labelled podShapeUncheckedLabel and returns the function that restores them.
// A labelled object created in between is one the rules never saw: a template
// stored before v2.0.0. The apiserver matches an update when either the old or
// the new object matches, so removing the label on update reaches the webhook again
// even before the restore lands.
func bypassRunnerTemplateAdmission(t *testing.T) (restore func()) {
	t.Helper()
	const configName = "validating-webhook-configuration"
	bypassed := map[string]bool{"vrunnertemplate-v2.kb.io": true, "vclusterrunnertemplate-v2.kb.io": true}

	var original admissionv1.ValidatingWebhookConfiguration
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: configName}, &original))
	originals := map[string]*metav1.LabelSelector{}
	for i := range original.Webhooks {
		if bypassed[original.Webhooks[i].Name] {
			originals[original.Webhooks[i].Name] = original.Webhooks[i].ObjectSelector
		}
	}
	require.Len(t, originals, len(bypassed), "both template webhook entries must be installed for the bypass to mean anything")

	patch := func(selector func(string) *metav1.LabelSelector) {
		require.Eventually(t, func() bool {
			var cfg admissionv1.ValidatingWebhookConfiguration
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: configName}, &cfg); err != nil {
				return false
			}
			for i := range cfg.Webhooks {
				if bypassed[cfg.Webhooks[i].Name] {
					cfg.Webhooks[i].ObjectSelector = selector(cfg.Webhooks[i].Name)
				}
			}
			return k8sClient.Update(ctx, &cfg) == nil
		}, 10*time.Second, 100*time.Millisecond, "patching %s must succeed", configName)
	}

	restore = func() { patch(func(name string) *metav1.LabelSelector { return originals[name] }) }
	t.Cleanup(restore)
	patch(func(string) *metav1.LabelSelector {
		return &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
			Key:      podShapeUncheckedLabel,
			Operator: metav1.LabelSelectorOpDoesNotExist,
		}}}
	})
	return restore
}

// requireStoredTemplateKeepsWorking drives one stored template through the upgrade
// path: stored is already in the cluster carrying a shape the rule refuses.
func requireStoredTemplateKeepsWorking(t *testing.T, stored, probe client.Object, spec func(client.Object) *agcv2.RunnerTemplateSpec, msg string) {
	t.Helper()
	key := client.ObjectKeyFromObject(stored)

	// The restored webhook refuses the same shape on a new object. Retried because the
	// apiserver caches webhook configuration.
	require.Eventually(t, func() bool {
		fresh := probe.DeepCopyObject().(client.Object)
		err := k8sClient.Create(ctx, fresh)
		if err == nil {
			_ = k8sClient.Delete(ctx, fresh)
		}
		return err != nil && strings.Contains(err.Error(), msg)
	}, 30*time.Second, 200*time.Millisecond, "the restored webhook must refuse a new object: %s", msg)

	update := func(edit func(client.Object)) error {
		got := stored.DeepCopyObject().(client.Object)
		if err := k8sClient.Get(ctx, key, got); err != nil {
			return err
		}
		edit(got)
		return k8sClient.Update(ctx, got)
	}

	// Dropping the bypass label is itself an edit outside podTemplate, and from here
	// the object matches the webhook whatever the selector says.
	require.NoError(t, update(func(o client.Object) { o.SetLabels(nil) }),
		"an edit outside podTemplate must be admitted")
	require.NoError(t, update(func(client.Object) {}), "re-applying it unchanged must be admitted")
	require.NoError(t, update(func(o client.Object) { spec(o).WorkerImage = "ghcr.io/example/runner:2" }),
		"editing workerImage must be admitted")

	err := update(func(o client.Object) { spec(o).PodTemplate.Spec.Containers[0].Image = "ghcr.io/example/runner:3" })
	require.Error(t, err, "an edit inside podTemplate must be re-checked")
	assert.Contains(t, err.Error(), msg)
}

// TestV2_RunnerTemplate_PodShape_StoredValueKeepsWorking: a template stored before
// the rules takes edits that leave podTemplate alone, on both kinds.
func TestV2_RunnerTemplate_PodShape_StoredValueKeepsWorking(t *testing.T) {
	const ns = "v2-rt-podshape-stored"
	createNamespace(t, ns)
	restore := bypassRunnerTemplateAdmission(t)
	bypass := map[string]string{podShapeUncheckedLabel: "true"}

	names := map[string]string{podShapeVolumeTypeMsg: "nfs", podShapeBaselineMsg: "caps", podShapeSATokenMsg: "token"}
	type storedCase struct {
		stored, probe client.Object
		spec          func(client.Object) *agcv2.RunnerTemplateSpec
		msg           string
	}
	var cases []storedCase
	for msg, shape := range podShapes() {
		stored := runnerTemplateWithContainer(ns, "stored-"+names[msg], corev1.Container{Name: "runner"})
		shape(&stored.Spec)
		probe := stored.DeepCopy()
		probe.Name = "probe-" + names[msg]
		stored.Labels = bypass
		requireCreateEventually(t, stored)
		t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), stored) })
		cases = append(cases, storedCase{stored, probe, func(o client.Object) *agcv2.RunnerTemplateSpec {
			return &o.(*agcv2.RunnerTemplate).Spec
		}, msg})
	}

	clusterStored := clusterRunnerTemplateWithContainer("podshape-stored-token", corev1.Container{Name: "runner"})
	podShapes()[podShapeSATokenMsg](&clusterStored.Spec)
	clusterProbe := clusterStored.DeepCopy()
	clusterProbe.Name = "podshape-probe-token"
	clusterStored.Labels = bypass
	requireCreateEventually(t, clusterStored)
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), clusterStored) })
	cases = append(cases, storedCase{clusterStored, clusterProbe, func(o client.Object) *agcv2.RunnerTemplateSpec {
		return &o.(*agcv2.ClusterRunnerTemplate).Spec
	}, podShapeSATokenMsg})

	restore()
	for _, c := range cases {
		requireStoredTemplateKeepsWorking(t, c.stored, c.probe, c.spec, c.msg)
	}
}

func podWithVolume(ns, name string, v corev1.Volume) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "c", Image: "registry.k8s.io/pause:3.10"}},
			Volumes:    []corev1.Volume{v},
		},
	}
}

// TestUpstream_PodSecurity_BaselineAdmitsNetworkVolumes pins the apiserver behaviour
// the volume-type rule exists for: Pod Security Admission at baseline forbids
// hostPath and admits nfs and iscsi, so in a baseline namespace nothing but the
// RunnerTemplate webhook stands between a tenant template and a node-mounted
// network volume. The hostPath case is the control that PSA is enforcing here at all.
func TestUpstream_PodSecurity_BaselineAdmitsNetworkVolumes(t *testing.T) {
	volumes := map[string]corev1.Volume{
		"nfs":   nfsVolume(),
		"iscsi": iscsiVolume(),
		"hostpath": {Name: "data", VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{Path: "/var/lib"},
		}},
	}
	for level, admitted := range map[string]map[string]bool{
		"baseline":   {"nfs": true, "iscsi": true, "hostpath": false},
		"restricted": {"nfs": false, "iscsi": false, "hostpath": false},
	} {
		ns := "upstream-psa-volumes-" + level
		createNamespaceWithLabels(t, ns, map[string]string{
			"pod-security.kubernetes.io/enforce":         level,
			"pod-security.kubernetes.io/enforce-version": "latest",
		})
		for kind, want := range admitted {
			pod := podWithVolume(ns, kind, volumes[kind])
			err := k8sClient.Create(ctx, pod)
			if want {
				require.NoError(t, err, "PSA %s must admit a pod with a %s volume", level, kind)
				t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), pod) })
				continue
			}
			require.True(t, apierrors.IsForbidden(err), "PSA %s must forbid a pod with a %s volume, got: %v", level, kind, err)
			assert.Contains(t, err.Error(), "violates PodSecurity")
		}
	}
}

// TestUpstream_ProjectedServiceAccountTokenSurvivesAutomountFalse pins the apiserver
// behaviour the ServiceAccount token rule exists for: automountServiceAccountToken:
// false does not stop a pod declaring its own projected serviceAccountToken source,
// at any Pod Security level. Minting the token is the kubelet's and is not observed
// here.
func TestUpstream_ProjectedServiceAccountTokenSurvivesAutomountFalse(t *testing.T) {
	const ns = "upstream-sa-token-projection"
	createNamespaceWithLabels(t, ns, map[string]string{
		"pod-security.kubernetes.io/enforce":         "baseline",
		"pod-security.kubernetes.io/enforce-version": "latest",
	})

	no := false
	pod := podWithVolume(ns, "token", saTokenVolume())
	pod.Spec.AutomountServiceAccountToken = &no
	// API validation refuses the source on a pod that names no ServiceAccount. The AGC
	// always names the worker one, and outside envtest admission defaults it.
	pod.Spec.ServiceAccountName = "worker"
	require.NoError(t, k8sClient.Create(ctx, pod))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), pod) })

	var got corev1.Pod
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), &got))
	require.Len(t, got.Spec.Volumes, 1, "nothing else was mounted, so automount really is off")
	require.NotNil(t, got.Spec.Volumes[0].Projected)
	require.Len(t, got.Spec.Volumes[0].Projected.Sources, 1)
	assert.NotNil(t, got.Spec.Volumes[0].Projected.Sources[0].ServiceAccountToken,
		"the apiserver stores the token projection as written")

	// What the AGC leaves behind when the token was the volume's only source: a
	// projected volume with no sources, which the apiserver must still admit or the
	// removal would cost the tenant the whole worker pod.
	emptied := podWithVolume(ns, "emptied", corev1.Volume{Name: "creds", VolumeSource: corev1.VolumeSource{
		Projected: &corev1.ProjectedVolumeSource{},
	}})
	emptied.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "creds", MountPath: "/var/run/creds"}}
	require.NoError(t, k8sClient.Create(ctx, emptied), "a projected volume with no sources must be admitted")
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), emptied) })
}
