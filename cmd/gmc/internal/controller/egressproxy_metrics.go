package controller

import (
	"fmt"

	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// v2 EgressProxy metrics-mTLS provisioning (Q324). The standalone proxy pool serves
// /metrics over mutual TLS on metricsPort, at parity with the classic/v1 proxy: a
// scraper must present a client cert signed by this EgressProxy's own metrics CA.
// Every derived name is keyed on the EgressProxy so pools in one namespace never
// share a metrics identity, and the whole PKI is a per-EgressProxy bundle
// (ensureMetricsCerts) — never reused across tenants. These names stay well within
// the Secret/ServiceMonitor 253-char budget: the CRD caps ep.Name at 52 chars.
const (
	// egressProxyMetricsTLSSuffix / egressProxyMetricsClientSuffix derive the two
	// metrics Secret names from the EgressProxy name: "<ep>-metrics-tls" (the server
	// bundle mounted into the proxy pod) and "<ep>-metrics-client" (the scraper
	// bundle published for the monitoring stack, never mounted into the proxy).
	egressProxyMetricsTLSSuffix    = "-metrics-tls"    //nolint:gosec // G101: a name suffix, not a credential
	egressProxyMetricsClientSuffix = "-metrics-client" //nolint:gosec // G101: a name suffix, not a credential

	// egressProxyMetricsServiceMonitorSuffix names the per-EgressProxy
	// Prometheus-Operator ServiceMonitor: "<ep>-proxy-metrics".
	egressProxyMetricsServiceMonitorSuffix = "-metrics"
)

// egressProxyMetricsTLSSecretName is the name of the EgressProxy's metrics server
// bundle Secret (ca.crt + tls.crt + tls.key), mounted read-only into the proxy pod.
func egressProxyMetricsTLSSecretName(ep *gmcv2alpha1.EgressProxy) string {
	return ep.Name + egressProxyMetricsTLSSuffix
}

// egressProxyMetricsClientSecretName is the name of the EgressProxy's scraper client
// bundle Secret (ca.crt + tls.crt + tls.key), published for the monitoring stack.
func egressProxyMetricsClientSecretName(ep *gmcv2alpha1.EgressProxy) string {
	return ep.Name + egressProxyMetricsClientSuffix
}

// egressProxyMetricsServiceMonitorName is the name of the EgressProxy's per-tenant
// ServiceMonitor: "<ep>-proxy-metrics" (the proxy Service name plus "-metrics").
func egressProxyMetricsServiceMonitorName(ep *gmcv2alpha1.EgressProxy) string {
	return proxyResourceName(ep) + egressProxyMetricsServiceMonitorSuffix
}

// egressProxyMetricsServiceDNSName returns the "<ep>-proxy.<ns>.svc" in-cluster DNS
// name used as the scrape TLS serverName. It is one of the SANs on the metrics server
// cert (metricsServerSANsV2 keyed on proxyResourceName), so a ServiceMonitor setting
// serverName here verifies the scrape against the EgressProxy's metrics CA.
func egressProxyMetricsServiceDNSName(ep *gmcv2alpha1.EgressProxy) string {
	return fmt.Sprintf("%s.%s.svc", proxyResourceName(ep), ep.Namespace)
}

// buildEgressProxyMetricsTLSSecret constructs the server bundle Secret mounted into
// the proxy pod: the metrics CA cert (to verify scraper client certs) plus the metrics
// server cert+key. It is a kubernetes.io/tls Secret with an extra ca.crt key.
func buildEgressProxyMetricsTLSSecret(ep *gmcv2alpha1.EgressProxy, b *metricsCertBundle) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      egressProxyMetricsTLSSecretName(ep),
			Namespace: ep.Namespace,
			Labels:    egressProxyLabels(ep),
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       b.serverCertPEM,
			corev1.TLSPrivateKeyKey: b.serverKeyPEM,
			metricsCACertKey:        b.caPEM,
		},
	}
}

