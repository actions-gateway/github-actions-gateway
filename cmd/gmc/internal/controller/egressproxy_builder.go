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
	"net"
	"strings"

	"github.com/actions-gateway/github-actions-gateway/api/apilabels"
	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// v2 EgressProxy resource derivation. Unlike v1's one-proxy-per-namespace model
// (fixed gmcnames.ProxyName), v2 permits multiple EgressProxy objects in one
// namespace, so every derived resource name and pod selector is keyed on the
// EgressProxy's own name. The derived names stay within the 52-char-name budget
// the CRD enforces (§H.6): "<ep>-proxy" adds 6 chars, "<ep>-proxy-tls" adds 10.
const (
	// egressProxyComponentLabel carries the owning EgressProxy's name on every
	// child object and pod. It is load-bearing twice over:
	//   1. Selector isolation — multiple EgressProxy pools in one namespace must not
	//      collide on a shared selector (v1 could assume a single proxy per
	//      namespace). Every Deployment/Service/PDB/HPA/NetworkPolicy selector and
	//      the pod anti-affinity key on this label.
	//   2. Free observability win (§H.8) — a per-EgressProxy Deployment means proxy
	//      metrics carry the proxy identity automatically once a scrape is wired.
	egressProxyComponentLabel = "actions-gateway.com/egress-proxy"

	// proxyContainerName / egressProxyResourceSuffix / egressProxyTLSSuffix derive
	// the child resource identities from the EgressProxy name.
	proxyContainerName        = "proxy"
	egressProxyResourceSuffix = "-proxy"
	egressProxyTLSSuffix      = "-proxy-tls"
)

// proxyResourceName is the name shared by an EgressProxy's Deployment, Service,
// HPA, PDB, and NetworkPolicy: "<ep>-proxy".
func proxyResourceName(ep *gmcv2alpha1.EgressProxy) string {
	return ep.Name + egressProxyResourceSuffix
}

// egressProxyTLSSecretName is the name of the EgressProxy's self-signed proxy TLS
// Secret: "<ep>-proxy-tls".
func egressProxyTLSSecretName(ep *gmcv2alpha1.EgressProxy) string {
	return ep.Name + egressProxyTLSSuffix
}

// egressProxyLabels returns the metadata labels stamped on every EgressProxy
// child: the managed-by marker plus the per-EgressProxy identity label.
func egressProxyLabels(ep *gmcv2alpha1.EgressProxy) map[string]string {
	l := apilabels.Recommended(proxyAppName, ep.Name, componentProxyLabel, "", labelManagerValue)
	l[egressProxyComponentLabel] = ep.Name
	return l
}

// egressProxyPodSelector returns the label set used as both the pod template
// labels and the Deployment/Service/PDB/NetworkPolicy selector. It carries the
// egress-proxy identity (so it never selects another EgressProxy's pods) and the
// generic app label so workload NetworkPolicies and tooling can match proxy pods.
func egressProxyPodSelector(ep *gmcv2alpha1.EgressProxy) map[string]string {
	return map[string]string{
		"app":                     proxyAppName,
		egressProxyComponentLabel: ep.Name,
	}
}

// egressProxyMinReplicas / egressProxyMaxReplicas / egressProxyTargetCPU read the
// spec knobs, falling back to the CRD defaults so a hand-built object (e.g. a unit
// test that skips apiserver defaulting) still produces a coherent pool.
func egressProxyMinReplicas(ep *gmcv2alpha1.EgressProxy) int32 {
	if ep.Spec.MinReplicas != nil {
		return *ep.Spec.MinReplicas
	}
	return 2
}

func egressProxyMaxReplicas(ep *gmcv2alpha1.EgressProxy) int32 {
	if ep.Spec.MaxReplicas != nil {
		return *ep.Spec.MaxReplicas
	}
	return 10
}

func egressProxyTargetCPU(ep *gmcv2alpha1.EgressProxy) int32 {
	if ep.Spec.TargetCPUUtilizationPercentage != nil {
		return *ep.Spec.TargetCPUUtilizationPercentage
	}
	return 60
}

// egressProxyResources returns the proxy container's resource requirements: the
// secure defaults overlaid with any spec.resources overrides — the same defaults
// and merge semantics as v1's proxyResources, decoupled from ActionsGateway.
func egressProxyResources(ep *gmcv2alpha1.EgressProxy) corev1.ResourceRequirements {
	res := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("10m"),
			corev1.ResourceMemory: resource.MustParse("32Mi"),
		},
		Limits: corev1.ResourceList{
			// 500m, not 100m: a 100m limit throttles the proxy before the HPA's
			// CPU-utilization signal can trip, so the pool never scales out under
			// real CONNECT load. Operators override via spec.resources.
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("64Mi"),
		},
	}
	for k, v := range ep.Spec.Resources.Requests {
		res.Requests[k] = v
	}
	for k, v := range ep.Spec.Resources.Limits {
		res.Limits[k] = v
	}
	return res
}

