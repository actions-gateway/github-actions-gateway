package controller

import (
	"fmt"
	"strings"

	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// CNI-native FQDN egress (Q208). On a DNS-aware policy CNI an operator can express
// the proxy pool's GitHub allowlist by hostname (Cilium toFQDNs / Calico destination
// domains) instead of the GMC's 24h GitHub-CIDR reconcile. This is an opt-in selected
// per EgressProxy via spec.egressPolicyMode; the CIDR default is unchanged and works
// on every CNI. The CNI-native object is emitted *in addition to* the standard
// NetworkPolicy, which in an FQDN mode default-denies GitHub egress (no CIDR rule) so
// the posture is fail-closed: if the CNI cannot enforce the native policy, GitHub
// egress stays denied rather than opening wide.
const (
	// egressProxyFQDNPolicySuffix is appended to the EgressProxy name to derive the
	// CNI-native FQDN policy's name: "<ep>-proxy-fqdn".
	egressProxyFQDNPolicySuffix = "-proxy-fqdn"
)

// ciliumNetworkPolicyGVK / calicoNetworkPolicyGVK / gkeFQDNNetworkPolicyGVK identify the
// CNI-native policy kinds the GMC emits in FQDN mode. They are addressed as unstructured
// objects so the GMC never takes a compile-time dependency on the Cilium, Calico, or GKE
// API modules, and so a cluster without the matching CRD installed simply NoMatch-errors
// loudly rather than forcing the dependency on every install.
var (
	ciliumNetworkPolicyGVK  = schema.GroupVersionKind{Group: "cilium.io", Version: "v2", Kind: "CiliumNetworkPolicy"}
	calicoNetworkPolicyGVK  = schema.GroupVersionKind{Group: "projectcalico.org", Version: "v3", Kind: "NetworkPolicy"}
	gkeFQDNNetworkPolicyGVK = schema.GroupVersionKind{Group: "networking.gke.io", Version: "v1alpha1", Kind: "FQDNNetworkPolicy"}
)

// FQDNBackend selects the CNI/platform mechanism the GMC uses to enforce an FQDN
// egressPolicyMode intent. It is an operator-level, install-wide choice (the GMC
// --fqdn-policy-backend flag), NOT a tenant field: a tenant expresses the durable
// intent (egressPolicyMode: FQDN) and the platform operator picks how it is enforced
// (Q245). The secure default is none — a cluster that declares no backend rejects FQDN
// intent at admission rather than guessing a mechanism or silently degrading.
type FQDNBackend string

const (
	// FQDNBackendNone is the secure default: the cluster declares no FQDN egress
	// backend, so FQDN intent is rejected at admission (fail-closed and loud).
	FQDNBackendNone FQDNBackend = "none"
	// FQDNBackendCilium emits a cilium.io/v2 CiliumNetworkPolicy (toFQDNs).
	FQDNBackendCilium FQDNBackend = "cilium"
	// FQDNBackendCalico emits a projectcalico.org/v3 NetworkPolicy (destination domains).
	FQDNBackendCalico FQDNBackend = "calico"
	// FQDNBackendGKE emits a networking.gke.io/v1alpha1 FQDNNetworkPolicy (the managed
	// GKE Dataplane V2 built-in). Additive-allow, so it composes with — never replaces —
	// the base default-deny NetworkPolicy (Q245).
	FQDNBackendGKE FQDNBackend = "gke"
)

// ParseFQDNBackend validates and normalizes the --fqdn-policy-backend flag value. An
// empty value is treated as the secure default (none); any other unrecognized value is
// a startup error so an operator typo fails loudly rather than silently disabling FQDN.
func ParseFQDNBackend(s string) (FQDNBackend, error) {
	switch FQDNBackend(s) {
	case "", FQDNBackendNone:
		return FQDNBackendNone, nil
	case FQDNBackendCilium, FQDNBackendCalico, FQDNBackendGKE:
		return FQDNBackend(s), nil
	default:
		return "", fmt.Errorf("invalid --fqdn-policy-backend %q: want one of none, cilium, calico, gke", s)
	}
}

// fqdnEmitterKind identifies which CNI-native FQDN policy the reconciler must emit for a
// given (intent, backend) pair — or none.
type fqdnEmitterKind int

const (
	fqdnEmitNone fqdnEmitterKind = iota
	fqdnEmitCilium
	fqdnEmitCalico
	fqdnEmitGKE
)

// resolveFQDNEmitter maps the tenant intent (egressPolicyMode) and the operator backend
// (--fqdn-policy-backend) to the concrete CNI-native emitter — the single decision point
// that replaces the old per-CNI enum switch (Q245). The deprecated CNI-specific intents
// CiliumFQDN/CalicoFQDN pin their namesake mechanism regardless of the operator backend,
// preserving exact pre-split behavior for stored objects; the intent value FQDN defers to
// the operator backend, and FQDN+none emits nothing. That last case is rejected at
// admission, so it is not reached in practice — and if it were, the base NetworkPolicy
// still default-denies GitHub egress, so it fails closed rather than opening wide.
func resolveFQDNEmitter(mode gmcv2alpha1.EgressPolicyMode, backend FQDNBackend) fqdnEmitterKind {
	switch mode {
	case gmcv2alpha1.EgressPolicyModeCiliumFQDN:
		return fqdnEmitCilium
	case gmcv2alpha1.EgressPolicyModeCalicoFQDN:
		return fqdnEmitCalico
	case gmcv2alpha1.EgressPolicyModeFQDN:
		switch backend {
		case FQDNBackendCilium:
			return fqdnEmitCilium
		case FQDNBackendCalico:
			return fqdnEmitCalico
		case FQDNBackendGKE:
			return fqdnEmitGKE
		default: // none or unset
			return fqdnEmitNone
		}
	default: // CIDR or empty
		return fqdnEmitNone
	}
}

// githubEgressFQDNs is the GitHub hostname allowlist used by the FQDN egress modes —
// the DNS equivalent of the api/actions/web CIDR families the IPRangeReconciler tracks
// (see ipranges.go githubMetaResponse). Each entry covers a distinct endpoint family a
// runner control-plane and workload must reach: token exchange / registration
// (api.github.com), git + releases (github.com), source archive download for checkout
// (codeload.github.com), release / LFS / object blobs (objects.githubusercontent.com),
// the Actions broker and job logs (*.actions.githubusercontent.com), and the Azure-blob
// backed Actions results / cache / artifact store (*.blob.core.windows.net). A "*."
// prefix is a wildcard subdomain match.
var githubEgressFQDNs = []string{
	"api.github.com",
	"github.com",
	"codeload.github.com",
	"objects.githubusercontent.com",
	"*.actions.githubusercontent.com",
	"*.blob.core.windows.net",
}

// egressFQDNs returns the full FQDN allowlist an FQDN-mode CNI policy must permit:
// the implicit GitHub hostnames (always allowed), the GitHub Enterprise Server hosts
// this proxy's referrers bind to it (Q506 #2, resolveReferrerGitHubHosts), plus the
// operator's extra destinationFQDNs (Q242 G.1). The extras are valid only in an FQDN
// mode (the CRD's CEL rule rejects destinationFQDNs without an FQDN-family
// egressPolicyMode), so they only ever reach the CNI-native policy. A fresh slice is
// returned so callers never mutate the shared githubEgressFQDNs backing array.
func egressFQDNs(ep *gmcv2alpha1.EgressProxy, gitHubHosts []string) []string {
	out := make([]string, 0, len(githubEgressFQDNs)+len(gitHubHosts)+len(ep.Spec.DestinationFQDNs))
	out = append(out, githubEgressFQDNs...)
	out = append(out, gitHubHosts...)
	out = append(out, ep.Spec.DestinationFQDNs...)
	return out
}

// egressModeOf returns the effective egress policy mode, treating the empty string as
// the CIDR default (so a hand-built object that skipped apiserver defaulting still
// behaves like a defaulted one).
func egressModeOf(spec gmcv2alpha1.EgressProxySpec) gmcv2alpha1.EgressPolicyMode {
	if spec.EgressPolicyMode == "" {
		return gmcv2alpha1.EgressPolicyModeCIDR
	}
	return spec.EgressPolicyMode
}

// egressUsesCIDR reports whether the proxy expresses its GitHub allowlist as CIDR
// ranges (the default). When false the standard NetworkPolicy omits the GitHub CIDR
// rule and the IPRangeReconciler skips this proxy.
func egressUsesCIDR(spec gmcv2alpha1.EgressProxySpec) bool {
	return egressModeOf(spec) == gmcv2alpha1.EgressPolicyModeCIDR
}

// egressProxyFQDNPolicyName is the name of the CNI-native FQDN egress policy:
// "<ep>-proxy-fqdn".
func egressProxyFQDNPolicyName(ep *gmcv2alpha1.EgressProxy) string {
	return ep.Name + egressProxyFQDNPolicySuffix
}

// toUnstructuredLabels copies a string label map into the map[string]interface{} shape
// unstructured objects require.
func toUnstructuredLabels(in map[string]string) map[string]interface{} {
	out := make(map[string]interface{}, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// buildEgressProxyCiliumNetworkPolicy builds the CiliumNetworkPolicy emitted in
// CiliumFQDN mode. It selects this pool's proxy pods and allows exactly two egress
// flows: DNS to cluster DNS (with a DNS-visibility rule so Cilium's FQDN proxy learns
// the resolved IPs) and TCP/443 to the GitHub FQDNs via toFQDNs. A CiliumNetworkPolicy
// makes its selected endpoints default-deny for egress, so everything else is denied —
// the same secure-by-default posture as the standard NetworkPolicy's CIDR rule.
func buildEgressProxyCiliumNetworkPolicy(ep *gmcv2alpha1.EgressProxy, gitHubHosts []string) *unstructured.Unstructured {
	allFQDNs := egressFQDNs(ep, gitHubHosts)
	fqdnRules := make([]interface{}, 0, len(allFQDNs))
	for _, f := range allFQDNs {
		if strings.Contains(f, "*") {
			fqdnRules = append(fqdnRules, map[string]interface{}{"matchPattern": f})
		} else {
			fqdnRules = append(fqdnRules, map[string]interface{}{"matchName": f})
		}
	}

	dnsEgress := map[string]interface{}{
		"toEndpoints": []interface{}{
			map[string]interface{}{
				"matchLabels": map[string]interface{}{
					"k8s:io.kubernetes.pod.namespace": dnsNamespaceValue,
					dnsPodLabel:                       dnsPodValue,
				},
			},
			// NodeLocal DNSCache redirect-to-pod path (Q229): on GKE Dataplane V2 the
			// kube-dns ClusterIP is redirected to the per-node node-local-dns pod, whose
			// identity carries k8s-app=node-local-dns, so the kube-dns endpoint above
			// does not match it. Mirrors the standard NetworkPolicy's dnsEgressRule.
			map[string]interface{}{
				"matchLabels": map[string]interface{}{
					"k8s:io.kubernetes.pod.namespace": dnsNamespaceValue,
					dnsPodLabel:                       dnsNodeLocalPodValue,
				},
			},
		},
		"toPorts": []interface{}{
			map[string]interface{}{
				"ports": []interface{}{
					map[string]interface{}{"port": "53", "protocol": "ANY"},
				},
				// DNS visibility: Cilium's DNS proxy must observe the responses to
				// populate the toFQDNs IP cache, otherwise the GitHub rule never matches.
				"rules": map[string]interface{}{
					"dns": []interface{}{
						map[string]interface{}{"matchPattern": "*"},
					},
				},
			},
		},
	}

	githubEgress := map[string]interface{}{
		"toFQDNs": fqdnRules,
		"toPorts": []interface{}{
			map[string]interface{}{
				"ports": []interface{}{
					map[string]interface{}{"port": "443", "protocol": "TCP"},
				},
			},
		},
	}

	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": ciliumNetworkPolicyGVK.GroupVersion().String(),
		"kind":       ciliumNetworkPolicyGVK.Kind,
		"metadata": map[string]interface{}{
			"name":      egressProxyFQDNPolicyName(ep),
			"namespace": ep.Namespace,
			"labels":    toUnstructuredLabels(egressProxyLabels(ep)),
		},
		"spec": map[string]interface{}{
			"endpointSelector": map[string]interface{}{
				"matchLabels": toUnstructuredLabels(egressProxyPodSelector(ep)),
			},
			"egress": []interface{}{dnsEgress, githubEgress},
		},
	}}
}

// buildEgressProxyCalicoNetworkPolicy builds the Calico (projectcalico.org/v3)
// NetworkPolicy emitted in CalicoFQDN mode. It selects this pool's proxy pods and
// allows DNS to cluster DNS plus TCP/443 to the GitHub destination domains. The policy
// declares types: [Egress] with no Allow rule beyond these, so Calico default-denies
// all other egress — matching the CIDR default's posture.
func buildEgressProxyCalicoNetworkPolicy(ep *gmcv2alpha1.EgressProxy, gitHubHosts []string) *unstructured.Unstructured {
	allFQDNs := egressFQDNs(ep, gitHubHosts)
	domains := make([]interface{}, 0, len(allFQDNs))
	for _, f := range allFQDNs {
		domains = append(domains, f)
	}

	// Calico uses a label-expression selector string, not a structured matchLabels.
	// Scope to this pool's proxy pods by the app label and the per-EgressProxy
	// identity label so it never governs another pool.
	podSelector := fmt.Sprintf("app == '%s' && %s == '%s'", proxyAppName, egressProxyComponentLabel, ep.Name)
	dnsNamespaceSelector := fmt.Sprintf("%s == '%s'", dnsNamespaceLabel, dnsNamespaceValue)

	// dnsRule allows port-53 to a specific cluster-DNS pod label. node-local-dns is
	// the redirect backend on GKE Dataplane V2, where the kube-dns ClusterIP is
	// redirected to a per-node node-local-dns pod (Q229); without it DNS is dropped.
	dnsRule := func(protocol, podValue string) map[string]interface{} {
		return map[string]interface{}{
			"action":   "Allow",
			"protocol": protocol,
			"destination": map[string]interface{}{
				"selector":          fmt.Sprintf("%s == '%s'", dnsPodLabel, podValue),
				"namespaceSelector": dnsNamespaceSelector,
				"ports":             []interface{}{int64(53)},
			},
		}
	}

	githubRule := map[string]interface{}{
		"action":   "Allow",
		"protocol": "TCP",
		"destination": map[string]interface{}{
			"domains": domains,
			"ports":   []interface{}{int64(443)},
		},
	}

	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": calicoNetworkPolicyGVK.GroupVersion().String(),
		"kind":       calicoNetworkPolicyGVK.Kind,
		"metadata": map[string]interface{}{
			"name":      egressProxyFQDNPolicyName(ep),
			"namespace": ep.Namespace,
			"labels":    toUnstructuredLabels(egressProxyLabels(ep)),
		},
		"spec": map[string]interface{}{
			"selector": podSelector,
			"types":    []interface{}{"Egress"},
			"egress": []interface{}{
				dnsRule("UDP", dnsPodValue), dnsRule("TCP", dnsPodValue),
				dnsRule("UDP", dnsNodeLocalPodValue), dnsRule("TCP", dnsNodeLocalPodValue),
				githubRule,
			},
		},
	}}
}

