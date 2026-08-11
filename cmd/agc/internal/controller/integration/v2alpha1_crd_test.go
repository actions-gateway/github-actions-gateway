//go:build integration

package integration_test

import (
	"strconv"
	"strings"
	"testing"

	agcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	agcv2beta1 "github.com/actions-gateway/github-actions-gateway/api/v2beta1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/utils/ptr"
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

// TestV2_RunnerSet_ScaleUpRoundTrip proves the opt-in worker-pod creation-rate
// limit (Q223) installs and round-trips through the real apiserver, including the
// optional burst pointer. The Burst=MaxPerSecond default is applied in the AGC (CRD
// defaulting cannot reference another field), so an omitted burst stays nil here.
func TestV2_RunnerSet_ScaleUpRoundTrip(t *testing.T) {
	const ns = "v2-runnerset-scaleup"
	createNSForAGC(t, ns)

	rs := newV2RunnerSet(ns, "linux", "acme", "default")
	burst := int32(20)
	rs.Spec.ScaleUp = &agcv2alpha1.ScaleUpRateLimit{MaxPerSecond: 10, Burst: &burst}
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, rs) })

	var got agcv2alpha1.RunnerSet
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "linux"}, &got))
	require.NotNil(t, got.Spec.ScaleUp, "scaleUp must round-trip")
	assert.Equal(t, int32(10), got.Spec.ScaleUp.MaxPerSecond)
	require.NotNil(t, got.Spec.ScaleUp.Burst)
	assert.Equal(t, int32(20), *got.Spec.ScaleUp.Burst)

	// Omitting burst is valid: it stays nil (the AGC defaults it to maxPerSecond).
	rs2 := newV2RunnerSet(ns, "linux2", "acme", "default")
	rs2.Spec.ScaleUp = &agcv2alpha1.ScaleUpRateLimit{MaxPerSecond: 5}
	require.NoError(t, k8sClient.Create(ctx, rs2))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, rs2) })
	var got2 agcv2alpha1.RunnerSet
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "linux2"}, &got2))
	require.NotNil(t, got2.Spec.ScaleUp)
	assert.Equal(t, int32(5), got2.Spec.ScaleUp.MaxPerSecond)
	assert.Nil(t, got2.Spec.ScaleUp.Burst, "an omitted burst stays nil at the apiserver")
}

// TestV2_RunnerSet_ScaleUpRejectsZeroRate proves the apiserver enforces the
// maxPerSecond minimum (a zero/negative rate is a footgun, not "disabled" — omit
// scaleUp entirely to disable).
func TestV2_RunnerSet_ScaleUpRejectsZeroRate(t *testing.T) {
	const ns = "v2-runnerset-scaleup-invalid"
	createNSForAGC(t, ns)

	rs := newV2RunnerSet(ns, "linux", "acme", "default")
	rs.Spec.ScaleUp = &agcv2alpha1.ScaleUpRateLimit{MaxPerSecond: 0}
	err := k8sClient.Create(ctx, rs)
	require.Error(t, err)
	assert.True(t, apierrors.IsInvalid(err), "expected Invalid for maxPerSecond: 0, got %v", err)
}

// TestV2_RunnerSet_NodeShareAllocatableRequiresCPUOrMemory pins Q484. The
// NodeShare profile only ever derives the cpu and memory keys, so an envelope
// carrying neither — empty, or extended resources only — actuates nothing while
// status.sizingProfileState still reports Active. The CEL rule makes that state
// unreachable at admission. It must not over-tighten: declaring one of the two
// is a legitimate envelope (the other resource keeps the template's ask), and
// both served versions carry the rule.
func TestV2_RunnerSet_NodeShareAllocatableRequiresCPUOrMemory(t *testing.T) {
	const ns = "v2-runnerset-nodeshare-envelope"
	createNSForAGC(t, ns)

	nodeShare := func(alloc corev1.ResourceList) *agcv2alpha1.WorkerSizing {
		return &agcv2alpha1.WorkerSizing{
			Profile:   agcv2alpha1.SizingProfileNodeShare,
			NodeShare: &agcv2alpha1.NodeShareSizing{Allocatable: alloc, WorkersPerNode: 4},
		}
	}

	empty := newV2RunnerSet(ns, "empty-envelope", "acme", "default")
	empty.Spec.Sizing = nodeShare(corev1.ResourceList{})
	err := k8sClient.Create(ctx, empty)
	require.Error(t, err)
	assert.True(t, apierrors.IsInvalid(err), "an empty allocatable should be Invalid, got %v", err)

	// The realistic mistake: an operator declares the envelope in terms of the
	// resource the profile exists to bin-pack against, which is the one key it
	// never divides.
	gpuOnly := newV2RunnerSet(ns, "gpu-only-envelope", "acme", "default")
	gpuOnly.Spec.Sizing = nodeShare(corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("4")})
	err = k8sClient.Create(ctx, gpuOnly)
	require.Error(t, err)
	assert.True(t, apierrors.IsInvalid(err), "a GPU-only allocatable should be Invalid, got %v", err)

	cpuOnly := newV2RunnerSet(ns, "cpu-only-envelope", "acme", "default")
	cpuOnly.Spec.Sizing = nodeShare(corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("8")})
	require.NoError(t, k8sClient.Create(ctx, cpuOnly),
		"an envelope declaring only cpu divides cpu and leaves memory to the template")
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, cpuOnly) })

	betaGPUOnly := &agcv2beta1.RunnerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "beta-gpu-only-envelope", Namespace: ns},
		Spec: agcv2beta1.RunnerSetSpec{
			GatewayRef:   agcv2beta1.ObjectRef{Name: "acme"},
			TemplateRef:  &agcv2beta1.ObjectRef{Name: "default"},
			RunnerLabels: []string{"self-hosted"},
			Sizing: &agcv2beta1.WorkerSizing{
				Profile: agcv2beta1.SizingProfileNodeShare,
				NodeShare: &agcv2beta1.NodeShareSizing{
					Allocatable:    corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("4")},
					WorkersPerNode: 4,
				},
			},
		},
	}
	err = k8sClient.Create(ctx, betaGPUOnly)
	require.Error(t, err)
	assert.True(t, apierrors.IsInvalid(err), "v2beta1 must carry the same rule, got %v", err)
}