// proxyAllowlistEnv returns the CONNECT destination-allowlist env (Q242 G.1) for
// the proxy container, or nil when the operator has not opted in.
//
// The proxy's CONNECT check is defense-in-depth on top of the pod-egress policy
// (the hard gate). To keep existing proxies byte-for-byte unchanged, the env is
// injected ONLY when the EgressProxy lists at least one extra destination
// (destinationFQDNs/destinationCIDRs); with no extras the proxy stays
// transport-only and the NetworkPolicy alone gates egress.
//
// When opted in, the proxy must permit the FULL set the egress policy allows, so:
//   - PROXY_ALLOWED_HOST_SUFFIXES carries the implicit GitHub hostnames PLUS the
//     operator's destinationFQDNs. Workers always reach GitHub by hostname, so the
//     GitHub set is expressed as host suffixes here regardless of egressPolicyMode;
//     the ~7400 GitHub CIDRs are deliberately NOT injected (no worker CONNECTs to
//     GitHub by literal IP, and an env var of thousands of CIDRs would be unwieldy).
//   - PROXY_ALLOWED_CIDRS carries the operator's destinationCIDRs only.
func proxyAllowlistEnv(ep *gmcv2alpha1.EgressProxy) []corev1.EnvVar {
	if len(ep.Spec.DestinationFQDNs) == 0 && len(ep.Spec.DestinationCIDRs) == 0 {
		return nil
	}
	suffixes := make([]string, 0, len(githubEgressFQDNs)+len(ep.Spec.DestinationFQDNs))
	for _, f := range githubEgressFQDNs {
		suffixes = append(suffixes, proxyHostSuffix(f))
	}
	suffixes = append(suffixes, ep.Spec.DestinationFQDNs...)

	env := []corev1.EnvVar{{Name: "PROXY_ALLOWED_HOST_SUFFIXES", Value: strings.Join(suffixes, ",")}}
	if len(ep.Spec.DestinationCIDRs) > 0 {
		env = append(env, corev1.EnvVar{Name: "PROXY_ALLOWED_CIDRS", Value: strings.Join(ep.Spec.DestinationCIDRs, ",")})
	}
	return env
}

// proxyHostSuffix normalizes an FQDN-policy entry into the bare host suffix the
// proxy's CONNECT suffix matcher expects: a leading "*." wildcard becomes the
// parent domain (the matcher already treats every entry as a subdomain suffix, so
// "actions.githubusercontent.com" matches "x.actions.githubusercontent.com").
func proxyHostSuffix(fqdn string) string {
	return strings.TrimPrefix(fqdn, "*.")
}

