package controller

import (
	"testing"

	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// nodePoolAffinity is a representative operator-supplied nodeAffinity: "schedule only
// onto the tenant-a node pool" — the Q243 egress-IP pinning shape.
func nodePoolAffinity(pool string) *corev1.Affinity {
	return &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{
					MatchExpressions: []corev1.NodeSelectorRequirement{{
						Key:      "cloud.google.com/gke-nodepool",
						Operator: corev1.NodeSelectorOpIn,
						Values:   []string{pool},
					}},
				}},
			},
		},
	}
}

// TestEgressProxyScheduling_NoneSetPreservesBuiltInAntiAffinity locks the
// backward-compatible default: an EgressProxy with no spec.scheduling produces exactly
// the pod placement it produced before Q282 — the built-in required cross-node
// anti-affinity, and no nodeSelector/tolerations.
func TestEgressProxyScheduling_NoneSetPreservesBuiltInAntiAffinity(t *testing.T) {
	ep := newEP("shared", "team-a", nil)
	pod := buildEgressProxyDeployment(ep, "proxy:v1", nil).Spec.Template.Spec

	require.NotNil(t, pod.Affinity)
	require.NotNil(t, pod.Affinity.PodAntiAffinity)
	terms := pod.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	require.Len(t, terms, 1)
	assert.Equal(t, "kubernetes.io/hostname", terms[0].TopologyKey)
	assert.Equal(t, egressProxyPodSelector(ep), terms[0].LabelSelector.MatchLabels)

	assert.Nil(t, pod.Affinity.NodeAffinity)
	assert.Nil(t, pod.NodeSelector)
	assert.Nil(t, pod.Tolerations)
	assert.Nil(t, pod.TopologySpreadConstraints)
	assert.Empty(t, pod.PriorityClassName)
}

// TestEgressProxyScheduling_NodeSelectorAndTolerationsPassThrough is the Q243
// egress-IP pinning path: nodeSelector + toleration land on the pod verbatim, and the
// built-in cross-node spread survives (so replicas still spread across the pinned
// pool's nodes).
func TestEgressProxyScheduling_NodeSelectorAndTolerationsPassThrough(t *testing.T) {
	tol := corev1.Toleration{
		Key:      "dedicated",
		Operator: corev1.TolerationOpEqual,
		Value:    "tenant-a",
		Effect:   corev1.TaintEffectNoSchedule,
	}
	ep := newEP("shared", "team-a", func(ep *gmcv2alpha1.EgressProxy) {
		ep.Spec.Scheduling = &gmcv2alpha1.PodScheduling{
			NodeSelector: map[string]string{"cloud.google.com/gke-nodepool": "pool-tenant-a"},
			Tolerations:  []corev1.Toleration{tol},
		}
	})
	pod := buildEgressProxyDeployment(ep, "proxy:v1", nil).Spec.Template.Spec

	assert.Equal(t, map[string]string{"cloud.google.com/gke-nodepool": "pool-tenant-a"}, pod.NodeSelector)
	assert.Equal(t, []corev1.Toleration{tol}, pod.Tolerations)

	// The built-in spread is preserved: pinning to a pool does not disable HA within it.
	require.NotNil(t, pod.Affinity)
	require.NotNil(t, pod.Affinity.PodAntiAffinity)
	assert.Len(t, pod.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution, 1)
}

// TestEgressProxyScheduling_NodeAffinityPreservesBuiltInAntiAffinity: supplying
// nodeAffinity (but not podAntiAffinity) must NOT drop the built-in spread. This is
// the composition rule that a naive "operator affinity replaces ours" would break.
func TestEgressProxyScheduling_NodeAffinityPreservesBuiltInAntiAffinity(t *testing.T) {
	ep := newEP("shared", "team-a", func(ep *gmcv2alpha1.EgressProxy) {
		ep.Spec.Scheduling = &gmcv2alpha1.PodScheduling{Affinity: nodePoolAffinity("pool-tenant-a")}
	})
	pod := buildEgressProxyDeployment(ep, "proxy:v1", nil).Spec.Template.Spec

	require.NotNil(t, pod.Affinity)
	require.NotNil(t, pod.Affinity.NodeAffinity, "supplied nodeAffinity must be applied")
	require.NotNil(t, pod.Affinity.PodAntiAffinity, "built-in anti-affinity must survive a nodeAffinity-only override")
	assert.Len(t, pod.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution, 1)
}

