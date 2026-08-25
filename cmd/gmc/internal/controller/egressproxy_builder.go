package controller

import (
	"fmt"
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
	//      namespace). It is the SOLE key of every Deployment/Service/PDB/
	//      NetworkPolicy selector and of the pod anti-affinity term, which is what
	//      also keeps a pool clear of v1's coexisting one — see
	//      egressProxyPodSelector (Q582).
	//   2. Free observability win (§H.8) — a per-EgressProxy Deployment means proxy
	//      metrics carry the proxy identity automatically once a scrape is wired.
	egressProxyComponentLabel = "actions-gateway.com/egress-proxy"

	// proxyContainerName / egressProxyResourceSuffix / egressProxyTLSSuffix derive
	// the child resource identities from the EgressProxy name.
	proxyContainerName        = "proxy"
	egressProxyResourceSuffix = "-proxy"
	egressProxyTLSSuffix      = "-proxy-tls"

	// auditLoggingOff is the EgressProxy.spec.auditLogging value that emits no
	// per-connection record — the CRD default, and the only one that injects no
	// audit env onto the proxy container.
	auditLoggingOff = "Off"
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
// labels and the Deployment/Service/PDB/NetworkPolicy selector. The per-EgressProxy
// identity is the whole selector, so it never selects another EgressProxy's pods —
// nor v1's.
//
// It deliberately does NOT carry v1's generic `app: actions-gateway-proxy` (Q582).
// v1 has one fixed-name pool per namespace and keys its PDB selector, its Deployment
// selector, and its required hostname anti-affinity term on that bare label, so a v2
// pod wearing it is claimed by all three throughout a migration's coexistence window:
// each pool's pods fall under the other's PDB, both HPAs wedge on AmbiguousSelector
// (the HPA controller reads the scale target's — the Deployment's — selector and
// refuses to act on pods another HPA also controls, so neither pool autoscales;
// measured on Kubernetes v1.35.5 and v1.36.1, Q591), and the two pools repel each
// other off every node. The generic "this is a proxy pod"
// identity for humans and tooling is the recommended `app.kubernetes.io/name` label,
// which is additive metadata that nothing selects on; the v2 workload NetworkPolicy
// reaches proxy pods via egressProxyPodPeerSelector instead.
func egressProxyPodSelector(ep *gmcv2alpha1.EgressProxy) map[string]string {
	return map[string]string{egressProxyComponentLabel: ep.Name}
}

// egressProxyPodPeerSelector selects the pods of EVERY EgressProxy pool in a
// namespace — any pod carrying the identity label, whichever pool owns it. It is the
// v2 workload NetworkPolicy's proxy peer: a gateway's RunnerSets may each name their
// own proxyRef, so the policy cannot key on one pool's name. No v1 proxy pod carries
// the label, so v2 workload egress no longer reaches v1's pool either (Q582).
func egressProxyPodPeerSelector() *metav1.LabelSelector {
	return &metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{{
			Key:      egressProxyComponentLabel,
			Operator: metav1.LabelSelectorOpExists,
		}},
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

// egressProxyManagedAutoscaling reports whether the GMC manages this pool's HPA
// (spec.managedAutoscaling, default true — Q173). False means the operator brings
// their own autoscaler and the GMC provisions no HPA.
func egressProxyManagedAutoscaling(ep *gmcv2alpha1.EgressProxy) bool {
	return ep.Spec.ManagedAutoscaling == nil || *ep.Spec.ManagedAutoscaling
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
//   - PROXY_ALLOWED_HOST_SUFFIXES carries the implicit GitHub hostnames, the GitHub
//     Enterprise Server hosts this proxy's referrers bind to it (Q506 #2), PLUS the
//     operator's destinationFQDNs. Workers always reach GitHub by hostname, so the
//     GitHub set is expressed as host suffixes here regardless of egressPolicyMode;
//     the ~7400 GitHub CIDRs are deliberately NOT injected (no worker CONNECTs to
//     GitHub by literal IP, and an env var of thousands of CIDRs would be unwieldy).
//   - PROXY_ALLOWED_CIDRS carries the operator's destinationCIDRs only.
func proxyAllowlistEnv(ep *gmcv2alpha1.EgressProxy, gitHubHosts []string) []corev1.EnvVar {
	if len(ep.Spec.DestinationFQDNs) == 0 && len(ep.Spec.DestinationCIDRs) == 0 {
		return nil
	}
	suffixes := make([]string, 0, len(githubEgressFQDNs)+len(gitHubHosts)+len(ep.Spec.DestinationFQDNs))
	for _, f := range githubEgressFQDNs {
		suffixes = append(suffixes, proxyHostSuffix(f))
	}
	suffixes = append(suffixes, gitHubHosts...)
	suffixes = append(suffixes, ep.Spec.DestinationFQDNs...)

	env := []corev1.EnvVar{{Name: "PROXY_ALLOWED_HOST_SUFFIXES", Value: strings.Join(suffixes, ",")}}
	if len(ep.Spec.DestinationCIDRs) > 0 {
		env = append(env, corev1.EnvVar{Name: "PROXY_ALLOWED_CIDRS", Value: strings.Join(ep.Spec.DestinationCIDRs, ",")})
	}
	return env
}

// proxyAuditEnv returns the per-connection egress audit env (Q564, design G.3)
// for the proxy container, or nil when the pool has not opted in.
//
// Injected only when spec.auditLogging is a non-Off value, for the same reason
// proxyAllowlistEnv is conditional: a pool that has not opted in keeps a
// byte-for-byte unchanged pod template, so upgrading the GMC does not roll every
// proxy pool in the cluster. The proxy binary defaults to Off with the variable
// absent, so the off-by-default guarantee holds at both ends independently.
//
// POD_NAMESPACE rides along from the downward API rather than being formatted in
// here: the record must name the namespace the pod actually runs in, and the
// downward API is the one source a template substitution cannot get wrong.
func proxyAuditEnv(ep *gmcv2alpha1.EgressProxy) []corev1.EnvVar {
	if ep.Spec.AuditLogging == "" || ep.Spec.AuditLogging == auditLoggingOff {
		return nil
	}
	return []corev1.EnvVar{
		{Name: "PROXY_AUDIT_LOGGING", Value: ep.Spec.AuditLogging},
		{Name: "POD_NAMESPACE", ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"},
		}},
	}
}

// proxyHostSuffix normalizes an FQDN-policy entry into the bare host suffix the
// proxy's CONNECT suffix matcher expects: a leading "*." wildcard becomes the
// parent domain (the matcher already treats every entry as a subdomain suffix, so
// "actions.githubusercontent.com" matches "x.actions.githubusercontent.com").
func proxyHostSuffix(fqdn string) string {
	return strings.TrimPrefix(fqdn, "*.")
}

// defaultEgressProxyAntiAffinity is the built-in cross-node spread every proxy pool
// gets unless the author overrides it: a REQUIRED anti-affinity keyed on this pool's
// identity, so replicas land on distinct nodes and one node failure never takes the
// whole pool down. Trade-off: it needs at least minReplicas schedulable nodes.
func defaultEgressProxyAntiAffinity(selector map[string]string) *corev1.PodAntiAffinity {
	return &corev1.PodAntiAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
			LabelSelector: &metav1.LabelSelector{MatchLabels: selector},
			TopologyKey:   "kubernetes.io/hostname",
		}},
	}
}