// buildEgressProxyDeployment builds the proxy pool Deployment for an EgressProxy.
// It mirrors v1's buildProxyDeployment (hardened container/pod SecurityContext,
// required cross-node anti-affinity, self-signed proxy TLS mount, /healthz +
// /readyz probes, 60s graceful drain) but is keyed on the EgressProxy: the pod
// labels/selector and the anti-affinity term use the per-EgressProxy identity so
// pools in one namespace stay isolated.
//
// M2 omits v1's metrics-mTLS volume/port/env: that listener shares a per-tenant
// metrics CA jointly owned with the AGC, which lands in M3a. Exposing a metrics
// port with no cert would serve plaintext (a regression) or nothing; the proxy
// binary boots fine without it (cmd/proxy/main.go treats metrics mTLS as optional).
func buildEgressProxyDeployment(ep *gmcv2alpha1.EgressProxy, proxyImage string) *appsv1.Deployment {
	replicas := egressProxyMinReplicas(ep)
	name := proxyResourceName(ep)
	selector := egressProxyPodSelector(ep)

	// Pod template labels: the functional selector (used as-is for matchLabels and
	// anti-affinity) plus the recommended app.kubernetes.io/* metadata, layered on a
	// clone so the selector map the Deployment/Service match on is never mutated.
	podLabels := map[string]string{}
	for k, v := range selector {
		podLabels[k] = v
	}
	apilabels.Merge(podLabels, proxyAppName, ep.Name, componentProxyLabel, "", labelManagerValue)

	// Mode 0o440 + fsGroup 65532: the non-root distroless proxy reads the cert via
	// group ownership without making it world-readable. See v1 buildProxyDeployment.
	tlsMode := int32(0o440)

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ep.Namespace, Labels: egressProxyLabels(ep)},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: selector},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: podLabels},
				Spec: corev1.PodSpec{
					SecurityContext:               nonrootPodSecurityContext(),
					TerminationGracePeriodSeconds: ptr(int64(60)),
					// Required (not preferred) anti-affinity keyed on this pool's
					// identity: replicas land on distinct nodes so one node failure
					// never takes the whole pool down. Trade-off: needs at least
					// minReplicas schedulable nodes (set minReplicas=1 on single-node
					// dev clusters).
					Affinity: &corev1.Affinity{
						PodAntiAffinity: &corev1.PodAntiAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
								LabelSelector: &metav1.LabelSelector{MatchLabels: selector},
								TopologyKey:   "kubernetes.io/hostname",
							}},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: proxyTLSVolumeName,
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName:  egressProxyTLSSecretName(ep),
									DefaultMode: &tlsMode,
								},
							},
						},
					},
					Containers: []corev1.Container{{
						Name:      proxyContainerName,
						Image:     proxyImage,
						Resources: egressProxyResources(ep),
						Ports: []corev1.ContainerPort{
							{Name: "proxy", ContainerPort: proxyPort, Protocol: corev1.ProtocolTCP},
							{Name: "health", ContainerPort: healthMetricsPort, Protocol: corev1.ProtocolTCP},
						},
						Env: append([]corev1.EnvVar{
							{Name: "PROXY_TLS_CERT_FILE", Value: proxyTLSMountPath + "/" + corev1.TLSCertKey},
							{Name: "PROXY_TLS_KEY_FILE", Value: proxyTLSMountPath + "/" + corev1.TLSPrivateKeyKey},
						}, proxyAllowlistEnv(ep)...),
						VolumeMounts: []corev1.VolumeMount{{
							Name:      proxyTLSVolumeName,
							MountPath: proxyTLSMountPath,
							ReadOnly:  true,
						}},
						LivenessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt32(healthMetricsPort)},
							},
						},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{Path: "/readyz", Port: intstr.FromInt32(healthMetricsPort)},
							},
						},
						SecurityContext: hardenedContainerSecurityContext(),
					}},
				},
			},
		},
	}
}

// buildEgressProxyService builds the ClusterIP Service that fronts an EgressProxy's
// pool on the proxy port. The selector carries the per-EgressProxy identity so it
// never fronts another pool's pods.
func buildEgressProxyService(ep *gmcv2alpha1.EgressProxy) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: proxyResourceName(ep), Namespace: ep.Namespace, Labels: egressProxyLabels(ep)},
		Spec: corev1.ServiceSpec{
			Selector: egressProxyPodSelector(ep),
			Ports: []corev1.ServicePort{
				{Name: "proxy", Port: proxyPort, TargetPort: intstr.FromInt32(proxyPort), Protocol: corev1.ProtocolTCP},
			},
			Type: corev1.ServiceTypeClusterIP,
		},
	}
}

// buildEgressProxyHPA builds the HorizontalPodAutoscaler that scales an
// EgressProxy's Deployment on CPU utilization, between spec.minReplicas and
// spec.maxReplicas. Mirrors v1's buildHPA, keyed on the EgressProxy.
func buildEgressProxyHPA(ep *gmcv2alpha1.EgressProxy) *autoscalingv2.HorizontalPodAutoscaler {
	minReplicas := egressProxyMinReplicas(ep)
	maxReplicas := egressProxyMaxReplicas(ep)
	targetCPU := egressProxyTargetCPU(ep)
	name := proxyResourceName(ep)
	return &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ep.Namespace, Labels: egressProxyLabels(ep)},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: name},
			MinReplicas:    &minReplicas,
			MaxReplicas:    maxReplicas,
			Metrics: []autoscalingv2.MetricSpec{{
				Type: autoscalingv2.ResourceMetricSourceType,
				Resource: &autoscalingv2.ResourceMetricSource{
					Name: corev1.ResourceCPU,
					Target: autoscalingv2.MetricTarget{
						Type:               autoscalingv2.UtilizationMetricType,
						AverageUtilization: &targetCPU,
					},
				},
			}},
		},
	}
}

// buildEgressProxyPDB builds the PodDisruptionBudget that keeps at least one proxy
// replica available across voluntary disruptions. Mirrors v1's buildPDB, keyed on
// the EgressProxy identity selector.
func buildEgressProxyPDB(ep *gmcv2alpha1.EgressProxy) *policyv1.PodDisruptionBudget {
	return &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: proxyResourceName(ep), Namespace: ep.Namespace, Labels: egressProxyLabels(ep)},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: ptr(intstr.FromInt32(1)),
			Selector:     &metav1.LabelSelector{MatchLabels: egressProxyPodSelector(ep)},
		},
	}
}