// TestEgressProxyScheduling_PodAntiAffinityReplacesBuiltIn: a supplied podAntiAffinity
// takes over entirely — "set it and you own it".
func TestEgressProxyScheduling_PodAntiAffinityReplacesBuiltIn(t *testing.T) {
	ep := newEP("shared", "team-a", func(ep *gmcv2alpha1.EgressProxy) {
		ep.Spec.Scheduling = &gmcv2alpha1.PodScheduling{
			Affinity: &corev1.Affinity{
				PodAntiAffinity: &corev1.PodAntiAffinity{
					// Preferred, not required: soft spread, so a 1-node pool still schedules.
					PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{{
						Weight: 100,
						PodAffinityTerm: corev1.PodAffinityTerm{
							LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "proxy"}},
							TopologyKey:   "kubernetes.io/hostname",
						},
					}},
				},
			},
		}
	})
	pod := buildEgressProxyDeployment(ep, "proxy:v1", nil).Spec.Template.Spec

	require.NotNil(t, pod.Affinity.PodAntiAffinity)
	assert.Empty(t, pod.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution,
		"built-in required term must be replaced, not merged")
	assert.Len(t, pod.Affinity.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution, 1)
}

// TestEgressProxyScheduling_EmptyPodAntiAffinityOptsOut is the single-node tenant pool
// escape hatch (Q243): an explicit `podAntiAffinity: {}` is a non-nil pointer with no
// terms, so it disables cross-node spreading entirely and replica 2 can schedule onto
// the pool's only node. This is the behaviour a nil-vs-empty confusion would silently
// break — hence its own test.
func TestEgressProxyScheduling_EmptyPodAntiAffinityOptsOut(t *testing.T) {
	ep := newEP("shared", "team-a", func(ep *gmcv2alpha1.EgressProxy) {
		ep.Spec.Scheduling = &gmcv2alpha1.PodScheduling{
			Affinity: &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{}},
		}
	})
	pod := buildEgressProxyDeployment(ep, "proxy:v1", nil).Spec.Template.Spec

	require.NotNil(t, pod.Affinity)
	require.NotNil(t, pod.Affinity.PodAntiAffinity)
	assert.Empty(t, pod.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution)
	assert.Empty(t, pod.Affinity.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution)
}

// TestEgressProxyScheduling_DoesNotAliasSpec guards the informer-object invariant: the
// builder must never hand the Deployment a pointer into the shared EgressProxy spec,
// or a later mutation of the built Deployment would corrupt the cache.
func TestEgressProxyScheduling_DoesNotAliasSpec(t *testing.T) {
	ep := newEP("shared", "team-a", func(ep *gmcv2alpha1.EgressProxy) {
		ep.Spec.Scheduling = &gmcv2alpha1.PodScheduling{
			NodeSelector: map[string]string{"pool": "a"},
			Tolerations:  []corev1.Toleration{{Key: "dedicated", Value: "tenant-a"}},
			Affinity:     nodePoolAffinity("pool-tenant-a"),
		}
	})
	pod := buildEgressProxyDeployment(ep, "proxy:v1", nil).Spec.Template.Spec

	pod.NodeSelector["pool"] = "MUTATED"
	pod.Tolerations[0].Value = "MUTATED"
	pod.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[0].Values[0] = "MUTATED"

	assert.Equal(t, "a", ep.Spec.Scheduling.NodeSelector["pool"])
	assert.Equal(t, "tenant-a", ep.Spec.Scheduling.Tolerations[0].Value)
	assert.Equal(t, "pool-tenant-a",
		ep.Spec.Scheduling.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[0].Values[0])
}

