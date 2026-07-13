package v2beta1

import (
	corev1 "k8s.io/api/core/v1"
)

// PodScheduling is the pod placement pass-through shared by EgressProxy (the proxy
// pool pods) and ActionsGateway (the AGC control-plane pod). It carries the three
// standard Kubernetes placement controls verbatim, so the pods GAG builds on a
// tenant's behalf can be steered exactly like any other pod.
//
// Why it exists (Q243/Q282). A per-tenant egress IP is realized by pinning a
// tenant's proxy pods to a node pool whose egress path (cloud NAT gateway, dedicated
// subnet, or SNAT-ing gateway node) owns that IP. Without a placement knob on the
// EgressProxy, the proxy pool's replicas spread across whatever pools exist and one
// tenant egresses from several IPs — the gap the Q243 live validation found. Worker
// pods already have the full placement surface via RunnerTemplate.podTemplate; this
// type closes the same gap for the two pods GAG owns directly.
//
// # Placement is tenant-settable by design
//
// These fields are NOT gated by a platform allowlist, and that is a deliberate,
// reviewed decision rather than an oversight (Q282):
//
//   - Choosing an egress path is a feature, not an escape. Distinct per-tenant egress
//     IPs exist so tenants do not share a rate-limit or block radius with each other.
//     A tenant electing where its own traffic leaves the cluster is the intended use.
//
//   - The capability is already reachable, though the symmetry is partial — state it
//     precisely. RunnerTemplate.podTemplate is a full PodTemplateSpec whose
//     reserved-field guardrail (§H.4) withholds serviceAccountName,
//     automountServiceAccountToken, hostPID, hostNetwork, and hostIPC — but
//     deliberately not nodeSelector/tolerations/affinity. In DIRECT-egress mode
//     (§H.10, no proxyRef) worker pods reach GitHub without a proxy, so worker
//     placement already selects the egress IP with no gate. In PROXIED mode (the
//     default) it does not: NetworkPolicy forces worker traffic through the proxy, so
//     the proxy pool's placement is what selects the IP. This field therefore does
//     extend the capability to the default posture — it is not purely a symmetry
//     argument, and gating it would still leave the direct-egress path open.
//
//   - Constraining placement is the platform's job, and only the platform has a sound
//     tool. A per-CRD *validating* allowlist of permitted placement values cannot
//     work: affinity.nodeAffinity supports NotIn/DoesNotExist, so "any pool except my
//     own" is expressible and no key=value allowlist rejects it in general. Pinning
//     nodeSelector by MUTATION is sound — Kubernetes ANDs nodeSelector with
//     nodeAffinity, so affinity can only narrow the candidate nodes, never widen them
//     — and mutation is a policy-engine (Gatekeeper, Kyverno) capability, not a CRD
//     webhook one. See docs/operations/admission-policies.md for ready-to-apply
//     samples covering this type and RunnerTemplate alike.
//
// The property this weakens is *attribution*, not isolation: if a tenant retargets
// its proxy onto another pool's egress path, traffic from both tenants leaves via one
// IP and per-tenant IP attribution no longer holds. Nothing about namespace
// isolation, RBAC, or the egress choke point changes. See docs/design/05-security.md.
//
// # Why a narrow block rather than a PodTemplateSpec
//
// Worker pods take placement through the full corev1.PodTemplateSpec on
// RunnerTemplate. This type deliberately does not, for two reasons:
//
//   - Size. A PodTemplateSpec generates ~600 KB of OpenAPI. It is why the
//     RunnerTemplate CRDs weigh 1.21 MB each and why the v2 CRDs ship in their own
//     opt-in chart at all (§H.13, Q149 — embedding them pushed the main chart's Helm
//     release Secret past 1 MiB). Two more copies, across two served versions, would
//     put single CRD objects at the apiserver's ~1.5 MiB ceiling.
//   - Ownership. The proxy and AGC pods' image, container, and securityContext are
//     controller-enforced invariants. A PodTemplateSpec would invite an author to set
//     them and then require a reserved-field CEL guardrail to reject it, which is the
//     complexity RunnerTemplate carries and these kinds do not need.
//
// The consequence is a real gap: workers can express topologySpreadConstraints,
// priorityClassName, schedulerName, runtimeClassName, and nodeName; these pods cannot.
// The two that matter are topologySpreadConstraints (the modern successor to the
// anti-affinity spread this proxy pool already relies on) and priorityClassName (an
// evicted proxy pod takes that tenant's whole egress path down). Growing this struct
// is purely additive — no conversion work, no breaking change. Tracked as Q284.
type PodScheduling struct {
	// NodeSelector constrains the pods to nodes carrying all of these labels — the
	// simplest way to pin a proxy pool to a tenant's node pool (and thus to that
	// pool's egress IP). Merged into the pod spec verbatim.
	//
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Tolerations let the pods schedule onto nodes carrying matching taints — the
	// companion to NodeSelector when a tenant's node pool is taint-guarded so that
	// unrelated workloads do not land on it. Applied to the pod spec verbatim.
	//
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// Affinity is the standard pod/node affinity and anti-affinity block.
	//
	// Precedence against the built-in anti-affinity (EgressProxy only). The GMC
	// stamps a REQUIRED cross-node podAntiAffinity on every proxy pool so one node
	// failure cannot take the whole pool down. Composition with this field is:
	//
	//   - nodeAffinity and podAffinity are applied as given, alongside the built-in
	//     anti-affinity, which is preserved.
	//   - podAntiAffinity, when set to any non-nil value, REPLACES the built-in term
	//     entirely — set it and you own it. An explicit empty value
	//     (podAntiAffinity: {}) therefore opts out of cross-node spreading, which is
	//     what a single-node tenant pool needs: the required built-in term would
	//     otherwise strand every replica after the first in Pending. Lowering
	//     minReplicas to 1 is the other way to get there.
	//
	// ActionsGateway has no built-in affinity, so the block applies verbatim there.
	//
	// +optional
	Affinity *corev1.Affinity `json:"affinity,omitempty"`

	// TopologySpreadConstraints spreads the pods across failure domains (zones,
	// nodes) — the modern successor to the cross-node podAntiAffinity this proxy
	// pool already relies on. It expresses "spread across zones, tolerate a skew of
	// 1", which anti-affinity cannot. Applied to the pod spec verbatim.
	//
	// Composition with the built-in anti-affinity (EgressProxy only). Unlike
	// Affinity, this field COMPOSES with the built-in required cross-node
	// anti-affinity — it never replaces it. `podAntiAffinity: {}` on Affinity stays
	// the single opt-out for the built-in cross-node spread; this field does not add
	// a second one. That asymmetry is deliberate: Affinity has to be able to displace
	// the built-in term because podAntiAffinity occupies the same field with nowhere
	// else to go, whereas topologySpreadConstraints is a different field with no such
	// collision, so composing is safe (it can only NARROW the candidate node set,
	// like nodeSelector AND-ing with nodeAffinity) and having one author silently
	// lose cross-node spread by asking for zonal spread would be a footgun. Its
	// labelSelector counts only pods in the constraint's own namespace, so one
	// tenant's spread cannot be skewed by another tenant's pods.
	//
	// The Pending trap. An author who asks for a SOFT zonal spread
	// (whenUnsatisfiable: ScheduleAnyway) still inherits the REQUIRED built-in
	// cross-node anti-affinity, so replicas beyond the pinned pool's node count
	// strand in Pending — the same behavior as any proxy pool whose minReplicas
	// exceeds its node count. The escapes are the same: `podAntiAffinity: {}` to opt
	// out of the built-in cross-node spread, or lower minReplicas.
	//
	// Unlike PriorityClassName, this needs no allowlist: it is namespace-scoped and
	// can only narrow placement, so it carries no cross-tenant lever.
	//
	// +optional
	TopologySpreadConstraints []corev1.TopologySpreadConstraint `json:"topologySpreadConstraints,omitempty"`

	// PriorityClassName names a cluster-scoped PriorityClass for these pods, raising
	// them above best-effort workloads under node pressure. It matters because an
	// evicted proxy pod takes that tenant's ENTIRE egress path down, and an evicted
	// AGC pod takes that tenant's control plane down; without a priority class both
	// are as evictable as any best-effort pod.
	//
	// Gated by a SEPARATE, infra-only allowlist. This field is validated against the
	// GMC --allowed-infra-priority-classes flag, which is DISJOINT by construction
	// from the worker-facing --allowed-priority-classes that gates priorityTiers and
	// RunnerTemplate podTemplate.spec.priorityClassName. The two must not intersect,
	// and the GMC refuses to start if they do.
	//
	// Why a second allowlist rather than reusing the worker one. Infra pods must sit
	// ABOVE workers — that is the whole point of prioritizing them. If the worker
	// allowlist were reused and a high class added so a proxy could name it, that
	// same class would become nameable from a worker pod, and any tenant could lift
	// its WORKERS to infra priority and preempt OTHER tenants' proxy pods. The gate
	// meant to protect the proxy would become the mechanism for evicting it.
	//
	// Why gated at all, when nodeSelector/tolerations/affinity are not. Placement is
	// a choice about the tenant's own traffic; it weakens attribution, not isolation.
	// Priority is a cluster-wide, cross-tenant preemption lever: a pod naming a
	// preempting class can evict OTHER tenants' pods off a node. And `system-*`
	// PriorityClasses are NOT kube-system-scoped — a pod in any namespace may name
	// system-cluster-critical (value 2000000000, PreemptLowerPriority), with no
	// built-in admission check restricting it (verified against a real apiserver).
	// So an ungated priorityClassName on a tenant-writable CR is a cluster-wide
	// preemption escape, which is why it is the one PodScheduling field behind a gate.
	//
	// The empty string (the default) names no class and is always permitted, so an
	// unset allowlist forbids every NAMED class without forbidding unprioritized pods.
	//
	// +optional
	PriorityClassName string `json:"priorityClassName,omitempty"`
}
