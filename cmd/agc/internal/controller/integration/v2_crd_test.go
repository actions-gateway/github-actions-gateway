//go:build integration

package integration_test

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	agcv2 "github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
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

// These tests prove the v2 (actions-gateway.com) AGC kinds install into the real
// apiserver and round-trip, and that the CEL/structural validation behaves under
// real-apiserver semantics — defaulting, the gatewayRef selectable field, and CEL
// rejections that only the apiserver applies. No reconciler is exercised.

func newV2RunnerTemplate(ns, name string) *agcv2.RunnerTemplate {
	return &agcv2.RunnerTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: agcv2.RunnerTemplateSpec{
			PodTemplate: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "runner", Image: "runner:latest"}},
				},
			},
			WorkerImage: "runner:latest",
		},
	}
}

func newV2RunnerSet(ns, name, gateway, template string) *agcv2.RunnerSet {
	return &agcv2.RunnerSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: agcv2.RunnerSetSpec{
			GatewayRef:   agcv2.ObjectRef{Name: gateway},
			TemplateRef:  &agcv2.ObjectRef{Name: template},
			RunnerLabels: []string{"self-hosted", "linux"},
		},
	}
}

func TestV2_RunnerSet_RoundTrip(t *testing.T) {
	const ns = "v2-runnerset-rt"
	createNSForAGC(t, ns)

	rs := newV2RunnerSet(ns, "linux", "acme", "default")
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, rs) })

	var got agcv2.RunnerSet
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "linux"}, &got))

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
	// apiserver filters server-side — the mechanism the AGC's watch scoping relies on.
	var list agcv2.RunnerSetList
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
	rs.Spec.PriorityTiers = []agcv2.PriorityTier{{PriorityClassName: "pc", Threshold: 10}}
	err := k8sClient.Create(ctx, rs)
	require.Error(t, err)
	assert.True(t, apierrors.IsInvalid(err), "expected Invalid for maxWorkers != last tier, got %v", err)
}

func TestV2_RunnerSet_PriorityTiersMustAscend(t *testing.T) {
	const ns = "v2-runnerset-tier-order"
	createNSForAGC(t, ns)

	tiers := func(thresholds ...int32) []agcv2.PriorityTier {
		out := make([]agcv2.PriorityTier, len(thresholds))
		for i, th := range thresholds {
			out[i] = agcv2.PriorityTier{PriorityClassName: "pc-" + strconv.Itoa(i), Threshold: th}
		}
		return out
	}
	for name, thresholds := range map[string][]int32{
		"descending":   {10, 5},
		"equal":        {5, 5},
		"dip-mid-list": {2, 8, 4, 10},
	} {
		t.Run("rejects "+name, func(t *testing.T) {
			rs := newV2RunnerSet(ns, name, "acme", "default")
			rs.Spec.PriorityTiers = tiers(thresholds...)
			err := k8sClient.Create(ctx, rs)
			require.Error(t, err)
			assert.True(t, apierrors.IsInvalid(err), "expected Invalid, got %v", err)
			assert.Contains(t, err.Error(), "priorityTiers thresholds must be strictly ascending")
		})
	}
	for name, thresholds := range map[string][]int32{
		"single":    {3},
		"ascending": {2, 5, 10},
	} {
		t.Run("admits "+name, func(t *testing.T) {
			rs := newV2RunnerSet(ns, name, "acme", "default")
			rs.Spec.PriorityTiers = tiers(thresholds...)
			require.NoError(t, k8sClient.Create(ctx, rs))
			t.Cleanup(func() { _ = k8sClient.Delete(ctx, rs) })
		})
	}
}

