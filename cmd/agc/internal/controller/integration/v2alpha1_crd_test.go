//go:build integration

package integration_test

import (
	"strconv"
	"strings"
	"testing"

	agcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// serverMinor returns the apiserver's minor version (e.g. 31, 35). CRD field
// selectors (KEP-4358) are alpha-off in k8s 1.30 and only queryable on 1.31+;
// the CI integration tier now runs envtest 1.35 (bumped for M3b's field-selector
// scoping), but the gate stays so the assertion self-skips on an older local
// apiserver instead of failing spuriously.
func serverMinor(t *testing.T) int {
	t.Helper()
	dc, err := discovery.NewDiscoveryClientForConfig(testEnv.Config)
	require.NoError(t, err)
	info, err := dc.ServerVersion()
	require.NoError(t, err)
	minor := strings.TrimRight(info.Minor, "+")
	n, err := strconv.Atoi(minor)
	require.NoError(t, err, "parse server minor %q", info.Minor)
	return n
}

// These tests prove the v2alpha1 (actions-gateway.com) AGC kinds install into the
// real apiserver and round-trip alongside v1alpha1 (Q149, M1 exit criterion), and
// that the CEL/structural validation behaves under real-apiserver semantics —
// defaulting, the gatewayRef selectable field, and CEL rejections that only the
// apiserver applies. No reconciler is exercised: M1 is the API foundation only.

func newV2RunnerTemplate(ns, name string) *agcv2alpha1.RunnerTemplate {
	return &agcv2alpha1.RunnerTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: agcv2alpha1.RunnerTemplateSpec{
			PodTemplate: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "runner", Image: "runner:latest"}},
				},
			},
			WorkerImage: "runner:latest",
		},
	}
}

func newV2RunnerSet(ns, name, gateway, template string) *agcv2alpha1.RunnerSet {
	return &agcv2alpha1.RunnerSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: agcv2alpha1.RunnerSetSpec{
			GatewayRef:  agcv2alpha1.ObjectRef{Name: gateway},
			TemplateRef: &agcv2alpha1.ObjectRef{Name: template},
			// Pin Classic explicitly: the default is ScaleSet (Q264 P5), which forbids
			// this fixture's two labels, and these CRD/reconciler tests exercise the
			// classic path. Tests asserting the new default omit or override this field.
			AcquisitionProtocol: agcv2alpha1.AcquisitionProtocolClassic,
			RunnerLabels:        []string{"self-hosted", "linux"},
		},
	}
}

func TestV2_RunnerSet_RoundTripAndDefaulting(t *testing.T) {
	const ns = "v2-runnerset-rt"
	createNSForAGC(t, ns)

	rs := newV2RunnerSet(ns, "linux", "acme", "default")
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, rs) })

	var got agcv2alpha1.RunnerSet
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "linux"}, &got))

	// maxListeners defaults to 10 in v2 (was 1 in v1alpha1) — applied by the apiserver.
	assert.Equal(t, int32(10), got.Spec.MaxListeners, "maxListeners should default to 10")
	assert.Equal(t, "acme", got.Spec.GatewayRef.Name)
	assert.Equal(t, "default", got.Spec.TemplateRef.Name)
}

func TestV2_RunnerSet_GatewayRefSelectableField(t *testing.T) {
	if m := serverMinor(t); m < 31 {
		t.Skipf("CRD field selectors (KEP-4358) are queryable only on k8s >= 1.31; apiserver is 1.%d", m)
	}

	const ns = "v2-runnerset-field"
	createNSForAGC(t, ns)

	a := newV2RunnerSet(ns, "set-a", "gw-a", "tmpl")
	b := newV2RunnerSet(ns, "set-b", "gw-b", "tmpl")
	require.NoError(t, k8sClient.Create(ctx, a))
	require.NoError(t, k8sClient.Create(ctx, b))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, a); _ = k8sClient.Delete(ctx, b) })

	// The CRD declares spec.gatewayRef.name a selectable field (KEP-4358), so the
	// apiserver filters server-side — the mechanism M3b's AGC watch-scoping relies on.
	var list agcv2alpha1.RunnerSetList
	require.NoError(t, k8sClient.List(ctx, &list,
		client.InNamespace(ns),
		client.MatchingFields{"spec.gatewayRef.name": "gw-a"}))
	require.Len(t, list.Items, 1)
	assert.Equal(t, "set-a", list.Items[0].Name)
}