// buildEgressProxyGKEFQDNNetworkPolicy builds the managed GKE Dataplane V2 built-in
// networking.gke.io/v1alpha1 FQDNNetworkPolicy emitted for the gke backend (Q245). It
// selects this pool's proxy pods and allows TCP/443 to the GitHub FQDNs, each split into
// {name: …} (exact) or {pattern: …} (wildcard) exactly like the Cilium/Calico builders.
//
// Unlike Cilium/Calico, this object carries NO DNS rule and is NOT self-default-denying:
// GKE's FQDNNetworkPolicy is additive-allow (a union with any NetworkPolicy on the same
// pod), and DNS bypasses FQDN enforcement entirely. Both concerns are handled by the base
// standard NetworkPolicy that the reconciler always applies — it default-denies GitHub
// egress (dropping the CIDR rule) and carries the DNS-only allow (including the Q229
// node-local-dns peer). So the fail-closed guarantee for gke DEPENDS on that base NP being
// present: the FQDNNetworkPolicy only widens the union to permit GitHub, and if it is
// absent or unenforced, GitHub egress stays denied by the base NP rather than opening wide.
func buildEgressProxyGKEFQDNNetworkPolicy(ep *gmcv2alpha1.EgressProxy, gitHubHosts []string) *unstructured.Unstructured {
	allFQDNs := egressFQDNs(ep, gitHubHosts)
	matches := make([]interface{}, 0, len(allFQDNs))
	for _, f := range allFQDNs {
		if strings.Contains(f, "*") {
			matches = append(matches, map[string]interface{}{"pattern": f})
		} else {
			matches = append(matches, map[string]interface{}{"name": f})
		}
	}

	githubEgress := map[string]interface{}{
		"matches": matches,
		"ports": []interface{}{
			map[string]interface{}{"protocol": "TCP", "port": int64(443)},
		},
	}

	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": gkeFQDNNetworkPolicyGVK.GroupVersion().String(),
		"kind":       gkeFQDNNetworkPolicyGVK.Kind,
		"metadata": map[string]interface{}{
			"name":      egressProxyFQDNPolicyName(ep),
			"namespace": ep.Namespace,
			"labels":    toUnstructuredLabels(egressProxyLabels(ep)),
		},
		"spec": map[string]interface{}{
			"podSelector": map[string]interface{}{
				"matchLabels": toUnstructuredLabels(egressProxyPodSelector(ep)),
			},
			"egress": []interface{}{githubEgress},
		},
	}}
}