// TestV2_RunnerSet_StoredOutOfOrderTiersKeepWorking: a RunnerSet stored before the
// ordering rule keeps accepting writes that leave priorityTiers unchanged, which only a
// stored object can show, so the CRD is edited to admit one. The rule passes an unchanged
// value itself because ratcheting misses the status subresource on Kubernetes 1.31 and
// 1.32 (api-review.md § narrowest field); this suite runs 1.35, so only the
// optionalOldSelf pin below can fail here.
func TestV2_RunnerSet_StoredOutOfOrderTiersKeepWorking(t *testing.T) {
	const ns = "v2-runnerset-tier-ratchet"
	createNSForAGC(t, ns)

	crd := &unstructured.Unstructured{}
	crd.SetGroupVersionKind(schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"})
	crdKey := types.NamespacedName{Name: "runnersets.actions-gateway.com"}
	rulePath := []string{"schema", "openAPIV3Schema", "properties", "spec", "properties", "priorityTiers", "x-kubernetes-validations"}
	tierRules := func() []any {
		require.NoError(t, k8sClient.Get(ctx, crdKey, crd))
		versions, _, _ := unstructured.NestedSlice(crd.Object, "spec", "versions")
		for _, v := range versions {
			version := v.(map[string]any)
			if version["name"] == "v2" {
				rules, _, err := unstructured.NestedSlice(version, rulePath...)
				require.NoError(t, err)
				return rules
			}
		}
		t.Fatal("no v2 version in the CRD")
		return nil
	}
	setRules := func(rules []any) {
		require.Eventually(t, func() bool {
			if err := k8sClient.Get(ctx, crdKey, crd); err != nil {
				return false
			}
			versions, _, _ := unstructured.NestedSlice(crd.Object, "spec", "versions")
			for i, v := range versions {
				version := v.(map[string]any)
				if version["name"] != "v2" {
					continue
				}
				if len(rules) == 0 {
					unstructured.RemoveNestedField(version, rulePath...)
				} else if err := unstructured.SetNestedSlice(version, rules, rulePath...); err != nil {
					return false
				}
				versions[i] = version
			}
			if err := unstructured.SetNestedSlice(crd.Object, versions, "spec", "versions"); err != nil {
				return false
			}
			return k8sClient.Update(ctx, crd) == nil
		}, 15*time.Second, 200*time.Millisecond, "update the CRD")
	}
	original := tierRules()
	require.Len(t, original, 1, "the rule this test exercises must exist")
	require.Equal(t, true, original[0].(map[string]any)["optionalOldSelf"], "an unchanged value must pass on the status path too")
	t.Cleanup(func() { setRules(original) })

	outOfOrder := []agcv2.PriorityTier{{PriorityClassName: "high", Threshold: 10}, {PriorityClassName: "low", Threshold: 5}}
	newSet := func(name string) *agcv2.RunnerSet {
		rs := newV2RunnerSet(ns, name, "acme", "default")
		rs.Spec.PriorityTiers = append([]agcv2.PriorityTier(nil), outOfOrder...)
		return rs
	}

	setRules(nil)
	stored := newSet("stored")
	require.Eventually(t, func() bool { return k8sClient.Create(ctx, stored) == nil },
		15*time.Second, 200*time.Millisecond, "the CRD with no rule must admit the value")
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), stored) })

	setRules(original)
	require.Eventually(t, func() bool {
		probe := newSet("probe")
		err := k8sClient.Create(ctx, probe)
		if err == nil {
			_ = k8sClient.Delete(ctx, probe)
		}
		return apierrors.IsInvalid(err)
	}, 15*time.Second, 200*time.Millisecond, "the restored rule must reject a new object")

	key := types.NamespacedName{Namespace: ns, Name: "stored"}
	update := func(edit func(*agcv2.RunnerSet)) error {
		var got agcv2.RunnerSet
		if err := k8sClient.Get(ctx, key, &got); err != nil {
			return err
		}
		edit(&got)
		return k8sClient.Update(ctx, &got)
	}
	require.NoError(t, update(func(*agcv2.RunnerSet) {}), "re-applying it unchanged must succeed")
	require.NoError(t, update(func(rs *agcv2.RunnerSet) { rs.Spec.RunnerGroup = "edited" }),
		"editing another field must succeed")
	require.NoError(t, update(func(rs *agcv2.RunnerSet) { rs.Finalizers = append(rs.Finalizers, "example.com/hold") }),
		"a metadata write must succeed")
	require.NoError(t, update(func(rs *agcv2.RunnerSet) { rs.Finalizers = nil }))
	var got agcv2.RunnerSet
	require.NoError(t, k8sClient.Get(ctx, key, &got))
	got.Status.Conditions = []metav1.Condition{{Type: "Probe", Status: metav1.ConditionTrue, Reason: "Probe", LastTransitionTime: metav1.Now()}}
	require.NoError(t, k8sClient.Status().Update(ctx, &got), "a status write, as the AGC makes, must succeed")

	err := update(func(rs *agcv2.RunnerSet) { rs.Spec.PriorityTiers[1].Threshold = 7 })
	require.Error(t, err, "changing it to another out-of-order list must be rejected")
	assert.True(t, apierrors.IsInvalid(err), "expected Invalid, got %v", err)
	require.NoError(t, update(func(rs *agcv2.RunnerSet) { rs.Spec.PriorityTiers[1].Threshold = 12 }),
		"fixing it must succeed")
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
	rs.Spec.ScaleUp = &agcv2.ScaleUpRateLimit{MaxPerSecond: 10, Burst: &burst}
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, rs) })

	var got agcv2.RunnerSet
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "linux"}, &got))
	require.NotNil(t, got.Spec.ScaleUp, "scaleUp must round-trip")
	assert.Equal(t, int32(10), got.Spec.ScaleUp.MaxPerSecond)
	require.NotNil(t, got.Spec.ScaleUp.Burst)
	assert.Equal(t, int32(20), *got.Spec.ScaleUp.Burst)

	// Omitting burst is valid: it stays nil (the AGC defaults it to maxPerSecond).
	rs2 := newV2RunnerSet(ns, "linux2", "acme", "default")
	rs2.Spec.ScaleUp = &agcv2.ScaleUpRateLimit{MaxPerSecond: 5}
	require.NoError(t, k8sClient.Create(ctx, rs2))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, rs2) })
	var got2 agcv2.RunnerSet
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
	rs.Spec.ScaleUp = &agcv2.ScaleUpRateLimit{MaxPerSecond: 0}
	err := k8sClient.Create(ctx, rs)
	require.Error(t, err)
	assert.True(t, apierrors.IsInvalid(err), "expected Invalid for maxPerSecond: 0, got %v", err)
}