// egressProxyAffinity composes spec.scheduling.affinity with the built-in cross-node
// anti-affinity (Q282). The precedence contract, documented on the PodScheduling API
// type and in docs/operations/:
//
//   - No affinity supplied ⇒ the built-in anti-affinity alone.
//   - nodeAffinity / podAffinity supplied ⇒ applied as given, and the built-in
//     anti-affinity is PRESERVED alongside them (the common case: pin to a node pool,
//     keep spreading across that pool's nodes).
//   - podAntiAffinity supplied (any non-nil value) ⇒ it REPLACES the built-in term
//     entirely — set it and you own it. An explicit `podAntiAffinity: {}` is a non-nil
//     pointer with no terms, so it opts out of spreading altogether. That is the
//     escape hatch a single-node tenant node pool needs: the required built-in term
//     would otherwise strand every replica after the first in Pending (Q243).
func egressProxyAffinity(ep *gmcv2alpha1.EgressProxy, selector map[string]string) *corev1.Affinity {
	builtIn := defaultEgressProxyAntiAffinity(selector)
	if ep.Spec.Scheduling == nil || ep.Spec.Scheduling.Affinity == nil {
		return &corev1.Affinity{PodAntiAffinity: builtIn}
	}
	// DeepCopy: the returned Affinity is written into a Deployment the caller may
	// mutate, and ep is a shared informer object that must never be modified.
	affinity := ep.Spec.Scheduling.Affinity.DeepCopy()
	if affinity.PodAntiAffinity == nil {
		affinity.PodAntiAffinity = builtIn
	}
	return affinity
}

