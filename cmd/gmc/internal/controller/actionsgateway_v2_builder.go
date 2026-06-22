package controller

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"time"

	agcnames "github.com/actions-gateway/github-actions-gateway/agc/names"
	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// v2 ActionsGateway resource derivation. M3a is single-gateway-per-namespace, so
// the AGC control-plane children reuse the fixed v1 names (agc, worker, the
// workload/AGC NetworkPolicies, the metrics Secrets) — one gateway per namespace
// keeps them unique. Per-gateway derived naming lands in M3b for multi-gateway.
//
// Unlike v1, the v2 ActionsGateway reconciler does NOT provision the egress proxy
// pool (now a standalone EgressProxy, M2) or stamp Pod Security Admission labels
// (now the NamespacePSAReconciler, Q175). It provisions only the AGC control plane
// and wires its egress through the resolved EgressProxy.

// gatewayComponentLabel carries the owning ActionsGateway's name on every child,
// so M3b's multi-gateway naming/selectors have an identity to key on and operators
// can `kubectl get -l` a gateway's resources today.
const gatewayComponentLabel = "actions-gateway.com/gateway"

// v2GatewayLabels returns the metadata labels stamped on every AGC control-plane
// child: the managed-by marker plus the per-gateway identity label.
func v2GatewayLabels(ag *gmcv2alpha1.ActionsGateway) map[string]string {
	return map[string]string{
		labelManagedBy:        labelManagerValue,
		gatewayComponentLabel: ag.Name,
	}
}

func buildAGCServiceAccountV2(ag *gmcv2alpha1.ActionsGateway) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: agcSAName, Namespace: ag.Namespace, Labels: v2GatewayLabels(ag)},
	}
}

func buildWorkerServiceAccountV2(ag *gmcv2alpha1.ActionsGateway) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: workerSAName, Namespace: ag.Namespace, Labels: v2GatewayLabels(ag)},
	}
}

// buildAGCRoleBindingV2 binds the per-tenant AGC ServiceAccount to the shipped
// agc-tenant-role ClusterRole (scoped to this namespace), identical to v1.
func buildAGCRoleBindingV2(ag *gmcv2alpha1.ActionsGateway) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: agcSAName, Namespace: ag.Namespace, Labels: v2GatewayLabels(ag)},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: agcTenantRoleName},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: agcSAName, Namespace: ag.Namespace}},
	}
}

// buildAGCServiceV2 builds the AGC metrics Service (mTLS metrics port :8443), so a
// ServiceMonitor can scrape it. Mirrors v1's buildAGCService.
func buildAGCServiceV2(ag *gmcv2alpha1.ActionsGateway) *corev1.Service {
	labels := v2GatewayLabels(ag)
	labels["app"] = agcAppName
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: agcAppName, Namespace: ag.Namespace, Labels: labels},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": agcAppName},
			Ports:    []corev1.ServicePort{{Name: "metrics", Port: metricsPort, TargetPort: intstr.FromInt32(metricsPort), Protocol: corev1.ProtocolTCP}},
			Type:     corev1.ServiceTypeClusterIP,
		},
	}
}

// buildWorkloadNetworkPolicyV2 is the v2 workload egress lockdown: AGC and worker
// pods may reach DNS and the proxy on :8080 only; default-deny ingress. Identical
// in shape to v1's buildWorkloadNetworkPolicy (the proxy podSelector "app: proxy"
// matches the EgressProxy pods, which carry that label), so worker egress-IP
// attribution is no weaker than v1.
func buildWorkloadNetworkPolicyV2(ag *gmcv2alpha1.ActionsGateway) *networkingv1.NetworkPolicy {
	egress := []networkingv1.NetworkPolicyEgressRule{
		dnsEgressRule(),
		{
			Ports: []networkingv1.NetworkPolicyPort{{Port: ptr(intstr.FromInt32(proxyPort))}},
			To: []networkingv1.NetworkPolicyPeer{{
				PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": proxyAppName}},
			}},
		},
	}
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: npWorkloadName, Namespace: ag.Namespace, Labels: v2GatewayLabels(ag)},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{labelComponent: componentWorkload}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress, networkingv1.PolicyTypeIngress},
			Egress:      egress,
			// No Ingress rules: PolicyTypeIngress with an empty rule set is default-deny.
		},
	}
}