func TestV2_RunnerSet_NameMaxLengthRejected(t *testing.T) {
	const ns = "v2-runnerset-name"
	createNSForAGC(t, ns)

	// 54 chars — over the 52-char budget the CEL root rule enforces.
	longName := "a23456789012345678901234567890123456789012345678901234"
	require.Len(t, longName, 54)
	rs := newV2RunnerSet(ns, longName, "acme", "default")
	err := k8sClient.Create(ctx, rs)
	require.Error(t, err)
	assert.True(t, apierrors.IsInvalid(err), "expected Invalid for over-length name, got %v", err)
}

func TestV2_RunnerSet_MaxWorkersMustMatchLastTier(t *testing.T) {
	const ns = "v2-runnerset-tiers"
	createNSForAGC(t, ns)

	rs := newV2RunnerSet(ns, "tiers", "acme", "default")
	mw := int32(5) // does not match the last tier threshold (10)
	rs.Spec.MaxWorkers = &mw
	rs.Spec.PriorityTiers = []agcv2alpha1.PriorityTier{{PriorityClassName: "pc", Threshold: 10}}
	err := k8sClient.Create(ctx, rs)
	require.Error(t, err)
	assert.True(t, apierrors.IsInvalid(err), "expected Invalid for maxWorkers != last tier, got %v", err)
}

func TestV2_RunnerSet_AcquisitionProtocolDefaultsScaleSet(t *testing.T) {
	const ns = "v2-runnerset-acqproto-default"
	createNSForAGC(t, ns)

	// Omit acquisitionProtocol entirely — as of Q264 P5 the apiserver must default it
	// to ScaleSet. A bare set must therefore carry a single label (ScaleSet ⇒ one
	// runnerLabel), so this fixture declares one.
	rs := newV2RunnerSet(ns, "linux", "acme", "default")
	rs.Spec.AcquisitionProtocol = "" // clear the helper's Classic pin to test the default
	rs.Spec.RunnerLabels = []string{"self-hosted"}
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, rs) })

	var got agcv2alpha1.RunnerSet
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "linux"}, &got))
	assert.Equal(t, agcv2alpha1.AcquisitionProtocolScaleSet, got.Spec.AcquisitionProtocol,
		"acquisitionProtocol must default to ScaleSet (Q264 P5)")
}

// TestV2_RunnerSet_BareMultiLabelRejected documents the user-visible consequence of
// the Q264 P5 default flip: a runner set that omits acquisitionProtocol AND declares
// more than one runnerLabel is now rejected — it defaults to ScaleSet, which requires
// exactly one label. Such a set must set acquisitionProtocol: Classic explicitly (the
// deprecated multi-label path) or reduce to a single label.
func TestV2_RunnerSet_BareMultiLabelRejected(t *testing.T) {
	const ns = "v2-runnerset-acqproto-baremulti"
	createNSForAGC(t, ns)

	bare := newV2RunnerSet(ns, "multi", "acme", "default") // two labels
	bare.Spec.AcquisitionProtocol = ""                     // omit → defaults to ScaleSet
	err := k8sClient.Create(ctx, bare)
	require.Error(t, err)
	assert.True(t, apierrors.IsInvalid(err),
		"bare multi-label set should default to ScaleSet and be Invalid, got %v", err)

	// The same labels are accepted when the set opts into the deprecated Classic path.
	classic := newV2RunnerSet(ns, "multi-classic", "acme", "default") // helper pins Classic
	require.NoError(t, k8sClient.Create(ctx, classic))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, classic) })
}