// egressProxyNodeSelector / egressProxyTolerations return the pass-through placement
// fields, deep-copied out of the (shared, informer-owned) EgressProxy spec.
func egressProxyNodeSelector(ep *gmcv2alpha1.EgressProxy) map[string]string {
	if ep.Spec.Scheduling == nil || len(ep.Spec.Scheduling.NodeSelector) == 0 {
		return nil
	}
	out := make(map[string]string, len(ep.Spec.Scheduling.NodeSelector))
	for k, v := range ep.Spec.Scheduling.NodeSelector {
		out[k] = v
	}
	return out
}

func egressProxyTolerations(ep *gmcv2alpha1.EgressProxy) []corev1.Toleration {
	if ep.Spec.Scheduling == nil || len(ep.Spec.Scheduling.Tolerations) == 0 {
		return nil
	}
	out := make([]corev1.Toleration, len(ep.Spec.Scheduling.Tolerations))
	for i, t := range ep.Spec.Scheduling.Tolerations {
		t.DeepCopyInto(&out[i])
	}
	return out
}

// egressProxyTopologySpreadConstraints returns the pass-through
// topologySpreadConstraints, deep-copied out of the (shared, informer-owned)
// EgressProxy spec. It COMPOSES with the built-in cross-node anti-affinity rather
// than replacing it (Q284): the constraints are applied verbatim alongside the
// anti-affinity that egressProxyAffinity still stamps, so a soft zonal spread narrows
// placement without dropping the cross-node durability invariant.
func egressProxyTopologySpreadConstraints(ep *gmcv2alpha1.EgressProxy) []corev1.TopologySpreadConstraint {
	if ep.Spec.Scheduling == nil || len(ep.Spec.Scheduling.TopologySpreadConstraints) == 0 {
		return nil
	}
	out := make([]corev1.TopologySpreadConstraint, len(ep.Spec.Scheduling.TopologySpreadConstraints))
	for i, c := range ep.Spec.Scheduling.TopologySpreadConstraints {
		c.DeepCopyInto(&out[i])
	}
	return out
}

// egressProxyPriorityClassName returns the pass-through priorityClassName, or "" when
// unset. It is a bare string, so no copy is needed; the empty default leaves the pod
// unprioritized. The name is gated at admission against the infra-only allowlist
// (--allowed-infra-priority-classes, Q284), not the worker allowlist.
func egressProxyPriorityClassName(ep *gmcv2alpha1.EgressProxy) string {
	if ep.Spec.Scheduling == nil {
		return ""
	}
	return ep.Spec.Scheduling.PriorityClassName
}