// zonalSpread is a representative soft cross-zone topologySpreadConstraint: "spread
// replicas across zones, tolerate a skew of 1, schedule anyway if unsatisfiable".
func zonalSpread() corev1.TopologySpreadConstraint {
	return corev1.TopologySpreadConstraint{
		MaxSkew:           1,
		TopologyKey:       "topology.kubernetes.io/zone",
		WhenUnsatisfiable: corev1.ScheduleAnyway,
		LabelSelector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": proxyAppName}},
	}
}

// TestEgressProxyScheduling_TopologySpreadComposesWithBuiltInAntiAffinity is the Q284
// composition rule: topologySpreadConstraints lands on the pod verbatim AND the
// built-in required cross-node anti-affinity survives — the field composes with the
// invariant, it does not replace it (unlike a supplied podAntiAffinity).
func TestEgressProxyScheduling_TopologySpreadComposesWithBuiltInAntiAffinity(t *testing.T) {
	ep := newEP("shared", "team-a", func(ep *gmcv2alpha1.EgressProxy) {
		ep.Spec.Scheduling = &gmcv2alpha1.PodScheduling{
			TopologySpreadConstraints: []corev1.TopologySpreadConstraint{zonalSpread()},
		}
	})
	pod := buildEgressProxyDeployment(ep, "proxy:v1", nil).Spec.Template.Spec

	require.Len(t, pod.TopologySpreadConstraints, 1)
	assert.Equal(t, "topology.kubernetes.io/zone", pod.TopologySpreadConstraints[0].TopologyKey)

	// The built-in cross-node spread is NOT dropped: composition, not replacement.
	require.NotNil(t, pod.Affinity)
	require.NotNil(t, pod.Affinity.PodAntiAffinity)
	assert.Len(t, pod.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution, 1)
}

// TestEgressProxyScheduling_PriorityClassNamePassThrough: a spec.scheduling
// priorityClassName lands on the proxy pod verbatim (admission gates the value; the
// builder is an unconditional pass-through).
func TestEgressProxyScheduling_PriorityClassNamePassThrough(t *testing.T) {
	ep := newEP("shared", "team-a", func(ep *gmcv2alpha1.EgressProxy) {
		ep.Spec.Scheduling = &gmcv2alpha1.PodScheduling{PriorityClassName: "gag-infra-critical"}
	})
	pod := buildEgressProxyDeployment(ep, "proxy:v1", nil).Spec.Template.Spec
	assert.Equal(t, "gag-infra-critical", pod.PriorityClassName)
}

// TestEgressProxyScheduling_TopologySpreadDoesNotAliasSpec guards the informer-object
// invariant for the new slice field.
func TestEgressProxyScheduling_TopologySpreadDoesNotAliasSpec(t *testing.T) {
	ep := newEP("shared", "team-a", func(ep *gmcv2alpha1.EgressProxy) {
		ep.Spec.Scheduling = &gmcv2alpha1.PodScheduling{
			TopologySpreadConstraints: []corev1.TopologySpreadConstraint{zonalSpread()},
		}
	})
	pod := buildEgressProxyDeployment(ep, "proxy:v1", nil).Spec.Template.Spec

	pod.TopologySpreadConstraints[0].TopologyKey = "MUTATED"
	assert.Equal(t, "topology.kubernetes.io/zone",
		ep.Spec.Scheduling.TopologySpreadConstraints[0].TopologyKey)
}

// TestAGCScheduling_PassThrough: the AGC pod has no built-in affinity, so the whole
// block applies verbatim.
func TestAGCScheduling_PassThrough(t *testing.T) {
	tol := corev1.Toleration{Key: "control-plane", Operator: corev1.TolerationOpExists}
	ag := v2Gateway("gw", "team-a", "github-app", "shared")
	ag.Spec.Scheduling = &gmcv2alpha1.PodScheduling{
		NodeSelector: map[string]string{"pool": "control"},
		Tolerations:  []corev1.Toleration{tol},
		Affinity:     nodePoolAffinity("pool-control"),
	}
	pod := buildAGCDeploymentV2(ag, "agc:v1", nil, gmcv2alpha1.SecurityProfileBaseline, nil).Spec.Template.Spec

	assert.Equal(t, map[string]string{"pool": "control"}, pod.NodeSelector)
	assert.Equal(t, []corev1.Toleration{tol}, pod.Tolerations)
	require.NotNil(t, pod.Affinity)
	require.NotNil(t, pod.Affinity.NodeAffinity)
	assert.Nil(t, pod.Affinity.PodAntiAffinity, "AGC has no built-in anti-affinity to compose with")
}