// buildAGCNetworkPolicyV2 is the v2 AGC egress policy: additive to the workload
// policy, it also permits Kubernetes API server egress (443/6443) and admits
// monitoring-namespace metrics scrapes. Identical in shape to v1's
// buildAGCNetworkPolicy.
func buildAGCNetworkPolicyV2(ag *gmcv2alpha1.ActionsGateway, apiServerCIDRs []string) *networkingv1.NetworkPolicy {
	return buildAGCNetworkPolicyFrom(ag.Namespace, v2GatewayLabels(ag), apiServerCIDRs)
}

// v2ProxyServiceAddr is the in-cluster HTTPS proxy address the AGC egresses
// through: the resolved EgressProxy's Service. Mirrors v1's buildProxyServiceAddr
// but keyed on the EgressProxy name (<ep>-proxy) rather than the fixed v1 proxy
// Service.
func v2ProxyServiceAddr(namespace, epName string) string {
	return fmt.Sprintf("https://%s-proxy.%s.svc.cluster.local:%d", epName, namespace, proxyPort)
}

// v2ProxyTLSSecretName is the resolved EgressProxy's self-signed TLS Secret name,
// matching the EgressProxy reconciler's egressProxyTLSSecretName (<ep>-proxy-tls).
func v2ProxyTLSSecretName(epName string) string { return epName + egressProxyTLSSuffix }

// buildAGCDeploymentV2 builds the AGC Deployment for a v2 ActionsGateway. It
// mirrors v1's buildAGCDeployment (credentials mounted as files never env, proxy
// CA pinning, metrics mTLS mount, hardened SecurityContext, probes) but wires
// egress through the resolved EgressProxy and reads the security profile from the
// namespace (PSA is namespace-scoped in v2). proxyName is the resolved EgressProxy
// name; securityProfile is the namespace's effective profile.
func buildAGCDeploymentV2(ag *gmcv2alpha1.ActionsGateway, agcImage, proxyName, securityProfile string, noProxyCIDRs []string, extraEnv []corev1.EnvVar) *appsv1.Deployment {
	proxyAddr := v2ProxyServiceAddr(ag.Namespace, proxyName)
	proxyTLSSecret := v2ProxyTLSSecretName(proxyName)

	env := []corev1.EnvVar{
		{Name: "POD_NAMESPACE", ValueFrom: fieldRef("metadata.namespace")},
		{Name: "WORKER_SERVICE_ACCOUNT", Value: workerSAName},
		{Name: "GITHUB_ORG_URL", Value: ag.Spec.GitHubURL},
		{Name: "HTTP_PROXY", Value: proxyAddr},
		{Name: "HTTPS_PROXY", Value: proxyAddr},
		{Name: "NO_PROXY", Value: buildNoProxy(noProxyCIDRs)},
		{Name: "PROXY_TLS_SECRET_NAME", Value: proxyTLSSecret},
		// SECURITY_PROFILE comes from the namespace security-profile label (v2 moved
		// PSA to the namespace, Q175). The reconciler reads the label; it does not
		// stamp the PSA labels — that is the NamespacePSAReconciler's job.
		{Name: "SECURITY_PROFILE", Value: securityProfile},
		{Name: "LOG_LEVEL", Value: logLevelOrDefault(ag.Spec.LogLevel)},
		{Name: "GITHUB_RUNNER_VERSION", Value: agcnames.RunnerVersion},
	}
	env = append(env, tracingEnvV2(ag.Spec.Tracing)...)
	env = append(env, extraEnv...)

	// The pod spec (credential/proxy-CA/metrics mounts, hardened SecurityContext,
	// probes) is identical to v1; only the metadata labels, the proxy CA Secret
	// name, and the env differ. Share the assembly via buildAGCDeploymentFrom.
	return buildAGCDeploymentFrom(ag.Namespace, v2GatewayLabels(ag), ag.Spec.GitHubAppRef.Name, proxyTLSSecret, agcImage, env)
}

