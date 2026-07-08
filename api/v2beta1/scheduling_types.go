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
}