// TestAGCScheduling_TopologySpreadAndPriorityClassPassThrough: the two Q284 fields land
// on the AGC pod verbatim. The AGC has no built-in anti-affinity, so there is nothing to
// compose with — topologySpreadConstraints applies as given.
func TestAGCScheduling_TopologySpreadAndPriorityClassPassThrough(t *testing.T) {
	ag := v2Gateway("gw", "team-a", "github-app", "shared")
	ag.Spec.Scheduling = &gmcv2alpha1.PodScheduling{
		TopologySpreadConstraints: []corev1.TopologySpreadConstraint{zonalSpread()},
		PriorityClassName:         "gag-infra-critical",
	}
	pod := buildAGCDeploymentV2(ag, "agc:v1", nil, gmcv2alpha1.SecurityProfileBaseline, nil).Spec.Template.Spec

	require.Len(t, pod.TopologySpreadConstraints, 1)
	assert.Equal(t, "topology.kubernetes.io/zone", pod.TopologySpreadConstraints[0].TopologyKey)
	assert.Equal(t, "gag-infra-critical", pod.PriorityClassName)

	// Informer-object invariant: mutating the built pod must not touch the shared spec.
	pod.TopologySpreadConstraints[0].TopologyKey = "MUTATED"
	assert.Equal(t, "topology.kubernetes.io/zone",
		ag.Spec.Scheduling.TopologySpreadConstraints[0].TopologyKey)
}

// TestAGCScheduling_UnsetLeavesPodSpecUntouched: a gateway with no spec.scheduling must
// produce a byte-for-byte unchanged AGC Deployment (no spurious rollout on upgrade).
func TestAGCScheduling_UnsetLeavesPodSpecUntouched(t *testing.T) {
	ag := v2Gateway("gw", "team-a", "github-app", "shared")
	pod := buildAGCDeploymentV2(ag, "agc:v1", nil, gmcv2alpha1.SecurityProfileBaseline, nil).Spec.Template.Spec

	assert.Nil(t, pod.NodeSelector)
	assert.Nil(t, pod.Tolerations)
	assert.Nil(t, pod.Affinity)
	assert.Nil(t, pod.TopologySpreadConstraints)
	assert.Empty(t, pod.PriorityClassName)
}

// TestAGCScheduling_DoesNotAliasSpec — informer-object invariant, as above.
func TestAGCScheduling_DoesNotAliasSpec(t *testing.T) {
	ag := v2Gateway("gw", "team-a", "github-app", "shared")
	ag.Spec.Scheduling = &gmcv2alpha1.PodScheduling{
		NodeSelector: map[string]string{"pool": "control"},
		Tolerations:  []corev1.Toleration{{Key: "k", Value: "orig"}},
		Affinity:     nodePoolAffinity("pool-control"),
	}
	pod := buildAGCDeploymentV2(ag, "agc:v1", nil, gmcv2alpha1.SecurityProfileBaseline, nil).Spec.Template.Spec

	pod.NodeSelector["pool"] = "MUTATED"
	pod.Tolerations[0].Value = "MUTATED"
	pod.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[0].Values[0] = "MUTATED"

	assert.Equal(t, "control", ag.Spec.Scheduling.NodeSelector["pool"])
	assert.Equal(t, "orig", ag.Spec.Scheduling.Tolerations[0].Value)
	assert.Equal(t, "pool-control",
		ag.Spec.Scheduling.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[0].Values[0])
}