// TestV2_RunnerSet_NodeShareAllocatableRequiresCPUOrMemory pins Q484. The
// NodeShare profile only ever derives the cpu and memory keys, so an envelope
// carrying neither — empty, or extended resources only — actuates nothing while
// status.sizingProfileState still reports Active. The CEL rule makes that state
// unreachable at admission. It must not over-tighten: declaring one of the two
// is a legitimate envelope (the other resource keeps the template's ask).
func TestV2_RunnerSet_NodeShareAllocatableRequiresCPUOrMemory(t *testing.T) {
	const ns = "v2-runnerset-nodeshare-envelope"
	createNSForAGC(t, ns)

	nodeShare := func(alloc corev1.ResourceList) *agcv2.WorkerSizing {
		return &agcv2.WorkerSizing{
			Profile:   agcv2.SizingProfileNodeShare,
			NodeShare: &agcv2.NodeShareSizing{Allocatable: alloc, WorkersPerNode: 4},
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
}

func TestV2_RunnerSet_AcceptsMultiLabel(t *testing.T) {
	const ns = "v2-runnerset-acqproto-label"
	createNSForAGC(t, ns)

	// A set with more than one runnerLabel: every label is
	// registered on the scale set, the first one naming it (Q726).
	multi := newV2RunnerSet(ns, "multi", "acme", "default") // labels: self-hosted, linux
	require.NoError(t, k8sClient.Create(ctx, multi))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, multi) })

	// One label remains the ordinary shape.
	single := newV2RunnerSet(ns, "single", "acme", "default")
	single.Spec.RunnerLabels = []string{"scale-set-linux"}
	require.NoError(t, k8sClient.Create(ctx, single))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, single) })

	// The per-item constraints survive the relaxation: MinItems=1 still bites, and so
	// does the no-whitespace-or-commas pattern on EVERY item rather than only the
	// first — a list validation is the easiest thing to relax past its intent.
	none := newV2RunnerSet(ns, "no-labels", "acme", "default")
	none.Spec.RunnerLabels = []string{}
	err := k8sClient.Create(ctx, none)
	require.Error(t, err)
	assert.True(t, apierrors.IsInvalid(err), "an empty runnerLabels should be Invalid, got %v", err)

	comma := newV2RunnerSet(ns, "comma-label", "acme", "default")
	comma.Spec.RunnerLabels = []string{"linux", "gpu,big"}
	err = k8sClient.Create(ctx, comma)
	require.Error(t, err)
	assert.True(t, apierrors.IsInvalid(err),
		"a comma in a NON-first label should be Invalid, got %v", err)
}

