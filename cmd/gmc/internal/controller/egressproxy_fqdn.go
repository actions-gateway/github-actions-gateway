/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

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

// ciliumNetworkPolicyGVK / calicoNetworkPolicyGVK identify the CNI-native policy
// kinds the GMC emits in FQDN mode. They are addressed as unstructured objects so the
// GMC never takes a compile-time dependency on the Cilium or Calico API modules, and
// so a cluster without the matching CRD installed simply NoMatch-errors loudly rather
// than forcing the dependency on every install.
var (
	ciliumNetworkPolicyGVK = schema.GroupVersionKind{Group: "cilium.io", Version: "v2", Kind: "CiliumNetworkPolicy"}
	calicoNetworkPolicyGVK = schema.GroupVersionKind{Group: "projectcalico.org", Version: "v3", Kind: "NetworkPolicy"}
)

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
func buildEgressProxyCiliumNetworkPolicy(ep *gmcv2alpha1.EgressProxy) *unstructured.Unstructured {
	fqdnRules := make([]interface{}, 0, len(githubEgressFQDNs))
	for _, f := range githubEgressFQDNs {
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
func buildEgressProxyCalicoNetworkPolicy(ep *gmcv2alpha1.EgressProxy) *unstructured.Unstructured {
	domains := make([]interface{}, 0, len(githubEgressFQDNs))
	for _, f := range githubEgressFQDNs {
		domains = append(domains, f)
	}

	// Calico uses a label-expression selector string, not a structured matchLabels.
	// Scope to this pool's proxy pods by the app label and the per-EgressProxy
	// identity label so it never governs another pool.
	podSelector := fmt.Sprintf("app == '%s' && %s == '%s'", proxyAppName, egressProxyComponentLabel, ep.Name)
	dnsSelector := fmt.Sprintf("%s == '%s'", dnsPodLabel, dnsPodValue)
	dnsNamespaceSelector := fmt.Sprintf("%s == '%s'", dnsNamespaceLabel, dnsNamespaceValue)

	dnsRule := func(protocol string) map[string]interface{} {
		return map[string]interface{}{
			"action":   "Allow",
			"protocol": protocol,
			"destination": map[string]interface{}{
				"selector":          dnsSelector,
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
			"egress":   []interface{}{dnsRule("UDP"), dnsRule("TCP"), githubRule},
		},
	}}
}