// buildEgressProxyDeployment builds the proxy pool Deployment for an EgressProxy.
// It mirrors v1's buildProxyDeployment (hardened container/pod SecurityContext,
// cross-node anti-affinity, self-signed proxy TLS mount, /healthz + /readyz probes,
// 60s graceful drain, mTLS /metrics listener) but is keyed on the EgressProxy: the
// pod labels/selector and the anti-affinity term use the per-EgressProxy identity so
// pools in one namespace stay isolated. Unlike v1's, the anti-affinity is a *default*
// the author can override via spec.scheduling.affinity (Q282) — see egressProxyAffinity.
//
// Metrics mTLS (Q324): the proxy serves /metrics over mutual TLS on metricsPort,
// verifying scraper client certs against the per-EgressProxy metrics CA. The three
// PROXY_METRICS_* env vars mirror the classic proxy exactly; without them the proxy
// binary falls back to serving /metrics plaintext on the health port, so mounting
// the bundle is what keeps the endpoint from regressing to unauthenticated plaintext.
// The bundle is a per-EgressProxy PKI (ensureMetricsCerts), never shared cross-tenant.
func buildEgressProxyDeployment(ep *gmcv2alpha1.EgressProxy, proxyImage string, gitHubHosts []string) *appsv1.Deployment {
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
					SecurityContext: nonrootPodSecurityContext(),
					// Covers the proxy's whole SIGTERM sequence — see the arithmetic
					// above proxyDrainBudgetSeconds in builder.go. Shared with v1's
					// buildProxyDeployment: same image, same shutdown sequence.
					TerminationGracePeriodSeconds: ptr(int64(proxyTerminationGracePeriodSeconds)),
					// Placement pass-through (Q282): spec.scheduling pins the pool to a
					// tenant's node pool — and thus to that pool's egress IP (Q243).
					// egressProxyAffinity composes any supplied affinity with the
					// built-in required cross-node anti-affinity; see its doc comment
					// for the precedence contract.
					Affinity:                  egressProxyAffinity(ep, selector),
					NodeSelector:              egressProxyNodeSelector(ep),
					Tolerations:               egressProxyTolerations(ep),
					TopologySpreadConstraints: egressProxyTopologySpreadConstraints(ep),
					PriorityClassName:         egressProxyPriorityClassName(ep),
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
						{
							// Metrics mTLS server bundle (ca.crt + tls.crt + tls.key):
							// the proxy serves /metrics over mTLS on metricsPort and
							// verifies scraper client certs against ca.crt (Q324).
							Name: metricsTLSVolumeName,
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName:  egressProxyMetricsTLSSecretName(ep),
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
							{Name: "metrics", ContainerPort: metricsPort, Protocol: corev1.ProtocolTCP},
						},
						Env: append([]corev1.EnvVar{
							{Name: "PROXY_TLS_CERT_FILE", Value: proxyTLSMountPath + "/" + corev1.TLSCertKey},
							{Name: "PROXY_TLS_KEY_FILE", Value: proxyTLSMountPath + "/" + corev1.TLSPrivateKeyKey},
							// Metrics mTLS listener (Q324). All three files are mounted
							// from the per-EgressProxy metrics bundle; the proxy binary
							// enables the dedicated mTLS /metrics listener only when all
							// three are set (else it falls back to plaintext metrics on
							// the health port), so this is what enforces the mTLS gate.
							{Name: "PROXY_METRICS_PORT", Value: fmt.Sprintf("%d", metricsPort)},
							{Name: "PROXY_METRICS_TLS_CERT_FILE", Value: metricsTLSMountPath + "/" + corev1.TLSCertKey},
							{Name: "PROXY_METRICS_TLS_KEY_FILE", Value: metricsTLSMountPath + "/" + corev1.TLSPrivateKeyKey},
							{Name: "PROXY_METRICS_CLIENT_CA_FILE", Value: metricsTLSMountPath + "/" + metricsCACertKey},
							// LOG_LEVEL mirrors spec.logLevel (info|debug, default
							// info) — the per-pool debug knob, v1 parity (Q327). A
							// change here reaches the pod template, which is what
							// rolls the pool so the new level takes effect.
							{Name: "LOG_LEVEL", Value: logLevelOrDefault(ep.Spec.LogLevel)},
						}, append(proxyAllowlistEnv(ep, gitHubHosts), proxyAuditEnv(ep)...)...),
						VolumeMounts: []corev1.VolumeMount{
							{
								Name:      proxyTLSVolumeName,
								MountPath: proxyTLSMountPath,
								ReadOnly:  true,
							},
							{
								Name:      metricsTLSVolumeName,
								MountPath: metricsTLSMountPath,
								ReadOnly:  true,
							},
						},
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
// pool on the proxy port and the mTLS metrics port. The selector carries the
// per-EgressProxy identity so it never fronts another pool's pods.
func buildEgressProxyService(ep *gmcv2alpha1.EgressProxy) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: proxyResourceName(ep), Namespace: ep.Namespace, Labels: egressProxyLabels(ep)},
		Spec: corev1.ServiceSpec{
			Selector: egressProxyPodSelector(ep),
			Ports: []corev1.ServicePort{
				{Name: "proxy", Port: proxyPort, TargetPort: intstr.FromInt32(proxyPort), Protocol: corev1.ProtocolTCP},
				// metrics: the mTLS Prometheus listener (metricsPort/:8443, Q324).
				// Named "metrics" so the EgressProxy ServiceMonitor targets it by name
				// without scraping the :8080 data port.
				{Name: "metrics", Port: metricsPort, TargetPort: intstr.FromInt32(metricsPort), Protocol: corev1.ProtocolTCP},
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
	// GitHub-CIDR 443 allowlist: the shared helper (builder.go) is the single
	// spelling of this rule across v1 and v2, so the proxy and workload policies
	// cannot silently diverge on which CIDRs may be reached on 443. The helper's
	// `ok` return encodes the len(githubCIDRs) > 0 fail-closed check; egressUsesCIDR
	// gates it to CIDR mode (an FQDN mode carries the GitHub allowlist on a
	// CNI-native policy instead — see this function's doc comment).
	if managed && egressUsesCIDR(ep.Spec) {
		if rule, ok := githubCIDREgressRule(githubCIDRs); ok {
			egress = append(egress, rule)
		}
	}

	// Operator-allowlisted extra CIDRs (Q242 G.1) are native ipBlock peers on the
	// standard NetworkPolicy, which is applied in EVERY egress mode — so a CIDR
	// destination works without a DNS-aware CNI and regardless of how GitHub egress
	// is expressed (CIDR rule here vs. toFQDNs on the CNI-native policy). FQDN-mode
	// proxies still get the standard NetworkPolicy, so these peers are honored there
	// too (NetworkPolicies are additive). Gated on managedNetworkPolicy: when the GMC
	// is not managing this proxy's policy, it adds nothing for the operator to layer.
	//
	// This is deliberately NOT githubCIDREgressRule: destinationCIDRs are EXTRA,
	// non-GitHub ranges (spec doc), a []string operator allowlist gated WITHOUT
	// egressUsesCIDR (it applies in every egress mode) (Q364).
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

	// Ingress: workload pods may reach the proxy port; monitoring namespaces may
	// scrape the mTLS metrics port (Q324); default-deny otherwise. The plaintext
	// kubelet probe port (healthMetricsPort) carries no metrics and needs no rule —
	// kubelet probe traffic is exempt from NetworkPolicy on every CNI this targets.
	ingress := []networkingv1.NetworkPolicyIngressRule{
		{
			From: []networkingv1.NetworkPolicyPeer{{
				PodSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{labelComponent: componentWorkload},
				},
			}},
			Ports: []networkingv1.NetworkPolicyPort{{Port: ptr(intstr.FromInt32(proxyPort))}},
		},
		metricsScrapeIngressRule(),
	}
	// Cross-namespace consumers (M4, §H.9). One peer struct per granted namespace,
	// carrying BOTH selectors: within a single peer they AND, so only workload pods
	// in that namespace match. Splitting them across two entries of From would OR
	// instead, admitting every pod in the granted namespace AND every workload pod in
	// every namespace — the whole grant, silently voided.
	//
	// Absent or empty allowedNamespaces adds nothing, leaving the same-namespace-only
	// default above untouched.
	if managed && ep.Spec.Sharing != nil {
		for _, ns := range ep.Spec.Sharing.AllowedNamespaces {
			if ns == ep.Namespace {
				continue // already covered by the same-namespace rule
			}
			ingress = append(ingress, networkingv1.NetworkPolicyIngressRule{
				From: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{corev1.LabelMetadataName: ns},
					},
					PodSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{labelComponent: componentWorkload},
					},
				}},
				Ports: []networkingv1.NetworkPolicyPort{{Port: ptr(intstr.FromInt32(proxyPort))}},
			})
		}
	}

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