// tracingEnvV2 translates the v2 spec.tracing into the OTEL_* environment the AGC
// reads. Mirrors v1's tracingEnv (the field set is identical); auth headers are
// deliberately not mapped (this project keeps secrets out of env vars).
func tracingEnvV2(t gmcv2alpha1.TracingConfig) []corev1.EnvVar {
	if t.Endpoint == "" {
		return nil
	}
	env := []corev1.EnvVar{{Name: "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", Value: t.Endpoint}}
	if t.Insecure != nil && *t.Insecure {
		env = append(env, corev1.EnvVar{Name: "OTEL_EXPORTER_OTLP_TRACES_INSECURE", Value: "true"})
	}
	if t.Sampler != "" {
		env = append(env, corev1.EnvVar{Name: "OTEL_TRACES_SAMPLER", Value: t.Sampler})
	}
	if t.SamplerArg != "" {
		env = append(env, corev1.EnvVar{Name: "OTEL_TRACES_SAMPLER_ARG", Value: t.SamplerArg})
	}
	if attrs := formatResourceAttributes(t.ResourceAttributes); attrs != "" {
		env = append(env, corev1.EnvVar{Name: "OTEL_RESOURCE_ATTRIBUTES", Value: attrs})
	}
	return env
}

// --- metrics mTLS PKI (v2) ---

// metricsServerSANsV2 lists the in-cluster DNS names the v2 metrics server cert
// must be valid for: the AGC Service (short + FQDN forms). v1 also covered the
// proxy Service, but in v2 the proxy is a standalone EgressProxy whose metrics
// listener is wired separately (M3b); the AGC owns this bundle for its own
// /metrics endpoint.
func metricsServerSANsV2(namespace string) []string {
	var sans []string
	sans = append(sans,
		agcAppName,
		fmt.Sprintf("%s.%s", agcAppName, namespace),
		fmt.Sprintf("%s.%s.svc", agcAppName, namespace),
		fmt.Sprintf("%s.%s.svc.cluster.local", agcAppName, namespace),
	)
	return sans
}

// generateMetricsCertsV2 builds the per-tenant metrics mTLS bundle (CA + server
// leaf for the AGC metrics listener + client leaf for the scraper). Mirrors v1's
// generateMetricsCerts, reusing the shared signLeaf/encodeCertPEM helpers; the CA
// key is not persisted (the whole bundle regenerates together on renewal).
func generateMetricsCertsV2(namespace string) (*metricsCertBundle, error) {
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generate CA key: %w", err)
	}
	caSerial, err := randSerial()
	if err != nil {
		return nil, err
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          caSerial,
		Subject:               pkix.Name{CommonName: fmt.Sprintf("actions-gateway-metrics-ca.%s", namespace)},
		NotBefore:             time.Now().Add(-1 * time.Minute),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("create CA cert: %w", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, fmt.Errorf("parse CA cert: %w", err)
	}
	caPEM := encodeCertPEM(caDER)

	serverCertPEM, serverKeyPEM, err := signLeaf(caCert, caKey, &x509.Certificate{
		Subject:     pkix.Name{CommonName: fmt.Sprintf("%s.%s.svc", agcAppName, namespace)},
		DNSNames:    metricsServerSANsV2(namespace),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	if err != nil {
		return nil, fmt.Errorf("sign server cert: %w", err)
	}

	clientCertPEM, clientKeyPEM, err := signLeaf(caCert, caKey, &x509.Certificate{
		Subject:     pkix.Name{CommonName: metricsScraperCN},
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	if err != nil {
		return nil, fmt.Errorf("sign client cert: %w", err)
	}

	return &metricsCertBundle{
		caPEM:         caPEM,
		serverCertPEM: serverCertPEM,
		serverKeyPEM:  serverKeyPEM,
		clientCertPEM: clientCertPEM,
		clientKeyPEM:  clientKeyPEM,
	}, nil
}

func buildMetricsTLSSecretV2(ag *gmcv2alpha1.ActionsGateway, b *metricsCertBundle) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: metricsTLSSecretName, Namespace: ag.Namespace, Labels: v2GatewayLabels(ag)},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       b.serverCertPEM,
			corev1.TLSPrivateKeyKey: b.serverKeyPEM,
			metricsCACertKey:        b.caPEM,
		},
	}
}

func buildMetricsClientSecretV2(ag *gmcv2alpha1.ActionsGateway, b *metricsCertBundle) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: metricsClientSecretName, Namespace: ag.Namespace, Labels: v2GatewayLabels(ag)},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       b.clientCertPEM,
			corev1.TLSPrivateKeyKey: b.clientKeyPEM,
			metricsCACertKey:        b.caPEM,
		},
	}
}