// buildEgressProxyMetricsClientSecret constructs the scraper bundle Secret published
// for the monitoring stack: the metrics CA cert (to verify the proxy metrics server)
// plus the scraper client cert+key (to authenticate to the mTLS listener). It is
// never mounted into the proxy pod.
func buildEgressProxyMetricsClientSecret(ep *gmcv2alpha1.EgressProxy, b *metricsCertBundle) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      egressProxyMetricsClientSecretName(ep),
			Namespace: ep.Namespace,
			Labels:    egressProxyLabels(ep),
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       b.clientCertPEM,
			corev1.TLSPrivateKeyKey: b.clientKeyPEM,
			metricsCACertKey:        b.caPEM,
		},
	}
}

// buildEgressProxyServiceMonitor builds the per-EgressProxy ServiceMonitor that
// scrapes the proxy pool's mTLS metrics port (Q324). It mirrors v1's
// buildMetricsServiceMonitor: an unstructured object (so the GMC keeps no
// compile-time dependency on the prometheus-operator API), living in the EgressProxy's
// namespace with no namespaceSelector, so it can only ever select Services in that
// namespace. The selector matches the "<ep>-proxy" metrics Service by the
// per-EgressProxy identity labels; the single endpoint scrapes the "metrics" port over
// HTTPS, presenting this EgressProxy's scraper client bundle for mTLS. serverName is
// the "<ep>-proxy.<ns>.svc" DNS name (a SAN on the metrics server cert), so the scrape
// verifies without insecureSkipVerify.
func buildEgressProxyServiceMonitor(ep *gmcv2alpha1.EgressProxy) *unstructured.Unstructured {
	clientSecret := egressProxyMetricsClientSecretName(ep)
	secretRef := func(key string) map[string]interface{} {
		return map[string]interface{}{
			"secret": map[string]interface{}{
				"name": clientSecret,
				"key":  key,
			},
		}
	}

	sm := &unstructured.Unstructured{}
	sm.SetGroupVersionKind(serviceMonitorGVK)
	sm.SetName(egressProxyMetricsServiceMonitorName(ep))
	sm.SetNamespace(ep.Namespace)
	sm.SetLabels(egressProxyLabels(ep))

	endpoint := map[string]interface{}{
		"port":   "metrics",
		"path":   "/metrics",
		"scheme": "https",
		"tlsConfig": map[string]interface{}{
			"serverName": egressProxyMetricsServiceDNSName(ep),
			"ca":         secretRef(metricsCACertKey),
			"cert":       secretRef(corev1.TLSCertKey),
			// keySecret is a bare SecretKeySelector (no enclosing "secret").
			"keySecret": map[string]interface{}{
				"name": clientSecret,
				"key":  corev1.TLSPrivateKeyKey,
			},
		},
		// The proxy exposes no intrinsic `namespace` label on its metrics (it does not
		// know its tenant), so stamp `namespace` from the scrape target's namespace —
		// which, since the proxy runs in the tenant namespace, is exactly the tenant —
		// so tenant dashboards can filter by $namespace (mirrors v1's proxy, Q314).
		"relabelings": []interface{}{
			map[string]interface{}{
				"action":       "replace",
				"sourceLabels": []interface{}{"__meta_kubernetes_namespace"},
				"targetLabel":  "namespace",
			},
		},
	}

	spec := map[string]interface{}{
		"selector": map[string]interface{}{
			"matchLabels": toStringMapIface(egressProxyLabels(ep)),
		},
		"endpoints": []interface{}{endpoint},
	}
	// unstructured.SetNestedMap deep-copies spec into sm.Object; it only errors on
	// non-JSON value types, and every value above is a JSON-compatible type.
	if err := unstructured.SetNestedMap(sm.Object, spec, "spec"); err != nil {
		panic(fmt.Sprintf("build EgressProxy ServiceMonitor spec: %v", err))
	}
	return sm
}