func TestV2_RunnerSet_AcquisitionProtocolDefaultsScaleSet(t *testing.T) {
	const ns = "v2-runnerset-acqproto-default"
	createNSForAGC(t, ns)

	// Omit acquisitionProtocol entirely — as of Q264 P5 the apiserver must default it
	// to ScaleSet.
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

// TestV2_RunnerSet_BareMultiLabelAccepted is the inverse of the rule Q264 P5's default
// flip left behind: a runner set that omits acquisitionProtocol AND declares more than
// one runnerLabel used to be rejected, because it defaults to ScaleSet and ScaleSet
// required exactly one label. Q726 registers every label on the scale set, so the bare
// multi-label shape — the one an ARC user's runs-on array maps onto — now admits, and
// pinning Classic is no longer the price of multi-label matching.
func TestV2_RunnerSet_BareMultiLabelAccepted(t *testing.T) {
	const ns = "v2-runnerset-acqproto-baremulti"
	createNSForAGC(t, ns)

	bare := newV2RunnerSet(ns, "multi", "acme", "default") // two labels
	bare.Spec.AcquisitionProtocol = ""                     // omit → defaults to ScaleSet
	require.NoError(t, k8sClient.Create(ctx, bare),
		"a bare multi-label set must admit and default to ScaleSet (Q726)")
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, bare) })

	var got agcv2alpha1.RunnerSet
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "multi"}, &got))
	assert.Equal(t, agcv2alpha1.AcquisitionProtocolScaleSet, got.Spec.AcquisitionProtocol,
		"the multi-label set must have taken the ScaleSet default, not been steered to Classic")
	assert.Equal(t, []string{"self-hosted", "linux"}, got.Spec.RunnerLabels)

	// The same labels are still accepted on the deprecated Classic path.
	classic := newV2RunnerSet(ns, "multi-classic", "acme", "default") // helper pins Classic
	require.NoError(t, k8sClient.Create(ctx, classic))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, classic) })
}

func TestV2_RunnerSet_ScaleSetAcceptsMultiLabel(t *testing.T) {
	const ns = "v2-runnerset-acqproto-label"
	createNSForAGC(t, ns)

	// An explicit ScaleSet set with more than one runnerLabel: every label is
	// registered on the scale set, the first one naming it (Q726).
	multi := newV2RunnerSet(ns, "multi", "acme", "default") // labels: self-hosted, linux
	multi.Spec.AcquisitionProtocol = agcv2alpha1.AcquisitionProtocolScaleSet
	require.NoError(t, k8sClient.Create(ctx, multi))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, multi) })

	// One label remains the ordinary shape.
	single := newV2RunnerSet(ns, "single", "acme", "default")
	single.Spec.AcquisitionProtocol = agcv2alpha1.AcquisitionProtocolScaleSet
	single.Spec.RunnerLabels = []string{"scale-set-linux"}
	require.NoError(t, k8sClient.Create(ctx, single))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, single) })

	// The per-item constraints survive the relaxation: MinItems=1 still bites, and so
	// does the no-whitespace-or-commas pattern on EVERY item rather than only the
	// first — a list validation is the easiest thing to relax past its intent.
	none := newV2RunnerSet(ns, "no-labels", "acme", "default")
	none.Spec.AcquisitionProtocol = agcv2alpha1.AcquisitionProtocolScaleSet
	none.Spec.RunnerLabels = []string{}
	err := k8sClient.Create(ctx, none)
	require.Error(t, err)
	assert.True(t, apierrors.IsInvalid(err), "an empty runnerLabels should be Invalid, got %v", err)

	comma := newV2RunnerSet(ns, "comma-label", "acme", "default")
	comma.Spec.AcquisitionProtocol = agcv2alpha1.AcquisitionProtocolScaleSet
	comma.Spec.RunnerLabels = []string{"linux", "gpu,big"}
	err = k8sClient.Create(ctx, comma)
	require.Error(t, err)
	assert.True(t, apierrors.IsInvalid(err),
		"a comma in a NON-first label should be Invalid, got %v", err)
}