func TestV2_RunnerSet_ScaleSetRequiresSingleLabel(t *testing.T) {
	const ns = "v2-runnerset-acqproto-label"
	createNSForAGC(t, ns)

	// A ScaleSet set with more than one runnerLabel is rejected by the spec-level CEL
	// rule: the scale set's name is its single runs-on label (Q264 §5a-U7).
	multi := newV2RunnerSet(ns, "multi", "acme", "default") // labels: self-hosted, linux
	multi.Spec.AcquisitionProtocol = agcv2alpha1.AcquisitionProtocolScaleSet
	err := k8sClient.Create(ctx, multi)
	require.Error(t, err)
	assert.True(t, apierrors.IsInvalid(err), "ScaleSet with >1 label should be Invalid, got %v", err)

	// Exactly one label is accepted.
	single := newV2RunnerSet(ns, "single", "acme", "default")
	single.Spec.AcquisitionProtocol = agcv2alpha1.AcquisitionProtocolScaleSet
	single.Spec.RunnerLabels = []string{"scale-set-linux"}
	require.NoError(t, k8sClient.Create(ctx, single))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, single) })
}

func TestV2_RunnerSet_AcquisitionProtocolImmutable(t *testing.T) {
	const ns = "v2-runnerset-acqproto-immutable"
	createNSForAGC(t, ns)

	rs := newV2RunnerSet(ns, "linux", "acme", "default")
	rs.Spec.RunnerLabels = []string{"scale-set-linux"}
	rs.Spec.AcquisitionProtocol = agcv2alpha1.AcquisitionProtocolScaleSet
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, rs) })

	var got agcv2alpha1.RunnerSet
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "linux"}, &got))
	got.Spec.AcquisitionProtocol = agcv2alpha1.AcquisitionProtocolClassic
	err := k8sClient.Update(ctx, &got)
	require.Error(t, err)
	assert.True(t, apierrors.IsInvalid(err), "switching acquisitionProtocol should be Invalid, got %v", err)
}

func TestV2_RunnerTemplate_RoundTrip(t *testing.T) {
	const ns = "v2-runnertemplate-rt"
	createNSForAGC(t, ns)

	rt := newV2RunnerTemplate(ns, "default")
	require.NoError(t, k8sClient.Create(ctx, rt))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, rt) })

	var got agcv2alpha1.RunnerTemplate
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "default"}, &got))
	assert.Equal(t, "runner:latest", got.Spec.WorkerImage)
	require.Len(t, got.Spec.PodTemplate.Spec.Containers, 1)
}

func TestV2_RunnerTemplate_ReservedPodFieldsRejected(t *testing.T) {
	const ns = "v2-runnertemplate-reserved"
	createNSForAGC(t, ns)

	cases := map[string]func(*agcv2alpha1.RunnerTemplate){
		"hostPID": func(rt *agcv2alpha1.RunnerTemplate) {
			rt.Spec.PodTemplate.Spec.HostPID = true
		},
		"hostNetwork": func(rt *agcv2alpha1.RunnerTemplate) {
			rt.Spec.PodTemplate.Spec.HostNetwork = true
		},
		"hostIPC": func(rt *agcv2alpha1.RunnerTemplate) {
			rt.Spec.PodTemplate.Spec.HostIPC = true
		},
		"serviceAccountName": func(rt *agcv2alpha1.RunnerTemplate) {
			rt.Spec.PodTemplate.Spec.ServiceAccountName = "smuggled"
		},
		"automountServiceAccountToken": func(rt *agcv2alpha1.RunnerTemplate) {
			yes := true
			rt.Spec.PodTemplate.Spec.AutomountServiceAccountToken = &yes
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			rt := newV2RunnerTemplate(ns, "reserved-"+name)
			mutate(rt)
			err := k8sClient.Create(ctx, rt)
			require.Error(t, err)
			assert.True(t, apierrors.IsInvalid(err),
				"reserved field %s should be rejected as Invalid, got %v", name, err)
		})
	}
}

func TestV2_ClusterRunnerTemplate_RoundTrip(t *testing.T) {
	crt := &agcv2alpha1.ClusterRunnerTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "golden-dind"},
		Spec: agcv2alpha1.RunnerTemplateSpec{
			PodTemplate: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "runner", Image: "dind:latest"}},
				},
			},
			WorkerImage: "dind:latest",
		},
	}
	require.NoError(t, k8sClient.Create(ctx, crt))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, crt) })

	var got agcv2alpha1.ClusterRunnerTemplate
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "golden-dind"}, &got))
	assert.Equal(t, "dind:latest", got.Spec.WorkerImage)
}