// TestV2_RunnerSet_MultiLabelCreateAccepted is the shape Q726 exists for: an ARC
// workflow's `runs-on: [linux, gpu]` expressed directly on a runner set.
func TestV2_RunnerSet_MultiLabelCreateAccepted(t *testing.T) {
	const ns = "v2-runnerset-multi"
	createNSForAGC(t, ns)

	multi := &agcv2.RunnerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "multi", Namespace: ns},
		Spec: agcv2.RunnerSetSpec{
			GatewayRef:   agcv2.ObjectRef{Name: "acme"},
			TemplateRef:  &agcv2.ObjectRef{Name: "default"},
			RunnerLabels: []string{"self-hosted", "linux", "gpu"},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, multi))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, multi) })

	var got agcv2.RunnerSet
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "multi"}, &got))
	assert.Equal(t, []string{"self-hosted", "linux", "gpu"}, got.Spec.RunnerLabels,
		"every declared label must round-trip in order")
}

func TestV2_RunnerTemplate_RoundTrip(t *testing.T) {
	const ns = "v2-runnertemplate-rt"
	createNSForAGC(t, ns)

	rt := newV2RunnerTemplate(ns, "default")
	require.NoError(t, k8sClient.Create(ctx, rt))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, rt) })

	var got agcv2.RunnerTemplate
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "default"}, &got))
	assert.Equal(t, "runner:latest", got.Spec.WorkerImage)
	require.Len(t, got.Spec.PodTemplate.Spec.Containers, 1)
}

func TestV2_RunnerTemplate_ReservedPodFieldsRejected(t *testing.T) {
	const ns = "v2-runnertemplate-reserved"
	createNSForAGC(t, ns)

	cases := map[string]func(*agcv2.RunnerTemplate){
		"hostPID": func(rt *agcv2.RunnerTemplate) {
			rt.Spec.PodTemplate.Spec.HostPID = true
		},
		"hostNetwork": func(rt *agcv2.RunnerTemplate) {
			rt.Spec.PodTemplate.Spec.HostNetwork = true
		},
		"hostIPC": func(rt *agcv2.RunnerTemplate) {
			rt.Spec.PodTemplate.Spec.HostIPC = true
		},
		"serviceAccountName": func(rt *agcv2.RunnerTemplate) {
			rt.Spec.PodTemplate.Spec.ServiceAccountName = "smuggled"
		},
		"automountServiceAccountToken": func(rt *agcv2.RunnerTemplate) {
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
	crt := &agcv2.ClusterRunnerTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "golden-dind"},
		Spec: agcv2.RunnerTemplateSpec{
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

	var got agcv2.ClusterRunnerTemplate
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "golden-dind"}, &got))
	assert.Equal(t, "dind:latest", got.Spec.WorkerImage)
}