// TestV2_RunnerSet_ClassicMultiLabelEditableThroughHub is Q398's regression guard,
// outliving the rule that caused it. A Classic multi-label set is STORED as a v2beta1
// hub object, and v2beta1 once rejected more than one runnerLabel — so every
// unqualified `kubectl edit/patch/apply`, which addresses the storage version, failed
// on a field that had nothing to do with labels. Q726 removed the rule outright, which
// removes the cause; this keeps asserting the symptom is gone, because the shape that
// produced it (a hub object holding a spoke-authored value) is permanent and the next
// hub-wide rule would bring it straight back.
func TestV2_RunnerSet_ClassicMultiLabelEditableThroughHub(t *testing.T) {
	const ns = "v2-runnerset-hub-edit"
	createNSForAGC(t, ns)

	// Author on v2alpha1 — the only version that admits the multi-label shape.
	classic := newV2RunnerSet(ns, "ci", "acme", "default") // Classic; labels: self-hosted, linux
	require.NoError(t, k8sClient.Create(ctx, classic))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, classic) })

	key := types.NamespacedName{Namespace: ns, Name: "ci"}

	// Read/modify/write through the hub, which is what an unqualified kubectl does.
	var hub agcv2beta1.RunnerSet
	require.NoError(t, k8sClient.Get(ctx, key, &hub))
	require.Equal(t, []string{"self-hosted", "linux"}, hub.Spec.RunnerLabels,
		"the stored hub object carries the Classic set's multi-label shape")

	hub.Spec.MaxWorkers = ptr.To(int32(7))
	require.NoError(t, k8sClient.Update(ctx, &hub),
		"an unqualified edit of an unrelated field must not fail on runnerLabels (Q398)")

	// And the labels themselves are now editable through the hub, which is what Q726
	// changed: before it, this write was the one ratcheting could not forgive.
	var again agcv2beta1.RunnerSet
	require.NoError(t, k8sClient.Get(ctx, key, &again))
	require.NotNil(t, again.Spec.MaxWorkers)
	assert.Equal(t, int32(7), *again.Spec.MaxWorkers, "the unrelated edit should have landed")
	again.Spec.RunnerLabels = []string{"self-hosted", "linux", "arm64"}
	require.NoError(t, k8sClient.Update(ctx, &again),
		"changing runnerLabels through v2beta1 must now be accepted (Q726)")

	// The v2alpha1 view is unharmed: protocol intact, both edits visible.
	var spoke agcv2alpha1.RunnerSet
	require.NoError(t, k8sClient.Get(ctx, key, &spoke))
	assert.Equal(t, agcv2alpha1.AcquisitionProtocolClassic, spoke.Spec.AcquisitionProtocol,
		"a hub-side edit must not re-protocol the spoke object")
	assert.Equal(t, []string{"self-hosted", "linux", "arm64"}, spoke.Spec.RunnerLabels)
	require.NotNil(t, spoke.Spec.MaxWorkers)
	assert.Equal(t, int32(7), *spoke.Spec.MaxWorkers)
}

// TestV2beta1_RunnerSet_MultiLabelCreateAccepted is the shape Q726 exists for: an ARC
// workflow's `runs-on: [linux, gpu]` expressed directly on the graduated version, with
// no acquisitionProtocol lever to pull and no v2alpha1 Classic detour.
func TestV2beta1_RunnerSet_MultiLabelCreateAccepted(t *testing.T) {
	const ns = "v2beta1-runnerset-multi"
	createNSForAGC(t, ns)

	multi := &agcv2beta1.RunnerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "multi", Namespace: ns},
		Spec: agcv2beta1.RunnerSetSpec{
			GatewayRef:   agcv2beta1.ObjectRef{Name: "acme"},
			TemplateRef:  &agcv2beta1.ObjectRef{Name: "default"},
			RunnerLabels: []string{"self-hosted", "linux", "gpu"},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, multi))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, multi) })

	var got agcv2beta1.RunnerSet
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "multi"}, &got))
	assert.Equal(t, []string{"self-hosted", "linux", "gpu"}, got.Spec.RunnerLabels,
		"every declared label must round-trip through the storage version in order")
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