// buildEgressProxyNetworkPolicy builds the egress-lockdown NetworkPolicy for an
// EgressProxy's pool. It mirrors v1's buildProxyNetworkPolicy and preserves its
// secure-by-default semantics exactly:
//   - Egress: DNS always; GitHub CIDRs on 443 only when managedNetworkPolicy is
//     true (the default), the egress mode is CIDR (the default), and the IP cache is
//     populated. managedNetworkPolicy=false omits the GitHub rule so an operator can
//     layer their own (NetworkPolicies are additive), never a silent loosening. In an
//     FQDN egress mode (Q208) the GitHub rule is also omitted here — a CNI-native
//     policy carries the GitHub allowlist instead — so this policy default-denies
//     GitHub egress and the posture stays fail-closed if the CNI cannot enforce the
//     FQDN policy.
//   - Ingress: workload pods may reach the proxy port; default-deny otherwise.
//
// The pod selector keys on the per-EgressProxy identity so the policy governs only
// this pool's pods.
func buildEgressProxyNetworkPolicy(ep *gmcv2alpha1.EgressProxy, githubCIDRs []net.IPNet) *networkingv1.NetworkPolicy {
	egress := []networkingv1.NetworkPolicyEgressRule{dnsEgressRule()}
	managed := ep.Spec.ManagedNetworkPolicy == nil || *ep.Spec.ManagedNetworkPolicy
	if managed && egressUsesCIDR(ep.Spec) && len(githubCIDRs) > 0 {
		peers := make([]networkingv1.NetworkPolicyPeer, 0, len(githubCIDRs))
		for _, cidr := range githubCIDRs {
			c := cidr.String()
			peers = append(peers, networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: c}})
		}
		egress = append(egress, networkingv1.NetworkPolicyEgressRule{
			Ports: []networkingv1.NetworkPolicyPort{{Port: ptr(intstr.FromInt32(443))}},
			To:    peers,
		})
	}

	// Operator-allowlisted extra CIDRs (Q242 G.1) are native ipBlock peers on the
	// standard NetworkPolicy, which is applied in EVERY egress mode — so a CIDR
	// destination works without a DNS-aware CNI and regardless of how GitHub egress
	// is expressed (CIDR rule here vs. toFQDNs on the CNI-native policy). FQDN-mode
	// proxies still get the standard NetworkPolicy, so these peers are honored there
	// too (NetworkPolicies are additive). Gated on managedNetworkPolicy: when the GMC
	// is not managing this proxy's policy, it adds nothing for the operator to layer.
	if managed && len(ep.Spec.DestinationCIDRs) > 0 {
		peers := make([]networkingv1.NetworkPolicyPeer, 0, len(ep.Spec.DestinationCIDRs))
		for _, c := range ep.Spec.DestinationCIDRs {
			peers = append(peers, networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: c}})
		}
		egress = append(egress, networkingv1.NetworkPolicyEgressRule{
			Ports: []networkingv1.NetworkPolicyPort{{Port: ptr(intstr.FromInt32(443))}},
			To:    peers,
		})
	}

	ingress := []networkingv1.NetworkPolicyIngressRule{{
		From: []networkingv1.NetworkPolicyPeer{{
			PodSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{labelComponent: componentWorkload},
			},
		}},
		Ports: []networkingv1.NetworkPolicyPort{{Port: ptr(intstr.FromInt32(proxyPort))}},
	}}

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: proxyResourceName(ep), Namespace: ep.Namespace, Labels: egressProxyLabels(ep)},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: egressProxyPodSelector(ep)},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress, networkingv1.PolicyTypeIngress},
			Egress:      egress,
			Ingress:     ingress,
		},
	}
}

// buildEgressProxyCertSecret constructs the kubernetes.io/tls Secret holding the
// EgressProxy's self-signed proxy cert+key. The proxy Deployment mounts both; a
// same-namespace consumer (the AGC, wired in M3a) mounts only the public cert.
func buildEgressProxyCertSecret(ep *gmcv2alpha1.EgressProxy, certPEM, keyPEM []byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      egressProxyTLSSecretName(ep),
			Namespace: ep.Namespace,
			Labels:    egressProxyLabels(ep),
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       certPEM,
			corev1.TLSPrivateKeyKey: keyPEM,
		},
	}
}
